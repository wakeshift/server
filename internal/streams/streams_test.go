package streams_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/agents"
	"github.com/wakeshift/server/internal/db"
	"github.com/wakeshift/server/internal/dbtest"
	"github.com/wakeshift/server/internal/deliveries"
	"github.com/wakeshift/server/internal/events"
	"github.com/wakeshift/server/internal/notify"
	"github.com/wakeshift/server/internal/pipeline"
	"github.com/wakeshift/server/internal/streams"
	"github.com/wakeshift/server/internal/subscriptions"
)

var ctx = context.Background()

type fixture struct {
	t      *testing.T
	d      *db.DB
	tenant string
	s      *streams.Server
	n      int
}

func setup(t *testing.T) *fixture {
	t.Helper()
	d := dbtest.New(t)
	tenant := dbtest.NewTenant(t, d)
	f := &fixture{t: t, d: d, tenant: tenant}
	f.tx(func(tx pgx.Tx) error {
		agents.Create(ctx, tx, "codex", "")
		agents.Create(ctx, tx, "claude-code", "")
		_, err := subscriptions.Create(ctx, tx, subscriptions.New{AgentID: "codex", EventType: "slack.message", CreatedBy: "test"})
		return err
	})
	runCtx, stop := context.WithCancel(ctx)
	t.Cleanup(stop)
	hub := notify.New(d.Pool, deliveries.NotifyChannel, streams.TakeoverChannel, streams.RevokedChannel)
	go hub.Run(runCtx)
	go (&pipeline.Runner{DB: d, Poll: 50 * time.Millisecond}).Run(runCtx)
	f.s = &streams.Server{DB: d, Key: []byte(strings.Repeat("k", 32)), Notify: hub, Lease: time.Minute, Poll: 100 * time.Millisecond}
	srv := httptest.NewServer(f.s)
	t.Cleanup(srv.Close)
	f.s.PublicURL = srv.URL
	time.Sleep(100 * time.Millisecond) // let the listeners start
	return f
}

func (f *fixture) tx(fn func(tx pgx.Tx) error) {
	f.t.Helper()
	if err := f.d.InTenant(ctx, f.tenant, fn); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) slack(text string) {
	f.t.Helper()
	f.n++
	f.tx(func(tx pgx.Tx) error {
		_, _, err := events.Append(ctx, tx, events.Event{
			Type: "slack.message", Source: "slack", SourceEventID: fmt.Sprint("ev", f.n), Subject: text,
			Trust: events.External, From: events.Party{Kind: "slack_user", ID: "U1"},
		})
		return err
	})
}

func (f *fixture) link(ack bool, ttl time.Duration) string {
	f.t.Helper()
	var u string
	f.tx(func(tx pgx.Tx) (err error) {
		u, _, err = f.s.Mint(ctx, tx, f.tenant, streams.NewLink{Agent: "codex", Ack: ack, CreatedBy: "test", TTL: ttl})
		return err
	})
	return u
}

func (f *fixture) dial(ack bool) *websocket.Conn {
	f.t.Helper()
	u := f.link(ack, time.Hour)
	c, _, err := websocket.Dial(ctx, u, nil)
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { c.CloseNow() })
	return c
}

type msg struct {
	Type       string
	DeliveryID string `json:"delivery_id"`
	Attempt    int    `json:"attempt"`
	Event      events.Event
}

// read is the stream's next message, waiting up to 10s for it: what a test
// waits for comes much sooner, but a CI runner under load (go test -race
// across every package) can take seconds.
func read(t *testing.T, c *websocket.Conn) (msg, error) {
	t.Helper()
	return readWithin(t, c, 10*time.Second)
}

// readWithin is read, waiting up to d (for a test that expects nothing).
func readWithin(t *testing.T, c *websocket.Conn, d time.Duration) (msg, error) {
	t.Helper()
	rctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	_, b, err := c.Read(rctx)
	var m msg
	if err == nil {
		json.Unmarshal(b, &m)
	}
	return m, err
}

func mustRead(t *testing.T, c *websocket.Conn) msg {
	t.Helper()
	m, err := read(t, c)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func (f *fixture) state(id string) string {
	var s string
	f.tx(func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT state FROM deliveries WHERE id = $1`, id).Scan(&s)
	})
	return s
}

// deliveryOf is the delivery of an event (a lines stream without acks
// names only the event).
func (f *fixture) deliveryOf(eventID string) string {
	var id string
	f.tx(func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT id FROM deliveries WHERE event_id = $1`, eventID).Scan(&id)
	})
	return id
}

