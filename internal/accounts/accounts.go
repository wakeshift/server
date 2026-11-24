// Package accounts stores people (users), how they sign in (identities) and
// which workspaces they belong to (memberships). A workspace is a tenant.
package accounts

import (
	"context"
	"crypto/rand"
	"errors"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/wakeshift/server/internal/audit"
	"github.com/wakeshift/server/internal/orgs"
)

// TermsVersion is the version of Wakeshift's own Terms of Service, which a
// new account on its hosted service accepts: their "Last updated" date,
// recorded on users with the time (Store.TermsVersion; internal/config's
// default there). Change it with the Terms; TestTermsVersion fails until it
// matches.
const TermsVersion = "2026-11-01"

// User is a person who signs in.
type User struct {
	ID    string `json:"id"`
	Email string `json:"email"`
	Name  string `json:"name"`
	// SessionEpoch is the user's session generation: sessions from an
	// earlier one are no longer valid (see SignOutEverywhere).
	SessionEpoch int `json:"-"`
}

// Workspace is a tenant a user belongs to.
type Workspace struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Kind string `json:"kind"` // personal or team
	Role string `json:"role"` // the user's: owner or member
}

// Identity is what a sign-in provider vouches for. Email must be verified
// by the provider. HostedDomain is the organization that manages the
// account (Google's "hd" claim), empty for personal accounts.
type Identity struct {
	Provider, Subject, Email, Name, HostedDomain string
	// Invite is the invite code a new identity signs up with, if any: it
	// makes an account whatever the sign-up cap.
	Invite string
	// OrgInvite is the token of an organization invite a new identity
	// signs up with: admitted only if it's for this identity's verified
	// email, spending one of the inviter's sign-ups, and joining the
	// organization in the sign-up's transaction. Ignored for an existing
	// user, who accepts on the join page.
	OrgInvite string
	// Ref is where a new identity's sign-in link came from (?ref=quickstart on
	// a link straight to the console), as CleanRef left it: saved on the
	// account it makes, or on its waitlist entry, and ignored for an
	// existing user. Never logged, and never sent to analytics.
	Ref string
}

// refRE is a ref: a short lowercase tag, such as quickstart or cli.
var refRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

// CleanRef is a sign-in link's ref, lowercased, or "" if it isn't a short
// tag (refRE): anything else is dropped, never stored.
func CleanRef(ref string) string {
	ref = strings.ToLower(ref)
	if !refRE.MatchString(ref) {
		return ""
	}
	return ref
}

var (
	ErrNotMember = errors.New("accounts: not a member of this workspace")
	// ErrAccountExists means another sign-in method already owns this
	// email. Identities are never linked by email alone: a different
	// account with the same address (a personal Google account made with a
	// work address, a re-registered domain) must not become that user.
	ErrAccountExists = errors.New("accounts: this email belongs to an account that signs in another way")
)

// Store reads and writes accounts. They aren't tenant-scoped, so it uses
// the pool directly.
type Store struct {
	Pool *pgxpool.Pool
	// Allowed admits a new identity whatever the sign-up cap (existing
	// users can always sign in). Nil admits nobody past the cap.
	Allowed func(Identity) bool
	// InvitesPerUser is how many people each user can invite (default
	// DefaultInvites).
	InvitesPerUser int
	// TermsVersion is the Terms of Service a new account accepts, recorded
	// on users with the time (WAKESHIFT_TERMS_VERSION; TermsVersion on
	// Wakeshift's hosted service). Empty: the server has no Terms, and
	// neither is recorded.
	TermsVersion string
}

// DefaultInvites is how many sign-ups a user's invite code is good for.
const DefaultInvites = 5

// SignIn returns the user an identity belongs to, creating the user (and a
// personal workspace) on first sign-in. A new identity whose email belongs
// to an existing user is refused (ErrAccountExists), never linked. A new
// identity with no invite, past the sign-up cap and not invited off the
// waitlist, is put on the waitlist with no account (*WaitlistedError).
func (s *Store) SignIn(ctx context.Context, id Identity) (User, error) {
	email := strings.ToLower(strings.TrimSpace(id.Email))
	if id.Provider == "" || id.Subject == "" || email == "" {
		return User{}, errors.New("accounts: incomplete identity")
	}
	// Two first sign-ins at once (two tabs finishing the callback) can both
	// see no user and both insert one; the loser hits the unique email or
	// identity. Its retry finds the winner's user and links to it.
	for attempt := 0; ; attempt++ {
		u, err := s.signIn(ctx, id, email)
		var pgErr *pgconn.PgError
		if attempt < 2 && errors.As(err, &pgErr) && pgErr.Code == "23505" {
			continue
		}
		return u, err
	}
}

