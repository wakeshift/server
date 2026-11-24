package db_test

import (
	"context"
	"testing"

	"github.com/wakeshift/server/internal/db"
)

// 171 records each existing key's maker's role, as their role in the app's
// organization (a key from before created_by was its app's creator's), only
// where it's known to be the role they made the key with: a member since
// before the key (or in their personal organization) whose role hasn't
// changed since. A key whose maker left, was promoted since or joined
// since, and a WAKESHIFT_APPS key, get none. Keys registered from the CLI's
// link are marked, by their log entry.
func TestAppKeyOriginMigration(t *testing.T) {
	ctx := context.Background()
	d := emptyDB(t)
	if err := d.MigrateWith(ctx, migrationsUpTo(t, "169_session_install_choice.sql"), db.MigrateOptions{Replaced: db.Replaced}); err != nil {
		t.Fatal(err)
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := d.Pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	const ada, cy, gone, dee, eve, pat = "00000000-0000-0000-0000-00000000000a", "00000000-0000-0000-0000-00000000000c", "00000000-0000-0000-0000-00000000000f",
		"00000000-0000-0000-0000-00000000000d", "00000000-0000-0000-0000-00000000000e", "00000000-0000-0000-0000-000000000001"
	exec(`INSERT INTO users (id, email) VALUES ($1, 'ada@example.com'), ($2, 'cy@example.com'), ($3, 'gone@example.com'), ($4, 'dee@example.com'),
		($5, 'eve@example.com'), ($6, 'pat@example.com')`, ada, cy, gone, dee, eve, pat)
	const org, pats = "00000000-0000-0000-0000-0000000000a1", "00000000-0000-0000-0000-0000000000b1"
	exec(`INSERT INTO organizations (id, name) VALUES ($1, 'Org')`, org)
	exec(`INSERT INTO organizations (id, name, personal_of) VALUES ($1, 'Pat', $2)`, pats, pat)
	// Members since an hour ago; eve joined after her key (below); pat's
	// personal organization's membership is newer than her old key.
	exec(`INSERT INTO organization_members (org_id, user_id, role, created_at) VALUES ($1, $2, 'owner', now() - interval '1 hour'),
		($1, $3, 'developer', now() - interval '1 hour'), ($1, $4, 'admin', now() - interval '1 hour'), ($1, $5, 'admin', now()), ($6, $7, 'owner', now())`,
		org, ada, cy, dee, eve, pats, pat)
	exec(`INSERT INTO apps (id, name, org_id, created_by) VALUES ('quiz', 'Quiz', $1, $2), ('patsapp', 'Pat''s', $3, $4)`, org, ada, pats, pat)
	exec(`INSERT INTO apps (id, name) VALUES ('ours', 'Ours')`)
	for _, k := range []struct {
		app, id, by string
		ago         string
	}{
		{"quiz", "ada-key", "user:" + ada, "1 minute"},
		{"quiz", "cy-key", "user:" + cy, "1 minute"},
		{"quiz", "old-key", "", "1 minute"},
		{"quiz", "gone-key", "user:" + gone, "1 minute"},
		{"quiz", "dee-key", "user:" + dee, "30 minutes"},
		{"quiz", "eve-key", "user:" + eve, "30 minutes"},
		{"patsapp", "pat-key", "user:" + pat, "1 year"},
		{"ours", "ours-key", "", "1 minute"},
	} {
		exec(`INSERT INTO app_keys (app_id, key_id, kid, name, public_key, created_by, created_at)
			VALUES ($1::text, $2::text, $2::text, 'key', sha256(convert_to($2::text, 'UTF8')), nullif($3, ''), now() - $4::interval)`,
			k.app, k.id, k.by, k.ago)
	}
	// Dee was a developer when she made her key, and was made an admin since.
	exec(`INSERT INTO org_log (org_id, actor, action, detail, at) VALUES ($1, $2, 'org.role_changed', jsonb_build_object('user_id', $3::text, 'old_role', 'developer', 'role', 'admin'), now() - interval '10 minutes')`,
		org, "user:"+ada, dee)
	exec(`INSERT INTO app_log (app_id, actor, action, key_id, detail) VALUES ('quiz', $1, 'app.key_created', 'cy-key', '{"via": "cli"}'), ('quiz', $2, 'app.key_created', 'ada-key', '{}')`,
		"user:"+cy, "user:"+ada)

	if err := d.MigrateWith(ctx, db.Migrations, db.MigrateOptions{Replaced: db.Replaced}); err != nil {
		t.Fatal(err)
	}
	want := map[string][2]string{
		"ada-key":  {"owner", ""},
		"cy-key":   {"developer", "cli_link"},
		"old-key":  {"owner", ""},
		"gone-key": {"", ""},
		"dee-key":  {"", ""},
		"eve-key":  {"", ""},
		"pat-key":  {"owner", ""},
		"ours-key": {"", ""},
	}
	rows, err := d.Pool.Query(ctx, `SELECT key_id, coalesce(created_role, ''), coalesce(via, '') FROM app_keys`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var id, role, via string
		if err := rows.Scan(&id, &role, &via); err != nil {
			t.Fatal(err)
		}
		n++
		if w := want[id]; role != w[0] || via != w[1] {
			t.Errorf("%s: role %q via %q, want %q %q", id, role, via, w[0], w[1])
		}
	}
	if n != len(want) {
		t.Errorf("%d keys, want %d", n, len(want))
	}
}
