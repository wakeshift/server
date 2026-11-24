package coretest_test

import (
	"strings"
	"testing"
	"time"

	"github.com/wakeshift/server/internal/beacon"
	"github.com/wakeshift/server/internal/coretest"
)

// Codex sends its thread ID, and Antigravity its conversation ID, to every
// MCP server the user has, so an ID can name a conversation but never act
// as it. A malicious server that learned the victim's ID calls the same app
// as a client, with the same client name and _meta and no session key: it
// gets a conversation of its own, never the victim's. It can't read what's
// held for the victim, end the victim's subscriptions or get a beacon (or
// stream) for the victim's session; the victim, with its key, keeps all of
// it.
func TestAConversationIDAloneNeverActsAsTheConversation(t *testing.T) {
	for name, victim := range map[string]caller{
		"codex":       {Client: "codex-mcp-client", Meta: map[string]any{"x-codex-turn-metadata": map[string]any{"thread_id": "codex-victim-id"}}},
		"antigravity": {Client: "antigravity-client", Meta: map[string]any{"antigravity.google/conversation_id": "antigravity-victim-id"}},
	} {
		t.Run(name, func(t *testing.T) {
			c := coretest.Start(t)
			app := noteApp(t, c, "ci")
			theirs, err := subscribe(app, victim, "ci/acme", subOpts{})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := emitNote(app, []string{"ci/acme"}, note{Text: "the victim's build log"}); err != nil {
				t.Fatal(err)
			}
			victimTenant, _ := topicSubRow(t, c, theirs.ID)

			attacker := victim // the same client, the same _meta, no key
			if r, err := annotate(app, attacker); err != nil || strings.Contains(r.Text, "build log") || len(r.Meta) != 0 || beacon.InText(r.Text) != "" {
				t.Fatalf("annotate as the victim: %+v %v", r, err)
			}
			if n, err := unsubscribe(app, attacker, "ci/acme"); err != nil || n != 0 {
				t.Fatalf("unsubscribe as the victim: %d %v", n, err)
			}
			mine, err := subscribe(app, attacker, "ci/acme", subOpts{})
			if err != nil {
				t.Fatal(err)
			}
			tenant, _ := topicSubRow(t, c, mine.ID)
			if mine.Session == theirs.Session || mine.ID == theirs.ID || tenant == victimTenant || strings.Contains(mine.Addition.Text, "build log") {
				t.Fatalf("subscribing as the victim reached its conversation: %+v", mine)
			}
			// Its beacon redeems for its own session, never the victim's.
			tok, _ := mine.Addition.Meta[beacon.MetaKey].(string)
			if tok == "" {
				tok = beacon.InText(mine.Addition.Text)
			}
			links := count(t, c, victimTenant, `SELECT count(*) FROM stream_links`)
			claims, err := beacon.VerifyBound(tok, beacon.Binding{Conversation: name + "-victim-id"}, []string{c.URL}, coreKeys(t, c), time.Now())
			if err != nil {
				t.Fatalf("the attacker's beacon: %v %+v", err, claims)
			}
			if code, _ := redeemTicket(t, claims.Redeem, claims.Ticket, claims.Thread); code != 200 || count(t, c, victimTenant, `SELECT count(*) FROM stream_links`) != links {
				t.Fatalf("the attacker's redeem (%d) reached the victim's workspace", code)
			}
			if n := count(t, c, victimTenant, `SELECT count(*) FROM topic_subscriptions WHERE ended_at IS NULL`); n != 1 {
				t.Fatalf("the victim's subscriptions: %d", n)
			}

			// The victim, with its key: its conversation, and what was held
			// still waits for its own connector.
			victim.Session = theirs.Session
			if _, sub := topicSubRow(t, c, theirs.ID); count(t, c, victimTenant, `SELECT count(*) FROM deliveries WHERE subscription_id = $1 AND state = 'pending'`, sub) != 1 {
				t.Fatal("the victim's held message is gone")
			}
			again, err := subscribe(app, victim, "ci/acme", subOpts{})
			if err != nil || again.ID != theirs.ID || again.Session != theirs.Session {
				t.Fatalf("the victim again: %+v %v", again, err)
			}
		})
	}
}
