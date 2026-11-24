package appapi_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/accounts"
	"github.com/wakeshift/server/internal/agents"
	"github.com/wakeshift/server/internal/appapi"
	"github.com/wakeshift/server/internal/appkey"
	"github.com/wakeshift/server/internal/apps"
	"github.com/wakeshift/server/internal/auth"
	"github.com/wakeshift/server/internal/console"
	"github.com/wakeshift/server/internal/dbtest"
	"github.com/wakeshift/server/internal/grants"
	"github.com/wakeshift/server/internal/jws"
	"github.com/wakeshift/server/internal/oauth"
	"github.com/wakeshift/server/internal/server"
	"github.com/wakeshift/server/internal/streams"
)

var ctx = context.Background()

type fixture struct {
	t    *testing.T
	base string
	key  ed25519.PrivateKey // the Sources app's
	user *http.Client
	n    int
}

func setup(t *testing.T) *fixture {
	t.Helper()
	d := dbtest.New(t)
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	hs := httptest.NewUnstartedServer(nil)
	hs.Start()
	t.Cleanup(hs.Close)
	registry := &apps.Registry{Pool: d.Pool, Issuer: hs.URL}
	if err := registry.Sync(ctx, []apps.Config{{ID: "sources", Name: "Sources", MCPURL: "https://sources.test/mcp", PublicKey: base64.StdEncoding.EncodeToString(pub)}}); err != nil {
		t.Fatal(err)
	}
	signIn := &auth.Console{Key: bytes.Repeat([]byte("k"), 32), DevSignIn: true, DevSignInToken: "appapi-test-dev-token", Accounts: &accounts.Store{Pool: d.Pool}, PublicURL: hs.URL}
	links := &streams.Server{DB: d, Key: bytes.Repeat([]byte("s"), 32), PublicURL: hs.URL}
	hs.Config.Handler = (&server.Server{DB: d, Web: fstest.MapFS{}, APIs: []server.Registrar{
		signIn, &console.API{DB: d, Auth: signIn, Streams: links, Apps: registry}, &appapi.API{DB: d, Apps: registry},
	}}).Handler()
	jar, _ := cookiejar.New(nil)
	f := &fixture{t: t, base: hs.URL, key: priv, user: &http.Client{Jar: jar}}
	f.call(f.user, "POST", "/api/v1/session/dev", "", map[string]string{"email": "ada@example.com", "token": "appapi-test-dev-token"}, nil)
	return f
}

// call sends a request; bearer, if set, is the app's assertion.
func (f *fixture) call(c *http.Client, method, path, bearer string, body, out any) int {
	f.t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(method, f.base+path, bytes.NewReader(b))
	req.Header.Set("Origin", f.base)
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := c.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer resp.Body.Close()
	if out != nil {
		json.NewDecoder(resp.Body).Decode(out)
	}
	return resp.StatusCode
}

func (f *fixture) assertion() string {
	f.n++
	now := time.Now()
	tok, _ := jws.Sign(f.key, "", "app+jwt", apps.Assertion{Issuer: "sources", Subject: "sources", Audience: f.base + "/api/apps",
		IssuedAt: now.Unix(), Expires: now.Add(time.Minute).Unix(), ID: "a" + strings.Repeat("x", f.n)})
	return tok
}

// Account mode is turned off: an app (even one of ours, approved for
// accounts) can't ask to publish into people's own workspaces or sign
// them in, the console can't connect one to a workspace, and the app API
// that redeemed connections and published into people's workspaces is
// gone. Its manifest without them is fine.
func TestNoAppReachesPeoplesWorkspaces(t *testing.T) {
	f := setup(t)
	workspace := json.RawMessage(`{"publish":[{"pattern":"app.sources.slack.*"}],"connect_url":"https://sources.test/connect","ttl_days":90}`)
	for name, manifest := range map[string]apps.Manifest{
		"workspace scope": {Workspace: &workspace},
		"accounts":        {Accounts: apps.AccountsWakeshift, Agent: &apps.AgentScope{TTLDays: 30}},
		"redirect URIs":   {Agent: &apps.AgentScope{TTLDays: 30, RedirectURIs: []string{"https://sources.test/cb"}}},
	} {
		var out struct {
			Error struct{ Code, Message string }
		}
		if code := f.call(http.DefaultClient, "PUT", "/api/apps/v1/manifest", f.assertion(), manifest, &out); code != 400 ||
			out.Error.Code != "bad_manifest" || !strings.Contains(out.Error.Message, "no longer available") {
			t.Fatalf("%s: %d %+v", name, code, out)
		}
	}
	ok := apps.Manifest{Description: "Your Slack", Agent: &apps.AgentScope{TTLDays: 30}}
	if code := f.call(http.DefaultClient, "PUT", "/api/apps/v1/manifest", f.assertion(), ok, nil); code != 200 {
		t.Fatalf("manifest: %d", code)
	}
	if code := f.call(http.DefaultClient, "PUT", "/api/apps/v1/manifest", "not-an-assertion", ok, nil); code != 401 {
		t.Fatalf("manifest without an assertion: %d", code)
	}
	for _, r := range []struct {
		c                    *http.Client
		method, path, bearer string
	}{
		{f.user, "GET", "/api/v1/apps/sources/workspace-consent", ""},
		{f.user, "POST", "/api/v1/apps/sources/connect", ""},
		{http.DefaultClient, "POST", "/api/apps/v1/connect/redeem", f.assertion()},
		{http.DefaultClient, "POST", "/api/apps/v1/workspace/events", f.assertion()},
	} {
		if code := f.call(r.c, r.method, r.path, r.bearer, map[string]string{}, nil); code != 404 && code != 405 {
			t.Errorf("%s %s: %d", r.method, r.path, code)
		}
	}
	var list struct{ Apps []any }
	if code := f.call(f.user, "GET", "/api/v1/apps", "", nil, &list); code != 200 || len(list.Apps) != 0 {
		t.Fatalf("apps: %d %+v", code, list)
	}
}

