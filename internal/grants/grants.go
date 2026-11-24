// Package grants records consents. A grant lets one app act for one agent
// (post as it, subscribe it, give it stream links), for the scope the app's
// manifest asked for at the time; inside that scope nothing prompts again,
// and the app can't widen it without a new consent. Revoking a grant undoes
// what was created under it.
//
// Grants are made only in workspaces an app keeps for its users (Create),
// confirmed by the app. Account mode, an app's grants in people's own
// workspaces through Wakeshift's consent screen, is turned off: kind
// "workspace" and grants confirmed by a user are only rows from before,
// which expire.
package grants

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/wakeshift/server/internal/apps"
	"github.com/wakeshift/server/internal/audit"
	"github.com/wakeshift/server/internal/streams"
	"github.com/wakeshift/server/internal/subscriptions"
)

// Agent is the kind of grant Create makes ("workspace", an app publishing
// into a person's workspace, was account mode's).
const Agent = "agent"

// Grant is a consent.
type Grant struct {
	ID        string `json:"id"`
	AppID     string `json:"app_id"`
	Kind      string `json:"kind"`
	AgentID   string `json:"agent_id,omitempty"`
	Scope     Scope  `json:"scope"`
	Shown     Shown  `json:"shown"`
	GrantedBy string `json:"granted_by,omitempty"` // the user who allowed it, for a grant from account mode
	// ConfirmedBy is how it was allowed: "app" (a headless app, whose user
	// added it to the agent), or "user" (Wakeshift's consent screen, in
	// account mode).
	ConfirmedBy string     `json:"confirmed_by"`
	CreatedAt   time.Time  `json:"created_at"`
	ExpiresAt   time.Time  `json:"expires_at"`
	RevokedAt   *time.Time `json:"revoked_at,omitempty"`
}

// Scope is what was consented to: the app's manifest scope at the time.
type Scope struct {
	Agent *apps.AgentScope `json:"agent,omitempty"`
}

// Shown is what the consent screen said, kept with the grant.
type Shown struct {
	Risks   []Risk   `json:"risks"`
	Warning *Warning `json:"warning,omitempty"` // the one shown up front
	DryRun  DryRun   `json:"dry_run"`
}

var (
	ErrNotFound = errors.New("grants: no such grant")
	ErrExists   = errors.New("grants: already granted")
	// ErrWrongMode: an app in a workspace it doesn't own. No app acts in
	// people's own workspaces: account mode is turned off.
	ErrWrongMode = errors.New("grants: an app acts only in workspaces it keeps for its users")
)

// New is what Create needs.
type New struct {
	App     apps.App
	AgentID string
	Shown   Shown
}

// Create records a consent for the app's current agent scope, confirmed by
// the app: a headless app's user adding it to the agent was the consent.
func Create(ctx context.Context, tx pgx.Tx, n New) (Grant, error) {
	scope := n.App.Manifest.Agent
	if scope == nil {
		return Grant{}, errors.New("grants: the app doesn't act for agents")
	}
	// The one gate on where an app may act: only in a workspace it owns,
	// never in a person's own (account mode is turned off, whatever the
	// app's manifest says). Read from the database, not
	// the caller's copy.
	var allowed bool
	if err := tx.QueryRow(ctx, `SELECT t.kind = 'app' AND t.app_id = a.id AND a.deleted_at IS NULL
		FROM apps a, tenants t WHERE a.id = $1 AND t.id = current_tenant()`, n.App.ID).Scan(&allowed); err != nil {
		return Grant{}, err
	}
	if !allowed {
		return Grant{}, ErrWrongMode
	}
	sb, _ := json.Marshal(Scope{Agent: scope})
	shb, _ := json.Marshal(n.Shown)
	// An expired grant still holds the "one live grant" slot until it's
	// revoked (with what it created).
	rows, _ := tx.Query(ctx, `SELECT id::text FROM grants WHERE app_id = $1 AND kind = 'agent' AND agent_id = $2
		AND revoked_at IS NULL AND expires_at <= now()`, n.App.ID, n.AgentID)
	expired, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return Grant{}, err
	}
	for _, id := range expired {
		if _, err := revoke(ctx, tx, id, audit.Hub, audit.GrantExpired); err != nil {
			return Grant{}, err
		}
	}
	// In a savepoint: a concurrent consent's insert makes this one fail
	// (ErrExists) without aborting the caller's transaction.
	sp, err := tx.Begin(ctx)
	if err != nil {
		return Grant{}, err
	}
	defer sp.Rollback(ctx)
	g, err := scan(sp.QueryRow(ctx, `INSERT INTO grants (tenant_id, app_id, kind, agent_id, scope, shown, expires_at, confirmed_by)
		VALUES (current_tenant(), $1, 'agent', $2, $3, $4, now() + make_interval(days => $5), 'app') RETURNING `+columns,
		n.App.ID, n.AgentID, sb, shb, scope.TTLDays))
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return Grant{}, ErrExists
	}
	if err != nil {
		return g, err
	}
	if err := sp.Commit(ctx); err != nil {
		return g, err
	}
	// What the screen would have shown is in the grant (g.Shown); the entry
	// points at it.
	return g, audit.Record(ctx, tx, audit.Entry{Actor: "app:" + g.AppID, Action: audit.GrantCreated, AgentID: g.AgentID, AppID: g.AppID, GrantID: g.ID,
		Detail: map[string]any{"kind": g.Kind, "expires_at": g.ExpiresAt, "confirmed_by": g.ConfirmedBy}})
}