func (s *Store) signIn(ctx context.Context, id Identity, email string) (User, error) {
	var u User
	var waitlisted *WaitlistedError
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		u, waitlisted = User{}, nil
		// Known identity.
		err := tx.QueryRow(ctx, `UPDATE users SET last_sign_in_at = now() FROM identities i
			WHERE i.provider = $1 AND i.subject = $2 AND users.id = i.user_id
			RETURNING users.id, users.email, users.name, users.session_epoch`, id.Provider, id.Subject).Scan(&u.ID, &u.Email, &u.Name, &u.SessionEpoch)
		if err == nil || !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		// A new identity: a new user, unless the email is taken. One query
		// checks both the email's owner and whether this very identity is
		// theirs, since a concurrent first sign-in may have just committed
		// both (each statement sees newly committed rows).
		var ownerID string
		var ours bool
		err = tx.QueryRow(ctx, `SELECT u.id, i.user_id IS NOT NULL FROM users u
			LEFT JOIN identities i ON i.provider = $2 AND i.subject = $3 AND i.user_id = u.id
			WHERE u.email = $1`, email, id.Provider, id.Subject).Scan(&ownerID, &ours)
		switch {
		case err == nil && ours:
			return tx.QueryRow(ctx, `UPDATE users SET last_sign_in_at = now() WHERE id = $1 RETURNING id, email, name, session_epoch`, ownerID).
				Scan(&u.ID, &u.Email, &u.Name, &u.SessionEpoch)
		case err == nil:
			return ErrAccountExists
		case !errors.Is(err, pgx.ErrNoRows):
			return err
		}
		// An organization invite for this very address (verified by the
		// provider), locked until the sign-up commits.
		var orgInvite *orgs.Pending
		var orgInviteErr error
		if id.OrgInvite != "" {
			p, err := orgs.PendingFor(ctx, tx, id.OrgInvite, email)
			if err == nil {
				orgInvite = &p
			} else if !orgs.InviteRefused(err) {
				return err
			}
			orgInviteErr = err
		}
		var invitedBy *string
		allowed := s.Allowed != nil && s.Allowed(Identity{Provider: id.Provider, Subject: id.Subject, Email: email, Name: id.Name, HostedDomain: id.HostedDomain})
		if !allowed && id.Invite != "" {
			// Spending a use, in the sign-up's transaction: a failed sign-up
			// doesn't spend it, and two at once can't both take the last.
			// An invite that's used up or unknown is no invite: the sign-up
			// goes on as one without (the sign-in page said so).
			err := tx.QueryRow(ctx, `UPDATE invites SET uses_left = uses_left - 1 WHERE code = $1 AND uses_left > 0
				RETURNING created_by`, normalizeCode(id.Invite)).Scan(&invitedBy)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
		}
		detail := map[string]string{}
		switch {
		case allowed:
			detail = nil // the operator's list (and development sign-in): no audit entry
		case invitedBy != nil:
			detail["via"], detail["invited_by"] = "invite", *invitedBy
		case orgInvite != nil:
			// Signing up through an organization invite spends one of the
			// inviter's sign-ups, as their invite code would: otherwise
			// anyone could make an organization and invite unlimited
			// people past the sign-up cap.
			if err := s.spendSignUp(ctx, tx, orgInvite.CreatedBy); err != nil {
				return err
			}
			invitedBy = &orgInvite.CreatedBy
			detail = map[string]string{"via": "org_invite", "org_id": orgInvite.Org, "invited_by": orgInvite.CreatedBy, "invite_id": orgInvite.ID}
		case orgInviteErr != nil:
			return orgInviteErr
		default:
			// Under the sign-up cap, or invited off the waitlist; else onto
			// the waitlist, committed with no account.
			via, entry, err := admit(ctx, tx, id, email)
			if err != nil {
				return err
			}
			if via == "" {
				waitlisted = &WaitlistedError{Entry: entry}
				return nil
			}
			detail["via"] = via
			if entry != "" {
				detail["waitlist_entry"] = entry
			}
		}
		// Creating the account accepts the Terms the sign-in page names
		// (Terms §2), if the server has any: recorded with the version, for
		// every sign-up. Its ref is the one its waitlist entry kept, if any
		// (where they first came from), else this sign-in's.
		if err := tx.QueryRow(ctx, `INSERT INTO users (email, name, last_sign_in_at, invited_by, terms_version, terms_accepted_at, signup_ref)
			VALUES ($1, $2, now(), $3, $4::text, CASE WHEN $4::text IS NULL THEN NULL ELSE now() END, coalesce(
				(SELECT signup_ref FROM waitlist WHERE ((provider = $6 AND subject = $7) OR email = $1) AND signed_up_at IS NULL AND signup_ref IS NOT NULL
				 ORDER BY created_at LIMIT 1),
				$5))
			RETURNING id, email, name`, email, id.Name, invitedBy, nullable(s.TermsVersion), nullable(CleanRef(id.Ref)), id.Provider, id.Subject).Scan(&u.ID, &u.Email, &u.Name); err != nil {
			return err
		}
		workspace, err := createWorkspace(ctx, tx, u.ID, personalName(u), "personal")
		if err != nil {
			return err
		}
		// And their personal organization, for the apps they build.
		if _, err := orgs.CreatePersonal(ctx, tx, u.ID, u.Name, u.Email); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO identities (provider, subject, user_id, email) VALUES ($1, $2, $3, $4)`,
			id.Provider, id.Subject, u.ID, email); err != nil {
			return err
		}
		// A waitlist entry for them, however they got in, now has its
		// account (and goes after WaitlistRetention).
		if err := signedUp(ctx, tx, id, email, u.ID); err != nil {
			return err
		}
		if orgInvite != nil {
			// The organization they were invited to, in the sign-up's
			// transaction.
			if err := orgs.Join(ctx, tx, *orgInvite, u.ID); err != nil {
				return err
			}
		}
		if detail == nil {
			return nil
		}
		// Signing up is recorded in their own workspace, in the sign-up's
		// transaction, with how they got in: under the cap ("cap"), an
		// invite link, an organization invite or off the waitlist.
		if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, workspace); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{Actor: audit.User(u.ID), Action: audit.AccountCreated, Detail: detail})
	})
	if err == nil && waitlisted != nil {
		return User{}, waitlisted
	}
	return u, err
}

// ErrNoSignUpsLeft: whoever sent the organization invite has no sign-ups
// left to spend on a new account. The invite stays valid for an existing
// account.
var ErrNoSignUpsLeft = errors.New("accounts: whoever invited you has no sign-ups left; ask them to contact us")

// spendSignUp spends one of user's sign-ups (their invite code's uses,
// made now if they haven't one yet), in the sign-up's transaction.
func (s *Store) spendSignUp(ctx context.Context, tx pgx.Tx, user string) error {
	if _, err := tx.Exec(ctx, `INSERT INTO invites (code, created_by, uses_left) VALUES ($1, $2, $3) ON CONFLICT (created_by) DO NOTHING`,
		newCode(), user, s.invitesPerUser()); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `UPDATE invites SET uses_left = uses_left - 1 WHERE created_by = $1 AND uses_left > 0`, user)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNoSignUpsLeft
	}
	return nil
}

func (s *Store) invitesPerUser() int {
	if s.InvitesPerUser <= 0 {
		return DefaultInvites
	}
	return s.InvitesPerUser
}

// nullable is s, or NULL if it's empty.
func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func personalName(u User) string {
	if u.Name != "" {
		return u.Name
	}
	return u.Email
}

func createWorkspace(ctx context.Context, tx pgx.Tx, ownerID, name, kind string) (string, error) {
	var id string
	if err := tx.QueryRow(ctx, `INSERT INTO tenants (name, kind) VALUES ($1, $2) RETURNING id::text`, name, kind).Scan(&id); err != nil {
		return "", err
	}
	_, err := tx.Exec(ctx, `INSERT INTO memberships (tenant_id, user_id, role) VALUES ($1, $2, 'owner')`, id, ownerID)
	return id, err
}

// Invite is an invite code and how many more sign-ups it's good for.
type Invite struct {
	Code     string `json:"code"`
	UsesLeft int    `json:"uses_left"`
}

// UserInvite returns the user's invite code, making it on first use.
func (s *Store) UserInvite(ctx context.Context, userID string) (Invite, error) {
	n := s.InvitesPerUser
	if n <= 0 {
		n = DefaultInvites
	}
	var in Invite
	err := s.Pool.QueryRow(ctx, `WITH made AS (
			INSERT INTO invites (code, created_by, uses_left) VALUES ($1, $2, $3) ON CONFLICT (created_by) DO NOTHING
			RETURNING code, uses_left)
		SELECT code, uses_left FROM made UNION ALL SELECT code, uses_left FROM invites WHERE created_by = $2`,
		newCode(), userID, n).Scan(&in.Code, &in.UsesLeft)
	if errors.Is(err, pgx.ErrNoRows) {
		// Another request made it at the same time: the insert waited for
		// it and did nothing, and the select's snapshot predates it.
		err = s.Pool.QueryRow(ctx, `SELECT code, uses_left FROM invites WHERE created_by = $1`, userID).Scan(&in.Code, &in.UsesLeft)
	}
	return in, err
}

// CheckInvite reports whether a code can still be used.
func (s *Store) CheckInvite(ctx context.Context, code string) (bool, error) {
	var ok bool
	err := s.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM invites WHERE code = $1 AND uses_left > 0)`, normalizeCode(code)).Scan(&ok)
	return ok, err
}

