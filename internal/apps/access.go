package apps

// Console apps belong to organizations: every reader and changer of one
// is a member of its organization, with a role that allows the action.
// Access is the one check; every registry method that reads or changes a
// console app calls it with its own action, so a handler can't skip the
// role check.

import (
	"context"
	"crypto/ed25519"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/wakeshift/server/internal/appkey"
	"github.com/wakeshift/server/internal/orgs"
)

// Owned is an app as a member of its organization sees it in the console.
type Owned struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	MCPURL    string    `json:"mcp_url"`
	CreatedAt time.Time `json:"created_at"`
	// Keys is how many of its keys aren't revoked (ListKeys has them).
	Keys              int        `json:"keys"`
	ManifestUpdatedAt *time.Time `json:"manifest_updated_at,omitempty"`
	// DeletedAt is set for a deleted app: it has no key, and its
	// organization's members can still read its log.
	DeletedAt *time.Time `json:"deleted_at,omitempty"`
	// OrgID is the organization the app belongs to; Role is the reader's
	// role in it (the console shows only what the role allows).
	OrgID string    `json:"org_id"`
	Role  orgs.Role `json:"role"`
	// SuspendedAt is set while we've suspended the app: its keys and
	// publish URLs are refused. SuspendedReason is ours, written for its
	// owners.
	SuspendedAt     *time.Time `json:"suspended_at,omitempty"`
	SuspendedReason string     `json:"suspended_reason,omitempty"`
}

// ownedColumns and ownedFrom read apps as a member ($1) of their
// organization sees them: an app outside the user's organizations, or in a
// deleted one, has no row.
const (
	ownedColumns = `apps.id, apps.name, coalesce(apps.mcp_url, ''), apps.created_at,
	(SELECT count(*) FROM app_keys k WHERE k.app_id = apps.id AND k.revoked_at IS NULL), apps.manifest_updated_at, apps.deleted_at,
	apps.org_id::text, m.role, apps.suspended_at, coalesce(apps.suspended_reason, '')`
	ownedFrom = ` FROM apps JOIN organizations o ON o.id = apps.org_id AND o.deleted_at IS NULL
	JOIN organization_members m ON m.org_id = apps.org_id AND m.user_id = $1`
)

// KeyID names a public key in the console, its routes and the app's log:
// the first 6 bytes of its SHA-256, base64url (8 characters), which is the
// start of its kid (appkey.KeyID).
func KeyID(pub []byte) string { return appkey.ShortID(pub) }

func scanOwned(row pgx.Row) (Owned, error) {
	var o Owned
	err := row.Scan(&o.ID, &o.Name, &o.MCPURL, &o.CreatedAt, &o.Keys, &o.ManifestUpdatedAt, &o.DeletedAt, &o.OrgID, &o.Role,
		&o.SuspendedAt, &o.SuspendedReason)
	if errors.Is(err, pgx.ErrNoRows) {
		return o, ErrNotOwner
	}
	return o, err
}

// Access is the one authorization check for an app in the console:
// user, a member of app id's organization, sees it if their role allows
// a. No membership (or no such app, or one of ours) is ErrNotOwner, 404,
// alike; a role that doesn't allow a is orgs.ErrForbidden, 403.
// Membership is read every time, never cached, so a removal or a change
// of role applies to the next request.
func (r *Registry) Access(ctx context.Context, tx pgx.Tx, user, id string, a orgs.Action) (Owned, error) {
	return r.access(ctx, tx, user, id, a, false)
}

// access is Access; with lock, the app's row is locked until tx ends, so
// changes to one app wait their turn.
func (r *Registry) access(ctx context.Context, tx pgx.Tx, user, id string, a orgs.Action, lock bool) (Owned, error) {
	q := `SELECT ` + ownedColumns + ownedFrom + ` WHERE apps.id = $2`
	if lock {
		q += ` FOR UPDATE OF apps`
	}
	o, err := scanOwned(tx.QueryRow(ctx, q, user, id))
	if err != nil {
		return o, err
	}
	if !o.Role.Can(a) {
		return o, orgs.ErrForbidden
	}
	return o, nil
}

// forChange is access for a change that a deleted app refuses
// (ErrDeleted), with the app's row locked.
func (r *Registry) forChange(ctx context.Context, tx pgx.Tx, user, id string, a orgs.Action) (Owned, error) {
	o, err := r.access(ctx, tx, user, id, a, true)
	if err == nil && o.DeletedAt != nil {
		err = ErrDeleted
	}
	return o, err
}

// orgFor is the organization user means: org, or with none their default
// one.
func orgFor(ctx context.Context, tx pgx.Tx, user, org string) (string, error) {
	if org != "" {
		return org, nil
	}
	return orgs.Default(ctx, tx, user)
}

// Create registers a test app in tx, in organization org ("": the user's
// default) as user, a member whose role may create apps there. It returns
// the app and its first key, named "Default" (shown once; Wakeshift keeps
// only the public half). n must have passed Validate. An organization has
// at most MaxOwned apps that aren't deleted, and an account creates at most
// MaxAppsPerHour and MaxAppsPerDay (RateLimitError).
func (r *Registry) Create(ctx context.Context, tx pgx.Tx, user, org string, n NewApp) (Owned, Key, string, error) {
	return r.create(ctx, tx, user, org, n, nil, "")
}

