// Package subscriptions stores what each agent is subscribed to.
package subscriptions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/agents"
	"github.com/wakeshift/server/internal/events"
)

// Subscription delivers events of EventType (or a pattern ending in ".*")
// whose attributes match every filter to an agent.
type Subscription struct {
	ID         string            `json:"id"`
	AgentID    string            `json:"agent_id"`
	EventType  string            `json:"event_type"`
	Filters    map[string]string `json:"filters"`
	Status     string            `json:"status"`
	Reason     string            `json:"reason,omitempty"`
	StartSeq   uint64            `json:"start_seq"`
	CreatedBy  string            `json:"created_by"`
	CreatedAt  time.Time         `json:"created_at"`
	CanceledAt *time.Time        `json:"canceled_at,omitempty"` // column cancelled_at
	SessionID  string            `json:"session_id,omitempty"`  // the session that joined
}

// Statuses. Canceled was stored and reported as "cancelled" before
// migration 116; rows the previous release wrote while it rolled out
// still say so, and scan reports them as Canceled (AGENTS.md, Pending
// removals).
const (
	Active   = "active"
	Canceled = "canceled"
)

// cancelledBefore116 is how a canceled subscription was stored before
// migration 116.
const cancelledBefore116 = "cancelled"

var (
	ErrNotFound = errors.New("subscriptions: no such subscription")
	ErrBadType  = errors.New("subscriptions: invalid event type")
)

// New is what Create needs.
type New struct {
	AgentID   string
	EventType string
	Filters   map[string]string
	Reason    string
	CreatedBy string // who created it: a user's email, or "app:<id>"
	GrantID   string // the grant an app created it under (revoking the grant cancels it)
	SessionID string // the session that joined: deliveries follow it
	// Replaces is the active subscription this one takes over from (the
	// same conversation subscribing again): it starts where that one is,
	// so what that one hadn't yet delivered, this one does. Cancel the old
	// one after: its open deliveries pass to this one if it matches them.
	Replaces string
}

// Create adds an active subscription, starting after the tenant's latest
// event (or, replacing one, where that one is).
func Create(ctx context.Context, tx pgx.Tx, n New) (Subscription, error) {
	if err := ValidType(n.EventType); err != nil {
		return Subscription{}, err
	}
	if ok, err := agents.Exists(ctx, tx, n.AgentID); err != nil {
		return Subscription{}, err
	} else if !ok {
		return Subscription{}, agents.ErrNotFound
	}
	if n.Filters == nil {
		n.Filters = map[string]string{}
	}
	start, err := events.LastSeq(ctx, tx)
	if err != nil {
		return Subscription{}, err
	}
	last := start
	if n.Replaces != "" {
		// Locking its cursor waits out a pipeline step on it, so the place
		// read here is the one its deliveries reached.
		err := tx.QueryRow(ctx, `SELECT s.start_seq, c.last_seq FROM subscriptions s
			JOIN subscription_cursors c ON c.tenant_id = s.tenant_id AND c.subscription_id = s.id
			WHERE s.id = $1 AND s.agent_id = $2 AND s.event_type = $3 AND s.status = 'active' FOR UPDATE OF c`,
			n.Replaces, n.AgentID, n.EventType).Scan(&start, &last)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) { // gone already: start after the latest event
			return Subscription{}, err
		}
		if err == nil {
			// The lock may have waited out a cancellation that committed
			// after this statement's snapshot: a new statement sees it. A
			// canceled subscription's deliveries are gone, so start after
			// the latest event instead.
			var active bool
			if err := tx.QueryRow(ctx, `SELECT status = 'active' FROM subscriptions WHERE id = $1`, n.Replaces).Scan(&active); err != nil {
				return Subscription{}, err
			}
			if !active {
				if start, err = events.LastSeq(ctx, tx); err != nil {
					return Subscription{}, err
				}
				last = start
			}
		}
	}
	var grant *string
	if n.GrantID != "" {
		grant = &n.GrantID
	}
	sub, err := scan(tx.QueryRow(ctx, `INSERT INTO subscriptions (tenant_id, agent_id, event_type, filters, reason, start_seq, created_by, grant_id, session_id)
		VALUES (current_tenant(), $1, $2, $3, $4, $5, $6, $7, nullif($8, '')::uuid) RETURNING `+columns,
		n.AgentID, n.EventType, n.Filters, n.Reason, start, n.CreatedBy, grant, n.SessionID))
	if err != nil {
		return sub, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO subscription_cursors (tenant_id, subscription_id, last_seq) VALUES (current_tenant(), $1, $2)`, sub.ID, last)
	return sub, err
}

// List returns subscriptions, newest first: all of them, or one agent's.
func List(ctx context.Context, tx pgx.Tx, agentID string) ([]Subscription, error) {
	rows, err := tx.Query(ctx, `SELECT `+columns+` FROM subscriptions
		WHERE $1 = '' OR agent_id = $1 ORDER BY created_at DESC, id`, agentID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Subscription, error) { return scan(r) })
}

// Get returns one subscription.
func Get(ctx context.Context, tx pgx.Tx, id string) (Subscription, error) {
	s, err := scan(tx.QueryRow(ctx, `SELECT `+columns+` FROM subscriptions WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return s, ErrNotFound
	}
	return s, err
}

