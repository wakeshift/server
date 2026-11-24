package coretest_test

import (
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

// The log lines the Grok metrics count (in the hosted deployment)
// carry their labels and values, and nothing that identifies a user, a bot,
// a webhook or a key.
func TestGrokMetricLines(t *testing.T) {
	var logs syncBuffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	c := startGrok(t, nil)
	f := newFakeCursor(t, c)
	app := noteApp(t, c, "ci")
	bot := &grokBot{c: c, vm: newGrokVM(t), id: grokBotID}
	sub, _, _ := grokSetUp(t, c, f, app, bot, "gina", "ci/grok", "metricsHook1")
	tenant, _ := topicSubRow(t, c, sub.ID)
	emitNote(app, []string{"ci/grok"}, note{Text: "counted"})
	waitFor(t, "the delivery", func() bool { _, s := deliveryOf(t, c, tenant, "counted"); return s == "acked" })
	url, key := f.webhook("metricsHook1", bot)

	want := map[string][]string{
		"grok webhooks: proof":                {"outcome", "app", "proof_ms"},
		"grok webhooks: setup challenge sent": {"outcome", "app"},
		"agent webhooks: delivered":           {"kind", "app", "age_ms"},
	}
	seen := map[string]bool{}
	for _, line := range strings.Split(logs.String(), "\n") {
		var e map[string]any
		if json.Unmarshal([]byte(line), &e) != nil {
			continue
		}
		msg, _ := e["msg"].(string)
		fields, ok := want[msg]
		if !ok {
			continue
		}
		if msg == "agent webhooks: delivered" && e["kind"] != "cursor-automations" {
			continue
		}
		for _, k := range fields {
			if _, has := e[k]; !has {
				t.Fatalf("%q lacks %q: %s", msg, k, line)
			}
		}
		if msg == "grok webhooks: proof" && e["outcome"] == "bound" {
			if ms, _ := e["proof_ms"].(float64); ms <= 0 {
				t.Fatalf("a bind without its time: %s", line)
			}
		}
		for _, secret := range []string{grokBotID, url, key, "metricsHook1", "gina"} {
			if strings.Contains(line, secret) {
				t.Fatalf("%q carries %q: %s", msg, secret, line)
			}
		}
		seen[msg] = true
	}
	for msg := range want {
		if !seen[msg] {
			t.Fatalf("no %q line", msg)
		}
	}
}
