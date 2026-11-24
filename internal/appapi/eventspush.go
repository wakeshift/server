package appapi

// events/stream: MCP Events' push delivery (SEP-3415). The app's MCP
// server (the SDKs' attach) holds the client's stream and
// opens one upstream here per client stream, relaying each line it gets as
// a notifications/events/* message tagged with the client's request ID:
//
//	POST /api/apps/v1/events/stream {call, name, arguments, cursor, max_age_ms, heartbeat_ms, voucher?}
//	  → 200 application/x-ndjson, one frame a line:
//	    {"type":"active","cursor","truncated","heartbeat_ms"}   first, and again after a gap
//	    {"type":"event","occurrence":{eventId, name, timestamp, data, cursor, _meta}}
//	    {"type":"heartbeat","cursor"}                            every heartbeat_ms, events or not
//	    {"type":"terminated","error":{"name","message","data"}}  the subscription ended; the stream ends
//	    {"type":"close"}                                         the stream ends; the client reconnects with its cursor
//	POST /api/apps/v1/events/stream/end {call, name, arguments, cursor} → {ended}
//
// A push subscription is a poll subscription read by a stream: an event
// subscription (destination 'push') with a session of its own whose queue
// only its stream reads, the consent record the console and
// list_subscriptions show and stop. Unlike poll's, a null cursor always
// makes a new one (two clients of one user watching the same thing don't
// share a queue), and a cursor names its subscription (pushcursor.go): a
// resume needs the subscription's own MAC'd cursor and the same principal,
// or for a voucher's subscription the voucher and its binding.
//
// The stream is built on the core's stream server (streams.Server): the
// instance's cap and drain, the notify hub's wakes, one holder per session
// with the newest winning (stream_leases), lease renewal, and confirmation
// by keepalive as every one-way core stream has it: a delivery written
// becomes delivered once a write at least a heartbeat interval later has
// gone through, and a lost connection hands back what it confirmed in the
// last minute. The client's cursor is the authority: one on open
// acknowledges what it covers and replays what's after it, delivered rows
// included, within the replay window.
//
// The flag WAKESHIFT_MCP_EVENTS_PUSH turns it on (API.Push); off, the
// route answers 501 unsupported. Never logged: cursors, arguments,
// vouchers.

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/apps"
	"github.com/wakeshift/server/internal/audit"
	"github.com/wakeshift/server/internal/deliveries"
	"github.com/wakeshift/server/internal/httpx"
	"github.com/wakeshift/server/internal/mcpevents"
	"github.com/wakeshift/server/internal/sessions"
	"github.com/wakeshift/server/internal/streams"
)

// FeaturePush is the feature the manifest response reports while push is
// served (manifestOut.Features).
const FeaturePush = "push"

// harnessOpencode is opencode's harness ID (internal/harnesses).
const harnessOpencode = "opencode"

// toPush is a push subscription's destination; madeByPush its via.
const (
	toPush     = "push"
	madeByPush = "push"
)

// Push limits. Variables where a test needs them small.
const (
	// PushLease is how long a push subscription lives with no stream
	// holding it.
	PushLease = 10 * time.Minute
	// pushRefreshAfter: a renewal moves the lease on only once this much
	// of it has passed.
	pushRefreshAfter = time.Minute
	// MaxPushSubscriptions is how many live push subscriptions an app's
	// user may have (counted apart from webhooks).
	MaxPushSubscriptions = 20
	// pushReplayWindow and pushReplayMax bound what a cursor replays of
	// what was delivered already: an hour, at most mcpevents.MaxReplay.
	pushReplayWindow = time.Hour
	// pushBatch is how many deliveries a stream claims at once.
	pushBatch = 50
	// pushHoldStaleFloor: a stream's hold stops counting toward the app's
	// streams once it hasn't renewed for 4 renewals, and never sooner than
	// this (pushHoldStale).
	pushHoldStaleFloor = 2 * time.Minute
	// pushNearCap: past this share of MaxPushStreams, opens log a warning.
	pushNearCap = 0.8
)

