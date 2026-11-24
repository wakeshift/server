package apps_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/appkey"
	"github.com/wakeshift/server/internal/apps"
	"github.com/wakeshift/server/internal/dbtest"
	"github.com/wakeshift/server/internal/orgs"
)

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func fromHex(t *testing.T, h string) string {
	t.Helper()
	b, err := hex.DecodeString(h)
	if err != nil {
		t.Fatal(err)
	}
	return b64(b)
}

// The public keys in the SDKs' shared vectors parse, to the kid and key ID
// the vectors give: the CLI encodes the public key exactly so.
func TestParsePublicKeyVectors(t *testing.T) {
	b, err := os.ReadFile("../../sdk/typescript/test/app-keys.json")
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		Seeds []struct {
			Seed      string `json:"seed"`
			PublicKey string `json:"public_key"`
			KID       string `json:"kid"`
			KeyID     string `json:"key_id"`
		} `json:"seeds"`
	}
	if err := json.Unmarshal(b, &v); err != nil || len(v.Seeds) == 0 {
		t.Fatalf("vectors: %v", err)
	}
	for _, s := range v.Seeds {
		pub, err := apps.ParsePublicKey(s.PublicKey)
		if err != nil {
			t.Fatalf("%s: %v", s.PublicKey, err)
		}
		seed, _ := base64.RawURLEncoding.DecodeString(s.Seed)
		if !pub.Equal(ed25519.NewKeyFromSeed(seed).Public()) || appkey.KeyID(pub) != s.KID || apps.KeyID(pub) != s.KeyID ||
			!strings.HasPrefix(s.KID, s.KeyID) {
			t.Fatalf("%s: kid %s, key ID %s", s.PublicKey, appkey.KeyID(pub), apps.KeyID(pub))
		}
	}
}

// Anything but a canonical, large-order Ed25519 point in 43 characters of
// unpadded base64url is refused: wrong lengths, other encodings, points off
// the curve, non-canonical encodings and the eight small-order points (a
// key whose signatures anyone could forge).
func TestParsePublicKeyRefuses(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	if got, err := apps.ParsePublicKey(b64(pub)); err != nil || !got.Equal(pub) {
		t.Fatalf("a good key: %v", err)
	}
	bad := map[string]string{
		"empty":               "",
		"31 bytes":            b64(pub[:31]),
		"33 bytes":            b64(append(append([]byte{}, pub...), 0)),
		"64 bytes":            b64(append(append([]byte{}, pub...), pub...)),
		"padded":              base64.URLEncoding.EncodeToString(pub),
		"standard base64":     strings.NewReplacer("-", "+", "_", "/").Replace(b64(pub)) + "+",
		"spaces":              " " + b64(pub)[1:],
		"a seed-shaped wst_":  appkey.Format(make([]byte, 32)),
		"off the curve":       fromHex(t, "0200000000000000000000000000000000000000000000000000000000000000"),
		"non-canonical y = p": fromHex(t, "edffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff7f"),
		"non-canonical y = 1": fromHex(t, "eeffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff7f"),
		"identity":            fromHex(t, "0100000000000000000000000000000000000000000000000000000000000000"),
		"order 2":             fromHex(t, "ecffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff7f"),
		"order 4":             fromHex(t, "0000000000000000000000000000000000000000000000000000000000000000"),
		"order 4, negative x": fromHex(t, "0000000000000000000000000000000000000000000000000000000000000080"),
		"order 8 (a)":         fromHex(t, "26e8958fc2b227b045c3f489f2ef98f0d5dfac05d3c63339b13802886d53fc05"),
		"order 8 (a), neg x":  fromHex(t, "26e8958fc2b227b045c3f489f2ef98f0d5dfac05d3c63339b13802886d53fc85"),
		"order 8 (b)":         fromHex(t, "c7176a703d4dd84fba3c0b760d10670f2a2053fa2c39ccc64ec7fd7792ac037a"),
		"order 8 (b), neg x":  fromHex(t, "c7176a703d4dd84fba3c0b760d10670f2a2053fa2c39ccc64ec7fd7792ac03fa"),
		"non-alphabet":        strings.Repeat("!", 43),
		"trailing bits set":   b64(pub)[:42] + "B",
	}
	for name, s := range bad {
		if _, err := apps.ParsePublicKey(s); !errors.Is(err, apps.ErrBadPublicKey) {
			t.Errorf("%s (%q): %v", name, s, err)
		}
	}
}

// Making an app from a public key is one transaction: if storing the key
// fails, there's no app, no log entry and nothing to retry around (the ID
// is free again).
func TestCreateWithKeyIsAtomic(t *testing.T) {
	d := dbtest.New(t)
	r := &apps.Registry{Pool: d.Pool, Issuer: "https://wakeshift.test"}
	var owner string
	if err := d.Pool.QueryRow(ctx, `INSERT INTO users (email) VALUES ('ada@example.com') RETURNING id::text`).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	if err := pgx.BeginFunc(ctx, d.Pool, func(tx pgx.Tx) error {
		_, err := orgs.CreatePersonal(ctx, tx, owner, "", "ada@example.com")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// A key insert that fails after the app's row is written.
	if _, err := dbtest.Owner(t, d).Exec(ctx, `
		CREATE FUNCTION fail_key() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'no keys today'; END $$;
		CREATE TRIGGER fail_key BEFORE INSERT ON app_keys FOR EACH ROW WHEN (NEW.name = 'boom') EXECUTE FUNCTION fail_key();`); err != nil {
		t.Fatal(err)
	}
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	create := func(keyName string) error {
		return pgx.BeginFunc(ctx, d.Pool, func(tx pgx.Tx) error {
			_, _, err := r.CreateWithKey(ctx, tx, owner, "", apps.NewApp{ID: "acme-dev", Name: "Acme", KeyName: keyName}, pub, apps.ViaCLI)
			return err
		})
	}
	if err := create("boom"); err == nil || !strings.Contains(err.Error(), "no keys today") {
		t.Fatalf("a failing key: %v", err)
	}
	var apps_, keys, logs int
	if err := d.Pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM apps WHERE id = 'acme-dev'), (SELECT count(*) FROM app_keys WHERE app_id = 'acme-dev'),
		(SELECT count(*) FROM app_log WHERE app_id = 'acme-dev')`).Scan(&apps_, &keys, &logs); err != nil || apps_+keys+logs != 0 {
		t.Fatalf("after a failed create: %d apps, %d keys, %d log entries (%v)", apps_, keys, logs, err)
	}
	// The same app, ID and key go through once the key can be stored.
	if err := create("Default"); err != nil {
		t.Fatal(err)
	}
	if err := d.Pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM apps WHERE id = 'acme-dev'), (SELECT count(*) FROM app_keys WHERE app_id = 'acme-dev' AND kid = $1),
		(SELECT count(*) FROM app_log WHERE app_id = 'acme-dev' AND detail->>'via' = 'cli')`, appkey.KeyID(pub)).Scan(&apps_, &keys, &logs); err != nil ||
		apps_ != 1 || keys != 1 || logs != 2 {
		t.Fatalf("after the create: %d apps, %d keys, %d log entries via cli (%v)", apps_, keys, logs, err)
	}
}
