package auth_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/accounts"
	"github.com/wakeshift/server/internal/appkey"
	"github.com/wakeshift/server/internal/apps"
)

func (a *app) capAt(t *testing.T, n int) {
	t.Helper()
	if err := pgx.BeginFunc(t.Context(), a.accounts.Pool, func(tx pgx.Tx) error {
		_, err := accounts.SetSignupCap(t.Context(), tx, n)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func (a *app) note(t *testing.T, origin, body string) int {
	t.Helper()
	req, _ := http.NewRequestWithContext(t.Context(), "POST", a.URL+"/api/v1/waitlist/note", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	resp, err := a.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// Under the cap, a sign-in makes an account. At it, it lands on the
// waitlist page, signed out, with a note to add; signing in again shows the
// same entry; once invited, the next sign-in makes the account.
func TestWaitlistPage(t *testing.T) {
	a := setup(t, "")
	if to := a.signIn(t, person, nil); to != "/" || a.session(t)["authenticated"] != true {
		t.Fatalf("under the cap: %s %v", to, a.session(t))
	}
	a.capAt(t, 1)
	a.client.Jar, _ = cookiejar.New(nil)
	bob := map[string]any{"sub": "google-sub-bob", "email": "bob@example.com", "email_verified": true, "name": "Bob"}
	if to := a.signIn(t, bob, nil); to != "/" {
		t.Fatalf("at the cap: redirected to %s", to)
	}
	s := a.session(t)
	wl, _ := s["waitlist"].(map[string]any)
	if s["authenticated"] == true || wl == nil || wl["joined_at"] == "" || wl["invited"] != false {
		t.Fatalf("session = %v", s)
	}
	var users int
	a.accounts.Pool.QueryRow(t.Context(), `SELECT count(*) FROM users`).Scan(&users)
	if users != 1 {
		t.Fatalf("%d users", users)
	}

	// The note: from the console only, at most 500 characters.
	if code := a.note(t, "https://evil.example", `{"note": "x"}`); code != http.StatusForbidden {
		t.Errorf("another origin: %d", code)
	}
	if code := a.note(t, a.URL, `{"note": "`+strings.Repeat("x", 501)+`"}`); code != http.StatusBadRequest {
		t.Errorf("too long: %d", code)
	}
	if code := a.note(t, a.URL, `{"note": "A reviewer bot"}`); code != http.StatusOK {
		t.Fatalf("note: %d", code)
	}
	if wl := a.session(t)["waitlist"].(map[string]any); wl["note"] != "A reviewer bot" {
		t.Errorf("note on the page: %v", wl)
	}

	// Again, from another browser: the same entry, with its note.
	a.client.Jar, _ = cookiejar.New(nil)
	if code := a.note(t, a.URL, `{"note": "x"}`); code != http.StatusUnauthorized {
		t.Errorf("a note without the cookie: %d", code)
	}
	a.signIn(t, bob, nil)
	if wl, _ := a.session(t)["waitlist"].(map[string]any); wl == nil || wl["note"] != "A reviewer bot" {
		t.Fatalf("again: %v", a.session(t))
	}
	var entries int
	a.accounts.Pool.QueryRow(t.Context(), `SELECT count(*) FROM waitlist`).Scan(&entries)
	if entries != 1 {
		t.Fatalf("%d entries", entries)
	}

	// Invited: the page says so, and the next sign-in makes the account and
	// drops the waitlist cookie.
	if err := pgx.BeginFunc(t.Context(), a.accounts.Pool, func(tx pgx.Tx) error {
		var id string
		tx.QueryRow(t.Context(), `SELECT id::text FROM waitlist`).Scan(&id)
		_, err := accounts.InviteFromWaitlist(t.Context(), tx, []string{id}, "admin@example.com")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if wl := a.session(t)["waitlist"].(map[string]any); wl["invited"] != true {
		t.Errorf("invited on the page: %v", wl)
	}
	if to := a.signIn(t, bob, nil); to != "/" {
		t.Fatalf("invited: %s", to)
	}
	s = a.session(t)
	if user, _ := s["user"].(map[string]any); s["authenticated"] != true || user["email"] != "bob@example.com" || s["waitlist"] != nil {
		t.Fatalf("after the invitation: %v", s)
	}
}

// holdKey posts a public key to the waitlist's key route, as the page does
// with the CLI link's, from origin, with cookie in place of the jar's
// waitlist cookie when it isn't "".
func (a *app) holdKey(t *testing.T, origin, cookie, body string) int {
	t.Helper()
	req, _ := http.NewRequestWithContext(t.Context(), "POST", a.URL+"/api/v1/waitlist/key", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	client := a.client
	if cookie != "" {
		client = &http.Client{}
		req.AddCookie(&http.Cookie{Name: "ws_waitlist", Value: cookie})
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// waitlistCookie is the jar's ws_waitlist cookie.
func (a *app) waitlistCookie(t *testing.T) string {
	t.Helper()
	u, _ := url.Parse(a.URL + "/api/v1/")
	for _, c := range a.client.Jar.Cookies(u) {
		if c.Name == "ws_waitlist" {
			return c.Value
		}
	}
	t.Fatal("no waitlist cookie")
	return ""
}

func publicKey(t *testing.T) (string, ed25519.PublicKey) {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(pub), pub
}

func keyBody(pk string) string { return `{"public_key": "` + pk + `"}` }

// The waitlist page holds the CLI link's public key against the person's
// entry, with their waitlist cookie only, from the console only; an app's
// key, another entry's, a malformed one or one too many are refused, and a
// cookie can't be forged, reused past its expiry, or made to name another
// entry.
func TestWaitlistKey(t *testing.T) {
	a := setup(t, "")
	a.signIn(t, person, nil) // the one account
	a.capAt(t, 1)
	a.client.Jar, _ = cookiejar.New(nil)
	bob := map[string]any{"sub": "google-sub-bob", "email": "bob@example.com", "email_verified": true, "name": "Bob"}
	a.signIn(t, bob, nil)
	bobCookie := a.waitlistCookie(t)
	var bobEntry string
	a.accounts.Pool.QueryRow(t.Context(), `SELECT id::text FROM waitlist WHERE email = 'bob@example.com'`).Scan(&bobEntry)

	pk, pub := publicKey(t)
	held := func() string {
		var entry string
		a.accounts.Pool.QueryRow(t.Context(), `SELECT entry_id::text FROM waitlist_keys WHERE kid = $1`, appkey.KeyID(pub)).Scan(&entry)
		return entry
	}

	// Refused before anything is held.
	if code := a.holdKey(t, "https://evil.example", "", keyBody(pk)); code != http.StatusForbidden {
		t.Errorf("another origin: %d", code)
	}
	if code := a.holdKey(t, a.URL, "", `{"public_key": "not-a-key"}`); code != http.StatusBadRequest {
		t.Errorf("malformed: %d", code)
	}
	small := base64.RawURLEncoding.EncodeToString(make([]byte, 32)) // a small-order point
	if code := a.holdKey(t, a.URL, "", keyBody(small)); code != http.StatusBadRequest {
		t.Errorf("small-order key: %d", code)
	}
	noJar := &app{Server: a.Server, client: &http.Client{}}
	if code := noJar.holdKey(t, a.URL, "", keyBody(pk)); code != http.StatusUnauthorized {
		t.Errorf("no cookie: %d", code)
	}
	body, sig, _ := strings.Cut(bobCookie, ".")
	if code := a.holdKey(t, a.URL, body+"."+sig[:len(sig)-2]+"AA", keyBody(pk)); code != http.StatusUnauthorized {
		t.Errorf("forged signature: %d", code)
	}
	if held() != "" {
		t.Fatal("held after refusals")
	}

	if code := a.holdKey(t, a.URL, "", keyBody(pk)); code != http.StatusOK {
		t.Fatalf("hold: %d", code)
	}
	if code := a.holdKey(t, a.URL, "", keyBody(pk)); code != http.StatusOK {
		t.Fatalf("hold again: %d", code)
	}
	if held() != bobEntry {
		t.Fatalf("held by %q, want bob's entry", held())
	}

	// Another person on the waitlist can't take it, and a cookie re-signed
	// to name bob's entry isn't valid.
	a.client.Jar, _ = cookiejar.New(nil)
	cy := map[string]any{"sub": "google-sub-cy", "email": "cy@example.com", "email_verified": true, "name": "Cy"}
	a.signIn(t, cy, nil)
	cyCookie := a.waitlistCookie(t)
	if code := a.holdKey(t, a.URL, "", keyBody(pk)); code != http.StatusConflict {
		t.Errorf("cy holding bob's key: %d", code)
	}
	cyBody, cySig, _ := strings.Cut(cyCookie, ".")
	raw, _ := base64.RawURLEncoding.DecodeString(cyBody)
	var cl map[string]any
	json.Unmarshal(raw, &cl)
	cl["w"] = bobEntry
	swapped, _ := json.Marshal(cl)
	pk2, _ := publicKey(t)
	if code := a.holdKey(t, a.URL, base64.RawURLEncoding.EncodeToString(swapped)+"."+cySig, keyBody(pk2)); code != http.StatusUnauthorized {
		t.Errorf("cy's cookie naming bob's entry: %d", code)
	}
	if held() != bobEntry {
		t.Errorf("after cy: held by %q", held())
	}

	// An app's key can't be held.
	r := &apps.Registry{Pool: a.accounts.Pool, Issuer: a.URL}
	appPub, _, _ := ed25519.GenerateKey(rand.Reader)
	if err := r.Sync(t.Context(), []apps.Config{{ID: "acme", Name: "Acme", PublicKey: base64.StdEncoding.EncodeToString(appPub)}}); err != nil {
		t.Fatal(err)
	}
	if code := a.holdKey(t, a.URL, "", keyBody(base64.RawURLEncoding.EncodeToString(appPub))); code != http.StatusConflict {
		t.Errorf("an app's key: %d", code)
	}

	// At most MaxPendingKeys an entry.
	for i := 1; i < apps.MaxPendingKeys; i++ {
		k, _ := publicKey(t)
		if code := a.holdKey(t, a.URL, bobCookie, keyBody(k)); code != http.StatusOK {
			t.Fatalf("bob's key %d: %d", i, code)
		}
	}
	extra, _ := publicKey(t)
	if code := a.holdKey(t, a.URL, bobCookie, keyBody(extra)); code != http.StatusTooManyRequests {
		t.Errorf("one too many: %d", code)
	}

	// Past the cookie's expiry: refused, though the browser still sends it.
	a.console.Now = func() time.Time { return time.Now().Add(8 * 24 * time.Hour) }
	if code := a.holdKey(t, a.URL, bobCookie, keyBody(pk)); code != http.StatusUnauthorized {
		t.Errorf("expired cookie: %d", code)
	}
	a.console.Now = nil

	// Invited: nothing to wait for.
	if _, err := a.accounts.Pool.Exec(t.Context(), `UPDATE waitlist SET invited_at = now() WHERE id::text = $1`, bobEntry); err != nil {
		t.Fatal(err)
	}
	if code := a.holdKey(t, a.URL, bobCookie, keyBody(extra)); code != http.StatusNotFound {
		t.Errorf("invited: %d", code)
	}
}
