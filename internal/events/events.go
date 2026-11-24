// Package events is the event log: append-only, per tenant, in one order.
//
// Functions take a transaction from db.InTenant, so callers can append an
// event together with other writes (an inbound webhook's dedupe, a publish's
// lineage checks) and readers see only their tenant's events.
package events

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Event is one entry in the log.
type Event struct {
	Seq            uint64            `json:"seq"`
	ID             string            `json:"id"`
	Type           string            `json:"type"`
	Source         string            `json:"source"`
	SourceEventID  string            `json:"source_event_id,omitempty"`
	SourceRevision string            `json:"source_revision,omitempty"`
	OccurredAt     time.Time         `json:"occurred_at"`
	ReceivedAt     time.Time         `json:"received_at"`
	From           Party             `json:"from"`
	Subject        string            `json:"subject,omitempty"`
	Body           string            `json:"body,omitempty"`
	Data           json.RawMessage   `json:"data,omitempty"`
	Attrs          map[string]string `json:"attrs,omitempty"`
	Mentions       []string          `json:"mentions,omitempty"`     // agents (codex) and sessions (codex#2)
	FromSession    string            `json:"from_session,omitempty"` // the posting session's ID, if known
	FromHandle     string            `json:"from_handle,omitempty"`  // and its handle (codex#2)
	TraceID        string            `json:"trace_id"`
	ParentID       string            `json:"parent_id,omitempty"`
	Hop            int               `json:"hop"`
	Trust          Trust             `json:"trust"`
	// DeleteOnDelivery: the event's content (Subject, Body, Data) is
	// deleted once it's delivered (deliveries.RedactDelivered), as its
	// app's event type asks (apps.EventType.DeleteOnDelivery).
	DeleteOnDelivery bool `json:"-"`
	// Redacted: its content was deleted after delivery; what's left is
	// metadata. Never delivered again.
	Redacted bool `json:"redacted,omitempty"`
}

// ErrRedacted is asking for an event's content after it was deleted
// (DeleteOnDelivery): it was delivered, and isn't available again.
var ErrRedacted = errors.New("events: this event's content was deleted once it was delivered")

// Party is who an event is from: a source user, an agent, an app, the hub.
type Party struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

// Trust says whether an event's content comes from outside the user's agents.
type Trust string

const (
	External Trust = "external" // e.g. a Slack message from anyone in a channel
	Internal Trust = "internal" // the user's own agents, or the hub
)

