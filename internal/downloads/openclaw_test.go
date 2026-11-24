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

// /install/openclaw.sh fetches the plugin, checks its SHA-256, pins this
// Wakeshift and then installs it with OpenClaw's own commands; an install
// that would change nothing changes nothing, a command that hangs stops it
// with the way out, a download that doesn't match installs nothing, and a
// server without the plugin built serves no script.
func TestOpenClawInstallScript(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("the script is for macOS and Linux")
	}
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, OpenClawPlugin), []byte("plugin bytes"), 0o644)
	s := &Server{ConnectorDir: dir}
	mux := http.NewServeMux()
	for p, h := range s.Routes() {
		mux.Handle(p, h)
	}
	srv := httptest.NewServer(mux)
	defer srv.Close()
	s.PublicURL = srv.URL
	if got := s.HarnessInstall("openclaw").Command; got != "curl -fsSL "+srv.URL+"/install/openclaw.sh | sh" {
		t.Fatalf("command %q", got)
	}

	// A stand-in openclaw that records what it was asked, keeps a copy of the
	// plugin it was handed and the settings it was given ($HOME/issuer,
	// $HOME/allow; $HOME/bind is gateway.bind, $HOME/hooks the hooks shown),
	// keeps a config patch it's handed, and says whether the plugin is
	// enabled; with $HOME/hang, its install hangs.
	bin := t.TempDir()
	os.WriteFile(filepath.Join(bin, "openclaw"), []byte(`#!/bin/sh
echo "$@" >> "$HOME/ran"
case "$1 $2" in
  "config get")
    case "$3" in
      plugins.entries.wakeshift.config.issuer) cat "$HOME/issuer" 2>/dev/null ;;
      plugins.entries.wakeshift.config.allowNetworkHooks) cat "$HOME/allow" 2>/dev/null ;;
      gateway.bind) cat "$HOME/bind" 2>/dev/null ;;
      hooks) cat "$HOME/hooks" 2>/dev/null || { echo '{"ok":false}'; exit 1; } ;;
    esac ;;
  "config set")
    # What 2026.10.4 says while the plugin's settings come before the plugin.
    [ -f "$HOME/enabled" ] || echo "Config warnings: plugins.entries.wakeshift: plugin not found: wakeshift (stale config entry ignored; remove it from plugins config)" >&2
    echo "Updated $3."
    case "$3" in
      plugins.entries.wakeshift.config.issuer) printf '%s' "$4" > "$HOME/issuer" ;;
      plugins.entries.wakeshift.config.allowNetworkHooks) printf '%s' "$4" > "$HOME/allow" ;;
    esac ;;
  "config patch") cat > "$HOME/patched" ;;
  "plugins install") [ -f "$HOME/hang" ] && exec sleep 30; cp "${3#npm-pack:}" "$HOME/got"; [ -f "$HOME/disabled" ] || touch "$HOME/enabled" ;;
  "plugins enable") touch "$HOME/enabled" ;;
  "plugins uninstall") rm -f "$HOME/enabled"; touch "$HOME/uninstalled" ;;
  "plugins inspect") pgrep -f "wakeshift-watchdog $TMPDIR" >> "$HOME/stale"; [ -f "$HOME/enabled" ] && echo "Status: enabled"; [ -f "$HOME/enabled" ] ;;
esac
`), 0o755)
	// The script's temporary files (and its watchdog's marker) are here, so
	// a watchdog left running would name it.
	tmpdir := t.TempDir()
	// The stand-in also looks for one each time the script asks whether the
	// plugin is enabled, right after a command it watched.
	noWatchdog := func(when string, homes ...string) {
		t.Helper()
		for _, h := range homes {
			if b, _ := os.ReadFile(filepath.Join(h, "stale")); len(b) > 0 {
				t.Fatalf("%s: a watchdog outlived its command: %s", when, b)
			}
		}
		if out, err := exec.Command("pgrep", "-f", "wakeshift-watchdog "+tmpdir).Output(); err == nil {
			t.Fatalf("%s: a watchdog outlived its command: %s", when, out)
		}
	}
	runScript := func(script, home string, env ...string) (string, string, error) {
		// A gateway on loopback unless the test says otherwise (OpenClaw's
		// default in a container is auto, which isn't).
		if _, err := os.Stat(filepath.Join(home, "bind")); err != nil {
			os.WriteFile(filepath.Join(home, "bind"), []byte("loopback"), 0o644)
		}
		resp, err := http.Get(srv.URL + script)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		cmd := exec.Command("sh")
		cmd.Stdin = resp.Body
		cmd.Env = append(append(os.Environ(), "HOME="+home, "PATH="+bin+":"+os.Getenv("PATH"), "WAKESHIFT_OPENCLAW_TIMEOUT=2", "OPENCLAW_STATE_DIR=", "TMPDIR="+tmpdir, "WAKESHIFT_OPENCLAW_NETWORK_HOOKS="), env...)
		out, err := cmd.CombinedOutput()
		ran, _ := os.ReadFile(filepath.Join(home, "ran"))
		got, _ := os.ReadFile(filepath.Join(home, "got"))
		return string(out) + "|" + strings.TrimSpace(string(ran)), string(got), err
	}
	run := func(home string, env ...string) (string, string, error) {
		return runScript("/install/openclaw.sh", home, env...)
	}
	home := t.TempDir()
	out, got, err := run(home)
	if err != nil || got != "plugin bytes" {
		t.Fatalf("install: %v %s", err, out)
	}
	// The issuer first (so the plugin never loads without it), then the
	// install, which enables it: no separate enable.
	set := strings.Index(out, `config set plugins.entries.wakeshift.config.issuer "`+srv.URL+`" --strict-json`)
	install := strings.Index(out, "plugins install npm-pack:")
	noWatchdog("after the install", home)
	if set < 0 || install < set || !strings.Contains(out, " --force --accept-capabilities") || strings.Contains(out, "plugins enable") {
		t.Fatalf("order: %s", out)
	}
	if !strings.Contains(out, "openclaw gateway restart") || !strings.Contains(out, "docker restart") {
		t.Fatalf("no recovery step: %s", out)
	}
	// OpenClaw's own output is shown, but not its warning about settings
	// for a plugin it doesn't have yet (set first on purpose).
	if !strings.Contains(out, "Updated plugins.entries.wakeshift.config.issuer.") || strings.Contains(out, "plugin not found") {
		t.Fatalf("output: %s", out)
	}
	// Again, nothing to change: nothing changed (a loaded plugin waits for
	// the turn running the installer).
	os.Remove(filepath.Join(home, "ran"))
	if out, _, err := run(home); err != nil || !strings.Contains(out, "nothing changed") || strings.Contains(out, "config set") || strings.Contains(out, "plugins install") {
		t.Fatalf("again: %v %s", err, out)
	}
	// Disabled before: enabled again. The issuer set already isn't set again.
	disabled := t.TempDir()
	os.WriteFile(filepath.Join(disabled, "disabled"), nil, 0o644)
	os.WriteFile(filepath.Join(disabled, "issuer"), []byte(`"`+srv.URL+`"`), 0o644)
	if out, _, err := run(disabled); err != nil || !strings.Contains(out, "plugins enable wakeshift --accept-capabilities") || strings.Contains(out, "config set") {
		t.Fatalf("disabled: %v %s", err, out)
	}
	noWatchdog("after installing over a disabled one", disabled)
	// A command that hangs: stopped, with the way out.
	hung := t.TempDir()
	os.WriteFile(filepath.Join(hung, "hang"), nil, 0o644)
	if out, _, err := run(hung); err == nil || !strings.Contains(out, "openclaw plugins install didn't finish. If OpenClaw stops answering, restart its gateway") {
		t.Fatalf("hung: %v %s", err, out)
	}
	noWatchdog("after a command that hung")
	// A gateway that listens beyond this computer: nothing installed or
	// changed without the user's yes, and the script says how to give it.
	lan := t.TempDir()
	os.WriteFile(filepath.Join(lan, "bind"), []byte("lan"), 0o644)
	out, got, err = run(lan)
	if code := exitCode(err); code != 3 || got != "" || strings.Contains(out, "config set") || strings.Contains(out, "plugins install") ||
		!strings.Contains(out, "Nothing was installed") || !strings.Contains(out, "openclaw.sh | WAKESHIFT_OPENCLAW_NETWORK_HOOKS=1 sh") || !strings.Contains(out, "blocks /hooks/") {
		t.Fatalf("lan, no yes: %d %s", code, out)
	}
	// With it: recorded where the plugin reads it, before the install.
	out, got, err = run(lan, "WAKESHIFT_OPENCLAW_NETWORK_HOOKS=1")
	allow := strings.Index(out, "config set plugins.entries.wakeshift.config.allowNetworkHooks true --strict-json")
	if err != nil || got != "plugin bytes" || allow < 0 || allow > strings.Index(out, "plugins install") {
		t.Fatalf("lan, yes: %v %s", err, out)
	}
	// Given once, not asked again.
	os.Remove(filepath.Join(lan, "ran"))
	if out, _, err := run(lan); err != nil || strings.Contains(out, "Nothing was installed") {
		t.Fatalf("lan, yes given before: %v %s", err, out)
	}
	// A custom bind to another address listens beyond this computer too.
	custom := t.TempDir()
	os.WriteFile(filepath.Join(custom, "bind"), []byte("custom"), 0o644)
	if _, got, err := run(custom); err == nil || got != "" {
		t.Fatalf("custom elsewhere: %v", err)
	}

	// Uninstalling: the plugin goes, then what it changed in hooks.* is put
	// back with the plugin's own undo (restore.mjs), then its state.
	if _, err := exec.LookPath("node"); err == nil {
		gone := t.TempDir()
		state := filepath.Join(gone, ".openclaw", "wakeshift")
		os.MkdirAll(state, 0o700)
		os.WriteFile(filepath.Join(gone, "enabled"), nil, 0o644)
		os.WriteFile(filepath.Join(gone, "hooks"), []byte(`{"enabled":true,"token":"__OPENCLAW_REDACTED__"}`), 0o644)
		os.WriteFile(filepath.Join(state, "hooks.json"), []byte(`{"version":1}`), 0o600)
		// A stand-in undo: echoes what it was given, as a patch.
		os.WriteFile(filepath.Join(state, "restore.mjs"), []byte(`import { readFileSync } from "node:fs";
const cur = JSON.parse(readFileSync(0, "utf8"));
process.stdout.write(JSON.stringify({ hooks: { enabled: null, seen: cur.enabled, record: JSON.parse(readFileSync(process.argv[2], "utf8")).version } }) + "\n");
`), 0o644)
		out, _, err := runScript("/install/openclaw-uninstall.sh", gone)
		patched, _ := os.ReadFile(filepath.Join(gone, "patched"))
		if err != nil || !strings.Contains(out, "plugins uninstall wakeshift --force") || strings.TrimSpace(string(patched)) != `{"hooks":{"enabled":null,"seen":true,"record":1}}` {
			t.Fatalf("uninstall: %v %s patched=%s", err, out, patched)
		}
		if _, err := os.Stat(state); !os.IsNotExist(err) {
			t.Fatalf("state left: %v", err)
		}
		if strings.Index(out, "plugins uninstall") > strings.Index(out, "config patch") {
			t.Fatalf("patched before the plugin was gone: %s", out)
		}
		// Nothing recorded: only the plugin goes.
		plain := t.TempDir()
		os.WriteFile(filepath.Join(plain, "enabled"), nil, 0o644)
		if out, _, err := runScript("/install/openclaw-uninstall.sh", plain); err != nil || strings.Contains(out, "config patch") || !strings.Contains(out, "plugins uninstall wakeshift --force") {
			t.Fatalf("uninstall, nothing recorded: %v %s", err, out)
		}
	}

	// Swapped after the sum was taken: refused, nothing run.
	os.WriteFile(filepath.Join(dir, OpenClawPlugin), []byte("other bytes"), 0o644)
	if out, _, err := run(t.TempDir()); exitCode(err) != ExitVerify || !strings.Contains(out, "didn't match its checksum") || !strings.HasSuffix(out, "|") {
		t.Fatalf("a plugin that doesn't match: %v %s", err, out)
	}
	// Not built here: no script, no download.
	empty := &Server{ConnectorDir: t.TempDir(), PublicURL: "https://x.test"}
	rec := httptest.NewRecorder()
	empty.openclawScript(rec)
	if rec.Code != 404 {
		t.Fatalf("unbuilt: %d", rec.Code)
	}
}

func exitCode(err error) int {
	if e, ok := err.(*exec.ExitError); ok {
		return e.ExitCode()
	}
	if err != nil {
		return -1
	}
	return 0
}
