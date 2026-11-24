package dblimit_test

import (
	"bytes"
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/wakeshift/server/internal/dblimit"
	"github.com/wakeshift/server/internal/dbtest"
)

func counter(t *testing.T) *dblimit.Counter {
	t.Helper()
	c, err := dblimit.New(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// A limit allows exactly Max in a window, refuses the next with what's left
// of the window, and allows again once the window is over.
func TestLimitAtItsBoundary(t *testing.T) {
	d := dbtest.New(t)
	ctx := context.Background()
	c := counter(t)
	l := dblimit.Limit{Name: "test", Max: 5, Window: time.Hour}
	start := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	for i := range 5 {
		ok, _, err := c.Take(ctx, d.Pool, l, "203.0.113.7", start.Add(time.Duration(i)*time.Minute))
		if err != nil || !ok {
			t.Fatalf("request %d refused: %v", i+1, err)
		}
	}
	if over, _, _ := c.Over(ctx, d.Pool, l, "203.0.113.7", start.Add(5*time.Minute)); !over {
		t.Fatal("Over didn't see the limit used up")
	}
	ok, retry, err := c.Take(ctx, d.Pool, l, "203.0.113.7", start.Add(10*time.Minute))
	if err != nil || ok {
		t.Fatalf("the sixth request was allowed (%v)", err)
	}
	if retry != 50*time.Minute {
		t.Fatalf("retry %v, want the 50 minutes left of the window", retry)
	}
	// Another subject, and another limit's name, count apart.
	if ok, _, _ := c.Take(ctx, d.Pool, l, "203.0.113.8", start.Add(10*time.Minute)); !ok {
		t.Fatal("another address was refused")
	}
	other := dblimit.Limit{Name: "other", Max: 1, Window: time.Hour}
	if ok, _, _ := c.Take(ctx, d.Pool, other, "203.0.113.7", start.Add(10*time.Minute)); !ok {
		t.Fatal("another limit shared the count")
	}
	// A second before the window ends: still refused, retry a second.
	if ok, retry, _ := c.Take(ctx, d.Pool, l, "203.0.113.7", start.Add(time.Hour-time.Second)); ok || retry != time.Second {
		t.Fatalf("allowed %v, retry %v just before the window's end", ok, retry)
	}
	// At its end, a new window.
	if ok, _, _ := c.Take(ctx, d.Pool, l, "203.0.113.7", start.Add(time.Hour)); !ok {
		t.Fatal("refused after the window ended")
	}
	if over, _, _ := c.Over(ctx, d.Pool, l, "203.0.113.7", start.Add(time.Hour)); over {
		t.Fatal("Over still saw the old window")
	}
}

// Over counts nothing; Add counts without a limit.
func TestOverAndAdd(t *testing.T) {
	d := dbtest.New(t)
	ctx := context.Background()
	c := counter(t)
	l := dblimit.Limit{Name: "failures", Max: 3, Window: 10 * time.Minute}
	now := time.Now()
	for range 10 {
		if over, _, err := c.Over(ctx, d.Pool, l, "a", now); err != nil || over {
			t.Fatalf("over %v (%v) with nothing counted", over, err)
		}
	}
	for i := range 3 {
		n, _, err := c.Add(ctx, d.Pool, l, "a", now)
		if err != nil || n != i+1 {
			t.Fatalf("Add: %d (%v), want %d", n, err, i+1)
		}
	}
	if over, retry, _ := c.Over(ctx, d.Pool, l, "a", now); !over || retry != 10*time.Minute {
		t.Fatalf("over %v, retry %v after three", over, retry)
	}
}

// Two instances (two pools on one database) share the counts: exactly Max
// of many concurrent requests get through, wherever they land.
func TestLimitAcrossInstances(t *testing.T) {
	d, owner := dbtest.NewWithURL(t)
	ctx := context.Background()
	second, err := pgxpool.New(ctx, dbtest.RuntimeURL(t, owner))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(second.Close)
	pools := []dblimit.Querier{d.Pool, second}
	c := counter(t)
	l := dblimit.Limit{Name: "test", Max: 3, Window: time.Hour}
	now := time.Now()
	var allowed atomic.Int32
	var wg sync.WaitGroup
	for i := range 40 {
		wg.Go(func() {
			ok, _, err := c.Take(ctx, pools[i%2], l, "link", now)
			if err != nil {
				t.Error(err)
			}
			if ok {
				allowed.Add(1)
			}
		})
	}
	wg.Wait()
	if n := allowed.Load(); n != 3 {
		t.Fatalf("%d allowed across two instances, want 3", n)
	}
	// The second instance sees the first's count.
	if over, _, _ := c.Over(ctx, second, l, "link", now); !over {
		t.Fatal("the second instance didn't see the count")
	}
}

// The table holds neither the subject nor the limit's name, and Prune
// deletes only windows that are over.
func TestStoredHashedAndPruned(t *testing.T) {
	d := dbtest.New(t)
	ctx := context.Background()
	c := counter(t)
	now := time.Now()
	c.Add(ctx, d.Pool, dblimit.Limit{Name: "short", Max: 1, Window: time.Minute}, "198.51.100.23", now)
	c.Add(ctx, d.Pool, dblimit.Limit{Name: "long", Max: 1, Window: time.Hour}, "198.51.100.23", now)
	var leaked bool
	if err := d.Pool.QueryRow(ctx, `SELECT bool_or(position('198.51.100.23'::bytea IN key) > 0 OR position('short'::bytea IN key) > 0) FROM rate_counts`).Scan(&leaked); err != nil {
		t.Fatal(err)
	}
	if leaked {
		t.Fatal("a key holds the subject or the limit's name")
	}
	n, err := dblimit.Prune(ctx, d.Pool, now.Add(2*time.Minute))
	if err != nil || n != 1 {
		t.Fatalf("pruned %d (%v), want the one window that's over", n, err)
	}
	var left int
	d.Pool.QueryRow(ctx, `SELECT count(*) FROM rate_counts`).Scan(&left)
	if left != 1 {
		t.Fatalf("%d rows left, want 1", left)
	}
}

func TestShortKeyRefused(t *testing.T) {
	if _, err := dblimit.New([]byte("short")); err == nil {
		t.Fatal("a short key was accepted")
	}
}