// Cancel stops a subscription. Its deliveries that haven't reached the
// agent are canceled, unless another of the agent's active subscriptions
// also owes it (matches the event, and would deliver it to the same
// place), which then takes them over. Canceling one already canceled is
// a no-op.
func Cancel(ctx context.Context, tx pgx.Tx, id string) (Subscription, error) {
	// Touch the cursor first: if the pipeline runner is mid-step on this
	// subscription, wait for it to commit, so the deliveries it's creating
	// are visible below (a step that began before this commits retries and
	// then sees the subscription canceled).
	if _, err := tx.Exec(ctx, `UPDATE subscription_cursors SET updated_at = now() WHERE subscription_id = $1`, id); err != nil {
		return Subscription{}, err
	}
	s, err := scan(tx.QueryRow(ctx, `UPDATE subscriptions SET status = 'canceled', cancelled_at = coalesce(cancelled_at, now())
		WHERE id = $1 RETURNING `+columns, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return s, ErrNotFound
	}
	if err != nil {
		return s, err
	}
	others, err := List(ctx, tx, s.AgentID)
	if err != nil {
		return s, err
	}
	rows, _ := tx.Query(ctx, `SELECT id::text, event_id::text, target FROM deliveries
		WHERE subscription_id = $1 AND state IN ('pending', 'in_flight')`, id)
	type open struct{ ID, EventID, Target string }
	list, err := pgx.CollectRows(rows, pgx.RowToStructByPos[open])
	if err != nil {
		return s, err
	}
	for _, d := range list {
		e, err := events.Get(ctx, tx, d.EventID)
		if err != nil {
			return s, err
		}
		heir := ""
		for _, o := range others {
			if o.ID == id || !o.Matches(e) {
				continue
			}
			// The heir is one that would deliver it to the same place: the
			// same conversation's subscription, the one whose own
			// conversation the copy is for, or one no session claimed (it's
			// the agent's as a whole, and delivers to whichever session it
			// picks, pipeline.Targets). Another session's subscription
			// mustn't keep this one's delivery alive.
			if o.SessionID == s.SessionID || o.SessionID == d.Target || o.SessionID == "" {
				heir = o.ID
				break
			}
		}
		if heir != "" {
			_, err = tx.Exec(ctx, `UPDATE deliveries SET subscription_id = $2 WHERE id = $1`, d.ID, heir)
		} else {
			_, err = tx.Exec(ctx, `UPDATE deliveries SET state = 'cancelled', finished_at = now(), lease_until = NULL, lease_holder = NULL
				WHERE id = $1`, d.ID)
		}
		if err != nil {
			return s, err
		}
	}
	return s, nil
}

// CancelAll cancels each of the subscriptions, as Cancel does.
func CancelAll(ctx context.Context, tx pgx.Tx, ids []string) error {
	for _, id := range ids {
		if _, err := Cancel(ctx, tx, id); err != nil {
			return err
		}
	}
	return nil
}

// Filters and attributes for topics (an app's events, internal/appapi). Keys starting with
// "$" are Wakeshift's: apps can't set them on events or subscriptions.
const (
	// OnceFilter "true": the subscription ends after its first delivery
	// (the pipeline ends it).
	OnceFilter = "$once"
	// DataFilterPrefix starts a filter on a top-level field of the event's
	// data: "$data.branch", with the field's value in CanonicalJSON form
	// (`"main"`, `42`, `true`).
	DataFilterPrefix = "$data."
	// ForAttr, on an event, limits it to the subscription with that ID (an
	// app's send to one conversation), which gets it whatever its data
	// filters say.
	ForAttr = "$subscription"
)

// CanonicalJSON is a scalar JSON value (string, number, boolean or null) in
// the form data filters compare: compact, numbers as Go formats a float64.
// Objects, arrays and anything invalid aren't comparable.
func CanonicalJSON(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 {
		return "", false
	}
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return "", false
	}
	switch v.(type) {
	case string, float64, bool, nil:
		b, _ := json.Marshal(v)
		return string(b), true
	}
	return "", false
}

