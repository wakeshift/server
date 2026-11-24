package pipeline

import (
	"testing"
	"time"
)

// A full sweep is slow against its own interval, not the poll's: one that
// takes a minute every 15 minutes isn't falling behind.
func TestSlowAfter(t *testing.T) {
	r := &Runner{Poll: time.Minute, FullEvery: 15 * time.Minute}
	if got := r.slowAfter(false); got != 30*time.Second {
		t.Fatalf("marked sweep: %v, want 30s", got)
	}
	if got := r.slowAfter(true); got != 7*time.Minute+30*time.Second {
		t.Fatalf("full sweep: %v, want 7m30s", got)
	}
}
