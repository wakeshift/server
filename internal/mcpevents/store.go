package mcpevents

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/deliveries"
	"github.com/wakeshift/server/internal/mcpevents/event"
)

// Sealer encrypts secrets at rest (MCP Events webhooks' and publish URLs'
// signing secrets), each bound to its row's ID.
type Sealer struct{ aead cipher.AEAD }

// NewSealer makes a Sealer from a 32-byte key.
func NewSealer(key []byte) (*Sealer, error) {
	if len(key) < 32 {
		return nil, errors.New("mcpevents: the key must be 32 bytes")
	}
	block, err := aes.NewCipher(key[:32])
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Sealer{aead: aead}, nil
}

func (s *Sealer) seal(id string, secret []byte) []byte { return s.Seal("mcp-events:"+id, secret) }

func (s *Sealer) open(id string, sealed []byte) ([]byte, error) {
	return s.Open("mcp-events:"+id, sealed)
}

// Seal encrypts secret, bound to what it's for (a purpose and the row's
// ID): Open with anything else fails. Each use of a Sealer's key keeps its
// own binding prefix.
func (s *Sealer) Seal(binding string, secret []byte) []byte {
	nonce := make([]byte, s.aead.NonceSize())
	rand.Read(nonce)
	return s.aead.Seal(nonce, nonce, secret, []byte(binding))
}

// Open decrypts what Seal sealed with the same binding.
func (s *Sealer) Open(binding string, sealed []byte) ([]byte, error) {
	n := s.aead.NonceSize()
	if len(sealed) < n {
		return nil, errors.New("mcpevents: sealed secret too short")
	}
	b, err := s.aead.Open(nil, sealed[:n], sealed[n:], []byte(binding))
	if err != nil {
		return nil, errors.New("mcpevents: can't decrypt a secret (was WAKESHIFT_SECRET_KEY changed?)")
	}
	return b, nil
}

// Webhook is a session's subscription, as subscribe makes it.
type Webhook struct {
	ID            string
	App, Agent    string
	Session       string // its ID
	SessionHandle string
	Name, URL     string
	Secret        []byte
	TTL           time.Duration
	Dialect       string // a webhook dialect (event.WebhookDialect): fixed when it is made
	// Cursor is the client's last persisted cursor (nil: start from now).
	// It's a replay point only when the subscription is new or had lapsed;
	// on a live one it changes nothing.
	Cursor *string
	// MaxAge bounds a replay: nothing older than now - MaxAge (0: no bound).
	MaxAge time.Duration
}

// DeliveryStatus is how a refreshed subscription's deliveries are going
// (deliveryStatus). Wakeshift doesn't suspend delivery (it abandons a
// delivery after RetryWindow instead), so Active is always true.
type DeliveryStatus struct {
	Active         bool       `json:"active"`
	LastDeliveryAt *time.Time `json:"lastDeliveryAt"`
	LastError      *string    `json:"lastError"`
	FailedSince    *time.Time `json:"failedSince,omitempty"`
}

// Subscribed is what a subscribe did.
type Subscribed struct {
	ExpiresAt time.Time
	Created   bool // a new subscription (otherwise a refresh)
	Cursor    string
	Truncated bool            // events were skipped: the cursor was too old, maxAgeMs cut it, or a delivery was abandoned
	Status    *DeliveryStatus // on a refresh
}

