package auth

import (
	"cmp"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/wakeshift/server/internal/accounts"
	"github.com/wakeshift/server/internal/httpx"
)

// DevTokenHeader carries the development sign-in token, for clients other
// than the console's form (which sends it in the body).
const DevTokenHeader = "X-Wakeshift-Dev-Token"

// MinDevTokenLen is the shortest development sign-in token accepted.
const MinDevTokenLen = 16

// devSignInAllowed is true only for a server the developer runs locally:
// the flag must be set, with a token, and the public URL must be localhost
// (http, or https through a local proxy). A deployed server can't enable it
// by accident. (internal/config also requires a loopback listen address.)
func (c *Console) devSignInAllowed() bool {
	if !c.DevSignIn || len(c.DevSignInToken) < MinDevTokenLen {
		return false
	}
	u, err := url.Parse(c.PublicURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return false
	}
	host := u.Hostname()
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

// proxyHeaders are the headers a reverse proxy adds to say whom it
// forwarded a request for; any X-Forwarded-* header counts too.
var proxyHeaders = []string{"Forwarded", "X-Real-IP"}

const xForwarded = "X-Forwarded-"

// proxied reports whether a proxy says it forwarded the request: any
// X-Forwarded-* header (For, Host, Proto, Port…) or proxyHeaders, even
// empty. Keys are compared ignoring case, not looked up: net/http
// canonicalizes the keys it parses (X-Real-IP is stored as X-Real-Ip), but
// a key it can't canonicalize is kept as it came.
func proxied(r *http.Request) bool {
	for k := range r.Header {
		if len(k) >= len(xForwarded) && strings.EqualFold(k[:len(xForwarded)], xForwarded) {
			return true
		}
		for _, h := range proxyHeaders {
			if strings.EqualFold(k, h) {
				return true
			}
		}
	}
	return false
}

// devSignIn signs in as any email, creating the user on first use. Only
// clients on this machine may use it: the Origin header can be forged by
// anything that isn't a browser, so the connection itself must be loopback.
// A reverse proxy on the same host (nginx, Caddy, a tunnel) connects from
// loopback whoever its client is, and passes on the Origin it was sent, so
// a request carrying a proxy's headers is refused too. A local HTTPS proxy
// may still sit in front if it listens on loopback only and drops those
// headers (as a local Caddy for HTTPS can). Forwarders that add no
// headers (socat, ssh -R, a plain proxy_pass) can't be told apart, so the
// request must also carry the server's development sign-in token
// (DevSignInToken: printed at startup, or WAKESHIFT_DEV_SIGN_IN_TOKEN), in
// the body or DevTokenHeader.
func (c *Console) devSignIn(w http.ResponseWriter, r *http.Request) {
	if !loopback(r.RemoteAddr) || proxied(r) {
		httpx.WriteError(w, http.StatusForbidden, "not_local", "development sign-in only works from this machine, not through a proxy")
		return
	}
	if !c.sameOrigin(r) {
		httpx.WriteError(w, http.StatusForbidden, "bad_origin", "request must come from the console")
		return
	}
	var in struct {
		Email string `json:"email"`
		Token string `json:"token"`
	}
	if !httpx.Decode(w, r, &in) {
		return
	}
	// The token the server printed at startup (or was given): proof the
	// caller can read this server's log or environment, which a forwarder
	// that adds no headers (socat, ssh -R, a plain proxy_pass) can't give.
	token := cmp.Or(r.Header.Get(DevTokenHeader), in.Token)
	if !c.devTokenOK(token) {
		httpx.WriteError(w, http.StatusForbidden, "bad_dev_token", "enter the development sign-in token from the server's log")
		return
	}
	email := strings.ToLower(strings.TrimSpace(in.Email))
	if !strings.Contains(email, "@") {
		httpx.WriteError(w, http.StatusBadRequest, "bad_request", "enter an email address")
		return
	}
	dev := *c.Accounts
	dev.Allowed = func(accounts.Identity) bool { return true }
	u, err := dev.SignIn(r.Context(), accounts.Identity{Provider: "dev", Subject: email, Email: email})
	if err == nil {
		err = c.signedIn(w, r, u)
	}
	if errors.Is(err, accounts.ErrAccountExists) {
		httpx.WriteError(w, http.StatusConflict, "account_exists", "that email belongs to an account that signs in with Google")
		return
	}
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "internal", "couldn't sign in")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]bool{"authenticated": true})
}

// devTokenOK compares a presented token with the server's in constant time
// (over their hashes, so neither length leaks either).
func (c *Console) devTokenOK(token string) bool {
	if token == "" || len(c.DevSignInToken) < MinDevTokenLen {
		return false
	}
	got, want := sha256.Sum256([]byte(token)), sha256.Sum256([]byte(c.DevSignInToken))
	return subtle.ConstantTimeCompare(got[:], want[:]) == 1
}

func loopback(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