func TestDeliversInOrder(t *testing.T) {
	f := setup(t)
	f.s.PingEvery = 100 * time.Millisecond
	f.slack("one")
	f.slack("two")
	c := f.dial(false)
	if a, b := mustRead(t, c), mustRead(t, c); a.Event.Subject != "one" || b.Event.Subject != "two" || a.Type != "event" || a.Attempt != 1 {
		t.Fatalf("got %+v, %+v", a, b)
	}
	start := time.Now()
	f.slack("three")
	m := mustRead(t, c)
	if m.Event.Subject != "three" {
		t.Fatalf("got %+v", m)
	}
	if time.Since(start) > time.Second {
		t.Errorf("a live event took %v", time.Since(start))
	}
	// Without acks, written isn't delivered yet: a keepalive written at
	// least one interval later has to go through (the pong came back) first.
	if s := f.state(m.DeliveryID); s != "in_flight" {
		t.Fatalf("state right after writing %s", s)
	}
	// Keepalives every 100ms confirm it within a few intervals. A fixed
	// wait of four intervals failed under CI's load (go test -race across
	// every package), so wait for it, well inside the lease (a minute) and
	// the renewal (30s), the only other things that could change its state.
	c.CloseRead(ctx) // keep reading, so pings get their pongs
	waitFor(t, 5*time.Second, "keepalives confirm what was written", func() bool {
		return f.state(m.DeliveryID) == "delivered"
	})
}

func TestAcksAndRedelivery(t *testing.T) {
	f := setup(t)
	f.slack("needs an ack")
	c := f.dial(true)
	first := mustRead(t, c)
	c.Close(websocket.StatusNormalClosure, "") // gone without acking

	c = f.dial(true)
	again := mustRead(t, c)
	if again.DeliveryID != first.DeliveryID || again.Attempt != 2 {
		t.Fatalf("redelivery %+v after %+v", again, first)
	}
	ack, _ := json.Marshal(map[string]string{"type": "ack", "delivery_id": again.DeliveryID})
	c.Write(ctx, websocket.MessageText, ack)
	if receipt := mustRead(t, c); receipt.Type != "acked" || receipt.DeliveryID != again.DeliveryID {
		t.Fatalf("receipt %+v", receipt)
	}
	if s := f.state(again.DeliveryID); s != "acked" {
		t.Fatalf("state %s", s)
	}
	c.Close(websocket.StatusNormalClosure, "")
	c = f.dial(true)
	if m, err := readWithin(t, c, 3*time.Second); err == nil {
		t.Fatalf("an acked delivery came back: %+v", m)
	}
}

func TestNewestConnectionWins(t *testing.T) {
	f := setup(t)
	old := f.dial(false)
	time.Sleep(100 * time.Millisecond)
	newer := f.dial(false)
	if _, err := read(t, old); websocket.CloseStatus(err) != streams.CodeReplaced {
		t.Fatalf("old connection: %v", err)
	}
	f.slack("for the newer one")
	if m := mustRead(t, newer); m.Event.Subject != "for the newer one" {
		t.Fatalf("got %+v", m)
	}
}

func TestBadLinks(t *testing.T) {
	f := setup(t)
	good := f.link(false, time.Hour)
	expired := f.link(false, -time.Minute)
	other := &streams.Server{Key: []byte(strings.Repeat("x", 32)), PublicURL: f.s.PublicURL}
	var forged string
	f.tx(func(tx pgx.Tx) (err error) {
		forged, _, err = other.Mint(ctx, tx, f.tenant, streams.NewLink{Agent: "codex", CreatedBy: "test", TTL: time.Hour})
		return err
	})
	for name, u := range map[string]string{"expired": expired, "forged": forged, "missing": strings.Split(good, "?")[0]} {
		_, resp, err := websocket.Dial(ctx, u, nil)
		if err == nil || resp == nil || resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s link: %v", name, err)
		}
	}
}

func TestDrainReleasesAndAsksClientsToReconnect(t *testing.T) {
	f := setup(t)
	f.slack("in flight")
	c := f.dial(true)
	m := mustRead(t, c)
	dctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	go f.s.Drain(dctx)
	if _, err := read(t, c); websocket.CloseStatus(err) != websocket.StatusServiceRestart {
		t.Fatalf("drain: %v", err)
	}
	f.s.Drain(dctx) // waits for the connection to finish
	if s := f.state(m.DeliveryID); s != "pending" {
		t.Fatalf("unacknowledged delivery left %s", s)
	}
}

// Taking a stream over returns the old connection's unacknowledged
// deliveries at once (not after the lease expires) and closes it promptly.
func TestTakeoverReleasesInFlight(t *testing.T) {
	f := setup(t)
	f.s.Poll = time.Minute // so only the takeover notice can close the old one quickly
	f.slack("in flight")
	old := f.dial(true)
	first := mustRead(t, old)
	start := time.Now()
	newer := f.dial(true)
	again := mustRead(t, newer)

	if again.DeliveryID != first.DeliveryID || again.Attempt != 2 {
		t.Fatalf("after takeover: %+v (first %+v)", again, first)
	}
	if _, err := read(t, old); websocket.CloseStatus(err) != streams.CodeReplaced {
		t.Fatalf("old connection: %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Errorf("takeover took %v", time.Since(start))
	}
}

// An ack for a delivery that isn't this consumer's gets no receipt.
func TestForeignAckGetsNoReceipt(t *testing.T) {
	f := setup(t)
	c := f.dial(true)
	other, _ := json.Marshal(map[string]string{"type": "ack", "delivery_id": "00000000-0000-0000-0000-000000000000"})
	c.Write(ctx, websocket.MessageText, other)
	rctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	if _, b, err := c.Read(rctx); err == nil {
		t.Fatalf("got %s", b)
	}
}

func TestRevokingClosesStreams(t *testing.T) {
	f := setup(t)
	u := f.link(false, time.Hour)
	c, _, err := websocket.Dial(ctx, u, nil)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	f.tx(func(tx pgx.Tx) error {
		_, err := streams.RevokeLinks(ctx, tx, "codex")
		return err
	})
	if _, err := read(t, c); websocket.CloseStatus(err) != streams.CodeRevoked {
		t.Fatalf("open stream after revoke: %v", err)
	}
	if _, resp, err := websocket.Dial(ctx, u, nil); err == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("connecting with a revoked link: %v", err)
	}
}

