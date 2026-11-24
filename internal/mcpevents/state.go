package mcpevents

import (
	"context"
	"errors"
	"log/slog"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/deliveries"
	"github.com/wakeshift/server/internal/events"
	"github.com/wakeshift/server/internal/grants"
)

// A webhook's delivery state, as the Deliverer reads and writes it
// (deliver.go is the sending): access checks, what's to be replayed, each
// attempt's outcome, and the hold one instance has on a webhook. Writes
// that follow an attempt use a context that isn't canceled, so an attempt
// that happened is always recorded.

// authorized checks a webhook's access: the app's grant for the agent is
// live (every pass), and, when recheck is set (at least every
// ReauthEvery), that the session is still the agent's. It returns why
// access ended, or "".
func authorized(ctx context.Context, tx pgx.Tx, app, agent, session string, recheck bool) (string, error) {
	if _, err := grants.Live(ctx, tx, app, agent); errors.Is(err, grants.ErrNotFound) {
		return "Access revoked", nil
	} else if err != nil {
		return "", err
	}
	if !recheck {
		return "", nil
	}
	var owner string
	err := tx.QueryRow(ctx, `SELECT agent_id FROM sessions WHERE id = $1`, session).Scan(&owner)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && owner != agent {
		return "The conversation is gone", nil
	}
	return "", err
}

