package orgs

// The organization's own changes: create, rename and delete it;
// list, re-role and remove its members; leave it. Every change locks the
// organization's row and writes exactly one org_log entry in the same
// transaction; a refused change writes none.
//
// An organization always has an owner: demoting, removing or leaving as
// the last owner is refused (ErrLastOwner). Two owners demoting each other
// at once can't both succeed, since each locks the organization's row and
// counts its owners in the same transaction.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

// Limits.
const (
	// MaxCreated is how many organizations (not counting their personal
	// one) a user may have created that aren't deleted.
	MaxCreated = 5
	// MaxMembers is how many members an organization may have.
	MaxMembers = 100
)

var (
	// ErrLastOwner: the change would leave the organization without an
	// owner (409).
	ErrLastOwner = errors.New("orgs: make someone else an owner first")
	// ErrHasApps: an organization with apps that aren't deleted can't be
	// deleted (409).
	ErrHasApps = errors.New("orgs: delete its apps first")
	// ErrNoMember: the user named isn't a member (404).
	ErrNoMember = errors.New("orgs: no such member")
	// ErrTooMany: the user has made MaxCreated organizations already.
	ErrTooMany = fmt.Errorf("orgs: at most %d organizations each", MaxCreated)
)

// ValidationError is input a user must fix.
type ValidationError struct{ Message string }

func (e *ValidationError) Error() string { return e.Message }

// ValidName trims an organization's name, or says what's wrong with it. It
// appears in invite emails and on the join page, so it's one line, without
// angle brackets.
func ValidName(name string) (string, error) {
	name = strings.TrimSpace(name)
	switch {
	case name == "" || utf8.RuneCountInString(name) > maxNameRunes:
		return "", &ValidationError{fmt.Sprintf("a name of 1 to %d characters is required", maxNameRunes)}
	case strings.ContainsAny(name, "<>") || strings.ContainsFunc(name, unicode.IsControl):
		return "", &ValidationError{"an organization's name is one line, without angle brackets"}
	}
	return name, nil
}

// Org is an organization as one of its members sees it.
type Org struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Personal  bool      `json:"personal"` // the one made for its creator at sign-up
	Role      Role      `json:"role"`     // the reader's
	Members   int       `json:"members"`
	Apps      int       `json:"apps"` // not deleted
	CreatedAt time.Time `json:"created_at"`
}

const orgColumns = `o.id::text, o.name, o.personal_of IS NOT NULL, m.role,
	(SELECT count(*) FROM organization_members c WHERE c.org_id = o.id),
	(SELECT count(*) FROM apps a WHERE a.org_id = o.id AND a.deleted_at IS NULL), o.created_at`

func scanOrg(row pgx.Row) (Org, error) {
	var o Org
	err := row.Scan(&o.ID, &o.Name, &o.Personal, &o.Role, &o.Members, &o.Apps, &o.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return o, ErrNotMember
	}
	return o, err
}

// List is user's organizations, personal first, then in the order they
// joined.
func List(ctx context.Context, tx pgx.Tx, user string) ([]Org, error) {
	rows, err := tx.Query(ctx, `SELECT `+orgColumns+` FROM organization_members m JOIN organizations o ON o.id = m.org_id AND o.deleted_at IS NULL
		WHERE m.user_id = $1 ORDER BY o.personal_of = $1 DESC NULLS LAST, m.created_at, o.id`, user)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Org, error) { return scanOrg(row) })
	if out == nil {
		out = []Org{}
	}
	return out, err
}

// Get is organization org as user, a member, sees it.
func Get(ctx context.Context, tx pgx.Tx, user, org string) (Org, error) {
	if _, err := Check(ctx, tx, user, org, OrgRead); err != nil {
		return Org{}, err
	}
	return scanOrg(tx.QueryRow(ctx, `SELECT `+orgColumns+` FROM organizations o JOIN organization_members m ON m.org_id = o.id AND m.user_id = $1
		WHERE o.id = $2 AND o.deleted_at IS NULL`, user, org))
}

