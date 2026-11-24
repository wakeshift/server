package coretest_test

import (
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/connector"
	"github.com/wakeshift/server/internal/coretest"
)

// waiterPolls polls the link a redeem answered (out["url"]) once, as the
// hooks' waiter does when it starts listening: only from then on do the
// hooks count as delivering the session's messages (#1272:
// streams.HooksDeliver). A redeem alone counts for nothing.
func waiterPolls(t *testing.T, c *coretest.Core, out map[string]any) {
	t.Helper()
	link, _ := out["url"].(string)
	if link == "" {
		t.Fatalf("no link in the redeem's answer: %v", out)
	}
	if _, err := (&connector.HTTPPoller{Issuers: []string{c.URL}}).Poll(ctx, link, 0, false); err != nil {
		t.Fatalf("the waiter's poll: %v", err)
	}
}

// hooksPolledSQL gives each session the condition selects (SQL on
// sessions) a connector's link, as a redeem makes, last polled ago (a
// Postgres interval: "0 seconds" for a waiter that polled just now).
func hooksPolledSQL(tx pgx.Tx, where, ago string) error {
	_, err := tx.Exec(ctx, `INSERT INTO stream_links (tenant_id, agent_id, target, created_by, expires_at, connector, used_at)
		SELECT tenant_id, agent_id, id::text, 'test', now() + interval '1 hour', 'wakeshift/2026.11.13.0000 claude-code', now() - $1::interval
		FROM sessions WHERE `+where, ago)
	return err
}
