package downloads

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/wakeshift/server/internal/harnesses"
)

// release writes a release into a fresh directory, served by its own
// file server (the release's origin), with fake connectors that record how
// they were run. The issuer is a host that doesn't resolve: nothing in the
// release may fetch anything from it.
func release(t *testing.T) (dir string, origin *httptest.Server, names []string) {
	t.Helper()
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("the scripts are for macOS and Linux")
	}
	dir = t.TempDir()
	for _, p := range connectorPlatforms {
		fake := "#!/bin/sh\n# " + p + "\necho \"$@\" > \"$HOME/ran\"\n"
		os.WriteFile(filepath.Join(dir, "wakeshift-"+p), []byte(fake), 0o755)
	}
	os.WriteFile(filepath.Join(dir, OpenClawPlugin), []byte("the openclaw plugin"), 0o644)
	origin = httptest.NewServer(http.FileServer(http.Dir(dir)))
	t.Cleanup(origin.Close)
	s := &Server{PublicURL: "https://wakeshift.invalid", ConnectorDir: dir, ReleaseURL: origin.URL + "/"}
	names, err := s.WriteRelease()
	if err != nil {
		t.Fatal(err)
	}
	return dir, origin, names
}

// fetch is the body at u.
func fetch(t *testing.T, u string) string {
	t.Helper()
	resp, err := http.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("%s: %d", u, resp.StatusCode)
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// A release holds every script, the plugins and the connectors, and
// SHA256SUMS covers each of them. Its scripts fetch only from the release
// and pin the issuer they were built for.
func TestWriteRelease(t *testing.T) {
	dir, origin, names := release(t)
	want := map[string]bool{SumsFile: true, HermesPlugin: true, OpenClawPlugin: true, "openclaw-uninstall.sh": true}
	for _, p := range connectorPlatforms {
		want["wakeshift-"+p] = true
	}
	for _, h := range harnesses.All() {
		if h.Installable {
			want[h.ID+".sh"] = true
		}
	}
	if len(names) != len(want) {
		t.Fatalf("release %v, want %v", names, want)
	}
	for _, n := range names {
		if !want[n] {
			t.Fatalf("unexpected %s in %v", n, names)
		}
	}

	sums, _ := os.ReadFile(filepath.Join(dir, SumsFile))
	lines := strings.Split(strings.TrimSuffix(string(sums), "\n"), "\n")
	if len(lines) != len(want)-1 {
		t.Fatalf("SHA256SUMS has %d lines, want %d:\n%s", len(lines), len(want)-1, sums)
	}
	for _, l := range lines {
		sum, name, ok := strings.Cut(l, "  ")
		b, err := os.ReadFile(filepath.Join(dir, name))
		got := sha256.Sum256(b)
		if !ok || err != nil || hex.EncodeToString(got[:]) != sum {
			t.Fatalf("SHA256SUMS line %q doesn't match %s (%v)", l, name, err)
		}
	}
	// sha256sum -c, as a user verifying by hand runs it.
	if tool, err := exec.LookPath("sha256sum"); err == nil {
		cmd := exec.Command(tool, "-c", SumsFile)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("sha256sum -c: %v %s", err, out)
		}
	}

	for n := range want {
		if !strings.HasSuffix(n, ".sh") {
			continue
		}
		b, _ := os.ReadFile(filepath.Join(dir, n))
		script := string(b)
		if strings.Contains(script, "wakeshift.invalid/download") || strings.Contains(script, "wakeshift.invalid/install") {
			t.Fatalf("%s fetches from the issuer:\n%s", n, script)
		}
		if n != "openclaw-uninstall.sh" && !strings.Contains(script, origin.URL+"/") {
			t.Fatalf("%s doesn't fetch from the release", n)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "openclaw.sh")); !strings.Contains(string(b), "curl -fsSL "+origin.URL+"/openclaw-uninstall.sh | sh") {
		t.Fatalf("openclaw.sh names the uninstaller outside the release:\n%s", b)
	}
}

