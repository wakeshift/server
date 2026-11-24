package coretest_test

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/appapi"
	"github.com/wakeshift/server/internal/audit"
	"github.com/wakeshift/server/internal/beacon"
	"github.com/wakeshift/server/internal/core"
	"github.com/wakeshift/server/internal/coretest"
	"github.com/wakeshift/server/pkg/wakeshift"
)

// MCP Events' push delivery (events/stream), at the core's upstream
// endpoint as an SDK's relay opens it.

const pushWait = 5 * time.Second

func pushBody(user string, cursor any, extra map[string]any) map[string]any {
	call := map[string]any{"client": "opencode"}
	if user != "" {
		call["user"] = user
	}
	b := map[string]any{"call": call, "name": "chat.message", "arguments": map[string]any{"channel": "eng"}, "cursor": cursor}
	for k, v := range extra {
		b[k] = v
	}
	return b
}

// fastHeartbeats lets a stream grant short heartbeats (the bounds are 10 s
// to 45 s), so confirmation and cadence are testable.
func fastHeartbeats(t *testing.T) {
	min, def := appapi.PushHeartbeatMin, appapi.PushHeartbeat
	appapi.PushHeartbeatMin, appapi.PushHeartbeat = 50*time.Millisecond, 200*time.Millisecond
	t.Cleanup(func() { appapi.PushHeartbeatMin, appapi.PushHeartbeat = min, def })
}

func mustActive(t *testing.T, s *coretest.PushStream) map[string]any {
	t.Helper()
	if s.Status != 200 {
		t.Fatalf("open: %d %s", s.Status, s.Error)
	}
	f := s.Next(pushWait)
	if f == nil || f["type"] != "active" || !strings.HasPrefix(fmt.Sprint(f["cursor"]), "c2.") {
		t.Fatalf("first frame: %+v", f)
	}
	return f
}

func eventText(f map[string]any) string {
	occ, _ := f["occurrence"].(map[string]any)
	data, _ := occ["data"].(map[string]any)
	return fmt.Sprint(data["text"])
}

func pushErrCode(s *coretest.PushStream) string {
	var e struct {
		Error struct {
			Code  string
			Limit string
			Max   int
		}
	}
	json.Unmarshal(s.Error, &e)
	return e.Error.Code
}

