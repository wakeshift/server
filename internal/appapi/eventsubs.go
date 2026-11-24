package appapi

// Event subscriptions: a client subscribes to one of the app's event
// types (its manifest's events) by name and arguments. The arguments pick
// a topic and a filter (apps.EventType.Subscribe), so routing stays with
// topics; the subscription is also recorded on its own,
// source-agnostically (event_subscriptions: source, event, canonical
// arguments, principal, destination), as the consent record the console
// lists and revokes.
//
// Two paths make the same record:
//
//	POST /api/apps/v1/events/subscribe        a client that speaks MCP Events (ChatGPT): a webhook,
//	                                          one per (principal, callback, event, arguments), with
//	                                          a session of its own that the webhook delivers
//	POST /api/apps/v1/event-subscriptions     any other client (bridged): the calling conversation
//	                                          {call, name, arguments, once?, replay?, ttl?}
//	POST /api/apps/v1/event-subscriptions/unsubscribe {call, name, arguments} → {ended, addition?}: addition
//	                                          says the conversation's session key stays the same (an
//	                                          SDK's unsubscribe_events adds it to its result), or, for
//	                                          a call without the key that ended nothing, to call again
//	                                          with it (any tool: a wrapped tool's call adds it to its block)
//	POST /api/apps/v1/event-subscriptions/list        {user} → the user's live subscriptions; {call} (no
//	                                          user) → the calling conversation's
//	POST /api/apps/v1/event-subscriptions/end         {user, id}: ends one of them, any destination;
//	                                          {call, id}: one of the calling conversation's
//	POST /api/apps/v1/events/emit             {name, arguments, payload, text, id?, to?}: the payload
//	                                          is checked against the event's schema and published to
//	                                          topic(arguments)
//
// A personal event (apps.EventType.Personal) gives each subscriber its own
// topic, derived from the caller's user (call.user), never from the
// arguments; emit names the subscriber it's for (to), so only that
// subscriber's subscriptions, native or bridged, receive it; or a list of
// users, subscriptions by ID and native subscribers (emit.go).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/agents"
	"github.com/wakeshift/server/internal/apps"
	"github.com/wakeshift/server/internal/audit"
	"github.com/wakeshift/server/internal/grants"
	"github.com/wakeshift/server/internal/harnesses"
	"github.com/wakeshift/server/internal/httpx"
	"github.com/wakeshift/server/internal/mcpevents"
	"github.com/wakeshift/server/internal/sessions"
)

// Where an event subscription's occurrences go.
const (
	toConversation = "conversation"
	toWebhook      = "webhook"
)

// Why an event subscription ended.
const (
	evEndUnsubscribed = "unsubscribed"
	evEndRevoked      = "revoked"
	evEndExpired      = "expired"   // its topic subscription ended (ttl, its grant, a once one FinishedOnce skipped)
	evEndDelivered    = "delivered" // a "once" subscription made its one delivery (FinishedOnce)
	// evEndRolledBack: the SDK ended it because the tool call that made it
	// failed (its result never reached the agent).
	evEndRolledBack = "rolled_back"
)

// sourceApp is the source kind of Wakeshift's own apps.
const sourceApp = "app"

