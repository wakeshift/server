package coretest_test

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/appapi"
	"github.com/wakeshift/server/internal/core"
	"github.com/wakeshift/server/internal/coretest"
	"github.com/wakeshift/server/internal/mcpevents"
	"github.com/wakeshift/server/pkg/wakeshift"
)

// Subscribe vouchers: a tool's result hands a native MCP Events client
// (ChatGPT) a voucher for exactly one webhook subscription to a
// tools-only event with those arguments; the client redeems it in
// events/subscribe. These are the core tests of its threat model
// and rollout.

var voucherInText = regexp.MustCompile(`wsv_[a-z2-7]{32}`)

// voucherHook is a native client's callback for these tests: it answers
// verification per path (echo, a status, or a delay first), and records
// what it receives per path.
type voucherHook struct {
	srv      *httptest.Server
	mu       sync.Mutex
	verifies map[string]int
	events   map[string][]map[string]any
	delay    map[string]time.Duration
	status   map[string]int
}

func newVoucherHook(t *testing.T) *voucherHook {
	h := &voucherHook{verifies: map[string]int{}, events: map[string][]map[string]any{}, delay: map[string]time.Duration{}, status: map[string]int{}}
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		p := strings.TrimPrefix(r.URL.Path, "/")
		h.mu.Lock()
		delay, status := h.delay[p], h.status[p]
		if body["type"] == "verification" {
			h.verifies[p]++
		} else if body["type"] == nil {
			h.events[p] = append(h.events[p], body)
		}
		h.mu.Unlock()
		if body["type"] == "verification" {
			time.Sleep(delay)
			if status != 0 {
				w.WriteHeader(status)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"challenge": body["challenge"]})
		}
	}))
	t.Cleanup(h.srv.Close)
	return h
}

func (h *voucherHook) url(p string) string { return h.srv.URL + "/" + p }

func (h *voucherHook) set(p string, delay time.Duration, status int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.delay[p], h.status[p] = delay, status
}

func (h *voucherHook) verified(p string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.verifies[p]
}

// texts are the events' payload texts received at p, in order.
func (h *voucherHook) texts(p string) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for _, e := range h.events[p] {
		d, _ := e["data"].(map[string]any)
		out = append(out, fmt.Sprint(d["text"]))
	}
	return out
}

func voucherSecret() string {
	b := make([]byte, 32)
	rand.Read(b)
	return "whsec_" + base64.StdEncoding.EncodeToString(b)
}

func delivery(url, secret string) map[string]any {
	return map[string]any{"mode": "webhook", "url": url, "secret": secret}
}

// voucherApp is an app whose chat.message (and chat.other) are tools-only.
func voucherApp(t *testing.T, c *coretest.Core, id string) *wakeshift.Client {
	t.Helper()
	app := eventsApp(t, c, id)
	if got, code := publishManifest(t, c, app, false, map[string]string{"chat.message": "tools", "chat.other": "tools"}); code != 200 {
		t.Fatalf("manifest: %d %+v", code, got)
	}
	return app
}

// anonCall is a tool call without a verified user, to the app's tool
// watch_job, on a transport session (anonTransport: an anonymous voucher
// is handed out only bound to one); unboundCall is one on none (ChatGPT's,
// whose protocol has no sessions); userCall is one with user.
func anonCall() map[string]any {
	return map[string]any{"client": "openai-mcp", "tool": "watch_job", "transport_session": anonTransport}
}
func unboundCall() map[string]any { return map[string]any{"client": "openai-mcp", "tool": "watch_job"} }

var anonTransport = strings.Repeat("ef", 32)

func userCall(user string) map[string]any {
	return map[string]any{"client": "openai-mcp", "tool": "watch_job", "user": user}
}

var eng = map[string]any{"channel": "eng"}

// handOut is a tool call's /calls/addition with a native line for event
// with args (and options), returning the text and the voucher in it ("" if
// none).
func handOut(t *testing.T, c *coretest.Core, app *wakeshift.Client, call map[string]any, event string, args map[string]any, opts map[string]any) (string, string) {
	t.Helper()
	line := map[string]any{"name": event, "arguments": args}
	for k, v := range opts {
		line[k] = v
	}
	var out struct{ Text string }
	if code, e := appAPI(t, c, app, "/api/apps/v1/calls/addition", map[string]any{"call": call, "native": []any{line}}, &out); code != 200 {
		t.Fatalf("addition: %d %s", code, e)
	}
	return out.Text, voucherInText.FindString(out.Text)
}

// redeemReq is an events/subscribe with a voucher, as the SDK passes it on
// for ChatGPT; extra is merged into the body.
func redeemReq(call map[string]any, event string, args map[string]any, voucher string, dl map[string]any, extra map[string]any) map[string]any {
	body := map[string]any{"call": call, "name": event, "arguments": args, "delivery": dl, "voucher": voucher, "dialect": "chatgpt"}
	for k, v := range extra {
		body[k] = v
	}
	return body
}

func redeem(t *testing.T, c *coretest.Core, app *wakeshift.Client, body map[string]any) (int, json.RawMessage) {
	t.Helper()
	code, raw, err := appPostRaw(c, app, "/api/apps/v1/events/subscribe", body)
	if err != nil {
		t.Fatal(err)
	}
	return code, raw
}

// redeemed decodes a redemption's answer.
type redeemed struct {
	ID            string `json:"id"`
	Created       bool   `json:"created"`
	Via           string `json:"via"`
	Tool          string `json:"tool"`
	RefreshBefore time.Time
}

func (r *redeemed) UnmarshalJSON(b []byte) error {
	var v struct {
		ID            string    `json:"id"`
		Created       bool      `json:"created"`
		Via           string    `json:"via"`
		Tool          string    `json:"tool"`
		RefreshBefore time.Time `json:"refresh_before"`
	}
	err := json.Unmarshal(b, &v)
	*r = redeemed{v.ID, v.Created, v.Via, v.Tool, v.RefreshBefore}
	return err
}

func mustRedeem(t *testing.T, c *coretest.Core, app *wakeshift.Client, body map[string]any) redeemed {
	t.Helper()
	code, raw := redeem(t, c, app, body)
	if code != 200 {
		t.Fatalf("redeem: %d %s", code, raw)
	}
	var r redeemed
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatal(err)
	}
	return r
}

// badVoucher is every voucher refusal, byte for byte.
const badVoucher = `{"error":{"code":"bad_voucher","message":"This voucher can't subscribe to this event: it was used, it expired, or it's for other arguments."}}`

func wantBadVoucher(t *testing.T, what string, code int, raw json.RawMessage) {
	t.Helper()
	if code != 403 || string(raw) != badVoucher {
		t.Fatalf("%s: %d %s", what, code, raw)
	}
}

// voucherCol reads a column of the voucher row for token.
func voucherCol[T any](t *testing.T, c *coretest.Core, token, col string) T {
	t.Helper()
	var v T
	if err := c.DB.Pool.QueryRow(ctx, `SELECT `+col+` FROM subscribe_vouchers WHERE token_hash = sha256(convert_to($1::text, 'UTF8'))`, token).Scan(&v); err != nil {
		t.Fatalf("voucher %s: %v", col, err)
	}
	return v
}

