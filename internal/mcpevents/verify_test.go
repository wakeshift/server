package mcpevents

import (
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The Standard Webhooks spec's example (also Svix's documented one): this
// secret, ID, timestamp and body sign to this v1 signature.
const (
	vectorSecret = "whsec_MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw"
	vectorID     = "msg_p5jXN8AQM9LWM0D4loKWxJek"
	vectorTS     = 1614265330
	vectorBody   = `{"test": 2432232314}`
	vectorSig    = "v1,g0hM9SsE+OTPJTGt/tmIKtSyZlE3uFJELVlNIOLJ1OE="
)

func signedHeader(prefix, id string, ts int64, sig string) http.Header {
	h := http.Header{}
	h.Set(prefix+"id", id)
	h.Set(prefix+"timestamp", strconv.FormatInt(ts, 10))
	h.Set(prefix+"signature", sig)
	return h
}

func TestVerifySignedVector(t *testing.T) {
	key, err := ParseSecret(vectorSecret)
	if err != nil {
		t.Fatal(err)
	}
	if got := Sign(vectorID, vectorTS, []byte(vectorBody), key); got != vectorSig {
		t.Fatalf("Sign = %q, want the spec's %q", got, vectorSig)
	}
	at := time.Unix(vectorTS, 0)
	for _, prefix := range []string{"webhook-", "svix-", "Webhook-", "SVIX-"} {
		id, err := VerifySigned(signedHeader(prefix, vectorID, vectorTS, vectorSig), []byte(vectorBody), at.Add(4*time.Minute), key)
		if err != nil || id != vectorID {
			t.Fatalf("%s headers: %q %v", prefix, id, err)
		}
	}
}

func TestVerifySignedRefuses(t *testing.T) {
	key, _ := ParseSecret(vectorSecret)
	other, _ := ParseSecret("whsec_" + strings.Repeat("B", 32))
	at := time.Unix(vectorTS, 0)
	body := []byte(vectorBody)
	ok := signedHeader("webhook-", vectorID, vectorTS, vectorSig)
	cases := map[string]struct {
		h    http.Header
		body []byte
		now  time.Time
		keys [][]byte
		want string
	}{
		"wrong secret":    {ok, body, at, [][]byte{other}, "no signature"},
		"tampered body":   {ok, []byte(`{"test": 2432232315}`), at, [][]byte{key}, "no signature"},
		"tampered id":     {signedHeader("webhook-", "msg_other", vectorTS, vectorSig), body, at, [][]byte{key}, "no signature"},
		"old timestamp":   {ok, body, at.Add(5*time.Minute + time.Second), [][]byte{key}, "5 minutes"},
		"future":          {ok, body, at.Add(-5*time.Minute - time.Second), [][]byte{key}, "5 minutes"},
		"no id":           {signedHeader("webhook-", "", vectorTS, vectorSig), body, at, [][]byte{key}, "required"},
		"no signature":    {signedHeader("webhook-", vectorID, vectorTS, ""), body, at, [][]byte{key}, "required"},
		"no headers":      {http.Header{}, body, at, [][]byte{key}, "required"},
		"bad timestamp":   {func() http.Header { h := ok.Clone(); h.Set("webhook-timestamp", "soon"); return h }(), body, at, [][]byte{key}, "Unix time"},
		"other scheme":    {signedHeader("webhook-", vectorID, vectorTS, "v1a,"+vectorSig[3:]), body, at, [][]byte{key}, "no signature"},
		"bad base64":      {signedHeader("webhook-", vectorID, vectorTS, "v1,!!!"), body, at, [][]byte{key}, "no signature"},
		"no secrets":      {ok, body, at, nil, "no signature"},
		"timestamp in ms": {signedHeader("webhook-", vectorID, vectorTS*1000, vectorSig), body, at, [][]byte{key}, "5 minutes"},
	}
	for name, tc := range cases {
		id, err := VerifySigned(tc.h, tc.body, tc.now, tc.keys...)
		if err == nil || id != "" || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %q %v, want an error with %q", name, id, err, tc.want)
		}
		if err != nil && (strings.Contains(err.Error(), vectorSig[3:]) || strings.Contains(err.Error(), vectorSecret[6:])) {
			t.Errorf("%s: the error carries a signature or the secret: %v", name, err)
		}
	}
}

// A sender rotating its key sends a signature per key, and other schemes'
// next to ours: any v1 signature matching any of the URL's secrets passes.
func TestVerifySignedSeveral(t *testing.T) {
	key, _ := ParseSecret(vectorSecret)
	other, _ := ParseSecret("whsec_" + strings.Repeat("B", 32))
	at := time.Unix(vectorTS, 0)
	both := Sign(vectorID, vectorTS, []byte(vectorBody), other, key)
	if !strings.Contains(both, " ") {
		t.Fatalf("two signatures: %q", both)
	}
	for name, sig := range map[string]string{
		"theirs, then ours":  both,
		"another scheme too": "v1a,AAAA " + vectorSig,
		"padded with spaces": "  " + vectorSig + "  ",
		"an unparseable one": "garbage " + vectorSig,
	} {
		if _, err := VerifySigned(signedHeader("webhook-", vectorID, vectorTS, sig), []byte(vectorBody), at, key); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	// The URL's previous secret, during its grace: either key passes.
	if _, err := VerifySigned(signedHeader("webhook-", vectorID, vectorTS, vectorSig), []byte(vectorBody), at, other, key); err != nil {
		t.Errorf("previous key: %v", err)
	}
	// webhook-* wins over svix-* when both are sent.
	h := signedHeader("webhook-", vectorID, vectorTS, vectorSig)
	h.Set("svix-id", "msg_svix")
	h.Set("svix-timestamp", "1")
	h.Set("svix-signature", "v1,AAAA")
	if id, err := VerifySigned(h, []byte(vectorBody), at, key); err != nil || id != vectorID {
		t.Errorf("both header sets: %q %v", id, err)
	}
}