// eventSubIn is a subscribe or unsubscribe by event name and arguments.
type eventSubIn struct {
	Call      callIn          `json:"call"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
	Once      bool            `json:"once"`
	Replay    *duration       `json:"replay"`
	TTL       *duration       `json:"ttl"`
	// Reason is why the app subscribes the conversation, shown in the
	// console (call.subscribe's reason).
	Reason string `json:"reason"`
	// Label names a new session (agent@label), for a call with an agent
	// token.
	Label string `json:"label"`
	// DeferAddition: the subscribe writes no instructions and mints no
	// beacon; it makes or updates the call's pending-addition record
	// (Pending, if an earlier request in the call made it), and the SDK
	// asks /calls/addition once at the end of the call (calls.go).
	DeferAddition bool   `json:"defer_addition"`
	Pending       string `json:"pending"`
}

// builtinUnsubscribe is the SDKs' own unsubscribe tool, which adds the
// unsubscribe's addition to its reply rather than asking /calls/addition.
const builtinUnsubscribe = "unsubscribe_events"

// maxReason is the longest reason a subscription may give.
const maxReason = 200

// eventConversation is the conversation a subscribe or unsubscribe is for:
// with an agent token, the agent the app acts for, in the workspace it
// keeps for it (agentCall; its subscriber is <workspace>/<agent>);
// otherwise the app's user's conversation (findConversation). handle is
// the session handle an agent call passed.
func (a *API) eventConversation(ctx context.Context, app apps.App, call callIn, create bool) (cv *conversation, handle string, err error) {
	if call.AgentToken != "" {
		return a.agentCall(ctx, app, call)
	}
	cv, err = a.findConversation(ctx, app, call, create)
	return cv, "", err
}

// eventResolve finds (with create, makes) cv's session in the transaction:
// an agent call's by its agent (agentResolve), the app's user's by the call
// (resolve).
func (a *API) eventResolve(ctx context.Context, tx pgx.Tx, cv *conversation, handle, label string, create bool) error {
	if cv.agentHandles {
		return agentResolve(ctx, tx, cv, handle, label, create)
	}
	return a.resolve(ctx, tx, cv, create)
}

// errPersonalNoUser: a personal event's subscription belongs to the app's
// user, so the call must name one.
var errPersonalNoUser = badRequest{errors.New("this is a personal event: each subscription belongs to the app's user, so call.user is required")}

// eventTarget checks an event subscription's name and arguments against
// the app's manifest. A personal event's topic is the subscriber's own
// (apps.PersonalTopic), from user: the caller's identity, never an
// argument.
func eventTarget(app apps.App, name string, args json.RawMessage, user string) (apps.Subscription, error) {
	et, err := app.Manifest.EventType(name)
	if err != nil {
		return apps.Subscription{}, err
	}
	sub, err := et.Subscribe(args)
	if err != nil {
		return sub, err
	}
	if et.Personal {
		user = strings.TrimSpace(user)
		if user == "" {
			return sub, errPersonalNoUser
		}
		sub.Topic = apps.PersonalTopic(sub.Topic, user)
	} else if _, private := privateTo(sub.Topic); private {
		// Arguments that render a shared event's topic into the private
		// space ({owner} = "u", {repo} = "p<hash>") would read someone's
		// personal events.
		return sub, badRequest{errors.New(`this event's arguments can't make a private topic (u/…): use another value than "u" for its first argument`)}
	}
	if err := checkTopic(sub.Topic); err != nil {
		return sub, err
	}
	return sub, nil
}

// eventSubscribe subscribes the calling conversation (a bridged client) to
// an event type, or refreshes its subscription. With an agent token, the
// conversation is the agent's the app acts for (its subscriber the agent's
// user, <workspace>/<agent>); with defer_addition, the call's block comes
// later, from /calls/addition.
func (a *API) eventSubscribe(w http.ResponseWriter, r *http.Request) {
	var in eventSubIn
	if !httpx.Decode(w, r, &in) {
		return
	}
	app := AppFrom(r.Context())
	in.Reason = strings.TrimSpace(in.Reason)
	if len(in.Reason) > maxReason || len(in.Label) > 64 {
		a.respond(w, nil, badRequest{errors.New("reason is at most 200 characters, label 64")})
		return
	}
	deferred := in.DeferAddition || in.Pending != ""
	// An agent call's subscriber is its token's agent, so its conversation
	// is found first; the app's user's, after the arguments are checked
	// (finding it may make the user's workspace).
	var cv *conversation
	var handle string
	var err error
	user := in.Call.User
	if in.Call.AgentToken != "" {
		if cv, handle, err = a.eventConversation(r.Context(), app, in.Call, true); err != nil {
			a.respondEventSub(w, nil, err)
			return
		}
		user = cv.call.User
	}
	via := conversationVia(in.Call)
	sub, err := eventTarget(app, in.Name, in.Arguments, user)
	if err == nil {
		err = refuseToolsOnly(app, in.Name, via)
	}
	if err != nil {
		a.respondEventSub(w, nil, err)
		return
	}
	ttl := defaultTopicTTL
	if in.TTL != nil {
		ttl = time.Duration(*in.TTL)
	}
	var replay time.Duration
	if in.Replay != nil {
		replay = time.Duration(*in.Replay)
	}
	switch {
	case ttl < minTopicTTL || ttl > maxTopicTTL:
		a.respond(w, nil, badRequest{errors.New("ttl is between 1 minute and 365 days")})
		return
	case replay < 0 || replay > maxReplay:
		a.respond(w, nil, badRequest{errors.New("replay is at most 24 hours")})
		return
	}
	filters, err := topicFilters(sub.Topic, sub.Filter, in.Once)
	if err != nil {
		a.respond(w, nil, err)
		return
	}
	if cv == nil {
		if cv, err = a.findConversation(r.Context(), app, in.Call, true); err != nil {
			a.respond(w, nil, err)
			return
		}
	}
	mode := modeTopic
	if cv.agentHandles {
		mode = modeAgent
	}
	var out struct {
		EventSubscription struct {
			ID        string          `json:"id"`
			User      string          `json:"user"`
			Event     string          `json:"event"`
			Arguments json.RawMessage `json:"arguments"`
			Created   bool            `json:"created"`
			ExpiresAt time.Time       `json:"expires_at"`
		} `json:"event_subscription"`
		Session  string     `json:"session"`
		Addition toolResult `json:"addition"`
		Agent    recognized `json:"agent"`
		// Pending is the call's pending-addition record (deferred).
		Pending string `json:"pending,omitempty"`
		// NativeLine: the SDK gives this caller the native push line for
		// the event, keeping this bridged subscription as the fallback;
		// false keeps today's block.
		NativeLine bool `json:"native_line"`
		// Wakes: Wakeshift wakes this conversation as it stands (wakePath):
		// the SDKs' call.wakes.
		Wakes bool `json:"wakes"`
		// CallLimit: how many seconds the recognized harness lets one tool
		// call take (harnesses.Harness.CallLimit), if known: the SDKs'
		// call.callLimit.
		CallLimit int `json:"call_limit,omitempty"`
	}
	err = a.DB.InTenant(r.Context(), cv.tenant, func(tx pgx.Tx) error {
		ctx := r.Context()
		// First, before anything is locked (policyLock).
		if err := holdPolicy(ctx, tx, app.ID, sub.Event, via); err != nil {
			return err
		}
		locked, err := lockTopic(ctx, tx, app.ID, sub.Topic)
		if err != nil {
			return err
		}
		if err := lockStripe(ctx, tx, app.ID, sub.Topic); err != nil {
			return err
		}
		if err := a.eventResolve(ctx, tx, cv, handle, in.Label, true); err != nil {
			return err
		}
		// Right after resolving, as an unsubscribe takes it.
		if err := lockWatching(ctx, tx, cv.session.ID); err != nil {
			return err
		}
		if err := serialize(ctx, tx, "event:"+cv.session.ID+":"+sub.Event+":"+sub.Key); err != nil {
			return err
		}
		// The call's record, before anything is subscribed: its first says
		// whether the conversation had subscribed before this call.
		var pending pendingRecord
		if deferred {
			if pending, err = pendingFor(ctx, tx, cv, in.Pending, mode); err != nil {
				return err
			}
		}
		if _, err := closeLapsed(ctx, tx); err != nil {
			return err
		}
		id, subID, err := liveConversationSub(ctx, tx, app.ID, sub, cv.session.ID)
		if err == nil {
			// Its routing may have ended since closeLapsed looked (the
			// expiry pass doesn't lock the record): then it's ended here,
			// and a new one is made.
			if err = refreshTopicSub(ctx, tx, subID, sub.Topic, filters, in.Once, ttl); errors.Is(err, errRoutingEnded) {
				if _, err = closeLapsed(ctx, tx); err == nil {
					err = pgx.ErrNoRows
				}
			}
		}
		created := errors.Is(err, pgx.ErrNoRows)
		switch {
		case err == nil:
			// Renewed by another call (or outside one): the call that made
			// it can no longer roll it back. Renewed by one of the app's
			// tools, it's the tool's from now on (a close keeps it).
			if _, err := tx.Exec(ctx, `UPDATE event_subscriptions SET refreshed_at = now(), reason = coalesce(nullif($2, ''), reason),
				created_pending = CASE WHEN created_pending = nullif($3, '') THEN created_pending END,
				via = CASE WHEN $4 LIKE 'tool:%' THEN $4 ELSE via END WHERE id = $1::uuid`, id, in.Reason, pending.ID, via); err != nil {
				return err
			}
		case created:
			if subID, err = insertTopicSub(ctx, tx, cv, sub.Topic, filters, in.Once, ttl); err != nil {
				return err
			}
			if id, err = recordEventSub(ctx, tx, app, sub, cv, subID, toConversation, via, "", ""); err != nil {
				return err
			}
			if in.Reason != "" || pending.ID != "" {
				if _, err := tx.Exec(ctx, `UPDATE event_subscriptions SET reason = coalesce(nullif($2, ''), reason), created_pending = nullif($3, '')
					WHERE id = $1::uuid`, id, in.Reason, pending.ID); err != nil {
					return err
				}
			}
			if replay > 0 {
				if err := a.replay(ctx, tx, app, sub.Topic, subID, filters, locked, replay); err != nil {
					return err
				}
			}
		default:
			return err
		}
		if err := sessions.SetWatching(ctx, tx, cv.session.ID, true); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT expires_at FROM topic_subscriptions WHERE subscription_id = $1`, subID).Scan(&out.EventSubscription.ExpiresAt); err != nil {
			return err
		}
		e := &out.EventSubscription
		e.ID, e.User, e.Event, e.Arguments, e.Created = id, cv.call.User, sub.Event, sub.Arguments, created
		out.Session, out.Agent = cv.handle, cv.recognized
		if out.NativeLine, err = a.nativePush(ctx, tx, cv); err != nil {
			return err
		}
		if out.Wakes, err = a.wakePath(ctx, tx, cv); err != nil {
			return err
		}
		out.CallLimit = cv.callLimit()
		if deferred {
			out.Pending = pending.ID
			var once, stale string
			if in.Once {
				once = onceNote
			}
			if cv.staleHandle {
				stale = staleNote()
			}
			return armPending(ctx, tx, pending.ID, once, stale)
		}
		out.Addition, err = a.harnessResult(ctx, tx, cv, forSubscribe)
		if in.Once {
			out.Addition.Text = notesLast(out.Addition.Text, onceNote)
		}
		return err
	})
	a.respondEventSub(w, out, err)
}

