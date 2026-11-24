// Package db is Wakeshift's access to Postgres.
//
// Every tenant's data lives in the same tables, separated by a tenant_id
// column and enforced by row-level security: each tenant-scoped table has
// FORCE ROW LEVEL SECURITY and a policy matching app.tenant_id, which InTenant
// sets for the length of a transaction. Outside InTenant, tenant-scoped tables
// read as empty and reject writes, so a missing tenant fails closed.
//
// Row-level security doesn't apply to superusers or roles with BYPASSRLS, so
// Open refuses to run as one, or as a role that's a member of one (directly
// or through other roles): a member can SET ROLE to it.
package db

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DB is a connection pool.
type DB struct {
	Pool  *pgxpool.Pool
	locks exclusiveLocks
}

// Open connects, checks the role can't bypass row-level security, and applies
// pending migrations.
func Open(ctx context.Context, url string, opts ...Option) (*DB, error) {
	return OpenWithOwner(ctx, url, "", opts...)
}

// Option changes how Open sets the database up.
type Option func(*options)

type options struct {
	adminLogin string
}

// AdminReadLogin grants wakeshift_admin_read, the admin service's read role
// (080_admin.sql), to login after migrating, if that login
// exists (on Cloud SQL, the admin service's IAM database user). The role
// that migrates made the role, so it holds its admin option. A login that
// doesn't exist yet is logged and skipped: the core still starts, and the
// admin service, which checks its own grants, doesn't.
func AdminReadLogin(login string) Option { return func(o *options) { o.adminLogin = login } }