// Create makes an organization named name, with user as its owner.
func Create(ctx context.Context, tx pgx.Tx, user, name string) (Org, error) {
	name, err := ValidName(name)
	if err != nil {
		return Org{}, err
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('orgs.created_by:' || $1))`, user); err != nil {
		return Org{}, err
	}
	var n int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM organizations WHERE created_by = $1 AND personal_of IS NULL AND deleted_at IS NULL`, user).Scan(&n); err != nil {
		return Org{}, err
	}
	if n >= MaxCreated {
		return Org{}, ErrTooMany
	}
	var id string
	if err := tx.QueryRow(ctx, `INSERT INTO organizations (name, created_by) VALUES ($1, $2) RETURNING id::text`, name, user).Scan(&id); err != nil {
		return Org{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO organization_members (org_id, user_id, role) VALUES ($1, $2, 'owner')`, id, user); err != nil {
		return Org{}, err
	}
	if err := Log(ctx, tx, Entry{Org: id, Actor: UserActor(user), Action: LogCreated, Detail: map[string]any{"name": name}}); err != nil {
		return Org{}, err
	}
	return Get(ctx, tx, user, id)
}

// lock locks organization org's row for a change by user, whose role it
// returns: every change to members, invites, the name or the organization
// itself goes through here, so they happen one at a time. ErrNotMember for
// a non-member.
func lock(ctx context.Context, tx pgx.Tx, user, org string) (Role, error) {
	if !validID(org) {
		return "", ErrNotMember
	}
	var deleted bool
	err := tx.QueryRow(ctx, `SELECT deleted_at IS NOT NULL FROM organizations WHERE id = $1 FOR UPDATE`, org).Scan(&deleted)
	if errors.Is(err, pgx.ErrNoRows) || deleted {
		return "", ErrNotMember
	}
	if err != nil {
		return "", err
	}
	return RoleIn(ctx, tx, user, org, false)
}

// Rename renames org as user (admin and up).
func Rename(ctx context.Context, tx pgx.Tx, user, org, name string) (Org, error) {
	role, err := lock(ctx, tx, user, org)
	if err != nil {
		return Org{}, err
	}
	if !role.Can(OrgRename) {
		return Org{}, ErrForbidden
	}
	name, err = ValidName(name)
	if err != nil {
		return Org{}, err
	}
	var old string
	if err := tx.QueryRow(ctx, `SELECT name FROM organizations WHERE id = $1`, org).Scan(&old); err != nil {
		return Org{}, err
	}
	if old != name {
		if _, err := tx.Exec(ctx, `UPDATE organizations SET name = $2 WHERE id = $1`, org, name); err != nil {
			return Org{}, err
		}
		if err := Log(ctx, tx, Entry{Org: org, Actor: UserActor(user), Action: LogRenamed, Detail: map[string]any{"old_name": old, "name": name}}); err != nil {
			return Org{}, err
		}
	}
	return Get(ctx, tx, user, org)
}

// Delete deletes org as user (an owner): refused (ErrHasApps) while it has
// apps that aren't deleted. It marks it deleted, removes its members and
// revokes its pending invites. The logs of its apps deleted in the last 30
// days are no longer readable by anyone; they're still deleted on
// schedule.
func Delete(ctx context.Context, tx pgx.Tx, user, org string) error {
	role, err := lock(ctx, tx, user, org)
	if err != nil {
		return err
	}
	if !role.Can(OrgDelete) {
		return ErrForbidden
	}
	// Creating an app holds the organization's row (shared) until it
	// commits, so none can appear after this count.
	var apps int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM apps WHERE org_id = $1 AND deleted_at IS NULL`, org).Scan(&apps); err != nil {
		return err
	}
	if apps > 0 {
		return ErrHasApps
	}
	members, err := tx.Exec(ctx, `DELETE FROM organization_members WHERE org_id = $1`, org)
	if err != nil {
		return err
	}
	invites, err := tx.Exec(ctx, `UPDATE organization_invites SET revoked_at = now() WHERE org_id = $1 AND accepted_at IS NULL AND revoked_at IS NULL`, org)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE organizations SET deleted_at = now() WHERE id = $1`, org); err != nil {
		return err
	}
	return Log(ctx, tx, Entry{Org: org, Actor: UserActor(user), Action: LogDeleted,
		Detail: map[string]any{"members_removed": members.RowsAffected(), "invites_revoked": invites.RowsAffected()}})
}

// Member is one of an organization's members.
type Member struct {
	UserID    string    `json:"user_id"`
	Email     string    `json:"email"`
	Name      string    `json:"name"`
	Role      Role      `json:"role"`
	InvitedBy string    `json:"invited_by,omitempty"` // their email
	JoinedAt  time.Time `json:"joined_at"`
}

// Members lists org's members (user must be one): owners first, then
// admins, then developers, each in the order they joined.
func Members(ctx context.Context, tx pgx.Tx, user, org string) ([]Member, error) {
	if _, err := Check(ctx, tx, user, org, OrgRead); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `SELECT u.id::text, u.email, u.name, m.role, coalesce(i.email, ''), m.created_at
		FROM organization_members m JOIN users u ON u.id = m.user_id LEFT JOIN users i ON i.id = m.invited_by
		WHERE m.org_id = $1 ORDER BY array_position(ARRAY['owner', 'admin', 'developer'], m.role), m.created_at, u.id`, org)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, pgx.RowToStructByPos[Member])
	if out == nil {
		out = []Member{}
	}
	return out, err
}

// owners counts org's owners (in the caller's locked transaction).
func owners(ctx context.Context, tx pgx.Tx, org string) (int, error) {
	var n int
	err := tx.QueryRow(ctx, `SELECT count(*) FROM organization_members WHERE org_id = $1 AND role = 'owner'`, org).Scan(&n)
	return n, err
}

// roleOf is target's role in org, or ErrNoMember.
func roleOf(ctx context.Context, tx pgx.Tx, org, target string) (Role, error) {
	if !validID(target) {
		return "", ErrNoMember
	}
	var r Role
	err := tx.QueryRow(ctx, `SELECT role FROM organization_members WHERE org_id = $1 AND user_id = $2`, org, target).Scan(&r)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNoMember
	}
	return r, err
}

// SetRole changes target's role in org to role, as user. Only owners
// change roles (admins manage developers, who have no other role to move
// to). Demoting the last owner, themselves included, is ErrLastOwner.
func SetRole(ctx context.Context, tx pgx.Tx, user, org, target string, role Role) error {
	actor, err := lock(ctx, tx, user, org)
	if err != nil {
		return err
	}
	if !actor.Can(OrgChangeRoles) {
		return ErrForbidden
	}
	if !role.Valid() {
		return &ValidationError{"role is owner, admin or developer"}
	}
	old, err := roleOf(ctx, tx, org, target)
	if err != nil {
		return err
	}
	if !actor.CanManage(old) || !actor.CanGrant(role) {
		return ErrForbidden
	}
	if old == role {
		return nil
	}
	if old == Owner {
		if n, err := owners(ctx, tx, org); err != nil {
			return err
		} else if n <= 1 {
			return ErrLastOwner
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE organization_members SET role = $3 WHERE org_id = $1 AND user_id = $2`, org, target, role); err != nil {
		return err
	}
	return Log(ctx, tx, Entry{Org: org, Actor: UserActor(user), Action: LogRoleChanged,
		Detail: map[string]any{"user_id": target, "old_role": old, "role": role}})
}

