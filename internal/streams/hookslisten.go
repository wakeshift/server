package streams

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// When Wakeshift's hooks count as delivering a session's messages (#1272):
// they connected it (a connector redeemed for it: runtime 'stream',
// connector_seen_at), and one of the session's connector links was polled
// within HooksListenWindow. A redeem alone doesn't count: a Claude Code
// process that redeems and can't be woken (claude -p, a one-shot Agent SDK
// query) never polls, and counting it shut the fallback (the Monitor, the
// MCP wait task) out of sessions nothing else would wake. used_at alone, not
// the link's created_at: the redeem creates the link.
//
// Checked in four places that must agree: a wait's or MCP wait task's
// until=hooks (longPoll, hooksConnected), the session-setup URL, and Claude
// Code's subscribing result (internal/appapi).
//
// A waiter polls at least every 25 seconds, even while it yields to a live
// Monitor, so the window is about four missed polls. used_at can't tell a
// waiter's poll from `wakeshift wait` on the same link; both listen on the
// hooks' link, so either counts.

// HooksListenWindow is how recently a session's connector link must have
// been polled for its hooks to count as delivering.
const HooksListenWindow = "2 minutes"

// hooksDeliverSQL is that condition on a sessions row aliased s. Only the
// session's own links: same agent, the session's ID as their target, in
// the transaction's tenant (row-level security), so no other session's
// poll can count for it. Links not revoked (the partial index on target);
// a renewal's successor is polled within a second of the renewal.
const hooksDeliverSQL = `s.runtime = 'stream' AND s.connector_seen_at IS NOT NULL
	AND EXISTS (SELECT 1 FROM stream_links k WHERE k.tenant_id = s.tenant_id AND k.agent_id = s.agent_id AND k.target = s.id::text AND k.connector IS NOT NULL
		AND k.revoked_at IS NULL AND k.used_at > now() - interval '` + HooksListenWindow + `')`

// HooksDeliver reports, in the transaction's tenant, whether Wakeshift's
// hooks deliver the session's messages (hooksDeliverSQL).
func HooksDeliver(ctx context.Context, tx pgx.Tx, session string) (bool, error) {
	if !uuidRE.MatchString(session) {
		return false, nil
	}
	var hooks bool
	err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM sessions s WHERE s.id = $1::uuid AND `+hooksDeliverSQL+`)`, session).Scan(&hooks)
	return hooks, err
}
