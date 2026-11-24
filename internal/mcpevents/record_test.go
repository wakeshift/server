package mcpevents

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/agents"
	"github.com/wakeshift/server/internal/db"
	"github.com/wakeshift/server/internal/dbtest"
	"github.com/wakeshift/server/internal/deliveries"
	"github.com/wakeshift/server/internal/events"
	"github.com/wakeshift/server/internal/grants"
	"github.com/wakeshift/server/internal/sessions"
	"github.com/wakeshift/server/internal/subscriptions"
)

// recording is a webhook with one delivery claimed by a pass, ready for
// the attempt to be recorded.
type recording struct {
	d                        *db.DB
	dv                       *Deliverer
	tenant, grant, sub, hook string
	session, sessionHandle   string
	c                        claimed
	dl                       deliveries.Delivery
}

func newRecording(t *testing.T) recording {
	t.Helper()
	ctx := context.Background()
	r := recording{d: dbtest.New(t), hook: "sub_test"}
	r.tenant = dbtest.NewTenant(t, r.d)
	if _, err := r.d.Pool.Exec(ctx, `INSERT INTO apps (id, name) VALUES ('chat', 'Chat')`); err != nil {
		t.Fatal(err)
	}
	seal, err := NewSealer(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	r.dv = &Deliverer{DB: r.d, Seal: seal}
	r.dv.init()
	err = r.d.InTenant(ctx, r.tenant, func(tx pgx.Tx) error {
		if _, err := agents.Create(ctx, tx, "codex", ""); err != nil {
			return err
		}
		s, err := sessions.Register(ctx, tx, sessions.New{Agent: "codex", CreatedBy: "app:chat"})
		if err != nil {
			return err
		}
		r.session, r.sessionHandle = s.ID, s.Handle
		n := subscriptions.New{AgentID: "codex", EventType: "test.*", CreatedBy: "app:chat", SessionID: s.ID}
		n.GrantID = dbtest.Grant(t, tx, "chat", "codex", time.Hour)
		r.grant = n.GrantID
		sb, err := subscriptions.Create(ctx, tx, n)
		if err != nil {
			return err
		}
		r.sub = sb.ID
		if _, err := Subscribe(ctx, tx, seal, Webhook{ID: r.hook, App: "chat", Agent: "codex", Session: s.ID, SessionHandle: s.Handle,
			Name: "test.ping", URL: "https://example.com/hook", Secret: []byte("secret"), TTL: time.Hour}, time.Now()); err != nil {
			return err
		}
		e, _, err := events.Append(ctx, tx, events.Event{Type: "test.ping", Source: "test", Subject: "one", From: events.Party{Kind: "hub", ID: "hub"}, Trust: events.Internal})
		if err != nil {
			return err
		}
		if _, err := deliveries.Insert(ctx, tx, deliveries.New{AgentID: "codex", Target: s.ID, SubscriptionID: sb.ID, Event: e}); err != nil {
			return err
		}
		list, err := deliveries.ClaimFor(ctx, tx, "codex", s.ID, "chat", r.dv.holder, lease, perPass)
		if err != nil || len(list) != 1 {
			t.Fatalf("claimed %d: %v", len(list), err)
		}
		r.dl = list[0]
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	r.c = claimed{hook: hook{tenant: r.tenant, id: r.hook, agent: "codex", session: r.session}}
	return r
}

// record records the claimed delivery as acknowledged (a 200).
func (r recording) record(ctx context.Context) error {
	return r.d.InTenant(ctx, r.tenant, func(tx pgx.Tx) error {
		return r.dv.record(ctx, tx, r.c, r.dl, acked, false, "", "", 200)
	})
}

// Recording an attempt and unsubscribing at the same moment both finish:
// an unsubscribe removes the webhook and then cancels its subscription's
// deliveries, so recording takes the webhook before the delivery too (as
// claiming does). Taken the other way round, the two deadlocked and one
// failed (the unsubscribe answered 500).
func TestRecordAndUnsubscribeDontDeadlock(t *testing.T) {
	ctx := context.Background()
	r := newRecording(t)
	d := r.d

	// The unsubscribe: the webhook goes first and its transaction stays
	// open while the attempt is recorded alongside.
	recorded := make(chan error, 1)
	err := d.InTenant(ctx, r.tenant, func(tx pgx.Tx) error {
		if _, err := Unsubscribe(ctx, tx, r.hook); err != nil {
			return err
		}
		go func() { recorded <- r.record(ctx) }()
		// Recording is waiting on the webhook the unsubscribe holds.
		deadline := time.Now().Add(10 * time.Second)
		for {
			var waiting int
			if err := d.Pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock'`).
				Scan(&waiting); err != nil {
				return err
			}
			if waiting > 0 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("recording never waited on the unsubscribe")
			}
			time.Sleep(10 * time.Millisecond)
		}
		_, err := subscriptions.Cancel(ctx, tx, r.sub)
		return err
	})
	if err != nil {
		t.Errorf("unsubscribe: %v", err)
	}
	if err := <-recorded; err != nil {
		t.Errorf("record: %v", err)
	}
}

// Revoking the app's grant while a webhook delivery is being recorded:
// both finish. Revoking cancels the grant's subscriptions (their deliveries)
// and then ends the agent's webhooks; recording locks the webhook, then the
// delivery. Revoking now locks the webhooks before it cancels anything, so
// the recording waits for it. Before, recording held the webhook and waited
// for the delivery, revoking held the delivery and waited for the webhook,
// and Postgres failed one of them (40P01): the webhook's pass stopped early
// ("recording a delivery failed") and sent the delivery again, or the
// revocation answered 500.

// hold makes one kind of statement wait mid-transaction, as a deadlock's
// first side would be when the second arrives: a statement trigger on
// table that, for a statement whose text contains needle, waits on an
// advisory lock the test holds until release.
type hold struct {
	t        *testing.T
	owner    *pgx.Conn
	released bool
}

func newHold(t *testing.T, d *db.DB, table, timing, needle string) *hold {
	t.Helper()
	ctx := context.Background()
	h := &hold{t: t, owner: dbtest.Owner(t, d)}
	for _, q := range []string{
		`CREATE FUNCTION zz_hold() RETURNS trigger LANGUAGE plpgsql AS $$
			BEGIN
				IF position('` + needle + `' in current_query()) > 0 THEN PERFORM pg_advisory_xact_lock_shared(4244); END IF;
				RETURN NULL;
			END $$`,
		`CREATE TRIGGER zz_hold ` + timing + ` UPDATE ON ` + table + ` FOR EACH STATEMENT EXECUTE FUNCTION zz_hold()`,
		`SELECT pg_advisory_lock(4244)`,
	} {
		if _, err := h.owner.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(h.release)
	return h
}

func (h *hold) release() {
	if !h.released {
		h.released = true
		h.owner.Exec(context.Background(), `SELECT pg_advisory_unlock(4244)`)
	}
}

// blocked waits until a connection to the test's database waits for a
// lock of the kind given (advisory, or not).
func (h *hold) blocked(what string, advisory bool) {
	h.t.Helper()
	for deadline := time.Now().Add(15 * time.Second); ; {
		var n int
		if err := h.owner.QueryRow(context.Background(), `SELECT count(*) FROM pg_locks l JOIN pg_stat_activity a ON a.pid = l.pid
			WHERE NOT l.granted AND a.datname = current_database() AND (l.locktype = 'advisory') = $1`, advisory).Scan(&n); err != nil {
			h.t.Fatal(err)
		}
		if n > 0 {
			return
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("%s never waited", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// race runs first until the hold stops it, then the recording until it
// waits on a lock, then lets both go: both must finish.
func (r recording) race(t *testing.T, h *hold, what string, first func(tx pgx.Tx) error) {
	t.Helper()
	ctx := context.Background()
	done := make(chan error, 1)
	go func() { done <- r.d.InTenant(ctx, r.tenant, first) }()
	h.blocked(what, true)
	recorded := make(chan error, 1)
	go func() { recorded <- r.record(ctx) }()
	h.blocked("the recording", false)
	h.release()
	if err := <-done; err != nil {
		t.Errorf("%s: %v", what, err)
	}
	if err := <-recorded; err != nil {
		t.Errorf("recording: %v", err)
	}
}

// Revoking the app's grant while a webhook delivery is being recorded:
// both finish. Revoking cancels the grant's subscriptions (their deliveries)
// and ends the agent's webhooks; recording locks the webhook, then the
// delivery. Revoking now locks the webhooks before it cancels anything, so
// the recording waits for it. Before, revoking ended the webhooks after
// canceling their deliveries: recording held the webhook and waited for
// the delivery, revoking held the delivery and waited for the webhook, and
// Postgres failed one of them (40P01): the webhook's pass stopped early
// ("recording a delivery failed") and sent the delivery again, or the
// revocation failed.
func TestRecordAndRevokeDontDeadlock(t *testing.T) {
	ctx := context.Background()
	r := newRecording(t)
	if err := r.d.InTenant(ctx, r.tenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO event_subscriptions (tenant_id, source_kind, source_id, event, arguments, arguments_key,
			destination, agent_id, session_id, webhook_url, webhook_id, subscription_id, created_by)
			VALUES (current_tenant(), 'app', 'chat', 'test.ping', '{}', 'k', 'webhook', 'codex', $1, 'https://example.com/hook', $2, $3, 'app:chat')`,
			r.session, r.hook, r.sub)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// Held just before it ends the webhook (endEventSubs' statement), after
	// canceling the grant's subscription and its delivery.
	h := newHold(t, r.d, "mcp_event_webhooks", "BEFORE", "Access revoked")
	r.race(t, h, "revoking", func(tx pgx.Tx) error {
		_, err := grants.Revoke(ctx, tx, r.grant, "test")
		return err
	})
	var state, ending string
	if err := r.d.InTenant(ctx, r.tenant, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT state FROM deliveries WHERE id = $1`, r.dl.ID).Scan(&state); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT coalesce(ending_reason, '') FROM mcp_event_webhooks WHERE id = $1`, r.hook).Scan(&ending)
	}); err != nil {
		t.Fatal(err)
	}
	if state != "cancelled" || ending != "Access revoked" {
		t.Errorf("delivery %s, webhook ending %q; want cancelled, Access revoked", state, ending)
	}
}
