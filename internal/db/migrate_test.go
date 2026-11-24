package db_test

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/wakeshift/server/internal/db"
	"github.com/wakeshift/server/internal/dbtest"
)

// emptyDB is a new, empty database, connected as its owner and not yet
// migrated.
func emptyDB(t *testing.T) *db.DB {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), dbtest.EmptyURL(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return &db.DB{Pool: pool}
}

// schema describes what a database's migrations made: tables, columns,
// constraints, indexes, triggers, functions, views, comments, policies, and
// what the admin service's read role is granted.
func schema(t *testing.T, d *db.DB) map[string][]string {
	t.Helper()
	queries := map[string]string{
		"tables": `SELECT table_name || ' ' || table_type FROM information_schema.tables WHERE table_schema = 'public'`,
		"columns": `SELECT table_name || '.' || column_name || ' ' || data_type || ' ' || is_nullable || ' ' || coalesce(column_default, '')
			FROM information_schema.columns WHERE table_schema = 'public'`,
		"constraints": `SELECT conrelid::regclass::text || ' ' || conname || ' ' || pg_get_constraintdef(oid)
			FROM pg_constraint WHERE connamespace = 'public'::regnamespace`,
		"indexes":   `SELECT indexdef FROM pg_indexes WHERE schemaname = 'public'`,
		"triggers":  `SELECT pg_get_triggerdef(oid) FROM pg_trigger WHERE NOT tgisinternal`,
		"functions": `SELECT proname || ' ' || prosrc FROM pg_proc WHERE pronamespace = 'public'::regnamespace`,
		"views":     `SELECT viewname || ' ' || definition FROM pg_views WHERE schemaname = 'public'`,
		"comments": `SELECT c.relname || ' ' || d.description FROM pg_description d JOIN pg_class c ON c.oid = d.objoid
			WHERE d.classoid = 'pg_class'::regclass AND c.relnamespace = 'public'::regnamespace`,
		"policies": `SELECT tablename || ' ' || policyname || ' ' || permissive || ' ' || roles::text || ' ' || cmd || ' ' || coalesce(qual, '') || ' ' || coalesce(with_check, '')
			FROM pg_policies WHERE schemaname = 'public'`,
		"table grants": `SELECT table_name || ' ' || privilege_type FROM information_schema.role_table_grants
			WHERE table_schema = 'public' AND grantee = 'wakeshift_admin_read'`,
		"column grants": `SELECT table_name || '.' || column_name || ' ' || privilege_type FROM information_schema.column_privileges
			WHERE table_schema = 'public' AND grantee = 'wakeshift_admin_read'`,
		"usage grants": `SELECT object_name || ' ' || object_type || ' ' || privilege_type FROM information_schema.role_usage_grants
			WHERE grantee = 'wakeshift_admin_read'`,
		"acls": `SELECT c.relname || ' ' || coalesce(c.relacl::text, '') FROM pg_class c WHERE c.relnamespace = 'public'::regnamespace AND c.relkind IN ('r', 'v', 'S')`,
		"database and schema": `SELECT has_database_privilege('wakeshift_admin_read', current_database(), 'CONNECT')::text || ' ' ||
			has_schema_privilege('wakeshift_admin_read', 'public', 'USAGE')::text`,
		"migrations": `SELECT name FROM schema_migrations`,
	}
	out := map[string][]string{}
	for k, q := range queries {
		rows, _ := d.Pool.Query(context.Background(), q)
		got, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatalf("%s: %v", k, err)
		}
		slices.Sort(got)
		out[k] = got
	}
	return out
}

