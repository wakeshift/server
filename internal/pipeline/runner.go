// Package pipeline turns events into deliveries.
//
// For each active subscription, the Runner reads the events after the
// subscription's cursor, runs its pipeline on each, writes the resulting
// deliveries and advances the cursor, all in one transaction. A crash
// anywhere rolls the whole step back, and because delivery IDs are derived
// from (agent, event, target), redoing a step never creates duplicates.
//
// Runners on several instances share the work: each claims cursor rows with
// FOR UPDATE SKIP LOCKED, so a subscription is processed by one runner at a
// time. One of them (the leader) handles event notifications; every one
// can sweep, one at a time.
package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/wakeshift/server/internal/apps"
	"github.com/wakeshift/server/internal/audit"
	"github.com/wakeshift/server/internal/db"
	"github.com/wakeshift/server/internal/deliveries"
	"github.com/wakeshift/server/internal/events"
	"github.com/wakeshift/server/internal/sessions"
	"github.com/wakeshift/server/internal/subscriptions"
)

// Runner processes subscriptions until its context ends.
type Runner struct {
	DB *db.DB
	// Batch is how many events one step reads per subscription (default 200).
	Batch int
	// Poll is how often tenants are checked even without a notification
	// (default 1m), by one instance at a time: those marked due
	// (pipeline_due: an event since, or a delivery leased), and every
	// tenant once FullEvery. Expired delivery leases are reaped at the same
	// pace. Notifications are what normally wake it: none is dropped while
	// the listener is connected, and every tenant is checked when it
	// (re)connects.
	Poll time.Duration
	// FullEvery is how often a sweep checks every tenant, marked or not
	// (default 15m): a safety net for a mark that's missing (deleted by
	// hand, or by an older revision's settle during a rollout).
	FullEvery time.Duration
	// Workers is how many tenants are processed at once after
	// notifications (default 8; only the leader has any).
	Workers int
	// Delivered, if set, is told of each agent a step made deliveries for,
	// once the step commits (so MCP Events webhooks send them at once).
	Delivered func(tenant, agent string)
	// Finished, if set, runs in a step's transaction after a "once"
	// subscription ends with its first delivery (subscriptions.Finish),
	// so what was made with it ends too (appapi.FinishedOnce): the
	// delivery it just made is in the same transaction, still to be sent.
	// An error rolls the step back.
	Finished func(ctx context.Context, tx pgx.Tx, subscriptionID string) error

	// beforeCommit, if set, runs at the end of each step's transaction; an
	// error rolls the step back. Tests use it to simulate a crash.
	beforeCommit func() error
	// onSweep, if set, is told of each sweep Run asks for, and whether
	// it's full (tests).
	onSweep func(full bool)
}

const subscriptionsPerStep = 10

// Run processes every tenant's subscriptions, waking on new events and
// sweeping as a fallback (the tenants marked due, and now and then every
// tenant), until ctx ends. The sweep runs
// beside the notified work, so a long one doesn't hold up wake-ups.
func (r *Runner) Run(ctx context.Context) error {
	q := newQueue()
	resync := make(chan struct{}, 1)
	go r.listen(ctx, q, resync)
	var wg sync.WaitGroup
	for range r.workers() {
		wg.Go(func() {
			for {
				tenant, ok := q.next(ctx)
				if !ok {
					return
				}
				if _, err := r.Tenant(ctx, tenant); err != nil && ctx.Err() == nil {
					slog.Error("pipeline: tenant failed", "tenant", tenant, "err", err)
				}
				q.done(tenant)
			}
		})
	}
	defer wg.Wait()
	ticker := time.NewTicker(r.poll())
	defer ticker.Stop()
	var lastFull time.Time
	sweep := func(full bool) {
		full = full || time.Since(lastFull) >= r.fullEvery()
		if r.onSweep != nil {
			r.onSweep(full)
		}
		if r.sweep(ctx, full) && full {
			lastFull = time.Now()
		}
	}
	sweep(true)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-resync:
			// The marked tenants only: every event marks its tenant
			// (pipeline_due), so they're all an event notified while
			// nothing listened could be in; a full sweep, a transaction
			// for every tenant, still runs every FullEvery.
			sweep(false)
		case <-ticker.C:
			sweep(false)
		}
	}
}

// sweepLock is the advisory lock key under which one instance at a time
// sweeps every tenant.
const sweepLock = 0x77616b6501 // "wake" 1

