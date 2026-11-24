package downloads

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/wakeshift/server/internal/harnesses"
)

// SumsFile is a release's list of SHA-256 checksums, in the format
// `sha256sum -c` reads. CI signs it.
const SumsFile = "SHA256SUMS"

// WriteRelease writes a release of the installers into ConnectorDir, which
// already holds the connector for every platform and the OpenClaw plugin:
// every /install script (<harness>.sh, openclaw-uninstall.sh), the Hermes
// plugin they install, and SumsFile over all of it.
//
// The scripts are the ones this server serves, but they fetch and name
// files in ReleaseURL, where the release is published, and pin PublicURL as
// their issuer. Each carries the SHA-256 of what it downloads, so a script
// fetched from the release, never from the server, is checked against
// nothing the server can change.
func (s *Server) WriteRelease() ([]string, error) {
	if s.ConnectorDir == "" {
		return nil, errors.New("downloads: a release needs ConnectorDir")
	}
	for _, u := range []string{s.PublicURL, s.ReleaseURL} {
		p, err := url.Parse(u)
		ok := err == nil && p.Host != "" && p.RawQuery == "" && p.Fragment == "" && p.User == nil &&
			(p.Scheme == "https" || p.Scheme == "http" && (p.Hostname() == "127.0.0.1" || p.Hostname() == "localhost"))
		if !ok {
			return nil, fmt.Errorf("downloads: a release needs https URLs with no query (PublicURL and ReleaseURL), not %q", u)
		}
	}
	for _, p := range connectorPlatforms {
		if _, ok := s.connectorSums()[p]; !ok {
			return nil, fmt.Errorf("downloads: a release needs the connector for every platform; wakeshift-%s is missing", p)
		}
	}
	if s.openclawPluginSum() == "" {
		return nil, errors.New("downloads: a release needs the OpenClaw plugin, " + OpenClawPlugin)
	}
	tar, _ := s.hermesPack()
	if tar == nil {
		return nil, errors.New("downloads: the Hermes plugin couldn't be packed")
	}
	if err := os.WriteFile(filepath.Join(s.ConnectorDir, HermesPlugin), tar, 0o644); err != nil {
		return nil, err
	}

	mux := http.NewServeMux()
	for p, h := range s.Routes() {
		mux.Handle(p, h)
	}
	scripts := []string{"openclaw-uninstall.sh"}
	for _, h := range harnesses.All() {
		if h.Installable {
			scripts = append(scripts, h.ID+".sh")
		}
	}
	for _, name := range scripts {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest("GET", "/install/"+name, nil))
		if rec.Code != http.StatusOK {
			return nil, fmt.Errorf("downloads: /install/%s: %d %s", name, rec.Code, strings.TrimSpace(rec.Body.String()))
		}
		if err := os.WriteFile(filepath.Join(s.ConnectorDir, name), rec.Body.Bytes(), 0o644); err != nil {
			return nil, err
		}
	}

	entries, err := os.ReadDir(s.ConnectorDir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.Type().IsRegular() && e.Name() != SumsFile {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	var sums strings.Builder
	for _, n := range names {
		b, err := os.ReadFile(filepath.Join(s.ConnectorDir, n))
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(b)
		fmt.Fprintf(&sums, "%s  %s\n", hex.EncodeToString(sum[:]), n)
	}
	if err := os.WriteFile(filepath.Join(s.ConnectorDir, SumsFile), []byte(sums.String()), 0o644); err != nil {
		return nil, err
	}
	return append(names, SumsFile), nil
}
