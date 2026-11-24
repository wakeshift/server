package oauth_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/wakeshift/server/internal/accounts"
	"github.com/wakeshift/server/internal/apps"
	"github.com/wakeshift/server/internal/auth"
	"github.com/wakeshift/server/internal/db"
	"github.com/wakeshift/server/internal/dbtest"
	"github.com/wakeshift/server/internal/oauth"
	"github.com/wakeshift/server/internal/server"
)

const appMCP = "https://chat.example/mcp"

// An Ed25519 public key (the private half isn't needed here).
const testAppKey = "11qYAYKxCrfVS/7TyWQHOg7hcvPapiMlrwIaaPcHURo="

type fixture struct {
	t    *testing.T
	d    *db.DB
	base string
	user *http.Client // a signed-in browser
}

func setup(t *testing.T) *fixture {
	t.Helper()
	d := dbtest.New(t)
	registry := &apps.Registry{Pool: d.Pool}
	if err := registry.Sync(context.Background(), []apps.Config{{ID: "agentchat", Name: "Agent Chat", MCPURL: appMCP, PublicKey: testAppKey}}); err != nil {
		t.Fatal(err)
	}
	signIn := &auth.Console{Key: bytes.Repeat([]byte("k"), 32), DevSignIn: true, DevSignInToken: "oauth-test-dev-sign-in", Accounts: &accounts.Store{Pool: d.Pool}}
	hs := httptest.NewUnstartedServer(nil)
	hs.Start()
	t.Cleanup(hs.Close)
	signIn.PublicURL = hs.URL
	registry.Issuer = hs.URL
	srv, err := oauth.New(d, hs.URL, bytes.Repeat([]byte("o"), 32), registry)
	if err != nil {
		t.Fatal(err)
	}
	hs.Config.Handler = (&server.Server{DB: d, Web: fstest.MapFS{}, APIs: []server.Registrar{signIn, srv}}).Handler()
	f := &fixture{t: t, d: d, base: hs.URL}
	jar, _ := cookiejar.New(nil)
	f.user = &http.Client{Jar: jar}
	if code, _ := f.json(f.user, "POST", "/api/v1/session/dev", map[string]string{"email": "ada@example.com", "token": "oauth-test-dev-sign-in"}); code != 200 {
		t.Fatalf("sign in: %d", code)
	}
	return f
}

func (f *fixture) json(c *http.Client, method, path string, body any) (int, map[string]any) {
	f.t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(method, f.base+path, bytes.NewReader(b))
	req.Header.Set("Origin", f.base)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// form posts to the token or revoke endpoint, as a client would.
func (f *fixture) form(path string, v url.Values) (int, map[string]any) {
	f.t.Helper()
	resp, err := http.PostForm(f.base+path, v)
	if err != nil {
		f.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// Sign in with Wakeshift for apps is turned off: registration, the
// authorize page (signed in or not, for an app's MCP URL or an app signing
// users in itself) and every grant type at the token endpoint, the token
// exchange included, refuse with a reason, and the authorization server's
// metadata is gone. The signing keys are still published.
func TestSignInWithWakeshiftIsOff(t *testing.T) {
	f := setup(t)
	code, out := f.json(http.DefaultClient, "POST", "/oauth/register", map[string]any{"client_name": "Codex", "redirect_uris": []string{"http://127.0.0.1:9/cb"}})
	if code != 400 || out["error"] != "invalid_client_metadata" || !strings.Contains(out["error_description"].(string), "turned off") {
		t.Fatalf("register: %d %v", code, out)
	}
	query := url.Values{"response_type": {"code"}, "client_id": {"app:agentchat"}, "redirect_uri": {"https://chat.example/cb"},
		"code_challenge": {strings.Repeat("a", 43)}, "code_challenge_method": {"S256"}, "resource": {appMCP}}.Encode()
	for _, c := range []*http.Client{f.user, http.DefaultClient} {
		for _, method := range []string{"GET", "POST"} {
			path, body := "/api/v1/oauth/authorize?"+query, any(nil)
			if method == "POST" {
				path, body = "/api/v1/oauth/authorize", map[string]any{"query": query, "decision": "allow", "agent": "codex", "new_agent": true}
			}
			code, out := f.json(c, method, path, body)
			e, _ := out["error"].(map[string]any)
			if code != 400 || e["code"] != "invalid_target" || !strings.Contains(e["message"].(string), "turned off") {
				t.Fatalf("authorize %s (signed in: %v): %d %v", method, c == f.user, code, out)
			}
		}
	}
	for _, form := range []url.Values{
		{"grant_type": {"authorization_code"}, "code": {"wsc_x_y"}, "client_id": {"wsk_x"}},
		{"grant_type": {"refresh_token"}, "refresh_token": {"wsr_x_y"}, "client_id": {"wsk_x"}},
		{"grant_type": {"urn:ietf:params:oauth:grant-type:token-exchange"}, "subject_token": {"x"},
			"subject_token_type": {"urn:ietf:params:oauth:token-type:access_token"}},
		{},
	} {
		code, out := f.form("/oauth/token", form)
		if code != 400 || out["error"] != "unauthorized_client" || !strings.Contains(out["error_description"].(string), "/agents/{agent}/token") {
			t.Fatalf("token %v: %d %v", form["grant_type"], code, out)
		}
	}
	for _, p := range []string{"/.well-known/oauth-authorization-server", "/.well-known/oauth-authorization-server/mcp"} {
		resp, err := http.Get(f.base + p)
		if err != nil {
			t.Fatal(err)
		}
		var e struct{ Error struct{ Code string } }
		json.NewDecoder(resp.Body).Decode(&e)
		resp.Body.Close()
		if resp.StatusCode != 404 || e.Error.Code != "not_found" {
			t.Fatalf("%s: %d %+v", p, resp.StatusCode, e)
		}
	}
	var jwks struct{ Keys []map[string]string }
	resp, err := http.Get(f.base + "/oauth/jwks")
	if err != nil {
		t.Fatal(err)
	}
	json.NewDecoder(resp.Body).Decode(&jwks)
	resp.Body.Close()
	if resp.StatusCode != 200 || len(jwks.Keys) == 0 || jwks.Keys[0]["crv"] != "Ed25519" {
		t.Fatalf("jwks: %d %v", resp.StatusCode, jwks)
	}
}

// Revoking answers 200 for any token (RFC 7009): nothing accepts the
// refresh tokens Sign in with Wakeshift issued.
func TestRevokeAnswersOK(t *testing.T) {
	f := setup(t)
	for _, form := range []url.Values{{"token": {"wsr_x_y"}, "client_id": {"wsk_codex"}}, {"token": {"anything"}}, {}} {
		if code, _ := f.form("/oauth/revoke", form); code != 200 {
			t.Fatalf("revoke %v: %d", form, code)
		}
	}
}