// liveConversationSub finds the session's live subscription to the event
// with these arguments (pgx.ErrNoRows if none), its ID and its routing's,
// locking the record as every path that ends one does first (event.end,
// the console, an unsubscribe): one ending it meanwhile is waited for, and
// then it's not found, rather than renewed after it ended.
func liveConversationSub(ctx context.Context, tx pgx.Tx, app string, sub apps.Subscription, session string) (id, subID string, err error) {
	err = tx.QueryRow(ctx, `SELECT id::text, subscription_id::text FROM event_subscriptions
		WHERE source_kind = $1 AND source_id = $2 AND event = $3 AND arguments_key = $4 AND session_id = $5
		AND destination = 'conversation' AND ended_at IS NULL FOR UPDATE`, sourceApp, app, sub.Event, sub.Key, session).Scan(&id, &subID)
	return id, subID, err
}

// lockTopic takes the topic's members lock and returns when it was taken.
// A subscribe takes it first, before anything else is locked: no publish
// to the topic runs while it does, so one either finished before it (and
// is replayed) or reads the new member (and delivers live).
func lockTopic(ctx context.Context, tx pgx.Tx, app, topic string) (time.Time, error) {
	var locked time.Time
	err := tx.QueryRow(ctx, `SELECT clock_timestamp() FROM (SELECT pg_advisory_xact_lock(hashtext('topic-members:' || $1 || ':' || $2))) l`,
		app, topic).Scan(&locked)
	return locked, err
}

// recordEventSub records a new event subscription routed by the topic
// subscription subID, made through via (subscribepolicy.go; "" for none),
// with its audit entry, returning its ID.
func recordEventSub(ctx context.Context, tx pgx.Tx, app apps.App, sub apps.Subscription, cv *conversation, subID, dest, via, hookURL, hookID string) (string, error) {
	return recordEventSubAs(ctx, tx, app, sub, cv, cv.call.User, subID, dest, via, hookURL, hookID)
}

