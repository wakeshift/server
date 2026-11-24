package orgs

// Organization invites: owners and admins invite an email address with a
// role. Wakeshift keeps the SHA-256 of a random 32-byte token, which the
// invite link carries in its fragment (https://…/join#<token>), so it
// never reaches a server log or a Referer. An invite is bound to one
// address, single use, expires after 7 days and can be revoked; resending
// replaces its token.
//
// Accepting needs a signed-in user whose verified email is the invite's
// exactly, and the invite's creator must still be allowed to send it (a
// member whose role may grant the invite's role). Signing up through an
// invite (accounts.SignIn) needs the same address and spends one of the
// inviter's sign-ups.
//
// Log entries carry invite IDs and masked addresses, never tokens.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5"
)

// Invite limits.
const (
	InviteTTL = 7 * 24 * time.Hour
	// MaxPendingInvites is how many invites an organization may have
	// waiting (expired ones don't count).
	MaxPendingInvites = 50
	// MaxEmailsPerHour is how many invite emails (sent or resent) an
	// organization may send an hour, and MaxEmailsPerAddress how many one
	// address may get a day, from every organization: the console can't
	// be used to send mail at volume.
	MaxEmailsPerHour    = 20
	MaxEmailsPerAddress = 5
)

var (
	// ErrInviteInvalid: no such invite, or it was used, revoked or has
	// expired (404 for the join page, alike).
	ErrInviteInvalid = errors.New("orgs: this invite isn't valid any more; ask for a new one")
	// ErrInviterCant: whoever sent the invite has since been removed or
	// lost the role to send it.
	ErrInviterCant = errors.New("orgs: whoever invited you can't any more; ask for a new invite")
	// ErrAlreadyMember: the address (or the user) is a member already.
	ErrAlreadyMember = errors.New("orgs: already a member")
	// ErrInvitePending: the address has an invite that's still valid.
	ErrInvitePending = errors.New("orgs: that address is invited already; resend the invite instead")
	// ErrTooManyInvites: MaxPendingInvites are waiting already.
	ErrTooManyInvites = fmt.Errorf("orgs: at most %d invites can wait at once", MaxPendingInvites)
	// ErrRateLimited: too many invite emails for now.
	ErrRateLimited = errors.New("orgs: too many invite emails for now; try again later")
	// ErrTooManyMembers: the organization has MaxMembers already.
	ErrTooManyMembers = fmt.Errorf("orgs: an organization has at most %d members", MaxMembers)
)

// WrongAccountError: the signed-in user isn't the invited address. Masked
// is the invited address with most of it hidden (a•••@example.com), shown
// only to someone holding the token.
type WrongAccountError struct{ Masked string }

func (e *WrongAccountError) Error() string {
	return "orgs: this invite is for " + e.Masked + "; sign in with that account"
}

// InviteRefused says whether err is an invite refused for the person
// holding it (invalid, for another address, or its sender can't any more),
// rather than a failure.
func InviteRefused(err error) bool {
	var wrong *WrongAccountError
	return errors.Is(err, ErrInviteInvalid) || errors.Is(err, ErrInviterCant) || errors.As(err, &wrong)
}

// Mask hides most of an email address: ada@example.com is a•••@example.com.
func Mask(email string) string {
	local, domain, ok := strings.Cut(email, "@")
	if !ok || local == "" {
		return "•••"
	}
	return string([]rune(local)[:1]) + "•••@" + domain
}

// normalizeEmail trims and lowercases an address, or says what's wrong.
func normalizeEmail(s string) (string, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	at := strings.LastIndex(s, "@")
	if len(s) > 254 || at < 1 || at == len(s)-1 || strings.ContainsFunc(s, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) ||
		strings.ContainsAny(s, "<>,;\"") {
		return "", &ValidationError{"enter one email address"}
	}
	return s, nil
}

// newToken is an invite token (32 random bytes, base64url) and the hash
// that's kept.
func newToken() (string, []byte, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", nil, err
	}
	t := base64.RawURLEncoding.EncodeToString(b)
	return t, hashToken(t), nil
}

