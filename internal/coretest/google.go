package coretest

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"

	"github.com/wakeshift/server/internal/auth"
)

// Google is a fake Google (OpenID Connect) for a core's sign-in: dev
// sign-in always makes an account, so a test that needs the sign-up cap
// and the waitlist signs in here. Set it with StartWith's
// core.Options.Google = g.Config().
type Google struct {
	*httptest.Server
	key *rsa.PrivateKey

	mu     sync.Mutex
	codes  map[string]string         // code → PKCE challenge
	claims map[string]map[string]any // code → ID token claims
}

// NewGoogle starts a fake Google for the test.
func NewGoogle(t *testing.T) *Google {
	t.Helper()
	g := &Google{codes: map[string]string{}, claims: map[string]map[string]any{}}
	g.key, _ = rsa.GenerateKey(rand.Reader, 2048)
	mux := http.NewServeMux()
	g.Server = httptest.NewServer(mux)
	t.Cleanup(g.Close)
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"issuer": g.URL, "authorization_endpoint": g.URL + "/authorize", "token_endpoint": g.URL + "/token",
			"jwks_uri": g.URL + "/jwks", "id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &g.key.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"}}})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		code := r.Form.Get("code")
		g.mu.Lock()
		challenge, claims := g.codes[code], g.claims[code]
		delete(g.codes, code)
		g.mu.Unlock()
		sum := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
		if challenge == "" || base64.RawURLEncoding.EncodeToString(sum[:]) != challenge {
			http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
			return
		}
		cl := map[string]any{"iss": g.URL, "aud": "coretest-client", "exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix()}
		for k, v := range claims {
			cl[k] = v
		}
		signer, _ := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: g.key}, (&jose.SignerOptions{}).WithHeader("kid", "k1"))
		idt, _ := jwt.Signed(signer).Claims(cl).Serialize()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"access_token": "at", "token_type": "Bearer", "id_token": idt})
	})
	return g
}

// Config is the core's Google sign-in, pointed at g.
func (g *Google) Config() *auth.Google {
	return &auth.Google{ClientID: "coretest-client", ClientSecret: "secret", Issuer: g.URL}
}

// GoogleSignIn signs a person in with g, as a browser would (the console's
// start, Google, the callback), with Google subject sub and a verified
// email. It returns the browser, signed in or, past the sign-up cap, with
// its waitlist cookie, and where the callback sent it.
func (c *Core) GoogleSignIn(g *Google, sub, email string) (*User, string) {
	c.t.Helper()
	jar, _ := cookiejar.New(nil)
	noFollow := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := noFollow.Get(c.URL + "/auth/google/start")
	if err != nil {
		c.t.Fatal(err)
	}
	resp.Body.Close()
	loc, err := url.Parse(resp.Header.Get("Location"))
	if err != nil || loc.Host != mustHost(g.URL) {
		c.t.Fatalf("Google sign-in: start sent the browser to %v", loc)
	}
	q := loc.Query()
	code := "code-" + sub
	g.mu.Lock()
	g.codes[code] = q.Get("code_challenge")
	g.claims[code] = map[string]any{"sub": sub, "email": email, "email_verified": true, "nonce": q.Get("nonce")}
	g.mu.Unlock()
	resp, err = noFollow.Get(c.URL + "/auth/google/callback?" + url.Values{"code": {code}, "state": {q.Get("state")}}.Encode())
	if err != nil {
		c.t.Fatal(err)
	}
	resp.Body.Close()
	return &User{c: c, Client: &http.Client{Jar: jar}}, resp.Header.Get("Location")
}

func mustHost(raw string) string {
	u, _ := url.Parse(raw)
	return u.Host
}
