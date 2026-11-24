package streams_test

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/deliveries"
	"github.com/wakeshift/server/internal/streams"
)

// waitServer serves the wait, the poll and the stream for f's links.
func (f *fixture) waitServer() string {
	f.t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/stream/wait", f.s.ServeWait)
	mux.HandleFunc("GET /v1/stream/poll", f.s.ServePoll)
	mux.Handle("GET /v1/stream", f.s)
	srv := httptest.NewServer(mux)
	f.t.Cleanup(srv.Close)
	return srv.URL
}

// onServer is a URL from the core (PublicURL) on base instead.
func onServer(base, u string) string {
	p, _ := url.Parse(u)
	b, _ := url.Parse(base)
	p.Scheme, p.Host = b.Scheme, b.Host
	return p.String()
}

// waited is what one wait wrote: its JSON lines (empty keepalives
// skipped), the last one apart.
type waited struct {
	code    int
	events  []map[string]any
	cursors []string // the cursor lines' after
	last    map[string]any
	blanks  int
}

// waitClient bounds every wait a test makes: a stuck one fails the test
// rather than hanging it.
var waitClient = &http.Client{Timeout: time.Minute}

func wait(t *testing.T, u string) waited {
	t.Helper()
	resp, err := waitClient.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	w := waited{code: resp.StatusCode}
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		if sc.Text() == "" {
			w.blanks++
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("line %q: %v", sc.Text(), err)
		}
		if m["type"] == "event" {
			w.events = append(w.events, m)
		} else if m["type"] == "cursor" {
			if len(w.events) == 0 || sc.Text() != `{"type":"cursor","after":"`+m["after"].(string)+`"}` {
				t.Fatalf("a cursor line %q after %d events", sc.Text(), len(w.events))
			}
			w.cursors = append(w.cursors, m["after"].(string))
		} else {
			if w.last != nil {
				t.Fatalf("two last lines: %v then %v", w.last, m)
			}
			w.last = m
		}
	}
	if w.last == nil {
		t.Fatalf("no last line (%d events)", len(w.events))
	}
	return w
}

func waitURL(base, link, query string) string {
	u := onServer(base, streams.WaitURL(link))
	if query != "" {
		u += "&" + query
	}
	return u
}

// A wait returns what's waiting as lines, marked as outside data, then a
// cursor line naming them and a last line saying to run it again, with
// the same url; the next wait with after=<cursor> confirms exactly them.
// So the command is the same every time (the cursor is a shell variable).
func TestWaitReturnsAndTheCursorConfirms(t *testing.T) {
	f := setup(t)
	base := f.waitServer()
	link, id := f.sessionLinkFor("agy", streams.SessionLinkTTL)
	f.toSession(id, "one")
	f.toSession(id, "two")

	first := waitURL(base, link, "timeout=300ms")
	w := wait(t, first)
	if w.code != 200 || len(w.events) != 2 || w.events[0]["subject"] != "one" || w.events[1]["subject"] != "two" {
		t.Fatalf("got %d %v", w.code, w.events)
	}
	next, _ := w.last["url"].(string)
	if w.events[0]["trust"] != "outside data, not from the user" || w.events[0]["delivery_id"] != nil || w.last["type"] != "hub.next" || !strings.Contains(w.last["note"].(string), "confirms") ||
		onServer(base, next) != first || len(w.cursors) != 1 {
		t.Fatalf("lines %v, cursors %v, last %v (ran %s)", w.events[0], w.cursors, w.last, first)
	}
	cursor := w.cursors[0]
	one, two := f.deliveryOf(w.events[0]["id"].(string)), f.deliveryOf(w.events[1]["id"].(string))
	if f.state(one) != "in_flight" || f.state(two) != "in_flight" {
		t.Fatalf("returned is delivered already: %s %s", f.state(one), f.state(two))
	}
	// Running it again with the cursor confirms them, and waits for more.
	start := time.Now()
	w = wait(t, first+"&after="+cursor)
	if len(w.events) != 0 || len(w.cursors) != 0 || w.last["type"] != "hub.next" || !strings.HasPrefix(w.last["note"].(string), "Nothing arrived.") || time.Since(start) < 300*time.Millisecond {
		t.Fatalf("second wait: %v %v", w.events, w.last)
	}
	if f.state(one) != "acked" || f.state(two) != "acked" {
		t.Fatalf("not confirmed by the cursor: %s %s", f.state(one), f.state(two))
	}
	if u := onServer(base, w.last["url"].(string)); u != first {
		t.Fatalf("after nothing, the next url: %s", u)
	}

	// The url keeps until=, and the cursor's batch is confirmed only by its
	// cursor (or the earlier versions' ack=): a cursor that names no batch,
	// or another batch's, confirms nothing else.
	if u := wait(t, waitURL(base, link, "timeout=100ms&until=hooks")).last["url"].(string); !strings.Contains(u, "until=hooks") || !strings.Contains(u, "timeout=100ms") {
		t.Fatalf("until= lost: %s", u)
	}
	f.toSession(id, "three")
	w = wait(t, first)
	three := f.deliveryOf(w.events[0]["id"].(string))
	for _, after := range []string{"nope", cursor, "0000000000000000"} {
		if w := wait(t, first+"&after="+after); w.code != 200 || f.state(three) == "acked" {
			t.Fatalf("after=%s: %d %v %s", after, w.code, w.last, f.state(three))
		}
	}
	// (each wait above took the stream, handing three back: it's pending.)
	w = wait(t, first)
	if len(w.events) != 1 || w.events[0]["redelivered"] != true {
		t.Fatalf("three again: %v", w.events)
	}
	if wait(t, first+"&ack="+w.cursors[0]); f.state(three) != "acked" {
		t.Fatalf("ack=<cursor>: %s", f.state(three))
	}
}