// Remove removes target from org as user: an owner removes anyone, an
// admin only developers; anyone removes themselves (leaving). Either way
// the last owner can't go (ErrLastOwner). Their next request to any of the
// organization's apps is 404; their own workspace is untouched.
func Remove(ctx context.Context, tx pgx.Tx, user, org, target string) error {
	actor, err := lock(ctx, tx, user, org)
	if err != nil {
		return err
	}
	leaving := target == user
	if !leaving && !actor.Can(OrgRemove) {
		return ErrForbidden
	}
	old, err := roleOf(ctx, tx, org, target)
	if err != nil {
		return err
	}
	if !leaving && !actor.CanManage(old) {
		return ErrForbidden
	}
	if old == Owner {
		if n, err := owners(ctx, tx, org); err != nil {
			return err
		} else if n <= 1 {
			return ErrLastOwner
		}
	}
	if _, err := tx.Exec(ctx, `DELETE FROM organization_members WHERE org_id = $1 AND user_id = $2`, org, target); err != nil {
		return err
	}
	if leaving {
		return Log(ctx, tx, Entry{Org: org, Actor: UserActor(user), Action: LogMemberLeft, Detail: map[string]any{"role": old}})
	}
	return Log(ctx, tx, Entry{Org: org, Actor: UserActor(user), Action: LogMemberRemoved, Detail: map[string]any{"user_id": target, "role": old}})
}