// Exists reports whether the transaction's workspace has a webhook with
// this ID that hasn't expired, ended or finished.
func Exists(ctx context.Context, tx pgx.Tx, id string) (bool, error) {
	var ok bool
	err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM mcp_event_webhooks WHERE id = $1 AND expires_at > now() AND ending_reason IS NULL
		AND finished_at IS NULL)`, id).Scan(&ok)
	return ok, err
}

// Subscribe creates or refreshes w in the transaction's workspace. A
// session has one webhook: a new one replaces the others. A refresh with a
// new secret keeps the old one signing for RotationWindow. A webhook that
// expired is made again, so its cursor is a replay point.
func Subscribe(ctx context.Context, tx pgx.Tx, s *Sealer, w Webhook, now time.Time) (Subscribed, error) {
	out := Subscribed{ExpiresAt: now.Add(w.TTL).UTC().Truncate(time.Second)}
	if w.Dialect == "" {
		w.Dialect = event.DialectDraft
	}
	var replayFrom *int64
	if w.Cursor != nil {
		c, err := ParseCursor(*w.Cursor)
		if err != nil {
			return out, err
		}
		replayFrom = &c
	}
	// Replaced (another webhook of the session), expired or ended: gone,
	// so this one starts afresh. So is a finished one with this ID (Finish:
	// its subscription made its one delivery, and this is a new one, in a
	// session of its own). The webhooks go before their targets, in two
	// statements, as everywhere (a delivery pass locks a webhook, then may
	// end it), so the two can't deadlock.
	rows, _ := tx.Query(ctx, `DELETE FROM mcp_event_webhooks WHERE session_id = $1 AND (id <> $2 OR expires_at <= $3 OR ending_reason IS NOT NULL)
		OR id = $2 AND finished_at IS NOT NULL RETURNING id`,
		w.Session, w.ID, now)
	gone, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return out, err
	}
	if len(gone) > 0 {
		if _, err := tx.Exec(ctx, `DELETE FROM mcp_event_targets WHERE tenant_id = current_tenant() AND webhook_id = ANY($1)`, gone); err != nil {
			return out, err
		}
	}
	var (
		old         []byte
		lastAt, ret *time.Time
		lastErr     *string
		truncated   bool
	)
	err = tx.QueryRow(ctx, `SELECT secret, last_delivery_at, last_error, retrying_since, truncated FROM mcp_event_webhooks WHERE id = $1 FOR UPDATE`, w.ID).
		Scan(&old, &lastAt, &lastErr, &ret, &truncated)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		out.Created = true
		// At most MaxWebhooks live for the app in the workspace (its
		// user's), counted under a lock so two at once can't both pass. A
		// finished one (its one delivery made, maybe not yet sent) isn't.
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('mcp_event_webhooks:' || current_tenant()::text || ':' || $1, 0))`,
			w.App); err != nil {
			return out, err
		}
		var live int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM mcp_event_webhooks WHERE app_id = $1 AND expires_at > $2 AND ending_reason IS NULL AND finished_at IS NULL`,
			w.App, now).Scan(&live); err != nil {
			return out, err
		}
		if live >= MaxWebhooks {
			return out, ErrTooManyWebhooks
		}
		if _, err := tx.Exec(ctx, `INSERT INTO mcp_event_webhooks (tenant_id, id, app_id, agent_id, session_id, session_handle, name, url, secret, expires_at, dialect, authorized_at)
			VALUES (current_tenant(), $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
			w.ID, w.App, w.Agent, w.Session, w.SessionHandle, w.Name, w.URL, s.seal(w.ID, w.Secret), out.ExpiresAt, w.Dialect, now); err != nil {
			return out, err
		}
		if replayFrom != nil {
			if out.Truncated, err = setupReplay(ctx, tx, w, *replayFrom, now); err != nil {
				return out, err
			}
		}
	case err == nil:
		// A refresh: the cursor changes nothing (delivery goes on), and
		// reports what happened since the last one.
		out.Truncated = truncated
		out.Status = &DeliveryStatus{Active: true, LastDeliveryAt: lastAt, LastError: lastErr, FailedSince: ret}
		// The dialect stays what the subscription was made with: the
		// envelopes its receiver takes don't change with whoever refreshes.
		prev, err := s.open(w.ID, old)
		set := `expires_at = $2, refreshed_at = now(), session_handle = $3, authorized_at = $4, truncated = false`
		args := []any{w.ID, out.ExpiresAt, w.SessionHandle, now}
		switch {
		case err != nil: // unreadable (the key changed): the new one only
			_, err = tx.Exec(ctx, `UPDATE mcp_event_webhooks SET secret = $5, previous_secret = NULL, previous_until = NULL, `+set+` WHERE id = $1`,
				append(args, s.seal(w.ID, w.Secret))...)
		case bytes.Equal(prev, w.Secret):
			_, err = tx.Exec(ctx, `UPDATE mcp_event_webhooks SET `+set+` WHERE id = $1`, args...)
		default:
			_, err = tx.Exec(ctx, `UPDATE mcp_event_webhooks SET secret = $5, previous_secret = secret, previous_until = $6, `+set+` WHERE id = $1`,
				append(args, s.seal(w.ID, w.Secret), now.Add(RotationWindow))...)
		}
		if err != nil {
			return out, err
		}
	default:
		return out, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO mcp_event_targets (tenant_id, webhook_id, expires_at) VALUES (current_tenant(), $1, $2)
		ON CONFLICT (tenant_id, webhook_id) DO UPDATE SET expires_at = excluded.expires_at`, w.ID, out.ExpiresAt); err != nil {
		return out, err
	}
	wm, err := Watermark(ctx, tx, w.ID)
	out.Cursor = Cursor(wm)
	return out, err
}

// setupReplay makes a new webhook replay what its conversation had
// acknowledged or abandoned after the client's cursor: at most MaxReplay
// deliveries, none older than w.MaxAge. It reports whether anything was
// skipped: beyond those limits, or an event whose content was deleted
// once delivered (events.Event.DeleteOnDelivery). Waiting deliveries
// aren't touched, whatever their age: they're messages the conversation
// never got, which it gets in any case (its next tool results read them
// too), so maxAgeMs bounds only what's sent again.
func setupReplay(ctx context.Context, tx pgx.Tx, w Webhook, from int64, now time.Time) (bool, error) {
	var floor *time.Time
	if w.MaxAge > 0 {
		f := now.Add(-w.MaxAge)
		floor = &f
	}
	truncated := false
	var through int64
	var total int
	if err := tx.QueryRow(ctx, `SELECT coalesce(max(event_seq), 0), count(*) FILTER (WHERE event_seq > $3) FROM deliveries
		WHERE agent_id = $1 AND target = $2 AND state IN ('acked', 'dead') AND `+deliveries.OfApp("deliveries", "$4"),
		w.Agent, w.Session, from, w.App).Scan(&through, &total); err != nil {
		return false, err
	}
	if through <= from {
		return truncated, nil
	}
	// The newest MaxReplay eligible (the event itself newer than the floor:
	// its timestamp, as the occurrence carries it), counted and their first
	// found in the database: only MaxReplay rows are read.
	var first *int64
	var kept int
	if err := tx.QueryRow(ctx, `SELECT min(event_seq), count(*) FROM (SELECT d.event_seq FROM deliveries d
		JOIN events e ON e.tenant_id = d.tenant_id AND e.id = d.event_id
		WHERE d.agent_id = $1 AND d.target = $2 AND d.state IN ('acked', 'dead') AND d.event_seq > $3
		AND ($4::timestamptz IS NULL OR e.occurred_at >= $4) AND e.redacted_at IS NULL AND `+deliveries.OfApp("d", "$6")+`
		ORDER BY d.event_seq DESC LIMIT $5) newest`, w.Agent, w.Session, from, floor, MaxReplay, w.App).Scan(&first, &kept); err != nil {
		return false, err
	}
	if kept < total {
		truncated = true
	}
	if first == nil {
		return truncated, nil
	}
	if _, err := tx.Exec(ctx, `UPDATE mcp_event_webhooks SET replay_after = $2, replay_through = $3 WHERE id = $1`, w.ID, *first-1, through); err != nil {
		return truncated, err
	}
	// A replay is a new message: what's replayed gets a new webhook-id (in
	// the spec's dialect), so a receiver that deduplicates by it takes it.
	// These are this webhook's alone (its session's, which has one webhook)
	// and already acknowledged or abandoned, so no other send is retrying
	// them.
	_, err := tx.Exec(ctx, `UPDATE deliveries SET message_id = NULL WHERE agent_id = $1 AND target = $2 AND state IN ('acked', 'dead')
		AND event_seq > $3 AND event_seq <= $4 AND message_id IS NOT NULL AND `+deliveries.OfApp("deliveries", "$5"),
		w.Agent, w.Session, *first-1, through, w.App)
	return truncated, err
}

// Watermark is a webhook's safe-to-persist position: every delivery of its
// conversation at or before it is acknowledged or abandoned, and nothing
// at or before it is still to be replayed. Every subscribe and refresh
// reads it, so both reads go by index to the conversation's own deliveries
// (deliveries_queue, deliveries_target_seq), never through the rest of the
// agent's.
//
// The webhook's session ID is cast to text once, in w, not in each
// subquery: under row-level security, Postgres 17.12 and later won't use
// `target = w.session_id::text` (an I/O cast of an outer column, so not
// leakproof) as an index condition, and read all the agent's deliveries
// instead. OFFSET 0 keeps the planner from folding the cast back in.
func Watermark(ctx context.Context, tx pgx.Tx, id string) (int64, error) {
	var wm int64
	err := tx.QueryRow(ctx, watermarkSQL, id).Scan(&wm)
	return wm, err
}

var watermarkSQL = `SELECT least(
			coalesce((SELECT min(d.event_seq) - 1 FROM deliveries d WHERE d.agent_id = w.agent_id AND d.target = w.target AND d.state IN ('pending', 'in_flight')
					AND ` + deliveries.OfApp("d", "w.app_id") + `),
				(SELECT max(d.event_seq) FROM deliveries d WHERE d.agent_id = w.agent_id AND d.target = w.target AND ` + deliveries.OfApp("d", "w.app_id") + `), 0),
			CASE WHEN w.replay_after < w.replay_through THEN w.replay_after END)
		FROM (SELECT agent_id, app_id, session_id::text AS target, replay_after, replay_through
			FROM mcp_event_webhooks WHERE id = $1 OFFSET 0) w`

// Terminate ends a webhook because access to it ended (revoked in the
// console): nothing more is delivered, and the deliverer's next pass sends
// its receiver a terminated envelope with reason, then removes it.
func Terminate(ctx context.Context, tx pgx.Tx, id, reason string) error {
	_, err := tx.Exec(ctx, `UPDATE mcp_event_webhooks SET ending_reason = $2 WHERE id = $1 AND ending_reason IS NULL`, id, reason)
	return err
}

// Finish marks a webhook finished: its "once" subscription made its one
// delivery. Nothing more is routed to it, so it no longer counts toward
// MaxWebhooks, but it isn't removed: the deliverer sends what still waits
// for it (that delivery), then removes it.
func Finish(ctx context.Context, tx pgx.Tx, id string) error {
	_, err := tx.Exec(ctx, `UPDATE mcp_event_webhooks SET finished_at = now() WHERE id = $1 AND finished_at IS NULL`, id)
	return err
}

// Unsubscribe ends a webhook in the transaction's workspace, reporting
// whether there was one. The webhook goes before its target (as in
// Subscribe), so this can't deadlock with a Subscribe replacing it.
func Unsubscribe(ctx context.Context, tx pgx.Tx, id string) (bool, error) {
	tag, err := tx.Exec(ctx, `DELETE FROM mcp_event_webhooks WHERE id = $1`, id)
	if err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM mcp_event_targets WHERE tenant_id = current_tenant() AND webhook_id = $1`, id); err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}
