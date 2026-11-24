package agenthooks

// Cursor Automations webhooks: how a Grok Bot is woken.
// Grok Bot runs in Cursor's cloud, and its routines are Cursor Automations:
// a routine with a webhook trigger, created by the agent inside its bot,
// starts a run in that bot each time its webhook is POSTed to, even while
// the bot is idle. The agent can't see the webhook's URL or key; the user
// pastes both on a one-time page (SetupLinks), and the webhook is bound
// only once the run it starts proves the bot's key (internal/botkeys).
//
// The URL and the key are credentials: sealed together at rest (SealCursor,
// bound to the workspace and session), never logged, traced, put in an
// error, an audit entry or last_error. Only Cursor's exact URL shape is
// accepted (CheckCursorURL), and the key is checked to be a plain header
// value (CheckCursorKey) on the page and again before each POST.

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/url"
	"regexp"
	"strings"
)

// KindCursor is a Cursor Automations webhook destination (Grok Bot).
const KindCursor = "cursor-automations"

// CursorOrigin and CursorPath are where Cursor's Automations webhooks
// live: CursorOrigin + CursorPath + <id>.
const (
	CursorOrigin = "https://api2.cursor.sh"
	CursorPath   = "/automations/webhook/"
)

// cursorIDRE is a webhook's ID: a plain URL-safe charset and a sane length
// (Cursor doesn't document the format, so this stays broad).
var cursorIDRE = regexp.MustCompile(`^[A-Za-z0-9_-]{8,128}$`)

// Dead reasons of a Cursor destination.
const (
	ReasonUnauthorized = "unauthorized" // 401 or 403: the key was rotated or revoked
	ReasonFailing      = "failing"      // a day of failures and no success
)

// ErrBadCursorURL is a URL that isn't a Cursor Automations webhook's. Its
// message never echoes the URL.
var ErrBadCursorURL = errors.New("this isn't an Automations webhook URL: it starts with " + CursorOrigin + CursorPath)

// ErrBadCursorKey is a webhook key that can't be one. Its message never
// echoes the key.
var ErrBadCursorKey = errors.New("this doesn't look like the webhook's key")

func cursorOriginOf() string {
	if o := testOrigin.Load(); o != nil {
		return *o
	}
	return CursorOrigin
}

// CheckCursorURL accepts only a Cursor Automations webhook URL, exactly:
// CursorOrigin + CursorPath + an ID matching cursorIDRE. Anything else
// (another host, http, a port, userinfo, a query or fragment, another path,
// an encoded character, a dot segment, a trailing slash, an overlong value)
// is ErrBadCursorURL.
func CheckCursorURL(raw string) error {
	if len(raw) > MaxURL {
		return ErrBadCursorURL
	}
	origin := cursorOriginOf()
	id, ok := strings.CutPrefix(raw, origin+CursorPath)
	if !ok || !cursorIDRE.MatchString(id) {
		return ErrBadCursorURL
	}
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" ||
		u.Scheme+"://"+u.Host != origin || u.Path != CursorPath+id || u.RawPath != "" {
		return ErrBadCursorURL
	}
	if origin == CursorOrigin && (u.Scheme != "https" || u.Host != "api2.cursor.sh" || u.Port() != "") {
		return ErrBadCursorURL
	}
	return nil
}

// CheckCursorKey accepts a webhook key that can only be a header value:
// printable ASCII, no spaces or control characters, 32 to 256 bytes.
func CheckCursorKey(key string) error {
	if len(key) < 32 || len(key) > 256 {
		return ErrBadCursorKey
	}
	for i := 0; i < len(key); i++ {
		if c := key[i]; c <= ' ' || c > '~' {
			return ErrBadCursorKey
		}
	}
	return nil
}

// CursorHook is a Cursor webhook's URL and key: the credential.
type CursorHook struct {
	URL string `json:"url"`
	Key string `json:"key"`
}

// Check checks both.
func (h CursorHook) Check() error {
	if err := CheckCursorURL(h.URL); err != nil {
		return err
	}
	return CheckCursorKey(h.Key)
}

// SealCursor seals a webhook's URL and key together, bound to what they're
// for (binding: a destination's, or a setup's): Open with anything else
// fails.
func SealCursor(seal interface {
	Seal(string, []byte) []byte
}, binding string, h CursorHook) []byte {
	b, _ := json.Marshal(h)
	return seal.Seal(binding, b)
}

