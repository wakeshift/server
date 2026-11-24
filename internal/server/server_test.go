package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/wakeshift/server/internal/downloads"
)

type fakeDB struct{ err error }

func (f fakeDB) Ping(context.Context) error { return f.err }

func get(t *testing.T, h http.Handler, path string) *http.Response {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
	return rec.Result()
}

func body(r *http.Response) string { b, _ := io.ReadAll(r.Body); return string(b) }

func TestSPA(t *testing.T) {
	web := fstest.MapFS{
		"index.html":       {Data: []byte("<html>app</html>")},
		"assets/app-1a.js": {Data: []byte("js")},
	}
	h := (&Server{DB: fakeDB{}, Web: web}).Handler()

	if r := get(t, h, "/assets/app-1a.js"); body(r) != "js" || !strings.Contains(r.Header.Get("Cache-Control"), "immutable") {
		t.Fatalf("asset: %v", r.Header)
	}
	// Client-side routes get the app.
	for _, p := range []string{"/", "/consent/grant_1", "/agents"} {
		if r := get(t, h, p); body(r) != "<html>app</html>" || r.Header.Get("Cache-Control") != "no-cache" {
			t.Fatalf("%s: %d", p, r.StatusCode)
		}
	}
	// A missing asset, or an unknown server path, is a 404, not the app.
	for _, p := range []string{"/assets/app-old.js", "/auth/nope", "/v1/nope", "/admin/nope"} {
		if r := get(t, h, p); r.StatusCode != 404 {
			t.Fatalf("%s: %d", p, r.StatusCode)
		}
	}
	// Unknown API paths are JSON 404s, not the app.
	if r := get(t, h, "/api/v1/nope"); r.StatusCode != 404 || !strings.Contains(body(r), `"not_found"`) {
		t.Fatalf("unknown API path: %d", r.StatusCode)
	}
	// Nothing may frame the app.
	if r := get(t, h, "/"); !strings.Contains(r.Header.Get("Content-Security-Policy"), "frame-ancestors 'none'") {
		t.Fatal("missing frame-ancestors")
	}
}

// oauthRoutes stands in for internal/oauth's routes that are still served.
type oauthRoutes struct{}

func (oauthRoutes) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /oauth/jwks", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte(`{"keys":[]}`)) })
	mux.HandleFunc("POST /oauth/revoke", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte(`{}`)) })
}

// OAuth discovery and endpoints that are gone, or never were, are JSON
// 404s, not the app's HTML with 200; the routes still served answer, and
// /oauth/authorize is the app's page.
func TestRemovedOAuthPathsAreNotFound(t *testing.T) {
	web := fstest.MapFS{"index.html": {Data: []byte("<html>app</html>")}}
	h := (&Server{DB: fakeDB{}, Web: web, APIs: []Registrar{oauthRoutes{}}}).Handler()
	for _, p := range []string{
		"/.well-known/oauth-authorization-server",
		"/.well-known/oauth-authorization-server/mcp",
		"/.well-known/oauth-protected-resource",
		"/.well-known/openid-configuration",
		"/oauth/register",
		"/oauth/token",
		"/oauth/nope",
	} {
		r := get(t, h, p)
		var e struct {
			Error struct{ Code, Message string }
		}
		if err := json.NewDecoder(r.Body).Decode(&e); err != nil || r.StatusCode != 404 || e.Error.Code != "not_found" ||
			!strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
			t.Fatalf("%s: %d %s %+v %v", p, r.StatusCode, r.Header.Get("Content-Type"), e, err)
		}
	}
	if r := get(t, h, "/oauth/jwks"); r.StatusCode != 200 || body(r) != `{"keys":[]}` {
		t.Fatalf("jwks: %d", r.StatusCode)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/oauth/revoke", nil))
	if rec.Code != 200 || rec.Body.String() != `{}` {
		t.Fatalf("revoke: %d", rec.Code)
	}
	for _, method := range []string{"GET", "HEAD"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(method, "/oauth/authorize?client_id=x", nil))
		if rec.Code != 200 || (method == "GET" && rec.Body.String() != "<html>app</html>") {
			t.Fatalf("%s authorize page: %d %q", method, rec.Code, rec.Body.String())
		}
	}
}

