package console_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/coretest"
)

type auditEntry struct {
	ID      int64
	Actor   string
	Email   string `json:"actor_email"`
	Action  string
	AgentID string `json:"agent_id"`
	AppID   string `json:"app_id"`
	GrantID string `json:"grant_id"`
	Detail  map[string]any
	Shown   *struct {
		Risks  []struct{ Title string }
		DryRun struct{ Days int } `json:"dry_run"`
	}
}

// Every revocation and change made in the console shows in the
// workspace's audit log, newest first, a page at a time: here, revoking a
// grant an app was left with from account mode (turned off: nothing new
// is granted in a person's own workspace).
func TestConsoleActionsAreAudited(t *testing.T) {
	c := coretest.Start(t)
	c.RegisterApp("agentchat", "Agent Chat", "https://chat.test/mcp")
	ada := c.SignIn("ada@example.com")
	var me struct{ User struct{ ID string } }
	ada.Do("GET", "/api/v1/session", nil, &me)
	user := "user:" + me.User.ID
	if code := ada.Do("POST", "/api/v1/agents", map[string]string{"id": "claude-code"}, nil); code != 200 && code != 201 {
		t.Fatalf("agent: %d", code)
	}
	legacyGrant(t, c, ada.Workspace(t), me.User.ID, "agentchat", "claude-code")

	var sub struct{ ID string }
	ada.Do("POST", "/api/v1/subscriptions", map[string]any{"agent_id": "claude-code", "event_type": "test.ping"}, &sub)
	ada.Do("POST", "/api/v1/subscriptions/"+sub.ID+"/cancel", nil, nil)
	var minted struct{ URL string }
	ada.Do("POST", "/api/v1/agents/claude-code/stream-link", map[string]bool{}, &minted)
	ada.Do("POST", "/api/v1/agents/claude-code/stream-links/revoke", nil, nil)
	var listed struct {
		Apps []struct {
			ID     string
			Grants []struct{ ID string }
		}
	}
	ada.Do("GET", "/api/v1/apps", nil, &listed)
	if len(listed.Apps) != 1 || listed.Apps[0].ID != "agentchat" || len(listed.Apps[0].Grants) != 1 {
		t.Fatalf("apps: %+v", listed)
	}
	ada.Do("POST", "/api/v1/grants/"+listed.Apps[0].Grants[0].ID+"/revoke", nil, nil)

	var page struct{ Entries []auditEntry }
	if code := ada.Do("GET", "/api/v1/audit", nil, &page); code != 200 {
		t.Fatalf("audit: %d", code)
	}
	var lines []string
	for _, e := range page.Entries {
		lines = append(lines, fmt.Sprintf("%s %s %s/%s", e.Action, e.Actor, e.AppID, e.AgentID))
	}
	want := []string{
		"grant.revoked " + user + " agentchat/claude-code",
		"stream_links.revoked " + user + " /claude-code",
		"stream_link.created " + user + " /claude-code",
		"subscription.canceled " + user + " /claude-code",
		"subscription.created " + user + " /claude-code",
	}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Fatalf("audit log:\n%s\nwant:\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}
	if page.Entries[0].Email != "ada@example.com" {
		t.Fatalf("actor email %q", page.Entries[0].Email)
	}
	if page.Entries[0].Detail["subscriptions_canceled"] != float64(0) || page.Entries[1].Detail["revoked"] != float64(1) {
		t.Fatalf("revocations say what they undid: %v %v", page.Entries[0].Detail, page.Entries[1].Detail)
	}
	// Minting a link names it by its ID, with its expiry, never its URL.
	made := page.Entries[2].Detail
	if minted.URL == "" || made["link"] == nil || made["link"] == "" || made["expires_at"] == nil || made["ack"] != false {
		t.Fatalf("stream_link.created: %v", made)
	}
	var raw string
	if err := c.DB.InTenant(context.Background(), ada.Workspace(t), func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT coalesce(string_agg(detail::text, ' '), '') FROM audit_log`).Scan(&raw)
	}); err != nil || !strings.Contains(raw, made["link"].(string)) {
		t.Fatalf("the workspace's audit log: %v %s", err, raw)
	}
	if u := minted.URL; strings.Contains(raw, u) || strings.Contains(raw, u[strings.LastIndex(u, "/")+1:]) {
		t.Fatalf("the audit log holds the link's URL or token: %s", raw)
	}
	ada.Do("GET", "/api/v1/apps", nil, &listed)
	if len(listed.Apps) != 0 {
		t.Fatalf("apps after revoking: %+v", listed)
	}

	// Pages.
	var first, next struct{ Entries []auditEntry }
	ada.Do("GET", "/api/v1/audit?limit=3", nil, &first)
	ada.Do("GET", fmt.Sprintf("/api/v1/audit?limit=100&before=%d", first.Entries[2].ID), nil, &next)
	if len(first.Entries) != 3 || len(next.Entries) != 2 || next.Entries[1].Action != "subscription.created" {
		t.Fatalf("pages: %d then %d", len(first.Entries), len(next.Entries))
	}
	for _, q := range []string{"limit=0", "limit=101", "before=x", "before=-1"} {
		if code := ada.Do("GET", "/api/v1/audit?"+q, nil, nil); code != 400 {
			t.Errorf("%s: %d", q, code)
		}
	}

	// Another workspace sees none of it (an empty list, not null).
	var other map[string]any
	c.SignIn("bob@example.com").Do("GET", "/api/v1/audit", nil, &other)
	if entries, ok := other["entries"].([]any); !ok || len(entries) != 0 {
		t.Fatalf("bob's workspace: %v", other)
	}
}

// legacyGrant records app's grant for agent in a person's own workspace,
// as Sign in with Wakeshift made them before account mode was turned off.
func legacyGrant(t *testing.T, c *coretest.Core, tenant, user, app, agent string) string {
	t.Helper()
	ctx := context.Background()
	var grant string
	err := c.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `INSERT INTO grants (tenant_id, app_id, kind, agent_id, scope, shown, granted_by, expires_at, confirmed_by)
			VALUES (current_tenant(), $1, 'agent', $2, '{"agent":{"wake":[],"post":[],"ttl_days":30}}', '{}', $3, now() + interval '30 days', 'user')
			RETURNING id::text`, app, agent, user).Scan(&grant)
	})
	if err != nil {
		t.Fatal(err)
	}
	return grant
}
