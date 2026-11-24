package oauth

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/dbtest"
)

// A key that stopped signing more than keyRetireAfter ago is retired: it
// leaves JWKS and stops verifying. The key that replaced it, and a key
// still in its grace period, stay.
func TestOldSigningKeysRetire(t *testing.T) {
	ctx := context.Background()
	d := dbtest.New(t)
	k, err := newKeyStore(d.Pool, bytes.Repeat([]byte("o"), 32))
	if err != nil {
		t.Fatal(err)
	}
	// Three keys: made 200, 100 and 2 days ago (the last rotation was 2
	// days ago, so the 100-day-old key stopped signing then).
	ages := []time.Duration{200 * 24 * time.Hour, 100 * 24 * time.Hour, 2 * 24 * time.Hour}
	var kids []string
	for _, age := range ages {
		err := pgx.BeginFunc(ctx, d.Pool, func(tx pgx.Tx) error {
			key, err := k.create(ctx, tx)
			if err != nil {
				return err
			}
			kids = append(kids, key.kid)
			_, err = tx.Exec(ctx, `UPDATE oauth_signing_keys SET created_at = now() - make_interval(secs => $2) WHERE kid = $1`, key.kid, age.Seconds())
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	keys, err := k.load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 || keys[0].kid != kids[2] {
		t.Fatalf("left %d keys, want only the newest", len(keys))
	}
	var retired int
	if err := d.Pool.QueryRow(ctx, `SELECT count(*) FROM oauth_signing_keys WHERE retired_at IS NOT NULL AND kid = ANY($1)`, kids[:2]).Scan(&retired); err != nil || retired != 2 {
		t.Fatalf("retired %d, %v", retired, err)
	}

	// A key replaced only an hour ago (past its warmup) still verifies.
	err = pgx.BeginFunc(ctx, d.Pool, func(tx pgx.Tx) error {
		key, err := k.create(ctx, tx)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE oauth_signing_keys SET created_at = now() - interval '1 hour' WHERE kid = $1`, key.kid)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if keys, err = k.load(ctx); err != nil || len(keys) != 2 {
		t.Fatalf("left %d keys, %v; want the new one and the one it replaced", len(keys), err)
	}
}
