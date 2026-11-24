package auth_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"

	"github.com/wakeshift/server/internal/accounts"
	"github.com/wakeshift/server/internal/auth"
	"github.com/wakeshift/server/internal/dbtest"
)

// provider is a fake OpenID Connect provider: discovery, JWKS, and a token
// endpoint that checks PKCE and returns an ID token with whatever claims
// the test sets.
type provider struct {
	*httptest.Server
	key *rsa.PrivateKey

	mu         sync.Mutex
	challenges map[string]string // code → code_challenge
	claims     map[string]any    // for the next ID token
	nonce      string            // overrides the nonce, if set
}

func newProvider(t *testing.T) *provider {
	p := &provider{challenges: map[string]string{}}
	p.key, _ = rsa.GenerateKey(rand.Reader, 2048)
	mux := http.NewServeMux()
	p.Server = httptest.NewServer(mux)
	t.Cleanup(p.Close)
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"issuer": p.URL, "authorization_endpoint": p.URL + "/authorize", "token_endpoint": p.URL + "/token",
			"jwks_uri": p.URL + "/jwks", "id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &p.key.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"}}})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		p.mu.Lock()
		challenge := p.challenges[r.Form.Get("code")]
		claims, nonceOverride := p.claims, p.nonce
		p.mu.Unlock()
		sum := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
		if challenge == "" || base64.RawURLEncoding.EncodeToString(sum[:]) != challenge {
			http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
			return
		}
		cl := map[string]any{"iss": p.URL, "aud": "client-1", "sub": "google-sub-1", "exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix()}
		for k, v := range claims {
			cl[k] = v
		}
		if nonceOverride != "" {
			cl["nonce"] = nonceOverride
		}
		signer, _ := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: p.key}, (&jose.SignerOptions{}).WithHeader("kid", "k1"))
		idt, _ := jwt.Signed(signer).Claims(cl).Serialize()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"access_token": "at", "token_type": "Bearer", "id_token": idt})
	})
	return p
}

type app struct {
	*httptest.Server
	client *http.Client
	p      *provider
	// accounts is the console's store (its Pool, the test database).
	accounts *accounts.Store
	console  *auth.Console
	// start is the start URL's query, if any ("?ref=quickstart").
	start string
}

func setup(t *testing.T, allowed string) *app {
	t.Helper()
	d := dbtest.New(t)
	p := newProvider(t)
	c := &auth.Console{
		Key:      []byte(strings.Repeat("k", 32)),
		Accounts: &accounts.Store{Pool: d.Pool, Allowed: accounts.AllowList(allowed)},
		Google:   &auth.Google{ClientID: "client-1", ClientSecret: "secret", Issuer: p.URL},
	}
	mux := http.NewServeMux()
	c.Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c.PublicURL = srv.URL
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return &app{Server: srv, client: client, p: p, accounts: c.Accounts, console: c}
}

// signIn runs the flow: start, "authorize" at the provider (recording the
// PKCE challenge for the code), and come back to the callback. tamper can
// change the callback's query. It returns where the callback redirected.
func (a *app) signIn(t *testing.T, claims map[string]any, tamper func(q url.Values)) string {
	t.Helper()
	resp, err := a.client.Get(a.URL + "/auth/google/start" + a.start)
	if err != nil {
		t.Fatal(err)
	}
	loc, _ := url.Parse(resp.Header.Get("Location"))
	q := loc.Query()
	if !strings.HasPrefix(loc.String(), a.p.URL+"/authorize") || q.Get("code_challenge_method") != "S256" || q.Get("nonce") == "" {
		t.Fatalf("start redirected to %s", loc)
	}
	a.p.mu.Lock()
	a.p.challenges["code-1"] = q.Get("code_challenge")
	a.p.claims = map[string]any{"nonce": q.Get("nonce")}
	for k, v := range claims {
		a.p.claims[k] = v
	}
	a.p.mu.Unlock()
	back := url.Values{"code": {"code-1"}, "state": {q.Get("state")}}
	if tamper != nil {
		tamper(back)
	}
	resp, err = a.client.Get(a.URL + "/auth/google/callback?" + back.Encode())
	if err != nil {
		t.Fatal(err)
	}
	return resp.Header.Get("Location")
}

