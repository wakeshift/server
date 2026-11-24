package coretest_test

import (
	"testing"
	"time"

	"github.com/wakeshift/server/apps/agentkit"
	"github.com/wakeshift/server/internal/coretest"
	"github.com/wakeshift/server/internal/server"
)

// A request has server.BodyReadTimeout to send its body, but one that sent
// it at once and is then held open outlasts the deadline: an MCP push
// stream gets an event emitted well after it, and an app's wait returns
// the message that came well after it.
func TestHeldRequestsOutlastTheBodyDeadline(t *testing.T) {
	old := server.BodyReadTimeout
	server.BodyReadTimeout = 200 * time.Millisecond
	t.Cleanup(func() { server.BodyReadTimeout = old })
	c := coretest.Start(t)

	events := eventsApp(t, c, "chat")
	s := c.OpenPush(t, events, pushBody("u1", nil, nil))
	mustActive(t, s)
	chat := chatApp(t, c)
	codex := actAs(t, chat, "ada", "codex")
	w, err := agentkit.Wait(ctx, codex, agentkit.WaitRequest{})
	if err != nil {
		t.Fatal(err)
	}
	lineSub(t, c, chat, codex, w.Session)
	ch := waitAsync(codex, agentkit.WaitRequest{Session: w.Session, WaitSeconds: 10})

	time.Sleep(5 * server.BodyReadTimeout)
	emitChat(t, c, events, "late")
	if f := s.NextOf("event", pushWait); f == nil {
		t.Fatal("the push stream ended at the body's deadline")
	}
	postLine(t, c, chat, codex, "hello")
	if r := await(t, ch, 5*time.Second); r.err != nil || len(r.w.Deliveries) != 1 {
		t.Fatalf("the wait, held past the body's deadline: %+v %v", r.w, r.err)
	}
}
