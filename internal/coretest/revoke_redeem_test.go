package coretest_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/beacon"
	"github.com/wakeshift/server/internal/coretest"
	"github.com/wakeshift/server/internal/grants"
)

// Revoking the app's grant while a redeem supersedes an earlier session
// made under it waits for one or the other, rather than deadlocking: the
// redeem takes the grant before ending the earlier session's links and
// subscriptions, which revoking ends too, after locking the grant. The
// revocation stands, and the redeem is refused.
func TestGrantRevokeDuringSupersedingRedeem(t *testing.T) {
	c := coretest.Start(t)
	app := noteApp(t, c, "ci")
	keys := coreKeys(t, c)
	const topic = "ci/acme/web"
	const conv = "openclaw/0123456789abcdef/agent:main:main"
	viaCodex := func(n int) beacon.Claims {
		t.Helper()
		meta := map[string]any{"x-codex-turn-metadata": map[string]any{"thread_id": fmt.Sprintf("01a155a9-0000-7000-8000-%012d", n), "thread_source": "openclaw"}}
		sub, err := subscribe(app, caller{User: "oc", Client: "codex-mcp-client", Meta: meta}, topic, subOpts{})
		if err != nil {
			t.Fatal(err)
		}
		tok, _ := sub.Addition.Meta[beacon.MetaKey].(string)
		cl, err := beacon.VerifyBound(tok, beacon.Binding{Conversation: conv, Unbound: true}, []string{c.URL}, keys, time.Now())
		if err != nil {
			t.Fatalf("beacon: %v", err)
		}
		return cl
	}
	code, first := redeemFor(t, viaCodex(1), conv, "")
	if code != 200 || first == "" {
		t.Fatalf("first redeem: %d", code)
	}
	cl := viaCodex(2) // a new Codex thread, the same conversation
	tenant := strings.SplitN(cl.Ticket, "_", 3)[1]
	var grant string
	if err := c.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT grant_id::text FROM connector_tickets WHERE redeemed_at IS NULL`).Scan(&grant)
	}); err != nil {
		t.Fatal(err)
	}

	redeemed := make(chan int, 1)
	err := c.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		// The revocation's lock on the grant, held open while the redeem
		// starts.
		if _, err := tx.Exec(ctx, `SELECT 1 FROM grants WHERE id = $1 FOR UPDATE`, grant); err != nil {
			return err
		}
		go func() {
			code, _ := redeemFor(t, cl, conv, first)
			redeemed <- code
		}()
		time.Sleep(300 * time.Millisecond) // the redeem is waiting now
		_, err := grants.Revoke(ctx, tx, grant, "user:test")
		return err
	})
	code = <-redeemed
	if err != nil {
		t.Fatalf("revoking the grant: %v", err)
	}
	if code != 403 {
		t.Fatalf("the redeem after the revocation: %d, want 403", code)
	}
}
