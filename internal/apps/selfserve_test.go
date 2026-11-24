package apps_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"strings"
	"testing"

	"github.com/wakeshift/server/internal/appkey"
	"github.com/wakeshift/server/internal/apps"
	"github.com/wakeshift/server/pkg/wakeshift"
)

// The older key format (wst_<mode>_<app id>_<seed>), which the console no
// longer issues: keys made in it still parse, on the server and in the SDK.
func TestAppKeyFormat(t *testing.T) {
	// A seed whose base64url has underscores and dashes in it.
	seed := bytes.Repeat([]byte{0xff, 0xfe, 0xfb}, 11)[:32]
	key := appkey.FormatOld(appkey.Test, "my-app", seed)
	if !strings.HasPrefix(key, "wst_test_my-app_") || len(key) != len("wst_test_my-app_")+43 || !strings.ContainsAny(key[16:], "_-") {
		t.Fatalf("key %q", key)
	}
	mode, id, priv, err := apps.ParseKey(key)
	if err != nil || mode != apps.Test || id != "my-app" || !bytes.Equal(priv.Seed(), seed) {
		t.Fatalf("server parse: %q %q %v", mode, id, err)
	}
	app, err := wakeshift.New(wakeshift.WithKey(key), wakeshift.WithCore("https://core.test"))
	if err != nil || app.KeyID() != appkey.KeyID(priv.Public().(ed25519.PublicKey)) {
		t.Fatalf("wakeshift.New: %v %v", app, err)
	}
	if id, err := app.AppID(context.Background()); err != nil || id != "my-app" {
		t.Fatalf("AppID: %q %v", id, err)
	}

	for name, bad := range map[string]string{
		"no prefix":      key[4:],
		"unknown mode":   strings.Replace(key, "_test_", "_prod_", 1),
		"no app ID":      "wst_test__" + key[16:],
		"cut short":      key[:len(key)-1],
		"one extra char": key + "A",
		"not base64":     key[:len(key)-1] + "!",
		"uppercase ID":   strings.Replace(key, "my-app", "My-app", 1),
	} {
		if _, _, _, err := apps.ParseKey(bad); err == nil {
			t.Errorf("server: %s: accepted", name)
		}
		if _, err := wakeshift.New(wakeshift.WithKey(bad), wakeshift.WithCore("https://core.test")); !wakeshift.IsCode(err, wakeshift.CodeConfig) {
			t.Errorf("SDK: %s: accepted", name)
		}
	}
}
