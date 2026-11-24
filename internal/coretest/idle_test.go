package coretest_test

import (
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/appapi"
	"github.com/wakeshift/server/internal/core"
	"github.com/wakeshift/server/internal/coretest"
	"github.com/wakeshift/server/pkg/wakeshift"
)

// startIdle is a core whose own expiry pass never finds a workspace idle
// (it runs at start, and on a slow runner could come after a test aged
// its workspaces, forgetting them first): the tests run ExpireIdle
// themselves, with appapi.IdleFor.
func startIdle(t *testing.T) *coretest.Core {
	t.Helper()
	return coretest.StartWith(t, func(o *core.Options) { o.IdleFor = 100 * 365 * 24 * time.Hour })
}

// Only topics' conversations without a user expire: an app's users are
// never forgotten by Wakeshift, however long unused, whatever their ID
// looks like ("guest:…" was the SDKs' guest sign-in's, whose workspaces
// have been forgotten: such an ID is now an ordinary user's).
func TestIdleUsersAreKept(t *testing.T) {
	c := startIdle(t)
	app := headlessApp(t, c, "quiz", false)
	for _, u := range []string{"guest:old", "alice"} {
		if _, err := app.Workspace(ctx, u, ""); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.DB.Pool.Exec(ctx, `UPDATE tenants SET created_at = now() - interval '400 days', active_at = NULL WHERE app_id = 'quiz'`); err != nil {
		t.Fatal(err)
	}
	if n, err := appapi.ExpireIdle(ctx, c.DB, appapi.IdleFor, 100); err != nil || n != 0 {
		t.Fatalf("forgot %d, %v", n, err)
	}
	var left int
	if err := c.DB.Pool.QueryRow(ctx, `SELECT count(*) FROM tenants WHERE app_id = 'quiz'`).Scan(&left); err != nil || left != 2 {
		t.Fatalf("left %d, %v", left, err)
	}
}

// idleConversations subscribes one conversation without a user per
// client (each its own workspace) for an hour, then makes them look long
// unused, their subscriptions run out: what the expiry pass forgets. The
// first client's is the oldest. It returns their workspaces by client.
func idleConversations(t *testing.T, c *coretest.Core, app *wakeshift.Client, clients ...string) map[string]string {
	t.Helper()
	ws := map[string]string{}
	for i, client := range clients {
		if _, err := subscribe(app, caller{Client: client}, "news", subOpts{TTL: time.Hour}); err != nil {
			t.Fatal(err)
		}
		var id string
		if err := c.DB.Pool.QueryRow(ctx, `SELECT id::text FROM tenants WHERE app_id = $1 ORDER BY created_at DESC LIMIT 1`, coretest.CredsOf(app).ID).Scan(&id); err != nil {
			t.Fatal(err)
		}
		ws[client] = id
		if _, err := c.DB.Pool.Exec(ctx, `UPDATE tenants SET created_at = now() - make_interval(days => $2), active_at = NULL WHERE id = $1`, id, 50-i); err != nil {
			t.Fatal(err)
		}
		if err := c.DB.InTenant(ctx, id, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE topic_subscriptions SET expires_at = now() - interval '1 day'`)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	return ws
}

// An idle conversation's workspace is forgotten as if the app had
// forgotten the user: its daily usage counts go with it, and the app's
// log says Wakeshift did it and why. One the app acted for lately is
// kept.
func TestIdleConversationsAreForgottenAndLogged(t *testing.T) {
	c := startIdle(t)
	app := headlessApp(t, c, "quiz", false)
	ws := idleConversations(t, c, app, "claude-code", "codex")
	if _, err := c.DB.Pool.Exec(ctx, `UPDATE tenants SET active_at = now() WHERE id = $1`, ws["codex"]); err != nil {
		t.Fatal(err)
	}
	for _, id := range ws {
		if _, err := c.DB.Pool.Exec(ctx, `INSERT INTO usage_daily (day, tenant_id, app_id, delivered, dead, cancelled, age_ms)
			VALUES (current_date - 35, $1, 'quiz', 2, 0, 0, '{}')`, id); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := appapi.ExpireIdle(ctx, c.DB, appapi.IdleFor, 100); err != nil || n != 1 {
		t.Fatalf("forgot %d, %v", n, err)
	}
	rows, _ := c.DB.Pool.Query(ctx, `SELECT id::text FROM tenants WHERE app_id = 'quiz'`)
	left, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil || len(left) != 1 || left[0] != ws["codex"] {
		t.Fatalf("left %v (want %s), %v", left, ws["codex"], err)
	}
	for client, want := range map[string]int{"claude-code": 0, "codex": 1} {
		var got int
		c.DB.Pool.QueryRow(ctx, `SELECT count(*) FROM usage_daily WHERE tenant_id = $1`, ws[client]).Scan(&got)
		if got != want {
			t.Errorf("%s: %d usage rows, want %d", client, got, want)
		}
	}
	var actor, reason string
	var days int
	if err := c.DB.Pool.QueryRow(ctx, `SELECT actor, detail->>'reason', (detail->>'idle_days')::int FROM app_log
		WHERE app_id = 'quiz' AND action = 'app.user_forgotten'`).Scan(&actor, &reason, &days); err != nil {
		t.Fatal(err)
	}
	if actor != "hub" || reason != "idle" || days < 49 {
		t.Fatalf("logged: %s %s %d", actor, reason, days)
	}
	if n, err := appapi.ExpireIdle(ctx, c.DB, appapi.IdleFor, 100); err != nil || n != 0 {
		t.Fatalf("forgot %d more, %v", n, err)
	}
}

// An idle conversation's workspace that can't be erased (here a trigger
// refuses it) doesn't hold up the others: the pass forgets the rest and
// reports the failure. It goes oldest first, so before, the one that
// failed stopped every pass, and none after it was ever forgotten.
func TestAFailingIdleConversationDoesntHoldUpTheRest(t *testing.T) {
	c := startIdle(t)
	app := headlessApp(t, c, "quiz", false)
	ws := idleConversations(t, c, app, "claude-code", "codex") // claude-code's is the oldest
	// As a superuser, in the core's database: erasing the oldest fails.
	cfg, err := pgx.ParseConfig(os.Getenv("WAKESHIFT_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Database = c.DB.Pool.Config().ConnConfig.Database
	admin, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	if _, err := admin.Exec(ctx, `CREATE FUNCTION refuse_erase() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'refused'; END $$;
		CREATE TRIGGER refuse_erase BEFORE DELETE ON tenants FOR EACH ROW WHEN (OLD.id = '`+ws["claude-code"]+`') EXECUTE FUNCTION refuse_erase()`); err != nil {
		t.Fatal(err)
	}

	n, err := appapi.ExpireIdle(ctx, c.DB, appapi.IdleFor, 100)
	if err == nil || n != 1 {
		t.Fatalf("forgot %d, %v (want 1, and the failure)", n, err)
	}
	rows, _ := c.DB.Pool.Query(ctx, `SELECT id::text FROM tenants WHERE app_id = 'quiz'`)
	if left, err := pgx.CollectRows(rows, pgx.RowTo[string]); err != nil || len(left) != 1 || left[0] != ws["claude-code"] {
		t.Fatalf("left %v, %v", left, err)
	}
}

// Topics' conversations without a user (~conversation:…) are forgotten
// once idle, unless a live subscription still delivers to
// them: that counts as use.
func TestIdleConversationsAreForgotten(t *testing.T) {
	c := startIdle(t)
	app := headlessApp(t, c, "quiz", false)
	if _, err := subscribe(app, caller{Client: "claude-code"}, "news", subOpts{TTL: time.Hour}); err != nil {
		t.Fatal(err)
	}
	if _, err := subscribe(app, caller{Client: "codex"}, "news", subOpts{TTL: 90 * 24 * time.Hour}); err != nil {
		t.Fatal(err)
	}
	var short, long string
	if err := c.DB.Pool.QueryRow(ctx, `SELECT (SELECT id::text FROM tenants WHERE app_id = 'quiz' ORDER BY created_at LIMIT 1),
		(SELECT id::text FROM tenants WHERE app_id = 'quiz' ORDER BY created_at DESC LIMIT 1)`).Scan(&short, &long); err != nil {
		t.Fatal(err)
	}
	// Long unused; the first's subscription ran out (the expiry pass would
	// end it), the second's still runs.
	if _, err := c.DB.Pool.Exec(ctx, `UPDATE tenants SET created_at = now() - interval '40 days', active_at = NULL WHERE app_id = 'quiz'`); err != nil {
		t.Fatal(err)
	}
	if err := c.DB.InTenant(ctx, short, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE topic_subscriptions SET expires_at = now() - interval '1 day'`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	n, err := appapi.ExpireIdle(ctx, c.DB, appapi.IdleFor, 100)
	if err != nil || n != 1 {
		t.Fatalf("forgot %d, %v", n, err)
	}
	rows, _ := c.DB.Pool.Query(ctx, `SELECT id::text FROM tenants WHERE app_id = 'quiz'`)
	left, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil || len(left) != 1 || left[0] != long {
		t.Fatalf("left %v (want %s), %v", left, long, err)
	}
	// Noted as in use, so the next pass doesn't pick it again.
	var active bool
	if err := c.DB.Pool.QueryRow(ctx, `SELECT active_at > now() - interval '1 minute' FROM tenants WHERE id = $1`, long).Scan(&active); err != nil || !active {
		t.Fatalf("active: %v %v", active, err)
	}
}