// 091_admin_read.sql replaces 080_admin.sql: a database that applied 080
// (staging) and then 091 ends up exactly like one that ran 091 in 080's
// place (production, fresh databases).
func TestAdminReadReplacementMatchesOriginal(t *testing.T) {
	ctx := context.Background()
	original := emptyDB(t)
	if err := original.MigrateWith(ctx, db.Migrations, db.MigrateOptions{}); err != nil {
		t.Fatalf("migrating with 080: %v", err)
	}
	replaced := emptyDB(t)
	if err := replaced.MigrateWith(ctx, db.Migrations, db.MigrateOptions{Replaced: db.Replaced}); err != nil {
		t.Fatalf("migrating with 091 in 080's place: %v", err)
	}
	a, b := schema(t, original), schema(t, replaced)
	for k := range a {
		if !slices.Equal(a[k], b[k]) {
			for _, s := range a[k] {
				if !slices.Contains(b[k], s) {
					t.Errorf("%s: only after 080 then 091: %s", k, s)
				}
			}
			for _, s := range b[k] {
				if !slices.Contains(a[k], s) {
					t.Errorf("%s: only after 091 alone: %s", k, s)
				}
			}
		}
	}
	for _, k := range []string{"policies", "column grants", "tables"} {
		if len(a[k]) == 0 {
			t.Errorf("no %s to compare", k)
		}
	}
	if !slices.Contains(b["migrations"], "080_admin.sql") || !slices.Contains(b["migrations"], "091_admin_read.sql") {
		t.Errorf("080 and 091 not both recorded: %v", b["migrations"])
	}
	// Run again, in its own turn, it changes nothing (091 is idempotent).
	// Up to 091 only: later migrations drop columns it grants (103, mode).
	upTo091 := migrationsUpTo(t, "091_admin_read.sql")
	at091 := emptyDB(t)
	if err := at091.MigrateWith(ctx, upTo091, db.MigrateOptions{Replaced: db.Replaced}); err != nil {
		t.Fatalf("migrating up to 091: %v", err)
	}
	before := schema(t, at091)
	if _, err := at091.Pool.Exec(ctx, `DELETE FROM schema_migrations WHERE name = '091_admin_read.sql'`); err != nil {
		t.Fatal(err)
	}
	if err := at091.MigrateWith(ctx, upTo091, db.MigrateOptions{Replaced: db.Replaced}); err != nil {
		t.Fatalf("running 091 again: %v", err)
	}
	if again := schema(t, at091); fmt.Sprint(again) != fmt.Sprint(before) {
		t.Error("running 091 again changed the schema")
	}
}

// migrationsUpTo is the server's migrations up to and including last.
func migrationsUpTo(t *testing.T, last string) fstest.MapFS {
	t.Helper()
	names, err := fs.Glob(db.Migrations, "*.sql")
	if err != nil {
		t.Fatal(err)
	}
	out := fstest.MapFS{}
	for _, name := range names {
		if name > last {
			continue
		}
		data, err := fs.ReadFile(db.Migrations, name)
		if err != nil {
			t.Fatal(err)
		}
		out[name] = &fstest.MapFile{Data: data}
	}
	return out
}

