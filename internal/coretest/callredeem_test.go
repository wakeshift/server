package coretest_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/appapi"
	"github.com/wakeshift/server/internal/beacon"
	"github.com/wakeshift/server/internal/core"
	"github.com/wakeshift/server/internal/coretest"
	"github.com/wakeshift/server/internal/dbtest"
	"github.com/wakeshift/server/internal/hookproof"
	"github.com/wakeshift/server/internal/pluginmark"
)

// callCore drives redeem by call (internal/appapi, callredeem.go) the way a
// plugin's hooks do, over HTTP: a claim before the call, the subscribing
// call itself, and a redeem by its ID.
type callCore struct {
	t    *testing.T
	c    *coretest.Core
	mark string
}

func (cc callCore) post(path string, body any) (int, map[string]any, time.Duration) {
	cc.t.Helper()
	b, _ := json.Marshal(body)
	start := time.Now()
	resp, err := http.Post(cc.c.URL+path, "application/json", bytes.NewReader(b))
	if err != nil {
		cc.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out, time.Since(start)
}

// claim claims call for conv with key ("" app: the server isn't pinned).
func (cc callCore) claim(key ed25519.PrivateKey, call, conv, app string) (int, bool) {
	p := hookproof.Sign(key, hookproof.Claim, cc.c.URL, call, conv, time.Now())
	st, out, _ := cc.post("/v1/connector/claim", map[string]any{"call": call, "thread": conv, "app": app, "key": p.Key, "sig": p.Sig, "ts": p.TS})
	claimed, _ := out["claimed"].(bool)
	return st, claimed
}

// register registers key for conv (the session's waiter at its start).
func (cc callCore) register(key ed25519.PrivateKey, conv string) (int, bool) {
	p := hookproof.Sign(key, hookproof.Register, cc.c.URL, "", conv, time.Now())
	st, out, _ := cc.post("/v1/connector/register", map[string]any{"thread": conv, "key": p.Key, "sig": p.Sig, "ts": p.TS})
	ok, _ := out["registered"].(bool)
	return st, ok
}

// redeem redeems call by its ID for conv with key, signed at at.
func (cc callCore) redeem(key ed25519.PrivateKey, call, conv string, at time.Time) (int, map[string]any, time.Duration) {
	p := hookproof.Sign(key, hookproof.Call, cc.c.URL, call, conv, at)
	return cc.post("/v1/connector/redeem-call", map[string]any{"call": call, "thread": conv, "tool": "watch_builds", "app": "ci", "key": p.Key, "sig": p.Sig, "ts": p.TS})
}

func newKey() ed25519.PrivateKey {
	_, k, _ := ed25519.GenerateKey(rand.Reader)
	return k
}

// ticket is the newest ticket for call in tenant: its binding and how it
// was redeemed ("-": not yet).
func ticketOf(t *testing.T, c *coretest.Core, tenant, call string) string {
	t.Helper()
	var out string
	h := sha256.Sum256([]byte("wakeshift-call\x00" + call))
	if err := c.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT binding || '/' || coalesce(redeemed_via, '-') FROM connector_tickets
			WHERE call_hash = $1 ORDER BY created_at DESC LIMIT 1`, h[:]).Scan(&out)
	}); err != nil {
		t.Fatalf("ticket %s: %v", call, err)
	}
	return out
}

// redeemBeacon redeems a result's beacon as a hook does (from the
// transcript), for conv.
func redeemBeacon(t *testing.T, c *coretest.Core, s noteSub, call, conv string) int {
	t.Helper()
	return redeemBeaconHeld(t, c, s, call, conv, "")
}

// redeemBeaconHeld is redeemBeacon from hooks that hold a link for the
// conversation (held, "" for none), which they show.
func redeemBeaconHeld(t *testing.T, c *coretest.Core, s noteSub, call, conv, held string) int {
	t.Helper()
	tok, _ := s.Addition.Meta[beacon.MetaKey].(string)
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("no beacon in %v", s.Addition.Meta)
	}
	var claims beacon.Claims
	b, _ := base64.RawURLEncoding.DecodeString(parts[1])
	json.Unmarshal(b, &claims)
	in := map[string]any{"ticket": claims.Ticket, "thread": conv, "call": call}
	if held != "" {
		in["held"] = held
	}
	body, _ := json.Marshal(in)
	req, _ := http.NewRequest("POST", c.URL+"/v1/connector/redeem", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Wakeshift-Call-Redeem", "no_claim")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func TestRedeemByCallCore(t *testing.T) {
	c := coretest.StartWith(t, func(o *core.Options) { o.RedeemByToolCall = true })
	app := noteApp(t, c, "ci")
	cc := callCore{t: t, c: c, mark: pluginmark.Value("claude-code", "2026.11.12.0000", []string{c.URL})}
	sub := func(call, session string) noteSub {
		t.Helper()
		s, err := subscribe(app, caller{User: "bo", Client: "claude-code", Tool: "watch_builds", Meta: map[string]any{"claudecode/toolUseId": call},
			Session: session, Harness: cc.mark}, "ci/acme/web", subOpts{})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}

	// 1. Claimed before the call, bound at issue, redeemed by its ID.
	keyA := newKey()
	const convA = "0a1f1c2e-5b53-4d0e-9a71-0c3e2f6d8b10"
	if st, ok := cc.claim(keyA, "toolu_A1", convA, ""); st != 200 || !ok {
		t.Fatalf("claim: %d %v", st, ok)
	}
	sub("toolu_A1", "")
	tenant := workspaceOf(t, c, "ci", "bo")
	if got := ticketOf(t, c, tenant, "toolu_A1"); got != "stored/-" {
		t.Fatalf("A1 at issue: %s", got)
	}
	st, out, _ := cc.redeem(keyA, "toolu_A1", convA, time.Now())
	if st != 200 || out["url"] == nil || out["app"] == nil {
		t.Fatalf("by call: %d %v", st, out)
	}
	if got := ticketOf(t, c, tenant, "toolu_A1"); got != "stored/call" {
		t.Errorf("A1: %s", got)
	}
	if st, _, _ := cc.redeem(keyA, "toolu_A1", convA, time.Now()); st != 404 {
		t.Errorf("single use: %d", st)
	}

	// 2. A stranger with the ID: refused (padded), counted, audited; the
	// conversation's own redeem then says someone tried.
	const convC = "0c1f1c2e-5b53-4d0e-9a71-0c3e2f6d8b10"
	keyC, stranger := newKey(), newKey()
	cc.claim(keyC, "toolu_C1", convC, "ci")
	c1 := sub("toolu_C1", "")
	for _, conv := range []string{convC, "0d1f1c2e-5b53-4d0e-9a71-0c3e2f6d8b10"} {
		st, out, d := cc.redeem(stranger, "toolu_C1", conv, time.Now())
		if st != 404 || out["url"] != nil {
			t.Errorf("stranger: %d %v", st, out)
		}
		if d < 70*time.Millisecond {
			t.Errorf("a refusal answered in %v: not padded", d)
		}
	}
	// Counted each time, audited once per ticket.
	if n := count(t, c, tenant, `SELECT count(*) FROM audit_log WHERE action = 'connector.call_key_mismatch'`); n != 1 {
		t.Errorf("mismatch audit entries: %d", n)
	}
	st, out, _ = cc.redeem(keyC, "toolu_C1", convC, time.Now())
	if st != 200 || out["refused_attempts"] != float64(2) {
		t.Fatalf("victim after the stranger: %d %v", st, out)
	}
	// A call ID nobody bound is padded the same.
	if _, _, d := cc.redeem(stranger, "toolu_nobody", convC, time.Now()); d < 70*time.Millisecond {
		t.Errorf("an unknown call answered in %v", d)
	}

	// 3. Two keys claim one call (the async claim races the call, so the
	// other may have come first): contested, nothing bound, neither key
	// redeems it by call; the beacon connects it.
	const convD = "0e1f1c2e-5b53-4d0e-9a71-0c3e2f6d8b10"
	keyD := newKey()
	cc.claim(stranger, "toolu_D1", convD, "ci") // first
	if st, ok := cc.claim(keyD, "toolu_D1", convD, "ci"); st != 200 || ok {
		t.Errorf("the second key's claim taken: %d %v", st, ok)
	}
	d1 := sub("toolu_D1", "")
	if got := ticketOf(t, c, tenant, "toolu_D1"); got != "contested/-" {
		t.Errorf("D1: %s", got)
	}
	for _, k := range []ed25519.PrivateKey{stranger, keyD} {
		if st, _, _ := cc.redeem(k, "toolu_D1", convD, time.Now()); st != 404 {
			t.Errorf("a contested call by its ID: %d", st)
		}
	}
	if st := redeemBeacon(t, c, d1, "toolu_D1", convD); st != 200 {
		t.Errorf("contested, by beacon: %d", st)
	}

	// 3a. The second key's claim comes after the ticket was bound to the
	// first (the plugin's async claim, late): contested all the same, so
	// the first key can't redeem it by its ID; the beacon connects it.
	const convL = "0f1f1c2e-5b53-4d0e-9a71-0c3e2f6d8b10"
	keyL := newKey()
	cc.claim(stranger, "toolu_L1", convL, "ci") // first, and bound at issue
	l1 := sub("toolu_L1", "")
	if got := ticketOf(t, c, tenant, "toolu_L1"); got != "stored/-" {
		t.Fatalf("L1 at issue: %s", got)
	}
	if st, ok := cc.claim(keyL, "toolu_L1", convL, "ci"); st != 200 || ok {
		t.Errorf("the late second claim taken: %d %v", st, ok)
	}
	for _, k := range []ed25519.PrivateKey{stranger, keyL} {
		if st, _, _ := cc.redeem(k, "toolu_L1", convL, time.Now()); st != 404 {
			t.Errorf("a call contested after its bind, by its ID: %d", st)
		}
	}
	// The key that isn't the ticket's is still a key mismatch: counted on
	// the ticket and audited (once per ticket: C1's above, and L1's).
	lh := sha256.Sum256([]byte("wakeshift-call\x00toolu_L1"))
	if n := count(t, c, tenant, `SELECT refused_attempts FROM connector_tickets WHERE call_hash = $1`, lh[:]); n != 1 {
		t.Errorf("L1's refused attempts: %d", n)
	}
	if n := count(t, c, tenant, `SELECT count(*) FROM audit_log WHERE action = 'connector.call_key_mismatch'`); n != 2 {
		t.Errorf("mismatch audit entries after L1: %d", n)
	}
	if st := redeemBeacon(t, c, l1, "toolu_L1", convL); st != 200 {
		t.Errorf("contested after the bind, by beacon: %d", st)
	}

	// 3b. Two workspaces mint a ticket for one live call ID (someone sent
	// another app's server the ID): ambiguous, not redeemed by call.
	keyE := newKey()
	cc.claim(keyE, "toolu_E1", "0e2f1c2e-5b53-4d0e-9a71-0c3e2f6d8b10", "")
	sub("toolu_E1", "")
	if _, err := subscribe(app, caller{User: "cy", Client: "claude-code", Tool: "watch_builds", Meta: map[string]any{"claudecode/toolUseId": "toolu_E1"},
		Harness: cc.mark}, "ci/acme/web", subOpts{}); err != nil {
		t.Fatal(err)
	}
	if st, _, _ := cc.redeem(keyE, "toolu_E1", "0e2f1c2e-5b53-4d0e-9a71-0c3e2f6d8b10", time.Now()); st != 404 {
		t.Errorf("an ambiguous call by its ID: %d", st)
	}

	// 3c. A proof whose call or conversation has a newline: refused.
	p := hookproof.Sign(keyE, hookproof.Claim, c.URL, "toolu_N\nx", convD, time.Now())
	if st, _, _ := cc.post("/v1/connector/claim", map[string]any{"call": "toolu_N\nx", "thread": convD, "key": p.Key, "sig": p.Sig, "ts": p.TS}); st != 401 {
		t.Errorf("a newline in the call: %d", st)
	}

	// 4. The conversation's key, for another conversation: refused, kept.
	cc.claim(keyC, "toolu_C2", convC, "ci")
	sub("toolu_C2", c1.Session)
	if st, _, _ := cc.redeem(keyC, "toolu_C2", "0f1f1c2e-5b53-4d0e-9a71-0c3e2f6d8b10", time.Now()); st != 404 {
		t.Errorf("another conversation: %d", st)
	}
	// 5. Skew: six minutes off is refused, four isn't.
	if st, _, _ := cc.redeem(keyC, "toolu_C2", convC, time.Now().Add(-6*time.Minute)); st != 401 {
		t.Errorf("six minutes off: %d", st)
	}
	st, c2, _ := cc.redeem(keyC, "toolu_C2", convC, time.Now().Add(4*time.Minute))
	if st != 200 {
		t.Errorf("four minutes ahead: %d", st)
	}
	heldC, _ := c2["url"].(string) // the link C's hooks now hold

	// 5a. A conversation its hooks connected by beacon (D, contested above:
	// no key bound) can't be taken by a new key's first redeem by call
	// without the hooks' link (#1152), while C's own key, bound to C,
	// needed no link.
	keyM := newKey()
	cc.claim(keyM, "toolu_D2", convD, "ci")
	sub("toolu_D2", d1.Session)
	if got := ticketOf(t, c, tenant, "toolu_D2"); got != "stored/-" {
		t.Fatalf("D2 at issue: %s", got)
	}
	if st, out, _ := cc.redeem(keyM, "toolu_D2", convD, time.Now()); st != http.StatusConflict || out["url"] != nil {
		t.Errorf("a new key's first redeem by call of a connected conversation: %d %v", st, out)
	}

	// 6. A new key for a conversation bound to another (a resume without
	// the key file): no binding; the beacon connects it, from hooks that
	// show the link they hold (without it, they're refused: #1152).
	cc.claim(newKey(), "toolu_C3", convC, "ci")
	c3 := sub("toolu_C3", c1.Session)
	if got := ticketOf(t, c, tenant, "toolu_C3"); got != "key_changed/-" {
		t.Errorf("C3: %s", got)
	}
	if st := redeemBeaconHeld(t, c, c3, "toolu_C3", convC, heldC); st != 200 {
		t.Errorf("C3 by beacon: %d", st)
	}
	if got := ticketOf(t, c, tenant, "toolu_C3"); got != "key_changed/beacon" {
		t.Errorf("C3 after: %s", got)
	}

	// 7. A fork: a claim for another conversation on C's session.
	cc.claim(newKey(), "toolu_F1", "0b1f1c2e-5b53-4d0e-9a71-0c3e2f6d8b10", "ci")
	sub("toolu_F1", c1.Session)
	if got := ticketOf(t, c, tenant, "toolu_F1"); got != "thread_mismatch/-" {
		t.Errorf("F1: %s", got)
	}

	// 8. No claim: no binding, the beacon connects it.
	n1 := sub("toolu_N1", "")
	if got := ticketOf(t, c, tenant, "toolu_N1"); got != "no_claim/-" {
		t.Errorf("N1: %s", got)
	}
	if st := redeemBeacon(t, c, n1, "toolu_N1", "0b2f1c2e-5b53-4d0e-9a71-0c3e2f6d8b10"); st != 200 {
		t.Errorf("N1 by beacon: %d", st)
	}

	// 9. A claim naming an app there isn't: refused.
	if st, ok := cc.claim(newKey(), "toolu_X1", convA, "no-such-app"); st != 200 || ok {
		t.Errorf("unknown app: %d %v", st, ok)
	}

	// 10. A registered session: its conversation takes claims only by its
	// key; another key can't register it, the same key refreshes it.
	const convR = "0a2f1c2e-5b53-4d0e-9a71-0c3e2f6d8b10"
	keyR := newKey()
	if st, ok := cc.register(keyR, convR); st != 200 || !ok {
		t.Fatalf("register: %d %v", st, ok)
	}
	if st, ok := cc.register(stranger, convR); st != 200 || ok {
		t.Errorf("another key registered: %d %v", st, ok)
	}
	if st, ok := cc.register(keyR, convR); st != 200 || !ok {
		t.Errorf("refresh: %d %v", st, ok)
	}
	if st, ok := cc.claim(stranger, "toolu_R1", convR, "ci"); st != 200 || ok {
		t.Errorf("a claim by another key for a registered conversation: %d %v", st, ok)
	}
	if st, ok := cc.claim(keyR, "toolu_R2", convR, "ci"); st != 200 || !ok {
		t.Errorf("the registered key's claim: %d %v", st, ok)
	}
	sub("toolu_R2", "")
	if st, _, _ := cc.redeem(keyR, "toolu_R2", convR, time.Now()); st != 200 {
		t.Errorf("registered, by call: %d", st)
	}

	// 11. Erasing the user's workspace takes its conversation keys and call
	// index rows.
	if ok, err := app.ForgetUser(ctx, "bo"); err != nil || !ok {
		t.Fatalf("forget: %v %v", ok, err)
	}
	var left int
	c.DB.Pool.QueryRow(ctx, `SELECT count(*) FROM connector_call_index WHERE tenant_id = $1`, tenant).Scan(&left)
	if left != 0 {
		t.Errorf("%d call index rows kept", left)
	}
	if n := count(t, c, tenant, `SELECT count(*) FROM connector_conversation_keys WHERE tenant_id = $1`, tenant); n != 0 {
		t.Errorf("%d conversation keys kept", n)
	}
}

// With the flag off, nothing changes: the routes answer 404 and nothing is
// bound at issue.
func TestRedeemByCallOff(t *testing.T) {
	c := coretest.Start(t)
	app := noteApp(t, c, "ci")
	cc := callCore{t: t, c: c, mark: pluginmark.Value("claude-code", "2026.11.12.0000", []string{c.URL})}
	key := newKey()
	if st, _ := cc.claim(key, "toolu_O1", "0a1f1c2e-5b53-4d0e-9a71-0c3e2f6d8b10", ""); st != 404 {
		t.Errorf("claim: %d", st)
	}
	if _, err := subscribe(app, caller{User: "bo", Client: "claude-code", Tool: "watch_builds", Meta: map[string]any{"claudecode/toolUseId": "toolu_O1"},
		Harness: cc.mark}, "ci/acme/web", subOpts{}); err != nil {
		t.Fatal(err)
	}
	if n := count(t, c, workspaceOf(t, c, "ci", "bo"), `SELECT count(*) FROM connector_tickets WHERE call_hash IS NOT NULL OR binding <> ''`); n != 0 {
		t.Errorf("%d tickets bound or indexed by call with the flag off", n)
	}
	if st, _, _ := cc.redeem(key, "toolu_O1", "0a1f1c2e-5b53-4d0e-9a71-0c3e2f6d8b10", time.Now()); st != 404 {
		t.Errorf("redeem: %d", st)
	}
}

// The prune deletes a batch at a time: limit rows, never more. On a
// database of its own (a core's own prune pass would run too).
func TestPruneCallClaimsBatches(t *testing.T) {
	d := dbtest.New(t)
	ctx := context.Background()
	for i := range 5 {
		h := sha256.Sum256([]byte{byte(i)})
		if _, err := d.Pool.Exec(ctx, `INSERT INTO connector_call_claims (call_hash, key_hash, thread_hash, expires_at) VALUES ($1, $1, $1, now() - interval '2 hours')`, h[:]); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := appapi.PruneCallClaims(ctx, d, 2); err != nil || n != 2 {
		t.Fatalf("first batch: %d %v", n, err)
	}
	var left int
	d.Pool.QueryRow(ctx, `SELECT count(*) FROM connector_call_claims`).Scan(&left)
	if left != 3 {
		t.Errorf("left after a batch of 2: %d", left)
	}
}

// A contest committing while a redeem by call is under way: the redeem
// reads the claim under its row's share lock, so it waits for the contest
// and refuses; and a fresh claim over an expired, contested one starts
// uncontested.
func TestRedeemByCallWaitsForAContest(t *testing.T) {
	c := coretest.StartWith(t, func(o *core.Options) { o.RedeemByToolCall = true })
	app := noteApp(t, c, "ci")
	cc := callCore{t: t, c: c, mark: pluginmark.Value("claude-code", "2026.11.12.0000", []string{c.URL})}
	sub := func(call string) noteSub {
		t.Helper()
		s, err := subscribe(app, caller{User: "bo", Client: "claude-code", Tool: "watch_builds", Meta: map[string]any{"claudecode/toolUseId": call},
			Harness: cc.mark}, "ci/acme/web", subOpts{})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	hashOf := func(call string) []byte { h := sha256.Sum256([]byte("wakeshift-call\x00" + call)); return h[:] }

	const conv = "1a1f1c2e-5b53-4d0e-9a71-0c3e2f6d8b10"
	key := newKey()
	cc.claim(key, "toolu_M1", conv, "ci")
	sub("toolu_M1")
	tenant := workspaceOf(t, c, "ci", "bo")
	if got := ticketOf(t, c, tenant, "toolu_M1"); got != "stored/-" {
		t.Fatalf("M1 at issue: %s", got)
	}
	// The contest, uncommitted, holds the claim's row.
	tx, err := c.DB.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `UPDATE connector_call_claims SET contested = true WHERE call_hash = $1`, hashOf("toolu_M1")); err != nil {
		t.Fatal(err)
	}
	done := make(chan int, 1)
	go func() { st, _, _ := cc.redeem(key, "toolu_M1", conv, time.Now()); done <- st }()
	select {
	case st := <-done:
		t.Fatalf("the redeem didn't wait for the contest: %d", st)
	case <-time.After(500 * time.Millisecond):
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if st := <-done; st != 404 {
		t.Errorf("redeemed by call after a contest committed: %d", st)
	}
	if got := ticketOf(t, c, tenant, "toolu_M1"); got != "stored/-" {
		t.Errorf("M1 after the refusal: %s (kept for the beacon)", got)
	}

	// An expired, contested claim, claimed again: uncontested, and bound.
	other, fresh := newKey(), newKey()
	cc.claim(key, "toolu_N1", conv, "ci")
	cc.claim(other, "toolu_N1", conv, "ci")
	if _, err := c.DB.Pool.Exec(ctx, `UPDATE connector_call_claims SET expires_at = now() - interval '1 minute' WHERE call_hash = $1`, hashOf("toolu_N1")); err != nil {
		t.Fatal(err)
	}
	if st, ok := cc.claim(fresh, "toolu_N1", conv, "ci"); st != 200 || !ok {
		t.Fatalf("a fresh claim over an expired one: %d %v", st, ok)
	}
	var contested bool
	if err := c.DB.Pool.QueryRow(ctx, `SELECT contested FROM connector_call_claims WHERE call_hash = $1`, hashOf("toolu_N1")).Scan(&contested); err != nil || contested {
		t.Errorf("the fresh claim inherited contested: %v %v", contested, err)
	}
	sub("toolu_N1")
	if got := ticketOf(t, c, tenant, "toolu_N1"); got != "stored/-" {
		t.Errorf("N1 at issue: %s", got)
	}
	if st, _, _ := cc.redeem(fresh, "toolu_N1", conv, time.Now()); st != 200 {
		t.Errorf("the fresh claim's redeem by call: %d", st)
	}
}
