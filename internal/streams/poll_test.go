package streams_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/deliveries"
	"github.com/wakeshift/server/internal/sessions"
	"github.com/wakeshift/server/internal/streams"
)

type pollBody struct {
	Deliveries []msg    `json:"deliveries"`
	Acked      []string `json:"acked"`
	Session    string   `json:"session"`
	Rotate     *struct {
		Link, URL string
		ExpiresAt string `json:"expires_at"`
	} `json:"rotate"`
	Background bool                           `json:"background"`
	Error      struct{ Code, Message string } `json:"error"`
}

// pollServer serves the poll and ack endpoints for f's streams.
func (f *fixture) pollServer() string {
	f.t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/stream/poll", f.s.ServePoll)
	mux.HandleFunc("POST /v1/stream/ack", f.s.ServeAck)
	srv := httptest.NewServer(mux)
	f.t.Cleanup(srv.Close)
	return srv.URL
}

func token(link string) string {
	u, _ := url.Parse(link)
	return u.Query().Get("link")
}

func poll(t *testing.T, base, tok, query string) (int, pollBody) {
	t.Helper()
	req, _ := http.NewRequest("GET", base+"/v1/stream/poll?"+query, nil)
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var b pollBody
	json.NewDecoder(resp.Body).Decode(&b)
	return resp.StatusCode, b
}

func mustPoll(t *testing.T, base, tok, query string) pollBody {
	t.Helper()
	code, b := poll(t, base, tok, query)
	if code != 200 {
		t.Fatalf("poll %s: %d %+v", query, code, b.Error)
	}
	return b
}

// sessionLink mints a link for a new session of codex's.
func (f *fixture) sessionLink(label string) (link, id string) {
	f.t.Helper()
	f.tx(func(tx pgx.Tx) error {
		s, err := sessions.Register(ctx, tx, sessions.New{Agent: "codex", Label: label, CreatedBy: "test"})
		if err != nil {
			return err
		}
		id = s.ID
		link, _, err = f.s.Mint(ctx, tx, f.tenant, streams.NewLink{Agent: "codex", Target: s.ID, Ack: true, CreatedBy: "test", TTL: streams.SessionLinkTTL})
		return err
	})
	return link, id
}

