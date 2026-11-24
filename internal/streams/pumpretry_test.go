package streams_test

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wakeshift/server/internal/streams"
)

// A claim transaction that fails (a database failover, a pool briefly
// full) is retried on the same connection, rather than ending the stream
// and sending its client back into the same window to reconnect.
func TestStreamRetriesFailedClaims(t *testing.T) {
	f := setup(t)
	f.slack("after a blip")
	var failures atomic.Int32
	streams.SetBeforeClaim(func() error {
		if failures.Add(1) <= 2 {
			return errors.New("conn closed")
		}
		return nil
	})
	t.Cleanup(func() { streams.SetBeforeClaim(nil) })
	t.Cleanup(streams.SetPumpRetryWait(50 * time.Millisecond))
	c := f.dial(true)
	m := mustRead(t, c)
	if m.Type != "event" || m.Event.Subject != "after a blip" {
		t.Fatalf("got %+v", m)
	}
	if n := failures.Load(); n < 3 {
		t.Fatalf("claimed %d times; want two failures, then the claim", n)
	}
}

// A claim that keeps failing still ends the stream, after its retries.
func TestStreamEndsAfterClaimRetries(t *testing.T) {
	f := setup(t)
	f.slack("never")
	var calls atomic.Int32
	streams.SetBeforeClaim(func() error { calls.Add(1); return errors.New("conn closed") })
	t.Cleanup(func() { streams.SetBeforeClaim(nil) })
	t.Cleanup(streams.SetPumpRetryWait(10 * time.Millisecond))
	c := f.dial(true)
	for {
		m, err := read(t, c)
		if err != nil {
			break // closed
		}
		if m.Type == "event" {
			t.Fatalf("got %+v", m)
		}
	}
	if n := calls.Load(); n != 6 {
		t.Fatalf("claimed %d times; want the first and 5 retries", n)
	}
}
