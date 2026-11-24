package agenthooks

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wakeshift/server/internal/mcpevents"
)

const cursorID = "wh_9xQ2LmNpRsTuVwYz"

// Only Cursor's exact Automations webhook URL shape is accepted.
func TestCheckCursorURL(t *testing.T) {
	good := []string{
		CursorOrigin + CursorPath + cursorID,
		CursorOrigin + CursorPath + "abcdefgh",
		CursorOrigin + CursorPath + strings.Repeat("a", 128),
	}
	for _, u := range good {
		if err := CheckCursorURL(u); err != nil {
			t.Errorf("%s: %v", u, err)
		}
	}
	bad := []string{
		"",
		"http://api2.cursor.sh" + CursorPath + cursorID,
		"https://api2.cursor.sh.evil.com" + CursorPath + cursorID,
		"https://api.cursor.sh" + CursorPath + cursorID,
		"https://evil.com" + CursorPath + cursorID,
		"https://127.0.0.1" + CursorPath + cursorID,
		"https://169.254.169.254" + CursorPath + cursorID,
		"https://api2.cursor.sh:443" + CursorPath + cursorID,
		"https://u:p@api2.cursor.sh" + CursorPath + cursorID,
		"https://api2.cursor.sh@evil.com" + CursorPath + cursorID,
		CursorOrigin + CursorPath + cursorID + "?x=1",
		CursorOrigin + CursorPath + cursorID + "#f",
		CursorOrigin + CursorPath + cursorID + "/",
		CursorOrigin + CursorPath + "%2e%2e/" + cursorID,
		CursorOrigin + CursorPath + "../" + cursorID,
		CursorOrigin + CursorPath + "a.b.c.d.e",
		CursorOrigin + CursorPath + "short",
		CursorOrigin + CursorPath + strings.Repeat("a", 129),
		CursorOrigin + "/automations/other/" + cursorID,
		CursorOrigin + "/automations/webhook" + cursorID,
		CursorOrigin + CursorPath + cursorID + " ",
		" " + CursorOrigin + CursorPath + cursorID,
		CursorOrigin + CursorPath + strings.Repeat("a", 600),
		CapyOrigin + CapyPath + cursorID + "aaaaaaa",
	}
	for _, u := range bad {
		if err := CheckCursorURL(u); !errors.Is(err, ErrBadCursorURL) {
			t.Errorf("%q accepted", u)
		}
	}
	if strings.Contains(ErrBadCursorURL.Error(), cursorID) {
		t.Fatal("the error echoes the URL")
	}
}

// The key is a plain header value: printable ASCII with no spaces or
// control characters, 32 to 256 bytes.
func TestCheckCursorKey(t *testing.T) {
	if err := CheckCursorKey("crsr_" + strings.Repeat("k", 64)); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"", strings.Repeat("k", 31), strings.Repeat("k", 257), strings.Repeat("k", 40) + " x", strings.Repeat("k", 40) + "\r\nX-Evil: 1",
		strings.Repeat("k", 40) + "\x00", strings.Repeat("k", 40) + "\t", strings.Repeat("k", 40) + "é"} {
		if CheckCursorKey(k) == nil {
			t.Errorf("%q accepted", k)
		}
	}
}

// Setup tokens are signed in a domain of their own: a registration link
// isn't one, and a token naming another workspace doesn't verify. A bind's
// ID is the setup's own.
func TestSetupLinks(t *testing.T) {
	l := NewSetupLinks([]byte("0123456789abcdef0123456789abcdef"))
	s := Setup{Tenant: "0b8f3c1e-2d4a-4b6c-8e0f-1a2b3c4d5e6f", ID: "5e6f7a8b-9c0d-4e1f-a2b3-c4d5e6f7a8b9"}
	tok := l.SetupToken(s)
	if got, err := l.ParseSetup(tok); err != nil || got != s {
		t.Fatalf("setup token: %v %v", got, err)
	}
	if a, b := BindingID(s), BindingID(Setup{Tenant: s.Tenant, ID: "6e6f7a8b-9c0d-4e1f-a2b3-c4d5e6f7a8b9"}); bytes.Equal(a, b) || !bytes.Equal(a, BindingID(s)) {
		t.Fatal("binding IDs aren't one per setup")
	}
	if u := l.URL("https://app.wakeshift.ai/", s); u != "https://app.wakeshift.ai/webhook-setup#t="+tok {
		t.Fatalf("link: %s", u)
	}
	other := NewSetupLinks([]byte("another key, another key, another"))
	if _, err := other.ParseSetup(tok); err == nil {
		t.Fatal("another key's token verifies")
	}
	reg := NewLinks([]byte("0123456789abcdef0123456789abcdef")).URL("https://x", s.Tenant, "link", KindCapy, time.Now().Add(time.Hour))
	if _, err := l.ParseSetup(reg[strings.Index(reg, "link=")+5:]); err == nil {
		t.Fatal("a registration link sets up")
	}
	// Another workspace in the claims, with the same signature.
	body, sig, _ := strings.Cut(tok, ".")
	forged := l.SetupToken(Setup{Tenant: "11111111-2222-4333-8444-555555555555", ID: s.ID})
	fbody, _, _ := strings.Cut(forged, ".")
	if _, err := l.ParseSetup(fbody + "." + sig); err == nil || body == fbody {
		t.Fatal("a token for another workspace verifies")
	}
}