// Finish ends a subscription that has done what it was for (a "once"
// subscription after its delivery): unlike Cancel, what it already
// delivered or queued stands.
func Finish(ctx context.Context, tx pgx.Tx, id string) error {
	_, err := tx.Exec(ctx, `UPDATE subscriptions SET status = 'canceled', cancelled_at = coalesce(cancelled_at, now()) WHERE id = $1`, id)
	return err
}

// PruneEndedCursors deletes, at most limit at a time, the cursors of
// subscriptions that ended over CursorKeep ago, returning how many. Every
// reader of a cursor joins it to an active subscription, or (ending one)
// takes it if it's there, and nothing makes an ended subscription active
// again, so an ended subscription's cursor is never read; left in place,
// it's only rows the pipeline's step and sweep scan past (ended
// subscriptions are never deleted). Rows a step holds are skipped (SKIP
// LOCKED); the next pass gets them.
func PruneEndedCursors(ctx context.Context, tx pgx.Tx, limit int) (int64, error) {
	tag, err := tx.Exec(ctx, `DELETE FROM subscription_cursors WHERE tenant_id = current_tenant() AND subscription_id IN (
		SELECT c.subscription_id FROM subscription_cursors c
		JOIN subscriptions s ON s.tenant_id = c.tenant_id AND s.id = c.subscription_id
		WHERE c.tenant_id = current_tenant() AND s.status <> 'active'
			AND (s.cancelled_at IS NULL OR s.cancelled_at < now() - $2::interval)
		LIMIT $1 FOR UPDATE OF c SKIP LOCKED)`, limit, CursorKeep.String())
	return tag.RowsAffected(), err
}

// CursorKeep is how long an ended subscription's cursor is kept: far past
// any pipeline step or ending that began before it ended.
const CursorKeep = time.Hour

// Matches reports whether an event passes a subscription's type and filters.
// A filter matches an attribute with the same value (a key no event
// carries, such as a membership's old $wake, matches nothing). An agent is
// never woken by its own posts.
func (s Subscription) Matches(e events.Event) bool {
	if s.Status != Active || !events.MatchType(s.EventType, e.Type) || e.Seq <= s.StartSeq {
		return false
	}
	direct := false
	if to := e.Attrs[ForAttr]; to != "" {
		if to != s.ID {
			return false
		}
		direct = true
	}
	if e.From.Kind == "agent" && e.From.ID == s.AgentID {
		// The agent's own posts: only another of its sessions, named, is
		// woken (codex#1 asking codex#2). Never the agent as a whole.
		if e.FromSession == "" || !slices.ContainsFunc(e.Mentions, func(m string) bool { return mentionsSessionOf(m, s.AgentID) }) {
			return false
		}
	}
	var data map[string]json.RawMessage
	for k, v := range s.Filters {
		if k == OnceFilter {
			continue
		}
		if field, ok := strings.CutPrefix(k, DataFilterPrefix); ok {
			if direct {
				continue
			}
			if data == nil {
				data = map[string]json.RawMessage{}
				json.Unmarshal(e.Data, &data)
			}
			if got, ok := CanonicalJSON(data[field]); !ok || got != v {
				return false
			}
			continue
		}
		if e.Attrs[k] != v {
			return false
		}
	}
	return true
}

// mentionsSessionOf reports whether a mention names one of agent's
// sessions (agent#n or agent@label).
func mentionsSessionOf(m, agent string) bool {
	rest, ok := strings.CutPrefix(m, agent)
	return ok && len(rest) > 1 && (rest[0] == '#' || rest[0] == '@')
}

// typeRE is an event type ("slack.message", "app.agentchat.channel.pr-482")
// or a prefix pattern ending in ".*". Types are lowercase; matching is exact.
var typeRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*(\.[a-z0-9][a-z0-9_-]*)*(\.\*)?$`)

// ValidType accepts an event type or a prefix pattern.
func ValidType(t string) error {
	if len(t) > 200 || !typeRE.MatchString(t) {
		return fmt.Errorf("%w: %q isn't a lowercase event type or a pattern ending in \".*\"", ErrBadType, t)
	}
	return nil
}

const columns = `id, agent_id, event_type, filters, status, reason, start_seq, created_by, created_at, cancelled_at, coalesce(session_id::text, '')`

func scan(row pgx.Row) (Subscription, error) {
	var s Subscription
	err := row.Scan(&s.ID, &s.AgentID, &s.EventType, &s.Filters, &s.Status, &s.Reason, &s.StartSeq, &s.CreatedBy, &s.CreatedAt, &s.CanceledAt, &s.SessionID)
	if s.Status == cancelledBefore116 {
		s.Status = Canceled
	}
	return s, err
}