var (
	// PushHeartbeat is the heartbeat granted without a suggestion;
	// PushHeartbeatMin and PushHeartbeatMax bound a suggestion (30 s,
	// [10 s, 45 s]; tests shorten them).
	PushHeartbeat    = 30 * time.Second
	PushHeartbeatMin = 10 * time.Second
	PushHeartbeatMax = 45 * time.Second
	// MaxPushStreams is how many push streams an app may hold open at once,
	// across the core's instances (2,000; tests lower it).
	MaxPushStreams = 2000
)

type pushIn struct {
	Call        callIn         `json:"call"`
	Name        string         `json:"name"`
	Arguments   map[string]any `json:"arguments"`
	Cursor      *string        `json:"cursor"`
	MaxAgeMs    *int64         `json:"max_age_ms"`
	HeartbeatMs *int64         `json:"heartbeat_ms"`
	// Voucher is a tools-only event's subscribe voucher (vouchers.go).
	// Never logged.
	Voucher string `json:"voucher"`
}

// errPushLimit is a cap reached at open: limit is "subscriptions" or
// "streams" (403 subscription_limit, with limit and max).
type errPushLimit struct {
	limit string
	max   int
}

func (e errPushLimit) Error() string {
	if e.limit == "streams" {
		return fmt.Sprintf("this app holds %d push streams already, the most allowed: try again later", e.max)
	}
	return fmt.Sprintf("this user already has %d live push subscriptions to this app, the most allowed: let one lapse, or end one, first", e.max)
}

// errPushUnsupported: push isn't served (the flag is off).
var errPushUnsupported = errors.New("push delivery isn't served by this Wakeshift: use a webhook or poll")

// clampHeartbeat is the heartbeat granted for a client's suggestion.
func clampHeartbeat(ms *int64) time.Duration {
	if ms == nil || *ms <= 0 {
		return PushHeartbeat
	}
	d := time.Duration(min(*ms, int64(time.Hour/time.Millisecond))) * time.Millisecond
	return min(max(d, PushHeartbeatMin), PushHeartbeatMax)
}

// pushSub is a push subscription as a stream opens it.
type pushSub struct {
	tenant, id, agent, session, routing, principal string
	// from is the position the stream starts after; truncated, that what
	// came before it wasn't all kept.
	from      int64
	truncated bool
	created   bool
	// voucherEnds is when a voucher's ttl ends the subscription (zero:
	// none).
	voucherEnds time.Time
	voucher     bool
	// tool is a voucher redemption's tool (the first active says via
	// tool:<tool>, for the SDK's onSubscribe).
	tool string
}

func (a *API) eventsStream(w http.ResponseWriter, r *http.Request) {
	if !a.Push || a.Streams == nil {
		writeUnsupportedPush(w)
		return
	}
	var in pushIn
	if !httpx.Decode(w, r, &in) {
		return
	}
	ctx := r.Context()
	app := AppFrom(ctx)
	req, err := a.checkPush(app, in)
	if err != nil {
		a.respondPush(ctx, w, app, in.Name, err)
		return
	}
	// Counted in before any database work, so a full instance refuses
	// without any.
	// Push's shares of the instance (streams.PushShare, PushAppShare) keep
	// one app, and push as a whole, from taking every stream slot.
	if err := a.Streams.AdmitPush(app.ID); err != nil {
		streams.TurnAway(w, err)
		return
	}
	defer a.Streams.LeavePush(app.ID)
	holder := streams.NewPushHolder()
	if err := a.holdPushStream(ctx, app.ID, holder); err != nil {
		a.respondPush(ctx, w, app, in.Name, err)
		return
	}
	defer a.dropPushHold(ctx, holder)
	sub, err := a.openPush(ctx, app, in, req, holder)
	if err != nil {
		a.respondPush(ctx, w, app, in.Name, err)
		return
	}
	h, _, _, _, _ := harnessOf(in.Call)
	st := &pushStream{a: a, app: app, sub: sub, name: in.Name, holder: holder, grant: req.heartbeat, opencode: h == harnessOpencode, w: w,
		rc: http.NewResponseController(w)}
	st.run(ctx)
}

// pushReq is a stream request's checked parts.
type pushReq struct {
	sub       apps.Subscription
	et        apps.EventType
	cursor    *pushCursor
	heartbeat time.Duration
	maxAge    time.Duration
	voucher   string
}

