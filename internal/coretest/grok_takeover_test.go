package coretest_test

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/agenthooks"
	"github.com/wakeshift/server/internal/botkeys"
	"github.com/wakeshift/server/internal/coretest"
	"github.com/wakeshift/server/internal/harnesses"
)

// signedFetch fetches the bot's open setup challenge as the connector
// does, signed by k.
func signedFetch(t *testing.T, c *coretest.Core, bot string, k testKey) (int, string) {
	t.Helper()
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	var b bytes.Buffer
	w := multipart.NewWriter(&b)
	w.WriteField("bot", bot)
	w.WriteField("public_key", k.pem())
	w.WriteField("time", ts)
	w.WriteField("signature", base64.StdEncoding.EncodeToString(ed25519.Sign(k.priv, botkeys.FetchMessage(bot, ts))))
	w.Close()
	resp, err := http.Post(c.URL+"/v1/stream/proof/challenge", w.FormDataContentType(), &b)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

// botKeyFP is the workspace's one bot key's fingerprint.
func botKeyFP(t *testing.T, c *coretest.Core, tenant string) []byte {
	t.Helper()
	var fp []byte
	if err := c.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT fingerprint FROM bot_keys LIMIT 1`).Scan(&fp)
	}); err != nil {
		t.Fatal(err)
	}
	return fp
}

// eventsAt: the fake Cursor got events from app at webhook id.
func eventsAt(f *fakeCursor, id, app string) bool {
	for _, r := range f.of(agenthooks.TypeEvents) {
		if r.ID == id && r.Body.App == app {
			return true
		}
	}
	return false
}

// A malicious app M that the bot once used (so M's session holds the
// bot's key K) proves its own key on a register challenge of its own
// session, takes the rekey setup link the proof answers, pastes its own
// webhook and proves the setup with its own key. That binds M's own
// session only: no other app's destination, in any workspace, gets M's key
// or webhook, and V's events keep going to V's webhook.
func TestGrokRekeyDoesNotTakeOverPeers(t *testing.T) {
	c := startGrok(t, nil)
	f := newFakeCursor(t, c)
	bot := &grokBot{c: c, vm: newGrokVM(t), id: grokBotID}

	// The victim's real setup through app V.
	v := noteApp(t, c, "ci")
	vsub, _, _ := grokSetUp(t, c, f, v, bot, "gina", "ci/grok", "realHook001")
	vtenant, _ := topicSubRow(t, c, vsub.ID)
	realFP := botKeyFP(t, c, vtenant)

	// The bot also uses the malicious app M, and joins as designed.
	m := noteApp(t, c, "cd")
	mcall := bot.call("gina")
	msub, err := subscribe(m, mcall, "cd/grok", subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	mcall.Session = msub.Session
	if out := bot.keyStep(t, msub.Addition.Text); !strings.Contains(out, "now it wakes it for") {
		t.Fatalf("join: %s", out)
	}
	mtenant, _ := topicSubRow(t, c, msub.ID)

	// M, on its own: unwatch, subscribe again for a fresh register
	// challenge, and prove its own key on it.
	if _, err := unsubscribe(m, mcall, "cd/grok"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "M's destination to end", func() bool { st, _, _ := hookState(t, c, mtenant); return st == "" })
	msub2, err := subscribe(m, mcall, "cd/grok", subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	ch := registerCh(t, c, msub2.Addition.Text)
	evil := newTestKey()
	_, out := prove(t, c, evil.form(bot.id, ch))
	token := setupToken(t, out)

	// M's own webhook (a routine in M's own bot); M proves directly.
	evilBot := &grokBot{c: c, vm: newGrokVM(t), id: "7e6d5c4b-3a29-4180-9f7e-6d5c4b3a2918"}
	eurl, ekey := f.webhook("evilHook001", evilBot)
	f.set("evilHook001", func(h *fakeHook) { h.silent = true })
	if code, o := save(t, c, token, eurl, ekey); code != 200 {
		t.Fatalf("M's save: %d %v", code, o)
	}
	code, sch := signedFetch(t, c, bot.id, evil)
	if code != 200 {
		t.Fatalf("setup fetch: %d %s", code, sch)
	}
	if code, out := prove(t, c, evil.form(bot.id, strings.TrimSpace(sch))); code != 200 || !strings.Contains(out, "Wake-ups are on") {
		t.Fatalf("M's setup proof (its own session): %d %s", code, out)
	}
	// What a carry-over would do has had time to happen.
	time.Sleep(500 * time.Millisecond)

	// V's destination, in V's workspace: still V's key and webhook.
	if !bytes.Equal(botKeyFP(t, c, vtenant), realFP) {
		t.Fatal("V's key was replaced by M's")
	}
	if !liveHook(t, c, vtenant) {
		t.Fatal("V's destination isn't live")
	}
	emitNote(v, []string{"ci/grok"}, note{Text: "V's private event"})
	waitFor(t, "V's event at V's webhook", func() bool { return eventsAt(f, "realHook001", "Ci") })
	if eventsAt(f, "evilHook001", "Ci") {
		t.Fatal("V's event went to M's webhook")
	}
	if n := count(t, c, vtenant, `SELECT count(*) FROM audit_log WHERE action = 'bot_key.replaced'`); n != 0 {
		t.Fatalf("%d key replacements in V's workspace", n)
	}
}

// The same bot, with the same key, sets up its webhook again (a new
// routine, after the old one was deleted): every other app's live
// destination on that key follows to the new webhook; one the deleted
// routine already killed isn't revived by another app's setup.
func TestGrokResetupSameKeyCarriesOver(t *testing.T) {
	c := startGrok(t, nil)
	f := newFakeCursor(t, c)
	bot := &grokBot{c: c, vm: newGrokVM(t), id: grokBotID}
	a := noteApp(t, c, "ci")
	asub, acall, _ := grokSetUp(t, c, f, a, bot, "gina", "ci/grok", "sameHook001")
	atenant, _ := topicSubRow(t, c, asub.ID)
	join := func(app, topic string) string {
		sub, err := subscribe(noteApp(t, c, app), bot.call("gina"), topic, subOpts{})
		if err != nil {
			t.Fatal(err)
		}
		if out := bot.keyStep(t, sub.Addition.Text); !strings.Contains(out, "now it wakes it for") {
			t.Fatalf("join: %s", out)
		}
		tenant, _ := topicSubRow(t, c, sub.ID)
		return tenant
	}
	btenant := join("cd", "cd/grok")
	ctenant := join("ef", "ef/grok")
	fp := botKeyFP(t, c, atenant)

	// The routine is deleted: A's and E's destinations die as they send;
	// B's, which sends nothing, stays live.
	f.remove("sameHook001")
	time.Sleep(400 * time.Millisecond)
	emitNote(a, []string{"ci/grok"}, note{Text: "a to a deleted routine"})
	waitFor(t, "A's destination to die", func() bool { st, _, _ := hookState(t, c, atenant); return st == "dead" })
	// E's destination was killed the same way.
	if err := c.DB.InTenant(ctx, ctenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE agent_webhooks SET state = 'dead', dead_reason = 'not_found', dead_at = now()`)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	// A sets up again, with the same key, on a new routine.
	dead, _ := subscribe(a, acall, "ci/grok", subOpts{})
	token := setupToken(t, dead.Addition.Text)
	url, key := f.webhook("sameHook002", bot)
	if code, out := save(t, c, token, url, key); code != 200 {
		t.Fatalf("save: %d %v", code, out)
	}
	waitOn(t, c, token)

	// B follows, on the same key; E stays dead.
	waitFor(t, "B on the new webhook", func() bool {
		var routine, aroutine []byte
		c.DB.Pool.QueryRow(ctx, `SELECT routine FROM bot_webhooks WHERE tenant_id = $1`, btenant).Scan(&routine)
		c.DB.Pool.QueryRow(ctx, `SELECT routine FROM bot_webhooks WHERE tenant_id = $1`, atenant).Scan(&aroutine)
		return bytes.Equal(routine, aroutine)
	})
	if !liveHook(t, c, btenant) || !bytes.Equal(botKeyFP(t, c, btenant), fp) {
		t.Fatal("B isn't live on the same key")
	}
	if st, _, _ := hookState(t, c, ctenant); st != "dead" {
		t.Fatalf("E's killed destination was revived: %s", st)
	}
	emitNote(a, []string{"ci/grok"}, note{Text: "a after the re-setup"})
	waitFor(t, "A's event at the new webhook", func() bool { return eventsAt(f, "sameHook002", "Ci") })
}