// Append adds e to the log and returns it as stored. If e has a
// SourceEventID and that (source, ID, revision) is already stored, Append
// returns the stored event and inserted is false: retried webhooks are kept
// once. TraceID defaults to a new trace starting at this event. ID is new
// unless set (the copies of one publish in several workspaces share one).
//
// Append holds the tenant's sequence lock until tx ends; keep tx short.
func Append(ctx context.Context, tx pgx.Tx, e Event) (stored Event, inserted bool, err error) {
	if e.Type == "" || e.Source == "" || e.From.Kind == "" || e.From.ID == "" {
		return Event{}, false, errors.New("events: type, source and from are required")
	}
	if e.Trust != External && e.Trust != Internal {
		return Event{}, false, fmt.Errorf("events: trust must be %q or %q", External, Internal)
	}
	if e.OccurredAt.IsZero() {
		e.OccurredAt = time.Now()
	}
	if e.Attrs == nil {
		e.Attrs = map[string]string{}
	}
	if e.Mentions == nil {
		e.Mentions = []string{}
	}
	var sourceEventID, traceID, parentID *string
	if e.SourceEventID != "" {
		sourceEventID = &e.SourceEventID
		// A stored duplicate needs no sequence number; check before taking
		// the lock.
		if dup, err := getBySource(ctx, tx, e.Source, e.SourceEventID, e.SourceRevision); err == nil {
			return dup, false, nil
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return Event{}, false, err
		}
	}
	if e.TraceID != "" {
		traceID = &e.TraceID
	}
	if e.ParentID != "" {
		parentID = &e.ParentID
	}
	var data []byte
	if len(e.Data) > 0 {
		data = e.Data
	}
	row := tx.QueryRow(ctx, `
		WITH next AS (
			INSERT INTO event_counters (tenant_id, last_seq) VALUES (current_tenant(), 1)
			ON CONFLICT (tenant_id) DO UPDATE SET last_seq = event_counters.last_seq + 1
			RETURNING last_seq
		), ids AS (
			SELECT coalesce(nullif($19, '')::uuid, gen_random_uuid()) AS id
		)
		INSERT INTO events (tenant_id, seq, id, type, source, source_event_id, source_revision,
			occurred_at, from_kind, from_id, subject, body, data, attrs, mentions,
			trace_id, parent_id, hop, trust, from_session, from_handle, delete_on_delivery)
		SELECT current_tenant(), next.last_seq, ids.id, $1, $2, $3, $4,
			$5, $6, $7, $8, $9, $10, $11, $12,
			coalesce($13::uuid, ids.id), $14, $15, $16, nullif($17, '')::uuid, $18, $20
		FROM next, ids
		ON CONFLICT (tenant_id, source, source_event_id, source_revision) WHERE source_event_id IS NOT NULL DO NOTHING
		RETURNING `+columns,
		e.Type, e.Source, sourceEventID, e.SourceRevision,
		e.OccurredAt, e.From.Kind, e.From.ID, e.Subject, e.Body, data, e.Attrs, e.Mentions,
		traceID, parentID, e.Hop, e.Trust, e.FromSession, e.FromHandle, e.ID, e.DeleteOnDelivery)
	stored, err = scan(row)
	if errors.Is(err, pgx.ErrNoRows) && sourceEventID != nil {
		// Inserted concurrently by another transaction since our check.
		dup, err := getBySource(ctx, tx, e.Source, e.SourceEventID, e.SourceRevision)
		return dup, false, err
	}
	if err != nil {
		return Event{}, false, fmt.Errorf("events: append: %w", err)
	}
	// Wake the pipeline runner once this commits (a hint; it also polls),
	// and mark the workspace for its sweep (pipeline_due). Appends in a
	// workspace already wait for each other on the counter's row, so its
	// row here adds no contention.
	if _, err := tx.Exec(ctx, `SELECT pg_notify($1, current_tenant()::text)`, NotifyChannel); err != nil {
		return Event{}, false, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO pipeline_due (tenant_id, seq) VALUES (current_tenant(), $1)
		ON CONFLICT (tenant_id) DO UPDATE SET seq = GREATEST(pipeline_due.seq, excluded.seq)`, int64(stored.Seq)); err != nil {
		return Event{}, false, err
	}
	return stored, true, nil
}

// MarkDue marks the workspace for the pipeline's sweep (pipeline_due), if
// it isn't already: for work the sweep does that no new event marks (a
// delivery's lease, to reap if it runs out). It holds the workspace's mark
// lock (shared, so marks don't wait for each other) until tx ends: a sweep
// unmarking the workspace (HoldMarks) waits for tx, so it sees what tx
// leased rather than taking off the mark tx found already there.
func MarkDue(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, `WITH held AS (SELECT pg_advisory_xact_lock_shared($1, hashtext(current_tenant()::text)))
		INSERT INTO pipeline_due (tenant_id, seq)
		SELECT current_tenant(), coalesce((SELECT last_seq FROM event_counters WHERE tenant_id = current_tenant()), 0) FROM held
		ON CONFLICT (tenant_id) DO NOTHING`, markLock)
	return err
}

// HoldMarks waits for every transaction that marked the workspace due
// (MarkDue) to end, and holds off new marks until tx ends: a statement run
// after it sees what they did. For the sweep, before it unmarks the
// workspace.
func HoldMarks(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1, hashtext(current_tenant()::text))`, markLock)
	return err
}

// markLock is the first key of a workspace's mark lock (the second is a
// hash of its ID; MarkDue, HoldMarks). Two-key advisory locks never
// conflict with the one-key ones the migrator and sweeps take.
const markLock int32 = 0x77616b05

