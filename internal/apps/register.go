package apps

// Registering a key made elsewhere: @wakeshift/cli generates a key on the
// developer's machine, writes it to .env and sends only its public half, in
// a console link's fragment. The person, signed in, confirms on the
// console's page, which posts the public key here: a new app with it as its
// only key (CreateWithKey), or one more key for an app (RegisterKey).
// Wakeshift never sees the seed, as it keeps only public halves anyway.
//
// A public key is checked strictly before it's stored: 32 bytes in
// unpadded base64url (the encoding the SDKs' vectors use), a canonical
// encoding of a point on the curve, and not of small order (a small-order
// key would verify signatures anyone can forge). Its kid must be new across
// every app, revoked keys included, so a link can't be replayed onto
// another app.
//
// Both write app_log in the same transaction, with detail.via ("cli"), and
// count against the abuse limits (limits.go).

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"errors"

	"filippo.io/edwards25519"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/wakeshift/server/internal/appkey"
)

// ViaCLI is detail.via for an app or key registered from @wakeshift/cli's
// link.
const ViaCLI = "cli"

var (
	// ErrBadPublicKey is a public key that isn't a usable Ed25519 key.
	ErrBadPublicKey = errors.New("apps: the public key isn't a valid Ed25519 public key (32 bytes, base64url)")
	// ErrKeyUsed is a public key whose kid some app's key already has.
	ErrKeyUsed = errors.New("apps: that key is registered already")
)

// ParsePublicKey reads a public key as the CLI sends it: 43 characters of
// unpadded base64url, decoding to a canonical, non-small-order Ed25519
// point. Anything else is ErrBadPublicKey.
func ParsePublicKey(s string) (ed25519.PublicKey, error) {
	if len(s) != base64.RawURLEncoding.EncodedLen(ed25519.PublicKeySize) {
		return nil, ErrBadPublicKey
	}
	b, err := base64.RawURLEncoding.Strict().DecodeString(s)
	if err != nil || len(b) != ed25519.PublicKeySize {
		return nil, ErrBadPublicKey
	}
	if !validPoint(b) {
		return nil, ErrBadPublicKey
	}
	return ed25519.PublicKey(b), nil
}

// validPoint reports whether b is the canonical encoding of a point of
// large order. SetBytes accepts some non-canonical encodings, so the point
// must encode back to b exactly; [8]P is the identity exactly for the
// eight small-order points.
func validPoint(b []byte) bool {
	p, err := new(edwards25519.Point).SetBytes(b)
	if err != nil || string(p.Bytes()) != string(b) {
		return false
	}
	return new(edwards25519.Point).MultByCofactor(p).Equal(edwards25519.NewIdentityPoint()) == 0
}

