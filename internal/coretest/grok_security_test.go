package coretest_test

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/agenthooks"
	"github.com/wakeshift/server/internal/botkeys"
	"github.com/wakeshift/server/internal/core"
	"github.com/wakeshift/server/internal/coretest"
	"github.com/wakeshift/server/internal/harnesses"
)

// prove posts a proof form, as curl -F does.
func prove(t *testing.T, c *coretest.Core, form map[string]string) (int, string) {
	t.Helper()
	var b bytes.Buffer
	w := multipart.NewWriter(&b)
	for k, v := range form {
		w.WriteField(k, v)
	}
	w.Close()
	resp, err := http.Post(c.URL+"/v1/stream/proof", w.FormDataContentType(), &b)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

// testKey is a bot key made here: its public PEM, and a signer.
type testKey struct {
	pub  ed25519.PublicKey
	priv ed25519.PrivateKey
}

func newTestKey() testKey {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	return testKey{pub, priv}
}

func (k testKey) pem() string {
	b, _ := x509.MarshalPKIXPublicKey(k.pub)
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: b}))
}

func (k testKey) sign(bot, ch string) string {
	return base64.StdEncoding.EncodeToString(ed25519.Sign(k.priv, botkeys.Message(bot, ch)))
}

func (k testKey) form(bot, ch string) map[string]string {
	return map[string]string{"challenge": ch, "public_key": k.pem(), "signature": k.sign(bot, ch)}
}

// fileKey reads the key a bot made in its VM (as another of the user's
// bots could).
func fileKey(t *testing.T, b *grokBot) testKey {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(b.vm.dir(b.id), "signing.pem"))
	if err != nil {
		t.Fatal(err)
	}
	blk, _ := pem.Decode(raw)
	k, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	priv := k.(ed25519.PrivateKey)
	return testKey{priv.Public().(ed25519.PublicKey), priv}
}

