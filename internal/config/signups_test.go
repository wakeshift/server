package config

import "testing"

// Sign-up is closed on a self-hosted server unless WAKESHIFT_OPEN_SIGNUPS=1,
// and stays as it was on the hosted service (by its public URL).
func TestSignupsClosed(t *testing.T) {
	for _, c := range []struct {
		url    string
		open   bool
		closed bool
	}{
		{"https://app.wakeshift.ai", false, false},
		{"https://app.staging.wakeshift.ai", false, false},
		{"https://app.wakeshift.ai", true, false},
		{"https://wakeshift.example.com", false, true},
		{"https://wakeshift.example.com", true, false},
		{"http://localhost:8370", false, true},
		{"https://app.wakeshift.ai.evil.example", false, true},
	} {
		if got := (Config{PublicURL: c.url, OpenSignups: c.open}).SignupsClosed(); got != c.closed {
			t.Errorf("%s, open %v: closed %v, want %v", c.url, c.open, got, c.closed)
		}
	}
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("WAKESHIFT_SECRET_KEY", "0123456789abcdef0123456789abcdef-random")
	t.Setenv("WAKESHIFT_PUBLIC_URL", "https://wakeshift.example.com")
	t.Setenv("WAKESHIFT_OPEN_SIGNUPS", "1")
	if c, err := FromEnv(); err != nil || !c.OpenSignups || c.SignupsClosed() {
		t.Fatalf("WAKESHIFT_OPEN_SIGNUPS=1: %+v %v", c.OpenSignups, err)
	}
}
