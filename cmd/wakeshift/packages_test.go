package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wakeshift/server/internal/config"
	"github.com/wakeshift/server/internal/harnesses"
	"github.com/wakeshift/server/internal/npmtag"
)

// Each package takes its own setting: @wakeshift/connect's sets Cursor's
// offer, opencode's plugin's opencode's.
func TestOfferPackages(t *testing.T) {
	t.Cleanup(func() { harnesses.SetPackageVersion("cursor", ""); harnesses.SetPackageVersion("opencode", "") })
	const local = "http://localhost:8370"
	offerPackages(context.Background(), config.Config{PublicURL: local, ConnectPackage: "1.5.0", OpencodePlugin: "off"})
	cursor, _ := harnesses.Get("cursor")
	opencode, _ := harnesses.Get("opencode")
	if c := harnesses.OfferCommand(cursor, local); c != "npx -y @wakeshift/connect@1.5.0 cursor" {
		t.Errorf("cursor: %q", c)
	}
	if c := harnesses.OfferCommand(opencode, local); !strings.HasPrefix(c, "curl -fsSL ") {
		t.Errorf("opencode: %q", c)
	}
}

func TestPackageSource(t *testing.T) {
	const prod, staging = "https://app.wakeshift.ai", "https://app.staging.wakeshift.ai"
	for _, c := range []struct{ setting, url, version, tag string }{
		{"", prod, "", "latest"}, // production: its released packages
		{"", staging, "", ""},    // another Wakeshift: the installer, unless it says
		{"off", prod, "", ""},
		{"1.5.0", staging, "1.5.0", ""},
		{"staging", staging, "", "staging"},
	} {
		v, tag := packageSource(c.setting, c.url)
		if v != c.version || tag != c.tag {
			t.Errorf("%q at %s: version %q, tag %q; want %q, %q", c.setting, c.url, v, tag, c.version, c.tag)
		}
	}
}

// On "latest", a prerelease (the placeholder that set up trusted publishing)
// is never offered; another dist-tag may point at one.
func TestOfferable(t *testing.T) {
	for _, c := range []struct {
		tag, v string
		ok     bool
	}{
		{"latest", "1.5.0", true},
		{"latest", "0.0.0-placeholder", false},
		{"latest", "1.6.0-rc.1", false},
		{"staging", "1.6.0-staging.2", true},
		{"staging", "1.6; id", false},
	} {
		if got := offerable(c.tag)(c.v); got != c.ok {
			t.Errorf("%s -> %s: %v, want %v", c.tag, c.v, got, c.ok)
		}
	}
}

// @wakeshift/pi on npm with only its placeholder on "latest" (npm put it
// there when the placeholder was published, November 8, 2026): production
// keeps offering Wakeshift's installer; the first release on "latest" moves
// the offer to pi's own command.
func TestPiPlaceholderNotOffered(t *testing.T) {
	t.Cleanup(func() { harnesses.SetPackageVersion("pi", "") })
	var mu sync.Mutex
	latest := "0.0.0-placeholder"
	reg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.EscapedPath() != "/-/package/@wakeshift%2Fpi/dist-tags" {
			http.NotFound(w, r)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		json.NewEncoder(w).Encode(map[string]string{"latest": latest, "placeholder": "0.0.0-placeholder"})
	}))
	defer reg.Close()
	pi, _ := harnesses.Get("pi")
	const prod = harnesses.ProductionIssuer
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	version, tag := packageSource("", prod)
	if version != "" || tag != "latest" {
		t.Fatalf("production's source: %q %q", version, tag)
	}
	set := func(v string) { harnesses.SetPackageVersion("pi", v) }
	go npmtag.Watch(ctx, reg.Client(), reg.URL, "@wakeshift/pi", tag, 20*time.Millisecond, offerable(tag), set)
	time.Sleep(200 * time.Millisecond)
	if c := harnesses.OfferCommand(pi, prod); c != "curl -fsSL "+prod+"/install/pi.sh | sh" {
		t.Fatalf("with the placeholder on latest: %q", c)
	}
	mu.Lock()
	latest = "1.5.0"
	mu.Unlock()
	deadline := time.Now().Add(3 * time.Second)
	for harnesses.OfferCommand(pi, prod) != "pi install npm:@wakeshift/pi@1.5.0" {
		if time.Now().After(deadline) {
			t.Fatalf("after a release: %q", harnesses.OfferCommand(pi, prod))
		}
		time.Sleep(20 * time.Millisecond)
	}
}
