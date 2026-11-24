package streams_test

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/deliveries"
	"github.com/wakeshift/server/internal/streams"
)

// lines reads a one-way stream line by line.
type lines struct {
	t  *testing.T
	ch chan string
}

func openStream(t *testing.T, u string, header map[string]string) *lines {
	t.Helper()
	rctx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	req, _ := http.NewRequestWithContext(rctx, "GET", u, nil)
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("GET %s: %d", u, resp.StatusCode)
	}
	l := &lines{t: t, ch: make(chan string, 100)}
	go func() {
		defer close(l.ch)
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			l.ch <- sc.Text()
		}
	}()
	return l
}

// next returns the next line, or ok=false at the end or after a few
// seconds.
func (l *lines) next(within time.Duration) (string, bool) {
	select {
	case s, ok := <-l.ch:
		return s, ok
	case <-time.After(within):
		return "", false
	}
}

// event reads the next SSE event (skipping comments and retry), as its
// fields.
func (l *lines) event() map[string]string {
	l.t.Helper()
	ev := map[string]string{}
	for {
		s, ok := l.next(3 * time.Second)
		if !ok {
			l.t.Fatalf("stream ended or stalled; event so far %v", ev)
		}
		if s == "" {
			if len(ev) > 0 {
				return ev
			}
			continue
		}
		if strings.HasPrefix(s, ":") || strings.HasPrefix(s, "retry:") {
			continue
		}
		k, v, _ := strings.Cut(s, ": ")
		ev[k] = v
	}
}

func data(t *testing.T, ev map[string]string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(ev["data"]), &m); err != nil {
		t.Fatalf("data %q: %v", ev["data"], err)
	}
	return m
}

func subject(m map[string]any) string {
	e, _ := m["event"].(map[string]any)
	s, _ := e["subject"].(string)
	return s
}

func TestSSEDeliversWithKeepalives(t *testing.T) {
	f := setup(t)
	f.s.PingEvery = 200 * time.Millisecond
	f.slack("one")
	time.Sleep(200 * time.Millisecond)
	// Selected by Accept, as EventSource sends it.
	s := openStream(t, strings.Replace(f.link(false, time.Hour), "ws", "http", 1), map[string]string{"Accept": "text/event-stream"})
	ev := s.event()
	m := data(t, ev)
	if ev["event"] != "event" || ev["id"] != m["delivery_id"] || subject(m) != "one" {
		t.Fatalf("event %v", ev)
	}
	time.Sleep(700 * time.Millisecond) // a keepalive at least one interval after it
	if st := f.state(ev["id"]); st != "delivered" {
		t.Fatalf("a link without acks left it %s", st)
	}
	keepalive := false
	for !keepalive {
		line, ok := s.next(2 * time.Second)
		if !ok {
			t.Fatal("no keepalive")
		}
		keepalive = line == ": keepalive"
	}
	f.slack("two")
	if m := data(t, s.event()); subject(m) != "two" {
		t.Fatalf("live event %v", m)
	}
}

// Lines: one JSON line per frame, and empty lines as keepalives (Monitor
// skips them, so they don't wake the agent).
func TestLinesAreOneJSONLinePerEvent(t *testing.T) {
	f := setup(t)
	f.s.PingEvery = 100 * time.Millisecond
	f.slack("one")
	f.slack("two")
	time.Sleep(200 * time.Millisecond)
	s := openStream(t, streams.HTTPStreamURL(f.link(false, time.Hour), streams.FormatLines), nil)
	if line, ok := s.next(time.Second); !ok || line != "" {
		t.Fatalf("a lines stream should open with an empty line: %q, %v", line, ok)
	}
	for _, want := range []string{"one", "two"} {
		line, ok := s.next(3 * time.Second)
		var m map[string]any
		// A compact line for Monitor: the event's fields at the top level,
		// without a delivery ID on a link that doesn't take acks by it.
		if !ok || json.Unmarshal([]byte(line), &m) != nil || m["type"] != "event" || m["subject"] != want || m["id"] == "" || m["delivery_id"] != nil {
			t.Fatalf("line %q", line)
		}
	}
	for range 3 {
		if line, ok := s.next(time.Second); !ok || line != "" {
			t.Fatalf("a keepalive should be an empty line: %q, %v", line, ok)
		}
	}
}

