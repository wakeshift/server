package mcpevents

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/wakeshift/server/internal/dbtest"
)

// A Wake for an agent in a workspace with no webhook is never handed to a
// claim (no claim transaction); one in a workspace with a webhook is.
func TestWakeSkipsWorkspacesWithoutWebhooks(t *testing.T) {
	ctx := context.Background()
	d := dbtest.New(t)
	with, without := dbtest.NewTenant(t, d), dbtest.NewTenant(t, d)
	f := newFixture(t, d, "chat")
	if _, err := f.hook(with, "chat", "https://cb.example/hook", false); err != nil {
		t.Fatal(err)
	}
	got, err := (&Deliverer{DB: d}).hasTargets(ctx, []string{with, without})
	if err != nil {
		t.Fatal(err)
	}
	if !got[with] || got[without] || len(got) != 1 {
		t.Fatalf("workspaces with webhooks: %v; want only %s", got, with)
	}

	dv := &Deliverer{DB: d, Seal: f.seal}
	dv.Wake(without, "codex")
	dv.Wake(with, "codex")
	var claimed []string
	dv.gate.Check(ctx, dv.hasTargets, func(tenant, agent string) { claimed = append(claimed, tenant+"/"+agent) })
	if len(claimed) != 1 || claimed[0] != with+"/codex" {
		t.Fatalf("handed to a claim: %v; want only %s/codex", claimed, with)
	}
}

// A webhook subscribed after the deliverer started (as on another
// instance) is still sent its delivery at once on a Wake, not on the next
// pass (an hour away here): the gate reads the targets after the wake.
func TestWakeSendsToANewWebhook(t *testing.T) {
	ctx := context.Background()
	d := dbtest.New(t)
	AllowLoopback = true
	t.Cleanup(func() { AllowLoopback = false })
	hits := make(chan string, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits <- r.URL.Path }))
	t.Cleanup(srv.Close)
	f := newFixture(t, d, "chat")
	probe, tenant := dbtest.NewTenant(t, d), dbtest.NewTenant(t, d)
	if _, err := f.hook(probe, "chat", srv.URL+"/probe", true); err != nil {
		t.Fatal(err)
	}

	runCtx, stop := context.WithCancel(ctx)
	dv := &Deliverer{DB: d, Seal: f.seal, Poll: time.Hour}
	ran := make(chan struct{})
	go func() {
		dv.Run(runCtx)
		close(ran)
	}()
	t.Cleanup(func() {
		stop()
		<-ran
	})
	// The start's pass sent the probe's: it's over (or nearly), and the
	// next is an hour away.
	select {
	case p := <-hits:
		if p != "/probe" {
			t.Fatalf("got %s first", p)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the pass sent nothing")
	}
	waitFor(t, "the start's pass to end", func() bool {
		got, err := dv.DB.Exclusive(ctx, passLock, func() {})
		return err == nil && got
	})

	if _, err := f.hook(tenant, "chat", srv.URL+"/new", true); err != nil {
		t.Fatal(err)
	}
	dv.Wake(tenant, "codex")
	select {
	case p := <-hits:
		if p != "/new" {
			t.Fatalf("got %s, want the new webhook's", p)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the new webhook's delivery waited for a pass")
	}
}
