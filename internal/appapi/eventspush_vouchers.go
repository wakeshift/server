package appapi

// Vouchers over push (eventspush.go).

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/apps"
	"github.com/wakeshift/server/internal/audit"
	"github.com/wakeshift/server/internal/sessions"
	"github.com/wakeshift/server/internal/streams"
)

// ── Vouchers over push ────────────────────────────────────────────────────

// vrNoSession: an anonymous voucher with no transport session, which push
// refuses (its only resume factor would be the cursor).
const vrNoSession = "no_session"

// openVoucherPush redeems a voucher over push (a null cursor: its one
// subscription), or resumes the subscription it made (its cursor), with
// the voucher's binding checked every time: its user, or its transport
// session. A voucher bound to neither is refused; a used voucher with a
// null cursor, or another subscription's cursor, is refused.
func (a *API) openVoucherPush(ctx context.Context, app apps.App, in pushIn, req pushReq, holder string) (*pushSub, error) {
	if !a.SubscribeVouchers {
		return nil, errBadVoucher{vrDisabled}
	}
	if !voucherRE.MatchString(req.voucher) {
		return nil, errBadVoucher{vrFormat}
	}
	v, err := a.lookupVoucher(ctx, app.ID, req.voucher)
	if err != nil {
		return nil, err
	}
	if v == nil {
		return nil, errBadVoucher{vrUnknown}
	}
	if v.AppUser == nil && v.Session == nil {
		return nil, errBadVoucher{vrNoSession}
	}
	if reason := voucherBinding(v, req.et, req.sub, in.Call); reason != "" {
		return nil, errBadVoucher{reason}
	}
	user := strings.TrimSpace(in.Call.User)
	if req.cursor == nil {
		if v.UsedAt != nil {
			return nil, errBadVoucher{vrUsed}
		}
		return a.redeemVoucherPush(ctx, app, in, req, v, holder)
	}
	// A resume: the voucher's own subscription, by its cursor.
	if v.UsedAt == nil || v.Tenant == nil || v.EventSub == nil || !strings.EqualFold(*v.EventSub, req.cursor.sub) {
		return nil, errBadVoucher{vrUsed}
	}
	var ends time.Time
	if v.TTL != nil {
		ends = v.UsedAt.Add(time.Duration(*v.TTL * float64(time.Second)))
	}
	sub, lapsed, err := a.resumePush(ctx, app, *v.Tenant, req, holder, pushResumer{voucher: true, user: user, ends: ends})
	if lapsed {
		// A voucher never makes a second subscription.
		a.endedVoucherLiveUntil(ctx, *v.EventSub)
		return nil, errBadVoucher{vrUsed}
	}
	return sub, err
}

