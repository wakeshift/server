package coretest_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/wakeshift/server/internal/beacon"
	"github.com/wakeshift/server/internal/connector"
	"github.com/wakeshift/server/internal/coretest"
	"github.com/wakeshift/server/internal/harnesses"
)

// Right after Wakeshift's hooks for Claude Code are installed, the Monitor
// (the session's wait in a loop) and the hooks were both live, and a post
// was lost. Now the hooks take over when they connect: their first poll
// (the PostToolUse hook's, at once) takes the stream, the Monitor's command
// ends with the note that they're connected (so the model learns it now,
// not at the next user prompt), and the next post reaches the hooks'
// waiter. The note isn't repeated in the next tool result.
func TestClaudeHooksTakeOverFromTheMonitor(t *testing.T) {
	c := coretest.Start(t)
	app := noteApp(t, c, "ci")
	keys := coreKeys(t, c)
	call := caller{User: "bo", Client: "claude-code", Meta: map[string]any{"claudecode/toolUseId": "toolu_1"}}
	sub, err := subscribe(app, call, "ci/acme", subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	call.Session = sub.Session
	mon := monitorRE.FindStringSubmatch(sub.Addition.Text)
	if mon == nil || !strings.Contains(mon[1], "until=hooks") {
		t.Fatalf("no Monitor command: %s", sub.Addition.Text)
	}
	monitor, _ := runMonitor(t, mon[1])
	waitFor(t, "the Monitor connected", func() bool {
		st, _ := noteStatus(app, "ci/acme")
		return st.Connected == 1
	})
	emitNote(app, []string{"ci/acme"}, note{Text: "on the Monitor"})
	monitor.expect("on the Monitor")

	// The user installs the hooks: the next tool result's beacon connects.
	tok, _ := sub.Addition.Meta[beacon.MetaKey].(string)
	claims, err := beacon.VerifyBound(tok, beacon.Binding{Conversation: "s1", Call: "toolu_1"}, []string{c.URL}, keys, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	code, out := redeemTicket(t, claims.Redeem, claims.Ticket, "s1")
	link, _ := out["url"].(string)
	if code != 200 || link == "" {
		t.Fatalf("redeem: %d %v", code, out)
	}
	p := &connector.HTTPPoller{Issuers: []string{c.URL}}
	if b, err := p.Poll(ctx, link, 0, false); err != nil || b.Background {
		t.Fatalf("the hook's first poll stood down: %+v %v", b, err)
	}
	if f, err := monitor.next(5 * time.Second); err == nil || err.Error() != "closed: hooks" || !strings.HasPrefix(monitor.closedNote, harnesses.Connected+".") {
		t.Fatalf("the Monitor: %v %+v (%s)", err, f, monitor.closedNote)
	}
	// The hooks' waiter gets what comes next.
	got := make(chan connector.Batch, 1)
	go func() {
		b, _ := p.Poll(ctx, link, 20*time.Second, true)
		got <- b
	}()
	time.Sleep(200 * time.Millisecond)
	emitNote(app, []string{"ci/acme"}, note{Text: "for the hooks"})
	select {
	case b := <-got:
		if len(b.Deliveries) != 1 {
			t.Fatalf("the waiter: %+v", b)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the hooks' waiter got nothing")
	}
	if r, _ := annotate(app, call); strings.Contains(r.Text, harnesses.Connected) {
		t.Fatalf("the note again: %s", r.Text)
	}
}

// Without a Monitor running, the note that the hooks connected comes with
// the next tool result, once (Claude Code's hook context reaches the model
// only at the next user prompt; Antigravity's at the next turn).
func TestConnectedNoteComesWithTheNextResult(t *testing.T) {
	c := coretest.Start(t)
	app := noteApp(t, c, "ci")
	keys := coreKeys(t, c)
	for _, tc := range []struct {
		name string
		call caller
		bind beacon.Binding
		text bool // the beacon is in the text
	}{
		{"claude-code", caller{User: "cl", Client: "claude-code", Meta: map[string]any{"claudecode/toolUseId": "toolu_9"}}, beacon.Binding{Conversation: "s9", Call: "toolu_9"}, false},
		{"antigravity", caller{User: "ag", Client: "antigravity-client", Meta: map[string]any{"antigravity.google/conversation_id": "7d1f2c3b-4a5e-4f60-9b7a-8c9d0e1f2a3b"}},
			beacon.Binding{Conversation: "7d1f2c3b-4a5e-4f60-9b7a-8c9d0e1f2a3b"}, true},
	} {
		sub, err := subscribe(app, tc.call, "ci/"+tc.name, subOpts{})
		if err != nil {
			t.Fatal(err)
		}
		call := tc.call
		call.Session = sub.Session
		tok, _ := sub.Addition.Meta[beacon.MetaKey].(string)
		if tc.text {
			tok = beacon.InText(sub.Addition.Text)
		}
		claims, err := beacon.VerifyBound(tok, tc.bind, []string{c.URL}, keys, time.Now())
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		code, out := redeemTicket(t, claims.Redeem, claims.Ticket, tc.bind.Conversation)
		if code != 200 {
			t.Fatalf("%s: redeem %d %v", tc.name, code, out)
		}
		waiterPolls(t, c, out) // the hooks deliver once their waiter listens
		r, _ := annotate(app, call)
		if !strings.HasPrefix(r.Text, harnesses.Connected+". Wakeshift's hooks for ") {
			t.Fatalf("%s: the next result: %s", tc.name, r.Text)
		}
		if r, _ := annotate(app, call); strings.Contains(r.Text, harnesses.Connected) {
			t.Fatalf("%s: again: %s", tc.name, r.Text)
		}
	}
}

// Plugin first: a Claude Code conversation asks about the plugin before it
// starts the Monitor, so nothing waits on its stream while the user decides
// and reloads. What arrives meanwhile is held. The agent calls nothing
// again (the tool may have side effects): once the plugin is loaded, the
// waiter that runs when the user's next turn ends finds the result that
// offered the install in the transcript and redeems its beacon late, and
// the hooks get what was held. A subscribe made anyway says the plugin
// connects by itself, and asks for no confirming call.
func TestClaudeCodePluginFirstHoldsMessages(t *testing.T) {
	c := coretest.Start(t)
	app := noteApp(t, c, "ci")
	keys := coreKeys(t, c)
	call := caller{User: "bo", Client: "claude-code", Meta: map[string]any{"claudecode/toolUseId": "toolu_1"}}
	sub, err := subscribe(app, call, "ci/acme", subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(sub.Addition.Text, "Subscribed, but that alone won't tell you when something arrives") || strings.Contains(sub.Addition.Text, "this tool again") ||
		strings.Contains(sub.Addition.Text, "once more") {
		t.Fatalf("not plugin first, or asks for another call: %s", sub.Addition.Text)
	}
	// While the user decides (and reloads), no Monitor runs.
	emitNote(app, []string{"ci/acme"}, note{Text: "while asking"})
	call.Session = sub.Session

	// A subscribe made anyway: the plugin connects by itself.
	call.Meta = map[string]any{"claudecode/toolUseId": "toolu_2"}
	back, err := subscribe(app, call, "ci/acme", subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(back.Addition.Text, "Subscribed. Ci's notifications reach this conversation the way the user picks") ||
		!strings.Contains(back.Addition.Text, "If Wakeshift's hooks for Claude Code were installed, start nothing") ||
		!strings.Contains(back.Addition.Text, "connect this conversation by themselves") || strings.Contains(back.Addition.Text, "once more") || strings.Contains(back.Addition.Text, "with this command") {
		t.Fatalf("back: %s", back.Addition.Text)
	}

	// The first result's beacon, as the waiter reads it from the transcript
	// after the install: its ticket has expired.
	tok, _ := sub.Addition.Meta[beacon.MetaKey].(string)
	claims, err := beacon.VerifyBound(tok, beacon.Binding{Conversation: "s1", Call: "toolu_1"}, []string{c.URL}, keys, time.Unix(claims0(tok), 0))
	if err != nil {
		t.Fatal(err)
	}
	tenant, _ := topicSubRow(t, c, sub.ID)
	if err := c.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE connector_tickets SET expires_at = now() - interval '10 minutes'`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// Not late: expired.
	if code, _ := lateRedeem(t, claims.Redeem, claims.Ticket, "s1", "toolu_1", false); code != 404 {
		t.Fatalf("an expired ticket, not late: %d", code)
	}
	code, out := lateRedeem(t, claims.Redeem, claims.Ticket, "s1", "toolu_1", true)
	link, _ := out["url"].(string)
	if code != 200 || link == "" {
		t.Fatalf("late redeem: %d %v", code, out)
	}
	p := &connector.HTTPPoller{Issuers: []string{c.URL}}
	b, err := p.Poll(ctx, link, 10*time.Second, true)
	if err != nil || len(b.Deliveries) != 1 || !strings.Contains(fmt.Sprint(b.Deliveries[0].Event), "while asking") {
		t.Fatalf("the held message: %+v %v", b, err)
	}
	// Single use, and not once a connector has connected the session.
	if code, _ := lateRedeem(t, claims.Redeem, claims.Ticket, "s1", "toolu_1", true); code != 404 {
		t.Fatalf("the same ticket again: %d", code)
	}
	tok2, _ := back.Addition.Meta[beacon.MetaKey].(string)
	c2, err := beacon.VerifyBound(tok2, beacon.Binding{Conversation: "s1", Call: "toolu_2"}, []string{c.URL}, keys, time.Unix(claims0(tok2), 0))
	if err != nil {
		t.Fatal(err)
	}
	if code, _ := lateRedeem(t, c2.Redeem, c2.Ticket, "s1", "toolu_2", true); code != 404 {
		t.Fatalf("late, once connected: %d", code)
	}
	// The next subscribe starts with the "connected" note.
	call.Meta = map[string]any{"claudecode/toolUseId": "toolu_3"}
	confirm, err := subscribe(app, call, "ci/acme", subOpts{})
	if err != nil || !strings.HasPrefix(confirm.Addition.Text, harnesses.Connected+".") || strings.Contains(confirm.Addition.Text, "curl -s") {
		t.Fatalf("after: %s %v", confirm.Addition.Text, err)
	}
}

// A late redeem is only for a beacon the model never sees (Claude Code's,
// bound to a call; Codex's, bound to its thread: TestCodexLateRedeemLimits),
// within the hour after its expiry: Cursor's (in text) isn't, nor one past
// the window, nor one without the call it's bound to.
func TestLateRedeemOnlyForMetaBeacons(t *testing.T) {
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
	// Cursor: its beacon is in text, which the model may see.
	cu, err := subscribe(app, caller{User: "cu", Client: "cursor", Conversation: "8c0a3a7e-6b8f-4f57-9a35-1b4f3c2d9e10"}, "ci/acme", subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if tok := beacon.InText(cu.Addition.Text); tok != "" {
		cc, err := beacon.VerifyBound(tok, beacon.Binding{Conversation: "8c0a3a7e-6b8f-4f57-9a35-1b4f3c2d9e10"}, []string{c.URL}, keys, time.Unix(claims0(tok), 0))
		if err != nil {
			t.Fatal(err)
		}
		expire(cu, "1 minute")
		if code, _ := lateRedeem(t, cc.Redeem, cc.Ticket, "8c0a3a7e-6b8f-4f57-9a35-1b4f3c2d9e10", "", true); code != 404 {
			t.Fatalf("Cursor, late: %d", code)
		}
	} else {
		t.Fatalf("no text beacon for Cursor: %s", cu.Addition.Text)
	}
	// Claude Code, past the hour.
	cl, err := subscribe(app, caller{User: "cl", Client: "claude-code", Meta: map[string]any{"claudecode/toolUseId": "toolu_9"}}, "ci/acme", subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	tok, _ := cl.Addition.Meta[beacon.MetaKey].(string)
	ccl, err := beacon.VerifyBound(tok, beacon.Binding{Conversation: "s9", Call: "toolu_9"}, []string{c.URL}, keys, time.Unix(claims0(tok), 0))
	if err != nil {
		t.Fatal(err)
	}
	expire(cl, "61 minutes")
	if code, _ := lateRedeem(t, ccl.Redeem, ccl.Ticket, "s9", "toolu_9", true); code != 404 {
		t.Fatalf("past the hour: %d", code)
	}
	// Without the call it names.
	cl2, _ := subscribe(app, caller{User: "cl2", Client: "claude-code", Meta: map[string]any{"claudecode/toolUseId": "toolu_8"}}, "ci/acme", subOpts{})
	tok, _ = cl2.Addition.Meta[beacon.MetaKey].(string)
	ccl2, err := beacon.VerifyBound(tok, beacon.Binding{Conversation: "s8", Call: "toolu_8"}, []string{c.URL}, keys, time.Unix(claims0(tok), 0))
	if err != nil {
		t.Fatal(err)
	}
	expire(cl2, "5 minutes")
	if code, _ := lateRedeem(t, ccl2.Redeem, ccl2.Ticket, "s8", "", true); code != 404 {
		t.Fatalf("no call: %d", code)
	}
	// A late redeem refused before the ticket is looked at further doesn't
	// use it up: with its call, it works.
	if code, _ := lateRedeem(t, ccl2.Redeem, ccl2.Ticket, "s8", "toolu_8", true); code != 200 {
		t.Fatalf("with its call: %d", code)
	}
}

// lateRedeem redeems a ticket as the waiter does after an install
// (connector.installedSince): late, naming the call.
func lateRedeem(t *testing.T, url, ticket, thread, call string, late bool) (int, map[string]any) {
	t.Helper()
	in := map[string]any{"ticket": ticket, "thread": thread}
	if call != "" {
		in["call"] = call
	}
	if late {
		in["late"] = true
	}
	b, _ := json.Marshal(in)
	resp, err := http.Post(url, "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// claims0 is a beacon's iat, read without checking it (to verify it as
// of when it was minted, as the waiter does).
func claims0(tok string) int64 {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return 0
	}
	b, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var c struct {
		IssuedAt int64 `json:"iat"`
	}
	json.Unmarshal(b, &c)
	return c.IssuedAt
}
