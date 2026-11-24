package db_test

import (
	"context"
	"os"
	"testing"

	"github.com/wakeshift/server/internal/dbtest"
)

// The organizations' backfill (072) gives every user exactly one personal
// organization, with them as its owner; each console app its creator's;
// our own apps none; and log entries their app's organization. Running it
// again changes nothing. The rows here are what the revision before 072
// wrote (users without an organization, apps with only owner_id).
func TestOrganizationsBackfill(t *testing.T) {
	d := dbtest.NewOwner(t)
	ctx := context.Background()
	backfill, err := os.ReadFile("migrations/072_organizations_backfill.sql")
	if err != nil {
		t.Fatal(err)
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := d.Pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	// 072 ran when apps still had owner_id, which 117 dropped: put it back
	// for the backfill to read.
	exec(`ALTER TABLE apps ADD COLUMN owner_id uuid REFERENCES users(id)`)
	var ada, bob, cy string
	for _, u := range []struct {
		id          *string
		email, name string
	}{{&ada, "ada@example.com", "Ada"}, {&bob, "bob@example.com", ""}, {&cy, "cy@example.com", "  "}} {
		if err := d.Pool.QueryRow(ctx, `INSERT INTO users (email, name) VALUES ($1, $2) RETURNING id::text`, u.email, u.name).Scan(u.id); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO apps (id, name, owner_id) VALUES ('ada-one', 'One', $1), ('ada-two', 'Two', $1), ('bob-app', 'Bob', $2)`, ada, bob)
	exec(`INSERT INTO apps (id, name) VALUES ('ours', 'Ours')`)
	exec(`INSERT INTO app_log (app_id, actor, action) VALUES ('ada-one', 'user:' || $1, 'app.created'), ('bob-app', 'user:' || $2, 'app.created'), ('ours', 'hub', 'app.manifest_published')`, ada, bob)

	type state struct {
		orgs, members, logs int
		personal            map[string]string // user → their organization
		names               map[string]string // user → its name
	}
	read := func() state {
		t.Helper()
		s := state{personal: map[string]string{}, names: map[string]string{}}
		if err := d.Pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM organizations), (SELECT count(*) FROM organization_members), (SELECT count(*) FROM org_log)`).
			Scan(&s.orgs, &s.members, &s.logs); err != nil {
			t.Fatal(err)
		}
		rows, err := d.Pool.Query(ctx, `SELECT o.personal_of::text, o.id::text, o.name FROM organizations o
			JOIN organization_members m ON m.org_id = o.id AND m.user_id = o.personal_of AND m.role = 'owner'`)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var user, org, name string
			if err := rows.Scan(&user, &org, &name); err != nil {
				t.Fatal(err)
			}
			s.personal[user], s.names[user] = org, name
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return s
	}
	check := func() state {
		t.Helper()
		s := read()
		if s.orgs != 3 || s.members != 3 || s.logs != 3 || len(s.personal) != 3 {
			t.Fatalf("after the backfill: %d organizations, %d members, %d log entries, %d personal: %+v", s.orgs, s.members, s.logs, len(s.personal), s)
		}
		if s.names[ada] != "Ada" || s.names[bob] != "bob@example.com" || s.names[cy] != "cy@example.com" {
			t.Errorf("names: %v", s.names)
		}
		for app, want := range map[string]*string{"ada-one": &ada, "ada-two": &ada, "bob-app": &bob, "ours": nil} {
			var org, createdBy *string
			if err := d.Pool.QueryRow(ctx, `SELECT org_id::text, created_by::text FROM apps WHERE id = $1`, app).Scan(&org, &createdBy); err != nil {
				t.Fatal(err)
			}
			switch {
			case want == nil && (org != nil || createdBy != nil):
				t.Errorf("our own app %s got organization %v, created_by %v", app, org, createdBy)
			case want != nil && (org == nil || *org != s.personal[*want] || createdBy == nil || *createdBy != *want):
				t.Errorf("app %s: organization %v, created_by %v; want %s's", app, org, createdBy, *want)
			}
		}
		rows, err := d.Pool.Query(ctx, `SELECT l.app_id, l.org_id::text FROM app_log l`)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var app string
			var org *string
			if err := rows.Scan(&app, &org); err != nil {
				t.Fatal(err)
			}
			switch {
			case app == "ours" && org != nil:
				t.Errorf("our app's log entry got organization %s", *org)
			case app == "ada-one" && (org == nil || *org != s.personal[ada]), app == "bob-app" && (org == nil || *org != s.personal[bob]):
				t.Errorf("%s's log entry: organization %v", app, org)
			}
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return s
	}

	// The migrations already ran the backfill on an empty database; these
	// rows are new to it.
	exec(string(backfill))
	first := check()
	exec(string(backfill))
	if second := check(); second.personal[ada] != first.personal[ada] || second.personal[bob] != first.personal[bob] {
		t.Errorf("a second run changed the organizations: %v, then %v", first.personal, second.personal)
	}
}
