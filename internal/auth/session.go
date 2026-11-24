// Package auth signs people in to the console and keeps them signed in.
//
// People sign in with Google (OpenID Connect) or GitHub, or, on a local
// development server only, with just an email. Anyone can sign up while
// there are fewer accounts than the sign-up cap; past it, a new sign-in joins
// the waitlist (waitlist.go), unless an invite link (each user has one to
// share), an organization invite or the allow list admits them. A session is a signed cookie naming the
// user and the workspace they're working in, so it works across instances
// without a table; every request also checks the user is still a member of
// that workspace, so removing someone takes effect at once.
//
// Requests that change anything must come from the console's own origin: the
// cookie is SameSite=Strict, and unsafe methods also need a matching Origin
// header, which browsers send on every cross-site request.
//
// There is no bearer token for the console API: the device tokens Wakeshift
// for Mac used are gone, and an Authorization header signs nobody in.
package auth

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/wakeshift/server/internal/accounts"
	"github.com/wakeshift/server/internal/apps"
	"github.com/wakeshift/server/internal/httpx"
)

const (
	sessionCookie = "ws_session"
	sessionTTL    = 12 * time.Hour
)

// Console holds the console's sign-in configuration.
type Console struct {
	Key       []byte // signs cookies
	PublicURL string // the console's origin; cookies are Secure when it's https
	Accounts  *accounts.Store
	Google    *Google // nil when Google sign-in isn't configured
	GitHub    *GitHub // nil when GitHub sign-in isn't configured
	// DevSignIn allows signing in with just an email and DevSignInToken.
	// It only works when PublicURL is a localhost URL and the token is at
	// least MinDevTokenLen long.
	DevSignIn      bool
	DevSignInToken string
	// The links and address the console shows (Site): the public site
	// (empty: Wakeshift's docs), the Terms new accounts accept and the
	// Privacy Policy beside them (empty: none), and where app owners write
	// (empty: apps.DefaultSupportEmail).
	SiteURL, TermsURL, PrivacyURL, SupportEmail string
	Now                                         func() time.Time
}

// PublicDocs is where the console's Docs links go on a server with no site
// of its own (Console.SiteURL): Wakeshift's documentation.
const PublicDocs = "https://wakeshift.ai/docs"

// siteOut is what the console links to and names (GET /api/v1/session's
// site).
type siteOut struct {
	Docs string `json:"docs"`
	// Terms and Privacy: none on a server without Terms, which then shows
	// no "you agree" line.
	Terms        string `json:"terms,omitempty"`
	Privacy      string `json:"privacy,omitempty"`
	SupportEmail string `json:"support_email"`
}

func (c *Console) site() siteOut {
	docs := PublicDocs
	if c.SiteURL != "" {
		docs = strings.TrimSuffix(c.SiteURL, "/") + "/docs"
	}
	return siteOut{Docs: docs, Terms: c.TermsURL, Privacy: c.PrivacyURL, SupportEmail: apps.Support(c.SupportEmail)}
}

type claims struct {
	User   string `json:"u"`
	Tenant string `json:"t"`
	Epoch  int    `json:"g"` // the user's session generation when issued
	Exp    int64  `json:"e"`
}

type ctxKey struct{}

type principal struct {
	user      accounts.User
	workspace accounts.Workspace
}

// TenantFrom returns the signed-in user's current workspace (tenant).
func TenantFrom(ctx context.Context) string {
	p, _ := ctx.Value(ctxKey{}).(principal)
	return p.workspace.ID
}

// UserFrom returns the signed-in user.
func UserFrom(ctx context.Context) accounts.User {
	p, _ := ctx.Value(ctxKey{}).(principal)
	return p.user
}