// OpenWithOwner is Open with the tables owned by another role, ownerURL's,
// which runs the migrations and grants url's role reads and writes on every
// table, and nothing more: it can't alter a table, so it can't turn off
// row-level security. An empty ownerURL is Open: url's role owns the tables
// and migrates them.
func OpenWithOwner(ctx context.Context, url, ownerURL string, opts ...Option) (*DB, error) {
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("parsing the database URL: %w", err)
	}
	configure(cfg)
	// The owner first: it migrates, and grants the role it may connect
	// (databases are closed to other roles: REVOKE CONNECT … FROM PUBLIC).
	if ownerURL != "" {
		if err := migrateAsOwner(ctx, ownerURL, cfg.ConnConfig.User, migrations, o); err != nil {
			return nil, err
		}
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connecting: %w", err)
	}
	d := &DB{Pool: pool}
	if err := d.checkRole(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	if ownerURL == "" {
		err := d.migrateThen(ctx, migrations, func(conn *pgx.Conn) error {
			return grantAdminRead(ctx, conn, o.adminLogin, cfg.ConnConfig.User)
		})
		if err != nil {
			pool.Close()
			return nil, err
		}
	}
	if err := d.checkNotAdminRead(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return d, nil
}

// configure sets the bounds every connection of the server's pool runs
// under, as connection parameters, whatever the role's own settings (an
// ALTER ROLE is lost when the role is made again) or the URL's options
// say, and how the pool checks and replaces its connections.
//
// The server may reach Postgres through a proxy such as the Cloud SQL
// connector: a Unix socket on each side (Postgres logs the sessions as
// host=[local]) and a tunnel between. Neither Postgres's tcp_keepalives_*
// nor the driver's TCP keepalives apply to a Unix socket, so a connection
// whose tunnel stalls looks alive to both ends, and a transaction on it
// keeps its locks while requests queue behind it. What ends one is these
// timeouts, on the server's side, and the pool's ping, on ours.
func configure(cfg *pgxpool.Config) {
	p := cfg.ConnConfig.RuntimeParams
	ms := func(d time.Duration) string { return strconv.FormatInt(d.Milliseconds(), 10) }
	// No statement runs unbounded.
	p["statement_timeout"] = ms(StatementTimeout)
	// Nor may a transaction sit open with nothing running (a snapshot
	// held back from vacuum, locks held, for as long as the client is
	// gone or its connection stalled).
	p["idle_in_transaction_session_timeout"] = ms(IdleInTransactionTimeout)
	// A statement queued for a lock gives up rather than adding to a
	// queue that every later caller joins.
	p["lock_timeout"] = ms(LockTimeout)
	// A statement whose client has gone stops at the next check rather
	// than running to its end.
	p["client_connection_check_interval"] = ms(ClientCheckInterval)
	// A connection handed out after sitting idle is pinged first (pgxpool
	// pings one idle for more than a second); one that doesn't answer in
	// PingTimeout is dropped and another taken, rather than the caller
	// waiting on a stalled one until its own context ends.
	cfg.PingTimeout = PingTimeout
	// Connections are replaced every half hour or so (not all at once),
	// so none outlives a problem on its path for long.
	cfg.MaxConnLifetime = MaxConnLifetime
	cfg.MaxConnLifetimeJitter = MaxConnLifetime / 6
}

// StatementTimeout is how long any statement on the server's connections
// may run (Postgres's statement_timeout, set as a connection parameter).
// Migrations, which run longer, lift it for their own session.
const StatementTimeout = 5 * time.Minute

// IdleInTransactionTimeout is how long a transaction on the server's
// connections may sit open with no statement running before Postgres ends
// the session (idle_in_transaction_session_timeout): the transaction rolls
// back, its locks and snapshot go, and the pool replaces the connection.
// The server's transactions run their statements back to back; none waits
// on the network, a timer or another pool connection between them, so only
// a stuck, stalled or leaked transaction gets near this. It's what frees
// the locks of a transaction whose connection stalled (configure), so it
// is short: a subscribe's locks are what every subscribe to the same topic
// and every manifest change for the app wait for.
const IdleInTransactionTimeout = 20 * time.Second

// LockTimeout is how long a statement on the server's connections waits
// for a lock (lock_timeout) before failing with 55P03. Longer than
// IdleInTransactionTimeout, so a statement queued behind a stalled
// transaction gets its lock once that transaction is ended, rather than
// failing first; a holder that runs a statement for longer than this
// fails the statements queued behind it. Migrations set their own,
// shorter (migrator.lockTimeout).
const LockTimeout = 30 * time.Second

// ClientCheckInterval is how often Postgres checks, while a statement runs,
// that its client is still connected (client_connection_check_interval),
// and cancels the statement if not: an instance that's gone (a crash, a
// SIGKILL) leaves nothing running behind it.
const ClientCheckInterval = 10 * time.Second

// PingTimeout bounds the ping the pool sends a connection that sat idle
// before handing it out (pgxpool.Config.PingTimeout).
const PingTimeout = 5 * time.Second

// MaxConnLifetime is how long the pool keeps a connection, give or take a
// sixth (pgxpool.Config.MaxConnLifetime and MaxConnLifetimeJitter).
const MaxConnLifetime = 30 * time.Minute

// AdminReadRole is the admin service's read role (080_admin.sql, 091_admin_read.sql).
const AdminReadRole = "wakeshift_admin_read"

// grantAdminRead grants AdminReadRole to login, if it's set and exists.
// runtime is the core's own login, which must never get it
// (ErrAdminReadRuntime).
func grantAdminRead(ctx context.Context, conn *pgx.Conn, login, runtime string) error {
	if login == "" {
		return nil
	}
	if login == AdminReadRole || strings.HasPrefix(login, "pg_") || strings.HasPrefix(login, "cloudsql") {
		return fmt.Errorf("db: %q can't be the admin service's login", login)
	}
	if login == runtime {
		return fmt.Errorf("%w (WAKESHIFT_ADMIN_DB_USER is the core's own database login)", ErrAdminReadRuntime)
	}
	var exists, member bool
	err := conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1),
		EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1 AND pg_has_role(oid, $2, 'MEMBER'))`, login, AdminReadRole).Scan(&exists, &member)
	if err != nil {
		return fmt.Errorf("checking the admin service's login: %w", err)
	}
	switch {
	case !exists:
		slog.Warn("db: the admin service's database login doesn't exist yet; not granting it the read role", "login", login)
		return nil
	case member:
		return nil
	}
	if _, err := conn.Exec(ctx, `GRANT `+AdminReadRole+` TO `+pgx.Identifier{login}.Sanitize()); err != nil {
		return fmt.Errorf("granting the admin service's login the read role: %w", err)
	}
	slog.Info("db: granted the admin service's login the read role", "login", login)
	return nil
}

var ErrRuntimeOwns = errors.New("the database role can alter the tables (it owns some, or is a member of the role that owns them), so it could turn off row-level security; use a role that only reads and writes")

// migrateAsOwner migrates with the owner's connection, then grants runtime
// (a role) the database's CONNECT and reads and writes on every table and
// sequence, now and to come (default privileges), except updating or
// deleting from audit_log (append-only: erasing a workspace deletes its
// audit log through erase_tenant, 170_erase_tenant.sql, which runtime may
// call). The grants are made under the migration lock: instances starting
// together would otherwise race on the same catalog rows.
func migrateAsOwner(ctx context.Context, ownerURL, runtime string, files fs.FS, opts options) error {
	cfg, err := pgxpool.ParseConfig(ownerURL)
	if err != nil {
		return fmt.Errorf("parsing the owner's database URL: %w", err)
	}
	cfg.MinConns, cfg.MaxConns = 0, 1 // only for migrating; closed after
	owner, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return fmt.Errorf("connecting as the owner: %w", err)
	}
	defer owner.Close()
	o := &DB{Pool: owner}
	if err := o.checkRole(ctx); err != nil {
		return fmt.Errorf("owner: %w", err)
	}
	return o.migrateThen(ctx, files, func(conn *pgx.Conn) error {
		var owns bool
		var database string
		err := conn.QueryRow(ctx, `SELECT pg_has_role($1, current_user, 'MEMBER') OR EXISTS (
			SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
			WHERE n.nspname = 'public' AND c.relowner = (SELECT oid FROM pg_roles WHERE rolname = $1)), current_database()`, runtime).Scan(&owns, &database)
		if err != nil {
			return fmt.Errorf("checking the database role: %w", err)
		}
		if owns {
			return ErrRuntimeOwns
		}
		var holds bool
		err = conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $2
			AND (pg_has_role($1, oid, 'USAGE') OR pg_has_role($1, oid, 'SET')))`, runtime, AdminReadRole).Scan(&holds)
		if err != nil {
			return fmt.Errorf("checking the database role: %w", err)
		}
		if holds {
			return ErrAdminReadRuntime // before granting it anything
		}
		r := pgx.Identifier{runtime}.Sanitize()
		_, err = conn.Exec(ctx, `
			GRANT CONNECT ON DATABASE `+pgx.Identifier{database}.Sanitize()+` TO `+r+`;
			GRANT USAGE ON SCHEMA public TO `+r+`;
			GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO `+r+`;
			GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO `+r+`;
			ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO `+r+`;
			ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT USAGE, SELECT ON SEQUENCES TO `+r+`;
			REVOKE UPDATE, DELETE ON audit_log FROM `+r+`;
			GRANT EXECUTE ON FUNCTION erase_tenant() TO `+r+`;
			REVOKE UPDATE, DELETE ON admin_log FROM `+r+`;
			REVOKE INSERT, UPDATE, DELETE ON schema_migrations FROM `+r)
		if err != nil {
			return fmt.Errorf("granting the database role reads and writes: %w", err)
		}
		return grantAdminRead(ctx, conn, opts.adminLogin, runtime)
	})
}

