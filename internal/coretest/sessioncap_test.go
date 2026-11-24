package coretest_test

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/wakeshift/server/apps/agentkit"
	"github.com/wakeshift/server/internal/appapi"
	"github.com/wakeshift/server/internal/coretest"
	"github.com/wakeshift/server/pkg/wakeshift"
)

// An app's stream-links requests start at most MaxNewSessionsPerHour new
// sessions for one agent an hour: past it, a new label (or none) is
// refused with 429 session_rate_limited and makes nothing, while resuming
// a session, reusing a label, and another agent's sessions still work.
func TestStreamLinksCapNewSessions(t *testing.T) {
	old := appapi.MaxNewSessionsPerHour
	appapi.MaxNewSessionsPerHour = 3
	t.Cleanup(func() { appapi.MaxNewSessionsPerHour = old })
	c := coretest.Start(t)
	app := headlessApp(t, c, "quiz", false)
	ws, err := app.Workspace(ctx, "guest-1", "")
	if err != nil {
		t.Fatal(err)
	}
	actAs := func(agent string) *wakeshift.Agent {
		t.Helper()
		if _, err := app.AddAgent(ctx, ws, agent, ""); err != nil {
			t.Fatal(err)
		}
		ag, err := app.ActAs(ctx, ws, agent)
		if err != nil {
			t.Fatal(err)
		}
		return ag
	}
	ag := actAs("claude-code")
	var first agentkit.Link
	for i := range 3 {
		l, err := agentkit.SessionLink(ctx, ag, agentkit.LinkRequest{Label: fmt.Sprint("l", i)})
		if err != nil {
			t.Fatalf("new session %d: %v", i, err)
		}
		if i == 0 {
			first = l
		}
	}
	for _, r := range []agentkit.LinkRequest{{Label: "l3"}, {}} {
		if _, err := agentkit.SessionLink(ctx, ag, r); !wakeshift.IsCode(err, "session_rate_limited") {
			t.Fatalf("a new session past the cap (%+v): %v", r, err)
		}
	}
	// What exists still gets links.
	if l, err := agentkit.SessionLink(ctx, ag, agentkit.LinkRequest{Session: first.Session}); err != nil || l.Session != first.Session {
		t.Fatalf("resuming at the cap: %+v %v", l, err)
	}
	if l, err := agentkit.SessionLink(ctx, ag, agentkit.LinkRequest{Label: "l1"}); err != nil || l.Session != "claude-code@l1" {
		t.Fatalf("an existing label at the cap: %+v %v", l, err)
	}
	// The refused ones made nothing: the agent has its three sessions.
	if list, err := agentkit.Sessions(ctx, ag); err != nil || len(list) != 3 {
		t.Fatalf("sessions: %d %v", len(list), err)
	}
	// Another agent has its own count.
	codex := actAs("codex")
	if _, err := agentkit.SessionLink(ctx, codex, agentkit.LinkRequest{Label: "x"}); err != nil {
		t.Fatalf("another agent: %v", err)
	}

	// Concurrent requests for new sessions don't race past the cap: of
	// many at once, exactly the cap's remainder (2) get one.
	var wg sync.WaitGroup
	var made atomic.Int32
	for i := range 12 {
		wg.Go(func() {
			_, err := agentkit.SessionLink(ctx, codex, agentkit.LinkRequest{Label: fmt.Sprint("c", i)})
			switch {
			case err == nil:
				made.Add(1)
			case !wakeshift.IsCode(err, "session_rate_limited"):
				t.Errorf("concurrent new session %d: %v", i, err)
			}
		})
	}
	wg.Wait()
	if made.Load() != 2 {
		t.Fatalf("%d concurrent new sessions made, want 2", made.Load())
	}
	if list, err := agentkit.Sessions(ctx, codex); err != nil || len(list) != 3 {
		t.Fatalf("codex's sessions: %d %v", len(list), err)
	}
}
