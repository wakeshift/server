package coretest_test

import (
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/appapi"
	"github.com/wakeshift/server/internal/apps"
	"github.com/wakeshift/server/internal/beacon"
	"github.com/wakeshift/server/internal/coretest"
	"github.com/wakeshift/server/pkg/wakeshift"
)

// One block per tool call (the pending addition): deferred subscribes,
// /calls/session and /calls/addition.

// deferredOut is a deferred subscribe's response.
type deferredOut struct {
	EventSubscription struct {
		ID   string `json:"id"`
		User string `json:"user"`
	} `json:"event_subscription"`
	Session  string `json:"session"`
	Pending  string `json:"pending"`
	Addition struct {
		Text string         `json:"text"`
		Meta map[string]any `json:"meta"`
	} `json:"addition"`
}

// additionOut is /calls/addition's response.
type additionOut struct {
	Text    string         `json:"text"`
	Trailer string         `json:"trailer"`
	Meta    map[string]any `json:"meta"`
}

// codexCall is a Codex thread's tool call for user, to tool.
func codexCall(user, thread, session string) map[string]any {
	call := map[string]any{"user": user, "client": "codex-mcp-client", "tool": "join", "meta": map[string]any{"x-codex-turn-metadata": map[string]any{"thread_id": thread}}}
	if session != "" {
		call["session"] = session
	}
	return call
}

func tickets(t *testing.T, c *coretest.Core, tenant string) int {
	t.Helper()
	return count(t, c, tenant, `SELECT count(*) FROM connector_tickets`)
}

// Two deferred subscribes in one call write no instructions and mint no
// beacon; /calls/addition then gives the call's one block: the full
// instructions the first time, with one beacon and the notes; a later
// call's block is the short form. A record is read once.
func TestCallsOneBlockPerCall(t *testing.T) {
	c := coretest.Start(t)
	app := pokerApp(t, c, "poker")
	sub := func(body map[string]any) (int, string, deferredOut) {
		var out deferredOut
		code, e := appAPI(t, c, app, "/api/apps/v1/event-subscriptions", body, &out)
		return code, e, out
	}
	call := codexCall("u1", "thread-1", "")
	code, e, first := sub(map[string]any{"call": call, "name": "game.turn", "arguments": map[string]any{"table": "juniper"}, "defer_addition": true, "reason": "Playing at juniper"})
	if code != 200 || first.Pending == "" || first.Addition.Text != "" || first.Addition.Meta != nil || first.EventSubscription.User != "u1" {
		t.Fatalf("first deferred subscribe: %d %s %+v", code, e, first)
	}
	tenant := userTenant(t, c, "poker", "u1")
	code, e, second := sub(map[string]any{"call": codexCall("u1", "thread-1", first.Session), "name": "game.turn", "arguments": map[string]any{"table": "oak"},
		"defer_addition": true, "pending": first.Pending, "once": true})
	if code != 200 || second.Pending != first.Pending || second.Addition.Text != "" {
		t.Fatalf("second deferred subscribe: %d %s %+v", code, e, second)
	}
	if n := tickets(t, c, tenant); n != 0 {
		t.Fatalf("deferred subscribes minted %d tickets", n)
	}
	var block additionOut
	if code, e := appAPI(t, c, app, "/api/apps/v1/calls/addition", map[string]any{"call": codexCall("u1", "thread-1", first.Session), "pending": first.Pending}, &block); code != 200 {
		t.Fatalf("addition: %d %s", code, e)
	}
	if block.Meta[beacon.MetaKey] == nil || tickets(t, c, tenant) != 1 {
		t.Fatalf("one beacon: meta %v, %d tickets", block.Meta, tickets(t, c, tenant))
	}
	// The action first, the once note last (in testing, Codex quoted a
	// block that opened with it as if that were all).
	if !strings.HasPrefix(block.Text, "Wakeshift can't wake this conversation yet. Ask the user now") || !strings.HasSuffix(block.Text, "This subscription ends after its first message.") ||
		strings.Contains(block.Text, "an earlier result of this app's tools") {
		t.Fatalf("the first block: %q", block.Text)
	}
	// Read once.
	if code, e := appAPI(t, c, app, "/api/apps/v1/calls/addition", map[string]any{"call": codexCall("u1", "thread-1", first.Session), "pending": first.Pending}, nil); code != 400 {
		t.Fatalf("a used record: %d %s", code, e)
	}
	// The next call: the short form.
	code, _, again := sub(map[string]any{"call": codexCall("u1", "thread-1", first.Session), "name": "game.turn", "arguments": map[string]any{"table": "juniper"}, "defer_addition": true})
	if code != 200 || again.Pending == first.Pending {
		t.Fatalf("the next call's subscribe: %d %+v", code, again)
	}
	if code, e := appAPI(t, c, app, "/api/apps/v1/calls/addition", map[string]any{"call": codexCall("u1", "thread-1", first.Session), "pending": again.Pending}, &block); code != 200 {
		t.Fatalf("addition: %d %s", code, e)
	}
	if !strings.HasPrefix(block.Text, "Subscribed, but nothing wakes this conversation until") || !strings.Contains(block.Text, "as an earlier result of this app's tools says") || strings.Contains(block.Text, "codex.sh") {
		t.Fatalf("the later block isn't the short form: %q", block.Text)
	}
	// The reason is listed.
	var list struct {
		Subscriptions []struct {
			Event  string `json:"event"`
			Reason string `json:"reason"`
		} `json:"subscriptions"`
	}
	appAPI(t, c, app, "/api/apps/v1/event-subscriptions/list", map[string]any{"user": "u1"}, &list)
	found := false
	for _, s := range list.Subscriptions {
		found = found || s.Reason == "Playing at juniper"
	}
	if !found {
		t.Fatalf("the reason isn't listed: %+v", list)
	}
}

