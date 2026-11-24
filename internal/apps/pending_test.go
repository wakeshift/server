package apps_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/appkey"
	"github.com/wakeshift/server/internal/apps"
	"github.com/wakeshift/server/internal/orgs"
)

func newKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv
}

func waitlistEntry(t *testing.T, r *apps.Registry, email string) string {
	t.Helper()
	var id string
	if err := r.Pool.QueryRow(ctx, `INSERT INTO waitlist (provider, subject, email) VALUES ('google', $1, $1) RETURNING id::text`, email).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func waitlisted(t *testing.T, r *apps.Registry, name, tok string) {
	t.Helper()
	_, err := r.Authenticate(ctx, tok, time.Now())
	if !errors.Is(err, apps.ErrWaitlisted) || errors.Is(err, apps.ErrUnauthenticated) {
		t.Errorf("%s: %v, want waitlisted", name, err)
	}
}

// A key held on the waitlist is told so, only when its own key signed a
// valid assertion; every other assertion naming it is refused as an unknown
// key would be, and nothing about the entry is said.
func TestPendingKeyIsWaitlisted(t *testing.T) {
	r, chat := setup(t)
	ada := waitlistEntry(t, r, "ada@example.com")
	pub, priv := newKey(t)
	kid := appkey.KeyID(pub)
	now := time.Now()

	// Not held yet: an unknown key, as today.
	if err := refused(t, r, "before it's held", kidAssertion(priv, "", "p0", nil)); errors.Is(err, apps.ErrWaitlisted) {
		t.Fatal("waitlisted before it was held")
	}
	if fresh, err := apps.HoldPendingKey(ctx, r.Pool, ada, pub, now); err != nil || !fresh {
		t.Fatalf("hold: %v %v", fresh, err)
	}
	if fresh, err := apps.HoldPendingKey(ctx, r.Pool, ada, pub, now); err != nil || fresh {
		t.Fatalf("hold again: %v %v", fresh, err)
	}
	waitlisted(t, r, "held", kidAssertion(priv, "", "p1", nil))
	// A replay is told the same: nothing was authenticated, nothing recorded.
	tok := kidAssertion(priv, "", "p2", nil)
	waitlisted(t, r, "first", tok)
	waitlisted(t, r, "replayed", tok)
	var refusedAt *time.Time
	if err := r.Pool.QueryRow(ctx, `SELECT refused_at FROM waitlist_keys WHERE kid = $1`, kid).Scan(&refusedAt); err != nil || refusedAt == nil {
		t.Errorf("refused_at: %v %v", refusedAt, err)
	}

	// Forged and invalid: refused as unknown, never waitlisted.
	_, other := newKey(t)
	for name, tok := range map[string]string{
		"signed by another key":  kidAssertion(other, kid, "f1", nil),
		"wrong audience":         kidAssertion(priv, "", "f2", func(a *apps.Assertion) { a.Audience = "https://elsewhere/api/apps" }),
		"expired":                kidAssertion(priv, "", "f3", func(a *apps.Assertion) { a.Expires = time.Now().Add(-time.Second).Unix() }),
		"too long-lived":         kidAssertion(priv, "", "f4", func(a *apps.Assertion) { a.Expires = time.Now().Add(time.Hour).Unix() }),
		"sub not the kid":        kidAssertion(priv, "", "f5", func(a *apps.Assertion) { a.Subject = "agentchat" }),
		"another app's kid, iss": kidAssertion(priv, appkey.KeyID(chat.Public().(ed25519.PublicKey)), "f6", nil),
	} {
		if err := refused(t, r, name, tok); errors.Is(err, apps.ErrWaitlisted) {
			t.Errorf("%s: waitlisted", name)
		}
	}

	// Another entry can't hold it, or change it.
	bob := waitlistEntry(t, r, "bob@example.com")
	if _, err := apps.HoldPendingKey(ctx, r.Pool, bob, pub, now.Add(time.Hour)); !errors.Is(err, apps.ErrKeyUsed) {
		t.Errorf("another entry's hold: %v", err)
	}
	var owner string
	var expires time.Time
	r.Pool.QueryRow(ctx, `SELECT entry_id::text, expires_at FROM waitlist_keys WHERE kid = $1`, kid).Scan(&owner, &expires)
	if owner != ada || !expires.Before(now.Add(apps.PendingKeyLife+time.Minute)) {
		t.Errorf("after bob's hold: held by %s until %v", owner, expires)
	}
	// An app's key, or a malformed entry, can't be held.
	if _, err := apps.HoldPendingKey(ctx, r.Pool, bob, chat.Public().(ed25519.PublicKey), now); !errors.Is(err, apps.ErrKeyUsed) {
		t.Errorf("an app's key: %v", err)
	}
	if _, err := apps.HoldPendingKey(ctx, r.Pool, "00000000-0000-0000-0000-000000000000", pub, now); !errors.Is(err, apps.ErrNoPendingEntry) {
		t.Errorf("no such entry: %v", err)
	}

	// Expired: unknown again, and another entry may hold it.
	if _, err := r.Pool.Exec(ctx, `UPDATE waitlist_keys SET expires_at = now() - interval '1 second' WHERE kid = $1`, kid); err != nil {
		t.Fatal(err)
	}
	if err := refused(t, r, "expired hold", kidAssertion(priv, "", "e1", nil)); errors.Is(err, apps.ErrWaitlisted) {
		t.Error("an expired hold is waitlisted")
	}
	if _, err := r.Pool.Exec(ctx, `UPDATE waitlist_keys SET expires_at = now() + interval '1 hour' WHERE kid = $1`, kid); err != nil {
		t.Fatal(err)
	}

	// Invited: their next sign-in makes the account, so the key is unknown
	// again (init goes on waiting for the person to sign in and confirm),
	// and no key can be held.
	if _, err := r.Pool.Exec(ctx, `UPDATE waitlist SET invited_at = now() WHERE id = $1`, ada); err != nil {
		t.Fatal(err)
	}
	if err := refused(t, r, "invited", kidAssertion(priv, "", "i1", nil)); errors.Is(err, apps.ErrWaitlisted) {
		t.Error("an invited entry's key is waitlisted")
	}
	pub2, _ := newKey(t)
	if _, err := apps.HoldPendingKey(ctx, r.Pool, ada, pub2, now); !errors.Is(err, apps.ErrNoPendingEntry) {
		t.Errorf("hold for an invited entry: %v", err)
	}
}

// An entry holds at most MaxPendingKeys unexpired keys; expired ones make
// room.
func TestPendingKeysPerEntry(t *testing.T) {
	r, _ := setup(t)
	ada := waitlistEntry(t, r, "ada@example.com")
	now := time.Now()
	for i := range apps.MaxPendingKeys {
		pub, _ := newKey(t)
		if _, err := apps.HoldPendingKey(ctx, r.Pool, ada, pub, now.Add(time.Duration(i)*time.Hour)); err != nil {
			t.Fatalf("key %d: %v", i, err)
		}
	}
	extra, _ := newKey(t)
	if _, err := apps.HoldPendingKey(ctx, r.Pool, ada, extra, now.Add(3*time.Hour)); !errors.Is(err, apps.ErrTooManyPendingKeys) {
		t.Fatalf("one too many: %v", err)
	}
	// Another entry isn't limited by ada's.
	bob := waitlistEntry(t, r, "bob@example.com")
	if _, err := apps.HoldPendingKey(ctx, r.Pool, bob, extra, now); err != nil {
		t.Fatalf("bob's: %v", err)
	}
	// A day after the first, it's expired, and there's room for one more.
	another, _ := newKey(t)
	if _, err := apps.HoldPendingKey(ctx, r.Pool, ada, another, now.Add(apps.PendingKeyLife+time.Minute)); err != nil {
		t.Fatalf("after the first expired: %v", err)
	}
	var n int
	r.Pool.QueryRow(ctx, `SELECT count(*) FROM waitlist_keys WHERE entry_id = $1`, ada).Scan(&n)
	if n != apps.MaxPendingKeys {
		t.Errorf("ada holds %d keys", n)
	}
	// The expiry pass deletes expired keys; removing an entry deletes its own.
	if _, err := r.Pool.Exec(ctx, `UPDATE waitlist_keys SET expires_at = now() - interval '1 second' WHERE entry_id = $1`, bob); err != nil {
		t.Fatal(err)
	}
	if n, err := apps.PrunePendingKeys(ctx, r.Pool, 100); err != nil || n != 1 {
		t.Errorf("pruned %d (%v)", n, err)
	}
	if _, err := r.Pool.Exec(ctx, `DELETE FROM waitlist WHERE id = $1`, ada); err != nil {
		t.Fatal(err)
	}
	r.Pool.QueryRow(ctx, `SELECT count(*) FROM waitlist_keys`).Scan(&n)
	if n != 0 {
		t.Errorf("%d keys left", n)
	}
}

// Once admitted, confirming the link registers the held key as usual: it's
// the app's key, and no longer pending.
func TestPendingKeyRegistered(t *testing.T) {
	r, _ := setup(t)
	ada := waitlistEntry(t, r, "ada@example.com")
	pub, priv := newKey(t)
	if _, err := apps.HoldPendingKey(ctx, r.Pool, ada, pub, time.Now()); err != nil {
		t.Fatal(err)
	}
	var owner string
	if err := r.Pool.QueryRow(ctx, `INSERT INTO users (email) VALUES ('ada@example.com') RETURNING id::text`).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	if err := pgx.BeginFunc(ctx, r.Pool, func(tx pgx.Tx) error {
		if _, err := orgs.CreatePersonal(ctx, tx, owner, "", "ada@example.com"); err != nil {
			return err
		}
		_, _, err := r.CreateWithKey(ctx, tx, owner, "", apps.NewApp{ID: "acme-dev", Name: "Acme"}, pub, apps.ViaCLI)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if app, err := r.Authenticate(ctx, kidAssertion(priv, "", "r1", nil), time.Now()); err != nil || app.ID != "acme-dev" {
		t.Fatalf("after registering: %+v %v", app, err)
	}
	var n int
	r.Pool.QueryRow(ctx, `SELECT count(*) FROM waitlist_keys`).Scan(&n)
	if n != 0 {
		t.Errorf("%d pending keys after registering", n)
	}
	// And it can't be held again.
	if _, err := apps.HoldPendingKey(ctx, r.Pool, ada, pub, time.Now()); !errors.Is(err, apps.ErrKeyUsed) {
		t.Errorf("held again after registering: %v", err)
	}
}
