package apps

// An app's keys (031_app_keys.sql): several named keys per
// app, each shown once when it's made, listed by name and key ID, and
// revoked on its own. Wakeshift keeps only the public halves. A request
// signed by any key that isn't revoked authenticates the app
// (Authenticate); revoking a key stops it at once, since keys are read on
// every request.
//
// Making, rotating and revoking a key each write the app's log in the same
// transaction.

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/appkey"
	"github.com/wakeshift/server/internal/orgs"
)

// MaxKeys is how many keys an app may have that aren't revoked.
const MaxKeys = 10

// keyUseResolution is how stale a key's last_used_at may get: a key used
// all the time is written at most once a minute.
const keyUseResolution = time.Minute

var (
	ErrNoKey       = errors.New("apps: no such key")
	ErrTooManyKeys = fmt.Errorf("apps: at most %d keys per app", MaxKeys)
	// ErrDeleted is a change asked of a deleted app.
	ErrDeleted = errors.New("apps: the app is deleted")
)

// Key is one of an app's keys, as its organization's members see it (never the key).
type Key struct {
	ID         string     `json:"key_id"`
	Name       string     `json:"name"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
}

const keyColumns = `key_id, name, created_at, last_used_at, revoked_at`

func scanKey(row pgx.Row) (Key, error) {
	var k Key
	err := row.Scan(&k.ID, &k.Name, &k.CreatedAt, &k.LastUsedAt, &k.RevokedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return k, ErrNoKey
	}
	return k, err
}

// activeKey is a key Authenticate may verify with.
type activeKey struct {
	app      string
	id       string
	kid      *string // null for a key the previous revision made during a deploy
	name     string
	public   ed25519.PublicKey
	lastUsed *time.Time
}

const activeKeyColumns = `app_id, key_id, kid, name, public_key, last_used_at`

func scanActiveKey(row pgx.Row) (activeKey, error) {
	var k activeKey
	var pub []byte
	err := row.Scan(&k.app, &k.id, &k.kid, &k.name, &pub, &k.lastUsed)
	k.public = pub
	if err == nil && len(pub) != ed25519.PublicKeySize {
		err = fmt.Errorf("apps: %s's key %s is malformed", k.app, k.id)
	}
	return k, err
}

func (r *Registry) activeKeys(ctx context.Context, app string) ([]activeKey, error) {
	rows, err := r.Pool.Query(ctx, `SELECT `+activeKeyColumns+` FROM app_keys WHERE app_id = $1 AND revoked_at IS NULL ORDER BY created_at DESC LIMIT $2`,
		app, MaxKeys)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (activeKey, error) { return scanActiveKey(row) })
}

// keyByKID returns the key (not revoked) whose kid is kid: at most one,
// across every app (app_keys_kid). ok is false for none.
func (r *Registry) keyByKID(ctx context.Context, kid string) (k activeKey, ok bool, err error) {
	k, err = scanActiveKey(r.Pool.QueryRow(ctx, `SELECT `+activeKeyColumns+` FROM app_keys WHERE kid = $1 AND revoked_at IS NULL`, kid))
	if errors.Is(err, pgx.ErrNoRows) {
		return k, false, nil
	}
	return k, err == nil, err
}

// fillKID writes the kid of a key the previous revision made during a
// deploy, without one, the first time it authenticates.
func (r *Registry) fillKID(ctx context.Context, k activeKey) error {
	if k.kid != nil {
		return nil
	}
	_, err := r.Pool.Exec(ctx, `UPDATE app_keys SET kid = $3 WHERE app_id = $1 AND key_id = $2 AND kid IS NULL`, k.app, k.id, appkey.KeyID(k.public))
	return err
}

// markKeyUsed records that k signed a request: its first use once (and,
// for a console app, the log says so, in the same transaction), then at
// most once a minute, so a busy key costs a write a minute, not one a
// request.
func (r *Registry) markKeyUsed(ctx context.Context, app App, k activeKey, now time.Time) error {
	if k.lastUsed != nil {
		if now.Sub(*k.lastUsed) < keyUseResolution {
			return nil
		}
		_, err := r.Pool.Exec(ctx, `UPDATE app_keys SET last_used_at = now() WHERE app_id = $1 AND key_id = $2 AND last_used_at < now() - make_interval(secs => $3)`,
			app.ID, k.id, keyUseResolution.Seconds())
		return err
	}
	return pgx.BeginFunc(ctx, r.Pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE app_keys SET last_used_at = now() WHERE app_id = $1 AND key_id = $2 AND last_used_at IS NULL`, app.ID, k.id)
		if err != nil || tag.RowsAffected() == 0 || !app.owned {
			return err
		}
		return Log(ctx, tx, LogEntry{AppID: app.ID, Actor: "app:" + app.ID, Action: LogKeyFirstUsed, KeyID: k.id})
	})
}

