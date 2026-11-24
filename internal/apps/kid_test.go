package apps_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/appkey"
	"github.com/wakeshift/server/internal/apps"
	"github.com/wakeshift/server/internal/jws"
	"github.com/wakeshift/server/internal/orgs"
)

// kidAssertion is assertion with a kid in the header: kid's own (the key's)
// unless kid is given, and iss = sub = the kid unless mutate says
// otherwise.
func kidAssertion(key ed25519.PrivateKey, kid, jti string, mutate func(*apps.Assertion)) string {
	if kid == "" {
		kid = appkey.KeyID(key.Public().(ed25519.PublicKey))
	}
	now := time.Now()
	a := apps.Assertion{Issuer: kid, Subject: kid, Audience: "https://wakeshift.test/api/apps",
		IssuedAt: now.Unix(), Expires: now.Add(time.Minute).Unix(), ID: jti}
	if mutate != nil {
		mutate(&a)
	}
	tok, _ := jws.Sign(key, kid, "app+jwt", a)
	return tok
}

// setupTwo registers agentchat (setup's) and sources, each with its own key.
func setupTwo(t *testing.T) (r *apps.Registry, chat, sources ed25519.PrivateKey) {
	t.Helper()
	r, chat = setup(t)
	pub, sources, _ := ed25519.GenerateKey(rand.Reader)
	if err := r.Sync(ctx, []apps.Config{{ID: "sources", Name: "Sources", PublicKey: base64.StdEncoding.EncodeToString(pub)}}); err != nil {
		t.Fatal(err)
	}
	return r, chat, sources
}

func refused(t *testing.T, r *apps.Registry, name, tok string) error {
	t.Helper()
	_, err := r.Authenticate(ctx, tok, time.Now())
	if !errors.Is(err, apps.ErrUnauthenticated) {
		t.Errorf("%s: %v, want a refusal", name, err)
	}
	return err
}

func TestAssertionsWithAKid(t *testing.T) {
	r, chat, sources := setupTwo(t)
	chatKID := appkey.KeyID(chat.Public().(ed25519.PublicKey))

	// iss = sub = the kid: a key that names no app.
	app, err := r.Authenticate(ctx, kidAssertion(chat, "", "k1", nil), time.Now())
	if err != nil || app.ID != "agentchat" || app.KeyID != chatKID[:8] || app.KeyName != "WAKESHIFT_APPS" {
		t.Fatalf("iss = kid: %+v, %v", app, err)
	}
	// iss = sub = the app: an old key, from a current SDK.
	if app, err := r.Authenticate(ctx, kidAssertion(chat, "", "k2", func(a *apps.Assertion) { a.Issuer, a.Subject = "agentchat", "agentchat" }), time.Now()); err != nil || app.ID != "agentchat" {
		t.Fatalf("iss = the app: %+v, %v", app, err)
	}
	// Each app's kid finds that app, and only that one.
	if app, err := r.Authenticate(ctx, kidAssertion(sources, "", "k3", nil), time.Now()); err != nil || app.ID != "sources" {
		t.Fatalf("sources' kid: %+v, %v", app, err)
	}

	// Confusion across apps: agentchat's kid can't be made to speak for
	// sources, nor sources' key for agentchat.
	refused(t, r, "agentchat's kid, iss sources", kidAssertion(chat, "", "c1", func(a *apps.Assertion) { a.Issuer, a.Subject = "sources", "sources" }))
	refused(t, r, "agentchat's kid, signed by sources' key", kidAssertion(sources, chatKID, "c2", nil))
	refused(t, r, "agentchat's kid, signed by sources' key, iss agentchat", kidAssertion(sources, chatKID, "c3", func(a *apps.Assertion) { a.Issuer, a.Subject = "agentchat", "agentchat" }))
	refused(t, r, "agentchat's kid and iss, sub sources", kidAssertion(chat, "", "c4", func(a *apps.Assertion) { a.Subject = "sources" }))
	refused(t, r, "iss the kid, sub the app", kidAssertion(chat, "", "c5", func(a *apps.Assertion) { a.Subject = "agentchat" }))
	// iss naming another app's kid: refused, whichever key signed.
	sourcesKID := appkey.KeyID(sources.Public().(ed25519.PublicKey))
	refused(t, r, "iss sources' kid, header agentchat's", kidAssertion(chat, "", "c6", func(a *apps.Assertion) { a.Issuer, a.Subject = sourcesKID, sourcesKID }))
	// iss = the kid needs the kid in the header (older SDKs never send it).
	noHeader := assertion(chat, func(a *apps.Assertion) { a.Issuer, a.Subject, a.ID = chatKID, chatKID, "c7" })
	refused(t, r, "iss the kid, no kid in the header", noHeader)
	// The rest of the checks apply as before.
	refused(t, r, "wrong audience", kidAssertion(chat, "", "c8", func(a *apps.Assertion) { a.Audience = "https://elsewhere/api/apps" }))
	refused(t, r, "expired", kidAssertion(chat, "", "c9", func(a *apps.Assertion) { a.Expires = time.Now().Add(-time.Second).Unix() }))
	refused(t, r, "no jti", kidAssertion(chat, "", "", nil))

	// Without a kid (older SDKs): today's path.
	if app, err := r.Authenticate(ctx, assertion(chat, func(a *apps.Assertion) { a.ID = "old" }), time.Now()); err != nil || app.ID != "agentchat" {
		t.Fatalf("no kid: %+v, %v", app, err)
	}
}