// refusedReasons are the reasons the app's log gives for refused vouchers.
func refusedReasons(t *testing.T, c *coretest.Core, app string) []string {
	t.Helper()
	rows, _ := c.DB.Pool.Query(ctx, `SELECT detail->>'reason' FROM app_log WHERE app_id = $1 AND action = 'app.voucher_refused' ORDER BY id`, app)
	out, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func tenantCount(t *testing.T, c *coretest.Core, app string) int {
	t.Helper()
	var n int
	c.DB.Pool.QueryRow(ctx, `SELECT count(*) FROM tenants WHERE app_id = $1`, app).Scan(&n)
	return n
}

// A ChatGPT conversation that subscribes natively (MCP Events: web Work
// chats) gets the native lines byte for byte, the voucher's and an open
// event's, and none of the can't-wake note ChatGPT gets for what an app's
// tools subscribe.
func TestChatGPTNativeLinesCarryNoCantWakeNote(t *testing.T) {
	eventsTestSetup(t)
	c := coretest.Start(t)
	app := voucherApp(t, c, "chat")
	if got, code := publishManifest(t, c, app, false, map[string]string{"chat.other": "any"}); code != 200 {
		t.Fatalf("opening: %+v", got)
	}
	var out struct{ Text string }
	if code, e := appAPI(t, c, app, "/api/apps/v1/calls/addition", map[string]any{"call": anonCall(), "native": []any{
		map[string]any{"name": "chat.message", "arguments": eng},
		map[string]any{"name": "chat.other", "arguments": eng},
	}}, &out); code != 200 {
		t.Fatalf("addition: %d %s", code, e)
	}
	tok := voucherInText.FindString(out.Text)
	want := "To be woken by chat.message with {\"channel\":\"eng\"}, subscribe to it (MCP Events) with wakeshift_voucher set to " + tok +
		" in its arguments. It works once, for these arguments. To be woken by chat.other with {\"channel\":\"eng\"}, subscribe to it (MCP Events)."
	if tok == "" || out.Text != want {
		t.Fatalf("native lines: %q", out.Text)
	}
	if strings.Contains(out.Text, appapi.ChatGPTNote) || strings.Contains(out.Text, "watching") {
		t.Fatalf("native lines carry the can't-wake note: %q", out.Text)
	}
}

// The hand-out (§6.1): a native line for a tools-only event becomes the
// voucher line, the token stored only as its hash, with no workspace made;
// every "otherwise" renders no line and never fails the addition; open
// events keep today's line; a user with a live voucher subscription is
// told so instead.
func TestVoucherHandOut(t *testing.T) {
	eventsTestSetup(t)
	c := coretest.Start(t)
	app := voucherApp(t, c, "chat")
	before := tenantCount(t, c, "chat")

	text, tok := handOut(t, c, app, anonCall(), "chat.message", eng, nil)
	want := "To be woken by chat.message with {\"channel\":\"eng\"}, subscribe to it (MCP Events) with wakeshift_voucher set to " + tok + " in its arguments. It works once, for these arguments."
	if tok == "" || text != want {
		t.Fatalf("the line: %q", text)
	}
	if n := tenantCount(t, c, "chat"); n != before {
		t.Fatalf("a hand-out made a workspace: %d → %d", before, n)
	}
	// Stored as a hash: no column holds the token.
	var leaked int
	c.DB.Pool.QueryRow(ctx, `SELECT count(*) FROM subscribe_vouchers v WHERE row_to_json(v)::text LIKE '%' || $1 || '%'`, tok[4:]).Scan(&leaked)
	if leaked != 0 || voucherCol[string](t, c, tok, "tool") != "watch_job" || voucherCol[*string](t, c, tok, "app_user") != nil {
		t.Fatalf("stored: leaked %d", leaked)
	}
	if exp := voucherCol[time.Time](t, c, tok, "expires_at"); time.Until(exp) < appapi.VoucherTTL-time.Minute || time.Until(exp) > appapi.VoucherTTL+time.Minute {
		t.Fatalf("expires at %v", exp)
	}

	// A call that names no tool, invalid arguments, bad options, a user ID
	// that isn't one: no line, the addition still succeeds, the app's log
	// says why.
	noTool := anonCall()
	delete(noTool, "tool")
	for name, tc := range map[string]struct {
		call map[string]any
		args map[string]any
		opts map[string]any
	}{
		"no tool":       {noTool, eng, nil},
		"builtin":       {map[string]any{"client": "openai-mcp", "tool": "subscribe_events"}, eng, nil},
		"bad arguments": {anonCall(), map[string]any{"room": "eng"}, nil},
		"bad ttl":       {anonCall(), eng, map[string]any{"ttl": 5}},
		"bad replay":    {anonCall(), eng, map[string]any{"replay": "48h"}},
	} {
		if got, v := handOut(t, c, app, tc.call, "chat.message", tc.args, tc.opts); v != "" || got != "" {
			t.Fatalf("%s: %q", name, got)
		}
	}
	var notIssued []string
	rows, _ := c.DB.Pool.Query(ctx, `SELECT DISTINCT detail->>'reason' FROM app_log WHERE app_id = 'chat' AND action = 'app.voucher_not_issued' ORDER BY 1`)
	notIssued, _ = pgx.CollectRows(rows, pgx.RowTo[string])
	if fmt.Sprint(notIssued) != "[arguments no_tool options]" {
		t.Fatalf("logged: %v", notIssued)
	}

	// An open event keeps today's line, with no voucher.
	if got, code := publishManifest(t, c, app, false, map[string]string{"chat.other": "any"}); code != 200 {
		t.Fatalf("opening: %+v", got)
	}
	if got, v := handOut(t, c, app, anonCall(), "chat.other", eng, nil); v != "" || got != "To be woken by chat.other with {\"channel\":\"eng\"}, subscribe to it (MCP Events)." {
		t.Fatalf("open event: %q", got)
	}

	// A user with a live voucher subscription is told so, and gets no
	// voucher.
	hook := newVoucherHook(t)
	_, utok := handOut(t, c, app, userCall("ada"), "chat.message", eng, nil)
	if got := voucherCol[*string](t, c, utok, "app_user"); got == nil || *got != "ada" {
		t.Fatalf("user-bound: %v", got)
	}
	mustRedeem(t, c, app, redeemReq(userCall("ada"), "chat.message", eng, utok, delivery(hook.url("ada"), voucherSecret()), nil))
	if got, v := handOut(t, c, app, userCall("ada"), "chat.message", eng, nil); v != "" || got != "You're subscribed to chat.message with {\"channel\":\"eng\"}: its events reach you." {
		t.Fatalf("subscribed: %q", got)
	}
}

func mergeMap(a, b map[string]any) map[string]any {
	for k, v := range b {
		a[k] = v
	}
	return a
}

// The pools (§6.1, T10): 50 unused per user, 1,000 anonymous per app,
// independent, counted without entering any workspace; the anonymous cap
// is logged at most once a minute with a count; expired vouchers don't
// count.
func TestVoucherCaps(t *testing.T) {
	c := coretest.Start(t)
	app := voucherApp(t, c, "chat")
	seed := func(user *string, n int, expired bool) {
		t.Helper()
		exp := "now() + interval '1 hour'"
		if expired {
			exp = "now() - interval '1 second'"
		}
		if _, err := c.DB.Pool.Exec(ctx, `INSERT INTO subscribe_vouchers (token_hash, app_id, event, arguments_key, app_user, tool, expires_at)
			SELECT sha256(convert_to(gen_random_uuid()::text, 'UTF8')), 'chat', 'chat.message', 'k', $1, 'watch_job', `+exp+` FROM generate_series(1, $2)`, user, n); err != nil {
			t.Fatal(err)
		}
	}
	ada := "ada"
	seed(nil, 999, false)
	seed(nil, 50, true) // expired: not counted
	if _, v := handOut(t, c, app, anonCall(), "chat.message", eng, nil); v == "" {
		t.Fatal("the 1,000th anonymous voucher wasn't handed out")
	}
	for i := range 2 {
		if text, v := handOut(t, c, app, anonCall(), "chat.message", eng, nil); v != "" || text != "" {
			t.Fatalf("the %d anonymous voucher past the cap: %q", 1001+i, text)
		}
	}
	// Signed-in users still get theirs.
	seed(&ada, 49, false)
	if _, v := handOut(t, c, app, userCall("ada"), "chat.message", eng, nil); v == "" {
		t.Fatal("ada's 50th voucher wasn't handed out while the anonymous pool is full")
	}
	if text, v := handOut(t, c, app, userCall("ada"), "chat.message", eng, nil); v != "" || text != "" {
		t.Fatalf("ada's 51st: %q", text)
	}
	if _, v := handOut(t, c, app, userCall("bob"), "chat.message", eng, nil); v == "" {
		t.Fatal("bob's first voucher wasn't handed out")
	}
	var anonLogs, userLogs int
	var count float64
	c.DB.Pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE detail->>'reason' = 'anonymous_cap'), count(*) FILTER (WHERE detail->>'reason' = 'user_cap'),
		coalesce(max((detail->>'count')::float8) FILTER (WHERE detail->>'reason' = 'anonymous_cap'), 0)
		FROM app_log WHERE app_id = 'chat' AND action = 'app.voucher_not_issued'`).Scan(&anonLogs, &userLogs, &count)
	if anonLogs != 1 || count != 1 || userLogs != 1 {
		t.Fatalf("logged: anonymous %d (count %v), user %d", anonLogs, count, userLogs)
	}
}

// An anonymous redemption, end to end (§6.2, §11.3), on a transport
// session: an emit between the hand-out and the subscribe is replayed
// exactly once and nothing older is; the subscription is the tool's (via
// tool:<name>) in a new anonymous workspace, audited there; later emits
// are delivered; the refresh rules (same URL and secret; no rotation; no
// new URL; not without the voucher) and unsubscribing by voucher and URL.
//
// Anonymous without a transport session (ChatGPT's, whose protocol has
// none), a voucher would be bound to nothing: a bearer token for whoever
// reads it in the conversation's text. So none is handed out (logged as
// no_session), and one from before that is refused, as push refuses it.
func TestVoucherRedeemAnonymous(t *testing.T) {
	eventsTestSetup(t)
	c := coretest.Start(t)
	app := voucherApp(t, c, "chat")
	hook := newVoucherHook(t)

	if text, v := handOut(t, c, app, unboundCall(), "chat.message", eng, nil); v != "" || text != "" {
		t.Fatalf("anonymous without a transport session: %q", text)
	}
	var reason string
	c.DB.Pool.QueryRow(ctx, `SELECT detail->>'reason' FROM app_log WHERE app_id = 'chat' AND action = 'app.voucher_not_issued'`).Scan(&reason)
	if reason != "no_session" {
		t.Fatalf("not issued, logged as %q", reason)
	}
	_, legacy := handOut(t, c, app, anonCall(), "chat.message", eng, nil)
	if _, err := c.DB.Pool.Exec(ctx, `UPDATE subscribe_vouchers SET transport_session = NULL WHERE token_hash = sha256(convert_to($1::text, 'UTF8'))`, legacy); err != nil {
		t.Fatal(err)
	}
	for _, call := range []map[string]any{unboundCall(), anonCall(), userCall("ada")} {
		code, raw := redeem(t, c, app, redeemReq(call, "chat.message", eng, legacy, delivery(hook.url("legacy"), voucherSecret()), nil))
		wantBadVoucher(t, "an unbound anonymous voucher", code, raw)
	}
	if hook.verified("legacy") != 0 {
		t.Fatal("an unbound voucher's callback was verified")
	}

	emitChat(t, c, app, "old") // before the hand-out
	time.Sleep(20 * time.Millisecond)
	_, tok := handOut(t, c, app, anonCall(), "chat.message", eng, nil)
	before := tenantCount(t, c, "chat")
	emitChat(t, c, app, "between")
	secret := voucherSecret()
	dl := delivery(hook.url("a"), secret)
	r := mustRedeem(t, c, app, redeemReq(anonCall(), "chat.message", eng, tok, dl, map[string]any{"ttl_ms": 3600000}))
	if !r.Created || r.Via != "tool:watch_job" || r.Tool != "watch_job" || hook.verified("a") != 1 {
		t.Fatalf("redeemed: %+v, verified %d", r, hook.verified("a"))
	}
	if n := tenantCount(t, c, "chat"); n != before+1 {
		t.Fatalf("workspaces: %d → %d", before, n)
	}
	tenant := voucherCol[string](t, c, tok, "tenant_id::text")
	var external string
	c.DB.Pool.QueryRow(ctx, `SELECT external_id FROM tenants WHERE id = $1`, tenant).Scan(&external)
	var via, principal, dest string
	c.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT via, principal, destination FROM event_subscriptions`).Scan(&via, &principal, &dest)
	})
	if !strings.HasPrefix(external, "~conversation:") || principal != external || via != "tool:watch_job" || dest != "webhook" {
		t.Fatalf("recorded: %s %s %s %s", external, principal, via, dest)
	}
	if got := auditActions(t, c, tenant); fmt.Sprint(got) != "[event_subscription.created event_subscription.voucher_redeemed]" {
		t.Fatalf("audit: %v", got)
	}
	waitFor(t, "the replay", func() bool { return len(hook.texts("a")) == 1 })
	emitChat(t, c, app, "after")
	waitFor(t, "a live event", func() bool { return len(hook.texts("a")) == 2 })
	time.Sleep(300 * time.Millisecond)
	if got := hook.texts("a"); fmt.Sprint(got) != "[zqbetweenzq zqafterzq]" {
		t.Fatalf("delivered: %v", got)
	}

	// A voucher handed out after those emits replays none of them.
	_, later := handOut(t, c, app, anonCall(), "chat.message", eng, nil)
	mustRedeem(t, c, app, redeemReq(anonCall(), "chat.message", eng, later, delivery(hook.url("later"), voucherSecret()), nil))
	time.Sleep(400 * time.Millisecond)
	if got := hook.texts("later"); len(got) != 0 {
		t.Fatalf("replayed from before its hand-out: %v", got)
	}

	// A refresh: same URL and secret.
	r = mustRedeem(t, c, app, redeemReq(anonCall(), "chat.message", eng, tok, dl, map[string]any{"ttl_ms": 3600000}))
	if r.Created || r.Via != "tool:watch_job" || hook.verified("a") != 1 {
		t.Fatalf("refresh: %+v", r)
	}
	// No rotation, no new URL, not without the voucher.
	code, raw := redeem(t, c, app, redeemReq(anonCall(), "chat.message", eng, tok, delivery(hook.url("a"), voucherSecret()), nil))
	wantBadVoucher(t, "rotation", code, raw)
	code, raw = redeem(t, c, app, redeemReq(anonCall(), "chat.message", eng, tok, delivery(hook.url("b"), secret), nil))
	wantBadVoucher(t, "a new URL", code, raw)
	if hook.verified("b") != 0 {
		t.Fatal("a used voucher's new URL was verified")
	}
	if code, e := appAPI(t, c, app, "/api/apps/v1/events/subscribe", map[string]any{"call": anonCall(), "name": "chat.message", "arguments": eng,
		"delivery": dl, "dialect": "chatgpt"}, nil); code != 403 || e != "tools_only" {
		t.Fatalf("a refresh without the voucher: %d %s", code, e)
	}
	// A poll with the voucher, from a valid cursor: refused as tools-only (T5).
	if code, e := appAPI(t, c, app, "/api/apps/v1/events/poll", map[string]any{"call": anonCall(), "name": "chat.message", "arguments": eng,
		"cursor": "c1.0", "voucher": tok}, nil); code != 403 || e != "tools_only" {
		t.Fatalf("a poll with a voucher: %d %s", code, e)
	}

	// Unsubscribing: another URL ends nothing; the voucher and the URL end it.
	var out struct{ Ended bool }
	unsub := func(url string) bool {
		t.Helper()
		out.Ended = false
		if code, e := appAPI(t, c, app, "/api/apps/v1/events/unsubscribe", map[string]any{"call": anonCall(), "name": "chat.message", "arguments": eng,
			"delivery": map[string]any{"url": url}, "voucher": tok}, &out); code != 200 {
			t.Fatalf("unsubscribe: %d %s", code, e)
		}
		return out.Ended
	}
	if unsub(hook.url("b")) {
		t.Fatal("ended without the callback URL")
	}
	if !unsub(hook.url("a")) {
		t.Fatal("not ended with the voucher and the URL")
	}
	code, raw = redeem(t, c, app, redeemReq(anonCall(), "chat.message", eng, tok, dl, nil))
	wantBadVoucher(t, "refreshing an ended subscription", code, raw)
	if got := refusedReasons(t, c, "chat"); fmt.Sprint(got) != "[no_session no_session no_session rotation used used]" {
		t.Fatalf("logged: %v", got)
	}
}

