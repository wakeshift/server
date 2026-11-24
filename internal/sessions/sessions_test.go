package sessions_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/agents"
	"github.com/wakeshift/server/internal/dbtest"
	"github.com/wakeshift/server/internal/sessions"
)

var ctx = context.Background()

// Two registrations for the same conversation at once (a harness's parallel
// tool calls on a new thread) get the same session; neither fails.
func TestConcurrentRegistrationsShareTheConversation(t *testing.T) {
	d := dbtest.New(t)
	tenant := dbtest.NewTenant(t, d)
	if err := d.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		_, err := agents.Create(ctx, tx, "codex", "")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	n := sessions.New{Agent: "codex", Conversation: "thread-1", Runtime: sessions.Connector, CreatedBy: "test"}
	var first, second sessions.Session
	registered, done := make(chan struct{}), make(chan error, 1)
	go func() {
		done <- d.InTenant(ctx, tenant, func(tx pgx.Tx) (err error) {
			first, err = sessions.Register(ctx, tx, n)
			close(registered)
			time.Sleep(300 * time.Millisecond) // the other one looks, then waits for the lock
			return err
		})
	}()
	<-registered
	err := d.InTenant(ctx, tenant, func(tx pgx.Tx) (err error) {
		second, err = sessions.Register(ctx, tx, n)
		return err
	})
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err != nil {
		t.Fatalf("the second registration: %v", err)
	}
	if first.ID != second.ID || first.Handle != "codex#1" {
		t.Fatalf("got %s (%s) and %s (%s)", first.ID, first.Handle, second.ID, second.Handle)
	}
}
