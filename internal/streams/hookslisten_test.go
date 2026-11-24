package streams_test

import (
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/agents"
	"github.com/wakeshift/server/internal/dbtest"
	"github.com/wakeshift/server/internal/streams"
)

// hooksClient is what a connector's redeem records on the hooks' link.
const hooksClient = "wakeshift/2026.11.13.0000 claude-code"

// hooksRedeem is what a connector's redeem does to a session, in tx (as
// appapi's: the session's row, the takeover, the hooks' own link) and
// returns that link. polled: their waiter polled it since (used_at), in
// the same transaction, so a test that races the redeem sees both at once.
func (f *fixture) hooksRedeem(tx pgx.Tx, id string, polled bool) string {
	f.t.Helper()
	if _, err := tx.Exec(ctx, `UPDATE sessions SET runtime = 'stream', connector_seen_at = now(), connected_note_due = true WHERE id = $1`, id); err != nil {
		f.t.Fatal(err)
	}
	if err := streams.TakeOver(ctx, tx, "codex", id); err != nil {
		f.t.Fatal(err)
	}
	link, _, err := f.s.Mint(ctx, tx, f.tenant, streams.NewLink{Agent: "codex", Target: id, CreatedBy: "test", TTL: streams.SessionLinkTTL, Connector: hooksClient})
	if err != nil {
		f.t.Fatal(err)
	}
	if polled {
		if _, err := tx.Exec(ctx, `UPDATE stream_links SET used_at = now() WHERE target = $1 AND connector IS NOT NULL`, id); err != nil {
			f.t.Fatal(err)
		}
	}
	return link
}

// #1272: a Claude Code wait (until=hooks) ends only once the hooks' waiter
// listens: a redeem alone (claude -p, a one-shot Agent SDK query: no
// waiter) leaves it running and delivering, and so does a hooks link last
// polled over HooksListenWindow ago. The waiter's poll ends it at the next
// round. Each message arrives once.
func TestUntilHooksWaitsForTheHooksPoll(t *testing.T) {
	f := setup(t)
	base := f.waitServer()
	link, id := f.sessionLinkFor("claude-sdk", streams.SessionLinkTTL)
	done := make(chan waited, 1)
	go func() { done <- wait(t, waitURL(base, link, "timeout=20s&until=hooks")) }()
	waitFor(t, 3*time.Second, "the wait holds the stream", func() bool { return f.holder(id, "poll_wait_") })

	// The redeem, with no waiter after it: it takes the stream over, but
	// the wait isn't told the hooks deliver: it goes on, and delivers.
	var hooks string
	f.tx(func(tx pgx.Tx) error { hooks = f.hooksRedeem(tx, id, false); return nil })
	f.toSession(id, "for the Monitor")
	var w waited
	select {
	case w = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the wait didn't deliver after a redeem alone")
	}
	if w.last["type"] == "hub.closed" {
		t.Fatalf("a redeem alone ended the wait: %v", w.last)
	}
	if len(w.events) == 0 {
		// Told to run again (the takeover): its next round delivers.
		if w.last["type"] != "hub.next" {
			t.Fatalf("after a redeem alone: %v", w.last)
		}
		w = wait(t, waitURL(base, link, "timeout=5s&until=hooks"))
	}
	if len(w.events) != 1 || w.events[0]["subject"] != "for the Monitor" {
		t.Fatalf("the wait after a redeem alone: %+v", w)
	}
	var due bool
	f.tx(func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT connected_note_due FROM sessions WHERE id = $1`, id).Scan(&due)
	})
	if !due {
		t.Fatal("the connected note was spent on a wait the hooks didn't replace")
	}
	cursor := w.cursors[0]

	// A hooks link last polled 3 minutes ago (a waiter that stopped) counts
	// for nothing either.
	f.tx(func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE stream_links SET used_at = now() - interval '3 minutes' WHERE target = $1 AND connector IS NOT NULL`, id)
		return err
	})
	if w := wait(t, waitURL(base, link, "timeout=300ms&until=hooks&after="+cursor)); w.last["type"] != "hub.next" {
		t.Fatalf("a hooks link polled 3 minutes ago: %v", w.last)
	}

	// The waiter's first poll comes while a round holds the stream (it
	// yields to it, patiently): the round ends within a tick, saying the
	// hooks deliver, and the waiter takes the stream. The next message is
	// the waiter's, once.
	round := make(chan waited, 1)
	go func() { round <- wait(t, waitURL(base, link, "timeout=20s&until=hooks")) }()
	waitFor(t, 3*time.Second, "the round holds the stream", func() bool { return f.holder(id, "poll_wait_") })
	start := time.Now()
	polled := make(chan pollBody, 1)
	go func() { polled <- mustPoll(t, base, token(hooks), "wait=10s") }()
	select {
	case w := <-round:
		if w.last["type"] != "hub.closed" || w.last["reason"] != "hooks" || !strings.HasPrefix(w.last["note"].(string), "Wakeshift: connected") {
			t.Fatalf("after the waiter's poll: %v", w.last)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the round went on after the waiter polled")
	}
	f.toSession(id, "for the waiter")
	b := <-polled
	if len(b.Deliveries) != 1 || b.Deliveries[0].Event.Subject != "for the waiter" || b.Deliveries[0].Attempt != 1 || time.Since(start) > 8*time.Second {
		t.Fatalf("the waiter: %+v after %v", b, time.Since(start))
	}
	// And a round started now ends as it starts.
	if w := wait(t, waitURL(base, link, "timeout=5s&until=hooks")); w.last["type"] != "hub.closed" || w.last["reason"] != "hooks" {
		t.Fatalf("a round after the hooks deliver: %v", w.last)
	}
}

