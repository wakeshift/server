package coretest_test

import (
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/appapi"
	"github.com/wakeshift/server/internal/coretest"
	"github.com/wakeshift/server/internal/dbtest"
)

// A publish copies its event into each subscribed workspace in a
// transaction of its own: a failure in one workspace doesn't undo the
// others, and a retry with the same ID (or the expiry pass, for a publish
// without one) makes the missing copy, once.
func TestPublishFailingInOneWorkspace(t *testing.T) {
	c := coretest.Start(t)
	app := noteApp(t, c, "fan")
	var tenants []string
	for _, user := range []string{"ann", "bob"} {
		sub, err := subscribe(app, caller{User: user, Client: "goose-cli"}, "news", subOpts{})
		if err != nil {
			t.Fatal(err)
		}
		tenant, _ := topicSubRow(t, c, sub.ID)
		tenants = append(tenants, tenant)
	}
	ann, bob := tenants[0], tenants[1]
	owner := dbtest.Owner(t, c.DB)
	breakBob := func(on bool) {
		t.Helper()
		sql := `DROP TRIGGER IF EXISTS fail_bob ON events`
		if on {
			sql = `CREATE OR REPLACE FUNCTION fail_bob() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'bob is down'; END $$;
				CREATE TRIGGER fail_bob BEFORE INSERT ON events FOR EACH ROW WHEN (NEW.tenant_id = '` + bob + `') EXECUTE FUNCTION fail_bob()`
		}
		if _, err := owner.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	copies := func(tenant, source string) int {
		var event string
		if err := c.DB.Pool.QueryRow(ctx, `SELECT event_id::text FROM topic_events WHERE source_id = $1`, source).Scan(&event); err != nil {
			t.Fatal(err)
		}
		return count(t, c, tenant, `SELECT count(*) FROM events WHERE id = $1::uuid`, event)
	}

	breakBob(true)
	if _, err := emitNote(app, []string{"news"}, note{ID: "p1", Text: "first"}); err == nil {
		t.Fatal("a publish failing in one workspace reported success")
	}
	if copies(ann, "p1") != 1 || copies(bob, "p1") != 0 {
		t.Fatalf("after the failure: ann %d, bob %d", copies(ann, "p1"), copies(bob, "p1"))
	}
	breakBob(false)
	p, err := emitNote(app, []string{"news"}, note{ID: "p1", Text: "first"})
	if err != nil || !p.Duplicate || p.Matched != 2 {
		t.Fatalf("the retry: %+v %v", p, err)
	}
	if copies(ann, "p1") != 1 || copies(bob, "p1") != 1 {
		t.Fatalf("after the retry: ann %d, bob %d", copies(ann, "p1"), copies(bob, "p1"))
	}

	// Without an ID, the app can't retry it: the expiry pass finishes it.
	breakBob(true)
	if _, err := emitNote(app, []string{"news"}, note{Text: "second"}); err == nil {
		t.Fatal("a publish failing in one workspace reported success")
	}
	breakBob(false)
	var source string
	if err := c.DB.Pool.QueryRow(ctx, `UPDATE topic_fanout SET created_at = now() - interval '2 minutes' RETURNING source_id`).Scan(&source); err != nil {
		t.Fatal(err)
	}
	if n, err := appapi.FinishFanouts(ctx, c.DB); err != nil || n != 1 {
		t.Fatalf("finished %d: %v", n, err)
	}
	if copies(ann, source) != 1 || copies(bob, source) != 1 {
		t.Fatalf("after the expiry pass: ann %d, bob %d", copies(ann, source), copies(bob, source))
	}
	var matched int
	c.DB.Pool.QueryRow(ctx, `SELECT matched FROM topic_events WHERE source_id = $1`, source).Scan(&matched)
	if matched != 2 {
		t.Fatalf("matched %d", matched)
	}
}

// An interrupted publish that keeps failing in one workspace doesn't stop
// the expiry pass finishing the others: it reports the failure and goes
// on. Before, it returned at the first failure, so one stuck publish held
// up the same others on every pass until they expired.
func TestAStuckPublishDoesntHoldUpTheRest(t *testing.T) {
	c := coretest.Start(t)
	app := noteApp(t, c, "fan")
	tenant := map[string]string{}
	for _, user := range []string{"ann", "bob"} {
		sub, err := subscribe(app, caller{User: user, Client: "goose-cli"}, user, subOpts{})
		if err != nil {
			t.Fatal(err)
		}
		tenant[user], _ = topicSubRow(t, c, sub.ID)
	}
	owner := dbtest.Owner(t, c.DB)
	if _, err := owner.Exec(ctx, `CREATE FUNCTION fail_copy() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'down'; END $$`); err != nil {
		t.Fatal(err)
	}
	breakTenant := func(user string, on bool) {
		t.Helper()
		sql := `DROP TRIGGER IF EXISTS fail_` + user + ` ON events`
		if on {
			sql = `CREATE TRIGGER fail_` + user + ` BEFORE INSERT ON events FOR EACH ROW WHEN (NEW.tenant_id = '` + tenant[user] + `') EXECUTE FUNCTION fail_copy()`
		}
		if _, err := owner.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	// Bob's publish stays stuck; ann's three were interrupted, then can go.
	breakTenant("bob", true)
	breakTenant("ann", true)
	ids := []string{"a-stuck", "b1", "b2", "b3"}
	for _, id := range ids {
		topic := "ann"
		if id == "a-stuck" {
			topic = "bob"
		}
		if _, err := emitNote(app, []string{topic}, note{ID: id, Text: id}); err == nil {
			t.Fatalf("%s: a publish failing in its workspace reported success", id)
		}
	}
	breakTenant("ann", false)
	if _, err := c.DB.Pool.Exec(ctx, `UPDATE topic_fanout SET created_at = now() - interval '2 minutes'`); err != nil {
		t.Fatal(err)
	}

	n, err := appapi.FinishFanouts(ctx, c.DB)
	if err == nil || n != 3 {
		t.Fatalf("finished %d, %v (want 3, and the failure)", n, err)
	}
	var left []string
	rows, _ := c.DB.Pool.Query(ctx, `SELECT source_id FROM topic_fanout ORDER BY source_id`)
	if left, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil || len(left) != 1 || left[0] != "a-stuck" {
		t.Fatalf("still to finish: %v, %v", left, err)
	}
}
