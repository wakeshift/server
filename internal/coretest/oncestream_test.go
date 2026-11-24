package coretest_test

import (
	"testing"
	"time"

	"github.com/wakeshift/server/internal/beacon"
	"github.com/wakeshift/server/internal/coretest"
)

// A Codex thread's stream is its session's, not a subscription's: with two
// subscriptions on the thread, one of them once, the once delivery ends
// only that subscription, and the other's events keep arriving on the
// same stream (no new beacon redeemed), whichever order they were made in
// and whether or not the other is once too.
func TestOnceKeepsTheThreadsStream(t *testing.T) {
	for _, order := range []string{"both before connecting", "second after connecting", "both once"} {
		t.Run(order, func(t *testing.T) {
			c := coretest.Start(t)
			app := noteApp(t, c, "ci")
			const thread = "019a14bd-bbbc-7b70-a723-0abb9f38118f"
			codexCall := caller{User: "ada", Client: "codex-mcp-client", Meta: map[string]any{"x-codex-turn-metadata": map[string]any{"thread_id": thread}}}

			first, err := subscribe(app, codexCall, "deploys/acme", subOpts{Once: true})
			if err != nil {
				t.Fatal(err)
			}
			var second noteSub
			if order != "second after connecting" {
				if second, err = subscribe(app, codexCall, "ci/acme/api", subOpts{Once: order == "both once", Replay: 10 * time.Minute}); err != nil {
					t.Fatal(err)
				}
			}
			tok, _ := first.Addition.Meta[beacon.MetaKey].(string)
			claims, err := beacon.Verify(tok, thread, []string{c.URL}, coreKeys(t, c), time.Now())
			if err != nil {
				t.Fatalf("beacon: %v", err)
			}
			code, redeemed := redeemTicket(t, claims.Redeem, claims.Ticket, thread)
			if code != 200 {
				t.Fatalf("redeem: %d %v", code, redeemed)
			}
			codex := dialStream(t, redeemed["url"].(string), true)
			if order == "second after connecting" {
				waitFor(t, "connected", func() bool {
					r, _ := annotate(app, codexCall)
					return len(r.Meta) == 0
				})
				if second, err = subscribe(app, codexCall, "ci/acme/api", subOpts{}); err != nil {
					t.Fatal(err)
				}
			}
			if second.Session != first.Session {
				t.Fatalf("two sessions for one thread: %s, %s", first.Session, second.Session)
			}

			emitNote(app, []string{"deploys/acme"}, note{Text: "deploy 1"})
			codex.expect("deploy 1")
			time.Sleep(time.Second) // the runner ends the once subscription; a keepalive could end the stream
			if p, _ := emitNote(app, []string{"ci/acme/api"}, note{Text: "CI failed"}); p.Matched != 1 {
				t.Fatalf("the other subscription: matched %d", p.Matched)
			}
			codex.expect("CI failed")
			if p, _ := emitNote(app, []string{"deploys/acme"}, note{Text: "deploy 2"}); p.Matched != 0 {
				t.Fatalf("after once: matched %d", p.Matched)
			}
			// The stream is still the one the first beacon opened. (With both
			// once, it ends at its next renewal, 4001 "once": nothing is left.)
			codex.expectNothing()
		})
	}
}