// /calls/addition refuses a record that isn't the call's: another app's,
// another conversation's, an expired one, or a made-up one; and mints
// nothing for it.
func TestCallsAdditionRefusesOthersRecords(t *testing.T) {
	c := coretest.Start(t)
	app := pokerApp(t, c, "poker")
	other := pokerApp(t, c, "other")
	var out deferredOut
	if code, e := appAPI(t, c, app, "/api/apps/v1/event-subscriptions", map[string]any{"call": codexCall("u1", "thread-1", ""), "name": "game.turn",
		"arguments": map[string]any{"table": "juniper"}, "defer_addition": true}, &out); code != 200 {
		t.Fatalf("subscribe: %d %s", code, e)
	}
	// u2's own conversation, in its own workspace.
	var u2 deferredOut
	appAPI(t, c, app, "/api/apps/v1/event-subscriptions", map[string]any{"call": codexCall("u2", "thread-2", ""), "name": "game.turn",
		"arguments": map[string]any{"table": "juniper"}, "defer_addition": true}, &u2)
	tenant := userTenant(t, c, "poker", "u1")
	addition := func(a *wakeshift.Client, call map[string]any, pending string) (int, string) {
		return appAPI(t, c, a, "/api/apps/v1/calls/addition", map[string]any{"call": call, "pending": pending}, nil)
	}
	for name, try := range map[string]func() (int, string){
		"another app":         func() (int, string) { return addition(other, codexCall("u1", "thread-1", out.Session), out.Pending) },
		"another user's call": func() (int, string) { return addition(app, codexCall("u2", "thread-2", u2.Session), out.Pending) },
		"another client": func() (int, string) {
			return addition(app, map[string]any{"user": "u1", "client": "claude-code", "session": out.Session}, out.Pending)
		},
		"made up": func() (int, string) {
			return addition(app, codexCall("u1", "thread-1", out.Session), "pa_AAAAAAAAAAAAAAAAAAAAAA")
		},
		"not a record ID":      func() (int, string) { return addition(app, codexCall("u1", "thread-1", out.Session), "ts_x") },
		"a subscribe's record": func() (int, string) { return addition(app, codexCall("u1", "thread-1", out.Session), u2.Pending) },
	} {
		if code, e := try(); code != 400 || e != "bad_request" {
			t.Errorf("%s: %d %s", name, code, e)
		}
	}
	// Expired.
	if err := c.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE pending_additions SET expires_at = now() - interval '1 second'`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if code, e := addition(app, codexCall("u1", "thread-1", out.Session), out.Pending); code != 400 {
		t.Fatalf("an expired record: %d %s", code, e)
	}
	if n := tickets(t, c, tenant); n != 0 {
		t.Fatalf("%d tickets minted for refused records", n)
	}
	// A pending record of another conversation can't be passed to a
	// subscribe either.
	if code, e := appAPI(t, c, app, "/api/apps/v1/event-subscriptions", map[string]any{"call": codexCall("u1", "thread-1", out.Session), "name": "game.turn",
		"arguments": map[string]any{"table": "oak"}, "pending": u2.Pending}, nil); code != 400 {
		t.Fatalf("a subscribe with another conversation's record: %d %s", code, e)
	}
	// The expiry pass prunes a record an hour past its expiry, with its ID.
	if err := c.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE pending_additions SET expires_at = now() - interval '2 hours'`); err != nil {
			return err
		}
		n, err := appapi.PrunePendingAdditions(ctx, tx, 100)
		if err == nil && n != 1 {
			t.Errorf("pruned %d", n)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var left int
	c.DB.Pool.QueryRow(ctx, `SELECT count(*) FROM pending_addition_ids WHERE id = $1`, out.Pending).Scan(&left)
	if left != 0 {
		t.Fatal("the record's ID outlived it")
	}
}

