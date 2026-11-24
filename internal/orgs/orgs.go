// Package orgs is organizations: who manages which platform-level app.
// Every app a user registers in the console belongs to one organization,
// whose members each have one role: owner, admin or developer. Every user
// has a personal organization, made at sign-up.
//
// Organizations are platform-level, like apps: no tenant_id, no row-level
// security, read before any workspace is chosen. Every change writes the
// organization's log (org_log) in the same transaction.
package orgs

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

// personalName is a personal organization's name: the user's name, else
// their email, cut to the 100 characters a name may have.
func personalName(name, email string) string {
	n := strings.TrimSpace(name)
	if n == "" {
		n = email
	}
	if utf8.RuneCountInString(n) > maxNameRunes {
		n = string([]rune(n)[:maxNameRunes])
	}
	return n
}

// maxNameRunes is how long an organization's name may be.
const maxNameRunes = 100

// CreatePersonal makes user's personal organization, with them as its
// owner, in tx (sign-up's transaction). It returns its ID. A user who
// already has one keeps it (and gets its ID).
func CreatePersonal(ctx context.Context, tx pgx.Tx, user, name, email string) (string, error) {
	var id string
	err := tx.QueryRow(ctx, `INSERT INTO organizations (name, personal_of, created_by) VALUES ($1, $2, $2)
		ON CONFLICT (personal_of) DO NOTHING RETURNING id::text`, personalName(name, email), user).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		err = tx.QueryRow(ctx, `SELECT id::text FROM organizations WHERE personal_of = $1`, user).Scan(&id)
		return id, err
	}
	if err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO organization_members (org_id, user_id, role) VALUES ($1, $2, 'owner')`, id, user); err != nil {
		return "", err
	}
	return id, Log(ctx, tx, Entry{Org: id, Actor: UserActor(user), Action: LogCreated, Detail: map[string]any{"personal": true}})
}

// RoleIn is user's role in org (not deleted), or ErrNotMember. Membership
// is read on every request, never cached: a change of role or a removal
// applies to the member's next request. With lock, the membership row is
// locked until tx ends (a change to it waits).
func RoleIn(ctx context.Context, tx pgx.Tx, user, org string, lock bool) (Role, error) {
	if !validID(org) {
		return "", ErrNotMember
	}
	q := `SELECT m.role FROM organization_members m JOIN organizations o ON o.id = m.org_id AND o.deleted_at IS NULL
		WHERE m.org_id = $1 AND m.user_id = $2`
	if lock {
		q += ` FOR SHARE OF m`
	}
	var r Role
	err := tx.QueryRow(ctx, q, org, user).Scan(&r)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotMember
	}
	return r, err
}

// Check is user's role in org if it allows a: ErrNotMember (404) for a
// non-member, ErrForbidden (403) for a member whose role doesn't.
func Check(ctx context.Context, tx pgx.Tx, user, org string, a Action) (Role, error) {
	r, err := RoleIn(ctx, tx, user, org, false)
	if err != nil {
		return r, err
	}
	if !r.Can(a) {
		return r, ErrForbidden
	}
	return r, nil
}

// Default is the organization the console opens for user when it names
// none (an old console during a deploy): their personal one, else the
// first they joined. ErrNotMember if they have none.
func Default(ctx context.Context, tx pgx.Tx, user string) (string, error) {
	var id string
	err := tx.QueryRow(ctx, `SELECT o.id::text FROM organization_members m JOIN organizations o ON o.id = m.org_id AND o.deleted_at IS NULL
		WHERE m.user_id = $1 ORDER BY o.personal_of = $1 DESC NULLS LAST, m.created_at, o.id LIMIT 1`, user).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotMember
	}
	return id, err
}

// validID says whether s could be an organization's ID (a UUID), so a
// malformed one reads as not found rather than as a database error.
func validID(s string) bool { return idRE.MatchString(s) }

var idRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