// recordEventSubAs is recordEventSub for principal: an anonymous voucher
// redemption's is its workspace's external ID (~conversation:…), which no
// app user ID can equal.
func recordEventSubAs(ctx context.Context, tx pgx.Tx, app apps.App, sub apps.Subscription, cv *conversation, principal, subID, dest, via, hookURL, hookID string) (string, error) {
	var id string
	if err := tx.QueryRow(ctx, `INSERT INTO event_subscriptions (tenant_id, source_kind, source_id, event, arguments, arguments_key, principal,
		destination, agent_id, session_id, webhook_url, webhook_id, subscription_id, created_by, via)
		VALUES (current_tenant(), $1, $2, $3, $4, $5, $6, $7, $8, $9, nullif($10, ''), nullif($11, ''), $12, $13, nullif($14, '')) RETURNING id::text`,
		sourceApp, app.ID, sub.Event, string(sub.Arguments), sub.Key, principal, dest, cv.act.agent, cv.session.ID, hookURL, hookID, subID,
		"app:"+app.ID, via).Scan(&id); err != nil {
		return "", err
	}
	detail := map[string]any{"id": id, "event": sub.Event, "arguments": json.RawMessage(sub.Arguments), "destination": dest}
	if via != "" {
		detail["via"] = via
	}
	return id, audit.Record(ctx, tx, audit.Entry{Actor: audit.App(app.ID), Action: "event_subscription.created", AgentID: cv.act.agent, AppID: app.ID,
		GrantID: cv.act.grant.ID, Detail: detail})
}

// eventUnsubscribe ends the calling conversation's subscription to an
// event type with these arguments (idempotent). The conversation asked, so
// there's no notice and what still waits for the subscription is dropped;
// if it was the conversation's last live subscription, its listener stops
// at once (endEventSubs). With reason "rolled_back", the SDK is undoing a
// subscription its failed tool call made: audited as rolled_back. With the
// call's pending record too, it's ended only if that call made it and no
// other call has renewed it since.
func (a *API) eventUnsubscribe(w http.ResponseWriter, r *http.Request) {
	var in eventSubIn
	if !httpx.Decode(w, r, &in) {
		return
	}
	app := AppFrom(r.Context())
	reason := evEndUnsubscribed
	switch in.Reason {
	case "":
	case evEndRolledBack:
		reason = evEndRolledBack
	default:
		a.respond(w, nil, badRequest{errors.New(`reason is "rolled_back" (a failed tool call's subscription), or none`)})
		return
	}
	// Found by event and arguments alone, so a subscription to an event the
	// manifest no longer lists can still be ended.
	key, err := apps.ArgumentsKey(in.Arguments)
	if err != nil {
		a.respondEventSub(w, nil, err)
		return
	}
	sub := apps.Subscription{Event: in.Name, Key: key}
	cv, handle, err := a.eventConversation(r.Context(), app, in.Call, false)
	ended, found := false, false
	note := "" // for an SDK's own unsubscribe tool (a wrapped tool's call gets it from /calls/addition)
	if err == nil {
		err = a.DB.InTenant(r.Context(), cv.tenant, func(tx pgx.Tx) error {
			ctx := r.Context()
			if err := a.eventResolve(ctx, tx, cv, handle, "", false); err != nil {
				return err
			}
			found = true
			// Before anything is read: whether this ends the session's last
			// subscription (endListening) mustn't miss a subscribe
			// committing meanwhile, whose listener it would stop.
			if err := lockWatching(ctx, tx, cv.session.ID); err != nil {
				return err
			}
			// A rollback naming its call's pending record ends only what
			// that call made and no other call has renewed since.
			var made string
			if reason == evEndRolledBack {
				made = in.Pending
			}
			list, err := queryEventSubs(ctx, tx, `es.source_kind = $1 AND es.source_id = $2 AND es.event = $3 AND es.arguments_key = $4
				AND es.session_id = $5 AND es.destination = 'conversation' AND es.ended_at IS NULL
				AND ($6::text = '' OR es.created_pending = $6::text) FOR UPDATE OF es`,
				sourceApp, app.ID, sub.Event, sub.Key, cv.session.ID, made)
			if err != nil {
				return err
			}
			ended = len(list) > 0
			if ended && reason == evEndUnsubscribed && !cv.agentHandles && passesKey(cv) {
				if in.Call.Tool == builtinUnsubscribe {
					// An SDK's own tool adds it to its reply; it asks for no block.
					note = harnesses.KeepSessionNote(cv.handle)
				} else if _, err := tx.Exec(ctx, `UPDATE sessions SET keep_key_note_due = true WHERE id = $1`, cv.session.ID); err != nil {
					// A wrapped tool's: its block says it (keepSession).
					return err
				}
			}
			return endEventSubs(ctx, tx, list, reason, audit.App(app.ID), false)
		})
	}
	if errors.Is(err, errUnknownKey) && reason == evEndUnsubscribed {
		note = unknownKeyNote() // its key named nothing, so nothing ended
	}
	if errors.Is(err, errNoConversation) || errors.Is(err, errNoSession) {
		err = nil // nothing to end
	}
	if err == nil && !found && reason == evEndUnsubscribed && keyOnlyBinding(in.Call) {
		// Never matched to a session by anything else (the same user's other
		// conversations look the same): the agent is told to pass its key.
		note = harnesses.KeylessUnsubscribeNote
	}
	out := map[string]any{"ended": ended}
	if note != "" && err == nil {
		out["addition"] = map[string]any{"text": note}
	}
	a.respondEventSub(w, out, err)
}

