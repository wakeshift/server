package console_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/agents"
	"github.com/wakeshift/server/internal/db"
	"github.com/wakeshift/server/internal/sessions"
	"github.com/wakeshift/server/internal/subscriptions"
)

// seedEventSub records an event subscription to the chat app's
// chat.message in a workspace, routed by a topic subscription of its own,
// as the app API makes them.
func seedEventSub(t *testing.T, d *db.DB, tenant string) string {
	t.Helper()
	ctx := context.Background()
	if _, err := d.Pool.Exec(ctx, `INSERT INTO apps (id, name, manifest) VALUES ('chat', 'Chat',
		'{"events":[{"name":"chat.message","description":"A message in a channel.","topic":"channels/{channel}","arguments":{"type":"object"},"payload":{"type":"object"}}]}')
		ON CONFLICT DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	var id string
	if err := d.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		if _, err := agents.Create(ctx, tx, "claude-code", ""); err != nil {
			return err
		}
		s, err := sessions.Register(ctx, tx, sessions.New{Agent: "claude-code", Label: "abc", CreatedBy: "app:chat"})
		if err != nil {
			return err
		}
		sub, err := subscriptions.Create(ctx, tx, subscriptions.New{AgentID: "claude-code", EventType: "topics.chat", CreatedBy: "app:chat", SessionID: s.ID})
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO topic_subscriptions (tenant_id, subscription_id, app_id, topic, session_id, ttl_seconds, expires_at)
			VALUES (current_tenant(), $1, 'chat', 'channels/eng', $2, 3600, now() + interval '1 hour')`, sub.ID, s.ID); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `INSERT INTO event_subscriptions (tenant_id, source_kind, source_id, event, arguments, arguments_key, destination,
			agent_id, session_id, subscription_id, created_by)
			VALUES (current_tenant(), 'app', 'chat', 'chat.message', '{"channel":"eng"}', 'k', 'conversation', 'claude-code', $1, $2, 'app:chat')
			RETURNING id::text`, s.ID, sub.ID).Scan(&id)
	}); err != nil {
		t.Fatal(err)
	}
	return id
}

type listedEventSub struct {
	ID          string `json:"id"`
	SourceName  string `json:"source_name"`
	Event       string `json:"event"`
	Description string `json:"description"`
	Arguments   map[string]string
	EndedReason string `json:"ended_reason"`
}

// The console lists a workspace's event subscriptions and revokes them one
// at a time, and only its own: another workspace's are neither listed nor
// revocable.
func TestConsoleEventSubscriptions(t *testing.T) {
	d, ada := setup(t)
	ada.signIn()
	bob := newClient(t, ada.base)
	bob.signInAs("bob@example.com")
	var s struct{ Workspace struct{ ID string } }
	ada.do("GET", "/api/v1/session", nil, &s)
	id := seedEventSub(t, d, s.Workspace.ID)

	var list struct{ Subscriptions []listedEventSub }
	if code := ada.do("GET", "/api/v1/event-subscriptions", nil, &list); code != 200 || len(list.Subscriptions) != 1 {
		t.Fatalf("ada's list: %d %+v", code, list)
	}
	if got := list.Subscriptions[0]; got.ID != id || got.SourceName != "Chat" || got.Event != "chat.message" || got.Description != "A message in a channel." ||
		got.Arguments["channel"] != "eng" {
		t.Fatalf("listed: %+v", got)
	}
	list.Subscriptions = nil
	if code := bob.do("GET", "/api/v1/event-subscriptions", nil, &list); code != 200 || len(list.Subscriptions) != 0 {
		t.Fatalf("bob sees ada's: %+v", list)
	}
	if code := bob.do("POST", "/api/v1/event-subscriptions/"+id+"/revoke", nil, nil); code != 404 {
		t.Fatalf("bob revoking ada's: %d", code)
	}
	for _, bad := range []string{"not-a-uuid", "00000000-0000-4000-8000-000000000000"} {
		if code := ada.do("POST", "/api/v1/event-subscriptions/"+bad+"/revoke", nil, nil); code != 404 {
			t.Errorf("revoke %q: %d", bad, code)
		}
	}
	var revoked listedEventSub
	if code := ada.do("POST", "/api/v1/event-subscriptions/"+id+"/revoke", nil, &revoked); code != 200 || revoked.EndedReason != "revoked" {
		t.Fatalf("revoke: %d %+v", code, revoked)
	}
	if code := ada.do("POST", "/api/v1/event-subscriptions/"+id+"/revoke", nil, nil); code != 404 {
		t.Fatalf("revoking twice: %d", code)
	}
	ada.do("GET", "/api/v1/event-subscriptions", nil, &list)
	if len(list.Subscriptions) != 1 || list.Subscriptions[0].EndedReason != "revoked" {
		t.Fatalf("after revoking: %+v", list.Subscriptions)
	}
	var actor string
	d.InTenant(context.Background(), s.Workspace.ID, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT actor FROM audit_log WHERE action = 'event_subscription.revoked'`).Scan(&actor)
	})
	if len(actor) < 6 || actor[:5] != "user:" {
		t.Fatalf("revocation actor: %q", actor)
	}
}
