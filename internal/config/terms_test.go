package config

import (
	"testing"

	"github.com/wakeshift/server/internal/accounts"
)

// The Terms (and Privacy Policy) apply only where they're configured, or on
// Wakeshift's hosted service, whose defaults are today's: its site's /terms
// and /privacy and accounts.TermsVersion. Production and staging set only
// WAKESHIFT_SITE_URL, so nothing changes there.
func TestTerms(t *testing.T) {
	hosted := Config{TermsURL: "https://wakeshift.ai/terms", PrivacyURL: "https://wakeshift.ai/privacy", TermsVersion: accounts.TermsVersion}
	for name, c := range map[string]struct {
		env  map[string]string
		want Config
		ok   bool
	}{
		"production":                      {map[string]string{"WAKESHIFT_PUBLIC_URL": "https://app.wakeshift.ai", "WAKESHIFT_SITE_URL": "https://wakeshift.ai"}, hosted, true},
		"production, its site by default": {map[string]string{"WAKESHIFT_PUBLIC_URL": "https://app.wakeshift.ai"}, hosted, true},
		"staging": {map[string]string{"WAKESHIFT_PUBLIC_URL": "https://app.staging.wakeshift.ai", "WAKESHIFT_SITE_URL": "https://staging.wakeshift.ai"},
			Config{TermsURL: "https://staging.wakeshift.ai/terms", PrivacyURL: "https://staging.wakeshift.ai/privacy", TermsVersion: accounts.TermsVersion}, true},
		"self-hosted":                     {map[string]string{"WAKESHIFT_PUBLIC_URL": "https://wake.example.com"}, Config{}, true},
		"self-hosted at app.":             {map[string]string{"WAKESHIFT_PUBLIC_URL": "https://app.example.com"}, Config{}, true},
		"production, another site URL":    {map[string]string{"WAKESHIFT_PUBLIC_URL": "https://app.wakeshift.ai", "WAKESHIFT_SITE_URL": "https://www.example.com"}, hosted, true},
		"another wakeshift.ai host":       {map[string]string{"WAKESHIFT_PUBLIC_URL": "https://app.preview.wakeshift.ai"}, Config{}, true},
		"a look-alike domain":             {map[string]string{"WAKESHIFT_PUBLIC_URL": "https://app.notwakeshift.ai"}, Config{}, true},
		"localhost":                       {map[string]string{"WAKESHIFT_PUBLIC_URL": "http://localhost:8370"}, Config{}, true},
		"localhost with Wakeshift's site": {map[string]string{"WAKESHIFT_PUBLIC_URL": "http://localhost:8370", "WAKESHIFT_SITE_URL": "https://staging.wakeshift.ai"}, Config{}, true},
		"self-hosted with Terms": {map[string]string{"WAKESHIFT_PUBLIC_URL": "https://wake.example.com", "WAKESHIFT_TERMS_URL": "https://example.com/legal/terms",
			"WAKESHIFT_TERMS_VERSION": "2026-01-01", "WAKESHIFT_PRIVACY_URL": "https://example.com/legal/privacy"},
			Config{TermsURL: "https://example.com/legal/terms", PrivacyURL: "https://example.com/legal/privacy", TermsVersion: "2026-01-01"}, true},
		"Terms without a privacy policy": {map[string]string{"WAKESHIFT_PUBLIC_URL": "https://wake.example.com", "WAKESHIFT_TERMS_URL": "https://example.com/terms", "WAKESHIFT_TERMS_VERSION": "v3"},
			Config{TermsURL: "https://example.com/terms", TermsVersion: "v3"}, true},
		"hosted, other Terms": {map[string]string{"WAKESHIFT_PUBLIC_URL": "https://app.wakeshift.ai", "WAKESHIFT_TERMS_URL": "https://wakeshift.ai/terms-v2", "WAKESHIFT_TERMS_VERSION": "v2"},
			Config{TermsURL: "https://wakeshift.ai/terms-v2", TermsVersion: "v2"}, true},
		"a URL without a version": {map[string]string{"WAKESHIFT_TERMS_URL": "https://example.com/terms"}, Config{}, false},
		"a version without a URL": {map[string]string{"WAKESHIFT_TERMS_VERSION": "v1"}, Config{}, false},
		"privacy without Terms":   {map[string]string{"WAKESHIFT_PRIVACY_URL": "https://example.com/privacy"}, Config{}, false},
		"Terms over http":         {map[string]string{"WAKESHIFT_TERMS_URL": "http://example.com/terms", "WAKESHIFT_TERMS_VERSION": "v1"}, Config{}, false},
		"Terms that aren't a URL": {map[string]string{"WAKESHIFT_TERMS_URL": "/terms", "WAKESHIFT_TERMS_VERSION": "v1"}, Config{}, false},
		"Terms with no host":      {map[string]string{"WAKESHIFT_TERMS_URL": "https:terms", "WAKESHIFT_TERMS_VERSION": "v1"}, Config{}, false},
		// url.Parse lowercases the scheme, so its case doesn't matter.
		"an uppercase scheme": {map[string]string{"WAKESHIFT_TERMS_URL": "HTTPS://example.com/terms", "WAKESHIFT_TERMS_VERSION": "v1"},
			Config{TermsURL: "HTTPS://example.com/terms", TermsVersion: "v1"}, true},
		"uppercase plain http":     {map[string]string{"WAKESHIFT_TERMS_URL": "HTTP://example.com/terms", "WAKESHIFT_TERMS_VERSION": "v1"}, Config{}, false},
		"a privacy URL that's not": {map[string]string{"WAKESHIFT_TERMS_URL": "https://example.com/terms", "WAKESHIFT_TERMS_VERSION": "v1", "WAKESHIFT_PRIVACY_URL": "javascript:alert(1)"}, Config{}, false},
	} {
		t.Run(name, func(t *testing.T) {
			for k, v := range map[string]string{"DATABASE_URL": "postgres://x", "WAKESHIFT_SECRET_KEY": "0123456789abcdef0123456789abcdef-random",
				"WAKESHIFT_PUBLIC_URL": "https://wakeshift.dev", "WAKESHIFT_SITE_URL": "", "WAKESHIFT_TERMS_URL": "", "WAKESHIFT_TERMS_VERSION": "",
				"WAKESHIFT_PRIVACY_URL": "", "WAKESHIFT_SUPPORT_EMAIL": "", "WAKESHIFT_DEV_SIGN_IN": "", "K_SERVICE": ""} {
				t.Setenv(k, v)
			}
			for k, v := range c.env {
				t.Setenv(k, v)
			}
			got, err := FromEnv()
			if (err == nil) != c.ok {
				t.Fatalf("err = %v", err)
			}
			if err != nil {
				return
			}
			if got.TermsURL != c.want.TermsURL || got.PrivacyURL != c.want.PrivacyURL || got.TermsVersion != c.want.TermsVersion {
				t.Fatalf("terms %q, privacy %q, version %q; want %q, %q, %q", got.TermsURL, got.PrivacyURL, got.TermsVersion, c.want.TermsURL, c.want.PrivacyURL, c.want.TermsVersion)
			}
		})
	}
}