// A replaced migration's replacement runs in its place, before the
// migrations after it (which may depend on it), and isn't run again in its
// own turn. Where the original was applied, the replacement runs in its own
// turn.
func TestReplacedMigrationRunsInItsPlace(t *testing.T) {
	ctx := context.Background()
	files := fstest.MapFS{
		"900_a.sql":      {Data: []byte("CREATE TABLE rep_a (x int);\n")},
		"901_b.sql":      {Data: []byte("ALTER TABLE rep_a ADD COLUMN y int;\n")},
		"902_a_safe.sql": {Data: []byte("-- migrate:no-transaction\nCREATE TABLE IF NOT EXISTS rep_a (x int);\nCREATE TABLE IF NOT EXISTS rep_c (x int);\n")},
	}
	rep := map[string]string{"900_a.sql": "902_a_safe.sql"}

	fresh := emptyDB(t)
	if err := fresh.MigrateWith(ctx, files, db.MigrateOptions{Replaced: rep}); err != nil {
		t.Fatal(err)
	}
	var n int
	fresh.Pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations WHERE name IN ('900_a.sql', '901_b.sql', '902_a_safe.sql')`).Scan(&n)
	if n != 3 {
		t.Fatalf("%d recorded", n)
	}
	if _, err := fresh.Pool.Exec(ctx, `SELECT y FROM rep_a, rep_c`); err != nil {
		t.Fatal(err)
	}

	// The original applied already (as staging applied 080): the
	// replacement runs in its own turn.
	applied := emptyDB(t)
	if err := applied.MigrateWith(ctx, fstest.MapFS{"900_a.sql": files["900_a.sql"]}, db.MigrateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := applied.MigrateWith(ctx, files, db.MigrateOptions{Replaced: rep}); err != nil {
		t.Fatal(err)
	}
	if _, err := applied.Pool.Exec(ctx, `SELECT y FROM rep_a, rep_c`); err != nil {
		t.Fatal(err)
	}
}

// holdLock locks table in mode on a connection of its own, in a
// transaction, until the returned function (or the test's end) releases it.
func holdLock(t *testing.T, d *db.DB, table, mode string) func() {
	t.Helper()
	ctx := context.Background()
	conn, err := d.Pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `LOCK TABLE `+table+` IN `+mode+` MODE`); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	release := func() {
		once.Do(func() {
			tx.Rollback(ctx)
			conn.Release()
		})
	}
	t.Cleanup(release)
	return release
}

// A migration that can't get its lock in time (live load holds it) is
// retried, from a fresh transaction, and succeeds once the lock is free;
// outside a transaction, the file is run again from its first statement
// (so each of them must be idempotent).
func TestMigrationRetriesLockConflicts(t *testing.T) {
	ctx := context.Background()
	d := emptyDB(t)
	if err := d.Migrate(ctx, fstest.MapFS{"900_hot.sql": {Data: []byte("CREATE TABLE mig_hot (id int);\nCREATE TABLE mig_hot2 (id int);\n")}}); err != nil {
		t.Fatal(err)
	}
	opts := db.MigrateOptions{LockTimeout: 100 * time.Millisecond, Backoff: 100 * time.Millisecond, Attempts: 20}
	const hold = 1200 * time.Millisecond

	for _, tc := range []struct{ name, sql string }{
		{"901_tx.sql", "CREATE TABLE mig_side (id int);\nALTER TABLE mig_hot ADD COLUMN x int;\n"},
		{"902_notx.sql", "-- migrate:no-transaction\nALTER TABLE mig_hot2 ADD COLUMN IF NOT EXISTS x int;\nDO $$\nBEGIN\n    IF NOT EXISTS (SELECT 1 FROM pg_policy WHERE polname = 'p' AND polrelid = 'mig_hot'::regclass) THEN\n        CREATE POLICY p ON mig_hot USING (true);\n    END IF;\nEND $$;\n"},
	} {
		release := holdLock(t, d, "mig_hot", "ACCESS SHARE")
		time.AfterFunc(hold, release)
		start := time.Now()
		if err := d.MigrateWith(ctx, fstest.MapFS{tc.name: {Data: []byte(tc.sql)}}, opts); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if waited := time.Since(start); waited < hold {
			t.Errorf("%s applied after %v, while the lock was held", tc.name, waited)
		}
	}
	if _, err := d.Pool.Exec(ctx, `SELECT mig_hot.x, mig_hot2.x, mig_side.id FROM mig_hot, mig_hot2, mig_side`); err != nil {
		t.Fatal(err)
	}
	var policies int
	d.Pool.QueryRow(ctx, `SELECT count(*) FROM pg_policy WHERE polrelid = 'mig_hot'::regclass`).Scan(&policies)
	if policies != 1 {
		t.Errorf("%d policies", policies)
	}

	// Held throughout: it fails after its tries, applying nothing.
	holdLock(t, d, "mig_hot", "ACCESS SHARE")
	err := d.MigrateWith(ctx, fstest.MapFS{"903_never.sql": {Data: []byte("ALTER TABLE mig_hot ADD COLUMN z int;\n")}},
		db.MigrateOptions{LockTimeout: 50 * time.Millisecond, Backoff: 10 * time.Millisecond, Attempts: 3})
	var pgErr *pgconn.PgError
	if err == nil || !strings.Contains(err.Error(), "903_never") || !errors.As(err, &pgErr) || pgErr.Code != "55P03" {
		t.Fatalf("lock held throughout: %v", err)
	}
	var n int
	d.Pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations WHERE name = '903_never.sql'`).Scan(&n)
	if n != 0 {
		t.Error("recorded")
	}
}

// indexValid reports whether index exists and is valid.
func indexValid(t *testing.T, d *db.DB, index string) (exists, valid bool) {
	t.Helper()
	err := d.Pool.QueryRow(context.Background(), `SELECT i.indisvalid FROM pg_index i JOIN pg_class c ON c.oid = i.indexrelid WHERE c.relname = $1`, index).Scan(&valid)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return true, valid
}

// recorded reports whether name is in schema_migrations.
func recorded(t *testing.T, d *db.DB, name string) bool {
	t.Helper()
	var n int
	if err := d.Pool.QueryRow(context.Background(), `SELECT count(*) FROM schema_migrations WHERE name = $1`, name).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n == 1
}

