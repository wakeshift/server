package streams_test

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/deliveries"
	"github.com/wakeshift/server/internal/events"
	"github.com/wakeshift/server/internal/streams"
)

// Without acks, a write counts as delivered once a later keepalive goes
// through, with no polling or renewal to prompt it.
func TestKeepaliveConfirmsWithoutPolling(t *testing.T) {
	f := setup(t)
	f.quiet()
	f.s.PingEvery = 100 * time.Millisecond
	s := openStream(t, streams.HTTPStreamURL(f.link(false, time.Hour), streams.FormatLines), nil)
	f.slack("one")
	got := monitorLines(t, s, 1)
	ev, _ := got[0]["id"].(string)
	id := f.deliveryOf(ev)
	if s := f.state(id); s != "in_flight" {
		t.Fatalf("state right after the write: %s", s)
	}
	waitFor(t, 2*time.Second, "a keepalive confirms it", func() bool { return f.state(id) == "delivered" })
}

// Deliveries a lost connection hands back after another took the stream
// (unconfirmed ones, and ones it confirmed too late to trust) notify, so
// the stream that took over picks them up without polling.
func TestHandedBackDeliveriesWakeTheStream(t *testing.T) {
	f := setup(t)
	f.quiet()
	f.slack("in flight on a connection going away")
	first := f.queued()
	f.slack("confirmed by a lost connection")
	second := f.queued()
	for i := 0; second == first && i < 100; i++ {
		time.Sleep(30 * time.Millisecond)
		second = f.queued()
	}
	f.tx(func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE deliveries SET state = 'in_flight', attempts = 1, lease_holder = 'conn_gone', lease_until = now() + interval '1 hour' WHERE id = $1`, first); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE deliveries SET state = 'delivered', attempts = 1, finished_at = now(), lease_holder = 'conn_lost' WHERE id = $1`, second)
		return err
	})
	c := f.dial(false)
	time.Sleep(200 * time.Millisecond) // connected, with nothing to take

	f.tx(func(tx pgx.Tx) error { return deliveries.Release(ctx, tx, "conn_gone") })
	if m := mustRead(t, c); m.DeliveryID != first || m.Attempt != 2 {
		t.Fatalf("released: got %+v", m)
	}
	f.tx(func(tx pgx.Tx) error {
		n, err := deliveries.ResendLost(ctx, tx, "codex", "conn_lost", time.Minute)
		if err == nil && n != 1 {
			t.Errorf("handed back %d", n)
		}
		return err
	})
	if m := mustRead(t, c); m.DeliveryID != second || m.Attempt != 2 {
		t.Fatalf("resent: got %+v", m)
	}
}

// quiet turns off the stream's periodic checks, so only notifications and
// timers can deliver.
func (f *fixture) quiet() {
	f.s.Poll, f.s.Renew = time.Hour, time.Hour
}

// queued waits for the pipeline to queue codex's delivery, returning its ID.
func (f *fixture) queued() string {
	f.t.Helper()
	for range 100 {
		var id string
		f.d.InTenant(ctx, f.tenant, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT id FROM deliveries WHERE agent_id = 'codex' ORDER BY event_seq DESC LIMIT 1`).Scan(&id)
		})
		if id != "" {
			return id
		}
		time.Sleep(30 * time.Millisecond)
	}
	f.t.Fatal("nothing queued")
	return ""
}

// A paced delivery (available later) arrives when it comes due, on the
// stream's own timer: nothing notifies then.
//
// It mustn't arrive early. How long it has yet to wait is read on the
// database's clock as it's paced (the clock claiming compares with), and
// timed here from before then: arriving sooner than that after start means
// it went out before it was due, however slow the setup ran.
func TestPacedDeliveryArrivesWhenDue(t *testing.T) {
	f := setup(t)
	f.quiet()
	f.slack("later")
	id := f.queued()
	start := time.Now()
	var due float64 // seconds from when it was paced, on the database's clock
	f.tx(func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `UPDATE deliveries SET available_at = now() + interval '800 milliseconds' WHERE id = $1
			RETURNING extract(epoch FROM available_at - clock_timestamp())::float8`, id).Scan(&due)
	})
	wait := time.Duration(due * float64(time.Second))
	c := f.dial(false)
	m := mustRead(t, c)
	if took := time.Since(start); m.DeliveryID != id || took < wait {
		t.Fatalf("got %+v after %s, due after %s", m, took, wait)
	}
}

// A poll waiting for a paced delivery answers when it comes due.
func TestPollAnswersWhenAPacedDeliveryComesDue(t *testing.T) {
	f := setup(t)
	f.quiet()
	base := f.pollServer()
	f.slack("later")
	id := f.queued()
	f.tx(func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE deliveries SET available_at = now() + interval '800 milliseconds' WHERE id = $1`, id)
		return err
	})
	start := time.Now()
	b := mustPoll(t, base, token(f.link(true, time.Hour)), "wait=10s")
	if len(b.Deliveries) != 1 || b.Deliveries[0].DeliveryID != id || time.Since(start) > 5*time.Second {
		t.Fatalf("got %+v after %s", b, time.Since(start))
	}
}