// WAKESHIFT_SUPPORT_EMAIL: today's address by default, else an address.
func TestSupportEmail(t *testing.T) {
	for k, v := range map[string]string{"DATABASE_URL": "postgres://x", "WAKESHIFT_SECRET_KEY": "0123456789abcdef0123456789abcdef-random",
		"WAKESHIFT_PUBLIC_URL": "https://wakeshift.dev", "WAKESHIFT_DEV_SIGN_IN": "", "K_SERVICE": ""} {
		t.Setenv(k, v)
	}
	for in, want := range map[string]string{"": "support@wakeshift.ai", " help@example.com ": "help@example.com", "ops+wake@example.org": "ops+wake@example.org"} {
		t.Setenv("WAKESHIFT_SUPPORT_EMAIL", in)
		got, err := FromEnv()
		if err != nil || got.SupportEmail != want {
			t.Errorf("%q: %q (%v), want %q", in, got.SupportEmail, err, want)
		}
	}
	for _, bad := range []string{"support", "@example.com", "help@", "Help <help@example.com>", "a@b.com, c@d.com"} {
		t.Setenv("WAKESHIFT_SUPPORT_EMAIL", bad)
		if _, err := FromEnv(); err == nil {
			t.Errorf("%q: accepted", bad)
		}
	}
}
