package deliveries

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// For consumers that push rather than hold a stream (MCP Events webhooks,
// internal/mcpevents): a delivery the holder couldn't hand over is retried
// after a backoff, or given up on.

// RetryAt returns a leased delivery to pending, available again at at,
// noting why it failed. Only the lease holder can.
func RetryAt(ctx context.Context, tx pgx.Tx, holder, id string, at time.Time, why string) error {
	_, err := tx.Exec(ctx, `UPDATE deliveries SET state = 'pending', available_at = $3, lease_until = NULL, last_error = $4
		WHERE id = $1::uuid AND lease_holder = $2 AND state = 'in_flight'`, id, holder, at, why)
	return err
}

// GiveUp marks a leased delivery dead (the consumer refused it for good, or
// it ran out of attempts), noting why. Only the lease holder can.
func GiveUp(ctx context.Context, tx pgx.Tx, holder, id, why string) error {
	_, err := tx.Exec(ctx, `UPDATE deliveries SET state = 'dead', finished_at = now(), lease_until = NULL, last_error = $3
		WHERE id = $1::uuid AND lease_holder = $2 AND state = 'in_flight'`, id, holder, why)
	return err
}