// GET /app tells an SDK whose key names no app which app and key it is: by
// the kid alone, or by the app's ID as before.
func TestWhichApp(t *testing.T) {
	f := setup(t)
	kid := appkey.KeyID(f.key.Public().(ed25519.PublicKey))
	now := time.Now()
	byKID, _ := jws.Sign(f.key, kid, "app+jwt", apps.Assertion{Issuer: kid, Subject: kid, Audience: f.base + "/api/apps",
		IssuedAt: now.Unix(), Expires: now.Add(time.Minute).Unix(), ID: "by-kid"})
	for name, tok := range map[string]string{"by kid": byKID, "by app ID": f.assertion()} {
		var got struct {
			ID, Name string
			Key      struct {
				KeyID string `json:"key_id"`
				Name  string
			}
		}
		if code := f.call(http.DefaultClient, "GET", "/api/apps/v1/app", tok, nil, &got); code != 200 ||
			got.ID != "sources" || got.Name != "Sources" || got.Key.KeyID != kid[:8] || got.Key.Name != "WAKESHIFT_APPS" {
			t.Fatalf("%s: %d %+v", name, code, got)
		}
	}
	if code := f.call(http.DefaultClient, "GET", "/api/apps/v1/app", "", nil, nil); code != 401 {
		t.Fatalf("without an assertion: %d", code)
	}
}

// An app's token for an agent works only in a workspace the app keeps: one
// naming a grant left in a person's own workspace from account mode (being
// wound down) is refused, however live the grant.
func TestAgentTokensOnlyInTheAppsOwnWorkspaces(t *testing.T) {
	d := dbtest.New(t)
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	signPub, signKey, _ := ed25519.GenerateKey(rand.Reader)
	hs := httptest.NewUnstartedServer(nil)
	hs.Start()
	t.Cleanup(hs.Close)
	registry := &apps.Registry{Pool: d.Pool, Issuer: hs.URL}
	if err := registry.Sync(ctx, []apps.Config{{ID: "agentchat", Name: "Agent Chat", PublicKey: base64.StdEncoding.EncodeToString(pub)}}); err != nil {
		t.Fatal(err)
	}
	if err := registry.SetManifest(ctx, "agentchat", apps.Manifest{Agent: &apps.AgentScope{TTLDays: 30}}); err != nil {
		t.Fatal(err)
	}
	app, _ := registry.Get(ctx, "agentchat")
	api := &appapi.API{DB: d, Apps: registry, Issuer: hs.URL,
		Keys: func(_ context.Context, kid string) ([]byte, bool) { return signPub, kid == "k1" }}
	hs.Config.Handler = (&server.Server{DB: d, Web: fstest.MapFS{}, APIs: []server.Registrar{api}}).Handler()

	var owned string
	if err := d.Pool.QueryRow(ctx, `INSERT INTO tenants (name, kind, app_id, external_id) VALUES ('ada', 'app', 'agentchat', 'ada') RETURNING id`).Scan(&owned); err != nil {
		t.Fatal(err)
	}
	persons := dbtest.NewTenant(t, d)
	var user string
	d.Pool.QueryRow(ctx, `INSERT INTO users (email) VALUES ('ada@example.com') RETURNING id`).Scan(&user)
	grantIn := func(tenant string, legacy bool) (grant string) {
		t.Helper()
		err := d.InTenant(ctx, tenant, func(tx pgx.Tx) error {
			if _, err := agents.Create(ctx, tx, "claude-code", ""); err != nil {
				return err
			}
			if legacy { // as Sign in with Wakeshift made them
				return tx.QueryRow(ctx, `INSERT INTO grants (tenant_id, app_id, kind, agent_id, scope, shown, granted_by, expires_at, confirmed_by)
					VALUES (current_tenant(), 'agentchat', 'agent', 'claude-code', '{"agent":{"wake":[],"post":[],"ttl_days":30}}', '{}', $1, now() + interval '30 days', 'user')
					RETURNING id::text`, user).Scan(&grant)
			}
			g, err := grants.EnsureByApp(ctx, tx, app, "claude-code")
			grant = g.ID
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		return grant
	}
	call := func(tenant, grant string) int {
		t.Helper()
		now := time.Now()
		tok, _ := jws.Sign(signKey, "k1", "at+jwt", oauth.AccessClaims{Issuer: hs.URL, Subject: tenant + "/claude-code", Audience: hs.URL + "/api/apps",
			ClientID: "app:agentchat", IssuedAt: now.Unix(), Expires: now.Add(time.Minute).Unix(), ID: grant, Tenant: tenant, Agent: "claude-code",
			Actor: &oauth.Actor{Subject: "app:agentchat"}, Grant: grant})
		req, _ := http.NewRequest("GET", hs.URL+"/api/apps/v1/agent/sessions", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := call(owned, grantIn(owned, false)); code != 200 {
		t.Fatalf("in its own workspace: %d", code)
	}
	if code := call(persons, grantIn(persons, true)); code != 403 {
		t.Fatalf("in a person's own workspace: %d", code)
	}
}
