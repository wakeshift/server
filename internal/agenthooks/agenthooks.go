// Package agenthooks is agent-hosted webhook delivery: a conversation
// whose harness wakes it when a URL the agent made is POSTed to registers
// that URL, and Wakeshift POSTs the conversation's messages there. Two
// harnesses work this way. Grok Bot's is a Cursor Automations routine's
// webhook, whose URL and key the user pastes (cursor.go). Capy's is below:
// an agent creates an incoming_webhook watch on its own thread
// (capy.subscribe), which returns
// https://api.capy.ai/webhooks/automations/<secret>, and each POST to it
// wakes the thread, even idle, with no process or awake machine.
//
// The URL is the credential (no signature, no auth header), and an
// agent-supplied URL makes Wakeshift's servers POST somewhere, so:
//
//   - only Capy's exact URL shape is accepted (CheckURL): https, the host
//     api.capy.ai, no userinfo, port, query or fragment, and a secret of a
//     plain charset and bounded length after /webhooks/automations/;
//   - it's registered only with a registration link (Links): a capability
//     derived from the session's stream link, in a signing domain of its
//     own, naming the harness it was handed to (only a conversation
//     recognized as Capy gets one), and working only while that link does;
//   - it's sealed at rest (mcpevents.Sealer, bound to the workspace and
//     session), and never logged, traced, put in an error or an audit
//     entry: what's logged about a destination is its session and a
//     failure's category;
//   - the client connects to public addresses only, follows no redirects,
//     and times out quickly (Client); one instance sends to a destination
//     at a time, in order, spaced (Deliverer.Spacing), within a per-instance
//     cap on sends at once.
package agenthooks

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/url"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Kinds of destination.
const KindCapy = "capy"

// CapyOrigin and CapyPath are where Capy's webhook watch URLs live:
// CapyOrigin + CapyPath + <secret>.
const (
	CapyOrigin = "https://api.capy.ai"
	CapyPath   = "/webhooks/automations/"
)

// secretRE is a watch URL's secret: a plain URL-safe charset (no '/', '.',
// '%', '?', '#', '@' or ':', so nothing after the prefix can change where
// the URL goes) and a sane length.
var secretRE = regexp.MustCompile(`^[A-Za-z0-9_-]{16,128}$`)

// MaxURL is the longest URL accepted.
const MaxURL = 512

// ErrBadURL is a URL that isn't a Capy webhook watch's. Its message never
// echoes the URL.
var ErrBadURL = errors.New("url must be exactly the URL capy.subscribe returned for an incoming_webhook watch: " +
	CapyOrigin + CapyPath + "<secret>, with no port, query or fragment")

// testOrigin, if set, is accepted in place of CapyOrigin: a test's server
// on this machine standing in for Capy (AllowTestOrigin).
var testOrigin atomic.Pointer[string]

// AllowTestOrigin accepts origin (a test server's, http://127.0.0.1:port)
// in place of CapyOrigin, and lets the client connect to loopback
// addresses, until the returned func runs. It panics outside a test
// binary (testing.Testing), so no configuration can turn it on in
// production.
func AllowTestOrigin(origin string) (undo func()) {
	if !testing.Testing() {
		panic("agenthooks: AllowTestOrigin is for tests only")
	}
	testOrigin.Store(&origin)
	return func() { testOrigin.Store(nil) }
}

func originOf() string {
	if o := testOrigin.Load(); o != nil {
		return *o
	}
	return CapyOrigin
}

// CheckURL accepts only a Capy webhook watch URL, exactly:
// CapyOrigin + CapyPath + a secret matching secretRE. Anything else (another
// host, http, a port, userinfo, a query or fragment, another path, an
// encoded character, a trailing slash, an overlong value) is ErrBadURL.
func CheckURL(raw string) error {
	if len(raw) > MaxURL {
		return ErrBadURL
	}
	origin := originOf()
	secret, ok := strings.CutPrefix(raw, origin+CapyPath)
	if !ok || !secretRE.MatchString(secret) {
		return ErrBadURL
	}
	// The same, as a URL parser reads it: nothing in the prefix can be
	// read another way.
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" ||
		u.Scheme+"://"+u.Host != origin || u.Path != CapyPath+secret || u.RawPath != "" {
		return ErrBadURL
	}
	if origin == CapyOrigin && (u.Scheme != "https" || u.Host != "api.capy.ai" || u.Port() != "") {
		return ErrBadURL
	}
	return nil
}

// Links makes and reads registration links: a capability for one
// session's destination, derived from the session's stream link (its ID),
// signed in a domain of its own (a stream link can't register, and a
// registration link can't read a stream), and naming the harness it was
// handed to.
type Links struct{ key []byte }

// NewLinks makes Links with a key of their own.
func NewLinks(key []byte) *Links { return &Links{key: key} }

// claims is what a registration link carries.
type claims struct {
	Tenant string `json:"t"`
	Link   string `json:"l"` // the session's stream link's ID
	Kind   string `json:"k"`
	Exp    int64  `json:"e"`
}

// Registration is what a registration link names.
type Registration struct {
	Tenant, Link, Kind string
}

// RegisterPath is the route a registration link points at: POST registers
// (or replaces) the destination, DELETE removes it. It's under /v1/stream,
// whose query strings are kept out of every request log.
const RegisterPath = "/v1/stream/webhook"

// URL is the registration link for a session's stream link (tenant, link)
// handed to a conversation of harness kind, working until exp at most
// (and only while the stream link works), on the Wakeshift at base.
func (l *Links) URL(base, tenant, link, kind string, exp time.Time) string {
	body, _ := json.Marshal(claims{Tenant: tenant, Link: link, Kind: kind, Exp: exp.Unix()})
	b := base64.RawURLEncoding.EncodeToString(body)
	token := b + "." + base64.RawURLEncoding.EncodeToString(l.mac(b))
	return strings.TrimSuffix(base, "/") + RegisterPath + "?" + url.Values{"link": {token}}.Encode()
}

// ErrBadLink is a registration link that isn't one, or expired.
var ErrBadLink = errors.New("this registration link isn't valid or has expired")

// Parse reads a registration link's token.
func (l *Links) Parse(token string, now time.Time) (Registration, error) {
	var c claims
	b, sig, ok := strings.Cut(token, ".")
	if !ok {
		return Registration{}, ErrBadLink
	}
	got, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil || !hmac.Equal(got, l.mac(b)) {
		return Registration{}, ErrBadLink
	}
	body, err := base64.RawURLEncoding.DecodeString(b)
	if err != nil || json.Unmarshal(body, &c) != nil || c.Tenant == "" || c.Link == "" || c.Kind == "" {
		return Registration{}, ErrBadLink
	}
	if now.Unix() > c.Exp {
		return Registration{}, ErrBadLink
	}
	return Registration{Tenant: c.Tenant, Link: c.Link, Kind: c.Kind}, nil
}

func (l *Links) mac(b string) []byte {
	m := hmac.New(sha256.New, l.key)
	m.Write([]byte("agent-webhook-register:" + b))
	return m.Sum(nil)
}
