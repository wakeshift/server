package coretest_test

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/appapi"
	"github.com/wakeshift/server/internal/coretest"
	"github.com/wakeshift/server/internal/dbtest"
	"github.com/wakeshift/server/internal/deliveries"
	"github.com/wakeshift/server/pkg/wakeshift"
)

// Multi-recipient emit: one occurrence of a personal event to users, to
// subscriptions by ID and to users' native subscriptions, in one
// request.

// multiOut is a multi-recipient emit's response.
type multiOut struct {
	Matched   int                 `json:"matched"`
	Duplicate bool                `json:"duplicate"`
	Unmatched []json.RawMessage   `json:"unmatched"`
	Ended     []map[string]string `json:"ended"`
}

// pokerApp is an app with turnEvents published.
func pokerApp(t *testing.T, c *coretest.Core, id string) *wakeshift.Client {
	t.Helper()
	app := noteApp(t, c, id)
	if err := c.Apps.SetManifest(ctx, id, turnEvents()); err != nil {
		t.Fatal(err)
	}
	return app
}

// subscribeTurns subscribes a new conversation of user to game.turn at
// table, returning the event subscription's ID and its session handle.
func subscribeTurns(t *testing.T, c *coretest.Core, app *wakeshift.Client, user, table string, extra map[string]any) eventSubResult {
	t.Helper()
	body := map[string]any{"call": map[string]any{"user": user, "client": "claude-code"}, "name": "game.turn", "arguments": map[string]any{"table": table}}
	for k, v := range extra {
		body[k] = v
	}
	var out eventSubResult
	if code, e := appAPI(t, c, app, "/api/apps/v1/event-subscriptions", body, &out); code != 200 {
		t.Fatalf("%s subscribes: %d %s", user, code, e)
	}
	return out
}

// emitTurns emits a turn at table to, returning the code, error code and
// response.
func emitTurns(t *testing.T, c *coretest.Core, app *wakeshift.Client, table, card, id string, to any) (int, string, multiOut) {
	t.Helper()
	body := map[string]any{"name": "game.turn", "arguments": map[string]any{"table": table}, "payload": map[string]any{"hole": card, "turn": "t-" + card},
		"text": "Your move, cards " + card, "to": to}
	if id != "" {
		body["id"] = id
	}
	var out multiOut
	code, e := appAPI(t, c, app, "/api/apps/v1/events/emit", body, &out)
	return code, e, out
}

