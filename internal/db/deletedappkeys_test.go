package db_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"testing"

	"github.com/wakeshift/server/internal/db"
)

// 130 deletes the keys of apps deleted before deleting an app deleted its
// keys (the privacy policy: a deleted app is kept "without its keys"),
// revoked or not, after keeping each one's fingerprint (SHA-256 of its kid,
// for a key without a kid the one its public key gives), and leaves live
// apps' keys and every app's log alone.
func TestDeletedAppKeysMigration(t *testing.T) {
	ctx := context.Background()
	d := emptyDB(t)
	if err := d.MigrateWith(ctx, migrationsUpTo(t, "129_session_wait_tasks.sql"), db.MigrateOptions{Replaced: db.Replaced}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Pool.Exec(ctx, `WITH o AS (INSERT INTO organizations (name) VALUES ('Org') RETURNING id)
		INSERT INTO apps (id, name, org_id, deleted_at) SELECT 'gone', 'Gone', id, now() FROM o UNION ALL SELECT 'live', 'Live', id, NULL FROM o`); err != nil {
		t.Fatal(err)
	}
	for _, k := range []struct{ app, id string }{{"gone", "k1"}, {"gone", "k2"}, {"live", "k3"}} {
		if _, err := d.Pool.Exec(ctx, `INSERT INTO app_keys (app_id, key_id, kid, name, public_key, revoked_at)
			VALUES ($1::text, $2::text, $2::text, 'key', sha256(convert_to($2::text, 'UTF8')), CASE WHEN $1::text = 'gone' THEN now() END)`, k.app, k.id); err != nil {
			t.Fatal(err)
		}
	}
	// A key the previous revision made before kid was written.
	if _, err := d.Pool.Exec(ctx, `UPDATE app_keys SET kid = NULL WHERE key_id = 'k2'`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Pool.Exec(ctx, `INSERT INTO app_log (app_id, actor, action, key_id, detail) VALUES ('gone', 'user:x', 'app.deleted', 'k1', '{}')`); err != nil {
		t.Fatal(err)
	}
	if err := d.MigrateWith(ctx, db.Migrations, db.MigrateOptions{Replaced: db.Replaced}); err != nil {
		t.Fatal(err)
	}
	var gone, live, logged int
	if err := d.Pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE app_id = 'gone'), count(*) FILTER (WHERE app_id = 'live') FROM app_keys`).Scan(&gone, &live); err != nil {
		t.Fatal(err)
	}
	if err := d.Pool.QueryRow(ctx, `SELECT count(*) FROM app_log WHERE app_id = 'gone' AND key_id = 'k1'`).Scan(&logged); err != nil {
		t.Fatal(err)
	}
	var want [][]byte
	for _, id := range []string{"k1", "k2"} {
		k1 := sha256.Sum256([]byte(id)) // the test's public keys
		kid := sha256.Sum256(k1[:])
		kidStr := base64.RawURLEncoding.EncodeToString(kid[:16])
		if id == "k1" {
			kidStr = "k1" // written as its kid
		}
		h := sha256.Sum256([]byte(kidStr))
		want = append(want, h[:])
	}
	var tombstones int
	if err := d.Pool.QueryRow(ctx, `SELECT count(*) FROM deleted_key_ids WHERE kid_hash = ANY($1)`, want).Scan(&tombstones); err != nil {
		t.Fatal(err)
	}
	var all int
	d.Pool.QueryRow(ctx, `SELECT count(*) FROM deleted_key_ids`).Scan(&all)
	if tombstones != 2 || all != 2 {
		t.Errorf("fingerprints: %d of the deleted keys', %d in all; want 2 and 2", tombstones, all)
	}
	if gone != 0 || live != 1 || logged != 1 {
		t.Errorf("after 130: the deleted app has %d keys (want 0), the live one %d (want 1), log entries naming a deleted key %d (want 1)", gone, live, logged)
	}
}
