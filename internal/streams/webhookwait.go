package streams

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/deliveries"
)

// A Grok Bot waits in the foreground on its session's one-shot wait until
// its Automations webhook is bound; from then on the webhook delivers, so
// a running wait would race it for the same messages. A wait on a session
// with a bound, live Cursor Automations destination ends with hub.closed
// "webhook" (WebhookNote), whether it was running when the webhook was
// bound (EndWaitForWebhook) or started after. One whose destination died,
// or needs setup, waits as before: the foreground wait is the fallback
// again. With the webhook wake off (Server.CursorWebhooks), no wait ends
// for a webhook: the deliverer skips Cursor destinations, so their
// messages wait for the foreground wait, and none is stranded.

// WebhookNote is a wait's last line once the conversation's webhook
// delivers its messages.
const WebhookNote = "Wake-ups now come through this bot's webhook; don't run this command again."

// errWebhookBound: a wait found its session's webhook bound.
var errWebhookBound = errors.New("streams: the session's webhook is bound")

// webhookBound reports whether a session (target) has a bound, live Cursor
// Automations destination, in the transaction's tenant.
func webhookBound(ctx context.Context, tx pgx.Tx, target string) (bool, error) {
	if !uuidRE.MatchString(target) {
		return false, nil
	}
	var bound bool
	err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM agent_webhooks WHERE session_id = $1::uuid AND kind = 'cursor-automations'
		AND state = 'live' AND needs_setup_at IS NULL)`, target).Scan(&bound)
	return bound, err
}

// EndWaitForWebhook, in the transaction's tenant, stops a wait holding the
// session's stream (agent, target) and hands back what it hasn't
// confirmed: the session's webhook was just bound. The wait ends with
// WebhookNote once the transaction commits.
func EndWaitForWebhook(ctx context.Context, tx pgx.Tx, agent, target string) error {
	var holder string
	err := tx.QueryRow(ctx, `UPDATE stream_leases SET renewed_at = NULL WHERE agent_id = $1 AND target = $2 AND holder LIKE 'poll\_wait\_%' RETURNING holder`,
		agent, target).Scan(&holder)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := deliveries.Release(ctx, tx, holder); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `SELECT pg_notify($1, $2)`, TakeoverChannel, holder)
	return err
}

// LinkLive is a stored link in the transaction's tenant that hasn't been
// revoked (nor its grant ended, nor its user left), whatever its sliding
// expiry: a capability derived from it (a webhook setup link) works only
// while it does.
func LinkLive(ctx context.Context, tx pgx.Tx, id string) (Link, error) {
	return loadTx(ctx, tx, id, true)
}

// ErrLinkInvalid is LinkLive's (and UsableLink's) answer for a link that no
// longer works.
var ErrLinkInvalid = errLinkInvalid