// Open from null: active first (a new subscription, the default grant),
// then each emit as an event with its own cursor, in order; the consent
// record is a push subscription, audited; the grant is clamped.
func TestPushOpenAndDeliver(t *testing.T) {
	c := coretest.Start(t)
	app := eventsApp(t, c, "chat")
	s := c.OpenPush(t, app, pushBody("u1", nil, nil))
	a := mustActive(t, s)
	if a["created"] != true || a["truncated"] != false || a["heartbeat_ms"] != float64(30000) || a["via"] != nil {
		t.Fatalf("active: %+v", a)
	}
	emitChat(t, c, app, "a", "b")
	var cursors []string
	for _, want := range []string{"zqazq", "zqbzq"} {
		f := s.NextOf("event", pushWait)
		if f == nil || eventText(f) != want {
			t.Fatalf("event %s: %+v", want, f)
		}
		occ := f["occurrence"].(map[string]any)
		if occ["name"] != "chat.message" || occ["eventId"] == "" || !strings.HasPrefix(fmt.Sprint(occ["cursor"]), "c2.") {
			t.Fatalf("occurrence: %+v", occ)
		}
		cursors = append(cursors, occ["cursor"].(string))
	}
	if cursors[0] == cursors[1] || cursors[0] == a["cursor"] {
		t.Fatalf("cursors don't move: %v %v", a["cursor"], cursors)
	}
	tenant := userTenant(t, c, "chat", "u1")
	var dest, via string
	c.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT destination, via FROM event_subscriptions`).Scan(&dest, &via)
	})
	if dest != "push" || via != "push" {
		t.Fatalf("record: %s %s", dest, via)
	}
	if acts := auditActions(t, c, tenant); fmt.Sprint(acts) != "[event_subscription.created]" {
		t.Fatalf("audit: %v", acts)
	}
	for ms, want := range map[int64]float64{1: 10000, 20000: 20000, 600000: 45000} {
		g := mustActive(t, c.OpenPush(t, app, pushBody("u2", nil, map[string]any{"heartbeat_ms": ms})))
		if g["heartbeat_ms"] != want {
			t.Fatalf("grant for %d: %v", ms, g["heartbeat_ms"])
		}
	}
}

// Heartbeats keep their cadence while events flow (the conformance suite
// watches for one within its interval whatever else arrives).
func TestPushHeartbeatsWhileEventsFlow(t *testing.T) {
	fastHeartbeats(t)
	c := coretest.Start(t)
	app := eventsApp(t, c, "chat")
	s := c.OpenPush(t, app, pushBody("u1", nil, nil))
	mustActive(t, s)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range 8 {
			appPost(c, app, "/api/apps/v1/events/emit", map[string]any{"name": "chat.message", "arguments": map[string]any{"channel": "eng"},
				"payload": map[string]any{"text": fmt.Sprintf("zq%dzq", i), "from": "ada"}, "text": "m"}, nil)
			time.Sleep(100 * time.Millisecond)
		}
	}()
	beats, events := 0, 0
	end := time.Now().Add(1200 * time.Millisecond)
	for time.Now().Before(end) {
		f := s.Next(time.Until(end))
		switch {
		case f == nil:
		case f["type"] == "heartbeat":
			beats++
		case f["type"] == "event":
			events++
		}
	}
	<-done
	if events == 0 || beats < 3 {
		t.Fatalf("while events flowed: %d heartbeats, %d events", beats, events)
	}
}

// Heartbeats come at the grant with the stream's position, never behind
// an event cursor already written; a write a grant later confirms what
// came before it (delivered).
func TestPushHeartbeatsConfirm(t *testing.T) {
	fastHeartbeats(t)
	c := coretest.Start(t)
	app := eventsApp(t, c, "chat")
	s := c.OpenPush(t, app, pushBody("u1", nil, nil))
	a := mustActive(t, s)
	if a["heartbeat_ms"] != float64(200) {
		t.Fatalf("grant: %v", a["heartbeat_ms"])
	}
	if hb := s.NextOf("heartbeat", pushWait); hb == nil || hb["cursor"] != a["cursor"] {
		t.Fatalf("idle heartbeat: %+v", hb)
	}
	emitChat(t, c, app, "a")
	ev := s.NextOf("event", pushWait)
	cur := ev["occurrence"].(map[string]any)["cursor"]
	if hb := s.NextOf("heartbeat", pushWait); hb == nil || hb["cursor"] != cur {
		t.Fatalf("heartbeat after the event: %+v, want %v", hb, cur)
	}
	waitFor(t, "delivered", func() bool {
		s.Next(300 * time.Millisecond)
		return deliveryStates(t, c, "chat", "u1")["delivered"] == 1
	})
}

// A cursor resumes its subscription: it acknowledges what it covers, and
// replays what's after it, delivered rows included; the newest stream wins
// (the older gets close). Resuming needs the same principal: another
// user's cursor, a tampered one, or one from another workspace is a bad
// request. A null cursor always makes a new subscription.
func TestPushResume(t *testing.T) {
	fastHeartbeats(t)
	c := coretest.Start(t)
	app := eventsApp(t, c, "chat")
	first := c.OpenPush(t, app, pushBody("u1", nil, nil))
	start := mustActive(t, first)["cursor"].(string)
	emitChat(t, c, app, "a", "b")
	ev := first.NextOf("event", pushWait)
	first.NextOf("event", pushWait)
	afterA := ev["occurrence"].(map[string]any)["cursor"].(string)
	waitFor(t, "delivered", func() bool {
		first.Next(300 * time.Millisecond)
		return deliveryStates(t, c, "chat", "u1")["delivered"] == 2
	})

	second := c.OpenPush(t, app, pushBody("u1", afterA, nil))
	a := mustActive(t, second)
	if a["created"] != nil || a["truncated"] != false || a["cursor"] != afterA {
		t.Fatalf("resumed active: %+v", a)
	}
	if f := second.NextOf("event", pushWait); f == nil || eventText(f) != "zqbzq" {
		t.Fatalf("replayed: %+v", f)
	}
	if f := first.NextOf("close", pushWait); f == nil || f["type"] != "close" {
		t.Fatalf("the older stream: %+v", f)
	}
	// From the start: both again (a delivered row the client says it
	// never saw comes back).
	third := c.OpenPush(t, app, pushBody("u1", start, nil))
	mustActive(t, third)
	for _, want := range []string{"zqazq", "zqbzq"} {
		if f := third.NextOf("event", pushWait); f == nil || eventText(f) != want {
			t.Fatalf("replay from the start, %s: %+v", want, f)
		}
	}

	for name, body := range map[string]map[string]any{
		"another user's cursor": pushBody("u2", afterA, nil),
		"a tampered cursor":     pushBody("u1", afterA[:len(afterA)-2]+"AA", nil),
		"a malformed cursor":    pushBody("u1", "c2.nope", nil),
		"a poll cursor":         pushBody("u1", "c1.3", nil),
		"another app's cursor":  pushBody("u1", afterA, nil),
	} {
		target := app
		if name == "another app's cursor" {
			target = eventsApp(t, c, "chat2")
		}
		if s := c.OpenPush(t, target, body); s.Status != 400 || pushErrCode(s) != "bad_request" {
			t.Fatalf("%s: %d %s", name, s.Status, s.Error)
		}
	}
	// A null cursor is a new subscription, with its own queue.
	if a := mustActive(t, c.OpenPush(t, app, pushBody("u1", nil, nil))); a["created"] != true {
		t.Fatalf("null cursor: %+v", a)
	}
	var n int
	c.DB.InTenant(ctx, userTenant(t, c, "chat", "u1"), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM event_subscriptions WHERE destination = 'push' AND ended_at IS NULL`).Scan(&n)
	})
	if n != 2 {
		t.Fatalf("live push subscriptions: %d", n)
	}
}

