package db

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"math/rand/v2"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Migrations are .sql files applied in name order (NNN_name.sql). Each runs in
// its own transaction with its schema_migrations row, so a failure applies
// nothing. Don't put BEGIN/COMMIT in a file. Never rename or edit an applied
// file; add a new one.
//
// A file whose first line is noTransaction runs outside a transaction, one
// statement at a time (each ends with ";" at the end of a line; a $$-quoted
// body, as in a DO block, may span lines): for CREATE INDEX CONCURRENTLY,
// which doesn't block writes to a large table while it builds, and for DDL
// that must lock many hot tables (policies on every tenant table), one
// table per statement or per COMMIT inside a DO block, so it never holds
// many exclusive locks at once (091_admin_read.sql). A failure part-way is
// retried from the file's first statement (on a lock conflict, below, or on
// the next run), so the whole file must be safe to run again; a
// CONCURRENTLY build that failed leaves an invalid index behind, so such a
// file drops it first (DROP INDEX CONCURRENTLY IF EXISTS). One index per
// file keeps that simple. Once every statement has run, each index the file
// creates must be valid (pg_index.indisvalid): one that isn't (a leftover a
// CREATE ... IF NOT EXISTS skipped) is dropped and the file run again, and
// the file is recorded only once they all are.
//
// Lock conflicts are retried: a migration that fails with a deadlock
// (40P01) or a lock timeout (55P03) runs again from the start, in a fresh
// transaction (outside a transaction, from its first statement), after a
// backoff with jitter, up to migrateAttempts times (about two minutes with
// the 10 s lock timeout) before the migration fails. Live load holding
// locks on the tables a migration alters then delays it rather than
// failing the release.
//
// A migration that turned out unsafe to run in production, but is already
// applied somewhere (so can't be edited), is replaced: replaced maps it to a
// later file that does the same work safely. A database that hasn't applied
// it runs the replacement in its place (at its position in the order, since
// later migrations may depend on what it makes) and records both; one that
// has runs the replacement in its own turn, so the replacement must be
// idempotent, a no-op where the original ran, and depend on nothing newer
// than the original.
//
// Row-level security applies to migrations: outside a tenant, tenant tables
// read as empty, so a data migration that reads them silently does nothing.
// Loop over tenants and set app.tenant_id for each (see 005_deliveries.sql).
//
//go:embed migrations/*.sql
var migrationFiles embed.FS

var migrations, _ = fs.Sub(migrationFiles, "migrations")

// replaced maps a migration to the later one that replaces it (above).
var replaced = map[string]string{
	// 080 took ACCESS EXCLUSIVE on every tenant table in one transaction
	// (CREATE POLICY admin_read) and deadlocked with production's load.
	"080_admin.sql": "091_admin_read.sql",
}

// migrationLock is the advisory lock key held while migrating, so instances
// starting together don't race.
const migrationLock = 0x77616b65 // "wake"

// noTransaction is the first line of a migration that runs outside a
// transaction.
const noTransaction = "-- migrate:no-transaction"

// migrator holds how migrations are applied; tests shorten its waits.
type migrator struct {
	lockTimeout time.Duration     // each statement's lock_timeout
	attempts    int               // tries per migration on a lock conflict or invalid index
	backoff     time.Duration     // the first retry's wait, doubled each time
	maxBackoff  time.Duration     // the longest wait between tries
	replaced    map[string]string // see replaced
}

const migrateAttempts = 10

var defaultMigrator = migrator{
	lockTimeout: 10 * time.Second,
	attempts:    migrateAttempts,
	backoff:     500 * time.Millisecond,
	maxBackoff:  8 * time.Second,
	replaced:    replaced,
}

func (d *DB) migrate(ctx context.Context, files fs.FS) error {
	return d.migrateThen(ctx, files, nil)
}

// migrateThen migrates, then runs then (if not nil) on the same connection,
// still under the migration lock.
func (d *DB) migrateThen(ctx context.Context, files fs.FS, then func(*pgx.Conn) error) error {
	return defaultMigrator.run(ctx, d, files, then)
}

