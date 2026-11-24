package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/wakeshift/server/internal/appkey"
	"github.com/wakeshift/server/internal/apps"
)

// -app's lines: a key the SDKs parse back to the seed, and a WAKESHIFT_APPS
// the server reads, registering that key's public half.
func TestWriteEnv(t *testing.T) {
	seed := bytes.Repeat([]byte{7}, 32)
	var out bytes.Buffer
	if err := writeEnv(&out, "example", "", seed); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("lines: %q", lines)
	}
	key, ok := strings.CutPrefix(lines[0], "WAKESHIFT_KEY=")
	if !ok {
		t.Fatalf("first line: %q", lines[0])
	}
	k, err := appkey.Parse(key)
	if err != nil || !bytes.Equal(k.Seed, seed) {
		t.Fatalf("the key doesn't parse back to its seed: %v", err)
	}
	raw, ok := strings.CutPrefix(lines[1], "WAKESHIFT_APPS=")
	if !ok {
		t.Fatalf("second line: %q", lines[1])
	}
	var cfg []apps.Config
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil || len(cfg) != 1 {
		t.Fatalf("WAKESHIFT_APPS: %v %+v", err, cfg)
	}
	if cfg[0].ID != "example" || cfg[0].Name != "example" {
		t.Fatalf("the app: %+v", cfg[0])
	}
	if pub, err := base64.StdEncoding.DecodeString(cfg[0].PublicKey); err != nil || appkey.KeyID(pub) != k.KeyID() {
		t.Fatalf("the public key isn't the key's: %v", err)
	}
	var named bytes.Buffer
	if err := writeEnv(&named, "example", "Example App", seed); err != nil || !strings.Contains(named.String(), `"name":"Example App"`) {
		t.Fatalf("a name: %v %s", err, named.String())
	}
	for _, bad := range []string{"Example", "1app", "a", "has space", strings.Repeat("a", 41)} {
		if writeEnv(&bytes.Buffer{}, bad, "", seed) == nil {
			t.Errorf("app ID %q was accepted", bad)
		}
	}
}