// /calls/session finds or starts the conversation's session and makes a
// record, without marking it watching; its block, unarmed, is what any
// other result carries (nothing for a session that isn't watching).
func TestCallsSession(t *testing.T) {
	c := coretest.Start(t)
	app := pokerApp(t, c, "poker")
	var s struct {
		Session string `json:"session"`
		Pending string `json:"pending"`
	}
	if code, e := appAPI(t, c, app, "/api/apps/v1/calls/session", map[string]any{"call": codexCall("u1", "thread-1", "")}, &s); code != 200 || s.Pending == "" || s.Session == "" {
		t.Fatalf("session: %d %s %+v", code, e, s)
	}
	tenant := userTenant(t, c, "poker", "u1")
	if n := count(t, c, tenant, `SELECT count(*) FROM sessions WHERE watching`); n != 0 {
		t.Fatalf("%d sessions watching", n)
	}
	var block additionOut
	if code, e := appAPI(t, c, app, "/api/apps/v1/calls/addition", map[string]any{"call": codexCall("u1", "thread-1", s.Session), "pending": s.Pending}, &block); code != 200 {
		t.Fatalf("addition: %d %s", code, e)
	}
	if block.Text != "" || block.Meta != nil || tickets(t, c, tenant) != 0 {
		t.Fatalf("an unarmed record's block: %+v", block)
	}
	// A used record can't be passed to a later subscribe.
	var sub deferredOut
	if code, e := appAPI(t, c, app, "/api/apps/v1/event-subscriptions", map[string]any{"call": codexCall("u1", "thread-1", s.Session), "name": "game.turn",
		"arguments": map[string]any{"table": "juniper"}, "pending": s.Pending}, &sub); code != 400 {
		t.Fatalf("a used record passed to a subscribe: %d %s", code, e)
	}
	// A subscribe in the same call as /calls/session arms its record: the
	// call's block is the subscribe's.
	var s2 struct {
		Session string `json:"session"`
		Pending string `json:"pending"`
	}
	if code, e := appAPI(t, c, app, "/api/apps/v1/calls/session", map[string]any{"call": codexCall("u1", "thread-2", "")}, &s2); code != 200 || s2.Pending == "" {
		t.Fatalf("second session: %d %s %+v", code, e, s2)
	}
	if code, e := appAPI(t, c, app, "/api/apps/v1/event-subscriptions", map[string]any{"call": codexCall("u1", "thread-2", s2.Session), "name": "game.turn",
		"arguments": map[string]any{"table": "juniper"}, "defer_addition": true, "pending": s2.Pending}, &sub); code != 200 {
		t.Fatalf("a subscribe with the session's record: %d %s", code, e)
	}
	before := tickets(t, c, tenant)
	if code, e := appAPI(t, c, app, "/api/apps/v1/calls/addition", map[string]any{"call": codexCall("u1", "thread-2", s2.Session), "pending": s2.Pending}, &block); code != 200 || block.Text == "" || tickets(t, c, tenant) != before+1 {
		t.Fatalf("an armed record's block: %d %s %+v", code, e, block)
	}
	// With no pending: the annotate addition, nothing made.
	if code, e := appAPI(t, c, app, "/api/apps/v1/calls/addition", map[string]any{"call": codexCall("u9", "thread-9", "")}, &block); code != 200 || block.Text != "" {
		t.Fatalf("no pending, no conversation: %d %s %+v", code, e, block)
	}
}

