package downloads

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/wakeshift/server/internal/version"
	"github.com/wakeshift/server/plugins/hermes"
)

// /install/hermes.sh writes the plugin into $HERMES_HOME/plugins/wakeshift,
// pins this Wakeshift in its config.json and enables it with Hermes's own
// command, found off PATH too; it installs no binary and says nothing about
// hooks. A reinstall keeps the settings; a "wakeshift" plugin that isn't
// Wakeshift's is left alone.
func TestHermesInstallScript(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("the script is for macOS and Linux")
	}
	s := &Server{}
	mux := http.NewServeMux()
	for p, h := range s.Routes() {
		mux.Handle(p, h)
	}
	srv := httptest.NewServer(mux)
	defer srv.Close()
	s.PublicURL = srv.URL
	if got := s.HarnessInstall("hermes").Command; got != "curl -fsSL "+srv.URL+"/install/hermes.sh | sh" {
		t.Fatalf("command %q", got)
	}
	resp, err := http.Get(srv.URL + "/install/hermes.sh")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	script := string(raw)
	for _, bad := range []string{"hook", "/.wakeshift/bin", "download/wakeshift/"} {
		if strings.Contains(script, bad) {
			t.Fatalf("the script mentions %q", bad)
		}
	}

	// A stand-in hermes that records what it was asked, off PATH.
	bin := t.TempDir()
	fake := filepath.Join(bin, "hermes")
	os.WriteFile(fake, []byte("#!/bin/sh\necho \"$@\" >> \"$HOME/ran\"\n"), 0o755)
	run := func(home, hermesHome string, env ...string) (string, error) {
		cmd := exec.Command("sh", "-c", script)
		cmd.Env = append(os.Environ(), append([]string{"HOME=" + home, "HERMES_HOME=" + hermesHome, "PATH=/usr/bin:/bin"}, env...)...)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	home, hh := t.TempDir(), t.TempDir()
	out, err := run(home, hh, "HERMES_BIN="+fake)
	if err != nil {
		t.Fatalf("install: %v %s", err, out)
	}
	dir := filepath.Join(hh, "plugins", "wakeshift")
	init, _ := os.ReadFile(filepath.Join(dir, "__init__.py"))
	want, _ := hermes.Files.ReadFile("wakeshift/__init__.py")
	if string(init) != string(want) || !strings.Contains(string(init), hermes.Marker) {
		t.Fatalf("plugin not written: %s", out)
	}
	if v, _ := os.ReadFile(filepath.Join(dir, hermes.VersionFileName)); string(v) != string(hermes.VersionFile(version.Version)) {
		t.Fatalf("its version: %q", v)
	}
	var cfg struct{ Issuers []string }
	b, _ := os.ReadFile(filepath.Join(dir, "config.json"))
	if json.Unmarshal(b, &cfg) != nil || len(cfg.Issuers) != 1 || cfg.Issuers[0] != srv.URL {
		t.Fatalf("config.json: %s", b)
	}
	// Enabled, and let start turns in Hermes's chats (every Hermes chat needs it), saying so.
	allow := "plugins.entries.wakeshift.allow_gateway_injection"
	if ran, _ := os.ReadFile(filepath.Join(home, "ran")); strings.TrimSpace(string(ran)) != "plugins enable wakeshift\nconfig get "+allow+"\nconfig set "+allow+" true" {
		t.Fatalf("hermes ran %q", ran)
	}
	if !strings.Contains(out, "Let it start turns in your Hermes chats ("+allow+": true)") || !strings.Contains(out, "only in conversations connected to Wakeshift") {
		t.Fatalf("doesn't say it lets it start turns: %s", out)
	}
	if _, err := os.Stat(filepath.Join(dir, ".allowed-gateway-injection")); err != nil {
		t.Fatalf("no record that the install set it: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".wakeshift")); !os.IsNotExist(err) {
		t.Fatalf("installed something in ~/.wakeshift: %v", err)
	}
	for _, w := range []string{"Enabled it in Hermes", "hermes chat --continue", "A new chat works too"} {
		if !strings.Contains(out, w) {
			t.Fatalf("missing %q in %s", w, out)
		}
	}

	// Again, with a setting of the user's and another Wakeshift trusted:
	// both kept, this one added once.
	os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"issuers": ["https://other.test"], "mine": 1}`), 0o644)
	if out, err := run(home, hh, "HERMES_BIN="+fake); err != nil || !strings.Contains(out, "added "+srv.URL) {
		t.Fatalf("reinstall: %v %s", err, out)
	}
	b, _ = os.ReadFile(filepath.Join(dir, "config.json"))
	var kept map[string]any
	if json.Unmarshal(b, &kept) != nil || kept["mine"] != 1.0 || len(kept["issuers"].([]any)) != 2 {
		t.Fatalf("kept: %s", b)
	}
	if out, err := run(home, hh, "HERMES_BIN="+fake); err != nil || !strings.Contains(out, "as they were") {
		t.Fatalf("third: %v %s", err, out)
	}

	// Only Hermes's Python (HERMES_PYTHON, as its gateway sets it): hermes_cli through it.
	py := filepath.Join(bin, "python")
	os.WriteFile(py, []byte("#!/bin/sh\ncase \"$1\" in -c) exit 0 ;; -m) shift 2; echo \"py $*\" >> \"$HOME/ran\"; exit 0 ;; esac\nexit 1\n"), 0o755)
	pyHome := t.TempDir()
	if out, err := run(pyHome, t.TempDir(), "HERMES_PYTHON="+py); err != nil || !strings.Contains(out, "Enabled it in Hermes") {
		t.Fatalf("HERMES_PYTHON: %v %s", err, out)
	}
	if ran, _ := os.ReadFile(filepath.Join(pyHome, "ran")); strings.TrimSpace(string(ran)) != "py plugins enable wakeshift\npy config get "+allow+"\npy config set "+allow+" true" {
		t.Fatalf("HERMES_PYTHON ran %q", ran)
	}

	// No hermes anywhere it looks: installed, with the command to enable it.
	other := t.TempDir()
	if out, err := run(t.TempDir(), other); err == nil || !strings.Contains(out, "run 'hermes plugins enable wakeshift' where Hermes runs") || strings.Contains(out, "A new chat works too") {
		t.Fatalf("no hermes: %v %s", err, out)
	}
	// A hermes whose enable fails: the same, no success line.
	failing := filepath.Join(t.TempDir(), "hermes")
	os.WriteFile(failing, []byte("#!/bin/sh\nexit 3\n"), 0o755)
	if out, err := run(t.TempDir(), t.TempDir(), "HERMES_BIN="+failing); err == nil || !strings.Contains(out, "not enabled") || strings.Contains(out, "Enabled it") || strings.Contains(out, "A new chat works too") {
		t.Fatalf("enable fails: %v %s", err, out)
	}
	// A setting the user turned off stays off, and the install says how to turn it on.
	off := filepath.Join(t.TempDir(), "hermes")
	os.WriteFile(off, []byte("#!/bin/sh\necho \"$@\" >> \"$HOME/ran\"\nif [ \"$2\" = get ]; then echo false; fi\n"), 0o755)
	offHome, offHH := t.TempDir(), t.TempDir()
	if out, err := run(offHome, offHH, "HERMES_BIN="+off); err != nil || !strings.Contains(out, "Left "+allow+" off") || !strings.Contains(out, "hermes config set "+allow+" true") {
		t.Fatalf("turned off: %v %s", err, out)
	}
	if _, err := os.Stat(filepath.Join(offHH, "plugins", "wakeshift", ".allowed-gateway-injection")); !os.IsNotExist(err) {
		t.Fatalf("marked a setting it didn't set: %v", err)
	}
	if ran, _ := os.ReadFile(filepath.Join(offHome, "ran")); strings.Contains(string(ran), "config set") {
		t.Fatalf("changed a setting the user turned off: %q", ran)
	}
	if strings.Contains(script, "/proc") {
		t.Fatal("the script reads other processes")
	}

	// Someone else's "wakeshift" plugin: left alone.
	foreign := t.TempDir()
	os.MkdirAll(filepath.Join(foreign, "plugins", "wakeshift"), 0o755)
	os.WriteFile(filepath.Join(foreign, "plugins", "wakeshift", "__init__.py"), []byte("# not ours\n"), 0o644)
	if out, err := run(t.TempDir(), foreign, "HERMES_BIN="+fake); err == nil || !strings.Contains(out, "isn't Wakeshift's plugin") {
		t.Fatalf("foreign: %v %s", err, out)
	}
	if b, _ := os.ReadFile(filepath.Join(foreign, "plugins", "wakeshift", "__init__.py")); string(b) != "# not ours\n" {
		t.Fatal("overwrote another plugin")
	}

	// The download is what the script checks.
	resp, _ = http.Get(srv.URL + "/download/hermes/" + HermesPlugin)
	resp.Body.Close()
	if _, sum := s.hermesPack(); resp.StatusCode != 200 || !strings.Contains(script, sum) {
		t.Fatalf("download: %d", resp.StatusCode)
	}
}
