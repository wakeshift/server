// Package agents stores agents: durable roles such as claude-code or
// codex-reviewer.
package agents

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Agent is a durable role.
type Agent struct {
	ID          string    `json:"id"`
	DisplayName string    `json:"display_name"`
	CreatedAt   time.Time `json:"created_at"`
	// ConnectorSessions counts its connector sessions: harness
	// conversations (e.g. Codex threads) delivered on the agent's own
	// stream until the hooks or a wait take them over. Set by List.
	ConnectorSessions int `json:"connector_sessions"`
}

var (
	ErrExists   = errors.New("agents: an agent with this ID already exists")
	ErrNotFound = errors.New("agents: no such agent")

	idRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,39}$`)
)

// ValidID reports whether id can name an agent: 2–40 lowercase letters,
// digits and dashes, not starting with a dash.
func ValidID(id string) bool { return idRE.MatchString(id) }

// Create adds an agent.
func Create(ctx context.Context, tx pgx.Tx, id, displayName string) (Agent, error) {
	if !ValidID(id) {
		return Agent{}, fmt.Errorf("agents: %q isn't a valid ID: use 2-40 lowercase letters, digits and dashes", id)
	}
	var a Agent
	err := tx.QueryRow(ctx, `INSERT INTO agents (tenant_id, id, display_name) VALUES (current_tenant(), $1, $2)
		RETURNING id, display_name, created_at`, id, displayName).Scan(&a.ID, &a.DisplayName, &a.CreatedAt)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return Agent{}, ErrExists
	}
	return a, err
}

// Ensure adds an agent unless it exists. Two transactions making the same
// one at once (an agent's first two calls in parallel) both succeed: a
// conflict leaves the other's, without aborting the transaction as
// Create's ErrExists does.
func Ensure(ctx context.Context, tx pgx.Tx, id string) error {
	if !ValidID(id) {
		return fmt.Errorf("agents: %q isn't a valid ID: use 2-40 lowercase letters, digits and dashes", id)
	}
	_, err := tx.Exec(ctx, `INSERT INTO agents (tenant_id, id) VALUES (current_tenant(), $1) ON CONFLICT (tenant_id, id) DO NOTHING`, id)
	return err
}

// List returns the tenant's agents by ID.
func List(ctx context.Context, tx pgx.Tx) ([]Agent, error) {
	rows, _ := tx.Query(ctx, `SELECT a.id, a.display_name, a.created_at,
		(SELECT count(*) FROM sessions s WHERE s.agent_id = a.id AND s.runtime = 'connector')
		FROM agents a ORDER BY a.id`)
	return pgx.CollectRows(rows, pgx.RowToStructByPos[Agent])
}

// Exists reports whether the agent exists.
func Exists(ctx context.Context, tx pgx.Tx, id string) (bool, error) {
	var ok bool
	err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM agents WHERE id = $1)`, id).Scan(&ok)
	return ok, err
}