// kidUsed reports whether any app's key, revoked or not, has pub's kid,
// or had it before its app was deleted (deleted_key_ids).
func kidUsed(ctx context.Context, tx pgx.Tx, pub ed25519.PublicKey) (bool, error) {
	kid := appkey.KeyID(pub)
	var used bool
	err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM app_keys WHERE kid = $1) OR EXISTS (SELECT 1 FROM deleted_key_ids WHERE kid_hash = $2)`,
		kid, kidHash(kid)).Scan(&used)
	return used, err
}

// kidHash is a deleted key's fingerprint as deleted_key_ids keeps it: the
// SHA-256 of its kid, one-way, so nothing of the key is kept.
func kidHash(kid string) []byte {
	sum := sha256.Sum256([]byte(kid))
	return sum[:]
}

// kidSQL is a key's kid in SQL (k: app_keys): its kid, or for a key the
// previous revision made without one, the same from its public key
// (appkey.KeyID, 090_app_keys_kid.sql).
const kidSQL = `coalesce(k.kid, translate(rtrim(encode(substring(sha256(k.public_key) FROM 1 FOR 16), 'base64'), '='), '+/', '-_'))`

// DeleteKeys deletes every key of app id, revoked or not (a deleted app is
// kept without its keys, as the privacy policy says), keeping only each
// one's fingerprint (deleted_key_ids), so it can't be registered again. It
// returns the key IDs of the ones that were live (the app log names them).
func DeleteKeys(ctx context.Context, tx pgx.Tx, id string) ([]string, error) {
	if _, err := tx.Exec(ctx, `INSERT INTO deleted_key_ids (kid_hash) SELECT sha256(convert_to(`+kidSQL+`, 'UTF8')) FROM app_keys k WHERE k.app_id = $1
		ON CONFLICT DO NOTHING`, id); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `DELETE FROM app_keys WHERE app_id = $1 RETURNING key_id, revoked_at IS NULL`, id)
	if err != nil {
		return nil, err
	}
	type deletedKey struct {
		ID   string
		Live bool
	}
	keys, err := pgx.CollectRows(rows, pgx.RowToStructByPos[deletedKey])
	if err != nil {
		return nil, err
	}
	revoked := []string{}
	for _, k := range keys {
		if k.Live {
			revoked = append(revoked, k.ID)
		}
	}
	return revoked, nil
}

// insertPublicKey stores pub as one of app id's keys. A kid taken in the
// meantime (app_keys_kid is unique) is ErrKeyUsed, in a savepoint so the
// caller's transaction isn't aborted by it.
func insertPublicKey(ctx context.Context, tx pgx.Tx, id, name string, pub ed25519.PublicKey, by string) (Key, error) {
	sp, err := tx.Begin(ctx)
	if err != nil {
		return Key{}, err
	}
	defer sp.Rollback(ctx)
	k, err := scanKey(sp.QueryRow(ctx, `INSERT INTO app_keys (app_id, key_id, kid, name, public_key, created_by) VALUES ($1, $2, $3, $4, $5, $6) RETURNING `+keyColumns,
		id, KeyID(pub), appkey.KeyID(pub), name, []byte(pub), by))
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return Key{}, ErrKeyUsed
	}
	if err != nil {
		return Key{}, err
	}
	// Held on the waitlist before its person was admitted: an app's key now.
	if err := forgetPendingKey(ctx, sp, appkey.KeyID(pub)); err != nil {
		return Key{}, err
	}
	return k, sp.Commit(ctx)
}

// CreateWithKey registers a test app as Create does, but with pub (from
// ParsePublicKey) as its only key, made on the developer's machine, so
// nothing secret is returned. The log's app.created and app.key_created
// carry via. It counts as a public-key registration (MaxRegistrationsPerHour)
// as well as an app (MaxAppsPerHour, MaxAppsPerDay).
func (r *Registry) CreateWithKey(ctx context.Context, tx pgx.Tx, user, org string, n NewApp, pub ed25519.PublicKey, via string) (Owned, Key, error) {
	o, k, _, err := r.create(ctx, tx, user, org, n, pub, via)
	return o, k, err
}

// RegisterKey adds pub (from ParsePublicKey) to app id as user, whose role
// must allow managing keys, as CreateKey does, but with a key made on the
// developer's machine: nothing secret is returned, and the log's
// app.key_created carries via.
func (r *Registry) RegisterKey(ctx context.Context, tx pgx.Tx, user, id, name string, pub ed25519.PublicKey, via string) (Key, error) {
	name, err := validKeyName(name)
	if err != nil {
		return Key{}, err
	}
	// The account's lock first, then the app's row, as create takes the
	// account's before the organization's.
	if err := lockAccount(ctx, tx, user); err != nil {
		return Key{}, err
	}
	if err := r.forNewKey(ctx, tx, user, id); err != nil {
		return Key{}, err
	}
	if err := checkRegistrationRate(ctx, tx, user); err != nil {
		return Key{}, err
	}
	if err := checkKeyRate(ctx, tx, id); err != nil {
		return Key{}, err
	}
	if n, err := countKeys(ctx, tx, id); err != nil {
		return Key{}, err
	} else if n >= MaxKeys {
		return Key{}, ErrTooManyKeys
	}
	if used, err := kidUsed(ctx, tx, pub); err != nil {
		return Key{}, err
	} else if used {
		return Key{}, ErrKeyUsed
	}
	k, err := insertPublicKey(ctx, tx, id, name, pub, userActor(user))
	if err != nil {
		return Key{}, err
	}
	return k, Log(ctx, tx, LogEntry{AppID: id, Actor: userActor(user), Action: LogKeyCreated, KeyID: k.ID,
		Detail: map[string]string{"name": k.Name, "via": via}})
}