// A revocation in the console ends the stream with terminated within a
// second, and a reconnect with its cursor is refused (subscription_stopped)
// every time. A grant revocation ends every push subscription of the
// agent the same way.
func TestPushRevoked(t *testing.T) {
	c := coretest.Start(t)
	app := eventsApp(t, c, "chat")
	s := c.OpenPush(t, app, pushBody("u1", nil, nil))
	cur := mustActive(t, s)["cursor"]
	tenant := userTenant(t, c, "chat", "u1")
	var id string
	c.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT id::text FROM event_subscriptions`).Scan(&id)
	})
	at := time.Now()
	if err := c.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		_, err := appapi.RevokeEventSubscription(ctx, tx, id, audit.User("someone"))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	f := s.NextOf("terminated", 2*time.Second)
	if f == nil || f["type"] != "terminated" || time.Since(at) > time.Second {
		t.Fatalf("terminated: %+v after %s", f, time.Since(at))
	}
	if e := f["error"].(map[string]any); e["name"] != "Forbidden" || e["message"] != "Access revoked" {
		t.Fatalf("terminated: %+v", f)
	}
	for range 2 {
		if r := c.OpenPush(t, app, pushBody("u1", cur, nil)); r.Status != 403 || pushErrCode(r) != "subscription_stopped" {
			t.Fatalf("reconnect: %d %s", r.Status, r.Error)
		}
	}

	// Grant revocation.
	g := c.OpenPush(t, app, pushBody("u1", nil, nil))
	mustActive(t, g)
	if err := c.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT 1`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := appapi.RevokeGrantsIn(ctx, c.DB, "chat", audit.User("someone"), []string{tenant}); err != nil {
		t.Fatal(err)
	}
	if f := g.NextOf("terminated", 2*time.Second); f == nil || f["type"] != "terminated" {
		t.Fatalf("after grant revocation: %+v", f)
	}
}

// Closing the event to tools-only ends its push subscriptions
// (policy_changed); stdio's end call ends one and audits it.
func TestPushPolicyAndEnd(t *testing.T) {
	c := coretest.Start(t)
	app := eventsApp(t, c, "chat")
	s := c.OpenPush(t, app, pushBody("u1", nil, nil))
	cur := mustActive(t, s)["cursor"].(string)
	// The end call: a cursor for other arguments ends nothing.
	var out struct{ Ended bool }
	other := map[string]any{"call": map[string]any{"user": "u1", "client": "opencode"}, "name": "chat.message", "arguments": map[string]any{"channel": "x"}, "cursor": cur}
	if code, e := appAPI(t, c, app, "/api/apps/v1/events/stream/end", other, &out); code != 200 || out.Ended {
		t.Fatalf("end, other arguments: %d %s %v", code, e, out.Ended)
	}
	if code, _ := appAPI(t, c, app, "/api/apps/v1/events/stream/end", map[string]any{"call": map[string]any{"user": "u2", "client": "opencode"}, "name": "chat.message",
		"arguments": eng, "cursor": cur}, &out); code != 400 && out.Ended {
		t.Fatalf("end by another user ended it")
	}
	if code, e := appAPI(t, c, app, "/api/apps/v1/events/stream/end", map[string]any{"call": map[string]any{"user": "u1", "client": "claude-code"}, "name": "chat.message",
		"arguments": eng, "cursor": cur}, &out); code != 200 || out.Ended {
		t.Fatalf("end by the same user's other client: %d %s %v", code, e, out.Ended)
	}
	if code, e := appAPI(t, c, app, "/api/apps/v1/events/stream/end", map[string]any{"call": map[string]any{"user": "u1", "client": "opencode"}, "name": "chat.message",
		"arguments": eng, "cursor": cur}, &out); code != 200 || !out.Ended {
		t.Fatalf("end: %d %s %v", code, e, out.Ended)
	}
	if f := s.NextOf("terminated", 2*time.Second); f == nil {
		t.Fatal("no terminated after the end call")
	}
	tenant := userTenant(t, c, "chat", "u1")
	if acts := auditActions(t, c, tenant); fmt.Sprint(acts) != "[event_subscription.created event_subscription.unsubscribed]" {
		t.Fatalf("audit: %v", acts)
	}

	p := c.OpenPush(t, app, pushBody("u1", nil, nil))
	mustActive(t, p)
	if _, code := publishManifest(t, c, app, false, map[string]string{"chat.message": "tools"}); code != 200 {
		t.Fatalf("manifest: %d", code)
	}
	f := p.NextOf("terminated", 2*time.Second)
	if f == nil || f["error"].(map[string]any)["message"] == "Access revoked" {
		t.Fatalf("policy close: %+v", f)
	}
	if r := c.OpenPush(t, app, pushBody("u1", nil, nil)); r.Status != 403 || pushErrCode(r) != "tools_only" {
		t.Fatalf("tools-only without a voucher: %d %s", r.Status, r.Error)
	}
}

