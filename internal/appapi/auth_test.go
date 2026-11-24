package appapi_test

import (
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

	"github.com/wakeshift/server/internal/appapi"
	"github.com/wakeshift/server/internal/apps"
	"github.com/wakeshift/server/internal/dbtest"
	"github.com/wakeshift/server/internal/jws"
	"github.com/wakeshift/server/internal/server"
)

// A refused assertion is a 401 that says why; a failure reading the app
// (the database, here a closed pool) is a 500 that says nothing about it.
func TestAuthenticationErrorsDontLeak(t *testing.T) {
	d, url := dbtest.NewWithURL(t)
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	hs := httptest.NewUnstartedServer(nil)
	hs.Start()
	t.Cleanup(hs.Close)
	registry := &apps.Registry{Pool: pool, Issuer: hs.URL}
	if err := registry.Sync(ctx, []apps.Config{{ID: "sources", Name: "Sources", MCPURL: "https://sources.test/mcp", PublicKey: base64.StdEncoding.EncodeToString(pub)}}); err != nil {
		t.Fatal(err)
	}
	hs.Config.Handler = (&server.Server{DB: d, Web: fstest.MapFS{}, APIs: []server.Registrar{&appapi.API{DB: d, Apps: registry}}}).Handler()

	n := 0
	assertion := func(issuer string) string {
		n++
		now := time.Now()
		tok, _ := jws.Sign(priv, "", "app+jwt", apps.Assertion{Issuer: issuer, Subject: issuer, Audience: hs.URL + "/api/apps",
			IssuedAt: now.Unix(), Expires: now.Add(time.Minute).Unix(), ID: "a" + strings.Repeat("x", n)})
		return tok
	}
	call := func(bearer, body string) (int, string) {
		t.Helper()
		req, _ := http.NewRequest("PUT", hs.URL+"/api/apps/v1/manifest", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+bearer)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	if code, body := call(assertion("nope"), "{}"); code != 401 || !strings.Contains(body, `"message":"invalid assertion"`) {
		t.Fatalf("unknown app: %d %s", code, body)
	}
	if code, body := call("a.b.c", "{}"); code != 401 || !strings.Contains(body, `"message":"invalid assertion"`) {
		t.Fatalf("garbage assertion: %d %s", code, body)
	}
	// A manifest that fails validation still says what's wrong.
	if code, body := call(assertion("sources"), `{"agent":{"wake":[{"pattern":"app.other.*"}],"ttl_days":30}}`); code != 400 || !strings.Contains(body, "bad_manifest") {
		t.Fatalf("invalid manifest: %d %s", code, body)
	}

	pool.Close()
	code, body := call(assertion("sources"), "{}")
	if code != 500 || !strings.Contains(body, `"code":"internal"`) {
		t.Fatalf("database down: %d %s", code, body)
	}
	for _, leak := range []string{"pool", "closed", "apps:", "sql", "unknown app"} {
		if strings.Contains(strings.ToLower(body), leak) {
			t.Fatalf("database down: the response says %q: %s", leak, body)
		}
	}
}
