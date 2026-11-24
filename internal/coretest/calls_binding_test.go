package coretest_test

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/wakeshift/server/internal/beacon"
	"github.com/wakeshift/server/internal/coretest"
)

// A pending-addition record is its own conversation's: /calls/addition
// answers it only for a call that names the record's session, as the
// subscribe path does (pendingFor). Another conversation of the same user
// or agent, passing it, is refused, and the record stays its own's.

// beaconThread is the thread claim of the beacon in a block's _meta (no
// signature check: the test only reads which conversation it names).
func beaconThread(t *testing.T, meta map[string]any) string {
	t.Helper()
	tok, _ := meta[beacon.MetaKey].(string)
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return ""
	}
	b, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var c beacon.Claims
	json.Unmarshal(b, &c)
	return c.Thread
}

// Two Codex threads of one user: A's record, passed with B's call, gets no
// block (no beacon for A's thread in B's result), and A can still use it.
func TestCallsAdditionRefusesAnotherConversationsRecord(t *testing.T) {
	c := coretest.Start(t)
	app := pokerApp(t, c, "poker")
	var a, b deferredOut
	if code, e := appAPI(t, c, app, "/api/apps/v1/event-subscriptions", map[string]any{"call": codexCall("u1", "thread-1", ""), "name": "game.turn",
		"arguments": map[string]any{"table": "juniper"}, "defer_addition": true}, &a); code != 200 {
		t.Fatalf("A's subscribe: %d %s", code, e)
	}
	if code, e := appAPI(t, c, app, "/api/apps/v1/event-subscriptions", map[string]any{"call": codexCall("u1", "thread-2", ""), "name": "game.turn",
		"arguments": map[string]any{"table": "oak"}, "defer_addition": true}, &b); code != 200 {
		t.Fatalf("B's subscribe: %d %s", code, e)
	}
	if a.Session == b.Session {
		t.Fatalf("one session for two threads: %s", a.Session)
	}
	if code, _ := appAPI(t, c, app, "/api/apps/v1/event-subscriptions", map[string]any{"call": codexCall("u1", "thread-2", b.Session), "name": "game.turn",
		"arguments": map[string]any{"table": "oak"}, "defer_addition": true, "pending": a.Pending}, nil); code != 400 {
		t.Fatalf("B's subscribe took A's record: %d", code)
	}
	var block additionOut
	if code, e := appAPI(t, c, app, "/api/apps/v1/calls/addition", map[string]any{"call": codexCall("u1", "thread-2", b.Session), "pending": a.Pending}, &block); code != 400 {
		t.Fatalf("B's call got A's block: %d %s, beacon for thread %q", code, e, beaconThread(t, block.Meta))
	}
	block = additionOut{}
	if code, e := appAPI(t, c, app, "/api/apps/v1/calls/addition", map[string]any{"call": codexCall("u1", "thread-1", a.Session), "pending": a.Pending}, &block); code != 200 {
		t.Fatalf("A's own block after B's refusal: %d %s", code, e)
	}
	if got := beaconThread(t, block.Meta); got != "thread-1" {
		t.Fatalf("A's block names thread %q", got)
	}
}

// An app acting for its agents: one agent's two sessions (two Codex
// threads), A's record passed with B's call, is refused too.
func TestCallsAdditionRefusesAnotherSessionsRecordForAgents(t *testing.T) {
	c := coretest.Start(t)
	app, ag := agentsApp(t, c, "chat")
	tok := agentToken(t, c, app, ag)
	call := func(thread, session string) map[string]any {
		m := map[string]any{"agent_token": tok, "client": "codex-mcp-client", "tool": "join", "meta": map[string]any{"x-codex-turn-metadata": map[string]any{"thread_id": thread}}}
		if session != "" {
			m["session"] = session
		}
		return m
	}
	var a, b deferredOut
	if code, e := appAPI(t, c, app, "/api/apps/v1/event-subscriptions", map[string]any{"call": call("t-a", ""), "name": "game.turn",
		"arguments": map[string]any{"table": "juniper"}, "defer_addition": true}, &a); code != 200 {
		t.Fatalf("A: %d %s", code, e)
	}
	if code, e := appAPI(t, c, app, "/api/apps/v1/event-subscriptions", map[string]any{"call": call("t-b", ""), "name": "game.turn",
		"arguments": map[string]any{"table": "oak"}, "defer_addition": true}, &b); code != 200 {
		t.Fatalf("B: %d %s", code, e)
	}
	var block additionOut
	if code, e := appAPI(t, c, app, "/api/apps/v1/calls/addition", map[string]any{"call": call("t-b", b.Session), "pending": a.Pending}, &block); code != 400 {
		t.Fatalf("B's call (%s) got A's block (%s): %d %s, beacon for thread %q", b.Session, a.Session, code, e, beaconThread(t, block.Meta))
	}
	if code, e := appAPI(t, c, app, "/api/apps/v1/calls/addition", map[string]any{"call": call("t-a", a.Session), "pending": a.Pending}, nil); code != 200 {
		t.Fatalf("A's own block after B's refusal: %d %s", code, e)
	}
}

