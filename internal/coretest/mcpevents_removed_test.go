package coretest_test

import (
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/apps"
)

// An event type the app's manifest drops ends its webhooks at the next
// recheck: the receiver gets terminated with NotFound (data.kind "event"),
// as the draft says, not Forbidden, so it lists the events again.
func TestNativeEventTypeRemoved(t *testing.T) {
	c, _, cb, res := nativeChat(t)
	tenant := userTenant(t, c, "chat", "u1")
	got, err := c.Apps.Get(ctx, "chat")
	if err != nil {
		t.Fatal(err)
	}
	m := got.Manifest
	var kept []apps.EventType
	for _, e := range m.Events {
		if e.Name != "chat.message" {
			kept = append(kept, e)
		}
	}
	m.Events = kept
	if err := c.Apps.SetManifest(ctx, "chat", m); err != nil {
		t.Fatal(err)
	}
	if err := c.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE mcp_event_webhooks SET authorized_at = now() - interval '2 hours'`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the terminated envelope", func() bool { return len(cb.envelopes()) == 1 })
	env := cb.envelopes()[0]
	if env.Type != "terminated" || env.Error.Code != -32023 || env.Error.Message != "NotFound" || env.Error.Data["kind"] != "event" {
		t.Fatalf("terminated: %+v", env)
	}
	waitFor(t, "the webhook removed", func() bool {
		var n int
		c.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM mcp_event_webhooks WHERE id = $1`, res["id"]).Scan(&n)
		})
		return n == 0
	})
}
