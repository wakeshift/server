// Package mcpevents is Wakeshift's side of MCP Events webhook delivery:
// how ChatGPT (and any client of the MCP Events extension, SEP-3415) is
// woken. The client subscribes to one of an app's event types
// (apps.EventType) with its arguments, through the app's MCP server
// (pkg/wakeshift Attach, the TypeScript SDK's attach), with a
// callback URL and a Standard Webhooks secret; Wakeshift verifies the
// callback, then POSTs each occurrence to it, signed (EventBody). A 2xx
// is the ack.
//
// It follows the draft named by event.Revision (the constants are in
// internal/mcpevents/event, spec.go):
//   - events/subscribe params {name, arguments, delivery: {mode: "webhook",
//     url, secret: "whsec_…"}, ttlMs, cursor, maxAgeMs}; the result
//     {resultType, id, refreshBefore, cursor, truncated, deliveryStatus};
//   - a verification POST whose challenge the callback echoes, before
//     anything is delivered there;
//   - one event per POST, at most 256 KiB, with webhook-id,
//     webhook-timestamp, webhook-signature (v1, HMAC-SHA256 over
//     "id.timestamp.body") and X-MCP-Subscription-Id, and a fresh timestamp
//     and signature per attempt;
//   - strict order per subscription: one delivery in flight, and none after
//     one that's being retried (the draft allows best effort; the
//     conversation reads them in order);
//   - retries within RetryWindow, at most MaxAttempts, none after 410 or
//     413; a delivery abandoned after retries (or refused as too large) is
//     a gap;
//   - cursors from core's queue (a safe watermark: everything at or before
//     it is acknowledged or abandoned), and replay from a cursor on a fresh
//     subscription, bounded by maxAgeMs and MaxReplay (from the app's recent
//     publishes when the subscription lapsed: appapi's replaySince);
//   - gap and terminated envelopes (not to ChatGPT, which takes neither);
//   - grants of at most MaxTTL, never without expiry; access rechecked at
//     least every ReauthEvery;
//   - https to public addresses only, checked per connection, no redirects;
//   - at most MaxWebhooks live per app user, and an instance's send slots
//     shared between apps and workspaces (deliver.go, shares), so slow
//     callbacks hold up only their own app's and user's deliveries.
package mcpevents

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/wakeshift/server/internal/mcpevents/event"
)

// Limits.
const (
	MaxBody        = 256 << 10 // a delivery's body, as the draft and ChatGPT accept it
	Timeout        = 10 * time.Second
	MaxTTL         = 24 * time.Hour // the longest grant, and the one for no ttlMs or ttlMs: null
	MinTTL         = 5 * time.Minute
	MaxAttempts    = 5                // per delivery (Backoff), within RetryWindow
	RotationWindow = 10 * time.Minute // a replaced secret still signs this long
	ReauthEvery    = time.Hour        // access is rechecked at least this often
	MaxReplay      = 1000             // deliveries replayed from a cursor, at most: older ones are a gap
	// MaxWebhooks is how many live webhooks an app's user may have (in the
	// workspace the app owns for them): a new one past it is refused
	// (ErrTooManyWebhooks); a refresh of a live one never is. Real use
	// stays well under it.
	MaxWebhooks = 20
)

// ErrTooManyWebhooks is a new webhook past MaxWebhooks.
var ErrTooManyWebhooks = fmt.Errorf("this user already has %d live webhook subscriptions to this app, the most allowed: "+
	"unsubscribe from one (events/unsubscribe), or let one lapse, first", MaxWebhooks)

// RetryWindow is how long a delivery is retried from its first failure,
// before it's abandoned (a gap). A variable so tests can shorten it.
var RetryWindow = 15 * time.Minute

// BackoffBase is the wait before a delivery's second attempt; each next
// one waits twice as long, up to 8 minutes, so MaxAttempts fit in
// RetryWindow (tests shorten it).
var BackoffBase = 30 * time.Second

// Backoff is the wait before attempt n+1 of a delivery (n from 1).
func Backoff(n int) time.Duration {
	return min(BackoffBase<<max(n-1, 0), 8*time.Minute)
}

// MaxAgeCap is the longest replay bound: anything longer is the same as
// none (deliveries don't last that long).
const MaxAgeCap = 400 * 24 * time.Hour