// offered checks that the app's manifest still lists the event type a
// webhook is for, returning ReasonEventRemoved if it doesn't. A webhook
// with no event name (made before event types) is left alone.
func offered(ctx context.Context, tx pgx.Tx, app, name string) (string, error) {
	if name == "" {
		return "", nil
	}
	var ok bool
	err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM apps a, jsonb_array_elements(CASE WHEN jsonb_typeof(a.manifest->'events') = 'array'
		THEN a.manifest->'events' ELSE '[]'::jsonb END) e WHERE a.id = $1 AND e->>'name' = $2)`, app, name).Scan(&ok)
	if err != nil || ok {
		return "", err
	}
	return ReasonEventRemoved, nil
}

// replayable is what's to be replayed next: the conversation's acknowledged
// or abandoned deliveries in (after, through], in order. One whose event's
// content was deleted once delivered (events.Event.DeleteOnDelivery) can't
// be: it's skipped, and the webhook's next refresh says truncated.
func replayable(ctx context.Context, tx pgx.Tx, webhook, app, agent, session string, after, through int64) ([]deliveries.Delivery, error) {
	type row struct {
		ID, EventID string
		Seq         int64
	}
	var out []deliveries.Delivery
	skipped := false
	for {
		rows, _ := tx.Query(ctx, `SELECT id::text, event_id::text, event_seq FROM deliveries WHERE agent_id = $1 AND target = $2
			AND state IN ('acked', 'dead') AND event_seq > $3 AND event_seq <= $4 AND `+deliveries.OfApp("deliveries", "$6")+`
			ORDER BY event_seq LIMIT $5`, agent, session, after, through, perPass, app)
		list, err := pgx.CollectRows(rows, pgx.RowToStructByPos[row])
		if err != nil {
			return nil, err
		}
		for _, r := range list {
			e, err := events.Get(ctx, tx, r.EventID)
			if err != nil {
				return nil, err
			}
			if e.Redacted {
				skipped = true
				continue
			}
			out = append(out, deliveries.Delivery{ID: r.ID, AgentID: agent, Target: session, State: "acked", Event: e})
		}
		// Read on past what was skipped until a pass's worth is found or the
		// range ends, so an empty result means the range is done. What's
		// left goes on the next pass, from what this one sent (record moves
		// replay_after), which notes what it skips there.
		if len(out) >= perPass || len(list) < perPass {
			break
		}
		after = list[len(list)-1].Seq
	}
	if len(out) > perPass {
		out = out[:perPass]
	}
	if skipped {
		if _, err := tx.Exec(ctx, `UPDATE mcp_event_webhooks SET truncated = true WHERE id = $1`, webhook); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// record writes one attempt's outcome: the delivery's state (or, for a
// replay, how far the replay got) and the webhook's. The webhook is locked
// first, as claiming and unsubscribing lock it before its deliveries: the
// other way round, recording and an unsubscribe deadlock.
func (d *Deliverer) record(ctx context.Context, tx pgx.Tx, c claimed, dl deliveries.Delivery, o outcome, gap bool, why, category string, status int) error {
	_, err := tx.Exec(ctx, `SELECT 1 FROM mcp_event_webhooks WHERE id = $1 FOR NO KEY UPDATE`, c.id)
	if err != nil {
		return err
	}
	switch {
	case c.replay && o != retry:
		_, err = tx.Exec(ctx, `UPDATE mcp_event_webhooks SET replay_after = $2,
			replay_through = CASE WHEN $2 >= replay_through THEN NULL ELSE replay_through END WHERE id = $1`, c.id, int64(dl.Event.Seq))
		if err == nil {
			_, err = tx.Exec(ctx, `UPDATE mcp_event_webhooks SET replay_after = NULL WHERE id = $1 AND replay_through IS NULL`, c.id)
		}
	case c.replay:
	case o == acked:
		_, err = deliveries.Ack(ctx, tx, c.agent, dl.Target, dl.ID)
	case o == retry:
		err = deliveries.RetryAt(ctx, tx, d.holder, dl.ID, d.now().Add(Backoff(c.attempts+1)), why)
	default:
		err = deliveries.GiveUp(ctx, tx, d.holder, dl.ID, why)
	}
	if err != nil {
		return err
	}
	switch o {
	case acked:
		_, err = tx.Exec(ctx, `UPDATE mcp_event_webhooks SET delivered = delivered + 1, last_delivery_at = now(), last_status = $2, last_error = NULL,
			retry_attempts = 0, retrying_since = NULL, retry_delivery_id = NULL, next_attempt_at = NULL WHERE id = $1`, c.id, status)
	case retry:
		// For this delivery: c's count and start, reset by claimOne if the
		// webhook's were another delivery's.
		_, err = tx.Exec(ctx, `UPDATE mcp_event_webhooks SET last_status = nullif($2, 0), last_error = nullif($3, ''),
			retry_attempts = $5, retrying_since = coalesce($6, now()), retry_delivery_id = $7::uuid, next_attempt_at = $4 WHERE id = $1`,
			c.id, status, category, d.now().Add(Backoff(c.attempts+1)), c.attempts+1, c.since, dl.ID)
	default:
		_, err = tx.Exec(ctx, `UPDATE mcp_event_webhooks SET last_status = nullif($2, 0), last_error = nullif($3, ''),
			retry_attempts = 0, retrying_since = NULL, retry_delivery_id = NULL, next_attempt_at = NULL, truncated = truncated OR $4 WHERE id = $1`, c.id, status, category, gap)
	}
	return err
}

// free lets other passes take the webhook again.
func (d *Deliverer) free(ctx context.Context, c claimed) {
	bg := context.WithoutCancel(ctx)
	err := d.DB.InTenant(bg, c.tenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(bg, `UPDATE mcp_event_webhooks SET busy_holder = NULL, busy_until = NULL WHERE id = $1 AND busy_holder = $2`, c.id, d.holder)
		return err
	})
	if err != nil {
		slog.Warn("mcp events: freeing a webhook failed", "tenant", c.tenant, "webhook", c.id, "err", err)
	}
}

// release returns claimed deliveries to pending without counting the
// attempt: the ones after a failure (whose retry or death is already
// recorded), or, when recording failed, that one too.
func (d *Deliverer) release(ctx context.Context, c claimed, rest []deliveries.Delivery) {
	ids := make([]string, 0, len(rest))
	for _, dl := range rest {
		ids = append(ids, dl.ID)
	}
	bg := context.WithoutCancel(ctx)
	err := d.DB.InTenant(bg, c.tenant, func(tx pgx.Tx) error {
		return deliveries.Unclaim(bg, tx, d.holder, ids)
	})
	if err != nil {
		slog.Warn("mcp events: releasing deliveries failed", "tenant", c.tenant, "webhook", c.id, "err", err)
	}
}
