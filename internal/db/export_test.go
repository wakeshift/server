package db

import (
	"context"
	"io/fs"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Migrate applies migrations from files (tests of the migration runner).
func (d *DB) Migrate(ctx context.Context, files fs.FS) error { return d.migrate(ctx, files) }

// Migrations are the server's migrations, and Replaced the ones replaced by
// later ones.
var (
	Migrations = migrations
	Replaced   = replaced
)

// MigrateOptions change how MigrateWith applies migrations.
type MigrateOptions struct {
	Replaced    map[string]string
	LockTimeout time.Duration // 0: the default
	Attempts    int           // 0: the default
	Backoff     time.Duration // 0: the default
}

// MigrateWith is Migrate with o's replacements, lock timeout and retries.
func (d *DB) MigrateWith(ctx context.Context, files fs.FS, o MigrateOptions) error {
	m := defaultMigrator
	m.replaced = o.Replaced
	if o.LockTimeout > 0 {
		m.lockTimeout = o.LockTimeout
	}
	if o.Attempts > 0 {
		m.attempts = o.Attempts
	}
	if o.Backoff > 0 {
		m.backoff, m.maxBackoff = o.Backoff, 4*o.Backoff
	}
	return m.run(ctx, d, files, nil)
}

var (
	Statements     = statements
	LockConflict   = lockConflict
	CreatedIndexes = createdIndexes
)

// Configure applies the server's connection settings to cfg, and FromPool
// wraps a pool so made: for tests that connect their own way (through a
// connection they can stall).
func Configure(cfg *pgxpool.Config) { configure(cfg) }

func FromPool(pool *pgxpool.Pool) *DB { return &DB{Pool: pool} }
