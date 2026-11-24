package auth_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wakeshift/server/internal/accounts"
	"github.com/wakeshift/server/internal/auth"
	"github.com/wakeshift/server/internal/dbtest"
)

// The session tells the console where its links go, as the server is
// configured: the site's docs (Wakeshift's without a site), the Terms and
// Privacy Policy only where there are any, and the support address
// (today's by default).
func TestSessionSite(t *testing.T) {
	d := dbtest.New(t)
	type site struct {
		Docs         string  `json:"docs"`
		Terms        *string `json:"terms"`
		Privacy      *string `json:"privacy"`
		SupportEmail string  `json:"support_email"`
	}
	get := func(c *auth.Console) site {
		t.Helper()
		c.Key, c.Accounts, c.PublicURL = []byte(strings.Repeat("k", 32)), &accounts.Store{Pool: d.Pool}, "https://app.example.test"
		mux := http.NewServeMux()
		c.Register(mux)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), "GET", "/api/v1/session", nil))
		var out struct {
			Site *site `json:"site"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || rec.Code != 200 || out.Site == nil {
			t.Fatalf("status %d: %s (%v)", rec.Code, rec.Body, err)
		}
		return *out.Site
	}

	// A server with nothing configured: Wakeshift's docs, no Terms.
	s := get(&auth.Console{})
	if s.Docs != "https://wakeshift.ai/docs" || s.Terms != nil || s.Privacy != nil || s.SupportEmail != "support@wakeshift.ai" {
		t.Fatalf("unconfigured: %+v", s)
	}
	// Wakeshift's hosted service, as internal/config fills it in.
	s = get(&auth.Console{SiteURL: "https://wakeshift.ai", TermsURL: "https://wakeshift.ai/terms", PrivacyURL: "https://wakeshift.ai/privacy", SupportEmail: "support@wakeshift.ai"})
	if s.Docs != "https://wakeshift.ai/docs" || s.Terms == nil || *s.Terms != "https://wakeshift.ai/terms" || s.Privacy == nil || *s.Privacy != "https://wakeshift.ai/privacy" {
		t.Fatalf("hosted: %+v", s)
	}
	s = get(&auth.Console{SiteURL: "https://staging.wakeshift.ai"})
	if s.Docs != "https://staging.wakeshift.ai/docs" {
		t.Fatalf("staging: %+v", s)
	}
	// Someone else's.
	s = get(&auth.Console{SiteURL: "https://wake.example.com", TermsURL: "https://example.com/terms", SupportEmail: "help@example.com"})
	if s.Docs != "https://wake.example.com/docs" || s.Terms == nil || *s.Terms != "https://example.com/terms" || s.Privacy != nil || s.SupportEmail != "help@example.com" {
		t.Fatalf("self-hosted: %+v", s)
	}
}