// curl -sN prints the lines as they come: the exact command Monitor runs.
func TestCurlReadsLines(t *testing.T) {
	curl, err := exec.LookPath("curl")
	if err != nil {
		t.Skip("no curl")
	}
	f := setup(t)
	f.slack("hello from curl")
	time.Sleep(200 * time.Millisecond)
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cctx, curl, "-sN", streams.HTTPStreamURL(f.link(false, time.Hour), streams.FormatLines))
	out, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cancel(); cmd.Wait() }()
	sc := bufio.NewScanner(out)
	for sc.Scan() && sc.Text() == "" { // the empty line it opens with
	}
	var m map[string]any
	if err := json.Unmarshal(sc.Bytes(), &m); err != nil || m["subject"] != "hello from curl" {
		t.Fatalf("curl printed %q", sc.Text())
	}
}

func TestSSERotatesRevokesAndIsReplaced(t *testing.T) {
	f := setup(t)
	f.s.MaxAge = 300 * time.Millisecond // the connection's age limit: a session's stream hands over its next link
	link, _ := f.sessionLink("sse")
	s := openStream(t, streams.HTTPStreamURL(link, streams.FormatSSE), nil)
	ev := s.event()
	m := data(t, ev)
	next, _ := m["url"].(string)
	if ev["event"] != "hub.rotate" || !strings.HasPrefix(next, "http") || !strings.Contains(next, "format=sse") || m["link"] == "" ||
		!strings.Contains(m["note"].(string), "age limit") {
		t.Fatalf("rotation %v", ev)
	}
	select {
	case line, ok := <-s.ch:
		if ok {
			t.Fatalf("after rotating: %q", line)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the stream didn't end after rotating")
	}

	// The next link streams the same session; revoking it ends it with why.
	f.s.MaxAge = 0
	s = openStream(t, next, nil)
	time.Sleep(200 * time.Millisecond)
	f.tx(func(tx pgx.Tx) error { _, err := streams.RevokeLinks(ctx, tx, "codex"); return err })
	if ev := s.event(); ev["event"] != "hub.closed" || data(t, ev)["reason"] != "revoked" {
		t.Fatalf("after revoking: %v", ev)
	}

	// A newer connection replaces it.
	s = openStream(t, streams.HTTPStreamURL(f.link(false, time.Hour), streams.FormatSSE), nil)
	time.Sleep(200 * time.Millisecond)
	f.dial(false)
	if ev := s.event(); ev["event"] != "hub.closed" || data(t, ev)["reason"] != "replaced" {
		t.Fatalf("after a newer connection: %v", ev)
	}
}

// A reconnect with Last-Event-ID gets again what a link without acks wrote
// after that event; once it's confirmed, a reconnect without it gets
// nothing.
func TestSSEResumesFromLastEventID(t *testing.T) {
	f := setup(t)
	f.s.PingEvery = 300 * time.Millisecond
	f.slack("one")
	f.slack("two")
	time.Sleep(200 * time.Millisecond)
	u := streams.HTTPStreamURL(f.link(false, time.Hour), streams.FormatSSE)
	s := openStream(t, u, nil)
	one, two := s.event(), s.event()
	if subject(data(t, one)) != "one" || subject(data(t, two)) != "two" {
		t.Fatalf("got %v, %v", one, two)
	}
	time.Sleep(100 * time.Millisecond)

	s = openStream(t, u, map[string]string{"Last-Event-ID": one["id"]})
	again := s.event()
	// What the first connection hadn't confirmed yet and the client saw
	// (up to one) counts as delivered; what came after comes again.
	if again["id"] != two["id"] || data(t, again)["attempt"] != float64(2) {
		t.Fatalf("resumed with %v", again)
	}
	if st := f.state(one["id"]); st != "delivered" {
		t.Fatalf("what the client saw: %s", st)
	}
	time.Sleep(700 * time.Millisecond) // the second connection confirms it
	s = openStream(t, u, nil)
	if line, ok := s.next(700 * time.Millisecond); ok && line != "retry: 3000" && line != "" {
		t.Fatalf("without Last-Event-ID: %q", line)
	}
}

// Last-Event-ID confirms only what the connection wrote before that event,
// not everything with an earlier event: a delivery that became available
// late is written after later events, and if the client didn't see it, it
// comes again.
func TestSSELastEventIDDoesntConfirmLaterWrites(t *testing.T) {
	f := setup(t)
	f.slack("one")
	f.slack("two")
	time.Sleep(200 * time.Millisecond)
	var late string
	f.tx(func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `UPDATE deliveries SET available_at = now() + interval '1 hour'
			WHERE id = (SELECT id FROM deliveries ORDER BY event_seq LIMIT 1) RETURNING id::text`).Scan(&late)
	})
	u := streams.HTTPStreamURL(f.link(false, time.Hour), streams.FormatSSE)
	s := openStream(t, u, nil)
	two := s.event()
	if subject(data(t, two)) != "two" {
		t.Fatalf("first: %v", two)
	}
	f.tx(func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE deliveries SET available_at = now() WHERE id = $1`, late); err != nil {
			return err
		}
		// Streams don't poll: say so, as anything making deliveries does.
		_, err := tx.Exec(ctx, `SELECT pg_notify($1, current_tenant()::text || ':codex')`, deliveries.NotifyChannel)
		return err
	})
	if one := s.event(); one["id"] != late {
		t.Fatalf("then: %v", one)
	}
	// The client saw two, but not one, which came after it.
	s = openStream(t, u, map[string]string{"Last-Event-ID": two["id"]})
	again := s.event()
	if again["id"] != late || data(t, again)["attempt"] != float64(2) {
		t.Fatalf("resumed with %v", again)
	}
	if st := f.state(two["id"]); st != "delivered" {
		t.Fatalf("what the client saw: %s", st)
	}
}

// On an ack=true link, a lines stream's events carry their delivery ID,
// which acks them through the poll's ack endpoint.
func TestLinesWithAcksCarryDeliveryID(t *testing.T) {
	f := setup(t)
	base := f.pollServer()
	f.slack("one")
	link := f.link(true, time.Hour)
	s := openStream(t, streams.HTTPStreamURL(link, streams.FormatLines), nil)
	got := monitorLines(t, s, 1)
	id, _ := got[0]["delivery_id"].(string)
	if id == "" || id != f.deliveryOf(got[0]["id"].(string)) {
		t.Fatalf("line: %v", got[0])
	}
	b, _ := json.Marshal(map[string]any{"delivery_ids": []string{id}})
	ackReq, _ := http.NewRequest("POST", base+"/v1/stream/ack", strings.NewReader(string(b)))
	ackReq.Header.Set("Authorization", "Bearer "+token(link))
	if r, err := http.DefaultClient.Do(ackReq); err != nil || r.StatusCode != 200 {
		t.Fatalf("ack: %v %v", err, r)
	}
	if st := f.state(id); st != "acked" {
		t.Fatalf("acked: %s", st)
	}
}

// On an ack=true link, a stream with ?max= ends once what it wrote is
// acknowledged, not the moment it's written: ending at once handed it
// back before an ack could arrive, so the command started again got it
// again, an attempt each time, until it was dead. An ack that never comes
// holds it a lease at most, then it goes back as it would on a disconnect.
func TestMaxStreamWithAcksWaitsForThem(t *testing.T) {
	f := setup(t)
	f.s.PingEvery = 50 * time.Millisecond
	f.s.Lease = 2 * time.Second
	base := f.pollServer()
	f.slack("one")
	link := f.link(true, time.Hour)
	u := streams.HTTPStreamURL(link, streams.FormatLines) + "&max=1"
	ack := func(id string) {
		t.Helper()
		b, _ := json.Marshal(map[string]any{"delivery_ids": []string{id}})
		req, _ := http.NewRequest("POST", base+"/v1/stream/ack", strings.NewReader(string(b)))
		req.Header.Set("Authorization", "Bearer "+token(link))
		r, err := http.DefaultClient.Do(req)
		if err != nil || r.StatusCode != 200 {
			t.Fatalf("ack: %v %v", err, r)
		}
		r.Body.Close()
	}
	s := openStream(t, u, nil)
	got := monitorLines(t, s, 1)
	id, _ := got[0]["delivery_id"].(string)
	if id == "" {
		t.Fatalf("line: %v", got[0])
	}
	// Still open, the delivery leased, until it's acked.
	time.Sleep(300 * time.Millisecond)
	if st := f.state(id); st != "in_flight" {
		t.Fatalf("before its ack: %s", st)
	}
	ack(id)
	last := monitorLines(t, s, 1)[0]
	if last["type"] != "hub.closed" || last["reason"] != "max" || f.state(id) != "acked" {
		t.Fatalf("after its ack: %v, %s", last, f.state(id))
	}

	// Never acked: it ends after the lease, and the delivery comes again.
	f.slack("two")
	start := time.Now()
	s = openStream(t, u, nil)
	two := monitorLines(t, s, 2)
	id = two[0]["delivery_id"].(string)
	if two[1]["type"] != "hub.closed" || time.Since(start) < f.s.Lease {
		t.Fatalf("unacked: %v after %v", two, time.Since(start))
	}
	s = openStream(t, u, nil)
	if again := monitorLines(t, s, 1)[0]; again["delivery_id"] != id || again["redelivered"] != true {
		t.Fatalf("again: %v", again)
	}
}

// On an ack=true link, SSE is at least once: acks go through the poll's
// ack endpoint, and what isn't acked comes back on reconnect.
func TestSSEWithAcks(t *testing.T) {
	f := setup(t)
	base := f.pollServer()
	f.slack("acked")
	f.slack("not acked")
	time.Sleep(200 * time.Millisecond)
	link := f.link(true, time.Hour)
	u := streams.HTTPStreamURL(link, streams.FormatSSE)
	rctx, cancel := context.WithCancel(ctx)
	req, _ := http.NewRequestWithContext(rctx, "GET", u, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	s := &lines{t: t, ch: make(chan string, 100)}
	go func() {
		defer close(s.ch)
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			s.ch <- sc.Text()
		}
	}()
	first, second := s.event(), s.event()
	b, _ := json.Marshal(map[string]any{"delivery_ids": []string{first["id"]}})
	ackReq, _ := http.NewRequest("POST", base+"/v1/stream/ack", strings.NewReader(string(b)))
	ackReq.Header.Set("Authorization", "Bearer "+token(link))
	if r, err := http.DefaultClient.Do(ackReq); err != nil || r.StatusCode != 200 {
		t.Fatalf("ack: %v %v", err, r)
	}
	if st := f.state(first["id"]); st != "acked" {
		t.Fatalf("acked one: %s", st)
	}
	if st := f.state(second["id"]); st != "in_flight" {
		t.Fatalf("unacked one while connected: %s", st)
	}
	cancel()
	resp.Body.Close()
	time.Sleep(300 * time.Millisecond)
	s = openStream(t, u, nil)
	if ev := s.event(); ev["id"] != second["id"] {
		t.Fatalf("after reconnecting: %v", ev)
	}
}

// On an ack=true link only acks confirm: a reconnect's Last-Event-ID
// doesn't (the OpenClaw plugin resumes with it and acks only once OpenClaw
// has taken the message), and an acked delivery doesn't come back.
func TestSSEAckLinkIgnoresLastEventID(t *testing.T) {
	f := setup(t)
	base := f.pollServer()
	f.slack("acked")
	f.slack("seen, not acked")
	time.Sleep(200 * time.Millisecond)
	link := f.link(true, time.Hour)
	u := streams.HTTPStreamURL(link, streams.FormatSSE)
	rctx, cancel := context.WithCancel(ctx)
	req, _ := http.NewRequestWithContext(rctx, "GET", u, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	s := &lines{t: t, ch: make(chan string, 100)}
	go func() {
		defer close(s.ch)
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			s.ch <- sc.Text()
		}
	}()
	first, second := s.event(), s.event()
	b, _ := json.Marshal(map[string]any{"delivery_ids": []string{first["id"]}})
	ackReq, _ := http.NewRequest("POST", base+"/v1/stream/ack", strings.NewReader(string(b)))
	ackReq.Header.Set("Authorization", "Bearer "+token(link))
	r, err := http.DefaultClient.Do(ackReq)
	if err != nil || r.StatusCode != 200 {
		t.Fatalf("ack: %v %v", err, r)
	}
	var got struct {
		Acked []string `json:"acked"`
	}
	json.NewDecoder(r.Body).Decode(&got)
	r.Body.Close()
	if len(got.Acked) != 1 || got.Acked[0] != first["id"] {
		t.Fatalf("ack receipt: %v", got)
	}
	cancel()
	resp.Body.Close()
	time.Sleep(300 * time.Millisecond)

	s = openStream(t, u, map[string]string{"Last-Event-ID": second["id"]})
	again := s.event()
	if again["id"] != second["id"] || data(t, again)["attempt"] != float64(2) {
		t.Fatalf("after reconnecting with Last-Event-ID: %v", again)
	}
	if st := f.state(second["id"]); st != "in_flight" {
		t.Fatalf("Last-Event-ID confirmed an unacked delivery: %s", st)
	}
	if st := f.state(first["id"]); st != "acked" {
		t.Fatalf("acked one: %s", st)
	}
}

// The stream takes the link's token as Authorization: Bearer, as the poll
// and ack do, so a client can keep it out of the URL; a wrong one is
// refused.
func TestSSEWithBearer(t *testing.T) {
	f := setup(t)
	base := f.pollServer()
	f.slack("by header")
	time.Sleep(200 * time.Millisecond)
	link := f.link(true, time.Hour)
	u, _, _ := strings.Cut(link, "?")
	u = strings.Replace(u, "ws", "http", 1) + "?format=sse"
	req, _ := http.NewRequest("GET", u, nil)
	req.Header.Set("Authorization", "Bearer not-a-link")
	if r, err := http.DefaultClient.Do(req); err != nil || r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a wrong token: %v %v", err, r)
	}
	s := openStream(t, u, map[string]string{"Authorization": "Bearer " + token(link)})
	ev := s.event()
	if subject(data(t, ev)) != "by header" {
		t.Fatalf("event: %v", ev)
	}
	b, _ := json.Marshal(map[string]any{"delivery_ids": []string{ev["id"]}})
	ackReq, _ := http.NewRequest("POST", base+"/v1/stream/ack", strings.NewReader(string(b)))
	ackReq.Header.Set("Authorization", "Bearer "+token(link))
	if r, err := http.DefaultClient.Do(ackReq); err != nil || r.StatusCode != 200 {
		t.Fatalf("ack: %v %v", err, r)
	}
	if st := f.state(ev["id"]); st != "acked" {
		t.Fatalf("acked: %s", st)
	}
}

// A one-way stream sends something as soon as it connects, not first at the
// keepalive: a proxy may hold the headers until the first bytes, and the
// client counts as connected only once it has them.
func TestOneWayStreamsAnswerAtOnce(t *testing.T) {
	f := setup(t)
	f.s.PingEvery = time.Minute
	for _, format := range []string{streams.FormatLines, streams.FormatSSE} {
		start := time.Now()
		rctx, cancel := context.WithCancel(ctx)
		req, _ := http.NewRequestWithContext(rctx, "GET", streams.HTTPStreamURL(f.link(false, time.Hour), format), nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil || resp.StatusCode != 200 {
			t.Fatalf("%s: %v %v", format, err, resp)
		}
		b := make([]byte, 1)
		if _, err := io.ReadFull(resp.Body, b); err != nil {
			t.Fatalf("%s: %v", format, err)
		}
		if d := time.Since(start); d > 2*time.Second {
			t.Fatalf("%s: first byte after %v", format, d)
		}
		if format == streams.FormatLines && b[0] != '\n' {
			t.Fatalf("lines opened with %q", b)
		}
		resp.Body.Close()
		cancel()
	}
}

// A Last-Event-ID that isn't a delivery's ID resends nothing, and doesn't
// fail the connection.
func TestSSEIgnoresAMalformedLastEventID(t *testing.T) {
	f := setup(t)
	f.slack("one")
	time.Sleep(200 * time.Millisecond)
	u := streams.HTTPStreamURL(f.link(false, time.Hour), streams.FormatSSE)
	s := openStream(t, u, map[string]string{"Last-Event-ID": "not-a-uuid"})
	if ev := s.event(); subject(data(t, ev)) != "one" {
		t.Fatalf("got %v", ev)
	}
}
