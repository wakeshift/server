package accounts_test

import (
	"errors"
	"testing"
	"time"

	"github.com/wakeshift/server/internal/accounts"
	"github.com/wakeshift/server/internal/dbtest"
)

// A sign-in moving to an address that another account takes at the same
// moment (committed after this sign-in's check, before its update) is
// refused, and changes nothing: neither the user's email nor the
// identity's.
func TestEmailChangeRaceChangesNothing(t *testing.T) {
	d := dbtest.New(t)
	s := &accounts.Store{Pool: d.Pool, Allowed: accounts.AllowList("ada@example.com")}
	ada, err := s.SignIn(ctx, accounts.Identity{Provider: "github", Subject: "42", Email: "ada@example.com"})
	if err != nil {
		t.Fatal(err)
	}

	// Another account takes the address, uncommitted: the sign-in's check
	// can't see it, and its update waits on the unique index.
	other, err := d.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Rollback(ctx)
	if _, err := other.Exec(ctx, `INSERT INTO users (email) VALUES ('same@example.com')`); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := s.SignIn(ctx, accounts.Identity{Provider: "github", Subject: "42", Email: "same@example.com"})
		done <- err
	}()
	// Wait until the sign-in is blocked on the other transaction's row.
	deadline := time.Now().Add(10 * time.Second)
	for {
		var waiting int
		if err := d.Pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("the sign-in finished without waiting: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("the sign-in never waited on the other account")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := other.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, accounts.ErrAccountExists) {
		t.Fatalf("the sign-in that lost the race: %v, want ErrAccountExists", err)
	}
	if n := count(t, d, `SELECT count(*) FROM users WHERE id = $1 AND email = 'ada@example.com'`, ada.ID); n != 1 {
		t.Error("the user's email changed")
	}
	if n := count(t, d, `SELECT count(*) FROM identities WHERE provider = 'github' AND subject = '42' AND email = 'ada@example.com'`); n != 1 {
		t.Error("the identity's email changed")
	}
}