// A subscription made with an anonymous voucher bound to no transport
// session (handed out before those stopped) isn't renewed through it: the
// refresh is refused as a redemption would be (bad_voucher, logged as
// no_session), the subscription stays as it was until its ttl, and the
// voucher reaches nothing new.
func TestVoucherUnboundRefreshRefused(t *testing.T) {
	eventsTestSetup(t)
	c := coretest.Start(t)
	app := voucherApp(t, c, "chat")
	hook := newVoucherHook(t)
	_, tok := handOut(t, c, app, anonCall(), "chat.message", eng, nil)
	dl := delivery(hook.url("a"), voucherSecret())
	r := mustRedeem(t, c, app, redeemReq(anonCall(), "chat.message", eng, tok, dl, map[string]any{"ttl_ms": 3600000}))
	if !r.Created {
		t.Fatalf("redeemed: %+v", r)
	}
	liveUntil := func() (lu time.Time) {
		t.Helper()
		c.DB.Pool.QueryRow(ctx, `SELECT live_until FROM subscribe_vouchers WHERE token_hash = sha256(convert_to($1::text, 'UTF8'))`, tok).Scan(&lu)
		return lu
	}
	// As one handed out before: bound to nothing.
	if _, err := c.DB.Pool.Exec(ctx, `UPDATE subscribe_vouchers SET transport_session = NULL WHERE token_hash = sha256(convert_to($1::text, 'UTF8'))`, tok); err != nil {
		t.Fatal(err)
	}
	before := liveUntil()
	for _, call := range []map[string]any{unboundCall(), anonCall(), userCall("ada")} {
		code, raw := redeem(t, c, app, redeemReq(call, "chat.message", eng, tok, dl, map[string]any{"ttl_ms": 3600000}))
		wantBadVoucher(t, "refreshing with an unbound voucher", code, raw)
	}
	if got := refusedReasons(t, c, "chat"); fmt.Sprint(got) != "[no_session no_session no_session]" {
		t.Fatalf("logged: %v", got)
	}
	if after := liveUntil(); !after.Equal(before) {
		t.Fatalf("renewed: %v → %v", before, after)
	}
	if hook.verified("a") != 1 {
		t.Fatalf("verified %d times", hook.verified("a"))
	}
}

