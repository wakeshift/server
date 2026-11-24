package appapi

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/dbtest"
)

// serialize's lock is the workspace's own: an agent posting in one
// workspace doesn't wait for a same-named agent posting in another.
func TestSerializeIsPerWorkspace(t *testing.T) {
	d := dbtest.New(t)
	ctx := context.Background()
	a, b := dbtest.NewTenant(t, d), dbtest.NewTenant(t, d)

	held, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- d.InTenant(ctx, a, func(tx pgx.Tx) error {
			if err := serialize(ctx, tx, "post:claude-code"); err != nil {
				return err
			}
			close(held)
			<-release
			return nil
		})
	}()
	<-held
	defer func() {
		close(release)
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()

	short, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := d.InTenant(short, b, func(tx pgx.Tx) error { return serialize(short, tx, "post:claude-code") }); err != nil {
		t.Fatalf("another workspace's lock blocked this one: %v", err)
	}
	// Within the workspace, it still serializes.
	if err := d.InTenant(short, a, func(tx pgx.Tx) error {
		_, err := tx.Exec(short, `SET LOCAL lock_timeout = '200ms'`)
		if err == nil {
			err = serialize(short, tx, "post:claude-code")
		}
		return err
	}); err == nil {
		t.Fatal("the same workspace's lock didn't serialize")
	}
}
