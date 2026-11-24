package downloads

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// OpenClawPlugin is Wakeshift's plugin for OpenClaw (plugins/openclaw), as
// npm pack makes it, in ConnectorDir. /install/openclaw.sh fetches it,
// checks its SHA-256, installs it with OpenClaw's own `openclaw plugins
// install`, and pins this Wakeshift as the issuer it trusts.
const OpenClawPlugin = "wakeshift-openclaw.tgz"

// openclawPluginSum hashes the plugin once ("" if it isn't built here).
func (s *Server) openclawPluginSum() string {
	s.pluginOnce.Do(func() {
		if s.ConnectorDir == "" {
			return
		}
		b, err := os.ReadFile(filepath.Join(s.ConnectorDir, OpenClawPlugin))
		if err != nil {
			return
		}
		sum := sha256.Sum256(b)
		s.pluginSum = hex.EncodeToString(sum[:])
	})
	return s.pluginSum
}

func (s *Server) openclawDownload(w http.ResponseWriter, r *http.Request) {
	if s.openclawPluginSum() == "" {
		http.Error(w, "Wakeshift's plugin for OpenClaw isn't built on this server", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+OpenClawPlugin+`"`)
	http.ServeFile(w, r, filepath.Join(s.ConnectorDir, OpenClawPlugin))
}

// openclawScript is /install/openclaw.sh. It runs where the OpenClaw
// gateway runs (the agent's exec tool), changes nothing if the download
// doesn't match, and uses OpenClaw's own commands for everything it
// changes; /install/openclaw-uninstall.sh undoes it.
//
// The plugin wakes conversations through OpenClaw's /hooks/agent, turning
// hooks on when it first connects one (plugins/openclaw/src/hooks.ts).
// Hooks share the gateway's listener, so on a gateway that listens beyond
// this computer (its bind isn't loopback) the script stops before
// changing anything and says to ask the user; with their yes
// (WAKESHIFT_OPENCLAW_NETWORK_HOOKS=1) it records it as the plugin's
// allowNetworkHooks, which the plugin checks too.
//
// Its order matters (verified against 2026.10.4): the issuer is set first,
// which OpenClaw accepts before the plugin exists, so the plugin never loads
// without it; setting it after an install made the gateway replace the
// just-loaded plugin, which sat behind a lifecycle lease for 5 minutes and
// wedged the enable that followed. A plugin that's loaded can't be
// replaced until the turn running the installer ends (each turn holds the
// plugins it started with), so an install that would change nothing
// changes nothing, and each command gets a time limit, past which the
// script says how to recover.
func (s *Server) openclawScript(w http.ResponseWriter) {
	sum := s.openclawPluginSum()
	if sum == "" {
		http.Error(w, "Wakeshift's plugin for OpenClaw isn't built on this server", http.StatusNotFound)
		return
	}
	base := strings.TrimSuffix(s.PublicURL, "/")
	w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	fmt.Fprintf(w, `#!/bin/sh
# Installs Wakeshift's plugin for OpenClaw, so Wakeshift messages can wake
# your OpenClaw sessions. It sets the plugin's setting, the Wakeshift it
# trusts (%[1]s), then installs the plugin with openclaw plugins install.
# When it first connects a session, the plugin turns on OpenClaw's hooks
# endpoint with a token of its own, allowing only the sessions and agents
# that connect through Wakeshift.
# Remove it (and undo that) with: curl -fsSL %[4]s | sh
# It exits %[7]d if the download doesn't match the checksum below (nothing is
# installed), 3 if the gateway's network setting needs the user's agreement
# first, and 1 on any other failure.
set -eu
command -v openclaw >/dev/null 2>&1 || { echo "openclaw isn't on PATH here: run this where the OpenClaw gateway runs" >&2; exit 1; }
recover="If OpenClaw stops answering, restart its gateway (openclaw gateway restart; in Docker, docker restart <its container>)."
limit="${WAKESHIFT_OPENCLAW_TIMEOUT:-120}"
# An OpenClaw command, given limit seconds: one that hangs (the gateway is
# stuck) stops the install with the way out. Its output is shown without
# OpenClaw's warning that the plugin's settings name a plugin it doesn't
# have yet: the script sets them before the install, on purpose (below).
oc() {
  st=0
  if command -v timeout >/dev/null 2>&1; then
    timeout "$limit" openclaw "$@" > "$tmp/oc.out" 2>&1 || st=$?
    [ "$st" -eq 124 ] && st=143
  else
    # A watchdog that stops the command after limit seconds. It signals
    # only while its marker is there (so never a later process that got the
    # same PID), and is stopped and reaped as soon as the command exits.
    openclaw "$@" > "$tmp/oc.out" 2>&1 &
    pid=$!
    marker="$tmp/watching.$pid"
    : > "$marker"
    sh -c 'i=0; while [ "$i" -lt "$3" ]; do sleep 1; [ -f "$1" ] || exit 0; kill -0 "$2" 2>/dev/null || exit 0; i=$((i + 1)); done; [ -f "$1" ] && kill "$2" 2>/dev/null; exit 0' wakeshift-watchdog "$marker" "$pid" "$limit" &
    dog=$!
    wait "$pid" || st=$?
    rm -f "$marker"
    kill "$dog" 2>/dev/null || true
    wait "$dog" 2>/dev/null || true
  fi
  grep -v 'plugins\.entries\.wakeshift: plugin not found' "$tmp/oc.out" || true
  [ "$st" -eq 0 ] && return 0
  if [ "$st" -gt 128 ]; then
    echo "openclaw $1 $2 didn't finish. $recover" >&2
  else
    echo "openclaw $1 $2 failed. $recover" >&2
  fi
  exit 1
}
tmp=$(mktemp -d "${TMPDIR:-/tmp}/wakeshift.XXXXXX")
trap 'rm -rf "$tmp"' EXIT
curl -fsSL "%[6]s" -o "$tmp/%[2]s"
got=$( (sha256sum "$tmp/%[2]s" 2>/dev/null || shasum -a 256 "$tmp/%[2]s") | cut -d' ' -f1)
if [ "$got" != "%[3]s" ]; then
  echo "The download didn't match its checksum; nothing was installed." >&2
  exit %[7]d
fi
# The running gateway's bind: its --bind, from the processes this runs
# under (the agent's exec tool runs inside the gateway), else
# gateway.bind, else OpenClaw's default (auto in a container).
bind_of() {
  pid=$PPID
  n=0
  while [ "${pid:-0}" -gt 1 ] && [ "$n" -lt 30 ]; do
    if [ -r "/proc/$pid/cmdline" ]; then
      args=$(tr '\0' ' ' < "/proc/$pid/cmdline")
      ppid=$(sed -n 's/^PPid:[[:space:]]*//p' "/proc/$pid/status" 2>/dev/null)
    else
      args=$(ps -o args= -p "$pid" 2>/dev/null || true)
      ppid=$(ps -o ppid= -p "$pid" 2>/dev/null | tr -d ' ')
    fi
    case "$args" in *gateway*--bind*) printf '%%s\n' "$args" | sed -n 's/.*--bind[= ][= ]*\([a-z]*\).*/\1/p'; return ;; esac
    pid=$ppid
    n=$((n + 1))
  done
  b=$(openclaw config get gateway.bind 2>/dev/null | tr -d '" ' | head -n 1)
  case "$b" in loopback|lan|auto|custom|tailnet) echo "$b"; return ;; esac
  if [ -f /.dockerenv ]; then echo auto; else echo loopback; fi
}
network=no
case "$(bind_of)" in
  loopback) ;;
  custom) case "$(openclaw config get gateway.customBindHost 2>/dev/null | tr -d '" ')" in 127.0.0.1|::1|localhost) ;; *) network=yes ;; esac ;;
  *) network=yes ;;
esac
if [ "$network" = yes ] && ! openclaw config get plugins.entries.wakeshift.config.allowNetworkHooks 2>/dev/null | grep -q true; then
  if [ "${WAKESHIFT_OPENCLAW_NETWORK_HOOKS:-}" != 1 ]; then
    echo "Nothing was installed. This OpenClaw gateway listens beyond this computer (its bind isn't loopback). Wakeshift's plugin wakes conversations through OpenClaw's hooks endpoint, which shares the gateway's listener, so it would be reachable there too, guarded by a token of the plugin's own. Ask the user whether that's all right; only if they agree, run: curl -fsSL %[5]s | WAKESHIFT_OPENCLAW_NETWORK_HOOKS=1 sh (and suggest a firewall or proxy rule that blocks /hooks/ from other machines)." >&2
    exit 3
  fi
  # The user agreed: recorded where the plugin reads it.
  oc config set plugins.entries.wakeshift.config.allowNetworkHooks true --strict-json
fi
state="${OPENCLAW_STATE_DIR:-$HOME/.openclaw}/wakeshift"
enabled() { openclaw plugins inspect wakeshift 2>/dev/null | grep -q 'Status: enabled'; }
# Installed already, this same plugin trusting this Wakeshift: nothing to
# change (changing a loaded plugin waits for the turn that runs this).
if [ "$(cat "$state/installed" 2>/dev/null || true)" = "%[3]s %[1]s" ] && enabled; then
  echo "Wakeshift's plugin for OpenClaw is installed already and trusts %[1]s; nothing changed. The next result of the app's tools you'd call anyway connects this session; don't call them just for that."
  exit 0
fi
# The Wakeshift it trusts, first: OpenClaw keeps the setting before the
# plugin is installed, so the plugin never loads without it.
if ! openclaw config get plugins.entries.wakeshift.config.issuer 2>/dev/null | grep -qF '%[1]s'; then
  oc config set plugins.entries.wakeshift.config.issuer '"%[1]s"' --strict-json
fi
# The user agreed to this install; the plugin declares a tool-result filter
# (it takes Wakeshift's beacon out of what the model sees) and hooks.
# OpenClaw installs, enables and loads it at once.
oc plugins install "npm-pack:$tmp/%[2]s" --force --accept-capabilities
# A reinstall can keep an earlier "enabled": false.
enabled || oc plugins enable wakeshift --accept-capabilities
mkdir -p "$state"
printf '%%s\n' "%[3]s %[1]s" > "$state/installed"
echo "Wakeshift's plugin for OpenClaw is installed and trusts %[1]s. There's nothing to approve and no restart: it starts with the next turn (a plugin that was loaded already is replaced once this turn ends), and connects this session from the next result of the app's tools you'd call anyway (don't call them just for that). When it connects, it turns on OpenClaw's hooks endpoint for that session only, which the uninstaller undoes. Only if OpenClaw stops answering after this, restart its gateway (openclaw gateway restart; in Docker, docker restart <its container>)."
`, base, OpenClawPlugin, sum,
		s.fileURL("install/openclaw-uninstall.sh", "openclaw-uninstall.sh"),
		s.fileURL("install/openclaw.sh", "openclaw.sh"),
		s.fileURL("download/openclaw/"+OpenClawPlugin, OpenClawPlugin),
		ExitVerify)
}

// openclawUninstallScript is /install/openclaw-uninstall.sh: it removes the
// plugin, then undoes what the plugin changed in OpenClaw's hooks settings,
// from the record it kept (<state>/hooks.json, with restore.mjs, the
// plugin's own undo, next to it): it takes out only what the plugin added
// and puts back a setting only while it still holds the plugin's value.
func (s *Server) openclawUninstallScript(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	fmt.Fprintf(w, `#!/bin/sh
# Removes Wakeshift's plugin for OpenClaw, and undoes what it changed in
# OpenClaw's hooks settings (hooks.*): only what it added, and a setting
# only while it still holds the plugin's value.
set -eu
command -v openclaw >/dev/null 2>&1 || { echo "openclaw isn't on PATH here: run this where the OpenClaw gateway runs" >&2; exit 1; }
state="${OPENCLAW_STATE_DIR:-$HOME/.openclaw}/wakeshift"
if openclaw plugins inspect wakeshift >/dev/null 2>&1; then
  openclaw plugins uninstall wakeshift --force
fi
if [ -f "$state/hooks.json" ] && [ -f "$state/restore.mjs" ]; then
  command -v node >/dev/null 2>&1 || { echo "node isn't on PATH here, so OpenClaw's hooks settings weren't put back; $state/hooks.json says what the plugin changed" >&2; exit 1; }
  patch=$( (openclaw config get hooks --json 2>/dev/null || true) | node "$state/restore.mjs" "$state/hooks.json")
  if [ -n "$patch" ]; then
    printf '%%s\n' "$patch" | openclaw config patch --stdin
    echo "OpenClaw's hooks settings are back as they were, apart from what you changed since."
  fi
fi
rm -rf "$state"
echo "Wakeshift's plugin for OpenClaw is removed. To install it again: curl -fsSL %[1]s | sh"
`, s.fileURL("install/openclaw.sh", "openclaw.sh"))
}