// redeemVoucherPush makes the voucher's one push subscription, in one
// transaction: the voucher locked and checked (unused, unexpired), a new
// workspace for an anonymous redeemer (session-bound), the anonymous
// fan-out caps, the subscription (via tool:<its tool>, its once and ttl,
// and the replay since it was handed out), the voucher marked used, and
// the redemption audited. There's no callback to verify, so nothing is
// claimed across transactions.
func (a *API) redeemVoucherPush(ctx context.Context, app apps.App, in pushIn, req pushReq, v *voucherRow, holder string) (*pushSub, error) {
	user := strings.TrimSpace(in.Call.User)
	identified := appUser(user)
	call := in.Call
	call.Session, call.User = "", user
	var cv *conversation
	var err error
	if identified {
		if cv, err = a.findConversation(ctx, app, call, true); err != nil {
			return nil, err
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
	filters, err := topicFilters(req.sub.Topic, req.sub.Filter, v.Once)
	if err != nil {
		return nil, err
	}
	hash := voucherHash(req.voucher)
	var sub *pushSub
	err = a.DB.InTenant(ctx, cv.tenant, func(tx pgx.Tx) error {
		if !identified {
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
		var expired, used bool
		err := tx.QueryRow(ctx, `SELECT now() >= expires_at, used_at IS NOT NULL OR (claimed_until IS NOT NULL AND claimed_until > now())
			FROM subscribe_vouchers WHERE token_hash = $1 AND app_id = $2 FOR UPDATE`, hash, app.ID).Scan(&expired, &used)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return errBadVoucher{vrUnknown}
		case err != nil:
			return err
		case used:
			return errBadVoucher{vrUsed}
		case expired:
			return errBadVoucher{vrExpired}
		}
		if !identified {
			// Anonymous fan-out, as a webhook redemption counts it.
			if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('voucher-fanout:' || $1, 0))`, app.ID); err != nil {
				return err
			}
			var perKey, perApp int
			if err := tx.QueryRow(ctx, `SELECT count(*) FILTER (WHERE v.event = $2 AND v.arguments_key = $3), count(*)
				FROM subscribe_vouchers v JOIN tenants t ON t.id = v.tenant_id
				WHERE v.app_id = $1 AND v.used_at IS NOT NULL AND v.live_until > now() AND starts_with(t.external_id, '`+conversationPrefix+`')`,
				app.ID, req.sub.Event, req.sub.Key).Scan(&perKey, &perApp); err != nil {
				return err
			}
			if perKey >= voucherFanoutPerKey || perApp >= voucherFanoutPerApp {
				return errBadVoucher{vrFanout}
			}
		}
		locked, err := lockTopic(ctx, tx, app.ID, req.sub.Topic)
		if err != nil {
			return err
		}
		if err := lockStripe(ctx, tx, app.ID, req.sub.Topic); err != nil {
			return err
		}
		if err := ensureActor(ctx, tx, cv); err != nil {
			return err
		}
		if identified {
			if err := serialize(ctx, tx, "push:"+app.ID+"\x1f"+principal); err != nil {
				return err
			}
			var live int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM event_subscriptions es JOIN topic_subscriptions ts ON ts.tenant_id = es.tenant_id AND ts.subscription_id = es.subscription_id
				WHERE es.source_kind = $1 AND es.source_id = $2 AND es.principal = $3 AND es.destination = 'push' AND es.ended_at IS NULL
				AND ts.ended_at IS NULL AND ts.expires_at > now()`, sourceApp, app.ID, principal).Scan(&live); err != nil {
				return err
			}
			if live >= MaxPushSubscriptions {
				return errPushLimit{"subscriptions", MaxPushSubscriptions}
			}
		}
		if cv.session, err = newSession(ctx, tx, cv, ""); err != nil {
			return err
		}
		cv.handle = handleOf(cv.session)
		lease := PushLease
		var ends time.Time
		if v.TTL != nil {
			ttl := time.Duration(*v.TTL * float64(time.Second))
			lease = min(lease, ttl)
			// On the database's clock, as used_at (which a resume counts the
			// ttl from) is.
			if err := tx.QueryRow(ctx, `SELECT now() + make_interval(secs => $1)`, ttl.Seconds()).Scan(&ends); err != nil {
				return err
			}
		}
		routing, err := insertTopicSub(ctx, tx, cv, req.sub.Topic, filters, v.Once, max(lease, time.Second))
		if err != nil {
			return err
		}
		via := madeByToolPrefix + v.Tool
		id, err := recordEventSubAs(ctx, tx, app, req.sub, cv, principal, routing, toPush, via, "", "")
		if err != nil {
			return err
		}
		// The latest occurrence since the voucher was handed out (or within
		// its replay), as a webhook redemption replays.
		var since float64
		if err := tx.QueryRow(ctx, `SELECT extract(epoch FROM now() - $1::timestamptz)::float8`, v.IssuedAt).Scan(&since); err != nil {
			return err
		}
		window := time.Duration(max(since, 0) * float64(time.Second))
		if v.Replay != nil {
			window = max(window, time.Duration(*v.Replay*float64(time.Second)))
		}
		if err := a.replay(ctx, tx, app, req.sub.Topic, routing, filters, locked, min(window, maxReplay+VoucherTTL)); err != nil {
			return err
		}
		if err := sessions.SetWatching(ctx, tx, cv.session.ID, true); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE subscribe_vouchers SET used_at = now(), tenant_id = current_tenant(), event_subscription_id = $3::uuid,
			claim_id = NULL, claimed_until = NULL, live_until = (SELECT expires_at FROM topic_subscriptions WHERE subscription_id = $4)
			WHERE token_hash = $1 AND app_id = $2`, hash, app.ID, id, routing); err != nil {
			return err
		}
		bound := "session"
		if v.AppUser != nil {
			bound = "user"
		}
		if err := audit.Record(ctx, tx, audit.Entry{Actor: audit.App(app.ID), Action: audit.VoucherRedeemed, AgentID: cv.act.agent, AppID: app.ID,
			GrantID: cv.act.grant.ID, Detail: map[string]any{"id": id, "event": req.sub.Event, "arguments": json.RawMessage(req.sub.Arguments),
				"tool": v.Tool, "destination": toPush, "workspace": cv.tenant, "voucher": bound}}); err != nil {
			return err
		}
		sub = &pushSub{tenant: cv.tenant, id: id, agent: cv.act.agent, session: cv.session.ID, routing: routing, principal: principal,
			created: true, voucher: true, voucherEnds: ends, tool: v.Tool}
		if err := streams.TakePush(ctx, tx, sub.agent, sub.session, holder); err != nil {
			return err
		}
		// The replay's deliveries are owed: the stream starts before them.
		sub.from = 0
		return a.skipOld(ctx, tx, sub, req.maxAge)
	})
	return sub, err
}