// A subscription a failed tool call made is ended with reason rolled_back:
// audited as such, with no notice to the conversation. Any other reason is
// refused.
func TestRollbackUnsubscribe(t *testing.T) {
	c := coretest.Start(t)
	app := pokerApp(t, c, "poker")
	var sub deferredOut
	if code, e := appAPI(t, c, app, "/api/apps/v1/event-subscriptions", map[string]any{"call": map[string]any{"user": "u1", "client": "claude-code"},
		"name": "game.turn", "arguments": map[string]any{"table": "juniper"}, "defer_addition": true}, &sub); code != 200 {
		t.Fatalf("subscribe: %d %s", code, e)
	}
	unsub := func(reason string) (int, string, bool) {
		var out struct{ Ended bool }
		code, e := appAPI(t, c, app, "/api/apps/v1/event-subscriptions/unsubscribe", map[string]any{"call": map[string]any{"user": "u1", "client": "claude-code", "session": sub.Session},
			"name": "game.turn", "arguments": map[string]any{"table": "juniper"}, "reason": reason}, &out)
		return code, e, out.Ended
	}
	if code, e, _ := unsub("because"); code != 400 {
		t.Fatalf("another reason: %d %s", code, e)
	}
	if code, e, ended := unsub("rolled_back"); code != 200 || !ended {
		t.Fatalf("rollback: %d %s %v", code, e, ended)
	}
	tenant := userTenant(t, c, "poker", "u1")
	if got := auditActions(t, c, tenant); !contains(got, "event_subscription.rolled_back") {
		t.Fatalf("audit %v", got)
	}
	if n := count(t, c, tenant, `SELECT count(*) FROM event_subscriptions WHERE ended_reason = 'rolled_back'`); n != 1 {
		t.Fatalf("%d rolled back", n)
	}
	time.Sleep(200 * time.Millisecond)
	if got := subjects(t, c, tenant); len(got) != 0 {
		t.Fatalf("the conversation was told: %q", got)
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// agentsApp is an app acting for its agents (an agent scope, no groups)
// with turnEvents, and one agent in a workspace it keeps for "ada".
func agentsApp(t *testing.T, c *coretest.Core, id string) (*wakeshift.Client, *wakeshift.Agent) {
	t.Helper()
	app := bareApp(t, c, id)
	m := turnEvents()
	m.Description = "Agents."
	m.Agent = &apps.AgentScope{TTLDays: 30}
	if err := c.Apps.SetManifest(ctx, id, m); err != nil {
		t.Fatal(err)
	}
	return app, actAs(t, app, "ada", "codex")
}

// With an agent token, an event subscription is the agent's (its user is
// <workspace>/<agent>), in the workspace the app keeps for it, with the
// session found or started as /agent/watch does: and a multi-recipient
// emit reaches it as {user, subscription}, found through that workspace.
func TestAgentTokenEventSubscribe(t *testing.T) {
	c := coretest.Start(t)
	app, ag := agentsApp(t, c, "chat")
	tok := agentToken(t, c, app, ag)
	call := map[string]any{"agent_token": tok, "client": "codex-mcp-client", "tool": "watch", "meta": map[string]any{"x-codex-turn-metadata": map[string]any{"thread_id": "agent-thread"}}}
	var sub deferredOut
	if code, e := appAPI(t, c, app, "/api/apps/v1/event-subscriptions", map[string]any{"call": call, "name": "game.turn",
		"arguments": map[string]any{"table": "juniper"}, "label": "main", "defer_addition": true}, &sub); code != 200 {
		t.Fatalf("subscribe: %d %s", code, e)
	}
	if sub.EventSubscription.User != ag.User() || !strings.HasSuffix(sub.Session, "@main") {
		t.Fatalf("subscription %+v (want user %s, a session labeled main)", sub, ag.User())
	}
	// Again with its handle: the same session, a renewal.
	call["session"] = sub.Session
	var again deferredOut
	appAPI(t, c, app, "/api/apps/v1/event-subscriptions", map[string]any{"call": call, "name": "game.turn", "arguments": map[string]any{"table": "juniper"}}, &again)
	if again.Session != sub.Session || again.EventSubscription.ID != sub.EventSubscription.ID {
		t.Fatalf("again: %+v", again)
	}
	// Its block, for the agent's call.
	var block additionOut
	if code, e := appAPI(t, c, app, "/api/apps/v1/calls/addition", map[string]any{"call": call, "pending": sub.Pending}, &block); code != 200 || block.Meta[beacon.MetaKey] == nil {
		t.Fatalf("addition: %d %s %+v", code, e, block)
	}
	// A user other than the token's is refused, and so is a bad token.
	if code, e := appAPI(t, c, app, "/api/apps/v1/event-subscriptions", map[string]any{"call": map[string]any{"agent_token": tok, "user": "someone"}, "name": "game.turn",
		"arguments": map[string]any{"table": "juniper"}}, nil); code != 400 {
		t.Fatalf("another user: %d %s", code, e)
	}
	other, _ := agentsApp(t, c, "other")
	if code, e := appAPI(t, c, other, "/api/apps/v1/event-subscriptions", map[string]any{"call": map[string]any{"agent_token": tok}, "name": "game.turn",
		"arguments": map[string]any{"table": "juniper"}}, nil); code != 403 || e != "bad_agent_token" {
		t.Fatalf("another app's token: %d %s", code, e)
	}
	// The emit to it, as {user, subscription}.
	code, e, out := emitTurns(t, c, app, "juniper", "As", "to-agent", []any{map[string]any{"user": ag.User(), "subscription": sub.EventSubscription.ID}})
	if code != 200 || out.Matched != 1 || len(out.Ended) != 0 {
		t.Fatalf("emit to the agent: %d %s %+v", code, e, out)
	}
	waitFor(t, "the agent's turn", func() bool { return len(subjects(t, c, ag.Workspace)) == 1 })
	// Rolled back, as the agent.
	var ended struct{ Ended bool }
	if code, e := appAPI(t, c, app, "/api/apps/v1/event-subscriptions/unsubscribe", map[string]any{"call": call, "name": "game.turn",
		"arguments": map[string]any{"table": "juniper"}, "reason": "rolled_back"}, &ended); code != 200 || !ended.Ended {
		t.Fatalf("rollback: %d %s %+v", code, e, ended)
	}
}

// An app acting for its agents (Agent Chat, the games) has its agents told
// to pass their session_key, as every app's are: a subscribe's block says
// so, with the session's handle, except to a Codex thread, which its
// harness names itself.
func TestAgentCallsAreToldTheirSessionKey(t *testing.T) {
	c := coretest.Start(t)
	app, ag := agentsApp(t, c, "chat")
	tok := agentToken(t, c, app, ag)
	block := func(call map[string]any) (string, string) {
		t.Helper()
		var sub deferredOut
		if code, e := appAPI(t, c, app, "/api/apps/v1/event-subscriptions", map[string]any{"call": call, "name": "game.turn",
			"arguments": map[string]any{"table": "juniper"}, "defer_addition": true}, &sub); code != 200 {
			t.Fatalf("subscribe: %d %s", code, e)
		}
		// The addition names the session the subscribe found, as the SDKs'
		// wrappers do.
		named := maps.Clone(call)
		named["session"] = sub.Session
		var out additionOut
		if code, e := appAPI(t, c, app, "/api/apps/v1/calls/addition", map[string]any{"call": named, "pending": sub.Pending}, &out); code != 200 {
			t.Fatalf("addition: %d %s", code, e)
		}
		return out.Text, sub.Session
	}
	text, session := block(map[string]any{"agent_token": tok, "client": "claude-code", "harness": "claude-code", "tool": "join"})
	if session == "" || !strings.Contains(text, `Pass session_key="`+session+`"`) {
		t.Fatalf("Claude Code's block doesn't pass its session_key (%s): %q", session, text)
	}
	text, _ = block(map[string]any{"agent_token": tok, "client": "codex-mcp-client", "tool": "join", "meta": map[string]any{"x-codex-turn-metadata": map[string]any{"thread_id": "agent-thread"}}})
	if strings.Contains(text, "Pass session_key") {
		t.Fatalf("a Codex thread told to pass its session_key: %q", text)
	}
}

// A later call of an app acting for its agents that nothing recognizes (a
// stateless server sees no client name; OpenClaw's own runtime sends no
// _meta or User-Agent of its own) goes by the harness the agent said when
// it subscribed, as /agent/annotate does: its block carries a fresh beacon
// for that harness once the last went unredeemed, with or without a
// pending record (/calls/session).
func TestCallsAdditionUsesTheRecordedHarness(t *testing.T) {
	c := coretest.Start(t)
	app, ag := agentsApp(t, c, "chat")
	tok := agentToken(t, c, app, ag)
	watch := map[string]any{"agent_token": tok, "harness": "openclaw", "tool": "watch"}
	var sub deferredOut
	if code, e := appAPI(t, c, app, "/api/apps/v1/event-subscriptions", map[string]any{"call": watch, "name": "game.turn",
		"arguments": map[string]any{"table": "juniper"}, "defer_addition": true}, &sub); code != 200 {
		t.Fatalf("subscribe: %d %s", code, e)
	}
	named := maps.Clone(watch)
	named["session"] = sub.Session
	if code, e := appAPI(t, c, app, "/api/apps/v1/calls/addition", map[string]any{"call": named, "pending": sub.Pending}, nil); code != 200 {
		t.Fatalf("watch's block: %d %s", code, e)
	}
	age := func() {
		t.Helper()
		if err := c.DB.InTenant(ctx, ag.Workspace, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE connector_tickets SET created_at = created_at - interval '10 minutes'`)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	later := map[string]any{"agent_token": tok, "session": sub.Session, "tool": "join"}
	age()
	var resolved deferredOut
	if code, e := appAPI(t, c, app, "/api/apps/v1/calls/session", map[string]any{"call": later}, &resolved); code != 200 || resolved.Session != sub.Session {
		t.Fatalf("calls/session: %d %s %+v", code, e, resolved)
	}
	var block additionOut
	if code, e := appAPI(t, c, app, "/api/apps/v1/calls/addition", map[string]any{"call": later, "pending": resolved.Pending}, &block); code != 200 || block.Trailer == "" {
		t.Fatalf("a later call's block, with a record: %d %s %+v", code, e, block)
	}
	age()
	block = additionOut{}
	if code, e := appAPI(t, c, app, "/api/apps/v1/calls/addition", map[string]any{"call": later}, &block); code != 200 || block.Trailer == "" {
		t.Fatalf("a later call's block, without one: %d %s %+v", code, e, block)
	}
}

// A pending-addition record's ID goes when its user is forgotten.
func TestPendingAdditionIDsGoWithTheWorkspace(t *testing.T) {
	c := coretest.Start(t)
	app := pokerApp(t, c, "poker")
	var s struct {
		Pending string `json:"pending"`
	}
	if code, e := appAPI(t, c, app, "/api/apps/v1/calls/session", map[string]any{"call": map[string]any{"user": "u1", "client": "claude-code"}}, &s); code != 200 {
		t.Fatalf("session: %d %s", code, e)
	}
	if _, err := app.ForgetUser(ctx, "u1"); err != nil {
		t.Fatal(err)
	}
	var left int
	c.DB.Pool.QueryRow(ctx, `SELECT count(*) FROM pending_addition_ids WHERE id = $1`, s.Pending).Scan(&left)
	if left != 0 {
		t.Fatal("the record's ID outlived its workspace")
	}
}
