package coretest_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/beacon"
	"github.com/wakeshift/server/internal/codexconnector"
	"github.com/wakeshift/server/internal/connector"
	"github.com/wakeshift/server/internal/coretest"
	"github.com/wakeshift/server/pkg/wakeshift"
)

// An agent's first turn calls two of an app's subscribing tools in
// parallel, before any result has given it a session_key, so both calls
// come with no key. Neither finds the other's session (a call without a
// key never finds one by the conversation ID its harness reports), so the
// core starts two: in the app user's workspace, or, without a user, each
// in a conversation workspace of its own. The harness's hook runs once per
// result and redeems each beacon, one after the other or both at once;
// whichever way, a publish to either tool's subscription must wake the
// conversation, through the link the hook keeps.

// parallelFirst makes the two calls at once, and their results.
func parallelFirst(t *testing.T, app *wakeshift.Client, calls [2]caller, topics [2]string) [2]noteSub {
	t.Helper()
	var subs [2]noteSub
	var errs [2]error
	var wg sync.WaitGroup
	for i := range calls {
		wg.Add(1)
		go func() {
			defer wg.Done()
			subs[i], errs[i] = subscribe(app, calls[i], topics[i], subOpts{})
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err) // before the fix, an app user's first two calls raced to make its agent: one failed (500)
		}
	}
	if subs[0].Session == "" || subs[0].Session == subs[1].Session {
		t.Fatalf("sessions %q and %q: two first calls without a key start two", subs[0].Session, subs[1].Session)
	}
	return subs
}

// hookIn is Wakeshift's real hook for a harness, keeping its files in dir:
// two of them on one dir are two hook processes of one harness.
func hookIn(c *coretest.Core, harness, dir string) *connector.Hook {
	spec, _ := connector.Lookup(harness)
	set := connector.Settings{Dir: dir, Issuers: []string{c.URL}}
	return &connector.Hook{Harness: harness, Format: spec.Format, Verifier: &connector.Verifier{Settings: set}, Poller: &connector.HTTPPoller{Issuers: set.Issuers},
		Links: connector.LinksFor(set), When: spec.When, StopWait: 3 * time.Second, Unbound: spec.Unbound}
}

// eachWay runs both hooks one after the other, then (on a new core) both
// at once.
func eachWay(t *testing.T, run func(t *testing.T, together bool)) {
	for _, together := range []bool{false, true} {
		t.Run(map[bool]string{false: "hooks in turn", true: "hooks at once"}[together], func(t *testing.T) { run(t, together) })
	}
}

func both(together bool, f func(i int)) {
	if !together {
		f(0)
		f(1)
		return
	}
	var wg sync.WaitGroup
	for i := range 2 {
		wg.Add(1)
		go func() { defer wg.Done(); f(i) }()
	}
	wg.Wait()
}

