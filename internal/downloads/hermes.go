package downloads

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/wakeshift/server/internal/version"
	"github.com/wakeshift/server/plugins/hermes"
)

// HermesPlugin is Wakeshift's plugin for Hermes Agent (plugins/hermes, in
// Python), packed from the files the server embeds. /install/hermes.sh
// fetches it, checks its SHA-256, writes it to $HERMES_HOME/plugins/wakeshift,
// pins this Wakeshift in its config.json and enables it with Hermes's own
// command. Hermes runs no hook command, so nothing else is installed.
const HermesPlugin = "wakeshift-hermes.tar.gz"

// hermesPack is the plugin as a tar.gz (wakeshift/<file>), the same bytes
// every time, and its SHA-256.
func (s *Server) hermesPack() ([]byte, string) {
	s.hermesOnce.Do(func() {
		var buf bytes.Buffer
		gz, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
		gz.ModTime = time.Unix(0, 0)
		tw := tar.NewWriter(gz)
		err := fs.WalkDir(hermes.Files, "wakeshift", func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			b, err := hermes.Files.ReadFile(p)
			if err != nil {
				return err
			}
			// plugin.yaml says this server's version, as _version.py does.
			b = hermes.Stamped(path.Base(p), b, version.Version)
			hdr := &tar.Header{Name: "wakeshift/" + path.Base(p), Mode: 0o644, Size: int64(len(b)), ModTime: time.Unix(0, 0), Format: tar.FormatPAX}
			if err := tw.WriteHeader(hdr); err != nil {
				return err
			}
			_, err = tw.Write(b)
			return err
		})
		if err == nil {
			// Its version: this server's build (the binary's is the same).
			b := hermes.VersionFile(version.Version)
			hdr := &tar.Header{Name: "wakeshift/" + hermes.VersionFileName, Mode: 0o644, Size: int64(len(b)), ModTime: time.Unix(0, 0), Format: tar.FormatPAX}
			if err = tw.WriteHeader(hdr); err == nil {
				_, err = tw.Write(b)
			}
		}
		if err != nil || tw.Close() != nil || gz.Close() != nil {
			return
		}
		sum := sha256.Sum256(buf.Bytes())
		s.hermesTar, s.hermesSum = buf.Bytes(), hex.EncodeToString(sum[:])
	})
	return s.hermesTar, s.hermesSum
}

