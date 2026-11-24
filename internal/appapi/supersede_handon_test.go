package appapi

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/dbtest"
	"github.com/wakeshift/server/internal/events"
	"github.com/wakeshift/server/internal/sessions"
	"github.com/wakeshift/server/internal/subscriptions"
)

// A publish reached the old session's subscription, and the pipeline
// (which makes deliveries after the publish returns) hasn't made its
// delivery yet when the conversation's hook connects a newer session of the
// same app. Superseding the old session must still hand it on: the newer
// session's subscription starts after it, so ending the old one without
// catching up lost it (TestClaudeCodeHooksNewSessionSameApp failed on it in
// CI, now and then, when the pipeline was slower than the redeem). No
// pipeline runs here, so nothing else makes the delivery.
func TestSupersedeHandsOnWhatThePipelineHasntDelivered(t *testing.T) {
	d := dbtest.New(t)
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := d.Pool.Exec(ctx, `INSERT INTO apps (id, name) VALUES ('quizzer', 'Quizzer')`); err != nil {
		t.Fatal(err)
	}
	tenant := dbtest.NewTenant(t, d)
	const thread, topic = "thread-1", "rounds/1"
	var grant, link, oldSession, newSession, oldSub, newSub, event string
	watch := func(tx pgx.Tx, conversation string) (session, sub string, err error) {
		s, err := sessions.Register(ctx, tx, sessions.New{Agent: "claude", Conversation: conversation, CreatedBy: "app:quizzer"})
		if err != nil {
			return "", "", err
		}
		if err := sessions.SetWatching(ctx, tx, s.ID, true); err != nil {
			return "", "", err
		}
		ns, err := subscriptions.Create(ctx, tx, subscriptions.New{AgentID: "claude", EventType: topicsType("quizzer"), CreatedBy: "app:quizzer",
			GrantID: grant, SessionID: s.ID, Filters: map[string]string{topicAttr(topic): "1"}})
		if err != nil {
			return "", "", err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO topic_subscriptions (tenant_id, subscription_id, app_id, topic, session_id, ttl_seconds, expires_at)
			VALUES (current_tenant(), $1, 'quizzer', $2, $3, 3600, now() + interval '1 hour')`, ns.ID, topic, s.ID); err != nil {
			return "", "", err
		}
		_, err = tx.Exec(ctx, `INSERT INTO topic_members (app_id, topic, tenant_id, subscription_id) VALUES ('quizzer', $1, current_tenant(), $2)`, topic, ns.ID)
		return s.ID, ns.ID, err
	}
	must(d.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO agents (tenant_id, id) VALUES (current_tenant(), 'claude')`); err != nil {
			return err
		}
		grant = dbtest.Grant(t, tx, "quizzer", "claude", 24*time.Hour)
		var err error
		if oldSession, oldSub, err = watch(tx, thread); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `INSERT INTO stream_links (tenant_id, agent_id, target, ack, created_by, grant_id, expires_at)
			VALUES (current_tenant(), 'claude', $1, true, 'app:quizzer', $2, now() + interval '1 hour') RETURNING id`, oldSession, grant).Scan(&link)
	}))
	// The publish's copy here, as fanOut appends it: no delivery yet.
	must(d.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		e, _, err := events.Append(ctx, tx, events.Event{Type: topicsType("quizzer"), Source: "app:quizzer", From: events.Party{Kind: "app", ID: "quizzer"},
			Subject: "held by the first", Trust: events.External, Attrs: map[string]string{"app": "quizzer", "topic": topic, topicAttr(topic): "1"}})
		event = e.ID
		return err
	}))
	// The new session watches the same topic (after the publish), and the
	// hook's redeem supersedes the old one.
	must(d.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		var err error
		if newSession, newSub, err = watch(tx, ""); err != nil {
			return err
		}
		return supersede(ctx, tx, "quizzer", link, newSession, thread)
	}))

	must(d.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		rows, _ := tx.Query(ctx, `SELECT target, subscription_id::text, state FROM deliveries WHERE event_id = $1 ORDER BY target = $2`, event, newSession)
		type delivery struct{ Target, Sub, State string }
		got, err := pgx.CollectRows(rows, pgx.RowToStructByPos[delivery])
		if err != nil {
			return err
		}
		var handed bool
		for _, g := range got {
			switch {
			case g.Target == newSession && g.Sub == newSub && g.State == "pending":
				if handed {
					t.Errorf("handed on twice: %+v", got)
				}
				handed = true
			case g.Target == oldSession && g.Sub == oldSub && g.State == "cancelled":
			default:
				t.Errorf("unexpected delivery %+v (old session %s, new %s)", g, oldSession, newSession)
			}
		}
		if !handed {
			t.Errorf("not handed on to the new session: %+v", got)
		}
		return nil
	}))
}
