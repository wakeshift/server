package coretest_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/coretest"
	"github.com/wakeshift/server/internal/streams"
)

// expectClosed waits for the stream to end with code, saying reason.
func (s *stream) expectClosed(code websocket.StatusCode, reason string) {
	s.t.Helper()
	for {
		select {
		case f := <-s.frames:
			s.t.Fatalf("expected the stream to end, got %q", f.Event.Subject)
		case err := <-s.err:
			if websocket.CloseStatus(err) != code || !strings.Contains(err.Error(), `reason = "`+reason+`"`) {
				s.t.Fatalf("the stream ended with %v, not %d %q", err, code, reason)
			}
			return
		case <-time.After(10 * time.Second):
			s.t.Fatalf("the stream didn't end (%d %q)", code, reason)
		}
	}
}

// A Codex thread whose once subscription got its one delivery has nothing
// left, and its stream ends (4001, "once"), which Codex's connector takes
// as its link revoked: it stops and forgets the link. So the core revokes
// it too: the thread's next subscribe connects it again, with a redeem
// that holds no link, and its event wakes the thread, round after round.
// Before, the link stayed live in the core, so every later redeem was
// refused (409 conversation_taken, link_not_held) until it expired, and
// those events waited, unwoken (seen with real Codex runs).
// The same holds when the thread's hooks redeemed twice (two beacons
// before the connector took one): every link of the session's that a
// connector redeemed ends with its stream.
func TestCodexReconnectsAfterOnce(t *testing.T) {
	for _, redeems := range []int{1, 2} {
		t.Run(map[int]string{1: "one redeem", 2: "two redeems"}[redeems], func(t *testing.T) {
			c := coretest.Start(t)
			app := noteApp(t, c, "ci")
			const thread = "019a14bd-bbbc-7b70-a723-0abb9f3800a1"
			call := caller{User: "ada", Client: "codex-mcp-client", Meta: map[string]any{"x-codex-turn-metadata": map[string]any{"thread_id": thread}}}
			for round := 1; round <= 3; round++ {
				sub, err := subscribe(app, call, "deploys/acme", subOpts{Once: true})
				if err != nil {
					t.Fatal(err)
				}
				// The connector holds no link (it forgot the last at 4001).
				claims := codexBeacon(t, c, sub.Addition, thread)
				code, out := redeemTicket(t, claims.Redeem, claims.Ticket, thread)
				if code != http.StatusOK {
					t.Fatalf("round %d: redeem: %d %v", round, code, out)
				}
				link := out["url"].(string)
				if redeems == 2 {
					// A second beacon, redeemed by hooks that show the link.
					again, err := subscribe(app, call, "deploys/beta", subOpts{Once: true})
					if err != nil {
						t.Fatal(err)
					}
					cl := codexBeacon(t, c, again.Addition, thread)
					code, o := redeemHeld(t, cl, thread, link)
					if code != http.StatusOK || o.URL == "" {
						t.Fatalf("round %d: second redeem: %d %+v", round, code, o)
					}
					link = o.URL
				}
				s := dialStream(t, link, true)
				waitFor(t, "connected", func() bool {
					r, _ := annotate(app, call)
					return len(r.Meta) == 0
				})
				if p, err := emitNote(app, []string{"deploys/acme"}, note{Text: "deploy"}); err != nil || p.Matched != 1 {
					t.Fatalf("round %d: emit: %+v %v", round, p, err)
				}
				s.expect("deploy")
				if redeems == 2 {
					if p, err := emitNote(app, []string{"deploys/beta"}, note{Text: "beta"}); err != nil || p.Matched != 1 {
						t.Fatalf("round %d: emit: %+v %v", round, p, err)
					}
					s.expect("beta")
				}
				s.expectClosed(streams.CodeRevoked, "once")
			}
		})
	}
}

