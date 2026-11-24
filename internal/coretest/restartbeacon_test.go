package coretest_test

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/beacon"
	"github.com/wakeshift/server/internal/coretest"
	"github.com/wakeshift/server/internal/harnesses"
)

// The restart that loads Wakeshift's hooks (Copilot CLI, Cursor, agy)
// kills the agent's background command between rounds, and its last wait
// still counts as live for the wait's grace. The agent's first call after
// the restart is often another of the app's tools, within seconds: its
// result must carry a beacon, so the hooks can connect the conversation.
// The beacon names the conversation, is redeemed once, and comes with
// nothing else (no second command to start); the same tool again mints no
// more, and once the hooks have connected, none.
func TestBackgroundRestartBeacon(t *testing.T) {
	c := coretest.Start(t)
	keys := coreKeys(t, c)
	for _, tc := range []struct {
		name string
		call caller
		conv string
	}{
		{"copilot", caller{Client: "copilot-cli", Meta: map[string]any{"wakeshift/conversation": "3c1d6e0a-copilot"}}, "3c1d6e0a-copilot"},
		// Signed in: without an app user, a Cursor result carries no beacon
		// while its command counts as listening, its grace included
		// (TestKeyHolderCantConnectAListeningCommand).
		{"cursor", caller{User: "rb", Client: "Cursor", Conversation: "5e2f7a1b-cursor"}, "5e2f7a1b-cursor"},
		{"antigravity", caller{Client: "antigravity-client", Meta: map[string]any{"antigravity.google/conversation_id": "7a3b8c2d-agy"}}, "7a3b8c2d-agy"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app := noteApp(t, c, "rb-"+tc.name)
			call := tc.call
			call.Tool = "watch"
			sub, err := subscribe(app, call, "ci/acme", subOpts{})
			if err != nil {
				t.Fatal(err)
			}
			url := waitURLIn(sub.Addition.Text)
			if url == "" {
				t.Fatalf("no command: %s", sub.Addition.Text)
			}
			tenant, _ := topicSubRow(t, c, sub.ID)
			// A round of the command ends (quiet), and the harness's restart
			// kills the command before it runs the next: the wait counts as
			// live for its grace, though nothing will run it again. (A wait
			// cut off mid-round stops counting at once.)
			short := strings.Replace(url, "timeout="+harnesses.WaitTimeout, "timeout=1s", 1)
			if short == url {
				t.Fatalf("no timeout=%s to shorten: %s", harnesses.WaitTimeout, url)
			}
			resp, err := (&http.Client{Timeout: 20 * time.Second}).Get(short)
			if err != nil {
				t.Fatal(err)
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if st, _ := noteStatus(app, "ci/acme"); st.Connected != 1 {
				t.Fatalf("not live after a round: the race this covers didn't happen (%+v)", st)
			}
			// The first call after the restart, to another tool: a beacon, and nothing else.
			call.Session, call.Tool = sub.Session, "status"
			got, err := annotate(app, call)
			if err != nil {
				t.Fatal(err)
			}
			tok := beacon.InText(got.Text)
			if tok == "" {
				t.Fatalf("no beacon after the restart: %+v", got)
			}
			if strings.Contains(got.Text, "/v1/stream/wait") || strings.Contains(got.Text, "/install/") {
				t.Fatalf("more than a beacon: %s", got.Text)
			}
			claims, err := beacon.Verify(tok, tc.conv, []string{c.URL}, keys, time.Now())
			if err != nil || claims.Tool != "status" {
				t.Fatalf("beacon for the conversation: %+v %v", claims, err)
			}
			// The same tool again, nothing changed: no second beacon.
			if again, _ := annotate(app, call); beacon.InText(again.Text) != "" {
				t.Fatalf("a second beacon for the same tool: %s", again.Text)
			}
			// Bound to the conversation, and single-use.
			if _, err := beacon.Verify(tok, "another-conversation", []string{c.URL}, keys, time.Now()); err == nil {
				t.Fatal("the beacon verifies for another conversation")
			}
			if code, out := redeemTicket(t, claims.Redeem, claims.Ticket, tc.conv); code != 200 {
				t.Fatalf("redeem: %d %v", code, out)
			}
			if code, _ := redeemTicket(t, claims.Redeem, claims.Ticket, tc.conv); code == 200 {
				t.Fatal("redeemed twice")
			}
			if !hooksConnected(t, c, tenant, tc.conv) {
				t.Fatal("the hooks didn't connect the conversation")
			}
			// Connected: other results carry no beacon, live command or not.
			call.Tool = "log"
			if after, _ := annotate(app, call); beacon.InText(after.Text) != "" {
				t.Fatalf("a beacon once connected: %s", after.Text)
			}
		})
	}
}

// waitURLIn is the session's wait URL in a background result's command.
func waitURLIn(text string) string {
	if m := shellRE.FindStringSubmatch(text); m != nil {
		return strings.ReplaceAll(m[2], `\u0026`, "&")
	}
	if m := loopRE.FindStringSubmatch(text); m != nil {
		return m[2]
	}
	return ""
}

// hooksConnected reports whether Wakeshift's hooks connected the
// conversation's session (a stream runtime the connector has been seen on).
func hooksConnected(t *testing.T, c *coretest.Core, tenant, conversation string) bool {
	t.Helper()
	var ok bool
	if err := c.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT runtime = 'stream' AND connector_seen_at IS NOT NULL FROM sessions WHERE conversation = $1`, conversation).Scan(&ok)
	}); err != nil {
		t.Fatal(err)
	}
	return ok
}
