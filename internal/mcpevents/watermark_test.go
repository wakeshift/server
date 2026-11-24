package mcpevents

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/agents"
	"github.com/wakeshift/server/internal/dbtest"
	"github.com/wakeshift/server/internal/deliveries"
	"github.com/wakeshift/server/internal/events"
	"github.com/wakeshift/server/internal/subscriptions"
)

// A webhook's watermark (read by every subscribe and refresh) counts only
// its own conversation's deliveries, and reads them by index: not through
// the thousands an agent can have to its other conversations, which would
// make subscribing slow.
func TestWatermarkReadsOnlyItsConversation(t *testing.T) {
	d := dbtest.New(t)
	ctx := context.Background()
	tenant := dbtest.NewTenant(t, d)
	owner := dbtest.Owner(t, d)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	_, err := owner.Exec(ctx, `INSERT INTO apps (id, name, mcp_url) VALUES ('chat', 'Chat', 'https://chat.example/mcp')`)
	must(err)
	seal, err := NewSealer(make([]byte, 32))
	must(err)
	const others = 5000
	var want int64
	must(d.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		if _, err := agents.Create(ctx, tx, "chatgpt", ""); err != nil {
			return err
		}
		var session string
		if err := tx.QueryRow(ctx, `INSERT INTO sessions (tenant_id, agent_id, handle, created_by) VALUES (current_tenant(), 'chatgpt', 'chatgpt#1', 'user:u') RETURNING id::text`).Scan(&session); err != nil {
			return err
		}
		sub, err := subscriptions.Create(ctx, tx, subscriptions.New{AgentID: "chatgpt", EventType: "test.*", CreatedBy: "test",
			GrantID: dbtest.Grant(t, tx, "chat", "chatgpt", time.Hour)})
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO mcp_event_webhooks (tenant_id, id, app_id, agent_id, session_id, session_handle, name, url, secret, expires_at)
			VALUES (current_tenant(), 'sub_w', 'chat', 'chatgpt', $1, 'chatgpt#1', 'e', 'https://cb.example/', $2, now() + interval '1 hour')`,
			session, seal.seal("sub_w", []byte("s"))); err != nil {
			return err
		}
		// The conversation's own: two acknowledged, then one still pending.
		for i := range 3 {
			e, _, err := events.Append(ctx, tx, events.Event{Type: "test.ping", Source: "test", From: events.Party{Kind: "hub", ID: "hub"}, Trust: events.Internal})
			if err != nil {
				return err
			}
			if _, err := deliveries.Insert(ctx, tx, deliveries.New{AgentID: "chatgpt", Target: session, SubscriptionID: sub.ID, Event: e}); err != nil {
				return err
			}
			if i < 2 {
				if _, err := tx.Exec(ctx, `UPDATE deliveries SET state = 'acked', finished_at = now() WHERE event_id = $1`, e.ID); err != nil {
					return err
				}
			} else {
				want = int64(e.Seq) - 1
			}
		}
		// The agent's other conversations, acknowledged, all after it.
		if _, err := tx.Exec(ctx, `INSERT INTO events SELECT (jsonb_populate_record(NULL::events, to_jsonb(b) || jsonb_build_object('seq', b.seq + g, 'id', gen_random_uuid()))).*
			FROM (SELECT * FROM events ORDER BY seq DESC LIMIT 1) b, generate_series(1, $1) g`, others); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO deliveries (tenant_id, id, agent_id, target, event_id, event_seq, subscription_id, state, finished_at)
			SELECT current_tenant(), gen_random_uuid(), 'chatgpt', 'other-' || (e.seq % 500), e.id, e.seq, $1, 'acked', now()
			FROM events e WHERE e.seq > $2`, sub.ID, want+1)
		return err
	}))
	_, err = owner.Exec(ctx, `ANALYZE deliveries`)
	must(err)

	must(d.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		got, err := Watermark(ctx, tx, "sub_w")
		if err != nil {
			return err
		}
		if got != want {
			t.Errorf("watermark %d, want %d (just before the pending delivery)", got, want)
		}
		var plan []byte
		if err := tx.QueryRow(ctx, `EXPLAIN (FORMAT JSON) `+watermarkSQL, "sub_w").Scan(&plan); err != nil {
			return err
		}
		var used []string
		var walk func(any)
		walk = func(v any) {
			switch v := v.(type) {
			case map[string]any:
				if v["Relation Name"] == "deliveries" {
					used = append(used, v["Node Type"].(string)+" "+fmtIndex(v["Index Name"]))
				}
				for _, c := range v {
					walk(c)
				}
			case []any:
				for _, c := range v {
					walk(c)
				}
			}
		}
		var doc any
		if err := json.Unmarshal(plan, &doc); err != nil {
			return err
		}
		walk(doc)
		// The conversation's pending ones by the queue's index, the newest
		// of any state by deliveries_target_seq: both start at the
		// conversation, so the agent's other deliveries are never read.
		for _, u := range used {
			if u != "Index Scan deliveries_queue" && !strings.HasSuffix(u, " deliveries_target_seq") {
				t.Errorf("the watermark reads deliveries with %q (all: %v): not from its conversation", u, used)
			}
		}
		if !slices.ContainsFunc(used, func(u string) bool { return strings.HasSuffix(u, " deliveries_target_seq") }) {
			t.Errorf("the newest delivery isn't read by deliveries_target_seq: %v", used)
		}
		if t.Failed() {
			t.Logf("plan: %s", plan)
		}
		return nil
	}))
}

func fmtIndex(v any) string {
	s, _ := v.(string)
	return s
}
