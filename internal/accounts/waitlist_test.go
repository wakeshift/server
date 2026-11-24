package accounts_test

import (
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/accounts"
	"github.com/wakeshift/server/internal/db"
	"github.com/wakeshift/server/internal/dbtest"
)

func setCap(t *testing.T, d *db.DB, n int) {
	t.Helper()
	if err := pgx.BeginFunc(ctx, d.Pool, func(tx pgx.Tx) error {
		_, err := accounts.SetSignupCap(ctx, tx, n)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func count(t *testing.T, d *db.DB, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := d.Pool.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// via is how the user's account.created entry says they got in ("" for
// none).
func via(t *testing.T, d *db.DB, s *accounts.Store, user string) string {
	t.Helper()
	ws, err := s.Workspaces(ctx, user)
	if err != nil || len(ws) != 1 {
		t.Fatalf("workspaces: %v %v", ws, err)
	}
	var v string
	err = d.InTenant(ctx, ws[0].ID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT coalesce(max(detail->>'via'), '') FROM audit_log WHERE action = 'account.created' AND actor = $1`, "user:"+user).Scan(&v)
	})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestTheSignupCapStartsAt50MoreThanToday(t *testing.T) {
	d := dbtest.New(t)
	limit, used, err := accounts.SignupCap(ctx, d.Pool)
	if err != nil || limit != 50 || used != 0 {
		t.Fatalf("cap %d, used %d, %v", limit, used, err)
	}
}

// Under the cap, anyone gets an account; at it, a new sign-in joins the
// waitlist with no account and no workspace, once however often they sign
// in; invite links pass the cap and count towards it; an invited entry's
// next sign-in makes the account.
func TestSignUpCapAndWaitlist(t *testing.T) {
	d := dbtest.New(t)
	s := &accounts.Store{Pool: d.Pool}
	setCap(t, d, 1)

	ada, err := s.SignIn(ctx, accounts.Identity{Provider: "google", Subject: "ada", Email: "ada@example.com", Name: "Ada"})
	if err != nil {
		t.Fatalf("under the cap: %v", err)
	}
	if v := via(t, d, s, ada.ID); v != "cap" {
		t.Errorf("ada's account.created via %q", v)
	}

	users, tenants := count(t, d, `SELECT count(*) FROM users`), count(t, d, `SELECT count(*) FROM tenants`)
	bob := accounts.Identity{Provider: "github", Subject: "42", Email: "Bob@Example.com", Name: "Bob\nBuilder"}
	_, err = s.SignIn(ctx, bob)
	var w *accounts.WaitlistedError
	if !errors.As(err, &w) || w.Entry == "" {
		t.Fatalf("at the cap: %v", err)
	}
	if count(t, d, `SELECT count(*) FROM users`) != users || count(t, d, `SELECT count(*) FROM tenants`) != tenants ||
		count(t, d, `SELECT count(*) FROM organizations`) != 1 || count(t, d, `SELECT count(*) FROM identities WHERE provider = 'github'`) != 0 {
		t.Fatal("waitlisting made an account, a workspace, an organization or an identity")
	}
	var email, name string
	d.Pool.QueryRow(ctx, `SELECT email, name FROM waitlist WHERE id = $1`, w.Entry).Scan(&email, &name)
	if email != "bob@example.com" || name != "Bob Builder" {
		t.Errorf("the entry: %q %q", email, name)
	}
	// Again, and with another provider for the same address: the same entry.
	for _, again := range []accounts.Identity{bob, {Provider: "google", Subject: "bob-g", Email: "bob@example.com"}} {
		_, err := s.SignIn(ctx, again)
		var w2 *accounts.WaitlistedError
		if !errors.As(err, &w2) || w2.Entry != w.Entry {
			t.Fatalf("again (%s): %v", again.Provider, err)
		}
	}
	if n := count(t, d, `SELECT count(*) FROM waitlist`); n != 1 {
		t.Fatalf("%d entries", n)
	}
	e, err := s.WaitlistEntry(ctx, w.Entry)
	if err != nil || e.Invited || e.JoinedAt.IsZero() {
		t.Fatalf("the entry for its page: %+v %v", e, err)
	}

	// An invite link passes the cap, and the account counts towards it.
	code, err := s.UserInvite(ctx, ada.ID)
	if err != nil {
		t.Fatal(err)
	}
	cy, err := s.SignIn(ctx, accounts.Identity{Provider: "google", Subject: "cy", Email: "cy@example.com", Invite: code.Code})
	if err != nil {
		t.Fatalf("with an invite: %v", err)
	}
	if v := via(t, d, s, cy.ID); v != "invite" {
		t.Errorf("cy's account.created via %q", v)
	}
	if limit, used, _ := accounts.SignupCap(ctx, d.Pool); limit != 1 || used != 2 {
		t.Errorf("cap %d, used %d", limit, used)
	}
	// A used-up or unknown invite is no invite: at the cap, the waitlist.
	if _, err := s.SignIn(ctx, accounts.Identity{Provider: "google", Subject: "dee", Email: "dee@example.com", Invite: "madeup"}); !errors.As(err, &w) {
		t.Fatalf("a made-up invite at the cap: %v", err)
	}

	// Invited off the waitlist: the next sign-in, with either identity,
	// makes the account, past the cap.
	var invited []accounts.WaitlistInvite
	if err := pgx.BeginFunc(ctx, d.Pool, func(tx pgx.Tx) (err error) {
		var bobEntry string
		tx.QueryRow(ctx, `SELECT id::text FROM waitlist WHERE email = 'bob@example.com'`).Scan(&bobEntry)
		invited, err = accounts.InviteFromWaitlist(ctx, tx, []string{bobEntry}, "admin@example.com")
		return err
	}); err != nil || len(invited) != 1 || invited[0].Email != "bob@example.com" || invited[0].Again {
		t.Fatalf("invite: %+v %v", invited, err)
	}
	if e, _ := s.WaitlistEntry(ctx, invited[0].ID); !e.Invited {
		t.Error("the page doesn't say invited")
	}
	b, err := s.SignIn(ctx, bob)
	if err != nil {
		t.Fatalf("invited: %v", err)
	}
	if v := via(t, d, s, b.ID); v != "waitlist" {
		t.Errorf("bob's account.created via %q", v)
	}
	var signedUp bool
	var user string
	d.Pool.QueryRow(ctx, `SELECT signed_up_at IS NOT NULL, coalesce(user_id::text, '') FROM waitlist WHERE id = $1`, invited[0].ID).Scan(&signedUp, &user)
	if !signedUp || user != b.ID {
		t.Errorf("the entry after sign-up: %v %q", signedUp, user)
	}
	if _, err := s.WaitlistEntry(ctx, invited[0].ID); !errors.Is(err, accounts.ErrNoEntry) {
		t.Errorf("the page after sign-up: %v", err)
	}
	// Inviting it again does nothing: it has its account.
	if err := pgx.BeginFunc(ctx, d.Pool, func(tx pgx.Tx) (err error) {
		invited, err = accounts.InviteFromWaitlist(ctx, tx, []string{invited[0].ID}, "admin@example.com")
		return err
	}); err != nil || len(invited) != 0 {
		t.Fatalf("invite again: %+v %v", invited, err)
	}

	// Raising the cap lets a waitlisted person in on their next sign-in.
	setCap(t, d, 10)
	dee, err := s.SignIn(ctx, accounts.Identity{Provider: "google", Subject: "dee", Email: "dee@example.com"})
	if err != nil {
		t.Fatalf("after raising the cap: %v", err)
	}
	if v := via(t, d, s, dee.ID); v != "cap" {
		t.Errorf("dee's account.created via %q", v)
	}
	if n := count(t, d, `SELECT count(*) FROM waitlist WHERE signed_up_at IS NULL`); n != 0 {
		t.Errorf("%d entries still waiting", n)
	}
}

// Two sign-ups at once for the last place: one account, one entry.
func TestTheLastPlaceIsTakenOnce(t *testing.T) {
	d := dbtest.New(t)
	s := &accounts.Store{Pool: d.Pool}
	setCap(t, d, 1)
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = s.SignIn(ctx, accounts.Identity{Provider: "google", Subject: string(rune('a' + i)), Email: string(rune('a'+i)) + "@example.com"})
		}()
	}
	wg.Wait()
	var w *accounts.WaitlistedError
	ok, waited := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case errors.As(err, &w):
			waited++
		default:
			t.Fatal(err)
		}
	}
	if ok != 1 || waited != 1 || count(t, d, `SELECT count(*) FROM users`) != 1 {
		t.Fatalf("%d accounts, %d waitlisted", ok, waited)
	}
}

func TestWaitlistNote(t *testing.T) {
	d := dbtest.New(t)
	s := &accounts.Store{Pool: d.Pool}
	setCap(t, d, 0)
	_, err := s.SignIn(ctx, accounts.Identity{Provider: "google", Subject: "ada", Email: "ada@example.com"})
	var w *accounts.WaitlistedError
	if !errors.As(err, &w) {
		t.Fatal(err)
	}
	if err := s.SetWaitlistNote(ctx, w.Entry, "  A bot that\r\nreviews PRs  "); err != nil {
		t.Fatal(err)
	}
	if e, _ := s.WaitlistEntry(ctx, w.Entry); e.Note != "A bot that\nreviews PRs" {
		t.Errorf("note %q", e.Note)
	}
	for _, bad := range []string{strings.Repeat("é", accounts.MaxNote+1), "a\x00b", "\x1b[31m"} {
		if err := s.SetWaitlistNote(ctx, w.Entry, bad); !errors.Is(err, accounts.ErrBadNote) {
			t.Errorf("%q: %v", bad, err)
		}
	}
	if err := s.SetWaitlistNote(ctx, w.Entry, strings.Repeat("é", accounts.MaxNote)); err != nil {
		t.Errorf("at the limit: %v", err)
	}
	if err := s.SetWaitlistNote(ctx, "00000000-0000-0000-0000-000000000000", "x"); !errors.Is(err, accounts.ErrNoEntry) {
		t.Errorf("no such entry: %v", err)
	}
}

func TestSetSignupCap(t *testing.T) {
	d := dbtest.New(t)
	err := pgx.BeginFunc(ctx, d.Pool, func(tx pgx.Tx) error {
		old, err := accounts.SetSignupCap(ctx, tx, 120)
		if err != nil || old != 50 {
			t.Errorf("old %d, %v", old, err)
		}
		for _, bad := range []int{-1, accounts.MaxCap + 1} {
			if _, err := accounts.SetSignupCap(ctx, tx, bad); !errors.Is(err, accounts.ErrBadCap) {
				t.Errorf("%d: %v", bad, err)
			}
		}
		return nil
	})
	if limit, _, _ := accounts.SignupCap(ctx, d.Pool); err != nil || limit != 120 {
		t.Fatalf("cap %d, %v", limit, err)
	}
}

// Entries go 30 days after their sign-up, or 90 days after an invitation
// never used; one never invited stays; removing one deletes it.
func TestWaitlistRetention(t *testing.T) {
	d := dbtest.New(t)
	ins := func(email, extra string) string {
		t.Helper()
		var id string
		if err := d.Pool.QueryRow(ctx, `INSERT INTO waitlist (provider, subject, email, created_at, invited_at, signed_up_at)
			VALUES ('google', $1, $1, now() - interval '400 days', `+extra+`) RETURNING id::text`, email).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	ins("old-signup@x", `now() - interval '40 days', now() - interval '31 days'`)
	recent := ins("recent-signup@x", `now() - interval '40 days', now() - interval '29 days'`)
	ins("stale-invite@x", `now() - interval '91 days', NULL`)
	fresh := ins("fresh-invite@x", `now() - interval '89 days', NULL`)
	waiting := ins("waiting@x", `NULL, NULL`)
	removed := ins("removed@x", `NULL, NULL`)
	n, err := accounts.PruneWaitlist(ctx, d.Pool, 100)
	if err != nil || n != 2 {
		t.Fatalf("pruned %d, %v", n, err)
	}
	var left []string
	rows, _ := d.Pool.Query(ctx, `SELECT id::text FROM waitlist ORDER BY email`)
	left, _ = pgx.CollectRows(rows, pgx.RowTo[string])
	if strings.Join(left, ",") != strings.Join([]string{fresh, recent, removed, waiting}, ",") {
		t.Fatalf("left %v", left)
	}
	if err := pgx.BeginFunc(ctx, d.Pool, func(tx pgx.Tx) error {
		gone, err := accounts.RemoveFromWaitlist(ctx, tx, []string{removed, "00000000-0000-0000-0000-000000000000"})
		if len(gone) != 1 || gone[0] != removed {
			t.Errorf("removed %v", gone)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if count(t, d, `SELECT count(*) FROM waitlist WHERE id = $1`, removed) != 0 {
		t.Fatal("a removed entry stayed")
	}
}