// sweep checks the tenants marked due (every tenant, if full), unless
// another instance is sweeping now; it reports whether it ran.
func (r *Runner) sweep(ctx context.Context, full bool) bool {
	ran, err := r.DB.Exclusive(ctx, sweepLock, func() { r.all(ctx, full) })
	if err != nil && ctx.Err() == nil {
		slog.Error("pipeline: sweeping", "err", err)
	}
	return ran
}

// queue is the set of tenants notified and not yet processed. A tenant
// is handed to one worker at a time; notified again meanwhile, it's
// processed again after.
type queue struct {
	mu      sync.Mutex
	pending []string
	queued  map[string]bool // in pending
	busy    map[string]bool // with a worker
	again   map[string]bool // notified while busy
	ready   chan struct{}
}

func newQueue() *queue {
	return &queue{queued: map[string]bool{}, busy: map[string]bool{}, again: map[string]bool{}, ready: make(chan struct{}, 1)}
}

func (q *queue) add(tenant string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	switch {
	case q.busy[tenant]:
		q.again[tenant] = true
	case !q.queued[tenant]:
		q.queued[tenant] = true
		q.pending = append(q.pending, tenant)
		q.signal()
	}
}

func (q *queue) signal() {
	select {
	case q.ready <- struct{}{}:
	default:
	}
}

// next waits for a tenant to process, until ctx ends.
func (q *queue) next(ctx context.Context) (string, bool) {
	for {
		q.mu.Lock()
		if len(q.pending) > 0 {
			t := q.pending[0]
			q.pending = q.pending[1:]
			delete(q.queued, t)
			q.busy[t] = true
			if len(q.pending) > 0 {
				q.signal() // for the next worker
			}
			q.mu.Unlock()
			return t, true
		}
		q.mu.Unlock()
		select {
		case <-ctx.Done():
			return "", false
		case <-q.ready:
		}
	}
}

func (q *queue) done(tenant string) {
	q.mu.Lock()
	delete(q.busy, tenant)
	again := q.again[tenant]
	delete(q.again, tenant)
	q.mu.Unlock()
	if again {
		q.add(tenant)
	}
}

func (r *Runner) all(ctx context.Context, full bool) {
	rows, _ := r.DB.Pool.Query(ctx, `SELECT tenant_id::text FROM pipeline_due ORDER BY tenant_id`)
	tenants, err := pgx.CollectRows(rows, pgx.RowTo[string])
	// Only a marked tenant is settled (unmarked): a full sweep's others
	// have no mark to take off, and a mark made during it waits for the
	// next sweep.
	marked := make(map[string]bool, len(tenants))
	for _, t := range tenants {
		marked[t] = true
	}
	if err == nil && full {
		tenants, err = r.DB.Tenants(ctx)
	}
	if err != nil {
		if ctx.Err() == nil {
			slog.Error("pipeline: listing tenants", "err", err)
		}
		return
	}
	start := time.Now()
	for _, t := range tenants {
		_, err := r.tenant(ctx, t, true)
		if err == nil && marked[t] {
			err = r.settle(ctx, t)
		}
		if err != nil && ctx.Err() == nil {
			slog.Error("pipeline: tenant failed", "tenant", t, "err", err)
		}
	}
	// A transaction per tenant (two for a marked one): past a few hundred
	// thousand tenants a full sweep outlasts its interval.
	if d := time.Since(start); d > r.slowAfter(full) {
		slog.Warn("pipeline: the sweep is taking over half its interval", "tenants", len(tenants), "full", full, "took", d.Round(time.Millisecond))
	}
}

// settle unmarks a tenant (pipeline_due) once a sweep has nothing left
// there: no active subscription's cursor behind the last event, and no
// delivery leased (a lease to reap if it runs out). An event appended
// meanwhile keeps the mark (its sequence number is past the one read here,
// or it marks again after the delete). So does a delivery leased
// meanwhile: settle first waits for the claims marking the tenant
// (events.HoldMarks), so it sees their leases, and a claim after it marks
// again once the delete commits.
func (r *Runner) settle(ctx context.Context, tenant string) error {
	return r.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		if err := events.HoldMarks(ctx, tx); err != nil {
			return err
		}
		last, err := events.LastSeq(ctx, tx)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `DELETE FROM pipeline_due WHERE tenant_id = current_tenant() AND seq <= $1
			AND NOT EXISTS (SELECT 1 FROM subscription_cursors c JOIN subscriptions s ON s.tenant_id = c.tenant_id AND s.id = c.subscription_id
				WHERE s.status = 'active' AND c.last_seq < $1)
			AND NOT EXISTS (SELECT 1 FROM deliveries WHERE state = 'in_flight')`, int64(last))
		return err
	})
}