// A wait without a cursor (a command just started: an earlier copy may
// have died before it printed its last answer) confirms nothing: what an
// earlier wait returned comes again at once, marked as a redelivery. With
// confirm=1, a wait only confirms (a command that ends confirms what it
// printed before it exits), and answers at once without taking the stream.
func TestWaitWithoutACursorConfirmsNothing(t *testing.T) {
	f := setup(t)
	base := f.waitServer()
	link, id := f.sessionLinkFor("claude", streams.SessionLinkTTL)
	f.toSession(id, "into the void")
	lost := wait(t, waitURL(base, link, "timeout=1s")).events[0]
	w := wait(t, waitURL(base, link, "timeout=1s"))
	if len(w.events) != 1 || w.events[0]["id"] != lost["id"] || w.events[0]["redelivered"] != true || len(w.cursors) != 1 {
		t.Fatalf("a wait without a cursor: %v", w.events)
	}
	start := time.Now()
	c := wait(t, waitURL(base, link, "after="+w.cursors[0]+"&confirm=1&timeout=20s"))
	if c.code != 200 || len(c.events) != 0 || c.last["type"] != "hub.next" || time.Since(start) > 2*time.Second {
		t.Fatalf("confirm: %d %v after %v", c.code, c.last, time.Since(start))
	}
	if s := f.state(f.deliveryOf(lost["id"].(string))); s != "acked" {
		t.Fatalf("confirmed: %s", s)
	}
}

// A wait answers as soon as something arrives, with keepalives meanwhile.
func TestWaitAnswersWhenSomethingArrives(t *testing.T) {
	f := setup(t)
	f.s.WaitKeepalive = 100 * time.Millisecond
	base := f.waitServer()
	link, id := f.sessionLinkFor("agy", streams.SessionLinkTTL)
	go func() {
		time.Sleep(500 * time.Millisecond)
		f.toSession(id, "later")
	}()
	start := time.Now()
	w := wait(t, waitURL(base, link, "timeout=20s"))
	if len(w.events) != 1 || w.events[0]["subject"] != "later" || time.Since(start) > 3*time.Second {
		t.Fatalf("got %v after %v", w.events, time.Since(start))
	}
	if w.blanks < 2 {
		t.Fatalf("%d keepalives in 500ms", w.blanks)
	}
}

// What a wait returned comes again if no wait comes back for it in time,
// or if something else takes the stream first.
func TestWaitRedeliversWhenNotRunAgain(t *testing.T) {
	f := setup(t)
	f.s.WaitLease = 300 * time.Millisecond
	base := f.waitServer()
	link, id := f.sessionLinkFor("agy", streams.SessionLinkTTL)
	f.toSession(id, "unconfirmed")
	first := wait(t, waitURL(base, link, "")).events[0]
	time.Sleep(500 * time.Millisecond)
	f.tx(func(tx pgx.Tx) error { _, err := deliveries.Reap(ctx, tx); return err })
	again := wait(t, waitURL(base, link, "timeout=1s")).events
	if len(again) != 1 || again[0]["id"] != first["id"] || again[0]["redelivered"] != true {
		t.Fatalf("after its lease: %v (first %v)", again, first)
	}

	// Another reader taking the stream hands it back at once.
	f.s.WaitLease = time.Minute
	f.toSession(id, "taken")
	w := wait(t, waitURL(base, link, "timeout=1s"))
	var taken string
	for _, e := range w.events {
		if e["subject"] == "taken" {
			taken = e["id"].(string)
		}
	}
	if taken == "" {
		t.Fatalf("got %v", w.events)
	}
	s := openStream(t, onServer(base, streams.HTTPStreamURL(link, streams.FormatLines)), nil)
	for {
		line, ok := s.next(3 * time.Second)
		if !ok {
			t.Fatal("a stream after the wait didn't get it again")
		}
		if line == "" {
			continue
		}
		var m map[string]any
		json.Unmarshal([]byte(line), &m)
		if m["redelivered"] != true {
			t.Fatalf("a stream after the wait: %v", m)
		}
		if m["id"] == taken {
			break
		}
	}
}

