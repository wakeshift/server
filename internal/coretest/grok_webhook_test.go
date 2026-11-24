package coretest_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/agenthooks"
	"github.com/wakeshift/server/internal/coretest"
	"github.com/wakeshift/server/internal/harnesses"
)

const grokBotID = "4f1c2b3a-9d8e-4c7b-a6f5-0e1d2c3b4a59"

// The whole setup, end to end: the first subscribing result asks for the
// key step with the exact commands (and the foreground command as the
// fallback); the proof registers the key and answers with the routine and
// a one-time link; later subscribes show the same link; the page describes
// the setup without using it up; the paste sends a challenge through the
// webhook with its key; the run in the bot proves it; only then is the
// webhook bound, audited without a secret, with a running foreground wait
// closed; messages then go to the webhook in one batch, with the key, and
// are acknowledged; the "Stop wake-ups" token removes it.
func TestGrokWebhookSetup(t *testing.T) {
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
	text := sub.Addition.Text
	tenant, _ := topicSubRow(t, c, sub.ID)
	for _, want := range []string{"this bot needs a webhook, set up once", harnesses.GrokInitCommand(c.URL, grokBotID), harnesses.GrokProveCommand(c.URL, grokBotID, ""),
		"/download/wakeshift/", "Don't change the commands or write your own", "Never print, copy or send the key file's contents",
		"wait for messages in the foreground instead: Grok Bot can't be woken by a command in the background"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q: %s", want, text)
		}
	}
	// The file-writing tool is only for the foreground wait's link (its
	// step, and its command's notes), never for the key.
	fg := waitCmdRE.FindStringSubmatch(text)
	if fg == nil || strings.Contains(text, "wakeshift-beacon") || strings.Contains(text, "openssl") ||
		strings.Contains(strings.Replace(coretest.LinkStepRE.ReplaceAllString(text, ""), fg[0], "", 1), "file-writing tool") || strings.Contains(text, "wakeshift:v1:") {
		t.Fatalf("foreground command, or a beacon: %s", text)
	}
	// Shown again while it's open, not replaced.
	again, _ := subscribe(app, call, "ci/grok", subOpts{})
	if proveArgsRE.FindString(again.Addition.Text) != proveArgsRE.FindString(text) {
		t.Fatal("a new register challenge on every subscribe")
	}

	answer := bot.keyStep(t, text)
	for _, want := range []string{"Your Wakeshift key checks out", harnesses.GrokRoutineInstruction(c.URL, grokBotID), `"` + harnesses.GrokRoutineName + `"`, "It works once, within 24 hours"} {
		if !strings.Contains(answer, want) {
			t.Fatalf("proof answer lacks %q: %s", want, answer)
		}
	}
	fi, err := os.Stat(filepath.Join(bot.vm.dir(bot.id), "signing.pem"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("key file: %v", err)
	}
	if count(t, c, tenant, `SELECT count(*) FROM audit_log WHERE action = 'bot_key.registered' AND length(detail->>'fingerprint') = 16`) != 1 ||
		count(t, c, tenant, `SELECT count(*) FROM bot_keys`) != 1 {
		t.Fatal("key not registered or audited")
	}
	token := setupToken(t, answer)
	later, _ := subscribe(app, call, "ci/grok", subOpts{})
	if setupToken(t, later.Addition.Text) != token || !strings.Contains(later.Addition.Text, "isn't set up yet") || !strings.Contains(later.Addition.Text, "If this bot's routine said its Wakeshift key is missing") {
		t.Fatalf("pending: %s", later.Addition.Text)
	}

	// The page: described twice (it isn't used up).
	for range 2 {
		code, out := describe(t, c, token)
		if code != 200 || out["app"] != "Ci" || out["bot_tail"] != grokBotID[len(grokBotID)-6:] || out["state"] != "open" || out["expires_at"] == nil {
			t.Fatalf("describe: %d %v", code, out)
		}
	}
	// A foreground wait running now ends once the webhook is bound.
	m := waitCmdRE.FindStringSubmatch(text)
	waitDone := make(chan []string, 1)
	go func() {
		resp, err := http.Get(m[1] + "&timeout=30s")
		if err != nil {
			waitDone <- nil
			return
		}
		defer resp.Body.Close()
		var lines []string
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			if l := strings.TrimSpace(sc.Text()); l != "" {
				lines = append(lines, l)
			}
		}
		waitDone <- lines
	}()
	time.Sleep(300 * time.Millisecond)

	url, key := f.webhook("grokHookA1", bot)
	code, out := save(t, c, token, url, key)
	if code != 200 || out["awaiting_proof"] != true {
		t.Fatalf("save: %d %v", code, out)
	}
	waitOn(t, c, token)
	ch := f.of(agenthooks.TypeChallenge)
	if len(ch) != 1 || ch[0].Auth != "Bearer "+key || ch[0].Body.Bot != grokBotID || ch[0].Body.App != "Ci" ||
		ch[0].Body.Challenge != "" || len(ch[0].Body.Events) != 0 {
		t.Fatalf("challenge POST: %+v", ch)
	}
	if runs := f.ran(); len(runs) != 1 || !strings.Contains(runs[0], "Wake-ups are on") {
		t.Fatalf("the run answered: %v", runs)
	}
	var state string
	var sealed []byte
	if err := c.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT state, url FROM agent_webhooks WHERE kind = 'cursor-automations'`).Scan(&state, &sealed)
	}); err != nil || state != "live" || bytes.Contains(sealed, []byte(key)) || bytes.Contains(sealed, []byte("grokHookA1")) {
		t.Fatalf("destination: %s %v", state, err)
	}
	if count(t, c, tenant, `SELECT count(*) FROM audit_log WHERE action = 'agent_webhook.registered' AND actor = 'user' AND detail->>'kind' = 'cursor-automations'
			AND detail->>'via' = 'setup_page' AND detail->>'confirmed' = 'bot_key'`) != 1 ||
		count(t, c, tenant, `SELECT count(*) FROM audit_log WHERE detail::text LIKE '%' || $1 || '%' OR detail::text LIKE '%' || $2 || '%' OR detail::text LIKE '%' || $3 || '%'`,
			"grokHookA1", grokBotID, token[:20]) != 0 {
		t.Fatal("audit entry missing, or it holds a secret")
	}
	select {
	case lines := <-waitDone:
		if len(lines) == 0 || !strings.Contains(lines[len(lines)-1], `"reason":"webhook"`) || !strings.Contains(lines[len(lines)-1], "don't run this command again") {
			t.Fatalf("the foreground wait ended with %v", lines)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the foreground wait didn't end on the bind")
	}
	// A new foreground wait ends at once.
	if lines := runCommand(t, "curl -sN '"+m[1]+"&timeout=5s'"); !strings.Contains(lines[len(lines)-1], `"reason":"webhook"`) {
		t.Fatalf("a wait after the bind: %v", lines)
	}

	// Messages now go through the webhook.
	emitNote(app, []string{"ci/grok"}, note{Text: "after the bind"})
	waitFor(t, "the first message", func() bool { _, s := deliveryOf(t, c, tenant, "after the bind"); return s == "acked" })
	// Now subscribing asks for nothing.
	on, _ := subscribe(app, call, "ci/grok", subOpts{})
	if !strings.HasPrefix(on.Addition.Text, harnesses.GrokRegisteredNote("Ci")) || strings.Contains(on.Addition.Text, "curl") {
		t.Fatalf("registered: %s", on.Addition.Text)
	}
	if r, _ := annotate(app, call); r.Text != "" {
		t.Fatalf("annotate: %+v", r)
	}
	if st, _ := noteStatus(app, "ci/grok"); st.Connected != 1 || st.Via["webhook"] != 1 {
		t.Fatalf("status: %+v", st)
	}

	// A burst is one POST (each starts a billed run), with the key and the
	// first delivery's ID as its Idempotency-Key.
	before := len(f.of(agenthooks.TypeEvents))
	time.Sleep(400 * time.Millisecond) // past the spacing after the last POST
	for _, n := range []string{"burst one", "burst two", "burst three"} {
		emitNote(app, []string{"ci/grok"}, note{Text: n})
	}
	waitFor(t, "the burst", func() bool {
		_, s3 := deliveryOf(t, c, tenant, "burst three")
		return s3 == "acked"
	})
	evs := f.of(agenthooks.TypeEvents)[before:]
	n := 0
	for _, r := range evs {
		n += len(r.Body.Events)
		if r.Auth != "Bearer "+key || r.Body.Bot != grokBotID || r.Body.App != "Ci" || !strings.Contains(r.Body.Notice, "not instructions from the user") ||
			r.Idempotency == "" || !strings.Contains(r.Body.Details, "truncated") {
			t.Fatalf("events POST: %+v", r)
		}
		for _, e := range r.Body.Events {
			if s := string(e); strings.Contains(s, "link=") || strings.Contains(s, "wsc_") {
				t.Fatalf("an event carries a link or session key: %s", s)
			}
		}
	}
	if n != 3 || len(evs) > 2 {
		t.Fatalf("burst: %d events in %d POSTs", n, len(evs))
	}
	id1, _ := deliveryOf(t, c, tenant, "burst one")
	if evs[0].Idempotency != id1 {
		t.Fatalf("idempotency key %s, want %s", evs[0].Idempotency, id1)
	}
	raw, _ := json.Marshal(evs[0].Body)
	for _, not := range []string{"link=", "/v1/stream", "session_key", "wsc_", key} {
		if strings.Contains(string(raw), not) {
			t.Fatalf("the body carries %q", not)
		}
	}

	// No page stops wake-ups: the removal route is gone.
	if code, out, _ := page(t, c, "/v1/stream/webhook-setup/remove", map[string]string{"removal_token": "x"}); code == 200 && out["removed"] != nil {
		t.Fatalf("a removal route answered: %d %v", code, out)
	}
	// The user asks the bot to stop watching: its unsubscribe ends the
	// destination, audited.
	if _, err := unsubscribe(app, call, "ci/grok"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the destination to end", func() bool { return count(t, c, tenant, `SELECT count(*) FROM agent_webhooks`) == 0 })
	if count(t, c, tenant, `SELECT count(*) FROM audit_log WHERE action = 'agent_webhook.unregistered' AND detail->>'reason' = 'unwatched'`) != 1 {
		t.Fatal("the unwatch wasn't audited")
	}
	if code, _ := describe(t, c, token); code != 401 {
		t.Fatalf("describe after the unwatch: %d", code)
	}
	// Asked before: the brief text, and the foreground command.
	after, _ := subscribe(app, call, "ci/grok", subOpts{})
	if !strings.Contains(after.Addition.Text, "isn't set up yet") || !strings.Contains(after.Addition.Text, "If the user said no, don't ask again") || waitCmdRE.FindString(after.Addition.Text) == "" {
		t.Fatalf("after removal: %s", after.Addition.Text)
	}
}
