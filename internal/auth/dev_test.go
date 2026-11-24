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
	"github.com/wakeshift/server/internal/db"
	"github.com/wakeshift/server/internal/dbtest"
)

const devPublic = "http://localhost:8370"
const devToken = "0123456789abcdef-dev-token"

// devFixture is a development sign-in server, and a way to try signing in.
type devFixture struct {
	t   *testing.T
	d   *db.DB
	mux *http.ServeMux
}

func newDevFixture(t *testing.T) *devFixture {
	d := dbtest.New(t)
	mux := http.NewServeMux()
	(&auth.Console{Key: []byte(strings.Repeat("k", 32)), PublicURL: devPublic, DevSignIn: true, DevSignInToken: devToken, Accounts: &accounts.Store{Pool: d.Pool}}).Register(mux)
	return &devFixture{t: t, d: d, mux: mux}
}

// signIn posts a dev sign-in from remote with the console's Origin, the
// token in the body (if any) and header set as given (not canonicalized).
func (f *devFixture) signIn(remote, email, token string, header http.Header) *httptest.ResponseRecorder {
	f.t.Helper()
	body := map[string]string{"email": email}
	if token != "" {
		body["token"] = token
	}
	b, _ := json.Marshal(body)
	r := httptest.NewRequestWithContext(context.Background(), "POST", "/api/v1/session/dev", strings.NewReader(string(b)))
	r.RemoteAddr = remote
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Origin", devPublic)
	for k, v := range header {
		r.Header[k] = v // as given, not canonicalized
	}
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, r)
	return rec
}

func (f *devFixture) users(email string) int {
	f.t.Helper()
	var n int
	if err := f.d.Pool.QueryRow(context.Background(), `SELECT count(*) FROM users WHERE email = $1`, email).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

// refused checks a sign-in was refused with code, setting no cookie and
// making no user.
func (f *devFixture) refused(rec *httptest.ResponseRecorder, email, code string) {
	f.t.Helper()
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), code) {
		f.t.Fatalf("status %d: %s (want 403 %s)", rec.Code, rec.Body, code)
	}
	if rec.Header().Get("Set-Cookie") != "" {
		f.t.Fatal("a refused sign-in set a cookie")
	}
	if n := f.users(email); n != 0 {
		f.t.Fatalf("a refused sign-in made %d users", n)
	}
}

// Development sign-in works only from this machine: a loopback connection
// with the console's Origin. A reverse proxy on the same host connects from
// loopback whoever its client is, so a request carrying any proxy's
// forwarding header is refused, even with the token, and nobody is signed
// in.
func TestDevSignInRefusesProxiedRequests(t *testing.T) {
	f := newDevFixture(t)
	for _, tc := range []struct{ name, header, value string }{
		{"X-Forwarded-For", "X-Forwarded-For", "203.0.113.7"},
		{"X-Forwarded-For loopback", "X-Forwarded-For", "127.0.0.1"},
		{"X-Forwarded-For empty", "X-Forwarded-For", ""},
		{"X-Forwarded-Host", "X-Forwarded-Host", "dev.example.com"},
		{"X-Forwarded-Proto", "X-Forwarded-Proto", "https"},
		{"X-Forwarded-Port", "X-Forwarded-Port", "443"},
		{"X-Real-IP", "X-Real-IP", "203.0.113.7"},
		{"Forwarded", "Forwarded", "for=203.0.113.7;proto=https"},
		{"lowercase key", "x-forwarded-for", "203.0.113.7"},
		{"uppercase key", "X-REAL-IP", "203.0.113.7"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			email := "proxied-" + strings.ToLower(strings.ReplaceAll(tc.name, " ", "-")) + "@example.com"
			f.refused(f.signIn("127.0.0.1:50000", email, devToken, http.Header{tc.header: {tc.value}}), email, "not_local")
		})
	}
	t.Run("not loopback", func(t *testing.T) {
		f.refused(f.signIn("192.0.2.1:50000", "remote@example.com", devToken, nil), "remote@example.com", "not_local")
	})
}

// A forwarder that adds no headers (socat, ssh -R, nginx's plain
// proxy_pass) looks like a loopback client with a forged Origin: without
// the server's token, or with a wrong one, it's refused. Only the right
// token, in the body or the header, signs in.
func TestDevSignInNeedsTheToken(t *testing.T) {
	f := newDevFixture(t)
	for _, tc := range []struct {
		name, body string
		header     http.Header
	}{
		{"no token", "", nil},
		{"wrong token", "0123456789abcdef-wrong-tok", nil},
		{"token prefix", devToken[:len(devToken)-1], nil},
		{"token with more", devToken + "x", nil},
		{"wrong header", "", http.Header{auth.DevTokenHeader: {"0123456789abcdef-wrong-tok"}}},
		// The header, when sent, is the one checked.
		{"wrong header, right body", devToken, http.Header{auth.DevTokenHeader: {"0123456789abcdef-wrong-tok"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			email := "token-" + strings.ReplaceAll(tc.name, " ", "-") + "@example.com"
			email = strings.ReplaceAll(email, ",", "")
			f.refused(f.signIn("127.0.0.1:50000", email, tc.body, tc.header), email, "bad_dev_token")
		})
	}

	for _, tc := range []struct {
		name, remote, body string
		header             http.Header
	}{
		{"in the body, 127.0.0.1", "127.0.0.1:50000", devToken, nil},
		{"in the body, ::1", "[::1]:50000", devToken, nil},
		{"in the header", "127.0.0.1:50000", "", http.Header{auth.DevTokenHeader: {devToken}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			email := "local-" + strings.NewReplacer(" ", "-", ",", "", ":", "").Replace(tc.name) + "@example.com"
			rec := f.signIn(tc.remote, email, tc.body, tc.header)
			if rec.Code != http.StatusOK {
				t.Fatalf("status %d: %s", rec.Code, rec.Body)
			}
			if !strings.Contains(rec.Header().Get("Set-Cookie"), "ws_session=") {
				t.Fatalf("no session cookie: %q", rec.Header().Get("Set-Cookie"))
			}
			if n := f.users(email); n != 1 {
				t.Fatalf("%d users for %s", n, email)
			}
		})
	}
}

// With no token (or one too short to resist guessing), dev sign-in isn't
// served at all.
func TestDevSignInOffWithoutAToken(t *testing.T) {
	d := dbtest.New(t)
	for _, token := range []string{"", "short"} {
		mux := http.NewServeMux()
		(&auth.Console{Key: []byte(strings.Repeat("k", 32)), PublicURL: devPublic, DevSignIn: true, DevSignInToken: token, Accounts: &accounts.Store{Pool: d.Pool}}).Register(mux)
		r := httptest.NewRequestWithContext(context.Background(), "POST", "/api/v1/session/dev", strings.NewReader(`{"email":"a@example.com","token":"`+token+`"}`))
		r.RemoteAddr = "127.0.0.1:50000"
		r.Header.Set("Origin", devPublic)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, r)
		if rec.Code != http.StatusNotFound && rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("token %q: status %d", token, rec.Code)
		}
	}
}