// copiesOf is the events of the emit (by its source ID) in a workspace:
// the topic copy has the emit's event ID, a direct copy names it in $emit.
func copiesOf(t *testing.T, c *coretest.Core, app, source, tenant string) []map[string]string {
	t.Helper()
	var eventID string
	if err := c.DB.Pool.QueryRow(ctx, `SELECT event_id::text FROM topic_events WHERE app_id = $1 AND source_id = $2`, app, source).Scan(&eventID); err != nil {
		t.Fatal(err)
	}
	var out []map[string]string
	if err := c.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id::text, source_event_id, attrs FROM events WHERE id::text = $1 OR attrs->>'$emit' = $1 ORDER BY seq`, eventID)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id, source string
			var attrs map[string]string
			if err := rows.Scan(&id, &source, &attrs); err != nil {
				return err
			}
			attrs["id"], attrs["source_event_id"] = id, source
			out = append(out, attrs)
		}
		return rows.Err()
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

func unmatchedOf(out multiOut) []string {
	var got []string
	for _, u := range out.Unmatched {
		got = append(got, string(u))
	}
	slices.Sort(got)
	return got
}

// The three recipient forms in one emit: a user's personal topic, one
// subscription by ID (and its user), and a user's native subscription.
// Each reaches only what it names; one that names nothing live is
// reported, not refused; a duplicate returns the same lists.
func TestMultiRecipientEmit(t *testing.T) {
	eventsTestSetup(t)
	c := coretest.Start(t)
	app := pokerApp(t, c, "poker")
	u1 := subscribeTurns(t, c, app, "u1", "juniper", nil)
	u2a := subscribeTurns(t, c, app, "u2", "juniper", nil) // two conversations of u2
	subscribeTurns(t, c, app, "u2", "juniper", nil)
	chatgpt := newFakeChatGPT(t)
	if code, e := appAPI(t, c, app, "/api/apps/v1/events/subscribe", map[string]any{"call": map[string]any{"user": "u3", "client": "openai-mcp"},
		"name": "game.turn", "arguments": map[string]any{"table": "juniper"}, "delivery": map[string]any{"mode": "webhook", "url": chatgpt.url(), "secret": chatgpt.secret()},
		"ttl_ms": 3600000}, nil); code != 200 {
		t.Fatalf("u3 subscribes natively: %d %s", code, e)
	}

	to := []any{
		"u1",
		map[string]any{"user": "u2", "subscription": u2a.EventSubscription.ID},
		map[string]any{"native": "u3"},
		map[string]any{"user": "u2", "subscription": u1.EventSubscription.ID},                // u1's, named as u2's: nothing
		map[string]any{"user": "u9", "subscription": "00000000-0000-4000-8000-000000000000"}, // no such user
		"u8",                           // not subscribed
		map[string]any{"native": "u1"}, // u1 has no native subscription
	}
	code, e, out := emitTurns(t, c, app, "juniper", "As", "m1", to)
	if code != 200 {
		t.Fatalf("emit: %d %s", code, e)
	}
	if out.Matched != 3 || out.Duplicate {
		t.Fatalf("matched %d duplicate %v, want 3 and false", out.Matched, out.Duplicate)
	}
	if got := unmatchedOf(out); !slices.Equal(got, []string{`"u8"`, `{"native":"u1"}`}) {
		t.Fatalf("unmatched %v", got)
	}
	if len(out.Ended) != 2 || out.Ended[0]["subscription"] != u1.EventSubscription.ID || out.Ended[1]["user"] != "u9" {
		t.Fatalf("ended %v", out.Ended)
	}
	t1, t2 := userTenant(t, c, "poker", "u1"), userTenant(t, c, "poker", "u2")
	waitFor(t, "u1's and u2's turns", func() bool { return len(subjects(t, c, t1)) == 1 && len(subjects(t, c, t2)) == 1 })
	waitFor(t, "u3's turn at the webhook", func() bool { return len(chatgpt.got()) == 1 })
	time.Sleep(300 * time.Millisecond)
	if len(subjects(t, c, t1)) != 1 || len(subjects(t, c, t2)) != 1 || len(chatgpt.got()) != 1 {
		t.Fatalf("deliveries: u1 %d, u2 %d (one of its two conversations), u3 %d", len(subjects(t, c, t1)), len(subjects(t, c, t2)), len(chatgpt.got()))
	}
	// u2's copy is its own event, limited to that subscription, naming the
	// emit; never replayable.
	copies := copiesOf(t, c, "poker", "m1", t2)
	if len(copies) != 1 || copies[0]["$subscription"] == "" || !strings.HasPrefix(copies[0]["source_event_id"], "emit:") {
		t.Fatalf("u2's copies %v", copies)
	}
	var topics []string
	c.DB.Pool.QueryRow(ctx, `SELECT topics FROM topic_events WHERE app_id = 'poker' AND source_id = 'm1'`).Scan(&topics)
	if len(topics) != 2 {
		t.Fatalf("topics %v: only the user recipients' (u1's and u8's) personal topics", topics)
	}

	// The same emit again with its id, its list in another order: the
	// first's result, nothing new delivered.
	slices.Reverse(to)
	code, _, dup := emitTurns(t, c, app, "juniper", "As", "m1", to)
	if code != 200 || !dup.Duplicate || dup.Matched != 3 || len(dup.Ended) != 2 || !slices.Equal(unmatchedOf(dup), unmatchedOf(out)) {
		t.Fatalf("the duplicate: %d %+v", code, dup)
	}
	// The id with another list: refused, nothing delivered.
	if code, e, _ := emitTurns(t, c, app, "juniper", "As", "m1", []any{"u2", "u8"}); code != 409 || e != "id_reused" {
		t.Fatalf("the id with other recipients: %d %s, want 409 id_reused", code, e)
	}
	time.Sleep(300 * time.Millisecond)
	if len(subjects(t, c, t1)) != 1 || len(subjects(t, c, t2)) != 1 || len(chatgpt.got()) != 1 {
		t.Fatal("a duplicate was delivered")
	}
}

// One emit never wakes a conversation twice: a user recipient and that
// user's subscription in one conversation, or two subscriptions of one
// conversation, make one delivery.
func TestMultiRecipientEmitWakesAConversationOnce(t *testing.T) {
	c := coretest.Start(t)
	app := pokerApp(t, c, "poker")
	u1 := subscribeTurns(t, c, app, "u1", "juniper", nil)
	// u5's one conversation follows two tables.
	a := subscribeTurns(t, c, app, "u5", "juniper", nil)
	b := subscribeTurns(t, c, app, "u5", "oak", map[string]any{"call": map[string]any{"user": "u5", "client": "claude-code", "session": a.Session}})
	if a.Session != b.Session || a.EventSubscription.ID == b.EventSubscription.ID {
		t.Fatalf("u5's subscriptions: %+v %+v", a, b)
	}
	code, e, out := emitTurns(t, c, app, "juniper", "Kd", "once", []any{
		"u1", map[string]any{"user": "u1", "subscription": u1.EventSubscription.ID},
		map[string]any{"user": "u5", "subscription": a.EventSubscription.ID}, map[string]any{"user": "u5", "subscription": b.EventSubscription.ID},
	})
	if code != 200 || out.Matched != 2 || len(out.Ended) != 0 || len(out.Unmatched) != 0 {
		t.Fatalf("emit: %d %s %+v", code, e, out)
	}
	t1, t5 := userTenant(t, c, "poker", "u1"), userTenant(t, c, "poker", "u5")
	waitFor(t, "the turns", func() bool { return len(subjects(t, c, t1)) == 1 && len(subjects(t, c, t5)) == 1 })
	time.Sleep(300 * time.Millisecond)
	if len(subjects(t, c, t1)) != 1 || len(subjects(t, c, t5)) != 1 {
		t.Fatalf("u1 %d, u5 %d deliveries", len(subjects(t, c, t1)), len(subjects(t, c, t5)))
	}
}

// A direct emit isn't replayed: a later subscriber with replay gets the
// latest user emit from before it, never a later message sent to another
// subscription.
func TestDirectEmitsArentReplayed(t *testing.T) {
	c := coretest.Start(t)
	app := pokerApp(t, c, "poker")
	first := subscribeTurns(t, c, app, "u1", "juniper", nil)
	if code, e, out := emitTurns(t, c, app, "juniper", "3h", "user", "u1"); code != 200 || out.Matched != 1 {
		t.Fatalf("user emit: %d %s %+v", code, e, out)
	}
	if code, e, out := emitTurns(t, c, app, "juniper", "2c", "direct", []any{map[string]any{"user": "u1", "subscription": first.EventSubscription.ID},
		map[string]any{"user": "u1", "subscription": first.EventSubscription.ID}}); code != 200 || out.Matched != 1 {
		t.Fatalf("emit: %d %s %+v", code, e, out)
	}
	tenant := userTenant(t, c, "poker", "u1")
	waitFor(t, "the first conversation's copies", func() bool { return len(subjects(t, c, tenant)) == 2 })
	subscribeTurns(t, c, app, "u1", "juniper", map[string]any{"replay": "1h"})
	waitFor(t, "the later subscriber's replay", func() bool { return len(subjects(t, c, tenant)) == 3 })
	time.Sleep(300 * time.Millisecond)
	got := subjects(t, c, tenant)
	if len(got) != 3 || !strings.Contains(got[2], "3h") {
		t.Fatalf("the later subscriber's replay isn't the user emit alone: %q", got)
	}
}

// Subscription recipients are checked in their user's workspace: another
// app's subscription, or a subscription to another event, names nothing
// (ended), and nothing is delivered to it.
func TestEmitToAnotherAppsSubscription(t *testing.T) {
	c := coretest.Start(t)
	mine := pokerApp(t, c, "poker")
	other := pokerApp(t, c, "other")
	theirs := subscribeTurns(t, c, other, "u1", "juniper", nil)
	// A subscription of mine to another of my events.
	var chat eventSubResult
	if code, e := appAPI(t, c, mine, "/api/apps/v1/event-subscriptions", map[string]any{"call": map[string]any{"user": "u1", "client": "claude-code"},
		"name": "chat.message", "arguments": map[string]any{"channel": "eng"}}, &chat); code != 200 {
		t.Fatalf("chat subscribe: %d %s", code, e)
	}
	code, e, out := emitTurns(t, c, mine, "juniper", "Qh", "x", []any{
		map[string]any{"user": "u1", "subscription": theirs.EventSubscription.ID},
		map[string]any{"user": "u1", "subscription": chat.EventSubscription.ID},
	})
	if code != 200 || out.Matched != 0 || len(out.Ended) != 2 {
		t.Fatalf("emit: %d %s %+v", code, e, out)
	}
	time.Sleep(300 * time.Millisecond)
	for _, app := range []string{"poker", "other"} {
		if got := subjects(t, c, userTenant(t, c, app, "u1")); len(got) != 0 {
			t.Fatalf("%s's workspace for u1 got %q", app, got)
		}
	}
}

// What's refused before anything is recorded: an empty list, a malformed
// recipient, over 1,000 recipients, and a list on a shared event.
func TestMultiRecipientEmitRefusals(t *testing.T) {
	c := coretest.Start(t)
	app := pokerApp(t, c, "poker")
	many := make([]any, 1001)
	for i := range many {
		many[i] = fmt.Sprintf("u%d", i)
	}
	for name, to := range map[string]any{
		"empty":          []any{},
		"empty user":     []any{""},
		"both":           []any{map[string]any{"native": "u1", "user": "u1"}},
		"bad id":         []any{map[string]any{"user": "u1", "subscription": "ts_abc"}},
		"no user":        []any{map[string]any{"subscription": "00000000-0000-4000-8000-000000000000"}},
		"unknown field":  []any{map[string]any{"user": "u1", "session": "x"}},
		"over the limit": many,
		"a number":       []any{7},
		"tilde":          []any{"~conversation:x", "u2"},
	} {
		if code, e, _ := emitTurns(t, c, app, "juniper", "As", "", to); code != 400 || e != "bad_arguments" {
			t.Errorf("%s: %d %s", name, code, e)
		}
	}
	if code, e := appAPI(t, c, app, "/api/apps/v1/events/emit", map[string]any{"name": "chat.message", "arguments": map[string]any{"channel": "eng"},
		"payload": map[string]any{"text": "hi", "from": "ada"}, "text": "hi", "to": []any{"u1", "u2"}}, nil); code != 400 || e != "bad_arguments" {
		t.Fatalf("a list on a shared event: %d %s", code, e)
	}
	var n int
	c.DB.Pool.QueryRow(ctx, `SELECT count(*) FROM topic_events`).Scan(&n)
	if n != 0 {
		t.Fatalf("%d emits recorded", n)
	}
}

// An emit to 1,000 subscriptions fits in one request even with the longest
// user IDs and a near-full payload (over httpx's 64 KB, within the emit's
// own body limit), as does a status call for 1,000; a body over that limit
// is still 413, a 1,001st recipient still 400, and other routes keep 64 KB.
func TestMultiRecipientEmitBodySize(t *testing.T) {
	c := coretest.Start(t)
	app := pokerApp(t, c, "poker")
	live := subscribeTurns(t, c, app, "u1", "juniper", nil)

	long := func(i int) string { return fmt.Sprintf("%0200d", i) } // 200 characters, the most a user ID may be
	subID := func(i int) string { return fmt.Sprintf("00000000-0000-4000-8000-%012d", i) }
	to := []any{map[string]any{"user": "u1", "subscription": live.EventSubscription.ID}}
	refs := []map[string]string{{"user": "u1", "id": live.EventSubscription.ID}}
	for i := 1; i < 1000; i++ {
		to = append(to, map[string]any{"user": long(i), "subscription": subID(i)})
		refs = append(refs, map[string]string{"user": long(i), "id": subID(i)})
	}
	payload := map[string]any{"hole": "As", "turn": "t-As", "pad": strings.Repeat("x", 60<<10)}
	emit := func(to []any, payload map[string]any) (int, string, multiOut, int) {
		body := map[string]any{"name": "game.turn", "arguments": map[string]any{"table": "juniper"}, "payload": payload, "text": "Your move", "to": to}
		b, _ := json.Marshal(body)
		var out multiOut
		code, e := appAPI(t, c, app, "/api/apps/v1/events/emit", body, &out)
		return code, e, out, len(b)
	}

	code, e, out, size := emit(to, payload)
	if size < 300<<10 {
		t.Fatalf("the test's body is only %d bytes", size)
	}
	if code != 200 || out.Matched != 1 || len(out.Ended) != 999 {
		t.Fatalf("1,000 subscriptions in %d bytes: %d %s matched %d, %d ended", size, code, e, out.Matched, len(out.Ended))
	}
	var status struct {
		Subscriptions []struct {
			Live bool `json:"live"`
		} `json:"subscriptions"`
	}
	if code, e := appAPI(t, c, app, "/api/apps/v1/events/status", map[string]any{"name": "game.turn", "subscriptions": refs}, &status); code != 200 || len(status.Subscriptions) != 1000 || !status.Subscriptions[0].Live {
		t.Fatalf("status of 1,000: %d %s, %d answers", code, e, len(status.Subscriptions))
	}

	// A 1,001st recipient is refused by count, not size.
	if code, e, _, _ := emit(append(to, map[string]any{"user": long(1000), "subscription": subID(1000)}), payload); code != 400 || e != "bad_arguments" {
		t.Fatalf("1,001 recipients: %d %s", code, e)
	}
	// Over the emit's body limit: 413.
	big := map[string]any{"hole": "As", "turn": "t-As", "pad": strings.Repeat("x", 600<<10)}
	if code, e, _, _ := emit(to[:1], big); code != 413 || e != "too_large" {
		t.Fatalf("a 600 KB body: %d %s", code, e)
	}
	// The payload keeps its own 64 KB limit.
	if code, e, _, _ := emit(to[:1], map[string]any{"hole": "As", "turn": "t-As", "pad": strings.Repeat("x", 70<<10)}); code != 400 {
		t.Fatalf("a 70 KB payload: %d %s", code, e)
	}
	// Another route keeps 64 KB.
	if code, e := appAPI(t, c, app, "/api/apps/v1/event-subscriptions", map[string]any{"call": map[string]any{"user": "u2", "client": "claude-code"},
		"name": "game.turn", "arguments": map[string]any{"table": strings.Repeat("x", 70<<10)}}, nil); code != 413 || e != "too_large" {
		t.Fatalf("a 70 KB subscribe: %d %s", code, e)
	}
}

// The per-app limit counts the recipients of multi-recipient emits in a
// minute, in a counter row: a refused emit isn't counted and records
// nothing, a single-user emit isn't counted, the window rolls over, and
// the app's override applies.
func TestMultiRecipientEmitLimit(t *testing.T) {
	c := coretest.Start(t)
	app := pokerApp(t, c, "poker")
	if _, err := c.DB.Pool.Exec(ctx, `UPDATE apps SET emit_recipients_per_minute = 3 WHERE id = 'poker'`); err != nil {
		t.Fatal(err)
	}
	emit := func(id string, to any) (int, string) {
		code, e, _ := emitTurns(t, c, app, "juniper", "As", id, to)
		return code, e
	}
	counter := func() int {
		var n int
		c.DB.Pool.QueryRow(ctx, `SELECT recipients FROM app_emit_counters WHERE app_id = 'poker'`).Scan(&n)
		return n
	}
	if code, e := emit("a", []any{"u1", "u2"}); code != 200 {
		t.Fatalf("two recipients: %d %s", code, e)
	}
	if code, e := emit("b", []any{"u3", "u4"}); code != 429 || e != "rate_limited" {
		t.Fatalf("over the limit: %d %s", code, e)
	}
	var recorded bool
	c.DB.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM topic_events WHERE source_id = 'b')`).Scan(&recorded)
	if recorded || counter() != 2 {
		t.Fatalf("the refused emit: recorded %v, counter %d", recorded, counter())
	}
	for i := range 5 { // single-user emits aren't counted
		if code, e := emit(fmt.Sprintf("s%d", i), "u1"); code != 200 {
			t.Fatalf("a single-user emit: %d %s", code, e)
		}
	}
	if code, e := emit("c", []any{map[string]any{"native": "u3"}}); code != 200 || counter() != 3 {
		t.Fatalf("up to the limit: %d %s, counter %d", code, e, counter())
	}
	// A duplicate isn't counted either.
	if code, _ := emit("c", []any{map[string]any{"native": "u3"}}); code != 200 || counter() != 3 {
		t.Fatalf("a duplicate: %d, counter %d", code, counter())
	}
	// The next minute.
	if _, err := c.DB.Pool.Exec(ctx, `UPDATE app_emit_counters SET window_start = window_start - interval '1 minute'`); err != nil {
		t.Fatal(err)
	}
	if code, e := emit("d", []any{"u1", "u2", "u3"}); code != 200 || counter() != 3 {
		t.Fatalf("a new window: %d %s, counter %d", code, e, counter())
	}
	if _, err := c.DB.Pool.Exec(ctx, `UPDATE apps SET emit_recipients_per_minute = NULL WHERE id = 'poker'`); err != nil {
		t.Fatal(err)
	}
	if code, e := emit("e", []any{"u4", "u5"}); code != 200 {
		t.Fatalf("the default: %d %s", code, e)
	}
}