// Every refusal is the same answer, byte for byte (T12), and the app's
// log says which (T1, T3, T4).
func TestVoucherRefusals(t *testing.T) {
	eventsTestSetup(t)
	c := coretest.Start(t)
	app := voucherApp(t, c, "chat")
	other := voucherApp(t, c, "other")
	hook := newVoucherHook(t)
	n := 0
	dl := func() map[string]any { n++; return delivery(hook.url(fmt.Sprintf("r%d", n)), voucherSecret()) }
	refused := func(what string, a *wakeshift.Client, body map[string]any) {
		t.Helper()
		code, raw := redeem(t, c, a, body)
		wantBadVoucher(t, what, code, raw)
	}
	both := map[string]any{"channel": "eng", "from": "ada"}

	_, anon := handOut(t, c, app, anonCall(), "chat.message", eng, nil)
	_, withFrom := handOut(t, c, app, anonCall(), "chat.message", both, nil)
	_, ada := handOut(t, c, app, userCall("ada"), "chat.message", eng, nil)
	_, otherApps := handOut(t, c, other, anonCall(), "chat.message", eng, nil)
	_, expired := handOut(t, c, app, anonCall(), "chat.message", eng, nil)
	c.DB.Pool.Exec(ctx, `UPDATE subscribe_vouchers SET expires_at = now() - interval '1 millisecond' WHERE token_hash = sha256(convert_to($1::text, 'UTF8'))`, expired)
	sessionHash := strings.Repeat("ab", 32)
	sessCall := mergeMap(anonCall(), map[string]any{"transport_session": sessionHash})
	_, bound := handOut(t, c, app, sessCall, "chat.message", eng, nil)
	if voucherCol[*string](t, c, bound, "transport_session") == nil {
		t.Fatal("the transport session wasn't recorded")
	}

	refused("malformed", app, redeemReq(anonCall(), "chat.message", eng, "wsv_NOTONE", dl(), nil))
	refused("unknown", app, redeemReq(anonCall(), "chat.message", eng, "wsv_"+strings.Repeat("a", 32), dl(), nil))
	refused("another app's", app, redeemReq(anonCall(), "chat.message", eng, otherApps, dl(), nil))
	refused("expired", app, redeemReq(anonCall(), "chat.message", eng, expired, dl(), nil))
	refused("other arguments", app, redeemReq(anonCall(), "chat.message", map[string]any{"channel": "ops"}, anon, dl(), nil))
	refused("an extra argument", app, redeemReq(anonCall(), "chat.message", both, anon, dl(), nil))
	refused("a missing argument", app, redeemReq(anonCall(), "chat.message", eng, withFrom, dl(), nil))
	refused("another event", app, redeemReq(anonCall(), "chat.other", eng, anon, dl(), nil))
	refused("user A's, by user B", app, redeemReq(userCall("bob"), "chat.message", eng, ada, dl(), nil))
	refused("user A's, by no user", app, redeemReq(anonCall(), "chat.message", eng, ada, dl(), nil))
	refused("a bound one, without its session", app, redeemReq(anonCall(), "chat.message", eng, bound, dl(), nil))
	refused("a bound one, another session", app, redeemReq(mergeMap(anonCall(), map[string]any{"transport_session": strings.Repeat("cd", 32)}), "chat.message", eng, bound, dl(), nil))
	// Case and punctuation variants of the argument aren't stripped: they
	// fail closed (the schema has no such argument here, so as bad
	// arguments; with an open schema, as other arguments).
	for _, k := range []string{"Wakeshift_Voucher", "wakeshift-voucher"} {
		code, raw := redeem(t, c, app, redeemReq(anonCall(), "chat.message", map[string]any{"channel": "eng", k: anon}, anon, dl(), nil))
		if code == 200 {
			t.Fatalf("%s: accepted %s", k, raw)
		}
	}
	// No hook was asked anything.
	for i := 1; i <= n; i++ {
		if hook.verified(fmt.Sprintf("r%d", i)) != 0 {
			t.Fatalf("a refused redemption verified its callback (r%d)", i)
		}
	}
	// The right ones work: reordered keys, the session, user A.
	mustRedeem(t, c, app, redeemReq(anonCall(), "chat.message", map[string]any{"from": "ada", "channel": "eng"}, withFrom, dl(), nil))
	mustRedeem(t, c, app, redeemReq(sessCall, "chat.message", eng, bound, dl(), nil))
	mustRedeem(t, c, app, redeemReq(userCall("ada"), "chat.message", eng, ada, dl(), nil))
	// Used, with another URL; and an orphan (its workspace forgotten).
	refused("used", app, redeemReq(userCall("ada"), "chat.message", eng, ada, dl(), nil))
	if ok, err := app.ForgetUser(ctx, "ada"); err != nil || !ok {
		t.Fatalf("forget: %v %v", ok, err)
	}
	refused("orphaned", app, redeemReq(userCall("ada"), "chat.message", eng, ada, dl(), nil))

	if got := refusedReasons(t, c, "chat"); fmt.Sprint(got) != "[format expired binding binding binding binding user user session session used]" {
		t.Fatalf("logged: %v", got)
	}
	// Malformed and unknown vouchers (another app's, an orphan's) are what
	// anyone can send: logged once a minute, with a count.
	var probes float64
	c.DB.Pool.QueryRow(ctx, `SELECT (detail->>'count')::float8 FROM app_log WHERE app_id = 'chat' AND action = 'app.voucher_refused' AND detail->>'reason' = 'format'`).Scan(&probes)
	if probes != 1 {
		t.Fatalf("the first probe's count: %v", probes)
	}
	var pending int
	c.DB.Pool.QueryRow(ctx, `SELECT probe_refused FROM subscribe_voucher_counters WHERE app_id = 'chat'`).Scan(&pending)
	if pending != 3 {
		t.Fatalf("probes since: %d", pending)
	}
	var leaked int
	c.DB.Pool.QueryRow(ctx, `SELECT count(*) FROM app_log WHERE detail::text LIKE '%wsv_%'`).Scan(&leaked)
	if leaked != 0 {
		t.Fatalf("a token in the app's log: %d", leaked)
	}
}

// One voucher never makes two subscriptions (T2): 50 redemptions at once
// with different callbacks make exactly one, 50 times over; a redemption
// whose claim lapsed while it verified loses to the one that took it.
func TestVoucherConcurrentRedemption(t *testing.T) {
	eventsTestSetup(t)
	c := coretest.Start(t)
	app := voucherApp(t, c, "chat")
	hook := newVoucherHook(t)
	for iter := range 50 {
		user := fmt.Sprintf("u%d", iter)
		_, tok := handOut(t, c, app, userCall(user), "chat.message", map[string]any{"channel": fmt.Sprintf("c%d", iter)}, nil)
		var wg sync.WaitGroup
		var ok atomic.Int32
		errs := make(chan string, 50)
		for i := range 50 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				code, raw, err := appPostRaw(c, app, "/api/apps/v1/events/subscribe", redeemReq(userCall(user), "chat.message",
					map[string]any{"channel": fmt.Sprintf("c%d", iter)}, tok, delivery(hook.url(fmt.Sprintf("i%d-%d", iter, i)), voucherSecret()), nil))
				switch {
				case err != nil:
					errs <- err.Error()
				case code == 200:
					ok.Add(1)
				case code != 403 || string(raw) != badVoucher:
					errs <- fmt.Sprintf("%d %s", code, raw)
				}
			}()
		}
		wg.Wait()
		close(errs)
		for e := range errs {
			t.Fatalf("iteration %d: %s", iter, e)
		}
		if ok.Load() != 1 {
			t.Fatalf("iteration %d: %d subscriptions", iter, ok.Load())
		}
		// The claim: one voucher's callbacks are verified one at a time,
		// so only the redemption that took it verified one.
		verified := 0
		for i := range 50 {
			verified += hook.verified(fmt.Sprintf("i%d-%d", iter, i))
		}
		if verified != 1 {
			t.Fatalf("iteration %d: %d callbacks verified", iter, verified)
		}
	}
	var subs int
	for iter := range 50 {
		var n int
		c.DB.InTenant(ctx, userTenant(t, c, "chat", fmt.Sprintf("u%d", iter)), func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM event_subscriptions`).Scan(&n)
		})
		subs += n
	}
	if subs != 50 {
		t.Fatalf("%d subscriptions, want 50", subs)
	}

	// A stalls past its claim; B takes the voucher and makes the
	// subscription; A's creation is refused.
	claim := appapi.VoucherClaimFor
	appapi.VoucherClaimFor = 300 * time.Millisecond
	t.Cleanup(func() { appapi.VoucherClaimFor = claim })
	_, tok := handOut(t, c, app, userCall("bob"), "chat.message", eng, nil)
	hook.set("slow", 1200*time.Millisecond, 0)
	done := make(chan json.RawMessage)
	go func() {
		_, raw, _ := appPostRaw(c, app, "/api/apps/v1/events/subscribe", redeemReq(userCall("bob"), "chat.message", eng, tok, delivery(hook.url("slow"), voucherSecret()), nil))
		done <- raw
	}()
	time.Sleep(600 * time.Millisecond) // A's claim has lapsed; it's still verifying
	mustRedeem(t, c, app, redeemReq(userCall("bob"), "chat.message", eng, tok, delivery(hook.url("fast"), voucherSecret()), nil))
	if raw := <-done; string(raw) != badVoucher {
		t.Fatalf("the stalled redemption: %s", raw)
	}
	c.DB.InTenant(ctx, userTenant(t, c, "chat", "bob"), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM event_subscriptions`).Scan(&subs)
	})
	if subs != 1 {
		t.Fatalf("bob: %d subscriptions", subs)
	}

	// A's claim lapses and B takes it, but B is still verifying when A
	// finishes: A's creation is refused (the claim is B's), and B's makes
	// the one subscription.
	_, tok = handOut(t, c, app, userCall("cat"), "chat.message", eng, nil)
	hook.set("slowA", 1000*time.Millisecond, 0)
	hook.set("slowB", 1500*time.Millisecond, 0)
	doneA, doneB := make(chan json.RawMessage), make(chan json.RawMessage)
	go func() {
		_, raw, _ := appPostRaw(c, app, "/api/apps/v1/events/subscribe", redeemReq(userCall("cat"), "chat.message", eng, tok, delivery(hook.url("slowA"), voucherSecret()), nil))
		doneA <- raw
	}()
	time.Sleep(500 * time.Millisecond)
	go func() {
		_, raw, _ := appPostRaw(c, app, "/api/apps/v1/events/subscribe", redeemReq(userCall("cat"), "chat.message", eng, tok, delivery(hook.url("slowB"), voucherSecret()), nil))
		doneB <- raw
	}()
	if raw := <-doneA; string(raw) != badVoucher {
		t.Fatalf("A, its claim taken: %s", raw)
	}
	if raw := <-doneB; !strings.Contains(string(raw), `"created":true`) {
		t.Fatalf("B: %s", raw)
	}
}

