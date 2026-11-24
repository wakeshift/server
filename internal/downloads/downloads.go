// Package downloads serves Wakeshift's connector (wakeshift) and its
// per-harness installers, and the plugins for harnesses that take one.
// (An earlier desktop app's /install/mac.sh and /download/wakeshift-mac.zip
// are gone.)
//
// The connector (cmd/wakeshift-connector) is built into the server's image,
// one binary per platform in ConnectorDir (wakeshift-darwin-arm64, …);
// /install/<harness>.sh fetches the right one into
// ~/.wakeshift/bin/wakeshift, checks its SHA-256, and runs `wakeshift
// install --harness <harness>`, which adds that harness's hooks
// (internal/harnesses says which harnesses have one).
//
// The same scripts, pinned to production's URL and fetching from a release
// of github.com/wakeshift/connectors instead of this server, are what
// WriteRelease writes for CI to sign and publish: those are the ones a
// compromised server can't change.
package downloads

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/wakeshift/server/internal/harnesses"
)

// Server serves the downloads.
type Server struct {
	PublicURL string
	// ConnectorDir holds the connector, built per platform
	// (wakeshift-darwin-arm64, …); "" when it isn't built here.
	ConnectorDir string
	// ReleaseURL, when set, is where the scripts fetch everything they
	// download or name (the connector, the plugins, the other scripts), by
	// file name: a release's download URL, for the scripts WriteRelease
	// writes. "" means this server's own /download and /install paths.
	ReleaseURL string

	sumsOnce sync.Once
	sums     map[string]string // binary name → SHA-256

	pluginOnce sync.Once
	pluginSum  string // OpenClawPlugin's SHA-256

	hermesOnce sync.Once
	hermesTar  []byte // HermesPlugin
	hermesSum  string // its SHA-256
}

// ExitVerify is what every /install script exits with when something it
// downloaded doesn't match the checksum the script carries: nothing was
// installed (site/docs/hermes.md lists it with Hermes's other codes).
const ExitVerify = 7

// Connector platforms, as uname reports them after normalizing.
var connectorPlatforms = []string{"darwin-arm64", "darwin-amd64", "linux-arm64", "linux-amd64"}

// Install is how to get the connector a session needs.
type Install struct {
	Command  string `json:"command"`  // for the user to run (or an agent, with their agreement)
	Download string `json:"download"` // the app itself
	Help     string `json:"help"`     // where to connect and manage it
}

// HarnessInstall is the install instructions for Wakeshift's hooks for a
// harness (an internal/harnesses ID, e.g. "codex").
func (s *Server) HarnessInstall(harness string) Install {
	h, _ := harnesses.Get(harness)
	return Install{Command: harnesses.InstallCommand(h, s.PublicURL), Help: harnesses.ScriptURL(h, s.PublicURL)}
}

// Routes are the mux patterns to serve.
func (s *Server) Routes() map[string]http.Handler {
	return map[string]http.Handler{
		"GET /download/wakeshift/{platform}":       http.HandlerFunc(s.connectorDownload),
		"GET /install/{script}":                    http.HandlerFunc(s.harnessScript),
		"GET /download/openclaw/" + OpenClawPlugin: http.HandlerFunc(s.openclawDownload),
		"GET /install/openclaw-uninstall.sh":       http.HandlerFunc(s.openclawUninstallScript),
		"GET /download/hermes/" + HermesPlugin:     http.HandlerFunc(s.hermesDownload),
	}
}

// connectorCases are a shell case's branches selecting the build for
// "$platform" (as uname reports it, normalized as the scripts do) and its
// SHA-256 into want, one per platform built here; sep ends each branch.
// The install scripts and Grok Bot's install command share them.
func (s *Server) connectorCases(prefix, sep string) string {
	var cases strings.Builder
	for _, p := range connectorPlatforms {
		if sum, ok := s.connectorSums()[p]; ok {
			fmt.Fprintf(&cases, "%s%s) want=%s ;;%s", prefix, p, sum, sep)
		}
	}
	return cases.String()
}

// connectorUname sets platform from uname, as the install scripts do;
// unknown machines get arch "" (no case matches).
const connectorUname = `os=$(uname -s | tr '[:upper:]' '[:lower:]'); case "$(uname -m)" in arm64|aarch64) arch=arm64 ;; x86_64|amd64) arch=amd64 ;; *) arch= ;; esac; platform="$os-$arch"`

// GrokInstallCommand is Grok Bot's install of the connector: one line an
// agent runs as written (harnesses.GrokKeyNote), doing what
// /install/<harness>.sh does up to its `wakeshift install` (the same
// platforms, URL and checksums, the binary at ~/.wakeshift/bin/wakeshift),
// without piping a script into a shell. Once the binary matches it only
// says so. When it can't install a matching one it exits non-zero, so the
// agent goes no further in that chat (harnesses.GrokKeyNote), and it
// leaves an earlier release's connector in place: one an earlier run of
// this command installed and checked, which the routine's runs may still
// be using. "" when the connector isn't built here.
func (s *Server) GrokInstallCommand() string {
	if len(s.connectorSums()) == 0 {
		return ""
	}
	sum := func(f string) string {
		return `$( (sha256sum "` + f + `" 2>/dev/null || shasum -a 256 "` + f + `") | cut -d' ' -f1)`
	}
	return connectorUname + `; case "$platform" in ` + s.connectorCases("", " ") + `*) want= ;; esac; b="$HOME/.wakeshift/bin/wakeshift"; ` +
		`if [ -z "$want" ]; then echo "Wakeshift's connector isn't available for this machine."; false; ` +
		`elif [ -x "$b" ] && [ "` + sum("$b") + `" = "$want" ]; then echo "Wakeshift's connector is installed."; ` +
		`else mkdir -p "$HOME/.wakeshift/bin" && chmod 700 "$HOME/.wakeshift" && t=$(mktemp "$HOME/.wakeshift/bin/.wakeshift.XXXXXX") && ` +
		`if curl -fsSL "` + s.fileURL("download/wakeshift/$platform", "wakeshift-$platform") + `" -o "$t" && [ "` + sum("$t") + `" = "$want" ]; ` +
		`then chmod 755 "$t" && mv "$t" "$b" && echo "Wakeshift's connector is installed."; ` +
		`else rm -f "$t"; echo "The download failed or didn't match its checksum; nothing was installed."; false; fi; fi`
}