// Ensure returns the live grant for the app and agent, creating it if
// there's none. Two consents at once (a double click) end up with the same
// grant.
func Ensure(ctx context.Context, tx pgx.Tx, n New) (g Grant, created bool, err error) {
	if g, err := Live(ctx, tx, n.App.ID, n.AgentID); !errors.Is(err, ErrNotFound) {
		return g, false, err
	}
	g, err = Create(ctx, tx, n)
	if errors.Is(err, ErrExists) {
		g, err = Live(ctx, tx, n.App.ID, n.AgentID)
		return g, false, err
	}
	return g, err == nil, err
}

// RevokeExpired revokes the workspace's grants that have expired, undoing
// what they created (subscriptions stop waking agents, links stop working),
// the longest expired first, at most expiredPerPass: the expiry pass's
// transaction is bounded, and the next pass takes the rest. An expired
// grant authorizes nothing meanwhile (Live checks expires_at).
func RevokeExpired(ctx context.Context, tx pgx.Tx) (int, error) {
	rows, _ := tx.Query(ctx, `SELECT id::text FROM grants WHERE revoked_at IS NULL AND expires_at <= now() ORDER BY expires_at LIMIT $1`, expiredPerPass)
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return 0, err
	}
	for _, id := range ids {
		if _, err := revoke(ctx, tx, id, audit.Hub, audit.GrantExpired); err != nil {
			return 0, err
		}
	}
	return len(ids), nil
}

// expiredPerPass is how many expired grants RevokeExpired revokes at most.
const expiredPerPass = 200

// Live returns the live (unrevoked, unexpired) grant for an app and agent.
func Live(ctx context.Context, tx pgx.Tx, appID, agent string) (Grant, error) {
	return scan(tx.QueryRow(ctx, `SELECT `+columns+` FROM grants WHERE app_id = $1 AND kind = 'agent' AND agent_id = $2
		AND revoked_at IS NULL AND expires_at > now()`, appID, agent))
}

// Get returns a grant.
func Get(ctx context.Context, tx pgx.Tx, id string) (Grant, error) {
	return scan(tx.QueryRow(ctx, `SELECT `+columns+` FROM grants WHERE id = $1`, id))
}

