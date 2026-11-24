package mcpevents

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/agents"
	"github.com/wakeshift/server/internal/db"
	"github.com/wakeshift/server/internal/dbtest"
	"github.com/wakeshift/server/internal/deliveries"
	"github.com/wakeshift/server/internal/events"
	"github.com/wakeshift/server/internal/sessions"
	"github.com/wakeshift/server/internal/subscriptions"
)

// fixture makes webhooks for tests: each in a session of its own, with a
// subscription under the app's grant for the agent.
type fixture struct {
	t     *testing.T
	d     *db.DB
	seal  *Sealer
	n     int
	grant map[string]string // tenant/app → grant ID
	agent map[string]bool   // tenants with the agent made
}

func newFixture(t *testing.T, d *db.DB, apps ...string) *fixture {
	ctx := context.Background()
	for _, a := range apps {
		if _, err := d.Pool.Exec(ctx, `INSERT INTO apps (id, name) VALUES ($1, $1)`, a); err != nil {
			t.Fatal(err)
		}
	}
	seal, err := NewSealer(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{t: t, d: d, seal: seal, grant: map[string]string{}, agent: map[string]bool{}}
}

// hook subscribes a new webhook for app's agent "codex" in tenant, to url,
// with one delivery waiting if pending. It returns the webhook's ID, or
// Subscribe's error.
func (f *fixture) hook(tenant, app, url string, pending bool) (string, error) {
	f.t.Helper()
	ctx := context.Background()
	f.n++
	id := fmt.Sprintf("sub_%s_%d", app, f.n)
	err := f.d.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		if !f.agent[tenant] {
			if _, err := agents.Create(ctx, tx, "codex", ""); err != nil {
				return err
			}
			f.agent[tenant] = true
		}
		g := f.grant[tenant+"/"+app]
		if g == "" {
			g = dbtest.Grant(f.t, tx, app, "codex", time.Hour)
			f.grant[tenant+"/"+app] = g
		}
		s, err := sessions.Register(ctx, tx, sessions.New{Agent: "codex", CreatedBy: "app:" + app})
		if err != nil {
			return err
		}
		sb, err := subscriptions.Create(ctx, tx, subscriptions.New{AgentID: "codex", EventType: "test.*", CreatedBy: "app:" + app, SessionID: s.ID, GrantID: g})
		if err != nil {
			return err
		}
		if _, err := Subscribe(ctx, tx, f.seal, Webhook{ID: id, App: app, Agent: "codex", Session: s.ID, SessionHandle: s.Handle,
			Name: "test.ping", URL: url, Secret: []byte("secret"), TTL: time.Hour}, time.Now()); err != nil {
			return err
		}
		if !pending {
			return nil
		}
		e, _, err := events.Append(ctx, tx, events.Event{Type: "test.ping", Source: "test", Subject: "one", From: events.Party{Kind: "hub", ID: "hub"}, Trust: events.Internal})
		if err != nil {
			return err
		}
		_, err = deliveries.Insert(ctx, tx, deliveries.New{AgentID: "codex", Target: s.ID, SubscriptionID: sb.ID, Event: e})
		return err
	})
	return id, err
}

