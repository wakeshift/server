package orgs

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5/pgconn"
)

// The organization's log (org_log): its own history, as app_log is an
// app's. Every change writes one entry in the same transaction, so an
// entry exists exactly when its change committed. Entries carry invite IDs
// and masked addresses, never tokens.
const (
	LogCreated        = "org.created"
	LogRenamed        = "org.renamed"
	LogDeleted        = "org.deleted"
	LogMemberJoined   = "org.member_joined"
	LogMemberRemoved  = "org.member_removed"
	LogMemberLeft     = "org.member_left"
	LogRoleChanged    = "org.role_changed"
	LogInviteSent     = "org.invite_sent"
	LogInviteRevoked  = "org.invite_revoked"
	LogInviteResent   = "org.invite_resent"
	LogInviteAccepted = "org.invite_accepted"
)

// UserActor is a user as a log's actor.
func UserActor(id string) string { return "user:" + id }

// Entry is one thing that happened to an organization.
type Entry struct {
	Org    string
	Actor  string // user:<id> or hub
	Action string
	Detail any // marshaled to a JSON object; keep it small
}

type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Log appends e, in the caller's transaction.
func Log(ctx context.Context, tx execer, e Entry) error {
	detail := []byte("{}")
	if e.Detail != nil {
		b, err := json.Marshal(e.Detail)
		if err != nil {
			return err
		}
		detail = b
	}
	_, err := tx.Exec(ctx, `INSERT INTO org_log (org_id, actor, action, detail) VALUES ($1, $2, $3, $4)`, e.Org, e.Actor, e.Action, detail)
	return err
}
