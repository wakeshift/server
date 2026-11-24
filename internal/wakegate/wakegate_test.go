package wakegate

import (
	"context"
	"errors"
	"slices"
	"testing"
)

type passed struct{ tenant, agent string }

// Only wakes for workspaces with a destination are passed on, each once
// per batch, and the read is made once for the batch.
func TestGatePassesOnlyWorkspacesWithDestinations(t *testing.T) {
	g := New()
	g.Add("with", "a")
	g.Add("without", "b")
	g.Add("with", "c")
	g.Add("with", "a") // again, before the check: once
	reads := 0
	var asked []string
	has := func(_ context.Context, tenants []string) (map[string]bool, error) {
		reads++
		asked = tenants
		return map[string]bool{"with": true}, nil
	}
	var got []passed
	g.Check(context.Background(), has, func(tenant, agent string) { got = append(got, passed{tenant, agent}) })
	if reads != 1 || !slices.Equal(asked, []string{"with", "without"}) {
		t.Fatalf("%d reads, asked %v", reads, asked)
	}
	if want := []passed{{"with", "a"}, {"with", "c"}}; !slices.Equal(got, want) {
		t.Fatalf("passed %v, want %v", got, want)
	}
	// Nothing left: the next check reads nothing.
	g.Check(context.Background(), has, func(string, string) { t.Fatal("passed a wake twice") })
	if reads != 1 {
		t.Fatalf("an empty batch read: %d reads", reads)
	}
}

// A read that fails passes the whole batch on: the claim decides, as it
// did before the gate.
func TestGateFailsOpen(t *testing.T) {
	g := New()
	g.Add("t1", "a")
	g.Add("t2", "b")
	var got []passed
	g.Check(context.Background(), func(context.Context, []string) (map[string]bool, error) {
		return nil, errors.New("connection reset")
	}, func(tenant, agent string) { got = append(got, passed{tenant, agent}) })
	if len(got) != 2 {
		t.Fatalf("passed %v, want both", got)
	}
}

// A wake added while a read runs isn't decided by that read (it may have
// started before the destination was committed): it waits for the next.
func TestGateWakeDuringReadWaitsForTheNext(t *testing.T) {
	g := New()
	g.Add("t1", "a")
	var got []passed
	pass := func(tenant, agent string) { got = append(got, passed{tenant, agent}) }
	first := true
	has := func(_ context.Context, tenants []string) (map[string]bool, error) {
		if first {
			first = false
			g.Add("t2", "b") // arrives mid-read; t2's destination is new
			return map[string]bool{"t1": true}, nil
		}
		return map[string]bool{"t2": true}, nil
	}
	g.Check(context.Background(), has, pass)
	if !slices.Equal(got, []passed{{"t1", "a"}}) {
		t.Fatalf("first read passed %v", got)
	}
	g.Check(context.Background(), has, pass)
	if !slices.Equal(got, []passed{{"t1", "a"}, {"t2", "b"}}) {
		t.Fatalf("the wake that came mid-read was lost: %v", got)
	}
}

// Run passes wakes on as they come, until its context ends.
func TestGateRun(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	g := New()
	out := make(chan passed, 1)
	done := make(chan struct{})
	go func() {
		g.Run(ctx, func(context.Context, []string) (map[string]bool, error) { return map[string]bool{"t": true}, nil },
			func(tenant, agent string) { out <- passed{tenant, agent} })
		close(done)
	}()
	g.Add("t", "a")
	if p := <-out; p != (passed{"t", "a"}) {
		t.Fatalf("passed %v", p)
	}
	cancel()
	<-done
}
