// Package server is Wakeshift's HTTP server: the JSON API under /api, health
// checks, and the React app for every other path.
package server

import (
	"context"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/wakeshift/server/internal/httpx"
	"github.com/wakeshift/server/internal/version"
)

// Pinger reports whether the database is reachable.
type Pinger interface {
	Ping(ctx context.Context) error
}

// Server holds the HTTP server's dependencies.
type Server struct {
	DB  Pinger
	Web fs.FS // the built React app (web/dist)
	// APIs register their routes under /api/v1 (console sign-in, the
	// console's admin API).
	APIs []Registrar
	// HSTS makes browsers use https for this host from now on; set it when
	// the public URL is https.
	HSTS bool
	// Routes are other handlers by mux pattern, e.g. "GET /v1/stream".
	Routes map[string]http.Handler
	// SiteURL is the public site's URL (https://wakeshift.ai), where the
	// docs for coding agents live: /llms.txt, /llms-full.txt and /AGENTS.md
	// redirect there. Empty: they're 404s.
	SiteURL string
	// SupportEmail is the deployment's support address
	// (WAKESHIFT_SUPPORT_EMAIL), the contact /.well-known/security.txt
	// gives; empty: only the repository's security advisories.
	SupportEmail string
}

// Registrar adds routes to the server's mux.
type Registrar interface {
	Register(mux *http.ServeMux)
}

// Handler returns the server's routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("GET /ready", s.ready)
	mux.HandleFunc("GET /api/v1/status", s.status)
	for _, api := range s.APIs {
		api.Register(mux)
	}
	for pattern, h := range s.Routes {
		mux.Handle(pattern, h)
	}
	mux.HandleFunc("/api/", func(w http.ResponseWriter, _ *http.Request) {
		httpx.WriteError(w, http.StatusNotFound, "not_found", "no such API endpoint")
	})
	// OAuth discovery and endpoints that are gone (Sign in with Wakeshift's
	// authorization server metadata) or never were: a JSON 404, so a client
	// probing them isn't handed the app's HTML. The routes registered above
	// (JWKS, revoke, the refusals) are more specific and still answer.
	// /oauth/authorize is the app's page, which shows the refusal.
	notFound := func(w http.ResponseWriter, _ *http.Request) {
		httpx.WriteError(w, http.StatusNotFound, "not_found", "not found")
	}
	app := spa(s.Web)
	mux.HandleFunc("GET /robots.txt", robots(s.SiteURL))
	mux.HandleFunc("GET /.well-known/security.txt", securityTxt(s.SupportEmail))
	mux.HandleFunc("/.well-known/", notFound)
	mux.HandleFunc("/oauth/", notFound)
	// Downloads and install scripts the routes above don't serve (Wakeshift
	// for Mac's, removed) are 404s too, never the app's HTML, which curl
	// would save or pipe to sh.
	mux.HandleFunc("/download/", notFound)
	mux.HandleFunc("/install/", notFound)
	mux.Handle("GET /oauth/authorize", app)
	mux.Handle("/", discovery(s.SiteURL, app))
	return bodyDeadline(securityHeaders(s.HSTS, logRequests(mux)))
}

// BodyReadTimeout is how long a request has to send its body, from when
// its handler starts (the headers have ReadHeaderTimeout, cmd/wakeshift):
// a client that trickles a body in a byte at a time is cut off rather than
// holding a connection and a handler for good. Every body the server takes
// is small. A variable so tests can shorten it.
var BodyReadTimeout = 30 * time.Second