func (d *DB) Close() { d.Pool.Close() }

// Conn opens a connection outside the pool, for work that holds one for
// good (LISTEN): it would otherwise take a pool slot forever. It's a
// HeldConnConfig connection: wait on it with ReceiveNotifications.
func (d *DB) Conn(ctx context.Context) (*pgx.Conn, error) {
	return pgx.ConnectConfig(ctx, HeldConnConfig(d.Pool.Config().ConnConfig))
}

// HeldConnConfig is a copy of cfg for a connection held for good outside
// the pool: one that sits idle between notifications, and may hold a
// session lock all the while (the pipeline's leader). Postgres ends its
// session once it has been idle for HeldIdleTimeout (idle_session_timeout),
// and with it the session's locks, so a holder whose connection stalled or
// whose instance is gone doesn't keep them: the holder pings it more often
// than that (ReceiveNotifications).
func HeldConnConfig(cfg *pgx.ConnConfig) *pgx.ConnConfig {
	c := cfg.Copy()
	c.RuntimeParams["idle_session_timeout"] = strconv.FormatInt(heldNow().idle.Milliseconds(), 10)
	return c
}

// HeldIdleTimeout is how long a held connection (HeldConnConfig) may sit
// idle before Postgres ends its session; HeldPing, how often its holder
// pings it while waiting (ReceiveNotifications), each ping given
// HeldPingTimeout to answer. A stalled connection is noticed by its holder
// within HeldPing+HeldPingTimeout, and by Postgres within HeldIdleTimeout.
const (
	HeldIdleTimeout = 90 * time.Second
	HeldPing        = 30 * time.Second
	HeldPingTimeout = 10 * time.Second
)