// Refusals before open, as /events/poll's: an unknown event, no user, the
// flag off; and the caps.
func TestPushRefusals(t *testing.T) {
	c := coretest.Start(t)
	app := eventsApp(t, c, "chat")
	if s := c.OpenPush(t, app, map[string]any{"call": map[string]any{"user": "u1"}, "name": "nope", "arguments": eng}); s.Status != 404 || pushErrCode(s) != "unknown_event" {
		t.Fatalf("unknown event: %d %s", s.Status, s.Error)
	}
	if s := c.OpenPush(t, app, pushBody("", nil, nil)); s.Status != 400 || pushErrCode(s) != "bad_request" {
		t.Fatalf("no user: %d %s", s.Status, s.Error)
	}
	// 20 live push subscriptions per user; three at once always work.
	for i := range appapi.MaxPushSubscriptions {
		s := c.OpenPush(t, app, pushBody("u1", nil, nil))
		mustActive(t, s)
		if i >= 3 {
			s.Close()
		}
	}
	s := c.OpenPush(t, app, pushBody("u1", nil, nil))
	var e struct {
		Error struct {
			Code, Limit string
			Max         int
		}
	}
	json.Unmarshal(s.Error, &e)
	if s.Status != 403 || e.Error.Code != "subscription_limit" || e.Error.Limit != "subscriptions" || e.Error.Max != 20 {
		t.Fatalf("21st: %d %s", s.Status, s.Error)
	}
	// The app's streams.
	prev := appapi.MaxPushStreams
	appapi.MaxPushStreams = 3 // the three still open
	t.Cleanup(func() { appapi.MaxPushStreams = prev })
	s = c.OpenPush(t, app, pushBody("u2", nil, nil))
	json.Unmarshal(s.Error, &e)
	if s.Status != 403 || e.Error.Code != "subscription_limit" || e.Error.Limit != "streams" {
		t.Fatalf("past the app's streams: %d %s", s.Status, s.Error)
	}

	off := coretest.StartWith(t, func(o *core.Options) { o.MCPEventsPush = false })
	offApp := eventsApp(t, off, "chat")
	if s := off.OpenPush(t, offApp, pushBody("u1", nil, nil)); s.Status != 501 || pushErrCode(s) != "unsupported" {
		t.Fatalf("flag off: %d %s", s.Status, s.Error)
	}
	var m struct{ Features []string }
	if code, _ := appAPI(t, off, offApp, "/api/apps/v1/manifest/merge", map[string]any{"events": []any{}}, &m); code != 200 || strings.Contains(fmt.Sprint(m.Features), "push") {
		t.Fatalf("features with push off: %v", m.Features)
	}
}

// The instance's cap (MaxStreams) answers 503 with Retry-After.
func TestPushMaxStreams(t *testing.T) {
	c := coretest.StartWith(t, func(o *core.Options) { o.MaxStreams = 1 })
	app := eventsApp(t, c, "chat")
	mustActive(t, c.OpenPush(t, app, pushBody("u1", nil, nil)))
	s := c.OpenPush(t, app, pushBody("u1", nil, nil))
	if s.Status != 503 || s.Header.Get("Retry-After") == "" {
		t.Fatalf("at capacity: %d %v", s.Status, s.Header)
	}
}

