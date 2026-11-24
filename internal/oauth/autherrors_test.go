package oauth_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/wakeshift/server/internal/accounts"
	"github.com/wakeshift/server/internal/apps"
	"github.com/wakeshift/server/internal/auth"
	"github.com/wakeshift/server/internal/dbtest"
	"github.com/wakeshift/server/internal/jws"
	"github.com/wakeshift/server/internal/oauth"
	"github.com/wakeshift/server/internal/server"
)

// An app's refused assertion is a 401 that says why; a failure reading the
// app (the database, here a closed pool) is a 500 that says nothing about
// it: on the headless app API.
func TestAppAuthenticationErrorsDontLeak(t *testing.T) {
	d, dbURL := dbtest.NewWithURL(t)
	pool, err := pgxpool.New(context.Background(), dbURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	hs := httptest.NewUnstartedServer(nil)
	hs.Start()
	t.Cleanup(hs.Close)
	registry := &apps.Registry{Pool: pool, Issuer: hs.URL}
	if err := registry.Sync(context.Background(), []apps.Config{{ID: "agentchat", Name: "Agent Chat", MCPURL: appMCP, PublicKey: base64.StdEncoding.EncodeToString(pub)}}); err != nil {
		t.Fatal(err)
	}
	signIn := &auth.Console{Key: bytes.Repeat([]byte("k"), 32), DevSignIn: true, Accounts: &accounts.Store{Pool: d.Pool}, PublicURL: hs.URL}
	srv, err := oauth.New(d, hs.URL, bytes.Repeat([]byte("o"), 32), registry)
	if err != nil {
		t.Fatal(err)
	}
	hs.Config.Handler = (&server.Server{DB: d, Web: fstest.MapFS{}, APIs: []server.Registrar{signIn, srv}}).Handler()

	n := 0
	assertion := func(issuer string) string {
		n++
		now := time.Now()
		tok, _ := jws.Sign(priv, "", "app+jwt", apps.Assertion{Issuer: issuer, Subject: issuer, Audience: hs.URL + "/api/apps",
			IssuedAt: now.Unix(), Expires: now.Add(time.Minute).Unix(), ID: "a" + strings.Repeat("x", n)})
		return tok
	}
	read := func(resp *http.Response, err error) (int, string) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	headless := func(issuer string) (int, string) {
		req, _ := http.NewRequest("POST", hs.URL+"/api/apps/v1/workspaces", strings.NewReader(`{"external_id":"u1"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+assertion(issuer))
		return read(http.DefaultClient.Do(req))
	}

	if code, body := headless("nope"); code != 401 || !strings.Contains(body, `"message":"invalid assertion"`) {
		t.Fatalf("headless API, unknown app: %d %s", code, body)
	}

	pool.Close()
	noLeak := func(what string, code int, body, want string) {
		t.Helper()
		if code != 500 || !strings.Contains(body, want) {
			t.Fatalf("%s, database down: %d %s", what, code, body)
		}
		for _, leak := range []string{"pool", "closed", "apps:", "sql", "unknown app"} {
			if strings.Contains(strings.ToLower(body), leak) {
				t.Fatalf("%s, database down: the response says %q: %s", what, leak, body)
			}
		}
	}
	code, body := headless("agentchat")
	noLeak("headless API", code, body, `"code":"internal"`)
}
