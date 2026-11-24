package db_test

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/wakeshift/server/internal/db"
	"github.com/wakeshift/server/internal/dbtest"
)

// stall is a connection path that can stop carrying bytes, both ways,
// while both ends stay open: what the Cloud SQL connector's tunnel did on
// 2026-11-12, when a transaction's connection went quiet for 67 s and its
// locks stayed held. Neither end sees an error; bytes just stop.
type stall struct {
	mu      sync.Mutex
	stalled bool
	resumed chan struct{}
}

func newStall(t *testing.T) *stall {
	s := &stall{resumed: make(chan struct{})}
	t.Cleanup(s.resume) // so the pumps finish
	return s
}

func (s *stall) stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.stalled {
		s.stalled, s.resumed = true, make(chan struct{})
	}
}

func (s *stall) resume() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stalled {
		s.stalled = false
		close(s.resumed)
	}
}

// wait returns once bytes may flow.
func (s *stall) wait() {
	s.mu.Lock()
	stalled, resumed := s.stalled, s.resumed
	s.mu.Unlock()
	if stalled {
		<-resumed
	}
}

// dial connects through s: the driver gets one end of a pipe, and two
// pumps carry bytes between its other end and Postgres while s lets them.
// Even a close (Postgres ending the session) waits for s, as through a
// stalled tunnel.
func (s *stall) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	server, err := (&net.Dialer{}).DialContext(ctx, network, addr)
	if err != nil {
		return nil, err
	}
	client, inner := net.Pipe()
	pump := func(dst, src net.Conn) {
		buf := make([]byte, 32<<10)
		for {
			n, err := src.Read(buf)
			s.wait()
			if n > 0 {
				if _, werr := dst.Write(buf[:n]); werr != nil {
					break
				}
			}
			if err != nil {
				break
			}
		}
		dst.Close()
		src.Close()
	}
	go pump(server, inner)
	go pump(inner, server)
	return client, nil
}

// stallablePool is a pool with the server's settings (db.Configure) whose
// one connection runs through s, as d's role.
func stallablePool(t *testing.T, ownerURL string, s *stall, params map[string]string) *db.DB {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(dbtest.RuntimeURL(t, ownerURL))
	if err != nil {
		t.Fatal(err)
	}
	db.Configure(cfg)
	for k, v := range params {
		cfg.ConnConfig.RuntimeParams[k] = v
	}
	cfg.MaxConns = 1
	cfg.ConnConfig.DialFunc = s.dial
	cfg.ConnConfig.TLSConfig = nil // the test database's; nothing to verify on a pipe
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	return db.FromPool(pool)
}

// eventually polls f until it holds, failing the test after within.
func eventually(t *testing.T, within time.Duration, what string, f func() bool) time.Duration {
	t.Helper()
	start := time.Now()
	for !f() {
		if time.Since(start) > within {
			t.Fatalf("%s: not within %v", what, within)
		}
		time.Sleep(50 * time.Millisecond)
	}
	return time.Since(start)
}