// Reaping an expired lease notifies the agent, so its live stream takes the
// delivery at once rather than at its next check.
func TestReapedDeliveryWakesTheStream(t *testing.T) {
	f := setup(t)
	f.quiet()
	f.slack("orphaned")
	id := f.queued()
	// Leased to a connection that died with its instance (a claim, which
	// marks the workspace for the sweep); the lease runs out after the
	// stream connects, and the runner's sweep reaps it.
	f.tx(func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE deliveries SET state = 'in_flight', attempts = 1, lease_holder = 'conn_gone', lease_until = now() + interval '500 milliseconds'
			WHERE id = $1`, id); err != nil {
			return err
		}
		return events.MarkDue(ctx, tx)
	})
	c := f.dial(false)
	if m := mustRead(t, c); m.DeliveryID != id || m.Attempt != 2 {
		t.Fatalf("got %+v", m)
	}
}

// A delivery nothing announced (a lost notification) is found at the
// connection's next renewal.
func TestRenewalFindsUnannouncedDeliveries(t *testing.T) {
	f := setup(t)
	f.quiet()
	f.s.Renew = 200 * time.Millisecond
	f.slack("unannounced")
	id := f.queued()
	f.tx(func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE deliveries SET state = 'cancelled' WHERE id = $1`, id)
		return err
	})
	c := f.dial(false)
	time.Sleep(100 * time.Millisecond)
	f.tx(func(tx pgx.Tx) error { // pending again, without a notification
		_, err := tx.Exec(ctx, `UPDATE deliveries SET state = 'pending' WHERE id = $1`, id)
		return err
	})
	if m := mustRead(t, c); m.DeliveryID != id {
		t.Fatalf("got %+v", m)
	}
}

// A poll that yields to a live connection takes nothing, but its acks
// (recorded in the same transaction) still count.
func TestYieldingPollStillAcks(t *testing.T) {
	f := setup(t)
	f.slack("one")
	c := f.dial(true)
	m := mustRead(t, c)
	_, err := f.s.LongPoll(ctx, streams.PollRequest{Tenant: f.tenant, Agent: "codex", Yield: true, Acks: []string{m.DeliveryID},
		Valid: func(context.Context, pgx.Tx) error { return nil }})
	if !errors.Is(err, streams.ErrStreamConnected) {
		t.Fatalf("err %v", err)
	}
	if s := f.state(m.DeliveryID); s != "acked" {
		t.Fatalf("state %s", s)
	}
}

// Past MaxStreams, new streams and polls are refused at once with 503 and
// Retry-After; the ones held carry on, and a freed place is taken again.
func TestFullServerRefusesNewStreams(t *testing.T) {
	f := setup(t)
	f.s.MaxStreams = 1
	base := f.pollServer()
	link := f.link(false, time.Hour)
	c, _, err := websocket.Dial(ctx, link, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	time.Sleep(100 * time.Millisecond)
	if f.s.Active() != 1 {
		t.Fatalf("active %d", f.s.Active())
	}

	// The same link refused again is told to wait longer.
	lines := streams.HTTPStreamURL(f.link(false, time.Hour), streams.FormatLines)
	for _, want := range []int{15, 30, 60} {
		resp, err := http.Get(lines)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		// The wait is spread up to half again (exclusive) and sent in whole
		// seconds: for 15, up to 22 (22.4…), so compare in halves.
		if s, _ := strconv.Atoi(resp.Header.Get("Retry-After")); resp.StatusCode != http.StatusServiceUnavailable || s < want || s*2 >= want*3 {
			t.Fatalf("lines at capacity: %d, Retry-After %q, want %d+", resp.StatusCode, resp.Header.Get("Retry-After"), want)
		}
	}
	code, b := poll(t, base, token(f.link(true, time.Hour)), "wait=1s")
	if code != http.StatusServiceUnavailable || b.Error.Code != "at_capacity" {
		t.Fatalf("poll at capacity: %d %+v", code, b.Error)
	}

	f.slack("still served")
	if m := mustRead(t, c); m.Event.Subject != "still served" {
		t.Fatalf("got %+v", m)
	}
	c.Close(websocket.StatusNormalClosure, "")
	for range 50 {
		if f.s.Active() == 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if code, b := poll(t, base, token(f.link(true, time.Hour)), "wait=0"); code != 200 {
		t.Fatalf("after a place freed: %d %+v", code, b.Error)
	}
}
