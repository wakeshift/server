package auth_test

import (
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

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/accounts"
	"github.com/wakeshift/server/internal/auth"
	"github.com/wakeshift/server/internal/dbtest"
)

// fakeGitHub is GitHub's OAuth endpoints (checking PKCE) and the two API
// calls sign-in makes, for whichever account the test sets.
type fakeGitHub struct {
	*httptest.Server
	mu         sync.Mutex
	challenges map[string]string // code → code_challenge
	user       map[string]any
	emails     []map[string]any
}

func newGitHub(t *testing.T) *fakeGitHub {
	g := &fakeGitHub{challenges: map[string]string{}}
	mux := http.NewServeMux()
	g.Server = httptest.NewServer(mux)
	t.Cleanup(g.Close)
	mux.HandleFunc("POST /login/oauth/access_token", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		g.mu.Lock()
		challenge := g.challenges[r.Form.Get("code")]
		g.mu.Unlock()
		sum := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
		if challenge == "" || base64.RawURLEncoding.EncodeToString(sum[:]) != challenge || r.Form.Get("client_secret") != "gh-secret" {
			http.Error(w, `{"error":"bad_verification_code"}`, http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"access_token":"gho_test","token_type":"bearer","scope":"read:user,user:email"}`))
	})
	api := func(out func() any) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer gho_test" {
				http.Error(w, "", http.StatusUnauthorized)
				return
			}
			g.mu.Lock()
			defer g.mu.Unlock()
			json.NewEncoder(w).Encode(out())
		}
	}
	mux.HandleFunc("GET /user", api(func() any { return g.user }))
	mux.HandleFunc("GET /user/emails", api(func() any { return g.emails }))
	return g
}

type ghApp struct {
	*httptest.Server
	client   *http.Client
	gh       *fakeGitHub
	accounts *accounts.Store
	ref      string // the start URL's ref, if any
}

func setupGitHub(t *testing.T, allowed string) *ghApp {
	t.Helper()
	d := dbtest.New(t)
	gh := newGitHub(t)
	store := &accounts.Store{Pool: d.Pool, Allowed: accounts.AllowList(allowed), InvitesPerUser: 1}
	c := &auth.Console{Key: []byte(strings.Repeat("k", 32)), Accounts: store,
		GitHub: &auth.GitHub{ClientID: "gh-client", ClientSecret: "gh-secret", Web: gh.URL, API: gh.URL}}
	mux := http.NewServeMux()
	c.Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c.PublicURL = srv.URL
	a := &ghApp{Server: srv, gh: gh, accounts: store}
	a.newBrowser()
	return a
}

// capAt sets the sign-up cap.
func (a *ghApp) capAt(t *testing.T, n int) {
	t.Helper()
	if err := pgx.BeginFunc(t.Context(), a.accounts.Pool, func(tx pgx.Tx) error {
		_, err := accounts.SetSignupCap(t.Context(), tx, n)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func (a *ghApp) newBrowser() {
	jar, _ := cookiejar.New(nil)
	a.client = &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// signIn signs in as a GitHub account (with an invite, if given) and
// returns where the callback redirected.
func (a *ghApp) signIn(t *testing.T, id int, email string, verified bool, invite string) string {
	t.Helper()
	start := a.URL + "/auth/github/start?return_to=%2Fagents"
	if invite != "" {
		start += "&invite=" + invite
	}
	if a.ref != "" {
		start += "&ref=" + url.QueryEscape(a.ref)
	}
	resp, err := a.client.Get(start)
	if err != nil {
		t.Fatal(err)
	}
	loc, _ := url.Parse(resp.Header.Get("Location"))
	q := loc.Query()
	if !strings.HasPrefix(loc.String(), a.gh.URL+"/login/oauth/authorize") || q.Get("code_challenge_method") != "S256" ||
		q.Get("redirect_uri") != a.URL+"/auth/github/callback" || !strings.Contains(q.Get("scope"), "user:email") {
		t.Fatalf("start redirected to %s", loc)
	}
	a.gh.mu.Lock()
	a.gh.challenges["code-1"] = q.Get("code_challenge")
	a.gh.user = map[string]any{"id": id, "login": "ada-l", "name": ""}
	a.gh.emails = []map[string]any{{"email": "old@example.com", "primary": false, "verified": true}, {"email": email, "primary": true, "verified": verified}}
	a.gh.mu.Unlock()
	resp, err = a.client.Get(a.URL + "/auth/github/callback?" + url.Values{"code": {"code-1"}, "state": {q.Get("state")}}.Encode())
	if err != nil {
		t.Fatal(err)
	}
	return resp.Header.Get("Location")
}

func (a *ghApp) signedInAs(t *testing.T) string {
	t.Helper()
	resp, err := a.client.Get(a.URL + "/api/v1/session")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var s struct {
		User *struct{ Email, Name string }
	}
	json.NewDecoder(resp.Body).Decode(&s)
	if s.User == nil {
		return ""
	}
	return s.User.Email + " " + s.User.Name
}

func TestGitHubSignInWithAnInvite(t *testing.T) {
	a := setupGitHub(t, "@corp.example.com")
	// Staff sign up without an invite; they have one to share (one sign-up,
	// here).
	staff, err := a.accounts.SignIn(t.Context(), accounts.Identity{Provider: "google", Subject: "s", Email: "staff@corp.example.com", HostedDomain: "corp.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	invite, err := a.accounts.UserInvite(t.Context(), staff.ID)
	if again, _ := a.accounts.UserInvite(t.Context(), staff.ID); err != nil || invite.UsesLeft != 1 || again.Code != invite.Code {
		t.Fatalf("invite %+v %+v %v", invite, again, err)
	}

	// At the sign-up cap, without an invite (or with a made-up one), the
	// waitlist and no account.
	a.capAt(t, 0)
	if to := a.signIn(t, 1, "ada@example.com", true, ""); to != "/" || a.signedInAs(t) != "" {
		t.Fatalf("no invite: %s", to)
	}
	if to := a.signIn(t, 1, "ada@example.com", true, "madeup"); to != "/" || a.signedInAs(t) != "" {
		t.Fatalf("a made-up invite: %s", to)
	}
	// An unverified email isn't an identity, and doesn't spend the invite.
	if to := a.signIn(t, 1, "ada@example.com", false, invite.Code); !strings.Contains(to, "sign_in_error=failed") {
		t.Fatalf("unverified: %s", to)
	}
	if to := a.signIn(t, 1, "ada@example.com", true, strings.ToUpper(invite.Code)); to != "/agents" {
		t.Fatalf("with the invite: %s", to)
	}
	if got := a.signedInAs(t); got != "ada@example.com ada-l" {
		t.Fatalf("signed in as %q", got)
	}
	// Used up: the waitlist.
	a.newBrowser()
	if to := a.signIn(t, 2, "bob@example.com", true, invite.Code); to != "/" || a.signedInAs(t) != "" {
		t.Fatalf("a used-up invite: %s", to)
	}
	// Ada signs in again (an existing user needs no invite), and has her own
	// invite to share.
	a.newBrowser()
	if to := a.signIn(t, 1, "ada@example.com", true, ""); to != "/agents" {
		t.Fatalf("signing in again: %s", to)
	}
	resp, _ := a.client.Get(a.URL + "/api/v1/invite")
	var mine struct {
		Code, URL string
		UsesLeft  int `json:"uses_left"`
	}
	json.NewDecoder(resp.Body).Decode(&mine)
	resp.Body.Close()
	if mine.Code == "" || mine.Code == invite.Code || mine.URL != a.URL+"/?invite="+mine.Code || mine.UsesLeft != 1 {
		t.Fatalf("ada's invite %+v", mine)
	}
	for code, want := range map[string]bool{mine.Code: true, invite.Code: false, "nope": false} {
		resp, _ := http.Get(a.URL + "/api/v1/invites/" + code)
		var v struct{ Valid bool }
		json.NewDecoder(resp.Body).Decode(&v)
		resp.Body.Close()
		if v.Valid != want {
			t.Errorf("invite %s valid = %v", code, v.Valid)
		}
	}
}
