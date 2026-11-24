package auth_test

import (
	"bytes"
	"log/slog"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// signupRef is the ref saved on the account with this email ("" for NULL).
func signupRef(t *testing.T, pool *pgxpool.Pool, email string) string {
	t.Helper()
	var ref *string
	if err := pool.QueryRow(t.Context(), `SELECT signup_ref FROM users WHERE email = $1`, email).Scan(&ref); err != nil {
		t.Fatal(err)
	}
	if ref == nil {
		return ""
	}
	return *ref
}

// captureLogs sends everything logged, at every level, to a buffer for the
// rest of the test.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// A sign-in link's ?ref= rides in the flow cookie to the account it makes,
// lowercased, and is never logged. An existing user's sign-in doesn't
// change it.
func TestRefSavedOnTheNewAccount(t *testing.T) {
	logs := captureLogs(t)
	a := setup(t, "@example.com")
	a.start = "?return_to=%2Fapps&ref=RefCanary7"
	if to := a.signIn(t, person, nil); to != "/apps" {
		t.Fatalf("redirected to %s", to)
	}
	if got := signupRef(t, a.accounts.Pool, "ada@example.com"); got != "refcanary7" {
		t.Fatalf("signup_ref = %q", got)
	}
	a.client.Jar, _ = cookiejar.New(nil)
	a.start = "?ref=other"
	a.signIn(t, person, nil)
	if got := signupRef(t, a.accounts.Pool, "ada@example.com"); got != "refcanary7" {
		t.Fatalf("an existing user's sign-in changed signup_ref to %q", got)
	}
	// A failed sign-in, with the ref, logs neither it nor the start URL.
	a.client.Jar, _ = cookiejar.New(nil)
	a.start = "?ref=refcanary7"
	a.signIn(t, person, func(q url.Values) { q.Set("state", "forged") })
	if strings.Contains(strings.ToLower(logs.String()), "refcanary7") {
		t.Fatalf("the ref was logged:\n%s", logs)
	}
	if !strings.Contains(logs.String(), "sign-in failed") {
		t.Fatal("nothing was logged; the check above proves nothing")
	}
}

// Anything but a short tag is dropped: the account is made with no ref.
func TestRefValidation(t *testing.T) {
	for i, ref := range []string{"", "a@b.example", "has space", "https://evil.example/", "-leading", strings.Repeat("a", 33), "café", "a.b", "a/b", "x\n"} {
		a := setup(t, "@example.com")
		if ref != "" {
			a.start = "?" + url.Values{"ref": {ref}}.Encode()
		}
		a.signIn(t, person, nil)
		if got := signupRef(t, a.accounts.Pool, "ada@example.com"); got != "" {
			t.Errorf("%d: ref %q saved as %q", i, ref, got)
		}
	}
	a := setup(t, "@example.com")
	a.start = "?ref=" + strings.Repeat("a", 32)
	a.signIn(t, person, nil)
	if got := signupRef(t, a.accounts.Pool, "ada@example.com"); got != strings.Repeat("a", 32) {
		t.Errorf("a 32-character ref saved as %q", got)
	}
}

// Sent back to the sign-in page, the ref goes with them, so another try
// still carries it.
func TestRefKeptWhenSignInFails(t *testing.T) {
	a := setup(t, "@example.com")
	a.signIn(t, person, nil)
	a.client.Jar, _ = cookiejar.New(nil)
	a.p.mu.Lock()
	a.p.claims = nil
	a.p.mu.Unlock()
	a.start = "?ref=quickstart"
	other := map[string]any{"sub": "google-sub-2", "email": "ada@example.com", "email_verified": true}
	if to := a.signIn(t, other, nil); to != "/?ref=quickstart&sign_in_error=account_exists" {
		t.Fatalf("redirected to %s", to)
	}
}

// Past the cap, the waitlist entry keeps the first ref; the account made
// once they're invited takes it, whatever the later sign-in carries.
func TestRefThroughTheWaitlist(t *testing.T) {
	a := setup(t, "")
	a.capAt(t, 0)
	a.start = "?ref=hn"
	a.signIn(t, person, nil)
	a.client.Jar, _ = cookiejar.New(nil)
	a.start = "?ref=later"
	a.signIn(t, person, nil)
	var entryRef string
	if err := a.accounts.Pool.QueryRow(t.Context(), `SELECT signup_ref FROM waitlist WHERE email = 'ada@example.com'`).Scan(&entryRef); err != nil || entryRef != "hn" {
		t.Fatalf("waitlist entry's ref %q, %v", entryRef, err)
	}
	if _, err := a.accounts.Pool.Exec(t.Context(), `UPDATE waitlist SET invited_at = now() WHERE email = 'ada@example.com'`); err != nil {
		t.Fatal(err)
	}
	a.client.Jar, _ = cookiejar.New(nil)
	a.start = "?ref=invited"
	if to := a.signIn(t, person, nil); to != "/" || a.session(t)["authenticated"] != true {
		t.Fatalf("invited sign-in: %s", to)
	}
	if got := signupRef(t, a.accounts.Pool, "ada@example.com"); got != "hn" {
		t.Fatalf("signup_ref = %q", got)
	}
}

// GitHub's sign-in carries the ref the same way.
func TestRefWithGitHub(t *testing.T) {
	a := setupGitHub(t, "@example.com")
	a.ref = "cli"
	if to := a.signIn(t, 1, "ada@example.com", true, ""); to != "/agents" {
		t.Fatalf("redirected to %s", to)
	}
	if got := signupRef(t, a.accounts.Pool, "ada@example.com"); got != "cli" {
		t.Fatalf("signup_ref = %q", got)
	}
}