// held is the timings in use: the constants, or a test's (SetHeldTimings).
var held atomic.Pointer[heldTimings]

type heldTimings struct{ idle, ping, timeout time.Duration }

func heldNow() heldTimings {
	if t := held.Load(); t != nil {
		return *t
	}
	return heldTimings{HeldIdleTimeout, HeldPing, HeldPingTimeout}
}

// SetHeldTimings replaces HeldIdleTimeout, HeldPing and HeldPingTimeout
// for the process until restore is called: for tests of what holds
// connections (dbtest.HeldTimings), which can't wait out the real ones.
func SetHeldTimings(idle, ping, timeout time.Duration) (restore func()) {
	old := held.Swap(&heldTimings{idle, ping, timeout})
	return func() { held.Store(old) }
}

// ReceiveNotifications passes each notification on conn, a held connection
// (HeldConnConfig) that LISTENs, to handle, until ctx ends or the
// connection fails; handle runs on this goroutine, so it mustn't block.
//
// It pings the connection HeldPing after the last command it sent (a
// LISTEN just before the call, or its last ping), whether or not
// notifications came meanwhile: Postgres's idle_session_timeout counts
// from the last command, and notifications don't reset it, so a busy
// listener pinged only after a quiet HeldPing was ended every
// HeldIdleTimeout (#1311). A ping not answered within HeldPingTimeout
// means the connection has stalled or gone: it returns an error and the
// caller reconnects. (pgx keeps a connection whose wait only timed out,
// and queues notifications that arrive during a ping for the next wait.)
func ReceiveNotifications(ctx context.Context, conn *pgx.Conn, handle func(*pgconn.Notification)) error {
	lastCommand := time.Now()
	for {
		timings := heldNow()
		wctx, cancel := context.WithDeadline(ctx, lastCommand.Add(timings.ping))
		n, err := conn.WaitForNotification(wctx)
		cancel()
		if err == nil {
			handle(n)
			continue
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !errors.Is(err, context.DeadlineExceeded) || conn.IsClosed() {
			return err
		}
		pctx, cancel := context.WithTimeout(ctx, timings.timeout)
		err = conn.Ping(pctx)
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			// Dropped now, without waiting on it again (a done context
			// closes it at once): the caller reconnects.
			done, cancel := context.WithCancel(context.Background())
			cancel()
			conn.Close(done)
			return fmt.Errorf("db: the held connection didn't answer a ping: %w", err)
		}
		lastCommand = time.Now()
	}
}

// CloseHeld closes a held connection after UNLISTEN, each given
// HeldPingTimeout, for a deferred cleanup: on a connection that stalled,
// neither may wait for long.
func CloseHeld(conn *pgx.Conn) {
	ctx, cancel := context.WithTimeout(context.Background(), heldNow().timeout)
	defer cancel()
	if !conn.IsClosed() {
		conn.Exec(ctx, "UNLISTEN *")
	}
	conn.Close(ctx)
}

// Ping checks the database is reachable.
func (d *DB) Ping(ctx context.Context) error { return d.Pool.Ping(ctx) }