func (m migrator) run(ctx context.Context, d *DB, files fs.FS, then func(*pgx.Conn) error) error {
	conn, err := d.Pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	// Without the server's StatementTimeout (when the server's own role
	// migrates): a migration is bounded by its lock_timeout and the startup
	// probe instead. RESET returns the connection to it; a connection that
	// can't be reset is closed, not handed back unbounded.
	if _, err := conn.Exec(ctx, `SET statement_timeout = 0`); err != nil {
		return err
	}
	defer func() {
		reset, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if _, err := conn.Exec(reset, `RESET statement_timeout`); err != nil {
			conn.Conn().Close(reset)
		}
	}()
	if err := lockForMigration(ctx, conn.Conn()); err != nil {
		return fmt.Errorf("taking migration lock: %w", err)
	}
	defer conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, migrationLock)

	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		name       text PRIMARY KEY,
		applied_at timestamptz NOT NULL DEFAULT now()
	)`); err != nil {
		return fmt.Errorf("creating schema_migrations: %w", err)
	}
	rows, _ := conn.Query(ctx, `SELECT name FROM schema_migrations`)
	applied, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return fmt.Errorf("reading schema_migrations: %w", err)
	}

	names, err := fs.Glob(files, "*.sql")
	if err != nil {
		return err
	}
	slices.Sort(names)
	for _, name := range names {
		if slices.Contains(applied, name) {
			continue
		}
		// Not applied: run it, or its replacement in its place, recording
		// both.
		file, record := name, []string{name}
		if r, ok := m.replaced[name]; ok {
			file, record = r, []string{name, r}
		}
		sql, err := fs.ReadFile(files, file)
		if err != nil {
			return err
		}
		if strings.HasPrefix(string(sql), noTransaction+"\n") {
			err = m.applyOutsideTx(ctx, conn.Conn(), name, string(sql), record)
		} else {
			err = m.retry(ctx, name, func() error {
				return pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
					// Don't queue behind a long transaction holding a lock
					// the migration needs: every query would then wait
					// behind the migration. Fail, and retry.
					if _, err := tx.Exec(ctx, fmt.Sprintf(`SET LOCAL lock_timeout = %d`, m.lockTimeout.Milliseconds())); err != nil {
						return err
					}
					if _, err := tx.Exec(ctx, string(sql)); err != nil {
						return err
					}
					return recordMigrations(ctx, tx, record)
				})
			})
		}
		if err != nil {
			return fmt.Errorf("migration %s: %w", strings.TrimSuffix(name, ".sql"), err)
		}
		applied = append(applied, record...)
		if file != name {
			slog.Info("db: applied migration by its replacement", "name", name, "replacement", file)
		} else {
			slog.Info("db: applied migration", "name", name)
		}
	}
	if then != nil {
		return then(conn.Conn())
	}
	return nil
}

// recordMigrations records names as applied.
func recordMigrations(ctx context.Context, conn interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}, names []string) error {
	_, err := conn.Exec(ctx, `INSERT INTO schema_migrations (name) SELECT unnest($1::text[]) ON CONFLICT DO NOTHING`, names)
	return err
}

// retry runs f until it succeeds, fails other than by a lock conflict
// (deadlock or lock timeout) or an invalid index, or has been tried
// m.attempts times, waiting between tries with exponential backoff and
// jitter. f must start afresh each time (a new transaction, or a
// noTransaction file from its first statement). Retries are logged with the
// migration's name and the error's code, never its SQL.
func (m migrator) retry(ctx context.Context, name string, f func() error) error {
	wait := m.backoff
	for attempt := 1; ; attempt++ {
		err := f()
		code, retryable := lockConflict(err)
		var invalid *invalidIndexError
		if errors.As(err, &invalid) {
			code, retryable = "invalid_index", true
		}
		if !retryable || attempt >= m.attempts {
			return err
		}
		// Half the wait, plus up to as much again at random, so instances
		// retrying together spread out.
		d := wait/2 + rand.N(wait/2+1)
		slog.Warn("db: migration failed; retrying", "name", name, "code", code, "attempt", attempt, "wait", d)
		select {
		case <-ctx.Done():
			return err
		case <-time.After(d):
		}
		wait = min(wait*2, m.maxBackoff)
	}
}

// lockConflict reports whether err is a deadlock (40P01) or a lock timeout
// (55P03), and which.
func lockConflict(err error) (string, bool) {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && (pgErr.Code == "40P01" || pgErr.Code == "55P03") {
		return pgErr.Code, true
	}
	return "", false
}

// lockForMigration takes the migration lock for conn's session, waiting for
// another instance that holds it. It polls with pg_try_advisory_lock and
// sleeps between tries outside any statement, never blocking inside
// pg_advisory_lock: CREATE INDEX CONCURRENTLY, in the holder's migrations,
// waits for every transaction older than it to end, and a waiter's
// blocked pg_advisory_lock statement is one, which is a deadlock.
func lockForMigration(ctx context.Context, conn *pgx.Conn) error {
	wait := 50 * time.Millisecond
	for {
		var got bool
		if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, migrationLock).Scan(&got); err != nil {
			return err
		}
		if got {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
		wait = min(wait*2, time.Second)
	}
}

// applyOutsideTx runs a noTransaction migration's statements one at a time,
// then records it (record) once every index it creates is valid. A lock
// conflict, or an index left invalid, runs the whole file again from its
// first statement: retrying only the statement that failed would skip the
// file's DROP INDEX, and its CREATE INDEX CONCURRENTLY IF NOT EXISTS would
// then find the invalid index the failed build left and do nothing.
func (m migrator) applyOutsideTx(ctx context.Context, conn *pgx.Conn, name, sql string, record []string) error {
	defer conn.Exec(context.WithoutCancel(ctx), `RESET lock_timeout`)
	stmts := statements(sql)
	indexes := createdIndexes(stmts)
	err := m.retry(ctx, name, func() error {
		// As in a transaction: don't queue behind a long one holding a
		// lock. A file may SET its own (it lasts until the RESET); every
		// attempt starts from this one again, never a previous attempt's.
		if _, err := conn.Exec(ctx, fmt.Sprintf(`SET lock_timeout = %d`, m.lockTimeout.Milliseconds())); err != nil {
			return err
		}
		for _, stmt := range stmts {
			if _, err := conn.Exec(ctx, stmt); err != nil {
				return err
			}
		}
		return dropInvalidIndexes(ctx, conn, indexes)
	})
	if err != nil {
		return err
	}
	return recordMigrations(ctx, conn, record)
}

// invalidIndexError names the indexes a noTransaction migration creates
// that were invalid once it had run. They have been dropped, for the file
// to build them again.
type invalidIndexError struct{ indexes []string }

func (e *invalidIndexError) Error() string {
	return "invalid index after the migration ran (dropped, to build again): " + strings.Join(e.indexes, ", ")
}

// dropInvalidIndexes drops each of indexes that exists but isn't valid
// (pg_index.indisvalid: a CONCURRENTLY build failed part-way, and a CREATE
// ... IF NOT EXISTS skipped what it left), returning an invalidIndexError
// naming them, or nil if every one is valid or absent.
func dropInvalidIndexes(ctx context.Context, conn *pgx.Conn, indexes []string) error {
	if len(indexes) == 0 {
		return nil
	}
	rows, _ := conn.Query(ctx, `SELECT i.indexrelid::regclass::text FROM unnest($1::text[]) AS n(name)
		JOIN pg_index i ON i.indexrelid = to_regclass(n.name) WHERE NOT i.indisvalid`, indexes)
	invalid, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return fmt.Errorf("checking indexes are valid: %w", err)
	}
	if len(invalid) == 0 {
		return nil
	}
	for _, index := range invalid {
		// The name is Postgres's own rendering of the regclass, quoted
		// where it must be.
		if _, err := conn.Exec(ctx, `DROP INDEX CONCURRENTLY IF EXISTS `+index); err != nil {
			return err
		}
	}
	return &invalidIndexError{indexes: invalid}
}

// createIndex matches a CREATE INDEX statement, capturing the index's name
// (optionally schema-qualified, either part optionally quoted) as
// to_regclass reads it. An index without a name isn't checked: name them.
var createIndex = regexp.MustCompile(`(?is)^\s*CREATE\s+(?:UNIQUE\s+)?INDEX\s+(?:CONCURRENTLY\s+)?(?:IF\s+NOT\s+EXISTS\s+)?((?:(?:"[^"]+"|\w+)\.)?(?:"[^"]+"|\w+))\s+ON\s`)

// createdIndexes is the names of the indexes stmts create.
func createdIndexes(stmts []string) []string {
	var out []string
	for _, stmt := range stmts {
		if m := createIndex.FindStringSubmatch(stmt); m != nil && !slices.Contains(out, m[1]) {
			out = append(out, m[1])
		}
	}
	return out
}

// statements splits a noTransaction migration at lines ending with ";",
// leaving out comment and blank lines, but not inside a $$-quoted body
// (a DO block or function spanning lines).
func statements(sql string) []string {
	var out []string
	var cur strings.Builder
	quoted := false
	for line := range strings.Lines(sql) {
		t := strings.TrimSpace(line)
		if !quoted && (t == "" || strings.HasPrefix(t, "--")) {
			continue
		}
		cur.WriteString(line)
		if strings.Count(line, "$$")%2 == 1 {
			quoted = !quoted
		}
		if !quoted && strings.HasSuffix(t, ";") {
			out = append(out, strings.TrimSpace(cur.String()))
			cur.Reset()
		}
	}
	if s := strings.TrimSpace(cur.String()); s != "" {
		out = append(out, s)
	}
	return out
}