// create is Create, or with pub (CreateWithKey) the app's only key is pub,
// registered from via, and no key is returned.
func (r *Registry) create(ctx context.Context, tx pgx.Tx, user, org string, n NewApp, pub ed25519.PublicKey, via string) (Owned, Key, string, error) {
	org, err := orgFor(ctx, tx, user, org)
	if err != nil {
		return Owned{}, Key{}, "", err
	}
	// The account's creations go one at a time, so the rate limits
	// (limits.go) can't be raced past; always before the organization's
	// locks.
	if err := lockAccount(ctx, tx, user); err != nil {
		return Owned{}, Key{}, "", err
	}
	// Locked (shared): the organization can't be deleted, nor the member
	// removed, until this commits.
	role, err := orgs.RoleIn(ctx, tx, user, org, true)
	if err != nil {
		return Owned{}, Key{}, "", err
	}
	if !role.Can(orgs.AppCreate) {
		return Owned{}, Key{}, "", orgs.ErrForbidden
	}
	if err := checkAppRate(ctx, tx, user); err != nil {
		return Owned{}, Key{}, "", err
	}
	if pub != nil {
		if err := checkRegistrationRate(ctx, tx, user); err != nil {
			return Owned{}, Key{}, "", err
		}
		if used, err := kidUsed(ctx, tx, pub); err != nil {
			return Owned{}, Key{}, "", err
		} else if used {
			return Owned{}, Key{}, "", ErrKeyUsed
		}
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('apps.org:' || $1))`, org); err != nil {
		return Owned{}, Key{}, "", err
	}
	var count int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM apps WHERE org_id = $1 AND deleted_at IS NULL`, org).Scan(&count); err != nil {
		return Owned{}, Key{}, "", err
	}
	if count >= MaxOwned {
		return Owned{}, Key{}, "", ErrTooMany
	}
	// In a savepoint: a taken ID fails this insert without aborting the
	// caller's transaction.
	sp, err := tx.Begin(ctx)
	if err != nil {
		return Owned{}, Key{}, "", err
	}
	defer sp.Rollback(ctx)
	// The organization (org_id) says who manages the app, and created_by
	// who made it.
	_, err = sp.Exec(ctx, `INSERT INTO apps (id, name, mcp_url, org_id, created_by) VALUES ($1, $2, nullif($3, ''), $4, $5)`,
		n.ID, n.Name, n.MCPURL, org, user)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return Owned{}, Key{}, "", ErrTaken
	}
	if err != nil {
		return Owned{}, Key{}, "", err
	}
	if err := sp.Commit(ctx); err != nil {
		return Owned{}, Key{}, "", err
	}
	keyName := n.KeyName
	if keyName == "" { // a caller that skipped Validate
		keyName = "Default"
	}
	var k Key
	var key string
	if pub != nil {
		k, err = insertPublicKey(ctx, tx, n.ID, keyName, pub, userActor(user))
	} else {
		k, key, err = r.insertKey(ctx, tx, n.ID, keyName, userActor(user))
	}
	if err != nil {
		return Owned{}, Key{}, "", err
	}
	o, err := r.access(ctx, tx, user, n.ID, orgs.AppRead, false)
	if err != nil {
		return o, k, "", err
	}
	if pub == nil {
		return o, k, key, Log(ctx, tx, LogEntry{AppID: o.ID, Actor: userActor(user), Action: LogCreated, KeyID: k.ID,
			Detail: map[string]string{"name": o.Name, "mcp_url": o.MCPURL}})
	}
	if err := Log(ctx, tx, LogEntry{AppID: o.ID, Actor: userActor(user), Action: LogCreated, KeyID: k.ID,
		Detail: map[string]string{"name": o.Name, "mcp_url": o.MCPURL, "via": via}}); err != nil {
		return o, k, "", err
	}
	return o, k, "", Log(ctx, tx, LogEntry{AppID: o.ID, Actor: userActor(user), Action: LogKeyCreated, KeyID: k.ID,
		Detail: map[string]string{"name": k.Name, "via": via}})
}

// GetOwned returns app id (deleted or not) if user is a member of its
// organization: ErrNotOwner for anyone else, or no such app.
func (r *Registry) GetOwned(ctx context.Context, tx pgx.Tx, user, id string) (Owned, error) {
	return r.access(ctx, tx, user, id, orgs.AppRead, false)
}

// ListOwned returns organization org's apps ("": user's default
// organization), newest first, then the deleted ones, and the
// organization's ID: orgs.ErrNotMember unless user is a member.
func (r *Registry) ListOwned(ctx context.Context, tx pgx.Tx, user, org string) ([]Owned, string, error) {
	org, err := orgFor(ctx, tx, user, org)
	if err != nil {
		return nil, "", err
	}
	if _, err := orgs.Check(ctx, tx, user, org, orgs.AppRead); err != nil {
		return nil, "", err
	}
	rows, err := tx.Query(ctx, `SELECT `+ownedColumns+ownedFrom+` WHERE apps.org_id = $2 ORDER BY apps.deleted_at IS NOT NULL, apps.created_at DESC`, user, org)
	if err != nil {
		return nil, "", err
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Owned, error) { return scanOwned(row) })
	if out == nil {
		out = []Owned{}
	}
	return out, org, err
}