func TestPollDeliversAndAcks(t *testing.T) {
	f := setup(t)
	base := f.pollServer()
	f.slack("one")
	f.slack("two")
	time.Sleep(200 * time.Millisecond) // the runner queues them
	tok := token(f.link(false, time.Hour))

	b := mustPoll(t, base, tok, "wait=0")
	if len(b.Deliveries) != 2 || b.Deliveries[0].Event.Subject != "one" || b.Deliveries[1].Event.Subject != "two" || b.Deliveries[0].Attempt != 1 {
		t.Fatalf("got %+v", b.Deliveries)
	}
	// Returned isn't delivered, even on a link made without ack: a poller
	// acks.
	one, two := b.Deliveries[0].DeliveryID, b.Deliveries[1].DeliveryID
	if s := f.state(one); s != "in_flight" {
		t.Fatalf("state after a poll %s", s)
	}
	// Acks on the next poll, with a receipt; a foreign or malformed one gets none.
	b = mustPoll(t, base, tok, "wait=0&ack="+one+",00000000-0000-0000-0000-000000000000&ack=not-a-uuid")
	if len(b.Acked) != 1 || b.Acked[0] != one || f.state(one) != "acked" {
		t.Fatalf("acked %v, state %s", b.Acked, f.state(one))
	}
	// two wasn't acked: it's back (attempt 2), as on a reconnect.
	if len(b.Deliveries) != 1 || b.Deliveries[0].DeliveryID != two || b.Deliveries[0].Attempt != 2 {
		t.Fatalf("redelivery %+v", b.Deliveries)
	}
	// The ack endpoint, without polling.
	body, _ := json.Marshal(map[string]any{"delivery_ids": []string{two}})
	req, _ := http.NewRequest("POST", base+"/v1/stream/ack", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var acked struct{ Acked []string }
	json.NewDecoder(resp.Body).Decode(&acked)
	resp.Body.Close()
	if resp.StatusCode != 200 || len(acked.Acked) != 1 || f.state(two) != "acked" {
		t.Fatalf("ack endpoint: %d %v %s", resp.StatusCode, acked, f.state(two))
	}
	if b := mustPoll(t, base, tok, "wait=0"); len(b.Deliveries) != 0 {
		t.Fatalf("acked deliveries came back: %+v", b.Deliveries)
	}
}

func TestPollWaitsForANewDelivery(t *testing.T) {
	f := setup(t)
	base := f.pollServer()
	tok := token(f.link(true, time.Hour))

	start := time.Now()
	if b := mustPoll(t, base, tok, "wait=300ms"); len(b.Deliveries) != 0 || time.Since(start) < 300*time.Millisecond {
		t.Fatalf("empty poll: %+v after %v", b, time.Since(start))
	}

	f.s.Poll = time.Minute // so only the notification can answer quickly
	done := make(chan pollBody)
	go func() { done <- mustPoll(t, base, tok, "wait=10s") }()
	time.Sleep(300 * time.Millisecond)
	start = time.Now()
	f.slack("live")
	select {
	case b := <-done:
		if len(b.Deliveries) != 1 || b.Deliveries[0].Event.Subject != "live" {
			t.Fatalf("got %+v", b)
		}
		if time.Since(start) > 2*time.Second {
			t.Errorf("a live event took %v", time.Since(start))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the poll didn't answer when a delivery came")
	}
}

// What a poll returned and nobody acked goes back to pending once its lease
// expires, for whoever reads the stream next.
func TestPollRedeliversAfterTheLease(t *testing.T) {
	f := setup(t)
	f.s.Lease = 300 * time.Millisecond
	base := f.pollServer()
	f.slack("unacked")
	time.Sleep(200 * time.Millisecond)
	b := mustPoll(t, base, token(f.link(true, time.Hour)), "wait=0")
	if len(b.Deliveries) != 1 {
		t.Fatalf("got %+v", b)
	}
	first := b.Deliveries[0]
	time.Sleep(500 * time.Millisecond)
	f.tx(func(tx pgx.Tx) error { _, err := deliveries.Reap(ctx, tx); return err })
	if s := f.state(first.DeliveryID); s != "pending" {
		t.Fatalf("after the lease: %s", s)
	}
	c := f.dial(true)
	if m := mustRead(t, c); m.DeliveryID != first.DeliveryID || m.Attempt != 2 {
		t.Fatalf("redelivered %+v (first %+v)", m, first)
	}
}

func TestPollRevoked(t *testing.T) {
	f := setup(t)
	base := f.pollServer()
	tok := token(f.link(true, time.Hour))
	type result struct {
		code int
		b    pollBody
	}
	done := make(chan result)
	go func() { c, b := poll(t, base, tok, "wait=20s"); done <- result{c, b} }()
	time.Sleep(300 * time.Millisecond)
	f.tx(func(tx pgx.Tx) error { _, err := streams.RevokeLinks(ctx, tx, "codex"); return err })
	select {
	case r := <-done:
		if r.code != 401 || r.b.Error.Code != "link_revoked" {
			t.Fatalf("waiting poll after revoke: %d %+v", r.code, r.b.Error)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a waiting poll outlived its link's revocation")
	}
	if code, b := poll(t, base, tok, "wait=0"); code != 401 || b.Error.Code != "link_revoked" {
		t.Fatalf("polling a revoked link: %d %+v", code, b.Error)
	}
}

func TestPollBadRequests(t *testing.T) {
	f := setup(t)
	base := f.pollServer()
	good := token(f.link(true, time.Hour))
	expired := token(f.link(true, -time.Minute))
	for name, c := range map[string]struct {
		tok, query string
		code       int
		errCode    string
	}{
		"no link":       {"", "wait=0", 401, "invalid_link"},
		"forged":        {good[:len(good)-2] + "xx", "wait=0", 401, "invalid_link"},
		"expired":       {expired, "wait=0", 401, "link_expired"},
		"bad wait":      {good, "wait=soon", 400, "bad_request"},
		"bad limit":     {good, "wait=0&limit=0", 400, "bad_request"},
		"too many acks": {good, "wait=0&ack=" + strings.Repeat("00000000-0000-0000-0000-000000000000,", 101), 400, "bad_request"},
	} {
		if code, b := poll(t, base, c.tok, c.query); code != c.code || b.Error.Code != c.errCode {
			t.Errorf("%s: %d %+v", name, code, b.Error)
		}
	}
	// The link as ?link=, or the whole stream URL as the bearer, works too.
	if code, _ := poll(t, base, "", "wait=0&link="+url.QueryEscape(good)); code != 200 {
		t.Errorf("?link=: %d", code)
	}
	if code, _ := poll(t, base, f.s.PublicURL+"/v1/stream?link="+good, "wait=0"); code != 200 {
		t.Errorf("stream URL as the bearer: %d", code)
	}
}

// The newest poller holds the stream: an older poll still waiting answers
// 409, what an older one was handed and didn't ack goes to the newer one,
// and a WebSocket on the stream is replaced (4000).
func TestPollTakeover(t *testing.T) {
	f := setup(t)
	f.s.Poll = time.Minute // so only the takeover notice ends the old poll quickly
	base := f.pollServer()
	a, b := token(f.link(true, time.Hour)), token(f.link(true, time.Hour))
	type result struct {
		code int
		b    pollBody
	}
	done := make(chan result)
	go func() { c, body := poll(t, base, a, "wait=20s"); done <- result{c, body} }()
	time.Sleep(300 * time.Millisecond)
	start := time.Now()
	mustPoll(t, base, b, "wait=0")
	select {
	case r := <-done:
		if r.code != 409 || r.b.Error.Code != "replaced" {
			t.Fatalf("older poll: %d %+v", r.code, r.b.Error)
		}
		if time.Since(start) > 2*time.Second {
			t.Errorf("takeover took %v", time.Since(start))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the older poll kept waiting")
	}

	f.slack("in flight")
	time.Sleep(200 * time.Millisecond)
	first := mustPoll(t, base, a, "wait=2s").Deliveries
	again := mustPoll(t, base, b, "wait=2s").Deliveries
	if len(first) != 1 || len(again) != 1 || again[0].DeliveryID != first[0].DeliveryID || again[0].Attempt != 2 {
		t.Fatalf("after takeover: %+v (first %+v)", again, first)
	}

	ws := f.dial(true)
	if m := mustRead(t, ws); m.DeliveryID != first[0].DeliveryID {
		t.Fatalf("stream got %+v", m)
	}
	mustPoll(t, base, a, "wait=0")
	if _, err := read(t, ws); websocket.CloseStatus(err) != streams.CodeReplaced {
		t.Fatalf("stream after a poll took over: %v", err)
	}
}

// A session's link rotates in a poll's answer (same session), and polling
// counts as connected.
func TestPollRotatesAndIsPresence(t *testing.T) {
	f := setup(t)
	base := f.pollServer()
	link, id := f.sessionLink("poller")
	b := mustPoll(t, base, token(link), "wait=0")
	if b.Session != "codex@poller" || b.Rotate != nil {
		t.Fatalf("far from expiry: %+v", b)
	}
	var s sessions.Session
	f.tx(func(tx pgx.Tx) (err error) { s, err = sessions.Get(ctx, tx, id); return err })
	if !s.Connected || s.ConnectedVia != "poll" {
		t.Fatalf("after a poll: connected %v via %q", s.Connected, s.ConnectedVia)
	}

	f.s.RotateLead = streams.SessionLinkTTL // rotate at once
	b = mustPoll(t, base, token(link), "wait=0")
	if b.Rotate == nil || b.Rotate.Link == "" || token(b.Rotate.URL) != b.Rotate.Link || b.Rotate.Link == token(link) {
		t.Fatalf("near expiry: %+v", b.Rotate)
	}
	if next := mustPoll(t, base, b.Rotate.Link, "wait=0"); next.Session != "codex@poller" {
		t.Fatalf("polling the next link: %+v", next)
	}
	// A stream connection shows as one.
	c, _, err := websocket.Dial(ctx, link, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	time.Sleep(200 * time.Millisecond)
	f.tx(func(tx pgx.Tx) (err error) { s, err = sessions.Get(ctx, tx, id); return err })
	if s.ConnectedVia != "stream" {
		t.Fatalf("with a stream: via %q", s.ConnectedVia)
	}
}

// Draining answers a waiting poll at once (empty), so the client polls
// again elsewhere.
func TestPollDrain(t *testing.T) {
	f := setup(t)
	base := f.pollServer()
	tok := token(f.link(true, time.Hour))
	done := make(chan int)
	go func() { c, _ := poll(t, base, tok, "wait=20s"); done <- c }()
	time.Sleep(300 * time.Millisecond)
	go f.s.Drain(ctx)
	select {
	case code := <-done:
		if code != 200 {
			t.Fatalf("drained poll: %d", code)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("drain didn't end the poll")
	}
	if code, b := poll(t, base, tok, "wait=0"); code != 503 || b.Error.Code != "restarting" {
		t.Fatalf("poll while draining: %d %+v", code, b.Error)
	}
}
