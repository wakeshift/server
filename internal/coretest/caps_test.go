package coretest_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"testing"

	"github.com/wakeshift/server/internal/apps"
	"github.com/wakeshift/server/internal/coretest"
	"github.com/wakeshift/server/pkg/wakeshift"
)

// An app owns at most so many workspaces (headless users and topics'
// conversations alike); one it has already is still returned.
func TestWorkspaceCap(t *testing.T) {
	c := coretest.Start(t)
	was := apps.DefaultWorkspaceCap
	apps.DefaultWorkspaceCap = 2
	t.Cleanup(func() { apps.DefaultWorkspaceCap = was })
	app := headlessApp(t, c, "quiz", false)

	if _, err := app.Workspace(ctx, "~codex:thread", ""); !wakeshift.IsCode(err, "bad_request") {
		t.Fatalf("an external ID starting with ~: %v", err)
	}
	first, err := app.Workspace(ctx, "u1", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.Workspace(ctx, "u2", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := app.Workspace(ctx, "u3", ""); !wakeshift.IsCode(err, "workspace_limit") {
		t.Fatalf("a workspace over the cap: %v", err)
	}
	if again, err := app.Workspace(ctx, "u1", ""); err != nil || again != first {
		t.Fatalf("an existing workspace at the cap: %q %v", again, err)
	}
	// Topics' conversations without a user count too.
	if _, err := subscribe(app, caller{Client: "claude-code"}, "news", subOpts{}); !wakeshift.IsCode(err, "workspace_limit") {
		t.Fatalf("a new conversation over the cap: %v", err)
	}
	// An existing user's conversation still subscribes.
	if _, err := subscribe(app, caller{Client: "claude-code", User: "u1"}, "news", subOpts{}); err != nil {
		t.Fatalf("an existing user at the cap: %v", err)
	}
}

// Every app, test or live, console or ours, has the one default cap of
// 10,000 workspaces: the 10,001st is refused (workspace_limit) and an
// override, ours from WAKESHIFT_APPS, raises it.
func TestOneWorkspaceCap(t *testing.T) {
	c := coretest.Start(t)
	if apps.DefaultWorkspaceCap != 10_000 {
		t.Fatalf("the default cap is %d", apps.DefaultWorkspaceCap)
	}
	var made struct{ Key string }
	if code := c.SignIn("dev@example.com").Do("POST", "/api/v1/developer/apps", map[string]string{"id": "quiz", "name": "Quiz"}, &made); code != 200 {
		t.Fatalf("create app: %d", code)
	}
	test := coretest.KeyClient(t, made.Key, c.URL) // a console app, which used to be capped at 200
	if _, err := c.DB.Pool.Exec(ctx, `INSERT INTO tenants (name, kind, app_id, external_id)
		SELECT 'u', 'app', 'quiz', 'u' || n FROM generate_series(1, 10000) n`); err != nil {
		t.Fatal(err)
	}
	if _, err := test.Workspace(ctx, "u10001", ""); !wakeshift.IsCode(err, "workspace_limit") {
		t.Fatalf("the 10,001st workspace: %v", err)
	}
	if _, err := test.Workspace(ctx, "u42", ""); err != nil {
		t.Fatalf("one it has: %v", err)
	}

	// One of ours (live): the same cap, and WAKESHIFT_APPS' override.
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	ours := apps.Config{ID: "games", Name: "Games", PublicKey: base64.StdEncoding.EncodeToString(pub)}
	if err := c.Apps.Sync(ctx, []apps.Config{ours}); err != nil {
		t.Fatal(err)
	}
	if app, err := c.Apps.Get(ctx, "games"); err != nil || apps.WorkspaceCap(app) != 10_000 {
		t.Fatalf("ours: %d %v", apps.WorkspaceCap(app), err)
	}
	n := 20_000
	ours.WorkspaceCap = &n
	if err := c.Apps.Sync(ctx, []apps.Config{ours}); err != nil {
		t.Fatal(err)
	}
	if app, err := c.Apps.Get(ctx, "games"); err != nil || apps.WorkspaceCap(app) != 20_000 || apps.WorkspacesPerHour(app) != 4_000 {
		t.Fatalf("ours, overridden: %d %v", apps.WorkspaceCap(app), err)
	}
	ours.WorkspaceCap = nil
	if err := c.Apps.Sync(ctx, []apps.Config{ours}); err != nil {
		t.Fatal(err)
	}
	if app, _ := c.Apps.Get(ctx, "games"); app.WorkspaceCapOverride != nil {
		t.Fatal("removing it from WAKESHIFT_APPS didn't reset it")
	}
	bad := -1
	ours.WorkspaceCap = &bad
	if err := c.Apps.Sync(ctx, []apps.Config{ours}); err == nil {
		t.Fatal("a negative cap synced")
	}
}

// An app makes only so many workspaces an hour, for its own users and for
// topics' conversations without a user alike, counted in Postgres so every
// instance sees the same count. Workspaces it already has aren't limited.
func TestWorkspaceRate(t *testing.T) {
	c := coretest.Start(t)
	was := apps.DefaultWorkspacesPerHour
	apps.DefaultWorkspacesPerHour = 2
	t.Cleanup(func() { apps.DefaultWorkspacesPerHour = was })
	app := headlessApp(t, c, "quiz", false)

	first, err := app.Workspace(ctx, "p_a", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.Workspace(ctx, "alice", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := app.Workspace(ctx, "bob", ""); !wakeshift.IsCode(err, "workspace_rate_limited") {
		t.Fatalf("a third user in the hour: %v", err)
	}
	if _, err := subscribe(app, caller{Client: "claude-code"}, "news", subOpts{}); !wakeshift.IsCode(err, "workspace_rate_limited") {
		t.Fatalf("a new conversation over the rate: %v", err)
	}
	if again, err := app.Workspace(ctx, "p_a", ""); err != nil || again != first {
		t.Fatalf("an existing user: %q %v", again, err)
	}
	// An hour on, there's room again.
	if _, err := c.DB.Pool.Exec(ctx, `UPDATE tenants SET created_at = now() - interval '61 minutes' WHERE app_id = 'quiz'`); err != nil {
		t.Fatal(err)
	}
	if _, err := app.Workspace(ctx, "bob", ""); err != nil {
		t.Fatalf("a user an hour later: %v", err)
	}
}