// An agent that works longer than the wait's lease before running the
// next wait still confirms what it printed: the lease running out hands
// the batch back (pending), but the cursor still names it, so the next
// wait's after= confirms it rather than returning it again.
func TestWaitCursorConfirmsAfterItsLeaseRanOut(t *testing.T) {
	f := setup(t)
	base := f.waitServer()
	link, id := f.sessionLinkFor("agy", streams.SessionLinkTTL)
	f.toSession(id, "one")
	w := wait(t, waitURL(base, link, "timeout=300ms"))
	if len(w.events) != 1 || len(w.cursors) != 1 {
		t.Fatalf("first wait: %v %v", w.events, w.cursors)
	}
	one := f.deliveryOf(w.events[0]["id"].(string))
	f.tx(func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE deliveries SET lease_until = now() - interval '1 second' WHERE id = $1`, one); err != nil {
			return err
		}
		n, err := deliveries.Reap(ctx, tx)
		if err == nil && n != 1 {
			t.Fatalf("reaped %d", n)
		}
		return err
	})
	if s := f.state(one); s != "pending" {
		t.Fatalf("after its lease: %s", s)
	}
	w = wait(t, waitURL(base, link, "timeout=300ms&after="+w.cursors[0]))
	if len(w.events) != 0 || f.state(one) != "acked" {
		t.Fatalf("after=<cursor> once the lease ran out: returned %v again, %s", w.events, f.state(one))
	}
}

// Two waits at once: the newer holds the stream, the older ends saying
// there's nothing to do.
func TestConcurrentWaits(t *testing.T) {
	f := setup(t)
	base := f.waitServer()
	link, id := f.sessionLinkFor("agy", streams.SessionLinkTTL)
	older := make(chan waited)
	go func() { older <- wait(t, waitURL(base, link, "timeout=20s")) }()
	time.Sleep(300 * time.Millisecond)
	newer := make(chan waited)
	go func() { newer <- wait(t, waitURL(base, link, "timeout=20s")) }()
	select {
	case w := <-older:
		if len(w.events) != 0 || w.last["type"] != "hub.closed" || w.last["reason"] != "replaced" {
			t.Fatalf("older wait: %v %v", w.events, w.last)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the older wait didn't end")
	}
	f.toSession(id, "for the newer")
	select {
	case w := <-newer:
		if len(w.events) != 1 || w.events[0]["subject"] != "for the newer" {
			t.Fatalf("newer wait: %v", w.events)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the newer wait didn't answer")
	}
}

// A revoked link ends a waiting wait at once, saying why; a link that
// no longer works answers 401 with the same kind of line.
func TestWaitRevoked(t *testing.T) {
	f := setup(t)
	base := f.waitServer()
	link, _ := f.sessionLinkFor("agy", streams.SessionLinkTTL)
	done := make(chan waited)
	go func() { done <- wait(t, waitURL(base, link, "timeout=20s")) }()
	time.Sleep(300 * time.Millisecond)
	f.tx(func(tx pgx.Tx) error { _, err := streams.RevokeLinks(ctx, tx, "codex"); return err })
	select {
	case w := <-done:
		if w.last["type"] != "hub.closed" || w.last["reason"] != "revoked" {
			t.Fatalf("after revoking: %v", w.last)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a wait outlived its link's revocation")
	}
	if w := wait(t, waitURL(base, link, "")); w.code != 401 || w.last["type"] != "hub.closed" || w.last["reason"] != "revoked" {
		t.Fatalf("a revoked link: %d %v", w.code, w.last)
	}
	expired, _ := f.sessionLinkFor("old", -time.Minute)
	if w := wait(t, waitURL(base, expired, "")); w.code != 401 || w.last["reason"] != "expired" || !strings.Contains(w.last["note"].(string), "can't wait for messages any more") || !strings.Contains(w.last["note"].(string), "Tell the user") || strings.Contains(w.last["note"].(string), "again") {
		t.Fatalf("an expired link: %d %v", w.code, w.last)
	}
	if w := wait(t, base+"/v1/stream/wait?link=forged"); w.code != 401 || w.last["reason"] != "invalid" {
		t.Fatalf("a forged link: %d %v", w.code, w.last)
	}
}

// A wait ending close to its link's expiry hands over the next link, as a
// wait URL; that one confirms what the last returned too.
func TestWaitRotatesNearExpiry(t *testing.T) {
	f := setup(t)
	base := f.waitServer()
	link, id := f.sessionLinkFor("agy", 5*time.Minute)
	f.toSession(id, "one")
	w := wait(t, waitURL(base, link, "timeout=10s"))
	u, _ := url.Parse(w.last["url"].(string))
	if w.last["type"] != "hub.next" || u.Path != "/v1/stream/wait" || u.Query().Get("link") == token(link) || u.Query().Has("ack") ||
		u.Query().Get("timeout") != "10s" || w.last["expires_at"] == nil {
		t.Fatalf("near expiry: %v", w.last)
	}
	one := f.deliveryOf(w.events[0]["id"].(string))
	next := strings.Replace(onServer(base, w.last["url"].(string)), "timeout=10s", "timeout=200ms", 1) + "&after=" + w.cursors[0]
	if w := wait(t, next); w.code != 200 || len(w.events) != 0 || f.state(one) != "acked" {
		t.Fatalf("the next link: %d %v %s", w.code, w.events, f.state(one))
	}
}

// A hook's poll doesn't take the stream from a live wait: it waits and
// answers with nothing, and the wait still gets what comes.
func TestPollYieldsToABackgroundCommand(t *testing.T) {
	f := setup(t)
	base := f.waitServer()
	link, id := f.sessionLinkFor("agy", streams.SessionLinkTTL)
	done := make(chan waited)
	go func() { done <- wait(t, waitURL(base, link, "timeout=20s")) }()
	time.Sleep(300 * time.Millisecond)
	start := time.Now()
	if b := mustPoll(t, base, token(link), "wait=300ms"); len(b.Deliveries) != 0 || !b.Background || time.Since(start) < 300*time.Millisecond {
		t.Fatalf("a poll beside a wait: %+v after %v", b, time.Since(start))
	}
	// A hook that asks is told at once.
	start = time.Now()
	if b := mustPoll(t, base, token(link), "wait=20s&background=stop"); len(b.Deliveries) != 0 || !b.Background || time.Since(start) > 2*time.Second {
		t.Fatalf("background=stop beside a wait: %+v after %v", b, time.Since(start))
	}
	f.toSession(id, "for the wait")
	select {
	case w := <-done:
		if len(w.events) != 1 {
			t.Fatalf("the wait after a poll: %v %v", w.events, w.last)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the wait lost the stream to a poll")
	}
	// Without a live wait, a poll takes the stream as before.
	time.Sleep(10 * time.Millisecond)
	f.tx(func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE stream_leases SET renewed_at = now() - interval '2 minutes'`)
		return err
	})
	f.toSession(id, "for the poll")
	if b := mustPoll(t, base, token(link), "wait=1s"); len(b.Deliveries) == 0 {
		t.Fatalf("a poll after the wait: %+v", b)
	}
}