// An app's user has at most MaxWebhooks live webhooks: one more is
// refused, a refresh of a live one isn't, and once one ends there's room
// again. Another user (workspace), or another app, has its own.
func TestWebhookCap(t *testing.T) {
	ctx := context.Background()
	d := dbtest.New(t)
	tenant, other := dbtest.NewTenant(t, d), dbtest.NewTenant(t, d)
	f := newFixture(t, d, "chat", "games")
	var ids []string
	for range MaxWebhooks {
		id, err := f.hook(tenant, "chat", "https://cb.example/hook", false)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if _, err := f.hook(tenant, "chat", "https://cb.example/hook", false); !errors.Is(err, ErrTooManyWebhooks) {
		t.Fatalf("webhook %d: %v, want ErrTooManyWebhooks", MaxWebhooks+1, err)
	}
	if !strings.Contains(ErrTooManyWebhooks.Error(), fmt.Sprint(MaxWebhooks)) {
		t.Errorf("the error doesn't say the limit: %v", ErrTooManyWebhooks)
	}
	if _, err := f.hook(other, "chat", "https://cb.example/hook", false); err != nil {
		t.Fatalf("another user's: %v", err)
	}
	if _, err := f.hook(tenant, "games", "https://cb.example/hook", false); err != nil {
		t.Fatalf("another app's: %v", err)
	}

	// A refresh of one that's live goes through at the limit.
	err := d.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		var session, handle string
		if err := tx.QueryRow(ctx, `SELECT session_id::text, session_handle FROM mcp_event_webhooks WHERE id = $1`, ids[0]).Scan(&session, &handle); err != nil {
			return err
		}
		res, err := Subscribe(ctx, tx, f.seal, Webhook{ID: ids[0], App: "chat", Agent: "codex", Session: session, SessionHandle: handle,
			Name: "test.ping", URL: "https://cb.example/hook", Secret: []byte("secret"), TTL: time.Hour}, time.Now())
		if err == nil && res.Created {
			t.Error("a refresh made a new webhook")
		}
		return err
	})
	if err != nil {
		t.Fatalf("refresh at the limit: %v", err)
	}

	// Ended (and expired) ones don't count.
	err = d.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		if _, err := Unsubscribe(ctx, tx, ids[1]); err != nil {
			return err
		}
		if err := Terminate(ctx, tx, ids[2], "Access revoked"); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE mcp_event_webhooks SET expires_at = now() - interval '1 second' WHERE id = $1`, ids[3])
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		if _, err := f.hook(tenant, "chat", "https://cb.example/hook", false); err != nil {
			t.Fatalf("after one ended (%d): %v", i, err)
		}
	}
	if _, err := f.hook(tenant, "chat", "https://cb.example/hook", false); !errors.Is(err, ErrTooManyWebhooks) {
		t.Fatalf("at the limit again: %v", err)
	}
}

// Slots: an app has at most appSends of them, a workspace workspaceSends,
// and all of them sendSlots; a wake that found no room is handed back
// when a send that was in its way ends.
func TestShares(t *testing.T) {
	s := newShares()
	w := func(tenant string) wake { return wake{tenant: tenant, agent: "a"} }
	for i := range workspaceSends {
		if !s.take("t1", "slow", w("t1")) {
			t.Fatalf("slot %d refused", i)
		}
	}
	if s.take("t1", "slow", w("t1")) {
		t.Fatal("a workspace took more than its share")
	}
	for i := workspaceSends; i < appSends; i++ {
		if !s.take(fmt.Sprintf("t%d", i), "slow", w("x")) {
			t.Fatalf("app slot %d refused", i)
		}
	}
	if s.take("t99", "slow", w("t99")) {
		t.Fatal("an app took more than its share")
	}
	if !s.take("t100", "fast", w("t100")) {
		t.Fatal("another app found no slot")
	}
	// A slot given back in the workspace hands back what waited on it and
	// on the app, not the other app's.
	got := s.give("t1", "slow")
	if len(got) != 2 {
		t.Fatalf("handed back %v, want t1's and t99's", got)
	}
	if again := s.give("t100", "fast"); len(again) != 0 {
		t.Fatalf("handed back %v", again)
	}

	// Every slot taken, by apps within their shares: the next waits for any.
	s = newShares()
	for i := range sendSlots {
		if !s.take(fmt.Sprintf("t%d", i), fmt.Sprintf("app%d", i/appSends), w("x")) {
			t.Fatalf("slot %d refused", i)
		}
	}
	if s.take("new", "new", w("new")) {
		t.Fatal("more than sendSlots")
	}
	if got := s.give("t0", "app0"); len(got) != 1 || got[0].tenant != "new" {
		t.Fatalf("handed back %v", got)
	}
}

// One app's slow callbacks (each holding its send until the test lets it
// go) take at most appSends of an instance's slots, and at most
// workspaceSends for one of its users: another app's delivery goes at once
// meanwhile. Once the slow ones answer, the slow app's webhooks that found
// no slot are sent too, without waiting for a pass.
func TestSlowAppDoesntHoldUpAnother(t *testing.T) {
	ctx := context.Background()
	d := dbtest.New(t)
	AllowLoopback = true
	t.Cleanup(func() { AllowLoopback = false })

	var (
		mu       sync.Mutex
		inFlight = map[string]int{} // by path: /slow/<tenant>
		most     = map[string]int{}
		slowNow  int
		slowMost int
		slowDone int
	)
	release := make(chan struct{})
	fast := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/fast" {
			fast <- struct{}{}
			return
		}
		mu.Lock()
		inFlight[r.URL.Path]++
		most[r.URL.Path] = max(most[r.URL.Path], inFlight[r.URL.Path])
		slowNow++
		slowMost = max(slowMost, slowNow)
		mu.Unlock()
		select {
		case <-release:
		case <-r.Context().Done():
		}
		mu.Lock()
		inFlight[r.URL.Path]--
		slowNow--
		slowDone++
		mu.Unlock()
	}))
	t.Cleanup(srv.Close)
	stopped := false
	t.Cleanup(func() {
		if !stopped {
			close(release)
		}
	})

	// More slow webhooks than an instance has slots, in three of the slow
	// app's workspaces.
	f := newFixture(t, d, "slow", "fast")
	const perWorkspace = 12
	slowTenants := []string{dbtest.NewTenant(t, d), dbtest.NewTenant(t, d), dbtest.NewTenant(t, d)}
	for _, tn := range slowTenants {
		for range perWorkspace {
			if _, err := f.hook(tn, "slow", srv.URL+"/slow/"+tn, true); err != nil {
				t.Fatal(err)
			}
		}
	}
	total := perWorkspace * len(slowTenants)
	if total <= sendSlots {
		t.Fatalf("%d slow webhooks: the test needs more than the %d slots", total, sendSlots)
	}
	fastTenant := dbtest.NewTenant(t, d)
	if _, err := f.hook(fastTenant, "fast", srv.URL+"/fast", false); err != nil {
		t.Fatal(err)
	}

	runCtx, stop := context.WithCancel(ctx)
	dv := &Deliverer{DB: d, Seal: f.seal, Poll: time.Hour} // one pass, at the start
	ran := make(chan struct{})
	go func() {
		dv.Run(runCtx)
		close(ran)
	}()
	t.Cleanup(func() {
		stop()
		<-ran
	})

	// The pass sends the slow app its share and no more.
	waitFor(t, "the slow app's share in flight", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return slowNow == appSends
	})

	// The fast app's delivery, made now, goes at once.
	err := d.InTenant(ctx, fastTenant, func(tx pgx.Tx) error {
		var session, sub string
		if err := tx.QueryRow(ctx, `SELECT w.session_id::text, s.id::text FROM mcp_event_webhooks w JOIN subscriptions s ON s.session_id = w.session_id`).
			Scan(&session, &sub); err != nil {
			return err
		}
		e, _, err := events.Append(ctx, tx, events.Event{Type: "test.ping", Source: "test", Subject: "fast", From: events.Party{Kind: "hub", ID: "hub"}, Trust: events.Internal})
		if err != nil {
			return err
		}
		_, err = deliveries.Insert(ctx, tx, deliveries.New{AgentID: "codex", Target: session, SubscriptionID: sub, Event: e})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	dv.Wake(fastTenant, "codex")
	select {
	case <-fast:
	case <-time.After(5 * time.Second):
		t.Fatal("the fast app's delivery waited on the slow app's callbacks")
	}

	// The slow ones answer: the rest of the slow app's are sent, by the
	// wakes their slots hand back (the next pass is an hour away).
	close(release)
	stopped = true
	waitFor(t, "every slow delivery", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return slowDone == total
	})
	mu.Lock()
	defer mu.Unlock()
	if slowMost > appSends {
		t.Errorf("the slow app had %d sends at once, more than %d", slowMost, appSends)
	}
	for path, n := range most {
		if n > workspaceSends {
			t.Errorf("%s had %d sends at once, more than %d", path, n, workspaceSends)
		}
	}
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