// keyOnlyBinding reports whether call is a keyless call from a
// conversation that only its session key names: no key, no agent token
// (an app acting for its agents names the handle in its own tools), and no
// conversation its harness names in _meta (a Codex thread, Antigravity's,
// or Copilot's once our hook is installed), so the core couldn't have
// found its conversation (Claude Code, Cursor, opencode, an unrecognized
// client). Its unsubscribe that ended nothing is told to pass the key.
func keyOnlyBinding(call callIn) bool {
	if strings.TrimSpace(call.Session) != "" || call.AgentToken != "" {
		return false
	}
	h, _, _, _, _ := harnessOf(call)
	return threadOf(h, call.Meta) == ""
}

// eventSubsList lists one of the app's users' live event subscriptions,
// or, for a call without a user (an SDK's list_subscriptions on a server
// without identify), the calling conversation's: the session its call
// names, found as an unsubscribe finds it (none found lists nothing).
func (a *API) eventSubsList(w http.ResponseWriter, r *http.Request) {
	var in struct {
		User string  `json:"user"`
		Call *callIn `json:"call"`
	}
	if !httpx.Decode(w, r, &in) {
		return
	}
	app := AppFrom(r.Context())
	in.User = strings.TrimSpace(in.User)
	if in.User == "" && in.Call != nil {
		out := []EventSubscription{}
		err := a.inCallConversation(r.Context(), app, *in.Call, func(ctx context.Context, tx pgx.Tx, session string) error {
			if _, err := closeLapsed(ctx, tx); err != nil {
				return err
			}
			list, err := queryEventSubs(ctx, tx, `es.source_kind = $1 AND es.source_id = $2 AND es.ended_at IS NULL
				AND es.session_id = $3 AND es.destination = 'conversation' ORDER BY es.created_at`, sourceApp, app.ID, session)
			out = list
			return err
		})
		a.respond(w, map[string]any{"subscriptions": out}, err)
		return
	}
	if !appUser(in.User) {
		a.respond(w, nil, badRequest{errors.New("user is the app's ID for the person (at most 200 characters), or call is the calling conversation")})
		return
	}
	out := []EventSubscription{}
	where, err := a.userWorkspaces(r.Context(), app, in.User)
	for _, uw := range where {
		if err != nil {
			break
		}
		err = a.DB.InTenant(r.Context(), uw.tenant, func(tx pgx.Tx) error {
			if _, err := closeLapsed(r.Context(), tx); err != nil {
				return err
			}
			list, err := queryEventSubs(r.Context(), tx, `es.source_kind = $1 AND es.source_id = $2 AND es.ended_at IS NULL
				AND ($3 = '' OR es.agent_id = $3) AND ($4 = '' OR es.principal = $4) ORDER BY es.created_at`, sourceApp, app.ID, uw.agent, uw.principal)
			out = append(out, list...)
			return err
		})
	}
	a.respond(w, map[string]any{"subscriptions": out}, err)
}

// inCallConversation runs fn in the tenant of the conversation call names,
// with its session's ID, without making either: a call that names none
// (no session key, an unknown one) runs nothing and isn't an error.
func (a *API) inCallConversation(ctx context.Context, app apps.App, call callIn, fn func(ctx context.Context, tx pgx.Tx, session string) error) error {
	cv, handle, err := a.eventConversation(ctx, app, call, false)
	if err == nil {
		err = a.DB.InTenant(ctx, cv.tenant, func(tx pgx.Tx) error {
			if err := a.eventResolve(ctx, tx, cv, handle, "", false); err != nil {
				return err
			}
			return fn(ctx, tx, cv.session.ID)
		})
	}
	if errors.Is(err, errNoConversation) || errors.Is(err, errNoSession) {
		return nil
	}
	return err
}

// eventSubEnd ends one of an app's user's event subscriptions by its ID
// (from eventSubsList), whatever its destination: how a user of a headless
// app, who has no console, stops a subscription a native client made
// (idempotent; another user's or another app's ID names nothing). For a
// call without a user, it ends one of the calling conversation's (found
// as eventSubsList finds it); any other ID names nothing.
func (a *API) eventSubEnd(w http.ResponseWriter, r *http.Request) {
	var in struct {
		User string  `json:"user"`
		Call *callIn `json:"call"`
		ID   string  `json:"id"`
	}
	if !httpx.Decode(w, r, &in) {
		return
	}
	app := AppFrom(r.Context())
	in.User = strings.TrimSpace(in.User)
	byCall := in.User == "" && in.Call != nil
	if !byCall && !appUser(in.User) {
		a.respond(w, nil, badRequest{errors.New("user is the app's ID for the person (at most 200 characters), or call is the calling conversation")})
		return
	}
	ended := false
	if !uuidRE.MatchString(in.ID) {
		a.respond(w, map[string]any{"ended": ended}, nil)
		return
	}
	if byCall {
		// The conversation asked, as with an unsubscribe: no notice, and its
		// listener stops if this was its last subscription.
		err := a.inCallConversation(r.Context(), app, *in.Call, func(ctx context.Context, tx pgx.Tx, session string) error {
			if err := lockWatching(ctx, tx, session); err != nil {
				return err
			}
			list, err := queryEventSubs(ctx, tx, `es.id = $1::uuid AND es.source_kind = $2 AND es.source_id = $3
				AND es.ended_at IS NULL AND es.session_id = $4 AND es.destination = 'conversation' FOR UPDATE OF es`,
				in.ID, sourceApp, app.ID, session)
			if err != nil || len(list) == 0 {
				return err
			}
			ended = true
			return endEventSubs(ctx, tx, list, evEndUnsubscribed, audit.App(app.ID), false)
		})
		a.respond(w, map[string]any{"ended": ended}, err)
		return
	}
	where, err := a.userWorkspaces(r.Context(), app, in.User)
	for _, uw := range where {
		if err != nil {
			break
		}
		err = a.DB.InTenant(r.Context(), uw.tenant, func(tx pgx.Tx) error {
			ctx := r.Context()
			list, err := queryEventSubs(ctx, tx, `es.id = $1::uuid AND es.source_kind = $2 AND es.source_id = $3
				AND es.ended_at IS NULL AND ($4 = '' OR es.agent_id = $4) AND ($5 = '' OR es.principal = $5) FOR UPDATE OF es`,
				in.ID, sourceApp, app.ID, uw.agent, uw.principal)
			if err != nil || len(list) == 0 {
				return err
			}
			ended = true
			return endUnasked(ctx, tx, list[0], audit.App(app.ID))
		})
	}
	a.respond(w, map[string]any{"ended": ended}, err)
}