// Register adds the sign-in routes.
func (c *Console) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/session", c.session)
	mux.HandleFunc("DELETE /api/v1/session", c.Require(c.signOut))
	mux.HandleFunc("POST /api/v1/session/workspace", c.Require(c.switchWorkspace))
	if c.devSignInAllowed() {
		slog.Warn("auth: development sign-in is on (WAKESHIFT_DEV_SIGN_IN): anyone on this machine can sign in as any email")
		mux.HandleFunc("POST /api/v1/session/dev", c.devSignIn)
	} else if c.DevSignIn {
		slog.Warn("auth: WAKESHIFT_DEV_SIGN_IN is ignored: WAKESHIFT_PUBLIC_URL isn't a localhost URL, or there's no sign-in token")
	}
	if c.Google != nil {
		mux.HandleFunc("GET /auth/google/start", c.googleStart)
		mux.HandleFunc("GET /auth/google/callback", c.googleCallback)
	}
	if c.GitHub != nil {
		mux.HandleFunc("GET /auth/github/start", c.githubStart)
		mux.HandleFunc("GET /auth/github/callback", c.githubCallback)
	}
	mux.HandleFunc("GET /api/v1/invite", c.Require(c.invite))
	mux.HandleFunc("GET /api/v1/invites/{code}", c.checkInvite)
	mux.HandleFunc("POST /api/v1/join/sign-in", c.joinSignIn)
	mux.HandleFunc("POST /api/v1/waitlist/note", c.waitlistNote)
	mux.HandleFunc("POST /api/v1/waitlist/key", c.waitlistKey)
}

// Require wraps a handler that needs a signed-in user, adding them and
// their workspace to the request's context.
func (c *Console) Require(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !c.sameOrigin(r) {
			httpx.WriteError(w, http.StatusForbidden, "bad_origin", "request must come from the console")
			return
		}
		p, ok := c.current(r)
		if !ok {
			httpx.WriteError(w, http.StatusUnauthorized, "unauthenticated", "sign in to the console")
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, p)))
	}
}

// current is the request's signed-in user, if the cookie is valid and they
// still belong to its workspace.
func (c *Console) current(r *http.Request) (principal, bool) {
	cl, ok := c.read(r)
	if !ok {
		return principal{}, false
	}
	u, ws, err := c.Accounts.Member(r.Context(), cl.User, cl.Tenant)
	if err != nil {
		if !errors.Is(err, accounts.ErrNotMember) {
			slog.Warn("auth: checking membership failed", "err", err)
		}
		return principal{}, false
	}
	if cl.Epoch != u.SessionEpoch {
		return principal{}, false // signed out everywhere since
	}
	return principal{user: u, workspace: ws}, true
}

type sessionOut struct {
	Authenticated bool                 `json:"authenticated"`
	User          *accounts.User       `json:"user,omitempty"`
	Workspace     *accounts.Workspace  `json:"workspace,omitempty"`
	Workspaces    []accounts.Workspace `json:"workspaces,omitempty"`
	SignIn        map[string]bool      `json:"sign_in"` // available methods
	Site          siteOut              `json:"site"`
	// Waitlist: signed out, and on the waitlist (from their last sign-in).
	Waitlist *accounts.WaitlistEntry `json:"waitlist,omitempty"`
}

func (c *Console) session(w http.ResponseWriter, r *http.Request) {
	out := sessionOut{SignIn: map[string]bool{"google": c.Google != nil, "github": c.GitHub != nil, "dev": c.devSignInAllowed()}, Site: c.site()}
	if p, ok := c.current(r); ok {
		list, err := c.Accounts.Workspaces(r.Context(), p.user.ID)
		if err != nil {
			httpx.WriteError(w, http.StatusInternalServerError, "internal", "couldn't load your workspaces")
			return
		}
		out.Authenticated, out.User, out.Workspace, out.Workspaces = true, &p.user, &p.workspace, list
	} else {
		out.Waitlist = c.waitlist(r)
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// invite is the signed-in user's invite link, for people they'd like to
// bring.
func (c *Console) invite(w http.ResponseWriter, r *http.Request) {
	in, err := c.Accounts.UserInvite(r.Context(), UserFrom(r.Context()).ID)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "internal", "couldn't load your invite")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"code": in.Code, "uses_left": in.UsesLeft,
		"url": strings.TrimSuffix(c.PublicURL, "/") + "/?invite=" + in.Code})
}

// checkInvite says whether an invite can still be used, for the sign-in
// page.
func (c *Console) checkInvite(w http.ResponseWriter, r *http.Request) {
	code := r.PathValue("code")
	ok := false
	if len(code) <= 64 {
		var err error
		if ok, err = c.Accounts.CheckInvite(r.Context(), code); err != nil {
			httpx.WriteError(w, http.StatusInternalServerError, "internal", "couldn't check the invite")
			return
		}
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]bool{"valid": ok})
}

var workspaceIDRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func (c *Console) switchWorkspace(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID string `json:"id"`
	}
	if !httpx.Decode(w, r, &in) {
		return
	}
	if !workspaceIDRE.MatchString(in.ID) {
		httpx.WriteError(w, http.StatusBadRequest, "bad_request", "id is a workspace ID")
		return
	}
	user := UserFrom(r.Context())
	_, _, err := c.Accounts.Member(r.Context(), user.ID, in.ID)
	if errors.Is(err, accounts.ErrNotMember) {
		httpx.WriteError(w, http.StatusForbidden, "not_member", "you're not a member of that workspace")
		return
	}
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "internal", "couldn't switch workspace")
		return
	}
	c.issue(w, user, in.ID)
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"workspace": in.ID})
}

// signOut ends every session of the user, not just this browser's: a
// copied cookie stops working too.
func (c *Console) signOut(w http.ResponseWriter, r *http.Request) {
	if err := c.Accounts.SignOutEverywhere(r.Context(), UserFrom(r.Context()).ID); err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "internal", "couldn't sign out")
		return
	}
	c.clear(w, sessionCookie, "/")
	httpx.WriteJSON(w, http.StatusOK, map[string]bool{"authenticated": false})
}

// signedIn finishes any sign-in: the user works in their first workspace.
func (c *Console) signedIn(w http.ResponseWriter, r *http.Request, u accounts.User) error {
	list, err := c.Accounts.Workspaces(r.Context(), u.ID)
	if err != nil {
		return err
	}
	if len(list) == 0 {
		return errors.New("auth: user has no workspace")
	}
	c.issue(w, u, list[0].ID)
	slog.Info("auth: signed in", "user", u.ID)
	return nil
}

func (c *Console) issue(w http.ResponseWriter, u accounts.User, tenantID string) {
	exp := c.now().Add(sessionTTL)
	http.SetCookie(w, c.cookie(sessionCookie, "/", c.sign("session", claims{User: u.ID, Tenant: tenantID, Epoch: u.SessionEpoch, Exp: exp.Unix()}), exp, http.SameSiteStrictMode))
}

func (c *Console) read(r *http.Request) (claims, bool) {
	var cl claims
	ck, err := r.Cookie(sessionCookie)
	if err != nil || !c.verify("session", ck.Value, &cl) || cl.User == "" || cl.Tenant == "" || c.now().Unix() > cl.Exp {
		return claims{}, false
	}
	return cl, true
}

func (c *Console) cookie(name, path, value string, exp time.Time, sameSite http.SameSite) *http.Cookie {
	return &http.Cookie{
		Name: name, Value: value, Path: path, Expires: exp,
		HttpOnly: true, SameSite: sameSite, Secure: strings.HasPrefix(c.PublicURL, "https://"),
	}
}

func (c *Console) clear(w http.ResponseWriter, name, path string) {
	http.SetCookie(w, c.cookie(name, path, "", time.Unix(0, 0), http.SameSiteStrictMode))
}

// sign and verify protect a cookie's JSON body with an HMAC; purpose keeps
// one kind of cookie from being replayed as another.
func (c *Console) sign(purpose string, v any) string {
	body, _ := json.Marshal(v)
	b := base64.RawURLEncoding.EncodeToString(body)
	return b + "." + base64.RawURLEncoding.EncodeToString(c.mac(purpose, b))
}

func (c *Console) verify(purpose, value string, v any) bool {
	b, sig, ok := strings.Cut(value, ".")
	if !ok {
		return false
	}
	got, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil || !hmac.Equal(got, c.mac(purpose, b)) {
		return false
	}
	body, err := base64.RawURLEncoding.DecodeString(b)
	return err == nil && json.Unmarshal(body, v) == nil
}

func (c *Console) mac(purpose, b string) []byte {
	m := hmac.New(sha256.New, c.Key)
	m.Write([]byte(purpose + ":" + b))
	return m.Sum(nil)
}

// sameOrigin accepts safe methods, and unsafe ones whose Origin is the
// console's.
func (c *Console) sameOrigin(r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	switch r.Header.Get("Origin") {
	case c.origin():
		return true
	case "", "null":
		// Referrer-Policy no-referrer makes browsers send Origin: null on
		// the console's own requests; Sec-Fetch-Site, which a page can't
		// set, still says same-origin.
		return r.Header.Get("Sec-Fetch-Site") == "same-origin"
	default:
		return false
	}
}

func (c *Console) origin() string {
	u, err := url.Parse(c.PublicURL)
	if err != nil {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

func (c *Console) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}