// What the fix leaves alone: while the thread's connector holds its link
// (its stream open, or dropped and not yet back, and the session still has
// something live to receive), a redeem that shows none of the session's
// links is refused (409 conversation_taken), so a session_key's holder
// can't take the thread's messages; and the connector's stream goes on.
func TestNoLinkRedeemRefusedWhileTheConnectorHoldsIt(t *testing.T) {
	c := coretest.Start(t)
	app := noteApp(t, c, "ci")
	const thread = "019a14bd-bbbc-7b70-a723-0abb9f3800b2"
	call := caller{User: "ada", Client: "codex-mcp-client", Meta: map[string]any{"x-codex-turn-metadata": map[string]any{"thread_id": thread}}}
	first, err := subscribe(app, call, "deploys/acme", subOpts{Once: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := subscribe(app, call, "ci/acme", subOpts{}); err != nil {
		t.Fatal(err)
	}
	claims := codexBeacon(t, c, first.Addition, thread)
	code, out := redeemTicket(t, claims.Redeem, claims.Ticket, thread)
	if code != http.StatusOK {
		t.Fatalf("redeem: %d %v", code, out)
	}
	link := out["url"].(string)
	s := dialStream(t, link, true)
	waitFor(t, "connected", func() bool {
		r, _ := annotate(app, call)
		return len(r.Meta) == 0
	})
	// The once subscription's delivery: the other is live, so the stream
	// stays, and its link with it.
	emitNote(app, []string{"deploys/acme"}, note{Text: "deploy"})
	s.expect("deploy")
	s.expectNothing()

	refused := func(when string) {
		t.Helper()
		sub, err := subscribe(app, call, "deploys/acme", subOpts{Once: true})
		if err != nil {
			t.Fatal(err)
		}
		cl := codexBeacon(t, c, sub.Addition, thread)
		code, out := redeemTicket(t, cl.Redeem, cl.Ticket, thread)
		if code != http.StatusConflict {
			t.Fatalf("%s: a redeem without the link: %d %v", when, code, out)
		}
		e, _ := out["error"].(map[string]any)
		if e["code"] != "conversation_taken" || e["reason"] != "link_not_held" {
			t.Fatalf("%s: refused with %v", when, out)
		}
	}
	refused("while its stream is open")
	if p, _ := emitNote(app, []string{"ci/acme"}, note{Text: "CI failed"}); p.Matched != 1 {
		t.Fatalf("matched %d", p.Matched)
	}
	s.expect("CI failed")

	// The connection drops (not 4001: the network), and the connector
	// hasn't reconnected yet: it still holds the link, so still refused,
	// and it reconnects with that link.
	s.ws.CloseNow()
	refused("while its connection is down")
	back := dialStream(t, link, true)
	if p, _ := emitNote(app, []string{"ci/acme"}, note{Text: "CI fixed"}); p.Matched != 1 {
		t.Fatalf("matched %d", p.Matched)
	}
	back.expect("CI fixed")
}

// A redeem in progress for the session as its connector's stream would end
// (the once delivery acknowledged): the end waits for the redeem's lock
// (streams.LockRedeems), and then sees what was committed meanwhile. Here
// the thread subscribed again while the end waited, so the session has
// something to receive: the stream stays, its link isn't revoked, and a
// redeem without it is still refused.
func TestOnceEndWaitsForARedeemInProgress(t *testing.T) {
	c := coretest.Start(t)
	app := noteApp(t, c, "ci")
	const thread = "019a14bd-bbbc-7b70-a723-0abb9f3800c3"
	call := caller{User: "ada", Client: "codex-mcp-client", Meta: map[string]any{"x-codex-turn-metadata": map[string]any{"thread_id": thread}}}
	sub, err := subscribe(app, call, "deploys/acme", subOpts{Once: true})
	if err != nil {
		t.Fatal(err)
	}
	claims := codexBeacon(t, c, sub.Addition, thread)
	code, out := redeemTicket(t, claims.Redeem, claims.Ticket, thread)
	if code != http.StatusOK {
		t.Fatalf("redeem: %d %v", code, out)
	}
	tenant := strings.SplitN(claims.Ticket, "_", 3)[1]
	s := dialStream(t, out["url"].(string), true)
	waitFor(t, "connected", func() bool {
		r, _ := annotate(app, call)
		return len(r.Meta) == 0
	})
	var session string
	if err := c.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT target FROM stream_links WHERE coalesce(connector, '') <> '' LIMIT 1`).Scan(&session)
	}); err != nil {
		t.Fatal(err)
	}

	// A redeem holds the session's lock.
	hold, err := c.DB.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer hold.Rollback(ctx)
	if _, err := hold.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenant); err != nil {
		t.Fatal(err)
	}
	if err := streams.LockRedeems(ctx, hold, session); err != nil {
		t.Fatal(err)
	}
	emitNote(app, []string{"deploys/acme"}, note{Text: "deploy"})
	s.expect("deploy") // acknowledged: the end now waits for the lock
	time.Sleep(300 * time.Millisecond)
	again, err := subscribe(app, call, "deploys/acme", subOpts{Once: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := hold.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	s.expectNothing()
	live := `SELECT count(*) FROM stream_links WHERE coalesce(connector, '') <> '' AND revoked_at IS NULL`
	if n := count(t, c, tenant, live); n != 1 {
		t.Fatalf("live connector links: %d", n)
	}
	cl := codexBeacon(t, c, again.Addition, thread)
	if code, out := redeemTicket(t, cl.Redeem, cl.Ticket, thread); code != http.StatusConflict {
		t.Fatalf("a redeem without the link: %d %v", code, out)
	}
	emitNote(app, []string{"deploys/acme"}, note{Text: "deploy 2"})
	s.expect("deploy 2")
	s.expectClosed(streams.CodeRevoked, "once")
	if n := count(t, c, tenant, live); n != 0 {
		t.Fatalf("live connector links after the end: %d", n)
	}
}
