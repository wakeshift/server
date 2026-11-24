package coretest_test

import (
	"strings"
	"testing"

	"github.com/wakeshift/server/internal/appkey"
	"github.com/wakeshift/server/internal/coretest"
	"github.com/wakeshift/server/pkg/wakeshift"
)

// An app a developer registered in the console, with its key, can use
// topics: its users' workspaces are test workspaces, like the app.
func TestSelfServeAppUsesTopics(t *testing.T) {
	c := coretest.Start(t)
	dev := c.SignIn("dev@example.com")
	var made struct{ Key string }
	if code := dev.Do("POST", "/api/v1/developer/apps", map[string]string{"id": "ci-bot", "name": "CI bot"}, &made); code != 200 || made.Key == "" {
		t.Fatalf("create app: %d", code)
	}
	app := coretest.KeyClient(t, made.Key, c.URL)
	defineNotes(t, app)
	if err := app.PublishManifest(ctx, wakeshift.Manifest{Description: "CI."}); err != nil {
		t.Fatal(err)
	}
	sub, err := subscribe(app, caller{User: "ada", Client: "claude-code"}, "ci/acme/api", subOpts{})
	if err != nil {
		t.Fatalf("subscribe with a console key: %v", err)
	}
	res, err := emitNote(app, []string{"ci/acme/api"}, note{Text: "CI failure on main: tests"})
	if err != nil || res.Matched != 1 {
		t.Fatalf("publish: %+v %v (subscription %s)", res, err, sub.ID)
	}
}

// A current key (wst_ + base58) names no app: wakeshift.Open asks the core
// which app it is, by the key's kid, and the app works as with its older
// key, which is the same key in the older format. A key the core doesn't
// have is refused with a pointer to WAKESHIFT_CORE.
func TestCurrentKeyFormat(t *testing.T) {
	c := coretest.Start(t)
	dev := c.SignIn("dev@example.com")
	var made struct{ Key string }
	if code := dev.Do("POST", "/api/v1/developer/apps", map[string]string{"id": "ci-bot", "name": "CI bot"}, &made); code != 200 || made.Key == "" {
		t.Fatalf("create app: %d", code)
	}
	old, err := appkey.Parse(made.Key)
	if err != nil {
		t.Fatal(err)
	}
	app := coretest.KeyClient(t, appkey.Format(old.Seed), c.URL)
	if id, err := app.AppID(ctx); err != nil || id != "ci-bot" {
		t.Fatalf("AppID: %q, %v", id, err)
	}
	defineNotes(t, app)
	if err := app.PublishManifest(ctx, wakeshift.Manifest{Description: "CI."}); err != nil {
		t.Fatal(err)
	}
	info, err := app.Info(ctx)
	if err != nil || info.Name != "CI bot" || info.Key.Name != "Default" || info.Key.KeyID != appkey.ShortID(old.Public()) {
		t.Fatalf("Info: %+v, %v", info, err)
	}
	if _, err := subscribe(app, caller{User: "ada", Client: "claude-code"}, "ci/acme/api", subOpts{}); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if res, err := emitNote(app, []string{"ci/acme/api"}, note{Text: "CI failure on main: tests"}); err != nil || res.Matched != 1 {
		t.Fatalf("publish: %+v %v", res, err)
	}
	stranger := make([]byte, 32)
	stranger[0] = 1
	if _, err := coretest.KeyClient(t, appkey.Format(stranger), c.URL).Info(ctx); !wakeshift.IsCode(err, "unauthenticated") || !strings.Contains(err.Error(), "WAKESHIFT_CORE") {
		t.Fatalf("a key from elsewhere: %v", err)
	}
}
