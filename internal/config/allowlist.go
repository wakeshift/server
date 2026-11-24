package config

import (
	"errors"
	"strings"

	"github.com/wakeshift/server/internal/apps"
)

// Allowlist is a feature setting that's off, on for every app, or on for
// some apps by ID: "off" (or empty), "all", or a comma-separated list.
type Allowlist struct {
	all bool
	ids map[string]bool
}

// ParseAllowlist reads an Allowlist setting.
func ParseAllowlist(v string) (Allowlist, error) {
	v = strings.TrimSpace(v)
	switch v {
	case "", "off":
		return Allowlist{}, nil
	case "all":
		return Allowlist{all: true}, nil
	case "on", "true", "yes":
		// Meant as "all", which says which apps.
		return Allowlist{}, errors.New(`must be "off", "all" or a comma-separated list of app IDs, not ` + quote(v))
	}
	a := Allowlist{ids: map[string]bool{}}
	for _, id := range strings.Split(v, ",") {
		id = strings.TrimSpace(id)
		if !apps.ValidID(id) {
			return Allowlist{}, errors.New(`must be "off", "all" or a comma-separated list of app IDs, not ` + quote(id))
		}
		a.ids[id] = true
	}
	return a, nil
}

// Allows says whether the setting is on for app.
func (a Allowlist) Allows(app string) bool { return a.all || a.ids[app] }

// AllowlistOf is an Allowlist for these apps ("all" for every app), for tests.
func AllowlistOf(ids ...string) Allowlist {
	a := Allowlist{ids: map[string]bool{}}
	for _, id := range ids {
		if id == "all" {
			a.all = true
		}
		a.ids[id] = true
	}
	return a
}

func quote(s string) string {
	if len(s) > 40 {
		s = s[:40] + "…"
	}
	return `"` + s + `"`
}
