package coretest_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/beacon"
	"github.com/wakeshift/server/internal/codexconnector"
	"github.com/wakeshift/server/internal/connector"
	"github.com/wakeshift/server/internal/coretest"
)

// codexDaemon answers thread/read for thread on a unix socket, as Codex's
// app-server daemon does (the hook checks the thread is on it).
func codexDaemon(t *testing.T, thread string) string {
	t.Helper()
	dir, _ := os.MkdirTemp("", "cx") // short: unix socket paths are limited
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		for {
			_, b, err := c.Read(context.Background())
			if err != nil {
				return
			}
			var m struct {
				ID     *int64
				Method string
				Params struct {
					ThreadID string `json:"threadId"`
				}
			}
			json.Unmarshal(b, &m)
			if m.ID == nil {
				continue
			}
			reply := map[string]any{"id": *m.ID, "result": map[string]any{}}
			if m.Method == "thread/read" {
				if m.Params.ThreadID == thread {
					reply["result"] = map[string]any{"thread": map[string]any{"id": thread, "status": map[string]any{"type": "idle"}}}
				} else {
					delete(reply, "result")
					reply["error"] = map[string]any{"code": -32600, "message": "thread not found"}
				}
			}
			out, _ := json.Marshal(reply)
			c.Write(context.Background(), websocket.MessageText, out)
		}
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return sock
}

