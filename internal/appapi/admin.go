package appapi

// What the core's internal admin API (internal/adminapi) needs from the app
// API: revoking an app's grants where it acts, and forgetting a user with
// the admin's own log entry in the same transaction.

import (
	"context"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/db"
	"github.com/wakeshift/server/internal/grants"
)

// RevokeGrantsIn revokes app's live grants in each of workspaces, each in
// that workspace's transaction (grants.Revoke records grant.revoked in its
// audit log and ends what the grant made): how many, and in how many of
// them. It stops at the first failure; running it again finishes.
func RevokeGrantsIn(ctx context.Context, d *db.DB, app, by string, workspaces []string) (revoked, in int, err error) {
	for _, ws := range workspaces {
		n := 0
		err := d.InTenant(ctx, ws, func(tx pgx.Tx) error {
			rows, _ := tx.Query(ctx, `SELECT id::text FROM grants WHERE app_id = $1 AND revoked_at IS NULL`, app)
			ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
			if err != nil {
				return err
			}
			for _, id := range ids {
				if _, err := grants.Revoke(ctx, tx, id, by); err != nil {
					return err
				}
				n++
			}
			return nil
		})
		if err != nil {
			return revoked, in, err
		}
		if n > 0 {
			revoked += n
			in++
		}
	}
	return revoked, in, nil
}

// ForgetUserLogged is ForgetUser with also run in its transaction when
// there is someone to forget (the admin API's own log entry), so the
// erasure and its record commit together or not at all.
func (a *API) ForgetUserLogged(ctx context.Context, app, user, by string, also func(pgx.Tx) error) (forgotten bool, err error) {
	user = strings.TrimSpace(user)
	if !appUser(user) {
		return false, badRequest{errBadForgetUser}
	}
	return forgetWith(ctx, a.DB, app, user, by, 0, also)
}
