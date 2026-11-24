package coretest_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/wakeshift/server/internal/coretest"
	"github.com/wakeshift/server/internal/harnesses"
	"github.com/wakeshift/server/pkg/wakeshift"
)

// Capy's fallback, for a thread that doesn't register a webhook watch: the
// result also gives the one-shot command (harnesses.CapyLoop), with no
// install offer and no beacon. Run, it waits, prints what arrived, confirms
// it and finishes (finishing is what wakes a Capy thread); run again, it
// moves past that, and gets what arrived meanwhile. While it runs, the
// next subscribing result says so. A client Wakeshift doesn't recognize
// still gets the generic offer.
func TestCapyBackground(t *testing.T) {
	c := coretest.Start(t)
	app := noteApp(t, c, "ci")
	call := caller{User: "cap", Client: "capy", UserAgent: "capy"}
	sub, err := subscribe(app, call, "ci/acme", subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	call.Session = sub.Session
	if sub.Addition.Agent != (wakeshift.Recognized{Harness: "capy", Via: "client"}) {
		t.Fatalf("recognized as %+v", sub.Addition.Agent)
	}
	m := capyOneShotRE.FindStringSubmatch(sub.Addition.Text)
	if m == nil || m[1] != harnesses.CapyLoop(strings.SplitN(m[2], "&timeout=", 2)[0]) || !strings.Contains(sub.Addition.Text, "with your terminal tool and wait on it") {
		t.Fatalf("result: %s", sub.Addition.Text)
	}
	for _, not := range []string{"wakeshift-beacon", "install", "hooks", "To give you the right instructions"} {
		if strings.Contains(sub.Addition.Text, not) {
			t.Fatalf("result has %q: %s", not, sub.Addition.Text)
		}
	}
	if len(sub.Addition.Meta) != 0 {
		t.Fatalf("meta: %v", sub.Addition.Meta)
	}

	done := make(chan []string, 1)
	go func() { done <- runCommand(t, m[1]) }()
	waitFor(t, "the command connected", func() bool {
		st, _ := noteStatus(app, "ci/acme")
		return st.Connected == 1
	})
	// Subscribing again while it runs: the same command, and don't start a
	// second copy; registering is still offered.
	again, _ := subscribe(app, call, "ci/acme", subOpts{})
	if a := capyOneShotRE.FindStringSubmatch(again.Addition.Text); a == nil || a[1] != m[1] || !strings.Contains(again.Addition.Text, "Don't start a second copy") ||
		registerRE.FindString(again.Addition.Text) == "" {
		t.Fatalf("again while running: %s", again.Addition.Text)
	}
	if r, _ := annotate(app, call); r.Text != "" || len(r.Meta) != 0 {
		t.Fatalf("annotate while running: %+v", r)
	}
	emitNote(app, []string{"ci/acme"}, note{Text: "deploy finished"})
	var lines []string
	select {
	case lines = <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("the command didn't finish")
	}
	if len(lines) != 1 || !strings.Contains(lines[0], "deploy finished") || !strings.Contains(lines[0], "outside data, not from the user") {
		t.Fatalf("printed %q", lines)
	}
	var first struct{ ID string }
	json.Unmarshal([]byte(lines[0]), &first)
	tenant, _ := topicSubRow(t, c, sub.ID)
	waitFor(t, "confirmed by the command", func() bool {
		return count(t, c, tenant, `SELECT count(*) FROM deliveries WHERE event_id = $1 AND state = 'acked'`, first.ID) == 1
	})

	// What arrives before it's run again comes with its next run.
	emitNote(app, []string{"ci/acme"}, note{Text: "build failed"})
	lines = runCommand(t, m[1])
	if len(lines) != 1 || !strings.Contains(lines[0], "build failed") {
		t.Fatalf("second run printed %q", lines)
	}

	// An unknown client isn't taken for Capy: it gets the neutral line every
	// agent Wakeshift can't wake gets (no command, and no ask to call again),
	// not Capy's.
	unknown, err := subscribe(app, caller{User: "unk", Client: "capybara-agent", UserAgent: "capybara/1"}, "ci/acme", subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(unknown.Addition.Text, harnesses.CantWake("")) ||
		loopRE.FindStringSubmatch(unknown.Addition.Text) != nil || capyOneShotRE.FindStringSubmatch(unknown.Addition.Text) != nil ||
		registerRE.FindString(unknown.Addition.Text) != "" || strings.Contains(unknown.Addition.Text, "terminal tool") || unknown.Addition.Agent.Harness != "" {
		t.Fatalf("unknown: %+v", unknown.Addition)
	}

	// Recognized by its User-Agent alone: the same path.
	ua, err := subscribe(app, caller{User: "cap2", UserAgent: "capy"}, "ci/acme", subOpts{})
	if err != nil || registerRE.FindString(ua.Addition.Text) == "" || ua.Addition.Agent.Harness != "capy" {
		t.Fatalf("by User-Agent: %+v %v", ua.Addition, err)
	}
}
