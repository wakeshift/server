package db_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/wakeshift/server/internal/db"
	"github.com/wakeshift/server/internal/dbtest"
)

func newTenant(t *testing.T, d *db.DB, name string) string {
	t.Helper()
	var id string
	if err := d.Pool.QueryRow(context.Background(), `INSERT INTO tenants (name) VALUES ($1) RETURNING id`, name).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// A tenant-scoped table, set up the way every migration must set one up.
func createScopedTable(t *testing.T, d *db.DB) {
	t.Helper()
	_, err := d.Pool.Exec(context.Background(), `
		CREATE TABLE notes (
			tenant_id uuid NOT NULL REFERENCES tenants(id),
			body      text NOT NULL
		);
		ALTER TABLE notes ENABLE ROW LEVEL SECURITY;
		ALTER TABLE notes FORCE ROW LEVEL SECURITY;
		CREATE POLICY tenant_isolation ON notes
			USING (tenant_id = current_tenant()) WITH CHECK (tenant_id = current_tenant());`)
	if err != nil {
		t.Fatal(err)
	}
}

func TestTenantIsolation(t *testing.T) {
	d := dbtest.NewOwner(t)
	ctx := context.Background()
	createScopedTable(t, d)
	a, b := newTenant(t, d, "a"), newTenant(t, d, "b")

	insert := func(tenant, owner, body string) error {
		return d.InTenant(ctx, tenant, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO notes (tenant_id, body) VALUES ($1, $2)`, owner, body)
			return err
		})
	}
	if err := insert(a, a, "a's note"); err != nil {
		t.Fatal(err)
	}
	if err := insert(b, b, "b's note"); err != nil {
		t.Fatal(err)
	}
	// Writing into another tenant is refused.
	if err := insert(a, b, "planted"); err == nil || !strings.Contains(err.Error(), "row-level security") {
		t.Fatalf("cross-tenant insert: %v", err)
	}

	read := func(tenant string) (bodies []string) {
		d.InTenant(ctx, tenant, func(tx pgx.Tx) error {
			rows, _ := tx.Query(ctx, `SELECT body FROM notes`)
			bodies, _ = pgx.CollectRows(rows, pgx.RowTo[string])
			return nil
		})
		return bodies
	}
	if got := read(a); len(got) != 1 || got[0] != "a's note" {
		t.Fatalf("tenant a reads %v", got)
	}
	// Outside a tenant transaction, scoped tables read as empty.
	var n int
	d.Pool.QueryRow(ctx, `SELECT count(*) FROM notes`).Scan(&n)
	if n != 0 {
		t.Fatalf("unscoped read saw %d rows", n)
	}
}

// ReadTenants reads each tenant as InTenant would: only its rows while it's
// set, never another's, and nothing can be written.
func TestReadTenants(t *testing.T) {
	d := dbtest.NewOwner(t)
	ctx := context.Background()
	createScopedTable(t, d)
	a, b, c := newTenant(t, d, "a"), newTenant(t, d, "b"), newTenant(t, d, "c")
	for _, tenant := range []string{a, b} {
		err := d.InTenant(ctx, tenant, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO notes (tenant_id, body) VALUES ($1, $2)`, tenant, tenant)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	seen := map[string][]string{}
	err := d.ReadTenants(ctx, []string{b, a, c}, func(tenant string, tx pgx.Tx) error {
		rows, _ := tx.Query(ctx, `SELECT body FROM notes`)
		got, err := pgx.CollectRows(rows, pgx.RowTo[string])
		seen[tenant] = got
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(seen[a]) != 1 || seen[a][0] != a || len(seen[b]) != 1 || seen[b][0] != b || len(seen[c]) != 0 {
		t.Fatalf("read %v", seen)
	}
	err = d.ReadTenants(ctx, []string{a}, func(tenant string, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO notes (tenant_id, body) VALUES ($1, 'written')`, tenant)
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("a write in ReadTenants: %v", err)
	}
	if err := d.ReadTenants(ctx, []string{a, ""}, func(string, pgx.Tx) error { return nil }); err == nil {
		t.Fatal("an empty tenant ID was read")
	}
}

// Every table that has a tenant_id is isolated by row-level security, and
// no other table is added without a deliberate exemption.
func TestEveryTenantTableIsIsolated(t *testing.T) {
	d := dbtest.New(t)
	ctx := context.Background()
	// Exempt: tenants themselves, the migration log, who is who and who
	// belongs where (read before a workspace is chosen; 007_accounts.sql),
	// platform-level OAuth and app state (010_oauth.sql), invites (a
	// user's, used before the invitee has a workspace; 019_invites.sql), and
	// topics' platform-level tables: which workspaces a
	// publish is copied into, the app's recent publishes and its publish URLs
	// (026_topics.sql), and the app log: an app's own history, platform-level
	// like the app, read only by the members of the app's organization
	// (apps.Access, checked in apps.ListLog; 028_app_log.sql), an app's
	// keys: public keys, platform-level like the app,
	// managed only by its organization's members and read
	// by the app API to verify the app's assertions (031_app_keys.sql), a
	// publish's copies still to be made, platform-level like topic_members
	// (042_topic_fanout.sql), and which workspaces have an MCP Events
	// webhook, so the deliverer visits only those (IDs and an expiry; 044),
	// and which workspaces the pipeline's sweep visits (IDs and an event
	// sequence number; 049), and deleted apps whose users' data is still
	// being deleted (app IDs and times; 054),
	// and app assertions' IDs until they expire (064), and organizations:
	// who manages which platform-level app, read before any workspace is
	// chosen, like users and apps (organizations, organization_members,
	// organization_invites and the organization's own log, org_log; 071),
	// and the admin service's tables: its log of our own admin actions, its
	// Slack cursors, and the daily usage rollup (IDs and counts; 080), and
	// sign-ups' cap and waitlist: read before the person has any workspace
	// (a waitlisted person never gets one), like users (106), and each app's
	// count of recipients its multi-recipient emits named this minute (an
	// app ID, a time and a count; 108), and which workspace a pending-addition
	// record is in, by its random ID, so /calls/addition finds it before it
	// knows the workspace (IDs only; 109), and the growth milestones the
	// admin service has recorded and posted (app and organization IDs, a
	// count and times; 124), and deleted_key_ids, a one-way fingerprint of
	// each deleted app's key IDs, so they can't be registered again (130),
	// and which workspaces have an agent-hosted webhook destination, so its
	// deliverer visits only those (IDs only; 132), and subscribe_vouchers: a
	// voucher has no workspace until it's redeemed (hashes and IDs, an event
	// name, an app user ID that tenants already keeps, option values; its
	// subscription lives in a workspace), with subscribe_voucher_counters
	// (an app ID, times and counts; 135), and waitlist_keys: public keys
	// from @wakeshift/cli's links held against a waitlist entry, which has
	// no workspace, like waitlist (IDs and public keys; 139), and
	// bot_webhooks: which destinations each Grok Bot key has across
	// workspaces, so a proof by the key can join the bot's webhook and the
	// removal page can stop all of them (IDs, a public key's fingerprint and
	// a removal token's hash), with bot_setup_targets, which workspaces have
	// open setups for the expiry pass (IDs only; 140), and bot_routines:
	// each Grok Bot routine's one binding, the lock binds and joins take
	// (an HMAC of the bot and URL, a removal token's hash and a key's
	// fingerprint; 142), and rate_counts:
	// request limits counted across instances, before any workspace is
	// known (an HMAC of the limit and its subject, a time and a count; 141),
	// and bot_open_challenges: which open Grok Bot challenges each bot has
	// across workspaces, so the connector can fetch one by the bot's ID
	// before any workspace is known (IDs, a hash of the bot ID, a purpose and
	// an expiry; 146), and push_stream_holds: each app's open MCP Events
	// push streams counted across instances (a random holder ID, an app ID
	// and a time; 152), and expiry_state: where the expiry pass stands
	// (a workspace ID, flags and times; 161).
	unscoped := map[string]bool{"tenants": true, "schema_migrations": true, "users": true, "identities": true, "memberships": true, "apps": true,
		"oauth_clients": true, "oauth_signing_keys": true, "invites": true,
		"topic_members": true, "topic_events": true, "topic_publish_urls": true, "app_log": true,
		"app_keys": true, "topic_fanout": true, "mcp_event_targets": true, "pipeline_due": true, "app_deletions": true, "app_assertion_ids": true,
		"organizations": true, "organization_members": true, "organization_invites": true, "org_log": true,
		"admin_log": true, "admin_alert_state": true, "usage_daily": true,
		"signup_cap": true, "waitlist": true, "app_emit_counters": true, "pending_addition_ids": true,
		"growth_milestones": true, "deleted_key_ids": true, "agent_webhook_targets": true, "subscribe_vouchers": true, "subscribe_voucher_counters": true,
		"waitlist_keys": true, "bot_webhooks": true, "bot_setup_targets": true,
		"rate_counts": true, "bot_routines": true, "bot_open_challenges": true, "connector_call_index": true, "connector_call_claims": true, "connector_sessions": true,
		"push_stream_holds": true, "expiry_state": true}
	rows, err := d.Pool.Query(ctx, `
		SELECT c.relname,
		       c.relrowsecurity AND c.relforcerowsecurity,
		       EXISTS (SELECT 1 FROM pg_policy p WHERE p.polrelid = c.oid),
		       EXISTS (SELECT 1 FROM pg_attribute a WHERE a.attrelid = c.oid AND a.attname = 'tenant_id' AND NOT a.attisdropped)
		FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'public' AND c.relkind IN ('r', 'p') AND NOT c.relispartition`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	checked := map[string]bool{}
	for rows.Next() {
		var name string
		var forced, hasPolicy, hasTenant bool
		if err := rows.Scan(&name, &forced, &hasPolicy, &hasTenant); err != nil {
			t.Fatal(err)
		}
		if unscoped[name] {
			continue
		}
		checked[name] = true
		if !hasTenant || !forced || !hasPolicy {
			t.Errorf("table %s: tenant_id=%v forced RLS=%v policy=%v; tenant-scoped tables need all three (see 001_tenants.sql)", name, hasTenant, forced, hasPolicy)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	// A query that saw no tables would pass everything: the core's own
	// tenant tables must have been among those checked.
	for _, table := range []string{"events", "deliveries", "subscriptions", "grants", "audit_log"} {
		if !checked[table] {
			t.Errorf("tenant table %s wasn't checked: the catalog query missed it", table)
		}
	}

	// And the policies are the template's and the admin service's read
	// policy, exactly: policies combine with OR, so any other (or a looser
	// one) would open the table to every tenant. admin_read lets only
	// wakeshift_admin_read read, and only read (080_admin.sql): never
	// another role, another command, or a write.
	rows, err = d.Pool.Query(ctx, `
		SELECT p.polrelid::regclass::text, p.polname,
		       p.polcmd = '*' AND p.polpermissive AND p.polroles = '{0}'
		       AND pg_get_expr(p.polqual, p.polrelid) = '(tenant_id = current_tenant())'
		       AND pg_get_expr(p.polwithcheck, p.polrelid) = '(tenant_id = current_tenant())',
		       p.polcmd = 'r' AND p.polpermissive AND p.polroles = ARRAY[(SELECT oid FROM pg_roles WHERE rolname = 'wakeshift_admin_read')]
		       AND pg_get_expr(p.polqual, p.polrelid) = 'true' AND p.polwithcheck IS NULL
		FROM pg_policy p`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	isolated, readable := map[string]bool{}, map[string]bool{}
	for rows.Next() {
		var table, policy string
		var standard, adminRead bool
		if err := rows.Scan(&table, &policy, &standard, &adminRead); err != nil {
			t.Fatal(err)
		}
		switch {
		case policy == "tenant_isolation" && standard:
			isolated[table] = true
		case policy == "admin_read" && adminRead:
			readable[table] = true
		default:
			t.Errorf("table %s: policy %s is neither the tenant_isolation template (see 001_tenants.sql) nor admin_read (080_admin.sql)", table, policy)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	// Every tenant table has both: the admin service reads every
	// workspace's rows through admin_read (with only the columns granted).
	for table := range isolated {
		if !readable[table] {
			t.Errorf("table %s: no admin_read policy; add CREATE POLICY admin_read ON %s FOR SELECT TO wakeshift_admin_read USING (true) in its migration (no column grants unless the admin service needs them)", table, table)
		}
	}
	for table := range readable {
		if !isolated[table] {
			t.Errorf("table %s: admin_read without tenant_isolation", table)
		}
	}
}

func TestOpenRefusesRolesThatBypassRLS(t *testing.T) {
	adminURL := os.Getenv("WAKESHIFT_TEST_DATABASE_URL")
	if adminURL == "" {
		t.Fatal("set WAKESHIFT_TEST_DATABASE_URL")
	}
	// The test admin is a superuser (it creates roles and databases).
	if _, err := db.Open(context.Background(), adminURL); !errors.Is(err, db.ErrBypassesRLS) {
		t.Fatalf("Open as superuser: %v", err)
	}
}

// A login that can switch into (SET ROLE) or inherit from a superuser or
// BYPASSRLS role is refused too; one whose roles have neither (Cloud SQL's
// users are members of cloudsqlsuperuser, which has neither) is accepted.
func TestOpenRefusesMembersOfRolesThatBypassRLS(t *testing.T) {
	adminURL := os.Getenv("WAKESHIFT_TEST_DATABASE_URL")
	if adminURL == "" {
		t.Fatal("set WAKESHIFT_TEST_DATABASE_URL")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	b := make([]byte, 4)
	rand.Read(b)
	suffix := hex.EncodeToString(b)
	var created []string
	dbName := "wakeshift_test_roles_" + suffix
	t.Cleanup(func() {
		ctx := context.Background()
		admin, err := pgx.Connect(ctx, adminURL)
		if err != nil {
			return
		}
		defer admin.Close(ctx)
		admin.Exec(ctx, `DROP DATABASE IF EXISTS `+dbName+` WITH (FORCE)`)
		for i := len(created) - 1; i >= 0; i-- {
			admin.Exec(ctx, `DROP ROLE IF EXISTS `+created[i])
		}
	})
	exec := func(sql string) {
		t.Helper()
		if _, err := admin.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	role := func(name, attrs string) string {
		name += "_" + suffix
		exec(`CREATE ROLE ` + name + ` ` + attrs)
		created = append(created, name)
		return name
	}
	bypass := role("rls_bypass", "NOLOGIN BYPASSRLS")
	super := role("rls_super", "NOLOGIN SUPERUSER")
	plain := role("rls_plain", "NOLOGIN")             // like cloudsqlsuperuser: neither attribute
	middle := role("rls_middle", "NOLOGIN NOINHERIT") // a step between the login and bypass
	login := role("rls_login", "LOGIN PASSWORD 'rls_login' NOSUPERUSER NOBYPASSRLS")
	exec(`CREATE DATABASE ` + dbName + ` OWNER ` + login)
	u, err := url.Parse(adminURL)
	if err != nil {
		t.Fatal(err)
	}
	u.User = url.UserPassword(login, "rls_login")
	u.Path = "/" + dbName
	open := func() error {
		d, err := db.Open(ctx, u.String())
		if err == nil {
			d.Close()
		}
		return err
	}

	exec(`GRANT ` + plain + ` TO ` + login)
	if err := open(); err != nil {
		t.Fatalf("a member only of a role without BYPASSRLS was refused: %v", err)
	}
	for _, c := range []struct{ name, role, opts string }{
		{"inherit and set", bypass, "WITH INHERIT TRUE, SET TRUE"},
		{"set only", bypass, "WITH INHERIT FALSE, SET TRUE"},
		{"inherit only", bypass, "WITH INHERIT TRUE, SET FALSE"},
		{"superuser", super, "WITH SET TRUE"},
	} {
		exec(`GRANT ` + c.role + ` TO ` + login + ` ` + c.opts)
		if err := open(); !errors.Is(err, db.ErrBypassesRLS) {
			t.Errorf("%s: Open = %v, want ErrBypassesRLS", c.name, err)
		}
		exec(`REVOKE ` + c.role + ` FROM ` + login)
	}
	// Through another role: login → middle → bypass.
	exec(`GRANT ` + bypass + ` TO ` + middle)
	exec(`GRANT ` + middle + ` TO ` + login + ` WITH INHERIT FALSE, SET TRUE`)
	if err := open(); !errors.Is(err, db.ErrBypassesRLS) {
		t.Errorf("through another role: Open = %v, want ErrBypassesRLS", err)
	}
	exec(`REVOKE ` + middle + ` FROM ` + login)
	if err := open(); err != nil {
		t.Fatalf("after the memberships were revoked: %v", err)
	}
}

// With an owner, the server's role only reads and writes: it can't turn off
// row-level security, drop audit_log's append-only trigger, update the audit
// log or record migrations, and tables a later migration adds are granted
// to it too. A role that owns the tables, or can become their owner, is
// refused as the runtime role.
func TestOpenWithOwnerGrantsOnlyReadsAndWrites(t *testing.T) {
	ctx := context.Background()
	d, ownerURL := dbtest.NewWithURL(t)
	tenant := newTenant(t, d, "a")
	err := d.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO agents (tenant_id, id) VALUES ($1, 'claude-code')`, tenant); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE agents SET display_name = 'Claude' WHERE id = 'claude-code'`); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO audit_log (tenant_id, actor, action) VALUES ($1, 'test', 'test')`, tenant)
		return err
	})
	if err != nil {
		t.Fatalf("reads and writes: %v", err)
	}
	for _, sql := range []string{
		`ALTER TABLE agents DISABLE ROW LEVEL SECURITY`,
		`ALTER TABLE agents NO FORCE ROW LEVEL SECURITY`,
		`DROP POLICY tenant_isolation ON agents`,
		`DROP TRIGGER audit_log_append_only ON audit_log`,
		`UPDATE audit_log SET action = 'changed'`,
		`INSERT INTO schema_migrations (name) VALUES ('999_planted.sql')`,
		`TRUNCATE agents CASCADE`,
	} {
		_, err := d.Pool.Exec(ctx, sql)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "42501" && pgErr.Code != "42P01" {
			t.Errorf("%s: %v, want permission denied", sql, err)
		}
	}

	// A later migration's table (default privileges).
	owner, err := db.Open(ctx, ownerURL)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	runtimeURL := dbtest.RuntimeURL(t, ownerURL)
	later := fstest.MapFS{"999_later.sql": {Data: []byte(`CREATE TABLE later (id bigserial PRIMARY KEY, body text);`)}}
	if err := owner.Migrate(ctx, later); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Pool.Exec(ctx, `INSERT INTO later (body) VALUES ('x')`); err != nil {
		t.Fatalf("a later migration's table: %v", err)
	}

	// The owner as the runtime role too, or a runtime role that's a member
	// of the owner's.
	if _, err := db.OpenWithOwner(ctx, ownerURL, ownerURL); !errors.Is(err, db.ErrRuntimeOwns) {
		t.Fatalf("owner as the runtime role: %v", err)
	}
	var ownerRole, runtimeRole string
	owner.Pool.QueryRow(ctx, `SELECT current_user`).Scan(&ownerRole)
	d.Pool.QueryRow(ctx, `SELECT current_user`).Scan(&runtimeRole)
	admin, err := pgx.Connect(ctx, os.Getenv("WAKESHIFT_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	// A member of this test's own (the runtime role is shared by every test
	// database).
	b := make([]byte, 4)
	rand.Read(b)
	member := "rls_member_" + hex.EncodeToString(b)
	if _, err := admin.Exec(ctx, `CREATE ROLE `+member+` LOGIN PASSWORD 'rls_member' IN ROLE `+ownerRole); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		if admin, err := pgx.Connect(ctx, os.Getenv("WAKESHIFT_TEST_DATABASE_URL")); err == nil {
			admin.Exec(ctx, `DROP ROLE IF EXISTS `+member)
			admin.Close(ctx)
		}
	})
	u, _ := url.Parse(runtimeURL)
	u.User = url.UserPassword(member, "rls_member")
	if _, err := db.OpenWithOwner(ctx, u.String(), ownerURL); !errors.Is(err, db.ErrRuntimeOwns) {
		t.Fatalf("a member of the owner's role as the runtime role: %v", err)
	}
}

// Exclusive runs work on one instance at a time: a second caller under the
// same key, in this instance or another, skips while the first runs, and
// runs once it's done. The keys an instance holds take one pool connection
// between them.
func TestExclusive(t *testing.T) {
	ctx := context.Background()
	d, url := dbtest.NewWithURL(t)
	other, err := db.Open(ctx, url) // another instance
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	started, release, done := make(chan struct{}), make(chan struct{}), make(chan bool, 1)
	go func() {
		ran, err := d.Exclusive(ctx, 42, func() { close(started); <-release })
		if err != nil {
			t.Error(err)
		}
		if !ran {
			close(started) // so the test fails rather than hangs
		}
		done <- ran
	}()
	<-started
	if t.Failed() {
		t.FailNow()
	}
	instances := []struct {
		name string
		d    *db.DB
	}{{"this instance", d}, {"another instance", other}}
	for _, x := range instances {
		if ran, err := x.d.Exclusive(ctx, 42, func() { t.Error("ran while another caller held the key") }); err != nil || ran {
			t.Fatalf("second caller in %s: ran %v, %v", x.name, ran, err)
		}
	}
	if ran, err := d.Exclusive(ctx, 43, func() {
		if n := d.Pool.Stat().AcquiredConns(); n != 1 {
			t.Errorf("two keys held on %d pool connections, want 1", n)
		}
	}); err != nil || !ran {
		t.Fatalf("another key: ran %v, %v", ran, err)
	}
	if ran, err := other.Exclusive(ctx, 43, func() {}); err != nil || !ran {
		t.Fatalf("a released key, in another instance: ran %v, %v", ran, err)
	}
	close(release)
	if !<-done {
		t.Fatal("the first caller didn't run")
	}
	if n := d.Pool.Stat().AcquiredConns(); n != 0 {
		t.Fatalf("%d pool connections still taken with no key held", n)
	}
	for _, x := range instances {
		if ran, err := x.d.Exclusive(ctx, 42, func() {}); err != nil || !ran {
			t.Fatalf("after, in %s: ran %v, %v", x.name, ran, err)
		}
	}
}

func TestMigrationsAreIdempotent(t *testing.T) {
	_, url := dbtest.NewWithURL(t)
	d, err := db.Open(context.Background(), url) // migrates again
	if err != nil {
		t.Fatal(err)
	}
	d.Close()
}

// No two migrations share a number: two branches that each added NNN_*
// would otherwise apply in a different order on different databases.
func TestMigrationNumbersAreUnique(t *testing.T) {
	files, err := os.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]string{}
	for _, f := range files {
		n, _, _ := strings.Cut(f.Name(), "_")
		if other, ok := seen[n]; ok {
			t.Errorf("%s and %s share number %s: renumber the unmerged one", other, f.Name(), n)
		}
		seen[n] = f.Name()
	}
}

// Every index built CONCURRENTLY is dropped (CONCURRENTLY, IF EXISTS) first
// in the same file: a build that fails part-way leaves an INVALID index, and
// the retry's CREATE ... IF NOT EXISTS would skip it, leaving an index the
// planner never uses. 092 didn't, and is applied (097 builds its index
// again). An index production builds by hand ahead of a release (on a
// table too large to build at startup) has no DROP, which would rebuild
// it: its file relies on the migrator's validity check alone
// (dropInvalidIndexes), and is listed in prebuilt.
func TestConcurrentIndexBuildsDropFirst(t *testing.T) {
	applied := map[string]bool{"092_group_routes.sql": true}
	prebuilt := map[string]bool{"167_deliveries_grant_paced_index.sql": true}
	create := regexp.MustCompile(`(?im)^\s*CREATE\s+(?:UNIQUE\s+)?INDEX\s+CONCURRENTLY\s+(?:IF\s+NOT\s+EXISTS\s+)?(\w+)`)
	files, err := os.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if applied[f.Name()] || prebuilt[f.Name()] {
			continue
		}
		b, err := os.ReadFile(filepath.Join("migrations", f.Name()))
		if err != nil {
			t.Fatal(err)
		}
		sql := string(b)
		for _, m := range create.FindAllStringSubmatchIndex(sql, -1) {
			name := sql[m[2]:m[3]]
			drop := regexp.MustCompile(`(?im)^\s*DROP\s+INDEX\s+CONCURRENTLY\s+IF\s+EXISTS\s+` + regexp.QuoteMeta(name) + `\s*;`)
			if loc := drop.FindStringIndex(sql); loc == nil || loc[0] > m[0] {
				t.Errorf("%s builds %s CONCURRENTLY without DROP INDEX CONCURRENTLY IF EXISTS %s; before it", f.Name(), name, name)
			}
		}
	}
}

