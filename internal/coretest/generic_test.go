package coretest_test

import (
	"strings"
	"testing"

	"github.com/wakeshift/server/internal/coretest"
)

// A program Wakeshift doesn't recognize can't be woken: subscribing says so
// in one neutral line (and gives the session key), with no command to run
// and nothing that asks for another call; other results add nothing; what
// arrives waits for the session, and no result hands it over. The app's own
// tools are how the agent checks, as without Wakeshift. A known harness,
// and one recognized by a name others send too (Hermes Agent's "mcp"), get
// their own instructions.
func TestUnrecognizedHarnessCantBeWoken(t *testing.T) {
	c := coretest.Start(t)
	app := noteApp(t, c, "ci")
	const line = "Wakeshift can't wake this agent, so nothing will notify you here when something arrives."

	call := caller{Client: "my-homegrown-agent"}
	sub, err := subscribe(app, call, "ci/acme", subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(sub.Addition.Text, line) || loopRE.MatchString(sub.Addition.Text) || strings.Contains(sub.Addition.Text, "again") ||
		strings.Contains(sub.Addition.Text, "curl") || sub.Addition.Meta != nil {
		t.Fatalf("result: %s", sub.Addition.Text)
	}
	call.Session = sub.Session
	if r, _ := annotate(app, call); r.Text != "" || r.Meta != nil {
		t.Fatalf("annotate: %+v", r)
	}
	emitNote(app, []string{"ci/acme"}, note{Text: "build failed"})
	heldNotShown(t, c, app, call, sub.ID, "build failed")
	if again, _ := subscribe(app, call, "ci/acme", subOpts{}); !strings.HasPrefix(again.Addition.Text, line) || strings.Contains(again.Addition.Text, "build failed") {
		t.Fatalf("again: %s", again.Addition.Text)
	}

	// Known and unsure harnesses get their own instructions.
	for _, client := range []string{"claude-code", "mcp"} {
		k, err := subscribe(app, caller{Client: client}, "ci/acme", subOpts{})
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(k.Addition.Text, line) {
			t.Errorf("%s got the unrecognized line: %s", client, k.Addition.Text)
		}
	}
}
