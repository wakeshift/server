package accounts_test

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/accounts"
	"github.com/wakeshift/server/internal/dbtest"
	"github.com/wakeshift/server/internal/orgs"
)

// Each sign-in stores the provider's current verified email, so an
// organization invite matches the address the person holds now: once the
// GitHub account's email has moved on, an invite to the old address can't
// be accepted by it, and one to the new address can. An address another
// user has is refused, and nothing changes.
func TestSignInRefreshesTheEmail(t *testing.T) {
	d := dbtest.New(t)
	s := &accounts.Store{Pool: d.Pool, Allowed: accounts.AllowList("owner@corp.example,old@corp.example,eve@else.example")}
	owner, err := s.SignIn(ctx, accounts.Identity{Provider: "google", Subject: "o", Email: "owner@corp.example"})
	if err != nil {
		t.Fatal(err)
	}
	old, err := s.SignIn(ctx, accounts.Identity{Provider: "github", Subject: "42", Email: "old@corp.example"})
	if err != nil {
		t.Fatal(err)
	}
	again, err := s.SignIn(ctx, accounts.Identity{Provider: "github", Subject: "42", Email: "Personal@Else.example"})
	if err != nil || again.ID != old.ID || again.Email != "personal@else.example" {
		t.Fatalf("after the email change: %+v %v", again, err)
	}
	if n := count(t, d, `SELECT count(*) FROM identities WHERE provider = 'github' AND subject = '42' AND email = 'personal@else.example'`); n != 1 {
		t.Errorf("the identity's email wasn't updated")
	}

	var org string
	invite := func(email string) string {
		t.Helper()
		var token string
		if err := pgx.BeginFunc(ctx, d.Pool, func(tx pgx.Tx) (err error) {
			if org == "" {
				if org, err = orgs.Default(ctx, tx, owner.ID); err != nil {
					return err
				}
			}
			sent, err := orgs.CreateInvite(ctx, tx, owner.ID, org, email, orgs.Admin)
			token = sent.Token
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return token
	}
	accept := func(token string) error {
		return pgx.BeginFunc(ctx, d.Pool, func(tx pgx.Tx) error {
			_, err := orgs.Accept(ctx, tx, old.ID, token)
			return err
		})
	}
	var wrong *orgs.WrongAccountError
	if err := accept(invite("old@corp.example")); !errors.As(err, &wrong) {
		t.Fatalf("accepting the old address's invite: %v, want the wrong account", err)
	}
	if err := accept(invite("personal@else.example")); err != nil {
		t.Fatalf("accepting the new address's invite: %v", err)
	}

	// Moved to an address another account has: refused, and kept as it was.
	if _, err := s.SignIn(ctx, accounts.Identity{Provider: "google", Subject: "eve", Email: "eve@else.example"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SignIn(ctx, accounts.Identity{Provider: "github", Subject: "42", Email: "eve@else.example"}); !errors.Is(err, accounts.ErrAccountExists) {
		t.Fatalf("moving to another user's address: %v", err)
	}
	if n := count(t, d, `SELECT count(*) FROM users WHERE id = $1 AND email = 'personal@else.example'`, old.ID); n != 1 {
		t.Fatal("a refused sign-in changed the email")
	}
}
