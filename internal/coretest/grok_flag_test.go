package coretest_test

import (
	"strings"
	"testing"

	"github.com/wakeshift/server/internal/agenthooks"
	"github.com/wakeshift/server/internal/core"
	"github.com/wakeshift/server/internal/coretest"
	"github.com/wakeshift/server/internal/streams"
)

// Turning WAKESHIFT_GROK_WEBHOOKS off with a bot's webhook bound strands
// nothing: the revision with the flag off skips the webhook, the bot's
// subscribing result gives the foreground command again, and that wait
// delivers the messages instead of ending for the webhook. Turned back
// on, the webhook delivers again, and every message arrived once.
func TestGrokFlagOffFallsBack(t *testing.T) {
	on := startGrok(t, nil)
	f := newFakeCursor(t, on)
	bot := &grokBot{c: on, vm: newGrokVM(t), id: grokBotID}
	app := noteApp(t, on, "ci")
	sub, call, _ := grokSetUp(t, on, f, app, bot, "gina", "ci/grok", "flagHook1")
	tenant, _ := topicSubRow(t, on, sub.ID)
	emitNote(app, []string{"ci/grok"}, note{Text: "while on"})
	waitFor(t, "the webhook's delivery", func() bool { _, s := deliveryOf(t, on, tenant, "while on"); return s == "acked" })

	// The flag goes off: a new revision replaces the old.
	on.Stop()
	off := coretest.StartWith(t, func(o *core.Options) { o.DB = on.DB; o.GrokWebhooks = false })
	app = noteApp(t, off, "ci")
	posts := len(f.of(agenthooks.TypeEvents))
	again, err := subscribe(app, call, "ci/grok", subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	m := waitCmdRE.FindStringSubmatch(again.Addition.Text)
	if m == nil || strings.Contains(again.Addition.Text, "webhook-setup") {
		t.Fatalf("flag off, the subscribing result: %s", again.Addition.Text)
	}
	emitNote(app, []string{"ci/grok"}, note{Text: "while off"})
	waitFor(t, "the message to be made", func() bool { id, _ := deliveryOf(t, off, tenant, "while off"); return id != "" })
	out := strings.Join(runCommand(t, "curl -sN '"+m[1]+"&timeout=5s'"), "\n")
	if !strings.Contains(out, "while off") || strings.Contains(out, streams.WebhookNote) {
		t.Fatalf("the foreground wait with the flag off: %s", out)
	}
	if _, s := deliveryOf(t, off, tenant, "while off"); s == "pending" {
		t.Fatalf("the message: %s", s) // handed to the wait (confirmed by its next run)
	}
	if len(f.of(agenthooks.TypeEvents)) != posts {
		t.Fatal("the webhook was POSTed with the flag off")
	}

	// Back on: the webhook delivers again; nothing was lost or sent twice.
	off.Stop()
	back := startGrok(t, func(o *core.Options) { o.DB = on.DB })
	app = noteApp(t, back, "ci")
	emitNote(app, []string{"ci/grok"}, note{Text: "back on"})
	waitFor(t, "the webhook again", func() bool { _, s := deliveryOf(t, back, tenant, "back on"); return s == "acked" })
	seen := map[string]int{}
	for _, r := range f.of(agenthooks.TypeEvents) {
		for _, e := range r.Body.Events {
			for _, text := range []string{"while on", "while off", "back on"} {
				if strings.Contains(string(e), text) {
					seen[text]++
				}
			}
		}
	}
	if seen["while on"] != 1 || seen["back on"] != 1 || seen["while off"] != 0 {
		t.Fatalf("webhook deliveries: %v", seen)
	}
}
