// Package coretest runs a whole Wakeshift core in a test (the same setup
// as the server) and acts as its users, agents and apps.
package coretest

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"html"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/wakeshift/server/internal/apps"
	"github.com/wakeshift/server/internal/core"
	"github.com/wakeshift/server/internal/db"
	"github.com/wakeshift/server/internal/dbtest"
)

// Core is a running core.
type Core struct {
	*core.Core
	t   *testing.T
	URL string
	DB  *db.DB
	// stop ends the core's background work (Stop).
	stop func()
}

// Stop ends the core's background work now (its deliverers, expiry pass),
// as a revision being replaced does; its handler still answers.
func (c *Core) Stop() { c.stop() }

// Start runs a core on a test server with its background work, for the
// test's lifetime. Apps are registered later with RegisterApp.
func Start(t *testing.T) *Core {
	t.Helper()
	return StartWith(t, nil)
}

// StartWith is Start with options changed first (by set, if not nil).
func StartWith(t *testing.T, set func(*core.Options)) *Core {
	t.Helper()
	d := dbtest.New(t)
	hs := httptest.NewUnstartedServer(nil)
	hs.Start()
	t.Cleanup(hs.Close)
	// Subscribe vouchers, MCP Events push and Grok Bot's webhooks on, as in
	// development.
	// Claude Code's session-only steps inline (WAKESHIFT_CLAUDE_SESSION_STEPS=inline,
	// the rollback switch), which most tests drive straight from the first
	// result; sessionsetup_test.go starts cores with the default, the steps
	// sent only once they're picked.
	o := core.Options{DB: d, PublicURL: hs.URL, Key: Key, DevSignIn: true, DevSignInToken: DevToken, Web: fstest.MapFS{}, Poll: 50 * time.Millisecond, SubscribeVouchers: true,
		MCPEventsPush: true, GrokWebhooks: true, InlineSessionSteps: true}
	// The connector, built for this machine, served as Grok Bots install it.
	o.ConnectorDir = ConnectorDir(t)
	if set != nil {
		set(&o)
	}
	c, err := core.New(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	hs.Config.Handler = c.Handler
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); c.Run(ctx) }()
	var once sync.Once
	halt := func() { once.Do(func() { stop(); <-done }) }
	t.Cleanup(halt)
	time.Sleep(50 * time.Millisecond) // let the listeners start
	return &Core{Core: c, t: t, URL: hs.URL, DB: o.DB, stop: halt}
}

// Key is the key the core gets for a purpose (core.Options.Key): fixed,
// so a test can make what only the core should (a forged link, to check
// it's refused for its claims rather than its signature).
func Key(purpose string) []byte {
	sum := sha256.Sum256([]byte("coretest/" + purpose))
	return sum[:]
}

// RegisterApp registers an app with a new key, as WAKESHIFT_APPS would, and
// returns the private key.
func (c *Core) RegisterApp(id, name, mcpURL string) ed25519.PrivateKey {
	c.t.Helper()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	err := c.Apps.Sync(context.Background(), []apps.Config{{ID: id, Name: name, MCPURL: mcpURL, PublicKey: base64.StdEncoding.EncodeToString(pub)}})
	if err != nil {
		c.t.Fatal(err)
	}
	return priv
}

// User is a signed-in browser.
type User struct {
	c      *Core
	Client *http.Client
}

// DevToken is the core's development sign-in token (core.Options.DevSignInToken).
const DevToken = "coretest-dev-sign-in-token"

// SignIn signs a user in (dev sign-in), creating them on first use.
func (c *Core) SignIn(email string) *User {
	c.t.Helper()
	jar, _ := cookiejar.New(nil)
	u := &User{c: c, Client: &http.Client{Jar: jar}}
	if code := u.Do("POST", "/api/v1/session/dev", map[string]string{"email": email, "token": DevToken}, nil); code != 200 {
		c.t.Fatalf("sign in %s: %d", email, code)
	}
	return u
}

// Do sends a console request from the console's origin.
func (u *User) Do(method, path string, body, out any) int {
	u.c.t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(method, u.c.URL+path, bytes.NewReader(b))
	req.Header.Set("Origin", u.c.URL)
	req.Header.Set("Content-Type", "application/json")
	resp, err := u.Client.Do(req)
	if err != nil {
		u.c.t.Fatal(err)
	}
	defer resp.Body.Close()
	if out != nil {
		json.NewDecoder(resp.Body).Decode(out)
	}
	return resp.StatusCode
}

