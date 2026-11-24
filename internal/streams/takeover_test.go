package streams_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/sessions"
	"github.com/wakeshift/server/internal/streams"
)

// startGhostWait starts a wait through a front end and kills its client:
// the server can't tell (the front end hasn't written to the client since),
// so the wait goes on holding the stream, as a killed Monitor's or
// background command's did behind Google's front end.
func (f *fixture) startGhostWait(t *testing.T, base, link, id, query string) {
	t.Helper()
	fe := frontEnd(t, base)
	cctx, kill := context.WithCancel(ctx)
	req, _ := http.NewRequestWithContext(cctx, "GET", waitURL(fe, link, query), nil)
	done := make(chan struct{})
	go func() {
		if resp, err := http.DefaultClient.Do(req); err == nil {
			resp.Body.Close()
		}
		close(done)
	}()
	waitFor(t, 3*time.Second, "the wait holds the stream", func() bool { return f.holder(id, "poll_wait_") })
	kill()
	<-done
}

// holder reports whether a holder starting with prefix holds codex's
// stream (target), live.
func (f *fixture) holder(target, prefix string) bool {
	var ok bool
	f.tx(func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM stream_leases WHERE agent_id = 'codex' AND target = $1 AND starts_with(holder, $2)
			AND renewed_at > now() - interval '1 minute')`, target, prefix).Scan(&ok)
	})
	return ok
}

// The reported loss with hooks installed: the agent's wait command died
// (killed, or its harness restarted) behind a front end, a post went to
// the dead wait, and the hook stood down for it. Now a waiting hook's poll
// (patient: Claude Code's waiter) waits it out and takes the stream as soon
// as the dead wait stops counting as live, and what the wait took comes to
// it, marked as a redelivery.
func TestHooksTakeOverFromADeadWait(t *testing.T) {
	f := setup(t)
	f.s.WaitGrace = 300 * time.Millisecond
	f.s.WaitKeepalive = time.Hour // the front end never writes to the dead client
	base := f.waitServer()
	link, id := f.sessionLinkFor("claude", streams.SessionLinkTTL)
	f.startGhostWait(t, base, link, id, "timeout=2s")
	if !f.holder(id, "poll_wait_") {
		t.Fatal("the dead wait doesn't look live: the test proves nothing")
	}

	// A stop hook stands down at once for a wait that looks live (it
	// mustn't hold the turn open while the command is alive)...
	start := time.Now()
	if b := mustPoll(t, base, token(link), "wait=10s&background=stop"); !b.Background || time.Since(start) > 2*time.Second {
		t.Fatalf("background=stop: %+v after %v", b, time.Since(start))
	}
	// ...but a waiting hook doesn't: it gets the post made meanwhile.
	go func() {
		time.Sleep(200 * time.Millisecond)
		f.toSession(id, "while the command was dead")
	}()
	start = time.Now()
	b := mustPoll(t, base, token(link), "wait=20s")
	if len(b.Deliveries) != 1 || b.Deliveries[0].Event.Subject != "while the command was dead" || b.Background || time.Since(start) > 6*time.Second {
		t.Fatalf("a waiting hook beside a dead wait: %+v after %v", b, time.Since(start))
	}
}

// The bench's S4 on staging: the Monitor's curl was killed behind the
// front end, so its wait went on holding the stream, and a new Monitor
// started. The new wait takes over at once (the newest wins, as with
// connections): the dead one ends "replaced", nothing waits out its
// timeout and grace, and what the dead one took but its client never
// confirmed comes to the new one at once, marked as a redelivery.
func TestNewWaitTakesOverFromAHeldOpenWait(t *testing.T) {
	f := setup(t)
	f.s.WaitKeepalive = time.Hour // the front end never writes to the dead client
	base := f.waitServer()
	link, id := f.sessionLinkFor("claude", streams.SessionLinkTTL)
	f.startGhostWait(t, base, link, id, "timeout=30s")
	if !f.holder(id, "poll_wait_") {
		t.Fatal("the dead wait doesn't look live: the test proves nothing")
	}
	newer := make(chan waited, 1)
	start := time.Now()
	go func() { newer <- wait(t, waitURL(base, link, "timeout=30s")) }()
	time.Sleep(300 * time.Millisecond)
	f.toSession(id, "after the restart")
	select {
	case w := <-newer:
		if len(w.events) != 1 || w.events[0]["subject"] != "after the restart" || time.Since(start) > 3*time.Second {
			t.Fatalf("the new wait: %v after %v", w.events, time.Since(start))
		}
		wait(t, waitURL(base, link, "after="+w.cursors[0]+"&confirm=1")) // printed
	case <-time.After(5 * time.Second):
		t.Fatal("the new wait waited behind the dead one")
	}

	// A dead wait that had taken a post (its answer went into the front
	// end): the next wait gets it at once.
	f.toSession(id, "taken by the dead wait")
	fe := frontEnd(t, base)
	cctx, kill := context.WithCancel(ctx)
	req, _ := http.NewRequestWithContext(cctx, "GET", waitURL(fe, link, "timeout=30s"), nil)
	go func() {
		if resp, err := http.DefaultClient.Do(req); err == nil {
			resp.Body.Close()
		}
	}()
	waitFor(t, 3*time.Second, "the dead wait took it", func() bool {
		var n int
		f.tx(func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM deliveries WHERE target = $1 AND state = 'in_flight' AND lease_holder LIKE 'poll\_wait\_%'`, id).Scan(&n)
		})
		return n == 1
	})
	kill()
	start = time.Now()
	w := wait(t, waitURL(base, link, "timeout=30s"))
	if len(w.events) != 1 || w.events[0]["subject"] != "taken by the dead wait" || w.events[0]["redelivered"] != true || time.Since(start) > 2*time.Second {
		t.Fatalf("after the dead wait's post: %v after %v", w.events, time.Since(start))
	}
}