func TestParallelFirstCallsClaudeCode(t *testing.T) {
	for _, user := range []string{"bo", ""} {
		t.Run(map[bool]string{true: "app user", false: "no app user"}[user != ""], func(t *testing.T) {
			eachWay(t, func(t *testing.T, together bool) {
				c := coretest.Start(t)
				app := noteApp(t, c, "ci")
				dir := t.TempDir()
				const conv = "7a1f1c2e-5b53-4d0e-9a71-0c3e2f6d8b10"
				tr := filepath.Join(t.TempDir(), "s.jsonl")
				topics := [2]string{"ci/acme/web", "ci/acme/api"}
				tools := [2]string{"watch_builds", "watch_deploys"}
				var calls [2]caller
				for i := range calls {
					calls[i] = caller{User: user, Client: "claude-code", Tool: tools[i], Meta: map[string]any{"claudecode/toolUseId": fmt.Sprintf("toolu_%d", i+1)}}
				}
				subs := parallelFirst(t, app, calls, topics)
				// Claude Code writes both results to the transcript, then runs
				// PostToolUse for each.
				var lines []byte
				for i, sub := range subs {
					line, _ := json.Marshal(map[string]any{"type": "user", "sessionId": conv,
						"message": map[string]any{"role": "user", "content": []any{map[string]any{"tool_use_id": fmt.Sprintf("toolu_%d", i+1), "type": "tool_result", "content": "ok"}}},
						"mcpMeta": map[string]any{"_meta": sub.Addition.Meta}})
					lines = append(append(lines, line...), '\n')
				}
				if err := os.WriteFile(tr, lines, 0o600); err != nil {
					t.Fatal(err)
				}
				var outs [2]string
				both(together, func(i int) {
					outs[i] = runHook(hookIn(c, "claude-code", dir), "", map[string]any{"session_id": conv, "transcript_path": tr, "hook_event_name": "PostToolUse",
						"tool_name": "mcp__ci__" + tools[i], "tool_response": "ok", "tool_use_id": fmt.Sprintf("toolu_%d", i+1), "mcp_server": map[string]any{"name": "ci"}})
					if i == 0 && !together {
						// Published between the redeems: owed to each session as it
						// was, and still delivered once they're one.
						for j, topic := range topics {
							if _, err := emitNote(app, []string{topic}, note{Text: "between, to " + tools[j]}); err != nil {
								t.Error(err)
							}
						}
					}
				})
				for i, out := range outs {
					if !strings.Contains(out, "Wakeshift: connected") {
						t.Errorf("PostToolUse %d: %s", i+1, out)
					}
				}
				h := hookIn(c, "claude-code", dir)
				if got := h.Links.All(conv); len(got) != 1 {
					t.Fatalf("links: %+v", got)
				}
				h.Rewake, h.RewakeWait = true, 5*time.Second
				if !together {
					var got string
					for range 2 {
						got += runHook(h, "", map[string]any{"session_id": conv, "transcript_path": tr, "hook_event_name": "Stop"})
						if strings.Contains(got, "between, to "+tools[0]) && strings.Contains(got, "between, to "+tools[1]) {
							break
						}
					}
					for _, tool := range tools {
						if !strings.Contains(got, "between, to "+tool) {
							t.Errorf("a publish to %s between the redeems was lost: %q", tool, got)
						}
					}
				}
				for i, topic := range topics {
					msg := "published to " + tools[i]
					p, err := emitNote(app, []string{topic}, note{Text: msg})
					if err != nil {
						t.Fatal(err)
					}
					if out := runHook(h, "", map[string]any{"session_id": conv, "transcript_path": tr, "hook_event_name": "Stop"}); !strings.Contains(out, msg) {
						t.Errorf("a publish to %s (matched %d) didn't wake the conversation: %q", tools[i], p.Matched, out)
					}
				}
				// Either key the conversation was given finds both subscriptions
				// (with a user: the merged session's key finds the one it went
				// into), so unsubscribing from the first tool's works by its own.
				if user != "" {
					if n, err := unsubscribe(app, caller{User: user, Client: "claude-code", Session: subs[0].Session}, topics[0]); err != nil || n != 1 {
						t.Errorf("unsubscribe by the first result's key: %d %v", n, err)
					}
					if n, err := unsubscribe(app, caller{User: user, Client: "claude-code", Session: subs[1].Session}, topics[1]); err != nil || n != 1 {
						t.Errorf("unsubscribe by the second result's key: %d %v", n, err)
					}
				}
			})
		})
	}
}