// A lines stream with max= ends after that many events, once they're
// confirmed, handing over the next link with the same max.
func TestLinesStreamMax(t *testing.T) {
	f := setup(t)
	f.s.PingEvery = 100 * time.Millisecond
	base := f.waitServer()
	link, id := f.sessionLinkFor("cursor", streams.SessionLinkTTL)
	for _, s := range []string{"one", "two", "three"} {
		f.toSession(id, s)
	}
	u := onServer(base, streams.HTTPStreamURL(link, streams.FormatLines)) + "&max=2"
	s := openStream(t, u, nil)
	var got []map[string]any
	var last map[string]any
	for {
		line, ok := s.next(3 * time.Second)
		if !ok {
			break
		}
		if line == "" {
			continue
		}
		var m map[string]any
		json.Unmarshal([]byte(line), &m)
		if m["type"] == "event" {
			got = append(got, m)
		} else {
			last = m
		}
	}
	if len(got) != 2 || got[0]["subject"] != "one" || got[1]["subject"] != "two" {
		t.Fatalf("events %v", got)
	}
	if last["type"] != "hub.rotate" || !strings.Contains(last["url"].(string), "max=2") || !strings.Contains(last["url"].(string), "format=lines") || !strings.Contains(last["note"].(string), "2 messages") {
		t.Fatalf("last line %v", last)
	}
	for _, m := range got {
		if st := f.state(f.deliveryOf(m["id"].(string))); st != "delivered" {
			t.Fatalf("%v: %s", m["subject"], st)
		}
	}
	s = openStream(t, onServer(base, last["url"].(string)), nil)
	for {
		line, ok := s.next(3 * time.Second)
		if !ok {
			t.Fatal("the third didn't come on the next link")
		}
		if strings.Contains(line, `"three"`) {
			break
		}
	}
	for _, bad := range []string{"max=0", "max=1001", "max=x"} {
		resp, err := http.Get(onServer(base, streams.HTTPStreamURL(link, streams.FormatLines)) + "&" + bad)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 400 {
			t.Errorf("%s: %d", bad, resp.StatusCode)
		}
	}
}