// List returns the workspace's live grants, newest first.
func List(ctx context.Context, tx pgx.Tx) ([]Grant, error) {
	rows, err := tx.Query(ctx, `SELECT `+columns+` FROM grants WHERE revoked_at IS NULL AND expires_at > now() ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Grant, error) { return scan(r) })
}

// Revoke ends a grant and undoes what it allowed: the subscriptions the
// app made for the agent, its event subscriptions, and the stream links it
// handed out. by is who revoked it, for the audit log (e.g.
// audit.User(id)).
func Revoke(ctx context.Context, tx pgx.Tx, id, by string) (Grant, error) {
	return revoke(ctx, tx, id, by, audit.GrantRevoked)
}

func revoke(ctx context.Context, tx pgx.Tx, id, by, action string) (Grant, error) {
	before, err := scan(tx.QueryRow(ctx, `SELECT `+columns+` FROM grants WHERE id = $1 FOR UPDATE`, id))
	if err != nil || before.RevokedAt != nil {
		// Already revoked: don't cascade again (the agent's sign-ins now
		// belong to a newer grant).
		return before, err
	}
	if before.Kind == Agent {
		// The agent's live event subscriptions to the app, locked before
		// anything they hold: ending one (appapi's endEventSub) locks it,
		// then its subscription and webhook, so revoking takes them in the
		// same order (endEventSubs ends them below, after the grant's
		// subscriptions are canceled) and can't deadlock with it.
		if _, err := tx.Exec(ctx, `SELECT 1 FROM event_subscriptions
			WHERE source_kind = 'app' AND source_id = $1 AND agent_id = $2 AND ended_at IS NULL ORDER BY id FOR UPDATE`, before.AppID, before.AgentID); err != nil {
			return before, err
		}
		// Then their webhooks, before any delivery: canceling the grant's
		// subscriptions below cancels what waits for the webhooks, and
		// recording a webhook's delivery locks the webhook, then the
		// delivery. Ending the webhooks only after their deliveries
		// (endEventSubs), the two deadlocked.
		if _, err := tx.Exec(ctx, `SELECT 1 FROM mcp_event_webhooks WHERE id IN (SELECT webhook_id FROM event_subscriptions
			WHERE source_kind = 'app' AND source_id = $1 AND agent_id = $2 AND ended_at IS NULL) ORDER BY id FOR NO KEY UPDATE`, before.AppID, before.AgentID); err != nil {
			return before, err
		}
	}
	g, err := scan(tx.QueryRow(ctx, `UPDATE grants SET revoked_at = now() WHERE id = $1 RETURNING `+columns, id))
	if err != nil {
		return g, err
	}
	rows, err := tx.Query(ctx, `SELECT id::text FROM subscriptions WHERE grant_id = $1 AND status = 'active'`, id)
	if err != nil {
		return g, err
	}
	subs, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return g, err
	}
	if err := subscriptions.CancelAll(ctx, tx, subs); err != nil {
		return g, err
	}
	if err := streams.RevokeGrantLinks(ctx, tx, id, by); err != nil {
		return g, err
	}
	if g.Kind == Agent {
		if err := endEventSubs(ctx, tx, g, by); err != nil {
			return g, err
		}
	}
	return g, audit.Record(ctx, tx, audit.Entry{Actor: by, Action: action, AgentID: g.AgentID, AppID: g.AppID, GrantID: g.ID,
		Detail: map[string]any{"kind": g.Kind, "subscriptions_canceled": len(subs)}})
}

const columns = `id::text, app_id, kind, coalesce(agent_id, ''), scope, shown, coalesce(granted_by::text, ''), confirmed_by, created_at, expires_at, revoked_at`

func scan(row pgx.Row) (Grant, error) {
	var g Grant
	var scope, shown []byte
	err := row.Scan(&g.ID, &g.AppID, &g.Kind, &g.AgentID, &scope, &shown, &g.GrantedBy, &g.ConfirmedBy, &g.CreatedAt, &g.ExpiresAt, &g.RevokedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return g, ErrNotFound
	}
	if err == nil {
		json.Unmarshal(scope, &g.Scope)
		json.Unmarshal(shown, &g.Shown)
	}
	return g, err
}

// EnsureByApp returns the live grant for a headless app's agent, creating
// it at once if there's none: confirmed by the app (adding the app to the
// agent was the user's consent), with what Wakeshift's consent screen
// would have shown.
func EnsureByApp(ctx context.Context, tx pgx.Tx, app apps.App, agent string) (Grant, error) {
	if g, err := Live(ctx, tx, app.ID, agent); !errors.Is(err, ErrNotFound) {
		return g, err
	}
	risks, dry, err := AgentConsent(ctx, tx, app, agent)
	if err != nil {
		return Grant{}, err
	}
	warning := AgentWarning(app, agent, risks)
	g, _, err := Ensure(ctx, tx, New{App: app, AgentID: agent, Shown: Shown{Risks: risks, Warning: &warning, DryRun: dry}})
	return g, err
}

// endEventSubs ends the agent's event subscriptions to the app's event
// types (their topic subscriptions were the grant's, canceled above): each
// is recorded as revoked, with an audit entry, and a webhook's receiver is
// told (a terminated envelope: the deliverer sends it and removes the
// webhook, once marked as mcpevents.Terminate marks it; that package
// imports this one, so the one UPDATE is repeated here).
func endEventSubs(ctx context.Context, tx pgx.Tx, g Grant, by string) error {
	type sub struct{ ID, Event, Arguments, Destination, Webhook, Routing string }
	rows, _ := tx.Query(ctx, `SELECT id::text, event, arguments, destination, coalesce(webhook_id, ''), subscription_id::text FROM event_subscriptions
		WHERE source_kind = 'app' AND source_id = $1 AND agent_id = $2 AND ended_at IS NULL FOR UPDATE`, g.AppID, g.AgentID)
	subs, err := pgx.CollectRows(rows, pgx.RowToStructByPos[sub])
	if err != nil {
		return err
	}
	for _, s := range subs {
		if s.Webhook != "" {
			if _, err := tx.Exec(ctx, `UPDATE mcp_event_webhooks SET ending_reason = 'Access revoked' WHERE id = $1 AND ending_reason IS NULL`, s.Webhook); err != nil {
				return err
			}
		}
		// Its routing: the subscription was canceled with the grant's
		// (above); the topic subscription ends with it, as appapi's
		// endTopicSub ends one (reason "revoked").
		if _, err := tx.Exec(ctx, `UPDATE topic_subscriptions SET ended_reason = 'revoked', ended_at = now() WHERE subscription_id = $1 AND ended_at IS NULL`, s.Routing); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM topic_members WHERE tenant_id = current_tenant() AND subscription_id = $1`, s.Routing); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE event_subscriptions SET ended_at = now(), ended_reason = 'revoked', ended_by = $2 WHERE id = $1::uuid`, s.ID, by); err != nil {
			return err
		}
		if s.Destination == "push" {
			// The stream holding it ends at once (appapi's eventspush.go).
			if _, err := tx.Exec(ctx, `SELECT pg_notify($1, $2)`, streams.PushEndedChannel, s.ID); err != nil {
				return err
			}
		}
		if err := audit.Record(ctx, tx, audit.Entry{Actor: by, Action: "event_subscription.revoked", AgentID: g.AgentID, AppID: g.AppID, GrantID: g.ID,
			Detail: map[string]any{"id": s.ID, "event": s.Event, "arguments": json.RawMessage(s.Arguments), "destination": s.Destination}}); err != nil {
			return err
		}
	}
	return nil
}
