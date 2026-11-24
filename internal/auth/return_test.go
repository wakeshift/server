package auth

import "testing"

func TestSafeReturn(t *testing.T) {
	for in, want := range map[string]string{
		"/agents":            "/agents",
		"/oauth/authorize?x": "/oauth/authorize?x",
		"//evil.com":         "/",
		"/\t/evil.com":       "/", // browsers drop the tab: //evil.com
		"/\n/evil.com":       "/",
		"/\\evil.com":        "/",
		"https://evil.com":   "/",
		"/\x7f":              "/",
	} {
		if got := safeReturn(in); got != want {
			t.Errorf("safeReturn(%q) = %q, want %q", in, got, want)
		}
	}
}