var ErrBypassesRLS = errors.New("database role is a superuser or has BYPASSRLS, or can switch to a role that is, so tenant isolation wouldn't be enforced; connect as an ordinary role")

// checkRole refuses a login that is, or is a member of (so can SET ROLE to,
// or inherits from), a superuser or BYPASSRLS role. Neither attribute is
// inherited, but a member can switch into the role (SET ROLE) and then
// bypass row-level security, so any membership counts, whatever its INHERIT
// and SET options. Cloud SQL's users are members of cloudsqlsuperuser, which
// has neither.
func (d *DB) checkRole(ctx context.Context) error {
	var roles []string
	rows, err := d.Pool.Query(ctx, `SELECT rolname FROM pg_roles
		WHERE (rolsuper OR rolbypassrls) AND pg_has_role(current_user, oid, 'MEMBER') ORDER BY rolname`)
	if err == nil {
		roles, err = pgx.CollectRows(rows, pgx.RowTo[string])
	}
	if err != nil {
		return fmt.Errorf("checking database role: %w", err)
	}
	if len(roles) > 0 {
		return fmt.Errorf("%w (%s)", ErrBypassesRLS, strings.Join(roles, ", "))
	}
	return nil
}

var ErrAdminReadRuntime = errors.New("database role holds the admin service's read role (" + AdminReadRole + "), whose policy shows every workspace's rows, so tenant isolation wouldn't be enforced; give the admin service its own login")