func TestParallelFirstCallsCodex(t *testing.T) {
	for _, user := range []string{"cx", ""} {
		t.Run(map[bool]string{true: "app user", false: "no app user"}[user != ""], func(t *testing.T) {
			eachWay(t, func(t *testing.T, together bool) {
				c := coretest.Start(t)
				t.Setenv("CODEX_HOME", t.TempDir())
				t.Setenv("WAKESHIFT_HOME", t.TempDir())
				app := noteApp(t, c, "ci")
				thread := "01a1ae69-49bf-7fc3-9c57-5729ede96897"
				meta := map[string]any{"x-codex-turn-metadata": map[string]any{"thread_id": thread}}
				topics := [2]string{"ci/acme/web", "ci/acme/api"}
				tools := [2]string{"watch_builds", "watch_deploys"}
				var calls [2]caller
				for i := range calls {
					calls[i] = caller{User: user, Client: "codex-mcp-client", Tool: tools[i], Meta: meta}
				}
				var subs [2]noteSub
				if user != "" {
					// With a user, a Codex thread is found by its ID: one session
					// whichever call made it.
					var errs [2]error
					both(true, func(i int) { subs[i], errs[i] = subscribe(app, calls[i], topics[i], subOpts{}) })
					if errs[0] != nil || errs[1] != nil || subs[0].Session != subs[1].Session {
						t.Fatalf("a user's thread: %q %q %v %v", subs[0].Session, subs[1].Session, errs[0], errs[1])
					}
				} else {
					subs = parallelFirst(t, app, calls, topics)
				}
				set := connector.Settings{Dir: t.TempDir(), Issuers: []string{c.URL}}
				sock := codexDaemon(t, thread)
				var mu sync.Mutex
				var started []string
				var outs [2]string
				both(together, func(i int) {
					h := &codexconnector.Hook{Settings: set, Socket: sock, Start: func(_, link, _, _ string, _ bool) error {
						mu.Lock()
						defer mu.Unlock()
						started = append(started, link)
						return nil
					}}
					res := map[string]any{"content": []any{map[string]any{"type": "text", "text": subs[i].Addition.Text}}, "_meta": subs[i].Addition.Meta}
					b, _ := json.Marshal(map[string]any{"session_id": thread, "hook_event_name": "PostToolUse", "tool_name": "mcp__ci__" + tools[i], "tool_response": res})
					var out bytes.Buffer
					h.Run(ctx, bytes.NewReader(b), &out)
					outs[i] = out.String()
				})
				for i, out := range outs {
					if !strings.Contains(out, codexconnector.NoteConnected) {
						t.Errorf("PostToolUse %d: %s", i+1, out)
					}
				}
				// What the hook keeps is what it re-arms with at the user's next
				// message: both subscriptions arrive on it.
				kept := connector.LinksFor(set).LoadFor(thread, "ci")
				if kept == "" {
					t.Fatal("no link kept")
				}
				p := &connector.HTTPPoller{Issuers: []string{c.URL}}
				for i, topic := range topics {
					msg := "published to " + tools[i]
					if _, err := emitNote(app, []string{topic}, note{Text: msg}); err != nil {
						t.Fatal(err)
					}
					b, err := p.Poll(ctx, kept, 5*time.Second, true)
					if err != nil || len(b.Deliveries) != 1 || !strings.Contains(fmt.Sprint(b.Deliveries[0].Event), msg) {
						t.Errorf("a publish to %s on the kept link: %+v %v", tools[i], b, err)
						continue
					}
					if err := p.Ack(ctx, kept, []string{b.Deliveries[0].ID}); err != nil {
						t.Fatal(err)
					}
				}
			})
		})
	}
}

