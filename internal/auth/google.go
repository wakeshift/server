package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/wakeshift/server/internal/accounts"
	"github.com/wakeshift/server/internal/httpx"
	"github.com/wakeshift/server/internal/orgs"
)

const (
	flowCookie = "ws_signin"
	flowPath   = "/auth/google"
	flowTTL    = 10 * time.Minute
)

// Google is "Sign in with Google": the OpenID Connect authorization code
// flow with PKCE, state and nonce.
type Google struct {
	ClientID     string
	ClientSecret string
	// Issuer is Google's; tests point it at a fake provider.
	Issuer string

	mu       sync.Mutex
	provider *oidc.Provider
}

func (g *Google) issuer() string {
	if g.Issuer != "" {
		return g.Issuer
	}
	return "https://accounts.google.com"
}

// discover fetches the provider's configuration once (retrying after a
// failure, e.g. no network at startup).
func (g *Google) discover(ctx context.Context) (*oidc.Provider, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.provider != nil {
		return g.provider, nil
	}
	p, err := oidc.NewProvider(ctx, g.issuer())
	if err != nil {
		return nil, err
	}
	g.provider = p
	return p, nil
}

func (c *Console) oauthConfig(p *oidc.Provider) *oauth2.Config {
	return &oauth2.Config{
		ClientID:     c.Google.ClientID,
		ClientSecret: c.Google.ClientSecret,
		Endpoint:     p.Endpoint(),
		RedirectURL:  strings.TrimSuffix(c.PublicURL, "/") + flowPath + "/callback",
		Scopes:       []string{oidc.ScopeOpenID, "email", "profile"},
	}
}

// flow is what a sign-in remembers between leaving for Google and coming
// back, in a short-lived signed cookie scoped to /auth/google.
type flow struct {
	State    string `json:"s"`
	Nonce    string `json:"n"`
	Verifier string `json:"v"`
	Return   string `json:"r,omitempty"` // where to go after signing in
	Invite   string `json:"i,omitempty"` // the invite code to sign up with
	// OrgInvite is an organization invite's token, from the join page
	// (joinCookie), to sign up with: never in a URL.
	OrgInvite string `json:"o,omitempty"`
	// Ref is where the sign-in link came from (?ref=quickstart), as
	// accounts.CleanRef left it, for the account it makes.
	Ref string `json:"f,omitempty"`
	Exp int64  `json:"e"`
}

// safeReturn accepts only a path on this site ("/oauth/authorize?…"), never
// another origin ("//evil", "/\\evil", "https://…").
func safeReturn(p string) string {
	if len(p) > 4096 || !strings.HasPrefix(p, "/") || strings.HasPrefix(p, "//") || strings.ContainsRune(p, '\\') {
		return "/"
	}
	// Browsers drop tabs and newlines from URLs ("/\t/evil.com" becomes
	// "//evil.com"), so no control characters at all.
	for _, r := range p {
		if r < 0x20 || r == 0x7f {
			return "/"
		}
	}
	return p
}

