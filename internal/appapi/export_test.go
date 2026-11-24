package appapi

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/apps"
)

// The statements CancelStranded runs (all, and recent only), for tests
// that read their plans.
var CancelStrandedSQL, CancelStrandedRecentSQL = cancelStrandedSQL, cancelStrandedRecentSQL

// Replay is a subscribe's replay of the topic's recent publishes, for tests.
func Replay(ctx context.Context, tx pgx.Tx, app, topic, subID string, before time.Time, window time.Duration) error {
	return (&API{}).replay(ctx, tx, apps.App{ID: app}, topic, subID, nil, before, window)
}
