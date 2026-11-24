package coretest_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/beacon"
	"github.com/wakeshift/server/internal/connector"
	"github.com/wakeshift/server/internal/coretest"
)

// A Cursor conversation without an app user, not connected by hooks: its
// command, which claimed its link, is listening. A call with its
// session_key gets a result for the session as the conversation's own
// calls do.
// That result carries no beacon while the command listens, and a beacon
// minted before the command started can't be redeemed then: a key's
// holder can't connect the session and take its stream from the command,
// whether they name the conversation the session knows or teach it one of
// their own. Once the command has stopped, the conversation's own hooks
// connect from the next result.
func TestKeyHolderCantConnectAListeningCommand(t *testing.T) {
	c := coretest.Start(t)
	url := keylessChat(t, c)
	keys := coreKeys(t, c)
	verify := func(t *testing.T, text, conv string) beacon.Claims {
		t.Helper()
		cl, err := beacon.Verify(beacon.Trailer(text), conv, []string{c.URL}, keys, time.Now())
		if err != nil {
			t.Fatalf("no beacon: %v %q", err, text)
		}
		return cl
	}
	// start is the victim's first call (naming its conversation or not),
	// and its command, run until the session counts it as listening.
	start := func(t *testing.T, args map[string]any) (victim *relayAgent, key string, stop func()) {
		t.Helper()
		victim = keylessAgent(t, url)
		res := victim.call("watch", args)
		key = sessionKeyRE.FindString(allText(res))
		if key == "" {
			t.Fatalf("no session_key in the result: %q", allText(res))
		}
		_, stop = runMonitor(t, shellCommand(t, allText(res)))
		waitFor(t, "the command listening", func() bool { return commandListening(t, c, key) })
		return victim, key, stop
	}

	t.Run("the conversation it knows", func(t *testing.T) {
		const conv = "5d0b5a64-8a4e-4f0a-9d55-2b8a2c6f1f01"
		victim, key, stop := start(t, map[string]any{"harness": "cursor", "conversation_id": conv})
		mallory := keylessAgent(t, url)

		// A beacon from before the command listened (the key's holder kept
		// one, or called while it restarted).
		stop()
		setCommandListening(t, c, key, false)
		early := verify(t, lastText(mallory.call("watch", map[string]any{"harness": "cursor", "conversation_id": conv, "session_key": key})), conv)
		_, stop = runMonitor(t, shellCommand(t, allText(victim.call("watch", map[string]any{"harness": "cursor", "conversation_id": conv, "session_key": key}))))
		waitFor(t, "the command listening again", func() bool { return commandListening(t, c, key) })

		mres := mallory.call("watch", map[string]any{"harness": "cursor", "conversation_id": conv, "session_key": key})
		if tok := beacon.Trailer(lastText(mres)); tok != "" || strings.Contains(allText(mres), "wakeshift-beacon") {
			t.Errorf("a beacon while the command listens: %q", lastText(mres))
		}
		code, out := redeemTicket(t, early.Redeem, early.Ticket, conv)
		if e, _ := out["error"].(map[string]any); code != http.StatusConflict || e["reason"] != "link_not_held" {
			t.Fatalf("a key's holder connected the session: %d %v", code, out)
		}
		if connectorLinks(t, c, key) != 0 {
			t.Fatal("a connector link was minted")
		}

		// The command stops (the restart that loads the hooks): the
		// conversation's own hooks connect from its next result.
		stop()
		setCommandListening(t, c, key, false)
		hook := localHook(t, c, "cursor")
		res := victim.call("watch", map[string]any{"harness": "cursor", "conversation_id": conv, "session_key": key})
		if out := runLocalHook(hook, "", cursorPost(t, conv, "watch", res)); strings.Contains(out, connector.NoteTaken) || strings.Contains(out, connector.NoteLinkNotHeld) {
			t.Fatalf("the conversation's hooks were refused: %s", out)
		}
		if hook.Links.Load(conv) == "" {
			t.Fatal("the conversation's hooks didn't connect")
		}
	})

	t.Run("a conversation the key's holder names", func(t *testing.T) {
		const mine = "5d0b5a64-8a4e-4f0a-9d55-2b8a2c6f1f03"
		_, key, stop := start(t, map[string]any{"harness": "cursor"})
		mallory := keylessAgent(t, url)
		stop()
		setCommandListening(t, c, key, false)
		early := verify(t, lastText(mallory.call("watch", map[string]any{"harness": "cursor", "conversation_id": mine, "session_key": key})), mine)
		setCommandListening(t, c, key, true)
		if code, out := redeemTicket(t, early.Redeem, early.Ticket, mine); code != http.StatusConflict {
			t.Fatalf("a key's holder connected the session: %d %v", code, out)
		}
		if connectorLinks(t, c, key) != 0 {
			t.Fatal("a connector link was minted")
		}
	})
}

// commandListening: the session's background command holds its stream
// (appapi's backgroundLive).
func commandListening(t *testing.T, c *coretest.Core, key string) bool {
	t.Helper()
	var live bool
	if err := c.DB.InTenant(ctx, sessionTenant(t, c, key), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM stream_leases WHERE renewed_at > now() - interval '1 minute'
			AND (holder LIKE 'conn\_bg\_%' OR holder LIKE 'poll\_wait\_%'))`).Scan(&live)
	}); err != nil {
		t.Fatal(err)
	}
	return live
}

// setCommandListening makes the session's command lease count as
// listening (renewed now), or not (renewed long ago: it stopped), without
// waiting out its minute.
func setCommandListening(t *testing.T, c *coretest.Core, key string, on bool) {
	t.Helper()
	at := "now() - interval '1 hour'"
	if on {
		at = "now()"
	}
	if err := c.DB.InTenant(ctx, sessionTenant(t, c, key), func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE stream_leases SET renewed_at = `+at+` WHERE holder LIKE 'conn\_bg\_%' OR holder LIKE 'poll\_wait\_%'`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// connectorLinks counts the links redeems minted in the key's workspace.
func connectorLinks(t *testing.T, c *coretest.Core, key string) int {
	t.Helper()
	var n int
	if err := c.DB.InTenant(ctx, sessionTenant(t, c, key), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM stream_links WHERE coalesce(connector, '') <> ''`).Scan(&n)
	}); err != nil {
		t.Fatal(err)
	}
	return n
}
