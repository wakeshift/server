package pipeline

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/wakeshift/server/internal/dbtest"
	"github.com/wakeshift/server/internal/events"
)

// The leader, notified of events more often than db.HeldPing, keeps its
// session (and with it the leader's lock) past db.HeldIdleTimeout (#1311:
// leadership moved often, and each new leader swept every tenant). When its connection does drop, it sweeps only the
// marked tenants as it leads again, not every tenant. Timings shortened.
func TestBusyLeaderKeepsLeading(t *testing.T) {
	dbtest.HeldTimings(t, 1500*time.Millisecond, 300*time.Millisecond, 500*time.Millisecond)
	d := dbtest.New(t)
	tenant := dbtest.NewTenant(t, d)
	var mu sync.Mutex
	var sweeps []bool // full or not, in order
	r := &Runner{DB: d, Poll: time.Hour, FullEvery: time.Hour, onSweep: func(full bool) {
		mu.Lock()
		sweeps = append(sweeps, full)
		mu.Unlock()
	}}
	swept := func() []bool {
		mu.Lock()
		defer mu.Unlock()
		return append([]bool(nil), sweeps...)
	}
	rctx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); r.Run(rctx) }()
	defer func() { stop(); <-done }()

	leader := func() int {
		t.Helper()
		var pid int
		if err := d.Pool.QueryRow(ctx, `SELECT coalesce(max(pid), 0) FROM pg_locks
			WHERE locktype = 'advisory' AND granted AND (classid::bigint << 32 | objid::bigint) = $1`, int64(leaderLock)).Scan(&pid); err != nil {
			t.Fatal(err)
		}
		return pid
	}
	waitUntil := func(what string, f func() bool) {
		t.Helper()
		for end := time.Now().Add(5 * time.Second); !f(); time.Sleep(20 * time.Millisecond) {
			if time.Now().After(end) {
				t.Fatalf("%s: not within 5s", what)
			}
		}
	}
	var first int
	waitUntil("a leader", func() bool { first = leader(); return first != 0 })
	waitUntil("the first sweep", func() bool { return len(swept()) > 0 })

	const run = 4 * time.Second // over twice the idle limit
	for start := time.Now(); time.Since(start) < run; time.Sleep(100 * time.Millisecond) {
		if _, err := d.Pool.Exec(ctx, `SELECT pg_notify($1, $2)`, events.NotifyChannel, tenant); err != nil {
			t.Fatal(err)
		}
	}
	if now := leader(); now != first {
		t.Fatalf("the busy leader's session changed (pid %d, then %d)", first, now)
	}
	// The first sweep (full) and the listener's first resync (marked only).
	before := swept()
	for i, full := range before {
		if full != (i == 0) {
			t.Fatalf("sweeps while leading %v: want one full sweep, at the start", before)
		}
	}

	// Dropped: it leads again, and sweeps the marked tenants only.
	if _, err := d.Pool.Exec(ctx, `SELECT pg_terminate_backend($1)`, first); err != nil {
		t.Fatal(err)
	}
	waitUntil("a new leader", func() bool { now := leader(); return now != 0 && now != first })
	waitUntil("the resync sweep", func() bool { return len(swept()) > len(before) })
	if after := swept(); after[len(before)] {
		t.Fatalf("the reconnected leader swept every tenant: %v", after)
	}
}
