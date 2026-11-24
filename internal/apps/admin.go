package apps

// Wakeshift's own actions on an app: suspend and unsuspend, the
// workspace cap override, and deleting an app as us. Only the core's
// internal admin API (internal/adminapi) calls them, each in its
// transaction with the app's log entry written here (actor AdminActor,
// which the console shows as "Wakeshift", never our email) and the
// admin_log entry the API adds.

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// AdminActor is the actor in an app's log (and its users' workspaces'
// audit logs) for what we did through the admin service.
const AdminActor = "admin"

// Admin actions in the app's log.
const (
	LogSuspended       = "app.suspended"         // by us: keys and publish URLs refused; reason in detail
	LogUnsuspended     = "app.unsuspended"       // by us
	LogWorkspaceCapSet = "app.workspace_cap_set" // by us: cap and old (null: the default)
)

var (
	// ErrNoChange is an admin action that would change nothing (suspending
	// a suspended app, say).
	ErrNoChange = errors.New("apps: nothing to change")
	// ErrOurs is an admin action that our own apps (WAKESHIFT_APPS) don't
	// take: they're configured, not managed.
	ErrOurs = errors.New("apps: one of Wakeshift's own apps; change it in WAKESHIFT_APPS")
)

// MaxReason is how long an admin action's reason may be.
const MaxReason = 500

// AdminApp is what an admin action reads about the app, locked for the
// change.
type AdminApp struct {
	ID              string
	Name            string
	MCPURL          string
	Ours            bool // registered from WAKESHIFT_APPS
	Deleted         bool
	Suspended       bool
	SuspendedReason string
	WorkspaceCap    *int
}

// ForAdmin locks app id for an admin action. ErrNotFound if there's none.
func ForAdmin(ctx context.Context, tx pgx.Tx, id string) (AdminApp, error) {
	var a AdminApp
	err := tx.QueryRow(ctx, `SELECT id, name, coalesce(mcp_url, ''), org_id IS NULL, deleted_at IS NOT NULL, suspended_at IS NOT NULL,
		coalesce(suspended_reason, ''), workspace_cap FROM apps WHERE id = $1 FOR UPDATE`, id).Scan(
		&a.ID, &a.Name, &a.MCPURL, &a.Ours, &a.Deleted, &a.Suspended, &a.SuspendedReason, &a.WorkspaceCap)
	if errors.Is(err, pgx.ErrNoRows) {
		return a, ErrNotFound
	}
	return a, err
}

// liveForAdmin is ForAdmin for a change to an app that isn't deleted.
func liveForAdmin(ctx context.Context, tx pgx.Tx, id string) (AdminApp, error) {
	a, err := ForAdmin(ctx, tx, id)
	if err == nil && a.Deleted {
		err = ErrDeleted
	}
	return a, err
}

// Suspend stops the app at once: Authenticate refuses its assertions and
// its publish URLs refuse posts (ErrSuspended), and its owners can't make
// keys or publish URLs. Data, grants and subscriptions are kept, and
// deliveries already made still drain. by names who did it (for the row;
// the log says AdminActor); reason is shown to the app's owners.
func (r *Registry) Suspend(ctx context.Context, tx pgx.Tx, id, by, reason string) error {
	a, err := liveForAdmin(ctx, tx, id)
	switch {
	case err != nil:
		return err
	case a.Ours:
		return ErrOurs
	case a.Suspended:
		return ErrNoChange
	}
	if _, err := tx.Exec(ctx, `UPDATE apps SET suspended_at = now(), suspended_by = $2, suspended_reason = $3 WHERE id = $1`, id, by, reason); err != nil {
		return err
	}
	return Log(ctx, tx, LogEntry{AppID: id, Actor: AdminActor, Action: LogSuspended, Detail: map[string]string{"reason": reason}})
}

// Unsuspend restores a suspended app as it was.
func (r *Registry) Unsuspend(ctx context.Context, tx pgx.Tx, id, reason string) error {
	a, err := ForAdmin(ctx, tx, id)
	switch {
	case err != nil:
		return err
	case !a.Suspended:
		return ErrNoChange
	}
	if _, err := tx.Exec(ctx, `UPDATE apps SET suspended_at = NULL, suspended_by = NULL, suspended_reason = NULL WHERE id = $1`, id); err != nil {
		return err
	}
	return Log(ctx, tx, LogEntry{AppID: id, Actor: AdminActor, Action: LogUnsuspended, Detail: map[string]string{"reason": reason}})
}

// MaxWorkspaceCap is the largest workspace cap override.
const MaxWorkspaceCap = 10_000_000

// SetWorkspaceCap overrides the app's workspace cap (WorkspaceCap); nil
// goes back to the default. It returns the old override.
func (r *Registry) SetWorkspaceCap(ctx context.Context, tx pgx.Tx, id string, limit *int) (old *int, err error) {
	if limit != nil && (*limit < 0 || *limit > MaxWorkspaceCap) {
		return nil, &ValidationError{fmt.Sprintf("a workspace cap is 0 to %d", MaxWorkspaceCap)}
	}
	a, err := liveForAdmin(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	if a.Ours { // WAKESHIFT_APPS sets ours, on every start
		return a.WorkspaceCap, ErrOurs
	}
	if (a.WorkspaceCap == nil && limit == nil) || (a.WorkspaceCap != nil && limit != nil && *a.WorkspaceCap == *limit) {
		return a.WorkspaceCap, ErrNoChange
	}
	if _, err := tx.Exec(ctx, `UPDATE apps SET workspace_cap = $2 WHERE id = $1`, id, limit); err != nil {
		return nil, err
	}
	return a.WorkspaceCap, Log(ctx, tx, LogEntry{AppID: id, Actor: AdminActor, Action: LogWorkspaceCapSet, Detail: map[string]any{"cap": limit, "old": a.WorkspaceCap}})
}

// DeleteAsAdmin deletes a console app as Delete does for its owners, by
// us. Our own apps aren't deleted here (WAKESHIFT_APPS would register them
// again). already reports an app deleted before.
func (r *Registry) DeleteAsAdmin(ctx context.Context, tx pgx.Tx, id string) (already bool, err error) {
	a, err := ForAdmin(ctx, tx, id)
	switch {
	case err != nil:
		return false, err
	case a.Ours:
		return false, ErrOurs
	case a.Deleted:
		return true, nil
	}
	return false, deleteLocked(ctx, tx, id, a.Name, AdminActor)
}

// ValidReason checks an admin action's reason: written for the app's
// owners (it names the Terms section or the behavior), one to MaxReason
// characters.
func ValidReason(reason string) (string, error) {
	reason = strings.TrimSpace(reason)
	switch {
	case reason == "":
		return "", &ValidationError{"a reason is required"}
	case len(reason) > MaxReason:
		return "", &ValidationError{fmt.Sprintf("a reason is at most %d characters", MaxReason)}
	}
	return reason, nil
}