// logErr is a sign-in error as logged: a token endpoint's refusal by its
// error code and status only, since oauth2.RetrieveError's text can quote
// the provider's whole response.
func logErr(err error) error {
	var re *oauth2.RetrieveError
	if errors.As(err, &re) {
		status := 0
		if re.Response != nil {
			status = re.Response.StatusCode
		}
		return fmt.Errorf("token endpoint refused: %q (HTTP %d)", truncate(re.ErrorCode, 100), status)
	}
	return err
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func (c *Console) googleStart(w http.ResponseWriter, r *http.Request) {
	p, err := c.Google.discover(r.Context())
	if err != nil {
		slog.Error("auth: Google discovery failed", "err", err)
		http.Redirect(w, r, "/?sign_in_error=unavailable", http.StatusFound)
		return
	}
	f := c.newFlow(r)
	c.clear(w, joinCookie, joinPath) // carried in the flow now
	// Lax, not Strict: the cookie must come back on Google's top-level
	// redirect to the callback.
	http.SetCookie(w, c.cookie(flowCookie, flowPath, c.sign("sign-in", f), c.now().Add(flowTTL), http.SameSiteLaxMode))
	u := c.oauthConfig(p).AuthCodeURL(f.State, oidc.Nonce(f.Nonce), oauth2.S256ChallengeOption(f.Verifier),
		oauth2.SetAuthURLParam("prompt", "select_account"))
	http.Redirect(w, r, u, http.StatusFound)
}

func (c *Console) googleCallback(w http.ResponseWriter, r *http.Request) {
	c.clear(w, flowCookie, flowPath)
	u, f, err := c.finishGoogle(r)
	c.finish(w, r, "Google", u, f, err)
}

// newFlow starts a sign-in: where to come back to, the invite to sign up
// with and where the link came from (ref), from the start URL.
func (c *Console) newFlow(r *http.Request) flow {
	q := r.URL.Query()
	invite := q.Get("invite")
	if len(invite) > 64 {
		invite = ""
	}
	return flow{State: random(), Nonce: random(), Verifier: oauth2.GenerateVerifier(), Return: safeReturn(q.Get("return_to")),
		Invite: invite, OrgInvite: c.joinToken(r), Ref: accounts.CleanRef(q.Get("ref")), Exp: c.now().Add(flowTTL).Unix()}
}

// An organization invite's token reaches sign-in in a short-lived signed
// cookie, set by the join page (POST /api/v1/join/sign-in) and read when
// the sign-in starts, never in a URL, where it could reach a log.
const (
	joinCookie = "ws_join"
	joinPath   = "/auth"
)

type joinClaims struct {
	Token string `json:"t"`
	Exp   int64  `json:"e"`
}

// joinSignIn keeps an invite's token for the sign-in that follows.
func (c *Console) joinSignIn(w http.ResponseWriter, r *http.Request) {
	// From the console only: another site mustn't sign a visitor up into
	// an organization of its choosing.
	if !c.sameOrigin(r) {
		httpx.WriteError(w, http.StatusForbidden, "bad_origin", "request must come from the console")
		return
	}
	var in struct {
		Token string `json:"token"`
	}
	if !httpx.Decode(w, r, &in) {
		return
	}
	if in.Token == "" || len(in.Token) > 100 {
		httpx.WriteError(w, http.StatusBadRequest, "bad_request", "token is the invite's")
		return
	}
	exp := c.now().Add(flowTTL)
	// Lax, as the sign-in's own cookie: it's read on the top-level
	// navigation to /auth/…/start.
	http.SetCookie(w, c.cookie(joinCookie, joinPath, c.sign("join", joinClaims{Token: in.Token, Exp: exp.Unix()}), exp, http.SameSiteLaxMode))
	httpx.WriteJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// joinToken is the invite token the join page kept for this sign-in, if
// any.
func (c *Console) joinToken(r *http.Request) string {
	ck, err := r.Cookie(c.cookieName(joinCookie))
	var j joinClaims
	if err != nil || !c.verify("join", ck.Value, &j) || c.now().Unix() > j.Exp {
		return ""
	}
	return j.Token
}

// finish ends a provider's callback: signed in, or back to the sign-in page
// with why not (keeping the invite and the ref, so they can try another
// account).
func (c *Console) finish(w http.ResponseWriter, r *http.Request, provider string, u accounts.User, f flow, err error) {
	if err == nil {
		err = c.signedIn(w, r, u)
	}
	fail := func(code string) {
		v := url.Values{"sign_in_error": {code}}
		if f.Invite != "" {
			v.Set("invite", f.Invite)
		}
		if f.Ref != "" {
			v.Set("ref", f.Ref)
		}
		// Back to the join page, which still holds the invite, to say why.
		if f.OrgInvite != "" {
			http.Redirect(w, r, "/join?"+v.Encode(), http.StatusFound)
			return
		}
		http.Redirect(w, r, "/?"+v.Encode(), http.StatusFound)
	}
	var wrong *orgs.WrongAccountError
	var waitlisted *accounts.WaitlistedError
	switch {
	case err == nil:
		c.clear(w, waitlistCookie, waitlistPath)
		http.Redirect(w, r, safeReturn(f.Return), http.StatusFound)
	case errors.As(err, &waitlisted):
		// On the waitlist, with no account: the sign-in page shows it.
		c.onWaitlist(w, waitlisted.Entry)
		http.Redirect(w, r, "/", http.StatusFound)
	case errors.As(err, &wrong):
		fail("org_invite_email")
	case errors.Is(err, orgs.ErrInviteInvalid):
		fail("org_invite_invalid")
	case errors.Is(err, orgs.ErrInviterCant):
		fail("org_invite_inviter")
	case errors.Is(err, accounts.ErrNoSignUpsLeft):
		fail("org_invite_no_signups")
	case errors.Is(err, orgs.ErrTooManyMembers):
		fail("org_full")
	case errors.Is(err, accounts.ErrAccountExists):
		fail("account_exists")
	default:
		slog.Warn("auth: sign-in failed", "provider", provider, "err", logErr(err))
		fail("failed")
	}
}

func (c *Console) finishGoogle(r *http.Request) (accounts.User, flow, error) {
	f, err := c.readFlow(r, flowCookie, "sign-in")
	if err != nil {
		return accounts.User{}, f, err
	}
	q := r.URL.Query()
	p, err := c.Google.discover(r.Context())
	if err != nil {
		return accounts.User{}, f, err
	}
	tok, err := c.oauthConfig(p).Exchange(r.Context(), q.Get("code"), oauth2.VerifierOption(f.Verifier))
	if err != nil {
		return accounts.User{}, f, err
	}
	raw, _ := tok.Extra("id_token").(string)
	if raw == "" {
		return accounts.User{}, f, errors.New("no id_token")
	}
	idt, err := p.Verifier(&oidc.Config{ClientID: c.Google.ClientID}).Verify(r.Context(), raw)
	if err != nil {
		return accounts.User{}, f, err
	}
	if idt.Nonce != f.Nonce {
		return accounts.User{}, f, errors.New("nonce mismatch")
	}
	var cl struct {
		Email         string `json:"email"`
		EmailVerified bool   `json:"email_verified"`
		Name          string `json:"name"`
		HostedDomain  string `json:"hd"`
	}
	if err := idt.Claims(&cl); err != nil {
		return accounts.User{}, f, err
	}
	if cl.Email == "" || !cl.EmailVerified {
		return accounts.User{}, f, errors.New("email not verified")
	}
	u, err := c.Accounts.SignIn(r.Context(), accounts.Identity{Provider: "google", Subject: idt.Subject, Email: cl.Email, Name: cl.Name,
		HostedDomain: cl.HostedDomain, Invite: f.Invite, OrgInvite: f.OrgInvite, Ref: f.Ref})
	return u, f, err
}

// readFlow reads a sign-in's flow cookie, with the state checked against the
// callback's.
func (c *Console) readFlow(r *http.Request, cookie, purpose string) (flow, error) {
	var f flow
	ck, err := r.Cookie(c.cookieName(cookie))
	if err != nil || !c.verify(purpose, ck.Value, &f) || c.now().Unix() > f.Exp {
		return flow{}, errors.New("no sign-in in progress (or it expired)")
	}
	q := r.URL.Query()
	if q.Get("error") != "" {
		return f, errors.New("provider returned " + truncate(q.Get("error"), 100))
	}
	if q.Get("state") == "" || q.Get("state") != f.State {
		return f, errors.New("state mismatch")
	}
	return f, nil
}

func random() string {
	b := make([]byte, 24)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}