// A migration marked "-- migrate:no-transaction" runs outside a
// transaction, so it can build an index CONCURRENTLY; once applied, it isn't
// run again.
func TestMigrationOutsideATransaction(t *testing.T) {
	d := dbtest.NewOwner(t)
	ctx := context.Background()
	files := fstest.MapFS{
		"900_table.sql": {Data: []byte("CREATE TABLE mig_notes (body text);\n")},
		"901_index.sql": {Data: []byte("-- migrate:no-transaction\n-- One index, rebuilt if a failed build left it invalid.\n" +
			"DROP INDEX CONCURRENTLY IF EXISTS mig_notes_body;\nCREATE INDEX CONCURRENTLY IF NOT EXISTS mig_notes_body\n    ON mig_notes (body);\n")},
	}
	for range 2 {
		if err := d.Migrate(ctx, files); err != nil {
			t.Fatal(err)
		}
	}
	var valid bool
	if err := d.Pool.QueryRow(ctx, `SELECT i.indisvalid FROM pg_index i JOIN pg_class c ON c.oid = i.indexrelid WHERE c.relname = 'mig_notes_body'`).Scan(&valid); err != nil || !valid {
		t.Fatalf("index valid %v, %v", valid, err)
	}
	var n int
	d.Pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations WHERE name IN ('900_table.sql', '901_index.sql')`).Scan(&n)
	if n != 2 {
		t.Fatalf("%d recorded", n)
	}
	// Without the marker, CONCURRENTLY fails (and nothing is recorded).
	bad := fstest.MapFS{"902_bad.sql": {Data: []byte("CREATE INDEX CONCURRENTLY mig_notes_bad ON mig_notes (body);\n")}}
	if err := d.Migrate(ctx, bad); err == nil || !strings.Contains(err.Error(), "902_bad") {
		t.Fatalf("a CONCURRENTLY build in a transaction: %v", err)
	}
}

// The per-tenant data migration pattern (005_deliveries.sql) sees each
// tenant's rows; a plain INSERT … SELECT would see none.
func TestDataMigrationPatternSeesEveryTenant(t *testing.T) {
	d := dbtest.NewOwner(t)
	ctx := context.Background()
	createScopedTable(t, d)
	a, b := newTenant(t, d, "a"), newTenant(t, d, "b")
	for _, tenant := range []string{a, b} {
		d.InTenant(ctx, tenant, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO notes (tenant_id, body) VALUES ($1, 'x')`, tenant)
			return err
		})
	}
	var plain, looped int
	d.Pool.QueryRow(ctx, `SELECT count(*) FROM notes`).Scan(&plain)
	_, err := d.Pool.Exec(ctx, `
		CREATE TEMP TABLE seen (n int);
		DO $$
		DECLARE t uuid;
		BEGIN
			FOR t IN SELECT id FROM tenants LOOP
				PERFORM set_config('app.tenant_id', t::text, true);
				INSERT INTO seen SELECT count(*) FROM notes;
			END LOOP;
		END $$;`)
	if err != nil {
		t.Fatal(err)
	}
	d.Pool.QueryRow(ctx, `SELECT sum(n) FROM seen`).Scan(&looped)
	if plain != 0 || looped != 2 {
		t.Fatalf("plain read %d rows, per-tenant loop %d", plain, looped)
	}
}