// fetchV2 fetches a register challenge as a v2 connector does: the
// challenge, then what it's for.
func fetchV2(t *testing.T, c *coretest.Core, bot, id string) (int, string) {
	t.Helper()
	var b bytes.Buffer
	w := multipart.NewWriter(&b)
	w.WriteField("bot", bot)
	w.WriteField("challenge", id)
	w.WriteField("v", "2")
	w.Close()
	resp, err := http.Post(c.URL+"/v1/stream/proof/challenge", w.FormDataContentType(), &b)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

// An unsigned report that the key is missing, on a register challenge
// still open for a session whose webhook is live (anyone holding the
// challenge can send one), changes nothing: the key isn't marked missing,
// the destination doesn't need setup and keeps delivering, and the answer
// doesn't carry the session's wait URL.
func TestGrokUnsignedMissingReportChangesNothing(t *testing.T) {
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
	// Subscribing again while the link is open hands out a register
	// challenge (for a lost key), which stays open after the bind.
	pending, _ := subscribe(app, call, "ci/grok", subOpts{})
	ch := registerCh(t, c, pending.Addition.Text)
	url, key := f.webhook("missLive01", bot)
	if code, out := save(t, c, token, url, key); code != 200 {
		t.Fatalf("save: %d %v", code, out)
	}
	waitOn(t, c, token)

	// The answer says wake-ups go on (the webhook is live), not that
	// they're off until the user sets up again.
	code, body := prove(t, c, map[string]string{"challenge": ch, "missing": "1"})
	if code != 200 || strings.Contains(body, harnesses.LinkStart) || !strings.Contains(body, "Wake-ups for Ci continue") || strings.Contains(body, "are off") {
		t.Fatalf("the report: %d %s", code, body)
	}
	if st, _, ns := hookState(t, c, tenant); st != "live" || ns {
		t.Fatalf("the destination after an unsigned report: %s, needs setup %v", st, ns)
	}
	if n := count(t, c, tenant, `SELECT count(*) FROM bot_keys WHERE missing_at IS NOT NULL`); n != 0 {
		t.Fatal("an unsigned report marked the key missing")
	}
	emitNote(app, []string{"ci/grok"}, note{Text: "still through the webhook"})
	waitFor(t, "the message through the webhook", func() bool { _, s := deliveryOf(t, c, tenant, "still through the webhook"); return s == "acked" })
}

// A register proof says, and signs, which app in which workspace it's
// for. The connector prints the app, the workspace and the key's
// fingerprint before it sends anything; the prove command Wakeshift
// writes pins the app, so a command whose challenge ID was swapped for
// another app's challenge (here: an app in another workspace that
// recorded a session under this bot's ID) is refused and nothing is
// signed; a v2 proof signed for another app doesn't verify; and the setup
// page shows the fingerprint of the key it binds.
func TestGrokRegisterProofNamesItsApp(t *testing.T) {
	c := startGrok(t, nil)
	f := newFakeCursor(t, c)
	bot := &grokBot{c: c, vm: newGrokVM(t), id: grokBotID}
	v := noteApp(t, c, "ci")
	call := bot.call("gina")
	sub, err := subscribe(v, call, "ci/grok", subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	vtenant, _ := topicSubRow(t, c, sub.ID)
	prove1 := proveCmdRE.FindStringSubmatch(sub.Addition.Text)
	if prove1 == nil || !strings.HasSuffix(prove1[1], " --app 'ci'") {
		t.Fatalf("the prove command doesn't pin the app: %s", sub.Addition.Text)
	}
	answer := bot.keyStep(t, sub.Addition.Text)
	token := setupToken(t, answer)
	fp := botkeys.Short(botkeys.Fingerprint(fileKey(t, bot).pub))
	if !strings.Contains(bot.shown, "to the app Ci (ci)") || !strings.Contains(bot.shown, vtenant) || !strings.Contains(bot.shown, "fingerprint "+fp) ||
		!strings.Contains(bot.shown, "a workspace the app ci owns") {
		t.Fatalf("what prove showed: %q", bot.shown)
	}
	if code, out := describe(t, c, token); code != 200 || out["key"] != fp {
		t.Fatalf("the page's key: %d %v", code, out)
	}
	url, key := f.webhook("pinHook001", bot)
	if code, out := save(t, c, token, url, key); code != 200 {
		t.Fatalf("save: %d %v", code, out)
	}
	waitOn(t, c, token)

	// Another app, in another workspace, records a session under this
	// bot's ID and gets a register challenge for it.
	m := noteApp(t, c, "cd")
	msub, err := subscribe(m, caller{User: "hank", Client: "Cursor", UserAgent: "Cursor/1.0.0", Harness: "grok-bot", Conversation: grokBotID}, "cd/grok", subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	mtenant, _ := topicSubRow(t, c, msub.ID)
	margs := proveArgsRE.FindStringSubmatch(msub.Addition.Text)
	if margs == nil {
		t.Fatalf("no challenge: %s", msub.Addition.Text)
	}
	// The bot runs Ci's command with Cd's challenge ID in it: refused,
	// nothing signed, nothing joined.
	swapped := proveArgsRE.ReplaceAllString(prove1[1], "grok prove --bot '"+grokBotID+"' --issuer '"+c.URL+"' --challenge '"+margs[2]+"'")
	if !strings.Contains(swapped, margs[2]) || !strings.HasSuffix(swapped, "--app 'ci'") {
		t.Fatalf("swapped command: %s", swapped)
	}
	out := bot.vm.run(t, bot.id, swapped)
	if !strings.Contains(out, "refused") || !strings.Contains(out, "for the app Cd (cd), not ci") || !strings.Contains(out, "Nothing was sent") {
		t.Fatalf("a challenge for another app: %s", out)
	}
	if st, _, _ := hookState(t, c, mtenant); st != "" {
		t.Fatal("the other app's session joined the bot's webhook")
	}
	if n := count(t, c, mtenant, `SELECT count(*) FROM bot_challenges WHERE used_at IS NULL`); n != 1 {
		t.Fatalf("%d open challenges: the refused one was used", n)
	}

	// A v2 proof signed for another app, or another workspace, doesn't
	// verify; signed for the challenge's own, it does.
	code, body := fetchV2(t, c, grokBotID, margs[2])
	lines := strings.Split(strings.TrimSpace(body), "\n")
	if code != 200 || len(lines) != 6 || lines[1] != "app_id=cd" || lines[2] != "app=Cd" || lines[3] != "workspace_id="+mtenant || lines[5] != "workspace_app=cd" {
		t.Fatalf("v2 fetch: %d %q", code, body)
	}
	k := fileKey(t, bot)
	signed := func(tenant, app string) map[string]string {
		return map[string]string{"challenge": lines[0], "public_key": k.pem(), "v": "2",
			"signature": base64.StdEncoding.EncodeToString(ed25519.Sign(k.priv, botkeys.RegisterMessage(grokBotID, lines[0], tenant, app)))}
	}
	for _, form := range []map[string]string{signed(mtenant, "ci"), signed(vtenant, "cd")} {
		if code, body := prove(t, c, form); code != 401 || !strings.Contains(body, "didn't match") {
			t.Fatalf("signed for another app or workspace: %d %s", code, body)
		}
	}
	if code, body := prove(t, c, signed(mtenant, "cd")); code != 200 {
		t.Fatalf("signed for its own app and workspace: %d %s", code, body)
	}
}

// Made-up challenges (a burst of them, with nonces Wakeshift didn't
// derive, for a real challenge's workspace and ID or random ones) are
// refused before the database: each is logged as forged, never looked up
// (which would log "unknown"), and answers as any refusal does; the real
// challenge still works.
func TestGrokForgedChallengesSkipTheDatabase(t *testing.T) {
	var logs syncBuffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	c := startGrok(t, nil)
	app := noteApp(t, c, "ci")
	sub, err := subscribe(app, caller{User: "gina", Client: "Cursor", Harness: "grok-bot", Conversation: grokBotID}, "ci/forged", subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	ch := registerCh(t, c, sub.Addition.Text)
	real, err := botkeys.Parse(ch)
	if err != nil {
		t.Fatal(err)
	}
	k := newTestKey()
	const n = 50
	for i := range n {
		nonce := make([]byte, 16)
		rand.Read(nonce)
		forged := botkeys.Format(botkeys.Register, real.Tenant, real.ID, nonce)
		if i%2 == 1 {
			// A workspace and challenge that don't exist (plainly fake IDs).
			const fakeTenant, fakeID = "11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222"
			forged = botkeys.Format(botkeys.Register, fakeTenant, fakeID, nonce)
		}
		if code, body := prove(t, c, k.form(grokBotID, forged)); code != 401 || strings.TrimSpace(body) != harnesses.GrokChallengeGone {
			t.Fatalf("forged %d: %d %s", i, code, body)
		}
	}
	if got := strings.Count(logs.String(), `"reason":"forged"`); got != n {
		t.Fatalf("%d proofs refused as forged, want %d", got, n)
	}
	if strings.Contains(logs.String(), `"reason":"unknown"`) {
		t.Fatal("a forged challenge was looked up")
	}
	if code, body := prove(t, c, k.form(grokBotID, ch)); code != 200 {
		t.Fatalf("the real challenge after the burst: %d %s", code, body)
	}
}