// A Claude Code wait loop's round (no until=: Claude Code's sessions end
// once the hooks deliver) is refused only once the hooks' waiter polled.
func TestWaitLoopEndsOnlyOnceTheHooksPoll(t *testing.T) {
	f := setup(t)
	base := f.waitServer()
	link, id := f.loopSession("loop-claude-sdk", true)
	var hooks string
	f.tx(func(tx pgx.Tx) error { hooks = f.hooksRedeem(tx, id, false); return nil })
	f.toSession(id, "for the loop")
	if r := loopRound(t, loopURL(base, link, "1", "timeout=5s")); r.code != 200 || r.closed != nil || len(r.events) != 1 {
		t.Fatalf("a redeem alone: %+v", r)
	}
	// The waiter's first poll, between rounds: the last round still counts
	// as live (its grace), so it yields, but it has polled.
	if b := mustPoll(t, base, token(hooks), "wait=0"); !b.Background {
		t.Fatalf("the waiter's first poll: %+v", b)
	}
	if r := loopRound(t, loopURL(base, link, "1", "timeout=300ms")); r.code != 410 || r.closed == nil || r.closed["reason"] != "hooks" {
		t.Fatalf("after the waiter's poll: %+v", r)
	}
	// The loop's end ends its grace: the waiter takes the stream at once.
	f.toSession(id, "for the waiter")
	start := time.Now()
	if b := mustPoll(t, base, token(hooks), "wait=5s"); b.Background || len(b.Deliveries) != 1 || b.Deliveries[0].Event.Subject != "for the waiter" || time.Since(start) > 3*time.Second {
		t.Fatalf("the waiter after the loop ended: %+v after %v", b, time.Since(start))
	}
}

// No other session's hooks count for a session: not its own agent's other
// session's links, nor a link of another workspace naming its ID as the
// target (row-level security keeps that workspace's rows out), however
// recently they were polled. Only a link the session's own redeem made.
func TestHooksOfAnotherSessionDontCount(t *testing.T) {
	f := setup(t)
	base := f.waitServer()
	link, id := f.sessionLinkFor("victim", streams.SessionLinkTTL)
	_, other := f.sessionLinkFor("neighbor", streams.SessionLinkTTL)
	f.tx(func(tx pgx.Tx) error { f.hooksRedeem(tx, other, true); return nil })

	// Another workspace, with a connector link whose target is this
	// session's ID, polled now.
	t2 := dbtest.NewTenant(t, f.d)
	if err := f.d.InTenant(ctx, t2, func(tx pgx.Tx) error {
		if _, err := agents.Create(ctx, tx, "codex", ""); err != nil {
			return err
		}
		if _, _, err := f.s.Mint(ctx, tx, t2, streams.NewLink{Agent: "codex", Target: id, CreatedBy: "test", TTL: streams.SessionLinkTTL, Connector: hooksClient}); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE stream_links SET used_at = now() WHERE target = $1`, id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// And the session's own row says a connector redeemed for it, as a
	// session the hooks once connected does: still, nothing listens.
	f.tx(func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE sessions SET runtime = 'stream', connector_seen_at = now() WHERE id = $1`, id)
		return err
	})
	f.tx(func(tx pgx.Tx) error {
		for _, s := range []string{id, other} {
			hooks, err := streams.HooksDeliver(ctx, tx, s)
			if err != nil {
				return err
			}
			if hooks != (s == other) {
				t.Errorf("session %s: hooks deliver %v", s, hooks)
			}
		}
		return nil
	})
	if w := wait(t, waitURL(base, link, "timeout=300ms&until=hooks")); w.last["type"] != "hub.next" {
		t.Fatalf("another session's hooks ended this one's wait: %v", w.last)
	}
}
