package downloads

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// /install/<harness>.sh fetches this platform's connector into
// ~/.wakeshift/bin/wakeshift, checks its SHA-256, and runs its install for
// that harness with this Wakeshift as the issuer; a binary that doesn't
// match is refused, and names that aren't harnesses with an install aren't
// served.
func TestHarnessInstallScript(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("the script is for macOS and Linux")
	}
	dir := t.TempDir()
	platform := runtime.GOOS + "-" + runtime.GOARCH
	fake := "#!/bin/sh\necho \"$@\" > \"$HOME/ran\"\n"
	os.WriteFile(filepath.Join(dir, "wakeshift-"+platform), []byte(fake), 0o755)
	s := &Server{ConnectorDir: dir}
	mux := http.NewServeMux()
	for p, h := range s.Routes() {
		mux.Handle(p, h)
	}
	srv := httptest.NewServer(mux)
	defer srv.Close()
	s.PublicURL = srv.URL
	if got := s.HarnessInstall("cursor").Command; got != "curl -fsSL "+srv.URL+"/install/cursor.sh | sh" {
		t.Fatalf("command %q", got)
	}

	run := func(script string) (string, error) {
		home := t.TempDir()
		resp, err := http.Get(srv.URL + "/install/" + script)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		cmd := exec.Command("sh")
		cmd.Stdin = resp.Body
		cmd.Env = append(os.Environ(), "HOME="+home)
		out, err := cmd.CombinedOutput()
		ran, _ := os.ReadFile(filepath.Join(home, "ran"))
		if err == nil {
			if st, serr := os.Stat(filepath.Join(home, ".wakeshift", "bin", "wakeshift")); serr != nil || st.Mode().Perm() != 0o755 {
				t.Fatalf("installed binary: %v %v", st, serr)
			}
		}
		return string(out) + "|" + strings.TrimSpace(string(ran)), err
	}
	for _, h := range []string{"codex", "claude-code", "opencode"} {
		if out, err := run(h + ".sh"); err != nil || !strings.HasSuffix(out, "|install --harness "+h+" --issuer "+srv.URL+" --yes") {
			t.Fatalf("%s: %v %s", h, err, out)
		}
	}
	// Swapped after the sums were taken: refused, nothing run.
	os.WriteFile(filepath.Join(dir, "wakeshift-"+platform), []byte(fake+"# changed\n"), 0o755)
	if out, err := run("codex.sh"); exitCode(err) != ExitVerify || !strings.Contains(out, "didn't match its checksum") || !strings.HasSuffix(out, "|") {
		t.Fatalf("a binary that doesn't match: %v %s", err, out)
	}
	if resp, _ := http.Get(srv.URL + "/download/wakeshift/plan9-mips"); resp.StatusCode != 404 {
		t.Fatalf("an unbuilt platform: %d", resp.StatusCode)
	}
	for _, bad := range []string{"x.sh", "notepad.sh", "goose.sh", "gemini.sh", "Cursor.sh", "cursor", "a;b.sh", "..%2F.sh"} {
		if r, _ := http.Get(srv.URL + "/install/" + bad); r.StatusCode != 404 {
			t.Fatalf("%s: %d", bad, r.StatusCode)
		}
	}
	// An earlier desktop app's installer and app are gone.
	for _, gone := range []string{"/install/mac.sh", "/download/wakeshift-mac.zip"} {
		if r, _ := http.Get(srv.URL + gone); r.StatusCode != 404 {
			t.Fatalf("%s: %d", gone, r.StatusCode)
		}
	}
}
