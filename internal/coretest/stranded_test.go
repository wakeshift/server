package coretest_test

import (
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/apps/agentkit"
	"github.com/wakeshift/server/internal/appapi"
	"github.com/wakeshift/server/internal/beacon"
	"github.com/wakeshift/server/internal/coretest"
)

// A watching Codex thread whose connector stopped, with a message waiting
// for it past StrandedAfter, is counted as stranded (the expiry pass logs
// it for a metric); one whose connector holds its stream, whose message is
// new, that never had a connector, or that stopped watching isn't.
func TestStrandedCodexThreads(t *testing.T) {
	c := coretest.Start(t)
	app := chatApp(t, c)
	codex := actAs(t, app, "ada", "codex")

	const thread = "01a14bd7-bbbc-7b70-a723-0abb9f38118e"
	sess, err := agentkit.ConversationSession(ctx, codex, thread)
	if err != nil {
		t.Fatal(err)
	}
	lineSub(t, c, app, codex, sess.Handle)
	stranded := func() int64 {
		t.Helper()
		var n int64
		if err := c.DB.InTenant(ctx, codex.Workspace, func(tx pgx.Tx) (err error) {
			n, err = appapi.StrandedCodex(ctx, tx, appapi.StrandedAfter)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return n
	}
	exec := func(sql string) {
		t.Helper()
		if err := c.DB.InTenant(ctx, codex.Workspace, func(tx pgx.Tx) error { _, err := tx.Exec(ctx, sql); return err }); err != nil {
			t.Fatal(err)
		}
	}
	waiting := func() int {
		var n int
		c.DB.InTenant(ctx, codex.Workspace, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM deliveries WHERE state = 'pending' AND target <> ''`).Scan(&n)
		})
		return n
	}
	// Waiting for longer than StrandedAfter, as if published then.
	age := func() {
		exec(`UPDATE deliveries SET available_at = now() - interval '3 minutes' WHERE state = 'pending'`)
	}

	// Never connected through the connector: not counted (the results tell
	// the agent to install it).
	watched, err := threadWatch(t, c, app, codex, thread)
	if err != nil {
		t.Fatal(err)
	}
	postLine(t, c, app, codex, "one")
	waitFor(t, "waiting", func() bool { return waiting() > 0 })
	age()
	if n := stranded(); n != 0 {
		t.Fatalf("no connector ever: %d stranded", n)
	}

	// The connector connects and holds the stream: not stranded.
	claims, err := beacon.Verify(watched.Beacon, thread, []string{c.URL}, coreKeys(t, c), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	code, out := redeemTicket(t, claims.Redeem, claims.Ticket, thread)
	if code != 200 {
		t.Fatalf("redeem: %d %v", code, out)
	}
	exec(`UPDATE sessions SET connector = 'wakeshift/2026.10.27.0857+49a3673 codex'`)
	ws, _, err := websocket.Dial(ctx, out["url"].(string), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.CloseNow()
	waitFor(t, "connected", func() bool {
		s, _ := agentkit.ConversationSession(ctx, codex, thread)
		return s.Connector != nil && s.Connector.Running
	})

	// It stops (its lease goes quiet); a message waits for it.
	ws.Close(websocket.StatusNormalClosure, "")
	waitFor(t, "stopped", func() bool {
		s, _ := agentkit.ConversationSession(ctx, codex, thread)
		return s.Connector != nil && !s.Connector.Running
	})
	exec(`UPDATE stream_leases SET renewed_at = now() - interval '2 minutes'`)
	postLine(t, c, app, codex, "two")
	waitFor(t, "waiting", func() bool { return waiting() > 0 })
	exec(`UPDATE deliveries SET available_at = now() WHERE state = 'pending'`)
	if n := stranded(); n != 0 {
		t.Fatalf("a message just published: %d stranded", n)
	}
	age()
	if n := stranded(); n != 1 {
		t.Fatalf("stopped, with a message waiting: %d stranded", n)
	}
	// A connector holding the stream again (its lease renewed): not stranded.
	exec(`UPDATE stream_leases SET renewed_at = now()`)
	if n := stranded(); n != 0 {
		t.Fatalf("a held stream: %d stranded", n)
	}
	exec(`UPDATE stream_leases SET renewed_at = now() - interval '2 minutes'`)
	// Another workspace counts none of it.
	other := c.SignIn("bob@example.com").Workspace(t)
	if err := c.DB.InTenant(ctx, other, func(tx pgx.Tx) error {
		n, err := appapi.StrandedCodex(ctx, tx, appapi.StrandedAfter)
		if err == nil && n != 0 {
			t.Errorf("another workspace: %d stranded", n)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// Unwatched: it doesn't want to be woken, so nothing is stranded.
	if err := agentkit.Unwatch(ctx, codex, sess.Handle); err != nil {
		t.Fatal(err)
	}
	if n := stranded(); n != 0 {
		t.Fatalf("unwatched: %d stranded", n)
	}
}
