package auth

import (
	"net/http/httptest"
	"testing"
)

// The console's own requests pass even when the browser withholds Origin
// (Referrer-Policy no-referrer sends "null"), as long as Sec-Fetch-Site
// says same-origin; anything else stays refused.
func TestConsoleSameOrigin(t *testing.T) {
	c := &Console{PublicURL: "https://app.example.test"}
	for _, tc := range []struct {
		origin, site string
		ok           bool
	}{
		{"https://app.example.test", "", true},
		{"null", "same-origin", true},
		{"", "same-origin", true},
		{"null", "cross-site", false},
		{"null", "", false},
		{"", "", false},
		{"https://evil.example", "same-origin", false},
	} {
		r := httptest.NewRequest("POST", "/api/v1/x", nil)
		if tc.origin != "" {
			r.Header.Set("Origin", tc.origin)
		}
		if tc.site != "" {
			r.Header.Set("Sec-Fetch-Site", tc.site)
		}
		if got := c.sameOrigin(r); got != tc.ok {
			t.Errorf("Origin %q, Sec-Fetch-Site %q: %v, want %v", tc.origin, tc.site, got, tc.ok)
		}
	}
}
