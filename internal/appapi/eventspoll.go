package appapi

// events/poll: MCP Events' poll delivery (SEP-3415), for clients that
// can't take a webhook (no public URL: a local agent, a client behind NAT)
// and for those that prefer to poll (Mastra). The app's MCP server (the
// SDKs' attach) passes each poll on here:
//
//	POST /api/apps/v1/events/poll {call, name, arguments, cursor, max_age_ms, max_events}
//	  → {events: [EventOccurrence…], cursor, truncated, has_more, next_poll_ms, created}
//
// The draft has no subscribe step for poll: the first poll makes the
// subscription. Wakeshift keeps one, as it does a webhook's: an event
// subscription per (principal, client, event, arguments), destination
// 'poll', with a session of its own that only its polls read, so what's
// emitted between polls waits there (durably, as every delivery does), and
// the subscription is the consent record the console and
// list_subscriptions show and stop. It lapses after PollLease without a
// poll, as a webhook lapses without a refresh.
//
// Cursors are the session's event sequence numbers (mcpevents.Cursor), so
// a poll is at least once: a poll with cursor c acknowledges everything
// at or before c (the client has it) and returns what's after it, in
// order. A cursor of null starts from now: the first poll returns none,
// and a live subscription's waiting events are dropped. The occurrences
// are the webhook's (mcpevents.Occurrence): the same eventId for the same
// delivery on every poll, the payload untouched, the rendered line in
// _meta.
//
// Ending: a poll subscription the person (or the app) ended answers the
// next poll that carries one of its cursors with Forbidden (poll's
// termination signal), so a client's poll loop stops rather than quietly
// starting it again; a poll from null is a new subscription, as a webhook
// refresh after termination is.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/apps"
	"github.com/wakeshift/server/internal/deliveries"
	"github.com/wakeshift/server/internal/events"
	"github.com/wakeshift/server/internal/httpx"
	"github.com/wakeshift/server/internal/mcpevents"
	"github.com/wakeshift/server/internal/sessions"
)

// toPoll is a poll subscription's destination.
const toPoll = "poll"

// Poll limits.
const (
	// PollLease is how long a poll subscription lives without a poll.
	PollLease = 10 * time.Minute
	// PollInterval is the nextPollMs a poll suggests.
	PollInterval = 10 * time.Second
	// pollDefault and pollMax: occurrences in one poll's result.
	pollDefault = 50
	pollMax     = 100
	// pollRefreshAfter: a poll moves the lease on only once this much of
	// it has passed, so a client polling every second doesn't write every
	// second.
	pollRefreshAfter = time.Minute
)

type pollIn struct {
	Call      callIn         `json:"call"`
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
	Cursor    *string        `json:"cursor"`
	MaxAgeMs  *int64         `json:"max_age_ms"`
	MaxEvents *int           `json:"max_events"`
	// Voucher is accepted and ignored: a voucher never polls (a tools-only
	// event's poll is refused, voucher or not). Never logged.
	Voucher string `json:"voucher"`
}

// pollOut is a poll's result: events are EventOccurrences as the client
// gets them.
type pollOut struct {
	Events     []json.RawMessage `json:"events"`
	Cursor     string            `json:"cursor"`
	Truncated  bool              `json:"truncated"`
	HasMore    bool              `json:"has_more"`
	NextPollMs int64             `json:"next_poll_ms"`
	Created    bool              `json:"created"`
}

// errPollEnded: the poll subscription its cursor came from was ended by
// the person or the app (Forbidden).
var errPollEnded = errors.New("this subscription was stopped: subscribe again (poll with a null cursor) only if you're asked to")

// livePoll is a poll subscription that's live: its record, session and
// routing.
type livePoll struct {
	ID, Session, Subscription, Agent string
}

func (a *API) eventsPoll(w http.ResponseWriter, r *http.Request) {
	var in pollIn
	if !httpx.Decode(w, r, &in) {
		return
	}
	out, err := a.poll(r.Context(), AppFrom(r.Context()), in)
	a.respondPoll(w, out, err)
}

