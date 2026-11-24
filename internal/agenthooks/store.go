package agenthooks

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/wakeshift/server/internal/mcpevents"
)

// States of a destination.
const (
	Live = "live"
	Dead = "dead" // gone (404, 410) or filtering everything out (no-match): registering again revives it
)

// Dead reasons.
const (
	ReasonNotFound   = "not_found"  // 404: the watch is unknown or disabled (expired, stopped)
	ReasonGone       = "gone"       // 410
	ReasonNoMatch    = "no_match"   // 202 no-match: the watch's conditions filtered the message out
	ReasonUnreadable = "unreadable" // its sealed URL can't be opened (the key changed)
)

// Destination is a session's registered destination, without its URL.
type Destination struct {
	Session, Agent, Kind string
	App, Grant           string
	State, DeadReason    string
	RegisteredAt         time.Time
	DeadAt               *time.Time
	// Cursor Automations destinations (KindCursor) only.
	Bot         string
	Fingerprint []byte
	Joined      bool
	NeedsSetup  bool
	DeadTold    bool
	// Routine is the destination's routine (SetupLinks.Routine), written
	// to the index by RegisterCursor.
	Routine []byte
}

// Get is the session's destination in the transaction's workspace, if it
// has one.
func Get(ctx context.Context, tx pgx.Tx, session string) (Destination, bool, error) {
	var d Destination
	err := tx.QueryRow(ctx, `SELECT session_id::text, agent_id, kind, app_id, coalesce(grant_id::text, ''), state, coalesce(dead_reason, ''), registered_at, dead_at,
			coalesce(bot_id, ''), key_fingerprint, joined, needs_setup_at IS NOT NULL, dead_told_at IS NOT NULL
		FROM agent_webhooks WHERE session_id = $1`, session).
		Scan(&d.Session, &d.Agent, &d.Kind, &d.App, &d.Grant, &d.State, &d.DeadReason, &d.RegisteredAt, &d.DeadAt,
			&d.Bot, &d.Fingerprint, &d.Joined, &d.NeedsSetup, &d.DeadTold)
	if errors.Is(err, pgx.ErrNoRows) {
		return d, false, nil
	}
	return d, err == nil, err
}

// binding is what a destination's sealed URL is bound to: Open with any
// other workspace or session fails.
func binding(tenant, session string) string { return "agent-webhook:" + tenant + ":" + session }