// A connector script from the release installs the release's connector
// with the issuer it was built for, without ever reaching that issuer; a
// connector swapped at the origin after the release was written is refused
// with ExitVerify, and nothing is installed or run.
func TestReleaseScriptRefusesTampering(t *testing.T) {
	dir, origin, _ := release(t)
	platform := runtime.GOOS + "-" + runtime.GOARCH
	run := func(name string) (string, int, string) {
		home := t.TempDir()
		cmd := exec.Command("sh")
		cmd.Stdin = strings.NewReader(fetch(t, origin.URL+"/"+name))
		cmd.Env = append(os.Environ(), "HOME="+home)
		out, err := cmd.CombinedOutput()
		ran, _ := os.ReadFile(filepath.Join(home, "ran"))
		if _, serr := os.Stat(filepath.Join(home, ".wakeshift", "bin", "wakeshift")); (serr == nil) != (err == nil) {
			t.Fatalf("%s: installed %v, exit %v", name, serr == nil, err)
		}
		return string(out), exitCode(err), strings.TrimSpace(string(ran))
	}
	if out, code, ran := run("codex.sh"); code != 0 || ran != "install --harness codex --issuer https://wakeshift.invalid --yes" {
		t.Fatalf("install: %d %q %s", code, ran, out)
	}

	// Tampered: the same length and name, different bytes.
	b, _ := os.ReadFile(filepath.Join(dir, "wakeshift-"+platform))
	b[len(b)-2] ^= 1
	os.WriteFile(filepath.Join(dir, "wakeshift-"+platform), b, 0o755)
	if out, code, ran := run("codex.sh"); code != ExitVerify || ran != "" || !strings.Contains(out, "didn't match its checksum; nothing was installed") {
		t.Fatalf("tampered connector: %d %q %s", code, ran, out)
	}
	// Another platform's genuine connector, swapped in: refused too.
	other := "linux-amd64"
	if platform == other {
		other = "darwin-arm64"
	}
	genuine, _ := os.ReadFile(filepath.Join(dir, "wakeshift-"+other))
	os.WriteFile(filepath.Join(dir, "wakeshift-"+platform), genuine, 0o755)
	if out, code, ran := run("cursor.sh"); code != ExitVerify || ran != "" {
		t.Fatalf("another platform's connector: %d %q %s", code, ran, out)
	}
}

// Hermes's script from the release exits with HermesExitVerify (7) when the
// plugin at the origin was swapped, as partners' installers read it, and
// writes nothing; the genuine plugin installs.
func TestReleaseHermesRefusesTampering(t *testing.T) {
	dir, origin, _ := release(t)
	script := fetch(t, origin.URL+"/hermes.sh")
	if !strings.Contains(script, origin.URL+"/"+HermesPlugin) {
		t.Fatalf("hermes.sh doesn't fetch the plugin from the release")
	}
	ok := fakeHermes(t, filepath.Join(t.TempDir(), "ran"), 0)
	env := func(hh string) []string {
		return []string{"HOME=" + t.TempDir(), "HERMES_HOME=" + hh, "HERMES_BIN=" + ok}
	}
	hh := t.TempDir()
	if _, errs, code := runScript(t, script, env(hh)); code != HermesExitOK {
		t.Fatalf("install: %d %s", code, errs)
	}
	if cfg, _ := os.ReadFile(filepath.Join(hh, "plugins", "wakeshift", "config.json")); !strings.Contains(string(cfg), `"https://wakeshift.invalid"`) {
		t.Fatalf("issuer not pinned: %s", cfg)
	}

	b, _ := os.ReadFile(filepath.Join(dir, HermesPlugin))
	b[len(b)/2] ^= 0xff
	os.WriteFile(filepath.Join(dir, HermesPlugin), b, 0o644)
	hh = t.TempDir()
	if _, errs, code := runScript(t, script, env(hh)); code != HermesExitVerify || !strings.Contains(errs, "didn't match its checksum") {
		t.Fatalf("tampered plugin: %d %s", code, errs)
	}
	if _, err := os.Stat(filepath.Join(hh, "plugins", "wakeshift")); !os.IsNotExist(err) {
		t.Fatalf("wrote the plugin: %v", err)
	}
}

// A release is complete or isn't written: every platform's connector and
// the OpenClaw plugin, and https URLs (http only on this computer, for tests).
func TestWriteReleaseRefusesIncomplete(t *testing.T) {
	full := func() string {
		d := t.TempDir()
		for _, p := range connectorPlatforms {
			os.WriteFile(filepath.Join(d, "wakeshift-"+p), []byte(p), 0o755)
		}
		os.WriteFile(filepath.Join(d, OpenClawPlugin), []byte("plugin"), 0o644)
		return d
	}
	const rel = "https://github.com/wakeshift/connectors/releases/download/v1.0.0"
	d := full()
	os.Remove(filepath.Join(d, "wakeshift-linux-arm64"))
	if _, err := (&Server{PublicURL: "https://wakeshift.ai", ConnectorDir: d, ReleaseURL: rel}).WriteRelease(); err == nil || !strings.Contains(err.Error(), "linux-arm64") {
		t.Fatalf("missing platform: %v", err)
	}
	d = full()
	os.Remove(filepath.Join(d, OpenClawPlugin))
	if _, err := (&Server{PublicURL: "https://wakeshift.ai", ConnectorDir: d, ReleaseURL: rel}).WriteRelease(); err == nil {
		t.Fatal("missing OpenClaw plugin: no error")
	}
	for _, bad := range [][2]string{
		{"http://wakeshift.ai", rel},
		{"https://wakeshift.ai", "http://github.com/x"},
		{"https://wakeshift.ai", rel + "?x=1"},
		{"https://wakeshift.ai", ""},
		{"", rel},
	} {
		if _, err := (&Server{PublicURL: bad[0], ConnectorDir: full(), ReleaseURL: bad[1]}).WriteRelease(); err == nil {
			t.Fatalf("%v: no error", bad)
		}
	}
	d = full()
	if _, err := (&Server{PublicURL: "https://wakeshift.ai", ConnectorDir: d, ReleaseURL: rel}).WriteRelease(); err != nil {
		t.Fatal(err)
	}
}
