package coretest_test

import (
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/coretest"
	"github.com/wakeshift/server/pkg/wakeshift"
)

type statusOne struct {
	ID, User  string
	Live      bool
	Connected bool
	Via       string
}

// Event status: how many conversations follow an event with some
// arguments (a personal event's, one user's), and, for many subscriptions
// by ID at once, whether each is live. Another app's, another user's or a
// made-up ID reads as not live, never as an error; and it only reads: a
// lapsed subscription isn't ended by being asked about.
func TestEventStatus(t *testing.T) {
	eventsTestSetup(t)
	c := coretest.Start(t)
	app := pokerApp(t, c, "poker")
	other := pokerApp(t, c, "other")
	sub := func(a, user, name string, args map[string]any) eventSubResult {
		t.Helper()
		var out eventSubResult
		if code, e := appAPI(t, c, map[string]*wakeshift.Client{"poker": app, "other": other}[a], "/api/apps/v1/event-subscriptions", map[string]any{"call": map[string]any{"user": user, "client": "claude-code"},
			"name": name, "arguments": args}, &out); code != 200 {
			t.Fatalf("subscribe: %d %s", code, e)
		}
		return out
	}
	table := map[string]any{"table": "juniper"}
	u1 := sub("poker", "u1", "game.turn", table)
	u2 := sub("poker", "u2", "game.turn", table)
	sub("poker", "u1", "chat.message", map[string]any{"channel": "eng"})
	sub("poker", "u2", "chat.message", map[string]any{"channel": "eng"})
	theirs := sub("other", "u1", "game.turn", table)
	chatgpt := newFakeChatGPT(t)
	if code, e := appAPI(t, c, app, "/api/apps/v1/events/subscribe", map[string]any{"call": map[string]any{"user": "u3", "client": "openai-mcp"},
		"name": "game.turn", "arguments": table, "delivery": map[string]any{"mode": "webhook", "url": chatgpt.url(), "secret": chatgpt.secret()},
		"ttl_ms": 3600000}, nil); code != 200 {
		t.Fatalf("u3 subscribes natively: %d %s", code, e)
	}
	var listed struct{ Subscriptions []struct{ ID string } }
	appAPI(t, c, app, "/api/apps/v1/event-subscriptions/list", map[string]any{"user": "u3"}, &listed)
	if len(listed.Subscriptions) != 1 {
		t.Fatalf("u3's subscriptions: %+v", listed)
	}
	native := listed.Subscriptions[0].ID

	// By arguments.
	var counts struct {
		Subscriptions, Connected int
		Topic                    string
	}
	if code, e := appAPI(t, c, app, "/api/apps/v1/events/status", map[string]any{"name": "chat.message", "arguments": map[string]any{"channel": "eng"}}, &counts); code != 200 || counts.Subscriptions != 2 {
		t.Fatalf("a shared event's: %d %s %+v", code, e, counts)
	}
	if code, _ := appAPI(t, c, app, "/api/apps/v1/events/status", map[string]any{"name": "game.turn", "arguments": table, "to": "u1"}, &counts); code != 200 || counts.Subscriptions != 1 || counts.Topic != "" {
		t.Fatalf("u1's personal: %d %+v", code, counts)
	}
	if code, e := appAPI(t, c, app, "/api/apps/v1/events/status", map[string]any{"name": "game.turn", "arguments": table}, nil); code != 400 || e != "bad_arguments" {
		t.Fatalf("a personal event without to: %d %s", code, e)
	}

	// By ID. u2's subscription is ended first.
	if code, _ := appAPI(t, c, app, "/api/apps/v1/event-subscriptions/end", map[string]any{"user": "u2", "id": u2.EventSubscription.ID}, nil); code != 200 {
		t.Fatal("ending u2's")
	}
	refs := []map[string]string{
		{"user": "u1", "id": u1.EventSubscription.ID},     // live
		{"user": "u2", "id": u1.EventSubscription.ID},     // u1's, asked as u2's
		{"user": "u2", "id": u2.EventSubscription.ID},     // ended
		{"user": "u1", "id": theirs.EventSubscription.ID}, // another app's
		{"user": "u3", "id": native},                      // a native one
		{"user": "u1", "id": "not-an-id"},
		{"user": "u9", "id": "00000000-0000-4000-8000-000000000000"},
	}
	var out struct{ Subscriptions []statusOne }
	if code, e := appAPI(t, c, app, "/api/apps/v1/events/status", map[string]any{"name": "game.turn", "subscriptions": refs}, &out); code != 200 || len(out.Subscriptions) != len(refs) {
		t.Fatalf("by ID: %d %s %+v", code, e, out)
	}
	want := []bool{true, false, false, false, true, false, false}
	for i, s := range out.Subscriptions {
		if s.Live != want[i] || s.ID != refs[i]["id"] || s.User != refs[i]["user"] {
			t.Errorf("%d: %+v, want live %v", i, s, want[i])
		}
	}
	// Asked about as another event: not live.
	appAPI(t, c, app, "/api/apps/v1/events/status", map[string]any{"name": "chat.message", "subscriptions": refs[:1]}, &out)
	if len(out.Subscriptions) != 1 || out.Subscriptions[0].Live {
		t.Fatalf("as another event: %+v", out)
	}

	// Only reads: a lapsed subscription reads as not live, and stays as it was.
	u1T := userTenant(t, c, "poker", "u1")
	if err := c.DB.InTenant(ctx, u1T, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE topic_subscriptions SET expires_at = now() - interval '1 minute'
			WHERE subscription_id = (SELECT subscription_id FROM event_subscriptions WHERE id = $1::uuid)`, u1.EventSubscription.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	out.Subscriptions = nil
	if code, e := appAPI(t, c, app, "/api/apps/v1/events/status", map[string]any{"name": "game.turn", "subscriptions": refs[:1]}, &out); code != 200 || len(out.Subscriptions) != 1 {
		t.Fatalf("status of a lapsed subscription: %d %s %+v", code, e, out)
	}
	if out.Subscriptions[0].Live {
		t.Fatal("a lapsed subscription reads as live")
	}
	if n := count(t, c, u1T, `SELECT count(*) FROM topic_subscriptions WHERE ended_at IS NOT NULL`); n != 0 {
		t.Fatalf("status ended %d subscriptions", n)
	}

	// Refused: too many, none, both forms, a user that isn't one.
	many := make([]map[string]string, 1001)
	for i := range many {
		many[i] = map[string]string{"user": fmt.Sprintf("u%d", i), "id": u1.EventSubscription.ID}
	}
	for name, body := range map[string]map[string]any{
		"too many":   {"name": "game.turn", "subscriptions": many},
		"none":       {"name": "game.turn", "subscriptions": []any{}},
		"both forms": {"name": "game.turn", "arguments": table, "subscriptions": refs[:1]},
		"no user":    {"name": "game.turn", "subscriptions": []map[string]string{{"id": u1.EventSubscription.ID}}},
	} {
		if code, _ := appAPI(t, c, app, "/api/apps/v1/events/status", body, nil); code != 400 {
			t.Errorf("%s: %d", name, code)
		}
	}
}