// MaxAgeOf is a client's maxAgeMs as a duration (0: no bound), capped at
// MaxAgeCap first so a huge value can't overflow into a short or negative
// one.
func MaxAgeOf(ms int64) time.Duration {
	if ms <= 0 {
		return 0
	}
	return time.Duration(min(ms, int64(MaxAgeCap/time.Millisecond))) * time.Millisecond
}

// TTLOf is the grant for a client's ttlMs (nil, null or ≤ 0: MaxTTL),
// clamped to [MinTTL, MaxTTL] without overflowing.
func TTLOf(ms *int64) time.Duration {
	if ms == nil || *ms <= 0 {
		return MaxTTL
	}
	return min(max(time.Duration(min(*ms, int64(MaxTTL/time.Millisecond)))*time.Millisecond, MinTTL), MaxTTL)
}

// Cursor is the cursor for a position in a conversation's queue: the event
// sequence number at or before which every delivery is acknowledged or
// abandoned. It's opaque to clients.
func Cursor(seq int64) string { return "c1." + strconv.FormatInt(max(seq, 0), 10) }

// ErrBadCursor is a cursor Wakeshift didn't make.
var ErrBadCursor = errors.New("cursor isn't one Wakeshift gave out")

// ParseCursor reads a Cursor.
func ParseCursor(s string) (int64, error) {
	raw, ok := strings.CutPrefix(s, "c1.")
	n, err := strconv.ParseInt(raw, 10, 64)
	if !ok || err != nil || n < 0 || Cursor(n) != s { // only what Cursor makes: no +1, no 01
		return 0, ErrBadCursor
	}
	return n, nil
}

// ParseSecret decodes a whsec_ secret: base64 of 24 to 64 bytes.
func ParseSecret(s string) ([]byte, error) {
	raw, ok := strings.CutPrefix(s, "whsec_")
	if !ok {
		return nil, errors.New("delivery.secret must start with whsec_")
	}
	b, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		b, err = base64.RawStdEncoding.DecodeString(raw)
	}
	if err != nil || len(b) < 24 || len(b) > 64 {
		return nil, errors.New("delivery.secret must be whsec_ and the base64 of 24 to 64 bytes")
	}
	return b, nil
}

// Sign is the webhook-signature value for a message: one v1 signature per
// secret, space-separated (during a rotation, both keys sign).
func Sign(id string, ts int64, body []byte, secrets ...[]byte) string {
	sigs := make([]string, 0, len(secrets))
	for _, s := range secrets {
		m := hmac.New(sha256.New, s)
		fmt.Fprintf(m, "%s.%d.", id, ts)
		m.Write(body)
		sigs = append(sigs, "v1,"+base64.StdEncoding.EncodeToString(m.Sum(nil)))
	}
	return strings.Join(sigs, " ")
}

// SignatureTolerance is how far a signed request's webhook-timestamp may
// be from now, either way (Standard Webhooks' recommendation).
const SignatureTolerance = 5 * time.Minute

// ErrSignature is a request VerifySigned refused; its message says why,
// and never carries a secret or a signature.
type ErrSignature struct{ Reason string }

func (e ErrSignature) Error() string { return e.Reason }

// VerifySigned checks a Standard Webhooks request, the inverse of Send: its
// webhook-id, webhook-timestamp and webhook-signature headers (or Svix's
// svix-id, svix-timestamp and svix-signature, which carry the same values),
// a timestamp within SignatureTolerance of now, and at least one v1
// signature in the space-separated list matching one of secrets (a sender
// rotating its key signs with both), compared in constant time. It returns
// the webhook-id, which only the signer could have chosen.
func VerifySigned(h http.Header, body []byte, now time.Time, secrets ...[]byte) (string, error) {
	prefix := "webhook-"
	if h.Get("webhook-id") == "" && h.Get("svix-id") != "" {
		prefix = "svix-"
	}
	id, rawTS, sigs := h.Get(prefix+"id"), h.Get(prefix+"timestamp"), h.Get(prefix+"signature")
	if id == "" || rawTS == "" || sigs == "" {
		return "", ErrSignature{"this publish URL takes only signed requests: webhook-id, webhook-timestamp and webhook-signature headers are required"}
	}
	ts, err := strconv.ParseInt(rawTS, 10, 64)
	if err != nil {
		return "", ErrSignature{prefix + "timestamp must be a Unix time in seconds"}
	}
	if d := now.Sub(time.Unix(ts, 0)); d > SignatureTolerance || d < -SignatureTolerance {
		return "", ErrSignature{prefix + "timestamp is more than 5 minutes from now"}
	}
	want := make([][]byte, 0, len(secrets))
	for _, s := range secrets {
		m := hmac.New(sha256.New, s)
		fmt.Fprintf(m, "%s.%d.", id, ts)
		m.Write(body)
		want = append(want, m.Sum(nil))
	}
	ok := false
	for _, sig := range strings.Fields(sigs) {
		v, b64, found := strings.Cut(sig, ",")
		if !found || v != "v1" {
			continue // another scheme (v1a is asymmetric): not ours
		}
		got, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			continue
		}
		for _, w := range want {
			if hmac.Equal(got, w) {
				ok = true
			}
		}
	}
	if !ok {
		return "", ErrSignature{"no signature in " + prefix + "signature matches this publish URL's signing secret"}
	}
	return id, nil
}

