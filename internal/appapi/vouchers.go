package appapi

// Subscribe vouchers: how a native MCP Events client (ChatGPT) is subscribed
// to a tools-only event. When one of the app's tools subscribes a native
// caller (call.subscribe), the SDK sends /calls/addition a native line for
// the event, and the core hands out a voucher in the line it renders: a
// random token (wsv_…), stored only as a hash, good for one webhook
// subscription to exactly that event and those arguments, within VoucherTTL
// (four hours); for only the verified user it was handed to, if there was
// one. The client writes it back as the event argument wakeshift_voucher in
// events/subscribe; the SDK takes it out of the arguments and passes it here
// as voucher.
//
// A voucher before redemption creates one subscription, and nothing else.
// After, for as long as that subscription lives, it refreshes it at the
// same callback URL (an anonymous voucher's only with the same secret) and
// unsubscribes it given the callback URL. It never polls, never creates a
// second subscription and never reaches anything else.
//
// Every refusal of a voucher gives the same answer, 403 bad_voucher with
// one message; the reason goes to the app's log (app.voucher_refused),
// never the token. The token is never logged, stored or put in an error.
//
// The whole feature is behind API.SubscribeVouchers (the core flag
// WAKESHIFT_SUBSCRIBE_VOUCHERS): off, nothing is handed out, every request
// carrying a voucher is refused (disabled), and the manifest response
// doesn't report the feature, so new SDKs don't list tools-only events.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/wakeshift/server/internal/apps"
	"github.com/wakeshift/server/internal/audit"
	"github.com/wakeshift/server/internal/db"
	"github.com/wakeshift/server/internal/httpx"
	"github.com/wakeshift/server/internal/mcpevents"
	"github.com/wakeshift/server/internal/sessions"
)

// FeatureSubscribeVouchers is the feature the manifest response reports
// while vouchers are on (manifestOut.Features).
const FeatureSubscribeVouchers = "subscribe_vouchers"

// Limits.
const (
	voucherPrefix = "wsv_"
	// VoucherTTL is how long an unused voucher can be redeemed: four hours.
	// A person who asks at once redeems it within seconds, but one who
	// comes back later can take more than an hour.
	VoucherTTL = 4 * time.Hour
	// Unused vouchers at once: per verified user of an app, and anonymous
	// ones per app, in separate pools (anonymous farming can't starve
	// signed-in users).
	voucherUserCap      = 50
	voucherAnonymousCap = 1000
	// A voucher whose callback failed verification this many times is spent.
	voucherMaxFailures = 3
	// Live anonymous voucher subscriptions: per event and arguments, and
	// per app. A user's are bounded by mcpevents.MaxWebhooks instead.
	voucherFanoutPerKey = 100
	voucherFanoutPerApp = 2000
	// Anonymous redemptions verify at most this many callbacks a minute per
	// app, and at most voucherPerHost at once per callback host (per
	// instance).
	voucherVerifyPerMinute = 30
	voucherPerHost         = 4
)

// VoucherClaimFor is how long a redemption's claim keeps another from
// verifying a callback with the same voucher (a variable so tests can
// shorten it).
var VoucherClaimFor = 30 * time.Second

var (
	voucherRE          = regexp.MustCompile(`^wsv_[a-z2-7]{32}$`)
	transportSessionRE = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// badVoucherMessage is every voucher refusal's message.
const badVoucherMessage = "This voucher can't subscribe to this event: it was used, it expired, or it's for other arguments."

// Why a voucher was refused (app.voucher_refused's reason).
const (
	vrDisabled  = "disabled"
	vrFormat    = "format"
	vrUnknown   = "unknown"
	vrBinding   = "binding"
	vrUser      = "user"
	vrSession   = "session"
	vrPolicy    = "policy"
	vrUsed      = "used"
	vrExpired   = "expired"
	vrAttempts  = "attempts"
	vrClaimed   = "claimed"
	vrRotation  = "rotation"
	vrEnded     = "ended" // its subscription's ttl ran out: ended on this refresh
	vrFanout    = "fanout"
	vrWorkspace = "workspace_cap"
)

// errBadVoucher is a refused voucher (403 bad_voucher). reason is for the
// app's log only.
type errBadVoucher struct{ reason string }

func (e errBadVoucher) Error() string { return badVoucherMessage }

// errCallbackGeneric is an anonymous redeemer's callback that failed
// verification (or wasn't tried, past the budget): one reason, whatever
// happened (400 callback_failed).
type errCallbackGeneric struct{ detail string }

func (e errCallbackGeneric) Error() string { return "callback verification failed" }

// Evident from the code, for the app's log.
const (
	logVoucherRefused   = "app.voucher_refused"
	logVoucherNotIssued = "app.voucher_not_issued"
)

// newVoucher is a new token: wsv_ and 32 lowercase base32 characters (160
// random bits).
func newVoucher() string {
	b := make([]byte, 20)
	rand.Read(b)
	return voucherPrefix + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b))
}

// voucherHash is how a token is kept: its SHA-256.
func voucherHash(token string) []byte {
	h := sha256.Sum256([]byte(token))
	return h[:]
}

// transportSession is the hash of the request's Mcp-Session-Id the SDK
// sent (call.transport_session), or "" if it sent none or it isn't one.
func transportSession(call callIn) string {
	if transportSessionRE.MatchString(call.TransportSession) {
		return call.TransportSession
	}
	return ""
}

// ── Handing out (/calls/addition) ─────────────────────────────────────────

