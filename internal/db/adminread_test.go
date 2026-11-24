package db_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/url"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/wakeshift/server/internal/db"
	"github.com/wakeshift/server/internal/dbtest"
)

// adminLogin makes a login role, as Cloud SQL makes the admin service's IAM
// user: it can log in, and nothing else. It's dropped when the test ends.
func adminLogin(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, os.Getenv("WAKESHIFT_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	b := make([]byte, 4)
	rand.Read(b)
	login := "wakeshift_test_admin_" + hex.EncodeToString(b)
	if _, err := admin.Exec(ctx, `CREATE ROLE `+login+` LOGIN PASSWORD 'admin' NOSUPERUSER NOBYPASSRLS`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		if admin, err := pgx.Connect(ctx, os.Getenv("WAKESHIFT_TEST_DATABASE_URL")); err == nil {
			admin.Exec(ctx, `DROP ROLE IF EXISTS `+login)
			admin.Close(ctx)
		}
	})
	return login
}

// connectAs connects to ownerURL's database as login.
func connectAs(t *testing.T, ownerURL, login string) *pgx.Conn {
	t.Helper()
	u, err := url.Parse(ownerURL)
	if err != nil {
		t.Fatal(err)
	}
	u.User = url.UserPassword(login, "admin")
	conn, err := pgx.Connect(context.Background(), u.String())
	if err != nil {
		t.Fatalf("connecting as %s: %v", login, err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })
	return conn
}

func denied(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "42501" // insufficient_privilege
}

