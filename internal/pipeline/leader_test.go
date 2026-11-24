package pipeline

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/dbtest"
)

// A runner that doesn't lead asks for the lock again on the connection it
// holds, not a new one each time: across several asks its session (pid and
// start) stays the same.
func TestFollowerKeepsItsConnection(t *testing.T) {
	d := dbtest.New(t)
	aCtx, stopA := context.WithCancel(ctx)
	defer stopA()
	go (&Runner{DB: d, Poll: time.Hour}).Run(aCtx)
	time.Sleep(300 * time.Millisecond) // a leads
	bCtx, stopB := context.WithCancel(ctx)
	defer stopB()
	go (&Runner{DB: d, Poll: time.Hour}).Run(bCtx)

	// The sessions that could be the follower's: their last statement was
	// the lock ask and they hold no advisory lock. The follower's is there
	// at every look, 4 s apart; a connection made for each ask lasts well
	// under a second.
	followers := func() map[string]bool {
		t.Helper()
		rows, _ := d.Pool.Query(ctx, `SELECT a.pid::text || ' ' || a.backend_start::text FROM pg_stat_activity a
			WHERE a.datname = current_database() AND a.pid <> pg_backend_pid() AND a.query LIKE '%pg_try_advisory_lock%'
			AND NOT EXISTS (SELECT 1 FROM pg_locks l WHERE l.pid = a.pid AND l.locktype = 'advisory')`)
		ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatal(err)
		}
		set := map[string]bool{}
		for _, id := range ids {
			set[id] = true
		}
		return set
	}
	time.Sleep(time.Second)
	kept := followers()
	for range 2 {
		time.Sleep(leaderRetry + time.Second) // another ask
		now := followers()
		for id := range kept {
			if !now[id] {
				delete(kept, id)
			}
		}
	}
	if len(kept) == 0 {
		t.Fatal("the follower didn't keep one session across its asks")
	}
}

// What a follower's ask for the lock costs: a new connection for each ask
// (as before), against the one it holds. Against the local Postgres
// (SCRAM over TCP), not counting the Cloud SQL connector's TLS handshake
// or the backend Postgres starts for each connection.
func BenchmarkLeaderAsk(b *testing.B) {
	d := dbtest.New(b)
	held, err := d.Conn(ctx)
	if err != nil {
		b.Fatal(err)
	}
	defer held.Close(ctx)
	if led, err := tryLead(ctx, held); err != nil || !led {
		b.Fatalf("taking the lock: %v %v", led, err)
	}
	b.Run("connection each ask", func(b *testing.B) {
		for b.Loop() {
			conn, err := d.Conn(ctx)
			if err != nil {
				b.Fatal(err)
			}
			if led, err := tryLead(ctx, conn); err != nil || led {
				b.Fatalf("ask: %v %v", led, err)
			}
			conn.Close(ctx)
		}
	})
	b.Run("held connection", func(b *testing.B) {
		conn, err := d.Conn(ctx)
		if err != nil {
			b.Fatal(err)
		}
		defer conn.Close(ctx)
		for b.Loop() {
			if led, err := tryLead(ctx, conn); err != nil || led {
				b.Fatalf("ask: %v %v", led, err)
			}
		}
	})
}
