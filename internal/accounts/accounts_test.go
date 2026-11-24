package accounts_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/wakeshift/server/internal/accounts"
	"github.com/wakeshift/server/internal/dbtest"
)

var ctx = context.Background()

func TestSignInCreatesUserAndPersonalWorkspace(t *testing.T) {
	d := dbtest.New(t)
	s := &accounts.Store{Pool: d.Pool, Allowed: accounts.AllowList("ada@example.com")}

	u, err := s.SignIn(ctx, accounts.Identity{Provider: "google", Subject: "1", Email: "Ada@Example.com", Name: "Ada"})
	if err != nil || u.Email != "ada@example.com" {
		t.Fatalf("%+v %v", u, err)
	}
	ws, _ := s.Workspaces(ctx, u.ID)
	if len(ws) != 1 || ws[0].Kind != "personal" || ws[0].Role != "owner" || ws[0].Name != "Ada" {
		t.Fatalf("workspaces = %+v", ws)
	}
	// The same identity is the same user and workspace.
	again, _ := s.SignIn(ctx, accounts.Identity{Provider: "google", Subject: "1", Email: "ada@example.com"})
	if again.ID != u.ID {
		t.Fatal("signing in again made another user")
	}
	// Another account with the same email (a personal Google account made
	// with a work address, another provider) is refused, never linked.
	for _, other := range []accounts.Identity{
		{Provider: "google", Subject: "2", Email: "ada@example.com"},
		{Provider: "github", Subject: "99", Email: "ada@example.com"},
	} {
		if _, err := s.SignIn(ctx, other); !errors.Is(err, accounts.ErrAccountExists) {
			t.Fatalf("%s/%s: %v", other.Provider, other.Subject, err)
		}
	}
	if ws, _ := s.Workspaces(ctx, u.ID); len(ws) != 1 {
		t.Fatalf("workspaces = %+v", ws)
	}
	if _, w, err := s.Member(ctx, u.ID, ws[0].ID); err != nil || w.Role != "owner" {
		t.Fatalf("member: %+v %v", w, err)
	}
	// And one personal organization, which they own, with its creation in
	// its log: signing in again doesn't make another.
	var orgs, owners, logged int
	var name string
	if err := d.Pool.QueryRow(ctx, `SELECT count(*), max(o.name), count(*) FILTER (WHERE m.role = 'owner'),
			(SELECT count(*) FROM org_log l WHERE l.org_id = max(o.id::text)::uuid AND l.action = 'org.created')
		FROM organizations o JOIN organization_members m ON m.org_id = o.id AND m.user_id = o.personal_of
		WHERE o.personal_of = $1`, u.ID).Scan(&orgs, &name, &owners, &logged); err != nil {
		t.Fatal(err)
	}
	if orgs != 1 || owners != 1 || name != "Ada" || logged != 1 {
		t.Fatalf("personal organizations %d (owner of %d, named %q, logged %d)", orgs, owners, name, logged)
	}
}

// The allow list admits past the sign-up cap; anyone else waits.
func TestTheAllowListPassesTheCap(t *testing.T) {
	d := dbtest.New(t)
	setCap(t, d, 0)
	s := &accounts.Store{Pool: d.Pool, Allowed: accounts.AllowList("@example.com")}
	var waitlisted *accounts.WaitlistedError
	if _, err := s.SignIn(ctx, accounts.Identity{Provider: "google", Subject: "2", Email: "eve@evil.com", HostedDomain: "evil.com"}); !errors.As(err, &waitlisted) {
		t.Fatalf("outsider: %v", err)
	}
	if _, err := s.SignIn(ctx, accounts.Identity{Provider: "google", Subject: "3", Email: "x@example.com", HostedDomain: "example.com"}); err != nil {
		t.Fatalf("on the allow list: %v", err)
	}
}

func TestMemberOfAnotherWorkspace(t *testing.T) {
	d := dbtest.New(t)
	s := &accounts.Store{Pool: d.Pool, Allowed: func(accounts.Identity) bool { return true }}
	a, _ := s.SignIn(ctx, accounts.Identity{Provider: "dev", Subject: "a", Email: "a@x.com"})
	b, _ := s.SignIn(ctx, accounts.Identity{Provider: "dev", Subject: "b", Email: "b@x.com"})
	bws, _ := s.Workspaces(ctx, b.ID)
	if _, _, err := s.Member(ctx, a.ID, bws[0].ID); !errors.Is(err, accounts.ErrNotMember) {
		t.Fatalf("a in b's workspace: %v", err)
	}
}