// The admin service's read role (080_admin.sql): the core grants it to the
// admin service's login at startup; it reads every workspace's rows, only
// the columns granted, and writes nothing but its log and its cursors.
func TestAdminReadRole(t *testing.T) {
	ctx := context.Background()
	d, ownerURL := dbtest.NewWithURL(t)
	login := adminLogin(t)

	// Before the grant, the login can't even connect (the database is
	// closed to other roles).
	u, _ := url.Parse(ownerURL)
	u.User = url.UserPassword(login, "admin")
	if c, err := pgx.Connect(ctx, u.String()); err == nil {
		c.Close(ctx)
		t.Fatal("the login connected before it was granted the read role")
	}
	// A login that doesn't exist yet is skipped, not fatal.
	if d2, err := db.OpenWithOwner(ctx, dbtest.RuntimeURL(t, ownerURL), ownerURL, db.AdminReadLogin("wakeshift_test_admin_missing")); err != nil {
		t.Fatalf("a missing admin login stopped the core: %v", err)
	} else {
		d2.Close()
	}
	for _, bad := range []string{"wakeshift_admin_read", "cloudsqlsuperuser", "pg_read_all_data"} {
		if d2, err := db.OpenWithOwner(ctx, dbtest.RuntimeURL(t, ownerURL), ownerURL, db.AdminReadLogin(bad)); err == nil {
			d2.Close()
			t.Errorf("AdminReadLogin(%q) was accepted", bad)
		}
	}
	// The core's startup grants it, twice without harm.
	for range 2 {
		d2, err := db.OpenWithOwner(ctx, dbtest.RuntimeURL(t, ownerURL), ownerURL, db.AdminReadLogin(login))
		if err != nil {
			t.Fatal(err)
		}
		d2.Close()
	}

	// Two workspaces, with content and secrets in them.
	if _, err := d.Pool.Exec(ctx, `INSERT INTO apps (id, name) VALUES ('app', 'App')`); err != nil {
		t.Fatal(err)
	}
	a, b := dbtest.NewTenant(t, d), dbtest.NewTenant(t, d)
	for _, tenant := range []string{a, b} {
		err := d.InTenant(ctx, tenant, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO agents (tenant_id, id, display_name) VALUES ($1, 'claude-code', 'Ada''s secret project')`, tenant)
			if err != nil {
				return err
			}
			_, err = tx.Exec(ctx, `INSERT INTO grants (tenant_id, app_id, kind, agent_id, scope, shown, expires_at, confirmed_by)
				VALUES ($1, 'app', 'agent', 'claude-code', '{"secret": "scope"}', '{}', now() + interval '1 day', 'app')`, tenant)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	conn := connectAs(t, ownerURL, login)
	var n int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM agents`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("agents across workspaces: %d, %v", n, err)
	}
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM grants WHERE app_id = 'app' AND revoked_at IS NULL`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("grants across workspaces: %d, %v", n, err)
	}

	// Content and secrets: never granted.
	never := map[string][]string{
		"events":               {"subject", "body", "data", "attrs", "mentions"},
		"topic_events":         {"text", "data", "topics", "fingerprint", "text_fingerprint"},
		"deliveries":           {"last_error"},
		"mcp_event_webhooks":   {"url", "secret", "previous_secret", "last_error"},
		"stream_links":         {"id", "agent_id"},
		"connector_tickets":    {"token_hash", "id", "call_hash", "key_hash", "claim_thread"},
		"oauth_codes":          {"code_hash"},
		"oauth_refresh_tokens": {"token_hash"},
		"topic_publish_urls":   {"token_hash", "topic", "signing_secret", "previous_signing_secret", "signing_fingerprint"},
		"invites":              {"code"},
		"app_keys":             {"public_key"},
		"oauth_signing_keys":   {"private_key", "public_key"},
		"tenants":              {"external_id", "name"},
		"users":                {"session_epoch"},
		"identities":           {"subject", "email"},
		"waitlist":             {"subject"},
		"devices":              {"token_hash"},
		"app_deletions":        {"last_error"},
		"sessions":             {"label", "conversation", "handle"},
		"grants":               {"scope", "shown"},
		"agents":               {"display_name"},
		"subscriptions":        {"filters", "reason", "created_by", "event_type"},
		"organization_invites": {"token_sha256", "email", "id"},
		"agent_webhooks":       {"url", "bot_id", "removal_hash"},
		"agent_webhook_setups": {"sealed", "bot_id", "public_key", "page_hash"},
		"bot_challenges":       {"nonce_hash", "bot_id"},
		"bot_keys":             {"public_key", "bot_id"},
	}
	for table, cols := range never {
		for _, col := range cols {
			if _, err := conn.Exec(ctx, `SELECT `+col+` FROM `+table+` LIMIT 1`); !denied(err) {
				t.Errorf("%s.%s: %v, want permission denied", table, col, err)
			}
		}
	}

	// The grants, exactly: a new one needs this list changed, and a look
	// at whether it can carry content or a secret.
	rows, err := conn.Query(ctx, `SELECT table_name || '.' || column_name FROM information_schema.column_privileges
		WHERE grantee = 'wakeshift_admin_read' AND privilege_type = 'SELECT' ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	got, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	want := expectedAdminColumns()
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("columns granted:\n got %s\nwant %s", strings.Join(got, ", "), strings.Join(want, ", "))
	}
	rows, err = conn.Query(ctx, `SELECT table_name || ':' || string_agg(privilege_type, ',' ORDER BY privilege_type) FROM information_schema.table_privileges
		WHERE grantee = 'wakeshift_admin_read' GROUP BY table_name ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	tables, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	if w := []string{"admin_alert_state:INSERT,SELECT,UPDATE", "admin_app_workspaces:SELECT", "admin_log:INSERT,SELECT", "growth_milestones:INSERT,SELECT", "usage_daily:SELECT"}; !slices.Equal(tables, w) {
		t.Errorf("tables granted: %v, want %v", tables, w)
	}

	// Writes: none to core data, in any workspace.
	for _, q := range []string{
		`INSERT INTO agents (tenant_id, id) VALUES ('` + a + `', 'x')`,
		`UPDATE agents SET display_name = 'x'`,
		`DELETE FROM agents`,
		`UPDATE tenants SET active_at = now()`,
		`INSERT INTO apps (id, name) VALUES ('x', 'x')`,
		`UPDATE apps SET name = 'x'`,
		`DELETE FROM admin_alert_state`,
		`TRUNCATE usage_daily`,
		`INSERT INTO usage_daily (day, tenant_id, delivered, dead, cancelled, age_ms) VALUES (current_date, '` + a + `', 1, 0, 0, '{}')`,
		`UPDATE admin_log SET reason = 'x'`,
		`UPDATE growth_milestones SET posted = true`,
		`DELETE FROM growth_milestones`,
		`DELETE FROM admin_log`,
		`SET ROLE wakeshift_test_runtime`,
		`CREATE TABLE mine (x int)`,
	} {
		if _, err := conn.Exec(ctx, q); err == nil {
			t.Errorf("%s: succeeded", q)
		}
	}
	// Its own tables.
	if _, err := conn.Exec(ctx, `INSERT INTO admin_log (actor, action, target_kind, target_id) VALUES ('admin@example.com', 'house.paused', 'house', 'house')`); err != nil {
		t.Errorf("appending to admin_log: %v", err)
	}
	if _, err := conn.Exec(ctx, `INSERT INTO admin_alert_state (source, last_id) VALUES ('users', 1)`); err != nil {
		t.Errorf("a cursor: %v", err)
	}
	if _, err := conn.Exec(ctx, `UPDATE admin_alert_state SET last_id = 2 WHERE source = 'users'`); err != nil {
		t.Errorf("moving a cursor: %v", err)
	}
	if _, err := conn.Exec(ctx, `INSERT INTO growth_milestones (subject, subject_id, milestone, posted) VALUES ('app', 'x', 1, true)`); err != nil {
		t.Errorf("recording a milestone: %v", err)
	}
	// And it can't bypass row-level security, or become a role that can.
	var bypass bool
	if err := conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_roles WHERE (rolsuper OR rolbypassrls) AND pg_has_role(current_user, oid, 'MEMBER'))`).Scan(&bypass); err != nil || bypass {
		t.Fatalf("bypass: %v %v", bypass, err)
	}

	// The core's own role may append to admin_log but never change it.
	err = pgx.BeginFunc(ctx, d.Pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO admin_log (actor, action, target_kind, target_id, reason) VALUES ('admin@example.com', 'app.suspended', 'app', 'x', 'test')`)
		return err
	})
	if err != nil {
		t.Fatalf("the core appending to admin_log: %v", err)
	}
	for _, q := range []string{`UPDATE admin_log SET reason = 'x'`, `DELETE FROM admin_log`} {
		if _, err := d.Pool.Exec(ctx, q); err == nil {
			t.Errorf("the core: %s succeeded", q)
		}
	}
}

func expectedAdminColumns() []string {
	grants := map[string][]string{
		"apps": {"id", "name", "mcp_url", "created_at", "manifest", "manifest_updated_at", "deleted_at", "org_id", "created_by",
			"suspended_at", "suspended_by", "suspended_reason", "workspace_cap"},
		"app_keys":             {"app_id", "key_id", "name", "created_at", "created_by", "last_used_at", "revoked_at", "revoked_by"},
		"app_log":              {"id", "app_id", "at", "kind", "actor", "action", "key_id", "detail", "org_id"},
		"organizations":        {"id", "name", "personal_of", "created_by", "created_at", "deleted_at"},
		"organization_members": {"org_id", "user_id", "role", "invited_by", "created_at"},
		"org_log":              {"id", "org_id", "at", "actor", "action", "detail"},
		"app_deletions":        {"app_id", "requested_at", "attempts", "last_attempt_at", "workspaces_erased", "data_deleted_at", "log_deleted_at"},
		"tenants":              {"id", "kind", "app_id", "created_at", "active_at"},
		"users":                {"id", "email", "name", "created_at", "last_sign_in_at", "invited_by", "signup_ref"},
		"memberships":          {"tenant_id", "user_id", "role", "created_at"},
		"invites":              {"created_by", "uses_left", "created_at"},
		"topic_members":        {"app_id", "tenant_id", "subscription_id"},
		"topic_events":         {"app_id", "published_at", "matched"},
		"topic_publish_urls":   {"id", "app_id", "created_at", "last_used_at", "revoked_at"},
		"agents":               {"tenant_id", "id", "created_at"},
		"sessions":             {"tenant_id", "id", "agent_id", "harness", "connector", "created_at", "last_active_at", "connector_seen_at", "watching", "app_ids"},
		"grants":               {"tenant_id", "id", "app_id", "kind", "agent_id", "created_at", "expires_at", "revoked_at"},
		"deliveries":           {"tenant_id", "id", "agent_id", "state", "attempts", "created_at", "finished_at", "connector"},
		"mcp_event_webhooks":   {"tenant_id", "id", "app_id", "created_at", "expires_at", "delivered", "last_delivery_at", "last_status", "retry_attempts", "retrying_since", "dialect"},
		"usage_daily":          {"day", "tenant_id", "app_id", "delivered", "dead", "cancelled", "age_ms", "updated_at", "billable"},
		"admin_log":            {"id", "at", "actor", "action", "target_kind", "target_id", "reason", "detail"},
		"admin_alert_state":    {"source", "last_id", "last_at", "detail"},
		// Growth milestones (124): app and organization IDs, counts, times.
		"growth_milestones":    {"subject", "subject_id", "milestone", "reached_on", "recorded_at", "posted"},
		"admin_app_workspaces": {"id", "app_id", "created_at", "active_at", "kind"},
		// The Harnesses page (127): a subscription's IDs, agent, session,
		// state and time; never its filters, reason or creator.
		"subscriptions": {"tenant_id", "id", "agent_id", "session_id", "status", "created_at"},
		// Sign-ups (106): the cap, the waitlist (its people's emails, names
		// and notes, for the sign-ups page and Slack; never the provider's
		// subject) and which provider each account signs in with.
		"signup_cap": {"cap", "updated_at"},
		"waitlist": {"id", "provider", "email", "name", "note", "created_at", "noted_at", "invited_at", "invited_by", "emailed_at",
			"signed_up_at", "user_id", "signup_ref"},
		"identities": {"user_id", "provider", "created_at"},
	}
	var out []string
	for table, cols := range grants {
		for _, c := range cols {
			out = append(out, table+"."+c)
		}
	}
	return out
}

// The core's own role must never hold the admin service's read role: its
// admin_read policy shows every workspace's rows to its members, so the
// core, which may read every column, would see every tenant's data inside
// InTenant. Open refuses it, as it refuses a role that bypasses row-level
// security, whether WAKESHIFT_ADMIN_DB_USER names the core's own login or
// the role was granted some other way.
func TestOpenRefusesRuntimeWithAdminRead(t *testing.T) {
	ctx := context.Background()
	d, ownerURL := dbtest.NewWithURL(t)
	login := adminLogin(t)
	u, _ := url.Parse(ownerURL)
	u.User = url.UserPassword(login, "admin")

	// Two workspaces, an agent in each.
	a, b := dbtest.NewTenant(t, d), dbtest.NewTenant(t, d)
	for _, tenant := range []string{a, b} {
		if err := d.InTenant(ctx, tenant, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO agents (tenant_id, id) VALUES ($1, 'claude-code')`, tenant)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}

	// The admin service's login configured as the core's own.
	d2, err := db.OpenWithOwner(ctx, u.String(), ownerURL, db.AdminReadLogin(login))
	if err == nil {
		defer d2.Close()
		var n int
		if err := d2.InTenant(ctx, a, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM agents`).Scan(&n)
		}); err != nil {
			t.Fatal(err)
		}
		t.Fatalf("the core's role was granted the admin read role and started; one workspace sees %d workspaces' agents", n)
	}
	if !errors.Is(err, db.ErrAdminReadRuntime) {
		t.Fatalf("got %v, want ErrAdminReadRuntime", err)
	}

	// Granted some other way: refused at the next start.
	admin, err := pgx.Connect(ctx, os.Getenv("WAKESHIFT_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	if _, err := admin.Exec(ctx, `GRANT `+db.AdminReadRole+` TO `+login); err != nil {
		t.Fatal(err)
	}
	if d3, err := db.OpenWithOwner(ctx, u.String(), ownerURL); !errors.Is(err, db.ErrAdminReadRuntime) {
		if err == nil {
			d3.Close()
		}
		t.Fatalf("a runtime role holding the admin read role: %v, want ErrAdminReadRuntime", err)
	}
}