// Workspace returns the user's current workspace ID.
func (u *User) Workspace(t testing.TB) string {
	t.Helper()
	var s struct{ Workspace struct{ ID string } }
	u.Do("GET", "/api/v1/session", nil, &s)
	return s.Workspace.ID
}

var (
	formTagRE  = regexp.MustCompile(`(?i)<(form|input)\b[^>]*>`)
	formAttrRE = regexp.MustCompile(`([a-zA-Z_:-]+)\s*=\s*(?:"([^"]*)"|'([^']*)')`)
)

// authorizeForm finds an authorize page's form: where it posts, and its
// CSRF token (attributes in any order, either quote).
func authorizeForm(page []byte) (action, csrf string, ok bool) {
	for _, tag := range formTagRE.FindAllSubmatch(page, -1) {
		a := map[string]string{}
		for _, m := range formAttrRE.FindAllSubmatch(tag[0], -1) {
			a[strings.ToLower(string(m[1]))] = html.UnescapeString(string(m[2]) + string(m[3]))
		}
		switch {
		case strings.EqualFold(string(tag[1]), "form") && strings.EqualFold(a["method"], "post") && action == "":
			action = a["action"]
		case strings.EqualFold(string(tag[1]), "input") && a["name"] == "csrf" && csrf == "":
			csrf = a["value"]
		}
	}
	return action, csrf, action != "" && csrf != ""
}

// AppSignIn signs an MCP client in to an app with its own OAuth server (our
// games; appBase is its public URL), as Claude Code would: dynamic
// registration, the authorize redirect (clicking Authorize, if the app shows
// its page) and the code exchange with the client secret registration gave
// it. Each call is a new sign-in. It returns the access token.
func (c *Core) AppSignIn(appBase, client string) string {
	c.t.Helper()
	const redirect = "http://127.0.0.1:9/callback"
	var reg struct {
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
	}
	b, _ := json.Marshal(map[string]any{"client_name": client, "redirect_uris": []string{redirect}})
	resp, err := http.Post(appBase+"/oauth/register", "application/json", bytes.NewReader(b))
	if err != nil {
		c.t.Fatal(err)
	}
	json.NewDecoder(resp.Body).Decode(&reg)
	resp.Body.Close()
	verifier := "v-" + strings.Repeat("b", 43)
	sum := sha256.Sum256([]byte(verifier))
	next := appBase + "/oauth/authorize?" + url.Values{"response_type": {"code"}, "client_id": {reg.ClientID}, "redirect_uri": {redirect},
		"state": {"s"}, "code_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])}, "code_challenge_method": {"S256"}}.Encode()
	jar, _ := cookiejar.New(nil)
	noFollow := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for range 5 {
		if strings.HasPrefix(next, redirect) {
			break
		}
		resp, err := noFollow.Get(next)
		if err != nil {
			c.t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			// The authorize page: click Authorize.
			action, csrf, ok := authorizeForm(body)
			if !ok {
				c.t.Fatalf("guest sign-in at %s: a page without the form", next)
			}
			if resp, err = noFollow.PostForm(action, url.Values{"csrf": {csrf}}); err != nil {
				c.t.Fatal(err)
			}
			resp.Body.Close()
		}
		if resp.StatusCode != http.StatusFound && resp.StatusCode != http.StatusSeeOther {
			c.t.Fatalf("guest sign-in at %s: %d", next, resp.StatusCode)
		}
		next = resp.Header.Get("Location")
	}
	back, _ := url.Parse(next)
	if back.Query().Get("code") == "" {
		c.t.Fatalf("guest sign-in ended at %s", next)
	}
	resp, err = http.PostForm(appBase+"/oauth/token", url.Values{"grant_type": {"authorization_code"}, "client_id": {reg.ClientID},
		"client_secret": {reg.ClientSecret}, "redirect_uri": {redirect}, "code": {back.Query().Get("code")}, "code_verifier": {verifier}})
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	json.NewDecoder(resp.Body).Decode(&tok)
	if tok.AccessToken == "" {
		c.t.Fatalf("no guest access token (%d)", resp.StatusCode)
	}
	return tok.AccessToken
}
