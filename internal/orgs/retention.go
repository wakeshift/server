package orgs

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Retention (the privacy policy's Section 9). The organization's own row
// stays after it's deleted, so its ID isn't reused and its deleted apps'
// log entries still name it; what can name people goes:
const (
	// InviteRetention is how long an invite (with the invited address) is
	// kept after it ended: accepted, revoked or expired, whichever came
	// first. An organization's deletion revokes its invites.
	InviteRetention = 30 * 24 * time.Hour
	// LogRetention is how long a deleted organization's log is kept after
	// the deletion, as a deleted app's is (apps.LogRetention).
	LogRetention = 30 * 24 * time.Hour
)

// Prune deletes invites that ended over InviteRetention ago and the logs of
// organizations deleted over LogRetention ago, at most limit rows of each,
// for the expiry pass (the rest go on the next). Nothing reads either
// once it's that old: the invite-email limits count the last day's.
func Prune(ctx context.Context, pool *pgxpool.Pool, limit int) (invites, logs int64, err error) {
	tag, err := pool.Exec(ctx, `DELETE FROM organization_invites WHERE id IN (SELECT id FROM organization_invites
		WHERE least(accepted_at, revoked_at, expires_at) < now() - make_interval(secs => $1) LIMIT $2)`,
		InviteRetention.Seconds(), limit)
	if err != nil {
		return 0, 0, err
	}
	invites = tag.RowsAffected()
	tag, err = pool.Exec(ctx, `DELETE FROM org_log WHERE id IN (SELECT l.id FROM organizations o JOIN org_log l ON l.org_id = o.id
		WHERE o.deleted_at < now() - make_interval(secs => $1) LIMIT $2)`,
		LogRetention.Seconds(), limit)
	return invites, tag.RowsAffected(), err
}
