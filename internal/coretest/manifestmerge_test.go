package coretest_test

import (
	"slices"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wakeshift/server/internal/coretest"
	"github.com/wakeshift/server/pkg/wakeshift"
)

type ciRepo struct {
	Repo string `json:"repo" jsonschema:"The repository"`
}

type ciRun struct {
	Status string `json:"status" jsonschema:"How it ended"`
}

// Processes sharing one key, each with its own events (found in the launch
// rehearsal): an MCP server, a second server and an emitter. Each server
// merges its events into the manifest; the emitter never sends it. None
// removes another's events, so every emit keeps working. Only an explicit
// publish, which replaces the manifest, removes one.
func TestProcessesSharingAKeyKeepEachOthersEvents(t *testing.T) {
	c := coretest.Start(t)
	a := bareApp(t, c, "ci")
	process := func() *wakeshift.Client {
		return coretest.SDKClientFor(t, coretest.CredsOf(a))
	}
	serve := func(app *wakeshift.Client) {
		app.Attach(mcp.NewServer(&mcp.Implementation{Name: "ci", Version: "1"}, nil), wakeshift.AttachOptions{})
	}
	define := func(app *wakeshift.Client, name, description string) *wakeshift.Event[ciRepo, ciRun] {
		t.Helper()
		ev, err := wakeshift.DefineEvent(app, name, wakeshift.EventDef[ciRepo, ciRun]{
			Description: description,
			Text:        func(r ciRun, a ciRepo) string { return a.Repo + ": " + r.Status },
		})
		if err != nil {
			t.Fatal(err)
		}
		return ev
	}
	emit := func(ev *wakeshift.Event[ciRepo, ciRun]) error {
		t.Helper()
		_, err := ev.Emit(ctx, ciRepo{Repo: "web"}, ciRun{Status: "passed"})
		return err
	}
	stored := func() []string {
		t.Helper()
		app, err := c.Apps.Get(ctx, "ci")
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, e := range app.Manifest.Events {
			out = append(out, e.Name+": "+e.Description)
		}
		return out
	}

	// Process A, the MCP server, defines ci.finished and emits.
	finished := define(a, "ci.finished", "A CI run finished.")
	serve(a)
	if err := emit(finished); err != nil {
		t.Fatalf("A's first emit: %v", err)
	}
	// Process B, another server with the same key, defines something else
	// and makes an app call.
	b := process()
	deployed := define(b, "deploy.done", "A deploy finished.")
	serve(b)
	if err := emit(deployed); err != nil {
		t.Fatalf("B's emit: %v", err)
	}
	// Process C only emits, with an older definition of A's event.
	emitter := process()
	if err := emit(define(emitter, "ci.finished", "Old wording.")); err != nil {
		t.Fatalf("C's emit: %v", err)
	}
	// A's next emit still works: nobody removed ci.finished, and C never
	// sent its older wording.
	if err := emit(finished); err != nil {
		t.Fatalf("A's emit after B's and C's: %v", err)
	}
	if got, want := stored(), []string{"ci.finished: A CI run finished.", "deploy.done: A deploy finished."}; !slices.Equal(got, want) {
		t.Fatalf("stored events %q, want %q", got, want)
	}

	// An explicit publish replaces the manifest: the way to remove an
	// event.
	if err := b.PublishManifest(ctx, wakeshift.Manifest{Description: "CI"}); err != nil {
		t.Fatal(err)
	}
	if got, want := stored(), []string{"deploy.done: A deploy finished."}; !slices.Equal(got, want) {
		t.Fatalf("stored events after a publish %q, want %q", got, want)
	}
	// A still defines it, so its next emit gets unknown_event, merges its
	// manifest and emits again: the event is back, and B's stays.
	if err := emit(finished); err != nil {
		t.Fatalf("A's emit of a removed event it still defines: %v", err)
	}
	if got, want := stored(), []string{"deploy.done: A deploy finished.", "ci.finished: A CI run finished."}; !slices.Equal(got, want) {
		t.Fatalf("stored events after A's emit %q, want %q", got, want)
	}
}

// An app that only emits, on a fresh app with no manifest (an app emitting
// at startup, before any agent has connected to its server): the emit's
// unknown_event sends this process's manifest (a merge, with no settings it
// didn't set) and the emit goes again. The error itself says why.
func TestEmitBeforeAnyManifest(t *testing.T) {
	c := coretest.Start(t)
	a := bareApp(t, c, "boot")
	ev, err := wakeshift.DefineEvent(a, "ci.finished", wakeshift.EventDef[ciRepo, ciRun]{
		Description: "A CI run finished.",
		Text:        func(r ciRun, a ciRepo) string { return a.Repo + ": " + r.Status },
	})
	if err != nil {
		t.Fatal(err)
	}
	// The core's answer before the SDK sends anything.
	code, e := appAPI(t, c, a, "/api/apps/v1/events/emit", map[string]any{"name": "ci.finished", "arguments": map[string]any{"repo": "web"}, "payload": map[string]any{"status": "passed"}, "text": "web: passed"}, nil)
	if code != 404 || e != "unknown_event" {
		t.Fatalf("emit before any manifest: %d %s", code, e)
	}
	if _, err := ev.Emit(ctx, ciRepo{Repo: "web"}, ciRun{Status: "passed"}); err != nil {
		t.Fatalf("the SDK's emit before any manifest: %v", err)
	}
	app, err := c.Apps.Get(ctx, "boot")
	if err != nil {
		t.Fatal(err)
	}
	if len(app.Manifest.Events) != 1 || app.Manifest.Events[0].Name != "ci.finished" || app.Manifest.Description != "" || app.Manifest.Agent != nil {
		t.Fatalf("stored manifest: %+v", app.Manifest)
	}
}