// SubscriptionID is a subscription's ID, derived from what identifies it:
// the same workspace, session, callback and event give the same ID, so
// subscribing again refreshes it. It's a routing handle, not a capability.
func SubscriptionID(tenant, session, callback, name string) string {
	sum := sha256.Sum256([]byte("mcp-events\x00" + tenant + "\x00" + session + "\x00" + callback + "\x00" + name))
	return "sub_" + hex.EncodeToString(sum[:12])
}

// AllowLoopback lets tests use http callbacks on loopback addresses. Never
// set it outside tests.
var AllowLoopback bool

// CheckURL refuses a callback that isn't an absolute https URL (plain http
// only to a loopback host, with AllowLoopback). Addresses are checked again
// when connecting (Client), so a name that resolves elsewhere later is
// refused then.
func CheckURL(raw string) error {
	u, err := url.Parse(raw)
	switch {
	case err != nil || u.Host == "" || u.User != nil:
		return NotHTTPSError{"delivery.url must be an absolute https URL"}
	case len(raw) > 2000:
		return errors.New("delivery.url is at most 2000 characters")
	case u.Scheme == "https":
		return nil
	case u.Scheme == "http" && AllowLoopback && loopbackHost(u.Hostname()):
		return nil
	}
	return NotHTTPSError{"delivery.url must be https"}
}

// loopbackHost reports whether a URL's host is a loopback address or
// localhost: the only hosts AllowLoopback admits plain http to, so tests
// see an http callback anywhere else refused, as it is in production.
func loopbackHost(h string) bool {
	if a, err := netip.ParseAddr(h); err == nil {
		return a.Unmap().IsLoopback()
	}
	h = strings.TrimSuffix(strings.ToLower(h), ".")
	return h == "localhost" || strings.HasSuffix(h, ".localhost")
}

// NotHTTPSError is a callback CheckURL refuses for not being an absolute
// https URL (not for its length): one nothing can be delivered to.
type NotHTTPSError struct{ msg string }

func (e NotHTTPSError) Error() string { return e.msg }

// PublicAddr reports whether an address is public: what a callback (or an
// agent-hosted webhook, internal/agenthooks) may connect to. It honors
// AllowLoopback.
func PublicAddr(a netip.Addr) bool { return publicAddr(a) }

// publicAddr reports whether an address may be called back.
func publicAddr(a netip.Addr) bool {
	a = a.Unmap()
	if AllowLoopback && a.IsLoopback() {
		return true
	}
	if !a.IsGlobalUnicast() || a.IsPrivate() || a.IsLoopback() || a.IsLinkLocalUnicast() {
		return false
	}
	for _, p := range notPublic {
		if p.Contains(a) {
			return false
		}
	}
	// IPv6 forms that carry an IPv4 address: judge the address they carry.
	if a.Is6() {
		b := a.As16()
		switch {
		case nat64.Contains(a): // 64:ff9b::a.b.c.d
			return publicAddr(netip.AddrFrom4([4]byte(b[12:16])))
		case sixToFour.Contains(a): // 2002:aabb:ccdd::
			return publicAddr(netip.AddrFrom4([4]byte(b[2:6])))
		case !globalUnicast6.Contains(a):
			// Outside the one block IANA allocates for global unicast:
			// IPv4-compatible ::/96, the discard and dummy prefixes, SRv6
			// SIDs, site-local, and whatever is assigned out there next.
			return false
		}
	}
	return true
}

