package appapi

// Who may subscribe to an app's event. An event the manifest marks
// subscribe "tools" (apps.EventType.ToolsOnly) is subscribed to only
// through the app's own tools: subscribe_events (the SDKs' built-in tool),
// events/subscribe and events/poll are refused for it, every request,
// creation or not, whatever cursor it carries. Unsubscribing and listing
// stay open on every path.
//
// Every subscription records how it was made (event_subscriptions.via):
//
//	tool:<name>       POST /event-subscriptions from the app's tool <name> (call.tool)
//	subscribe_events  POST /event-subscriptions from the SDKs' built-in subscribe_events
//	native            POST /events/subscribe (a webhook)
//	poll              POST /events/poll
//
// A conversation's subscription from a call that names no tool (an SDK
// from before call.tool, or the API called directly) records none, as rows
// from before the column have none.
//
// When a manifest closes an event (any → tools), the transaction that
// stores it ends the subscriptions closing makes illegitimate, in every
// workspace the app keeps (closeEvents): those made through
// subscribe_events, natively or by polling, and rows without via whose
// destination is native (a webhook or a poll). A conversation's without
// via is kept: it can't be told from one the app's tool made, and ending
// it would break those on upgrade (the design's decision 4). Each
// workspace's audit log records the change, with how many it ended and
// kept. Opening an event (tools → any) is recorded the same way.

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/apps"
	"github.com/wakeshift/server/internal/audit"
)

// How an event subscription was made (event_subscriptions.via).
const (
	madeByToolPrefix      = "tool:"
	madeBySubscribeEvents = "subscribe_events"
	madeByNative          = "native"
	madeByPoll            = "poll"
)

// builtinSubscribe is the SDKs' own subscribe tool.
const builtinSubscribe = "subscribe_events"

// evEndPolicyChanged: the app closed the event (subscribe "tools"), and the
// subscription wasn't made by one of its tools.
const evEndPolicyChanged = "policy_changed"

// policyTerminated is what a native client's receiver is told when a close
// ends its webhook.
const policyTerminated = "The app now lets only its own tools subscribe to this event"

// conversationVia is how a POST /event-subscriptions call subscribes: its
// tool (call.tool), the built-in subscribe_events, or "" when it names no
// tool.
func conversationVia(call callIn) string {
	switch tool := beaconTool(call.Tool); {
	case tool == builtinSubscribe:
		return madeBySubscribeEvents
	case tool != "":
		return madeByToolPrefix + tool
	}
	return ""
}

// madeByTool says via is one of the app's own tools.
func madeByTool(via string) bool { return strings.HasPrefix(via, madeByToolPrefix) }

// errToolsOnly refuses a subscription to a tools-only event that isn't
// made by one of the app's tools (403 tools_only).
type errToolsOnly struct {
	event  string
	noTool bool // the call named no tool
}

func (e errToolsOnly) Error() string {
	if e.noTool {
		return "the app lets only its own tools subscribe to " + e.event + ": the request must name the tool making it (call.tool)"
	}
	return "the app lets only its own tools subscribe to " + e.event + ": use the app's tool for it"
}

// refuseToolsOnly refuses (errToolsOnly) a subscription to app's event
// name made other than by one of its tools (via), if the event is
// tools-only. An event the manifest doesn't have is left to eventTarget.
func refuseToolsOnly(app apps.App, name, via string) error {
	et, err := app.Manifest.EventType(name)
	if err != nil || !et.ToolsOnly() || madeByTool(via) {
		return nil
	}
	return errToolsOnly{event: name, noTool: via == ""}
}

// policyLock is the advisory lock between a subscription that a close
// would end and the close: such a subscription holds it shared from the
// start of its transaction, and a manifest that closes an event holds it
// exclusively while it ends what's made that way, so one made with the
// event still open either committed before the close looked, or sees the
// event closed (holdPolicy).
func policyLock(app string) string { return "subscribe-policy:" + app }

