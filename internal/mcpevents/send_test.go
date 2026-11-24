package mcpevents

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/agents"
	"github.com/wakeshift/server/internal/dbtest"
	"github.com/wakeshift/server/internal/deliveries"
	"github.com/wakeshift/server/internal/events"
	"github.com/wakeshift/server/internal/sessions"
	"github.com/wakeshift/server/internal/subscriptions"
)

// A send stopped part-way (the instance is shutting down) puts what it
// hadn't recorded back to waiting at once, uncounted, the one it was
// sending too: left in flight until their lease ran out, later deliveries
// went before them.
func TestStoppedSendReleasesTheRest(t *testing.T) {
	ctx := context.Background()
	d := dbtest.New(t)
	tenant := dbtest.NewTenant(t, d)
	if _, err := d.Pool.Exec(ctx, `INSERT INTO apps (id, name) VALUES ('chat', 'Chat')`); err != nil {
		t.Fatal(err)
	}
	SetAllowLoopback(true)
	t.Cleanup(func() { SetAllowLoopback(false) })
	arrived, done := make(chan struct{}, 1), make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		arrived <- struct{}{}
		select { // doesn't answer: the instance stops first
		case <-r.Context().Done():
		case <-done:
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(done) })

	seal, err := NewSealer(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	dv := &Deliverer{DB: d, Seal: seal}
	dv.init()
	const hookID = "sub_test"
	c := claimed{hook: hook{tenant: tenant, id: hookID, url: srv.URL, agent: "codex", app: "Chat", event: "test.ping", secrets: [][]byte{[]byte("secret")}}}
	err = d.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		if _, err := agents.Create(ctx, tx, "codex", ""); err != nil {
			return err
		}
		s, err := sessions.Register(ctx, tx, sessions.New{Agent: "codex", CreatedBy: "app:chat"})
		if err != nil {
			return err
		}
		c.session = s.ID
		n := subscriptions.New{AgentID: "codex", EventType: "test.*", CreatedBy: "app:chat", SessionID: s.ID}
		n.GrantID = dbtest.Grant(t, tx, "chat", "codex", time.Hour)
		sb, err := subscriptions.Create(ctx, tx, n)
		if err != nil {
			return err
		}
		if _, err := Subscribe(ctx, tx, seal, Webhook{ID: hookID, App: "chat", Agent: "codex", Session: s.ID, SessionHandle: s.Handle,
			Name: "test.ping", URL: srv.URL, Secret: []byte("secret"), TTL: time.Hour}, time.Now()); err != nil {
			return err
		}
		for _, subject := range []string{"one", "two"} {
			e, _, err := events.Append(ctx, tx, events.Event{Type: "test.ping", Source: "test", Subject: subject, From: events.Party{Kind: "hub", ID: "hub"}, Trust: events.Internal})
			if err != nil {
				return err
			}
			if _, err := deliveries.Insert(ctx, tx, deliveries.New{AgentID: "codex", Target: s.ID, SubscriptionID: sb.ID, Event: e}); err != nil {
				return err
			}
		}
		c.list, err = deliveries.ClaimFor(ctx, tx, "codex", s.ID, "chat", dv.holder, lease, perPass)
		if err == nil && len(c.list) != 2 {
			t.Fatalf("claimed %d", len(c.list))
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	stopping, stop := context.WithCancel(ctx)
	go func() {
		<-arrived
		stop()
	}()
	if dv.send(stopping, c) {
		t.Fatal("a stopped send finished")
	}
	err = d.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		rows, _ := tx.Query(ctx, `SELECT state || ':' || attempts FROM deliveries ORDER BY event_seq`)
		got, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err == nil && (len(got) != 2 || got[0] != "pending:0" || got[1] != "pending:0") {
			t.Errorf("after a stopped send: %v, want both pending:0", got)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}
