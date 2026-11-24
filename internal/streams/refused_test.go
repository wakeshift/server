package streams_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/streams"
)

// fill takes the instance's one place (MaxStreams 1) with a stream.
func (f *fixture) fill() {
	f.t.Helper()
	f.s.MaxStreams = 1
	c, _, err := websocket.Dial(ctx, f.link(false, time.Hour), nil)
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { c.CloseNow() })
	waitFor(f.t, 2*time.Second, "the stream takes the place", func() bool { return f.s.Active() == 1 })
}

// A one-shot wait refused at capacity is answered as a wait that had
// nothing to hand out: a 200 and the same url to run next, never an error
// note (which would wake its agent to run it again), and it does no
// database work first: the batch its cursor names stays unconfirmed until
// a wait that gets in.
func TestWaitRefusedAtCapacity(t *testing.T) {
	f := setup(t)
	base := f.waitServer()
	f.s.LoopPause = -1 // no hold here (TestRefusalHold)
	link, id := f.sessionLinkFor("agy", streams.SessionLinkTTL)
	f.toSession(id, "one")
	first := waitURL(base, link, "timeout=300ms")
	w := wait(t, first)
	if len(w.events) != 1 || len(w.cursors) != 1 {
		t.Fatalf("first wait: %v %v", w.events, w.last)
	}
	one := f.deliveryOf(w.events[0]["id"].(string))

	f.fill()
	w = wait(t, first+"&after="+w.cursors[0])
	note, _ := w.last["note"].(string)
	if w.code != http.StatusOK || len(w.events) != 0 || w.last["type"] != "hub.next" || onServer(base, w.last["url"].(string)) != first ||
		!strings.HasPrefix(note, "Nothing was handed out: the server was busy.") || strings.Contains(note, "went wrong") {
		t.Fatalf("refused wait: %d %v", w.code, w.last)
	}
	if s := f.state(one); s != "in_flight" {
		t.Fatalf("a refused wait confirmed its cursor's batch: %s", s)
	}
	// A foreground wait is told the command to run with its cursor.
	w = wait(t, waitURL(base, link, "timeout=300ms&say=command&after=00112233aabbccdd"))
	if note, _ := w.last["note"].(string); w.code != http.StatusOK || !strings.Contains(note, "after=00112233aabbccdd") {
		t.Fatalf("refused foreground wait: %d %v", w.code, w.last)
	}
}

// A loop's round refused at capacity is a 200 with nothing (the loop goes
// round, and doesn't end). The first of a run of refused rounds confirms
// what the last round printed (in one transaction: the round that gets in
// after the hold could no longer); the ones after it do no database work.
func TestWaitLoopRefusedAtCapacity(t *testing.T) {
	f := setup(t)
	base := f.waitServer()
	link, id := f.loopSession("loop-refused", false)
	f.toSession(id, "one")
	r := loopRound(t, loopURL(base, link, "111", "timeout=300ms"))
	if r.code != 200 || len(r.events) != 1 {
		t.Fatalf("first round: %+v", r)
	}
	one := f.deliveryOf(r.events[0]["id"].(string))

	f.fill()
	r = loopRound(t, loopURL(base, link, "111", "timeout=300ms"))
	if r.code != http.StatusOK || len(r.events) != 0 || r.closed != nil {
		t.Fatalf("refused round: %+v", r)
	}
	if s := f.state(one); s != "acked" {
		t.Fatalf("the first refused round didn't confirm the last round's batch: %s", s)
	}
}

// A refused round confirms the last batch only through a conversation's
// own wait link, as a round that gets in must use (loopKind): never
// through another link to the same conversation, such as a connector's.
func TestWaitLoopRefusedConfirmsOnlyWithItsLink(t *testing.T) {
	f := setup(t)
	base := f.waitServer()
	link, id := f.loopSession("loop-refused-other", false)
	f.toSession(id, "one")
	r := loopRound(t, loopURL(base, link, "111", "timeout=300ms"))
	if r.code != 200 || len(r.events) != 1 {
		t.Fatalf("first round: %+v", r)
	}
	one := f.deliveryOf(r.events[0]["id"].(string))
	var other string
	f.tx(func(tx pgx.Tx) (err error) {
		other, err = f.mintFor(tx, id, false, streams.SessionLinkTTL) // a connector's link (acks), not a wait's
		return err
	})

	f.fill()
	r = loopRound(t, loopURL(base, other, "111", "timeout=300ms"))
	if r.code != http.StatusOK || len(r.events) != 0 {
		t.Fatalf("refused round: %+v", r)
	}
	if s := f.state(one); s == "acked" {
		t.Fatal("a refused round confirmed the loop's batch through a link that isn't the loop's")
	}
}