func (s *Server) hermesDownload(w http.ResponseWriter, r *http.Request) {
	b, _ := s.hermesPack()
	if b == nil {
		http.Error(w, "Wakeshift's plugin for Hermes couldn't be packed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+HermesPlugin+`"`)
	w.Write(b)
}

// What /install/hermes.sh exits with (site/docs/hermes.md lists them for
// third-party installers that run it). 2 is left to the shell: it's what sh
// exits with on a syntax error, such as a script cut short.
const (
	HermesExitOK          = 0          // installed (or updated) and enabled
	HermesExitRefused     = 1          // refused, nothing changed: not Wakeshift's plugin there, or config.json can't be updated
	HermesExitNotEnabled  = 3          // written, but Hermes's command wasn't found or couldn't enable it
	HermesExitDownload    = 4          // the download failed or couldn't be unpacked; nothing changed
	HermesExitUnsupported = 5          // a tool it needs is missing (curl, tar, mktemp, a SHA-256 tool); nothing changed
	HermesExitPermission  = 6          // it can't write the plugin's directory, or can't switch from root to its owner
	HermesExitVerify      = ExitVerify // the download didn't match the checksum the script carries; nothing changed
	HermesExitUsage       = 64         // an option it doesn't know
)

// hermesScript is /install/hermes.sh. It runs where Hermes runs (inside its
// container, if it's in one), changes nothing if the download doesn't
// match or a "wakeshift" plugin there isn't Wakeshift's, and finds Hermes's
// own command where Hermes's terminal tool doesn't have it on PATH. Run as
// root on a Hermes home that belongs to another user (Hermes's Docker image,
// through docker exec), it runs as that user, as Hermes's own command does,
// so nothing it writes is root's. --quiet and --chained are for another
// installer that runs it (a third-party one, before any chat).
//
// It's one function called on its last line, so a download cut short is a
// syntax error (2), never a half-run install.
func (s *Server) hermesScript(w http.ResponseWriter) {
	_, sum := s.hermesPack()
	if sum == "" {
		http.Error(w, "Wakeshift's plugin for Hermes couldn't be packed", http.StatusInternalServerError)
		return
	}
	base := strings.TrimSuffix(s.PublicURL, "/")
	w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	fmt.Fprintf(w, `#!/bin/sh
# Installs Wakeshift's plugin for Hermes Agent, so Wakeshift messages can wake
# your Hermes conversations. It writes the plugin to
# $HERMES_HOME/plugins/wakeshift (HERMES_HOME is ~/.hermes unless it's set),
# pins the Wakeshift it trusts (%[1]s) in the plugin's config.json,
# enables it with: hermes plugins enable wakeshift
# and lets it start turns in your Hermes chats, which it needs to wake any of
# them (unless you turned it off):
#   hermes config set plugins.entries.wakeshift.allow_gateway_injection true
# It starts them only in conversations connected to Wakeshift.
# Run it where Hermes runs. Run as root on a Hermes home that belongs to
# another user (Hermes's Docker image), it switches to that user first.
#
# Options (or the environment variables):
#   --quiet    (WAKESHIFT_QUIET=1)    print only one result line (implies --chained)
#   --chained  (WAKESHIFT_CHAINED=1)  for another installer that runs this one before any chat: a closing line for a person, not an agent
#
# Exit codes:
#   0   installed (or updated) and enabled
#   1   refused; nothing changed (a "wakeshift" plugin there isn't Wakeshift's, or its config.json can't be updated)
#   2   the shell's own: a syntax error, such as this script cut short by a failed download
#   3   written but not enabled (Hermes's command wasn't found, or enabling it failed)
#   4   the download failed or couldn't be unpacked; nothing changed
#   5   a tool it needs is missing (curl, tar, mktemp, sha256sum or shasum); nothing changed
#   6   permission problem: it can't write the plugin's directory, or runs as root and can't switch to the Hermes home's owner
#   7   the download didn't match the checksum this script carries (it may have been tampered with); nothing changed
#   64  an option it doesn't know
# Download it, then run it (curl -fsSL <url> -o hermes.sh && sh hermes.sh):
# piped into sh, a failed download runs nothing and exits 0.
#
# Remove it with: hermes plugins remove wakeshift (or disable it), then
# delete $HERMES_HOME/plugins/wakeshift and $HERMES_HOME/wakeshift; if this
# turned allow_gateway_injection on (it left .allowed-gateway-injection in the
# plugin's directory), also: hermes config unset plugins.entries.wakeshift.allow_gateway_injection
set -eu
# All of it in one function, called on the last line: a download cut short
# is a syntax error (2) and runs nothing.
main() {
quiet=
chained=
case "${WAKESHIFT_QUIET:-}" in 1 | true | yes) quiet=1 ;; esac
case "${WAKESHIFT_CHAINED:-}" in 1 | true | yes) chained=1 ;; esac
for a in "$@"; do
  case "$a" in
  --quiet) quiet=1 ;;
  --chained) chained=1 ;;
  -h | --help)
    echo "Usage: sh hermes.sh [--quiet] [--chained]"
    echo "  --quiet    print only one result line (implies --chained)"
    echo "  --chained  for another installer that runs this one: a closing line for a person, not an agent"
    echo "Exit codes: 0 installed and enabled; 1 refused, nothing changed; 3 written but not enabled;"
    echo "4 download failed; 5 a tool it needs is missing; 6 permission problem; 7 checksum mismatch; 64 unknown option."
    exit 0
    ;;
  *)
    echo "Unknown option $a (options: --quiet, --chained, --help); nothing was installed." >&2
    exit 64
    ;;
  esac
done
if [ -n "$quiet" ]; then chained=1; fi
WAKESHIFT_QUIET=$quiet
WAKESHIFT_CHAINED=$chained
export WAKESHIFT_QUIET WAKESHIFT_CHAINED
home="${HERMES_HOME:-$HOME/.hermes}"

