package streams_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/wakeshift/server/internal/streams"
)

// A wait returns at most DefaultPollBatch messages, which the public docs
// state (20 per wake: site/docs/events.md, site/docs/try.md): the rest stay
// waiting, and the next wait, with the cursor, returns them at once.
// Nothing is dropped.
func TestWaitReturnsABatchAndTheRestNext(t *testing.T) {
	if streams.DefaultPollBatch != 20 {
		t.Fatalf("DefaultPollBatch is %d: update site/docs/events.md and site/docs/try.md, which say 20 per wake", streams.DefaultPollBatch)
	}
	f := setup(t)
	base := f.waitServer()
	link, id := f.sessionLinkFor("agy", streams.SessionLinkTTL)
	const n = streams.DefaultPollBatch + 5
	for i := range n {
		f.toSession(id, fmt.Sprintf("m%02d", i))
	}
	first := waitURL(base, link, "timeout=300ms")
	w := wait(t, first)
	if len(w.events) != streams.DefaultPollBatch || w.events[0]["subject"] != "m00" || len(w.cursors) != 1 {
		t.Fatalf("first wait: %d events, cursors %v", len(w.events), w.cursors)
	}
	start := time.Now()
	w = wait(t, first+"&after="+w.cursors[0])
	if len(w.events) != 5 || w.events[0]["subject"] != fmt.Sprintf("m%02d", streams.DefaultPollBatch) || w.events[4]["subject"] != fmt.Sprintf("m%02d", n-1) {
		t.Fatalf("second wait: %v", w.events)
	}
	if time.Since(start) > 250*time.Millisecond {
		t.Fatalf("the rest waited for the timeout: %v", time.Since(start))
	}
}