var (
	nat64     = netip.MustParsePrefix("64:ff9b::/96")
	sixToFour = netip.MustParsePrefix("2002::/16")
	// IANA's IPv6 Address Space registry: global unicast is 2000::/3.
	globalUnicast6 = netip.MustParsePrefix("2000::/3")
	// Reserved ranges IsGlobalUnicast lets through: the entries of IANA's
	// IPv4 and IPv6 Special-Purpose Address registries that aren't
	// globally reachable, or that no callback is at (SEP-3415, Webhook
	// security). Checked against both registries on 2026-11-19.
	notPublic = []netip.Prefix{
		netip.MustParsePrefix("0.0.0.0/8"),
		netip.MustParsePrefix("100.64.0.0/10"),   // carrier-grade NAT
		netip.MustParsePrefix("192.0.0.0/24"),    // IETF protocol assignments
		netip.MustParsePrefix("192.0.2.0/24"),    // documentation
		netip.MustParsePrefix("192.88.99.0/24"),  // deprecated 6to4 relay anycast
		netip.MustParsePrefix("198.18.0.0/15"),   // benchmarking
		netip.MustParsePrefix("198.51.100.0/24"), // documentation
		netip.MustParsePrefix("203.0.113.0/24"),  // documentation
		netip.MustParsePrefix("240.0.0.0/4"),     // reserved
		netip.MustParsePrefix("::/96"),           // IPv4-compatible (deprecated)
		netip.MustParsePrefix("64:ff9b:1::/48"),  // local-use IPv4/IPv6 translation
		netip.MustParsePrefix("100::/64"),        // discard-only
		netip.MustParsePrefix("100:0:0:1::/64"),  // dummy prefix
		netip.MustParsePrefix("2001::/23"),       // IETF protocol assignments (Teredo, benchmarking, ORCHID)
		netip.MustParsePrefix("2001:db8::/32"),   // documentation
		netip.MustParsePrefix("3fff::/20"),       // documentation
		netip.MustParsePrefix("5f00::/16"),       // segment routing (SRv6) SIDs
		netip.MustParsePrefix("fc00::/7"),        // unique local
		netip.MustParsePrefix("fec0::/10"),       // site-local (deprecated)
	}
)

// errPrivate is a connection refused because the address isn't public.
var errPrivate = errors.New("the callback's address isn't public")