// The server's connections bound every statement (db.StatementTimeout),
// every idle open transaction (db.IdleInTransactionTimeout) and every lock
// wait (db.LockTimeout), and check for a vanished client
// (db.ClientCheckInterval), as connection parameters: an ALTER ROLE
// setting is lost when the role is made again, and production once ran a
// statement for 25 hours. The pool's connections and those outside it
// (Conn: the listeners) alike; those outside it also end once idle for
// db.HeldIdleTimeout, and the pool pings a connection before handing it
// out, briefly.
func TestConnectionsBoundStatements(t *testing.T) {
	d := dbtest.New(t)
	ctx := context.Background()
	conn, err := d.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	want := map[string]string{
		"statement_timeout":                   "5min",
		"idle_in_transaction_session_timeout": "20s",
		"lock_timeout":                        "30s",
		"client_connection_check_interval":    "10s",
		"idle_session_timeout":                "0",
	}
	if c := d.Pool.Config(); c.PingTimeout != db.PingTimeout || c.MaxConnLifetime != db.MaxConnLifetime || c.MaxConnLifetimeJitter == 0 {
		t.Errorf("pool: ping timeout %v, lifetime %v (jitter %v)", c.PingTimeout, c.MaxConnLifetime, c.MaxConnLifetimeJitter)
	}
	for name, q := range map[string]interface {
		QueryRow(context.Context, string, ...any) pgx.Row
	}{"pool": d.Pool, "conn": conn} {
		for setting, w := range want {
			if name == "conn" && setting == "idle_session_timeout" {
				w = "90s"
			}
			var got string
			if err := q.QueryRow(ctx, `SHOW `+setting).Scan(&got); err != nil {
				t.Fatal(err)
			}
			if got != w {
				t.Errorf("%s: %s %q, want %s", name, setting, got, w)
			}
		}
	}
}

// A transaction left open past the idle limit ends: it rolls back, and the
// pool replaces its connection, so the next transaction runs. (Shortened
// for the test: the limit is Postgres's to keep; this checks the pool
// recovers from it.)
func TestIdleTransactionEnds(t *testing.T) {
	d := dbtest.NewOwner(t)
	ctx := context.Background()
	createScopedTable(t, d)
	tenant := newTenant(t, d, "idle")
	err := d.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SET LOCAL idle_in_transaction_session_timeout = 200`); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO notes (tenant_id, body) VALUES ($1, 'left open')`, tenant); err != nil {
			return err
		}
		time.Sleep(time.Second)
		_, err := tx.Exec(ctx, `SELECT 1`)
		return err
	})
	if err == nil {
		t.Fatal("a transaction idle past the limit carried on")
	}
	var n int
	err = d.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM notes WHERE body = 'left open'`).Scan(&n)
	})
	if err != nil {
		t.Fatalf("the next transaction: %v", err)
	}
	if n != 0 {
		t.Fatalf("the ended transaction's insert stayed (%d rows)", n)
	}
}
