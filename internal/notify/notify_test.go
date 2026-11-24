package notify_test

import (
	"context"
	"testing"
	"time"

	"github.com/wakeshift/server/internal/dbtest"
	"github.com/wakeshift/server/internal/notify"
)

func got(ch <-chan struct{}, within time.Duration) bool {
	select {
	case <-ch:
		return true
	case <-time.After(within):
		return false
	}
}

// A notification wakes its waiters; when the listener reconnects, every
// waiter on a Resync channel is woken (it may have missed one), and waiters
// on other channels aren't (a wake there means something happened).
func TestHubDeliversAndResyncsAfterReconnecting(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d := dbtest.New(t)
	h := notify.New(d.Pool, "work", "event")
	h.Resync = []string{"work"}
	work, stopWork := h.Wait("work", "a")
	defer stopWork()
	event, stopEvent := h.Wait("event", "a")
	defer stopEvent()
	go h.Run(ctx)
	if !got(work, 3*time.Second) {
		t.Fatal("no resync when the listener first connected")
	}
	if got(event, 200*time.Millisecond) {
		t.Fatal("a non-resync waiter was woken on connect")
	}

	if _, err := d.Pool.Exec(ctx, `SELECT pg_notify('event', 'a')`); err != nil {
		t.Fatal(err)
	}
	if !got(event, 3*time.Second) {
		t.Fatal("notification not delivered")
	}
	if _, err := d.Pool.Exec(ctx, `SELECT pg_notify('work', 'b')`); err != nil {
		t.Fatal(err)
	}
	if got(work, 200*time.Millisecond) {
		t.Fatal("woken by another payload's notification")
	}

	// Drop the listener's connection: it reconnects and resyncs.
	var n int
	if err := d.Pool.QueryRow(ctx, `SELECT count(pg_terminate_backend(pid)) FROM pg_stat_activity
		WHERE datname = current_database() AND query LIKE 'LISTEN%'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("terminating the listener: %d, %v", n, err)
	}
	if !got(work, 5*time.Second) {
		t.Fatal("no resync after reconnecting")
	}
	if got(event, 200*time.Millisecond) {
		t.Fatal("a non-resync waiter was woken on reconnect")
	}
}

// A busy hub (a notification more often than db.HeldPing) keeps one
// listener connection past db.HeldIdleTimeout (#1311: it was ended every
// ~91 s, waking every Resync waiter at once and losing the notices sent
// while it reconnected), and a notice on a channel without resync, sent
// after the old connection would have ended, still arrives. Timings
// shortened.
func TestBusyHubKeepsItsConnection(t *testing.T) {
	dbtest.HeldTimings(t, 1500*time.Millisecond, 300*time.Millisecond, 500*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d := dbtest.New(t)
	h := notify.New(d.Pool, "work", "takeover")
	h.Resync = []string{"work"}
	work, stopWork := h.Wait("work", "a")
	defer stopWork()
	takeover, stopTakeover := h.Wait("takeover", "a")
	defer stopTakeover()
	done := make(chan struct{})
	go func() { defer close(done); h.Run(ctx) }()
	defer func() { cancel(); <-done }()
	if !got(work, 3*time.Second) {
		t.Fatal("no resync when the listener first connected")
	}

	const run = 4 * time.Second // over twice the idle limit
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	start := time.Now()
	noticed := false
	for time.Since(start) < run {
		<-tick.C
		if _, err := d.Pool.Exec(ctx, `SELECT pg_notify('work', 'a')`); err != nil {
			t.Fatal(err)
		}
		if !got(work, 2*time.Second) {
			t.Fatalf("a notification wasn't delivered after %v", time.Since(start))
		}
		if !noticed && time.Since(start) > 3*time.Second {
			if _, err := d.Pool.Exec(ctx, `SELECT pg_notify('takeover', 'a')`); err != nil {
				t.Fatal(err)
			}
			if !got(takeover, 2*time.Second) {
				t.Fatal("a takeover notice sent past the idle limit wasn't delivered")
			}
			noticed = true
		}
	}
	if n := notify.Connects(h); n != 1 {
		t.Fatalf("the busy hub connected %d times in %v, want once", n, run)
	}
}