// validKeyName trims a key's name, or says what's wrong with it.
func validKeyName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		name = "Untitled key"
	}
	if len(name) > 60 || strings.ContainsFunc(name, func(r rune) bool { return unicode.IsControl(r) }) {
		return "", &ValidationError{"a key's name is one line of at most 60 characters"}
	}
	return name, nil
}

// forKeys locks app id (not deleted) for a key change by user, whose role
// must allow managing keys. Concurrent changes to one app's keys wait their
// turn, so the count of keys can't be raced past.
func (r *Registry) forKeys(ctx context.Context, tx pgx.Tx, user, id string) error {
	_, err := r.forChange(ctx, tx, user, id, orgs.AppKeys)
	return err
}

// forNewKey is forKeys for a change that makes a key: a suspended app gets
// none (revoking one is still fine).
func (r *Registry) forNewKey(ctx context.Context, tx pgx.Tx, user, id string) error {
	o, err := r.forChange(ctx, tx, user, id, orgs.AppKeys)
	if err == nil && o.SuspendedAt != nil {
		err = ErrSuspended
	}
	return err
}

// insertKey makes a key for app id, returning it once.
func (r *Registry) insertKey(ctx context.Context, tx pgx.Tx, id, name, by string) (Key, string, error) {
	key, pub, err := newKey()
	if err != nil {
		return Key{}, "", err
	}
	// A fresh key never has a deleted key's kid, short of the same key; refused all the same.
	if used, err := kidUsed(ctx, tx, pub); err != nil || used {
		if err == nil {
			err = ErrKeyUsed
		}
		return Key{}, "", err
	}
	k, err := scanKey(tx.QueryRow(ctx, `INSERT INTO app_keys (app_id, key_id, kid, name, public_key, created_by) VALUES ($1, $2, $3, $4, $5, $6) RETURNING `+keyColumns,
		id, KeyID(pub), appkey.KeyID(pub), name, []byte(pub), by))
	return k, key, err
}

func countKeys(ctx context.Context, tx pgx.Tx, id string) (int, error) {
	var n int
	err := tx.QueryRow(ctx, `SELECT count(*) FROM app_keys WHERE app_id = $1 AND revoked_at IS NULL`, id).Scan(&n)
	return n, err
}

// ListKeys returns the app's keys: the ones in use, newest first, then
// the revoked ones. A deleted app's are all revoked.
func (r *Registry) ListKeys(ctx context.Context, tx pgx.Tx, user, id string) ([]Key, error) {
	if _, err := r.access(ctx, tx, user, id, orgs.AppRead, false); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `SELECT `+keyColumns+` FROM app_keys WHERE app_id = $1 ORDER BY revoked_at IS NOT NULL, coalesce(revoked_at, created_at) DESC LIMIT 100`, id)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Key, error) { return scanKey(row) })
	if out == nil {
		out = []Key{}
	}
	return out, err
}

// CreateKey makes a new key for the app, named name, and returns it
// once. The app's other keys keep working.
func (r *Registry) CreateKey(ctx context.Context, tx pgx.Tx, user, id, name string) (Key, string, error) {
	name, err := validKeyName(name)
	if err != nil {
		return Key{}, "", err
	}
	if err := r.forNewKey(ctx, tx, user, id); err != nil {
		return Key{}, "", err
	}
	if err := checkKeyRate(ctx, tx, id); err != nil {
		return Key{}, "", err
	}
	if n, err := countKeys(ctx, tx, id); err != nil {
		return Key{}, "", err
	} else if n >= MaxKeys {
		return Key{}, "", ErrTooManyKeys
	}
	k, key, err := r.insertKey(ctx, tx, id, name, userActor(user))
	if err != nil {
		return Key{}, "", err
	}
	return k, key, Log(ctx, tx, LogEntry{AppID: id, Actor: userActor(user), Action: LogKeyCreated, KeyID: k.ID, Detail: map[string]string{"name": k.Name}})
}