// A transaction whose connection stalls mid-way, holding an advisory lock
// (the subscribe's topic-members) and a row's lock, has its session ended
// by Postgres once idle past the limit (db.IdleInTransactionTimeout,
// shortened here): its locks go, so the transactions queued behind it run,
// rather than waiting for the stalled connection to come back. When it
// does, the stalled transaction fails (it rolled back) and the pool
// replaces the connection.
func TestStalledTransactionReleasesItsLocks(t *testing.T) {
	d, owner := dbtest.NewWithURL(t)
	ctx := context.Background()
	s := newStall(t)
	stalled := stallablePool(t, owner, s, map[string]string{"idle_in_transaction_session_timeout": "500"})
	tenant := dbtest.NewTenant(t, d)

	const lock = "topic-members:stall-test"
	held := make(chan struct{})
	done := make(chan error, 1)
	tctx, cancel := context.WithCancel(ctx)
	finished := make(chan struct{})
	// Before the stall's own cleanup (resume): on a failure, the stalled
	// transaction ends, and is waited for, before the test does.
	t.Cleanup(func() { cancel(); <-finished })
	go func() {
		defer close(finished)
		done <- stalled.InTenant(tctx, tenant, func(tx pgx.Tx) error {
			if _, err := tx.Exec(tctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, lock); err != nil {
				return err
			}
			if _, err := tx.Exec(tctx, `SELECT 1 FROM tenants WHERE id = $1 FOR UPDATE`, tenant); err != nil {
				return err
			}
			s.stop() // the next statement goes nowhere, for now
			close(held)
			_, err := tx.Exec(tctx, `SELECT 1`)
			return err
		})
	}()
	select {
	case <-held:
	case err := <-done:
		t.Fatalf("the transaction to stall: %v", err)
	}

	// Queued behind it, as subscribes and manifest merges were.
	took := eventually(t, 10*time.Second, "the stalled transaction's locks freed", func() bool {
		err := d.InTenant(ctx, tenant, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout = 200`); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, lock); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `SELECT 1 FROM tenants WHERE id = $1 FOR UPDATE`, tenant)
			return err
		})
		return err == nil
	})
	t.Logf("locks freed after %v", took)

	select {
	case err := <-done:
		t.Fatalf("the stalled transaction ended while stalled: %v", err)
	default:
	}
	s.resume()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("the stalled transaction committed after its session was ended")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the stalled transaction didn't end once its connection resumed")
	}
	// The pool's one connection is replaced.
	if err := stalled.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, lock)
		return err
	}); err != nil {
		t.Fatalf("the pool after the stall: %v", err)
	}
}

// A connection held for good (db.Conn: the listeners, the pipeline's
// leader with its session lock) stays up while its holder waits, since
// ReceiveNotifications pings it. When it stalls, the holder hears within a
// ping and its timeout (and reconnects), and Postgres ends the session once
// idle past db.HeldIdleTimeout, so its session lock goes and another
// instance can take it. Timings shortened.
func TestStalledHeldConnection(t *testing.T) {
	dbtest.HeldTimings(t, 1500*time.Millisecond, 300*time.Millisecond, 500*time.Millisecond)
	d, owner := dbtest.NewWithURL(t)
	ctx := context.Background()
	s := newStall(t)
	via := stallablePool(t, owner, s, nil)
	conn, err := via.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer db.CloseHeld(conn)
	const leader = 0x5354414c4c // as the pipeline's leaderLock
	var got bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, leader).Scan(&got); err != nil || !got {
		t.Fatalf("taking the lock: %v %v", got, err)
	}
	if _, err := conn.Exec(ctx, `LISTEN stall_test`); err != nil {
		t.Fatal(err)
	}
	notes := make(chan string, 4)
	waited := make(chan error, 1)
	wctx, stopWaiting := context.WithCancel(ctx)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		waited <- db.ReceiveNotifications(wctx, conn, func(n *pgconn.Notification) {
			select {
			case notes <- n.Payload:
			case <-wctx.Done():
			}
		})
	}()
	// Before CloseHeld (deferred above): the waiter is done with conn.
	defer func() { stopWaiting(); <-finished }()

	// Idle past the session limit twice over, but pinged: still up, still
	// listening, still leading.
	time.Sleep(3 * time.Second)
	if _, err := d.Pool.Exec(ctx, `SELECT pg_notify('stall_test', 'still here')`); err != nil {
		t.Fatal(err)
	}
	select {
	case p := <-notes:
		if p != "still here" {
			t.Fatalf("notification %q", p)
		}
	case err := <-waited:
		t.Fatalf("the held connection ended while it was fine: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("no notification on the held connection")
	}
	other, err := d.Pool.Acquire(ctx) // another instance, asking for the lock
	if err != nil {
		t.Fatal(err)
	}
	defer other.Release()
	defer other.Exec(context.Background(), `SELECT pg_advisory_unlock_all()`)
	var free bool
	if err := other.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, leader).Scan(&free); err != nil || free {
		t.Fatalf("the lock was free (%v, %v) while its holder was fine", free, err)
	}

	s.stop()
	stalledAt := time.Now()
	select {
	case err := <-waited:
		if err == nil || errors.Is(err, context.Canceled) {
			t.Fatalf("the holder's wait ended with %v", err)
		}
		t.Logf("the holder heard after %v: %v", time.Since(stalledAt), err)
	case <-time.After(5 * time.Second):
		t.Fatal("the holder kept waiting on a stalled connection")
	}
	// Postgres ends the stalled session; its session lock goes.
	eventually(t, 10*time.Second, "the stalled session's lock freed", func() bool {
		var got bool
		err := other.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, leader).Scan(&got)
		return err == nil && got
	})
	t.Logf("the lock was freed %v after the stall began", time.Since(stalledAt))
}

// A held connection that receives notifications more often than HeldPing
// (a busy instance's listener) keeps its session past HeldIdleTimeout:
// notifications don't reset Postgres's idle timer, so it's pinged on a
// fixed schedule regardless (#1311: busy listeners were ended every ~91 s,
// and the pipeline's leadership moved with them). Timings shortened: a
// notification every 100 ms against a 300 ms ping and a 1.5 s limit.
func TestBusyHeldConnectionKeepsItsSession(t *testing.T) {
	dbtest.HeldTimings(t, 1500*time.Millisecond, 300*time.Millisecond, 500*time.Millisecond)
	d := dbtest.New(t)
	ctx := context.Background()
	conn, err := d.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer db.CloseHeld(conn)
	var pid int
	if err := conn.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `LISTEN busy_test`); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	received := 0
	wctx, stop := context.WithCancel(ctx)
	waited := make(chan error, 1)
	go func() {
		waited <- db.ReceiveNotifications(wctx, conn, func(*pgconn.Notification) {
			mu.Lock()
			received++
			mu.Unlock()
		})
	}()

	const run = 4 * time.Second // over twice the idle limit
	sent := 0
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for end := time.Now().Add(run); time.Now().Before(end); {
		select {
		case err := <-waited:
			t.Fatalf("the busy held connection ended after %d notifications: %v", sent, err)
		case <-tick.C:
		}
		if _, err := d.Pool.Exec(ctx, `SELECT pg_notify('busy_test', 'n')`); err != nil {
			t.Fatal(err)
		}
		sent++
	}
	eventually(t, 5*time.Second, "every notification received", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return received == sent
	})
	stop()
	if err := <-waited; !errors.Is(err, context.Canceled) {
		t.Fatalf("the wait ended with %v", err)
	}
	// The same session throughout, still answering.
	var still int
	if err := conn.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&still); err != nil || still != pid {
		t.Fatalf("the held session after %v: pid %d (was %d), %v", run, still, pid, err)
	}
}