// Expiry is judged once, at the claim, by the database's clock (T8); a
// voucher's callback may fail verification three times, and the fourth
// try is refused (T13); an anonymous redeemer gets one generic reason
// whatever happened; private, loopback, link-local and metadata targets
// are refused before any packet; the per-app verification budget holds.
func TestVoucherExpiryAndVerification(t *testing.T) {
	eventsTestSetup(t)
	c := coretest.Start(t)
	app := voucherApp(t, c, "chat")
	hook := newVoucherHook(t)
	setExpiry := func(tok, at string) {
		if _, err := c.DB.Pool.Exec(ctx, `UPDATE subscribe_vouchers SET expires_at = `+at+` WHERE token_hash = sha256(convert_to($1::text, 'UTF8'))`, tok); err != nil {
			t.Fatal(err)
		}
	}
	// Claimed a second before it expires, verified slowly: it succeeds.
	_, tok := handOut(t, c, app, anonCall(), "chat.message", eng, nil)
	setExpiry(tok, "now() + interval '1 second'")
	hook.set("slow", 2*time.Second, 0)
	mustRedeem(t, c, app, redeemReq(anonCall(), "chat.message", eng, tok, delivery(hook.url("slow"), voucherSecret()), nil))
	// A millisecond after: refused.
	_, tok = handOut(t, c, app, anonCall(), "chat.message", eng, nil)
	setExpiry(tok, "now() - interval '1 millisecond'")
	code, raw := redeem(t, c, app, redeemReq(anonCall(), "chat.message", eng, tok, delivery(hook.url("late"), voucherSecret()), nil))
	wantBadVoucher(t, "expired", code, raw)

	// Failed verifications: one generic answer whatever the cause; a retry
	// after a failure succeeds; three failures spend it.
	generic := `{"error":{"code":"callback_failed","message":"callback verification failed","reason":"callback_failed"}}`
	_, tok = handOut(t, c, app, anonCall(), "chat.message", eng, nil)
	hook.set("e404", 0, 404)
	hook.set("e500", 0, 500)
	for _, p := range []string{"e404", "e500"} {
		code, raw := redeem(t, c, app, redeemReq(anonCall(), "chat.message", eng, tok, delivery(hook.url(p), voucherSecret()), nil))
		if code != 400 || string(raw) != generic {
			t.Fatalf("%s: %d %s", p, code, raw)
		}
	}
	mustRedeem(t, c, app, redeemReq(anonCall(), "chat.message", eng, tok, delivery(hook.url("ok"), voucherSecret()), nil))

	_, tok = handOut(t, c, app, anonCall(), "chat.message", eng, nil)
	// Private, loopback, link-local, metadata and NAT64-wrapped private
	// targets: refused at connect time, before any packet.
	mcpevents.SetAllowLoopback(false)
	for _, target := range []string{"https://169.254.169.254/latest", "https://metadata.google.internal/x", "https://[::ffff:10.0.0.1]/x"} {
		code, raw := redeem(t, c, app, redeemReq(anonCall(), "chat.message", eng, tok, delivery(target, voucherSecret()), nil))
		if code != 400 || string(raw) != generic {
			t.Fatalf("%s: %d %s", target, code, raw)
		}
	}
	mcpevents.SetAllowLoopback(true)
	if got := voucherCol[int](t, c, tok, "verify_failures"); got != 3 {
		t.Fatalf("failures: %d", got)
	}
	code, raw = redeem(t, c, app, redeemReq(anonCall(), "chat.message", eng, tok, delivery(hook.url("fourth"), voucherSecret()), nil))
	wantBadVoucher(t, "the fourth try", code, raw)
	if hook.verified("fourth") != 0 {
		t.Fatal("the fourth try was verified")
	}
	_, tok = handOut(t, c, app, anonCall(), "chat.message", eng, nil)
	mcpevents.SetAllowLoopback(false)
	code, raw = redeem(t, c, app, redeemReq(anonCall(), "chat.message", eng, tok, delivery("https://[64:ff9b::a00:1]/x", voucherSecret()), nil))
	mcpevents.SetAllowLoopback(true)
	if code != 400 || string(raw) != generic {
		t.Fatalf("NAT64: %d %s", code, raw)
	}

	// The app's anonymous verifications this minute are spent: no request
	// goes out, the same generic answer.
	if _, err := c.DB.Pool.Exec(ctx, `INSERT INTO subscribe_voucher_counters (app_id, verify_window, verifications) VALUES ('chat', date_trunc('minute', now()), 30)
		ON CONFLICT (app_id) DO UPDATE SET verify_window = excluded.verify_window, verifications = 30`); err != nil {
		t.Fatal(err)
	}
	code, raw = redeem(t, c, app, redeemReq(anonCall(), "chat.message", eng, tok, delivery(hook.url("budget"), voucherSecret()), nil))
	if code != 400 || string(raw) != generic || hook.verified("budget") != 0 {
		t.Fatalf("past the budget: %d %s, verified %d", code, raw, hook.verified("budget"))
	}
	// An identified redeemer isn't limited by it, and gets today's reasons.
	_, utok := handOut(t, c, app, userCall("ada"), "chat.message", eng, nil)
	if code, e := appAPI(t, c, app, "/api/apps/v1/events/subscribe", redeemReq(userCall("ada"), "chat.message", eng, utok, delivery(hook.url("e404"), voucherSecret()), nil), nil); code != 400 || e != "callback_failed" {
		t.Fatalf("identified: %d %s", code, e)
	}
	_, raw, _ = appPostRaw(c, app, "/api/apps/v1/events/subscribe", redeemReq(userCall("ada"), "chat.message", eng, utok, delivery(hook.url("e404"), voucherSecret()), nil))
	if !strings.Contains(string(raw), `"reason":"http_4xx"`) {
		t.Fatalf("identified reason: %s", raw)
	}
	// With the hint any identified subscriber gets (callbackhint_test.go);
	// ChatGPT has no plugin and can't poll, so no alternative.
	if !strings.Contains(string(raw), `"hint":{"alternatives":[],"kind":"callback_unreachable"}`) {
		t.Fatalf("identified hint: %s", raw)
	}
}

// Anonymous fan-out caps (T16): 100 live anonymous voucher subscriptions
// per event and arguments, and 2,000 per app.
func TestVoucherFanout(t *testing.T) {
	eventsTestSetup(t)
	c := coretest.Start(t)
	app := voucherApp(t, c, "chat")
	hook := newVoucherHook(t)
	var tenant string
	if err := c.DB.Pool.QueryRow(ctx, `INSERT INTO tenants (name, kind, app_id, external_id) VALUES ('x', 'app', 'chat', '~conversation:seeded') RETURNING id::text`).Scan(&tenant); err != nil {
		t.Fatal(err)
	}
	// The arguments' key as the core computes it: take it from a hand-out.
	_, probe := handOut(t, c, app, anonCall(), "chat.message", eng, nil)
	engKey := voucherCol[string](t, c, probe, "arguments_key")
	seed := func(argKey string, n int) {
		if _, err := c.DB.Pool.Exec(ctx, `INSERT INTO subscribe_vouchers (token_hash, app_id, event, arguments_key, tool, expires_at, used_at, tenant_id, event_subscription_id, live_until)
			SELECT sha256(convert_to(gen_random_uuid()::text, 'UTF8')), 'chat', 'chat.message', $1, 'watch_job', now(), now(), $2, gen_random_uuid(), now() + interval '1 hour'
			FROM generate_series(1, $3)`, argKey, tenant, n); err != nil {
			t.Fatal(err)
		}
	}
	seed(engKey, 99)
	mustRedeem(t, c, app, redeemReq(anonCall(), "chat.message", eng, probe, delivery(hook.url("100th"), voucherSecret()), nil))
	_, tok := handOut(t, c, app, anonCall(), "chat.message", eng, nil)
	code, raw := redeem(t, c, app, redeemReq(anonCall(), "chat.message", eng, tok, delivery(hook.url("101st"), voucherSecret()), nil))
	wantBadVoucher(t, "the 101st on one key", code, raw)
	// A signed-in user isn't counted against it.
	_, utok := handOut(t, c, app, anonCall(), "chat.message", eng, nil)
	mustRedeem(t, c, app, redeemReq(mergeMap(userCall("ada"), map[string]any{"transport_session": anonTransport}), "chat.message", eng, utok, delivery(hook.url("ada"), voucherSecret()), nil))
	// Per app.
	seed("other", 1900)
	ops := map[string]any{"channel": "ops"}
	_, tok = handOut(t, c, app, anonCall(), "chat.message", ops, nil)
	code, raw = redeem(t, c, app, redeemReq(anonCall(), "chat.message", ops, tok, delivery(hook.url("2001st"), voucherSecret()), nil))
	wantBadVoucher(t, "the 2,001st per app", code, raw)
	if got := refusedReasons(t, c, "chat"); fmt.Sprint(got) != "[fanout fanout]" {
		t.Fatalf("logged: %v", got)
	}
	// What was refused gave its claim back: still usable once there's room.
	c.DB.Pool.Exec(ctx, `UPDATE subscribe_vouchers SET live_until = now() - interval '1 second' WHERE arguments_key = 'other'`)
	mustRedeem(t, c, app, redeemReq(anonCall(), "chat.message", ops, tok, delivery(hook.url("2001st"), voucherSecret()), nil))
}

