package coretest_test

import (
	"bufio"
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/apps/agentkit"
	"github.com/wakeshift/server/internal/appapi"
	"github.com/wakeshift/server/internal/coretest"
	"github.com/wakeshift/server/internal/grants"
	"github.com/wakeshift/server/pkg/wakeshift"
)

type waited struct {
	w   agentkit.Waited
	err error
}

func waitAsync(ag *wakeshift.Agent, r agentkit.WaitRequest) chan waited {
	ch := make(chan waited, 1)
	go func() { w, err := agentkit.Wait(ctx, ag, r); ch <- waited{w, err} }()
	return ch
}

func await(t *testing.T, ch chan waited, within time.Duration) waited {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(within):
		t.Fatal("the wait didn't return")
		return waited{}
	}
}

// The app API's wait: a session's stream long-polled under the agent's
// grant, as a wait tool uses it.
func TestWaitForMessages(t *testing.T) {
	c := coretest.Start(t)
	app := chatApp(t, c)
	codex := actAs(t, app, "ada", "codex")
	claude := actAs(t, app, "ada", "claude-code")

	// No session named: the agent's wait session.
	w, err := agentkit.Wait(ctx, codex, agentkit.WaitRequest{})
	if err != nil || w.Session != "codex@wait" || len(w.Deliveries) != 0 {
		t.Fatalf("first wait: %+v %v", w, err)
	}
	lineSub(t, c, app, codex, w.Session)

	// Waits, and returns when a message comes.
	polls := c.Streams.Active()
	ch := waitAsync(codex, agentkit.WaitRequest{Session: "codex@wait", WaitSeconds: 10})
	waitFor(t, "the wait to be in progress", func() bool { return c.Streams.Active() > polls })
	start := time.Now()
	postLine(t, c, app, codex, "hello")
	r := await(t, ch, 5*time.Second)
	if r.err != nil || len(r.w.Deliveries) != 1 || r.w.Deliveries[0].Event.Subject != "hello" || r.w.Deliveries[0].Event.From.ID != "agentchat" {
		t.Fatalf("wait: %+v %v", r.w, r.err)
	}
	if time.Since(start) > 2*time.Second {
		t.Errorf("a live message took %v", time.Since(start))
	}
	d := r.w.Deliveries[0]

	// Waiting counts as connected (but isn't a connector).
	list, _ := agentkit.Sessions(ctx, codex)
	for _, s := range list {
		if s.Handle == "codex@wait" && !s.Connected {
			t.Fatalf("a waiting session isn't connected: %+v", s)
		}
	}

	// Unacked: the next wait gets it again; acked, it's gone.
	w, err = agentkit.Wait(ctx, codex, agentkit.WaitRequest{Session: "codex@wait"})
	if err != nil || len(w.Deliveries) != 1 || w.Deliveries[0].DeliveryID != d.DeliveryID || w.Deliveries[0].Attempt != 2 {
		t.Fatalf("unacked: %+v %v", w, err)
	}
	// Another agent can't ack it, or wait on this agent's session.
	if _, err := agentkit.Ack(ctx, claude, "codex@wait", []string{d.DeliveryID}); !wakeshift.IsCode(err, "unknown_session") {
		t.Fatalf("another agent's ack: %v", err)
	}
	if _, err := agentkit.Wait(ctx, claude, agentkit.WaitRequest{Session: "codex@wait"}); !wakeshift.IsCode(err, "unknown_session") {
		t.Fatalf("another agent's wait: %v", err)
	}
	if acked, err := agentkit.Ack(ctx, codex, "codex@wait", []string{d.DeliveryID}); err != nil || len(acked) != 1 {
		t.Fatalf("ack: %v %v", acked, err)
	}
	if w, _ := agentkit.Wait(ctx, codex, agentkit.WaitRequest{Session: "codex@wait"}); len(w.Deliveries) != 0 {
		t.Fatalf("an acked message came back: %+v", w)
	}

	// The inbox says what's waiting without delivering it.
	postLine(t, c, app, codex, "queued")
	var in agentkit.Inbox
	for range 50 {
		if in, err = agentkit.InboxOf(ctx, codex, "codex@wait"); err != nil || in.Pending > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil || in.Pending != 1 || len(in.From) != 1 || in.From[0] != "agentchat" {
		t.Fatalf("inbox: %+v %v", in, err)
	}
	if again, _ := agentkit.InboxOf(ctx, codex, "codex@wait"); again.Pending != 1 {
		t.Fatalf("reading the inbox delivered: %+v", again)
	}
	w, _ = agentkit.Wait(ctx, codex, agentkit.WaitRequest{Session: "codex@wait", Ack: nil})
	if len(w.Deliveries) != 1 {
		t.Fatalf("queued: %+v", w)
	}
	// Acks can ride on the next wait.
	if w, err := agentkit.Wait(ctx, codex, agentkit.WaitRequest{Session: "codex@wait", Ack: []string{w.Deliveries[0].DeliveryID}}); err != nil || len(w.Acked) != 1 || len(w.Deliveries) != 0 {
		t.Fatalf("ack on wait: %+v %v", w, err)
	}

	// Two waits for one session: the newer one wins.
	polls = c.Streams.Active()
	older := waitAsync(codex, agentkit.WaitRequest{Session: "codex@wait", WaitSeconds: 10})
	waitFor(t, "the older wait to be in progress", func() bool { return c.Streams.Active() > polls })
	if _, err := agentkit.Wait(ctx, codex, agentkit.WaitRequest{Session: "codex@wait"}); err != nil {
		t.Fatal(err)
	}
	if r := await(t, older, 3*time.Second); !wakeshift.IsCode(r.err, "replaced") {
		t.Fatalf("older wait: %+v %v", r.w, r.err)
	}

	// A session a live stream delivers to isn't waited for, and its stream
	// isn't taken.
	link, err := agentkit.SessionLink(ctx, codex, agentkit.LinkRequest{Session: "codex@wait"})
	if err != nil {
		t.Fatal(err)
	}
	ws, _, err := websocket.Dial(ctx, link.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if w, err := agentkit.Wait(ctx, codex, agentkit.WaitRequest{Session: "codex@wait", WaitSeconds: 5}); err != nil || !w.StreamConnected || len(w.Deliveries) != 0 {
		t.Fatalf("with a stream connected: %+v %v", w, err)
	}
	postLine(t, c, app, codex, "on the stream")
	rctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if _, b, err := ws.Read(rctx); err != nil || !strings.Contains(string(b), "on the stream") {
		t.Fatalf("the stream after a wait: %s %v", b, err)
	}
	ws.CloseNow()
	time.Sleep(500 * time.Millisecond) // the server notices it's gone

	// Gone before a keepalive confirmed it: the message comes again, to the
	// wait, marked as a second attempt.
	again, err := agentkit.Wait(ctx, codex, agentkit.WaitRequest{Session: "codex@wait", WaitSeconds: 3})
	if err != nil || len(again.Deliveries) != 1 || again.Deliveries[0].Event.Subject != "on the stream" || again.Deliveries[0].Attempt != 2 {
		t.Fatalf("after the stream went away unconfirmed: %+v %v", again, err)
	}

	// Revoking the grant ends a wait in progress.
	ch = waitAsync(codex, agentkit.WaitRequest{Session: "codex@wait", WaitSeconds: 20, Ack: []string{again.Deliveries[0].DeliveryID}})
	time.Sleep(300 * time.Millisecond)
	c.DB.InTenant(ctx, codex.Workspace, func(tx pgx.Tx) error {
		g, err := grants.Live(ctx, tx, "agentchat", "codex")
		if err != nil {
			return err
		}
		_, err = grants.Revoke(ctx, tx, g.ID, "user:test")
		return err
	})
	if r := await(t, ch, 3*time.Second); !wakeshift.IsCode(r.err, "no_grant") {
		t.Fatalf("wait after revoking: %+v %v", r.w, r.err)
	}
}

// A session's link read as JSON lines through the core (what Monitor's
// curl -sN does), with wakeshift's URL for it.
func TestLinesURLThroughTheCore(t *testing.T) {
	c := coretest.Start(t)
	app := chatApp(t, c)
	codex := actAs(t, app, "ada", "codex")
	link, err := agentkit.SessionLink(ctx, codex, agentkit.LinkRequest{Label: "monitor"})
	if err != nil {
		t.Fatal(err)
	}
	lineSub(t, c, app, codex, link.Session)
	if !strings.HasPrefix(link.LinesURL(), "http") || !strings.Contains(link.LinesURL(), "format=lines") {
		t.Fatalf("lines URL %s", link.LinesURL())
	}
	rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(rctx, "GET", link.LinesURL(), nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("GET: %v %v", err, resp)
	}
	defer resp.Body.Close()
	postLine(t, c, app, codex, "a line")
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() && sc.Text() == "" { // the empty line it opens with
	}
	if !strings.Contains(sc.Text(), `"subject":"a line"`) || !strings.HasPrefix(sc.Text(), `{"type":"event"`) {
		t.Fatalf("line %q", sc.Text())
	}
}

// A Codex thread with no connector waits for itself: what was queued for
// the agent's connector stream is its own from then on.
func TestWaitInACodexThread(t *testing.T) {
	c := coretest.Start(t)
	app := chatApp(t, c)
	codex := actAs(t, app, "ada", "codex")
	const thread = "019a0000-0000-7000-8000-000000000007"
	sess, err := agentkit.ConversationSession(ctx, codex, thread)
	if err != nil {
		t.Fatal(err)
	}
	lineSub(t, c, app, codex, sess.Handle)
	postLine(t, c, app, codex, "for the thread")
	time.Sleep(300 * time.Millisecond)

	w, err := agentkit.Wait(ctx, codex, agentkit.WaitRequest{Conversation: thread, WaitSeconds: 3})
	if err != nil || w.Session != sess.Handle || len(w.Deliveries) != 1 || w.Deliveries[0].Event.Subject != "for the thread" {
		t.Fatalf("thread wait: %+v %v", w, err)
	}
	if _, err := agentkit.Ack(ctx, codex, sess.Handle, []string{w.Deliveries[0].DeliveryID}); err != nil {
		t.Fatal(err)
	}
	// Present, but no connector delivers to it: beacons keep coming.
	again, _ := agentkit.ConversationSession(ctx, codex, thread)
	if !again.Connected || again.Connector == nil || again.Connector.Running {
		t.Fatalf("after waiting: connected %v, connector %+v", again.Connected, again.Connector)
	}
}

// A wait on a session nothing is subscribed for can get nothing new: it
// answers at once, saying so (unsubscribed), with what was still waiting
// for it, as it does once its only subscription has lapsed and been ended.
func TestWaitSaysWhenNothingCanCome(t *testing.T) {
	c := coretest.Start(t)
	app := chatApp(t, c)
	codex := actAs(t, app, "ada", "codex")

	// Never subscribed.
	start := time.Now()
	w, err := agentkit.Wait(ctx, codex, agentkit.WaitRequest{WaitSeconds: 10})
	if err != nil || !w.Unsubscribed || len(w.Deliveries) != 0 || time.Since(start) > 3*time.Second {
		t.Fatalf("never subscribed: %+v %v, after %v", w, err, time.Since(start))
	}
	session := w.Session

	// Subscribed: an empty wait waits its time.
	lineSub(t, c, app, codex, session)
	start = time.Now()
	if w, err := agentkit.Wait(ctx, codex, agentkit.WaitRequest{Session: session, WaitSeconds: 1}); err != nil || w.Unsubscribed || time.Since(start) < 900*time.Millisecond {
		t.Fatalf("subscribed: %+v %v, after %v", w, err, time.Since(start))
	}

	// A message waits, then the subscription lapses and the expiry pass
	// ends it: the message and the notice come at once, and the wait says
	// nothing more can.
	postLine(t, c, app, codex, "before it lapsed")
	if err := c.DB.InTenant(ctx, codex.Workspace, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE topic_subscriptions SET expires_at = now() - interval '1 second'`); err != nil {
			return err
		}
		_, err := appapi.ExpireTopics(ctx, tx)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	start = time.Now()
	w, err = agentkit.Wait(ctx, codex, agentkit.WaitRequest{Session: session, WaitSeconds: 10})
	if err != nil || !w.Unsubscribed || len(w.Deliveries) != 2 || w.Deliveries[0].Event.Subject != "before it lapsed" ||
		!strings.HasPrefix(w.Deliveries[1].Event.Subject, "Your subscription to chat.line") || time.Since(start) > 3*time.Second {
		t.Fatalf("lapsed: %+v %v, after %v", w, err, time.Since(start))
	}
}

// A subscription that has lapsed doesn't count for a wait even before the
// expiry pass ends it: the wait answers at once, saying nothing more can
// come, rather than waiting its time on a subscription that's over.
func TestWaitDoesntCountALapsedSubscription(t *testing.T) {
	c := coretest.Start(t)
	app := chatApp(t, c)
	codex := actAs(t, app, "ada", "codex")
	w, err := agentkit.Wait(ctx, codex, agentkit.WaitRequest{WaitSeconds: 1})
	if err != nil {
		t.Fatal(err)
	}
	session := w.Session
	lineSub(t, c, app, codex, session)
	if err := c.DB.InTenant(ctx, codex.Workspace, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE topic_subscriptions SET expires_at = now() - interval '1 second'`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	w, err = agentkit.Wait(ctx, codex, agentkit.WaitRequest{Session: session, WaitSeconds: 10})
	if err != nil || !w.Unsubscribed || time.Since(start) > 3*time.Second {
		t.Fatalf("lapsed, not yet ended: %+v %v, after %v", w, err, time.Since(start))
	}
}
