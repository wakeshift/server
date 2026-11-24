package streams

import (
	"errors"
	"strconv"
	"testing"
	"time"
)

// A link refused again and again is told to wait longer each time (15s,
// 30s, then 60s, each spread up to half again), until it gets in or hasn't
// been refused for a while.
func TestRefusalsBackOff(t *testing.T) {
	var r refusals
	now := time.Now()
	within := func(d, base time.Duration) bool { return d >= base && d < base*3/2 }
	for i, base := range []time.Duration{15 * time.Second, 30 * time.Second, 60 * time.Second, 60 * time.Second} {
		if d := r.refused("a", now); !within(d, base) {
			t.Fatalf("refusal %d: %v, want %v to %v", i+1, d, base, base*3/2)
		}
	}
	if d := r.refused("b", now); !within(d, 15*time.Second) {
		t.Fatalf("another link: %v", d)
	}
	r.admitted("a")
	if d := r.refused("a", now); !within(d, 15*time.Second) {
		t.Fatalf("after getting in: %v", d)
	}
	r.refused("a", now)
	if d := r.refused("a", now.Add(refusalMemory+time.Second)); !within(d, 15*time.Second) {
		t.Fatalf("after a quiet while: %v", d)
	}
	// The longest wait stays within what the clients honor (120s).
	if top := refusalWaits[len(refusalWaits)-1]; top*3/2 > 120*time.Second {
		t.Fatalf("longest wait %v", top*3/2)
	}
	if got := retryAfterOf(busyError{after: 29 * time.Second}); got != "29" {
		t.Fatalf("Retry-After %q", got)
	}
}

// The memory of refusals stays bounded.
func TestRefusalsAreBounded(t *testing.T) {
	var r refusals
	now := time.Now()
	for i := range refusalsKept + 10 {
		r.refused(strconv.Itoa(i), now)
	}
	if len(r.m) > refusalsKept {
		t.Fatalf("kept %d", len(r.m))
	}
}

// A refused wait or loop round is held for its refusal's wait, at most its
// own timeout, and at least the loop's pause.
func TestRefusalHold(t *testing.T) {
	busy := busyError{after: 40 * time.Second}
	for _, c := range []struct {
		err          error
		limit, floor time.Duration
		want         time.Duration
	}{
		{busy, 0, 2 * time.Second, 40 * time.Second},
		{busy, 50 * time.Second, 2 * time.Second, 40 * time.Second},
		{busy, 10 * time.Second, 2 * time.Second, 10 * time.Second},
		{busy, 300 * time.Millisecond, 2 * time.Second, 2 * time.Second},
		{errBusy, 0, 2 * time.Second, refusalWaits[0]},
	} {
		if got := refusalHold(c.err, c.limit, c.floor); got != c.want {
			t.Errorf("refusalHold(%v, %v, %v) = %v, want %v", c.err, c.limit, c.floor, got, c.want)
		}
	}
}

// The in-memory check before a wait's database work counts refusals per
// link as enter does, and forgets them only once the wait got in: seeing
// room isn't getting in (enter may still refuse it).
func TestAtCapacityBacksOff(t *testing.T) {
	s := &Server{MaxStreams: 1}
	s.active.Store(1)
	wait := func() time.Duration {
		var b busyError
		if !errors.As(s.atCapacity("k"), &b) {
			t.Fatal("not refused at capacity")
		}
		return b.after
	}
	if d := wait(); d < 15*time.Second || d >= 23*time.Second {
		t.Fatalf("first refusal: %v", d)
	}
	var b busyError
	if errors.As(s.atCapacity("first"), &b); !b.first {
		t.Fatal("a key's first refusal isn't marked first")
	}
	if errors.As(s.atCapacity("first"), &b); b.first {
		t.Fatal("a key's second refusal is marked first")
	}
	s.active.Store(0)
	if err := s.atCapacity("k"); err != nil {
		t.Fatal(err)
	}
	s.active.Store(1)
	if d := wait(); d < 30*time.Second {
		t.Fatalf("room seen, not taken, reset the backoff: %v", d)
	}
	s.admittedAs("k", nil)
	if d := wait(); d >= 23*time.Second {
		t.Fatalf("got in, yet still backing off: %v", d)
	}
}