// connected is whether a session's stream counts as connected (as
// sessions read it).
func (f *fixture) connected(target string) bool {
	var ok bool
	f.tx(func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM stream_leases WHERE target = $1 AND renewed_at > now() - interval '1 minute')`, target).Scan(&ok)
	})
	return ok
}

// A session counts as connected while a wait waits and for WaitGrace after
// it ends; a wait whose client goes away stops counting at once. What a
// wait returned and nobody confirmed waits for the next one.
func TestWaitPresence(t *testing.T) {
	f := setup(t)
	f.s.WaitGrace = 300 * time.Millisecond
	base := f.waitServer()
	link, id := f.sessionLinkFor("claude", streams.SessionLinkTTL)
	f.toSession(id, "unread")
	w := wait(t, waitURL(base, link, ""))
	if !f.connected(id) {
		t.Fatal("not connected right after a wait")
	}
	waitFor(t, 3*time.Second, "not connected after the grace", func() bool { return !f.connected(id) })
	d := f.deliveryOf(w.events[0]["id"].(string))

	// A client that goes away while its wait waits.
	f.tx(func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE deliveries SET state = 'acked' WHERE id = $1`, d)
		return err
	})
	rctx, cancel := context.WithCancel(ctx)
	req, _ := http.NewRequestWithContext(rctx, "GET", waitURL(base, link, "timeout=20s"), nil)
	go http.DefaultClient.Do(req)
	waitFor(t, 3*time.Second, "connected while waiting", func() bool { return f.connected(id) })
	cancel()
	waitFor(t, 3*time.Second, "not connected once the client is gone", func() bool { return !f.connected(id) })
}

// A wait whose client goes away while it's reading what's waiting (in a
// transaction, as on every tick and wake) stops counting as connected too,
// not a minute later. The read is held on the lease's row, so the client
// is certain to go while it runs.
func TestWaitGoneMidReadIsNotConnected(t *testing.T) {
	f := setup(t)
	base := f.waitServer()
	link, id := f.sessionLinkFor("claude", streams.SessionLinkTTL)
	rctx, cancel := context.WithCancel(ctx)
	defer cancel()
	req, _ := http.NewRequestWithContext(rctx, "GET", waitURL(base, link, "timeout=20s"), nil)
	done := make(chan struct{})
	go func() {
		defer close(done)
		if resp, err := http.DefaultClient.Do(req); err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}()
	waitFor(t, 3*time.Second, "connected while waiting", func() bool { return f.connected(id) })

	// Lock the lease's row, so the wait's next read blocks on it.
	locked, release, unlocked := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		unlocked <- f.d.InTenant(ctx, f.tenant, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `SELECT 1 FROM stream_leases WHERE target = $1 FOR UPDATE`, id); err != nil {
				return err
			}
			close(locked)
			<-release
			return nil
		})
	}()
	<-locked
	reading := func() bool {
		var n int
		f.d.Pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock'
			AND query LIKE '%stream_leases%FOR SHARE%'`).Scan(&n)
		return n > 0
	}
	waitFor(t, 3*time.Second, "the wait's read blocked on the lease", reading)
	cancel()
	<-done
	waitFor(t, 3*time.Second, "the read given up", func() bool { return !reading() })
	close(release)
	if err := <-unlocked; err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, "not connected once the client is gone", func() bool { return !f.connected(id) })
}