// Vouchers over push: a user-bound voucher redeems once (its replay
// first) and resumes only with its own cursor and user; a session-bound
// one only from its transport session; one bound to neither is refused; a
// used one with a null cursor, or another subscription's, is refused.
func TestPushVouchers(t *testing.T) {
	c := coretest.Start(t)
	app := voucherApp(t, c, "chat")
	_, unbound := handOut(t, c, app, anonCall(), "chat.message", eng, nil)
	voucherBody := func(call map[string]any, v string, cursor any) map[string]any {
		return map[string]any{"call": call, "name": "chat.message", "arguments": eng, "cursor": cursor, "voucher": v}
	}
	if s := c.OpenPush(t, app, voucherBody(anonCall(), unbound, nil)); s.Status != 403 || pushErrCode(s) != "bad_voucher" {
		t.Fatalf("anonymous without a session: %d %s", s.Status, s.Error)
	}
	if s := c.OpenPush(t, app, voucherBody(userCall("ada"), "", nil)); s.Status != 403 || pushErrCode(s) != "tools_only" {
		t.Fatalf("no voucher: %d %s", s.Status, s.Error)
	}

	_, ada := handOut(t, c, app, userCall("ada"), "chat.message", eng, nil)
	emitChat(t, c, app, "r")
	if s := c.OpenPush(t, app, voucherBody(userCall("bob"), ada, nil)); s.Status != 403 || pushErrCode(s) != "bad_voucher" {
		t.Fatalf("another user: %d %s", s.Status, s.Error)
	}
	s := c.OpenPush(t, app, voucherBody(userCall("ada"), ada, nil))
	a := mustActive(t, s)
	if a["created"] != true || a["via"] != "tool:watch_job" || a["tool"] != "watch_job" {
		t.Fatalf("redeemed: %+v", a)
	}
	if f := s.NextOf("event", pushWait); f == nil || eventText(f) != "zqrzq" {
		t.Fatalf("the voucher's replay: %+v", f)
	}
	cur := a["cursor"]
	if r := c.OpenPush(t, app, voucherBody(userCall("ada"), ada, nil)); r.Status != 403 || pushErrCode(r) != "bad_voucher" {
		t.Fatalf("used, null cursor: %d %s", r.Status, r.Error)
	}
	if r := c.OpenPush(t, app, voucherBody(userCall("bob"), ada, cur)); r.Status != 403 {
		t.Fatalf("resume by another user: %d %s", r.Status, r.Error)
	}
	if r := c.OpenPush(t, app, voucherBody(userCall("ada"), ada, cur)); r.Status != 200 {
		t.Fatalf("resume: %d %s", r.Status, r.Error)
	}

	sess := mergeMap(anonCall(), map[string]any{"transport_session": strings.Repeat("ab", 32)})
	_, bound := handOut(t, c, app, sess, "chat.message", eng, nil)
	b := c.OpenPush(t, app, voucherBody(sess, bound, nil))
	bcur := mustActive(t, b)["cursor"]
	otherSess := mergeMap(anonCall(), map[string]any{"transport_session": strings.Repeat("cd", 32)})
	if r := c.OpenPush(t, app, voucherBody(otherSess, bound, bcur)); r.Status != 403 || pushErrCode(r) != "bad_voucher" {
		t.Fatalf("another transport session: %d %s", r.Status, r.Error)
	}
	if r := c.OpenPush(t, app, voucherBody(sess, bound, cur)); r.Status != 403 || pushErrCode(r) != "bad_voucher" {
		t.Fatalf("another subscription's cursor: %d %s", r.Status, r.Error)
	}
	if r := c.OpenPush(t, app, voucherBody(sess, bound, bcur)); r.Status != 200 {
		t.Fatalf("resume from its session: %d %s", r.Status, r.Error)
	}
	// stdio's end call: the cursor alone, or from another session, ends
	// nothing; from its own session it ends it.
	for _, tc := range []struct {
		call map[string]any
		want bool
	}{{anonCall(), false}, {otherSess, false}, {sess, true}} {
		var out struct{ Ended bool }
		if code, e := appAPI(t, c, app, "/api/apps/v1/events/stream/end", map[string]any{"call": tc.call, "name": "chat.message", "arguments": eng,
			"cursor": bcur}, &out); code != 200 || out.Ended != tc.want {
			t.Fatalf("end from %v: %d %s %v", tc.call, code, e, out.Ended)
		}
	}
}