// Tenant processes a tenant's subscriptions until none has events to read,
// returning how many deliveries it created.
func (r *Runner) Tenant(ctx context.Context, tenant string) (int, error) {
	return r.tenant(ctx, tenant, false)
}

// tenant is Tenant, reaping expired leases in its first step if reap is
// set (a sweep: one transaction per tenant for both).
func (r *Runner) tenant(ctx context.Context, tenant string, reap bool) (int, error) {
	total, conflicts, waits := 0, 0, 0
	for {
		n, more, skipped, err := r.step(ctx, tenant, reap)
		if err == nil {
			reap = false
		}
		total += n
		if err == nil && !more && skipped {
			// Another step (a sweep, or a request catching a subscription
			// up) holds a cursor with unread events. Its snapshot may predate
			// the event this step was woken for, and nothing would notify
			// again: try again once it's done, for a while.
			if waits++; waits > maxLockedWaits {
				return total, nil
			}
			select {
			case <-ctx.Done():
				return total, ctx.Err()
			case <-time.After(time.Duration(min(waits, 5)*20) * time.Millisecond):
			}
			continue
		}
		if errors.Is(err, errConflict) {
			// Another runner (or a cancel) changed a cursor after this
			// snapshot began. Back off briefly; after a few, leave the rest
			// to the next notification or poll.
			if conflicts++; conflicts > 5 {
				return total, nil
			}
			select {
			case <-ctx.Done():
				return total, ctx.Err()
			case <-time.After(time.Duration(conflicts*20) * time.Millisecond):
			}
			continue
		}
		if err != nil || !more {
			return total, err
		}
	}
}

var errConflict = errors.New("pipeline: serialization conflict")

// maxLockedWaits is how many times a tenant's steps try again for cursors
// another transaction holds (about 5s), before leaving them to the next
// notification or poll.
const maxLockedWaits = 50

