package pipeline

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"
)

// The queue drops nothing: a tenant notified while queued is processed
// once; notified while a worker has it, it's processed again after; and no
// two workers have one tenant at once.
func TestQueueDropsNothing(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	q := newQueue()
	q.add("a")
	q.add("b")
	q.add("a") // already queued
	got := []string{}
	for range 2 {
		tn, ok := q.next(ctx)
		if !ok {
			t.Fatal("nothing queued")
		}
		got = append(got, tn)
	}
	slices.Sort(got)
	if !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("got %v", got)
	}
	// Notified again while a worker has it: it waits for that worker.
	q.add("a")
	short, stop := context.WithTimeout(ctx, 100*time.Millisecond)
	defer stop()
	if tn, ok := q.next(short); ok {
		t.Fatalf("handed out %s while a worker has it", tn)
	}
	q.done("b")
	q.done("a")
	if tn, ok := q.next(ctx); !ok || tn != "a" {
		t.Fatalf("again: %q %v", tn, ok)
	}
	short2, stop2 := context.WithTimeout(ctx, 100*time.Millisecond)
	defer stop2()
	if tn, ok := q.next(short2); ok {
		t.Fatalf("processed %s again without a notification", tn)
	}
}

// One runner listens for events at a time; when it stops, another takes
// over and wakes are still prompt (no sweep: Poll is an hour).
func TestLeaderHandsOver(t *testing.T) {
	f := setup(t)
	f.subscribe("codex", "slack.message", nil)
	waitFor := func(n int, within time.Duration) {
		t.Helper()
		deadline := time.Now().Add(within)
		for len(f.owed("codex")) < n {
			if time.Now().After(deadline) {
				t.Fatalf("delivered %d of %d", len(f.owed("codex")), n)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	aCtx, stopA := context.WithCancel(ctx)
	aDone := make(chan struct{})
	go func() { defer close(aDone); (&Runner{DB: f.d, Poll: time.Hour}).Run(aCtx) }()
	time.Sleep(300 * time.Millisecond) // a leads
	bCtx, stopB := context.WithCancel(ctx)
	defer stopB()
	go (&Runner{DB: f.d, Poll: time.Hour}).Run(bCtx)
	time.Sleep(300 * time.Millisecond)

	f.slack("e1", "C1")
	waitFor(1, 2*time.Second)
	stopA()
	<-aDone
	f.slack("e2", "C1")
	waitFor(2, leaderRetry+3*time.Second)
	f.slack("e3", "C1") // b leads now: prompt
	waitFor(3, 2*time.Second)
}

// Many notifications for many tenants, with several workers: every tenant
// is processed.
func TestQueueUnderLoad(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	q := newQueue()
	const tenants, rounds = 50, 20
	seen := make(chan string, tenants*rounds)
	for range 4 {
		go func() {
			for {
				tn, ok := q.next(ctx)
				if !ok {
					return
				}
				seen <- tn
				q.done(tn)
			}
		}()
	}
	for range rounds {
		for i := range tenants {
			q.add(fmt.Sprint("t", i))
		}
	}
	processed := map[string]bool{}
	deadline := time.After(5 * time.Second)
	for len(processed) < tenants {
		select {
		case tn := <-seen:
			processed[tn] = true
		case <-deadline:
			t.Fatalf("processed %d of %d tenants", len(processed), tenants)
		}
	}
}