// What makes it one session never lets a beacon move a conversation's
// subscriptions to another conversation's workspace: without a user, the
// ticket's session's subscriptions join the session the hook already
// holds, never the other way, so whoever holds the ticket's session_key
// gets nothing of the held one's.
func TestParallelFirstCallsFoldIntoTheHeldSession(t *testing.T) {
	c := coretest.Start(t)
	app := noteApp(t, c, "ci")
	h := hookIn(c, "claude-code", t.TempDir())
	const conv = "0b7c3a1d-1f0e-4a52-8d0c-2b6f9e4c7a31"
	tr := filepath.Join(t.TempDir(), "s.jsonl")
	connect := func(sub noteSub, call, tool string) {
		t.Helper()
		line, _ := json.Marshal(map[string]any{"type": "user", "sessionId": conv,
			"message": map[string]any{"role": "user", "content": []any{map[string]any{"tool_use_id": call, "type": "tool_result", "content": "ok"}}},
			"mcpMeta": map[string]any{"_meta": sub.Addition.Meta}})
		f, _ := os.OpenFile(tr, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o600)
		f.Write(append(line, '\n'))
		f.Close()
		if out := runHook(h, "", map[string]any{"session_id": conv, "transcript_path": tr, "hook_event_name": "PostToolUse",
			"tool_name": "mcp__ci__" + tool, "tool_response": "ok", "tool_use_id": call, "mcp_server": map[string]any{"name": "ci"}}); !strings.Contains(out, "Wakeshift: connected") {
			t.Fatalf("connecting %s: %s", call, out)
		}
	}
	mine, err := subscribe(app, caller{Client: "claude-code", Tool: "watch", Meta: map[string]any{"claudecode/toolUseId": "toolu_a"}}, "ci/acme/web", subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	connect(mine, "toolu_a", "watch")
	held := h.Links.LoadFor(conv, "ci")
	other, err := subscribe(app, caller{Client: "claude-code", Tool: "watch", Meta: map[string]any{"claudecode/toolUseId": "toolu_b"}}, "ci/acme/api", subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	connect(other, "toolu_b", "watch")
	mineTenant, _ := topicSubRow(t, c, mine.ID)
	otherTenant, _ := topicSubRow(t, c, other.ID)
	var otherExpiry time.Time
	if err := c.DB.InTenant(ctx, otherTenant, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT expires_at FROM topic_subscriptions WHERE topic = 'ci/acme/api'`).Scan(&otherExpiry)
	}); err != nil {
		t.Fatal(err)
	}
	// Both subscriptions are the held session's, in its workspace; the
	// other workspace keeps none, and the link the hook keeps is the held
	// session's (a new one).
	if n := count(t, c, mineTenant, `SELECT count(*) FROM topic_subscriptions WHERE ended_at IS NULL`); n != 2 {
		t.Fatalf("the held session's subscriptions: %d", n)
	}
	var folded time.Time
	if err := c.DB.InTenant(ctx, mineTenant, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT expires_at FROM topic_subscriptions WHERE topic = 'ci/acme/api' AND ended_at IS NULL`).Scan(&folded)
	}); err != nil || !folded.Equal(otherExpiry) {
		t.Fatalf("the folded subscription's expiry %v, was %v (%v)", folded, otherExpiry, err)
	}
	if n := count(t, c, otherTenant, `SELECT count(*) FROM topic_subscriptions WHERE ended_at IS NULL`); n != 0 {
		t.Fatalf("the other workspace's live subscriptions: %d", n)
	}
	if n := count(t, c, otherTenant, `SELECT count(*) FROM sessions WHERE watching`); n != 0 {
		t.Fatalf("the other session still watches: %d", n)
	}
	kept := h.Links.LoadFor(conv, "ci")
	if kept == held {
		t.Fatal("the hook kept the old link")
	}
	p := &connector.HTTPPoller{Issuers: []string{c.URL}}
	if _, err := emitNote(app, []string{"ci/acme/api"}, note{Text: "to the other's topic"}); err != nil {
		t.Fatal(err)
	}
	if b, err := p.Poll(ctx, kept, 5*time.Second, true); err != nil || len(b.Deliveries) != 1 || !strings.Contains(fmt.Sprint(b.Deliveries[0].Event), "to the other's topic") {
		t.Fatalf("the kept link: %+v %v", b, err)
	}
	// The other key names its own (empty) conversation: nothing of the
	// held one's.
	if r, err := annotate(app, caller{Client: "claude-code", Session: other.Session}); err != nil || beacon.InText(r.Text) != "" && strings.Contains(r.Text, mine.Session) {
		t.Fatalf("the other key: %+v %v", r, err)
	}
	if n, err := unsubscribe(app, caller{Client: "claude-code", Session: other.Session}, "ci/acme/web"); err != nil || n != 0 {
		t.Fatalf("the other key unsubscribed the held session's: %d %v", n, err)
	}
}