// holdPolicy, first in the transaction of a subscription made through via,
// refuses it (errToolsOnly) if the event is tools-only as stored now. A
// tool's (or one without via, which a close keeps) takes no lock.
func holdPolicy(ctx context.Context, tx pgx.Tx, app, event, via string) error {
	if via == "" || madeByTool(via) {
		return nil
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock_shared(hashtext($1))`, policyLock(app)); err != nil {
		return err
	}
	var policy string
	if err := tx.QueryRow(ctx, `SELECT coalesce((SELECT event_subscribe ->> $2 FROM apps WHERE id = $1), '')`, app, event).Scan(&policy); err != nil {
		return err
	}
	if policy == apps.SubscribeTools {
		return errToolsOnly{event: event}
	}
	return nil
}

// subscribeChanged is the manifest's apps.SubscribeHook: in every
// workspace the app keeps, a closed event's subscriptions not made by the
// app's tools end (closeEvents), and each change is recorded in the
// workspace's audit log (audit.EventSubscribeChanged: the event, from, to,
// and for a close how many it ended and how many conversation
// subscriptions without via it kept). It runs in the manifest's
// transaction, so the change and what it ends commit together.
func (a *API) subscribeChanged(ctx context.Context, tx pgx.Tx, app string, changes []apps.SubscribeChange) error {
	closing := false
	for _, c := range changes {
		closing = closing || c.Closes()
	}
	if closing {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, policyLock(app)); err != nil {
			return err
		}
	}
	rows, _ := tx.Query(ctx, `SELECT id::text FROM tenants WHERE app_id = $1 ORDER BY id`, app)
	tenants, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	for _, t := range tenants {
		if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, t); err != nil {
			return err
		}
		if err := closeEvents(ctx, tx, app, changes, closing); err != nil {
			return err
		}
	}
	_, err = tx.Exec(ctx, `SELECT set_config('app.tenant_id', '', true)`)
	return err
}

// closeEvents acts on changes in the transaction's workspace (see
// subscribeChanged), adding what it ends to each change's Ended.
func closeEvents(ctx context.Context, tx pgx.Tx, app string, changes []apps.SubscribeChange, closing bool) error {
	if closing {
		// Lapsed ones end as expired, not as closed.
		if _, err := closeLapsed(ctx, tx); err != nil {
			return err
		}
	}
	for i := range changes {
		c := &changes[i]
		detail := map[string]any{"event": c.Event, "from": c.From, "to": c.To}
		if c.Closes() {
			list, err := queryEventSubs(ctx, tx, `es.source_kind = $1 AND es.source_id = $2 AND es.event = $3 AND es.ended_at IS NULL
				AND (es.via IN ('subscribe_events', 'native', 'poll', 'push') OR es.via IS NULL AND es.destination IN ('webhook', 'poll'))
				ORDER BY es.id FOR UPDATE OF es`, sourceApp, app, c.Event)
			if err != nil {
				return err
			}
			if err := endEventSubs(ctx, tx, list, evEndPolicyChanged, audit.App(app), true); err != nil {
				return err
			}
			var kept int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM event_subscriptions WHERE source_kind = $1 AND source_id = $2 AND event = $3
				AND ended_at IS NULL AND via IS NULL AND destination = 'conversation'`, sourceApp, app, c.Event).Scan(&kept); err != nil {
				return err
			}
			c.Ended += len(list)
			detail["ended"], detail["kept_without_via"] = len(list), kept
		}
		if err := audit.Record(ctx, tx, audit.Entry{Actor: audit.App(app), Action: audit.EventSubscribeChanged, AppID: app, Detail: detail}); err != nil {
			return err
		}
	}
	return nil
}

// isToolsOnly reports err is a tools-only refusal.
func isToolsOnly(err error) bool { return errors.As(err, new(errToolsOnly)) }
