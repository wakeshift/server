package coretest

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wakeshift/server/internal/agenthooks"
)

// capyRegisterRE finds the registration link in a Capy subscribing result.
var capyRegisterRE = regexp.MustCompile(`curl -X POST '([^']+)' -H 'content-type: application/json'`)

// capyWebhookCanaries runs Capy's webhook route end to end for the leak
// checks: a Capy thread subscribes, registers a watch URL (on a stand-in
// for Capy, agenthooks.AllowTestOrigin) with the registration link its
// result gives, and a delivery is POSTed there. It adds to canaries the
// registration link's token and the watch URL's secret, credentials no
// exported log record or span may carry, and the token of a forged
// registration link it sends (POST and DELETE).
func capyWebhookCanaries(t *testing.T, c *Core, canaries map[string]string) {
	t.Helper()
	// 16 characters, the shortest Capy secret CheckURL takes: shorter
	// than the runs scrubText redacts on sight, so only the allowlist
	// keeps it out.
	const secret = "capyWtch5Lq8Rt2V"
	var mu sync.Mutex
	var got []string
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = append(got, r.URL.Path)
		mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
		w.Write([]byte(`{"status":"accepted"}`))
	}))
	t.Cleanup(fake.Close)
	t.Cleanup(agenthooks.AllowTestOrigin(fake.URL))

	for _, m := range []string{"POST", "DELETE"} {
		forged := "hooklnk" + strings.ToLower(m) + "7c3"
		req, _ := http.NewRequest(m, c.URL+"/v1/stream/webhook?link="+forged, strings.NewReader(`{"url":"`+fake.URL+agenthooks.CapyPath+secret+`"}`))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s with a forged registration link: %d", m, resp.StatusCode)
		}
		canaries[forged] = m + " /v1/stream/webhook (a forged registration link)"
	}

	app := c.NoteApp("capyci")
	sub, err := Subscribe(app, Caller{User: "capy-canary", Client: "capy", UserAgent: "capy"}, "capyci/acme/web", SubOpts{})
	if err != nil {
		t.Fatal(err)
	}
	m := capyRegisterRE.FindStringSubmatch(sub.Addition.Text)
	if m == nil {
		t.Fatalf("no registration command: %s", sub.Addition.Text)
	}
	link, err := url.Parse(m[1])
	if err != nil || link.Query().Get("link") == "" {
		t.Fatalf("registration link: %v", err)
	}
	canaries[link.Query().Get("link")] = "a registration link"
	canaries[secret] = "a Capy watch URL"
	body, _ := json.Marshal(map[string]string{"url": fake.URL + agenthooks.CapyPath + secret})
	resp, err := http.Post(m[1], "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("registering: %d", resp.StatusCode)
	}
	if _, err := EmitNote(app, []string{"capyci/acme/web"}, Note{Text: "deploy finished"}); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(15 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("nothing was delivered to the watch URL")
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if got[0] != agenthooks.CapyPath+secret {
		t.Fatalf("delivered to %s", got[0])
	}
}