// opencode's routing: a bridged subscribe from opencode that declared
// the extension, with push served and no plugin listening, is answered
// native_line, and its wakes (call.wakes) is true: the push line wakes the
// open session. Without the extension, or another client, there's no
// native line, and wakes is as wakePath has it for that client.
func TestPushOpencodeNativeLine(t *testing.T) {
	c := coretest.Start(t)
	app := pokerApp(t, c, "poker")
	for i, tc := range []struct {
		name        string
		call        map[string]any
		want, wakes bool
	}{
		{"opencode with the extension", map[string]any{"client": "opencode", "events_ext": true}, true, true},
		{"opencode without it", map[string]any{"client": "opencode"}, false, false},
		{"Claude Code with it", map[string]any{"client": "claude-code", "events_ext": true}, false, true},
	} {
		call := tc.call
		call["user"], call["tool"] = fmt.Sprintf("n%d", i), "join"
		var out struct {
			NativeLine *bool `json:"native_line"`
			Wakes      *bool `json:"wakes"`
		}
		code, e := appAPI(t, c, app, "/api/apps/v1/event-subscriptions",
			map[string]any{"call": call, "name": "game.turn", "arguments": map[string]any{"table": "oak"}, "defer_addition": true}, &out)
		if code != 200 || out.NativeLine == nil || *out.NativeLine != tc.want || out.Wakes == nil || *out.Wakes != tc.wakes {
			t.Fatalf("%s: %d %s native_line %v, wakes %v", tc.name, code, e, out.NativeLine, out.Wakes)
		}
		// The call's block, as the SDKs make it: the push line alone, with
		// no install offer; without the extension, today's offer.
		call["user"] = fmt.Sprintf("m%d", i)
		w := sdkWatch(t, c, app, call, "game.turn", map[string]any{"table": "oak"})
		switch {
		case tc.want:
			pushLineOnly(t, tc.name, w, `To be woken by game.turn with {"table":"oak"}, subscribe to it (MCP Events).`)
		case tc.call["client"] == "opencode":
			if w.native || !strings.Contains(w.text, installOffer[0]) || strings.Contains(w.text, "MCP Events") {
				t.Fatalf("%s's block: %q", tc.name, w.text)
			}
		default:
			if w.native || strings.Contains(w.text, "MCP Events") {
				t.Fatalf("%s's block: %q", tc.name, w.text)
			}
		}
	}
	// Push off: opencode with the extension is bridged, and not woken
	// without its plugin.
	off := coretest.StartWith(t, func(o *core.Options) { o.MCPEventsPush = false })
	offApp := pokerApp(t, off, "poker")
	var out struct {
		NativeLine bool `json:"native_line"`
		Wakes      bool `json:"wakes"`
	}
	if code, e := appAPI(t, off, offApp, "/api/apps/v1/event-subscriptions", map[string]any{"call": map[string]any{"client": "opencode", "events_ext": true,
		"user": "n9", "tool": "join"}, "name": "game.turn", "arguments": map[string]any{"table": "oak"}, "defer_addition": true}, &out); code != 200 || out.NativeLine || out.Wakes {
		t.Fatalf("push off: %d %s %+v", code, e, out)
	}
}

// No cursor, argument value or voucher reaches a log, on any push path.
func TestPushNotLogged(t *testing.T) {
	var buf syncBuffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	fastHeartbeats(t)
	c := coretest.Start(t)
	app := eventsApp(t, c, "chat")
	secretArgs := map[string]any{"channel": "zzsecretchannelzz"}
	s := c.OpenPush(t, app, map[string]any{"call": map[string]any{"user": "u1", "client": "opencode"}, "name": "chat.message", "arguments": secretArgs})
	cur := mustActive(t, s)["cursor"].(string)
	c.OpenPush(t, app, map[string]any{"call": map[string]any{"user": "u1"}, "name": "chat.message", "arguments": secretArgs, "cursor": cur})
	c.OpenPush(t, app, map[string]any{"call": map[string]any{"user": "u1"}, "name": "chat.message", "arguments": secretArgs, "cursor": cur + "x"})
	appAPI(t, c, app, "/api/apps/v1/events/stream/end", map[string]any{"call": map[string]any{"user": "u1", "client": "opencode"}, "name": "chat.message", "arguments": secretArgs, "cursor": cur}, nil)
	vapp := voucherApp(t, c, "vchat")
	_, v := handOut(t, c, vapp, userCall("ada"), "chat.message", eng, nil)
	vs := c.OpenPush(t, vapp, map[string]any{"call": userCall("ada"), "name": "chat.message", "arguments": eng, "voucher": v})
	vcur := mustActive(t, vs)["cursor"].(string)
	c.OpenPush(t, vapp, map[string]any{"call": userCall("bob"), "name": "chat.message", "arguments": eng, "voucher": v, "cursor": vcur})
	time.Sleep(300 * time.Millisecond)
	logs := buf.String()
	if !strings.Contains(logs, "push stream opened") {
		t.Fatalf("nothing logged: the canary checks nothing")
	}
	for _, secret := range []string{cur, cur[3:25], vcur, v, "zzsecretchannelzz"} {
		if strings.Contains(logs, secret) {
			t.Fatalf("logged: %q", secret)
		}
	}
	_ = wakeshift.Manifest{}
}

