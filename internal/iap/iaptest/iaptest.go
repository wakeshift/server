// Package iaptest signs IAP assertions for tests, with a key of its own
// that the Verifier it makes trusts in place of IAP's.
package iaptest

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/go-jose/go-jose/v4"

	"github.com/wakeshift/server/internal/iap"
)

// Audience is a backend's audience for tests.
const Audience = "/projects/123/global/backendServices/456"

// Signer holds a test signing key.
type Signer struct {
	key *ecdsa.PrivateKey
}

// New makes a Signer with a fresh ES256 key.
func New(t testing.TB) *Signer {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return &Signer{key: k}
}

// Keys is the key set that trusts this Signer.
func (s *Signer) Keys() oidc.KeySet {
	return &oidc.StaticKeySet{PublicKeys: []crypto.PublicKey{&s.key.PublicKey}}
}

// Verifier is an iap.Verifier for audience and admins that trusts this
// Signer.
func (s *Signer) Verifier(t testing.TB, audience string, admins ...string) *iap.Verifier {
	t.Helper()
	v, err := iap.NewWithKeys(audience, admins, s.Keys())
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// Claims are an assertion's claims; Sign fills in zero values.
type Claims struct {
	Issuer   string    `json:"iss"`
	Audience string    `json:"aud"`
	Subject  string    `json:"sub"`
	Email    string    `json:"email"`
	IssuedAt int64     `json:"iat"`
	Expires  int64     `json:"exp"`
	Now      time.Time `json:"-"`
}

// Assertion signs an assertion for email, valid now, to audience.
func (s *Signer) Assertion(t testing.TB, audience, email string) string {
	t.Helper()
	return s.Sign(t, Claims{Audience: audience, Email: email})
}

// Sign signs c, filling in IAP's issuer, a subject, and a 10-minute
// lifetime from now where they're zero.
func (s *Signer) Sign(t testing.TB, c Claims) string {
	t.Helper()
	now := c.Now
	if now.IsZero() {
		now = time.Now()
	}
	if c.Issuer == "" {
		c.Issuer = iap.Issuer
	}
	if c.Subject == "" {
		c.Subject = "accounts.google.com:1234567890"
	}
	if c.IssuedAt == 0 {
		c.IssuedAt = now.Unix()
	}
	if c.Expires == 0 {
		c.Expires = now.Add(10 * time.Minute).Unix()
	}
	return SignWith(t, s.key, jose.ES256, c)
}

// SignWith signs claims with key and alg (tests of refusals: another key,
// another algorithm).
func SignWith(t testing.TB, key any, alg jose.SignatureAlgorithm, claims any) string {
	t.Helper()
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: alg, Key: key}, (&jose.SignerOptions{}).WithType("JWT"))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(claims)
	obj, err := signer.Sign(b)
	if err != nil {
		t.Fatal(err)
	}
	out, err := obj.CompactSerialize()
	if err != nil {
		t.Fatal(err)
	}
	return out
}