// A CONCURRENTLY build that times out waiting for an older transaction
// (one writing to the table: it holds ROW EXCLUSIVE, which the build's wait
// for writers conflicts with) leaves an INVALID index behind. The migrator
// runs the whole file again, its DROP first, so the index ends up valid
// before the file is recorded. Production's 112 retried only the CREATE
// ... IF NOT EXISTS, which skipped the invalid index, and was recorded with
// it.
func TestConcurrentIndexRetryRebuildsTheIndex(t *testing.T) {
	ctx := context.Background()
	d := emptyDB(t)
	if err := d.Migrate(ctx, fstest.MapFS{"900_table.sql": {Data: []byte("CREATE TABLE mig_idx (id int);\n")}}); err != nil {
		t.Fatal(err)
	}
	const hold = 1200 * time.Millisecond
	release := holdLock(t, d, "mig_idx", "ROW EXCLUSIVE")
	time.AfterFunc(hold, release)
	file := fstest.MapFS{"901_idx.sql": {Data: []byte("-- migrate:no-transaction\n" +
		"DROP INDEX CONCURRENTLY IF EXISTS mig_idx_id;\nCREATE INDEX CONCURRENTLY IF NOT EXISTS mig_idx_id\n    ON mig_idx (id);\n")}}
	start := time.Now()
	if err := d.MigrateWith(ctx, file, db.MigrateOptions{LockTimeout: 100 * time.Millisecond, Backoff: 100 * time.Millisecond, Attempts: 20}); err != nil {
		t.Fatal(err)
	}
	if waited := time.Since(start); waited < hold {
		t.Errorf("applied after %v, while the lock was held", waited)
	}
	if exists, valid := indexValid(t, d, "mig_idx_id"); !exists || !valid {
		t.Fatalf("index exists %v, valid %v", exists, valid)
	}
	if !recorded(t, d, "901_idx.sql") {
		t.Fatal("not recorded")
	}

	// Held throughout: it fails after its tries, and isn't recorded with
	// the invalid index it leaves (the next run drops it first).
	if _, err := d.Pool.Exec(ctx, `CREATE TABLE mig_idx2 (id int)`); err != nil {
		t.Fatal(err)
	}
	holdLock(t, d, "mig_idx2", "ROW EXCLUSIVE")
	never := fstest.MapFS{"902_never.sql": {Data: []byte("-- migrate:no-transaction\n" +
		"DROP INDEX CONCURRENTLY IF EXISTS mig_idx2_id;\nCREATE INDEX CONCURRENTLY IF NOT EXISTS mig_idx2_id ON mig_idx2 (id);\n")}}
	err := d.MigrateWith(ctx, never, db.MigrateOptions{LockTimeout: 50 * time.Millisecond, Backoff: 10 * time.Millisecond, Attempts: 3})
	if _, conflict := db.LockConflict(err); !conflict {
		t.Fatalf("lock held throughout: %v", err)
	}
	if recorded(t, d, "902_never.sql") {
		t.Error("recorded")
	}
}