// NotifyChannel is the Postgres channel notified, with the tenant ID, when
// a transaction that appended events commits.
const NotifyChannel = "wakeshift_events"

// ReadAfter returns up to limit events after seq, oldest first, whose type
// matches one of patterns (all types if patterns is empty). Because events
// commit in seq order, the result never skips an event that appears later:
// a reader can advance its cursor to the last seq returned.
func ReadAfter(ctx context.Context, tx pgx.Tx, after uint64, patterns []string, limit int) ([]Event, error) {
	likes := make([]string, 0, len(patterns))
	for _, p := range patterns {
		likes = append(likes, likePattern(p))
	}
	rows, err := tx.Query(ctx, `SELECT `+columns+` FROM events
		WHERE seq > $1 AND (cardinality($2::text[]) = 0 OR type LIKE ANY($2))
		ORDER BY seq LIMIT $3`, after, likes, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Event, error) { return scan(r) })
}

// Get returns one event by ID.
func Get(ctx context.Context, tx pgx.Tx, id string) (Event, error) {
	return scan(tx.QueryRow(ctx, `SELECT `+columns+` FROM events WHERE id = $1`, id))
}

// LastSeq is the tenant's highest seq, or 0 with no events. A new
// subscription starts here so it doesn't receive history.
func LastSeq(ctx context.Context, tx pgx.Tx) (uint64, error) {
	var seq uint64
	err := tx.QueryRow(ctx, `SELECT coalesce(max(last_seq), 0) FROM event_counters`).Scan(&seq)
	return seq, err
}

func getBySource(ctx context.Context, tx pgx.Tx, source, id, revision string) (Event, error) {
	return scan(tx.QueryRow(ctx, `SELECT `+columns+` FROM events
		WHERE source = $1 AND source_event_id = $2 AND source_revision = $3`, source, id, revision))
}

const columns = `seq, id, type, source, coalesce(source_event_id, ''), source_revision,
	occurred_at, received_at, from_kind, from_id, subject, body, data, attrs, mentions,
	trace_id, coalesce(parent_id::text, ''), hop, trust, coalesce(from_session::text, ''), from_handle,
	delete_on_delivery, redacted_at IS NOT NULL`

func scan(row pgx.Row) (Event, error) {
	var e Event
	var data []byte
	err := row.Scan(&e.Seq, &e.ID, &e.Type, &e.Source, &e.SourceEventID, &e.SourceRevision,
		&e.OccurredAt, &e.ReceivedAt, &e.From.Kind, &e.From.ID, &e.Subject, &e.Body, &data, &e.Attrs, &e.Mentions,
		&e.TraceID, &e.ParentID, &e.Hop, &e.Trust, &e.FromSession, &e.FromHandle, &e.DeleteOnDelivery, &e.Redacted)
	if len(data) > 0 {
		e.Data = data
	}
	return e, err
}

// MatchType reports whether an event type matches a pattern: an exact type,
// or a prefix ending in ".*" ("app.agentchat.channel.*").
func MatchType(pattern, typ string) bool {
	if prefix, ok := strings.CutSuffix(pattern, "*"); ok {
		return strings.HasPrefix(typ, prefix)
	}
	return pattern == typ
}

// likePattern turns a type pattern into a LIKE pattern with the same meaning.
func likePattern(p string) string {
	esc := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	if prefix, ok := strings.CutSuffix(p, "*"); ok {
		return esc.Replace(prefix) + "%"
	}
	return esc.Replace(p)
}

// Recent returns up to limit events, newest first, before seq (or the newest
// events if before is 0). For browsing the log; consumers use ReadAfter.
func Recent(ctx context.Context, tx pgx.Tx, before uint64, limit int) ([]Event, error) {
	rows, err := tx.Query(ctx, `SELECT `+columns+` FROM events
		WHERE $1 = 0 OR seq < $1 ORDER BY seq DESC LIMIT $2`, before, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Event, error) { return scan(r) })
}
