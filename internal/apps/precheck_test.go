package apps_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/wakeshift/server/internal/apps"
)

// What a token alone shows wrong is refused before any database work: the
// registry here has a database nothing answers at, so a refusal that
// queried it would be an internal error, not a refusal. A well-formed
// token does reach it (the last check), with or without a kid.
func TestBadAssertionsCostNoQueries(t *testing.T) {
	pool, err := pgxpool.New(ctx, "postgres://nobody@127.0.0.1:1/none?connect_timeout=1&sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	r := &apps.Registry{Pool: pool, Issuer: "https://wakeshift.test"}
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	header := func(alg, typ string) string {
		return base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"` + alg + `","typ":"` + typ + `","kid":"k"}`))
	}
	good := assertion(key, nil)
	parts := strings.Split(good, ".")
	claims, sig := parts[1], parts[2]
	for name, tok := range map[string]string{
		"wrong audience": assertion(key, func(a *apps.Assertion) { a.Audience = "https://elsewhere/api/apps" }),
		"expired":        assertion(key, func(a *apps.Assertion) { a.Expires = time.Now().Add(-time.Second).Unix() }),
		"too long-lived": assertion(key, func(a *apps.Assertion) { a.Expires = time.Now().Add(time.Hour).Unix() }),
		"issued later":   assertion(key, func(a *apps.Assertion) { a.IssuedAt = time.Now().Add(time.Hour).Unix() }),
		"no jti":         assertion(key, func(a *apps.Assertion) { a.ID = "" }),
		"long jti":       assertion(key, func(a *apps.Assertion) { a.ID = strings.Repeat("j", 201) }),
		"iss isn't sub":  assertion(key, func(a *apps.Assertion) { a.Subject = "sources" }),
		"kid, expired":   kidAssertion(key, "", "j1", func(a *apps.Assertion) { a.Expires = time.Now().Add(-time.Second).Unix() }),
		"kid, audience":  kidAssertion(key, "", "j2", func(a *apps.Assertion) { a.Audience = "https://elsewhere/api/apps" }),
		"another alg":    header("HS256", "app+jwt") + "." + claims + "." + sig,
		"another typ":    header("EdDSA", "JWT") + "." + claims + "." + sig,
		"short sig":      parts[0] + "." + claims + "." + sig[:20],
		"no sig":         parts[0] + "." + claims + ".",
		"garbage":        "a.b.c",
	} {
		if _, err := r.Authenticate(ctx, tok, time.Now()); !errors.Is(err, apps.ErrUnauthenticated) {
			t.Errorf("%s: %v, want a refusal without a query", name, err)
		}
	}
	for name, tok := range map[string]string{"well-formed": good, "well-formed, kid": kidAssertion(key, "", "j3", nil)} {
		if _, err := r.Authenticate(ctx, tok, time.Now()); err == nil || errors.Is(err, apps.ErrUnauthenticated) {
			t.Errorf("%s: %v, want the database's error", name, err)
		}
	}
}
