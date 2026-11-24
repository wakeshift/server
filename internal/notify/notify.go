// Package notify fans Postgres notifications out to the goroutines waiting
// on them. One connection per instance listens; waiters register a channel
// for (channel, payload). Notifications are hints, never the only signal:
// waiters on Resync channels are all woken when the listener (re)connects,
// and a waiter also polls (slowly), so one missed costs only latency.
package notify

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/wakeshift/server/internal/db"
)

// Hub listens on Postgres channels and wakes waiters.
type Hub struct {
	pool     *pgxpool.Pool
	channels []string
	// Resync: channels whose waiters are all woken whenever the listener
	// (re)connects, since notifications sent while nothing listened are
	// lost. For channels whose waiters just check for work; not for ones
	// where a wake means something happened (a takeover).
	Resync []string

	mu      sync.Mutex
	waiters map[key]map[chan struct{}]bool

	connects atomic.Int64 // listener connections made (tests)
}

type key struct{ channel, payload string }

// New returns a Hub for channels; Run starts listening.
func New(pool *pgxpool.Pool, channels ...string) *Hub {
	return &Hub{pool: pool, channels: channels, waiters: map[key]map[chan struct{}]bool{}}
}

// Wait returns a channel that receives (without blocking the hub) when a
// notification on channel carries payload, and a function to stop waiting.
func (h *Hub) Wait(channel, payload string) (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	k := key{channel, payload}
	h.mu.Lock()
	if h.waiters[k] == nil {
		h.waiters[k] = map[chan struct{}]bool{}
	}
	h.waiters[k][ch] = true
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		delete(h.waiters[k], ch)
		if len(h.waiters[k]) == 0 {
			delete(h.waiters, k)
		}
		h.mu.Unlock()
	}
}

// Run listens until ctx ends, reconnecting after errors.
func (h *Hub) Run(ctx context.Context) {
	for ctx.Err() == nil {
		err := h.listen(ctx)
		if ctx.Err() != nil {
			return
		}
		slog.Warn("notify: listener stopped; reconnecting", "err", err)
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
		}
	}
}

// resyncPerWaiter and resyncSpread spread a resync's wakes out: each
// woken waiter runs a claim transaction, so waking all of a busy
// instance's streams at once (on every listener reconnect, a database
// failover or maintenance, on every instance together) can saturate its
// pool. Wakes go out about resyncPerWaiter apart, in random order, over
// at most resyncSpread: a few waiters are woken at once, a full instance's
// over about 5 s. Variables, so tests can shorten them.
var (
	resyncPerWaiter = 5 * time.Millisecond
	resyncSpread    = 5 * time.Second
)

// resyncSteps is how many batches a resync's wakes go out in, at most.
const resyncSteps = 50

// resync wakes every waiter on a Resync channel, spread out (resyncSpread)
// in the background until ctx ends. A waiter that stops waiting before its
// turn is woken harmlessly (its channel is buffered and no longer read).
func (h *Hub) resync(ctx context.Context) {
	h.mu.Lock()
	var chans []chan struct{}
	for k, set := range h.waiters {
		if !slices.Contains(h.Resync, k.channel) {
			continue
		}
		for ch := range set {
			chans = append(chans, ch)
		}
	}
	h.mu.Unlock()
	if len(chans) == 0 {
		return
	}
	rand.Shuffle(len(chans), func(i, j int) { chans[i], chans[j] = chans[j], chans[i] })
	spread := min(time.Duration(len(chans))*resyncPerWaiter, resyncSpread)
	steps := min(len(chans), resyncSteps)
	if spread < 10*time.Millisecond {
		steps = 1
	}
	go func() {
		for i := range steps {
			if i > 0 {
				select {
				case <-ctx.Done():
					return
				case <-time.After(spread / time.Duration(steps)):
				}
			}
			for _, ch := range chans[i*len(chans)/steps : (i+1)*len(chans)/steps] {
				select {
				case ch <- struct{}{}:
				default:
				}
			}
		}
	}()
}

func (h *Hub) listen(ctx context.Context) error {
	// Its own connection, not a pool slot: it's held for good, and pinged
	// on a fixed schedule (db.ReceiveNotifications), so one that stalls is
	// replaced rather than listened on, deaf, until its tunnel breaks.
	conn, err := pgx.ConnectConfig(ctx, db.HeldConnConfig(h.pool.Config().ConnConfig))
	if err != nil {
		return err
	}
	defer db.CloseHeld(conn)
	h.connects.Add(1)
	for _, c := range h.channels {
		if _, err := conn.Exec(ctx, `LISTEN "`+c+`"`); err != nil {
			return err
		}
	}
	// Notifications sent while no connection listened (before the first,
	// or while reconnecting) are lost: wake the Resync waiters to check.
	h.resync(ctx)
	return db.ReceiveNotifications(ctx, conn, func(n *pgconn.Notification) {
		h.mu.Lock()
		for ch := range h.waiters[key{n.Channel, n.Payload}] {
			select {
			case ch <- struct{}{}:
			default: // already signaled
			}
		}
		h.mu.Unlock()
	})
}