// With several keys, a kid names exactly one: another key of the same app
// doesn't verify for it.
func TestAKidNamesOneKey(t *testing.T) {
	r, chat := setup(t)
	pub2, priv2, _ := ed25519.GenerateKey(rand.Reader)
	if _, err := r.Pool.Exec(ctx, `INSERT INTO app_keys (app_id, key_id, kid, name, public_key) VALUES ('agentchat', $1, $2, 'second', $3)`,
		apps.KeyID(pub2), appkey.KeyID(pub2), []byte(pub2)); err != nil {
		t.Fatal(err)
	}
	chatKID := appkey.KeyID(chat.Public().(ed25519.PublicKey))
	refused(t, r, "the second key under the first's kid", kidAssertion(priv2, chatKID, "o1", func(a *apps.Assertion) { a.Issuer, a.Subject = "agentchat", "agentchat" }))
	if app, err := r.Authenticate(ctx, kidAssertion(priv2, "", "o2", nil), time.Now()); err != nil || app.KeyName != "second" {
		t.Fatalf("the second key: %+v, %v", app, err)
	}
}

// Unknown, revoked and deleted apps' kids all read alike, so the API
// can't be used to probe for keys; the refusal names WAKESHIFT_CORE, for
// a key from another console.
func TestUnknownKids(t *testing.T) {
	r, chat, sources := setupTwo(t)
	_, stranger, _ := ed25519.GenerateKey(rand.Reader)
	unknown := refused(t, r, "unknown kid", kidAssertion(stranger, "", "u1", nil))
	if unknown == nil || !strings.Contains(unknown.Error(), "WAKESHIFT_CORE") || !strings.Contains(unknown.Error(), "wakeshift.test") {
		t.Fatalf("unknown kid: %v", unknown)
	}
	if _, err := r.Pool.Exec(ctx, `UPDATE app_keys SET revoked_at = now() WHERE app_id = 'agentchat'`); err != nil {
		t.Fatal(err)
	}
	revoked := refused(t, r, "a revoked key's kid", kidAssertion(chat, "", "u2", nil))
	if revoked == nil || revoked.Error() != unknown.Error() {
		t.Fatalf("revoked %v, unknown %v", revoked, unknown)
	}
	refused(t, r, "a revoked key's kid, iss the app", kidAssertion(chat, "", "u3", func(a *apps.Assertion) { a.Issuer, a.Subject = "agentchat", "agentchat" }))
	// A deleted app (whose keys deleting didn't revoke, say, by a bug).
	// (Only console apps are deleted: give it an organization.)
	if _, err := r.Pool.Exec(ctx, `WITH o AS (INSERT INTO organizations (name) VALUES ('Acme') RETURNING id)
		UPDATE apps SET org_id = (SELECT id FROM o), deleted_at = now() WHERE id = 'sources'`); err != nil {
		t.Fatal(err)
	}
	refused(t, r, "a deleted app's kid", kidAssertion(sources, "", "u4", nil))
	refused(t, r, "a deleted app's kid, iss the app", kidAssertion(sources, "", "u5", func(a *apps.Assertion) { a.Issuer, a.Subject = "sources", "sources" }))
}

