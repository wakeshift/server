package accounts_test

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/accounts"
	"github.com/wakeshift/server/internal/dbtest"
	"github.com/wakeshift/server/internal/orgs"
)

// Closed sign-up (a self-hosted server's default): only the allow list
// makes accounts, whatever the cap. A stranger, someone with an invite code
// and someone with an organization invite all wait on the waitlist, with
// no account, and spend nothing; the operator's invite off the waitlist
// admits. Existing users still sign in.
func TestClosedSignupsAdmitOnlyTheAllowList(t *testing.T) {
	d := dbtest.New(t)
	s := &accounts.Store{Pool: d.Pool, Allowed: accounts.AllowList("ada@corp.example,@corp.example"), Closed: true}
	// The default cap (50) has room: it admits nobody while closed.
	if limit, used, err := accounts.SignupCap(ctx, d.Pool); err != nil || used >= limit {
		t.Fatalf("cap %d used %d: %v", limit, used, err)
	}
	waitlisted := func(id accounts.Identity) string {
		t.Helper()
		_, err := s.SignIn(ctx, id)
		var w *accounts.WaitlistedError
		if !errors.As(err, &w) {
			t.Fatalf("%s: %v, want the waitlist", id.Email, err)
		}
		return w.Entry
	}

	ada, err := s.SignIn(ctx, accounts.Identity{Provider: "github", Subject: "1", Email: "ada@corp.example"})
	if err != nil {
		t.Fatalf("on the list: %v", err)
	}
	// A Google account the listed domain manages.
	if _, err := s.SignIn(ctx, accounts.Identity{Provider: "google", Subject: "g", Email: "bob@corp.example", HostedDomain: "corp.example"}); err != nil {
		t.Fatalf("the listed domain: %v", err)
	}
	users := count(t, d, `SELECT count(*) FROM users`)

	// A stranger.
	waitlisted(accounts.Identity{Provider: "github", Subject: "999", Email: "stranger@evil.example"})
	// An invite code from an existing user: not spent.
	code, err := s.UserInvite(ctx, ada.ID)
	if err != nil {
		t.Fatal(err)
	}
	waitlisted(accounts.Identity{Provider: "github", Subject: "998", Email: "friend@else.example", Invite: code.Code})
	if left := count(t, d, `SELECT uses_left FROM invites WHERE code = $1`, code.Code); left != code.UsesLeft {
		t.Errorf("the invite code was spent: %d left of %d", left, code.UsesLeft)
	}
	// An organization invite for the very address.
	var token string
	if err := pgx.BeginFunc(ctx, d.Pool, func(tx pgx.Tx) error {
		org, err := orgs.Default(ctx, tx, ada.ID)
		if err != nil {
			return err
		}
		sent, err := orgs.CreateInvite(ctx, tx, ada.ID, org, "cy@else.example", orgs.Admin)
		token = sent.Token
		return err
	}); err != nil {
		t.Fatal(err)
	}
	cy := accounts.Identity{Provider: "google", Subject: "cy", Email: "cy@else.example", OrgInvite: token}
	entry := waitlisted(cy)
	if n := count(t, d, `SELECT count(*) FROM users`); n != users {
		t.Fatalf("closed sign-up made %d accounts", n-users)
	}

	// The operator invites cy off the waitlist: the next sign-in admits.
	if err := pgx.BeginFunc(ctx, d.Pool, func(tx pgx.Tx) error {
		_, err := accounts.InviteFromWaitlist(ctx, tx, []string{entry}, "operator@corp.example")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SignIn(ctx, cy); err != nil {
		t.Fatalf("invited off the waitlist: %v", err)
	}
	// Existing users sign in as ever.
	if again, err := s.SignIn(ctx, accounts.Identity{Provider: "github", Subject: "1", Email: "ada@corp.example"}); err != nil || again.ID != ada.ID {
		t.Fatalf("an existing user: %v", err)
	}
}

// Open (the hosted service, or WAKESHIFT_OPEN_SIGNUPS=1): as before, the
// cap admits anyone with room, and the list passes it.
func TestOpenSignupsKeepTheCap(t *testing.T) {
	d := dbtest.New(t)
	s := &accounts.Store{Pool: d.Pool, Allowed: accounts.AllowList("@corp.example")}
	if _, err := s.SignIn(ctx, accounts.Identity{Provider: "github", Subject: "999", Email: "stranger@else.example"}); err != nil {
		t.Fatalf("under the cap: %v", err)
	}
}