// voucherLine is the line a native caller gets for a tools-only event:
// the voucher line, the line saying its user is already subscribed, or
// nothing ("") when no voucher can be handed out (the flag, the call, a
// cap, an error). It never fails the call: what went wrong goes to the
// app's log.
func (a *API) voucherLine(ctx context.Context, app apps.App, call callIn, user string, where []userWorkspace, et apps.EventType, n nativeLine) string {
	if !a.SubscribeVouchers || call.AgentToken != "" {
		return ""
	}
	logNo := func(reason string) {
		if err := apps.Log(ctx, a.DB.Pool, apps.LogEntry{AppID: app.ID, Actor: audit.App(app.ID), Action: logVoucherNotIssued,
			Detail: map[string]any{"reason": reason, "event": et.Name}}); err != nil {
			slog.Warn("vouchers: logging a voucher not handed out", "app", app.ID, "err", err)
		}
	}
	tool := beaconTool(call.Tool)
	if tool == "" || tool == builtinSubscribe {
		logNo("no_tool")
		return ""
	}
	sub, err := et.Subscribe(n.Arguments)
	if err != nil {
		logNo("arguments")
		return ""
	}
	var ttl, replay *time.Duration
	if n.TTL != nil {
		d := time.Duration(*n.TTL)
		if d < minTopicTTL || d > maxTopicTTL {
			logNo("options")
			return ""
		}
		ttl = &d
	}
	if n.Replay != nil {
		d := time.Duration(*n.Replay)
		if d < 0 || d > maxReplay {
			logNo("options")
			return ""
		}
		replay = &d
	}
	var appUserID *string
	switch {
	case appUser(user):
		appUserID = &user
	case user != "":
		logNo("user")
		return ""
	}
	// Its user already has a live native subscription to it: that's what
	// the line says, and no voucher.
	for _, uw := range where {
		var live bool
		if err := a.DB.InTenant(ctx, uw.tenant, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM event_subscriptions es WHERE es.source_kind = $1 AND es.source_id = $2
				AND es.event = $3 AND es.arguments_key = $4 AND es.destination = 'webhook' AND es.ended_at IS NULL AND es.principal = $5
				AND ($6 = '' OR es.agent_id = $6) AND NOT `+webhookGone+`)`, sourceApp, app.ID, et.Name, sub.Key, user, uw.agent).Scan(&live)
		}); err != nil {
			slog.Warn("vouchers: checking for a live subscription", "app", app.ID, "err", err)
			logNo("error")
			return ""
		}
		if live {
			return "You're subscribed to " + et.Name + " with " + string(sub.Arguments) + ": its events reach you."
		}
	}
	var session *string
	if appUserID == nil {
		s := transportSession(call)
		if s == "" {
			// Anonymous, without a transport session: bound to nothing, the
			// voucher would be a bearer token for whoever reads it (it's in
			// the conversation's text), so none is handed out, and the
			// caller gets the result it gets without vouchers.
			logNo(vrNoSession)
			return ""
		}
		session = &s
	}
	token := newVoucher()
	capped := ""
	err = pgx.BeginFunc(ctx, a.DB.Pool, func(tx pgx.Tx) error {
		// The pool's count and the insert, one hand-out at a time per pool.
		pool := "~anonymous"
		if appUserID != nil {
			pool = *appUserID
		}
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('subscribe-vouchers:' || $1 || ':' || $2, 0))`, app.ID, pool); err != nil {
			return err
		}
		var unused int
		limit := voucherAnonymousCap
		if appUserID != nil {
			limit = voucherUserCap
		}
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM (SELECT 1 FROM subscribe_vouchers WHERE app_id = $1 AND app_user IS NOT DISTINCT FROM $2
			AND used_at IS NULL AND expires_at > now() LIMIT $3) v`, app.ID, appUserID, limit).Scan(&unused); err != nil {
			return err
		}
		if unused >= limit {
			capped = "user_cap"
			if appUserID == nil {
				capped = "anonymous_cap"
			}
			return nil
		}
		_, err := tx.Exec(ctx, `INSERT INTO subscribe_vouchers (token_hash, app_id, event, arguments_key, app_user, transport_session, tool, once, ttl, replay, expires_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, make_interval(secs => $9::float8), make_interval(secs => $10::float8), now() + make_interval(secs => $11))`,
			voucherHash(token), app.ID, et.Name, sub.Key, appUserID, session, tool, n.Once, seconds(ttl), seconds(replay), VoucherTTL.Seconds())
		return err
	})
	switch {
	case err != nil:
		slog.Warn("vouchers: handing out a voucher", "app", app.ID, "err", err)
		logNo("error")
		return ""
	case capped == "anonymous_cap":
		a.logAnonymousCap(ctx, app, et.Name)
		return ""
	case capped != "":
		logNo(capped)
		return ""
	}
	return "To be woken by " + et.Name + " with " + string(sub.Arguments) + ", subscribe to it (MCP Events) with wakeshift_voucher set to " +
		token + " in its arguments. It works once, for these arguments."
}

// seconds is d in seconds, nil for none.
func seconds(d *time.Duration) *float64 {
	if d == nil {
		return nil
	}
	s := d.Seconds()
	return &s
}

// logAnonymousCap records an anonymous voucher not handed out because the
// app's pool is full, at most once a minute, with how many since.
func (a *API) logAnonymousCap(ctx context.Context, app apps.App, event string) {
	a.logRateLimited(ctx, app, "cap", map[string]any{"reason": "anonymous_cap", "event": event}, logVoucherNotIssued)
}

// logRateLimited writes action to the app's log at most once a minute per
// kind (cap: the anonymous pool; probe: a malformed or unknown voucher),
// with how many happened since the last entry (count).
func (a *API) logRateLimited(ctx context.Context, app apps.App, kind string, detail map[string]any, action string) {
	at, n := kind+"_logged_at", kind+"_refused"
	if kind != "cap" && kind != "probe" {
		return
	}
	err := pgx.BeginFunc(ctx, a.DB.Pool, func(tx pgx.Tx) error {
		var count int
		var due bool
		err := tx.QueryRow(ctx, `INSERT INTO subscribe_voucher_counters (app_id, `+n+`) VALUES ($1, 1)
			ON CONFLICT (app_id) DO UPDATE SET `+n+` = subscribe_voucher_counters.`+n+` + 1
			RETURNING `+n+`, `+at+` < now() - interval '1 minute'`, app.ID).Scan(&count, &due)
		if err != nil || !due {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE subscribe_voucher_counters SET `+at+` = now(), `+n+` = 0 WHERE app_id = $1`, app.ID); err != nil {
			return err
		}
		detail["count"] = count
		return apps.Log(ctx, tx, apps.LogEntry{AppID: app.ID, Actor: audit.App(app.ID), Action: action, Detail: detail})
	})
	if err != nil {
		slog.Warn("vouchers: logging", "app", app.ID, "err", err)
	}
}

