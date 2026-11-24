package accounts_test

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/accounts"
	"github.com/wakeshift/server/internal/dbtest"
	"github.com/wakeshift/server/internal/orgs"
)

// Signing up through an organization invite: only the invited address,
// spending one of the inviter's sign-ups, joining the organization and
// recording account.created in the new user's workspace,
// all in one transaction.
func TestSignUpThroughAnOrgInvite(t *testing.T) {
	d := dbtest.New(t)
	s := &accounts.Store{Pool: d.Pool, Allowed: accounts.AllowList("ada@example.com"), InvitesPerUser: 2}
	ada, err := s.SignIn(ctx, accounts.Identity{Provider: "google", Subject: "ada", Email: "ada@example.com", Name: "Ada"})
	if err != nil {
		t.Fatal(err)
	}
	var org string
	invite := func(email string) string {
		t.Helper()
		var token string
		err := pgx.BeginFunc(ctx, d.Pool, func(tx pgx.Tx) (err error) {
			if org == "" {
				if org, err = orgs.Default(ctx, tx, ada.ID); err != nil {
					return err
				}
			}
			sent, err := orgs.CreateInvite(ctx, tx, ada.ID, org, email, orgs.Admin)
			token = sent.Token
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		return token
	}
	usesLeft := func() int {
		t.Helper()
		var n int
		if err := d.Pool.QueryRow(ctx, `SELECT coalesce((SELECT uses_left FROM invites WHERE created_by = $1), -1)`, ada.ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	users := func() int {
		var n int
		d.Pool.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&n)
		return n
	}

	cyToken := invite("cy@example.com")
	// Another address can't use it, and spends nothing.
	var wrong *orgs.WrongAccountError
	if _, err := s.SignIn(ctx, accounts.Identity{Provider: "google", Subject: "eve", Email: "eve@example.com", OrgInvite: cyToken}); !errors.As(err, &wrong) || wrong.Masked != "c•••@example.com" {
		t.Fatalf("eve with cy's invite: %v", err)
	}
	if _, err := s.SignIn(ctx, accounts.Identity{Provider: "google", Subject: "eve", Email: "eve@example.com", OrgInvite: "not-a-token"}); !errors.Is(err, orgs.ErrInviteInvalid) {
		t.Fatalf("eve with a made-up token: %v", err)
	}
	if users() != 1 || usesLeft() != -1 {
		t.Fatalf("after refusals: %d users, %d sign-ups left", users(), usesLeft())
	}

	cy, err := s.SignIn(ctx, accounts.Identity{Provider: "github", Subject: "cy", Email: "Cy@Example.com", Name: "Cy", OrgInvite: cyToken})
	if err != nil {
		t.Fatalf("cy: %v", err)
	}
	if usesLeft() != 1 {
		t.Errorf("ada's sign-ups left: %d, want 1", usesLeft())
	}
	var role, invitedBy string
	if err := d.Pool.QueryRow(ctx, `SELECT m.role, u.invited_by::text FROM organization_members m JOIN users u ON u.id = m.user_id WHERE m.org_id = $1 AND m.user_id = $2`,
		org, cy.ID).Scan(&role, &invitedBy); err != nil || role != "admin" || invitedBy != ada.ID {
		t.Fatalf("cy's membership: %q, invited by %q, %v", role, invitedBy, err)
	}
	// account.created, in cy's own workspace's audit log.
	ws, _ := s.Workspaces(ctx, cy.ID)
	var audited int
	if err := d.InTenant(ctx, ws[0].ID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE action = 'account.created' AND detail->>'via' = 'org_invite' AND detail->>'org_id' = $1`, org).Scan(&audited)
	}); err != nil || audited != 1 {
		t.Fatalf("account.created: %d %v", audited, err)
	}
	var logged int
	d.Pool.QueryRow(ctx, `SELECT count(*) FROM org_log WHERE org_id = $1 AND action IN ('org.member_joined', 'org.invite_accepted')`, org).Scan(&logged)
	if logged != 2 {
		t.Errorf("org log entries for joining: %d", logged)
	}
	// It's spent.
	if _, err := s.SignIn(ctx, accounts.Identity{Provider: "google", Subject: "cy2", Email: "cy@example.org", OrgInvite: cyToken}); !errors.Is(err, orgs.ErrInviteInvalid) {
		t.Fatalf("a spent invite: %v", err)
	}

	// The inviter's last sign-up, then none left: refused, and the invite
	// stays valid (for an existing account).
	if _, err := s.SignIn(ctx, accounts.Identity{Provider: "google", Subject: "dan", Email: "dan@example.com", OrgInvite: invite("dan@example.com")}); err != nil {
		t.Fatalf("dan: %v", err)
	}
	fayToken := invite("fay@example.com")
	if _, err := s.SignIn(ctx, accounts.Identity{Provider: "google", Subject: "fay", Email: "fay@example.com", OrgInvite: fayToken}); !errors.Is(err, accounts.ErrNoSignUpsLeft) {
		t.Fatalf("no sign-ups left: %v", err)
	}
	if usesLeft() != 0 || users() != 3 {
		t.Fatalf("after running out: %d left, %d users", usesLeft(), users())
	}
	if err := pgx.BeginFunc(ctx, d.Pool, func(tx pgx.Tx) error {
		_, err := orgs.PreviewInvite(ctx, tx, fayToken)
		return err
	}); err != nil {
		t.Fatalf("fay's invite after the refusal: %v", err)
	}

	// The allow list still admits people without an invite, and someone it
	// admits who holds an invite joins too, spending nothing.
	s.Allowed = accounts.AllowList("ada@example.com,fay@example.com")
	fay, err := s.SignIn(ctx, accounts.Identity{Provider: "google", Subject: "fay", Email: "fay@example.com", OrgInvite: fayToken})
	if err != nil {
		t.Fatalf("fay on the allow list: %v", err)
	}
	var member bool
	d.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM organization_members WHERE org_id = $1 AND user_id = $2)`, org, fay.ID).Scan(&member)
	if !member || usesLeft() != 0 {
		t.Fatalf("fay: member %v, ada's sign-ups %d", member, usesLeft())
	}
}

// An invite is honored only if its sender could still send it: removed or
// demoted below the invite's role, it's refused.
func TestOrgInviteNeedsItsSender(t *testing.T) {
	d := dbtest.New(t)
	s := &accounts.Store{Pool: d.Pool, Allowed: accounts.AllowList("ada@example.com,bob@example.com")}
	ada, _ := s.SignIn(ctx, accounts.Identity{Provider: "google", Subject: "ada", Email: "ada@example.com"})
	bob, _ := s.SignIn(ctx, accounts.Identity{Provider: "google", Subject: "bob", Email: "bob@example.com"})
	var org, token string
	if err := pgx.BeginFunc(ctx, d.Pool, func(tx pgx.Tx) (err error) {
		if org, err = orgs.Default(ctx, tx, ada.ID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO organization_members (org_id, user_id, role) VALUES ($1, $2, 'admin')`, org, bob.ID); err != nil {
			return err
		}
		sent, err := orgs.CreateInvite(ctx, tx, bob.ID, org, "cy@example.com", orgs.Developer)
		token = sent.Token
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// Bob is demoted to developer: developers can't invite.
	d.Pool.Exec(ctx, `UPDATE organization_members SET role = 'developer' WHERE org_id = $1 AND user_id = $2`, org, bob.ID)
	if _, err := s.SignIn(ctx, accounts.Identity{Provider: "google", Subject: "cy", Email: "cy@example.com", OrgInvite: token}); !errors.Is(err, orgs.ErrInviterCant) {
		t.Fatalf("demoted sender: %v", err)
	}
	// Removed: the same.
	d.Pool.Exec(ctx, `DELETE FROM organization_members WHERE org_id = $1 AND user_id = $2`, org, bob.ID)
	if _, err := s.SignIn(ctx, accounts.Identity{Provider: "google", Subject: "cy", Email: "cy@example.com", OrgInvite: token}); !errors.Is(err, orgs.ErrInviterCant) {
		t.Fatalf("removed sender: %v", err)
	}
}