// step claims a few subscriptions with unread events and processes one batch
// of each. more reports whether there may be more work: it claimed as many
// subscriptions as a step takes, or one had a full batch. Otherwise every
// subscription it could claim was read up to the snapshot's last event, and
// a later event comes with its own notification, so a step to find nothing
// isn't needed. skipped reports that a cursor with unread events was locked
// by another transaction: its holder's snapshot may not have the event this
// step was woken for, so the caller tries again once it's done.
func (r *Runner) step(ctx context.Context, tenant string, reap bool) (created int, more, skipped bool, err error) {
	notify := map[string]bool{} // agents with new deliveries
	err = r.DB.InTenantTx(ctx, tenant, pgx.TxOptions{IsoLevel: pgx.RepeatableRead}, func(tx pgx.Tx) error {
		created, more, skipped, notify = 0, false, false, map[string]bool{}
		paces := map[string]*pace{} // by grant: its window, read once a step
		if reap {
			// Expired leases back to pending (their agents are notified).
			n, err := deliveries.Reap(ctx, tx)
			if err != nil {
				return err
			}
			if n > 0 {
				slog.Info("pipeline: reaped expired leases", "tenant", tenant, "count", n)
			}
		}
		// Repeatable read: every query sees one snapshot. Events commit in seq
		// order, so the snapshot holds every event up to last, and a cursor
		// can move straight to last when nothing more matches.
		last, err := events.LastSeq(ctx, tx)
		if err != nil {
			return err
		}
		rows, _ := tx.Query(ctx, `
			SELECT c.subscription_id, c.last_seq FROM subscription_cursors c
			JOIN subscriptions s ON s.tenant_id = c.tenant_id AND s.id = c.subscription_id
			WHERE s.status = 'active' AND c.last_seq < $1
			ORDER BY c.updated_at LIMIT $2
			FOR UPDATE OF c SKIP LOCKED`, last, subscriptionsPerStep)
		type cursor struct {
			SubscriptionID string
			LastSeq        uint64
		}
		cursors, err := pgx.CollectRows(rows, pgx.RowToStructByPos[cursor])
		if err != nil {
			return err
		}
		more = len(cursors) == subscriptionsPerStep
		if !more {
			// Were any skipped (locked)? Counted in this snapshot, without
			// locks: the eligible cursors past the ones claimed.
			var eligible int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM (SELECT 1 FROM subscription_cursors c
				JOIN subscriptions s ON s.tenant_id = c.tenant_id AND s.id = c.subscription_id
				WHERE s.status = 'active' AND c.last_seq < $1 LIMIT $2) e`, last, subscriptionsPerStep+1).Scan(&eligible); err != nil {
				return err
			}
			skipped = eligible > len(cursors)
		}
		for _, c := range cursors {
			sub, err := subscriptions.Get(ctx, tx, c.SubscriptionID)
			if err != nil {
				return err
			}
			evs, err := events.ReadAfter(ctx, tx, c.LastSeq, []string{sub.EventType}, r.batch())
			if err != nil {
				return err
			}
			next := last
			if len(evs) == r.batch() {
				next = evs[len(evs)-1].Seq // a full batch: there may be more
				more = true
			}
			// A subscription an app made under a grant is paced by the grant's
			// wake limits: a wake-up past them is scheduled for when there's
			// room, or passed over if that's more than maxDelay away (a flood).
			// Past the grant's end (or once it's revoked), nothing is
			// delivered: the expiry pass ends the subscription later, but the
			// consent ends now.
			pace := &pace{}
			if len(evs) > 0 {
				if pace, err = pacing(ctx, tx, sub.ID, paces); err != nil {
					return err
				}
			}
			if pace.ended {
				evs = nil
			}
			// A "once" subscription (a topic's) ends with its first delivery.
			once := sub.Filters[subscriptions.OnceFilter] == "true"
			for _, e := range evs {
				targets, err := Targets(ctx, tx, sub, e)
				if err != nil {
					return err
				}
				delivered := false
				for _, t := range targets {
					at, now, err := pace.schedule(ctx, tx)
					if err != nil {
						return err
					}
					if at.After(now.Add(maxDelay)) {
						if err := pace.passedOver(ctx, tx, sub, e); err != nil {
							return err
						}
						continue
					}
					inserted, err := deliveries.Insert(ctx, tx, deliveries.New{AgentID: sub.AgentID, Target: t.Target, DeliverTo: t.DeliverTo,
						SubscriptionID: sub.ID, Event: e, AvailableAt: at})
					if err != nil {
						return err
					}
					if inserted {
						// Only a new row is a wake-up, as in the window it
						// loaded: a delivery the agent already had for the
						// event (kept as it was) counts at its own time,
						// from the window or from when it was recorded.
						if err := pace.record(ctx, tx, at); err != nil {
							return err
						}
						created++
						notify[sub.AgentID] = true
					}
					delivered = true
				}
				if once && delivered {
					if err := subscriptions.Finish(ctx, tx, sub.ID); err != nil {
						return err
					}
					if r.Finished != nil {
						if err := r.Finished(ctx, tx, sub.ID); err != nil {
							return err
						}
					}
					break
				}
			}
			if _, err := tx.Exec(ctx, `UPDATE subscription_cursors SET last_seq = $2, updated_at = now() WHERE subscription_id = $1`, c.SubscriptionID, next); err != nil {
				return err
			}
		}
		for agent := range notify {
			if _, err := tx.Exec(ctx, `SELECT pg_notify($1, current_tenant()::text || ':' || $2)`, deliveries.NotifyChannel, agent); err != nil {
				return err
			}
		}
		if r.beforeCommit != nil {
			return r.beforeCommit()
		}
		return nil
	})
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "40001", "40P01": // serialization failure, deadlock
			return 0, true, false, errConflict
		}
	}
	if err == nil && r.Delivered != nil {
		for agent := range notify {
			r.Delivered(tenant, agent)
		}
	}
	return created, more, skipped, err
}

// Targets runs a subscription's pipeline on an event, returning where to
// deliver it: none if it's filtered out (step set v0: type and attribute
// filters), else the session or sessions it's for.
func Targets(ctx context.Context, tx pgx.Tx, sub subscriptions.Subscription, e events.Event) ([]Target, error) {
	if !sub.Matches(e) {
		return nil, nil
	}
	var out []Target
	add := func(s sessions.Session) {
		if s.ID == e.FromSession {
			return // a session's own posts never wake it
		}
		t := Target{Target: s.ID}
		if s.Runtime == sessions.Connector {
			t.DeliverTo = s.ID
		}
		if !slices.Contains(out, t) {
			out = append(out, t)
		}
	}
	// Named sessions of this agent (@codex#2) get it, and only they do.
	named := false
	for _, m := range e.Mentions {
		if agent, ok := sessions.IsHandle(m); ok && agent == sub.AgentID {
			named = true
			if s, err := sessions.ByHandle(ctx, tx, m); err == nil && s.Agent == sub.AgentID {
				add(s)
			} else if !errors.Is(err, sessions.ErrNotFound) {
				return nil, err
			}
		}
	}
	if named || e.From.Kind == "agent" && e.From.ID == sub.AgentID {
		return out, nil
	}
	// Otherwise it's for the agent: every session of it that joined gets it,
	// each through its own subscription (this one's, if it has a session),
	// connected or not (it's queued for the session). Only a subscription no
	// session claimed falls back to a pick: the agent's most recently active
	// connected session, else its most recent; an agent without sessions
	// gets it as a whole.
	if sub.SessionID != "" {
		s, err := sessions.Get(ctx, tx, sub.SessionID)
		if err == nil {
			add(s)
			return out, nil
		}
		if !errors.Is(err, sessions.ErrNotFound) {
			return nil, err
		}
	}
	s, err := sessions.Pick(ctx, tx, sub.AgentID, sub.SessionID, e.FromSession)
	switch {
	case errors.Is(err, sessions.ErrNotFound):
		return []Target{{}}, nil
	case err != nil:
		return nil, err
	}
	add(s)
	return out, nil
}

// Target is where a delivery goes: a session (or "" for the agent as a
// whole), and, for a connector session, DeliverTo (it's delivered on the
// agent's connector stream).
type Target struct {
	Target, DeliverTo string
}

// listen queues tenants named in event notifications until ctx ends,
// reconnecting if the connection drops; each time it connects it asks for
// a sweep of the marked tenants (resync), for what was notified while it
// wasn't listening.
//
// Only one instance listens at a time (the leader, holding an advisory lock
// on its listening connection): every instance would otherwise run a step
// for every event, all but one of them finding nothing. If the leader goes,
// its connection's lock goes with it and another instance takes over
// within leaderRetry, sweeping first.
//
// The others ask for the lock again every leaderRetry on the connection
// they'll listen on, held meanwhile: a new connection for each ask cost a
// Postgres backend, a TLS handshake through the Cloud SQL connector and a
// SCRAM exchange every 3 seconds on every instance but one (production,
// 2026-10-29: 600 connections every 30 minutes, nearly all under a second
// long). The connection is idle between asks, never in a transaction.
func (r *Runner) listen(ctx context.Context, q *queue, resync chan<- struct{}) {
	for ctx.Err() == nil {
		led := false
		err := func() error {
			conn, err := r.DB.Conn(ctx) // held for good: not a pool slot
			if err != nil {
				return err
			}
			defer db.CloseHeld(conn)
			for {
				led, err := tryLead(ctx, conn)
				if err != nil {
					return err
				}
				if led {
					break
				}
				select {
				case <-ctx.Done():
					return nil
				case <-time.After(leaderRetry): // another instance listens
				}
			}
			led = true
			slog.Info("pipeline: leading (listening for events)")
			if _, err := conn.Exec(ctx, "LISTEN "+events.NotifyChannel); err != nil {
				return err
			}
			// Including the first time: events since Run's first sweep
			// began may have been notified before this listened.
			select {
			case resync <- struct{}{}:
			default:
			}
			// Pinged on a fixed schedule, however busy: if it stalls, this
			// instance stops leading and reconnects, and Postgres ends the
			// stalled session (db.HeldConnConfig), so its lock goes and
			// another instance leads.
			return db.ReceiveNotifications(ctx, conn, func(n *pgconn.Notification) { q.add(n.Payload) })
		}()
		if ctx.Err() != nil {
			return
		}
		if led {
			// Leadership moves: visible in the logs, with why.
			slog.Warn("pipeline: stopped leading; retrying", "err", err)
		} else {
			slog.Warn("pipeline: notification listener stopped; retrying", "err", err)
		}
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
		}
	}
}

// tryLead takes the leader's lock on conn if no other session holds it.
func tryLead(ctx context.Context, conn *pgx.Conn) (bool, error) {
	var led bool
	err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, leaderLock).Scan(&led)
	return led, err
}

// leaderLock is the advisory lock key held by the instance that listens
// for events; leaderRetry is how often the others check it's still held.
const (
	leaderLock  = 0x77616b6503 // "wake" 3
	leaderRetry = 3 * time.Second
)

func (r *Runner) batch() int {
	if r.Batch > 0 {
		return r.Batch
	}
	return 200
}

// slowAfter is how long a sweep may take before it's logged as slow: half
// its interval. A full sweep's interval is FullEvery; a sweep of the marked
// tenants runs every poll.
func (r *Runner) slowAfter(full bool) time.Duration {
	if full {
		return r.fullEvery() / 2
	}
	return r.poll() / 2
}

func (r *Runner) poll() time.Duration {
	if r.Poll > 0 {
		return r.Poll
	}
	return time.Minute
}

func (r *Runner) fullEvery() time.Duration {
	if r.FullEvery > 0 {
		return r.FullEvery
	}
	return 15 * time.Minute
}

func (r *Runner) workers() int {
	if r.Workers > 0 {
		return r.Workers
	}
	return 8
}

// maxDelay is how far ahead a paced wake-up may be scheduled; past it the
// wake-up is passed over (and noted in the audit log).
const maxDelay = time.Hour

// pace schedules a grant's wake-ups within its limits, for one step: it
// reads the grant's recent wake-ups once (load), then schedules every
// target of the step's events under the grant in memory, adding each
// wake-up it schedules (record).
type pace struct {
	grant  string
	app    string
	limits []apps.WakeLimit
	ended  bool // the grant expired or was revoked: nothing is delivered

	loaded   bool
	wakes    []time.Time // the grant's latest wake-ups, oldest first: all of the window's that a limit can count
	dbNow    time.Time   // the database's clock when loaded
	loadedAt time.Time   // this instance's (monotonic) clock then
	claimed  bool        // its grant_paces row is written in this step
}

// grantEnded is true for a grant (g) that no longer covers deliveries:
// revoked, or past its end though the expiry pass hasn't revoked it yet.
// Live delivery (pacing) and an ending subscription's catch-up
// (ConsentEnded) both use it, so they agree on when consent ends.
const grantEnded = `(g.revoked_at IS NOT NULL OR g.expires_at <= now())`

// ConsentEnded reports whether a subscription made under a grant may no
// longer be delivered to: its grant was revoked or is past its end. A
// subscription the user made, not under a grant, never is.
func ConsentEnded(ctx context.Context, tx pgx.Tx, subscription string) (bool, error) {
	var ended bool
	err := tx.QueryRow(ctx, `SELECT `+grantEnded+`
		FROM subscriptions s JOIN grants g ON g.tenant_id = s.tenant_id AND g.id = s.grant_id
		WHERE s.id = $1`, subscription).Scan(&ended)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return ended, err
}

// pacing is the pace for a subscription's deliveries: none (zero) for one
// the user made, not under a grant. Subscriptions under the same grant
// share one pace in a step (paces, by grant), so it counts what the step
// scheduled for each of them.
func pacing(ctx context.Context, tx pgx.Tx, subscription string, paces map[string]*pace) (*pace, error) {
	p := &pace{}
	var scope []byte
	err := tx.QueryRow(ctx, `SELECT g.id::text, g.app_id, g.scope, `+grantEnded+`
		FROM subscriptions s JOIN grants g ON g.tenant_id = s.tenant_id AND g.id = s.grant_id
		WHERE s.id = $1`, subscription).Scan(&p.grant, &p.app, &scope, &p.ended)
	if errors.Is(err, pgx.ErrNoRows) {
		return &pace{}, nil
	}
	if err != nil {
		return p, err
	}
	if known := paces[p.grant]; known != nil {
		return known, nil
	}
	if !p.ended {
		var sc struct {
			Agent *apps.AgentScope `json:"agent"`
		}
		json.Unmarshal(scope, &sc)
		if sc.Agent == nil {
			sc.Agent = &apps.AgentScope{}
		}
		p.limits = sc.Agent.Limits()
	}
	paces[p.grant] = p
	return p, nil
}

// schedule is the earliest time, no earlier than now or the grant's
// latest scheduled wake-up (so they stay in order), when every limit has
// room.
func (p *pace) schedule(ctx context.Context, tx pgx.Tx) (at, now time.Time, err error) {
	if p.grant == "" {
		return time.Time{}, time.Time{}, nil
	}
	if !p.loaded {
		if err := p.load(ctx, tx); err != nil {
			return at, now, err
		}
	}
	// The database's clock, which streams compare available_at with (an
	// instance's clock running ahead would otherwise schedule wake-ups
	// that aren't due yet when it says they are), advanced by the time
	// since it was read. To the microsecond, as Postgres keeps times.
	now = p.dbNow.Add(time.Since(p.loadedAt)).Truncate(time.Microsecond)
	return p.next(now), now, nil
}

// next is schedule at now.
func (p *pace) next(now time.Time) time.Time {
	t := now
	if n := len(p.wakes); n > 0 && p.wakes[n-1].After(t) {
		t = p.wakes[n-1]
	}
	// From here t is at or after every wake-up, so a limit's window
	// ending at t holds the most recent ones.
	for range 20 {
		moved := false
		for _, l := range p.limits {
			// The l.Max-th most recent wake-up in the window ending at t: if
			// there is one, the window is full until it slides past it.
			w := time.Duration(l.Seconds) * time.Second
			i := len(p.wakes) - l.Max
			if i < 0 || !p.wakes[i].After(t.Add(-w)) {
				continue
			}
			t = p.wakes[i].Add(w + time.Millisecond)
			moved = true
		}
		if !moved {
			break
		}
	}
	return t
}

// load reads the grant's wake-ups that a limit could count: those inside
// the longest window before now (or scheduled after it), the most recent
// as many as the largest limit allows. Every delivery made under the
// grant counts, whatever its state, by the grant's own index
// (deliveries_grant_paced): however many subscriptions the grant has had.
func (p *pace) load(ctx context.Context, tx pgx.Tx) error {
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&p.dbNow); err != nil {
		return err
	}
	p.loadedAt = time.Now()
	var longest time.Duration
	most := 0
	for _, l := range p.limits {
		longest = max(longest, time.Duration(l.Seconds)*time.Second)
		most = max(most, l.Max)
	}
	rows, _ := tx.Query(ctx, paceWindowSQL, p.grant, p.dbNow.Add(-longest), most)
	wakes, err := pgx.CollectRows(rows, pgx.RowTo[time.Time])
	if err != nil {
		return err
	}
	slices.Reverse(wakes)
	p.wakes, p.loaded = wakes, true
	return nil
}

// paceWindowSQL is a grant's ($1) latest $3 wake-ups after $2.
const paceWindowSQL = `SELECT available_at FROM deliveries WHERE grant_id = $1 AND available_at > $2
	ORDER BY available_at DESC LIMIT $3`

// record adds a wake-up the step scheduled (at, from schedule) and
// delivered, so the step's next schedule counts it. The first in a step
// writes the grant's grant_paces row: a concurrent step on the grant,
// scheduling from a snapshot without this one's wake-ups, then gets a
// serialization conflict when it writes the row too (repeatable read) and
// runs again on a snapshot that has them, so together they never exceed a
// limit. Nothing else locks the row, so the step can wait for it: no
// request holds it while waiting on a cursor the step holds.
func (p *pace) record(ctx context.Context, tx pgx.Tx, at time.Time) error {
	if p.grant == "" {
		return nil
	}
	p.wakes = append(p.wakes, at)
	if p.claimed {
		return nil
	}
	if _, err := tx.Exec(ctx, `INSERT INTO grant_paces (tenant_id, grant_id) VALUES (current_tenant(), $1)
		ON CONFLICT (tenant_id, grant_id) DO UPDATE SET paced_at = now()`, p.grant); err != nil {
		return err
	}
	p.claimed = true
	return nil
}

// passedOver notes, at most once an hour per grant, that wake-ups were
// passed over: the agent is being woken faster than its limits for longer
// than maxDelay.
func (p *pace) passedOver(ctx context.Context, tx pgx.Tx, sub subscriptions.Subscription, e events.Event) error {
	slog.Info("pipeline: wake limit reached; event passed over", "subscription", sub.ID, "event", e.ID, "grant", p.grant)
	var noted bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM audit_log WHERE action = 'wake.limited' AND grant_id = $1 AND at > now() - interval '1 hour')`,
		p.grant).Scan(&noted); err != nil || noted {
		return err
	}
	return audit.Record(ctx, tx, audit.Entry{Actor: "hub", Action: "wake.limited", AgentID: sub.AgentID, AppID: p.app, GrantID: p.grant,
		Detail: map[string]any{"event": e.ID, "type": e.Type, "note": "wake-ups past the app's limits for over an hour are passed over"}})
}