// checkPush checks a request without touching the database: the event
// and arguments, tools-only without a voucher, the cursor's form, a user.
func (a *API) checkPush(app apps.App, in pushIn) (pushReq, error) {
	var req pushReq
	args, _ := json.Marshal(in.Arguments)
	user := strings.TrimSpace(in.Call.User)
	var err error
	if req.et, err = app.Manifest.EventType(in.Name); err != nil {
		return req, err
	}
	if req.sub, err = eventTarget(app, in.Name, args, user); err != nil {
		return req, err
	}
	if req.et.ToolsOnly() {
		// Only with a voucher; without, refused as poll refuses.
		if in.Voucher == "" {
			return req, refuseToolsOnly(app, in.Name, madeByPush)
		}
		req.voucher = in.Voucher
	}
	if in.Cursor != nil {
		c, err := parsePushCursor(*in.Cursor)
		if err != nil {
			return req, err
		}
		req.cursor = &c
	}
	if req.voucher == "" && user == "" {
		return req, badRequest{errors.New("call.user is required: a push subscription belongs to the app's user")}
	}
	if user != "" && !appUser(user) {
		return req, badRequest{errors.New("call.user is at most 200 characters, not starting with ~")}
	}
	req.heartbeat = clampHeartbeat(in.HeartbeatMs)
	if in.MaxAgeMs != nil {
		req.maxAge = mcpevents.MaxAgeOf(*in.MaxAgeMs)
	}
	return req, nil
}