// Made is what a member made in an organization's apps that still works:
// keys and publish URLs. Keys belong to the app, not the person, and
// Wakeshift can't know who copied a key that was shown once, so removing
// someone shows these with Revoke and Rotate beside each.
type Made struct {
	Keys        []MadeKey        `json:"keys"`
	PublishURLs []MadePublishURL `json:"publish_urls"`
}

// MadeKey is a live key a member made.
type MadeKey struct {
	AppID     string    `json:"app_id"`
	KeyID     string    `json:"key_id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

// MadePublishURL is a live publish URL a member made.
type MadePublishURL struct {
	AppID     string    `json:"app_id"`
	ID        string    `json:"id"`
	Topic     string    `json:"topic"`
	CreatedAt time.Time `json:"created_at"`
}

// MadeBy lists the live keys and publish URLs target made in org's apps,
// for someone who may remove them (user).
func MadeBy(ctx context.Context, tx pgx.Tx, user, org, target string) (Made, error) {
	actor, err := Check(ctx, tx, user, org, OrgRead)
	if err != nil {
		return Made{}, err
	}
	old, err := roleOf(ctx, tx, org, target)
	if err != nil {
		return Made{}, err
	}
	if target != user && !(actor.Can(OrgRemove) && actor.CanManage(old)) {
		return Made{}, ErrForbidden
	}
	out := Made{Keys: []MadeKey{}, PublishURLs: []MadePublishURL{}}
	rows, err := tx.Query(ctx, `SELECT k.app_id, k.key_id, k.name, k.created_at FROM app_keys k JOIN apps a ON a.id = k.app_id
		WHERE a.org_id = $1 AND k.created_by = 'user:' || $2::text AND k.revoked_at IS NULL ORDER BY k.app_id, k.created_at`, org, target)
	if err != nil {
		return out, err
	}
	if out.Keys, err = pgx.CollectRows(rows, pgx.RowToStructByPos[MadeKey]); err != nil {
		return out, err
	}
	rows, err = tx.Query(ctx, `SELECT u.app_id, u.id::text, coalesce(u.event, u.topic), u.created_at FROM topic_publish_urls u JOIN apps a ON a.id = u.app_id
		WHERE a.org_id = $1 AND u.created_by = 'user:' || $2::text AND u.revoked_at IS NULL ORDER BY u.app_id, u.created_at`, org, target)
	if err != nil {
		return out, err
	}
	out.PublishURLs, err = pgx.CollectRows(rows, pgx.RowToStructByPos[MadePublishURL])
	if out.Keys == nil {
		out.Keys = []MadeKey{}
	}
	if out.PublishURLs == nil {
		out.PublishURLs = []MadePublishURL{}
	}
	return out, err
}

// LogItem is an organization's log entry, as its members see it.
type LogItem struct {
	ID         int64           `json:"id"`
	At         time.Time       `json:"at"`
	Actor      string          `json:"actor"`
	ActorEmail string          `json:"actor_email,omitempty"`
	Action     string          `json:"action"`
	Detail     json.RawMessage `json:"detail"`
}

// MaxLogLimit is the most entries ListLog returns at once.
const MaxLogLimit = 100

// ListLog returns up to limit of org's log entries, newest first, older
// than before (0 for the newest), for a member.
func ListLog(ctx context.Context, tx pgx.Tx, user, org string, before int64, limit int) ([]LogItem, error) {
	if _, err := Check(ctx, tx, user, org, OrgRead); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > MaxLogLimit {
		limit = MaxLogLimit
	}
	rows, err := tx.Query(ctx, `SELECT l.id, l.at, l.actor, coalesce(u.email, ''), l.action, l.detail
		FROM org_log l LEFT JOIN users u ON l.actor = 'user:' || u.id::text
		WHERE l.org_id = $1 AND ($2 = 0 OR l.id < $2) ORDER BY l.id DESC LIMIT $3`, org, before, limit)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, pgx.RowToStructByPos[LogItem])
	if out == nil {
		out = []LogItem{}
	}
	return out, err
}
