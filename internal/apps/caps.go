package apps

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// DefaultWorkspaceCap is how many workspaces an app may own (its users,
// conversations without a user) unless we've set its override (SetWorkspaceCap, or
// workspace_cap in WAKESHIFT_APPS for ours): one cap for every app. A
// variable so tests can lower it.
var DefaultWorkspaceCap = 10_000

// DefaultWorkspacesPerHour is how many new workspaces an app with the
// default cap may make in an hour: a fifth of its cap. An app's sign-up or
// sign-in, and conversations without a user, can make workspaces
// for anyone who reaches the app, so without it a few addresses could fill
// an app's cap faster than the app forgets them. Counted from
// tenants.created_at in Postgres, so it holds across instances. A variable
// so tests can lower it.
var DefaultWorkspacesPerHour = 2_000

// WorkspaceCap is how many workspaces app may own: our override for it,
// else DefaultWorkspaceCap.
func WorkspaceCap(app App) int {
	if app.WorkspaceCapOverride != nil {
		return *app.WorkspaceCapOverride
	}
	return DefaultWorkspaceCap
}

// WorkspacesPerHour is how many new workspaces app may make an hour:
// DefaultWorkspacesPerHour, or with an override, a fifth of it (at least
// one: the cap, checked first, is what stops an app with a cap of 0).
func WorkspacesPerHour(app App) int {
	if app.WorkspaceCapOverride != nil {
		return max(1, *app.WorkspaceCapOverride/5)
	}
	return DefaultWorkspacesPerHour
}

// DefaultEmitRecipientsPerMinute is how many recipients an app's
// multi-recipient emits may name in a minute, all together, unless we've
// set its override (apps.emit_recipients_per_minute). A single-user emit isn't counted. A variable so
// tests can lower it.
var DefaultEmitRecipientsPerMinute = 10_000

// EmitRecipientsPerMinute is app's limit on recipients a minute across its
// multi-recipient emits: our override, else DefaultEmitRecipientsPerMinute.
func EmitRecipientsPerMinute(app App) int {
	if app.EmitRecipientsOverride != nil {
		return *app.EmitRecipientsOverride
	}
	return DefaultEmitRecipientsPerMinute
}

// WorkspaceLimitError: the app owns as many workspaces as it may, so a new
// one isn't made. Refusals answer with Message, which names the server's
// support address.
type WorkspaceLimitError struct{ Cap int }

func (e WorkspaceLimitError) Error() string {
	return fmt.Sprintf("this app has reached its limit of %d workspaces", e.Cap)
}

// IsWorkspaceLimit reports whether err is a WorkspaceLimitError.
func IsWorkspaceLimit(err error) bool {
	var e WorkspaceLimitError
	return errors.As(err, &e)
}

// WorkspaceRateError: the app made as many workspaces in the last hour as
// it may, so a new one isn't made yet.
type WorkspaceRateError struct{ PerHour int }

func (e WorkspaceRateError) Error() string {
	return fmt.Sprintf("this app made %d workspaces in the last hour, its limit; try again later", e.PerHour)
}

// IsWorkspaceRate reports whether err is a WorkspaceRateError.
func IsWorkspaceRate(err error) bool {
	var e WorkspaceRateError
	return errors.As(err, &e)
}

// Querier is what CheckWorkspaceCap reads with (a pool or a transaction).
type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// CheckWorkspaceCap refuses (WorkspaceLimitError) a new workspace for app
// with externalID once the app owns WorkspaceCap of them, and
// (WorkspaceRateError) once the app made WorkspacesPerHour of them in the
// last hour; one it already has for externalID is always fine. Both are
// soft: callers racing at the limit may pass it by a few.
func CheckWorkspaceCap(ctx context.Context, q Querier, app App, externalID string) error {
	var exists bool
	var n, recent int
	// Each count stops at its limit, so a check reads at most that many
	// index entries (tenants_app_external, tenants_app_created), however
	// many workspaces the app has or makes.
	err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM tenants WHERE app_id = $1 AND external_id = $2),
		(SELECT count(*) FROM (SELECT 1 FROM tenants WHERE app_id = $1 LIMIT $3) t),
		(SELECT count(*) FROM (SELECT 1 FROM tenants WHERE app_id = $1 AND created_at > now() - interval '1 hour' LIMIT $4) r)`,
		app.ID, externalID, WorkspaceCap(app), WorkspacesPerHour(app)).Scan(&exists, &n, &recent)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	if n >= WorkspaceCap(app) {
		return WorkspaceLimitError{Cap: WorkspaceCap(app)}
	}
	if recent >= WorkspacesPerHour(app) {
		return WorkspaceRateError{PerHour: WorkspacesPerHour(app)}
	}
	return nil
}