// A replay is refused across both paths: the assertion's ID is kept by
// app, whichever way the key was found.
func TestReplayAcrossKidAndIssuer(t *testing.T) {
	r, chat := setup(t)
	if _, err := r.Authenticate(ctx, kidAssertion(chat, "", "same", nil), time.Now()); err != nil {
		t.Fatal(err)
	}
	refused(t, r, "the same jti, no kid", assertion(chat, func(a *apps.Assertion) { a.ID = "same" }))
	refused(t, r, "the same jti, iss the app", kidAssertion(chat, "", "same", func(a *apps.Assertion) { a.Issuer, a.Subject = "agentchat", "agentchat" }))
	refused(t, r, "the same assertion again", kidAssertion(chat, "", "same", nil))
}

// A suspended app's keys are refused by kid too, once the signature checks
// out: without one of its keys, nobody learns it's suspended.
func TestSuspendedByKid(t *testing.T) {
	r, chat := setup(t)
	if _, err := r.Pool.Exec(ctx, `UPDATE apps SET suspended_at = now() WHERE id = 'agentchat'`); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Authenticate(ctx, kidAssertion(chat, "", "s1", nil), time.Now()); !errors.Is(err, apps.ErrSuspended) {
		t.Fatalf("suspended, by kid: %v", err)
	}
	if _, err := r.Authenticate(ctx, assertion(chat, func(a *apps.Assertion) { a.ID = "s2" }), time.Now()); !errors.Is(err, apps.ErrSuspended) {
		t.Fatalf("suspended, by issuer: %v", err)
	}
	_, stranger, _ := ed25519.GenerateKey(rand.Reader)
	chatKID := appkey.KeyID(chat.Public().(ed25519.PublicKey))
	refused(t, r, "suspended, another key under its kid", kidAssertion(stranger, chatKID, "s3", nil))
	refused(t, r, "suspended, another key under its ID", assertion(stranger, func(a *apps.Assertion) { a.ID = "s4" }))
}

// A key the previous revision made during the deploy has no kid: a
// current SDK's assertion (kid in the header, iss the app) still works,
// and writes it.
func TestAKeyWithoutAKidYet(t *testing.T) {
	r, chat := setup(t)
	if _, err := r.Pool.Exec(ctx, `UPDATE app_keys SET kid = NULL`); err != nil {
		t.Fatal(err)
	}
	chatKID := appkey.KeyID(chat.Public().(ed25519.PublicKey))
	if _, err := r.Authenticate(ctx, kidAssertion(chat, "", "n1", func(a *apps.Assertion) { a.Issuer, a.Subject = "agentchat", "agentchat" }), time.Now()); err != nil {
		t.Fatal(err)
	}
	var kid *string
	if err := r.Pool.QueryRow(ctx, `SELECT kid FROM app_keys WHERE app_id = 'agentchat'`).Scan(&kid); err != nil || kid == nil || *kid != chatKID {
		t.Fatalf("kid %v, %v", kid, err)
	}
	if _, err := r.Authenticate(ctx, kidAssertion(chat, "", "n2", nil), time.Now()); err != nil {
		t.Fatalf("by kid, once written: %v", err)
	}
}

type goldenKeys struct {
	Seeds []struct {
		Name      string `json:"name"`
		Seed      string `json:"seed"`
		PublicKey string `json:"public_key"`
		KID       string `json:"kid"`
		KeyID     string `json:"key_id"`
	} `json:"seeds"`
	Accepted []struct {
		Key   string  `json:"key"`
		Seed  string  `json:"seed"`
		Mode  *string `json:"mode"`
		AppID *string `json:"app_id"`
	} `json:"accepted"`
	Refused []struct {
		Key string `json:"key"`
		Why string `json:"why"`
	} `json:"refused"`
}

func readGoldenKeys(t *testing.T) goldenKeys {
	t.Helper()
	b, err := os.ReadFile("../../sdk/typescript/test/app-keys.json")
	if err != nil {
		t.Fatal(err)
	}
	var g goldenKeys
	if err := json.Unmarshal(b, &g); err != nil {
		t.Fatal(err)
	}
	return g
}