// An earlier desktop app's installer and zip are gone: JSON 404s, never the app's
// HTML with 200, which `curl … | sh` would run. The harness installers still
// answer.
func TestRemovedDownloadsAreNotFound(t *testing.T) {
	web := fstest.MapFS{"index.html": {Data: []byte("<html>app</html>")}}
	dl := &downloads.Server{PublicURL: "https://wakeshift.test"}
	h := (&Server{DB: fakeDB{}, Web: web, Routes: dl.Routes()}).Handler()
	for _, p := range []string{"/install/mac.sh", "/download/wakeshift-mac.zip", "/download/", "/install/a/b.sh"} {
		r := get(t, h, p)
		if b := body(r); r.StatusCode != 404 || strings.Contains(b, "<html>") || strings.Contains(b, "#!/bin/sh") {
			t.Fatalf("%s: %d %q", p, r.StatusCode, b)
		}
	}
	if r := get(t, h, "/install/codex.sh"); r.StatusCode != 200 || !strings.HasPrefix(body(r), "#!/bin/sh") {
		t.Fatalf("codex.sh: %d", r.StatusCode)
	}
}

func TestSPANotBuilt(t *testing.T) {
	h := (&Server{DB: fakeDB{}, Web: fstest.MapFS{}}).Handler()
	if r := get(t, h, "/"); r.StatusCode != 404 || !strings.Contains(body(r), "make web") {
		t.Fatalf("unbuilt app: %d", r.StatusCode)
	}
}

func TestReadyAndStatus(t *testing.T) {
	up := (&Server{DB: fakeDB{}, Web: fstest.MapFS{}}).Handler()
	down := (&Server{DB: fakeDB{err: errors.New("down")}, Web: fstest.MapFS{}}).Handler()
	if r := get(t, up, "/ready"); r.StatusCode != 200 {
		t.Fatalf("ready: %d", r.StatusCode)
	}
	if r := get(t, down, "/ready"); r.StatusCode != 503 {
		t.Fatalf("ready with database down: %d", r.StatusCode)
	}
	if r := get(t, down, "/health"); r.StatusCode != 200 {
		t.Fatal("health shouldn't depend on the database")
	}
	var st struct {
		Version  string
		Database bool
	}
	json.NewDecoder(get(t, up, "/api/v1/status").Body).Decode(&st)
	if st.Version == "" || !st.Database {
		t.Fatalf("status: %+v", st)
	}
}

// An invite's code is a credential: the request log names the route, not
// the code. Query strings are never logged.
func TestRequestLogOmitsSecrets(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	h := (&Server{DB: fakeDB{}, Web: fstest.MapFS{}, Routes: map[string]http.Handler{
		"GET /api/v1/invites/{code}": http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}),
		"GET /v1/publish/{token}":    http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}),
	}}).Handler()
	get(t, h, "/api/v1/invites/inv-s3cret")
	get(t, h, "/api/v1/status?token=t0ken")
	get(t, h, "/v1/publish/wsp_s3cret2")
	if out := logs.String(); strings.Contains(out, "s3cret") || strings.Contains(out, "t0ken") ||
		!strings.Contains(out, "path=/api/v1/invites/{code}") || !strings.Contains(out, "path=/api/v1/status ") || !strings.Contains(out, "path=/v1/publish/{token}") {
		t.Fatalf("request log:\n%s", out)
	}
}

// A request on a credential path that its route doesn't take (a GET on a
// publish URL from a link unfurler, a trailing slash, another method on an
// invite) falls to another route, and still never logs the secret.
func TestRequestLogOmitsSecretsOffRoute(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	h := (&Server{DB: fakeDB{}, Web: fstest.MapFS{"index.html": {Data: []byte("app")}}, Routes: map[string]http.Handler{
		"GET /api/v1/invites/{code}": http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}),
		"POST /v1/publish/{token}":   http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}),
	}}).Handler()
	get(t, h, "/v1/publish/wsp_s3cret1")
	get(t, h, "/v1/publish/wsp_s3cret2/")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("HEAD", "/v1/publish/wsp_s3cret3", nil))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("DELETE", "/api/v1/invites/inv-s3cret4", nil))
	if out := logs.String(); strings.Contains(out, "s3cret") || strings.Count(out, "path=/v1/publish/{token}") != 3 || !strings.Contains(out, "path=/api/v1/invites/{code}") {
		t.Fatalf("request log:\n%s", out)
	}
}

