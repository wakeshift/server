package coretest_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wakeshift/server/internal/coretest"
	"github.com/wakeshift/server/pkg/wakeshift"
)

// Several apps whose callbacks answer the challenge, then hold every
// delivery for seconds (under the timeout, so it isn't retried later), want
// more than an instance's send slots between them. Another app's delivery
// still goes at once: the busy apps don't take the slots kept for apps
// with little to send, and a slot that frees goes to the app holding
// fewest.
func TestSlowAppsDontStallAnotherAppsDelivery(t *testing.T) {
	if testing.Short() {
		t.Skip("holds callbacks for seconds")
	}
	eventsTestSetup(t)
	c := coretest.Start(t)
	const hold = 6 * time.Second
	var slowHits atomic.Int64
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var ctl struct{ Type, Challenge string }
		json.Unmarshal(b, &ctl)
		if ctl.Type == "verification" {
			json.NewEncoder(w).Encode(map[string]string{"challenge": ctl.Challenge})
			return
		}
		slowHits.Add(1)
		select {
		case <-time.After(hold):
		case <-r.Context().Done():
		}
		w.WriteHeader(202)
	}))
	t.Cleanup(slow.Close)
	hook := newFakeChatGPT(t)
	victim := eventsApp(t, c, "chat")
	if code, e, _, _ := webhookSub(t, c, victim, "realuser", "eng", hook.url(), hook.secret()); code != 200 {
		t.Fatalf("victim sub: %d %s", code, e)
	}
	emit := func(app *wakeshift.Client, channel, text string) {
		t.Helper()
		if code, e := appAPI(t, c, app, "/api/apps/v1/events/emit", map[string]any{"name": "chat.message", "arguments": map[string]any{"channel": channel},
			"payload": map[string]any{"text": text, "from": "x"}, "text": "a message"}, nil); code != 200 {
			t.Fatalf("emit: %d %s", code, e)
		}
	}
	waitGot := func(n int, start time.Time) time.Duration {
		t.Helper()
		for len(hook.got()) < n && time.Since(start) < 60*time.Second {
			time.Sleep(10 * time.Millisecond)
		}
		if len(hook.got()) < n {
			t.Fatalf("the victim's delivery %d never came", n)
		}
		return time.Since(start)
	}
	start := time.Now()
	emit(victim, "eng", "baseline")
	t.Logf("the victim's latency, unloaded: %v", waitGot(1, start))

	// 6 apps x 2 users x 6 webhooks: 48 slots' worth of slow sends.
	const apps = 6
	secret := hook.secret()
	for a := range apps {
		app := eventsApp(t, c, fmt.Sprint("evil", a))
		for u := range 2 {
			for ch := range 6 {
				if code, e, _, _ := webhookSub(t, c, app, fmt.Sprint("u", u), fmt.Sprint("c", ch), slow.URL+fmt.Sprintf("/a%d/u%d/c%d", a, u, ch), secret); code != 200 {
					t.Fatalf("slow app's sub: %d %s", code, e)
				}
			}
		}
		for ch := range 6 {
			for range 3 {
				emit(app, fmt.Sprint("c", ch), "x")
			}
		}
	}
	time.Sleep(time.Second)
	if slowHits.Load() == 0 {
		t.Fatal("the slow apps' sends haven't started")
	}
	for i := range 4 {
		start = time.Now()
		emit(victim, "eng", fmt.Sprint("loaded", i))
		took := waitGot(2+i, start)
		t.Logf("the victim's latency with the slow apps sending: %v (slow sends so far %d)", took, slowHits.Load())
		if took > 2*time.Second {
			t.Errorf("the victim's delivery %d waited %v on the slow apps' sends", i, took)
		}
		time.Sleep(hold / 3)
	}
}