func (a *API) poll(ctx context.Context, app apps.App, in pollIn) (pollOut, error) {
	out := pollOut{Events: []json.RawMessage{}, NextPollMs: PollInterval.Milliseconds()}
	args, _ := json.Marshal(in.Arguments)
	sub, err := eventTarget(app, in.Name, args, in.Call.User)
	if err == nil {
		// Every poll, before its cursor is read: a cursor proves nothing.
		err = refuseToolsOnly(app, in.Name, madeByPoll)
	}
	if err != nil {
		return out, err
	}
	var from *int64
	if in.Cursor != nil {
		c, err := mcpevents.ParseCursor(*in.Cursor)
		if err != nil {
			return out, badRequest{err}
		}
		from = &c
	}
	if strings.TrimSpace(in.Call.User) == "" {
		return out, badRequest{errors.New("call.user is required: a poll subscription belongs to the app's user")}
	}
	limit := pollDefault
	if in.MaxEvents != nil && *in.MaxEvents > 0 {
		limit = min(*in.MaxEvents, pollMax)
	}
	var maxAge time.Duration
	if in.MaxAgeMs != nil {
		maxAge = mcpevents.MaxAgeOf(*in.MaxAgeMs)
	}
	call := in.Call
	call.Session = ""
	// A poll mostly finds its subscription live: looked up without making
	// the workspace (a write) first.
	cv, err := a.findConversation(ctx, app, call, false)
	if errors.Is(err, errNoConversation) {
		cv, err = a.findConversation(ctx, app, call, true)
	}
	if err != nil {
		return out, err
	}
	var live *livePoll
	err = a.DB.InTenant(ctx, cv.tenant, func(tx pgx.Tx) (err error) {
		if err := holdPolicy(ctx, tx, app.ID, sub.Event, madeByPoll); err != nil {
			return err
		}
		live, err = findPoll(ctx, tx, app.ID, cv.call.User, cv.act.agent, sub)
		if err != nil || live == nil {
			return err
		}
		return a.readPoll(ctx, tx, app, sub.Event, *live, from, limit, maxAge, &out)
	})
	if err != nil || live != nil {
		return out, err
	}
	// None live: a new subscription, unless the cursor is from one that was
	// stopped.
	filters, err := topicFilters(sub.Topic, sub.Filter, false)
	if err != nil {
		return out, err
	}
	err = a.DB.InTenant(ctx, cv.tenant, func(tx pgx.Tx) error {
		if err := holdPolicy(ctx, tx, app.ID, sub.Event, madeByPoll); err != nil {
			return err
		}
		if _, err := lockTopic(ctx, tx, app.ID, sub.Topic); err != nil {
			return err
		}
		if err := lockStripe(ctx, tx, app.ID, sub.Topic); err != nil {
			return err
		}
		if err := ensureActor(ctx, tx, cv); err != nil {
			return err
		}
		if err := serialize(ctx, tx, "poll:"+cv.call.User+"\x1f"+cv.act.agent+"\x1f"+sub.Event+"\x1f"+sub.Key); err != nil {
			return err
		}
		if _, err := closeLapsed(ctx, tx); err != nil {
			return err
		}
		// Another poll may have made it meanwhile.
		if live, err = findPoll(ctx, tx, app.ID, cv.call.User, cv.act.agent, sub); err != nil {
			return err
		} else if live != nil {
			return a.readPoll(ctx, tx, app, sub.Event, *live, from, limit, maxAge, &out)
		}
		if from != nil {
			var reason string
			err := tx.QueryRow(ctx, `SELECT ended_reason FROM event_subscriptions WHERE source_kind = $1 AND source_id = $2 AND principal = $3
				AND agent_id = $4 AND event = $5 AND arguments_key = $6 AND destination = 'poll' AND ended_at IS NOT NULL ORDER BY ended_at DESC LIMIT 1`,
				sourceApp, app.ID, cv.call.User, cv.act.agent, sub.Event, sub.Key).Scan(&reason)
			switch {
			case err == nil && reason != evEndExpired && reason != evEndRolledBack:
				return errPollEnded
			case err != nil && !errors.Is(err, pgx.ErrNoRows):
				return err
			}
			// It lapsed (or never was): what was emitted meanwhile went
			// nowhere.
			out.Truncated = true
		}
		var err error
		if cv.session, err = newSession(ctx, tx, cv, ""); err != nil {
			return err
		}
		cv.handle = handleOf(cv.session)
		subID, err := insertTopicSub(ctx, tx, cv, sub.Topic, filters, false, PollLease)
		if err != nil {
			return err
		}
		if _, err := recordEventSub(ctx, tx, app, sub, cv, subID, toPoll, madeByPoll, "", ""); err != nil {
			return err
		}
		if err := sessions.SetWatching(ctx, tx, cv.session.ID, true); err != nil {
			return err
		}
		out.Created = true
		// From now: nothing is owed the new session yet.
		out.Cursor = mcpevents.Cursor(0)
		return nil
	})
	return out, err
}