// ── Redeeming (/events/subscribe) ─────────────────────────────────────────

// voucherRow is a voucher as the redemption reads it.
type voucherRow struct {
	Event, Key, Tool string
	AppUser          *string
	Session          *string
	Once             bool
	TTL, Replay      *float64 // seconds
	IssuedAt         time.Time
	UsedAt           *time.Time
	Tenant, EventSub *string
}

// lookupVoucher reads the app's voucher with this token: one indexed read
// by (hash, app), so another app's voucher, an unknown token and one whose
// workspace is gone (its row went with it) are all not found (nil).
func (a *API) lookupVoucher(ctx context.Context, app, token string) (*voucherRow, error) {
	var v voucherRow
	err := a.DB.Pool.QueryRow(ctx, `SELECT event, arguments_key, tool, app_user, transport_session, once, extract(epoch FROM ttl)::float8,
		extract(epoch FROM replay)::float8, issued_at, used_at, tenant_id::text, event_subscription_id::text
		FROM subscribe_vouchers WHERE token_hash = $1 AND app_id = $2`, voucherHash(token), app).
		Scan(&v.Event, &v.Key, &v.Tool, &v.AppUser, &v.Session, &v.Once, &v.TTL, &v.Replay, &v.IssuedAt, &v.UsedAt, &v.Tenant, &v.EventSub)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &v, err
}

// refuseVoucher answers a refused voucher (403 bad_voucher, the same for
// every reason) and records why in the app's log: the reason and the
// event, never the token. What anyone can send without a voucher (a
// malformed or unknown one, or with the feature off) is logged at most
// once a minute, with a count.
func (a *API) refuseVoucher(ctx context.Context, w http.ResponseWriter, app apps.App, event, reason string) {
	if len(event) > 64 {
		event = event[:64]
	}
	detail := map[string]any{"reason": reason, "event": event}
	switch reason {
	case vrFormat, vrUnknown, vrDisabled:
		a.logRateLimited(ctx, app, "probe", detail, logVoucherRefused)
	default:
		if err := apps.Log(ctx, a.DB.Pool, apps.LogEntry{AppID: app.ID, Actor: audit.App(app.ID), Action: logVoucherRefused, Detail: detail}); err != nil {
			slog.Warn("vouchers: logging a refused voucher", "app", app.ID, "err", err)
		}
	}
	httpx.WriteError(w, http.StatusForbidden, "bad_voucher", badVoucherMessage)
}

// voucherBinding checks a found voucher against the request (steps 4 and
// 5): its event and arguments, its user, its transport session, and that
// the event is still tools-only. It returns the reason it's refused, or "".
func voucherBinding(v *voucherRow, et apps.EventType, sub apps.Subscription, call callIn) string {
	user := strings.TrimSpace(call.User)
	switch {
	case v.Event != sub.Event || subtle.ConstantTimeCompare([]byte(v.Key), []byte(sub.Key)) != 1:
		return vrBinding
	case v.AppUser != nil && (user == "" || subtle.ConstantTimeCompare([]byte(*v.AppUser), []byte(user)) != 1):
		return vrUser
	case v.Session != nil && subtle.ConstantTimeCompare([]byte(*v.Session), []byte(transportSession(call))) != 1:
		return vrSession
	case !et.ToolsOnly():
		return vrPolicy
	}
	return ""
}

