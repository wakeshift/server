package apps

// Keys waiting on the waitlist (139_waitlist_keys.sql). @wakeshift/cli's
// init polls GET /api/apps/v1/app with the key it made until the person
// creates the app. Past the sign-up cap the person lands on the waitlist
// instead, and the page holds the link's public key against their entry
// (HoldPendingKey). An assertion that key signs is then refused with
// ErrWaitlisted (403 waitlisted), so init stops at once and says why.
//
// A pending key authenticates nothing: Authenticate only ever refuses with
// it, and only after the signature is verified, so the answer reaches only
// the holder of the private key, and says nothing about anyone else's
// entry. A key no app has and no entry holds is refused as before.

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/wakeshift/server/internal/appkey"
	"github.com/wakeshift/server/internal/jws"
)

const (
	// PendingKeyLife is how long a pending key is held.
	PendingKeyLife = 24 * time.Hour
	// MaxPendingKeys is how many unexpired keys an entry may hold.
	MaxPendingKeys = 3
)

var (
	// ErrWaitlisted refuses a pending key's assertion: 403 waitlisted, with
	// this text (the CLI shows it to the person).
	ErrWaitlisted = errors.New("you're on Wakeshift's waitlist, so the app wasn't created; Wakeshift will email you when your account is ready: run init again then")
	// ErrNoPendingEntry: the entry is gone, signed up, or invited (its next
	// sign-in makes the account, so there's nothing to wait for).
	ErrNoPendingEntry = errors.New("apps: no waitlist entry waiting")
	// ErrTooManyPendingKeys: the entry holds MaxPendingKeys unexpired keys.
	ErrTooManyPendingKeys = errors.New("apps: too many pending keys")
)

// HoldPendingKey holds pub (from ParsePublicKey) against waitlist entry
// entry for PendingKeyLife, or holds it again, from now. A key some app has
// (or a deleted app had), or another entry holds, is ErrKeyUsed; an entry
// that isn't waiting is ErrNoPendingEntry; one holding MaxPendingKeys
// others is ErrTooManyPendingKeys. It reports whether the key is new to
// the entry.
func HoldPendingKey(ctx context.Context, pool *pgxpool.Pool, entry string, pub ed25519.PublicKey, now time.Time) (bool, error) {
	kid := appkey.KeyID(pub)
	var fresh bool
	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		// The entry's row, locked, so two requests can't both take the last
		// place.
		var waiting bool
		err := tx.QueryRow(ctx, `SELECT signed_up_at IS NULL AND invited_at IS NULL FROM waitlist WHERE id = $1 FOR UPDATE`, entry).Scan(&waiting)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && !waiting) {
			return ErrNoPendingEntry
		}
		if err != nil {
			return err
		}
		if used, err := kidUsed(ctx, tx, pub); err != nil {
			return err
		} else if used {
			return ErrKeyUsed
		}
		if _, err := tx.Exec(ctx, `DELETE FROM waitlist_keys WHERE entry_id = $1 AND expires_at <= $2`, entry, now); err != nil {
			return err
		}
		var owner string
		err = tx.QueryRow(ctx, `SELECT entry_id::text FROM waitlist_keys WHERE kid = $1 FOR UPDATE`, kid).Scan(&owner)
		switch {
		case err == nil && owner != entry:
			return ErrKeyUsed
		case err == nil:
			_, err = tx.Exec(ctx, `UPDATE waitlist_keys SET expires_at = $2 WHERE kid = $1`, kid, now.Add(PendingKeyLife))
			return err
		case !errors.Is(err, pgx.ErrNoRows):
			return err
		}
		var held int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM waitlist_keys WHERE entry_id = $1`, entry).Scan(&held); err != nil {
			return err
		}
		if held >= MaxPendingKeys {
			return ErrTooManyPendingKeys
		}
		// Another entry may have held it since the check: the primary key
		// refuses the second.
		tag, err := tx.Exec(ctx, `INSERT INTO waitlist_keys (kid, entry_id, public_key, created_at, expires_at) VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (kid) DO NOTHING`, kid, entry, []byte(pub), now, now.Add(PendingKeyLife))
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrKeyUsed
		}
		fresh = true
		return nil
	})
	return fresh, err
}

// pendingKey answers an assertion whose kid no app's key has: ErrWaitlisted
// if a waiting entry holds the key, the assertion is signed by it and is
// otherwise valid (audience and lifetime; its ID isn't recorded, since
// nothing is authenticated), else nil (the caller refuses it as an
// unknown key). The first refusal is recorded on the key (refused_at).
func (r *Registry) pendingKey(ctx context.Context, token, kid string, now time.Time) error {
	var pub []byte
	var entry string
	err := r.Pool.QueryRow(ctx, `SELECT k.public_key, k.entry_id::text FROM waitlist_keys k JOIN waitlist w ON w.id = k.entry_id
		WHERE k.kid = $1 AND k.expires_at > $2 AND w.signed_up_at IS NULL AND w.invited_at IS NULL`, kid, now).Scan(&pub, &entry)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("apps: reading a pending key: %w", err)
	}
	var a Assertion
	if _, err := jws.Verify(token, "app+jwt", func(jws.Header) ed25519.PublicKey { return pub }, &a); err != nil ||
		appkey.KeyID(pub) != kid || a.Issuer != kid || a.Subject != kid || a.Audience != r.AssertionAudience() ||
		now.Unix() >= a.Expires || a.Expires-a.IssuedAt > int64(maxAssertionLife.Seconds()) || a.IssuedAt > now.Add(time.Minute).Unix() {
		return nil
	}
	tag, err := r.Pool.Exec(ctx, `UPDATE waitlist_keys SET refused_at = $2 WHERE kid = $1 AND refused_at IS NULL`, kid, now)
	if err != nil {
		slog.Error("apps: recording a pending key's refusal", "err", err)
	} else if tag.RowsAffected() == 1 {
		slog.Info("apps: told a key held on the waitlist that its person is waitlisted", "entry", entry, "key_id", kid[:8])
	}
	return ErrWaitlisted
}

// forgetPendingKey deletes kid's pending key, in the transaction that
// registers it as an app's key.
func forgetPendingKey(ctx context.Context, tx pgx.Tx, kid string) error {
	_, err := tx.Exec(ctx, `DELETE FROM waitlist_keys WHERE kid = $1`, kid)
	return err
}

// PrunePendingKeys deletes expired pending keys, at most limit, for the
// expiry pass.
func PrunePendingKeys(ctx context.Context, pool *pgxpool.Pool, limit int) (int64, error) {
	tag, err := pool.Exec(ctx, `DELETE FROM waitlist_keys WHERE kid IN (SELECT kid FROM waitlist_keys WHERE expires_at <= now() LIMIT $1)`, limit)
	return tag.RowsAffected(), err
}