func hashToken(t string) []byte {
	sum := sha256.Sum256([]byte(strings.TrimSpace(t)))
	return sum[:]
}

// Invite is a pending invite, as owners and admins see it.
type Invite struct {
	ID        string    `json:"id"`
	Email     string    `json:"email"`
	Role      Role      `json:"role"`
	InvitedBy string    `json:"invited_by"` // their email
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
	Expired   bool      `json:"expired"`
}

// Sent is an invite just sent or resent: the token goes in the email and,
// once, to the console (never stored or logged).
type Sent struct {
	Invite  Invite
	Token   string
	OrgName string
	// From is who sent it: their name, else their email.
	From string
}

const inviteColumns = `i.id::text, i.email, i.role, coalesce(u.email, ''), i.created_at, i.expires_at, i.expires_at <= now()`

func scanInvite(row pgx.Row) (Invite, error) {
	var in Invite
	err := row.Scan(&in.ID, &in.Email, &in.Role, &in.InvitedBy, &in.CreatedAt, &in.ExpiresAt, &in.Expired)
	if errors.Is(err, pgx.ErrNoRows) {
		return in, ErrInviteInvalid
	}
	return in, err
}

// Invites lists org's pending invites (expired ones too, marked), newest
// first, for owners and admins.
func Invites(ctx context.Context, tx pgx.Tx, user, org string) ([]Invite, error) {
	if _, err := Check(ctx, tx, user, org, OrgInvitesRead); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `SELECT `+inviteColumns+` FROM organization_invites i LEFT JOIN users u ON u.id = i.created_by
		WHERE i.org_id = $1 AND i.accepted_at IS NULL AND i.revoked_at IS NULL ORDER BY i.created_at DESC, i.id`, org)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Invite, error) { return scanInvite(row) })
	if out == nil {
		out = []Invite{}
	}
	return out, err
}

// checkRate refuses another invite email from org to email beyond the
// limits (counting sent and resent invites in org_log).
func checkRate(ctx context.Context, tx pgx.Tx, org, email string) error {
	// The address's count spans organizations, whose own locks don't
	// serialize each other: one address's emails go one at a time. (Always
	// after the organization's row, so the order is the same everywhere.)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('orgs.invite_email:' || $1))`, email); err != nil {
		return err
	}
	var byOrg, byAddress int
	if err := tx.QueryRow(ctx, `SELECT
			(SELECT count(*) FROM org_log WHERE org_id = $1 AND action IN ($3, $4) AND at > now() - interval '1 hour'),
			(SELECT count(*) FROM org_log l JOIN organization_invites i ON l.detail->>'invite_id' = i.id::text
				WHERE i.email = $2 AND l.detail ? 'invite_id' AND l.action IN ($3, $4) AND l.at > now() - interval '1 day')`,
		org, email, LogInviteSent, LogInviteResent).Scan(&byOrg, &byAddress); err != nil {
		return err
	}
	if byOrg >= MaxEmailsPerHour || byAddress >= MaxEmailsPerAddress {
		return ErrRateLimited
	}
	return nil
}

// sender is who's sending, as the email names them, and the
// organization's name.
func sender(ctx context.Context, tx pgx.Tx, user, org string) (from, orgName string, err error) {
	err = tx.QueryRow(ctx, `SELECT coalesce(nullif(btrim(u.name), ''), u.email), o.name FROM users u, organizations o WHERE u.id = $1 AND o.id = $2`,
		user, org).Scan(&from, &orgName)
	return from, orgName, err
}

