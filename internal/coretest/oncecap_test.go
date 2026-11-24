package coretest_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/coretest"
	"github.com/wakeshift/server/internal/mcpevents"
	"github.com/wakeshift/server/pkg/wakeshift"
)

// emitTo emits chat.message to channel, its text "zq<tok>zq".
func emitTo(t *testing.T, c *coretest.Core, app *wakeshift.Client, channel, tok string) {
	t.Helper()
	if code, e := appAPI(t, c, app, "/api/apps/v1/events/emit", map[string]any{"name": "chat.message", "arguments": map[string]any{"channel": channel},
		"payload": map[string]any{"text": "zq" + tok + "zq", "from": "ada"}, "text": "message zq" + tok + "zq"}, nil); code != 200 {
		t.Fatalf("emit: %d %s", code, e)
	}
}

// A "once" subscription that made its one delivery stops counting toward
// its user's mcpevents.MaxWebhooks: more than MaxWebhooks one-shot voucher
// subscriptions in turn, each delivered, all go through, each getting its
// event exactly once. Its record ends (delivered, audited), the hand-out
// gives a new voucher for the same arguments (not "you're subscribed"),
// and that one gets its own event. The webhook is removed once nothing
// waits for it. Subscriptions that aren't once still count, and another
// user's are untouched.
func TestOnceSubscriptionsFreeTheWebhookCap(t *testing.T) {
	eventsTestSetup(t)
	// One user verifies 43 callbacks on one host within a minute here, past
	// VerifyPerPrincipalHost; this test is about the webhook cap, and
	// TestVerificationPerPrincipalHostLimit is about that limit.
	setVerifyLimits(t, 100, 100)
	c := coretest.Start(t)
	app := voucherApp(t, c, "chat")
	hook := newVoucherHook(t)

	for i := range mcpevents.MaxWebhooks + 1 {
		ch := fmt.Sprintf("job%d", i)
		args := map[string]any{"channel": ch}
		_, tok := handOut(t, c, app, userCall("ada"), "chat.message", args, map[string]any{"once": true})
		if tok == "" {
			t.Fatalf("job %d: no voucher", i)
		}
		code, raw := redeem(t, c, app, redeemReq(userCall("ada"), "chat.message", args, tok, delivery(hook.url(ch), voucherSecret()), nil))
		if code != 200 {
			t.Fatalf("job %d: redeem: %d %s", i, code, raw)
		}
		emitTo(t, c, app, ch, ch)
		waitFor(t, "job "+ch+"'s event", func() bool { return len(hook.texts(ch)) == 1 })
	}
	// Exactly once each, and nothing after its first.
	for i := range mcpevents.MaxWebhooks + 1 {
		emitTo(t, c, app, fmt.Sprintf("job%d", i), "again")
	}
	time.Sleep(400 * time.Millisecond)
	for i := range mcpevents.MaxWebhooks + 1 {
		ch := fmt.Sprintf("job%d", i)
		if got := hook.texts(ch); len(got) != 1 || got[0] != "zq"+ch+"zq" {
			t.Fatalf("%s got %v", ch, got)
		}
	}

	ada := userTenant(t, c, "chat", "ada")
	var live, ended, audited, counted int
	c.DB.InTenant(ctx, ada, func(tx pgx.Tx) error {
		tx.QueryRow(ctx, `SELECT count(*) FILTER (WHERE ended_at IS NULL), count(*) FILTER (WHERE ended_reason = 'delivered') FROM event_subscriptions`).Scan(&live, &ended)
		tx.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE action = 'event_subscription.delivered' AND actor = 'hub'`).Scan(&audited)
		return tx.QueryRow(ctx, `SELECT count(*) FROM mcp_event_webhooks WHERE expires_at > now() AND ending_reason IS NULL AND finished_at IS NULL`).Scan(&counted)
	})
	if live != 0 || ended != mcpevents.MaxWebhooks+1 || audited != mcpevents.MaxWebhooks+1 || counted != 0 {
		t.Fatalf("live %d, ended delivered %d, audited %d, webhooks counted %d", live, ended, audited, counted)
	}
	var liveUntil int
	c.DB.Pool.QueryRow(ctx, `SELECT count(*) FROM subscribe_vouchers WHERE tenant_id = $1 AND live_until > now()`, ada).Scan(&liveUntil)
	if liveUntil != 0 {
		t.Fatalf("%d vouchers still live", liveUntil)
	}

	// The same arguments again: a new voucher and a new subscription (the
	// finished webhook with its ID is replaced), which gets its own event.
	text, tok := handOut(t, c, app, userCall("ada"), "chat.message", map[string]any{"channel": "job0"}, map[string]any{"once": true})
	if tok == "" || strings.Contains(text, "You're subscribed") {
		t.Fatalf("hand-out after a delivered once: %q", text)
	}
	if r := mustRedeem(t, c, app, redeemReq(userCall("ada"), "chat.message", map[string]any{"channel": "job0"}, tok,
		delivery(hook.url("job0"), voucherSecret()), nil)); !r.Created {
		t.Fatalf("not created: %+v", r)
	}
	emitTo(t, c, app, "job0", "second")
	waitFor(t, "the second job0 event", func() bool { return len(hook.texts("job0")) == 2 })
	if got := hook.texts("job0"); got[1] != "zqsecondzq" {
		t.Fatalf("job0 got %v", got)
	}

	// Finished webhooks go once nothing waits for them (a minute after
	// they finished).
	c.DB.InTenant(ctx, ada, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE mcp_event_webhooks SET finished_at = finished_at - interval '2 minutes' WHERE finished_at IS NOT NULL`)
		return err
	})
	waitFor(t, "finished webhooks removed", func() bool {
		var n int
		c.DB.InTenant(ctx, ada, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM mcp_event_webhooks`).Scan(&n)
		})
		return n == 0
	})

	// Subscriptions that aren't once count: MaxWebhooks of them, and the
	// next is refused.
	for i := range mcpevents.MaxWebhooks + 1 {
		ch := fmt.Sprintf("live%d", i)
		args := map[string]any{"channel": ch}
		_, tok := handOut(t, c, app, userCall("ada"), "chat.message", args, nil)
		code, raw := redeem(t, c, app, redeemReq(userCall("ada"), "chat.message", args, tok, delivery(hook.url(ch), voucherSecret()), nil))
		var e struct{ Error struct{ Code string } }
		json.Unmarshal(raw, &e)
		switch {
		case i < mcpevents.MaxWebhooks && code != 200:
			t.Fatalf("live %d: %d %s", i, code, raw)
		case i == mcpevents.MaxWebhooks && (code != 403 || e.Error.Code != "subscription_limit"):
			t.Fatalf("one past the limit: %d %s", code, raw)
		}
	}
	// Another user's are their own.
	_, tok = handOut(t, c, app, userCall("bo"), "chat.message", eng, map[string]any{"once": true})
	mustRedeem(t, c, app, redeemReq(userCall("bo"), "chat.message", eng, tok, delivery(hook.url("bo"), voucherSecret()), nil))
	emitTo(t, c, app, "eng", "bo")
	waitFor(t, "bo's event", func() bool { return len(hook.texts("bo")) == 1 })
	var adaCounted int
	c.DB.InTenant(ctx, ada, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM mcp_event_webhooks WHERE finished_at IS NULL AND ending_reason IS NULL`).Scan(&adaCounted)
	})
	if adaCounted != mcpevents.MaxWebhooks {
		t.Fatalf("ada's counted webhooks: %d", adaCounted)
	}
}