// checkNotAdminRead refuses a login that holds AdminReadRole, by
// inheritance or by being able to SET ROLE to it (the role that made it,
// when one role both owns the tables and serves, holds only its admin
// option, which is neither): its admin_read policy
// (USING (true)) applies to its members, so the core, which may read every
// column, would see every workspace's rows inside InTenant. Only the admin
// service's own login may hold it.
func (d *DB) checkNotAdminRead(ctx context.Context) error {
	var holds bool
	err := d.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1
		AND (pg_has_role(current_user, oid, 'USAGE') OR pg_has_role(current_user, oid, 'SET')))`, AdminReadRole).Scan(&holds)
	if err != nil {
		return fmt.Errorf("checking database role: %w", err)
	}
	if holds {
		return ErrAdminReadRuntime
	}
	return nil
}

// InTenant runs fn in a transaction scoped to one tenant: row-level security
// limits every tenant-scoped table to that tenant's rows. The transaction
// commits if fn returns nil and rolls back otherwise.
func (d *DB) InTenant(ctx context.Context, tenantID string, fn func(pgx.Tx) error) error {
	return d.InTenantTx(ctx, tenantID, pgx.TxOptions{}, fn)
}

// InTenantTx is InTenant with transaction options, e.g. repeatable read for
// a consistent snapshot across several queries.
func (d *DB) InTenantTx(ctx context.Context, tenantID string, opts pgx.TxOptions, fn func(pgx.Tx) error) error {
	if tenantID == "" {
		return errors.New("db: empty tenant ID")
	}
	return pgx.BeginTxFunc(ctx, d.Pool, opts, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenantID); err != nil {
			return err
		}
		return fn(tx)
	})
}

// Exclusive runs fn unless another instance is running work under the same
// key (a session advisory lock), reporting whether it ran. For background
// sweeps over every tenant, which only one instance needs to do at a time.
//
// Every key this instance holds is held on one pool connection, taken while
// any of them is held: a connection for each would leave the pool short of
// the connections fn itself needs (with three sweeps and a pool of four, one
// connection was left for every sweep's work and every request).
func (d *DB) Exclusive(ctx context.Context, key int64, fn func()) (bool, error) {
	if got, err := d.locks.lock(ctx, d.Pool, key); err != nil || !got {
		return false, err
	}
	defer d.locks.unlock(ctx, key)
	fn()
	return true, nil
}

// exclusiveLocks are the advisory locks this instance holds for Exclusive,
// on one connection (conn, from the pool while held isn't empty).
type exclusiveLocks struct {
	mu     sync.Mutex
	conn   *pgxpool.Conn
	held   map[int64]bool
	broken bool // an unlock failed: closed, not returned to the pool, once held is empty
}

// lockWait bounds each wait Exclusive makes under exclusiveLocks.mu (for a
// connection, or a lock query), so a stalled one holds up the other keys'
// callers only that long.
const lockWait = 10 * time.Second

func (l *exclusiveLocks) lock(ctx context.Context, pool *pgxpool.Pool, key int64) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	// Held here already (session locks are reentrant, so Postgres wouldn't
	// refuse it), or on a connection that may still hold an old lock.
	if l.held[key] || l.broken {
		return false, nil
	}
	ctx, cancel := context.WithTimeout(ctx, lockWait)
	defer cancel()
	if l.conn == nil {
		conn, err := pool.Acquire(ctx)
		if err != nil {
			return false, err
		}
		l.conn = conn
	}
	var got bool
	err := l.conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, key).Scan(&got)
	if err == nil && got {
		if l.held == nil {
			l.held = map[int64]bool{}
		}
		l.held[key] = true
		return true, nil
	}
	if err != nil {
		l.broken = true // the connection may be gone, with any locks on it
	}
	l.release()
	return false, err
}

func (l *exclusiveLocks) unlock(ctx context.Context, key int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.held, key)
	// Even if ctx ended: the lock must go before the connection does back
	// to the pool.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), lockWait)
	defer cancel()
	if _, err := l.conn.Exec(ctx, `SELECT pg_advisory_unlock($1)`, key); err != nil {
		l.broken = true
	}
	l.release()
}

// release gives the connection back once no lock is held on it (closing it
// if an unlock failed: closing releases its locks). Called with mu held.
func (l *exclusiveLocks) release() {
	if len(l.held) > 0 || l.conn == nil {
		return
	}
	if l.broken {
		l.conn.Conn().Close(context.Background())
		l.broken = false
	}
	l.conn.Release()
	l.conn = nil
}

// ReadTenants calls fn once for each of tenants, in turn, inside one
// read-only transaction, with app.tenant_id set to that tenant for the
// call: for reading across many workspaces at once (the console's pages for
// an app read the workspaces it keeps for its users). Row-level security
// applies to every query exactly as in InTenant: during fn, scoped tables
// show only that tenant's rows. Nothing can be written (the transaction is
// read-only), and the tenant is cleared before it ends.
func (d *DB) ReadTenants(ctx context.Context, tenants []string, fn func(tenant string, tx pgx.Tx) error) error {
	if len(tenants) == 0 {
		return nil
	}
	return pgx.BeginTxFunc(ctx, d.Pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		for _, t := range tenants {
			if t == "" {
				return errors.New("db: empty tenant ID")
			}
			if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, t); err != nil {
				return err
			}
			if err := fn(t, tx); err != nil {
				return err
			}
		}
		_, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', '', true)`)
		return err
	})
}

// Touch records that a workspace is in use now (tenants.active_at), at
// most once an hour: a fresh one costs a lookup, no write.
func (d *DB) Touch(ctx context.Context, tenant string) error {
	_, err := d.Pool.Exec(ctx, `UPDATE tenants SET active_at = now() WHERE id = $1::uuid
		AND (active_at IS NULL OR active_at < now() - interval '1 hour')`, tenant)
	return err
}

// Tenants lists every tenant's ID, for work that runs across tenants (each
// tenant's part still runs inside InTenant).
func (d *DB) Tenants(ctx context.Context) ([]string, error) {
	rows, _ := d.Pool.Query(ctx, `SELECT id::text FROM tenants ORDER BY id`)
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// TenantsAfter lists the IDs of the tenants after the one given, in ID
// order (all of them, after ""): for a pass over every tenant that resumes
// where it stopped.
func (d *DB) TenantsAfter(ctx context.Context, after string) ([]string, error) {
	if after == "" {
		return d.Tenants(ctx)
	}
	rows, _ := d.Pool.Query(ctx, `SELECT id::text FROM tenants WHERE id > $1::uuid ORDER BY id`, after)
	return pgx.CollectRows(rows, pgx.RowTo[string])
}