// holdPushStream counts a stream toward its app's MaxPushStreams, across
// instances (push_stream_holds), refusing it past the cap.
func (a *API) holdPushStream(ctx context.Context, app, holder string) error {
	return pgx.BeginFunc(ctx, a.DB.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('push-streams:' || $1, 0))`, app); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM push_stream_holds WHERE app_id = $1 AND renewed_at < now() - make_interval(secs => $2)`,
			app, a.pushHoldStale().Seconds()); err != nil {
			return err
		}
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM push_stream_holds WHERE app_id = $1`, app).Scan(&n); err != nil {
			return err
		}
		if n >= MaxPushStreams {
			slog.Warn("mcp events: an app is at its push stream cap", "app", app, "streams", n, "max", MaxPushStreams)
			return errPushLimit{"streams", MaxPushStreams}
		}
		if float64(n+1) >= pushNearCap*float64(MaxPushStreams) && (n+1)%max(MaxPushStreams/100, 1) == 0 {
			// Every 1% from 80% up: the log-based alert counts these.
			slog.Warn("mcp events: an app's push streams near its cap", "app", app, "streams", n+1, "max", MaxPushStreams)
		}
		_, err := tx.Exec(ctx, `INSERT INTO push_stream_holds (holder, app_id) VALUES ($1, $2)`, holder, app)
		return err
	})
}

// pushHoldStale is how long a stream's hold counts without a renewal: 4
// renewals, at least pushHoldStaleFloor.
func (a *API) pushHoldStale() time.Duration {
	return max(4*a.Streams.RenewEvery(), pushHoldStaleFloor)
}

// dropPushHold stops a stream counting toward its app's streams.
func (a *API) dropPushHold(ctx context.Context, holder string) {
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if _, err := a.DB.Pool.Exec(fctx, `DELETE FROM push_stream_holds WHERE holder = $1`, holder); err != nil {
		slog.Warn("mcp events: dropping a push stream's hold failed (it stops counting four renewals on)", "err", err)
	}
}

// openPush finds or makes the request's subscription and takes its stream
// (holder): a user's (by call.user) or a voucher's.
func (a *API) openPush(ctx context.Context, app apps.App, in pushIn, req pushReq, holder string) (*pushSub, error) {
	if req.voucher != "" {
		return a.openVoucherPush(ctx, app, in, req, holder)
	}
	call := in.Call
	call.Session = ""
	cv, err := a.findConversation(ctx, app, call, false)
	if errors.Is(err, errNoConversation) {
		if req.cursor != nil {
			// A cursor from a workspace that's gone (its user forgotten).
			return nil, errBadPushCursor
		}
		cv, err = a.findConversation(ctx, app, call, true)
	}
	if err != nil {
		return nil, err
	}
	truncated := false
	if req.cursor != nil {
		sub, lapsed, err := a.resumePush(ctx, app, cv.tenant, req, holder, pushResumer{principal: cv.call.User, agent: cv.act.agent})
		if err != nil || !lapsed {
			return sub, err
		}
		// It lapsed: a new subscription, and what was emitted meanwhile went
		// nowhere.
		truncated = true
	}
	sub, err := a.createPush(ctx, app, cv, req, holder)
	if sub != nil {
		sub.truncated = sub.truncated || truncated
	}
	return sub, err
}

// pushRecord is a push subscription's record as a resume reads it.
type pushRecord struct {
	ID, Principal, Agent, Event, Key, Destination, Via, Session, Routing, Reason string
	Ended, Live                                                                  bool
}

// errPushEnded: the push subscription the cursor came from was ended by the
// person or the app (403 subscription_stopped, on every retry).
var errPushEnded = errors.New("this subscription was stopped: subscribe again (a null cursor) only if you're asked to")

// pushResumer is who may resume a push subscription; every resume
// names one of the two kinds:
//   - a user's (voucher false): the subscription's own principal and agent,
//     made through push (not a voucher's), the event still open (holdPolicy);
//   - a voucher's (voucher true; the voucher and its binding were checked by
//     the caller, openVoucherPush): a subscription a voucher made, resumed
//     only by its verified user if one redeemed it (user), within its ttl
//     (ends).
type pushResumer struct {
	voucher          bool
	principal, agent string    // a user's
	user             string    // a voucher's caller
	ends             time.Time // a voucher's ttl (zero: none)
}

// authorizeResume checks r against the subscription's record, in the
// resume's transaction.
func (a *API) authorizeResume(ctx context.Context, tx pgx.Tx, app apps.App, event string, r pushResumer, es pushRecord) error {
	fromVoucher := strings.HasPrefix(es.Via, madeByToolPrefix)
	if !r.voucher {
		if fromVoucher || r.principal == "" || es.Principal != r.principal || es.Agent != r.agent {
			return errBadPushCursor
		}
		return holdPolicy(ctx, tx, app.ID, event, madeByPush)
	}
	if !fromVoucher {
		return errBadPushCursor
	}
	if !strings.HasPrefix(es.Principal, "~") && subtle.ConstantTimeCompare([]byte(es.Principal), []byte(r.user)) != 1 {
		return errBadVoucher{vrUser}
	}
	if !r.ends.IsZero() && !a.now().Before(r.ends) {
		return errBadVoucher{vrEnded}
	}
	return nil
}

// resumePush resumes the subscription req's cursor names in tenant: its
// MAC, its record (authorized for r, in the transaction), the stream
// taken, the cursor acknowledged and what's after it replayed. lapsed: its
// lease ran out (or its routing ended without the person or app ending
// it), so nothing was resumed.
func (a *API) resumePush(ctx context.Context, app apps.App, tenant string, req pushReq, holder string, r pushResumer) (sub *pushSub, lapsed bool, err error) {
	c := *req.cursor
	if !a.validPushCursor(tenant, c) {
		return nil, false, errBadPushCursor
	}
	err = a.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		var es pushRecord
		err := tx.QueryRow(ctx, `SELECT es.id::text, es.principal, es.agent_id, es.event, es.arguments_key, es.destination, coalesce(es.via, ''),
			es.session_id::text, es.subscription_id::text, coalesce(es.ended_reason, ''), es.ended_at IS NOT NULL,
			EXISTS (SELECT 1 FROM topic_subscriptions ts JOIN subscriptions s ON s.tenant_id = ts.tenant_id AND s.id = ts.subscription_id
				WHERE ts.subscription_id = es.subscription_id AND ts.ended_at IS NULL AND ts.expires_at > now() AND s.status = 'active')
			FROM event_subscriptions es WHERE es.id = $1::uuid AND es.source_kind = $2 AND es.source_id = $3 FOR UPDATE OF es`,
			c.sub, sourceApp, app.ID).Scan(&es.ID, &es.Principal, &es.Agent, &es.Event, &es.Key, &es.Destination, &es.Via,
			&es.Session, &es.Routing, &es.Reason, &es.Ended, &es.Live)
		if errors.Is(err, pgx.ErrNoRows) {
			return errBadPushCursor
		}
		if err != nil {
			return err
		}
		if es.Destination != toPush || es.Event != req.sub.Event || subtle.ConstantTimeCompare([]byte(es.Key), []byte(req.sub.Key)) != 1 {
			return errBadPushCursor
		}
		if err := a.authorizeResume(ctx, tx, app, req.sub.Event, r, es); err != nil {
			return err
		}
		switch {
		case es.Ended && es.Reason != evEndExpired && es.Reason != evEndRolledBack:
			return errPushEnded
		case es.Ended || !es.Live:
			lapsed = true
			if !es.Ended {
				_, err := tx.Exec(ctx, `UPDATE event_subscriptions SET ended_at = now(), ended_reason = $2 WHERE id = $1::uuid AND ended_at IS NULL`, es.ID, evEndExpired)
				if err != nil {
					return err
				}
				return audit.Record(ctx, tx, audit.Entry{Actor: audit.Hub, Action: "event_subscription." + evEndExpired, AgentID: es.Agent, AppID: app.ID,
					Detail: map[string]any{"id": es.ID, "event": es.Event, "destination": toPush}})
			}
			return nil
		}
		sub = &pushSub{tenant: tenant, id: es.ID, agent: es.Agent, session: es.Session, routing: es.Routing, principal: es.Principal,
			voucher: strings.HasPrefix(es.Via, madeByToolPrefix), voucherEnds: r.ends}
		// The newest stream wins: the older is told to close, and what
		// it hadn't confirmed goes back, before anything is read.
		if err := streams.TakePush(ctx, tx, es.Agent, es.Session, holder); err != nil {
			return err
		}
		if err := renewPushLease(ctx, tx, sub, true); err != nil {
			return err
		}
		return a.positionPush(ctx, tx, sub, &c.seq, req.maxAge)
	})
	return sub, lapsed, err
}

// positionPush sets where a stream starts (sub.from) and what it sends
// first. With a cursor (from), what the cursor covers is acknowledged (the
// client has it), and what's after it that was delivered already goes
// again, within the replay window (pushReplayWindow, mcpevents.MaxReplay):
// past it, or for what can't be sent any more, truncated. What waits is
// always sent, except what maxAge skips (truncated too).
func (a *API) positionPush(ctx context.Context, tx pgx.Tx, sub *pushSub, from *int64, maxAge time.Duration) error {
	var head int64
	if err := tx.QueryRow(ctx, `SELECT coalesce(max(event_seq), 0) FROM deliveries WHERE agent_id = $1 AND target = $2`,
		sub.agent, sub.session).Scan(&head); err != nil {
		return err
	}
	if from == nil {
		sub.from = head
		return a.skipOld(ctx, tx, sub, maxAge)
	}
	through := *from
	if through > head {
		// Past anything the subscription has had: not one it gave out.
		return errBadPushCursor
	}
	sub.from = through
	rows, _ := tx.Query(ctx, `SELECT id::text FROM deliveries WHERE agent_id = $1 AND target = $2 AND state IN ('pending', 'in_flight')
		AND event_seq <= $3 ORDER BY event_seq FOR UPDATE SKIP LOCKED`, sub.agent, sub.session, through)
	covered, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	for _, id := range covered {
		if _, err := deliveries.Ack(ctx, tx, sub.agent, sub.session, id); err != nil {
			return err
		}
	}
	// What was delivered after the cursor: the newest MaxReplay within the
	// window go again; the rest (and any whose content was deleted once
	// delivered) are a gap.
	var total int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM deliveries WHERE agent_id = $1 AND target = $2 AND state IN ('delivered', 'acked')
		AND event_seq > $3`, sub.agent, sub.session, through).Scan(&total); err != nil {
		return err
	}
	if total > 0 {
		rows, _ := tx.Query(ctx, `SELECT d.id::text, d.event_seq FROM deliveries d JOIN events e ON e.tenant_id = d.tenant_id AND e.id = d.event_id
			WHERE d.agent_id = $1 AND d.target = $2 AND d.state IN ('delivered', 'acked') AND d.event_seq > $3
			AND d.created_at > now() - make_interval(secs => $4) AND e.redacted_at IS NULL
			ORDER BY d.event_seq DESC LIMIT $5 FOR UPDATE OF d`, sub.agent, sub.session, through, pushReplayWindow.Seconds(), mcpevents.MaxReplay)
		type kept struct {
			ID  string
			Seq int64
		}
		list, err := pgx.CollectRows(rows, pgx.RowToStructByPos[kept])
		if err != nil {
			return err
		}
		if len(list) < total {
			sub.truncated = true
			// Resume at the oldest kept: the cursor moves past what's lost.
			if len(list) > 0 {
				sub.from = max(through, list[len(list)-1].Seq-1)
			} else {
				var last int64
				if err := tx.QueryRow(ctx, `SELECT coalesce(max(event_seq), $3) FROM deliveries WHERE agent_id = $1 AND target = $2
					AND state IN ('delivered', 'acked') AND event_seq > $3`, sub.agent, sub.session, through).Scan(&last); err != nil {
					return err
				}
				sub.from = last
			}
		}
		ids := make([]string, len(list))
		for i, k := range list {
			ids[i] = k.ID
		}
		if _, err := tx.Exec(ctx, `UPDATE deliveries SET state = 'pending', finished_at = NULL, lease_until = NULL
			WHERE id = ANY($1::uuid[])`, ids); err != nil {
			return err
		}
	}
	return a.skipOld(ctx, tx, sub, maxAge)
}

