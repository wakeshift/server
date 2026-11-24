package botkeys

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"strings"
	"testing"
)

const (
	tenant = "0b8f3c1e-2d4a-4b6c-8e0f-1a2b3c4d5e6f"
	id     = "5e6f7a8b-9c0d-4e1f-a2b3-c4d5e6f7a8b9"
)

func pemOf(t *testing.T, k any) string {
	t.Helper()
	b, err := x509.MarshalPKIXPublicKey(k)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: b}))
}

// Challenges round-trip, pass the routine's check, and are derived per
// purpose, workspace and row.
func TestChallenge(t *testing.T) {
	c := NewCodec([]byte("0123456789abcdef0123456789abcdef"))
	s := c.Challenge(Setup, tenant, id)
	if !strings.HasPrefix(s, "wakeshift:v1:setup:") || !Pattern.MatchString(s) || len(s) > MaxChallenge {
		t.Fatalf("challenge %q", s)
	}
	p, err := Parse(s + "\n")
	if err != nil || p.Purpose != Setup || p.Tenant != tenant || p.ID != id || p.String() != s {
		t.Fatalf("parse: %+v %v", p, err)
	}
	if c.Challenge(Register, tenant, id) == s || NewCodec([]byte("x")).Challenge(Setup, tenant, id) == s {
		t.Fatal("not derived per purpose and key")
	}
	for _, bad := range []string{"", "wakeshift:v1:other:" + s[19:], s + "x", strings.Replace(s, "v1", "v2", 1), s + ";rm -rf ~", "$(id)"} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
}

// A signature verifies only by the key, over this bot's ID and this
// challenge.
func TestVerify(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	other, otherPriv, _ := ed25519.GenerateKey(rand.Reader)
	ch := NewCodec([]byte("k")).Challenge(Register, tenant, id)
	sig := ed25519.Sign(priv, Message("bot-1", ch))
	parsed, err := ParsePublicKey(pemOf(t, pub))
	if err != nil || !parsed.Equal(pub) {
		t.Fatalf("public key: %v", err)
	}
	enc, ok := ParseSignature(" " + base64.StdEncoding.EncodeToString(sig) + "\n")
	if !ok || !Verify(parsed, "bot-1", ch, enc) {
		t.Fatal("a good proof doesn't verify")
	}
	if Verify(other, "bot-1", ch, sig) || Verify(pub, "bot-2", ch, sig) || Verify(pub, "bot-1", ch+"x", sig) ||
		Verify(pub, "bot-1", ch, ed25519.Sign(otherPriv, Message("bot-1", ch))) || Verify(pub, "bot-1", ch, sig[:63]) {
		t.Fatal("a bad proof verifies")
	}
	if string(Message("b", "c")) != "wakeshift-proof\nb\nc" {
		t.Fatal("message")
	}
	rk, _ := rsa.GenerateKey(rand.Reader, 1024)
	for _, bad := range []string{"", "garbage", pemOf(t, &rk.PublicKey), strings.Replace(pemOf(t, pub), "PUBLIC", "PRIVATE", 2)} {
		if _, err := ParsePublicKey(bad); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
	if _, ok := ParseSignature("not base64!"); ok {
		t.Fatal("bad signature parsed")
	}
}

// A body holding a private key, as PEM or as a PKCS#8 Ed25519 key's base64,
// is recognized.
func TestHoldsPrivateKey(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	der, _ := x509.MarshalPKCS8PrivateKey(priv)
	p := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	line := strings.Split(p, "\n")[1]
	for _, body := range []string{p, "public_key=" + line, "x=" + base64.StdEncoding.EncodeToString(der)} {
		if !HoldsPrivateKey([]byte(body)) {
			t.Errorf("not recognized: %q", body)
		}
	}
	pub := priv.Public()
	if HoldsPrivateKey([]byte(pemOf(t, pub))) {
		t.Fatal("a public key looks private")
	}
}

func TestBotIDOK(t *testing.T) {
	for _, ok := range []string{"4f1c2b3a-9d8e-4c7b-a6f5-0e1d2c3b4a59", "bc-123"} {
		if !BotIDOK(ok) {
			t.Errorf("%q refused", ok)
		}
	}
	for _, bad := range []string{"", "a b", "a\nb", strings.Repeat("a", 201), "é", "a'b", "a/b", "$(id)", "a\"b", "a;b", "..x/../y"} {
		if BotIDOK(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
}

// Authentic: only a nonce the codec derived for what the challenge names.
func TestAuthentic(t *testing.T) {
	c, other := NewCodec([]byte("key-one-0123456789abcdef")), NewCodec([]byte("key-two-0123456789abcdef"))
	const tenant, id = "0b8f3c1e-2d4a-4b6c-8e0f-1a2b3c4d5e6f", "5e6f7a8b-9c0d-4e1f-a2b3-c4d5e6f7a8b9"
	ch, err := Parse(c.Challenge(Register, tenant, id))
	if err != nil || !c.Authentic(ch) {
		t.Fatalf("its own challenge: %v", err)
	}
	if other.Authentic(ch) {
		t.Fatal("another key's codec took it")
	}
	for _, alt := range []Challenge{
		{Purpose: Setup, Tenant: ch.Tenant, ID: ch.ID, Nonce: ch.Nonce},
		{Purpose: ch.Purpose, Tenant: id, ID: ch.ID, Nonce: ch.Nonce},
		{Purpose: ch.Purpose, Tenant: ch.Tenant, ID: tenant, Nonce: ch.Nonce},
		{Purpose: ch.Purpose, Tenant: ch.Tenant, ID: ch.ID, Nonce: make([]byte, 16)},
	} {
		if c.Authentic(alt) {
			t.Fatalf("an altered challenge: %+v", alt)
		}
	}
}
