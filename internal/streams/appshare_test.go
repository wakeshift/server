package streams_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/agents"
	"github.com/wakeshift/server/internal/streams"
)

// linkFor mints a link for agent (made now if it isn't), made by createdBy.
func (f *fixture) linkFor(agent, createdBy string) string {
	f.t.Helper()
	var u string
	f.tx(func(tx pgx.Tx) (err error) {
		if err := agents.Ensure(ctx, tx, agent); err != nil {
			return err
		}
		u, _, err = f.s.Mint(ctx, tx, f.tenant, streams.NewLink{Agent: agent, CreatedBy: createdBy, TTL: time.Hour})
		return err
	})
	return u
}

// waitActive waits until the server holds n streams.
func (f *fixture) waitActive(n int64) {
	f.t.Helper()
	waitFor(f.t, 5*time.Second, fmt.Sprintf("%d streams held", n), func() bool { return f.s.Active() == n })
}

// One app's links can't take every place on an instance: past its share,
// with no more than the reserve free, its next stream and poll are refused
// (503 at_capacity), while another app's and an agent's own link still get
// in, up to MaxStreams.
func TestAppShareOfMaxStreams(t *testing.T) {
	f := setup(t)
	f.quiet()
	f.s.MaxStreams = 8 // an app's share 2, the reserve 2
	base := f.pollServer()
	for i := range 6 {
		c, _, err := websocket.Dial(ctx, f.linkFor(fmt.Sprint("evil", i), "app:evil"), nil)
		if err != nil {
			t.Fatalf("evil's stream %d: %v", i, err)
		}
		defer c.CloseNow()
	}
	f.waitActive(6)
	_, resp, err := websocket.Dial(ctx, f.linkFor("evil6", "app:evil"), nil)
	if err == nil || resp == nil || resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("evil's seventh stream: %v %v", err, resp)
	}
	if code, b := poll(t, base, token(f.linkFor("evil7", "app:evil")), "wait=0"); code != http.StatusServiceUnavailable || b.Error.Code != "at_capacity" {
		t.Fatalf("evil's poll past its share: %d %+v", code, b.Error)
	}
	f.waitActive(6)
	// Another app's link and an agent's own get the reserve.
	for i, by := range []string{"app:chat", "test"} {
		c, _, err := websocket.Dial(ctx, f.linkFor(fmt.Sprint("other", i), by), nil)
		if err != nil {
			t.Fatalf("%s's stream: %v", by, err)
		}
		defer c.CloseNow()
	}
	f.waitActive(8)
	// Full: nobody gets in.
	if _, resp, err := websocket.Dial(ctx, f.linkFor("other9", "test"), nil); err == nil || resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("past MaxStreams: %v %v", err, resp)
	}
}

// A poll refused at capacity does no database work: its link isn't marked
// used (nor slid).
func TestPollAtCapacityTouchesNothing(t *testing.T) {
	f := setup(t)
	f.quiet()
	f.s.MaxStreams = 1
	base := f.pollServer()
	c := f.dial(false)
	f.waitActive(1)
	link := f.linkFor("claude-code", "test")
	code, b := poll(t, base, token(link), "wait=0")
	if code != http.StatusServiceUnavailable || b.Error.Code != "at_capacity" {
		t.Fatalf("poll at capacity: %d %+v", code, b.Error)
	}
	var used bool
	f.tx(func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT used_at IS NOT NULL FROM stream_links WHERE agent_id = 'claude-code'`).Scan(&used)
	})
	if used {
		t.Fatal("a poll refused at capacity marked its link used")
	}
	c.CloseNow()
	f.waitActive(0)
	if code, b := poll(t, base, token(link), "wait=0"); code != 200 {
		t.Fatalf("after a place freed: %d %+v", code, b.Error)
	}
}

// Every frame a WebSocket client sends counts toward its rate limit, not
// only acks: a flood of other frames closes the connection.
func TestWebSocketFramesAreRateLimited(t *testing.T) {
	f := setup(t)
	f.quiet()
	c := f.dial(false)
	f.waitActive(1)
	frame := []byte(`{"type":"hello"}`)
	for range 300 {
		if err := c.Write(ctx, websocket.MessageText, frame); err != nil {
			break // closed already
		}
	}
	_, err := readWithin(t, c, 5*time.Second)
	if websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
		t.Fatalf("after 300 frames in a second: %v, want a policy-violation close", err)
	}
	if !strings.Contains(err.Error(), "too many") {
		t.Errorf("close reason: %v", err)
	}
}
