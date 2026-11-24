package coretest_test

import (
	"strings"
	"testing"

	"github.com/wakeshift/server/internal/appkey"
	"github.com/wakeshift/server/internal/coretest"
	"github.com/wakeshift/server/pkg/wakeshift"
)

// The console issues only current-format keys (wst_ + 50 base58
// characters), for a new app, a new key and a rotation. They name no app:
// wakeshift.Open asks which it is. The same key in the older formats
// (wst_test_, wst_live_) still authenticates, as every older key does.
func TestConsoleIssuesCurrentKeys(t *testing.T) {
	c := coretest.Start(t)
	dev := c.SignIn("dev@example.com")
	var made struct {
		Key     string
		KeyInfo struct {
			ID string `json:"key_id"`
		} `json:"key_info"`
	}
	if code := dev.Do("POST", "/api/v1/developer/apps", map[string]string{"id": "ci-bot", "name": "CI bot"}, &made); code != 200 {
		t.Fatalf("create app: %d", code)
	}
	current := func(key string) appkey.Key {
		t.Helper()
		k, err := appkey.Parse(key)
		if err != nil || len(key) != 54 || strings.Count(key, "_") != 1 || k.AppID != "" || k.Mode != "" {
			t.Fatalf("not a current-format key (%d characters): %v", len(key), err)
		}
		return k
	}
	k := current(made.Key)
	if made.KeyInfo.ID != k.KeyID()[:8] {
		t.Fatalf("key_id %q isn't the start of the kid", made.KeyInfo.ID)
	}
	app := coretest.KeyClient(t, made.Key, c.URL)
	if id, err := app.AppID(ctx); err != nil || id != "ci-bot" {
		t.Fatalf("AppID: %q, %v", id, err)
	}
	defineNotes(t, app)
	if err := app.PublishManifest(ctx, wakeshift.Manifest{Description: "CI."}); err != nil {
		t.Fatal(err)
	}
	if _, err := emitNote(app, []string{"ci/acme/api"}, note{Text: "CI failure on main"}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	// The same key in the older format names the app itself.
	old := coretest.KeyClient(t, appkey.FormatOld(appkey.Test, "ci-bot", k.Seed), c.URL)
	if id, err := old.AppID(ctx); err != nil || id != "ci-bot" {
		t.Fatalf("AppID, a wst_test_ key: %q, %v", id, err)
	}
	if _, err := emitNote(old, []string{"ci/acme/api"}, note{Text: "again"}); err != nil {
		t.Fatalf("the older format: %v", err)
	}
	live := coretest.KeyClient(t, appkey.FormatOld(appkey.Live, "ci-bot", k.Seed), c.URL)
	if id, err := live.AppID(ctx); err != nil || id != "ci-bot" {
		t.Fatalf("AppID, a wst_live_ key: %q, %v", id, err)
	}
	if _, err := emitNote(live, []string{"ci/acme/api"}, note{Text: "and again"}); err != nil {
		t.Fatalf("a wst_live_ key: %v", err)
	}

	var more struct {
		Key     string
		KeyInfo struct {
			ID string `json:"key_id"`
		} `json:"key_info"`
	}
	if code := dev.Do("POST", "/api/v1/developer/apps/ci-bot/keys", map[string]string{"name": "CI"}, &more); code != 200 {
		t.Fatalf("new key: %d", code)
	}
	current(more.Key)
	var rotated struct{ Key string }
	if code := dev.Do("POST", "/api/v1/developer/apps/ci-bot/keys/"+more.KeyInfo.ID+"/rotate", nil, &rotated); code != 200 {
		t.Fatalf("rotate: %d", code)
	}
	current(rotated.Key)
	if id, err := coretest.KeyClient(t, rotated.Key, c.URL).AppID(ctx); err != nil || id != "ci-bot" {
		t.Fatalf("the rotated key: %q, %v", id, err)
	}
	if _, err := coretest.KeyClient(t, more.Key, c.URL).Info(ctx); !wakeshift.IsCode(err, "unauthenticated") {
		t.Fatalf("the key rotated away: %v", err)
	}
}
