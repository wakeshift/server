package console_test

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/agents"
	"github.com/wakeshift/server/internal/appapi"
	"github.com/wakeshift/server/internal/coretest"
	"github.com/wakeshift/server/internal/dbtest"
	"github.com/wakeshift/server/internal/deliveries"
	"github.com/wakeshift/server/internal/events"
	"github.com/wakeshift/server/pkg/wakeshift"
)

// appUsers gives app headless users with what they leave behind: a user
// with an agent and an agent-wide subscription, a guest subscribed to a
// topic with a publish delivered. It returns the users' workspaces.
func appUsers(t *testing.T, c *coretest.Core, app *wakeshift.Client) []string {
	t.Helper()
	ctx := context.Background()
	id, err := app.AppID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	coretest.DefineNotes(t, app)
	if err := app.PublishManifest(ctx, wakeshift.Manifest{Agent: &wakeshift.AgentScope{
		Wake: []wakeshift.ScopeEvent{{Pattern: "app." + id + ".game.*", Description: "Games"}},
		Post: []wakeshift.ScopeEvent{{Pattern: "app." + id + ".game.*", Description: "Games"}}, TTLDays: 30}}); err != nil {
		t.Fatal(err)
	}
	ws, err := app.Workspace(ctx, "player-1", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.AddAgent(ctx, ws, "claude-code", ""); err != nil {
		t.Fatal(err)
	}
	if err := c.DB.InTenant(ctx, ws, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO subscriptions (tenant_id, agent_id, event_type, start_seq, created_by)
			VALUES (current_tenant(), 'claude-code', $1, 0, $2)`, "app."+id+".game.g1", "app:"+id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := coretest.Subscribe(app, coretest.Caller{User: "guest:abc", Client: "claude-code"}, "rooms/r1", coretest.SubOpts{}); err != nil {
		t.Fatal(err)
	}
	if p, err := coretest.EmitNote(app, []string{"rooms/r1"}, coretest.Note{Text: "your turn"}); err != nil || p.Matched != 1 {
		t.Fatalf("publish: %+v %v", p, err)
	}
	var guest string
	if err := c.DB.Pool.QueryRow(ctx, `SELECT id::text FROM tenants WHERE app_id = $1 AND external_id = 'guest:abc'`, id).Scan(&guest); err != nil {
		t.Fatal(err)
	}
	return []string{ws, guest}
}

// accountData gives app what an account-mode app has in someone's own
// workspace: a grant from them for their agent, a subscription under it,
// and an event from the app delivered on it.
func accountData(t *testing.T, c *coretest.Core, tenant, user, app string) {
	t.Helper()
	ctx := context.Background()
	err := c.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		var exists bool
		tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM agents WHERE id = 'codex')`).Scan(&exists)
		if !exists {
			if _, err := agents.Create(ctx, tx, "codex", ""); err != nil {
				return err
			}
		}
		var grant, sub string
		if err := tx.QueryRow(ctx, `INSERT INTO grants (tenant_id, app_id, kind, agent_id, scope, granted_by, expires_at)
			VALUES (current_tenant(), $1, 'agent', 'codex', '{}', $2, now() + interval '30 days') RETURNING id::text`, app, user).Scan(&grant); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `INSERT INTO subscriptions (tenant_id, agent_id, event_type, start_seq, created_by, grant_id)
			VALUES (current_tenant(), 'codex', $1, 0, $2, $3) RETURNING id::text`, "app."+app+".game.g1", "app:"+app, grant).Scan(&sub); err != nil {
			return err
		}
		e, _, err := events.Append(ctx, tx, events.Event{Type: "app." + app + ".game.g1", Source: "app:" + app,
			From: events.Party{Kind: "app", ID: app}, Subject: "your move", Trust: events.External})
		if err != nil {
			return err
		}
		_, err = deliveries.Insert(ctx, tx, deliveries.New{AgentID: "codex", SubscriptionID: sub, Event: e})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

// inTenant counts query's rows in a workspace.
func inTenant(t *testing.T, c *coretest.Core, tenant, query string, args ...any) int {
	t.Helper()
	var n int
	if err := c.DB.InTenant(context.Background(), tenant, func(tx pgx.Tx) error { return tx.QueryRow(context.Background(), query, args...).Scan(&n) }); err != nil {
		t.Fatal(err)
	}
	return n
}

// Deleting an app deletes what Wakeshift keeps for its users once the
// expiry pass has run: the workspaces it kept (a guest's included) and
// everything in them, and, in someone's own workspace, its grants,
// subscriptions, events and deliveries; its publishes too.
// Another app's users and data are untouched; the app's log stays until
// its retention ends.
func TestDeletingAnAppDeletesItsUsersData(t *testing.T) {
	c := coretest.Start(t)
	ctx := context.Background()
	ada := c.SignIn("ada@example.com")
	var adaID string
	if err := c.DB.Pool.QueryRow(ctx, `SELECT id::text FROM users WHERE email = 'ada@example.com'`).Scan(&adaID); err != nil {
		t.Fatal(err)
	}
	quizzer := kit(t, c, createApp(t, ada, "quizzer", "Quizzer", "").Key, "")
	keeper := kit(t, c, createApp(t, ada, "keeper", "Keeper", "").Key, "")
	gone := appUsers(t, c, quizzer)
	kept := appUsers(t, c, keeper)
	own := ada.Workspace(t)
	accountData(t, c, own, adaID, "quizzer")
	accountData(t, c, own, adaID, "keeper")
	// Daily usage counts for both apps' users, and one for a workspace of
	// Quizzer's that's gone already (erased before erasing deleted them).
	for _, ws := range append(append([]string(nil), gone...), kept...) {
		if _, err := c.DB.Pool.Exec(ctx, `INSERT INTO usage_daily (day, tenant_id, app_id, delivered, dead, cancelled, age_ms)
			SELECT current_date - 1, id, app_id, 1, 0, 0, '{}' FROM tenants WHERE id = $1`, ws); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.DB.Pool.Exec(ctx, `INSERT INTO usage_daily (day, tenant_id, app_id, delivered, dead, cancelled, age_ms)
		VALUES (current_date - 300, gen_random_uuid(), 'quizzer', 1, 0, 0, '{}')`); err != nil {
		t.Fatal(err)
	}

	if code := ada.Do("DELETE", "/api/v1/developer/apps/quizzer", nil, nil); code != 200 {
		t.Fatalf("delete: %d", code)
	}
	var pending bool
	c.DB.Pool.QueryRow(ctx, `SELECT data_deleted_at IS NULL FROM app_deletions WHERE app_id = 'quizzer'`).Scan(&pending)
	if !pending {
		t.Fatal("the deletion isn't recorded as pending")
	}
	pass, err := appapi.FinishAppDeletions(ctx, c.DB, 100, 1000)
	if err != nil || pass.Finished != 1 || pass.Logs != 0 {
		t.Fatalf("pass: %+v %v", pass, err)
	}

	// The app's workspaces: no table keeps a row for them.
	rows, _ := c.DB.Pool.Query(ctx, `SELECT table_name FROM information_schema.columns WHERE table_schema = 'public' AND column_name = 'tenant_id'`)
	tables, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil || len(tables) < 15 {
		t.Fatalf("tables %v %v", tables, err)
	}
	for _, ws := range gone {
		for _, table := range tables {
			if n := inTenant(t, c, ws, `SELECT count(*) FROM `+table+` WHERE tenant_id = $1`, ws); n != 0 {
				t.Errorf("%s keeps %d rows of a deleted app's workspace", table, n)
			}
		}
	}
	var n int
	c.DB.Pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM tenants WHERE app_id = 'quizzer')
		+ (SELECT count(*) FROM topic_events WHERE app_id = 'quizzer') + (SELECT count(*) FROM topic_members WHERE app_id = 'quizzer')`).Scan(&n)
	if n != 0 {
		t.Errorf("%d platform rows of the deleted app kept", n)
	}
	// Its usage counts too; Keeper's stay.
	c.DB.Pool.QueryRow(ctx, `SELECT count(*) FROM usage_daily WHERE app_id = 'quizzer'`).Scan(&n)
	if n != 0 {
		t.Errorf("%d usage rows of the deleted app kept", n)
	}
	c.DB.Pool.QueryRow(ctx, `SELECT count(*) FROM usage_daily WHERE app_id = 'keeper'`).Scan(&n)
	if n != len(kept) {
		t.Errorf("keeper's usage rows: %d, want %d", n, len(kept))
	}
	// Ada's own workspace: the app's grant, subscription, event and
	// delivery are gone, and its audit log says so; Keeper's are there.
	for _, app := range []string{"quizzer", "keeper"} {
		want := 0
		if app == "keeper" {
			want = 1
		}
		for what, q := range map[string]string{
			"grants":        `SELECT count(*) FROM grants WHERE app_id = $1`,
			"subscriptions": `SELECT count(*) FROM subscriptions WHERE created_by = 'app:' || $1`,
			"events":        `SELECT count(*) FROM events WHERE source = 'app:' || $1`,
			"deliveries":    `SELECT count(*) FROM deliveries d JOIN events e ON e.tenant_id = d.tenant_id AND e.id = d.event_id WHERE e.source = 'app:' || $1`,
		} {
			if got := inTenant(t, c, own, q, app); got != want {
				t.Errorf("%s: %s in ada's workspace: %d, want %d", app, what, got, want)
			}
		}
	}
	if got := inTenant(t, c, own, `SELECT count(*) FROM audit_log WHERE action = 'app.data_deleted' AND app_id = 'quizzer' AND actor = 'hub'`); got != 1 {
		t.Errorf("ada's audit log has %d app.data_deleted entries", got)
	}
	if got := inTenant(t, c, own, `SELECT count(*) FROM agents WHERE id = 'codex'`); got != 1 {
		t.Error("ada's own agent went with the app")
	}
	// Keeper's users are as they were.
	for _, ws := range kept {
		if got := inTenant(t, c, ws, `SELECT count(*) FROM subscriptions WHERE status = 'active'`); got == 0 {
			t.Errorf("keeper's workspace %s lost its subscriptions", ws)
		}
	}
	c.DB.Pool.QueryRow(ctx, `SELECT count(*) FROM topic_events WHERE app_id = 'keeper'`).Scan(&n)
	if n != 1 {
		t.Errorf("keeper's platform rows: %d, want 1", n)
	}
	if p, err := coretest.EmitNote(keeper, []string{"rooms/r1"}, coretest.Note{Text: "still here"}); err != nil || p.Matched != 1 {
		t.Errorf("keeper publishing: %+v %v", p, err)
	}

	// The app's log stays for its owner, saying so, until its retention
	// ends; then it goes, and Keeper's stays.
	rows, _ = c.DB.Pool.Query(ctx, `SELECT action FROM app_log WHERE app_id = 'quizzer' ORDER BY id`)
	logs, _ := pgx.CollectRows(rows, pgx.RowTo[string])
	if !strings.Contains(strings.Join(logs, " "), "app.deleted app.grants_revoked app.users_data_deleted") {
		t.Errorf("quizzer's log: %v", logs)
	}
	if pass, err := appapi.FinishAppDeletions(ctx, c.DB, 100, 1000); err != nil || pass != (appapi.AppDeletionPass{}) {
		t.Fatalf("a pass with nothing to do: %+v %v", pass, err)
	}
	c.DB.Pool.Exec(ctx, `UPDATE app_deletions SET requested_at = now() - interval '30 days 1 minute' WHERE app_id = 'quizzer'`)
	if pass, err := appapi.FinishAppDeletions(ctx, c.DB, 100, 1000); err != nil || pass.Logs != 1 {
		t.Fatalf("deleting the log: %+v %v", pass, err)
	}
	c.DB.Pool.QueryRow(ctx, `SELECT count(*) FROM app_log WHERE app_id = 'quizzer'`).Scan(&n)
	if n != 0 {
		t.Errorf("%d entries of the deleted app's log kept", n)
	}
	c.DB.Pool.QueryRow(ctx, `SELECT count(*) FROM app_log WHERE app_id = 'keeper'`).Scan(&n)
	if n == 0 {
		t.Error("keeper's log went too")
	}
	// The ID still isn't reused.
	if code := ada.Do("POST", "/api/v1/developer/apps", map[string]string{"id": "quizzer", "name": "Again"}, nil); code != 409 {
		t.Errorf("reusing a deleted app's ID: %d", code)
	}
}

// A pass that fails part-way records the failure and leaves the deletion
// pending; the next pass finishes it. A limit on the app's own workspaces
// spreads a big app over passes.
func TestAppDeletionRetries(t *testing.T) {
	c := coretest.Start(t)
	ctx := context.Background()
	ada := c.SignIn("ada@example.com")
	var adaID string
	c.DB.Pool.QueryRow(ctx, `SELECT id::text FROM users WHERE email = 'ada@example.com'`).Scan(&adaID)
	quizzer := kit(t, c, createApp(t, ada, "quizzer", "Quizzer", "").Key, "")
	appUsers(t, c, quizzer)
	own := ada.Workspace(t)
	accountData(t, c, own, adaID, "quizzer")
	if code := ada.Do("DELETE", "/api/v1/developer/apps/quizzer", nil, nil); code != 200 {
		t.Fatalf("delete: %d", code)
	}

	// One workspace a pass.
	pass, err := appapi.FinishAppDeletions(ctx, c.DB, 1, 1000)
	if err != nil || pass.Finished != 0 || pass.Workspaces != 1 {
		t.Fatalf("a limited pass: %+v %v", pass, err)
	}
	// Then deleting in ada's workspace fails.
	owner := dbtest.Owner(t, c.DB)
	if _, err := owner.Exec(ctx, `CREATE FUNCTION refuse() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'refused for the test'; END $$;
		CREATE TRIGGER refuse BEFORE DELETE ON grants FOR EACH ROW EXECUTE FUNCTION refuse()`); err != nil {
		t.Fatal(err)
	}
	pass, err = appapi.FinishAppDeletions(ctx, c.DB, 100, 1000)
	if err == nil || !strings.Contains(err.Error(), "refused for the test") || pass.Finished != 0 {
		t.Fatalf("a failing pass: %+v %v", pass, err)
	}
	var attempts int
	var lastError string
	c.DB.Pool.QueryRow(ctx, `SELECT attempts, coalesce(last_error, '') FROM app_deletions WHERE app_id = 'quizzer' AND data_deleted_at IS NULL`).Scan(&attempts, &lastError)
	if attempts != 1 || !strings.Contains(lastError, "refused for the test") {
		t.Fatalf("recorded: %d %q", attempts, lastError)
	}
	// What failed rolled back: the grant is still there, revoked or not.
	if got := inTenant(t, c, own, `SELECT count(*) FROM grants WHERE app_id = 'quizzer'`); got != 1 {
		t.Fatalf("grants after the failure: %d", got)
	}

	// The retries, visiting one other workspace a pass, each carrying on
	// where the last stopped.
	owner.Exec(ctx, `DROP TRIGGER refuse ON grants`)
	var others int
	c.DB.Pool.QueryRow(ctx, `SELECT count(*) FROM tenants WHERE app_id IS DISTINCT FROM 'quizzer'`).Scan(&others)
	passes := 0
	for pass.Finished == 0 && passes <= others+1 {
		if pass, err = appapi.FinishAppDeletions(ctx, c.DB, 100, 1); err != nil {
			t.Fatalf("a retry: %v", err)
		}
		passes++
	}
	if pass.Finished != 1 || passes < others || passes > others+1 {
		t.Fatalf("finished after %d passes over %d other workspaces: %+v", passes, others, pass)
	}
	if got := inTenant(t, c, own, `SELECT count(*) FROM grants WHERE app_id = 'quizzer'`); got != 0 {
		t.Errorf("grants after the retry: %d", got)
	}
	var left int
	c.DB.Pool.QueryRow(ctx, `SELECT count(*) FROM tenants WHERE app_id = 'quizzer'`).Scan(&left)
	if left != 0 {
		t.Errorf("%d of its workspaces left", left)
	}
	c.DB.Pool.QueryRow(ctx, `SELECT count(*) FROM app_deletions WHERE app_id = 'quizzer' AND data_deleted_at IS NOT NULL AND last_error IS NULL`).Scan(&left)
	if left != 1 {
		t.Error("the deletion isn't recorded as done")
	}
}

// Pending deletions take turns: a big app that uses up each pass's budget
// doesn't hold back one deleted after it.
func TestAppDeletionsTakeTurns(t *testing.T) {
	c := coretest.Start(t)
	ctx := context.Background()
	ada := c.SignIn("ada@example.com")
	for _, id := range []string{"big", "small"} {
		appUsers(t, c, kit(t, c, createApp(t, ada, id, strings.ToUpper(id[:1])+id[1:], "").Key, ""))
		if code := ada.Do("DELETE", "/api/v1/developer/apps/"+id, nil, nil); code != 200 {
			t.Fatalf("delete %s: %d", id, code)
		}
	}
	// One workspace a pass: big's first, then small's.
	for range 2 {
		if _, err := appapi.FinishAppDeletions(ctx, c.DB, 1, 1000); err != nil {
			t.Fatal(err)
		}
	}
	var big, small int
	c.DB.Pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM tenants WHERE app_id = 'big'), (SELECT count(*) FROM tenants WHERE app_id = 'small')`).Scan(&big, &small)
	if big != 1 || small != 1 {
		t.Fatalf("after two passes: big has %d workspaces left, small %d", big, small)
	}
}
