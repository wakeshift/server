package coretest_test

import (
	"encoding/json"
	"net/url"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/accounts"
	"github.com/wakeshift/server/internal/appkey"
	"github.com/wakeshift/server/internal/core"
	"github.com/wakeshift/server/internal/coretest"
)

// TestSDKHarness runs a core for the TypeScript SDK's end-to-end test
// (sdk/typescript/scripts/e2e.sh): it registers a self-serve app in the
// console as a developer would, writes the core's URL and the app's test key
// to WAKESHIFT_SDK_HARNESS (a JSON file), and runs until that file is
// removed (or 10 minutes pass: WAKESHIFT_SDK_HARNESS_MINUTES sets another
// limit, for the MCP Events interop programs in bench/mcp-events). Skipped
// without WAKESHIFT_SDK_HARNESS.
//
// The sign-up cap is then at its accounts, and waitlist_cookie is the
// waitlist cookie of a person who signed in past it (with a fake Google),
// for the CLI's test of a waitlisted init (dev sign-in still makes
// accounts).
func TestSDKHarness(t *testing.T) {
	out := os.Getenv("WAKESHIFT_SDK_HARNESS")
	if out == "" {
		t.Skip("set WAKESHIFT_SDK_HARNESS to a file for the core's URL and a test key")
	}
	eventsTestSetup(t) // native subscriptions call back to the test's receiver on 127.0.0.1
	g := coretest.NewGoogle(t)
	c := coretest.StartWith(t, func(o *core.Options) { o.Google = g.Config() })
	dev := c.SignIn("dev@example.com")
	var made struct{ Key string }
	if code := dev.Do("POST", "/api/v1/developer/apps", map[string]string{"id": "ci-bot", "name": "CI bot"}, &made); code != 200 || made.Key == "" {
		t.Fatalf("create app: %d", code)
	}
	// new_key is the console's key, in the current format, which names no
	// app: the SDKs ask the core which app it is. key is the
	// same key in the older test format, which still authenticates.
	k, err := appkey.Parse(made.Key)
	if err != nil {
		t.Fatal(err)
	}
	if err := pgx.BeginFunc(t.Context(), c.DB.Pool, func(tx pgx.Tx) error {
		_, err := accounts.SetSignupCap(t.Context(), tx, 1)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	waitlisted, _ := c.GoogleSignIn(g, "sub-waitlisted", "waitlisted@example.com")
	u, _ := url.Parse(c.URL + "/api/v1/")
	var cookie string
	for _, ck := range waitlisted.Client.Jar.Cookies(u) {
		if ck.Name == "ws_waitlist" {
			cookie = ck.Value
		}
	}
	if cookie == "" {
		t.Fatal("the person past the cap has no waitlist cookie")
	}
	harness := map[string]any{"core": c.URL, "key": appkey.FormatOld(appkey.Test, "ci-bot", k.Seed), "new_key": made.Key, "waitlist_cookie": cookie, "dev_token": coretest.DevToken}
	b, _ := json.Marshal(harness)
	tmp := out + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, out); err != nil { // the script waits for the file: write it whole
		t.Fatal(err)
	}
	t.Logf("harness up: core %s", c.URL)
	limit := 10 * time.Minute
	if m, err := strconv.Atoi(os.Getenv("WAKESHIFT_SDK_HARNESS_MINUTES")); err == nil && m > 0 {
		limit = time.Duration(m) * time.Minute
	}
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(out); os.IsNotExist(err) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	os.Remove(out)
}
