package db_test

import (
	"context"
	"fmt"
	"io/fs"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/db"
	"github.com/wakeshift/server/internal/dbtest"
	"github.com/wakeshift/server/internal/events"
)

// 165-167 give deliveries their subscription's grant: 166 fills it in for
// the deliveries grant pacing can still count (the last day, or scheduled
// ahead), workspace by workspace, and leaves older ones and a user's own
// subscription's alone; from 165 on, the database sets it on every insert.
func TestDeliveriesGrantMigration(t *testing.T) {
	ctx := context.Background()
	d := emptyDB(t)
	if err := d.MigrateWith(ctx, migrationsUpTo(t, "165"), db.MigrateOptions{Replaced: db.Replaced}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Pool.Exec(ctx, `INSERT INTO apps (id, name) VALUES ('pacer', 'Pacer')`); err != nil {
		t.Fatal(err)
	}
	var grants []string
	var tenants []string
	for range 2 {
		tenant := dbtest.NewTenant(t, d)
		tenants = append(tenants, tenant)
		err := d.InTenant(ctx, tenant, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `INSERT INTO agents (tenant_id, id) VALUES (current_tenant(), 'codex')`); err != nil {
				return err
			}
			grant := dbtest.Grant(t, tx, "pacer", "codex", time.Hour)
			grants = append(grants, grant)
			if _, _, err := events.Append(ctx, tx, events.Event{Type: "test.ping", Source: "test", From: events.Party{Kind: "hub", ID: "hub"}, Trust: events.Internal}); err != nil {
				return err
			}
			// Under the grant: recent, scheduled ahead, two days old; and
			// a user's own.
			for _, s := range []struct {
				grant   *string
				created string
				at      string
				target  string
			}{
				{&grant, "now() - interval '2 hours'", "now() - interval '2 hours'", "recent"},
				{&grant, "now()", "now() + interval '30 minutes'", "ahead"},
				{&grant, "now() - interval '2 days'", "now() - interval '2 days'", "old"},
				{nil, "now()", "now()", "own"},
			} {
				var sub string
				if err := tx.QueryRow(ctx, `INSERT INTO subscriptions (tenant_id, agent_id, event_type, created_by, grant_id, start_seq)
					VALUES (current_tenant(), 'codex', 'test.*', 'test', $1, 0) RETURNING id::text`, s.grant).Scan(&sub); err != nil {
					return err
				}
				if _, err := tx.Exec(ctx, fmt.Sprintf(`INSERT INTO deliveries (tenant_id, id, agent_id, target, event_id, event_seq, subscription_id, created_at, available_at)
					SELECT current_tenant(), gen_random_uuid(), 'codex', $1, id, seq, $2, %s, %s FROM events`, s.created, s.at), s.target, sub); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := d.MigrateWith(ctx, db.Migrations, db.MigrateOptions{Replaced: db.Replaced}); err != nil {
		t.Fatal(err)
	}
	for i, tenant := range tenants {
		err := d.InTenant(ctx, tenant, func(tx pgx.Tx) error {
			rows, _ := tx.Query(ctx, `SELECT target || '=' || coalesce(grant_id::text, '') FROM deliveries ORDER BY target`)
			got, err := pgx.CollectRows(rows, pgx.RowTo[string])
			if err != nil {
				return err
			}
			g := grants[i]
			if want := []string{"ahead=" + g, "old=", "own=", "recent=" + g}; fmt.Sprint(got) != fmt.Sprint(want) {
				t.Errorf("workspace %d after 166: %v, want %v", i, got, want)
			}
			// A delivery inserted now carries its grant (the trigger).
			var grant *string
			err = tx.QueryRow(ctx, `INSERT INTO deliveries (tenant_id, id, agent_id, target, event_id, event_seq, subscription_id)
				SELECT current_tenant(), gen_random_uuid(), 'codex', 'new', e.id, e.seq, s.id FROM events e, subscriptions s WHERE s.grant_id IS NOT NULL LIMIT 1
				RETURNING grant_id::text`).Scan(&grant)
			if err != nil {
				return err
			}
			if grant == nil || *grant != g {
				t.Errorf("workspace %d: a new delivery's grant %v, want %s", i, grant, g)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

// Production applies 165, 166 and 167's index by hand ahead of the
// release, because deliveries is too large to backfill and index at
// startup. The release's migrations then change nothing: the column, trigger and index are the ones built by hand (the
// same objects, not rebuilt), and all three files are recorded.
func TestGrantPacingAppliedByHandFirst(t *testing.T) {
	ctx := context.Background()
	d := emptyDB(t)
	if err := d.MigrateWith(ctx, migrationsUpTo(t, "165"), db.MigrateOptions{Replaced: db.Replaced}); err != nil {
		t.Fatal(err)
	}
	read := func(name string) string {
		b, err := fs.ReadFile(db.Migrations, name)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	// As the runbook runs them: 165 in one transaction, 166 on its own
	// (it commits per workspace), then the index.
	tx, err := d.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, read("165_grant_pacing.sql")); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Pool.Exec(ctx, read("166_deliveries_grant_backfill.sql")); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Pool.Exec(ctx, `CREATE INDEX CONCURRENTLY IF NOT EXISTS deliveries_grant_paced
		ON deliveries (tenant_id, grant_id, available_at) WHERE grant_id IS NOT NULL`); err != nil {
		t.Fatal(err)
	}
	objects := func() string {
		var index, trigger, table uint32
		if err := d.Pool.QueryRow(ctx, `SELECT 'deliveries_grant_paced'::regclass::oid,
			(SELECT oid FROM pg_trigger WHERE tgrelid = 'deliveries'::regclass AND tgname = 'deliveries_grant'),
			'grant_paces'::regclass::oid`).Scan(&index, &trigger, &table); err != nil {
			t.Fatal(err)
		}
		return fmt.Sprint(index, trigger, table)
	}
	before := objects()
	if err := d.MigrateWith(ctx, db.Migrations, db.MigrateOptions{Replaced: db.Replaced}); err != nil {
		t.Fatal(err)
	}
	if after := objects(); after != before {
		t.Errorf("the migrations rebuilt what was applied by hand: %s, then %s", before, after)
	}
	var recorded int
	if err := d.Pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations WHERE name IN
		('165_grant_pacing.sql', '166_deliveries_grant_backfill.sql', '167_deliveries_grant_paced_index.sql')`).Scan(&recorded); err != nil {
		t.Fatal(err)
	}
	if recorded != 3 {
		t.Errorf("%d of 165-167 recorded, want 3", recorded)
	}
}
