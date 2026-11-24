// Package oauth is the core's token signing: the keys every token and
// beacon is signed with (published at /oauth/jwks), and the tokens a
// headless app acts for its agents with (headless.go).
//
// It was also "Sign in with Wakeshift", Wakeshift as the OAuth 2.1
// authorization server for apps' agents, giving an app a grant in a
// person's own workspace through Wakeshift's consent screen (account mode,
// accounts: "wakeshift"). That's turned off (October 2026): no app gets a
// grant in a person's own workspace. Its endpoints answer with a refusal
// rather than disappearing, so a client left configured for it is told
// why: authorize (invalid_target), token, including the token exchange
// (unauthorized_client), and registration (invalid_client_metadata).
// Revoking answers 200, as RFC 7009 asks: no token it could be given is
// accepted anywhere any more.
package oauth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/wakeshift/server/internal/apps"
	"github.com/wakeshift/server/internal/db"
	"github.com/wakeshift/server/internal/httpx"
	"github.com/wakeshift/server/internal/jws"
)

// exchangeTTL is how long a headless app's token for one of its agents
// lasts.
const exchangeTTL = 15 * time.Minute

// Server serves the signing keys, the headless app API's tokens and the
// refusals of Sign in with Wakeshift.
type Server struct {
	DB     *db.DB
	Issuer string // the public URL
	Apps   *apps.Registry
	// SupportEmail is where refusals tell app owners to write
	// (WAKESHIFT_SUPPORT_EMAIL; empty: apps.DefaultSupportEmail).
	SupportEmail string
	keys         *keyStore
	now          func() time.Time
}

// New returns a Server; secret (32+ bytes) encrypts signing keys at rest.
func New(d *db.DB, issuer string, secret []byte, registry *apps.Registry) (*Server, error) {
	keys, err := newKeyStore(d.Pool, secret)
	if err != nil {
		return nil, err
	}
	return &Server{DB: d, Issuer: strings.TrimSuffix(issuer, "/"), Apps: registry, keys: keys, now: time.Now}, nil
}

// Register adds the routes. /oauth/authorize itself is a page of the React
// app, which calls /api/v1/oauth/authorize and shows its refusal.
func (s *Server) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /oauth/jwks", s.jwks)
	mux.HandleFunc("POST /oauth/revoke", s.revoke)
	mux.HandleFunc("POST /oauth/register", refuseRegister)
	mux.HandleFunc("POST /oauth/token", refuseToken)
	mux.HandleFunc("GET /api/v1/oauth/authorize", refuseAuthorize)
	mux.HandleFunc("POST /api/v1/oauth/authorize", refuseAuthorize)
	s.registerHeadless(mux)
}

// accountModeOff is why Sign in with Wakeshift refuses: what every refusal
// says.
const accountModeOff = "Sign in with Wakeshift for apps is turned off: apps on Wakeshift identify their own users " +
	"(with their own sign-in), and act for their agents in workspaces the app keeps"

// refuseAuthorize answers the authorize page: no app gets a grant in a
// person's own workspace, whichever app or client asks.
func refuseAuthorize(w http.ResponseWriter, _ *http.Request) {
	httpx.WriteError(w, http.StatusBadRequest, "invalid_target", accountModeOff+".")
}

// refuseToken answers every grant type: codes and refresh tokens from
// before, and the token exchange an app acted for a person's agent with.
// A headless app's agents' tokens come from
// /api/apps/v1/workspaces/{workspace}/agents/{agent}/token.
func refuseToken(w http.ResponseWriter, _ *http.Request) {
	oauthError(w, http.StatusBadRequest, "unauthorized_client", accountModeOff+
		": act for an app's own agents with /api/apps/v1/workspaces/{workspace}/agents/{agent}/token.")
}

// refuseRegister answers client registration (RFC 7591): there's nothing
// to sign in to.
func refuseRegister(w http.ResponseWriter, _ *http.Request) {
	oauthError(w, http.StatusBadRequest, "invalid_client_metadata", accountModeOff+".")
}

func (s *Server) jwks(w http.ResponseWriter, r *http.Request) {
	keys, err := s.keys.public(r.Context())
	if err != nil {
		slog.Error("oauth: loading keys", "err", err)
		httpx.WriteError(w, http.StatusServiceUnavailable, "unavailable", "try again")
		return
	}
	out := make([]map[string]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, map[string]string{"kty": "OKP", "crv": "Ed25519", "x": b64(k.public), "kid": k.kid, "alg": "EdDSA", "use": "sig"})
	}
	w.Header().Set("Cache-Control", "public, max-age=300")
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"keys": out})
}

// Sign signs claims as a compact JWS of type typ with the current signing
// key (verifiable with /oauth/jwks), e.g. connector beacons. Not for access
// tokens: those are signed here, with their own checks.
func (s *Server) Sign(ctx context.Context, typ string, claims any) (string, error) {
	if typ == "at+jwt" {
		return "", errors.New("oauth: Sign doesn't make access tokens")
	}
	key, err := s.keys.signer(ctx)
	if err != nil {
		return "", err
	}
	return jws.Sign(key.private, key.kid, typ, claims)
}

// LoadKeys loads the signing keys (making the first, in a new database), so
// that signing never reads the database: call it at start.
func (s *Server) LoadKeys(ctx context.Context) error {
	_, err := s.keys.signer(ctx)
	return err
}

// PublicKey returns a signing key's public half, for verifying tokens.
func (s *Server) PublicKey(ctx context.Context, kid string) ([]byte, bool) {
	for attempt := 0; attempt < 2; attempt++ {
		keys, err := s.keys.public(ctx)
		if err != nil {
			return nil, false
		}
		for _, k := range keys {
			if k.kid == kid {
				return k.public, true
			}
		}
		s.keys.reloadFor(ctx, kid) // perhaps another instance's new key
	}
	return nil, false
}

// APIAudience is the audience of tokens for the app API: what a headless
// app gets for one of its agents (headless.go).
func (s *Server) APIAudience() string { return s.Issuer + "/api/apps" }

// ── Revoke (RFC 7009) ────────────────────────────────────────────────────

// revoke answers 200 for any token, as the RFC requires for unknown ones:
// the refresh tokens Sign in with Wakeshift issued are refused by the token
// endpoint (refuseToken), so there's nothing left to end.
func (s *Server) revoke(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if err := r.ParseForm(); err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_request", "bad form")
		return
	}
	w.WriteHeader(http.StatusOK)
}

// ── Helpers ──────────────────────────────────────────────────────────────

func oauthError(w http.ResponseWriter, status int, code, desc string) {
	w.Header().Set("Cache-Control", "no-store")
	httpx.WriteJSON(w, status, map[string]string{"error": code, "error_description": desc})
}

var uuidRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func randomString(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}
