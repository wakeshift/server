package appapi

// Event subscription records (eventsubs.go has the calls that make them):
// what the console lists and revokes, and how one ends.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/apps"
	"github.com/wakeshift/server/internal/audit"
	"github.com/wakeshift/server/internal/mcpevents"
	"github.com/wakeshift/server/internal/sessions"
	"github.com/wakeshift/server/internal/streams"
)

// EventSubscription is one event subscription, as the console lists it.
type EventSubscription struct {
	ID          string          `json:"id"`
	SourceKind  string          `json:"source_kind"`
	SourceID    string          `json:"source_id"`
	SourceName  string          `json:"source_name"`
	Event       string          `json:"event"`
	Description string          `json:"description"`
	Arguments   json.RawMessage `json:"arguments"`
	Principal   string          `json:"principal,omitempty"`
	Destination string          `json:"destination"`
	AgentID     string          `json:"agent_id"`
	// WebhookHost is where a webhook delivers (the host only: a callback
	// URL can carry a token).
	WebhookHost string     `json:"webhook_host,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	RefreshedAt time.Time  `json:"refreshed_at"`
	EndedAt     *time.Time `json:"ended_at,omitempty"`
	EndedReason string     `json:"ended_reason,omitempty"`
	// Reason is why the app subscribed, as it said it (call.subscribe's
	// reason), if it did.
	Reason string `json:"reason,omitempty"`
	// subscriptionID is the topic subscription that routes it.
	subscriptionID string
	sessionID      string
	webhookID      string
}

const eventSubColumns = `es.id::text, es.source_kind, es.source_id, coalesce(a.name, es.source_id), es.event, es.arguments, es.principal,
	es.destination, es.agent_id, coalesce(es.webhook_url, ''), es.created_at, es.refreshed_at, es.ended_at, coalesce(es.ended_reason, ''),
	es.subscription_id::text, es.session_id::text, coalesce(es.webhook_id, ''), coalesce(a.manifest, '{}'::jsonb), coalesce(es.reason, '')
	FROM event_subscriptions es LEFT JOIN apps a ON es.source_kind = 'app' AND a.id = es.source_id`

// queryEventSubs reads the event subscriptions where (the SQL after WHERE,
// with any ORDER BY or FOR UPDATE) picks.
func queryEventSubs(ctx context.Context, tx pgx.Tx, where string, args ...any) ([]EventSubscription, error) {
	rows, err := tx.Query(ctx, `SELECT `+eventSubColumns+` WHERE `+where, args...)
	if err != nil {
		return nil, err
	}
	return scanEventSubs(rows)
}

func scanEventSubs(rows pgx.Rows) ([]EventSubscription, error) {
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (EventSubscription, error) {
		var s EventSubscription
		var args, hook string
		var manifest apps.Manifest
		err := r.Scan(&s.ID, &s.SourceKind, &s.SourceID, &s.SourceName, &s.Event, &args, &s.Principal, &s.Destination, &s.AgentID, &hook,
			&s.CreatedAt, &s.RefreshedAt, &s.EndedAt, &s.EndedReason, &s.subscriptionID, &s.sessionID, &s.webhookID, &manifest, &s.Reason)
		s.Arguments = json.RawMessage(args)
		if u, perr := url.Parse(hook); hook != "" && perr == nil {
			s.WebhookHost = u.Host
		}
		if et, err := manifest.EventType(s.Event); err == nil {
			s.Description = et.Description
		}
		return s, err
	})
}

// ListEventSubscriptions lists the transaction's workspace's event
// subscriptions, newest first: live ones (their topic subscription still
// routes them), then up to 50 that ended.
func ListEventSubscriptions(ctx context.Context, tx pgx.Tx) ([]EventSubscription, error) {
	if _, err := closeLapsed(ctx, tx); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `(SELECT `+eventSubColumns+` WHERE es.ended_at IS NULL ORDER BY es.created_at DESC)
		UNION ALL (SELECT `+eventSubColumns+` WHERE es.ended_at IS NOT NULL ORDER BY es.ended_at DESC LIMIT 50)`)
	if err != nil {
		return nil, err
	}
	return scanEventSubs(rows)
}

// closeLapsed marks ended the event subscriptions whose topic subscription
// ended without them (its ttl, its one delivery, its grant).
func closeLapsed(ctx context.Context, tx pgx.Tx) (int64, error) {
	tag, err := tx.Exec(ctx, `UPDATE event_subscriptions es SET ended_at = now(), ended_reason = $1
		WHERE es.ended_at IS NULL AND NOT EXISTS (SELECT 1 FROM topic_subscriptions ts JOIN subscriptions s ON s.tenant_id = ts.tenant_id AND s.id = ts.subscription_id
			WHERE ts.subscription_id = es.subscription_id AND ts.ended_at IS NULL AND s.status = 'active')`, evEndExpired)
	return tag.RowsAffected(), err
}

// FinishedOnce ends, in the pipeline's transaction (pipeline.Runner's
// Finished), the event subscriptions routed by a "once" subscription that
// just made its one delivery: the record (delivered, audited) and its
// voucher's live time, so neither is listed nor counted as live any more.
// A webhook isn't removed here, since the delivery it was just made is
// still to be sent: it's marked finished (mcpevents.Finish), so it no
// longer counts toward MaxWebhooks, and the deliverer removes it once
// nothing waits for it. The topic subscription ended already
// (subscriptions.Finish); its conversation keeps listening, to get the
// delivery.
//
// A record another transaction holds (ending or refreshing it) is skipped,
// never waited for: the pipeline holds the subscription's cursor, which
// those take after the record, so waiting could deadlock. One left live
// this way is ended (expired) by the next closeLapsed, as before.
func FinishedOnce(ctx context.Context, tx pgx.Tx, subscriptionID string) error {
	rows, err := tx.Query(ctx, `SELECT id::text, coalesce(webhook_id, ''), agent_id, source_id, event, arguments, destination
		FROM event_subscriptions WHERE subscription_id = $1::uuid AND ended_at IS NULL FOR UPDATE SKIP LOCKED`, subscriptionID)
	if err != nil {
		return err
	}
	type finished struct {
		ID, Webhook, Agent, App, Event, Arguments, Destination string
	}
	list, err := pgx.CollectRows(rows, pgx.RowToStructByPos[finished])
	if err != nil {
		return err
	}
	for _, s := range list {
		if s.Webhook != "" {
			if err := mcpevents.Finish(ctx, tx, s.Webhook); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE event_subscriptions SET ended_at = now(), ended_reason = $2, ended_by = $3 WHERE id = $1::uuid AND ended_at IS NULL`,
			s.ID, evEndDelivered, audit.Hub); err != nil {
			return err
		}
		if s.Destination == toPush {
			// Its stream writes the delivery, then ends (eventspush.go).
			if err := notifyPushEnded(ctx, tx, s.ID); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE subscribe_vouchers SET live_until = least(live_until, now())
			WHERE tenant_id = current_tenant() AND event_subscription_id = $1::uuid`, s.ID); err != nil {
			return err
		}
		if err := audit.Record(ctx, tx, audit.Entry{Actor: audit.Hub, Action: "event_subscription." + evEndDelivered, AgentID: s.Agent, AppID: s.App,
			Detail: map[string]any{"id": s.ID, "event": s.Event, "arguments": json.RawMessage(s.Arguments), "destination": s.Destination}}); err != nil {
			return err
		}
	}
	return nil
}

// CancelStranded cancels, at most limit at a time, what still waits for a
// webhook's session after its routing ended and its webhook stopped
// delivering: only that webhook delivers the session, so nothing ever
// would. endTopicSub cancels it as the routing ends; this clears what
// routing that ended before it did left waiting.
//
// Only subscriptions ended within StrandedRecent are checked, unless all
// is set: ended subscriptions are never deleted, and what waits for one
// whose routing still has a live webhook (or isn't a webhook's at all)
// waits on, so checking every one on every pass re-read the same rows
// forever, a cost that grew without bound. A webhook
// can outlive its routing by longer than that, so the expiry pass runs
// this with all set once a day (internal/core) to clear the rest.
//
// It starts from the workspace's waiting deliveries (the deliveries_queue
// index: only what is still owed) and looks up each one's routing by
// index, so its cost follows what waits, not how many event subscriptions
// or finished deliveries the workspace has. Rows a stream or another pass
// holds are skipped (SKIP LOCKED), never waited for, so this takes no lock
// it could deadlock on; the next pass gets them.
func CancelStranded(ctx context.Context, tx pgx.Tx, limit int, all bool) (int64, error) {
	sql := cancelStrandedRecentSQL
	if all {
		sql = cancelStrandedSQL
	}
	tag, err := tx.Exec(ctx, sql, limit)
	return tag.RowsAffected(), err
}

// StrandedRecent is how recently a subscription must have ended for every
// pass's CancelStranded to check it (2 days: the daily check covers the
// rest, with a day to spare).
const StrandedRecent = 48 * time.Hour

// cancelStrandedSQL cancels what waits for a webhook's own routing (a
// topic subscription, canceled) whose webhook no longer delivers, or for a
// poll subscription's routing that ended (nothing polls it any more).
// cancelStrandedRecentSQL is the same, for subscriptions ended within
// StrandedRecent only: two statements, not a parameter that turns the
// bound off, so a generic plan can't drop the bound.
var (
	cancelStrandedSQL       = cancelStrandedFor("")
	cancelStrandedRecentSQL = cancelStrandedFor(fmt.Sprintf(` AND s.cancelled_at > now() - interval '%d seconds'`, int(StrandedRecent.Seconds())))
)

func cancelStrandedFor(ended string) string {
	return `UPDATE deliveries SET state = 'cancelled', finished_at = now(), lease_until = NULL, lease_holder = NULL
	WHERE tenant_id = current_tenant() AND state = 'pending' AND id IN (SELECT d.id FROM deliveries d
		JOIN subscriptions s ON s.tenant_id = d.tenant_id AND s.id = d.subscription_id AND s.status <> 'active'` + ended + `
		WHERE d.tenant_id = current_tenant() AND d.state = 'pending' AND EXISTS (SELECT 1 FROM event_subscriptions es
			WHERE es.tenant_id = d.tenant_id AND es.subscription_id = d.subscription_id
			AND (es.destination IN ('poll', 'push') OR es.destination = 'webhook' AND ` + webhookGone + `))
		LIMIT $1 FOR UPDATE OF d SKIP LOCKED)`
}

// ErrNoEventSubscription: no such live event subscription here.
var ErrNoEventSubscription = errors.New("no such event subscription")

// RevokeEventSubscription ends a live event subscription in the
// transaction's workspace for the person by (audit.User): delivery stops at
// once, a conversation is told it ended, and the revocation is recorded.
func RevokeEventSubscription(ctx context.Context, tx pgx.Tx, id, by string) (EventSubscription, error) {
	if _, err := closeLapsed(ctx, tx); err != nil {
		return EventSubscription{}, err
	}
	list, err := queryEventSubs(ctx, tx, `es.id = $1::uuid AND es.ended_at IS NULL FOR UPDATE OF es`, id)
	if err != nil {
		return EventSubscription{}, err
	}
	if len(list) == 0 {
		return EventSubscription{}, ErrNoEventSubscription
	}
	s := list[0]
	// As the app's event.end does (endEventSubs): its routing locked
	// first, and no notice for a conversation that stopped listening.
	if err := endEventSubs(ctx, tx, list[:1], evEndRevoked, by, true); err != nil {
		return s, err
	}
	now := time.Now()
	s.EndedAt, s.EndedReason = &now, evEndRevoked
	return s, nil
}

// endEventSubs ends event subscriptions (endEventSub) in one transaction:
// their routing is locked first, all of it before the first ends
// (lockEnding), as every transaction that ends several does.
//
// notice says who ended them. With notice, the app or the user did (by its
// ID, or in the console): a conversation gets what was emitted to it
// before, then a notice saying why, and its listener ends after that if
// nothing else is live for it (StreamEnding, which checks once what was
// written is delivered); its links aren't revoked nor its watching
// stopped here, or what it's owed couldn't reach it (a revoked link
// closes at once; Codex's hook re-arms only a watching session). If it
// had stopped listening already (an app's unwatch, which revoked its
// links), nothing is owed it: what waits is dropped, with no notice.
// Without notice, the conversation itself did (call.unsubscribe, unsubscribe_events, a rollback): what still
// waits is dropped, since it asked to stop, and a session left with no live
// subscription stops listening at once (endListening).
func endEventSubs(ctx context.Context, tx pgx.Tx, list []EventSubscription, reason, by string, notice bool) error {
	ids := make([]string, 0, len(list))
	for _, s := range list {
		if s.subscriptionID != "" {
			ids = append(ids, s.subscriptionID)
		}
	}
	if err := lockEnding(ctx, tx, ids); err != nil {
		return err
	}
	for _, s := range list {
		told := notice
		if told && s.Destination == toConversation && s.sessionID != "" {
			// A conversation that stopped listening (an app's own unwatch,
			// which revokes its links, before it ends the subscription) asked
			// to stop: nothing more is owed it, and a notice would only wait
			// for its next subscribe, to arrive after it.
			if err := tx.QueryRow(ctx, `SELECT coalesce((SELECT watching FROM sessions WHERE id = $1::uuid), false)`, s.sessionID).Scan(&told); err != nil {
				return err
			}
		}
		if err := endEventSub(ctx, tx, s, reason, by, told); err != nil {
			return err
		}
	}
	if notice {
		return nil
	}
	return endListening(ctx, tx, list)
}

// endUnasked ends s for the app or a person (by), not its conversation or
// a webhook's receiver, which didn't ask for it: a conversation gets what
// was emitted to it first, then a notice (endEventSubs); a receiver, a
// terminated envelope, as on revocation.
func endUnasked(ctx context.Context, tx pgx.Tx, s EventSubscription, by string) error {
	reason := evEndUnsubscribed
	if s.Destination == toWebhook || s.Destination == toPoll || s.Destination == toPush {
		// A native client's: its receiver is told (terminated), its poll
		// loop refused (Forbidden).
		reason = evEndRevoked
	}
	return endEventSubs(ctx, tx, []EventSubscription{s}, reason, by, true)
}

// lockWatching serializes, for one session, a subscribe (which makes it
// watch) with an unsubscribe that may stop it watching (endListening), so
// the unsubscribe's check for anything still live sees a subscribe that
// committed first, and a subscribe after it sees the session stopped. It
// locks the session's row, which either may already hold (resolving
// touches it) and both update (SetWatching): an advisory lock taken after
// that row could be would order the two the other way round.
func lockWatching(ctx context.Context, tx pgx.Tx, session string) error {
	_, err := tx.Exec(ctx, `SELECT 1 FROM sessions WHERE id = $1::uuid FOR NO KEY UPDATE`, session)
	return err
}

// endListening stops the sessions of the conversations whose subscriptions
// in list ended, each only if it has no live subscription left (to any
// app): it no longer watches, and its stream links are revoked, so a
// listener on them (a Monitor, a background command, Codex's connector)
// closes at once (4001) with the session's reason (StreamEnding: "You
// unsubscribed from everything you watched in …"), and the next subscribe
// gives a fresh command instead of saying the old one still runs. A
// session with another live subscription keeps its stream.
func endListening(ctx context.Context, tx pgx.Tx, list []EventSubscription) error {
	seen := map[string]bool{}
	for _, s := range list {
		if s.Destination != toConversation || s.sessionID == "" || seen[s.sessionID] {
			continue
		}
		seen[s.sessionID] = true
		// Held already by an unsubscribe, which takes it before reading
		// anything.
		if err := lockWatching(ctx, tx, s.sessionID); err != nil {
			return err
		}
		var live bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM topic_subscriptions ts JOIN subscriptions su ON su.tenant_id = ts.tenant_id AND su.id = ts.subscription_id
			WHERE ts.tenant_id = current_tenant() AND ts.session_id = $1::uuid AND ts.ended_at IS NULL AND su.status = 'active')`,
			s.sessionID).Scan(&live); err != nil {
			return err
		}
		if live {
			continue
		}
		if _, err := stopListening(ctx, tx, s.AgentID, s.sessionID); err != nil {
			return err
		}
	}
	return nil
}