// The Monitor's next round can start just as the hooks connect: it checks
// for them before their redeem commits, then takes the stream after their
// takeover. It mustn't hold the stream live then, or the hooks' first poll
// stands down for it (CI's TestClaudeHooksTakeOverFromTheMonitor flake).
// The redeem here (as the real one: the session's row, then the lease's,
// in one transaction) stays open until the round is blocked on it, which
// forces that order. It runs on connections of its own, outside the pool
// the server uses (holding a pool slot while the server waits for one
// hung CI, whose pool is 4), and every step is bounded.
func TestUntilHooksWaitRacingTheRedeem(t *testing.T) {
	f := setup(t)
	base := f.waitServer()
	link, id := f.sessionLinkFor("claude", streams.SessionLinkTTL)
	f.toSession(id, "a round before")
	wait(t, waitURL(base, link, "timeout=1s&until=hooks")) // the lease exists, held by a wait

	tctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	conn := func() *pgx.Conn {
		c, err := f.d.Conn(tctx)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close(context.Background()) })
		return c
	}
	watch := conn()
	redeem, err := conn().Begin(tctx)
	if err != nil {
		t.Fatal(err)
	}
	defer redeem.Rollback(context.Background())
	redeem.Exec(tctx, `SELECT set_config('app.tenant_id', $1, true)`, f.tenant)
	// The hooks' link polled in the same transaction: their waiter
	// listens (only then do they end the wait: TestUntilHooksWaitsForTheHooksPoll).
	f.hooksRedeem(redeem, id, true)
	round := make(chan waited, 1)
	go func() { round <- wait(t, waitURL(base, link, "timeout=20s&until=hooks")) }()
	waitFor(t, 5*time.Second, "the round blocked on the redeem", func() bool {
		var blocked bool
		if err := watch.QueryRow(tctx, `SELECT EXISTS (SELECT 1 FROM pg_locks WHERE NOT granted)`).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		return blocked
	})
	if err := redeem.Commit(tctx); err != nil {
		t.Fatal(err)
	}
	select {
	case w := <-round:
		if w.last["type"] != "hub.closed" || w.last["reason"] != "hooks" {
			t.Fatalf("the round: %v", w.last)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the round took the stream back from the hooks")
	}
	if b := mustPoll(t, base, token(link), "wait=0&background=stop"); b.Background {
		t.Fatalf("the hooks' first poll stood down: %+v", b)
	}
}

