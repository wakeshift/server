package auth

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wakeshift/server/internal/accounts"
)

// On https every console cookie is __Host-<name>, Secure, with Path=/, so no
// other host can plant one: an unprefixed session, from before the prefix
// or copied from a new one, signs nobody in. Plain http on localhost keeps
// the bare names and paths.
func TestHostPrefixedCookies(t *testing.T) {
	key := []byte(strings.Repeat("k", 32))
	c := &Console{Key: key, PublicURL: "https://wakeshift.example"}
	rec := httptest.NewRecorder()
	c.issue(rec, accounts.User{ID: "u1"}, "t1")
	set := rec.Result().Cookies()
	var session *http.Cookie
	clearedLegacy := false
	for _, ck := range set {
		switch ck.Name {
		case "__Host-ws_session":
			session = ck
		case "ws_session":
			clearedLegacy = ck.Value == "" && ck.Path == "/" && ck.Expires.Before(time.Now())
		}
	}
	if session == nil || session.Path != "/" || !session.Secure || !session.HttpOnly || session.Domain != "" || session.SameSite != http.SameSiteStrictMode {
		t.Fatalf("session cookie: %+v (all: %v)", session, set)
	}
	if !clearedLegacy {
		t.Errorf("the unprefixed session cookie isn't cleared: %v", set)
	}

	read := func(name, value string) bool {
		r := httptest.NewRequest("GET", "/", nil)
		r.AddCookie(&http.Cookie{Name: name, Value: value})
		cl, ok := c.read(r)
		return ok && cl.User == "u1"
	}
	if !read("__Host-ws_session", session.Value) {
		t.Fatal("the prefixed session doesn't read")
	}
	// Planted under the old name by another host: refused.
	if read("ws_session", session.Value) {
		t.Fatal("a new session reads under the unprefixed name")
	}
	// A session signed before the prefix, under either name: refused (a
	// one-time sign-out), so none can be planted.
	legacy := c.sign(legacySessionPurpose, claims{User: "u1", Tenant: "t1", Exp: time.Now().Add(time.Hour).Unix()})
	if read("ws_session", legacy) || read("__Host-ws_session", legacy) {
		t.Fatal("a session from before the prefix reads")
	}

	// The sign-in flow's and the waitlist's cookies: prefixed, Path=/.
	for _, name := range []string{flowCookie, githubCookie, joinCookie, waitlistCookie} {
		ck := c.cookie(name, "/auth/google", "v", time.Now().Add(time.Minute), http.SameSiteLaxMode)
		if ck.Name != "__Host-"+name || ck.Path != "/" || !ck.Secure || ck.Domain != "" {
			t.Errorf("%s: %+v", name, ck)
		}
	}

	// Plain http on localhost: the bare names and paths, and the session
	// signed as before.
	local := &Console{Key: key, PublicURL: "http://localhost:8370"}
	if ck := local.cookie(flowCookie, flowPath, "v", time.Now().Add(time.Minute), http.SameSiteLaxMode); ck.Name != flowCookie || ck.Path != flowPath || ck.Secure {
		t.Errorf("localhost flow cookie: %+v", ck)
	}
	rec = httptest.NewRecorder()
	local.issue(rec, accounts.User{ID: "u1"}, "t1")
	if got := rec.Result().Cookies(); len(got) != 1 || got[0].Name != "ws_session" {
		t.Fatalf("localhost session: %v", got)
	}
	r := httptest.NewRequest("GET", "/", nil)
	r.AddCookie(rec.Result().Cookies()[0])
	if _, ok := local.read(r); !ok {
		t.Fatal("the localhost session doesn't read")
	}
}