// eventEmit publishes one occurrence of an event type: the payload is
// checked against the event's schema, and goes to the topic its arguments
// name, as the occurrence's data. A personal event's occurrence goes to the
// recipients to names: one user (the single-user path), or a list
// (emitToMany).
func (a *API) eventEmit(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
		Payload   json.RawMessage `json:"payload"`
		Text      string          `json:"text"`
		ID        string          `json:"id"`
		// To is who a personal event's occurrence goes to: the app's user
		// ID, or a list of recipients (emit.go); only personal events take
		// it.
		To json.RawMessage `json:"to"`
	}
	if !httpx.DecodeLimit(w, r, &in, maxListBody) {
		return
	}
	app := AppFrom(r.Context())
	et, err := app.Manifest.EventType(in.Name)
	if err != nil {
		a.respondEventSub(w, nil, err)
		return
	}
	list, single, err := parseRecipients(in.To)
	if err != nil {
		a.respondEventSub(w, nil, err)
		return
	}
	var to string
	if single {
		to = list[0].User
	}
	multi := len(list) > 0 && !single
	var topic string
	if multi {
		if !et.Personal {
			// emitTopic's refusals, for a list.
			_, err = emitTopic(et, in.Arguments, "x")
		}
		if err == nil {
			topic, err = et.Target(in.Arguments)
		}
	} else {
		topic, err = emitTopic(et, in.Arguments, to)
	}
	if err == nil {
		err = et.CheckPayload(in.Payload)
	}
	ev := topicEvent{ID: in.ID, Text: in.Text, Data: in.Payload, deleteOnDelivery: et.DeleteOnDelivery, event: et.Name}
	if err == nil {
		err = ev.check()
	}
	if err == nil && ev.ID != "" {
		// What makes a retry with this id the same emit (checkRepeat).
		var base string
		if base, err = et.Target(in.Arguments); err == nil {
			ev.fingerprint, err = emitFingerprint(et.Name, base, list, ev.Data)
			// The text as sent, not as check trimmed it: two texts that
			// differ only past the cut are still different emits.
			ev.textFingerprint = textFingerprint(in.Text)
		}
	}
	if err != nil {
		a.respondEventSub(w, nil, err)
		return
	}
	if multi {
		out, err := a.emitToMany(r.Context(), app, et, topic, list, ev)
		a.respondEventSub(w, out, err)
		return
	}
	matched, dup, err := a.publishTopics(r.Context(), app.ID, []string{topic}, ev, "")
	// The same shape as a multi-recipient emit's: the one user recipient
	// is unmatched when nothing it has subscribed was reached.
	out := emitOut{Matched: matched, Duplicate: dup, Unmatched: []any{}, Ended: []map[string]string{}, TextTrimmed: ev.trimmed}
	if single && matched == 0 {
		out.Unmatched = append(out.Unmatched, to)
	}
	a.respondEventSub(w, out, err)
}

// emitTopic is the topic an occurrence of et with these arguments goes to:
// for a personal event, the one subscriber to names.
func emitTopic(et apps.EventType, args json.RawMessage, to string) (string, error) {
	topic, err := et.Target(args)
	to = strings.TrimSpace(to)
	switch {
	case err != nil:
		return "", err
	case et.Personal && to == "":
		return "", apps.EventArgsError{Msg: "this is a personal event: pass to, the one subscriber (the app's user ID) it's for"}
	case !et.Personal && to != "":
		return "", apps.EventArgsError{Msg: "to is only for personal events: this event goes to everyone subscribed with these arguments"}
	case et.Personal:
		topic = apps.PersonalTopic(topic, to)
	case strings.HasPrefix(topic, "u/"):
		return "", apps.EventArgsError{Msg: `this event's arguments can't make a private topic (u/…): use another value than "u" for its first argument`}
	}
	return topic, checkTopic(topic)
}