// Where a redemption lands (§5, T15): an anonymous voucher redeemed by a
// signed-in user is in that user's workspace, in their list, counted in
// their webhooks, and ended by forgetting them; a user-bound voucher may
// rotate its secret on refresh; forgetting a user deletes their unused
// vouchers, workspace or not.
func TestVoucherWorkspaces(t *testing.T) {
	eventsTestSetup(t)
	c := coretest.Start(t)
	app := voucherApp(t, c, "chat")
	hook := newVoucherHook(t)
	_, tok := handOut(t, c, app, anonCall(), "chat.message", eng, nil)
	// On the transport session it was handed out on (it's bound to it).
	mustRedeem(t, c, app, redeemReq(mergeMap(userCall("cy"), map[string]any{"transport_session": anonTransport}), "chat.message", eng, tok, delivery(hook.url("cy"), voucherSecret()), nil))
	tenant := userTenant(t, c, "chat", "cy")
	if voucherCol[string](t, c, tok, "tenant_id::text") != tenant {
		t.Fatal("not in cy's workspace")
	}
	var list struct{ Subscriptions []map[string]any }
	if code, _ := appAPI(t, c, app, "/api/apps/v1/event-subscriptions/list", map[string]any{"user": "cy"}, &list); code != 200 || len(list.Subscriptions) != 1 {
		t.Fatalf("cy's list: %d %+v", code, list)
	}
	var hooks int
	c.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM mcp_event_webhooks WHERE app_id = 'chat'`).Scan(&hooks)
	})
	if hooks != 1 {
		t.Fatalf("cy's webhooks: %d", hooks)
	}
	// Redeemed by cy, refreshed only by cy.
	code, raw := redeem(t, c, app, redeemReq(anonCall(), "chat.message", eng, tok, delivery(hook.url("cy"), voucherSecret()), nil))
	wantBadVoucher(t, "cy's, refreshed by nobody", code, raw)

	// A user-bound voucher's refresh may rotate the secret.
	_, utok := handOut(t, c, app, userCall("dee"), "chat.message", eng, nil)
	mustRedeem(t, c, app, redeemReq(userCall("dee"), "chat.message", eng, utok, delivery(hook.url("dee"), voucherSecret()), nil))
	if r := mustRedeem(t, c, app, redeemReq(userCall("dee"), "chat.message", eng, utok, delivery(hook.url("dee"), voucherSecret()), nil)); r.Created {
		t.Fatal("a refresh created")
	}
	// Unused vouchers of a user without a workspace go when they're
	// forgotten; cy's subscription and voucher go with cy's workspace.
	_, etok := handOut(t, c, app, userCall("eve"), "chat.message", eng, nil)
	_, cyUnused := handOut(t, c, app, userCall("cy"), "chat.message", map[string]any{"channel": "ops"}, nil)
	if _, err := app.ForgetUser(ctx, "eve"); err != nil {
		t.Fatal(err)
	}
	if ok, err := app.ForgetUser(ctx, "cy"); err != nil || !ok {
		t.Fatalf("forget cy: %v %v", ok, err)
	}
	var left int
	c.DB.Pool.QueryRow(ctx, `SELECT count(*) FROM subscribe_vouchers WHERE token_hash IN (sha256(convert_to($1::text, 'UTF8')), sha256(convert_to($2::text, 'UTF8')), sha256(convert_to($3::text, 'UTF8')))`,
		etok, tok, cyUnused).Scan(&left)
	if left != 0 {
		t.Fatalf("%d vouchers left after forgetting", left)
	}
}

// Who may subscribe changing (T9): opening the event voids unused
// vouchers and keeps voucher subscriptions; closing it again ends native
// subscriptions and keeps voucher ones, counted in the audit entries.
func TestVoucherPolicyChanges(t *testing.T) {
	eventsTestSetup(t)
	c := coretest.Start(t)
	app := voucherApp(t, c, "chat")
	hook := newVoucherHook(t)
	_, used := handOut(t, c, app, userCall("ada"), "chat.message", eng, nil)
	mustRedeem(t, c, app, redeemReq(userCall("ada"), "chat.message", eng, used, delivery(hook.url("ada"), voucherSecret()), nil))
	_, unused := handOut(t, c, app, userCall("ada"), "chat.message", map[string]any{"channel": "ops"}, nil)

	if got, code := publishManifest(t, c, app, false, map[string]string{"chat.message": "any"}); code != 200 || fmt.Sprint(got.Changed) != "[{chat.message tools any 0}]" {
		t.Fatalf("opening: %d %+v", code, got)
	}
	code, raw := redeem(t, c, app, redeemReq(userCall("ada"), "chat.message", map[string]any{"channel": "ops"}, unused, delivery(hook.url("ops"), voucherSecret()), nil))
	wantBadVoucher(t, "after opening", code, raw)
	// A native subscription the open event allows.
	if code, e := appAPI(t, c, app, "/api/apps/v1/events/subscribe", map[string]any{"call": userCall("bo"), "name": "chat.message", "arguments": eng,
		"delivery": delivery(hook.url("bo"), voucherSecret())}, nil); code != 200 {
		t.Fatalf("native: %d %s", code, e)
	}
	got, code := publishManifest(t, c, app, false, map[string]string{"chat.message": "tools"})
	if code != 200 || fmt.Sprint(got.Changed) != "[{chat.message any tools 1}]" {
		t.Fatalf("closing: %d %+v", code, got)
	}
	live := func(user string) bool {
		var n int
		c.DB.InTenant(ctx, userTenant(t, c, "chat", user), func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM event_subscriptions WHERE ended_at IS NULL`).Scan(&n)
		})
		return n == 1
	}
	if !live("ada") || live("bo") {
		t.Fatalf("after closing: ada %t, bo %t", live("ada"), live("bo"))
	}
	d := subscribeAuditDetails(t, c, userTenant(t, c, "chat", "ada"))
	if len(d) != 2 || d[1]["ended"] != float64(0) {
		t.Fatalf("ada's audit: %+v", d)
	}
	emitChat(t, c, app, "kept")
	waitFor(t, "the voucher subscription's event", func() bool { return len(hook.texts("ada")) == 1 })
	if got := refusedReasons(t, c, "chat"); fmt.Sprint(got) != "[policy]" {
		t.Fatalf("logged: %v", got)
	}
}

