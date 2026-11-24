package downloads

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/wakeshift/server/internal/version"
	"github.com/wakeshift/server/plugins/hermes"
)

// hermesScriptFor serves /install/hermes.sh from a test server and returns
// the script and the server's URL.
func hermesScriptFor(t *testing.T) (string, string) {
	t.Helper()
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("the script is for macOS and Linux")
	}
	s := &Server{}
	mux := http.NewServeMux()
	for p, h := range s.Routes() {
		mux.Handle(p, h)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	s.PublicURL = srv.URL
	resp, err := http.Get(srv.URL + "/install/hermes.sh")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return string(raw), srv.URL
}

// runScript runs script with sh and args, in env (added to a minimal one),
// and returns its stdout, stderr and exit code.
func runScript(t *testing.T, script string, env []string, args ...string) (string, string, int) {
	t.Helper()
	cmd := exec.Command("sh", append([]string{"-c", script, "hermes.sh"}, args...)...)
	cmd.Env = append([]string{"PATH=/usr/bin:/bin"}, env...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	code := 0
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return out.String(), errb.String(), code
}

// fakeHermes is a stand-in hermes that says something and records its
// arguments in log; exit is what it exits with.
func fakeHermes(t *testing.T, log string, exit int) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "hermes")
	os.WriteFile(p, []byte("#!/bin/sh\necho \"$@\" >> \""+log+"\"\necho 'hermes says hi'\nexit "+string(rune('0'+exit))+"\n"), 0o755)
	return p
}