// A long-lived stream (an older Cursor command's lines with max=) never
// makes a hook stand down: behind a front end, one whose client died
// looked live for 12 minutes. The hook's poll takes the stream.
func TestHooksDontStandDownForALinesStream(t *testing.T) {
	f := setup(t)
	base := f.waitServer()
	link, id := f.sessionLinkFor("cursor", streams.SessionLinkTTL)
	s := openStream(t, onServer(base, streams.HTTPStreamURL(link, streams.FormatLines))+"&max=50", nil)
	_ = s
	waitFor(t, 3*time.Second, "the lines stream holds the stream", func() bool { return f.holder(id, "conn_bg_") })
	f.toSession(id, "for the hook")
	// The lines stream may write it first; either way the hook's poll takes
	// the stream and gets it (again, if the stream had it).
	b := mustPoll(t, base, token(link), "wait=5s&background=stop")
	if b.Background || len(b.Deliveries) != 1 {
		t.Fatalf("a hook beside a lines stream: %+v", b)
	}
}

// When the hooks connect a session (the redeem: TakeOver), whatever held
// its stream stops counting as live and hands back what it had, so the
// hooks' first poll takes over at once. A wait they take it from is told
// to run again (Cursor's and Antigravity's commands keep running, and their
// next wait takes the stream back); Claude Code's (until=hooks) ends with
// the note that the hooks are connected, which is due no more.
func TestTakeOverAtConnect(t *testing.T) {
	f := setup(t)
	base := f.waitServer()
	for _, tc := range []struct {
		query, reason, note string
	}{
		{"timeout=20s", "", "Something else read"},
		{"timeout=20s&until=hooks", "hooks", "Wakeshift: connected."},
	} {
		link, id := f.sessionLinkFor("x"+tc.reason, streams.SessionLinkTTL)
		done := make(chan waited, 1)
		go func() { done <- wait(t, waitURL(base, link, tc.query)) }()
		waitFor(t, 3*time.Second, "the wait holds the stream", func() bool { return f.holder(id, "poll_wait_") })
		start := time.Now()
		if b := mustPoll(t, base, token(link), "wait=0&background=stop"); !b.Background {
			t.Fatalf("before the takeover: %+v", b)
		}
		// The redeem: the session's hooks connect, and take over; their
		// waiter has polled (TestUntilHooksWaitsForTheHooksPoll has a
		// redeem with none).
		f.tx(func(tx pgx.Tx) error { f.hooksRedeem(tx, id, true); return nil })
		if b := mustPoll(t, base, token(link), "wait=0&background=stop"); b.Background {
			t.Fatalf("after the takeover, the hook stood down: %+v", b)
		}
		var w waited
		select {
		case w = <-done:
		case <-time.After(3 * time.Second):
			t.Fatalf("%s: the wait didn't end", tc.query)
		}
		note, _ := w.last["note"].(string)
		switch {
		case tc.reason == "" && (w.last["type"] != "hub.next" || !strings.HasPrefix(note, tc.note)):
			t.Fatalf("%s: %v", tc.query, w.last)
		case tc.reason != "" && (w.last["type"] != "hub.closed" || w.last["reason"] != tc.reason || !strings.HasPrefix(note, tc.note)):
			t.Fatalf("%s: %v", tc.query, w.last)
		}
		var due bool
		f.tx(func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT connected_note_due FROM sessions WHERE id = $1`, id).Scan(&due)
		})
		if due != (tc.reason == "") {
			t.Fatalf("%s: the note due: %v", tc.query, due)
		}
		if time.Since(start) > 3*time.Second {
			t.Fatalf("slow: %v", time.Since(start))
		}
		// Once they're connected, Claude Code's command (until=hooks) ends
		// as it starts, whichever comes first.
		if tc.reason != "" {
			if w := wait(t, waitURL(base, link, tc.query)); w.last["type"] != "hub.closed" || w.last["reason"] != "hooks" {
				t.Fatalf("started after the hooks connected: %v", w.last)
			}
		}
	}
	// What a holder had goes back, for the hooks.
	link, id := f.sessionLinkFor("y", streams.SessionLinkTTL)
	f.toSession(id, "taken by the old wait")
	ev := wait(t, waitURL(base, link, "timeout=1s")).events[0]
	f.tx(func(tx pgx.Tx) error { return streams.TakeOver(ctx, tx, "codex", id) })
	if s := f.state(f.deliveryOf(ev["id"].(string))); s != "pending" {
		t.Fatalf("what the old holder had: %s", s)
	}
	if b := mustPoll(t, base, token(link), "wait=0&background=stop"); len(b.Deliveries) != 1 || b.Deliveries[0].Attempt != 2 {
		t.Fatalf("the hooks' poll: %+v", b)
	}
}

// A sliding session link keeps working while it's used (each use moves its
// expiry SessionLinkTTL ahead, up to SessionLinkMax), stops SessionLinkTTL
// after its last use, and is handed out again with the same URL.
func TestSlidingLink(t *testing.T) {
	f := setup(t)
	base := f.waitServer()
	var link, id string
	f.tx(func(tx pgx.Tx) error {
		s, err := sessions.Register(ctx, tx, sessions.New{Agent: "codex", Label: "slide", CreatedBy: "test"})
		if err != nil {
			return err
		}
		id = s.ID
		var exp time.Time
		link, exp, err = f.s.Mint(ctx, tx, f.tenant, streams.NewLink{Agent: "codex", Target: id, CreatedBy: "app:a", TTL: 30 * time.Second, Slide: streams.SessionLinkMax})
		if err == nil && time.Until(exp) < streams.SessionLinkMax-time.Minute {
			t.Errorf("the token's expiry: %v", exp)
		}
		return err
	})
	again := func() string {
		var u string
		f.tx(func(tx pgx.Tx) (err error) {
			u, err = f.s.SessionLink(ctx, tx, f.tenant, "codex", id, "app:a", "")
			return err
		})
		return u
	}
	if u := again(); u != "" {
		t.Fatalf("a link under a minute from expiry is handed out again: %s", u)
	}
	// Used, it slides.
	wait(t, waitURL(base, link, "timeout=100ms"))
	if u := again(); u != link {
		t.Fatalf("handed out again: %q, want %q", u, link)
	}
	f.tx(func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE stream_links SET expires_at = now() - interval '1 second' WHERE target = $1`, id)
		return err
	})
	if w := wait(t, waitURL(base, link, "timeout=100ms")); w.code != 401 || w.last["type"] != "hub.closed" {
		t.Fatalf("unused past its TTL: %d %v", w.code, w.last)
	}
	if u := again(); u != "" {
		t.Fatalf("an expired link handed out again: %s", u)
	}
	// Another app's, or one with acks (a hook's), isn't.
	f.tx(func(tx pgx.Tx) error {
		_, _, err := f.s.Mint(ctx, tx, f.tenant, streams.NewLink{Agent: "codex", Target: id, CreatedBy: "app:b", TTL: time.Hour, Slide: streams.SessionLinkMax})
		if err == nil {
			_, _, err = f.s.Mint(ctx, tx, f.tenant, streams.NewLink{Agent: "codex", Target: id, CreatedBy: "app:a", Ack: true, TTL: time.Hour, Slide: streams.SessionLinkMax})
		}
		return err
	})
	if u := again(); u != "" {
		t.Fatalf("another app's or a hook's link handed out: %s", u)
	}
}

// A one-way stream without acks ends by OneWayMaxAge (handing over the
// next link), so a dead one lets go of the stream by then.
func TestOneWayStreamsEndByOneWayMaxAge(t *testing.T) {
	f := setup(t)
	f.s.OneWayMaxAge = 300 * time.Millisecond
	link, _ := f.sessionLinkFor("lines", streams.SessionLinkTTL)
	s := openStream(t, streams.HTTPStreamURL(link, streams.FormatLines), nil)
	m := monitorLines(t, s, 1)[0]
	if m["type"] != "hub.rotate" {
		t.Fatalf("got %v", m)
	}
}
