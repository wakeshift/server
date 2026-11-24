package streams_test

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/wakeshift/server/internal/db"
	"github.com/wakeshift/server/internal/streams"
)

// A long poll keeps its session connected while it waits: its periodic
// check renews the stream's presence in the same transaction (there's no
// separate renewal any more), and stops once the poll ends.
func TestPollRenewsPresenceInItsCheck(t *testing.T) {
	f := setup(t)
	f.s.Renew = 300 * time.Millisecond
	f.s.Poll = 10 * time.Second // the tick is the sooner of the two
	f.s.WaitGrace = 100 * time.Millisecond
	base := f.waitServer()
	link, id := f.sessionLinkFor("cursor", streams.SessionLinkTTL)
	done := make(chan pollBody, 1)
	go func() { done <- mustPoll(t, base, token(link), "wait=3s") }()
	waitFor(t, 2*time.Second, "not connected while polling", func() bool { return f.connected(id) })
	// As if its last renewal were long ago: the next check renews it.
	f.tx(func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE stream_leases SET renewed_at = now() - interval '2 minutes' WHERE target = $1`, id)
		return err
	})
	if f.connected(id) {
		t.Fatal("still connected after its presence was set back")
	}
	waitFor(t, 2*time.Second, "the poll's check didn't renew its presence", func() bool { return f.connected(id) })
	select {
	case b := <-done:
		if len(b.Deliveries) != 0 {
			t.Fatalf("an idle poll returned %+v", b)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the poll didn't end")
	}
}

// An idle poll runs one transaction a tick (its check, which renews its
// presence too), not a check and a renewal each in a transaction of its
// own: with Poll and Renew both 30 s, as in production, that was two
// transactions every 30 s per waiting conversation (the scalability
// review's finding 5).
func TestIdlePollOneTransactionATick(t *testing.T) {
	f := setup(t)
	// The server's transactions, counted on a pool of its own.
	var begins atomic.Int64
	cfg := f.d.Pool.Config()
	cfg.ConnConfig.Tracer = beginCounter{&begins}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	f.s.DB = &db.DB{Pool: pool}
	f.s.Renew = 200 * time.Millisecond
	f.s.Poll = 200 * time.Millisecond
	f.s.WaitGrace = 100 * time.Millisecond
	base := f.waitServer()
	link, id := f.sessionLinkFor("cursor", streams.SessionLinkTTL)
	done := make(chan pollBody, 1)
	go func() { done <- mustPoll(t, base, token(link), "wait=4s") }()
	waitFor(t, 2*time.Second, "not connected while polling", func() bool { return f.connected(id) })
	from := begins.Load()
	time.Sleep(2 * time.Second)
	// 10 ticks: 10 transactions (with the renewal in a transaction of its
	// own, 30 here).
	if n := begins.Load() - from; n < 5 || n > 14 {
		t.Errorf("%d transactions in 10 ticks of an idle poll, want about 10", n)
	}
	select {
	case b := <-done:
		if len(b.Deliveries) != 0 {
			t.Fatalf("an idle poll returned %+v", b)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the poll didn't end")
	}
}

// beginCounter counts the transactions begun on a pool's connections.
type beginCounter struct{ n *atomic.Int64 }

func (c beginCounter) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(data.SQL)), "begin") {
		c.n.Add(1)
	}
	return ctx
}

func (beginCounter) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}