// A third-party installer can tell what happened from the exit code alone,
// and nothing is written on the failures that say "nothing changed".
func TestHermesInstallExitCodes(t *testing.T) {
	script, _ := hermesScriptFor(t)
	logf := filepath.Join(t.TempDir(), "ran")
	ok := fakeHermes(t, logf, 0)
	env := func(hh string, more ...string) []string {
		return append([]string{"HOME=" + t.TempDir(), "HERMES_HOME=" + hh, "HERMES_BIN=" + ok}, more...)
	}
	untouched := func(hh string) {
		t.Helper()
		if _, err := os.Stat(filepath.Join(hh, "plugins", "wakeshift", "__init__.py")); !os.IsNotExist(err) {
			t.Fatalf("wrote the plugin: %v", err)
		}
	}

	if _, _, code := runScript(t, script, env(t.TempDir())); code != HermesExitOK {
		t.Fatalf("install: %d", code)
	}

	hh := t.TempDir()
	if _, errs, code := runScript(t, script, env(hh), "--loud"); code != HermesExitUsage || !strings.Contains(errs, "Unknown option --loud") {
		t.Fatalf("unknown option: %d %s", code, errs)
	}
	untouched(hh)

	hh = t.TempDir()
	broken := strings.Replace(script, "/download/hermes/", "/download/nope/", 1)
	if _, errs, code := runScript(t, broken, env(hh)); code != HermesExitDownload || !strings.Contains(errs, "Couldn't download") {
		t.Fatalf("download fails: %d %s", code, errs)
	}
	untouched(hh)

	_, sum := (&Server{}).hermesPack()
	hh = t.TempDir()
	if _, errs, code := runScript(t, strings.ReplaceAll(script, sum, strings.Repeat("0", 64)), env(hh)); code != HermesExitVerify || !strings.Contains(errs, "checksum") {
		t.Fatalf("checksum: %d %s", code, errs)
	}
	untouched(hh)

	// A download cut short: the shell's syntax error, nothing run.
	hh = t.TempDir()
	if _, _, code := runScript(t, script[:len(script)*2/3], env(hh)); code != 2 {
		t.Fatalf("cut short: %d", code)
	}
	untouched(hh)

	// No tar: unsupported, before anything is fetched.
	tools := t.TempDir()
	for _, c := range []string{"sh", "curl", "mktemp", "cat", "grep", "dirname", "id", "cut", "rm"} {
		if p, err := exec.LookPath(c); err == nil {
			os.Symlink(p, filepath.Join(tools, c))
		}
	}
	hh = t.TempDir()
	if _, errs, code := runScript(t, script, append(env(hh), "PATH="+tools)); code != HermesExitUnsupported || !strings.Contains(errs, "needs tar") {
		t.Fatalf("no tar: %d %s", code, errs)
	}
	untouched(hh)

	// A Hermes home it can't write.
	hh = t.TempDir()
	os.MkdirAll(filepath.Join(hh, "plugins"), 0o555)
	t.Cleanup(func() { os.Chmod(filepath.Join(hh, "plugins"), 0o755) })
	if os.Getuid() != 0 {
		if _, errs, code := runScript(t, script, env(hh)); code != HermesExitPermission || !strings.Contains(errs, "can't write") {
			t.Fatalf("not writable: %d %s", code, errs)
		}
		untouched(hh)
	}

	// Hermes's enable fails, or there's no hermes: written, not enabled.
	hh = t.TempDir()
	failing := fakeHermes(t, logf, 3)
	if _, errs, code := runScript(t, script, env(hh, "HERMES_BIN="+failing)); code != HermesExitNotEnabled || !strings.Contains(errs, "not enabled") {
		t.Fatalf("enable fails: %d %s", code, errs)
	}
	if _, err := os.Stat(filepath.Join(hh, "plugins", "wakeshift", "__init__.py")); err != nil {
		t.Fatal("not written")
	}
	if _, _, code := runScript(t, script, []string{"HOME=" + t.TempDir(), "HERMES_HOME=" + t.TempDir()}); code != HermesExitNotEnabled {
		t.Fatalf("no hermes: %d", code)
	}

	// A config.json it can't add to: refused, the plugin's files as they were.
	hh = t.TempDir()
	dir := filepath.Join(hh, "plugins", "wakeshift")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "__init__.py"), []byte("# "+hermes.Marker+", an old one\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"issuers": "not a list"}`), 0o644)
	if _, errs, code := runScript(t, script, env(hh)); code != HermesExitRefused || !strings.Contains(errs, "Nothing was installed") {
		t.Fatalf("bad config.json: %d %s", code, errs)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "__init__.py")); !strings.Contains(string(b), "an old one") {
		t.Fatal("replaced the plugin")
	}
	// Nor one that isn't JSON: refused, saying why, without a traceback.
	os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{not json`), 0o644)
	if _, errs, code := runScript(t, script, env(hh)); code != HermesExitRefused || !strings.Contains(errs, "can't be read") || strings.Contains(errs, "Traceback") {
		t.Fatalf("config.json not JSON: %d %s", code, errs)
	}
}

// --quiet (or WAKESHIFT_QUIET=1) prints one line saying what happened and
// nothing else, neither Hermes's output nor a terms notice; --chained (or
// WAKESHIFT_CHAINED=1) closes with a line for a person, not an agent
// mid-chat. The default is for an agent.
func TestHermesInstallQuietAndChained(t *testing.T) {
	script, _ := hermesScriptFor(t)
	logf := filepath.Join(t.TempDir(), "ran")
	ok := fakeHermes(t, logf, 0)
	env := func(more ...string) []string {
		return append([]string{"HOME=" + t.TempDir(), "HERMES_HOME=" + t.TempDir(), "HERMES_BIN=" + ok}, more...)
	}
	for _, c := range []struct {
		name string
		args []string
		env  []string
	}{{"--quiet", []string{"--quiet"}, nil}, {"WAKESHIFT_QUIET", nil, []string{"WAKESHIFT_QUIET=1"}}} {
		out, errs, code := runScript(t, script, env(c.env...), c.args...)
		lines := strings.Split(strings.TrimSpace(out), "\n")
		if code != 0 || errs != "" || len(lines) != 1 ||
			!strings.HasPrefix(lines[0], "Installed Wakeshift's plugin for Hermes in ") || !strings.Contains(lines[0], "Start a new Hermes chat") {
			t.Fatalf("%s: %d %q %q", c.name, code, out, errs)
		}
	}
	// Quiet, and Hermes's enable fails: what Hermes said goes to stderr.
	failing := fakeHermes(t, logf, 3)
	out, errs, code := runScript(t, script, env("HERMES_BIN="+failing), "--quiet")
	if code != HermesExitNotEnabled || strings.TrimSpace(out) != "" || !strings.Contains(errs, "hermes says hi") || strings.Contains(errs, "watch tool") {
		t.Fatalf("quiet, enable fails: %d %q %q", code, out, errs)
	}

	for _, c := range []struct {
		name string
		args []string
		env  []string
	}{{"--chained", []string{"--chained"}, nil}, {"WAKESHIFT_CHAINED", nil, []string{"WAKESHIFT_CHAINED=1"}}} {
		out, _, code := runScript(t, script, env(c.env...), c.args...)
		if code != 0 || !strings.Contains(out, "Wrote Wakeshift's plugin") || !strings.Contains(out, "hermes says hi") ||
			!strings.Contains(out, "if Hermes's gateway is running, restart it") || strings.Contains(out, "Tell the user") || strings.Contains(out, "watch tool") {
			t.Fatalf("%s: %d %s", c.name, code, out)
		}
	}
	out, _, _ = runScript(t, script, env())
	if !strings.Contains(out, "Tell the user to resume it") {
		t.Fatalf("default: %s", out)
	}
	// WAKESHIFT_QUIET=0 isn't quiet.
	if out, _, _ := runScript(t, script, env("WAKESHIFT_QUIET=0")); !strings.Contains(out, "Tell the user") {
		t.Fatalf("WAKESHIFT_QUIET=0: %s", out)
	}
}

// Run as root on a Hermes home that's another user's, it installs as that
// user (here a stand-in id says it's root, and a stand-in setpriv records the
// switch); with no way to switch, it stops before anything is written.
func TestHermesInstallAsRoot(t *testing.T) {
	script, _ := hermesScriptFor(t)
	if os.Getuid() == 0 {
		t.Skip("needs a home that isn't root's")
	}
	fakes := t.TempDir()
	os.WriteFile(filepath.Join(fakes, "id"), []byte("#!/bin/sh\nif [ \"$*\" = -u ]; then echo 0; exit 0; fi\nexec /usr/bin/id \"$@\"\n"), 0o755)
	switched := filepath.Join(t.TempDir(), "switched")
	os.WriteFile(filepath.Join(fakes, "setpriv"), []byte("#!/bin/sh\necho \"$@\" > \""+switched+"\"\nwhile [ $# -gt 0 ]; do case \"$1\" in --*) shift ;; *) break ;; esac; done\nexec \"$@\"\n"), 0o755)
	logf := filepath.Join(t.TempDir(), "ran")
	ok := fakeHermes(t, logf, 0)
	hh := t.TempDir()
	out, errs, code := runScript(t, script, []string{"PATH=" + fakes + ":/usr/bin:/bin", "HOME=" + t.TempDir(), "HERMES_HOME=" + hh, "HERMES_BIN=" + ok})
	b, _ := os.ReadFile(switched)
	if code != 0 || !strings.Contains(out, "Installing as ") || !strings.HasPrefix(string(b), "--reuid=") {
		t.Fatalf("as root: %d %s %s (switched: %q)", code, out, errs, b)
	}
	if _, err := os.Stat(filepath.Join(hh, "plugins", "wakeshift", "__init__.py")); err != nil {
		t.Fatal("not written")
	}

	// WAKESHIFT_AS_ROOT=1: no switch.
	os.Remove(switched)
	if _, _, code := runScript(t, script, []string{"PATH=" + fakes + ":/usr/bin:/bin", "HOME=" + t.TempDir(), "HERMES_HOME=" + t.TempDir(), "HERMES_BIN=" + ok, "WAKESHIFT_AS_ROOT=1"}); code != 0 {
		t.Fatalf("WAKESHIFT_AS_ROOT: %d", code)
	}
	if _, err := os.Stat(switched); !os.IsNotExist(err) {
		t.Fatal("switched user with WAKESHIFT_AS_ROOT=1")
	}

	// Nothing to switch with: a permission problem, nothing written.
	bare := t.TempDir()
	os.Symlink(filepath.Join(fakes, "id"), filepath.Join(bare, "id"))
	for _, c := range []string{"stat", "cat"} {
		if p, err := exec.LookPath(c); err == nil {
			os.Symlink(p, filepath.Join(bare, c))
		}
	}
	hh = t.TempDir()
	_, errs, code = runScript(t, script, []string{"PATH=" + bare, "HOME=" + t.TempDir(), "HERMES_HOME=" + hh, "HERMES_BIN=" + ok})
	if code != HermesExitPermission || !strings.Contains(errs, "WAKESHIFT_AS_ROOT=1") {
		t.Fatalf("no way to switch: %d %s", code, errs)
	}
	if _, err := os.Stat(filepath.Join(hh, "plugins")); !os.IsNotExist(err) {
		t.Fatal("wrote something")
	}
}

// As root, a plugin an earlier run left root's is handed to the Hermes
// home's owner, who controls that home, so nothing there is followed: the
// chown runs inside the plugin's own directory, on what's directly in it
// (never a link or a file with another hard link) and the directory last,
// and not at all if plugins/ or the plugin's directory is a link, or the
// directory is its owner's already. Here a stand-in stat says the home
// belongs to "nobody" and a stand-in chown records where it ran and on
// what.
func TestHermesInstallAsRootChownFollowsNothing(t *testing.T) {
	script, _ := hermesScriptFor(t)
	if os.Getuid() == 0 {
		t.Skip("needs a home that isn't root's")
	}
	if exec.Command("id", "nobody").Run() != nil {
		t.Skip("no user nobody here")
	}
	fakes := t.TempDir()
	os.WriteFile(filepath.Join(fakes, "id"), []byte("#!/bin/sh\nif [ \"$*\" = -u ]; then echo 0; exit 0; fi\nexec /usr/bin/id \"$@\"\n"), 0o755)
	os.WriteFile(filepath.Join(fakes, "setpriv"), []byte("#!/bin/sh\nwhile [ $# -gt 0 ]; do case \"$1\" in --*) shift ;; *) break ;; esac; done\nexec \"$@\"\n"), 0o755)
	os.WriteFile(filepath.Join(fakes, "stat"), []byte("#!/bin/sh\necho nobody\n"), 0o755)
	chowned := filepath.Join(t.TempDir(), "chowned")
	os.WriteFile(filepath.Join(fakes, "chown"), []byte("#!/bin/sh\necho \"$(pwd -P) $*\" >> \""+chowned+"\"\n"), 0o755)
	logf := filepath.Join(t.TempDir(), "ran")
	ok := fakeHermes(t, logf, 0)
	// plugin lays out a plugin an earlier root run left, with a link, a
	// file with another hard link and a folder in it.
	plugin := func(dir string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
			t.Fatal(err)
		}
		os.WriteFile(filepath.Join(dir, "__init__.py"), []byte("# "+hermes.Marker+"\n"), 0o644)
		os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"issuers": []}`), 0o644)
		outside := filepath.Join(t.TempDir(), "someone-elses")
		os.WriteFile(outside, []byte("x"), 0o644)
		os.Symlink(outside, filepath.Join(dir, "link"))
		if err := os.Link(outside, filepath.Join(dir, "hard")); err != nil {
			t.Fatal(err)
		}
	}
	run := func(hh string) string {
		t.Helper()
		os.Remove(chowned)
		_, errs, code := runScript(t, script, []string{"PATH=" + fakes + ":/usr/bin:/bin", "HOME=" + t.TempDir(), "HERMES_HOME=" + hh, "HERMES_BIN=" + ok})
		if code != 0 {
			t.Fatalf("%d %s", code, errs)
		}
		b, _ := os.ReadFile(chowned)
		return string(b)
	}

	// The plugin's directory, in the Hermes home: handed over from inside
	// it, the directory last.
	hh := t.TempDir()
	plugin(filepath.Join(hh, "plugins", "wakeshift"))
	real, _ := filepath.EvalSymlinks(filepath.Join(hh, "plugins", "wakeshift"))
	got := strings.Fields(run(hh))
	if len(got) < 5 || got[0] != real || got[1] != "-h" || !strings.HasPrefix(got[2], "nobody:") || got[len(got)-1] != "." {
		t.Fatalf("chown: %q", got)
	}
	names := got[3:]
	for _, want := range []string{"./__init__.py", "./config.json", "./sub", "."} {
		if !slices.Contains(names, want) {
			t.Errorf("didn't chown %s: %q", want, names)
		}
	}
	for _, not := range []string{"./link", "./hard"} {
		if slices.Contains(names, not) {
			t.Errorf("chowned %s: %q", not, names)
		}
	}

	// plugins/ is a link (to another plugin with the marker, outside the
	// Hermes home): nothing is chowned.
	hh = t.TempDir()
	elsewhere := t.TempDir()
	plugin(filepath.Join(elsewhere, "wakeshift"))
	os.Symlink(elsewhere, filepath.Join(hh, "plugins"))
	if got := run(hh); got != "" {
		t.Fatalf("plugins/ a link: chowned %q", got)
	}

	// The plugin's directory is a link: nothing is chowned.
	hh = t.TempDir()
	elsewhere = t.TempDir()
	plugin(filepath.Join(elsewhere, "wakeshift"))
	os.MkdirAll(filepath.Join(hh, "plugins"), 0o755)
	os.Symlink(filepath.Join(elsewhere, "wakeshift"), filepath.Join(hh, "plugins", "wakeshift"))
	if got := run(hh); got != "" {
		t.Fatalf("the plugin's directory a link: chowned %q", got)
	}
}

