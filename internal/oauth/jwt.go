package oauth

import (
	"crypto/ed25519"
	"errors"
	"time"

	"github.com/wakeshift/server/internal/jws"
)

// AccessClaims are an access token's claims (RFC 9068, plus who it's for in
// Wakeshift terms).
type AccessClaims struct {
	Issuer   string `json:"iss"`
	Subject  string `json:"sub"` // "<tenant>/<agent>"
	Audience string `json:"aud"` // the app's MCP URL
	ClientID string `json:"client_id"`
	IssuedAt int64  `json:"iat"`
	Expires  int64  `json:"exp"`
	ID       string `json:"jti"`
	Tenant   string `json:"wakeshift_tenant"`
	Agent    string `json:"wakeshift_agent"`
	// Set on an app's tokens for its agents (headless.go): the app acting
	// for the agent, and the grant that lets it.
	Actor *Actor `json:"act,omitempty"`
	Grant string `json:"wakeshift_grant,omitempty"`
}

// Actor is who acts for the subject.
type Actor struct {
	Subject string `json:"sub"` // "app:<id>"
}

func signJWT(key signingKey, claims any) (string, error) {
	return jws.Sign(key.private, key.kid, "at+jwt", claims)
}

// VerifyAccessToken checks an access token's signature (with the key named
// by its kid), type, issuer, audience and expiry.
func VerifyAccessToken(token, issuer, audience string, keys func(kid string) (ed25519.PublicKey, bool), now time.Time) (AccessClaims, error) {
	var c AccessClaims
	_, err := jws.Verify(token, "at+jwt", func(h jws.Header) ed25519.PublicKey {
		k, _ := keys(h.Kid)
		return k
	}, &c)
	switch {
	case err != nil:
		return c, errors.New("invalid token")
	case c.Issuer != issuer:
		return c, errors.New("wrong issuer")
	case c.Audience != audience:
		return c, errors.New("token is for another resource")
	case now.Unix() >= c.Expires:
		return c, errors.New("token expired")
	}
	return c, nil
}
