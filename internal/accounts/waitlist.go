package accounts

// The sign-up cap and the waitlist. While there are fewer accounts than
// the cap (signup_cap, set from the admin service), anyone who signs in
// with Google or GitHub gets an account. At or past it, a new sign-in
// joins the waitlist instead: no account and no workspace, one entry per
// person however often they sign in. Inviting an entry (from the admin
// service) admits its next sign-in. Invite links, organization invites
// and the allow list admit people whatever the cap; every account counts
// towards it.

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// WaitlistedError is SignIn's answer past the sign-up cap: the identity is
// on the waitlist (Entry, its entry's ID), with no account. The entry is
// committed.
type WaitlistedError struct{ Entry string }

func (e *WaitlistedError) Error() string {
	return "accounts: sign-ups are at their cap; on the waitlist"
}

// Retention (the privacy policy's Section 9): an entry that has its account
// goes WaitlistRetention after the sign-up; one invited and never used goes
// InvitedRetention after the invitation. An entry never invited stays until
// it's invited or removed; removing one deletes it at once.
const (
	WaitlistRetention = 30 * 24 * time.Hour
	InvitedRetention  = 90 * 24 * time.Hour
)

// MaxNote is the longest note on an entry, in characters.
const MaxNote = 500

// maxName is the longest name kept from the provider, in characters.
const maxName = 200

type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// admit decides a new identity's sign-up, with no invite and not on the
// allow list, in its transaction: "waitlist" if an entry for it was invited
// (entry is its ID), "cap" if there are fewer accounts than the cap, or ""
// with the identity put on the waitlist (entry is its entry, new or not).
func admit(ctx context.Context, tx pgx.Tx, id Identity, email string) (via, entry string, err error) {
	var invited bool
	err = tx.QueryRow(ctx, `SELECT id::text, invited_at IS NOT NULL FROM waitlist
		WHERE (provider = $1 AND subject = $2) OR email = $3
		ORDER BY (provider = $1 AND subject = $2) DESC LIMIT 1 FOR UPDATE`, id.Provider, id.Subject, email).Scan(&entry, &invited)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return "", "", err
	}
	if invited {
		return "waitlist", entry, nil
	}
	// The cap's row is locked to the end of the sign-up, so two sign-ups
	// at once can't both take the last place; the count is read after the
	// lock, so it sees every account committed before.
	var limit int
	if err := tx.QueryRow(ctx, `SELECT cap FROM signup_cap FOR UPDATE`).Scan(&limit); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return "", "", err
	}
	var used int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&used); err != nil {
		return "", "", err
	}
	if used < limit {
		return "cap", entry, nil
	}
	ref := nullable(CleanRef(id.Ref))
	if entry != "" {
		// Still waiting: the entry keeps the first ref it was given.
		if ref != nil {
			if _, err := tx.Exec(ctx, `UPDATE waitlist SET signup_ref = $2 WHERE id = $1 AND signup_ref IS NULL`, entry, ref); err != nil {
				return "", "", err
			}
		}
		return "", entry, nil
	}
	err = tx.QueryRow(ctx, `INSERT INTO waitlist (provider, subject, email, name, signup_ref) VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT DO NOTHING RETURNING id::text`, id.Provider, id.Subject, email, clip(oneLine(id.Name), maxName), ref).Scan(&entry)
	if errors.Is(err, pgx.ErrNoRows) {
		// Another sign-in of theirs just added it.
		err = tx.QueryRow(ctx, `SELECT id::text FROM waitlist WHERE (provider = $1 AND subject = $2) OR email = $3 LIMIT 1`,
			id.Provider, id.Subject, email).Scan(&entry)
	}
	return "", entry, err
}

// signedUp records the account on the identity's waitlist entry, if it has
// one, however the account was made.
func signedUp(ctx context.Context, tx pgx.Tx, id Identity, email, user string) error {
	_, err := tx.Exec(ctx, `UPDATE waitlist SET signed_up_at = now(), user_id = $4
		WHERE ((provider = $1 AND subject = $2) OR email = $3) AND signed_up_at IS NULL`, id.Provider, id.Subject, email, user)
	return err
}

// WaitlistEntry is what the waitlist page shows its person.
type WaitlistEntry struct {
	JoinedAt time.Time `json:"joined_at"`
	Note     string    `json:"note"`
	// Invited: their next sign-in makes their account.
	Invited bool `json:"invited"`
}

// ErrNoEntry: no such waitlist entry (removed, or signed up and deleted).
var ErrNoEntry = errors.New("accounts: no such waitlist entry")

// WaitlistEntry returns an entry that hasn't signed up yet.
func (s *Store) WaitlistEntry(ctx context.Context, entry string) (WaitlistEntry, error) {
	var e WaitlistEntry
	err := s.Pool.QueryRow(ctx, `SELECT created_at, coalesce(note, ''), invited_at IS NOT NULL FROM waitlist
		WHERE id = $1 AND signed_up_at IS NULL`, entry).Scan(&e.JoinedAt, &e.Note, &e.Invited)
	if errors.Is(err, pgx.ErrNoRows) {
		return e, ErrNoEntry
	}
	return e, err
}

