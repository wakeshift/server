// Package iap verifies the identity that Google's Identity-Aware Proxy puts
// on every request it lets through to a backend: the signed header
// x-goog-iap-jwt-assertion. The admin service checks it on every request,
// and the core checks the same assertion when the admin service forwards it
// to the internal admin API.
//
// An assertion is accepted only if it's an ES256 JWT signed by one of IAP's
// keys, issued by https://cloud.google.com/iap, addressed to this backend
// (its audience, /projects/<number>/global/backendServices/<id>), unexpired,
// and its email is exactly one of the allowlist's: no case folding, no
// subaddresses, no dots ignored.
package iap

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/coreos/go-oidc/v3/oidc"
)

// Header is where IAP puts the assertion.
const Header = "X-Goog-Iap-Jwt-Assertion"

// Issuer is IAP's issuer.
const Issuer = "https://cloud.google.com/iap"

// KeysURL is where IAP publishes its signing keys, as a JWK set.
const KeysURL = "https://www.gstatic.com/iap/verify/public_key-jwk"

// ErrRefused is every refusal (see errors.Is): callers answer 404 without
// saying why.
var ErrRefused = errors.New("iap: refused")

// Verifier checks IAP assertions for one backend.
type Verifier struct {
	audience string
	admins   []string
	v        *oidc.IDTokenVerifier
}

// New returns a Verifier for audience (the backend's
// /projects/<number>/global/backendServices/<id>) that admits only admins
// (exact emails), with IAP's keys from KeysURL.
func New(ctx context.Context, audience string, admins []string) (*Verifier, error) {
	return NewWithKeys(audience, admins, oidc.NewRemoteKeySet(ctx, KeysURL))
}

// NewWithKeys is New with another key set (tests).
func NewWithKeys(audience string, admins []string, keys oidc.KeySet) (*Verifier, error) {
	if !strings.HasPrefix(audience, "/projects/") || !strings.Contains(audience, "/global/backendServices/") {
		return nil, errors.New("iap: the audience is /projects/<number>/global/backendServices/<id>")
	}
	if len(admins) == 0 {
		return nil, errors.New("iap: no admins: nobody could get in")
	}
	for _, a := range admins {
		if a == "" || a != strings.ToLower(a) || strings.Count(a, "@") != 1 || strings.Contains(a, "+") {
			return nil, errors.New("iap: admins are exact, lowercase emails without subaddresses")
		}
	}
	v := oidc.NewVerifier(Issuer, keys, &oidc.Config{ClientID: audience, SupportedSigningAlgs: []string{oidc.ES256}})
	return &Verifier{audience: audience, admins: slices.Clone(admins), v: v}, nil
}

// Identity is who IAP says made the request.
type Identity struct {
	Email   string
	Subject string
}

// Verify checks an assertion and returns the admin it names. Anything
// wrong is ErrRefused.
func (v *Verifier) Verify(ctx context.Context, assertion string) (Identity, error) {
	if assertion == "" || len(assertion) > 8192 {
		return Identity{}, ErrRefused
	}
	tok, err := v.v.Verify(ctx, assertion)
	if err != nil {
		return Identity{}, ErrRefused
	}
	var claims struct {
		Email string `json:"email"`
	}
	if err := tok.Claims(&claims); err != nil {
		return Identity{}, ErrRefused
	}
	// The assertion's email is the bare address (the
	// x-goog-authenticated-user-email header's "accounts.google.com:"
	// prefix isn't in it), compared exactly.
	if !IsAdmin(claims.Email, v.admins) {
		return Identity{}, ErrRefused
	}
	return Identity{Email: claims.Email, Subject: tok.Subject}, nil
}

// IsAdmin reports whether email is exactly one of admins: no folding of
// case, subaddresses or dots.
func IsAdmin(email string, admins []string) bool {
	return email != "" && slices.Contains(admins, email)
}

// Audience is the audience this Verifier accepts.
func (v *Verifier) Audience() string { return v.audience }