// eventsSubscribeNative is events/subscribe (events.go) for one of the
// app's event types: a webhook per (principal, callback, event,
// arguments), delivering from a session of its own in the principal's
// workspace.
func (a *API) eventsSubscribeNative(w http.ResponseWriter, r *http.Request, in eventsIn) {
	if in.Voucher != "" {
		a.voucherSubscribe(w, r, in) // vouchers.go
		return
	}
	app := AppFrom(r.Context())
	args, _ := json.Marshal(in.Arguments)
	sub, err := eventTarget(app, in.Name, args, in.Call.User)
	if err == nil {
		err = refuseToolsOnly(app, in.Name, madeByNative)
	}
	if err == nil && in.Delivery.Mode != "webhook" {
		err = errUnsupportedMode{in.Delivery.Mode}
	}
	if err == nil {
		err = mcpevents.CheckURL(in.Delivery.URL)
		if err != nil {
			err = a.unreachable(in.Call, in.dialect(), badRequest{err})
		}
	}
	if err == nil && in.Cursor != nil {
		if _, cerr := mcpevents.ParseCursor(*in.Cursor); cerr != nil {
			err = badRequest{cerr}
		}
	}
	var secret []byte
	if err == nil {
		secret, err = mcpevents.ParseSecret(in.Delivery.Secret)
		if err != nil {
			err = badRequest{err}
		}
	}
	if err == nil && strings.TrimSpace(in.Call.User) == "" {
		err = badRequest{errors.New("call.user is required: a webhook subscription belongs to the app's user")}
	}
	if err != nil {
		a.respondEventSub(w, nil, err)
		return
	}
	ttl := mcpevents.TTLOf(in.TTLMs) // at most a day, never without expiry
	var maxAge time.Duration
	if in.MaxAgeMs != nil {
		maxAge = mcpevents.MaxAgeOf(*in.MaxAgeMs)
	}
	filters, err := topicFilters(sub.Topic, sub.Filter, false)
	if err != nil {
		a.respond(w, nil, err)
		return
	}
	call := in.Call
	call.Session = ""
	cv, err := a.findConversation(r.Context(), app, call, true)
	if err != nil {
		a.respondEventSub(w, nil, err)
		return
	}

	ctx := r.Context()
	// The webhook's ID is the subscription's identity (the principal's
	// workspace, callback, event and arguments), so subscribing again
	// refreshes it; a callback the principal hasn't a live subscription at
	// (verifiedFor) is verified (outside a transaction: it's a request to
	// it) before anything is recorded.
	id := mcpevents.SubscriptionID(cv.tenant, "event:"+sub.Key, in.Delivery.URL, in.Name)
	var verified bool
	err = a.DB.InTenant(ctx, cv.tenant, func(tx pgx.Tx) (err error) {
		if verified, err = mcpevents.Exists(ctx, tx, id); err == nil && !verified {
			verified, err = verifiedFor(ctx, tx, app.ID, cv.call.User, in.Delivery.URL)
		}
		return err
	})
	if err == nil && !verified {
		err = a.unreachable(in.Call, in.dialect(), a.verifyCallback(ctx, app.ID, "user:"+cv.call.User, in.Delivery.URL, id, secret))
	}
	var res mcpevents.Subscribed
	if err == nil {
		err = a.DB.InTenant(ctx, cv.tenant, func(tx pgx.Tx) error {
			if err := holdPolicy(ctx, tx, app.ID, sub.Event, madeByNative); err != nil {
				return err
			}
			locked, err := lockTopic(ctx, tx, app.ID, sub.Topic)
			if err != nil {
				return err
			}
			if err := lockStripe(ctx, tx, app.ID, sub.Topic); err != nil {
				return err
			}
			if err := ensureActor(ctx, tx, cv); err != nil {
				return err
			}
			if err := serialize(ctx, tx, "webhook:"+id); err != nil {
				return err
			}
			if _, err := closeLapsed(ctx, tx); err != nil {
				return err
			}
			var evID, subID, sessionID, agent string
			err = tx.QueryRow(ctx, `SELECT id::text, subscription_id::text, session_id::text, agent_id FROM event_subscriptions
				WHERE source_kind = $1 AND source_id = $2 AND principal = $3 AND webhook_url = $4 AND event = $5 AND arguments_key = $6
				AND destination = 'webhook' AND ended_at IS NULL FOR UPDATE`,
				sourceApp, app.ID, cv.call.User, in.Delivery.URL, sub.Event, sub.Key).Scan(&evID, &subID, &sessionID, &agent)
			created := errors.Is(err, pgx.ErrNoRows)
			switch {
			case err == nil && agent != cv.act.agent:
				// Another client's subscription (same user, callback,
				// event and arguments): only the agent it was made for
				// refreshes it, under its own grant.
				return errOtherClient
			case err == nil:
				if cv.session, err = sessions.Get(ctx, tx, sessionID); err != nil {
					return err
				}
			case created:
				// A session of its own: the webhook delivers it, and only it.
				if cv.session, err = newSession(ctx, tx, cv, ""); err != nil {
					return err
				}
			default:
				return err
			}
			cv.handle = handleOf(cv.session)
			// A new subscription has a new session, so a cursor replays
			// nothing from there: replaySince (below) replays what was
			// published since it instead.
			if res, err = mcpevents.Subscribe(ctx, tx, a.Events, mcpevents.Webhook{ID: id, App: app.ID, Agent: cv.act.agent, Session: cv.session.ID,
				SessionHandle: cv.handle, Name: in.Name, URL: in.Delivery.URL, Secret: secret, TTL: ttl,
				Dialect: in.dialect(), Cursor: in.Cursor, MaxAge: maxAge}, a.now()); err != nil {
				return err
			}
			// The routing lapses with the webhook unless it's refreshed.
			topicTTL := max(ttl, minTopicTTL)
			if created {
				if subID, err = insertTopicSub(ctx, tx, cv, sub.Topic, filters, false, topicTTL); err != nil {
					return err
				}
				if _, err = recordEventSub(ctx, tx, app, sub, cv, subID, toWebhook, madeByNative, in.Delivery.URL, id); err != nil {
					return err
				}
				// A client whose subscription lapsed presents its last
				// cursor: what was published since then reached no session
				// here, so it's replayed from the app's recent publishes,
				// or the subscription is truncated.
				if in.Cursor != nil {
					from, _ := mcpevents.ParseCursor(*in.Cursor) // checked above
					var skipped bool
					if skipped, err = a.replaySince(ctx, tx, app, sub.Topic, subID, filters, locked, from, maxAge); err != nil {
						return err
					}
					res.Truncated = res.Truncated || skipped
				}
			} else {
				if err := refreshTopicSub(ctx, tx, subID, sub.Topic, filters, false, topicTTL); err != nil {
					return err
				}
				_, err = tx.Exec(ctx, `UPDATE event_subscriptions SET refreshed_at = now(), via = coalesce(via, $2) WHERE id = $1::uuid`, evID, madeByNative)
			}
			if err != nil {
				return err
			}
			return sessions.SetWatching(ctx, tx, cv.session.ID, true)
		})
	}
	out := map[string]any{"id": id, "refresh_before": res.ExpiresAt, "cursor": res.Cursor, "truncated": res.Truncated, "created": res.Created}
	if res.Status != nil {
		out["delivery_status"] = res.Status
	}
	a.respondEventSub(w, out, err)
}