// The tarball's plugin.yaml says the server's version, the same one
// _version.py gives the plugin's requests (X-Wakeshift-Connector); a local
// build's says 0.0.0+dev.
func TestHermesPackVersion(t *testing.T) {
	defer func(v string) { version.Version = v }(version.Version)
	for v, want := range map[string]string{"2026.10.31.1819+520c4d3": "2026.10.31.1819+520c4d3", "dev": "0.0.0+dev"} {
		version.Version = v
		b, _ := (&Server{}).hermesPack()
		files := map[string]string{}
		gz, err := gzip.NewReader(bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		tr := tar.NewReader(gz)
		for {
			h, err := tr.Next()
			if err != nil {
				break
			}
			c, _ := io.ReadAll(tr)
			files[h.Name] = string(c)
		}
		m := regexp.MustCompile(`(?m)^version: "([^"]*)"$`).FindStringSubmatch(files["wakeshift/plugin.yaml"])
		if m == nil || m[1] != want {
			t.Fatalf("%s: plugin.yaml says %v", v, m)
		}
		if files["wakeshift/"+hermes.VersionFileName] != string(hermes.VersionFile(v)) {
			t.Fatalf("%s: _version.py %q", v, files["wakeshift/"+hermes.VersionFileName])
		}
		if v != "dev" && !strings.Contains(files["wakeshift/"+hermes.VersionFileName], `"`+want+`"`) {
			t.Fatalf("%s: the versions differ", v)
		}
	}
}