// OpenCursor opens what SealCursor sealed, and checks it again.
func OpenCursor(seal interface {
	Open(string, []byte) ([]byte, error)
}, binding string, sealed []byte) (CursorHook, error) {
	var h CursorHook
	b, err := seal.Open(binding, sealed)
	if err != nil {
		return h, err
	}
	if err := json.Unmarshal(b, &h); err != nil {
		return CursorHook{}, err
	}
	return h, h.Check()
}

// Binding is what a destination's sealed URL is bound to.
func Binding(tenant, session string) string { return binding(tenant, session) }

// SetupBinding is what a setup's sealed URL and key are bound to while it
// awaits a proof.
func SetupBinding(tenant, setup string) string { return "agent-webhook-setup:" + tenant + ":" + setup }

// SetupLinks makes and reads the setup page's token: a signature over the
// setup's row (so the same link can be shown again without storing it), in
// a domain of its own, apart from Links' registration links and stream
// links. 160-bit signatures. There's no removal token: the user stops
// wake-ups by asking the agent (unwatch) or by deleting the routine.
type SetupLinks struct{ key []byte }

// NewSetupLinks makes SetupLinks with a key of their own.
func NewSetupLinks(key []byte) *SetupLinks { return &SetupLinks{key: key} }

// SetupPath is the page a setup link opens (the token in the fragment, so
// it never reaches a request log, a proxy or a link-preview fetch).
const SetupPath = "/webhook-setup"

// Setup is what a setup token names.
type Setup struct{ Tenant, ID string }

type setupClaims struct {
	Tenant string `json:"t"`
	Setup  string `json:"s"`
}

const domainSetup = "agent-webhook-setup:"

// SetupToken is the setup token for a setup row.
func (l *SetupLinks) SetupToken(s Setup) string { return l.token(domainSetup, s) }

// URL is the setup link on the console at base.
func (l *SetupLinks) URL(base string, s Setup) string {
	return strings.TrimSuffix(base, "/") + SetupPath + "#t=" + l.SetupToken(s)
}

// Routine names a bot's routine: an HMAC of the bot's ID and its webhook
// URL, so the index can hold it without the URL.
func (l *SetupLinks) Routine(bot, url string) []byte {
	m := hmac.New(sha256.New, l.key)
	m.Write([]byte("agent-webhook-routine:" + bot + "\n" + url))
	return m.Sum(nil)
}

// ErrBadSetupToken is a token that isn't one of ours, for this purpose.
var ErrBadSetupToken = errors.New("agenthooks: not a valid setup token")

// ParseSetup reads a setup token.
func (l *SetupLinks) ParseSetup(token string) (Setup, error) { return l.parse(domainSetup, token) }

// BindingID names one bind of a webhook (the setup that bound it): every
// destination a bind makes or carries its key to holds it, and the
// routine's current one is bot_routines', so a carry-over and a join can
// tell the current binding from an older one. Kept in the removal_hash
// columns, named for the removal token they held before there was none.
func BindingID(s Setup) []byte {
	h := sha256.Sum256([]byte("agent-webhook-binding:" + s.Tenant + ":" + s.ID))
	return h[:]
}

func (l *SetupLinks) token(domain string, s Setup) string {
	body, _ := json.Marshal(setupClaims{Tenant: s.Tenant, Setup: s.ID})
	b := base64.RawURLEncoding.EncodeToString(body)
	return b + "." + base64.RawURLEncoding.EncodeToString(l.mac(domain, b))
}

func (l *SetupLinks) parse(domain, token string) (Setup, error) {
	if len(token) > 512 {
		return Setup{}, ErrBadSetupToken
	}
	b, sig, ok := strings.Cut(token, ".")
	if !ok {
		return Setup{}, ErrBadSetupToken
	}
	got, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil || !hmac.Equal(got, l.mac(domain, b)) {
		return Setup{}, ErrBadSetupToken
	}
	var c setupClaims
	body, err := base64.RawURLEncoding.DecodeString(b)
	if err != nil || json.Unmarshal(body, &c) != nil || !uuidRE.MatchString(c.Tenant) || !uuidRE.MatchString(c.Setup) {
		return Setup{}, ErrBadSetupToken
	}
	return Setup{Tenant: c.Tenant, ID: c.Setup}, nil
}

func (l *SetupLinks) mac(domain, b string) []byte {
	m := hmac.New(sha256.New, l.key)
	m.Write([]byte(domain + b))
	return m.Sum(nil)[:20]
}

var uuidRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