// ErrBadNote: a note too long, or with control characters other than line
// breaks.
var ErrBadNote = errors.New("accounts: the note is at most 500 characters")

// ValidNote is note as kept: trimmed, at most MaxNote characters, with no
// control characters but line breaks.
func ValidNote(note string) (string, error) {
	note = strings.TrimSpace(strings.ReplaceAll(note, "\r\n", "\n"))
	if !utf8.ValidString(note) || utf8.RuneCountInString(note) > MaxNote ||
		strings.ContainsFunc(note, func(r rune) bool { return r != '\n' && unicode.IsControl(r) }) {
		return "", ErrBadNote
	}
	return note, nil
}

// SetWaitlistNote sets (or, with "", clears) the note on an entry that
// hasn't signed up.
func (s *Store) SetWaitlistNote(ctx context.Context, entry, note string) error {
	note, err := ValidNote(note)
	if err != nil {
		return err
	}
	tag, err := s.Pool.Exec(ctx, `UPDATE waitlist SET note = nullif($2, ''), noted_at = now() WHERE id = $1 AND signed_up_at IS NULL`, entry, note)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNoEntry
	}
	return err
}

// SignupCap returns the cap and the accounts that count towards it (all
// of them).
func SignupCap(ctx context.Context, q querier) (limit, used int, err error) {
	err = q.QueryRow(ctx, `SELECT coalesce((SELECT cap FROM signup_cap), 0), (SELECT count(*) FROM users)`).Scan(&limit, &used)
	return limit, used, err
}

// MaxCap is the highest cap that can be set.
const MaxCap = 10_000_000

// ErrBadCap: a cap below 0 or above MaxCap.
var ErrBadCap = errors.New("accounts: the cap is a whole number from 0 to 10,000,000")

// SetSignupCap sets the cap in tx, returning the one it replaces.
func SetSignupCap(ctx context.Context, tx pgx.Tx, limit int) (old int, err error) {
	if limit < 0 || limit > MaxCap {
		return 0, ErrBadCap
	}
	if err := tx.QueryRow(ctx, `SELECT coalesce((SELECT cap FROM signup_cap FOR UPDATE), 0)`).Scan(&old); err != nil {
		return 0, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO signup_cap (id, cap, updated_at) VALUES (true, $1, now())
		ON CONFLICT (id) DO UPDATE SET cap = excluded.cap, updated_at = now()`, limit)
	return old, err
}

// WaitlistInvite is an entry invited off the waitlist: whom to email.
type WaitlistInvite struct {
	ID, Email, Name, Provider string
	// Again: it had been invited before (the email is sent again).
	Again bool
}

// InviteFromWaitlist marks entries invited by admin, in tx: each one's next
// sign-in makes their account. Entries that signed up already, or don't
// exist, are left out.
func InviteFromWaitlist(ctx context.Context, tx pgx.Tx, ids []string, admin string) ([]WaitlistInvite, error) {
	rows, err := tx.Query(ctx, `UPDATE waitlist w SET invited_at = coalesce(w.invited_at, now()), invited_by = coalesce(w.invited_by, $2)
		FROM (SELECT id, invited_at FROM waitlist WHERE id = ANY($1::uuid[]) AND signed_up_at IS NULL FOR UPDATE) old
		WHERE w.id = old.id
		RETURNING w.id::text, w.email, w.name, w.provider, old.invited_at IS NOT NULL`, ids, admin)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (WaitlistInvite, error) {
		var in WaitlistInvite
		return in, r.Scan(&in.ID, &in.Email, &in.Name, &in.Provider, &in.Again)
	})
}

// MarkEmailed records that an invited entry's email was sent.
func MarkEmailed(ctx context.Context, pool *pgxpool.Pool, entry string) error {
	_, err := pool.Exec(ctx, `UPDATE waitlist SET emailed_at = now() WHERE id = $1`, entry)
	return err
}

// RemoveFromWaitlist deletes entries in tx, returning the IDs removed.
func RemoveFromWaitlist(ctx context.Context, tx pgx.Tx, ids []string) ([]string, error) {
	rows, err := tx.Query(ctx, `DELETE FROM waitlist WHERE id = ANY($1::uuid[]) RETURNING id::text`, ids)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// PruneWaitlist deletes entries past their retention, at most limit, for
// the expiry pass (the rest go on the next).
func PruneWaitlist(ctx context.Context, pool *pgxpool.Pool, limit int) (int64, error) {
	tag, err := pool.Exec(ctx, `DELETE FROM waitlist WHERE id IN (SELECT id FROM waitlist
		WHERE signed_up_at < now() - make_interval(secs => $1)
		   OR (signed_up_at IS NULL AND invited_at < now() - make_interval(secs => $2)) LIMIT $3)`,
		WaitlistRetention.Seconds(), InvitedRetention.Seconds(), limit)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// oneLine is s with every run of whitespace (line breaks too) one space.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// clip is s cut to at most n characters.
func clip(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}