// call.subscribe's options travel with the voucher: once ends the
// subscription after its first event; ttl caps it across refreshes, and a
// refresh after it ends it (expired) and is refused.
func TestVoucherOnceAndTTL(t *testing.T) {
	eventsTestSetup(t)
	c := coretest.Start(t)
	app := voucherApp(t, c, "chat")
	hook := newVoucherHook(t)
	_, tok := handOut(t, c, app, userCall("ada"), "chat.message", eng, map[string]any{"once": true})
	mustRedeem(t, c, app, redeemReq(userCall("ada"), "chat.message", eng, tok, delivery(hook.url("once"), voucherSecret()), nil))
	emitChat(t, c, app, "first")
	waitFor(t, "the one event", func() bool { return len(hook.texts("once")) == 1 })
	emitChat(t, c, app, "second")
	time.Sleep(400 * time.Millisecond)
	if got := hook.texts("once"); len(got) != 1 {
		t.Fatalf("once: %v", got)
	}

	ops := map[string]any{"channel": "ops"}
	_, tok = handOut(t, c, app, userCall("bo"), "chat.message", ops, map[string]any{"ttl": "10m"})
	r := mustRedeem(t, c, app, redeemReq(userCall("bo"), "chat.message", ops, tok, delivery(hook.url("ttl"), voucherSecret()), nil))
	if left := time.Until(r.RefreshBefore); left > 10*time.Minute+time.Second || left < 9*time.Minute {
		t.Fatalf("refresh_before in %v, want within the 10m ttl", left)
	}
	// Five minutes on, a refresh is capped by what's left.
	c.DB.Pool.Exec(ctx, `UPDATE subscribe_vouchers SET used_at = used_at - interval '5 minutes' WHERE token_hash = sha256(convert_to($1::text, 'UTF8'))`, tok)
	r = mustRedeem(t, c, app, redeemReq(userCall("bo"), "chat.message", ops, tok, delivery(hook.url("ttl"), voucherSecret()), nil))
	if left := time.Until(r.RefreshBefore); left > 5*time.Minute+time.Second || left < 4*time.Minute {
		t.Fatalf("refresh_before in %v, want within the 5m left", left)
	}
	// Past it: ended, refused.
	c.DB.Pool.Exec(ctx, `UPDATE subscribe_vouchers SET used_at = used_at - interval '6 minutes' WHERE token_hash = sha256(convert_to($1::text, 'UTF8'))`, tok)
	code, raw := redeem(t, c, app, redeemReq(userCall("bo"), "chat.message", ops, tok, delivery(hook.url("ttl"), voucherSecret()), nil))
	wantBadVoucher(t, "past the ttl", code, raw)
	var reason string
	c.DB.InTenant(ctx, userTenant(t, c, "chat", "bo"), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT coalesce(ended_reason, '') FROM event_subscriptions`).Scan(&reason)
	})
	if reason != "expired" {
		t.Fatalf("ended as %q", reason)
	}
}

// With the flag off (§9, T17): the manifest response doesn't report the
// feature, nothing is handed out, and redemption, refresh and
// unsubscribe-by-voucher are refused; turning it on again serves them.
func TestVoucherFlagOff(t *testing.T) {
	eventsTestSetup(t)
	c := coretest.Start(t)
	app := voucherApp(t, c, "chat")
	hook := newVoucherHook(t)
	_, used := handOut(t, c, app, anonCall(), "chat.message", eng, nil)
	secret := voucherSecret()
	mustRedeem(t, c, app, redeemReq(anonCall(), "chat.message", eng, used, delivery(hook.url("a"), secret), nil))
	_, unused := handOut(t, c, app, anonCall(), "chat.message", eng, nil)

	// The same database, served with the flag off.
	off := startFlagOff(t, c)
	var m struct{ Features []string }
	if code, _ := appAPI(t, off, app, "/api/apps/v1/manifest/merge", map[string]any{"events": []any{}}, &m); code != 200 || m.Features == nil || len(m.Features) != 0 {
		t.Fatalf("features with the flag off: %v", m.Features)
	}
	if code, _ := appAPI(t, c, app, "/api/apps/v1/manifest/merge", map[string]any{"events": []any{}}, &m); code != 200 || fmt.Sprint(m.Features) != "[subscribe_vouchers push]" {
		t.Fatalf("features with the flag on: %v", m.Features)
	}
	if text, v := handOut(t, off, app, anonCall(), "chat.message", eng, nil); v != "" || text != "" {
		t.Fatalf("a hand-out with the flag off: %q", text)
	}
	code, raw := redeem(t, off, app, redeemReq(anonCall(), "chat.message", eng, unused, delivery(hook.url("b"), voucherSecret()), nil))
	wantBadVoucher(t, "redemption", code, raw)
	code, raw = redeem(t, off, app, redeemReq(anonCall(), "chat.message", eng, used, delivery(hook.url("a"), secret), nil))
	wantBadVoucher(t, "refresh", code, raw)
	code, raw, _ = appPostRaw(off, app, "/api/apps/v1/events/unsubscribe", map[string]any{"call": anonCall(), "name": "chat.message", "arguments": eng,
		"delivery": map[string]any{"url": hook.url("a")}, "voucher": used})
	wantBadVoucher(t, "unsubscribe", code, raw)
	if hook.verified("b") != 0 {
		t.Fatal("verified with the flag off")
	}
	// On again: the unused one still works.
	mustRedeem(t, c, app, redeemReq(anonCall(), "chat.message", eng, unused, delivery(hook.url("b"), voucherSecret()), nil))
}

// startFlagOff serves c's database from a second core with vouchers off.
func startFlagOff(t *testing.T, c *coretest.Core) *coretest.Core {
	t.Helper()
	hs := httptest.NewUnstartedServer(nil)
	hs.Start()
	t.Cleanup(hs.Close)
	key := func(purpose string) []byte {
		sum := sha256.Sum256([]byte("coretest/" + purpose))
		return sum[:]
	}
	off, err := core.New(context.Background(), core.Options{DB: c.DB, PublicURL: hs.URL, Key: key, Web: fstest.MapFS{}, Poll: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	hs.Config.Handler = off.Handler
	return &coretest.Core{Core: off, URL: hs.URL, DB: c.DB}
}

// No token reaches a log (T6): every endpoint that takes a voucher, on
// success and failure, request logs included; nor the app's log or any
// workspace's audit log.
func TestVoucherNotLogged(t *testing.T) {
	eventsTestSetup(t)
	var buf syncBuffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	c := coretest.Start(t)
	app := voucherApp(t, c, "chat")
	hook := newVoucherHook(t)
	var tokens []string
	_, tok := handOut(t, c, app, anonCall(), "chat.message", eng, nil)
	tokens = append(tokens, tok)
	secret := voucherSecret()
	mustRedeem(t, c, app, redeemReq(anonCall(), "chat.message", eng, tok, delivery(hook.url("a"), secret), nil))
	mustRedeem(t, c, app, redeemReq(anonCall(), "chat.message", eng, tok, delivery(hook.url("a"), secret), nil))
	redeem(t, c, app, redeemReq(anonCall(), "chat.message", eng, tok, delivery(hook.url("b"), secret), nil))
	redeem(t, c, app, redeemReq(anonCall(), "chat.message", map[string]any{"channel": "x"}, tok, delivery(hook.url("b"), secret), nil))
	hook.set("bad", 0, 500)
	_, tok2 := handOut(t, c, app, userCall("ada"), "chat.message", eng, nil)
	tokens = append(tokens, tok2)
	redeem(t, c, app, redeemReq(userCall("ada"), "chat.message", eng, tok2, delivery(hook.url("bad"), secret), nil))
	redeem(t, c, app, redeemReq(userCall("bob"), "chat.message", eng, tok2, delivery(hook.url("bad"), secret), nil))
	appPostRaw(c, app, "/api/apps/v1/events/poll", map[string]any{"call": anonCall(), "name": "chat.message", "arguments": eng, "voucher": tok})
	appPostRaw(c, app, "/api/apps/v1/events/unsubscribe", map[string]any{"call": anonCall(), "name": "chat.message", "arguments": eng,
		"delivery": map[string]any{"url": hook.url("a")}, "voucher": tok})
	redeem(t, c, app, redeemReq(anonCall(), "chat.message", eng, "wsv_"+strings.Repeat("z", 32), delivery(hook.url("c"), secret), nil))
	time.Sleep(200 * time.Millisecond)
	logs := buf.String()
	if !strings.Contains(logs, "/api/apps/v1/events/subscribe") {
		t.Fatal("no request logs captured")
	}
	if strings.Contains(logs, "wsv_") {
		t.Fatalf("a voucher in the logs:\n%s", logs)
	}
	for _, q := range []string{`SELECT count(*) FROM app_log WHERE detail::text LIKE '%wsv_%'`, `SELECT count(*) FROM admin_log WHERE detail::text LIKE '%wsv_%'`} {
		var n int
		c.DB.Pool.QueryRow(ctx, q).Scan(&n)
		if n != 0 {
			t.Fatalf("%s: %d", q, n)
		}
	}
	tenants, _ := c.DB.Tenants(ctx)
	for _, tn := range tenants {
		c.DB.InTenant(ctx, tn, func(tx pgx.Tx) error {
			var n int
			tx.QueryRow(ctx, `SELECT (SELECT count(*) FROM audit_log WHERE detail::text LIKE '%wsv_%') + (SELECT count(*) FROM event_subscriptions WHERE row_to_json(event_subscriptions)::text LIKE '%wsv_%')`).Scan(&n)
			if n != 0 {
				t.Errorf("a voucher in workspace %s's records", tn)
			}
			return nil
		})
	}
}

// A voucher handed to a call recognized as another harness (Claude Code,
// say, which isn't native) gives nothing a client's word can stretch
// (T7): a user-bound one redeems only for the same user, an anonymous one
// once.
func TestVoucherOtherClient(t *testing.T) {
	eventsTestSetup(t)
	c := coretest.Start(t)
	app := voucherApp(t, c, "chat")
	hook := newVoucherHook(t)
	cc := map[string]any{"client": "claude-code", "tool": "watch_job", "user": "ada"}
	_, tok := handOut(t, c, app, cc, "chat.message", eng, nil)
	code, raw := redeem(t, c, app, redeemReq(userCall("bob"), "chat.message", eng, tok, delivery(hook.url("bob"), voucherSecret()), nil))
	wantBadVoucher(t, "another user", code, raw)
	mustRedeem(t, c, app, redeemReq(userCall("ada"), "chat.message", eng, tok, delivery(hook.url("ada"), voucherSecret()), nil))
	delete(cc, "user")
	cc["transport_session"] = anonTransport // an anonymous voucher is handed out only bound to one
	_, tok = handOut(t, c, app, cc, "chat.message", eng, nil)
	mustRedeem(t, c, app, redeemReq(anonCall(), "chat.message", eng, tok, delivery(hook.url("x"), voucherSecret()), nil))
	code, raw = redeem(t, c, app, redeemReq(anonCall(), "chat.message", eng, tok, delivery(hook.url("y"), voucherSecret()), nil))
	wantBadVoucher(t, "an anonymous one again", code, raw)
}

// ChatGPT's Work chats call tools and events/* through Codex: no client
// name on a stateless server, and the User-Agent "openai-mcp/1.0.0
// (Codex)", as observed. The core recognizes it as
// ChatGPT, hands out a voucher and redeems it, as for "openai-mcp/1.0.0":
// to a signed-in user. Anonymous, it has no transport session (the
// protocol has none), so it gets no voucher: one bound to nothing would be
// a bearer token for whoever reads it.
func TestVoucherChatGPTThroughCodex(t *testing.T) {
	eventsTestSetup(t)
	c := coretest.Start(t)
	app := voucherApp(t, c, "chat")
	hook := newVoucherHook(t)
	for i, ua := range []string{"openai-mcp/1.0.0 (Codex)", "openai-mcp/1.0.0"} {
		if _, tok := handOut(t, c, app, map[string]any{"user_agent": ua, "tool": "watch_job"}, "chat.message", eng, nil); tok != "" {
			t.Fatalf("%s, anonymous without a transport session: a voucher", ua)
		}
		call := map[string]any{"user_agent": ua, "tool": "watch_job", "user": fmt.Sprint("u", i)}
		text, tok := handOut(t, c, app, call, "chat.message", eng, nil)
		if tok == "" {
			t.Fatalf("%s: no voucher: %s", ua, text)
		}
		r := mustRedeem(t, c, app, redeemReq(call, "chat.message", eng, tok, delivery(hook.url(fmt.Sprint(i)), voucherSecret()), nil))
		if !r.Created || r.Via != "tool:watch_job" {
			t.Fatalf("%s: redeemed %+v", ua, r)
		}
	}
}

// wakeshift_voucher is reserved: no event may declare it (§4.4).
func TestVoucherReservedArgument(t *testing.T) {
	c := coretest.Start(t)
	app := eventsApp(t, c, "chat")
	m := chatEvents()
	m.Events[0].Name = "chat.reserved"
	m.Events[0].Topic = "reserved/{channel}" // its own: two events on one topic are refused (ambiguous_topic)
	schema := `{"type":"object","properties":{"channel":{"type":"string"},"from":{"type":"string"},"%s":{"type":"string"}},"required":["channel"]}`
	m.Events[0].Arguments = json.RawMessage(fmt.Sprintf(schema, "wakeshift_voucher"))
	if code, e := appAPI(t, c, app, "/api/apps/v1/manifest/merge", map[string]any{"events": m.Events}, nil); code != 400 || e != "bad_manifest" {
		t.Fatalf("declaring wakeshift_voucher: %d %s", code, e)
	}
	// The same event with another name for it is fine.
	m.Events[0].Arguments = json.RawMessage(fmt.Sprintf(schema, "voucher"))
	if code, e := appAPI(t, c, app, "/api/apps/v1/manifest/merge", map[string]any{"events": m.Events}, nil); code != 200 {
		t.Fatalf("another name: %d %s", code, e)
	}
}

// The expiry pass deletes unused vouchers an hour after they expire, and
// used ones an hour after their subscription stopped being live (T10).
func TestVoucherPrune(t *testing.T) {
	c := coretest.Start(t)
	app := voucherApp(t, c, "chat")
	_, keep := handOut(t, c, app, anonCall(), "chat.message", eng, nil)
	_, gone := handOut(t, c, app, anonCall(), "chat.message", eng, nil)
	c.DB.Pool.Exec(ctx, `UPDATE subscribe_vouchers SET expires_at = now() - interval '61 minutes' WHERE token_hash = sha256(convert_to($1::text, 'UTF8'))`, gone)
	n, err := appapi.PruneSubscribeVouchers(ctx, c.DB, 100)
	if err != nil || n != 1 {
		t.Fatalf("pruned %d, %v", n, err)
	}
	var left int
	c.DB.Pool.QueryRow(ctx, `SELECT count(*) FROM subscribe_vouchers WHERE token_hash = sha256(convert_to($1::text, 'UTF8'))`, keep).Scan(&left)
	if left != 1 {
		t.Fatal("pruned a live voucher")
	}
}

// The emergency stop (§9, T17): every live voucher subscription of the app
// ends now (stopped), its receiver is told, each workspace's audit log
// records it; other apps' and other subscriptions are untouched.
func TestVoucherEmergencyStop(t *testing.T) {
	eventsTestSetup(t)
	c := coretest.Start(t)
	app := voucherApp(t, c, "chat")
	other := voucherApp(t, c, "other")
	hook := newVoucherHook(t)
	_, a := handOut(t, c, app, anonCall(), "chat.message", eng, nil)
	mustRedeem(t, c, app, redeemReq(anonCall(), "chat.message", eng, a, delivery(hook.url("a"), voucherSecret()), nil))
	_, b := handOut(t, c, app, userCall("ada"), "chat.message", eng, nil)
	mustRedeem(t, c, app, redeemReq(userCall("ada"), "chat.message", eng, b, delivery(hook.url("b"), voucherSecret()), nil))
	_, o := handOut(t, c, other, anonCall(), "chat.message", eng, nil)
	mustRedeem(t, c, other, redeemReq(anonCall(), "chat.message", eng, o, delivery(hook.url("o"), voucherSecret()), nil))
	// ada's own tool-made conversation subscription stays.
	if code, e := bridgedSub(t, c, app, "ada", "watch", "ops"); code != 200 {
		t.Fatalf("bridged: %d %s", code, e)
	}

	ended, workspaces, err := (&appapi.API{DB: c.DB}).EndVoucherSubscriptions(ctx, "chat", "admin")
	if err != nil || ended != 2 || workspaces != 2 {
		t.Fatalf("ended %d in %d: %v", ended, workspaces, err)
	}
	for _, tok := range []string{a, b} {
		tenant := voucherCol[string](t, c, tok, "tenant_id::text")
		var reason, ending string
		var hooks int
		c.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
			tx.QueryRow(ctx, `SELECT ended_reason FROM event_subscriptions WHERE destination = 'webhook'`).Scan(&reason)
			// One statement, so the count and the reason come from one
			// snapshot: the deliverer can remove the webhook between two.
			return tx.QueryRow(ctx, `SELECT count(*), coalesce(max(ending_reason), '') FROM mcp_event_webhooks`).Scan(&hooks, &ending)
		})
		// The webhook is ending with the stop's reason, or the deliverer,
		// racing this check, has already ended and removed it (ChatGPT's
		// dialect gets no terminated envelope, so nothing else shows it).
		if reason != "stopped" || ending != "Wakeshift stopped this subscription" && hooks != 0 {
			t.Fatalf("%s: %q %q (webhooks %d)", tenant, reason, ending, hooks)
		}
		if got := auditActions(t, c, tenant); !strings.Contains(fmt.Sprint(got), "event_subscription.stopped") {
			t.Fatalf("audit: %v", got)
		}
	}
	var live int
	c.DB.InTenant(ctx, userTenant(t, c, "chat", "ada"), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM event_subscriptions WHERE ended_at IS NULL AND destination = 'conversation'`).Scan(&live)
	})
	otherTenant := voucherCol[string](t, c, o, "tenant_id::text")
	var otherLive int
	c.DB.InTenant(ctx, otherTenant, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM event_subscriptions WHERE ended_at IS NULL`).Scan(&otherLive)
	})
	if live != 1 || otherLive != 1 {
		t.Fatalf("untouched: conversation %d, other app %d", live, otherLive)
	}
	// Refreshing a stopped one is refused.
	code, raw := redeem(t, c, app, redeemReq(anonCall(), "chat.message", eng, a, delivery(hook.url("a"), voucherSecret()), nil))
	wantBadVoucher(t, "after the stop", code, raw)
	// Every app's.
	if ended, _, err := (&appapi.API{DB: c.DB}).EndVoucherSubscriptions(ctx, "", "admin"); err != nil || ended != 1 {
		t.Fatalf("every app: %d %v", ended, err)
	}
}

// A deleted app's vouchers go with its users' data (L3).
func TestVoucherAppDeletion(t *testing.T) {
	c := coretest.Start(t)
	app := voucherApp(t, c, "chat")
	handOut(t, c, app, anonCall(), "chat.message", eng, nil)
	handOut(t, c, app, userCall("ada"), "chat.message", eng, nil)
	if _, err := c.DB.Pool.Exec(ctx, `WITH o AS (INSERT INTO organizations (name) VALUES ('Acme') RETURNING id)
		UPDATE apps SET deleted_at = now(), org_id = (SELECT id FROM o) WHERE id = 'chat'`); err != nil {
		t.Fatal(err)
	}
	if _, err := appapi.FinishAppDeletions(ctx, c.DB, 100, 100); err != nil {
		t.Fatal(err)
	}
	var left int
	c.DB.Pool.QueryRow(ctx, `SELECT count(*) FROM subscribe_vouchers WHERE app_id = 'chat'`).Scan(&left)
	if left != 0 {
		t.Fatalf("%d vouchers left", left)
	}
}
