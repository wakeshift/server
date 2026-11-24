package coretest_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/wakeshift/server/apps/agentkit"
	"github.com/wakeshift/server/internal/beacon"
	"github.com/wakeshift/server/internal/connector"
	"github.com/wakeshift/server/internal/connectorversion"
	"github.com/wakeshift/server/internal/coretest"
	"github.com/wakeshift/server/pkg/wakeshift"
)

// heldFrame is what a session's stream writes, as the Codex connector reads it.
type heldFrame struct {
	Type       string `json:"type"`
	DeliveryID string `json:"delivery_id"`
	Event      struct {
		Subject string `json:"subject"`
	} `json:"event"`
}

// S12, October 29: Codex spent a 20-minute turn calling Agent Chat's wait
// tool, which answered "stream_connected" at once, while its connector held
// the person's DM for after the turn. A wait in a Codex thread whose
// connector holds the stream now takes what the connector holds and hasn't
// delivered (it delivers only between turns), and what comes while it
// waits; the connector is told ("taken"), so the same messages don't start
// a turn after this one. Messages an app showed the thread another way
// (Agent Chat's history: Seen) are taken the same way.
func TestWaitTakesWhatTheCodexConnectorHolds(t *testing.T) {
	c := coretest.Start(t)
	app := chatApp(t, c)
	codex := actAs(t, app, "ada", "codex")
	const thread = "01a14bd7-bbbc-7b70-a723-0abb9f381299"
	sess, err := agentkit.ConversationSession(ctx, codex, thread)
	if err != nil {
		t.Fatal(err)
	}
	lineSub(t, c, app, codex, sess.Handle)
	watched, err := threadWatch(t, c, app, codex, thread)
	if err != nil || watched.Beacon == "" {
		t.Fatalf("watch: %+v %v", watched, err)
	}
	claims, err := beacon.Verify(watched.Beacon, thread, []string{c.URL}, coreKeys(t, c), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	code, out := redeemTicket(t, claims.Redeem, claims.Ticket, thread)
	if code != 200 {
		t.Fatalf("redeem: %d %v", code, out)
	}
	ws, _, err := websocket.Dial(ctx, out["url"].(string), &websocket.DialOptions{HTTPHeader: http.Header{connectorversion.Header: {connector.ClientHeader("codex")}}})
	if err != nil {
		t.Fatal(err)
	}
	defer ws.CloseNow()
	read := func() heldFrame {
		t.Helper()
		rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		_, b, err := ws.Read(rctx)
		if err != nil {
			t.Fatalf("reading the connector's stream: %v", err)
		}
		var f heldFrame
		if err := json.Unmarshal(b, &f); err != nil {
			t.Fatalf("a frame that isn't JSON: %s", b)
		}
		return f
	}
	emit := func(id, text string) {
		t.Helper()
		if code, e := appAPI(t, c, app, "/api/apps/v1/events/emit", map[string]any{"name": "chat.line", "arguments": map[string]any{"channel": "general"},
			"payload": map[string]any{"id": id}, "text": text, "to": codex.User()}, nil); code != 200 {
			t.Fatalf("emit %q: %d %s", text, code, e)
		}
	}

	// The thread is in a turn: the connector holds the DM, unacked.
	emit("m1", "the DM")
	held := read()
	if held.Type != "event" || held.Event.Subject != "the DM" {
		t.Fatalf("the connector got %+v", held)
	}
	// The thread's wait takes it, and says the thread is woken anyway.
	w, err := agentkit.Wait(ctx, codex, agentkit.WaitRequest{Conversation: thread, WaitSeconds: 5})
	if err != nil || !w.Woken || w.StreamConnected || len(w.Deliveries) != 1 || w.Deliveries[0].DeliveryID != held.DeliveryID || w.Deliveries[0].Attempt != 1 {
		t.Fatalf("the wait: %+v %v", w, err)
	}
	if f := read(); f.Type != "taken" || f.DeliveryID != held.DeliveryID {
		t.Fatalf("the connector wasn't told: %+v", f)
	}
	if _, err := agentkit.Ack(ctx, codex, sess.Handle, []string{held.DeliveryID}); err != nil {
		t.Fatal(err)
	}

	// A wait in progress gets what comes, at once.
	polls := c.Streams.Active()
	ch := waitAsync(codex, agentkit.WaitRequest{Conversation: thread, WaitSeconds: 10})
	waitFor(t, "the wait to be in progress", func() bool { return c.Streams.Active() > polls })
	start := time.Now()
	emit("m2", "a mention")
	r := await(t, ch, 5*time.Second)
	if r.err != nil || !r.w.Woken || len(r.w.Deliveries) != 1 || r.w.Deliveries[0].Event.Subject != "a mention" {
		t.Fatalf("the waiting wait: %+v %v", r.w, r.err)
	}
	if time.Since(start) > 2*time.Second {
		t.Errorf("a mention took %v", time.Since(start))
	}
	got := r.w.Deliveries[0].DeliveryID
	if _, err := agentkit.Ack(ctx, codex, sess.Handle, []string{got}); err != nil {
		t.Fatal(err)
	}

	// Nothing waiting: the wait says so after its time, still woken.
	start = time.Now()
	if w, err := agentkit.Wait(ctx, codex, agentkit.WaitRequest{Conversation: thread, WaitSeconds: 1}); err != nil || !w.Woken || len(w.Deliveries) != 0 {
		t.Fatalf("an empty wait: %+v %v", w, err)
	}
	if took := time.Since(start); took < 900*time.Millisecond {
		t.Errorf("an empty wait returned after %v, before its time", took)
	}

	// Shown in history: taken from the connector the same way, and only
	// what was shown.
	emit("m3", "read in history")
	emit("m4", "not shown")
	// The connector may have had the mention before the wait: then it's
	// told the wait took it.
	var frames []heldFrame
	hadMention, toldTaken := false, false
	for len(frames) < 2 {
		switch f := read(); {
		case f.Type == "event" && f.DeliveryID == got:
			hadMention = true
		case f.Type == "taken" && f.DeliveryID == got:
			toldTaken = true
		case f.Type == "event":
			frames = append(frames, f)
		}
	}
	if hadMention && !toldTaken {
		t.Fatal("the connector had the mention, and wasn't told the wait took it")
	}
	acked, err := agentkit.Seen(ctx, codex, sess.Handle, []string{"m3", "elsewhere"})
	if err != nil || len(acked) != 1 {
		t.Fatalf("seen: %v %v", acked, err)
	}
	var shown string
	for _, f := range frames {
		if f.Event.Subject == "read in history" {
			shown = f.DeliveryID
		}
	}
	if acked[0] != shown {
		t.Fatalf("seen acked %v, not %s", acked, shown)
	}
	if f := read(); f.Type != "taken" || f.DeliveryID != shown {
		t.Fatalf("the connector wasn't told what history showed: %+v", f)
	}
	// Another agent can't mark this thread's messages seen.
	claude := actAs(t, app, "ada", "claude-code")
	if _, err := agentkit.Seen(ctx, claude, sess.Handle, []string{"m4"}); !wakeshift.IsCode(err, "unknown_session") {
		t.Fatalf("another agent's seen: %v", err)
	}
	// The connector still holds the one not shown: the turn after this
	// one starts with it, acked then.
	for _, f := range frames {
		if f.Event.Subject == "not shown" {
			ack, _ := json.Marshal(map[string]string{"type": "ack", "delivery_id": f.DeliveryID})
			if err := ws.Write(ctx, websocket.MessageText, ack); err != nil {
				t.Fatal(err)
			}
			if r := read(); r.Type != "acked" || r.DeliveryID != f.DeliveryID {
				t.Fatalf("the connector's ack: %+v", r)
			}
		}
	}
}