// newCode is an invite code: 12 characters of lowercase base32 without
// look-alikes (60 bits), e.g. "k3f9x2abq7mn".
func newCode() string {
	const alphabet = "abcdefghjkmnpqrstuvwxyz23456789"
	b := make([]byte, 12)
	rand.Read(b)
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b)
}

func normalizeCode(c string) string {
	return strings.ToLower(strings.ReplaceAll(strings.TrimSpace(c), "-", ""))
}

// Workspaces lists a user's workspaces, personal first.
func (s *Store) Workspaces(ctx context.Context, userID string) ([]Workspace, error) {
	rows, _ := s.Pool.Query(ctx, `SELECT t.id::text, t.name, t.kind, m.role FROM memberships m JOIN tenants t ON t.id = m.tenant_id
		WHERE m.user_id = $1 ORDER BY t.kind = 'personal' DESC, t.created_at, t.id`, userID)
	return pgx.CollectRows(rows, pgx.RowToStructByPos[Workspace])
}

// Member returns the user and their role in a workspace, or ErrNotMember.
func (s *Store) Member(ctx context.Context, userID, tenantID string) (User, Workspace, error) {
	var u User
	var w Workspace
	err := s.Pool.QueryRow(ctx, `SELECT u.id, u.email, u.name, u.session_epoch, t.id::text, t.name, t.kind, m.role
		FROM memberships m JOIN users u ON u.id = m.user_id JOIN tenants t ON t.id = m.tenant_id
		WHERE m.user_id = $1 AND m.tenant_id = $2`, userID, tenantID).Scan(&u.ID, &u.Email, &u.Name, &u.SessionEpoch, &w.ID, &w.Name, &w.Kind, &w.Role)
	if errors.Is(err, pgx.ErrNoRows) {
		return u, w, ErrNotMember
	}
	return u, w, err
}

// SignOutEverywhere ends all of a user's sessions.
func (s *Store) SignOutEverywhere(ctx context.Context, userID string) error {
	_, err := s.Pool.Exec(ctx, `UPDATE users SET session_epoch = session_epoch + 1 WHERE id = $1`, userID)
	return err
}

// AllowList returns an Allowed function for a comma-separated list of
// emails and "@domain" entries. A domain entry only admits accounts that
// domain's organization manages (the identity's HostedDomain), not personal
// accounts that happen to use an address there.
func AllowList(list string) func(Identity) bool {
	emails, domains := map[string]bool{}, map[string]bool{}
	for _, e := range strings.Split(list, ",") {
		e = strings.ToLower(strings.TrimSpace(e))
		switch {
		case strings.HasPrefix(e, "@") && len(e) > 1:
			domains[e] = true
		case strings.Contains(e, "@"):
			emails[e] = true
		}
	}
	return func(id Identity) bool {
		email := strings.ToLower(id.Email)
		at := strings.LastIndex(email, "@")
		if emails[email] {
			return true
		}
		return at > 0 && domains[email[at:]] && strings.EqualFold(id.HostedDomain, email[at+1:])
	}
}