// Concurrent /calls/addition requests for one armed record: one block, one
// ticket.
func TestCallsAdditionUsesARecordOnce(t *testing.T) {
	c := coretest.Start(t)
	app := pokerApp(t, c, "poker")
	var a deferredOut
	if code, e := appAPI(t, c, app, "/api/apps/v1/event-subscriptions", map[string]any{"call": codexCall("u1", "thread-1", ""), "name": "game.turn",
		"arguments": map[string]any{"table": "juniper"}, "defer_addition": true}, &a); code != 200 {
		t.Fatalf("subscribe: %d %s", code, e)
	}
	tenant := userTenant(t, c, "poker", "u1")
	const n = 8
	codes := make([]int, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i], _, errs[i] = appPost(c, app, "/api/apps/v1/calls/addition", map[string]any{"call": codexCall("u1", "thread-1", a.Session), "pending": a.Pending}, nil)
		}()
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		t.Fatal(err)
	}
	ok := 0
	for _, code := range codes {
		if code == 200 {
			ok++
		}
	}
	if ok != 1 || tickets(t, c, tenant) != 1 {
		t.Fatalf("%d blocks, %d tickets for one record (codes %v)", ok, tickets(t, c, tenant), codes)
	}
}

// A's block hands over the messages held for A's session; passed with B's
// call, A's record would have shown them in B's result and taken them from
// A.
func TestCallsAdditionKeepsAnotherConversationsMessages(t *testing.T) {
	c := coretest.Start(t)
	app := pokerApp(t, c, "poker")
	var a, b deferredOut
	if code, e := appAPI(t, c, app, "/api/apps/v1/event-subscriptions", map[string]any{"call": codexCall("u1", "thread-1", ""), "name": "game.turn",
		"arguments": map[string]any{"table": "juniper"}}, &a); code != 200 {
		t.Fatalf("A's subscribe: %d %s", code, e)
	}
	if code, e := appAPI(t, c, app, "/api/apps/v1/event-subscriptions", map[string]any{"call": codexCall("u1", "thread-2", ""), "name": "game.turn",
		"arguments": map[string]any{"table": "oak"}}, &b); code != 200 {
		t.Fatalf("B's subscribe: %d %s", code, e)
	}
	tenant := userTenant(t, c, "poker", "u1")
	if code, e, _ := emitTurns(t, c, app, "juniper", "AsKs", "for-a", []any{map[string]any{"user": "u1", "subscription": a.EventSubscription.ID}}); code != 200 {
		t.Fatalf("emit: %d %s", code, e)
	}
	waitFor(t, "A's delivery", func() bool { return len(subjects(t, c, tenant)) == 1 })
	var s struct {
		Session string `json:"session"`
		Pending string `json:"pending"`
	}
	if code, e := appAPI(t, c, app, "/api/apps/v1/calls/session", map[string]any{"call": codexCall("u1", "thread-1", a.Session)}, &s); code != 200 {
		t.Fatalf("A's calls/session: %d %s", code, e)
	}
	if code, e := appAPI(t, c, app, "/api/apps/v1/event-subscriptions", map[string]any{"call": codexCall("u1", "thread-1", a.Session), "name": "game.turn",
		"arguments": map[string]any{"table": "juniper"}, "defer_addition": true, "pending": s.Pending}, nil); code != 200 {
		t.Fatalf("A's renewal: %d %s", code, e)
	}
	var block additionOut
	code, _ := appAPI(t, c, app, "/api/apps/v1/calls/addition", map[string]any{"call": codexCall("u1", "thread-2", b.Session), "pending": s.Pending}, &block)
	if code != 400 || strings.Contains(block.Text, "AsKs") {
		t.Fatalf("B's call with A's record: %d, text %q", code, block.Text)
	}
	block = additionOut{}
	// A's own block is answered, and doesn't hand the message over either:
	// it waits for A's connector (no result delivers messages).
	if code, e := appAPI(t, c, app, "/api/apps/v1/calls/addition", map[string]any{"call": codexCall("u1", "thread-1", a.Session), "pending": s.Pending}, &block); code != 200 || strings.Contains(block.Text, "AsKs") {
		t.Fatalf("A's own block: %d %s, text %q", code, e, block.Text)
	}
}
