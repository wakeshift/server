package coretest_test

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wakeshift/server/internal/coretest"
	"github.com/wakeshift/server/pkg/wakeshift"
)

// An emit's id is an idempotency key: a retry of the same emit (event,
// arguments, recipients and payload) returns the first's result as a
// duplicate and delivers nothing new; the id with a different emit is
// refused (409 id_reused), never silently dropped.
func TestEmitIDReplayAndReuse(t *testing.T) {
	c := coretest.Start(t)
	app := pokerApp(t, c, "poker")
	subscribeTurns(t, c, app, "u1", "juniper", nil)
	tenant := userTenant(t, c, "poker", "u1")

	code, e, first := emitTurns(t, c, app, "juniper", "As", "r1", "u1")
	if code != 200 || first.Matched != 1 || first.Duplicate || len(first.Unmatched) != 0 {
		t.Fatalf("first emit: %d %s %+v", code, e, first)
	}
	// The same emit, its one user as a string or a list of one: a replay.
	for _, to := range []any{"u1", []any{"u1"}, []any{" u1 "}} {
		code, e, dup := emitTurns(t, c, app, "juniper", "As", "r1", to)
		if code != 200 || !dup.Duplicate || dup.Matched != 1 || len(dup.Unmatched) != 0 || len(dup.Ended) != 0 {
			t.Fatalf("retry to %v: %d %s %+v", to, code, e, dup)
		}
	}
	// The same payload with its keys in another order is the same emit.
	var dup multiOut
	if code, e := appAPI(t, c, app, "/api/apps/v1/events/emit", json.RawMessage(`{"to":"u1","id":"r1","text":"Your move, cards As","payload":{"turn":"t-As","hole":"As"},
		"arguments":{"table":"juniper"},"name":"game.turn"}`), &dup); code != 200 || !dup.Duplicate {
		t.Fatalf("retry with reordered keys: %d %s %+v", code, e, dup)
	}

	// The same payload with another text (an HTTP caller's own): another
	// emit, refused.
	if code, e, _ := appAPIEmit(t, c, app, "Your move, cards As (again)"); code != 409 || e != "id_reused" {
		t.Fatalf("the id with another text: %d %s, want 409 id_reused", code, e)
	}
	// The id with another payload, recipient, recipient list or arguments:
	// refused.
	for name, emit := range map[string]func() (int, string, multiOut){
		"payload":    func() (int, string, multiOut) { return emitTurns(t, c, app, "juniper", "Kd", "r1", "u1") },
		"recipient":  func() (int, string, multiOut) { return emitTurns(t, c, app, "juniper", "As", "r1", "u2") },
		"recipients": func() (int, string, multiOut) { return emitTurns(t, c, app, "juniper", "As", "r1", []any{"u1", "u2"}) },
		"arguments":  func() (int, string, multiOut) { return emitTurns(t, c, app, "oak", "As", "r1", "u1") },
	} {
		if code, e, _ := emit(); code != 409 || e != "id_reused" {
			t.Fatalf("the id with another %s: %d %s, want 409 id_reused", name, code, e)
		}
	}
	// The message names the event.
	var refused struct {
		Error struct{ Code, Message string } `json:"error"`
	}
	body := map[string]any{"name": "game.turn", "arguments": map[string]any{"table": "juniper"}, "payload": map[string]any{"hole": "Kd", "turn": "t-Kd"},
		"text": "x", "to": "u1", "id": "r1"}
	code, raw, err := appPostRaw(c, app, "/api/apps/v1/events/emit", body)
	if err != nil || code != 409 || json.Unmarshal(raw, &refused) != nil {
		t.Fatalf("refused emit: %d %v", code, err)
	}
	if refused.Error.Code != "id_reused" || !strings.Contains(refused.Error.Message, "game.turn") || !strings.Contains(refused.Error.Message, "already used") || !strings.Contains(refused.Error.Message, "different emit") {
		t.Fatalf("refusal: %+v", refused.Error)
	}

	time.Sleep(300 * time.Millisecond)
	if got := subjects(t, c, tenant); len(got) != 1 {
		t.Fatalf("u1 got %d messages, want the first emit's alone", len(got))
	}

	// A record without a text fingerprint (written by the revision before
	// it) matches a retry's text, still checking the rest.
	if _, err := c.DB.Pool.Exec(ctx, `UPDATE topic_events SET text_fingerprint = NULL WHERE app_id = 'poker' AND source_id = 'r1'`); err != nil {
		t.Fatal(err)
	}
	if code, e, dup := appAPIEmit(t, c, app, "Your move, cards As (again)"); code != 200 || !dup.Duplicate {
		t.Fatalf("another text on a record without a text fingerprint: %d %s %+v", code, e, dup)
	}
	if code, e, _ := emitTurns(t, c, app, "juniper", "Kd", "r1", "u1"); code != 409 || e != "id_reused" {
		t.Fatalf("another payload on a record without a text fingerprint: %d %s", code, e)
	}

	// A record without a fingerprint (written before fingerprints, or
	// redacted) matches any retry, as every repeat did before.
	if _, err := c.DB.Pool.Exec(ctx, `UPDATE topic_events SET fingerprint = NULL WHERE app_id = 'poker' AND source_id = 'r1'`); err != nil {
		t.Fatal(err)
	}
	if code, e, dup := emitTurns(t, c, app, "juniper", "Kd", "r1", "u2"); code != 200 || !dup.Duplicate || dup.Matched != 1 {
		t.Fatalf("a retry of a record without a fingerprint: %d %s %+v", code, e, dup)
	}

	// A multi-recipient emit's id: the same, with its list in any order.
	if code, e, _ := emitTurns(t, c, app, "juniper", "Qh", "r2", []any{"u1", "u2"}); code != 200 {
		t.Fatalf("multi emit: %d %s", code, e)
	}
	if code, e, dup := emitTurns(t, c, app, "juniper", "Qh", "r2", []any{"u2", "u1", "u2"}); code != 200 || !dup.Duplicate || !slices.Equal(unmatchedOf(dup), []string{`"u2"`}) {
		t.Fatalf("multi retry: %d %s %+v", code, e, dup)
	}
	if code, e, _ := emitTurns(t, c, app, "juniper", "Qh", "r2", []any{"u1", "u3"}); code != 409 || e != "id_reused" {
		t.Fatalf("multi with other recipients: %d %s", code, e)
	}
}

