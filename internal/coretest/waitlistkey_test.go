package coretest_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/accounts"
	"github.com/wakeshift/server/internal/appkey"
	"github.com/wakeshift/server/internal/core"
	"github.com/wakeshift/server/internal/coretest"
	"github.com/wakeshift/server/pkg/wakeshift"
)

// cliKey is a key as @wakeshift/cli's init makes it: the key for .env and
// the public half its link carries.
func cliKey(t *testing.T) (key, pk string) {
	t.Helper()
	seed := make([]byte, ed25519.SeedSize)
	rand.Read(seed)
	pub := ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey)
	return appkey.Format(seed), base64.RawURLEncoding.EncodeToString(pub)
}

// keyCode is pk's key code, as the CLI prints it and the person types it.
func keyCode(pk string) string {
	pub, _ := base64.RawURLEncoding.DecodeString(pk)
	return appkey.KeyID(pub)[:8]
}

// Past the sign-up cap, the person who opens init's link lands on the
// waitlist, and the page holds the link's public key: init's poll (GET
// /app with that key) gets 403 waitlisted at once, rather than 401 for ten
// minutes. Once they're admitted and confirm, the same key is the app's.
func TestWaitlistedInit(t *testing.T) {
	g := coretest.NewGoogle(t)
	c := coretest.StartWith(t, func(o *core.Options) { o.Google = g.Config() })
	c.SignIn("first@example.com") // the one account
	if err := pgx.BeginFunc(t.Context(), c.DB.Pool, func(tx pgx.Tx) error {
		_, err := accounts.SetSignupCap(t.Context(), tx, 1)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	key, pk := cliKey(t)
	poll := func() error {
		_, err := coretest.KeyClient(t, key, c.URL).Info(t.Context())
		return err
	}
	// Before the person opens the link: an unknown key, as today.
	if err := poll(); !wakeshift.IsCode(err, wakeshift.CodeUnauthenticated) {
		t.Fatalf("before the link: %v", err)
	}

	person, to := c.GoogleSignIn(g, "sub-ada", "ada@example.com")
	var s struct {
		Authenticated bool
		Waitlist      *struct{ Invited bool }
	}
	person.Do("GET", "/api/v1/session", nil, &s)
	if to != "/" || s.Authenticated || s.Waitlist == nil {
		t.Fatalf("past the cap: sent to %s, session %+v", to, s)
	}
	// The page holds the link's key; the person can't create the app.
	if code := person.Do("POST", "/api/v1/waitlist/key", map[string]string{"public_key": pk}, nil); code != 200 {
		t.Fatalf("hold the key: %d", code)
	}
	if code := person.Do("POST", "/api/v1/developer/apps/register", map[string]string{"id": "shop-dev", "name": "Shop", "public_key": pk, "key_code": keyCode(pk)}, nil); code != 401 {
		t.Fatalf("register while waitlisted: %d", code)
	}

	// init's poll: 403 waitlisted, with the message the CLI shows.
	err := poll()
	var api *wakeshift.APIError
	if !wakeshift.IsCode(err, "waitlisted") || !errors.As(err, &api) || api.Status != 403 {
		t.Fatalf("poll while waitlisted: %v", err)
	}
	// Another key, never held, is still just unknown: nothing about the
	// entry leaks to it.
	other, _ := cliKey(t)
	if _, err := coretest.KeyClient(t, other, c.URL).Info(t.Context()); !wakeshift.IsCode(err, wakeshift.CodeUnauthenticated) {
		t.Fatalf("a key never held: %v", err)
	}

	// Admitted (invited off the waitlist): the key is unknown again, so a
	// rerun of init waits for the person, who signs in and confirms.
	if err := pgx.BeginFunc(t.Context(), c.DB.Pool, func(tx pgx.Tx) error {
		var id string
		if err := tx.QueryRow(t.Context(), `SELECT id::text FROM waitlist WHERE email = 'ada@example.com'`).Scan(&id); err != nil {
			return err
		}
		_, err := accounts.InviteFromWaitlist(t.Context(), tx, []string{id}, "admin@example.com")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := poll(); !wakeshift.IsCode(err, wakeshift.CodeUnauthenticated) {
		t.Fatalf("invited: %v", err)
	}
	ada, _ := c.GoogleSignIn(g, "sub-ada", "ada@example.com")
	if code := ada.Do("POST", "/api/v1/developer/apps/register", map[string]string{"id": "shop-dev", "name": "Shop", "public_key": pk, "key_code": keyCode(pk)}, nil); code != 200 {
		t.Fatalf("register once admitted: %d", code)
	}
	info, err := coretest.KeyClient(t, key, c.URL).Info(t.Context())
	if err != nil || info.ID != "shop-dev" {
		t.Fatalf("after confirming: %+v %v", info, err)
	}
	var pending int
	c.DB.Pool.QueryRow(t.Context(), `SELECT count(*) FROM waitlist_keys`).Scan(&pending)
	if pending != 0 {
		t.Errorf("%d pending keys left", pending)
	}
}
