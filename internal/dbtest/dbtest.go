// Package dbtest gives tests their own migrated database.
//
// Tests need WAKESHIFT_TEST_DATABASE_URL: a Postgres URL for a role that can
// create roles and databases (`make test` starts one with Docker). Each call
// to New creates a fresh database owned by an ordinary role, so row-level
// security is enforced exactly as in production, and drops it afterwards.
// The database's owner migrates it; tests get a connection as another
// ordinary role that may only read and write, as the server can run in
// production (db.OpenWithOwner).
package dbtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/wakeshift/server/internal/db"
	"github.com/wakeshift/server/internal/pgtest"
)

const (
	runtimeRole = "wakeshift_test_runtime"
	runtimePass = "wakeshift_test_runtime"
)

// New returns a migrated database for this test, connected as an ordinary
// role that may only read and write. It's dropped when the test ends.
func New(t testing.TB) *db.DB {
	t.Helper()
	d, _ := NewWithURL(t)
	return d
}

// NewWithURL is New, also returning the URL of the database's owner (for
// tests that open their own connections or start a server: db.Open with it
// finds nothing left to migrate).
func NewWithURL(t testing.TB) (*db.DB, string) {
	t.Helper()
	u := EmptyURL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	d, err := db.OpenWithOwner(ctx, RuntimeURL(t, u), u)
	if err != nil {
		t.Fatalf("opening test database: %v", err)
	}
	owners.Store(d, u)
	t.Cleanup(func() { owners.Delete(d); d.Close() })
	return d, u
}

var owners sync.Map // *db.DB → its owner's URL

// Owner connects to d's database as its owner, for a test that changes the
// schema (adds a trigger, say): d's own role may only read and write. It's
// closed when the test ends.
func Owner(t testing.TB, d *db.DB) *pgx.Conn {
	t.Helper()
	u, ok := owners.Load(d)
	if !ok {
		t.Fatal("dbtest.Owner: not a database from dbtest.New")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, u.(string))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })
	return conn
}

// NewOwner returns a migrated database connected as its owner, for tests
// that change the schema themselves.
func NewOwner(t testing.TB) *db.DB {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	d, err := db.Open(ctx, EmptyURL(t))
	if err != nil {
		t.Fatalf("opening test database: %v", err)
	}
	t.Cleanup(d.Close)
	return d
}

// RuntimeURL is ownerURL's database as the tests' runtime role: an ordinary
// role that owns nothing (db.OpenWithOwner grants it reads and writes).
func RuntimeURL(t testing.TB, ownerURL string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, os.Getenv("WAKESHIFT_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatalf("connecting to WAKESHIFT_TEST_DATABASE_URL: %v", err)
	}
	defer admin.Close(context.Background())
	// Shared by every test database, as pgtest's owner role is.
	if _, err := admin.Exec(ctx, `CREATE ROLE `+runtimeRole+` LOGIN PASSWORD '`+runtimePass+`' NOSUPERUSER NOBYPASSRLS`); err != nil {
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || (pgErr.Code != "42710" && pgErr.Code != "23505") { // duplicate_object, unique_violation
			t.Fatalf("creating the test runtime role: %v", err)
		}
	}
	u, err := url.Parse(ownerURL)
	if err != nil {
		t.Fatal(err)
	}
	// Closed to other roles, as the hosted service's databases are: the
	// owner role grants the runtime role CONNECT.
	if _, err := admin.Exec(ctx, `REVOKE CONNECT, TEMPORARY ON DATABASE `+pgx.Identifier{strings.TrimPrefix(u.Path, "/")}.Sanitize()+` FROM PUBLIC`); err != nil {
		t.Fatalf("closing the test database: %v", err)
	}
	u.User = url.UserPassword(runtimeRole, runtimePass)
	return u.String()
}

// EmptyURL returns the URL of a new, empty database owned by an ordinary
// role (for an app's own storage). It's dropped when the test ends.
func EmptyURL(t testing.TB) string {
	t.Helper()
	return pgtest.EmptyURL(t)
}

// NewTenant creates a tenant (a workspace) and returns its ID.
func NewTenant(t testing.TB, d *db.DB) string {
	t.Helper()
	var id string
	if err := d.Pool.QueryRow(context.Background(), `INSERT INTO tenants (name) VALUES ('test') RETURNING id`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// Grant records app's grant for agent in tx's workspace, as a headless app
// gets it (confirmed by the app, an empty scope), expiring after expiresIn
// (negative: already expired), and returns its ID. The app and the agent
// must exist. Call it on the test's goroutine: it fails the test on error.
func Grant(t testing.TB, tx pgx.Tx, app, agent string, expiresIn time.Duration) string {
	t.Helper()
	var id string
	if err := tx.QueryRow(context.Background(), `INSERT INTO grants (tenant_id, app_id, kind, agent_id, scope, confirmed_by, expires_at)
		VALUES (current_tenant(), $1, 'agent', $2, '{}', 'app', now() + make_interval(secs => $3)) RETURNING id::text`,
		app, agent, expiresIn.Seconds()).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// AdminReadURL is d's database as a new login granted the admin service's
// read role (080_admin.sql), as the core grants it to the admin service's
// login (db.AdminReadLogin). The login is dropped when the test ends.
func AdminReadURL(t testing.TB, d *db.DB) string {
	t.Helper()
	ownerURL, ok := owners.Load(d)
	if !ok {
		t.Fatal("dbtest.AdminReadURL: not a database from dbtest.New")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, os.Getenv("WAKESHIFT_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatalf("connecting to WAKESHIFT_TEST_DATABASE_URL: %v", err)
	}
	defer admin.Close(context.Background())
	b := make([]byte, 4)
	rand.Read(b)
	login := "wakeshift_test_adminread_" + hex.EncodeToString(b)
	if _, err := admin.Exec(ctx, `CREATE ROLE `+login+` LOGIN PASSWORD 'adminread' NOSUPERUSER NOBYPASSRLS`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		if admin, err := pgx.Connect(ctx, os.Getenv("WAKESHIFT_TEST_DATABASE_URL")); err == nil {
			admin.Exec(ctx, `DROP ROLE IF EXISTS `+login)
			admin.Close(ctx)
		}
	})
	d2, err := db.OpenWithOwner(ctx, RuntimeURL(t, ownerURL.(string)), ownerURL.(string), db.AdminReadLogin(login))
	if err != nil {
		t.Fatalf("granting the admin read role: %v", err)
	}
	d2.Close()
	u, err := url.Parse(ownerURL.(string))
	if err != nil {
		t.Fatal(err)
	}
	u.User = url.UserPassword(login, "adminread")
	return u.String()
}

// HeldTimings shortens db.HeldIdleTimeout, db.HeldPing and
// db.HeldPingTimeout until the test ends (db.SetHeldTimings).
func HeldTimings(t testing.TB, idle, ping, timeout time.Duration) {
	t.Cleanup(db.SetHeldTimings(idle, ping, timeout))
}
