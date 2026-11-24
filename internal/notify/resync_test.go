package notify

import (
	"context"
	"testing"
	"time"
)

// A resync wakes every Resync waiter, but spread out rather than all at
// once: each wake is a claim transaction, and hundreds at once can
// saturate an instance's pool on every listener reconnect. A few waiters are woken at
// once.
func TestResyncSpreadsWakes(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := New(nil, "work", "event")
	h.Resync = []string{"work"}
	const n = 400 // 400 × 5 ms: spread over 2 s
	var waits []<-chan struct{}
	for range n {
		ch, stop := h.Wait("work", "a")
		defer stop()
		waits = append(waits, ch)
	}
	other, stopOther := h.Wait("event", "a")
	defer stopOther()
	woken := func() int {
		c := 0
		for _, ch := range waits {
			if len(ch) > 0 {
				c++
			}
		}
		return c
	}
	h.resync(ctx)
	time.Sleep(200 * time.Millisecond)
	if w := woken(); w == 0 || w > n/2 {
		t.Fatalf("%d of %d woken 200 ms into a 2 s resync: want some, not most", w, n)
	}
	deadline := time.Now().Add(5 * time.Second)
	for woken() < n && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if w := woken(); w != n {
		t.Fatalf("%d of %d woken after the resync", w, n)
	}
	if len(other) != 0 {
		t.Fatal("a non-resync waiter was woken")
	}

	// A handful of waiters: woken at once.
	h2 := New(nil, "work")
	h2.Resync = []string{"work"}
	ch, stop := h2.Wait("work", "b")
	defer stop()
	h2.resync(ctx)
	select {
	case <-ch:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("a lone waiter wasn't woken at once")
	}
}
