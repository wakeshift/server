package coretest_test

import (
	"strings"
	"testing"

	"github.com/wakeshift/server/apps/agentkit"
	"github.com/wakeshift/server/internal/core"
	"github.com/wakeshift/server/internal/coretest"
)

// Each core names its own support address (core.Options.SupportEmail,
// WAKESHIFT_SUPPORT_EMAIL) in a suspended app's refusals, today's by
// default: two cores in one process don't change each other's.
func TestSupportEmailPerCore(t *testing.T) {
	ours := coretest.StartWith(t, func(o *core.Options) { o.SupportEmail = "help@example.com" })
	theirs := coretest.Start(t)
	for _, tc := range []struct {
		c    *coretest.Core
		want string
	}{{ours, "help@example.com"}, {theirs, "support@wakeshift.ai"}} {
		app := headlessApp(t, tc.c, "quiz", false)
		ws, err := app.Workspace(ctx, "guest-1", "")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := app.AddAgent(ctx, ws, "claude-code", "Claude Code"); err != nil {
			t.Fatal(err)
		}
		ag, err := app.ActAs(ctx, ws, "claude-code")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tc.c.DB.Pool.Exec(ctx, `UPDATE apps SET suspended_at = now(), suspended_by = 'test', suspended_reason = 'test' WHERE id = 'quiz'`); err != nil {
			t.Fatal(err)
		}
		// The app's own call (its key) and its agent token's.
		if _, err := app.Workspace(ctx, "guest-2", ""); err == nil || !strings.Contains(err.Error(), "its owners can write to "+tc.want) {
			t.Errorf("app call: %v, want the address %s", err, tc.want)
		}
		if _, err := agentkit.Sessions(ctx, ag); err == nil || !strings.Contains(err.Error(), "its owners can write to "+tc.want) {
			t.Errorf("agent token: %v, want the address %s", err, tc.want)
		}
	}
}
