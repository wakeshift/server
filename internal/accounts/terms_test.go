package accounts_test

import (
	"testing"
	"time"

	"github.com/wakeshift/server/internal/accounts"
	"github.com/wakeshift/server/internal/dbtest"
)

// Every new account records the Terms version it accepted and when, however
// it got in (the operator's list, under the cap); signing in again changes
// neither, and a waitlisted sign-in, with no account, records nothing.
func TestSignUpRecordsTerms(t *testing.T) {
	d := dbtest.New(t)
	s := &accounts.Store{Pool: d.Pool, Allowed: accounts.AllowList("ada@example.com"), TermsVersion: accounts.TermsVersion}
	setCap(t, d, 2)
	ada, err := s.SignIn(ctx, accounts.Identity{Provider: "google", Subject: "1", Email: "ada@example.com", Name: "Ada"})
	if err != nil {
		t.Fatal(err)
	}
	bob, err := s.SignIn(ctx, accounts.Identity{Provider: "github", Subject: "2", Email: "bob@example.com", Name: "Bob"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SignIn(ctx, accounts.Identity{Provider: "github", Subject: "3", Email: "cy@example.com"}); err == nil {
		t.Fatal("cy got past the cap")
	}
	terms := func(id string) (string, time.Time) {
		t.Helper()
		var v string
		var at time.Time
		if err := d.Pool.QueryRow(ctx, `SELECT terms_version, terms_accepted_at FROM users WHERE id = $1`, id).Scan(&v, &at); err != nil {
			t.Fatal(err)
		}
		return v, at
	}
	for _, u := range []accounts.User{ada, bob} {
		v, at := terms(u.ID)
		if v != accounts.TermsVersion || time.Since(at) > time.Minute {
			t.Fatalf("%s: terms %q at %v", u.Email, v, at)
		}
	}
	_, first := terms(ada.ID)
	time.Sleep(10 * time.Millisecond)
	if _, err := s.SignIn(ctx, accounts.Identity{Provider: "google", Subject: "1", Email: "ada@example.com"}); err != nil {
		t.Fatal(err)
	}
	if _, again := terms(ada.ID); !again.Equal(first) {
		t.Fatalf("signing in again moved the acceptance from %v to %v", first, again)
	}
	var users int
	if err := d.Pool.QueryRow(ctx, `SELECT count(*) FROM users WHERE email = 'cy@example.com'`).Scan(&users); err != nil || users != 0 {
		t.Fatalf("a waitlisted person has %d users rows (%v)", users, err)
	}
}

// A server with no Terms (no WAKESHIFT_TERMS_VERSION, not Wakeshift's hosted
// service) records no acceptance: neither a version nor a time.
func TestSignUpWithoutTerms(t *testing.T) {
	d := dbtest.New(t)
	s := &accounts.Store{Pool: d.Pool, Allowed: accounts.AllowList("ada@example.com")}
	ada, err := s.SignIn(ctx, accounts.Identity{Provider: "google", Subject: "1", Email: "ada@example.com", Name: "Ada"})
	if err != nil {
		t.Fatal(err)
	}
	var v *string
	var at *time.Time
	if err := d.Pool.QueryRow(ctx, `SELECT terms_version, terms_accepted_at FROM users WHERE id = $1`, ada.ID).Scan(&v, &at); err != nil {
		t.Fatal(err)
	}
	if v != nil || at != nil {
		t.Fatalf("terms %v at %v, want neither", v, at)
	}
}