// Proofs: a forged signature, one by another key, one over another
// challenge or another bot's ID each fail and count, and the third ends
// the challenge; a used, expired, unknown or altered challenge (another
// purpose, another workspace) gets one answer, byte for byte; a good proof
// works once (its replay gets that same answer); a body holding a private
// key is refused, and neither it nor the body is logged.
func TestGrokProofRefusals(t *testing.T) {
	var logs syncBuffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	c := startGrok(t, nil)
	app := noteApp(t, c, "ci")
	k, other := newTestKey(), newTestKey()
	subFor := func(user, bot string) (caller, string, string) {
		call := caller{User: user, Client: "Cursor", Harness: "grok-bot", Conversation: bot}
		sub, err := subscribe(app, call, "ci/"+user, subOpts{})
		if err != nil {
			t.Fatal(err)
		}
		call.Session = sub.Session
		tenant, _ := topicSubRow(t, c, sub.ID)
		return call, registerCh(t, c, sub.Addition.Text), tenant
	}
	call, ch, tenant := subFor("gina", grokBotID)
	_, otherCh, otherTenant := subFor("hank", "9a8b7c6d-5e4f-4321-8abc-0123456789ab")

	code, gone := prove(t, c, map[string]string{"challenge": "wakeshift:v1:register:" + strings.Repeat("A", 43) + ":" + strings.Repeat("B", 22)})
	if code != 401 || strings.TrimSpace(gone) != harnesses.GrokChallengeGone {
		t.Fatalf("unknown: %d %s", code, gone)
	}
	same := func(name string, form map[string]string) {
		t.Helper()
		if code, body := prove(t, c, form); code != 401 || body != gone {
			t.Fatalf("%s: %d %q", name, code, body)
		}
	}
	same("malformed", map[string]string{"challenge": "$(id)"})
	same("another purpose", k.form(grokBotID, strings.Replace(ch, ":register:", ":setup:", 1)))
	nonce := []byte(ch)
	nonce[len(nonce)-1] ^= 1
	same("another nonce", k.form(grokBotID, string(nonce)))
	// Another workspace: the other's challenge with this one's ID part.
	parts, oparts := strings.Split(ch, ":"), strings.Split(otherCh, ":")
	same("another workspace", k.form(grokBotID, strings.Join([]string{parts[0], parts[1], parts[2], oparts[3], parts[4]}, ":")))

	// Three bad proofs end it.
	bad := []map[string]string{
		{"challenge": ch, "public_key": k.pem(), "signature": base64.StdEncoding.EncodeToString(make([]byte, 64))},
		{"challenge": ch, "public_key": k.pem(), "signature": other.sign(grokBotID, ch)},
		{"challenge": ch, "public_key": k.pem(), "signature": k.sign("9a8b7c6d-5e4f-4321-8abc-0123456789ab", ch)},
	}
	for i, form := range bad {
		code, body := prove(t, c, form)
		if code != 401 || (i < 2) != strings.Contains(body, "That signature didn't match") || !strings.Contains(body, harnesses.LinkStart) {
			t.Fatalf("bad proof %d: %d %s", i, code, body)
		}
	}
	same("ended", k.form(grokBotID, ch))
	if count(t, c, tenant, `SELECT count(*) FROM bot_keys`) != 0 {
		t.Fatal("a bad proof registered a key")
	}
	// Signed over another challenge (the other session's).
	if code, body := prove(t, c, map[string]string{"challenge": otherCh, "public_key": k.pem(), "signature": k.sign("9a8b7c6d-5e4f-4321-8abc-0123456789ab", ch)}); code != 401 || !strings.Contains(body, "didn't match") {
		t.Fatalf("over another challenge: %d %s", code, body)
	}

	// A new challenge (the old one ended), proved once.
	sub, _ := subscribe(app, call, "ci/gina", subOpts{})
	ch2 := registerCh(t, c, sub.Addition.Text)
	if ch2 == "" || ch2 == ch {
		t.Fatalf("no new challenge: %s", sub.Addition.Text)
	}
	good := k.form(grokBotID, ch2)
	if code, body := prove(t, c, good); code != 200 || !strings.Contains(body, "Your Wakeshift key checks out") {
		t.Fatalf("good proof: %d %s", code, body)
	}
	same("replayed", good)
	// Expired.
	if err := c.DB.InTenant(ctx, otherTenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE bot_challenges SET expires_at = now() - interval '1 second'`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	same("expired", k.form("9a8b7c6d-5e4f-4321-8abc-0123456789ab", otherCh))

	// A private key in the body.
	der, _ := x509.MarshalPKCS8PrivateKey(k.priv)
	privPEM := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	privLine := strings.Split(privPEM, "\n")[1]
	for _, form := range []map[string]string{
		{"challenge": ch2, "public_key": privPEM, "signature": "x"},
		{"challenge": ch2, "signature": privLine},
	} {
		if code, body := prove(t, c, form); code != 400 || strings.TrimSpace(body) != harnesses.GrokPrivateKeyNote {
			t.Fatalf("private key: %d %s", code, body)
		}
	}
	for _, secret := range []string{privLine, ch, ch2, otherCh, k.sign(grokBotID, ch2)} {
		if strings.Contains(logs.String(), secret) {
			t.Fatalf("logged a secret: %s", secret)
		}
	}
}

// A leaked link binds nothing: someone who opens it first and pastes
// their own webhook starts a run in their own bot, which can't even fetch
// the check (the setup fetch is signed by the key the link was minted
// for), let alone sign it; the attempt ends unconfirmed when its proof
// doesn't come, and the real user's paste on the same link still binds.
func TestGrokLeakedLink(t *testing.T) {
	c := startGrok(t, nil)
	f := newFakeCursor(t, c)
	app := noteApp(t, c, "ci")
	bot := &grokBot{c: c, vm: newGrokVM(t), id: grokBotID}
	call := bot.call("gina")
	sub, err := subscribe(app, call, "ci/grok", subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	tenant, _ := topicSubRow(t, c, sub.ID)
	token := setupToken(t, bot.keyStep(t, sub.Addition.Text))

	// Someone else's bot, in their own VM, with a key of their own kept
	// under this bot's ID (which the challenge names), and a routine that
	// answers whatever comes as that bot.
	thief := &grokBot{c: c, vm: newGrokVM(t), id: "7e6d5c4b-3a29-4180-9f7e-6d5c4b3a2918"}
	thief.vm.installDirect(t)
	thief.vm.run(t, grokBotID, harnesses.GrokInitCommand(c.URL, grokBotID))
	turl, tkey := f.webhook("thiefHook01", thief)
	f.set("thiefHook01", func(h *fakeHook) { h.reckless = true })
	if code, out := save(t, c, token, turl, tkey); code != 200 || out["awaiting_proof"] != true {
		t.Fatalf("thief's save: %d %v", code, out)
	}
	runs := f.ran()
	if len(runs) != 1 || !strings.Contains(runs[0], "no check is waiting for this bot") {
		t.Fatalf("the thief's run: %v", runs)
	}
	if count(t, c, tenant, `SELECT count(*) FROM agent_webhooks`) != 0 {
		t.Fatal("a leaked link bound a webhook")
	}
	// Its proof never comes; the attempt's 10 minutes pass.
	if err := c.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE agent_webhook_setups SET proof_due = now() - interval '1 second'`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	code, out := describe(t, c, token)
	if code != 200 || out["state"] != "open" || out["error"] != "unconfirmed" {
		t.Fatalf("after the thief: %d %v", code, out)
	}
	if count(t, c, tenant, `SELECT failures FROM agent_webhook_setups`) != 1 {
		t.Fatal("the attempt didn't count")
	}
	// The real user, the same link.
	url, key := f.webhook("realHook001", bot)
	if code, out := save(t, c, token, url, key); code != 200 {
		t.Fatalf("real save: %d %v", code, out)
	}
	waitOn(t, c, token)
	if count(t, c, tenant, `SELECT count(*) FROM agent_webhooks WHERE state = 'live'`) != 1 {
		t.Fatal("not bound")
	}
}

// The setup page's refusals: anything but a Cursor webhook URL, or a key
// that can't be one, never echoed; every bad, used or foreign token (a
// Capy registration link's, a stream link's, a challenge, tampered) gets
// the same 401, byte for byte; Cursor's 401, 404, a redirect (never
// followed) and a 5xx each fail the attempt with their own error, and the
// fifth failure ends the link; a save while a check awaits its proof is
// refused, and 50 at once make one attempt; a proof that never comes ends
// the attempt; 50 proofs at once bind once.
func TestGrokSetupRefusals(t *testing.T) {
	c := startGrok(t, nil)
	f := newFakeCursor(t, c)
	app := noteApp(t, c, "ci")
	bot := &grokBot{c: c, vm: newGrokVM(t), id: grokBotID}
	sub, err := subscribe(app, bot.call("gina"), "ci/grok", subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	tenant, _ := topicSubRow(t, c, sub.ID)
	token := setupToken(t, bot.keyStep(t, sub.Addition.Text))
	url, key := f.webhook("refuseHook1", bot)

	for _, u := range []string{
		"https://api2.cursor.sh.evil.com" + agenthooks.CursorPath + "refuseHook1",
		"https://evil.com" + agenthooks.CursorPath + "refuseHook1",
		"http://api2.cursor.sh" + agenthooks.CursorPath + "refuseHook1",
		"https://169.254.169.254" + agenthooks.CursorPath + "refuseHook1",
		"https://127.0.0.1" + agenthooks.CursorPath + "refuseHook1",
		agenthooks.CursorOrigin + agenthooks.CursorPath + "refuseHook1", // the real Cursor: not while a test's stands in
		url + "?x=1", url + "/", strings.Replace(url, "refuseHook1", "%2e%2e/refuse", 1),
	} {
		code, out, raw := page(t, c, "/v1/stream/webhook-setup", map[string]string{"token": token, "url": u, "key": key})
		if code != 400 || errCode(out) != "invalid_url" || bytes.Contains(raw, []byte("refuseHook1")) {
			t.Fatalf("%s: %d %s", u, code, raw)
		}
	}
	for _, k := range []string{"short", key + " x", key + "\r\nX: y"} {
		if code, out := save(t, c, token, url, k); code != 400 || errCode(out) != "invalid_key" {
			t.Fatalf("key %q: %d %v", k, code, out)
		}
	}
	if len(f.got()) != 0 {
		t.Fatal("a refused paste reached the webhook")
	}

	// Tokens: one answer.
	_, _, want := page(t, c, "/v1/stream/webhook-setup/describe", map[string]string{"token": "x"})
	capy := agenthooks.NewLinks(coretest.Key("agent-webhook-register")).URL(c.URL, tenant, "link", agenthooks.KindCapy, sub.ExpiresAt)
	forgedTenant := agenthooks.NewSetupLinks(coretest.Key("agent-webhook-setup")).SetupToken(agenthooks.Setup{Tenant: "11111111-2222-4333-8444-555555555555", ID: "11111111-2222-4333-8444-555555555555"})
	for name, tok := range map[string]string{
		"empty": "", "tampered": token[:len(token)-2] + "xx", "capy": capy[strings.Index(capy, "link=")+5:], "challenge": challengeIn(t, c, tenant, "register"),
		"stream link": waitCmdRE.FindStringSubmatch(sub.Addition.Text)[1], "unknown setup": forgedTenant,
	} {
		for _, path := range []string{"/v1/stream/webhook-setup/describe", "/v1/stream/webhook-setup"} {
			code, _, raw := page(t, c, path, map[string]string{"token": tok, "url": url, "key": key})
			if code != 401 || !bytes.Equal(raw, want) {
				t.Fatalf("%s at %s: %d %s", name, path, code, raw)
			}
		}
	}

	// Cursor's answers fail the attempt, each with its error; nothing
	// follows a redirect.
	for _, c2 := range []struct {
		plan int
		code int
		err  string
	}{{http.StatusUnauthorized, 400, "webhook_unauthorized"}, {http.StatusNotFound, 400, "webhook_not_found"}, {http.StatusFound, 502, "webhook_unanswered"}, {500, 502, "webhook_unanswered"}} {
		before := len(f.got())
		f.answer(c2.plan)
		code, out := save(t, c, token, url, key)
		if code != c2.code || errCode(out) != c2.err || len(f.got()) != before+1 {
			t.Fatalf("Cursor %d: %d %v (%d requests)", c2.plan, code, out, len(f.got())-before)
		}
	}
	if n := count(t, c, tenant, `SELECT failures FROM agent_webhook_setups`); n != 4 {
		t.Fatalf("failures: %d", n)
	}

	// A check awaiting its proof: one attempt for 50 saves at once.
	f.set("refuseHook1", func(h *fakeHook) { h.silent = true })
	var wg sync.WaitGroup
	var mu sync.Mutex
	codes := map[int]int{}
	before := len(f.of(agenthooks.TypeChallenge))
	for range 50 {
		wg.Go(func() {
			code, _ := save(t, c, token, url, key)
			mu.Lock()
			codes[code]++
			mu.Unlock()
		})
	}
	wg.Wait()
	if codes[200] != 1 || codes[409] != 49 || len(f.of(agenthooks.TypeChallenge)) != before+1 {
		t.Fatalf("50 saves: %v, %d challenges", codes, len(f.of(agenthooks.TypeChallenge))-before)
	}
	if code, out := describe(t, c, token); code != 200 || out["state"] != "awaiting_proof" {
		t.Fatalf("awaiting: %d %v", code, out)
	}
	// 50 proofs of that check at once: one binds.
	setupCh := challengeIn(t, c, tenant, "setup")
	k := fileKey(t, bot)
	// A proof of an earlier attempt's challenge (made usable again here)
	// doesn't bind this attempt's webhook.
	old := firstChallengeIn(t, c, tenant, "setup")
	if err := c.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE bot_challenges SET used_at = NULL WHERE purpose = 'setup' AND id <> (SELECT challenge_id FROM agent_webhook_setups)`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, body := prove(t, c, k.form(grokBotID, old)); strings.TrimSpace(body) != harnesses.GrokSetupFailedNote || count(t, c, tenant, `SELECT count(*) FROM agent_webhooks`) != 0 {
		t.Fatalf("an earlier challenge: %s", body)
	}
	answers := map[string]int{}
	for range 50 {
		wg.Go(func() {
			_, body := prove(t, c, k.form(grokBotID, setupCh))
			mu.Lock()
			answers[strings.TrimSpace(body)]++
			mu.Unlock()
		})
	}
	wg.Wait()
	if answers[harnesses.GrokBoundNote("Ci")] < 1 || answers[harnesses.GrokBoundNote("Ci")]+answers[harnesses.GrokChallengeGone] != 50 {
		t.Fatalf("50 proofs: %v", answers)
	}
	if count(t, c, tenant, `SELECT count(*) FROM agent_webhooks`) != 1 ||
		count(t, c, tenant, `SELECT count(*) FROM audit_log WHERE action = 'agent_webhook.registered'`) != 1 {
		t.Fatal("not bound exactly once")
	}
	// Used: one answer.
	if code, _, raw := page(t, c, "/v1/stream/webhook-setup", map[string]string{"token": token, "url": url, "key": key}); code != 401 || !bytes.Equal(raw, want) {
		t.Fatalf("used: %d %s", code, raw)
	}
}

// A proof that never comes ends the attempt, and the fifth failure ends
// the link; unwatching revokes it.
func TestGrokSetupExpiry(t *testing.T) {
	c := startGrok(t, nil)
	f := newFakeCursor(t, c)
	app := noteApp(t, c, "ci")
	bot := &grokBot{c: c, vm: newGrokVM(t), id: grokBotID}
	call := bot.call("gina")
	sub, err := subscribe(app, call, "ci/grok", subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	call.Session = sub.Session
	tenant, _ := topicSubRow(t, c, sub.ID)
	token := setupToken(t, bot.keyStep(t, sub.Addition.Text))
	url, key := f.webhook("expireHook1", bot)
	f.set("expireHook1", func(h *fakeHook) { h.silent = true })
	for i := range 5 {
		if code, out := save(t, c, token, url, key); code != 200 {
			t.Fatalf("save %d: %d %v", i, code, out)
		}
		// The 10 minutes pass.
		if err := c.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE agent_webhook_setups SET proof_due = now() - interval '1 second'`)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		code, out := describe(t, c, token)
		if i < 4 && (code != 200 || out["state"] != "open" || out["error"] != "unconfirmed") || i == 4 && code != 401 {
			t.Fatalf("after %d: %d %v", i+1, code, out)
		}
	}
	if count(t, c, tenant, `SELECT count(*) FROM agent_webhook_setups WHERE sealed IS NOT NULL`) != 0 {
		t.Fatal("a failed attempt kept the URL and key")
	}
	// A fresh link, then unwatch: it stops working with the session's links.
	sub2, _ := subscribe(app, call, "ci/grok", subOpts{})
	token2 := setupToken(t, sub2.Addition.Text)
	if code, _ := describe(t, c, token2); code != 200 {
		t.Fatalf("fresh link: %d", code)
	}
	if _, err := unsubscribe(app, call, "ci/grok"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the link to stop working", func() bool { code, _ := describe(t, c, token2); return code == 401 })
	if code, _ := save(t, c, token2, url, key); code != 401 {
		t.Fatalf("a save after unwatch: %d", code)
	}
}

// With WAKESHIFT_GROK_WEBHOOKS off, Grok Bot waits in the foreground as
// before, and the routes answer 404.
func TestGrokWebhooksOff(t *testing.T) {
	c := coretest.StartWith(t, func(o *core.Options) { o.GrokWebhooks = false })
	app := noteApp(t, c, "ci")
	sub, err := subscribe(app, caller{User: "gina", Client: "Cursor", Harness: "grok-bot", Conversation: grokBotID}, "ci/grok", subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(sub.Addition.Text, "Grok Bot can't be woken by a command in the background") || strings.Contains(sub.Addition.Text, "openssl") {
		t.Fatalf("off: %s", sub.Addition.Text)
	}
	for _, path := range []string{"/v1/stream/proof", "/v1/stream/webhook-setup/describe", "/v1/stream/webhook-setup"} {
		if code, _, _ := page(t, c, path, map[string]string{}); code != 404 {
			t.Fatalf("%s: %d", path, code)
		}
	}
}

// Knowing a bot's ID (as every app it uses does) isn't enough to read its
// setup check, and so to use it up with bad proofs or fail the user's
// attempt: the fetch must be signed by the key the setup was minted for.
// Unsigned, signed by another key, or signed long ago, the answer is the
// same "nothing waiting"; the bot's own run, signed, gets it and binds.
func TestGrokSetupFetchNeedsTheKey(t *testing.T) {
	c := startGrok(t, nil)
	f := newFakeCursor(t, c)
	app := noteApp(t, c, "ci")
	bot := &grokBot{c: c, vm: newGrokVM(t), id: grokBotID}
	sub, err := subscribe(app, bot.call("gina"), "ci/grok", subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	token := setupToken(t, bot.keyStep(t, sub.Addition.Text))
	url, key := f.webhook("fetchHook01", bot)
	f.set("fetchHook01", func(h *fakeHook) { h.silent = true })
	if code, out := save(t, c, token, url, key); code != 200 || out["awaiting_proof"] != true {
		t.Fatalf("save: %d %v", code, out)
	}
	signed := func(priv ed25519.PrivateKey, ts string) (int, string) {
		der, _ := x509.MarshalPKIXPublicKey(priv.Public())
		var b bytes.Buffer
		w := multipart.NewWriter(&b)
		w.WriteField("bot", grokBotID)
		w.WriteField("public_key", string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})))
		w.WriteField("time", ts)
		w.WriteField("signature", base64.StdEncoding.EncodeToString(ed25519.Sign(priv, botkeys.FetchMessage(grokBotID, ts))))
		w.Close()
		resp, err := http.Post(c.URL+"/v1/stream/proof/challenge", w.FormDataContentType(), &b)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(raw)
	}
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	now := time.Now()
	unsignedCode, unsigned := fetchChallenge(t, c, grokBotID, "")
	otherCode, otherBody := signed(other, strconv.FormatInt(now.Unix(), 10))
	if unsignedCode != 404 || otherCode != 404 || unsigned != otherBody || !strings.Contains(unsigned, "No Wakeshift check is waiting") {
		t.Fatalf("unsigned %d %q, another key %d %q", unsignedCode, unsigned, otherCode, otherBody)
	}
	// The bot's own key, but an old signature: refused too.
	raw, err := os.ReadFile(filepath.Join(bot.vm.dir(bot.id), "signing.pem"))
	if err != nil {
		t.Fatal(err)
	}
	blk, _ := pem.Decode(raw)
	k, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if code, _ := signed(k.(ed25519.PrivateKey), strconv.FormatInt(now.Add(-time.Hour).Unix(), 10)); code != 404 {
		t.Fatalf("an old signature: %d", code)
	}
	if code, body := signed(k.(ed25519.PrivateKey), strconv.FormatInt(now.Unix(), 10)); code != 200 || !strings.HasPrefix(body, "wakeshift:v1:setup:") {
		t.Fatalf("the bot's key: %d %q", code, body)
	}
	// The bot's routine, run as written, binds.
	if out := bot.vm.run(t, bot.id, harnesses.GrokProveCommand(c.URL, bot.id, "")); !strings.Contains(out, "Wake-ups are on") {
		t.Fatalf("the routine's command: %s", out)
	}
}