// A copy failing in one workspace leaves the others made: a retry with the
// same id makes it (and returns the stored lists), and so does the expiry
// pass for direct copies.
func TestMultiRecipientEmitFailingInOneWorkspace(t *testing.T) {
	c := coretest.Start(t)
	app := pokerApp(t, c, "poker")
	ann := subscribeTurns(t, c, app, "ann", "juniper", nil)
	bob := subscribeTurns(t, c, app, "bob", "juniper", nil)
	annT, bobT := userTenant(t, c, "poker", "ann"), userTenant(t, c, "poker", "bob")
	owner := dbtest.Owner(t, c.DB)
	breakBob := func(on bool) {
		t.Helper()
		sql := `DROP TRIGGER IF EXISTS fail_bob ON events`
		if on {
			sql = `CREATE OR REPLACE FUNCTION fail_bob() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'bob is down'; END $$;
				CREATE TRIGGER fail_bob BEFORE INSERT ON events FOR EACH ROW WHEN (NEW.tenant_id = '` + bobT + `') EXECUTE FUNCTION fail_bob()`
		}
		if _, err := owner.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	to := []any{map[string]any{"user": "ann", "subscription": ann.EventSubscription.ID}, map[string]any{"user": "bob", "subscription": bob.EventSubscription.ID}, "cy"}
	breakBob(true)
	if code, _, _ := emitTurns(t, c, app, "juniper", "As", "f1", to); code != 500 {
		t.Fatalf("an emit failing in one workspace answered %d", code)
	}
	if len(copiesOf(t, c, "poker", "f1", annT)) != 1 || len(copiesOf(t, c, "poker", "f1", bobT)) != 0 {
		t.Fatal("after the failure: ann's copy should be made, bob's not")
	}
	breakBob(false)
	code, _, out := emitTurns(t, c, app, "juniper", "As", "f1", to)
	if code != 200 || !out.Duplicate || out.Matched != 2 || len(out.Ended) != 0 || !slices.Equal(unmatchedOf(out), []string{`"cy"`}) {
		t.Fatalf("the retry: %d %+v", code, out)
	}
	if len(copiesOf(t, c, "poker", "f1", annT)) != 1 || len(copiesOf(t, c, "poker", "f1", bobT)) != 1 {
		t.Fatal("after the retry: one copy each")
	}

	// Without an id: the expiry pass finishes it.
	breakBob(true)
	if code, _, _ := emitTurns(t, c, app, "juniper", "Kd", "", to); code != 500 {
		t.Fatalf("an emit failing in one workspace answered %d", code)
	}
	breakBob(false)
	var source string
	if err := c.DB.Pool.QueryRow(ctx, `UPDATE topic_fanout SET created_at = now() - interval '2 minutes' RETURNING source_id`).Scan(&source); err != nil {
		t.Fatal(err)
	}
	if n, err := appapi.FinishFanouts(ctx, c.DB); err != nil || n != 1 {
		t.Fatalf("finished %d: %v", n, err)
	}
	if len(copiesOf(t, c, "poker", source, bobT)) != 1 {
		t.Fatal("the expiry pass didn't make bob's direct copy")
	}
}

// Billing counts an agent once per emit, however many of its
// conversations it reached; usage_daily still counts every delivery.
func TestMultiRecipientEmitBilling(t *testing.T) {
	c := coretest.Start(t)
	app := pokerApp(t, c, "poker")
	a := subscribeTurns(t, c, app, "u6", "juniper", nil)
	b := subscribeTurns(t, c, app, "u6", "juniper", nil)
	if code, e, out := emitTurns(t, c, app, "juniper", "As", "bill", []any{map[string]any{"user": "u6", "subscription": a.EventSubscription.ID},
		map[string]any{"user": "u6", "subscription": b.EventSubscription.ID}}); code != 200 || out.Matched != 2 {
		t.Fatalf("emit: %d %s %+v", code, e, out)
	}
	// And a single-user emit, which reaches both conversations as one event.
	if code, e, _ := emitTurns(t, c, app, "juniper", "Kd", "single", "u6"); code != 200 {
		t.Fatalf("single emit: %d %s", code, e)
	}
	tenant := userTenant(t, c, "poker", "u6")
	waitFor(t, "four deliveries", func() bool { return len(subjects(t, c, tenant)) == 4 })
	var delivered, billable int
	if err := c.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE deliveries SET state = 'acked', finished_at = now()`); err != nil {
			return err
		}
		if _, err := deliveries.RollUpUsage(ctx, tx, time.Now()); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT delivered, billable FROM usage_daily WHERE tenant_id = current_tenant()`).Scan(&delivered, &billable)
	}); err != nil {
		t.Fatal(err)
	}
	if delivered != 4 || billable != 2 {
		t.Fatalf("delivered %d billable %d, want 4 and 2 (one agent, two emits)", delivered, billable)
	}
}