// CreateInvite invites email to org with role, as user: owners invite any
// role, admins only developers. Inviting an address whose earlier invite
// expired revokes that one in the same transaction. The answer is the same
// whether or not the address has a Wakeshift account.
func CreateInvite(ctx context.Context, tx pgx.Tx, user, org, email string, role Role) (Sent, error) {
	actor, err := lock(ctx, tx, user, org)
	if err != nil {
		return Sent{}, err
	}
	if !actor.Can(OrgInvite) {
		return Sent{}, ErrForbidden
	}
	if !role.Valid() {
		return Sent{}, &ValidationError{"role is owner, admin or developer"}
	}
	if !actor.CanGrant(role) {
		return Sent{}, ErrForbidden
	}
	email, err = normalizeEmail(email)
	if err != nil {
		return Sent{}, err
	}
	var member bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM organization_members m JOIN users u ON u.id = m.user_id WHERE m.org_id = $1 AND u.email = $2)`,
		org, email).Scan(&member); err != nil {
		return Sent{}, err
	}
	if member {
		return Sent{}, ErrAlreadyMember
	}
	// An expired invite still holds the address's slot (an index can't
	// compare with now()): it goes now. One still valid is resent instead.
	expired, err := tx.Query(ctx, `UPDATE organization_invites SET revoked_at = now()
		WHERE org_id = $1 AND email = $2 AND accepted_at IS NULL AND revoked_at IS NULL AND expires_at <= now() RETURNING id::text`, org, email)
	if err != nil {
		return Sent{}, err
	}
	gone, err := pgx.CollectRows(expired, pgx.RowTo[string])
	if err != nil {
		return Sent{}, err
	}
	for _, id := range gone {
		if err := Log(ctx, tx, Entry{Org: org, Actor: UserActor(user), Action: LogInviteRevoked,
			Detail: map[string]any{"invite_id": id, "email": Mask(email), "expired": true}}); err != nil {
			return Sent{}, err
		}
	}
	var pending int
	var held bool
	if err := tx.QueryRow(ctx, `SELECT count(*) FILTER (WHERE expires_at > now()), count(*) FILTER (WHERE email = $2) > 0
		FROM organization_invites WHERE org_id = $1 AND accepted_at IS NULL AND revoked_at IS NULL`, org, email).Scan(&pending, &held); err != nil {
		return Sent{}, err
	}
	switch {
	case held:
		return Sent{}, ErrInvitePending
	case pending >= MaxPendingInvites:
		return Sent{}, ErrTooManyInvites
	}
	if err := checkRate(ctx, tx, org, email); err != nil {
		return Sent{}, err
	}
	token, sum, err := newToken()
	if err != nil {
		return Sent{}, err
	}
	var id string
	if err := tx.QueryRow(ctx, `INSERT INTO organization_invites (org_id, email, role, token_sha256, created_by, expires_at)
		VALUES ($1, $2, $3, $4, $5, now() + make_interval(secs => $6)) RETURNING id::text`,
		org, email, role, sum, user, InviteTTL.Seconds()).Scan(&id); err != nil {
		return Sent{}, err
	}
	if err := Log(ctx, tx, Entry{Org: org, Actor: UserActor(user), Action: LogInviteSent,
		Detail: map[string]any{"invite_id": id, "email": Mask(email), "role": role}}); err != nil {
		return Sent{}, err
	}
	return sent(ctx, tx, user, org, id, token)
}

func sent(ctx context.Context, tx pgx.Tx, user, org, id, token string) (Sent, error) {
	in, err := scanInvite(tx.QueryRow(ctx, `SELECT `+inviteColumns+` FROM organization_invites i LEFT JOIN users u ON u.id = i.created_by WHERE i.id = $1`, id))
	if err != nil {
		return Sent{}, err
	}
	from, name, err := sender(ctx, tx, user, org)
	return Sent{Invite: in, Token: token, OrgName: name, From: from}, err
}

// managedInvite is org's pending invite id, if user may manage it: owners
// any, admins only invites for developers.
func managedInvite(ctx context.Context, tx pgx.Tx, user, org, id string) (Invite, error) {
	actor, err := lock(ctx, tx, user, org)
	if err != nil {
		return Invite{}, err
	}
	if !actor.Can(OrgInvite) {
		return Invite{}, ErrForbidden
	}
	if !validID(id) {
		return Invite{}, ErrInviteInvalid
	}
	in, err := scanInvite(tx.QueryRow(ctx, `SELECT `+inviteColumns+` FROM organization_invites i LEFT JOIN users u ON u.id = i.created_by
		WHERE i.id = $1 AND i.org_id = $2 AND i.accepted_at IS NULL AND i.revoked_at IS NULL`, id, org))
	if err != nil {
		return in, err
	}
	if !actor.CanManage(in.Role) {
		return in, ErrForbidden
	}
	return in, nil
}

// RevokeInvite revokes org's pending invite id, as user: its link stops
// working at once.
func RevokeInvite(ctx context.Context, tx pgx.Tx, user, org, id string) error {
	in, err := managedInvite(ctx, tx, user, org, id)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE organization_invites SET revoked_at = now() WHERE id = $1`, id); err != nil {
		return err
	}
	return Log(ctx, tx, Entry{Org: org, Actor: UserActor(user), Action: LogInviteRevoked, Detail: map[string]any{"invite_id": id, "email": Mask(in.Email)}})
}