// The server parses every key in the golden file as the SDKs do, and
// 090_app_keys_kid.sql's SQL gives the kid and key ID the golden file has.
func TestKeysGolden(t *testing.T) {
	r, _ := setup(t)
	g := readGoldenKeys(t)
	pubs := map[string]string{}
	for _, s := range g.Seeds {
		pubs[s.Name] = s.PublicKey
		pub, _ := base64.RawURLEncoding.DecodeString(s.PublicKey)
		var kid, keyID string
		err := r.Pool.QueryRow(ctx, `SELECT translate(rtrim(encode(substring(sha256($1) FROM 1 FOR 16), 'base64'), '='), '+/', '-_'),
			translate(encode(substring(sha256($1) FROM 1 FOR 6), 'base64'), '+/', '-_')`, pub).Scan(&kid, &keyID)
		if err != nil || kid != s.KID || keyID != s.KeyID || apps.KeyID(pub) != s.KeyID {
			t.Fatalf("%s: SQL kid %q key_id %q, %v", s.Name, kid, keyID, err)
		}
	}
	for _, a := range g.Accepted {
		mode, id, key, err := apps.ParseKey(a.Key)
		if err != nil {
			t.Fatalf("%q: %v", a.Key, err)
		}
		wantMode, wantID := "", ""
		if a.Mode != nil {
			wantMode, wantID = *a.Mode, *a.AppID
		}
		if mode != wantMode || id != wantID || base64.RawURLEncoding.EncodeToString(key.Public().(ed25519.PublicKey)) != pubs[a.Seed] {
			t.Errorf("%q: %q %q", a.Key, mode, id)
		}
	}
	for _, x := range g.Refused {
		if _, _, _, err := apps.ParseKey(x.Key); err == nil {
			t.Errorf("%q (%s) parsed", x.Key, x.Why)
		}
	}
}

// Every key written from now on has its kid: WAKESHIFT_APPS' and the
// console's.
func TestKeysAreWrittenWithTheirKid(t *testing.T) {
	r, chat := setup(t)
	var kid string
	if err := r.Pool.QueryRow(ctx, `SELECT kid FROM app_keys WHERE app_id = 'agentchat' AND revoked_at IS NULL`).Scan(&kid); err != nil ||
		kid != appkey.KeyID(chat.Public().(ed25519.PublicKey)) {
		t.Fatalf("Sync: %q, %v", kid, err)
	}
	var owner string
	if err := r.Pool.QueryRow(ctx, `INSERT INTO users (email) VALUES ('ada@example.com') RETURNING id::text`).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	var keys []string
	err := pgx.BeginFunc(ctx, r.Pool, func(tx pgx.Tx) error {
		// Their personal organization, as signing up makes it.
		if _, err := orgs.CreatePersonal(ctx, tx, owner, "", "ada@example.com"); err != nil {
			return err
		}
		_, _, key, err := r.Create(ctx, tx, owner, "", apps.NewApp{ID: "acme-bot", Name: "Acme bot"})
		if err != nil {
			return err
		}
		k, key2, err := r.CreateKey(ctx, tx, owner, "acme-bot", "CI")
		if err != nil {
			return err
		}
		_, key3, err := r.RotateKey(ctx, tx, owner, "acme-bot", k.ID)
		keys = []string{key, key2, key3}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	for i, key := range keys {
		_, _, priv, err := apps.ParseKey(key)
		if err != nil {
			t.Fatal(err)
		}
		pub := priv.Public().(ed25519.PublicKey)
		if err := r.Pool.QueryRow(ctx, `SELECT kid FROM app_keys WHERE public_key = $1`, []byte(pub)).Scan(&kid); err != nil || kid != appkey.KeyID(pub) {
			t.Fatalf("console key %d: %q, %v", i, kid, err)
		}
		tok := kidAssertion(priv, "", "console"+strconv.Itoa(i), nil)
		if i == 1 { // rotated
			refused(t, r, "a rotated key by kid", tok)
			continue
		}
		if app, err := r.Authenticate(ctx, tok, time.Now()); err != nil || app.ID != "acme-bot" {
			t.Fatalf("console key %d by kid: %+v, %v", i, app, err)
		}
	}
}