// codexRollout writes a Codex 0.166 rollout for thread in which the app's
// server (server) returned result for tool, as Codex records it: an
// item_completed McpToolCall event carrying the whole result, _meta
// included.
func codexRollout(t *testing.T, dir, thread, server, tool string, result map[string]any) string {
	t.Helper()
	path := filepath.Join(dir, "rollout-2026-11-05T17-11-38-"+thread+".jsonl")
	lines := []map[string]any{
		{"type": "session_meta", "payload": map[string]any{"id": thread}},
		{"type": "event_msg", "payload": map[string]any{"type": "item_completed", "thread_id": thread, "turn_id": "t1",
			"item": map[string]any{"type": "McpToolCall", "id": "exec-1", "server": server, "tool": tool, "arguments": map[string]any{}, "status": "completed", "result": result}}},
		{"type": "response_item", "payload": map[string]any{"type": "custom_tool_call_output", "call_id": "call_1", "output": []any{}}},
	}
	var b bytes.Buffer
	for _, l := range lines {
		json.NewEncoder(&b).Encode(l)
	}
	if err := os.WriteFile(path, b.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// A Codex thread whose watch result offered the install, and whose agent
// then installed the plugin but called none of the app's tools again (each
// may have side effects): the hook connects it at the user's next message
// (SessionStart, UserPromptSubmit) from that result's beacon in the
// rollout, an hour-long late redeem the core accepts only for a beacon
// bound to the thread Codex itself named, for a session no connector has
// connected, once. What arrived while the user decided is delivered.
func TestCodexHookLateRedeemFromTheRollout(t *testing.T) {
	c := coretest.Start(t)
	t.Setenv("CODEX_HOME", t.TempDir())
	t.Setenv("WAKESHIFT_HOME", t.TempDir())
	app := noteApp(t, c, "ci")
	thread := "01a1ae69-49bf-7fc3-9c57-5729ede96896"
	call := caller{User: "cx", Client: "codex-mcp-client", Tool: "watch", Meta: map[string]any{"x-codex-turn-metadata": map[string]any{"thread_id": thread}}}
	sub, err := subscribe(app, call, "ci/acme", subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sub.Addition.Text, "Install Wakeshift") || strings.Contains(sub.Addition.Text, "this tool again") {
		t.Fatalf("not an install offer, or asks for another call: %s", sub.Addition.Text)
	}
	tok, _ := sub.Addition.Meta[beacon.MetaKey].(string)
	if tok == "" {
		t.Fatal("no beacon in the offer's _meta")
	}
	emitNote(app, []string{"ci/acme"}, note{Text: "while asking"})
	// The user installs and restarts Codex: the ticket has expired by then.
	tenant, _ := topicSubRow(t, c, sub.ID)
	if err := c.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE connector_tickets SET expires_at = now() - interval '10 minutes'`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	result := map[string]any{"content": []any{map[string]any{"type": "text", "text": sub.Addition.Text}}, "_meta": map[string]any{beacon.MetaKey: tok}}
	rollout := codexRollout(t, t.TempDir(), thread, "ci", "watch", result)

	var started []string
	set := connector.Settings{Dir: t.TempDir(), Issuers: []string{c.URL}, InstalledAt: time.Now().Add(time.Second)}
	h := &codexconnector.Hook{Settings: set, Socket: codexDaemon(t, thread),
		Start: func(th, link, app, _ string, _ bool) error { started = append(started, link); return nil }}
	run := func(event, path string) string {
		b, _ := json.Marshal(map[string]any{"session_id": thread, "hook_event_name": event, "transcript_path": path, "source": "resume"})
		var out bytes.Buffer
		h.Run(ctx, bytes.NewReader(b), &out)
		if out.Len() == 0 {
			return ""
		}
		var m struct {
			HookSpecificOutput struct{ HookEventName, AdditionalContext string }
		}
		if err := json.Unmarshal(out.Bytes(), &m); err != nil || m.HookSpecificOutput.HookEventName != event {
			t.Fatalf("hook output %q", out.String())
		}
		// The mark's note comes at every SessionStart (TestCodexPluginMark).
		return withoutCodexMark(m.HookSpecificOutput.AdditionalContext)
	}
	// Another thread's rollout (named for another thread) isn't read.
	other := codexRollout(t, t.TempDir(), "01a1ae69-0000-7fc3-9c57-5729ede96896", "ci", "watch", result)
	if got := run(codexconnector.EventSessionStart, other); got != "" || len(started) != 0 {
		t.Fatalf("another thread's rollout: %q %v", got, started)
	}
	if got := run(codexconnector.EventSessionStart, rollout); got != codexconnector.NoteConnected || len(started) != 1 {
		t.Fatalf("late: %q %v", got, started)
	}
	p := &connector.HTTPPoller{Issuers: []string{c.URL}}
	b, err := p.Poll(ctx, started[0], 10*time.Second, true)
	if err != nil || len(b.Deliveries) != 1 || !strings.Contains(fmt.Sprint(b.Deliveries[0].Event), "while asking") {
		t.Fatalf("the held message: %+v %v", b, err)
	}
	// Once: UserPromptSubmit, run with it, re-arms the kept link instead.
	if got := run(codexconnector.EventUserPromptSubmit, rollout); got != "" || len(started) != 2 {
		t.Fatalf("again: %q %v", got, started)
	}
	if started[1] != started[0] {
		t.Fatal("the re-arm didn't use the kept link")
	}
	// The same ticket, late again, by anyone: used up.
	claims, err := beacon.VerifyBound(tok, beacon.Binding{Conversation: thread}, []string{c.URL}, coreKeys(t, c), time.Unix(claims0(tok), 0))
	if err != nil {
		t.Fatal(err)
	}
	if code, _ := lateRedeem(t, claims.Redeem, claims.Ticket, thread, "", true); code != 404 {
		t.Fatalf("the same ticket again: %d", code)
	}
}

// The core's side of Codex's late redeem: only for the thread the beacon
// is bound to, within the hour after its expiry, while no connector has
// connected the session, once.
func TestCodexLateRedeemLimits(t *testing.T) {
	c := coretest.Start(t)
	app := noteApp(t, c, "ci")
	keys := coreKeys(t, c)
	expire := func(sub noteSub, by string) {
		tenant, _ := topicSubRow(t, c, sub.ID)
		if err := c.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE connector_tickets SET expires_at = now() - $1::interval`, by)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	codexSub := func(user, thread string) (noteSub, beacon.Claims) {
		t.Helper()
		sub, err := subscribe(app, caller{User: user, Client: "codex-mcp-client", Tool: "watch", Meta: map[string]any{"x-codex-turn-metadata": map[string]any{"thread_id": thread}}}, "ci/acme", subOpts{})
		if err != nil {
			t.Fatal(err)
		}
		tok, _ := sub.Addition.Meta[beacon.MetaKey].(string)
		claims, err := beacon.VerifyBound(tok, beacon.Binding{Conversation: thread}, []string{c.URL}, keys, time.Unix(claims0(tok), 0))
		if err != nil {
			t.Fatal(err)
		}
		return sub, claims
	}

	a := "01a1ae69-aaaa-7fc3-9c57-5729ede96896"
	sa, ca := codexSub("ua", a)
	expire(sa, "5 minutes")
	// Not late: expired.
	if code, _ := lateRedeem(t, ca.Redeem, ca.Ticket, a, "", false); code != 404 {
		t.Fatalf("not late: %d", code)
	}
	// Another thread: refused, and the ticket isn't used up by it.
	if code, _ := lateRedeem(t, ca.Redeem, ca.Ticket, "01a1ae69-bbbb-7fc3-9c57-5729ede96896", "", true); code == 200 {
		t.Fatalf("another thread: %d", code)
	}
	if code, out := lateRedeem(t, ca.Redeem, ca.Ticket, a, "", true); code != 200 || out["url"] == "" {
		t.Fatalf("late, its thread: %d %v", code, out)
	}
	if code, _ := lateRedeem(t, ca.Redeem, ca.Ticket, a, "", true); code != 404 {
		t.Fatalf("again: %d", code)
	}
	// Once a connector has connected the session, an older beacon of it
	// isn't redeemable late.
	sa2, ca2 := codexSub("ua", a)
	if sa2.Session != sa.Session {
		t.Fatalf("a new session for the same thread: %s %s", sa2.Session, sa.Session)
	}
	expire(sa2, "1 minute")
	if code, _ := lateRedeem(t, ca2.Redeem, ca2.Ticket, a, "", true); code != 404 {
		t.Fatalf("late, once connected: %d", code)
	}

	// Past the hour.
	b := "01a1ae69-cccc-7fc3-9c57-5729ede96896"
	sb, cb := codexSub("ub", b)
	expire(sb, "61 minutes")
	if code, _ := lateRedeem(t, cb.Redeem, cb.Ticket, b, "", true); code != 404 {
		t.Fatalf("past the hour: %d", code)
	}
}
