package coretest_test

import (
	"testing"

	"github.com/wakeshift/server/internal/coretest"
)

// Two tool calls on one conversation subscribe to the same event with the
// same arguments: call A creates the subscription, call B (in parallel, or
// while A's tool is still running) renews it and succeeds, its block
// telling the agent it's woken. A's tool then fails, and its rollback must
// not end the subscription B's result relies on.
func TestRollbackSparesAnotherCallsRenewal(t *testing.T) {
	c := coretest.Start(t)
	app := pokerApp(t, c, "poker")
	args := map[string]any{"table": "juniper"}
	var a deferredOut
	if code, e := appAPI(t, c, app, "/api/apps/v1/event-subscriptions", map[string]any{"call": map[string]any{"user": "u1", "client": "claude-code"},
		"name": "game.turn", "arguments": args, "defer_addition": true}, &a); code != 200 {
		t.Fatalf("call A subscribes: %d %s", code, e)
	}
	call := map[string]any{"user": "u1", "client": "claude-code", "session": a.Session}
	var b struct {
		EventSubscription struct{ Created bool } `json:"event_subscription"`
		Pending           string                 `json:"pending"`
	}
	if code, e := appAPI(t, c, app, "/api/apps/v1/event-subscriptions", map[string]any{"call": call,
		"name": "game.turn", "arguments": args, "defer_addition": true}, &b); code != 200 || b.EventSubscription.Created {
		t.Fatalf("call B renews: %d %s %+v", code, e, b)
	}
	if code, e := appAPI(t, c, app, "/api/apps/v1/calls/addition", map[string]any{"call": call, "pending": b.Pending}, nil); code != 200 {
		t.Fatalf("call B's block: %d %s", code, e)
	}
	// A's tool fails: its wrapper rolls back what A created.
	var ended struct{ Ended bool }
	if code, e := appAPI(t, c, app, "/api/apps/v1/event-subscriptions/unsubscribe", map[string]any{"call": call,
		"name": "game.turn", "arguments": args, "reason": "rolled_back", "pending": a.Pending}, &ended); code != 200 || ended.Ended {
		t.Fatalf("A's rollback: %d %s ended=%v", code, e, ended.Ended)
	}
	tenant := userTenant(t, c, "poker", "u1")
	if n := count(t, c, tenant, `SELECT count(*) FROM event_subscriptions WHERE ended_at IS NULL`); n != 1 {
		t.Fatalf("call B succeeded and told the agent it's subscribed, but A's rollback ended the subscription (%d live)", n)
	}
}

// A call that subscribes to the same thing twice, then fails, still rolls
// back what it made: its own renewal doesn't protect it.
func TestRollbackEndsItsOwnRenewal(t *testing.T) {
	c := coretest.Start(t)
	app := pokerApp(t, c, "poker")
	args := map[string]any{"table": "juniper"}
	var a deferredOut
	if code, e := appAPI(t, c, app, "/api/apps/v1/event-subscriptions", map[string]any{"call": map[string]any{"user": "u1", "client": "claude-code"},
		"name": "game.turn", "arguments": args, "defer_addition": true}, &a); code != 200 {
		t.Fatalf("subscribes: %d %s", code, e)
	}
	call := map[string]any{"user": "u1", "client": "claude-code", "session": a.Session}
	if code, e := appAPI(t, c, app, "/api/apps/v1/event-subscriptions", map[string]any{"call": call,
		"name": "game.turn", "arguments": args, "pending": a.Pending}, nil); code != 200 {
		t.Fatalf("subscribes again in the same call: %d %s", code, e)
	}
	var ended struct{ Ended bool }
	if code, e := appAPI(t, c, app, "/api/apps/v1/event-subscriptions/unsubscribe", map[string]any{"call": call,
		"name": "game.turn", "arguments": args, "reason": "rolled_back", "pending": a.Pending}, &ended); code != 200 || !ended.Ended {
		t.Fatalf("rollback: %d %s ended=%v", code, e, ended.Ended)
	}
}