// connectorSums hashes the built binaries once.
func (s *Server) connectorSums() map[string]string {
	s.sumsOnce.Do(func() {
		s.sums = map[string]string{}
		if s.ConnectorDir == "" {
			return
		}
		for _, p := range connectorPlatforms {
			b, err := os.ReadFile(filepath.Join(s.ConnectorDir, "wakeshift-"+p))
			if err != nil {
				continue
			}
			sum := sha256.Sum256(b)
			s.sums[p] = hex.EncodeToString(sum[:])
		}
	})
	return s.sums
}

func (s *Server) connectorDownload(w http.ResponseWriter, r *http.Request) {
	p := r.PathValue("platform")
	if _, ok := s.connectorSums()[p]; !ok {
		http.Error(w, "Wakeshift's connector isn't built for "+p+" on this server", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="wakeshift"`)
	http.ServeFile(w, r, filepath.Join(s.ConnectorDir, "wakeshift-"+p))
}

// harnessNameRE is a harness name in /install/<harness>.sh.
var harnessNameRE = regexp.MustCompile(`^[a-z][a-z0-9-]{1,30}$`)

// harnessScript serves /install/<harness>.sh: it fetches the connector
// binary into ~/.wakeshift/bin/wakeshift,
// checking its SHA-256, and runs `wakeshift install --harness <harness>`,
// for harnesses with an install (internal/harnesses).
func (s *Server) harnessScript(w http.ResponseWriter, r *http.Request) {
	harness, ok := strings.CutSuffix(r.PathValue("script"), ".sh")
	h, known := harnesses.Get(harness)
	if !ok || !harnessNameRE.MatchString(harness) || !known || !h.Installable {
		http.NotFound(w, r)
		return
	}
	switch {
	case h.Plugin: // a plugin the harness installs itself (OpenClaw is the only one)
		s.openclawScript(w)
		return
	case h.ID == "hermes": // a plugin in Python, with no hook command: no binary
		s.hermesScript(w)
		return
	}
	base := strings.TrimSuffix(s.PublicURL, "/")
	connector := s.fileURL("download/wakeshift/$platform", "wakeshift-$platform")
	cases := s.connectorCases("  ", "\n")
	w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	fmt.Fprintf(w, `#!/bin/sh
# Installs Wakeshift's hooks for %[3]s, so Wakeshift messages can reach your
# %[3]s conversations. It puts wakeshift in ~/.wakeshift/bin and adds its hooks
# to %[3]s's settings (keeping a backup; other settings are left alone).
# Remove them with: ~/.wakeshift/bin/wakeshift uninstall --harness %[3]s
# It exits %[5]d if the download doesn't match the checksum below (nothing is
# installed), and 1 on any other failure.
set -eu
os=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$(uname -m)" in
  arm64|aarch64) arch=arm64 ;;
  x86_64|amd64) arch=amd64 ;;
  *) echo "Wakeshift's connector isn't built for $(uname -m)" >&2; exit 1 ;;
esac
platform="$os-$arch"
case "$platform" in
%[2]s  *) echo "Wakeshift's connector isn't available for $platform here" >&2; exit 1 ;;
esac
mkdir -p "$HOME/.wakeshift/bin"
chmod 700 "$HOME/.wakeshift"
tmp=$(mktemp "$HOME/.wakeshift/bin/.wakeshift.XXXXXX")
trap 'rm -f "$tmp"' EXIT
curl -fsSL "%[4]s" -o "$tmp"
got=$( (shasum -a 256 "$tmp" 2>/dev/null || sha256sum "$tmp") | cut -d' ' -f1)
if [ "$got" != "$want" ]; then
  echo "The download didn't match its checksum; nothing was installed." >&2
  exit %[5]d
fi
chmod 755 "$tmp"
mv "$tmp" "$HOME/.wakeshift/bin/wakeshift"
trap - EXIT
"$HOME/.wakeshift/bin/wakeshift" install --harness %[3]s --issuer "%[1]s" --yes
`, base, cases, harness, connector, ExitVerify)
}

// fileURL is where a script fetches, or tells the user to fetch, one of
// the files it names: path on this server (download/… or install/…), or
// name in ReleaseURL.
func (s *Server) fileURL(path, name string) string {
	if s.ReleaseURL != "" {
		return strings.TrimSuffix(s.ReleaseURL, "/") + "/" + name
	}
	return strings.TrimSuffix(s.PublicURL, "/") + "/" + path
}
