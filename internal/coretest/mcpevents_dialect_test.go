package coretest_test

import (
	"encoding/json"
	"regexp"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/coretest"
	"github.com/wakeshift/server/internal/mcpevents"
	"github.com/wakeshift/server/internal/mcpevents/event"
)

// A webhook's dialect is picked from the client that subscribes, by the
// client table (event.WebhookDialect: clientInfo name, User-Agent product,
// _meta), and kept with the webhook. The spec's (everyone not listed) gets
// a webhook-id of its own per message, never its eventId, the same on
// every retry; TanStack AI and the Vercel AI SDK keep webhook-id equal to
// eventId, with gap envelopes; OpenClaw and ChatGPT keep it equal and get
// no envelopes (OpenClaw answers them 400). Every body has the cursor key
// (OpenClaw answers a body without one 400).
func TestMCPEventsWebhookDialects(t *testing.T) {
	msgID := regexp.MustCompile(`^msg_[0-9a-f]{32}$`)
	for _, tc := range []struct {
		name, client, ua string
		meta             map[string]any
		dialect          string
		envelopes        int
	}{
		{"a spec client", "acme-agent", "acme-agent/2", nil, event.DialectSpec, 1},
		{"a client declaring the extension (spec)", "mcpkit-client", "Go-http-client/1.1",
			map[string]any{"io.modelcontextprotocol/clientCapabilities": map[string]any{"extensions": map[string]any{event.ExtensionKey: map[string]any{}}}},
			event.DialectSpec, 1},
		{"TanStack AI", "tanstack-ai-mcp", "node", nil, event.DialectDraft, 1},
		{"the Vercel AI SDK, renamed", "my-app", "ai-sdk/2.0.97 runtime/node.js/22", nil, event.DialectDraft, 1},
		{"OpenClaw", "openclaw", "undici", nil, event.DialectOpenClaw, 0},
		{"ChatGPT", "openai-mcp (Codex)", "openai-mcp/1.0.0 (Codex)", nil, event.DialectChatGPT, 0},
		{"a ChatGPT Work chat by its openai/session", "acme-agent", "", map[string]any{"openai/session": "conv-1"}, event.DialectChatGPT, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			eventsTestSetup(t)
			c := coretest.Start(t)
			app, endpoint := chatEndpoint(t, c, "chat")
			cb := newFakeChatGPT(t)
			p := subscribeParams("eng", cb.url(), cb.secret())
			meta := map[string]any{"io.modelcontextprotocol/clientInfo": map[string]any{"name": tc.client, "version": "1"}}
			for k, v := range tc.meta {
				meta[k] = v
			}
			p["_meta"] = meta
			if res := rpcAs(t, endpoint, "u1", tc.ua, nil, "events/subscribe", p); res.Error != nil {
				t.Fatalf("subscribe: %+v", res.Error)
			}
			var dialect string
			if err := c.DB.InTenant(ctx, tenantOf(t, c, "chat", "u1"), func(tx pgx.Tx) error {
				return tx.QueryRow(ctx, `SELECT dialect FROM mcp_event_webhooks`).Scan(&dialect)
			}); err != nil || dialect != tc.dialect {
				t.Fatalf("dialect %q (%v), want %q", dialect, err, tc.dialect)
			}

			// The first event fails until it's abandoned (a gap); the next
			// goes through.
			cb.status = func(n int) int {
				if n <= mcpevents.MaxAttempts {
					return 500
				}
				return 200
			}
			publish(t, c, app, "lost", "next")
			waitFor(t, "the next event", func() bool { return len(cb.got()) == 1 })
			ids, e := cb.triedIDs(), cb.got()[0]
			if len(ids) != mcpevents.MaxAttempts+1 {
				t.Fatalf("%d attempts", len(ids))
			}
			for _, id := range ids[1:mcpevents.MaxAttempts] {
				if id != ids[0] {
					t.Fatalf("a retry's webhook-id changed: %v", ids)
				}
			}
			if ids[0] == e.ID {
				t.Fatalf("two messages, one webhook-id: %v", ids)
			}
			if tc.dialect == event.DialectSpec {
				if e.ID == e.Body.EventID || !msgID.MatchString(e.ID) || !msgID.MatchString(ids[0]) {
					t.Fatalf("spec webhook-id %q, eventId %q (attempts %v)", e.ID, e.Body.EventID, ids)
				}
			} else if e.ID != e.Body.EventID {
				t.Fatalf("webhook-id %q, eventId %q", e.ID, e.Body.EventID)
			}
			if n := len(cb.envelopes()); n != tc.envelopes {
				t.Fatalf("%d envelopes, want %d: %+v", n, tc.envelopes, cb.envelopes())
			}
			var body map[string]json.RawMessage
			if err := json.Unmarshal(e.Raw, &body); err != nil {
				t.Fatal(err)
			}
			if cursor, ok := body["cursor"]; !ok || string(cursor) == "null" || e.Body.Cursor == nil || *e.Body.Cursor == "" {
				t.Fatalf("the body's cursor: %s", e.Raw)
			}
		})
	}
}

// A webhook keeps the dialect it was made with: one made before dialects
// were per client (draft, whatever the client) keeps webhook-id equal to
// eventId on its refreshes and deliveries.
func TestMCPEventsWebhookKeepsItsDialect(t *testing.T) {
	c, app, endpoint, cb, channel := eventsConversation(t, false) // acme-agent: the spec's
	tenant := tenantOf(t, c, "chat", "u1")
	if err := c.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE mcp_event_webhooks SET dialect = 'draft'`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if res := draft(t, endpoint, "u1", "events/subscribe", draftParams(channel, cb.url(), cb.secret())); res.Error != nil {
		t.Fatalf("refresh: %+v", res.Error)
	}
	publish(t, c, app, "one")
	waitFor(t, "the event", func() bool { return len(cb.got()) == 1 })
	if e := cb.got()[0]; e.ID != e.Body.EventID {
		t.Fatalf("webhook-id %q, eventId %q", e.ID, e.Body.EventID)
	}
}
