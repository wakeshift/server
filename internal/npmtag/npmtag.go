// Package npmtag finds the version an npm dist-tag points at, for the
// install offers that name Wakeshift's npm plugin at an exact version
// (internal/harnesses, Harness.Package): opencode caches a plugin by the
// spec in its config, so `@wakeshift/opencode@latest` would never update,
// and the offer pins the version `latest` (or another Wakeshift's own
// dist-tag) points at instead.
//
// Only versions already published are offered this way: a release's
// packages are published after its server deploys (the installers' release
// waits for an approval), and the previous version keeps being offered
// until then.
package npmtag

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Registry is npm's public registry.
const Registry = "https://registry.npmjs.org"

// TagRE is a dist-tag name this package resolves.
var TagRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,30}$`)

var nameRE = regexp.MustCompile(`^(@[a-z0-9-]+/)?[a-z0-9][a-z0-9._-]{0,100}$`)

// Resolve returns the version pkg's dist-tag points at in registry.
func Resolve(ctx context.Context, hc *http.Client, registry, pkg, tag string) (string, error) {
	if !nameRE.MatchString(pkg) || !TagRE.MatchString(tag) {
		return "", errors.New("npmtag: not a package and dist-tag")
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	u := strings.TrimSuffix(registry, "/") + "/-/package/" + url.PathEscape(pkg) + "/dist-tags"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("npmtag: %s's dist-tags: %s", pkg, resp.Status)
	}
	var tags map[string]string
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&tags); err != nil {
		return "", fmt.Errorf("npmtag: %s's dist-tags: %w", pkg, err)
	}
	v, ok := tags[tag]
	if !ok {
		return "", fmt.Errorf("npmtag: %s has no dist-tag %q", pkg, tag)
	}
	return v, nil
}

// Watch resolves pkg's dist-tag now and then every interval until ctx ends,
// calling set with each version it finds that valid accepts. A failed
// lookup keeps the version it had (set isn't called).
func Watch(ctx context.Context, hc *http.Client, registry, pkg, tag string, every time.Duration, valid func(string) bool, set func(string)) {
	last := ""
	for {
		v, err := Resolve(ctx, hc, registry, pkg, tag)
		switch {
		case err != nil:
			if ctx.Err() == nil {
				slog.Warn("npmtag: couldn't look up the version to offer", "package", pkg, "tag", tag, "err", err)
			}
		case !valid(v):
			slog.Warn("npmtag: the dist-tag points at a version that can't be offered", "package", pkg, "tag", tag)
		case v != last:
			last = v
			set(v)
			slog.Info("npmtag: offering", "package", pkg, "version", v)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(every):
		}
	}
}