// bodyDeadline gives a request with a body BodyReadTimeout to send it: the
// connection's read deadline, cleared once the body has been read to its
// end. Only then does the server read the connection again (to notice the
// client going away, which cancels the request's context), so a handler
// that reads its body and then holds the request open (a long poll, a
// wait, an MCP push stream) isn't cut off, nor its context canceled, by
// the deadline. A request without a body (a WebSocket upgrade, a GET poll)
// gets no deadline. The server sets the connection's deadlines afresh for
// its next request.
func bodyDeadline(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body == nil || r.Body == http.NoBody || BodyReadTimeout <= 0 {
			next.ServeHTTP(w, r)
			return
		}
		rc := http.NewResponseController(w)
		if err := rc.SetReadDeadline(time.Now().Add(BodyReadTimeout)); err != nil {
			next.ServeHTTP(w, r) // a writer without deadlines (a test's recorder)
			return
		}
		r.Body = &deadlineBody{ReadCloser: r.Body, rc: rc}
		next.ServeHTTP(w, r)
	})
}

// deadlineBody clears the connection's read deadline once the body has
// been read to its end (bodyDeadline). net/http clears it too as it starts
// its background read there; this doesn't depend on that.
type deadlineBody struct {
	io.ReadCloser
	rc      *http.ResponseController
	cleared bool
}

func (b *deadlineBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err == io.EOF && !b.cleared {
		b.cleared = true
		b.rc.SetReadDeadline(time.Time{})
	}
	return n, err
}

func (s *Server) ready(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.DB.Ping(ctx); err != nil {
		slog.Warn("ready: database unreachable", "err", err)
		http.Error(w, "database unreachable", http.StatusServiceUnavailable)
		return
	}
	w.Write([]byte("ok"))
}

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"version":  version.Version,
		"database": s.DB.Ping(ctx) == nil,
	})
}

// securityHeaders applies to every response. frame-ancestors 'none' matters
// most: consent and approval screens must never be framed (clickjacking).
func securityHeaders(hsts bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		if hsts {
			h.Set("Strict-Transport-Security", "max-age=31536000")
		}
		h.Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// Unwrap lets http.ResponseController reach the underlying writer
// (WebSocket upgrades, flushing).
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// logRequests logs requests. It never logs query strings or headers, which
// can carry tokens, nor path segments that are secrets (an invite's code).
func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		if r.URL.Path == "/health" || r.URL.Path == "/ready" {
			return
		}
		// route is the pattern the mux matched ("" if none): the only
		// form of the path telemetry exports (internal/telemetry). The
		// request's context links the line to its trace.
		route := ""
		if _, rest, ok := strings.Cut(r.Pattern, "/"); ok {
			route = "/" + rest
		}
		slog.InfoContext(r.Context(), "http", "method", r.Method, "path", loggedPath(r), "route", route, "status", rec.status, "ms", time.Since(start).Milliseconds())
	})
}

// secretWildcards are route wildcards whose values are credentials.
var secretWildcards = []string{"code", "token"} // token: a publish URL (appapi)

// SecretPaths are path prefixes whose rest is a credential, whatever route
// the request matched (a GET on a publish URL, a method no route takes):
// the log shows the prefix and the placeholder. Each is also kept out of
// the hosted deployment's request logs.
var SecretPaths = map[string]string{
	"/v1/publish/":     "{token}",
	"/api/v1/invites/": "{code}",
}

// loggedPath is the request's path with secrets replaced by placeholders:
// whatever follows a SecretPaths prefix, and secret wildcards' values (the
// mux has set r.Pattern by now). The prefixes are matched on the cleaned
// path too: the mux redirects /v1//publish/<token> (a base URL ending in a
// slash, plus a path) to the clean path, so no route matched it.
func loggedPath(r *http.Request) string {
	p := r.URL.Path
	clean := path.Clean("/" + p)
	for prefix, placeholder := range SecretPaths {
		if strings.HasPrefix(p, prefix) && len(p) > len(prefix) || strings.HasPrefix(clean, prefix) && len(clean) > len(prefix) {
			return prefix + placeholder
		}
	}
	for _, name := range secretWildcards {
		if v := r.PathValue(name); v != "" && strings.Contains(r.Pattern, "{"+name+"}") {
			p = strings.ReplaceAll(p, v, "{"+name+"}")
		}
	}
	return p
}