func (a *app) session(t *testing.T) map[string]any {
	t.Helper()
	resp, err := a.client.Get(a.URL + "/api/v1/session")
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return out
}

var person = map[string]any{"email": "Ada@Example.com", "email_verified": true, "name": "Ada", "hd": "example.com"}

func TestGoogleSignIn(t *testing.T) {
	a := setup(t, "@example.com")
	if to := a.signIn(t, person, nil); to != "/" {
		t.Fatalf("redirected to %s", to)
	}
	s := a.session(t)
	user, _ := s["user"].(map[string]any)
	ws, _ := s["workspace"].(map[string]any)
	if s["authenticated"] != true || user["email"] != "ada@example.com" || ws["kind"] != "personal" || ws["role"] != "owner" {
		t.Fatalf("session = %v", s)
	}
	// Signing in again is the same user and workspace.
	a.signIn(t, person, nil)
	if again := a.session(t); again["workspace"].(map[string]any)["id"] != ws["id"] {
		t.Fatalf("second sign-in got another workspace: %v", again)
	}
}

func TestGoogleSignInRejects(t *testing.T) {
	for name, c := range map[string]struct {
		allowed string
		claims  map[string]any
		tamper  func(url.Values)
		nonce   string
		want    string
	}{
		"unverified email":  {allowed: "@example.com", claims: map[string]any{"email": "ada@example.com", "email_verified": false}, want: "failed"},
		"wrong state":       {allowed: "@example.com", claims: person, tamper: func(q url.Values) { q.Set("state", "forged") }, want: "failed"},
		"wrong code (PKCE)": {allowed: "@example.com", claims: person, tamper: func(q url.Values) { q.Set("code", "stolen") }, want: "failed"},
		"wrong nonce":       {allowed: "@example.com", claims: person, nonce: "replayed", want: "failed"},
	} {
		t.Run(name, func(t *testing.T) {
			a := setup(t, c.allowed)
			a.p.nonce = c.nonce
			if to := a.signIn(t, c.claims, c.tamper); to != "/?sign_in_error="+c.want {
				t.Fatalf("redirected to %s", to)
			}
			if a.session(t)["authenticated"] == true {
				t.Fatal("signed in anyway")
			}
		})
	}
}

func TestCallbackWithoutSignInInProgress(t *testing.T) {
	a := setup(t, "@example.com")
	resp, _ := a.client.Get(a.URL + "/auth/google/callback?code=code-1&state=anything")
	if to := resp.Header.Get("Location"); to != "/?sign_in_error=failed" {
		t.Fatalf("redirected to %s", to)
	}
}

func TestDevSignInOnlyOnLocalhost(t *testing.T) {
	d := dbtest.New(t)
	for url, want := range map[string]bool{
		"http://localhost:8370":   true,
		"http://127.0.0.1:9":      true,
		"https://localhost:8443":  true,
		"https://example.test":    false,
		"http://example.test":     false,
		"http://localhost.evil.x": false,
	} {
		mux := http.NewServeMux()
		(&auth.Console{Key: []byte(strings.Repeat("k", 32)), PublicURL: url, DevSignIn: true, DevSignInToken: strings.Repeat("t", auth.MinDevTokenLen), Accounts: &accounts.Store{Pool: d.Pool}}).Register(mux)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), "POST", "/api/v1/session/dev", strings.NewReader(`{}`)))
		if got := rec.Code != http.StatusNotFound && rec.Code != http.StatusMethodNotAllowed; got != want {
			t.Errorf("%s: dev sign-in available = %v (status %d)", url, got, rec.Code)
		}
	}
}

// Another Google account with the same email as an existing user (e.g. a
// personal account made with a work address) can't sign in as that user.
func TestSecondAccountWithSameEmailIsRefused(t *testing.T) {
	a := setup(t, "@example.com")
	a.signIn(t, person, nil)
	a.client.Jar, _ = cookiejar.New(nil) // a different browser
	a.p.mu.Lock()
	a.p.claims = nil
	a.p.mu.Unlock()
	other := map[string]any{"sub": "google-sub-2", "email": "ada@example.com", "email_verified": true}
	if to := a.signIn(t, other, nil); to != "/?sign_in_error=account_exists" {
		t.Fatalf("redirected to %s", to)
	}
	if a.session(t)["authenticated"] == true {
		t.Fatal("signed in as the existing user")
	}
}