// RevokeKey stops one of the app's keys at once: requests it signs are
// refused from the moment this commits. Revoking a revoked key changes
// nothing (and logs nothing).
func (r *Registry) RevokeKey(ctx context.Context, tx pgx.Tx, user, id, keyID string) (Key, error) {
	if err := r.forKeys(ctx, tx, user, id); err != nil {
		return Key{}, err
	}
	k, err := scanKey(tx.QueryRow(ctx, `SELECT `+keyColumns+` FROM app_keys WHERE app_id = $1 AND key_id = $2`, id, keyID))
	if err != nil || k.RevokedAt != nil {
		return k, err
	}
	if k, err = revokeKey(ctx, tx, id, keyID, userActor(user)); err != nil {
		return k, err
	}
	return k, Log(ctx, tx, LogEntry{AppID: id, Actor: userActor(user), Action: LogKeyRevoked, KeyID: k.ID, Detail: map[string]string{"name": k.Name}})
}

func revokeKey(ctx context.Context, tx pgx.Tx, id, keyID, by string) (Key, error) {
	return scanKey(tx.QueryRow(ctx, `UPDATE app_keys SET revoked_at = now(), revoked_by = $3 WHERE app_id = $1 AND key_id = $2 RETURNING `+keyColumns, id, keyID, by))
}

// ErrKeyRevoked refuses renaming a revoked key.
var ErrKeyRevoked = errors.New("apps: the key is revoked")

// RenameKey names one of the app's keys name (validKeyName's rules) and
// logs the old and new names. A revoked key keeps its name
// (ErrKeyRevoked); the same name changes nothing (and logs nothing).
func (r *Registry) RenameKey(ctx context.Context, tx pgx.Tx, user, id, keyID, name string) (Key, error) {
	name, err := validKeyName(name)
	if err != nil {
		return Key{}, err
	}
	if err := r.forKeys(ctx, tx, user, id); err != nil {
		return Key{}, err
	}
	old, err := scanKey(tx.QueryRow(ctx, `SELECT `+keyColumns+` FROM app_keys WHERE app_id = $1 AND key_id = $2 FOR UPDATE`, id, keyID))
	switch {
	case err != nil:
		return old, err
	case old.RevokedAt != nil:
		return old, ErrKeyRevoked
	case old.Name == name:
		return old, nil
	}
	k, err := scanKey(tx.QueryRow(ctx, `UPDATE app_keys SET name = $3 WHERE app_id = $1 AND key_id = $2 RETURNING `+keyColumns, id, keyID, name))
	if err != nil {
		return k, err
	}
	return k, Log(ctx, tx, LogEntry{AppID: id, Actor: userActor(user), Action: LogKeyRenamed, KeyID: k.ID,
		Detail: map[string]string{"old_name": old.Name, "name": k.Name}})
}

// RotateKey replaces one of the app's keys with a new one of the same
// name, returned once: the old key stops working the moment this commits
// (there's no overlap; for one, create a key and revoke the old one
// later).
func (r *Registry) RotateKey(ctx context.Context, tx pgx.Tx, user, id, keyID string) (Key, string, error) {
	if err := r.forNewKey(ctx, tx, user, id); err != nil {
		return Key{}, "", err
	}
	if err := checkKeyRate(ctx, tx, id); err != nil {
		return Key{}, "", err
	}
	old, err := scanKey(tx.QueryRow(ctx, `SELECT `+keyColumns+` FROM app_keys WHERE app_id = $1 AND key_id = $2 AND revoked_at IS NULL`, id, keyID))
	if err != nil {
		return Key{}, "", err
	}
	if _, err := revokeKey(ctx, tx, id, keyID, userActor(user)); err != nil {
		return Key{}, "", err
	}
	k, key, err := r.insertKey(ctx, tx, id, old.Name, userActor(user))
	if err != nil {
		return Key{}, "", err
	}
	return k, key, Log(ctx, tx, LogEntry{AppID: id, Actor: userActor(user), Action: LogKeyRotated, KeyID: k.ID,
		Detail: map[string]string{"old_key_id": old.ID, "name": k.Name}})
}
