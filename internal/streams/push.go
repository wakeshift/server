package streams

// What a push stream (MCP Events' events/stream, served by appapi's
// eventspush.go) takes from the stream server, so it has what every core
// stream has rather than parallel machinery: the instance's cap
// (MaxStreams) and drain, the notify hub's wakes, one holder per session
// (stream_leases: the newest wins, the older is told on TakeoverChannel
// and what it hadn't confirmed goes back), lease renewal, and handing back
// what a lost connection confirmed lately (lostWithin). appapi owns the
// rest: the subscription, its frames, cursors and checks.

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/deliveries"
)

// PushEndedChannel is notified with an event subscription's ID when a push
// subscription ends (revoked, ended by the app, its grant revoked, closed
// by a policy, its one delivery made, its user forgotten), so the stream
// holding it ends at once. The hub must listen on it too.
const PushEndedChannel = "wakeshift_push_ended"

// pushHolderPrefix starts a push stream's lease holder: a connection
// (isConnection), so a takeover tells it to close.
const pushHolderPrefix = "conn_push_"

// NewPushHolder is a new push stream's lease holder ID: random, naming
// nothing.
func NewPushHolder() string { return holderID(pushHolderPrefix) }

// ErrDisplaced: a newer stream holds the session now.
var ErrDisplaced = errDisplaced

// A push stream's share of an instance's MaxStreams, so one app's users
// (or push as a whole) can't take the room connectors and agents' own
// streams need: push streams in all take at most PushShare of it, one
// app's at most PushAppShare. Past either, a new push stream is turned
// away (503) as at capacity.
const (
	PushShare    = 0.5
	PushAppShare = 0.25
	// pushNearShare: a push share this full logs a warning (the 80% alert).
	pushNearShare = 0.8
)

// pushCaps are an instance's push caps for maxStreams (0: none).
func pushCaps(maxStreams int) (total, perApp int) {
	if maxStreams <= 0 {
		return 0, 0
	}
	return max(int(float64(maxStreams)*PushShare), 1), max(int(float64(maxStreams)*PushAppShare), 1)
}

// AdmitPush counts one of app's push streams in: against MaxStreams as
// every stream is, and against push's shares of it (PushShare,
// PushAppShare), unless the instance is draining. LeavePush counts it out.
func (s *Server) AdmitPush(app string) error {
	s.connsMu.Lock()
	defer s.connsMu.Unlock()
	if s.draining() {
		return errDraining
	}
	key := "push:" + app
	total, perApp := pushCaps(s.MaxStreams)
	if s.MaxStreams > 0 && (s.active.Load() >= int64(s.MaxStreams) || s.pushes >= total || s.pushesByApp[app] >= perApp) {
		slog.Warn("streams: push streams at their share of this instance", "app", app, "app_streams", s.pushesByApp[app], "push_streams", s.pushes,
			"app_cap", perApp, "push_cap", total)
		return busyError{after: s.refusals.refused(key, time.Now())}
	}
	s.refusals.admitted(key)
	s.active.Add(1)
	s.conns.Add(1)
	if s.pushesByApp == nil {
		s.pushesByApp = map[string]int{}
	}
	s.pushes++
	s.pushesByApp[app]++
	if perApp > 0 && float64(s.pushesByApp[app]) >= pushNearShare*float64(perApp) && s.pushesByApp[app]%max(perApp/20, 1) == 0 {
		slog.Warn("streams: an app's push streams near their share of this instance", "app", app, "app_streams", s.pushesByApp[app], "app_cap", perApp)
	}
	return nil
}

// LeavePush counts out one of app's push streams AdmitPush counted in.
func (s *Server) LeavePush(app string) {
	s.connsMu.Lock()
	s.pushes--
	if s.pushesByApp[app]--; s.pushesByApp[app] <= 0 {
		delete(s.pushesByApp, app)
	}
	s.connsMu.Unlock()
	s.leave()
}

// TurnAway writes Admit's refusal: 503 with Retry-After, restarting or at
// capacity.
func TurnAway(w http.ResponseWriter, err error) { turnAway(w, err) }

// Draining is closed when the instance drains (Drain): every stream ends
// so its client reconnects to another instance.
func (s *Server) Draining() <-chan struct{} { return s.drainCh() }

// RenewEvery is how often a stream renews and re-checks (Renew).
func (s *Server) RenewEvery() time.Duration { return s.renew() }

// StreamMaxAge is how long a stream stays open before it's closed to
// reconnect (MaxAge).
func (s *Server) StreamMaxAge() time.Duration { return s.maxAge() }

// DeliveryLease is how long a written delivery stays leased without a
// renewal (Lease).
func (s *Server) DeliveryLease() time.Duration { return s.lease() }

// LostWithin is how far back a lost connection hands back what it
// confirmed (the package doc).
const LostWithin = lostWithin

// TakePush, in the session's tenant (tx), makes holder the holder of the
// session's stream (agent, target): the newest wins. The previous holder's
// unconfirmed deliveries go back to pending, and it's told to close
// (TakeoverChannel). Locks go session row, then lease row, as every
// stream's do.
func TakePush(ctx context.Context, tx pgx.Tx, agent, target, holder string) error {
	st := &stream{l: Link{Agent: agent, Target: target}, holder: holder}
	held, err := st.lockLease(ctx, tx)
	if err != nil {
		return err
	}
	_, err = st.take(ctx, tx, held)
	return err
}

// HoldingPush checks, in tx, that holder still holds the session's stream
// (ErrDisplaced if not).
func HoldingPush(ctx context.Context, tx pgx.Tx, agent, target, holder string) error {
	var current string
	err := tx.QueryRow(ctx, `SELECT holder FROM stream_leases WHERE agent_id = $1 AND target = $2 FOR SHARE`, agent, target).Scan(&current)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && current != holder {
		return errDisplaced
	}
	return err
}

// RenewPush renews holder's lease on the session's stream (presence) and
// on what it has written and not confirmed (ErrDisplaced if a newer
// stream holds it).
func RenewPush(ctx context.Context, tx pgx.Tx, agent, target, holder string, lease time.Duration) error {
	tag, err := tx.Exec(ctx, `UPDATE stream_leases SET renewed_at = now() WHERE agent_id = $1 AND target = $2 AND holder = $3`, agent, target, holder)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return errDisplaced
	}
	return deliveries.Renew(ctx, tx, holder, lease)
}

// ReleasePush lets go of the session's stream: it no longer counts as
// connected (unless a newer stream holds it), what holder wrote and didn't
// confirm goes back to pending, and, for a lost connection, so does what
// it confirmed in the last LostWithin.
func ReleasePush(ctx context.Context, tx pgx.Tx, agent, target, holder string, lost bool) error {
	if _, err := tx.Exec(ctx, `UPDATE stream_leases SET renewed_at = NULL WHERE agent_id = $1 AND target = $2 AND holder = $3`,
		agent, target, holder); err != nil {
		return err
	}
	if lost {
		if _, err := deliveries.ResendLost(ctx, tx, agent, holder, lostWithin); err != nil {
			return err
		}
	}
	return deliveries.Release(ctx, tx, holder)
}