// skipOld finishes what waits for the stream from before maxAge (a gap:
// truncated).
func (a *API) skipOld(ctx context.Context, tx pgx.Tx, sub *pushSub, maxAge time.Duration) error {
	if maxAge <= 0 {
		return nil
	}
	tag, err := tx.Exec(ctx, `UPDATE deliveries d SET state = 'dead', finished_at = now(), lease_until = NULL, lease_holder = NULL,
		last_error = 'skipped by a push stream (too old for maxAgeMs)'
		FROM events e WHERE e.tenant_id = d.tenant_id AND e.id = d.event_id AND d.agent_id = $1 AND d.target = $2 AND d.state = 'pending'
		AND e.occurred_at < $3`, sub.agent, sub.session, a.now().Add(-maxAge))
	if err == nil && tag.RowsAffected() > 0 {
		sub.truncated = true
	}
	return err
}

// renewPushLease moves a push subscription's routing lease on (PushLease
// from now, within a voucher's ttl), at most once a minute unless force,
// with its record's refreshed_at and its voucher's live time.
func renewPushLease(ctx context.Context, tx pgx.Tx, sub *pushSub, force bool) error {
	after := pushRefreshAfter
	if force {
		after = 0
	}
	var ends *time.Time
	if !sub.voucherEnds.IsZero() {
		ends = &sub.voucherEnds
	}
	tag, err := tx.Exec(ctx, `UPDATE topic_subscriptions SET expires_at = least(now() + make_interval(secs => $2::int), coalesce($4::timestamptz, 'infinity'))
		WHERE subscription_id = $1 AND ended_at IS NULL AND expires_at < now() + make_interval(secs => $3::int)`,
		sub.routing, int(PushLease.Seconds()), int((PushLease - after).Seconds()), ends)
	if err != nil || tag.RowsAffected() == 0 {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE event_subscriptions SET refreshed_at = now() WHERE id = $1::uuid`, sub.id); err != nil {
		return err
	}
	if sub.voucher {
		_, err = tx.Exec(ctx, `UPDATE subscribe_vouchers SET live_until = (SELECT expires_at FROM topic_subscriptions WHERE subscription_id = $2)
			WHERE tenant_id = current_tenant() AND event_subscription_id = $1::uuid`, sub.id, sub.routing)
	}
	return err
}

// createPush makes a new push subscription for cv's user (null cursor, or
// one whose subscription lapsed) and takes its stream: a session of its
// own, routing with lease PushLease, the record (audited in the same
// transaction).
func (a *API) createPush(ctx context.Context, app apps.App, cv *conversation, req pushReq, holder string) (*pushSub, error) {
	filters, err := topicFilters(req.sub.Topic, req.sub.Filter, false)
	if err != nil {
		return nil, err
	}
	var sub *pushSub
	err = a.DB.InTenant(ctx, cv.tenant, func(tx pgx.Tx) error {
		if err := holdPolicy(ctx, tx, app.ID, req.sub.Event, madeByPush); err != nil {
			return err
		}
		if _, err := lockTopic(ctx, tx, app.ID, req.sub.Topic); err != nil {
			return err
		}
		if err := lockStripe(ctx, tx, app.ID, req.sub.Topic); err != nil {
			return err
		}
		if err := ensureActor(ctx, tx, cv); err != nil {
			return err
		}
		if err := serialize(ctx, tx, "push:"+app.ID+"\x1f"+cv.call.User); err != nil {
			return err
		}
		if _, err := closeLapsed(ctx, tx); err != nil {
			return err
		}
		var live int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM event_subscriptions es JOIN topic_subscriptions ts ON ts.tenant_id = es.tenant_id AND ts.subscription_id = es.subscription_id
			WHERE es.source_kind = $1 AND es.source_id = $2 AND es.principal = $3 AND es.destination = 'push' AND es.ended_at IS NULL
			AND ts.ended_at IS NULL AND ts.expires_at > now()`, sourceApp, app.ID, cv.call.User).Scan(&live); err != nil {
			return err
		}
		if live >= MaxPushSubscriptions {
			return errPushLimit{"subscriptions", MaxPushSubscriptions}
		}
		var err error
		if cv.session, err = newSession(ctx, tx, cv, ""); err != nil {
			return err
		}
		cv.handle = handleOf(cv.session)
		routing, err := insertTopicSub(ctx, tx, cv, req.sub.Topic, filters, false, PushLease)
		if err != nil {
			return err
		}
		id, err := recordEventSub(ctx, tx, app, req.sub, cv, routing, toPush, madeByPush, "", "")
		if err != nil {
			return err
		}
		if err := sessions.SetWatching(ctx, tx, cv.session.ID, true); err != nil {
			return err
		}
		sub = &pushSub{tenant: cv.tenant, id: id, agent: cv.act.agent, session: cv.session.ID, routing: routing, principal: cv.call.User, created: true}
		if err := streams.TakePush(ctx, tx, sub.agent, sub.session, holder); err != nil {
			return err
		}
		// From now: nothing is owed a new subscription yet.
		return a.positionPush(ctx, tx, sub, nil, req.maxAge)
	})
	return sub, err
}