// Every recipient an emit names is in exactly one of matched (counted) or
// unmatched, whether it names one or many, as a string or a list: an emit
// to one user with nothing subscribed listed nobody.
func TestEmitUnmatchedListsEveryRecipient(t *testing.T) {
	c := coretest.Start(t)
	app := pokerApp(t, c, "poker")
	subscribeTurns(t, c, app, "u1", "juniper", nil)
	subscribeTurns(t, c, app, "u4", "juniper", nil)

	for _, tc := range []struct {
		name      string
		to        any
		matched   int
		unmatched []string
	}{
		{"one unsubscribed, a string", "u2", 0, []string{`"u2"`}},
		{"one unsubscribed, a list", []any{"u2"}, 0, []string{`"u2"`}},
		{"one subscribed, a string", "u1", 1, nil},
		{"one subscribed, a list", []any{"u1"}, 1, nil},
		{"two unsubscribed", []any{"u2", "u3"}, 0, []string{`"u2"`, `"u3"`}},
		{"two subscribed", []any{"u1", "u4"}, 2, nil},
		{"a mix", []any{"u1", "u2", "u4", "u3"}, 2, []string{`"u2"`, `"u3"`}},
		{"one native, unsubscribed", []any{map[string]any{"native": "u2"}}, 0, []string{`{"native":"u2"}`}},
	} {
		code, e, out := emitTurns(t, c, app, "juniper", "As", "", tc.to)
		if code != 200 || out.Matched != tc.matched || !slices.Equal(unmatchedOf(out), tc.unmatched) || len(out.Ended) != 0 || out.Unmatched == nil {
			t.Fatalf("%s: %d %s matched %d unmatched %v ended %v, want matched %d unmatched %v",
				tc.name, code, e, out.Matched, unmatchedOf(out), out.Ended, tc.matched, tc.unmatched)
		}
	}

	// A duplicate of an emit to one unsubscribed user reports it too.
	for range 2 {
		code, e, out := emitTurns(t, c, app, "juniper", "As", "lonely", "u2")
		if code != 200 || out.Matched != 0 || !slices.Equal(unmatchedOf(out), []string{`"u2"`}) {
			t.Fatalf("emit to one unsubscribed user with an id: %d %s %+v", code, e, out)
		}
	}
}

// An SDK's retry renders the same text from the same payload, so it's a
// duplicate; the id on another payload is ErrIDReused.
func TestSDKEmitRetryIsADuplicate(t *testing.T) {
	c := coretest.Start(t)
	w := newWrappedApp(t, c, "cards")
	if res := w.call(t, "u:ada", "claude-code/2.1.280", "join", map[string]any{"table": "oak"}, nil); res.IsError {
		t.Fatalf("join: %s", res.text())
	}
	emit := func(toCall string) (wakeshift.Emitted, error) {
		return w.turn.Emit(ctx, tableArgs{Table: "oak"}, turn{ToCall: toCall}, wakeshift.To("ada"), wakeshift.WithID("sdk-1"))
	}
	if got, err := emit("5"); err != nil || got.Matched != 1 || got.Duplicate {
		t.Fatalf("first: %+v %v", got, err)
	}
	if got, err := emit("5"); err != nil || got.Matched != 1 || !got.Duplicate {
		t.Fatalf("retry: %+v %v", got, err)
	}
	if _, err := emit("10"); !wakeshift.IsCode(err, wakeshift.CodeIDReused) {
		t.Fatalf("the id with another payload: %v", err)
	}
	tenant := userTenant(t, c, "cards", "ada")
	waitFor(t, "ada's delivery", func() bool { return deliveryCount(t, c, tenant) == 1 })
	time.Sleep(300 * time.Millisecond)
	if n := deliveryCount(t, c, tenant); n != 1 {
		t.Fatalf("ada got %d deliveries, want 1", n)
	}
}

// appAPIEmit is r1's first emit (game.turn at juniper, As, to u1) with
// text as the HTTP caller's own.
func appAPIEmit(t *testing.T, c *coretest.Core, app *wakeshift.Client, text string) (int, string, multiOut) {
	t.Helper()
	var out multiOut
	code, e := appAPI(t, c, app, "/api/apps/v1/events/emit", map[string]any{"name": "game.turn", "arguments": map[string]any{"table": "juniper"},
		"payload": map[string]any{"hole": "As", "turn": "t-As"}, "text": text, "to": "u1", "id": "r1"}, &out)
	return code, e, out
}