// Register sets the session's destination to url (checked with CheckURL
// first) in the transaction's workspace (tenant), replacing any it had,
// live or dead: a new registration starts afresh. It reports whether it
// replaced one. The caller records the audit entry.
func Register(ctx context.Context, tx pgx.Tx, seal *mcpevents.Sealer, tenant string, d Destination, url string) (replaced bool, err error) {
	if err := CheckURL(url); err != nil {
		return false, err
	}
	var grant *string
	if d.Grant != "" {
		grant = &d.Grant
	}
	err = tx.QueryRow(ctx, `INSERT INTO agent_webhooks (tenant_id, session_id, agent_id, kind, url, app_id, grant_id)
		VALUES (current_tenant(), $1, $2, $3, $4, $5, $6)
		ON CONFLICT (tenant_id, session_id) DO UPDATE SET agent_id = excluded.agent_id, kind = excluded.kind, url = excluded.url,
			app_id = excluded.app_id, grant_id = excluded.grant_id, state = 'live', dead_reason = NULL, dead_at = NULL, registered_at = now(),
			failures = 0, next_attempt_at = NULL, last_status = NULL, last_error = NULL
		RETURNING xmax <> 0`, d.Session, d.Agent, d.Kind, seal.Seal(binding(tenant, d.Session), []byte(url)), d.App, grant).Scan(&replaced)
	if err != nil {
		return false, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO agent_webhook_targets (tenant_id, session_id) VALUES (current_tenant(), $1) ON CONFLICT DO NOTHING`, d.Session)
	return replaced, err
}

// RegisterCursor sets the session's destination to a Cursor Automations
// webhook (h, checked first) in the transaction's workspace (tenant),
// replacing any it had, for the bot (its ID and key's fingerprint), with
// its binding (BindingID), and records it in the bot key's index
// (bot_webhooks). joined: copied from another app's destination by a
// proof; announce: to be announced through the webhook (join_due). The
// caller records the audit entry.
func RegisterCursor(ctx context.Context, tx pgx.Tx, seal *mcpevents.Sealer, tenant string, d Destination, h CursorHook, bindingID []byte, joined, announce bool) (replaced bool, err error) {
	if err := h.Check(); err != nil {
		return false, err
	}
	var grant *string
	if d.Grant != "" {
		grant = &d.Grant
	}
	err = tx.QueryRow(ctx, `INSERT INTO agent_webhooks (tenant_id, session_id, agent_id, kind, url, app_id, grant_id, bot_id, key_fingerprint, joined, join_due, removal_hash)
		VALUES (current_tenant(), $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		ON CONFLICT (tenant_id, session_id) DO UPDATE SET agent_id = excluded.agent_id, kind = excluded.kind, url = excluded.url,
			app_id = excluded.app_id, grant_id = excluded.grant_id, state = 'live', dead_reason = NULL, dead_at = NULL, registered_at = now(),
			failures = 0, next_attempt_at = NULL, last_status = NULL, last_error = NULL, bot_id = excluded.bot_id,
			key_fingerprint = excluded.key_fingerprint, joined = excluded.joined, join_due = excluded.join_due, removal_hash = excluded.removal_hash,
			dead_told_at = NULL, needs_setup_at = NULL, failing_since = NULL
		RETURNING xmax <> 0`, d.Session, d.Agent, KindCursor, SealCursor(seal, binding(tenant, d.Session), h), d.App, grant,
		d.Bot, d.Fingerprint, joined, announce, bindingID).Scan(&replaced)
	if err != nil {
		return false, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO agent_webhook_targets (tenant_id, session_id) VALUES (current_tenant(), $1) ON CONFLICT DO NOTHING`, d.Session); err != nil {
		return false, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO bot_webhooks (tenant_id, session_id, fingerprint, removal_hash, routine) VALUES (current_tenant(), $1, $2, $3, $4)
		ON CONFLICT (tenant_id, session_id) DO UPDATE SET fingerprint = excluded.fingerprint, removal_hash = excluded.removal_hash, routine = excluded.routine`,
		d.Session, d.Fingerprint, bindingID, d.Routine)
	return replaced, err
}

// BindRoutine makes a bind's binding (BindingID) and key the routine's current
// binding (bot_routines), locking the routine's row until the transaction
// ends: joins and carry-overs on the routine wait for it, then see the
// new binding.
func BindRoutine(ctx context.Context, tx pgx.Tx, routine, bindingID, fingerprint []byte) error {
	_, err := tx.Exec(ctx, `INSERT INTO bot_routines (routine, removal_hash, fingerprint) VALUES ($1, $2, $3)
		ON CONFLICT (routine) DO UPDATE SET removal_hash = excluded.removal_hash, fingerprint = excluded.fingerprint, bound_at = now()`,
		routine, bindingID, fingerprint)
	return err
}

// RoutineBinding is the routine's current binding, its row locked
// for share until the transaction ends (a bind on the routine waits); ok
// is false for a routine with no row (one bound before bot_routines).
func RoutineBinding(ctx context.Context, tx pgx.Tx, routine []byte) (bindingID []byte, ok bool, err error) {
	err = tx.QueryRow(ctx, `SELECT removal_hash FROM bot_routines WHERE routine = $1 FOR SHARE`, routine).Scan(&bindingID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	return bindingID, err == nil, err
}

// StaleOnRoutine are the destinations the index holds on a routine whose
// binding isn't the routine's: what a re-setup
// still has to carry its binding to. Read across workspaces, only after a
// setup proof on the routine checked out.
func StaleOnRoutine(ctx context.Context, q interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}, routine, bindingID []byte) ([]Peer, error) {
	rows, err := q.Query(ctx, `SELECT tenant_id::text, session_id::text FROM bot_webhooks WHERE routine = $1 AND removal_hash <> $2 ORDER BY tenant_id, session_id LIMIT 100`,
		routine, bindingID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[Peer])
}

// PruneRoutines deletes routines no destination has named for an hour.
func PruneRoutines(ctx context.Context, q interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}) (int64, error) {
	tag, err := q.Exec(ctx, `DELETE FROM bot_routines r WHERE r.bound_at < now() - interval '1 hour'
		AND NOT EXISTS (SELECT 1 FROM bot_webhooks w WHERE w.routine = r.routine)`)
	return tag.RowsAffected(), err
}

// OpenDestination opens a Cursor destination's URL and key in the
// transaction's workspace (tenant).
func OpenDestination(ctx context.Context, tx pgx.Tx, seal *mcpevents.Sealer, tenant, session string) (CursorHook, error) {
	var sealed []byte
	if err := tx.QueryRow(ctx, `SELECT url FROM agent_webhooks WHERE session_id = $1 AND kind = $2`, session, KindCursor).Scan(&sealed); err != nil {
		return CursorHook{}, err
	}
	return OpenCursor(seal, binding(tenant, session), sealed)
}

// Peer is another workspace's destination in a bot key's index.
type Peer struct{ Tenant, Session string }

// PeersByKey are the destinations the index (bot_webhooks) holds for a
// key's fingerprint, in any workspace: the only read across workspaces,
// made only after a proof by the key checked out.
func PeersByKey(ctx context.Context, tx pgx.Tx, fingerprint []byte) ([]Peer, error) {
	rows, _ := tx.Query(ctx, `SELECT tenant_id::text, session_id::text FROM bot_webhooks WHERE fingerprint = $1 ORDER BY tenant_id, session_id LIMIT 100`, fingerprint)
	return pgx.CollectRows(rows, pgx.RowToStructByPos[Peer])
}

// Unregister removes the session's destination in the transaction's
// workspace, reporting whether it had one. Messages then wait for the
// conversation's other ways of being woken, as they did before it
// registered.
func Unregister(ctx context.Context, tx pgx.Tx, session string) (bool, error) {
	tag, err := tx.Exec(ctx, `DELETE FROM agent_webhooks WHERE session_id = $1`, session)
	if err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM agent_webhook_targets WHERE tenant_id = current_tenant() AND session_id = $1`, session); err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}