func TestAllowList(t *testing.T) {
	allowed := accounts.AllowList(" ada@example.com, @clawvisor.com ,bad-entry")
	for _, c := range []struct {
		email, hd string
		want      bool
	}{
		{"ada@example.com", "", true},
		{"ADA@example.com", "", true},
		{"eve@example.com", "", false},
		// A domain entry admits accounts the domain's organization manages,
		// not personal accounts that use an address there.
		{"x@clawvisor.com", "clawvisor.com", true},
		{"x@clawvisor.com", "", false},
		{"x@clawvisor.com", "other.com", false},
		{"x@evilclawvisor.com", "evilclawvisor.com", false},
		{"x@clawvisor.com.evil", "clawvisor.com.evil", false},
		{"bad-entry", "", false},
	} {
		if got := allowed(accounts.Identity{Email: c.email, HostedDomain: c.hd}); got != c.want {
			t.Errorf("%s (hd %q): %v", c.email, c.hd, got)
		}
	}
}

// Simultaneous first sign-ins (two tabs) end up as one user with one
// workspace, and none of them fails.
func TestConcurrentFirstSignIn(t *testing.T) {
	d := dbtest.New(t)
	s := &accounts.Store{Pool: d.Pool, Allowed: func(accounts.Identity) bool { return true }}
	var wg sync.WaitGroup
	ids := make([]string, 8)
	for i := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			u, err := s.SignIn(ctx, accounts.Identity{Provider: "google", Subject: "same", Email: "new@example.com"})
			if err != nil {
				t.Error(err)
			}
			ids[i] = u.ID
		}()
	}
	wg.Wait()
	for _, id := range ids {
		if id != ids[0] {
			t.Fatalf("different users: %v", ids)
		}
	}
	if ws, _ := s.Workspaces(ctx, ids[0]); len(ws) != 1 {
		t.Fatalf("workspaces = %+v", ws)
	}
}

// Two first requests for a user's invite at once (two tabs) both get it.
func TestConcurrentFirstInvite(t *testing.T) {
	d := dbtest.New(t)
	s := &accounts.Store{Pool: d.Pool, Allowed: func(accounts.Identity) bool { return true }}
	u, err := s.SignIn(ctx, accounts.Identity{Provider: "google", Subject: "1", Email: "ada@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	for round := 0; round < 5; round++ {
		d.Pool.Exec(ctx, `DELETE FROM invites WHERE created_by = $1`, u.ID)
		var wg sync.WaitGroup
		codes := make([]string, 8)
		for i := range codes {
			wg.Add(1)
			go func() {
				defer wg.Done()
				in, err := s.UserInvite(ctx, u.ID)
				if err != nil {
					t.Error(err)
				}
				codes[i] = in.Code
			}()
		}
		wg.Wait()
		for _, c := range codes {
			if c == "" || c != codes[0] {
				t.Fatalf("round %d: codes %v", round, codes)
			}
		}
	}
}

func TestCleanRef(t *testing.T) {
	for in, want := range map[string]string{
		"quickstart": "quickstart", "QuickStart": "quickstart", "quick_start-2": "quick_start-2", "cli": "cli", "a": "a",
		"": "", "-x": "", "_x": "", "a.b": "", "a b": "", "a@b.example": "", "https://x.example": "", "café": "",
		"x\n": "", "%41": "", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa": "", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	} {
		if got := accounts.CleanRef(in); got != want {
			t.Errorf("CleanRef(%q) = %q, want %q", in, got, want)
		}
	}
}

// SignIn cleans a ref itself: one that isn't a short tag is never stored,
// whoever passes it.
func TestSignInSavesOnlyACleanRef(t *testing.T) {
	d := dbtest.New(t)
	s := &accounts.Store{Pool: d.Pool, Allowed: accounts.AllowList("@example.com")}
	for sub, c := range map[string]struct{ ref, want string }{
		"1": {"Launch-HN", "launch-hn"},
		"2": {"someone@example.com", ""},
		"3": {"", ""},
	} {
		u, err := s.SignIn(ctx, accounts.Identity{Provider: "google", Subject: sub, Email: sub + "@example.com", Ref: c.ref})
		if err != nil {
			t.Fatal(err)
		}
		var got *string
		if err := d.Pool.QueryRow(ctx, `SELECT signup_ref FROM users WHERE id = $1`, u.ID).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if (got == nil) != (c.want == "") || got != nil && *got != c.want {
			t.Errorf("ref %q saved as %v", c.ref, got)
		}
	}
}