# The install itself, run with sh -c below (as the Hermes home's owner, when
# that's not root).
set_body() { body=$(cat); }
set_body <<'WAKESHIFT_BODY'
set -eu
home="${HERMES_HOME:-$HOME/.hermes}"
dir="$home/plugins/wakeshift"
say() { if [ -z "$WAKESHIFT_QUIET" ]; then echo "$@"; fi; }
fail() {
  code=$1
  shift
  echo "$@" >&2
  exit "$code"
}
for t in curl tar mktemp; do
  command -v "$t" >/dev/null 2>&1 || fail 5 "This needs $t, which isn't here; nothing was installed."
done
if command -v sha256sum >/dev/null 2>&1; then
  sha="sha256sum"
elif command -v shasum >/dev/null 2>&1; then
  sha="shasum -a 256"
else
  fail 5 "This needs sha256sum or shasum, and neither is here; nothing was installed."
fi
if [ -f "$dir/__init__.py" ] && ! grep -q "%[4]s" "$dir/__init__.py"; then
  fail 1 "$dir isn't Wakeshift's plugin; nothing was installed."
fi
# Writable, or creatable: checked before anything is fetched.
d=$dir
while [ ! -e "$d" ]; do d=$(dirname "$d"); done
if [ ! -d "$d" ] || [ ! -w "$d" ]; then
  fail 6 "$(id -un 2>/dev/null || echo "This user") can't write $d, so the plugin can't be installed in $dir; nothing was installed. Run this as the user Hermes runs as (with Docker: docker exec -u <that user>)."
fi

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
curl -fsSL "%[5]s" -o "$tmp/%[2]s" || fail 4 "Couldn't download Wakeshift's plugin for Hermes from %[5]s; nothing was installed."
got=$($sha "$tmp/%[2]s" | cut -d' ' -f1)
if [ "$got" != "%[3]s" ]; then
  fail %[6]d "The download didn't match its checksum; nothing was installed."
fi
mkdir -p "$tmp/x"
tar -xzf "$tmp/%[2]s" -C "$tmp/x" || fail 4 "The download couldn't be unpacked; nothing was installed."

# Hermes's command: on PATH, else its Python's (HERMES_PYTHON, which
# Hermes's gateway sets), else where Hermes's Docker image and installer put
# it. Hermes's terminal tool doesn't always have it on PATH.
hermes=""
pyhermes=""
if command -v hermes >/dev/null 2>&1; then
  hermes=$(command -v hermes)
elif [ -n "${HERMES_PYTHON:-}" ] && [ -x "$HERMES_PYTHON" ] && "$HERMES_PYTHON" -c 'import hermes_cli.main' >/dev/null 2>&1; then
  pyhermes=$HERMES_PYTHON
else
  for c in "${HERMES_BIN:-}" /opt/hermes/bin/hermes /opt/hermes/.venv/bin/hermes "$home/hermes-agent/venv/bin/hermes" "$HOME/.local/bin/hermes"; do
    if [ -n "$c" ] && [ -x "$c" ]; then
      hermes=$c
      break
    fi
  done
fi
py=python3
if [ -n "$pyhermes" ]; then
  py=$pyhermes
elif [ -n "${HERMES_PYTHON:-}" ] && [ -x "$HERMES_PYTHON" ]; then
  py=$HERMES_PYTHON
elif [ -n "$hermes" ] && [ -x "$(dirname "$hermes")/python3" ]; then
  py="$(dirname "$hermes")/python3"
fi
run_hermes() {
  if [ -n "$hermes" ]; then "$hermes" "$@"; elif [ -n "$pyhermes" ]; then "$pyhermes" -m hermes_cli.main "$@"; else return 127; fi
}

# The Wakeshift it trusts, added to config.json's issuers (keeping the rest),
# worked out before anything is written.
config=""
if [ ! -f "$dir/config.json" ]; then
  printf '{\n  "issuers": [\n    "%[1]s"\n  ]\n}\n' >"$tmp/config.json"
  config="Pinned %[1]s in $dir/config.json."
elif grep -qF '"%[1]s"' "$dir/config.json"; then
  say "Kept its settings, $dir/config.json, as they were."
elif "$py" - "$dir/config.json" "%[1]s" "$tmp/config.json" <<'PY'
import json, sys
p, iss, out = sys.argv[1], sys.argv[2], sys.argv[3]
try:
    cfg = json.load(open(p))