// A link stops working when the person who made it leaves the workspace.
func TestLinksEndWithTheirCreatorsMembership(t *testing.T) {
	f := setup(t)
	f.s.Renew = 200 * time.Millisecond
	var user string
	f.d.Pool.QueryRow(ctx, `INSERT INTO users (email) VALUES ('ada@example.com') RETURNING id`).Scan(&user)
	f.d.Pool.Exec(ctx, `INSERT INTO memberships (tenant_id, user_id, role) VALUES ($1, $2, 'member')`, f.tenant, user)
	var u string
	f.tx(func(tx pgx.Tx) (err error) {
		u, _, err = f.s.Mint(ctx, tx, f.tenant, streams.NewLink{Agent: "codex", CreatedBy: "user:" + user, TTL: time.Hour})
		return err
	})
	c, _, err := websocket.Dial(ctx, u, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.d.Pool.Exec(ctx, `DELETE FROM memberships WHERE user_id = $1`, user)
	if _, err := read(t, c); websocket.CloseStatus(err) != streams.CodeRevoked {
		t.Fatalf("after the creator left: %v", err)
	}
}

// A connected consumer that's slow to ack keeps its lease: the delivery
// isn't sent again (or reaped) while the connection is alive.
//
// Everything is checked while the connection is open. (A websocket Read
// whose context runs out closes the connection, and the server then
// releases the delivery to pending: checking its state after a timed read
// raced that release, and failed as "state pending".) The test ends once
// the delivery has outlived twice what its claim leased it for, by the
// database's clock, still in flight on its first attempt and leased past
// what the claim gave it: only renewals could have kept it.
func TestLiveConnectionsRenewLeases(t *testing.T) {
	f := setup(t)
	const lease = 2 * time.Second
	f.s.Lease, f.s.Renew = lease, 100*time.Millisecond
	f.slack("slow to ack")
	c := f.dial(true)
	first := mustRead(t, c)
	// The claim came before this, so it leased the delivery until start+lease at most.
	var start time.Time
	if err := f.d.Pool.QueryRow(ctx, `SELECT now()`).Scan(&start); err != nil {
		t.Fatal(err)
	}

	// Anything sent again, read without a deadline (see above).
	again := make(chan []byte, 1)
	go func() {
		if _, b, err := c.Read(ctx); err == nil {
			again <- b
		}
	}()
	stop, stopped := make(chan struct{}), make(chan struct{})
	go func() { // what the runner does every poll
		defer close(stopped)
		for {
			select {
			case <-stop:
				return
			case <-time.After(100 * time.Millisecond):
				f.d.InTenant(ctx, f.tenant, func(tx pgx.Tx) error { _, err := deliveries.Reap(ctx, tx); return err })
			}
		}
	}()
	defer func() { close(stop); <-stopped }()

	// The database's clock passing start+2*lease ends the test; guard only
	// keeps a broken run from hanging.
	guard := time.After(time.Minute)
	for {
		var state string
		var attempts int
		var renewed, outlived bool
		f.tx(func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT state, attempts, coalesce(lease_until > $2::timestamptz + $3::interval, false),
				now() > $2::timestamptz + 2 * $3::interval
				FROM deliveries WHERE id = $1`, first.DeliveryID, start, lease.String()).Scan(&state, &attempts, &renewed, &outlived)
		})
		if state != "in_flight" || attempts != 1 {
			t.Fatalf("while connected: state %s, attempt %d", state, attempts)
		}
		if outlived {
			if !renewed {
				t.Fatal("in flight past its lease without a renewal")
			}
			break
		}
		// A resend is a new claim, committed (attempt 2) before it's
		// written: the check above sees it first, so no wait is needed here.
		select {
		case b := <-again:
			t.Fatalf("sent again while connected: %s (first %+v)", b, first)
		case <-guard:
			t.Fatal("the database's clock didn't pass two leases")
		case <-time.After(50 * time.Millisecond):
		}
	}
	select {
	case b := <-again:
		t.Fatalf("sent again while connected: %s (first %+v)", b, first)
	default:
	}
}