// ResendInvite gives org's pending invite id a new token and seven more
// days, as user, for another email: the old link stops working.
func ResendInvite(ctx context.Context, tx pgx.Tx, user, org, id string) (Sent, error) {
	in, err := managedInvite(ctx, tx, user, org, id)
	if err != nil {
		return Sent{}, err
	}
	if err := checkRate(ctx, tx, org, in.Email); err != nil {
		return Sent{}, err
	}
	token, sum, err := newToken()
	if err != nil {
		return Sent{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE organization_invites SET token_sha256 = $2, expires_at = now() + make_interval(secs => $3) WHERE id = $1`,
		id, sum, InviteTTL.Seconds()); err != nil {
		return Sent{}, err
	}
	if err := Log(ctx, tx, Entry{Org: org, Actor: UserActor(user), Action: LogInviteResent, Detail: map[string]any{"invite_id": id, "email": Mask(in.Email)}}); err != nil {
		return Sent{}, err
	}
	return sent(ctx, tx, user, org, id, token)
}

// Preview is what the join page shows someone holding a token: the
// organization, who invited them and the role. Nothing else.
type Preview struct {
	OrgName   string `json:"org_name"`
	InvitedBy string `json:"invited_by"` // their name, else their email
	Role      Role   `json:"role"`
}

// PreviewInvite is the preview for token, or ErrInviteInvalid.
func PreviewInvite(ctx context.Context, tx pgx.Tx, token string) (Preview, error) {
	var p Preview
	err := tx.QueryRow(ctx, `SELECT o.name, coalesce(nullif(btrim(u.name), ''), u.email), i.role
		FROM organization_invites i JOIN organizations o ON o.id = i.org_id AND o.deleted_at IS NULL JOIN users u ON u.id = i.created_by
		WHERE i.token_sha256 = $1 AND i.accepted_at IS NULL AND i.revoked_at IS NULL AND i.expires_at > now()`, hashToken(token)).Scan(&p.OrgName, &p.InvitedBy, &p.Role)
	if errors.Is(err, pgx.ErrNoRows) {
		return p, ErrInviteInvalid
	}
	return p, err
}

// Pending is a valid invite, locked, that the user with its address may
// accept (Join).
type Pending struct {
	ID, Org, Email, CreatedBy string
	Role                      Role
}

// PendingFor is the invite for token, locked until tx ends, if it's still
// valid, it's for email (the signed-in or signing-up user's verified
// address), and its creator may still send it. A refused one changes
// nothing.
func PendingFor(ctx context.Context, tx pgx.Tx, token, email string) (Pending, error) {
	var p Pending
	sum := hashToken(token)
	// The organization's row first, as every member and invite change
	// locks it, then the invite's.
	err := tx.QueryRow(ctx, `SELECT org_id::text FROM organization_invites WHERE token_sha256 = $1`, sum).Scan(&p.Org)
	if errors.Is(err, pgx.ErrNoRows) {
		return p, ErrInviteInvalid
	}
	if err != nil {
		return p, err
	}
	var deleted bool
	if err := tx.QueryRow(ctx, `SELECT deleted_at IS NOT NULL FROM organizations WHERE id = $1 FOR UPDATE`, p.Org).Scan(&deleted); err != nil {
		return p, err
	}
	err = tx.QueryRow(ctx, `SELECT id::text, email, created_by::text, role FROM organization_invites
		WHERE token_sha256 = $1 AND org_id = $2 AND accepted_at IS NULL AND revoked_at IS NULL AND expires_at > now()
		FOR UPDATE`, sum, p.Org).Scan(&p.ID, &p.Email, &p.CreatedBy, &p.Role)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && deleted) {
		return p, ErrInviteInvalid
	}
	if err != nil {
		return p, err
	}
	if strings.ToLower(strings.TrimSpace(email)) != p.Email {
		return p, &WrongAccountError{Masked: Mask(p.Email)}
	}
	inviter, err := RoleIn(ctx, tx, p.CreatedBy, p.Org, false)
	if errors.Is(err, ErrNotMember) || (err == nil && !(inviter.Can(OrgInvite) && inviter.CanGrant(p.Role))) {
		return p, ErrInviterCant
	}
	return p, err
}

// Join makes user a member through invite p (from PendingFor, in the same
// transaction), spending it, and writes org.invite_accepted and
// org.member_joined.
func Join(ctx context.Context, tx pgx.Tx, p Pending, user string) error {
	var member bool
	var members int
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM organization_members WHERE org_id = $1 AND user_id = $2),
		(SELECT count(*) FROM organization_members WHERE org_id = $1)`, p.Org, user).Scan(&member, &members); err != nil {
		return err
	}
	switch {
	case member:
		return ErrAlreadyMember
	case members >= MaxMembers:
		return ErrTooManyMembers
	}
	tag, err := tx.Exec(ctx, `UPDATE organization_invites SET accepted_at = now(), accepted_by = $2
		WHERE id = $1 AND accepted_at IS NULL AND revoked_at IS NULL AND expires_at > now()`, p.ID, user)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrInviteInvalid
	}
	if _, err := tx.Exec(ctx, `INSERT INTO organization_members (org_id, user_id, role, invited_by) VALUES ($1, $2, $3, $4)`,
		p.Org, user, p.Role, p.CreatedBy); err != nil {
		return err
	}
	if err := Log(ctx, tx, Entry{Org: p.Org, Actor: UserActor(user), Action: LogInviteAccepted,
		Detail: map[string]any{"invite_id": p.ID, "email": Mask(p.Email)}}); err != nil {
		return err
	}
	return Log(ctx, tx, Entry{Org: p.Org, Actor: UserActor(user), Action: LogMemberJoined,
		Detail: map[string]any{"user_id": user, "role": p.Role, "invite_id": p.ID, "invited_by": p.CreatedBy}})
}

// Accept accepts the invite for token as user, whose verified email it
// must be: one transaction, so a refused accept changes nothing. It
// returns the organization's ID.
func Accept(ctx context.Context, tx pgx.Tx, user, token string) (string, error) {
	var email string
	if err := tx.QueryRow(ctx, `SELECT email FROM users WHERE id = $1`, user).Scan(&email); err != nil {
		return "", err
	}
	p, err := PendingFor(ctx, tx, token, email)
	if errors.Is(err, ErrInviteInvalid) {
		// Accepted already, by this user (signing up through it joins at
		// once, and the join page then accepts again): done.
		var org string
		if e := tx.QueryRow(ctx, `SELECT i.org_id::text FROM organization_invites i JOIN organization_members m ON m.org_id = i.org_id AND m.user_id = $2
			WHERE i.token_sha256 = $1 AND i.accepted_by = $2`, hashToken(token), user).Scan(&org); e == nil {
			return org, nil
		}
	}
	if err != nil {
		return "", err
	}
	return p.Org, Join(ctx, tx, p, user)
}