// eventsUnsubscribeNative is events/unsubscribe for one of the app's event
// types (idempotent).
func (a *API) eventsUnsubscribeNative(w http.ResponseWriter, r *http.Request, in eventsIn) {
	app := AppFrom(r.Context())
	if in.Voucher != "" && strings.TrimSpace(in.Call.User) == "" {
		// An anonymous voucher subscription, by its voucher and callback
		// (vouchers.go); a user's are ended by user, below.
		a.voucherUnsubscribe(w, r, in)
		return
	}
	args, _ := json.Marshal(in.Arguments)
	call := in.Call
	call.Session = ""
	if strings.TrimSpace(call.User) == "" {
		a.respondEventSub(w, map[string]any{"ended": false}, nil)
		return
	}
	// Found by event and arguments alone, so a subscription to an event the
	// manifest no longer lists can still be ended.
	key, err := apps.ArgumentsKey(args)
	if err != nil {
		a.respondEventSub(w, nil, err)
		return
	}
	sub := apps.Subscription{Event: in.Name, Key: key}
	cv, err := a.findConversation(r.Context(), app, call, false)
	ended := false
	if err == nil {
		err = a.DB.InTenant(r.Context(), cv.tenant, func(tx pgx.Tx) error {
			ctx := r.Context()
			list, err := queryEventSubs(ctx, tx, `es.source_kind = $1 AND es.source_id = $2 AND es.principal = $3 AND es.webhook_url = $4
				AND es.event = $5 AND es.arguments_key = $6 AND es.agent_id = $7 AND es.destination = 'webhook' AND es.ended_at IS NULL ORDER BY es.id FOR UPDATE OF es`,
				sourceApp, app.ID, cv.call.User, in.Delivery.URL, sub.Event, sub.Key, cv.act.agent)
			if err != nil {
				return err
			}
			ended = len(list) > 0
			return endEventSubs(ctx, tx, list, evEndUnsubscribed, audit.App(app.ID), false)
		})
	}
	if errors.Is(err, errNoConversation) || errors.Is(err, errNoSession) {
		err = nil
	}
	a.respondEventSub(w, map[string]any{"ended": ended}, err)
}

// ensureActor makes the caller's agent and the app's grant for it (held
// FOR SHARE, so a revocation waits for the transaction), as resolve does,
// without picking a session.
func ensureActor(ctx context.Context, tx pgx.Tx, cv *conversation) error {
	agent := cv.act.agent
	if err := agents.Ensure(ctx, tx, agent); err != nil {
		return err
	}
	g, err := grants.EnsureByApp(ctx, tx, topicsApp(cv.app), agent)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `SELECT 1 FROM grants WHERE id = $1 FOR SHARE`, g.ID); err != nil {
		return err
	}
	cv.act.grant = g
	return nil
}

// unknownEventMessage is unknown_event's message: the event's name, and the
// likely cause, an app that hasn't sent Wakeshift its manifest yet (such as
// one that emits at startup, before any agent has connected).
func unknownEventMessage(err error) string {
	named := "by that name"
	var u apps.UnknownEventError
	if errors.As(err, &u) {
		name := u.Name
		if len(name) > 64 {
			name = name[:64] + "…"
		}
		named = fmt.Sprintf("named %q", name)
	}
	return "your app has no event " + named + ". Wakeshift learns your app's events from its manifest: " +
		"publish it at startup (publishManifest in the SDKs, or PUT /api/apps/v1/manifest) or attach your MCP server before emitting, " +
		"and check the name matches an event your app defines"
}

// respondEventSub is respond, with the errors event subscriptions make.
func (a *API) respondEventSub(w http.ResponseWriter, v any, err error) {
	var bad apps.EventArgsError
	var rate emitRateError
	switch {
	case errors.Is(err, apps.ErrUnknownEventType):
		httpx.WriteError(w, http.StatusNotFound, "unknown_event", unknownEventMessage(err))
	case errors.As(err, &bad):
		httpx.WriteError(w, http.StatusBadRequest, "bad_arguments", bad.Error())
	case errors.As(err, &rate):
		httpx.WriteError(w, http.StatusTooManyRequests, "rate_limited", rate.message(a.SupportEmail))
	case errors.As(err, new(idReusedError)):
		httpx.WriteError(w, http.StatusConflict, "id_reused", err.Error())
	case isToolsOnly(err):
		httpx.WriteError(w, http.StatusForbidden, "tools_only", err.Error())
	default:
		a.respondEvents(w, v, err)
	}
}