except (OSError, ValueError) as e:
    sys.exit(p + " can't be read: " + str(e))
if not isinstance(cfg, dict) or not isinstance(cfg.get("issuers", []), list):
    sys.exit(p + ": issuers isn't a list")
cfg["issuers"] = cfg.get("issuers", []) + [iss]
open(out, "w").write(json.dumps(cfg, indent=2) + "\n")
PY
then
  config="Kept its settings, $dir/config.json, and added %[1]s to the Wakeshifts it trusts."
else
  fail 1 "Couldn't add %[1]s to the issuers in $dir/config.json: add it there, then run this again. Nothing was installed."
fi

# Each file replaced by a rename, so a file left by another user in a
# directory this user owns doesn't stop it.
mkdir -p "$dir" || fail 6 "Couldn't create $dir; nothing was installed."
for f in "$tmp/x/wakeshift/"*; do
  n=$(basename "$f")
  { cp "$f" "$dir/.$n.new" && mv -f "$dir/.$n.new" "$dir/$n"; } || fail 6 "Couldn't write $dir/$n, so the plugin there is only partly updated: run this again as the user Hermes runs as."
done
say "Wrote Wakeshift's plugin for Hermes to $dir."
if [ -f "$tmp/config.json" ]; then
  { cp "$tmp/config.json" "$dir/.config.json.new" && mv -f "$dir/.config.json.new" "$dir/config.json"; } || fail 6 "Couldn't write $dir/config.json: run this again as the user Hermes runs as."
  say "$config"
fi

if [ -n "$WAKESHIFT_CHAINED" ]; then
  next="Start a new Hermes chat (or resume one with 'hermes chat --continue'); if Hermes's gateway is running, restart it."
else
  next="Then ask the user to resume this chat (exit and run 'hermes chat --continue') or start a new one. Don't call the app's tools again just to connect: the next result of them you'd call anyway connects this conversation."
fi
if [ -n "$WAKESHIFT_QUIET" ]; then
  run_hermes plugins enable wakeshift >"$tmp/enable.log" 2>&1 && enabled=1 || enabled=""
  if [ -z "$enabled" ]; then cat "$tmp/enable.log" >&2; fi
else
  run_hermes plugins enable wakeshift && enabled=1 || enabled=""
fi
allow_key=plugins.entries.wakeshift.allow_gateway_injection
allow_cmd="hermes config set $allow_key true"
if [ -z "$enabled" ]; then
  fail 3 "Wakeshift's plugin for Hermes is written to $dir but not enabled: run 'hermes plugins enable wakeshift' where Hermes runs (in Hermes's Docker image: /opt/hermes/bin/hermes plugins enable wakeshift), then '$allow_cmd', which lets it start turns in your Hermes chats. $next"
fi
# Hermes's setting that lets the plugin start turns in its chats: the
# plugin starts none in any chat (API server or 'hermes chat') without it.
# Set unless it's set already (off: the user's choice, kept).
cur=$(run_hermes config get "$allow_key" 2>/dev/null | tail -n 1 | tr -d '[:space:]') || cur=""
case "$cur" in
true) allow="Hermes already lets it start turns in your chats ($allow_key: true)." ;;
false) allow="Left $allow_key off, as it was set: Wakeshift can't wake your Hermes chats until it's on ($allow_cmd)." ;;
*)
  if run_hermes config set "$allow_key" true >"$tmp/allow.log" 2>&1; then
    # So uninstalling removes it only if an install set it (internal/connector: HermesAllowedMarker).
    echo "set by Wakeshift's installer; uninstalling removes $allow_key" >"$dir/.allowed-gateway-injection" 2>/dev/null || true
    allow="Let it start turns in your Hermes chats ($allow_key: true), which it needs to wake any of them. It starts them only in conversations connected to Wakeshift. To turn it off: hermes config set $allow_key false."
  else
    allow="Couldn't let it start turns in your Hermes chats, which it needs to wake any of them: run '$allow_cmd' where Hermes runs."
  fi
  ;;
esac
if [ -n "$WAKESHIFT_QUIET" ]; then
  echo "Installed Wakeshift's plugin for Hermes in $dir and enabled it. $allow $next"