// A credential path that isn't in canonical form (a base URL ending in a
// slash, plus the path) matches no route: the mux redirects it to the clean
// path, and a client following the 307 publishes as usual. The log still
// never shows the secret.
func TestRequestLogOmitsSecretsInUncleanPaths(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	h := (&Server{DB: fakeDB{}, Web: fstest.MapFS{}, Routes: map[string]http.Handler{
		"POST /api/v1/invites/{code}": http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}),
		"POST /v1/publish/{token}":    http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}),
	}}).Handler()
	for _, p := range []string{"/v1//publish/wsp_s3cret1", "//v1/publish/wsp_s3cret2", "/v1/./publish/wsp_s3cret3", "/api/v1//invites/inv-s3cret4", "/api//v1/invites/inv-s3cret5"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", p, nil))
	}
	if out := logs.String(); strings.Contains(out, "s3cret") || strings.Count(out, "path=/v1/publish/{token}") != 3 || strings.Count(out, "path=/api/v1/invites/{code}") != 2 {
		t.Fatalf("request log:\n%s", out)
	}
}

// On the console's host, the site's docs for coding agents redirect to the
// site, paths that look like files are 404s rather than the app's HTML,
// robots.txt keeps crawlers out, and the app's own routes still work.
func TestDiscoveryPaths(t *testing.T) {
	web := fstest.MapFS{
		"index.html":    {Data: []byte("<html>app</html>")},
		"wakeshift.svg": {Data: []byte("<svg/>")},
	}
	h := (&Server{DB: fakeDB{}, Web: web, SiteURL: "https://wakeshift.ai"}).Handler()

	for p, want := range map[string]string{
		"/llms.txt":      "https://wakeshift.ai/llms.txt",
		"/llms-full.txt": "https://wakeshift.ai/llms-full.txt",
		"/AGENTS.md":     "https://wakeshift.ai/AGENTS.md",
		"/agents.md":     "https://wakeshift.ai/AGENTS.md",
		"/Agents.MD":     "https://wakeshift.ai/AGENTS.md",
	} {
		r := get(t, h, p)
		if r.StatusCode != http.StatusMovedPermanently || r.Header.Get("Location") != want {
			t.Errorf("%s: %d to %q, want 301 to %q", p, r.StatusCode, r.Header.Get("Location"), want)
		}
	}
	// No site known (a local console): 404s, not the app.
	local := (&Server{DB: fakeDB{}, Web: web}).Handler()
	for _, p := range []string{"/llms.txt", "/AGENTS.md"} {
		if r := get(t, local, p); r.StatusCode != 404 || strings.Contains(body(r), "app") {
			t.Errorf("%s with no site: %d", p, r.StatusCode)
		}
	}

	for _, p := range []string{"/sitemap.xml", "/index.md", "/docs/events.md", "/README.md", "/humans.txt", "/ads.txt", "/apps/x/notes.txt", "/wp-login.php", "/config.json"} {
		if r := get(t, h, p); r.StatusCode != 404 || strings.Contains(body(r), "<html>app</html>") {
			t.Errorf("%s: %d, want a 404 without the app", p, r.StatusCode)
		}
	}

	r := get(t, h, "/robots.txt")
	if b := body(r); r.StatusCode != 200 || !strings.HasPrefix(r.Header.Get("Content-Type"), "text/plain") || !strings.Contains(b, "User-agent: *\nAllow: /$\nDisallow: /\n") || strings.Contains(b, "<html") {
		t.Errorf("robots.txt: %d %q", r.StatusCode, b)
	}
	// Crawlers may read the root, to find it's noindex, as is every page.
	for _, p := range []string{"/", "/apps/my-app/keys", "/oauth/authorize"} {
		if r := get(t, h, p); r.Header.Get("X-Robots-Tag") != "noindex, nofollow" {
			t.Errorf("%s: X-Robots-Tag %q", p, r.Header.Get("X-Robots-Tag"))
		}
	}

	// The app's routes, deep links included, and its files still work.
	for _, p := range []string{"/", "/new-app", "/apps/my-app/keys", "/apps/my-app/overview", "/orgs/0b6c2a52-3f4e-4c5d-9a1b-2c3d4e5f6a7b/members", "/consent/grant_1", "/join", "/oauth/authorize"} {
		if r := get(t, h, p); r.StatusCode != 200 || body(r) != "<html>app</html>" {
			t.Errorf("%s: %d, want the app", p, r.StatusCode)
		}
	}
	if r := get(t, h, "/wakeshift.svg"); r.StatusCode != 200 || body(r) != "<svg/>" {
		t.Errorf("a file in the build: %d", r.StatusCode)
	}
}
