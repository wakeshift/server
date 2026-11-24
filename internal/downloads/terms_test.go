package downloads

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/wakeshift/server/internal/harnesses"
)

// No installer prints a terms notice: an app's own terms carry the
// Connector Terms and the Privacy Policy to its users (Terms of Service
// Section 6.6), and a Hermes install through Hermes's own command shows
// none either. The signed release (WriteRelease) is checked too.
func TestInstallersPrintNoTermsNotice(t *testing.T) {
	dir := t.TempDir()
	platform := runtime.GOOS + "-" + runtime.GOARCH
	os.WriteFile(filepath.Join(dir, "wakeshift-"+platform), []byte("#!/bin/sh\n"), 0o755)
	os.WriteFile(filepath.Join(dir, OpenClawPlugin), []byte("plugin bytes"), 0o644)
	s := &Server{ConnectorDir: dir}
	mux := http.NewServeMux()
	for p, h := range s.Routes() {
		mux.Handle(p, h)
	}
	srv := httptest.NewServer(mux)
	defer srv.Close()
	s.PublicURL = srv.URL
	scripts := []string{"openclaw-uninstall.sh"}
	for _, h := range harnesses.All() {
		if h.Installable && h.ReplacedBy == "" {
			scripts = append(scripts, h.ID+".sh")
		}
	}
	for _, name := range scripts {
		resp, err := http.Get(srv.URL + "/install/" + name)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s: %d", name, resp.StatusCode)
			continue
		}
		noTermsNotice(t, name, string(b))
	}

	t.Run("release", func(t *testing.T) {
		dir, _, names := release(t)
		n := 0
		for _, name := range names {
			if !strings.HasSuffix(name, ".sh") {
				continue
			}
			n++
			b, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil {
				t.Fatal(err)
			}
			noTermsNotice(t, "release "+name, string(b))
		}
		if n < 2 {
			t.Fatalf("release has %d scripts: %v", n, names)
		}
	})
}

// noTermsNotice fails if script carries any part of a terms notice.
func noTermsNotice(t *testing.T, name, script string) {
	t.Helper()
	for _, phrase := range []string{"Connector Terms", "connector-terms", "Privacy Policy", "wakeshift.ai/privacy", "apply to this connector", "you agree"} {
		if strings.Contains(script, phrase) {
			t.Errorf("%s says %q: installers print no terms notice", name, phrase)
		}
	}
}
