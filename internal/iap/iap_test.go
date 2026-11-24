package iap_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"

	"github.com/wakeshift/server/internal/iap"
	"github.com/wakeshift/server/internal/iap/iaptest"
)

const admin = "admin@example.com"

func TestVerify(t *testing.T) {
	s := iaptest.New(t)
	v := s.Verifier(t, iaptest.Audience, admin)
	ctx := context.Background()

	id, err := v.Verify(ctx, s.Assertion(t, iaptest.Audience, admin))
	if err != nil || id.Email != admin {
		t.Fatalf("a valid assertion: %+v, %v", id, err)
	}

	other, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	now := time.Now()
	refused := map[string]string{
		"missing":               "",
		"garbage":               "not.a.jwt",
		"another audience":      s.Assertion(t, "/projects/123/global/backendServices/999", admin),
		"another issuer":        s.Sign(t, iaptest.Claims{Audience: iaptest.Audience, Email: admin, Issuer: "https://accounts.google.com"}),
		"expired":               s.Sign(t, iaptest.Claims{Audience: iaptest.Audience, Email: admin, Now: now.Add(-time.Hour)}),
		"an email off the list": s.Assertion(t, iaptest.Audience, "mallory@example.com"),
		"case folded":           s.Assertion(t, iaptest.Audience, "Admin@example.com"),
		"subaddress":            s.Assertion(t, iaptest.Audience, "admin+x@example.com"),
		"no email":              s.Assertion(t, iaptest.Audience, ""),
		"a prefixed email":      s.Assertion(t, iaptest.Audience, "accounts.google.com:"+admin),
		"another key": iaptest.SignWith(t, other, jose.ES256, iaptest.Claims{Issuer: iap.Issuer, Audience: iaptest.Audience, Email: admin,
			IssuedAt: now.Unix(), Expires: now.Add(time.Minute).Unix()}),
		"unsigned": unsigned(map[string]any{"iss": iap.Issuer, "aud": iaptest.Audience, "email": admin, "exp": now.Add(time.Minute).Unix()}),
		"HS256":    hs256(map[string]any{"iss": iap.Issuer, "aud": iaptest.Audience, "email": admin, "exp": now.Add(time.Minute).Unix()}),
		"too long": strings.Repeat("a", 9000),
	}
	for name, a := range refused {
		if _, err := v.Verify(ctx, a); !errors.Is(err, iap.ErrRefused) {
			t.Errorf("%s: %v, want ErrRefused", name, err)
		}
	}
}

func TestNewRefusesBadSettings(t *testing.T) {
	s := iaptest.New(t)
	for name, c := range map[string]struct {
		aud    string
		admins []string
	}{
		"no audience":     {"", []string{admin}},
		"a bare number":   {"456", []string{admin}},
		"no admins":       {iaptest.Audience, nil},
		"uppercase admin": {iaptest.Audience, []string{"Admin@example.com"}},
		"subaddressed":    {iaptest.Audience, []string{"admin+admin@example.com"}},
		"not an email":    {iaptest.Audience, []string{"ada"}},
		"an empty admin":  {iaptest.Audience, []string{""}},
	} {
		if _, err := iap.NewWithKeys(c.aud, c.admins, s.Keys()); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func b64(v string) string { return base64.RawURLEncoding.EncodeToString([]byte(v)) }

func claims(c map[string]any) string {
	b, _ := json.Marshal(c)
	return string(b)
}

func unsigned(c map[string]any) string {
	return b64(`{"alg":"none","typ":"JWT"}`) + "." + b64(claims(c)) + "."
}

func hs256(c map[string]any) string {
	in := b64(`{"alg":"HS256","typ":"JWT"}`) + "." + b64(claims(c))
	m := hmac.New(sha256.New, []byte("secret"))
	m.Write([]byte(in))
	return in + "." + base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}