// Forgetting the user erases the workspace and ends its streams at once;
// the cursor then names nothing (a bad request).
func TestPushForgotten(t *testing.T) {
	c := coretest.Start(t)
	app := eventsApp(t, c, "chat")
	s := c.OpenPush(t, app, pushBody("u1", nil, nil))
	cur := mustActive(t, s)["cursor"]
	if code, e := appAPI(t, c, app, "/api/apps/v1/users/forget", map[string]any{"user": "u1"}, nil); code != 200 {
		t.Fatalf("forget: %d %s", code, e)
	}
	if f := s.NextOf("terminated", 2*time.Second); f == nil || f["type"] != "terminated" {
		t.Fatalf("after forgetting: %+v", f)
	}
	if r := c.OpenPush(t, app, pushBody("u1", cur, nil)); r.Status != 400 {
		t.Fatalf("its cursor: %d %s", r.Status, r.Error)
	}
}

// maxAgeMs skips what waits from before it, and says truncated.
func TestPushMaxAge(t *testing.T) {
	c := coretest.Start(t)
	app := eventsApp(t, c, "chat")
	s := c.OpenPush(t, app, pushBody("u1", nil, nil))
	cur := mustActive(t, s)["cursor"]
	s.Close()
	waitFor(t, "released", func() bool {
		var live bool
		c.DB.InTenant(ctx, userTenant(t, c, "chat", "u1"), func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM stream_leases WHERE renewed_at IS NOT NULL)`).Scan(&live)
		})
		return !live
	})
	emitChat(t, c, app, "old")
	waitFor(t, "queued", func() bool { return deliveryStates(t, c, "chat", "u1")["pending"] == 1 })
	time.Sleep(300 * time.Millisecond)
	r := c.OpenPush(t, app, pushBody("u1", cur, map[string]any{"max_age_ms": 100}))
	if a := mustActive(t, r); a["truncated"] != true {
		t.Fatalf("active: %+v", a)
	}
	if f := r.NextOf("event", 500*time.Millisecond); f != nil && f["type"] == "event" {
		t.Fatalf("an old event: %+v", f)
	}
}

// watchOut is what sdkWatch's tool call got.
type watchOut struct {
	native, wakes bool
	session, text string
	meta          map[string]any
}

// sdkWatch is a tool call's subscribe as the SDKs make it for a caller
// push wakes: the bridged subscribe (deferred), then the call's
// block, naming the session the subscribe resolved, with the native line
// when the core answered native_line.
func sdkWatch(t *testing.T, c *coretest.Core, app *wakeshift.Client, call map[string]any, event string, args map[string]any) watchOut {
	t.Helper()
	var sub struct {
		Pending    string `json:"pending"`
		Session    string `json:"session"`
		NativeLine bool   `json:"native_line"`
		Wakes      bool   `json:"wakes"`
	}
	if code, e := appAPI(t, c, app, "/api/apps/v1/event-subscriptions", map[string]any{"call": call, "name": event, "arguments": args, "defer_addition": true}, &sub); code != 200 {
		t.Fatalf("subscribe: %d %s", code, e)
	}
	body := map[string]any{"call": mergeMap(mergeMap(map[string]any{}, call), map[string]any{"session": sub.Session}), "pending": sub.Pending}
	if sub.NativeLine {
		body["native"] = []any{map[string]any{"name": event, "arguments": args}}
	}
	var out struct {
		Text string
		Meta map[string]any
	}
	if code, e := appAPI(t, c, app, "/api/apps/v1/calls/addition", body, &out); code != 200 {
		t.Fatalf("addition: %d %s", code, e)
	}
	return watchOut{native: sub.NativeLine, wakes: sub.Wakes, session: sub.Session, text: out.Text, meta: out.Meta}
}

// The install offer's words (harnesses.SubscribeNote, RepeatNote).
var installOffer = []string{"Wakeshift can't wake this conversation", "install Wakeshift's plugin", "won't be notified", "/install/opencode.sh"}

// pushLineOnly fails unless w is the native push line with no install
// offer: what opencode without its plugin gets (§8.2).
func pushLineOnly(t *testing.T, what string, w watchOut, line string) {
	t.Helper()
	if !w.native || !w.wakes || !strings.HasPrefix(w.text, line) || !strings.Contains(w.text, "In opencode, that's automation.create with an event trigger") {
		t.Fatalf("%s: native_line %v, wakes %v: %q", what, w.native, w.wakes, w.text)
	}
	for _, s := range installOffer {
		if strings.Contains(w.text, s) {
			t.Fatalf("%s: an install offer (%q) beside the push line: %q", what, s, w.text)
		}
	}
}

// opencode watches again after a wake (its 2.3 watch_mcp_event ended with
// the first event; on 2.4 a model often ends its automation then, and each
// build or message is a new watch anyway): the push line (to subscribe,
// never "you're subscribed") and no install offer on every watch, while
// the stream it opened is open and after it closed.
// A push subscription reaches only the stream holding it, maybe another
// conversation's, so it never makes the line say the caller has one.
func TestPushOpencodeRewatch(t *testing.T) {
	c := coretest.Start(t)
	app := eventsApp(t, c, "chat")
	call := map[string]any{"user": "u1", "client": "opencode", "events_ext": true, "tool": "watch"}
	const line = `To be woken by chat.message with {"channel":"eng"}, subscribe to it (MCP Events).`
	first := sdkWatch(t, c, app, call, "chat.message", eng)
	pushLineOnly(t, "the first watch", first, line)
	s := c.OpenPush(t, app, pushBody("u1", nil, nil))
	mustActive(t, s)
	pushLineOnly(t, "while its stream is open", sdkWatch(t, c, app, call, "chat.message", eng), line)
	// The same conversation, by its key.
	again := mergeMap(mergeMap(map[string]any{}, call), map[string]any{"session": first.session})
	pushLineOnly(t, "by its key, while its stream is open", sdkWatch(t, c, app, again, "chat.message", eng), line)
	s.Close()
	waitFor(t, "released", func() bool {
		var live bool
		c.DB.InTenant(ctx, userTenant(t, c, "chat", "u1"), func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM stream_leases WHERE renewed_at IS NOT NULL)`).Scan(&live)
		})
		return !live
	})
	pushLineOnly(t, "after its stream closed", sdkWatch(t, c, app, again, "chat.message", eng), line)
	pushLineOnly(t, "a new conversation after it", sdkWatch(t, c, app, call, "chat.message", eng), line)
	// Another user's stream changes nothing for u1, and the reverse.
	o := c.OpenPush(t, app, pushBody("u2", nil, nil))
	mustActive(t, o)
	pushLineOnly(t, "beside another user's stream", sdkWatch(t, c, app, again, "chat.message", eng), line)
}

