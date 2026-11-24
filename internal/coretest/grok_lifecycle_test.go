package coretest_test

import (
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/agenthooks"
	"github.com/wakeshift/server/internal/core"
	"github.com/wakeshift/server/internal/coretest"
	"github.com/wakeshift/server/internal/harnesses"
)

// hookState is a workspace's Cursor destination: its state, dead reason
// and whether it needs setup.
func hookState(t *testing.T, c *coretest.Core, tenant string) (state, reason string, needsSetup bool) {
	t.Helper()
	err := c.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT state, coalesce(dead_reason, ''), needs_setup_at IS NOT NULL FROM agent_webhooks WHERE kind = 'cursor-automations'`).
			Scan(&state, &reason, &needsSetup)
	})
	if err == pgx.ErrNoRows {
		return "", "", false
	}
	if err != nil {
		t.Fatal(err)
	}
	return state, reason, needsSetup
}

// A missing key fails clearly and falls back. During setup, the run's
// prove says the key is gone and sends nothing (the setup fetch needs the
// key), and the attempt ends unconfirmed; the next subscribing result
// gives the link again with the steps for a lost key, which make a new key
// whose proof gets a link for it, and binding that replaces the old key.
// In a chat, a prove run before any init reports the key missing on its
// register challenge: only that app's session needs setup (with the
// foreground command), and the first app's webhook keeps delivering. A
// destination that needs setup gets no POSTs; its messages wait for the
// foreground wait.
func TestGrokKeyMissing(t *testing.T) {
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
	bot.loseKey(t) // the VM was reset before the user pasted the webhook
	url, key := f.webhook("missHook001", bot)
	if code, _ := save(t, c, token, url, key); code != 200 {
		t.Fatal(code)
	}
	runs := f.ran()
	if len(runs) != 1 || !strings.HasPrefix(runs[0], "Wakeshift key missing: ") || !strings.Contains(runs[0], "nothing was sent") {
		t.Fatalf("the run: %v", runs)
	}
	if _, err := os.Stat(filepath.Join(bot.vm.dir(bot.id), "signing.pem")); !os.IsNotExist(err) {
		t.Fatal("the key was remade")
	}
	expireProof(t, c, tenant)
	if code, out := describe(t, c, token); code != 200 || out["state"] != "open" || out["error"] != "unconfirmed" {
		t.Fatalf("page: %d %v", code, out)
	}
	later, _ := subscribe(app, call, "ci/grok", subOpts{})
	text := later.Addition.Text
	if setupToken(t, text) != token || !strings.Contains(text, "If this bot's routine said its Wakeshift key is missing") || waitCmdRE.FindString(text) == "" {
		t.Fatalf("after the missing key: %s", text)
	}
	// Meanwhile messages come by the foreground wait.
	emitNote(app, []string{"ci/grok"}, note{Text: "by the foreground"})
	waitFor(t, "the message to be made", func() bool { id, _ := deliveryOf(t, c, tenant, "by the foreground"); return id != "" })
	if lines := runCommand(t, "curl -sN '"+waitCmdRE.FindStringSubmatch(text)[1]+"&timeout=5s'"); !strings.Contains(strings.Join(lines, "\n"), "by the foreground") {
		t.Fatalf("foreground wait: %v", lines)
	}

	// The user asks to set up again: the lost-key steps make a new key,
	// whose proof gets a link for it; the same webhook, pasted there, binds.
	answer := bot.keyStep(t, text)
	if !strings.Contains(answer, "isn't the key this bot registered before") {
		t.Fatalf("re-registration: %s", answer)
	}
	token2 := setupToken(t, answer)
	if code, _ := save(t, c, token2, url, key); code != 200 {
		t.Fatal(code)
	}
	waitOn(t, c, token2)
	if count(t, c, tenant, `SELECT count(*) FROM audit_log WHERE action = 'bot_key.replaced'`) != 1 || count(t, c, tenant, `SELECT count(*) FROM bot_keys WHERE missing_at IS NULL`) != 1 {
		t.Fatal("the key wasn't replaced")
	}
	if st, _, ns := hookState(t, c, tenant); st != "live" || ns {
		t.Fatalf("after re-setup: %s %v", st, ns)
	}

	// Another app in the same bot, with the key gone again and a prove run
	// before any init (the key file went between the two): the connector
	// says the key is missing and tells Wakeshift; only that app's session
	// needs setup.
	bot.loseKey(t)
	other := noteApp(t, c, "cd")
	ocall := bot.call("gina")
	osub, err := subscribe(other, ocall, "cd/grok", subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	ocall.Session = osub.Session
	if a := bot.vm.run(t, bot.id, proveCmdRE.FindStringSubmatch(osub.Addition.Text)[1]); !strings.HasPrefix(a, "Wakeshift key missing: ") || !strings.Contains(a, "wake-ups for Cd are off") {
		t.Fatalf("other app: %s", a)
	}
	if again, _ := subscribe(other, ocall, "cd/grok", subOpts{}); !strings.Contains(again.Addition.Text, "This bot's Wakeshift key is gone") {
		t.Fatalf("other app later: %s", again.Addition.Text)
	}
	emitNote(app, []string{"ci/grok"}, note{Text: "still through the webhook"})
	waitFor(t, "the first app's webhook", func() bool { _, s := deliveryOf(t, c, tenant, "still through the webhook"); return s == "acked" })

	// The routine is deleted; setting the webhook up again finds the key
	// gone. Only that attempt fails: the session's destination stays as it
	// was (dead, not marked as needing setup), its key isn't marked
	// missing, and the next subscribing result offers the key step.
	f.remove("missHook001")
	time.Sleep(400 * time.Millisecond)
	emitNote(app, []string{"ci/grok"}, note{Text: "to a deleted routine"})
	waitFor(t, "the destination to die", func() bool { st, _, _ := hookState(t, c, tenant); return st == "dead" })
	dead, _ := subscribe(app, call, "ci/grok", subOpts{})
	token3 := setupToken(t, dead.Addition.Text)
	url3, key3 := f.webhook("missHook002", bot)
	keysMissing := count(t, c, tenant, `SELECT count(*) FROM bot_keys WHERE missing_at IS NOT NULL`)
	if code, _ := save(t, c, token3, url3, key3); code != 200 {
		t.Fatal(code)
	}
	if runs := f.ran(); !strings.HasPrefix(runs[len(runs)-1], "Wakeshift key missing: ") {
		t.Fatalf("the run with the key gone: %v", runs)
	}
	expireProof(t, c, tenant)
	if st, _, ns := hookState(t, c, tenant); st != "dead" || ns {
		t.Fatalf("a key missing during setup changed the binding: %s, needs setup %v", st, ns)
	}
	if count(t, c, tenant, `SELECT count(*) FROM bot_keys WHERE missing_at IS NOT NULL`) != keysMissing {
		t.Fatal("a key missing during setup marked the session's key")
	}
	if code, out := describe(t, c, token3); code != 200 || out["state"] != "open" || out["error"] != "unconfirmed" {
		t.Fatalf("the link after one failed attempt: %d %v", code, out)
	}
	if next, _ := subscribe(app, call, "ci/grok", subOpts{}); !strings.Contains(next.Addition.Text, "If this bot's routine said its Wakeshift key is missing") {
		t.Fatalf("after the failed attempt: %s", next.Addition.Text)
	}
	if err := c.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE agent_webhooks SET needs_setup_at = now()`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// A destination that needs setup gets no POSTs, even live.
	if err := c.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE agent_webhooks SET state = 'live', dead_reason = NULL`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	before := len(f.of(agenthooks.TypeEvents))
	emitNote(app, []string{"ci/grok"}, note{Text: "not through the webhook"})
	time.Sleep(time.Second)
	if _, s := deliveryOf(t, c, tenant, "not through the webhook"); s != "pending" || len(f.of(agenthooks.TypeEvents)) != before {
		t.Fatalf("delivered while it needs setup: %s", s)
	}
}

// A dead webhook: Cursor refusing the key (rotated) kills it, messages
// wait, the next result of any tool says so once, the next subscribing
// result gives a new link (and the foreground command); the user's paste
// of the new key, proved by the bot, revives it, and what waited goes. A
// deleted routine (404) kills it too, with its own reason. 5xx, a redirect
// and a 200 without success are retried with the same body and
// Idempotency-Key, never acknowledged.
func TestGrokDeadWebhook(t *testing.T) {
	c := startGrok(t, nil)
	f := newFakeCursor(t, c)
	app := noteApp(t, c, "ci")
	bot := &grokBot{c: c, vm: newGrokVM(t), id: grokBotID}
	sub, call, _ := grokSetUp(t, c, f, app, bot, "gina", "ci/grok", "deadHook001")
	tenant, _ := topicSubRow(t, c, sub.ID)

	// Retries.
	f.answer(500, http.StatusFound, grokNoSuccess)
	emitNote(app, []string{"ci/grok"}, note{Text: "retried"})
	waitFor(t, "the retried message", func() bool { _, s := deliveryOf(t, c, tenant, "retried"); return s == "acked" })
	evs := f.of(agenthooks.TypeEvents)
	if len(evs) != 4 || evs[0].Idempotency == "" || evs[0].Idempotency != evs[3].Idempotency || string(evs[0].Body.Events[0]) != string(evs[3].Body.Events[0]) {
		t.Fatalf("retries: %d POSTs", len(evs))
	}

	// Rotated.
	newKey := f.rotate("deadHook001")
	emitNote(app, []string{"ci/grok"}, note{Text: "after the rotation"})
	waitFor(t, "the destination to die", func() bool { st, _, _ := hookState(t, c, tenant); return st == "dead" })
	if _, r, _ := hookState(t, c, tenant); r != "unauthorized" {
		t.Fatalf("reason %s", r)
	}
	if _, s := deliveryOf(t, c, tenant, "after the rotation"); s != "pending" {
		t.Fatalf("the message: %s", s)
	}
	if r, _ := annotate(app, call); r.Text != harnesses.GrokStoppedLine("Ci", "unauthorized") {
		t.Fatalf("stopped line: %q", r.Text)
	}
	if r, _ := annotate(app, call); r.Text != "" {
		t.Fatalf("stopped line twice: %q", r.Text)
	}
	again, _ := subscribe(app, call, "ci/grok", subOpts{})
	text := again.Addition.Text
	if !strings.Contains(text, harnesses.GrokReasonUnauthorized) || waitCmdRE.FindString(text) == "" || !strings.Contains(text, harnesses.GrokRoutineName) {
		t.Fatalf("dead note: %s", text)
	}
	token := setupToken(t, text)
	url := f.srv.URL + agenthooks.CursorPath + "deadHook001"
	if code, out := save(t, c, token, url, newKey); code != 200 {
		t.Fatalf("save: %d %v", code, out)
	}
	waitOn(t, c, token)
	waitFor(t, "what waited", func() bool { _, s := deliveryOf(t, c, tenant, "after the rotation"); return s == "acked" })
	if count(t, c, tenant, `SELECT count(*) FROM audit_log WHERE action = 'agent_webhook.registered' AND (detail->>'replaced')::bool`) != 1 {
		t.Fatal("not audited as replaced")
	}

	// Deleted.
	f.remove("deadHook001")
	time.Sleep(400 * time.Millisecond)
	emitNote(app, []string{"ci/grok"}, note{Text: "after the deletion"})
	waitFor(t, "the destination to die", func() bool { st, _, _ := hookState(t, c, tenant); return st == "dead" })
	if _, r, _ := hookState(t, c, tenant); r != "not_found" {
		t.Fatalf("reason %s", r)
	}
	if later, _ := subscribe(app, call, "ci/grok", subOpts{}); !strings.Contains(later.Addition.Text, harnesses.GrokReasonNotFound) {
		t.Fatalf("dead note: %s", later.Addition.Text)
	}
}

// Joins: another app in the same bot proves the same key on its own
// challenge and joins the bot's webhook, with no routine and no page, in
// its own workspace only, announced through the webhook once; its messages
// then go there too. A join needs the bot's webhook live, and the same
// bot: another bot of the same user, with its own key, gets the setup path,
// while one holding this bot's key and ID (the accepted risk) joins, and
// it's announced. The removal page stops every app's destination, each
// audited in its own workspace. Each destination is paced: past its hourly
// cap, messages wait.
func TestGrokJoinAndPacing(t *testing.T) {
	c := startGrok(t, func(o *core.Options) { o.GrokPacing.PerHour = 3 })
	f := newFakeCursor(t, c)
	vm := newGrokVM(t)
	bot := &grokBot{c: c, vm: vm, id: grokBotID}
	a := noteApp(t, c, "ci")
	asub, acall, _ := grokSetUp(t, c, f, a, bot, "gina", "ci/grok", "joinHook001")
	atenant, _ := topicSubRow(t, c, asub.ID)

	b := noteApp(t, c, "cd")
	bcall := bot.call("gina")
	bsub, err := subscribe(b, bcall, "cd/grok", subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	bcall.Session = bsub.Session
	btenant, _ := topicSubRow(t, c, bsub.ID)
	if btenant == atenant {
		t.Fatal("one workspace for two apps")
	}
	emitNote(b, []string{"cd/grok"}, note{Text: "waited for the join"})
	if answer := bot.keyStep(t, bsub.Addition.Text); strings.TrimSpace(answer) != harnesses.GrokJoinedNote("Cd") {
		t.Fatalf("join: %s", answer)
	}
	waitFor(t, "the join's announcement", func() bool { return len(f.of(agenthooks.TypeJoined)) == 1 })
	j := f.of(agenthooks.TypeJoined)[0]
	if j.ID != "joinHook001" || j.Body.App != "Cd" || j.Body.Bot != grokBotID || !strings.Contains(j.Body.Notice, "now wake this bot too") {
		t.Fatalf("announcement: %+v", j)
	}
	waitFor(t, "what waited", func() bool { _, s := deliveryOf(t, c, btenant, "waited for the join"); return s == "acked" })
	if count(t, c, btenant, `SELECT count(*) FROM audit_log WHERE action = 'agent_webhook.registered' AND actor = 'agent' AND (detail->>'joined')::bool AND detail->>'via' = 'bot_key'`) != 1 ||
		count(t, c, atenant, `SELECT count(*) FROM agent_webhooks`) != 1 {
		t.Fatal("join not audited, or not in its own workspace")
	}
	if len(f.of(agenthooks.TypeJoined)) != 1 {
		t.Fatal("announced twice")
	}
	if on, _ := subscribe(b, bcall, "cd/grok", subOpts{}); !strings.HasPrefix(on.Addition.Text, harnesses.GrokRegisteredNote("Cd")) {
		t.Fatalf("joined, later: %s", on.Addition.Text)
	}

	// Another bot of the same user, with its own key: no join.
	bot2 := &grokBot{c: c, vm: vm, id: "2b3c4d5e-6f70-4812-9a3b-4c5d6e7f8091"}
	d := noteApp(t, c, "de")
	dsub, _ := subscribe(d, bot2.call("gina"), "de/grok", subOpts{})
	if answer := bot2.keyStep(t, dsub.Addition.Text); !strings.Contains(answer, "Now set up the webhook") {
		t.Fatalf("another bot: %s", answer)
	}
	// One that reads this bot's key but signs as itself: no join (the
	// webhook is for another bot).
	k1, _ := os.ReadFile(filepath.Join(vm.dir(grokBotID), "signing.pem"))
	bot3 := &grokBot{c: c, vm: vm, id: "3c4d5e6f-7081-4923-8b4c-5d6e7f8091a2", memory: true}
	os.MkdirAll(vm.dir(bot3.id), 0o700)
	os.WriteFile(filepath.Join(vm.dir(bot3.id), "signing.pem"), k1, 0o600)
	d3 := noteApp(t, c, "dx")
	d3sub, _ := subscribe(d3, bot3.call("gina"), "dx/grok", subOpts{})
	if answer := bot3.keyStep(t, d3sub.Addition.Text); !strings.Contains(answer, "Now set up the webhook") {
		t.Fatalf("another bot with this bot's key: %s", answer)
	}
	// One that reads this bot's key and claims its ID joins (accepted
	// risk), announced.
	e := noteApp(t, c, "ef")
	thief := &grokBot{c: c, vm: vm, id: grokBotID, memory: true}
	esub, _ := subscribe(e, thief.call("gina"), "ef/grok", subOpts{})
	if answer := thief.keyStep(t, esub.Addition.Text); strings.TrimSpace(answer) != harnesses.GrokJoinedNote("Ef") {
		t.Fatalf("accepted risk: %s", answer)
	}
	waitFor(t, "its announcement", func() bool { return len(f.of(agenthooks.TypeJoined)) == 2 })

	// A dead webhook can't be joined.
	if err := c.DB.InTenant(ctx, atenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE agent_webhooks SET state = 'dead', dead_reason = 'not_found'`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	g := noteApp(t, c, "gh")
	gsub, _ := subscribe(g, bot.call("gina"), "gh/grok", subOpts{})
	etenant, _ := topicSubRow(t, c, esub.ID)
	if err := c.DB.InTenant(ctx, etenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE agent_webhooks SET state = 'dead', dead_reason = 'not_found'`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := c.DB.InTenant(ctx, btenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE agent_webhooks SET state = 'dead', dead_reason = 'not_found'`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if answer := bot.keyStep(t, gsub.Addition.Text); !strings.Contains(answer, "Now set up the webhook") {
		t.Fatalf("join of a dead webhook: %s", answer)
	}
	for _, tn := range []string{atenant, btenant, etenant} {
		if err := c.DB.InTenant(ctx, tn, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE agent_webhooks SET state = 'live', dead_reason = NULL`)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}

	// Pacing: at most 3 POSTs an hour to A's destination.
	time.Sleep(400 * time.Millisecond)
	posts := func() int {
		n := 0
		for _, r := range f.of(agenthooks.TypeEvents) {
			if r.Body.App == "Ci" {
				n++
			}
		}
		return n
	}
	for i := range 5 {
		emitNote(a, []string{"ci/grok"}, note{Text: "paced " + string(rune('a'+i))})
		time.Sleep(450 * time.Millisecond)
	}
	time.Sleep(time.Second)
	if n := posts(); n != 3 {
		t.Fatalf("%d POSTs to the paced destination in the hour", n)
	}
	if _, s := deliveryOf(t, c, atenant, "paced e"); s != "pending" {
		t.Fatalf("past the cap: %s", s)
	}

	// Stopping through the agent: Ci's unwatch ends Ci's destination only,
	// audited in its workspace; the joined apps keep theirs.
	if _, err := unsubscribe(a, acall, "ci/grok"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "Ci's destination to end", func() bool { return count(t, c, atenant, `SELECT count(*) FROM agent_webhooks`) == 0 })
	if count(t, c, atenant, `SELECT count(*) FROM audit_log WHERE action = 'agent_webhook.unregistered'`) != 1 {
		t.Fatal("the unwatch wasn't audited")
	}
	for _, tn := range []string{btenant, etenant} {
		if count(t, c, tn, `SELECT count(*) FROM agent_webhooks`) != 1 {
			t.Fatalf("workspace %s lost its destination", tn)
		}
	}
}

func countPlatform(t *testing.T, c *coretest.Core, q string) int {
	t.Helper()
	var n int
	if err := c.DB.Pool.QueryRow(ctx, q).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Nothing logged carries the webhook's key or ID, a setup or removal
// token, a challenge, the bot's private key or its ID.
func TestGrokSecretsNeverLogged(t *testing.T) {
	var logs syncBuffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	c := startGrok(t, nil)
	f := newFakeCursor(t, c)
	app := noteApp(t, c, "ci")
	bot := &grokBot{c: c, vm: newGrokVM(t), id: grokBotID}
	sub, call, token := grokSetUp(t, c, f, app, bot, "gina", "ci/grok", "logCanary99")
	tenant, _ := topicSubRow(t, c, sub.ID)
	emitNote(app, []string{"ci/grok"}, note{Text: "logged?"})
	waitFor(t, "the message", func() bool { _, s := deliveryOf(t, c, tenant, "logged?"); return s == "acked" })
	f.answer(500)
	emitNote(app, []string{"ci/grok"}, note{Text: "retried"})
	waitFor(t, "the retry", func() bool { _, s := deliveryOf(t, c, tenant, "retried"); return s == "acked" })
	unsubscribe(app, call, "ci/grok")
	priv, _ := os.ReadFile(filepath.Join(bot.vm.dir(bot.id), "signing.pem"))
	privLine := strings.Split(string(priv), "\n")[1]
	reg := challengeIn(t, c, tenant, "register")
	setup := challengeIn(t, c, tenant, "setup")
	_, key := f.webhook("logCanary99", bot)
	for name, secret := range map[string]string{"webhook key": key, "webhook ID": "logCanary99", "setup token": token,
		"register challenge": reg, "setup challenge": setup, "private key": privLine, "bot ID": grokBotID} {
		if strings.Contains(logs.String(), secret) {
			t.Errorf("the log carries the %s", name)
		}
	}
}

// expireProof lets every setup attempt's 10 minutes for a proof pass.
func expireProof(t *testing.T, c *coretest.Core, tenant string) {
	t.Helper()
	if err := c.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE agent_webhook_setups SET proof_due = now() - interval '1 second' WHERE proof_due IS NOT NULL`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}