// voucherSubscribe is events/subscribe with a voucher (eventsubs.go hands
// it over): the checks in order, then a claim, the callback's
// verification, and the subscription's creation (or, with a used voucher,
// the refresh of the subscription it made).
func (a *API) voucherSubscribe(w http.ResponseWriter, r *http.Request, in eventsIn) {
	ctx := r.Context()
	app := AppFrom(ctx)
	// 1. The flag, before anything else; 2. the token's form, before any
	// lookup.
	if !a.SubscribeVouchers {
		a.refuseVoucher(ctx, w, app, in.Name, vrDisabled)
		return
	}
	if !voucherRE.MatchString(in.Voucher) {
		a.refuseVoucher(ctx, w, app, in.Name, vrFormat)
		return
	}
	// The request itself, as for any native subscribe: nothing here depends
	// on the voucher.
	args, _ := json.Marshal(in.Arguments)
	user := strings.TrimSpace(in.Call.User)
	identified := appUser(user)
	et, err := app.Manifest.EventType(in.Name)
	var sub apps.Subscription
	if err == nil {
		sub, err = eventTarget(app, in.Name, args, user)
	}
	if err == nil && user != "" && !identified {
		err = badRequest{errors.New("call.user is at most 200 characters, not starting with ~")}
	}
	if err == nil && len(in.Call.Client) > 200 {
		err = badRequest{errors.New("call.client is at most 200 characters")}
	}
	if err == nil && in.Delivery.Mode != "webhook" {
		err = errUnsupportedMode{in.Delivery.Mode}
	}
	if err == nil {
		if cerr := mcpevents.CheckURL(in.Delivery.URL); cerr != nil {
			err = a.unreachable(in.Call, in.dialect(), badRequest{cerr})
		}
	}
	if err == nil && in.Cursor != nil {
		if _, cerr := mcpevents.ParseCursor(*in.Cursor); cerr != nil {
			err = badRequest{cerr}
		}
	}
	var secret []byte
	if err == nil {
		if secret, err = mcpevents.ParseSecret(in.Delivery.Secret); err != nil {
			err = badRequest{err}
		}
	}
	if err != nil {
		a.respondEventSub(w, nil, err)
		return
	}
	// 3. The lookup; 4. the binding; 5. the event's policy.
	v, err := a.lookupVoucher(ctx, app.ID, in.Voucher)
	if err != nil {
		a.respond(w, nil, err)
		return
	}
	if v == nil {
		a.refuseVoucher(ctx, w, app, in.Name, vrUnknown)
		return
	}
	if reason := voucherBinding(v, et, sub, in.Call); reason != "" {
		a.refuseVoucher(ctx, w, app, in.Name, reason)
		return
	}
	// Anonymous, bound to no transport session (handed out before those
	// stopped being handed out): a bearer token, refused as push refuses
	// it, for a redemption and a refresh alike. Its holder may not be the
	// caller it was handed to. A subscription one made isn't renewed, so
	// it lapses at its ttl.
	if v.AppUser == nil && v.Session == nil {
		a.refuseVoucher(ctx, w, app, in.Name, vrNoSession)
		return
	}
	// 6. Used: a refresh of the subscription it made, or nothing.
	if v.UsedAt != nil {
		a.voucherRefresh(w, r, in, sub, v, secret)
		return
	}
	hash := voucherHash(in.Voucher)
	// 7. Expiry, attempts and the claim, judged once, here, by the
	// database's clock.
	claim := make([]byte, 16)
	rand.Read(claim)
	claimID := base64.RawURLEncoding.EncodeToString(claim)
	reason := ""
	err = pgx.BeginFunc(ctx, a.DB.Pool, func(tx pgx.Tx) error {
		var expired, claimed, used bool
		var failures int
		err := tx.QueryRow(ctx, `SELECT now() >= expires_at, verify_failures, claimed_until IS NOT NULL AND claimed_until > now(), used_at IS NOT NULL
			FROM subscribe_vouchers WHERE token_hash = $1 AND app_id = $2 FOR UPDATE`, hash, app.ID).Scan(&expired, &failures, &claimed, &used)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			reason = vrUnknown
			return nil
		case err != nil:
			return err
		case used:
			reason = vrUsed
		case expired:
			reason = vrExpired
		case failures >= voucherMaxFailures:
			reason = vrAttempts
		case claimed:
			reason = vrClaimed
		}
		if reason != "" {
			return nil
		}
		_, err = tx.Exec(ctx, `UPDATE subscribe_vouchers SET claim_id = $3, claimed_until = now() + make_interval(secs => $4) WHERE token_hash = $1 AND app_id = $2`,
			hash, app.ID, claimID, VoucherClaimFor.Seconds())
		return err
	})
	if err != nil {
		a.respond(w, nil, err)
		return
	}
	if reason != "" {
		a.refuseVoucher(ctx, w, app, in.Name, reason)
		return
	}
	// From here, whatever fails gives the claim back (countFailure: and
	// counts a failed verification).
	release := func(countFailure bool) {
		// Not the request's context: a client that hung up mustn't leave
		// the voucher claimed.
		bg, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		inc := 0
		if countFailure {
			inc = 1
		}
		if _, err := a.DB.Pool.Exec(bg, `UPDATE subscribe_vouchers SET verify_failures = verify_failures + $4,
			claim_id = CASE WHEN claim_id = $3 THEN NULL ELSE claim_id END, claimed_until = CASE WHEN claim_id = $3 THEN NULL ELSE claimed_until END
			WHERE token_hash = $1 AND app_id = $2 AND used_at IS NULL`, hash, app.ID, claimID, inc); err != nil {
			slog.Warn("vouchers: releasing a claim", "app", app.ID, "err", err)
		}
	}

	// The workspace: a verified redeemer's own; for nobody verified, a new
	// conversation's, made only at creation (step 9), so nothing is left if
	// this fails before.
	call := in.Call
	call.Session, call.User = "", user
	var cv *conversation
	if identified {
		if cv, err = a.findConversation(ctx, app, call, true); err != nil {
			release(false)
			a.respondEventSub(w, nil, err)
			return
		}
	} else {
		cv = &conversation{app: app, call: call}
		var agent string
		cv.harness, agent, cv.recognized, cv.variant, cv.claim = harnessOf(call)
		cv.external, _ = newConversation()
		cv.tenant = newUUID()
		cv.act = actor{app: app.ID, tenant: cv.tenant, agent: agent}
	}
	principal := cv.call.User
	if !identified {
		principal = cv.external
	}
	id := mcpevents.SubscriptionID(cv.tenant, "event:"+sub.Key, in.Delivery.URL, in.Name)

	// 8. The callback's verification, outside any transaction.
	exists := false
	if identified {
		// A verified redeemer's callback is verified once per URL
		// (verifiedFor); an anonymous redemption's principal is new each
		// time, so it never has one.
		err = a.DB.InTenant(ctx, cv.tenant, func(tx pgx.Tx) (err error) {
			if exists, err = mcpevents.Exists(ctx, tx, id); err == nil && !exists {
				exists, err = verifiedFor(ctx, tx, app.ID, principal, in.Delivery.URL)
			}
			return err
		})
		if err != nil {
			release(false)
			a.respond(w, nil, err)
			return
		}
	}
	if !exists {
		if !identified {
			if !a.verifyBudget(ctx, app.ID) {
				release(false)
				a.respondVoucherCallback(ctx, w, app, in.Name, false, errCallbackGeneric{"budget"})
				return
			}
			done, ok := a.hostSlot(in.Delivery.URL)
			if !ok {
				release(false)
				a.respondVoucherCallback(ctx, w, app, in.Name, false, errCallbackGeneric{"host_busy"})
				return
			}
			err = a.verifyCallback(ctx, app.ID, "anonymous", in.Delivery.URL, id, secret) // anonymous redemptions share one count per app and host
			done()
		} else {
			err = a.verifyCallback(ctx, app.ID, "user:"+principal, in.Delivery.URL, id, secret)
		}
		if err != nil {
			release(true)
			a.respondVoucherCallback(ctx, w, app, in.Name, identified, a.unreachable(in.Call, in.dialect(), err))
			return
		}
	}

	// 9. Creation, in the subscription's transaction.
	var res mcpevents.Subscribed
	var evID string
	created := false
	err = a.DB.InTenant(ctx, cv.tenant, func(tx pgx.Tx) error {
		if !identified {
			// The anonymous redemption's own workspace, named and capped as
			// an anonymous conversation's.
			if err := apps.CheckWorkspaceCap(ctx, tx, app, cv.external); err != nil {
				if apps.IsWorkspaceLimit(err) || apps.IsWorkspaceRate(err) {
					return errBadVoucher{vrWorkspace}
				}
				return err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO tenants (id, name, kind, app_id, external_id) VALUES ($1, $2, 'app', $3, $4)`,
				cv.tenant, app.Name+" conversation", app.ID, cv.external); err != nil {
				return err
			}
		}
		// The claim must still be this request's: one whose claim lapsed and
		// was taken by another creates nothing.
		var owner *string
		var used bool
		if err := tx.QueryRow(ctx, `SELECT claim_id, used_at IS NOT NULL FROM subscribe_vouchers WHERE token_hash = $1 AND app_id = $2 FOR UPDATE`,
			hash, app.ID).Scan(&owner, &used); errors.Is(err, pgx.ErrNoRows) {
			return errBadVoucher{vrUnknown}
		} else if err != nil {
			return err
		}
		if used || owner == nil || subtle.ConstantTimeCompare([]byte(*owner), []byte(claimID)) != 1 {
			return errBadVoucher{vrClaimed}
		}
		if !identified {
			// Anonymous fan-out, counted under the app's lock so two
			// redemptions can't both pass.
			if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('voucher-fanout:' || $1, 0))`, app.ID); err != nil {
				return err
			}
			var perKey, perApp int
			if err := tx.QueryRow(ctx, `SELECT count(*) FILTER (WHERE v.event = $2 AND v.arguments_key = $3), count(*)
				FROM subscribe_vouchers v JOIN tenants t ON t.id = v.tenant_id
				WHERE v.app_id = $1 AND v.used_at IS NOT NULL AND v.live_until > now() AND starts_with(t.external_id, '`+conversationPrefix+`')`,
				app.ID, sub.Event, sub.Key).Scan(&perKey, &perApp); err != nil {
				return err
			}
			if perKey >= voucherFanoutPerKey || perApp >= voucherFanoutPerApp {
				return errBadVoucher{vrFanout}
			}
		}
		var err error
		res, created, evID, err = a.voucherCreate(ctx, tx, app, cv, principal, sub, v, in, secret, id)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE subscribe_vouchers SET used_at = now(), tenant_id = current_tenant(), event_subscription_id = $3::uuid,
			claim_id = NULL, claimed_until = NULL, live_until = $4 WHERE token_hash = $1 AND app_id = $2`, hash, app.ID, evID, res.ExpiresAt); err != nil {
			return err
		}
		bound := "anonymous"
		if v.AppUser != nil {
			bound = "user"
		}
		return audit.Record(ctx, tx, audit.Entry{Actor: audit.App(app.ID), Action: audit.VoucherRedeemed, AgentID: cv.act.agent, AppID: app.ID,
			GrantID: cv.act.grant.ID, Detail: map[string]any{"id": evID, "event": sub.Event, "arguments": json.RawMessage(sub.Arguments),
				"tool": v.Tool, "destination": toWebhook, "workspace": cv.tenant, "voucher": bound}})
	})
	var bad errBadVoucher
	switch {
	case errors.As(err, &bad):
		release(false)
		a.refuseVoucher(ctx, w, app, in.Name, bad.reason)
		return
	case err != nil:
		release(false)
		a.respondEventSub(w, nil, err)
		return
	}
	out := map[string]any{"id": id, "refresh_before": res.ExpiresAt, "cursor": res.Cursor, "truncated": res.Truncated, "created": created,
		"via": madeByToolPrefix + v.Tool, "tool": v.Tool}
	if res.Status != nil {
		out["delivery_status"] = res.Status
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// voucherCreate makes the voucher's subscription in tx's workspace (cv's),
// as a native webhook subscribe does, with via tool:<its tool>, its ttl,
// once and a replay since the voucher was handed out. A live subscription
// of the same principal, callback, event and arguments (only a race can
// leave one: the hand-out gives no voucher then) is refreshed instead, and
// the voucher names it. evID is the event subscription's ID.
func (a *API) voucherCreate(ctx context.Context, tx pgx.Tx, app apps.App, cv *conversation, principal string, sub apps.Subscription,
	v *voucherRow, in eventsIn, secret []byte, id string) (res mcpevents.Subscribed, created bool, evID string, err error) {
	via := madeByToolPrefix + v.Tool
	ttl := mcpevents.TTLOf(in.TTLMs)
	if v.TTL != nil {
		ttl = min(ttl, time.Duration(*v.TTL*float64(time.Second)))
	}
	var maxAge time.Duration
	if in.MaxAgeMs != nil {
		maxAge = mcpevents.MaxAgeOf(*in.MaxAgeMs)
	}
	filters, ferr := topicFilters(sub.Topic, sub.Filter, v.Once)
	if err = ferr; err != nil {
		return res, false, "", err
	}
	locked, err := lockTopic(ctx, tx, app.ID, sub.Topic)
	if err != nil {
		return res, false, "", err
	}
	if err := lockStripe(ctx, tx, app.ID, sub.Topic); err != nil {
		return res, false, "", err
	}
	if err := ensureActor(ctx, tx, cv); err != nil {
		return res, false, "", err
	}
	if err := serialize(ctx, tx, "webhook:"+id); err != nil {
		return res, false, "", err
	}
	if _, err := closeLapsed(ctx, tx); err != nil {
		return res, false, "", err
	}
	var subID, sessionID, agent string
	err = tx.QueryRow(ctx, `SELECT id::text, subscription_id::text, session_id::text, agent_id FROM event_subscriptions
		WHERE source_kind = $1 AND source_id = $2 AND principal = $3 AND webhook_url = $4 AND event = $5 AND arguments_key = $6
		AND destination = 'webhook' AND ended_at IS NULL FOR UPDATE`,
		sourceApp, app.ID, principal, in.Delivery.URL, sub.Event, sub.Key).Scan(&evID, &subID, &sessionID, &agent)
	created = errors.Is(err, pgx.ErrNoRows)
	switch {
	case err == nil && agent != cv.act.agent:
		return res, false, "", errOtherClient
	case err == nil:
		if cv.session, err = sessions.Get(ctx, tx, sessionID); err != nil {
			return res, false, "", err
		}
	case created:
		if cv.session, err = newSession(ctx, tx, cv, ""); err != nil {
			return res, false, "", err
		}
	default:
		return res, false, "", err
	}
	cv.handle = handleOf(cv.session)
	if res, err = mcpevents.Subscribe(ctx, tx, a.Events, mcpevents.Webhook{ID: id, App: app.ID, Agent: cv.act.agent, Session: cv.session.ID,
		SessionHandle: cv.handle, Name: in.Name, URL: in.Delivery.URL, Secret: secret, TTL: ttl,
		Dialect: in.dialect(), Cursor: in.Cursor, MaxAge: maxAge}, a.now()); err != nil {
		return res, false, "", err
	}
	topicTTL := max(ttl, minTopicTTL)
	if created {
		if subID, err = insertTopicSub(ctx, tx, cv, sub.Topic, filters, v.Once, topicTTL); err != nil {
			return res, false, "", err
		}
		if evID, err = recordEventSubAs(ctx, tx, app, sub, cv, principal, subID, toWebhook, via, in.Delivery.URL, id); err != nil {
			return res, false, "", err
		}
		// Always a replay: the latest occurrence since the voucher was
		// handed out (a person was between the result and this subscribe,
		// and the job has likely finished), or within its replay if longer.
		// Measured on the database's clock, as published_at is.
		var since float64
		if err := tx.QueryRow(ctx, `SELECT extract(epoch FROM now() - $1::timestamptz)::float8`, v.IssuedAt).Scan(&since); err != nil {
			return res, false, "", err
		}
		window := time.Duration(max(since, 0) * float64(time.Second))
		if v.Replay != nil {
			window = max(window, time.Duration(*v.Replay*float64(time.Second)))
		}
		if err := a.replay(ctx, tx, app, sub.Topic, subID, filters, locked, min(window, maxReplay+VoucherTTL)); err != nil {
			return res, false, "", err
		}
	} else {
		if err := refreshTopicSub(ctx, tx, subID, sub.Topic, filters, v.Once, topicTTL); err != nil {
			return res, false, "", err
		}
		if _, err := tx.Exec(ctx, `UPDATE event_subscriptions SET refreshed_at = now() WHERE id = $1::uuid`, evID); err != nil {
			return res, false, "", err
		}
	}
	return res, created, evID, sessions.SetWatching(ctx, tx, cv.session.ID, true)
}

// voucherRefresh is events/subscribe with a used voucher: it
// refreshes the subscription the voucher made, at the same callback URL,
// while that's live; an anonymous voucher's only with the stored secret
// (no rotation: the secret is the client's, never in its transcript), and
// one redeemed by a verified user only for that user. Every refresh stays
// within the vendor's ttl (ends_at); one after it ends the subscription
// (expired) and is refused. Anything else is refused (used).
func (a *API) voucherRefresh(w http.ResponseWriter, r *http.Request, in eventsIn, sub apps.Subscription, v *voucherRow, secret []byte) {
	ctx := r.Context()
	app := AppFrom(ctx)
	user := strings.TrimSpace(in.Call.User)
	if v.Tenant == nil || v.EventSub == nil {
		a.refuseVoucher(ctx, w, app, in.Name, vrUsed)
		return
	}
	tenant := *v.Tenant
	id := mcpevents.SubscriptionID(tenant, "event:"+sub.Key, in.Delivery.URL, in.Name)
	var res mcpevents.Subscribed
	ended := false
	err := a.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		list, err := queryEventSubs(ctx, tx, `es.id = $1::uuid AND es.source_kind = $2 AND es.source_id = $3 AND es.ended_at IS NULL
			AND es.destination = 'webhook' FOR UPDATE OF es`, *v.EventSub, sourceApp, app.ID)
		if err != nil {
			return err
		}
		if len(list) == 0 {
			return errBadVoucher{vrUsed}
		}
		es := list[0]
		var hookURL, principal string
		if err := tx.QueryRow(ctx, `SELECT coalesce(webhook_url, ''), principal FROM event_subscriptions WHERE id = $1::uuid`, es.ID).Scan(&hookURL, &principal); err != nil {
			return err
		}
		if hookURL != in.Delivery.URL || es.webhookID != id {
			return errBadVoucher{vrUsed}
		}
		// Redeemed by a verified user: refreshed only for that user.
		if !strings.HasPrefix(principal, "~") && subtle.ConstantTimeCompare([]byte(principal), []byte(user)) != 1 {
			return errBadVoucher{vrUser}
		}
		var sealed []byte
		var live bool
		err = tx.QueryRow(ctx, `SELECT secret, expires_at > now() AND ending_reason IS NULL FROM mcp_event_webhooks WHERE id = $1 FOR UPDATE`, id).Scan(&sealed, &live)
		if errors.Is(err, pgx.ErrNoRows) || err == nil && !live {
			return errBadVoucher{vrUsed}
		}
		if err != nil {
			return err
		}
		if v.AppUser == nil {
			stored, err := a.Events.Open("mcp-events:"+id, sealed)
			if err != nil || subtle.ConstantTimeCompare(stored, secret) != 1 {
				return errBadVoucher{vrRotation}
			}
		}
		ttl := mcpevents.TTLOf(in.TTLMs)
		if v.TTL != nil {
			var left float64
			if err := tx.QueryRow(ctx, `SELECT extract(epoch FROM used_at + ttl - now())::float8 FROM subscribe_vouchers WHERE tenant_id = current_tenant()
				AND event_subscription_id = $1::uuid AND app_id = $2`, es.ID, app.ID).Scan(&left); err != nil {
				return err
			}
			if left <= 0 {
				ended = true
				return endEventSubs(ctx, tx, list, evEndExpired, audit.App(app.ID), true)
			}
			ttl = min(ttl, time.Duration(left*float64(time.Second)))
		}
		var maxAge time.Duration
		if in.MaxAgeMs != nil {
			maxAge = mcpevents.MaxAgeOf(*in.MaxAgeMs)
		}
		filters, err := topicFilters(sub.Topic, sub.Filter, v.Once)
		if err != nil {
			return err
		}
		if _, err := lockTopic(ctx, tx, app.ID, sub.Topic); err != nil {
			return err
		}
		if err := lockStripe(ctx, tx, app.ID, sub.Topic); err != nil {
			return err
		}
		cv := &conversation{app: app, call: in.Call, tenant: tenant, act: actor{app: app.ID, tenant: tenant, agent: es.AgentID}}
		if err := ensureActor(ctx, tx, cv); err != nil {
			return err
		}
		if err := serialize(ctx, tx, "webhook:"+id); err != nil {
			return err
		}
		if cv.session, err = sessions.Get(ctx, tx, es.sessionID); err != nil {
			return err
		}
		cv.handle = handleOf(cv.session)
		if res, err = mcpevents.Subscribe(ctx, tx, a.Events, mcpevents.Webhook{ID: id, App: app.ID, Agent: cv.act.agent, Session: cv.session.ID,
			SessionHandle: cv.handle, Name: in.Name, URL: in.Delivery.URL, Secret: secret, TTL: ttl,
			Dialect: in.dialect(), Cursor: in.Cursor, MaxAge: maxAge}, a.now()); err != nil {
			return err
		}
		if err := refreshTopicSub(ctx, tx, es.subscriptionID, sub.Topic, filters, v.Once, max(ttl, minTopicTTL)); errors.Is(err, errRoutingEnded) {
			return errBadVoucher{vrUsed}
		} else if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE event_subscriptions SET refreshed_at = now() WHERE id = $1::uuid`, es.ID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE subscribe_vouchers SET live_until = $3 WHERE tenant_id = current_tenant() AND event_subscription_id = $1::uuid AND app_id = $2`,
			es.ID, app.ID, res.ExpiresAt); err != nil {
			return err
		}
		return sessions.SetWatching(ctx, tx, cv.session.ID, true)
	})
	var bad errBadVoucher
	switch {
	case err == nil && ended:
		// Committed: the subscription ended (its ttl ran out); the refresh is
		// refused.
		a.endedVoucherLiveUntil(ctx, *v.EventSub)
		a.refuseVoucher(ctx, w, app, in.Name, vrEnded)
		return
	case errors.As(err, &bad):
		a.refuseVoucher(ctx, w, app, in.Name, bad.reason)
		return
	case err != nil:
		a.respondEventSub(w, nil, err)
		return
	}
	out := map[string]any{"id": id, "refresh_before": res.ExpiresAt, "cursor": res.Cursor, "truncated": res.Truncated, "created": false,
		"via": madeByToolPrefix + v.Tool, "tool": v.Tool}
	if res.Status != nil {
		out["delivery_status"] = res.Status
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// endedVoucherLiveUntil records that the voucher subscription eventSub
// ended now (for the fan-out caps and the expiry pass).
func (a *API) endedVoucherLiveUntil(ctx context.Context, eventSub string) {
	if _, err := a.DB.Pool.Exec(ctx, `UPDATE subscribe_vouchers SET live_until = least(live_until, now()) WHERE event_subscription_id = $1::uuid`, eventSub); err != nil {
		slog.Warn("vouchers: recording a subscription's end", "err", err)
	}
}

// respondVoucherCallback answers a failed verification: an identified
// redeemer, as any identified native subscriber is (the reason); an
// anonymous one, one generic reason whatever happened, the detail going to
// the app's log.
func (a *API) respondVoucherCallback(ctx context.Context, w http.ResponseWriter, app apps.App, event string, identified bool, err error) {
	detail := map[string]any{"reason": "callback", "event": event}
	var cb *mcpevents.CallbackError
	var gen errCallbackGeneric
	switch {
	case errors.As(err, &cb):
		detail["callback"] = cb.Reason
	case errors.As(err, &gen):
		detail["callback"] = gen.detail
	}
	if lerr := apps.Log(ctx, a.DB.Pool, apps.LogEntry{AppID: app.ID, Actor: audit.App(app.ID), Action: logVoucherRefused, Detail: detail}); lerr != nil {
		slog.Warn("vouchers: logging a failed verification", "app", app.ID, "err", lerr)
	}
	if identified && cb != nil {
		a.respondEvents(w, nil, err)
		return
	}
	httpx.WriteJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]string{"code": "callback_failed",
		"message": "callback verification failed", "reason": "callback_failed"}})
}

// verifyBudget takes one of the app's anonymous verifications this
// minute, reporting whether there was one left.
func (a *API) verifyBudget(ctx context.Context, app string) bool {
	var n int
	err := a.DB.Pool.QueryRow(ctx, `INSERT INTO subscribe_voucher_counters (app_id, verify_window, verifications) VALUES ($1, date_trunc('minute', now()), 1)
		ON CONFLICT (app_id) DO UPDATE SET verifications = CASE WHEN excluded.verify_window > subscribe_voucher_counters.verify_window
			THEN 1 ELSE subscribe_voucher_counters.verifications + 1 END,
			verify_window = greatest(subscribe_voucher_counters.verify_window, excluded.verify_window)
		RETURNING verifications`, app).Scan(&n)
	if err != nil {
		slog.Warn("vouchers: counting verifications", "app", app, "err", err)
		return false
	}
	return n <= voucherVerifyPerMinute
}

// hostSlots counts the anonymous verifications running per callback host
// (this instance's).
var hostSlots = struct {
	sync.Mutex
	n map[string]int
}{n: map[string]int{}}

// hostSlot takes one of callback's host's slots (voucherPerHost), if one
// is free; done gives it back.
func (a *API) hostSlot(callback string) (done func(), ok bool) {
	u, err := url.Parse(callback)
	if err != nil {
		return nil, false
	}
	host := strings.ToLower(u.Hostname())
	hostSlots.Lock()
	defer hostSlots.Unlock()
	if hostSlots.n[host] >= voucherPerHost {
		return nil, false
	}
	hostSlots.n[host]++
	return func() {
		hostSlots.Lock()
		defer hostSlots.Unlock()
		if hostSlots.n[host]--; hostSlots.n[host] <= 0 {
			delete(hostSlots.n, host)
		}
	}, true
}

// newUUID is a random (version 4) UUID.
func newUUID() string {
	b := make([]byte, 16)
	rand.Read(b)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// ── Unsubscribing (/events/unsubscribe) ───────────────────────────────────

// voucherUnsubscribe ends an anonymous voucher subscription, given its
// voucher and its callback URL; anything else ends nothing ({ended:
// false}): an unused voucher (neither claimed nor consumed), another
// URL, one redeemed by a verified user (who unsubscribes as that user).
// With the feature off, a voucher is refused.
func (a *API) voucherUnsubscribe(w http.ResponseWriter, r *http.Request, in eventsIn) {
	ctx := r.Context()
	app := AppFrom(ctx)
	if !a.SubscribeVouchers {
		a.refuseVoucher(ctx, w, app, in.Name, vrDisabled)
		return
	}
	nothing := map[string]any{"ended": false}
	if !voucherRE.MatchString(in.Voucher) {
		a.respondEventSub(w, nothing, nil)
		return
	}
	args, _ := json.Marshal(in.Arguments)
	key, err := apps.ArgumentsKey(args)
	if err != nil {
		a.respondEventSub(w, nil, err)
		return
	}
	v, err := a.lookupVoucher(ctx, app.ID, in.Voucher)
	if err != nil {
		a.respond(w, nil, err)
		return
	}
	if v == nil || v.UsedAt == nil || v.Tenant == nil || v.EventSub == nil || v.Event != in.Name ||
		subtle.ConstantTimeCompare([]byte(v.Key), []byte(key)) != 1 ||
		v.Session != nil && subtle.ConstantTimeCompare([]byte(*v.Session), []byte(transportSession(in.Call))) != 1 {
		a.respondEventSub(w, nothing, nil)
		return
	}
	ended := false
	err = a.DB.InTenant(ctx, *v.Tenant, func(tx pgx.Tx) error {
		list, err := queryEventSubs(ctx, tx, `es.id = $1::uuid AND es.source_kind = $2 AND es.source_id = $3 AND es.ended_at IS NULL
			AND es.destination = 'webhook' AND es.webhook_url = $4 AND starts_with(es.principal, '~') FOR UPDATE OF es`,
			*v.EventSub, sourceApp, app.ID, in.Delivery.URL)
		if err != nil || len(list) == 0 {
			return err
		}
		ended = true
		return endEventSubs(ctx, tx, list, evEndUnsubscribed, audit.App(app.ID), false)
	})
	if err == nil && ended {
		a.endedVoucherLiveUntil(ctx, *v.EventSub)
	}
	a.respondEventSub(w, map[string]any{"ended": ended}, err)
}

// ── Background, erasure and the emergency stop ─────────────────────────────

// PruneSubscribeVouchers deletes, at most limit of each at a time, unused
// vouchers an hour past their expiry, and used ones an hour after their
// subscription stopped being live (so a late refresh is refused as a
// voucher, not as tools-only). It returns how many it deleted.
func PruneSubscribeVouchers(ctx context.Context, d *db.DB, limit int) (int64, error) {
	unused, err := d.Pool.Exec(ctx, `DELETE FROM subscribe_vouchers WHERE token_hash IN (SELECT token_hash FROM subscribe_vouchers
		WHERE used_at IS NULL AND expires_at < now() - interval '1 hour' LIMIT $1)`, limit)
	if err != nil {
		return 0, err
	}
	used, err := d.Pool.Exec(ctx, `DELETE FROM subscribe_vouchers WHERE token_hash IN (SELECT token_hash FROM subscribe_vouchers
		WHERE used_at IS NOT NULL AND live_until < now() - interval '1 hour' LIMIT $1)`, limit)
	if err != nil {
		return unused.RowsAffected(), err
	}
	return unused.RowsAffected() + used.RowsAffected(), nil
}

// forgetVouchers deletes the vouchers handed out to one of the app's
// users (forgetUser): the unused ones, which no workspace holds; used
// ones go with the workspace their subscription is in.
func forgetVouchers(ctx context.Context, q interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}, app, user string) error {
	_, err := q.Exec(ctx, `DELETE FROM subscribe_vouchers WHERE app_id = $1 AND app_user = $2`, app, user)
	return err
}

// evEndStopped: Wakeshift stopped every voucher subscription (the admin
// action, EndVoucherSubscriptions).
const evEndStopped = "stopped"

// voucherStopped is what a webhook's receiver is told when the admin
// action ends its subscription.
const voucherStopped = "Wakeshift stopped this subscription"

// EndVoucherSubscriptions is the emergency stop: it ends every live
// subscription a voucher made, of one app (app) or of every app (""), with
// reason stopped, each recorded in its workspace's audit log
// (event_subscription.stopped, by by), and its receiver told. It returns
// how many it ended and in how many workspaces; one workspace that fails
// doesn't stop the rest (the first error is returned).
func (a *API) EndVoucherSubscriptions(ctx context.Context, app, by string) (ended, workspaces int, err error) {
	rows, _ := a.DB.Pool.Query(ctx, `SELECT DISTINCT tenant_id::text FROM subscribe_vouchers WHERE used_at IS NOT NULL AND tenant_id IS NOT NULL
		AND ($1 = '' OR app_id = $1) ORDER BY 1`, app)
	tenants, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return 0, 0, err
	}
	var failed error
	for _, t := range tenants {
		n := 0
		err := a.DB.InTenant(ctx, t, func(tx pgx.Tx) error {
			list, err := queryEventSubs(ctx, tx, `es.ended_at IS NULL AND es.id IN (SELECT event_subscription_id FROM subscribe_vouchers
				WHERE tenant_id = current_tenant() AND used_at IS NOT NULL AND ($1 = '' OR app_id = $1)) ORDER BY es.id FOR UPDATE OF es`, app)
			if err != nil || len(list) == 0 {
				return err
			}
			if err := endEventSubs(ctx, tx, list, evEndStopped, by, true); err != nil {
				return err
			}
			n = len(list)
			_, err = tx.Exec(ctx, `UPDATE subscribe_vouchers SET live_until = least(live_until, now()) WHERE tenant_id = current_tenant()
				AND used_at IS NOT NULL AND ($1 = '' OR app_id = $1)`, app)
			return err
		})
		if err != nil {
			slog.Warn("vouchers: ending a workspace's voucher subscriptions failed; going on with the rest", "err", err)
			if failed == nil {
				failed = err
			}
			continue
		}
		if n > 0 {
			ended += n
			workspaces++
		}
	}
	return ended, workspaces, failed
}
