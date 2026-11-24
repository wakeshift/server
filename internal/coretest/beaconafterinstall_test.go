package coretest_test

import (
	"crypto/ed25519"
	"testing"
	"time"

	"github.com/wakeshift/server/internal/beacon"
	"github.com/wakeshift/server/internal/coretest"
	"github.com/wakeshift/server/pkg/wakeshift"
)

// opencode's plugin loads only when opencode restarts after the install, so
// watch's beacon (in the result that says to install) goes unredeemed, and
// the next tool's result, a moment later, is the first the plugin sees: it
// must carry a beacon for that tool, which connects the conversation
// (wakebench S12: join and post within a minute of watch got none, so
// opencode missed every mention). Repeated calls to a tool without a
// change still mint at most one ticket for it every few minutes. Both for
// an app acting for its agents (Agent Chat) and for a user's conversation.
func TestBeaconAfterInstall(t *testing.T) {
	c := coretest.Start(t)
	keys := coreKeys(t, c)
	t.Run("agent", func(t *testing.T) {
		app, ag := agentsApp(t, c, "chat")
		tok := agentToken(t, c, app, ag)
		call := func(tool, session string) map[string]any {
			m := map[string]any{"agent_token": tok, "harness": "opencode", "tool": tool}
			if session != "" {
				m["session"] = session
			}
			return m
		}
		afterInstall(t, c, app, ag.Workspace, keys, call)
	})
	t.Run("user", func(t *testing.T) {
		app := pokerApp(t, c, "poker")
		call := func(tool, session string) map[string]any {
			m := map[string]any{"user": "u1", "client": "opencode", "tool": tool}
			if session != "" {
				m["session"] = session
			}
			return m
		}
		afterInstall(t, c, app, "", keys, call)
	})
}

// afterInstall: watch, the install (nothing redeems watch's beacon), then
// join (with a pending record) and post (without) within the minute.
func afterInstall(t *testing.T, c *coretest.Core, app *wakeshift.Client, tenant string, keys func(string) ed25519.PublicKey, call func(tool, session string) map[string]any) {
	t.Helper()
	var sub deferredOut
	if code, e := appAPI(t, c, app, "/api/apps/v1/event-subscriptions", map[string]any{"call": call("watch", ""), "name": "game.turn",
		"arguments": map[string]any{"table": "juniper"}, "defer_addition": true}, &sub); code != 200 {
		t.Fatalf("watch: %d %s", code, e)
	}
	if tenant == "" {
		tenant = userTenant(t, c, coretest.CredsOf(app).ID, "u1")
	}
	var watched additionOut
	if code, e := appAPI(t, c, app, "/api/apps/v1/calls/addition", map[string]any{"call": call("watch", sub.Session), "pending": sub.Pending}, &watched); code != 200 {
		t.Fatalf("watch's block: %d %s", code, e)
	}
	if watched.Meta[beacon.MetaKey] == nil || tickets(t, c, tenant) != 1 {
		t.Fatalf("watch's beacon: %+v, %d tickets", watched, tickets(t, c, tenant))
	}
	beaconFor := func(tool string, pending bool) (string, bool) {
		t.Helper()
		body := map[string]any{"call": call(tool, sub.Session)}
		if pending {
			var resolved deferredOut
			if code, e := appAPI(t, c, app, "/api/apps/v1/calls/session", body, &resolved); code != 200 {
				t.Fatalf("calls/session: %d %s", code, e)
			}
			body["pending"] = resolved.Pending
		}
		var block additionOut
		if code, e := appAPI(t, c, app, "/api/apps/v1/calls/addition", body, &block); code != 200 {
			t.Fatalf("%s's block: %d %s", tool, code, e)
		}
		token, ok := block.Meta[beacon.MetaKey].(string)
		return token, ok
	}
	// The user installs and restarts opencode: the next calls, within the
	// minute, carry beacons the plugin can use.
	token, ok := beaconFor("join", true)
	if !ok {
		t.Fatalf("join, right after the install: no beacon")
	}
	const conversation = "ses_opencode1"
	cl, err := beacon.VerifyBound(token, beacon.Binding{Conversation: conversation, Unbound: true}, []string{c.URL}, keys, time.Now())
	if err != nil || cl.Tool != "join" || cl.Thread != "" {
		t.Fatalf("join's beacon: %+v %v", cl, err)
	}
	if _, ok := beaconFor("post", false); !ok {
		t.Fatalf("post, right after the install: no beacon")
	}
	// Nothing changed: the same tools again mint nothing more.
	before := tickets(t, c, tenant)
	for range 3 {
		if _, ok := beaconFor("post", false); ok {
			t.Fatalf("post again, nothing changed: a beacon")
		}
		if _, ok := beaconFor("join", true); ok {
			t.Fatalf("join again, nothing changed: a beacon")
		}
	}
	if n := tickets(t, c, tenant); n != before {
		t.Fatalf("repeated calls minted %d tickets", n-before)
	}
	// The plugin, now loaded, redeems join's beacon: connected.
	if code, out := redeemTicket(t, cl.Redeem, cl.Ticket, conversation); code != 200 {
		t.Fatalf("redeem: %d %v", code, out)
	}
}