// A subscribe to a personal topic and a multi-recipient emit to it share
// the topic's stripe lock: whichever starts second waits, so the
// subscriber gets the emit exactly once (live, or by replay), and neither
// deadlocks. Each side is paused at its conflict point in turn.
func TestMultiRecipientEmitAndSubscribeShareTheStripe(t *testing.T) {
	for _, paused := range []string{"emit", "subscribe"} {
		t.Run(paused, func(t *testing.T) {
			c := coretest.Start(t)
			app := pokerApp(t, c, "poker")
			owner := dbtest.Owner(t, c.DB)
			must := func(err error) {
				t.Helper()
				if err != nil {
					t.Fatal(err)
				}
			}
			// The paused side's insert waits on a lock the test holds: the
			// emit's record, or the subscribe's topic subscription.
			table := map[string]string{"emit": "topic_events", "subscribe": "topic_subscriptions"}[paused]
			_, err := owner.Exec(ctx, `CREATE FUNCTION zz_hold() RETURNS trigger LANGUAGE plpgsql AS $$
				BEGIN PERFORM pg_advisory_xact_lock_shared(4244); RETURN NEW; END $$`)
			must(err)
			_, err = owner.Exec(ctx, `CREATE TRIGGER zz_hold BEFORE INSERT ON `+table+` FOR EACH ROW EXECUTE FUNCTION zz_hold()`)
			must(err)
			_, err = owner.Exec(ctx, `SELECT pg_advisory_lock(4244)`)
			must(err)
			released := false
			release := func() {
				if !released {
					released = true
					owner.Exec(ctx, `SELECT pg_advisory_unlock(4244)`)
				}
			}
			defer release()
			waiting := func(n int) {
				t.Helper()
				for deadline := time.Now().Add(15 * time.Second); ; {
					var got int
					must(owner.QueryRow(ctx, `SELECT count(*) FROM pg_locks l JOIN pg_stat_activity a ON a.pid = l.pid
						WHERE NOT l.granted AND a.datname = current_database() AND l.locktype = 'advisory'`).Scan(&got))
					if got >= n {
						return
					}
					if time.Now().After(deadline) {
						t.Fatalf("%d waiting, never %d", got, n)
					}
					time.Sleep(10 * time.Millisecond)
				}
			}
			type result struct {
				code int
				e    string
				err  error
			}
			emitted, subscribed := make(chan result, 1), make(chan result, 1)
			emit := func() {
				code, e, err := appPost(c, app, "/api/apps/v1/events/emit", map[string]any{"name": "game.turn", "arguments": map[string]any{"table": "juniper"},
					"payload": map[string]any{"hole": "As", "turn": "t1"}, "text": "Your move", "id": "raced", "to": []any{"u1", "u2"}}, nil)
				emitted <- result{code, e, err}
			}
			subscribe := func() {
				code, e, err := appPost(c, app, "/api/apps/v1/event-subscriptions", map[string]any{"call": map[string]any{"user": "u1", "client": "claude-code"},
					"name": "game.turn", "arguments": map[string]any{"table": "juniper"}, "replay": "1h"}, nil)
				subscribed <- result{code, e, err}
			}
			if paused == "emit" {
				go emit()
				waiting(1) // the emit holds the stripe, paused at its record
				go subscribe()
				waiting(2) // the subscribe waits for the stripe
			} else {
				go subscribe()
				waiting(1) // the subscribe holds its topic and stripe, paused at its subscription
				go emit()
				waiting(2) // the emit waits for the stripe
			}
			release()
			for name, ch := range map[string]chan result{"emit": emitted, "subscribe": subscribed} {
				if r := <-ch; r.err != nil || r.code != 200 {
					t.Fatalf("%s: %d %s %v", name, r.code, r.e, r.err)
				}
			}
			tenant := userTenant(t, c, "poker", "u1")
			waitFor(t, "u1's turn", func() bool { return len(subjects(t, c, tenant)) >= 1 })
			time.Sleep(300 * time.Millisecond)
			if got := subjects(t, c, tenant); len(got) != 1 {
				t.Fatalf("u1 got %d copies, want exactly one: %q", len(got), got)
			}
		})
	}
}