elif [ -n "$WAKESHIFT_CHAINED" ]; then
  echo "Enabled it in Hermes (hermes plugins enable wakeshift). $allow Hermes loads plugins when a chat starts. $next"
else
  echo "Enabled it in Hermes (hermes plugins enable wakeshift)."
  echo "$allow"
  echo "Hermes loads plugins only when a chat starts or is resumed, so Wakeshift can't wake this chat yet. Tell the user to resume it: exit and run 'hermes chat --continue' (through Hermes's gateway or API server, restart the gateway and continue the session), then send any message. A new chat works too. Don't call the app's tools again just to connect: the next result of them you'd call anyway connects this conversation."
fi
WAKESHIFT_BODY

# Root on a Hermes home that's another user's (Hermes's Docker image, through
# docker exec): run as that user, as Hermes's own command does, so the
# gateway can update what's written. WAKESHIFT_AS_ROOT=1 stays root.
if [ "$(id -u)" = 0 ] && [ -z "${WAKESHIFT_AS_ROOT:-}" ] && [ -d "$home" ]; then
  owner=$(stat -c %%U "$home" 2>/dev/null || stat -f %%Su "$home" 2>/dev/null || true)
  if [ -n "$owner" ] && [ "$owner" != root ]; then
    if [ "$owner" = UNKNOWN ] || ! id "$owner" >/dev/null 2>&1; then
      echo "$home belongs to a user with no account here, so this won't install as root; nothing was installed. Run it as the user Hermes runs as." >&2
      exit 6
    fi
    if [ -x /command/s6-setuidgid ]; then
      as="/command/s6-setuidgid $owner"
    elif command -v setpriv >/dev/null 2>&1; then
      as="setpriv --reuid=$(id -u "$owner") --regid=$(id -g "$owner") --init-groups"
    elif command -v runuser >/dev/null 2>&1; then
      as="runuser -u $owner --"
    else
      echo "$home belongs to $owner: run this as $owner (with Docker: docker exec -u $owner), or set WAKESHIFT_AS_ROOT=1 to install as root. Nothing was installed." >&2
      exit 6
    fi
    # A plugin an earlier run left root's is handed to its user: the
    # directory and what's directly in it. $owner controls the Hermes home,
    # so nothing is followed there: the plugin's directory and plugins/ may
    # not be links, and the directory must be the Hermes home's own (its
    # real path). It's done from inside the directory (a link swapped into
    # the path meanwhile changes nothing) and only if the directory isn't
    # $owner's yet (they can't change what's in it until it's theirs):
    # what's directly in it first, the directory itself last, never a link
    # (chown -h changes a link, not its target) or a file with another hard
    # link (another file's).
    dir="$home/plugins/wakeshift"
    if [ -d "$dir" ] && [ ! -L "$home/plugins" ] && [ ! -L "$dir" ]; then
      (
        real=$(cd -P "$home" 2>/dev/null && pwd -P) || exit 0
        cd -P "$dir" 2>/dev/null || exit 0
        [ "$(pwd -P)" = "$real/plugins/wakeshift" ] || exit 0
        [ -z "$(find . -maxdepth 0 -user "$owner" -print 2>/dev/null)" ] || exit 0
        grep -q "%[4]s" ./__init__.py 2>/dev/null || exit 0
        find . -maxdepth 1 -depth \( -type d -o -type f -links 1 \) -exec chown -h "$owner:$(id -g "$owner")" {} + 2>/dev/null || true
      )
    fi
    HERMES_HOME=$home
    export HERMES_HOME
    # The user's own HOME, where getent can say it (it's only a default).
    h=""
    if command -v getent >/dev/null 2>&1; then
      h=$(getent passwd "$owner" | cut -d: -f6) || h=""
    fi
    if [ -n "$h" ]; then HOME=$h && export HOME; fi
    if [ -z "$quiet" ]; then echo "Installing as $owner, who owns $home."; fi
    # shellcheck disable=SC2086 # $as is words on purpose
    exec $as sh -c "$body"
  fi
fi
exec sh -c "$body"
}
main "$@"
`, base, HermesPlugin, sum, hermes.Marker, s.fileURL("download/hermes/"+HermesPlugin, HermesPlugin), HermesExitVerify)
}