// findPoll is the live poll subscription for (principal, agent, event,
// arguments), or nil. Its routing must be live too: one whose lease ran
// out is lapsed (closeLapsed ends the record later).
func findPoll(ctx context.Context, tx pgx.Tx, app, principal, agent string, sub apps.Subscription) (*livePoll, error) {
	var p livePoll
	err := tx.QueryRow(ctx, `SELECT es.id::text, es.session_id::text, es.subscription_id::text, es.agent_id FROM event_subscriptions es
		JOIN topic_subscriptions ts ON ts.tenant_id = es.tenant_id AND ts.subscription_id = es.subscription_id
		JOIN subscriptions s ON s.tenant_id = es.tenant_id AND s.id = es.subscription_id
		WHERE es.source_kind = $1 AND es.source_id = $2 AND es.principal = $3 AND es.agent_id = $4 AND es.event = $5 AND es.arguments_key = $6
		AND es.destination = 'poll' AND es.ended_at IS NULL AND ts.ended_at IS NULL AND ts.expires_at > now() AND s.status = 'active'`,
		sourceApp, app, principal, agent, sub.Event, sub.Key).Scan(&p.ID, &p.Session, &p.Subscription, &p.Agent)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &p, err
}

// readPoll answers a poll on a live subscription: its lease moves on, what
// the cursor covers is acknowledged, and what's after it is read.
//
// Locks: the routing's topic_subscriptions row first (the lease), then the
// session's deliveries, skipping any another transaction holds (one ending
// the subscription cancels them). Ending takes the routing's rows before
// its deliveries too, so the two can't deadlock.
func (a *API) readPoll(ctx context.Context, tx pgx.Tx, app apps.App, name string, p livePoll, from *int64, limit int, maxAge time.Duration, out *pollOut) error {
	// One poll of a subscription at a time: two at once would each read
	// and acknowledge the same deliveries. An advisory lock, which nothing
	// that ends a subscription takes, so it can't deadlock with one.
	if err := serialize(ctx, tx, "poll-read:"+p.ID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE topic_subscriptions SET expires_at = now() + make_interval(secs => $2::int)
		WHERE subscription_id = $1 AND ended_at IS NULL AND expires_at < now() + make_interval(secs => $3::int)`,
		p.Subscription, int(PollLease.Seconds()), int((PollLease - pollRefreshAfter).Seconds())); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE event_subscriptions SET refreshed_at = now() WHERE id = $1::uuid AND refreshed_at < now() - make_interval(secs => $2::int)`,
		p.ID, int(pollRefreshAfter.Seconds())); err != nil {
		return err
	}
	// What the session has waiting at or before the cursor: acknowledged
	// (the client has it) or, from null, dropped (it starts from now).
	// A cursor is never past what the subscription has had: one that is
	// isn't one of its own, and would acknowledge what it never returned.
	var head int64
	if err := tx.QueryRow(ctx, `SELECT coalesce(max(event_seq), 0) FROM deliveries WHERE agent_id = $1 AND target = $2 AND `+deliveries.OfApp("deliveries", "$3"),
		p.Agent, p.Session, app.ID).Scan(&head); err != nil {
		return err
	}
	through := head
	if from != nil {
		if *from > head {
			return badRequest{errors.New("cursor is past anything this subscription has had: pass a cursor a poll returned")}
		}
		through = *from
	}
	rows, _ := tx.Query(ctx, `SELECT id::text FROM deliveries WHERE agent_id = $1 AND target = $2 AND state IN ('pending', 'in_flight')
		AND event_seq <= $3 AND `+deliveries.OfApp("deliveries", "$4")+` ORDER BY event_seq FOR UPDATE SKIP LOCKED`, p.Agent, p.Session, through, app.ID)
	covered, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	if from == nil {
		if _, err := tx.Exec(ctx, `UPDATE deliveries SET state = 'cancelled', finished_at = now(), lease_until = NULL, lease_holder = NULL,
			last_error = 'the client polled from now' WHERE id = ANY($1::uuid[])`, covered); err != nil {
			return err
		}
		out.Cursor = mcpevents.Cursor(through)
		return nil
	}
	for _, id := range covered {
		if _, err := deliveries.AckFor(ctx, tx, p.Agent, p.Session, app.ID, id); err != nil {
			return err
		}
	}
	// What's after it, in order, one more than the limit (has_more), up to
	// the first that isn't to be sent yet (paced): the cursor never passes
	// a delivery it didn't return, or the next poll would acknowledge it.
	type row struct {
		ID, EventID, Subscription string
		Seq                       int64
		Ready                     bool
	}
	rows, _ = tx.Query(ctx, `SELECT id::text, event_id::text, subscription_id::text, event_seq, available_at <= now() FROM deliveries
		WHERE agent_id = $1 AND target = $2 AND state IN ('pending', 'in_flight') AND event_seq > $3 AND `+deliveries.OfApp("deliveries", "$5")+`
		ORDER BY event_seq LIMIT $4`, p.Agent, p.Session, through, limit+1, app.ID)
	list, err := pgx.CollectRows(rows, pgx.RowToStructByPos[row])
	if err != nil {
		return err
	}
	for i, d := range list {
		if !d.Ready {
			list, out.HasMore = list[:i], false
			break
		}
	}
	if len(list) > limit {
		list, out.HasMore = list[:limit], true
	}
	var floor time.Time
	if maxAge > 0 {
		floor = a.now().Add(-maxAge)
	}
	cursor := through
	var skipped []string
	for _, d := range list {
		cursor = d.Seq
		e, err := events.Get(ctx, tx, d.EventID)
		if err != nil {
			return err
		}
		dl := deliveries.Delivery{ID: d.ID, AgentID: p.Agent, Target: p.Session, SubscriptionID: d.Subscription, State: "pending", Event: e}
		if !floor.IsZero() && !e.OccurredAt.IsZero() && e.OccurredAt.Before(floor) {
			skipped = append(skipped, d.ID) // older than maxAgeMs
			continue
		}
		occ, err := mcpevents.OccurrenceJSON(dl, name, app.ID)
		if errors.Is(err, events.ErrRedacted) || errors.Is(err, mcpevents.ErrTooLarge) {
			skipped = append(skipped, d.ID) // can't be sent: a gap
			continue
		}
		if err != nil {
			return err
		}
		out.Events = append(out.Events, occ)
	}
	if len(skipped) > 0 {
		out.Truncated = true
		if _, err := tx.Exec(ctx, `UPDATE deliveries SET state = 'dead', finished_at = now(), lease_until = NULL, lease_holder = NULL,
			last_error = 'skipped by a poll (too old for maxAgeMs, too large, or its content deleted)'
			WHERE id = ANY($1::uuid[]) AND state IN ('pending', 'in_flight')`, skipped); err != nil {
			return err
		}
	}
	out.Cursor = mcpevents.Cursor(cursor)
	if out.HasMore {
		out.NextPollMs = 0
	}
	return nil
}

// respondPoll is respondEventSub, with a stopped subscription's Forbidden.
func (a *API) respondPoll(w http.ResponseWriter, v pollOut, err error) {
	if errors.Is(err, errPollEnded) {
		httpx.WriteError(w, http.StatusForbidden, "subscription_stopped", err.Error())
		return
	}
	a.respondEventSub(w, v, err)
}