// Client is the HTTP client for callbacks: public addresses only, checked
// on every connection (so DNS can't point it inside), no redirects, no
// proxy, a short timeout.
func Client() *http.Client {
	d := &net.Dialer{Timeout: 5 * time.Second, Control: func(_, address string, _ syscall.RawConn) error {
		ap, err := netip.ParseAddrPort(address)
		if err != nil || !publicAddr(ap.Addr()) {
			return errPrivate
		}
		return nil
	}}
	t := &http.Transport{DialContext: d.DialContext, Proxy: nil, TLSHandshakeTimeout: 5 * time.Second,
		ResponseHeaderTimeout: Timeout, MaxIdleConnsPerHost: 4, IdleConnTimeout: time.Minute}
	return &http.Client{Transport: t, Timeout: Timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// Sent is what a callback answered.
type Sent struct {
	Status int
	Body   []byte // its first 64 KiB
}

// Send POSTs one signed body to a callback.
func Send(ctx context.Context, c *http.Client, callback, sub, msgID string, body []byte, now time.Time, secrets ...[]byte) (Sent, error) {
	ts := now.Unix()
	req, err := http.NewRequestWithContext(ctx, "POST", callback, bytes.NewReader(body))
	if err != nil {
		return Sent{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Wakeshift")
	req.Header.Set("webhook-id", msgID)
	req.Header.Set("webhook-timestamp", strconv.FormatInt(ts, 10))
	req.Header.Set("webhook-signature", Sign(msgID, ts, body, secrets...))
	req.Header.Set("X-MCP-Subscription-Id", sub)
	resp, err := c.Do(req)
	if err != nil {
		return Sent{}, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	return Sent{Status: resp.StatusCode, Body: b}, nil
}

// Failure reasons, as SEP-3415 names them (CallbackEndpointError's
// data.reason, lastError).
const (
	ReasonRefused   = "connection_refused"
	ReasonTimeout   = "timeout"
	ReasonTLS       = "tls_error"
	Reason4xx       = "http_4xx"
	Reason5xx       = "http_5xx"
	ReasonChallenge = "challenge_failed"
)

// CallbackError is a callback that failed verification.
type CallbackError struct{ Reason, Detail string }

func (e *CallbackError) Error() string { return "callback verification failed: " + e.Detail }

// reasonOf is the category of a failed request.
func reasonOf(err error) string {
	var ne net.Error
	switch {
	case errors.As(err, &ne) && ne.Timeout():
		return ReasonTimeout
	case strings.Contains(err.Error(), "tls:"), strings.Contains(err.Error(), "x509:"):
		return ReasonTLS
	}
	return ReasonRefused
}

// Verify sends the callback a signed verification challenge and checks it
// echoes it, before anything is delivered there.
func Verify(ctx context.Context, c *http.Client, callback, sub string, secret []byte, now time.Time) error {
	b := make([]byte, 24)
	rand.Read(b)
	challenge := base64.RawURLEncoding.EncodeToString(b)
	body, _ := json.Marshal(map[string]string{"type": "verification", "challenge": challenge})
	got, err := Send(ctx, c, callback, sub, "msg_verification_"+challenge[:16], body, now, secret)
	if err != nil {
		if errors.Is(err, errPrivate) {
			return &CallbackError{Reason: ReasonRefused, Detail: "the callback's address isn't public"}
		}
		return &CallbackError{Reason: reasonOf(err), Detail: "the callback couldn't be reached"}
	}
	switch {
	case got.Status/100 == 4:
		return &CallbackError{Reason: Reason4xx, Detail: fmt.Sprintf("the callback answered HTTP %d", got.Status)}
	case got.Status/100 == 3:
		return &CallbackError{Reason: ReasonChallenge, Detail: fmt.Sprintf("the callback answered HTTP %d (redirects aren't followed)", got.Status)}
	case got.Status/100 != 2:
		return &CallbackError{Reason: Reason5xx, Detail: fmt.Sprintf("the callback answered HTTP %d", got.Status)}
	}
	var out struct {
		Challenge string `json:"challenge"`
	}
	json.Unmarshal(got.Body, &out)
	if subtle.ConstantTimeCompare([]byte(out.Challenge), []byte(challenge)) != 1 {
		return &CallbackError{Reason: ReasonChallenge, Detail: "the callback didn't echo the challenge"}
	}
	return nil
}

// Envelope types (non-event webhook bodies besides verification).
const (
	EnvelopeGap        = "gap"
	EnvelopeTerminated = "terminated"
)

// ControlID is the webhook-id of a control envelope: msg_<type>_<random>.
func ControlID(typ string) string {
	b := make([]byte, 12)
	rand.Read(b)
	return "msg_" + typ + "_" + hex.EncodeToString(b)
}

// MessageID is a new webhook-id for a delivery to a webhook in the spec's
// dialect (event.WebhookIDIsEventID): msg_<random>, kept for its retries
// (deliveries.message_id).
func MessageID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return "msg_" + hex.EncodeToString(b)
}

// GapBody is a gap envelope: events were skipped; cursor is where delivery
// goes on from.
func GapBody(cursor string) []byte {
	b, _ := json.Marshal(map[string]string{"type": EnvelopeGap, "cursor": cursor})
	return b
}

// ReasonEventRemoved is why a webhook ends whose event type the app no
// longer offers (its manifest dropped it).
const ReasonEventRemoved = "The app no longer offers this event"

// TerminatedBody is a terminated envelope: the subscription ended because
// access did (Forbidden, in the dialect's numbering), or, with
// ReasonEventRemoved, because its event type is gone: NotFound with
// data.kind "event", as the draft says, so the client lists the events again
// rather than treating it as a refusal.
func TerminatedBody(dialect, reason string) []byte {
	codes := event.CodesFor(dialect)
	e := map[string]any{"code": codes.Forbidden, "message": "Forbidden", "data": map[string]string{"reason": reason}}
	if reason == ReasonEventRemoved {
		e = map[string]any{"code": codes.NotFound, "message": "NotFound", "data": map[string]string{"kind": "event", "reason": reason}}
	}
	b, _ := json.Marshal(map[string]any{"type": EnvelopeTerminated, "error": e})
	return b
}

// CategoryOf is the lastError category of a failed delivery: the draft's
// categories only, never the endpoint's own words, so it can't be used to
// read attacker-chosen URLs.
func CategoryOf(status int, err error) string {
	switch {
	case err != nil && errors.Is(err, errPrivate):
		return ReasonRefused
	case err != nil:
		return reasonOf(err)
	case status/100 == 4:
		return Reason4xx
	case status/100 == 5, status/100 == 3:
		return Reason5xx
	}
	return ""
}