// Wakeshift's opencode plugin listening for the conversation wins.
// Once it connected the conversation from the push-line call's beacon,
// the next watch gets the bridged block (what the plugin delivers), no
// native line and no install offer; wakes stays true.
func TestPushOpencodePluginWins(t *testing.T) {
	c := coretest.Start(t)
	app := eventsApp(t, c, "chat")
	call := map[string]any{"user": "u1", "client": "opencode", "events_ext": true, "tool": "watch"}
	first := sdkWatch(t, c, app, call, "chat.message", eng)
	pushLineOnly(t, "before the plugin", first, "To be woken by chat.message")
	token, _ := first.meta[beacon.MetaKey].(string)
	if token == "" {
		t.Fatalf("the push-line call's block has no beacon for a plugin: %+v", first.meta)
	}
	const conversation = "ses_opencode_d3"
	cl, err := beacon.VerifyBound(token, beacon.Binding{Conversation: conversation, Unbound: true}, []string{c.URL}, coreKeys(t, c), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if code, out := redeemTicket(t, cl.Redeem, cl.Ticket, conversation); code != 200 {
		t.Fatalf("the plugin connects: %d %v", code, out)
	}
	key := mergeMap(mergeMap(map[string]any{}, call), map[string]any{"session": first.session})
	w := sdkWatch(t, c, app, key, "chat.message", eng)
	if w.native || !w.wakes || strings.Contains(w.text, "MCP Events") || !strings.Contains(w.text, "to this conversation: new messages from") {
		t.Fatalf("with the plugin listening: native_line %v, wakes %v: %q", w.native, w.wakes, w.text)
	}
	for _, s := range installOffer {
		if strings.Contains(w.text, s) {
			t.Fatalf("with the plugin listening, an install offer: %q", w.text)
		}
	}
}