// A webhook's URL and key are sealed together, bound to what they're for.
func TestSealCursor(t *testing.T) {
	seal, _ := mcpevents.NewSealer([]byte("0123456789abcdef0123456789abcdef"))
	h := CursorHook{URL: CursorOrigin + CursorPath + cursorID, Key: "crsr_" + strings.Repeat("k", 64)}
	b := SealCursor(seal, Binding("t1", "s1"), h)
	if strings.Contains(string(b), cursorID) || strings.Contains(string(b), h.Key) {
		t.Fatal("sealed in the clear")
	}
	if got, err := OpenCursor(seal, Binding("t1", "s1"), b); err != nil || got != h {
		t.Fatalf("open: %v %v", got, err)
	}
	for _, other := range []string{Binding("t2", "s1"), Binding("t1", "s2"), SetupBinding("t1", "s1")} {
		if _, err := OpenCursor(seal, other, b); err == nil {
			t.Fatalf("opened with %s", other)
		}
	}
}

// What a Cursor webhook's answer means: only 200 with "success": true
// acknowledges.
func TestJudgeCursor(t *testing.T) {
	for _, c := range []struct {
		status int
		body   string
		err    error
		want   CursorAnswer
	}{
		{200, `{"success":true,"runUuid":"r"}`, nil, CursorAccepted},
		{200, `{"success":false}`, nil, CursorFailed},
		{200, ``, nil, CursorFailed},
		{202, `{"success":true}`, nil, CursorFailed},
		{401, ``, nil, CursorUnauthorized},
		{403, ``, nil, CursorUnauthorized},
		{404, ``, nil, CursorNotFound},
		{410, ``, nil, CursorNotFound},
		{413, ``, nil, CursorTooLarge},
		{429, ``, nil, CursorFailed},
		{500, ``, nil, CursorFailed},
		{302, ``, nil, CursorFailed},
		{0, ``, errors.New("dial"), CursorFailed},
	} {
		if got := JudgeCursor(Sent{Status: c.status, Body: []byte(c.body)}, c.err); got != c.want {
			t.Errorf("%d %s: %v, want %v", c.status, c.body, got, c.want)
		}
	}
}

// The client never reaches a private address (with no test origin), and
// never follows a redirect.
func TestCursorClientRefusesPrivateAndRedirects(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		http.Redirect(w, r, "https://example.com/", http.StatusFound)
	}))
	defer srv.Close()
	h := CursorHook{URL: srv.URL + CursorPath + cursorID, Key: "crsr_" + strings.Repeat("k", 64)}
	// No test origin: the URL itself is refused, before any packet.
	if _, err := PostCursor(context.Background(), Client(), h, "", EventsBody("A", "b", nil)); !errors.Is(err, ErrBadCursorURL) {
		t.Fatalf("posted to %s: %v", srv.URL, err)
	}
	// Even a URL that passed: the dialer refuses loopback.
	req, _ := http.NewRequest("POST", srv.URL, nil)
	if _, err := Client().Do(req); err == nil || !errors.Is(err, errPrivate) {
		t.Fatalf("reached loopback: %v", err)
	}
	if hits != 0 {
		t.Fatal("a packet reached the server")
	}
	undo := AllowTestOrigin(srv.URL)
	defer undo()
	s, err := PostCursor(context.Background(), Client(), h, "", EventsBody("A", "b", nil))
	if err != nil || s.Status != http.StatusFound || hits != 1 || JudgeCursor(s, err) != CursorFailed {
		t.Fatalf("redirect: %d %v hits=%d", s.Status, err, hits)
	}
}