// stopListening stops a session listening: it no longer watches, and its
// stream links are revoked, so a listener on them closes at once (4001).
// revoked is how many links were.
func stopListening(ctx context.Context, tx pgx.Tx, agent, session string) (revoked int64, err error) {
	if err := sessions.SetWatching(ctx, tx, session, false); err != nil {
		return 0, err
	}
	return streams.RevokeSessionLinks(ctx, tx, agent, session)
}

// endEventSub ends an event subscription: its webhook, its topic
// subscription (telling a conversation, with notice: see endEventSubs),
// and the record, with an audit entry.
func endEventSub(ctx context.Context, tx pgx.Tx, s EventSubscription, reason, by string, notice bool) error {
	switch {
	case s.webhookID != "" && reason == evEndRevoked:
		// Revoked in the console: delivery stops now, and the deliverer
		// tells the receiver (a terminated envelope) and removes it.
		if err := mcpevents.Terminate(ctx, tx, s.webhookID, "Access revoked"); err != nil {
			return err
		}
	case s.webhookID != "" && reason == evEndStopped:
		// Wakeshift stopped voucher subscriptions (the admin action).
		if err := mcpevents.Terminate(ctx, tx, s.webhookID, voucherStopped); err != nil {
			return err
		}
	case s.webhookID != "" && reason == evEndPolicyChanged:
		// The app closed the event: its receiver is told the same way.
		if err := mcpevents.Terminate(ctx, tx, s.webhookID, policyTerminated); err != nil {
			return err
		}
	case s.webhookID != "":
		if _, err := mcpevents.Unsubscribe(ctx, tx, s.webhookID); err != nil {
			return err
		}
	}
	live, err := liveTopicSubs(ctx, tx, `ts.subscription_id = $1`, s.subscriptionID)
	if err != nil {
		return err
	}
	// Why, as a conversation is told (the notice, its stream's last line):
	// revoked by the user; ended by the conversation itself (an unsubscribe
	// or rollback without notice); else by the app.
	why := endRemoved
	switch {
	case reason == evEndRevoked:
		why = endRevoked
	case !notice && (reason == evEndUnsubscribed || reason == evEndRolledBack):
		why = endUnsubscribed
	}
	for _, ts := range live {
		// A webhook's own session has nothing to tell: the receiver hears
		// from a terminated envelope (on revocation) or its next refresh.
		if err := endTopicSub(ctx, tx, s.SourceID, ts, why, notice && s.Destination == toConversation); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE event_subscriptions SET ended_at = now(), ended_reason = $2, ended_by = $3 WHERE id = $1::uuid AND ended_at IS NULL`,
		s.ID, reason, by); err != nil {
		return err
	}
	if s.Destination == toPush {
		// The stream holding it ends at once (eventspush.go).
		if err := notifyPushEnded(ctx, tx, s.ID); err != nil {
			return err
		}
	}
	return audit.Record(ctx, tx, audit.Entry{Actor: by, Action: "event_subscription." + reason, AgentID: s.AgentID, AppID: s.SourceID,
		Detail: map[string]any{"id": s.ID, "event": s.Event, "arguments": s.Arguments, "destination": s.Destination}})
}
