package streams_test

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/deliveries"
	"github.com/wakeshift/server/internal/events"
	"github.com/wakeshift/server/internal/sessions"
	"github.com/wakeshift/server/internal/streams"
)

// frontEnd is a proxy that behaves like a hosting front end: it notices its
// client went away only when it next writes to it, and only then closes its
// connection to the server. Until then the server sees a healthy
// connection, and its writes go through.
func frontEnd(t *testing.T, upstream string) string {
	t.Helper()
	u, _ := url.Parse(upstream)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			up, err := net.Dial("tcp", u.Host)
			if err != nil {
				c.Close()
				continue
			}
			go func() { io.Copy(up, c) }() // the client's EOF goes nowhere
			go func() {
				io.Copy(c, up) // ends when a write to the client fails
				up.Close()
				c.Close()
			}()
		}
	}()
	return "http://" + ln.Addr().String()
}

// via is a stream URL through a proxy at base.
func via(base, stream string) string {
	u, _ := url.Parse(stream)
	b, _ := url.Parse(base)
	u.Scheme, u.Host = b.Scheme, b.Host
	return u.String()
}

// held reports whether a live connection holds codex's stream (target).
func (f *fixture) held(target string) bool {
	var live bool
	f.tx(func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT coalesce(bool_or(renewed_at > now() - interval '1 minute'), false) FROM stream_leases WHERE agent_id = 'codex' AND target = $1`, target).Scan(&live)
	})
	return live
}

func waitFor(t *testing.T, within time.Duration, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("not within %v: %s", within, what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// monitorLines reads JSON lines (skipping keepalives) until n arrive.
func monitorLines(t *testing.T, s *lines, n int) []map[string]any {
	t.Helper()
	var out []map[string]any
	for len(out) < n {
		line, ok := s.next(3 * time.Second)
		if !ok {
			t.Fatalf("got %d of %d lines: %v", len(out), n, out)
		}
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("line %q", line)
		}
		out = append(out, m)
	}
	return out
}

// The reported loss: a Monitor's curl ends, but the front end keeps the
// server's side open, so a publish was written into it, counted as
// delivered and never seen, and the session still showed as connected.
// Keepalives find the dead connection, and what it hadn't confirmed comes
// again on the next connection, marked as a redelivery.
func TestVanishedClientGetsItAgain(t *testing.T) {
	f := setup(t)
	f.s.PingEvery = 200 * time.Millisecond
	link := streams.HTTPStreamURL(f.link(false, time.Hour), streams.FormatLines)
	proxy := frontEnd(t, link)

	rctx, cancel := context.WithCancel(ctx)
	req, _ := http.NewRequestWithContext(rctx, "GET", via(proxy, link), nil)
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
	f.slack("one")
	if got := monitorLines(t, s, 1); got[0]["subject"] != "one" {
		t.Fatalf("got %v", got)
	}
	time.Sleep(600 * time.Millisecond) // confirmed by keepalives
	cancel()                           // curl exits; the front end doesn't tell the server
	resp.Body.Close()
	f.slack("two") // written into the front end, which takes it
	waitFor(t, 3*time.Second, "the dead connection is noticed", func() bool { return !f.held("") })

	again := monitorLines(t, openStream(t, link, nil), 2)
	if again[0]["subject"] != "one" || again[1]["subject"] != "two" || again[0]["redelivered"] != true || again[1]["redelivered"] != true {
		t.Fatalf("after reconnecting: %v", again)
	}
}

// The same with the real command Monitor runs, killed as Claude Code stops
// a Monitor, behind the front end.
func TestKilledCurlMissesNothing(t *testing.T) {
	curl, err := exec.LookPath("curl")
	if err != nil {
		t.Skip("no curl")
	}
	f := setup(t)
	f.s.PingEvery = 200 * time.Millisecond
	link := streams.HTTPStreamURL(f.link(false, time.Hour), streams.FormatLines)
	proxy := frontEnd(t, link)

	run := func(u string) (*exec.Cmd, chan string) {
		cmd := exec.Command(curl, "-sN", u)
		out, _ := cmd.StdoutPipe()
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
		ch := make(chan string, 100)
		go func() {
			defer close(ch)
			sc := bufio.NewScanner(out)
			for sc.Scan() {
				if sc.Text() != "" {
					ch <- sc.Text()
				}
			}
		}()
		return cmd, ch
	}
	next := func(ch chan string) map[string]any {
		t.Helper()
		select {
		case line := <-ch:
			var m map[string]any
			json.Unmarshal([]byte(line), &m)
			return m
		case <-time.After(3 * time.Second):
			t.Fatal("curl printed nothing")
		}
		return nil
	}
	cmd, ch := run(via(proxy, link))
	f.slack("before")
	if m := next(ch); m["subject"] != "before" {
		t.Fatalf("got %v", m)
	}
	time.Sleep(600 * time.Millisecond)
	cmd.Process.Kill()
	cmd.Wait()
	f.slack("while it was gone")
	waitFor(t, 3*time.Second, "the dead connection is noticed", func() bool { return !f.held("") })

	_, ch = run(link)
	seen := map[string]bool{}
	for range 2 {
		m := next(ch)
		seen[m["subject"].(string)] = true
	}
	if !seen["while it was gone"] {
		t.Fatalf("lost: got %v", seen)
	}
}

// sessionLinkFor mints a link (without acks) for a new session of codex's.
func (f *fixture) sessionLinkFor(label string, ttl time.Duration) (link, id string) {
	f.t.Helper()
	f.tx(func(tx pgx.Tx) error {
		s, err := sessions.Register(ctx, tx, sessions.New{Agent: "codex", Label: label, CreatedBy: "test"})
		if err != nil {
			return err
		}
		id = s.ID
		link, _, err = f.s.Mint(ctx, tx, f.tenant, streams.NewLink{Agent: "codex", Target: s.ID, CreatedBy: "test", TTL: ttl})
		return err
	})
	return link, id
}

// toSession queues an event for one of codex's sessions.
func (f *fixture) toSession(session, text string) {
	f.t.Helper()
	f.n++
	f.tx(func(tx pgx.Tx) error {
		var sub string
		if err := tx.QueryRow(ctx, `SELECT id::text FROM subscriptions WHERE agent_id = 'codex' LIMIT 1`).Scan(&sub); err != nil {
			return err
		}
		e, _, err := events.Append(ctx, tx, events.Event{Type: "slack.message", Source: "test", Subject: text, Trust: events.External,
			From: events.Party{Kind: "app", ID: "ci"}, Attrs: map[string]string{"topic": "ci/acme"}})
		if err != nil {
			return err
		}
		if _, err := deliveries.Insert(ctx, tx, deliveries.New{AgentID: "codex", Target: session, SubscriptionID: sub, Event: e}); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `SELECT pg_notify($1, current_tenant()::text || ':codex')`, deliveries.NotifyChannel)
		return err
	})
}

// A connection made in time outlives its link: an idle Monitor isn't woken
// to move to a new one. The link no longer opens new connections, and
// says why in a line Monitor shows.
func TestLiveConnectionOutlivesItsLink(t *testing.T) {
	f := setup(t)
	f.s.Renew = 100 * time.Millisecond
	link, id := f.sessionLinkFor("short", 2*time.Second)
	lines := streams.HTTPStreamURL(link, streams.FormatLines)
	s := openStream(t, lines, nil)
	time.Sleep(2500 * time.Millisecond) // past the link's expiry, with rechecks
	f.toSession(id, "after the link expired")
	if got := monitorLines(t, s, 1); got[0]["subject"] != "after the link expired" || got[0]["topic"] != "ci/acme" {
		t.Fatalf("got %v", got)
	}

	resp, err := http.Get(lines)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var closed struct{ Type, Reason, Note string }
	json.NewDecoder(resp.Body).Decode(&closed)
	if resp.StatusCode != http.StatusUnauthorized || closed.Type != "hub.closed" || closed.Reason != "expired" || !strings.Contains(closed.Note, "can't wait for messages any more") || strings.Contains(closed.Note, "again") {
		t.Fatalf("a new connection on the expired link: %d %+v", resp.StatusCode, closed)
	}
}

// A session's stream ends once Ending says the session has nothing left,
// and a revoked link ends with Ending's reason; neither says to get a new
// link.
func TestStreamsEndWithTheSessionsReason(t *testing.T) {
	f := setup(t)
	f.s.PingEvery = 100 * time.Millisecond
	// Here the session ends after its last delivery was confirmed, with
	// nothing written: an idle stream's renewals still notice, now and then.
	f.s.Renew = 20 * time.Millisecond
	var mu sync.Mutex
	done := false
	f.s.Ending = func(ctx context.Context, tx pgx.Tx, l streams.Link, valid bool) (streams.Ending, bool, error) {
		mu.Lock()
		defer mu.Unlock()
		if valid && !done {
			return streams.Ending{}, false, nil
		}
		return streams.Ending{Reason: "unsubscribed", Note: "Nothing more to do."}, true, nil
	}
	link, id := f.sessionLinkFor("ending", time.Hour)
	s := openStream(t, streams.HTTPStreamURL(link, streams.FormatLines), nil)
	f.toSession(id, "last one")
	monitorLines(t, s, 1)
	time.Sleep(300 * time.Millisecond)
	mu.Lock()
	done = true
	mu.Unlock()
	if m := monitorLines(t, s, 1)[0]; m["type"] != "hub.closed" || m["reason"] != "unsubscribed" || m["note"] != "Nothing more to do." {
		t.Fatalf("ended with %v", m)
	}
	if _, ok := s.next(time.Second); ok {
		t.Fatal("still open")
	}

	link, _ = f.sessionLinkFor("revoked", time.Hour)
	s = openStream(t, streams.HTTPStreamURL(link, streams.FormatLines), nil)
	time.Sleep(200 * time.Millisecond)
	f.tx(func(tx pgx.Tx) error { _, err := streams.RevokeLinks(ctx, tx, "codex"); return err })
	if m := monitorLines(t, s, 1)[0]; m["reason"] != "unsubscribed" {
		t.Fatalf("revoked: %v", m)
	}
}