// Settings is what a member (admin and up) may change about an app after
// registering it: its name and MCP URL (nil leaves one as it is). The ID
// is permanent.
type Settings struct {
	Name   *string `json:"name"`
	MCPURL *string `json:"mcp_url"`
}

// Update changes app id's name or MCP URL as user, logging what changed.
func (r *Registry) Update(ctx context.Context, tx pgx.Tx, user, id string, in Settings) (Owned, error) {
	old, err := r.forChange(ctx, tx, user, id, orgs.AppUpdate)
	if err != nil {
		return old, err
	}
	n := NewApp{ID: old.ID, Name: old.Name, MCPURL: old.MCPURL}
	if in.Name != nil {
		n.Name = *in.Name
	}
	if in.MCPURL != nil {
		n.MCPURL = *in.MCPURL
	}
	if err := n.validateSettings(); err != nil {
		return old, err
	}
	changes := []string{}
	if n.Name != old.Name {
		changes = append(changes, "name")
	}
	if n.MCPURL != old.MCPURL {
		changes = append(changes, "mcp_url")
	}
	if len(changes) == 0 {
		return old, nil
	}
	if _, err := tx.Exec(ctx, `UPDATE apps SET name = $2, mcp_url = nullif($3, '') WHERE id = $1`, id, n.Name, n.MCPURL); err != nil {
		return old, err
	}
	o, err := r.access(ctx, tx, user, id, orgs.AppRead, false)
	if err != nil {
		return o, err
	}
	return o, Log(ctx, tx, LogEntry{AppID: id, Actor: userActor(user), Action: LogUpdated,
		Detail: map[string]any{"changes": changes, "name": o.Name, "mcp_url": o.MCPURL, "old_name": old.Name, "old_mcp_url": old.MCPURL}})
}

// Delete deletes app id as user (admin and up): all its keys stop working
// at once. The row stays and the ID isn't reused. Deleting what Wakeshift
// keeps for the app's users is recorded in the same transaction
// (app_deletions, 054_app_deletions.sql) and done by the expiry pass
// (appapi.FinishAppDeletions), which retries until it's done; the app's
// log goes LogRetention after the deletion. The caller revokes the app's
// grants in the workspaces it kept (Workspaces) at once; deleting an app
// that's already deleted reports already, so the caller can finish that.
func (r *Registry) Delete(ctx context.Context, tx pgx.Tx, user, id string) (o Owned, already bool, err error) {
	o, err = r.access(ctx, tx, user, id, orgs.AppDelete, true)
	if err != nil || o.DeletedAt != nil {
		return o, err == nil, err
	}
	if err := deleteLocked(ctx, tx, id, o.Name, userActor(user)); err != nil {
		return o, false, err
	}
	o, err = r.access(ctx, tx, user, id, orgs.AppRead, false)
	return o, false, err
}

// deleteLocked deletes app id (named name), whose row the caller has
// locked, for actor: its keys are deleted (DeleteKeys; the log names the
// live ones it revoked) and its publish URLs stop, and deleting its users'
// data is recorded (app_deletions), with the log entry, in the caller's
// transaction.
func deleteLocked(ctx context.Context, tx pgx.Tx, id, name, actor string) error {
	revoked, err := DeleteKeys(ctx, tx, id)
	if err != nil {
		return err
	}
	// Its publish URLs stop with it, under the same lock (making or
	// revoking one holds the app's row too).
	urls, err := tx.Exec(ctx, `UPDATE topic_publish_urls SET revoked_at = now(), signing_secret = NULL, signing_fingerprint = NULL,
		previous_signing_secret = NULL, previous_signing_until = NULL WHERE app_id = $1 AND revoked_at IS NULL`, id)
	if err != nil {
		return err
	}
	var deleted time.Time
	if err := tx.QueryRow(ctx, `UPDATE apps SET deleted_at = now() WHERE id = $1 RETURNING deleted_at`, id).Scan(&deleted); err != nil {
		return err
	}
	// Its users' data goes with it: recorded here, so the deletion and the
	// promise to finish it commit together.
	if _, err := tx.Exec(ctx, `INSERT INTO app_deletions (app_id, requested_at) VALUES ($1, $2) ON CONFLICT DO NOTHING`, id, deleted); err != nil {
		return err
	}
	if revoked == nil {
		revoked = []string{}
	}
	return Log(ctx, tx, LogEntry{AppID: id, Actor: actor, Action: LogDeleted,
		Detail: map[string]any{"name": name, "keys_revoked": revoked, "publish_urls_revoked": urls.RowsAffected(),
			"log_deleted_after": deleted.Add(LogRetention).UTC().Format(time.RFC3339)}})
}