// ── Ending (stdio's notifications/cancelled) ──────────────────────────────

// eventsStreamEnd ends the push subscription a cursor names, when the
// request's app, principal, event and arguments are the subscription's:
// the client cancelled its stream (stdio's notifications/cancelled), so
// it ends at once rather than lapsing (unsubscribed, audited). Anything
// else ends nothing ({ended: false}); a cursor that doesn't verify is a
// bad request.
func (a *API) eventsStreamEnd(w http.ResponseWriter, r *http.Request) {
	if !a.Push {
		writeUnsupportedPush(w)
		return
	}
	var in struct {
		Call      callIn         `json:"call"`
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
		Cursor    string         `json:"cursor"`
	}
	if !httpx.Decode(w, r, &in) {
		return
	}
	ctx := r.Context()
	app := AppFrom(ctx)
	c, err := parsePushCursor(in.Cursor)
	if err != nil {
		a.respondEventSub(w, nil, err)
		return
	}
	args, _ := json.Marshal(in.Arguments)
	key, err := apps.ArgumentsKey(args)
	if err != nil {
		a.respondEventSub(w, nil, err)
		return
	}
	user := strings.TrimSpace(in.Call.User)
	// The workspace: the user's, or, for an anonymous voucher's
	// subscription, the one its voucher names.
	var tenant, agent string
	if user != "" {
		call := in.Call
		call.Session = ""
		cv, err := a.findConversation(ctx, app, call, false)
		if errors.Is(err, errNoConversation) {
			a.respond(w, map[string]any{"ended": false}, nil)
			return
		}
		if err != nil {
			a.respondEventSub(w, nil, err)
			return
		}
		tenant, agent = cv.tenant, cv.act.agent
	} else {
		_, agent, _, _, _ = harnessOf(in.Call)
		// An anonymous voucher's subscription: ended only from the transport
		// session its voucher is bound to, as only that session resumes it;
		// the cursor alone ends nothing.
		var bound string
		err := a.DB.Pool.QueryRow(ctx, `SELECT tenant_id::text, coalesce(transport_session, '') FROM subscribe_vouchers
			WHERE app_id = $1 AND event_subscription_id = $2::uuid AND app_user IS NULL`, app.ID, c.sub).Scan(&tenant, &bound)
		if errors.Is(err, pgx.ErrNoRows) || err == nil && (bound == "" || subtle.ConstantTimeCompare([]byte(bound), []byte(transportSession(in.Call))) != 1) {
			a.respond(w, map[string]any{"ended": false}, nil)
			return
		}
		if err != nil {
			a.respond(w, nil, err)
			return
		}
	}
	if !a.validPushCursor(tenant, c) {
		a.respondEventSub(w, nil, errBadPushCursor)
		return
	}
	ended := false
	err = a.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		list, err := queryEventSubs(ctx, tx, `es.id = $1::uuid AND es.source_kind = $2 AND es.source_id = $3 AND es.destination = 'push'
			AND es.event = $4 AND es.arguments_key = $5 AND es.ended_at IS NULL FOR UPDATE OF es`, c.sub, sourceApp, app.ID, in.Name, key)
		if err != nil || len(list) == 0 {
			return err
		}
		// As a resume checks (authorizeResume): a user's own subscription by
		// that user, an anonymous one only without a user (its session
		// checked above), and the same agent either way.
		p := list[0].Principal
		if user != "" && subtle.ConstantTimeCompare([]byte(p), []byte(user)) != 1 || user == "" && !strings.HasPrefix(p, "~") ||
			list[0].AgentID != agent {
			return nil
		}
		ended = true
		return endEventSubs(ctx, tx, list, evEndUnsubscribed, audit.App(app.ID), false)
	})
	a.respondEventSub(w, map[string]any{"ended": ended}, err)
}