// An invalid index left by an earlier failed attempt is dropped and built
// again, even by a file that doesn't drop it first (092 didn't): once the
// file has run, an invalid index it creates is dropped and the file run
// again, and it's recorded only once the index is valid.
func TestInvalidLeftoverIndexIsRebuilt(t *testing.T) {
	ctx := context.Background()
	d := emptyDB(t)
	if err := d.Migrate(ctx, fstest.MapFS{"900_table.sql": {Data: []byte("CREATE TABLE mig_left (id int);\n")}}); err != nil {
		t.Fatal(err)
	}
	// The earlier attempt: a build that timed out waiting for a writer.
	release := holdLock(t, d, "mig_left", "ROW EXCLUSIVE")
	conn, err := d.Pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SET lock_timeout = 100`); err != nil {
		t.Fatal(err)
	}
	_, err = conn.Exec(ctx, `CREATE INDEX CONCURRENTLY mig_left_id ON mig_left (id)`)
	if _, conflict := db.LockConflict(err); !conflict {
		t.Fatalf("the earlier build: %v", err)
	}
	conn.Exec(ctx, `RESET lock_timeout`)
	release()
	if exists, valid := indexValid(t, d, "mig_left_id"); !exists || valid {
		t.Fatalf("leftover exists %v, valid %v", exists, valid)
	}

	// No DROP first: the CREATE ... IF NOT EXISTS skips the leftover, and
	// the check after the file finds it.
	file := fstest.MapFS{"901_left.sql": {Data: []byte("-- migrate:no-transaction\nCREATE INDEX CONCURRENTLY IF NOT EXISTS mig_left_id ON mig_left (id);\n")}}
	if err := d.MigrateWith(ctx, file, db.MigrateOptions{Backoff: 10 * time.Millisecond}); err != nil {
		t.Fatal(err)
	}
	if exists, valid := indexValid(t, d, "mig_left_id"); !exists || !valid {
		t.Fatalf("index exists %v, valid %v", exists, valid)
	}
	if !recorded(t, d, "901_left.sql") {
		t.Fatal("not recorded")
	}
}

// Every noTransaction migration can run again from its first statement
// right after it ran, as the migrator does after a lock conflict part-way:
// each runs twice in a row (a copy named to sort just after it), and the
// schema ends up as with one run.
func TestNoTransactionMigrationsRunAgain(t *testing.T) {
	ctx := context.Background()
	names, err := fs.Glob(db.Migrations, "*.sql")
	if err != nil {
		t.Fatal(err)
	}
	twice := fstest.MapFS{}
	n := 0
	for _, name := range names {
		data, err := fs.ReadFile(db.Migrations, name)
		if err != nil {
			t.Fatal(err)
		}
		twice[name] = &fstest.MapFile{Data: data}
		if strings.HasPrefix(string(data), "-- migrate:no-transaction\n") {
			twice[name+"~again.sql"] = &fstest.MapFile{Data: data}
			n++
		}
	}
	if n == 0 {
		t.Fatal("no noTransaction migrations")
	}
	again := emptyDB(t)
	if err := again.MigrateWith(ctx, twice, db.MigrateOptions{Replaced: db.Replaced}); err != nil {
		t.Fatal(err)
	}
	once := emptyDB(t)
	if err := once.MigrateWith(ctx, db.Migrations, db.MigrateOptions{Replaced: db.Replaced}); err != nil {
		t.Fatal(err)
	}
	a, b := schema(t, once), schema(t, again)
	for k := range a {
		if k != "migrations" && !slices.Equal(a[k], b[k]) {
			t.Errorf("%s differs after running noTransaction migrations twice", k)
		}
	}
}

func TestCreatedIndexes(t *testing.T) {
	got := db.CreatedIndexes(db.Statements("-- migrate:no-transaction\nSET lock_timeout = '3s';\n" +
		"DROP INDEX CONCURRENTLY IF EXISTS a;\nCREATE INDEX CONCURRENTLY IF NOT EXISTS a\n    ON t (x);\n" +
		"create unique index concurrently b on t (y);\nCREATE INDEX public.c ON t (z);\nCREATE TABLE IF NOT EXISTS d (x int);\n" +
		"CREATE INDEX \"public\".\"E e\" ON t (z);\nCREATE INDEX ON t (z);\n"))
	if want := []string{"a", "b", "public.c", `"public"."E e"`}; !slices.Equal(got, want) {
		t.Fatalf("got %q", got)
	}
}

func TestLockConflict(t *testing.T) {
	for code, want := range map[string]bool{"40P01": true, "55P03": true, "42P01": false, "40001": false} {
		if _, got := db.LockConflict(fmt.Errorf("wrapped: %w", &pgconn.PgError{Code: code})); got != want {
			t.Errorf("%s: %v", code, got)
		}
	}
	if _, got := db.LockConflict(nil); got {
		t.Error("nil")
	}
}

// A noTransaction migration splits at lines ending with ";", but not inside
// a $$-quoted body.
func TestStatementsKeepDollarQuotedBodies(t *testing.T) {
	sql := "-- migrate:no-transaction\n-- A comment; not a statement.\nSET lock_timeout = '3s';\n\nDO $$\nBEGIN\n    -- inside;\n    PERFORM 1;\n    COMMIT;\nEND $$;\nDO $$ BEGIN PERFORM 2; END $$;\nCREATE TABLE t (\n    a int  -- x; y\n);\n"
	got := db.Statements(sql)
	want := []string{
		"SET lock_timeout = '3s';",
		"DO $$\nBEGIN\n    -- inside;\n    PERFORM 1;\n    COMMIT;\nEND $$;",
		"DO $$ BEGIN PERFORM 2; END $$;",
		"CREATE TABLE t (\n    a int  -- x; y\n);",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("got %q", got)
	}
}
