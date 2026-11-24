package agenthooks

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/wakeshift/server/internal/events"
	"github.com/wakeshift/server/internal/streams"
)

const secret = "aB3dE5fG7hJ9kL1mN3pQ5r"

// Only Capy's exact watch URL shape is accepted.
func TestCheckURL(t *testing.T) {
	good := []string{
		CapyOrigin + CapyPath + secret,
		CapyOrigin + CapyPath + "abc_DEF-0123456789",
		CapyOrigin + CapyPath + strings.Repeat("a", 128),
	}
	for _, u := range good {
		if err := CheckURL(u); err != nil {
			t.Errorf("refused %q: %v", u, err)
		}
	}
	bad := []string{
		"",
		"http://api.capy.ai" + CapyPath + secret, // not https
		"https://api.capy.ai.evil.com" + CapyPath + secret,      // another host
		"https://evil.com" + CapyPath + secret,                  // another host
		"https://evil.com/?u=https://api.capy.ai" + CapyPath,    // another host
		"https://api.capy.ai@evil.com" + CapyPath + secret,      // userinfo for another host
		"https://user:pw@api.capy.ai" + CapyPath + secret,       // userinfo
		"https://api.capy.ai:443" + CapyPath + secret,           // a port
		"https://api.capy.ai:8443" + CapyPath + secret,          // a port
		"https://127.0.0.1" + CapyPath + secret,                 // an IP literal
		"https://[::1]" + CapyPath + secret,                     // an IP literal
		"https://169.254.169.254" + CapyPath + secret,           // metadata
		"https://localhost" + CapyPath + secret,                 // localhost
		"https://API.CAPY.AI" + CapyPath + secret,               // not exactly the host
		"https://api.capy.ai./webhooks/automations/" + secret,   // trailing dot
		"https://api.capy.ai/webhooks/other/" + secret,          // another path
		"https://api.capy.ai/webhooks/automations/",             // no secret
		"https://api.capy.ai/webhooks/automations/short",        // too short
		"https://api.capy.ai" + CapyPath + secret + "/",         // trailing slash
		"https://api.capy.ai" + CapyPath + secret + "/../x",     // dot segments
		"https://api.capy.ai" + CapyPath + secret + "?a=b",      // query
		"https://api.capy.ai" + CapyPath + secret + "?",         // empty query
		"https://api.capy.ai" + CapyPath + secret + "#frag",     // fragment
		"https://api.capy.ai" + CapyPath + "%2e%2e%2f" + secret, // encoded
		"https://api.capy.ai" + CapyPath + secret + ".json",     // a dot
		"https://api.capy.ai" + CapyPath + strings.Repeat("a", 129),
		"https://api.capy.ai" + CapyPath + strings.Repeat("a", 600), // overlong
		" https://api.capy.ai" + CapyPath + secret,                  // whitespace
		"https://api.capy.ai" + CapyPath + secret + "\n",            // a newline
		"//api.capy.ai" + CapyPath + secret,                         // no scheme
		"https:api.capy.ai" + CapyPath + secret,                     // opaque
	}
	for _, u := range bad {
		if err := CheckURL(u); err == nil {
			t.Errorf("accepted %q", u)
		} else if strings.Contains(err.Error(), secret) {
			t.Errorf("the error echoes the URL: %v", err)
		}
	}
}

// The test origin replaces Capy's only while it's set, and only its exact
// prefix.
func TestAllowTestOrigin(t *testing.T) {
	undo := AllowTestOrigin("http://127.0.0.1:9999")
	if err := CheckURL("http://127.0.0.1:9999" + CapyPath + secret); err != nil {
		t.Fatal(err)
	}
	if CheckURL("http://127.0.0.1:9998"+CapyPath+secret) == nil || CheckURL(CapyOrigin+CapyPath+secret) == nil {
		t.Fatal("other origins accepted while the test origin is set")
	}
	undo()
	if CheckURL("http://127.0.0.1:9999"+CapyPath+secret) == nil {
		t.Fatal("the test origin is still accepted")
	}
}

// A registration link reads back as what it was made for; tampered,
// expired or made with another key, it's refused.
func TestLinks(t *testing.T) {
	l := NewLinks([]byte("k1k1k1k1k1k1k1k1k1k1k1k1k1k1k1k1"))
	now := time.Now()
	u := l.URL("https://w.test/", "ten", "lnk", KindCapy, now.Add(time.Hour))
	if !strings.HasPrefix(u, "https://w.test"+RegisterPath+"?link=") {
		t.Fatal(u)
	}
	tok := strings.TrimPrefix(u, "https://w.test"+RegisterPath+"?link=")
	r, err := l.Parse(tok, now)
	if err != nil || r != (Registration{Tenant: "ten", Link: "lnk", Kind: KindCapy}) {
		t.Fatalf("%+v %v", r, err)
	}
	if _, err := l.Parse(tok, now.Add(2*time.Hour)); err == nil {
		t.Error("expired link accepted")
	}
	if _, err := NewLinks([]byte("other-key-other-key-other-key-00")).Parse(tok, now); err == nil {
		t.Error("another key's link accepted")
	}
	b, sig, _ := strings.Cut(tok, ".")
	if _, err := l.Parse(b+"x."+sig, now); err == nil {
		t.Error("tampered link accepted")
	}
}

// The body is the Monitor line's fields, whole while it fits; a big
// payload goes as its preview, marked truncated, within MaxBody.
func TestBody(t *testing.T) {
	f := streams.Frame{Type: "event", Topic: "ci/acme", Event: events.Event{ID: "ev1", Subject: "deploy finished", Data: json.RawMessage(`{"n":1}`),
		From: events.Party{Kind: "app", ID: "ci"}}}
	b, preview := Body(f)
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil || preview {
		t.Fatal(string(b), err)
	}
	if m["type"] != "event" || m["id"] != "ev1" || m["topic"] != "ci/acme" || m["subject"] != "deploy finished" ||
		m["trust"] != "outside data, not from the user" || m["from"] == nil || m["data"] == nil || m["truncated"] != nil {
		t.Fatalf("body: %s", b)
	}
	f.Event.Data = json.RawMessage(`{"blob":"` + strings.Repeat("x", 300<<10) + `"}`)
	b, preview = Body(f)
	m = nil
	if err := json.Unmarshal(b, &m); err != nil || !preview || len(b) > MaxBody || m["truncated"] != true || m["data_preview"] == nil || m["id"] != "ev1" {
		t.Fatalf("preview (%d bytes): %.200s %v", len(b), b, err)
	}
}

func TestBackoff(t *testing.T) {
	if Backoff(1) != BackoffBase() || Backoff(2) != 2*BackoffBase() || Backoff(100) != maxBackoff {
		t.Fatal(Backoff(1), Backoff(2), Backoff(100))
	}
}