// nativePush reports whether a bridged subscribe's caller gets the native
// push line instead: opencode, which declared the MCP Events
// extension, while push is served and Wakeshift's opencode plugin doesn't
// listen for the conversation (hooksListening: it connected the session
// and hasn't lost its listener). The plugin listening wins, so one event
// doesn't wake the session twice. The SDK's native verdict stands for
// tools/list and events/list; this can only keep a call bridged.
func (a *API) nativePush(ctx context.Context, tx pgx.Tx, cv *conversation) (bool, error) {
	if !a.Push || !cv.call.EventsExt || cv.agentHandles || cv.harness != harnessOpencode {
		return false, nil
	}
	s, err := sessions.Get(ctx, tx, cv.session.ID)
	if err != nil {
		return false, err
	}
	listening, err := hooksListening(ctx, tx, s)
	return !listening, err
}

// notifyPushEnded tells the stream holding a push subscription that it
// ended (on commit), so it ends at once.
func notifyPushEnded(ctx context.Context, tx pgx.Tx, id string) error {
	_, err := tx.Exec(ctx, `SELECT pg_notify($1, $2)`, streams.PushEndedChannel, id)
	return err
}

// ── Answers ───────────────────────────────────────────────────────────────

// writeUnsupportedPush answers a stream request while push isn't served.
func writeUnsupportedPush(w http.ResponseWriter) {
	httpx.WriteJSON(w, http.StatusNotImplemented, map[string]any{"error": map[string]string{"code": "unsupported",
		"message": errPushUnsupported.Error(), "feature": "deliveryMode", "value": "push"}})
}

// respondPush answers a stream that didn't open: poll's errors, a refused
// voucher (bad_voucher, the reason to the app's log only), a stopped
// subscription, a cap.
func (a *API) respondPush(ctx context.Context, w http.ResponseWriter, app apps.App, event string, err error) {
	var bad errBadVoucher
	var limit errPushLimit
	switch {
	case errors.As(err, &bad):
		a.refuseVoucher(ctx, w, app, event, bad.reason)
	case errors.As(err, &limit):
		httpx.WriteJSON(w, http.StatusForbidden, map[string]any{"error": map[string]any{"code": "subscription_limit", "message": limit.Error(),
			"limit": limit.limit, "max": limit.max}})
	case errors.Is(err, errPushEnded):
		httpx.WriteError(w, http.StatusForbidden, "subscription_stopped", err.Error())
	default:
		a.respondEventSub(w, nil, err)
	}
}
