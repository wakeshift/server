package coretest_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/wakeshift/server/internal/coretest"
	"github.com/wakeshift/server/pkg/wakeshift"
)

// emitText emits a turn at juniper with text to, returning the code and
// the response as it came.
func emitText(t *testing.T, c *coretest.Core, app *wakeshift.Client, text, id string, to any) (int, map[string]any) {
	t.Helper()
	body := map[string]any{"name": "game.turn", "arguments": map[string]any{"table": "juniper"}, "payload": map[string]any{"hole": "As", "turn": "t-As"},
		"text": text, "to": to}
	if id != "" {
		body["id"] = id
	}
	code, raw, err := appPostRaw(c, app, "/api/apps/v1/events/emit", body)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	json.Unmarshal(raw, &out)
	return code, out
}

// nthSubject waits for a workspace's nth delivery and returns its text.
func nthSubject(t *testing.T, c *coretest.Core, tenant string, n int) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		got := subjects(t, c, tenant)
		if len(got) >= n {
			return got[n-1]
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d deliveries, want %d", len(got), n)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// An emit's text over 1000 bytes is trimmed, not refused: cut at a
// character boundary, ending with "…", at most 1000 bytes, delivered so,
// and the response says text_trimmed. Text at the limit is delivered as
// it is, with no text_trimmed. The single-user and the multi-recipient
// emit alike.
func TestEmitTrimsLongText(t *testing.T) {
	c := coretest.Start(t)
	app := pokerApp(t, c, "poker")
	subscribeTurns(t, c, app, "u1", "juniper", nil)
	subscribeTurns(t, c, app, "u2", "juniper", nil)
	u1, u2 := userTenant(t, c, "poker", "u1"), userTenant(t, c, "poker", "u2")

	exact := strings.Repeat("a", 1000)
	code, out := emitText(t, c, app, exact, "", "u1")
	if _, has := out["text_trimmed"]; code != 200 || out["matched"] != 1.0 || has {
		t.Fatalf("exactly 1000 bytes: %d %v", code, out)
	}
	if got := nthSubject(t, c, u1, 1); got != exact {
		t.Fatalf("exactly 1000 bytes delivered as %d bytes", len(got))
	}

	code, out = emitText(t, c, app, strings.Repeat("b", 1001), "", "u1")
	if code != 200 || out["matched"] != 1.0 || out["text_trimmed"] != true {
		t.Fatalf("1001 bytes: %d %v", code, out)
	}
	if got, want := nthSubject(t, c, u1, 2), strings.Repeat("b", 997)+"…"; got != want {
		t.Fatalf("1001 bytes delivered as %q (%d bytes)", got, len(got))
	}

	// An emoji straddling the cut is dropped whole.
	code, out = emitText(t, c, app, strings.Repeat("c", 995)+"😀"+strings.Repeat("c", 50), "", []any{"u1", "u2"})
	if code != 200 || out["matched"] != 2.0 || out["text_trimmed"] != true {
		t.Fatalf("multi-recipient, long: %d %v", code, out)
	}
	want := strings.Repeat("c", 995) + "…"
	if got := nthSubject(t, c, u1, 3); got != want {
		t.Fatalf("multi-recipient, u1 got %q (%d bytes)", got, len(got))
	}
	if got := nthSubject(t, c, u2, 1); got != want {
		t.Fatalf("multi-recipient, u2 got %q (%d bytes)", got, len(got))
	}
	code, out = emitText(t, c, app, "short", "", []any{"u1", "u2"})
	if _, has := out["text_trimmed"]; code != 200 || out["matched"] != 2.0 || has {
		t.Fatalf("multi-recipient, short: %d %v", code, out)
	}

	// The payload's limit still refuses.
	big := map[string]any{"name": "game.turn", "arguments": map[string]any{"table": "juniper"}, "to": "u1", "text": strings.Repeat("d", 2000),
		"payload": map[string]any{"hole": strings.Repeat("x", 70<<10), "turn": "t"}}
	if code, _, err := appPostRaw(c, app, "/api/apps/v1/events/emit", big); err != nil || code != 400 {
		t.Fatalf("a payload over 64 KB: %d %v, want 400", code, err)
	}
}

// An emit's id fingerprints the text as sent, before it's trimmed: a retry
// with the same long text is a duplicate (and says text_trimmed), and the
// id with a text that differs only past the cut is another emit, refused.
func TestEmitIDWithTrimmedText(t *testing.T) {
	c := coretest.Start(t)
	app := pokerApp(t, c, "poker")
	subscribeTurns(t, c, app, "u1", "juniper", nil)
	tenant := userTenant(t, c, "poker", "u1")

	long := strings.Repeat("e", 1200)
	for _, to := range []any{"u1", []any{"u1", "u2"}} {
		id := "single"
		if _, ok := to.([]any); ok {
			id = "multi"
		}
		code, out := emitText(t, c, app, long, id, to)
		if code != 200 || out["duplicate"] != false || out["text_trimmed"] != true {
			t.Fatalf("%s: first: %d %v", id, code, out)
		}
		code, out = emitText(t, c, app, long, id, to)
		if code != 200 || out["duplicate"] != true || out["text_trimmed"] != true {
			t.Fatalf("%s: retry with the same long text: %d %v", id, code, out)
		}
		code, out = emitText(t, c, app, long[:1100]+"f"+long[1101:], id, to)
		if code != 409 || out["error"].(map[string]any)["code"] != "id_reused" {
			t.Fatalf("%s: the id with a text that differs past the cut: %d %v, want 409 id_reused", id, code, out)
		}
	}
	time.Sleep(300 * time.Millisecond)
	if got := subjects(t, c, tenant); len(got) != 2 {
		t.Fatalf("u1 got %d deliveries, want one per first emit", len(got))
	}
}
