package appapi

// An open push stream (eventspush.go opens it): what it writes, how it
// confirms, re-checks and ends.

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/apps"
	"github.com/wakeshift/server/internal/audit"
	"github.com/wakeshift/server/internal/deliveries"
	"github.com/wakeshift/server/internal/events"
	"github.com/wakeshift/server/internal/grants"
	"github.com/wakeshift/server/internal/mcpevents"
	"github.com/wakeshift/server/internal/streams"
)

// ── The stream ────────────────────────────────────────────────────────────

// pushStream is one open push stream.
type pushStream struct {
	a        *API
	app      apps.App
	sub      *pushSub
	name     string
	holder   string
	grant    time.Duration
	opencode bool
	w        http.ResponseWriter
	rc       *http.ResponseController
	// pos is the highest position written (or the start): every cursor
	// the stream writes is at least it.
	pos int64
	// unconfirmed: written, not yet outlived by a write a heartbeat
	// interval later (oldest first).
	unconfirmed []pushSent
	lastWrite   time.Time
	hb          *time.Ticker
	replaced    <-chan struct{}
	lost        bool
}

type pushSent struct {
	id string
	at time.Time
}

// Why a stream ends without its client asking.
type pushEnd struct {
	terminated bool
	name, msg  string
	data       map[string]any
}

var (
	pushClose    = pushEnd{}
	pushRevoked  = pushEnd{terminated: true, name: "Forbidden", msg: "Access revoked"}
	pushRemoved  = pushEnd{terminated: true, name: "NotFound", msg: "The app no longer offers this event", data: map[string]any{"kind": "event"}}
	pushTTLEnded = pushEnd{terminated: true, name: "Forbidden", msg: "This subscription reached the end of its time"}
	pushFinished = pushEnd{terminated: true, name: "Forbidden", msg: "This subscription delivered its one event and ended"}
)

// endFor is how a stream whose subscription ended for reason says so.
func endFor(reason string) pushEnd {
	switch reason {
	case evEndPolicyChanged:
		return pushEnd{terminated: true, name: "Forbidden", msg: policyTerminated}
	case evEndStopped:
		return pushEnd{terminated: true, name: "Forbidden", msg: voucherStopped}
	case evEndDelivered:
		return pushFinished
	case evEndExpired:
		return pushEnd{terminated: true, name: "Forbidden", msg: "This subscription expired"}
	case evEndUnsubscribed:
		return pushEnd{terminated: true, name: "Forbidden", msg: "This subscription was ended"}
	}
	return pushRevoked
}

func (st *pushStream) tx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	return st.a.DB.InTenant(ctx, st.sub.tenant, fn)
}

func (st *pushStream) cursor(seq int64) string {
	return st.a.pushCursorOf(st.sub.tenant, st.sub.id, seq)
}

func (st *pushStream) run(reqCtx context.Context) {
	ctx, cancel := context.WithCancel(reqCtx)
	defer cancel()
	s := st.a.Streams
	log := slog.With("tenant", st.sub.tenant, "app", st.app.ID, "subscription", st.sub.id, "holder", st.holder)
	// Wait before anything is read, so no notification is missed between.
	wake, stopWake := s.Notify.Wait(deliveries.NotifyChannel, st.sub.tenant+":"+st.sub.agent)
	defer stopWake()
	replaced, stopReplaced := s.Notify.Wait(streams.TakeoverChannel, st.holder)
	defer stopReplaced()
	st.replaced = replaced
	ended, stopEnded := s.Notify.Wait(streams.PushEndedChannel, st.sub.id)
	defer stopEnded()
	defer func() {
		fctx, fcancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer fcancel()
		if err := st.tx(fctx, func(tx pgx.Tx) error {
			return streams.ReleasePush(fctx, tx, st.sub.agent, st.sub.session, st.holder, st.lost)
		}); err != nil {
			log.Warn("mcp events: releasing a push stream failed (its leases expire)", "err", err)
		}
	}()

	st.w.Header().Set("Content-Type", "application/x-ndjson")
	st.w.Header().Set("Cache-Control", "no-store")
	st.w.Header().Set("X-Accel-Buffering", "no")
	st.w.WriteHeader(http.StatusOK)
	// Heartbeats on a fixed cadence, events or not: SEP-3415 asks for one at
	// least every interval, and a client may watch for them alone.
	st.hb = time.NewTicker(st.grant)
	defer st.hb.Stop()
	st.pos = st.sub.from
	if err := st.writeFirstActive(); err != nil {
		st.lost = true
		return
	}
	log.Info("mcp events: push stream opened", "created", st.sub.created, "truncated", st.sub.truncated, "heartbeat_ms", st.grant.Milliseconds(),
		"voucher", st.sub.voucher)

	renew := time.NewTicker(s.RenewEvery())
	defer renew.Stop()
	age := time.NewTimer(s.StreamMaxAge())
	defer age.Stop()
	paced := time.NewTimer(time.Hour)
	paced.Stop()
	defer paced.Stop()

	finish := func(e pushEnd, why string) {
		st.end(ctx, e)
		log.Info("mcp events: push stream ended", "reason", why, "terminated", e.terminated)
	}
	due := true
	for {
		if due {
			due = false
			next, err := st.pump(ctx)
			switch {
			case errors.Is(err, streams.ErrDisplaced):
				finish(pushClose, "replaced")
				return
			case err != nil:
				if ctx.Err() == nil {
					log.Warn("mcp events: pushing failed", "err", err)
				}
				st.lost = true
				return
			}
			if !next.IsZero() {
				paced.Reset(max(time.Until(next), 0) + 10*time.Millisecond)
			}
		}
		select {
		case <-ctx.Done():
			st.lost = true
			log.Info("mcp events: push stream ended", "reason", "client_gone")
			return
		case <-s.Draining():
			finish(pushClose, "restarting")
			return
		case <-replaced:
			finish(pushClose, "replaced")
			return
		case <-age.C:
			finish(pushClose, "max_age")
			return
		case <-ended:
			// What waits goes first (a once subscription's one delivery).
			if _, err := st.pump(ctx); err != nil && !errors.Is(err, streams.ErrDisplaced) {
				st.lost = ctx.Err() != nil
			}
			if e, done, err := st.check(ctx, false); err == nil && done {
				finish(e, "ended")
				return
			} else if err != nil && ctx.Err() == nil {
				log.Warn("mcp events: checking a push subscription failed", "err", err)
			}
		case <-st.hb.C:
			if err := st.heartbeat(); err != nil {
				st.lost = true
				log.Info("mcp events: push stream ended", "reason", "write_failed")
				return
			}
			st.confirm(ctx, log)
		case <-renew.C:
			st.confirm(ctx, log)
			e, done, err := st.check(ctx, true)
			switch {
			case errors.Is(err, streams.ErrDisplaced):
				finish(pushClose, "replaced")
				return
			case err != nil:
				if ctx.Err() == nil {
					log.Warn("mcp events: renewing a push stream failed (keeping it)", "err", err)
				}
			case done:
				finish(e, "checked")
				return
			}
			due = true // the safety net, for a missed notification
		case <-wake:
			due = true
		case <-paced.C:
			due = true
		}
	}
}

// pushFrame is a claimed delivery ready to write: an event, or a gap.
type pushFrame struct {
	id  string
	seq int64
	occ json.RawMessage // nil: a gap
}

// pump writes everything the stream is owed, batch by batch, in order. next
// is when the earliest paced delivery becomes available (zero: none).
func (st *pushStream) pump(ctx context.Context) (next time.Time, err error) {
	s := st.a.Streams
	for {
		var frames []pushFrame
		var batch int
		err := st.tx(ctx, func(tx pgx.Tx) error {
			if err := streams.HoldingPush(ctx, tx, st.sub.agent, st.sub.session, st.holder); err != nil {
				return err
			}
			// What waits whose content was deleted (an event deleted once
			// delivered, or erased) is a gap the client is told of; Claim would
			// finish it silently. Only up to this batch's last position, so a
			// gap's cursor never passes a delivery still to be written.
			rows, _ := tx.Query(ctx, `UPDATE deliveries d SET state = 'dead', finished_at = now(), lease_until = NULL, lease_holder = NULL,
				last_error = 'skipped by a push stream (its content deleted)'
				FROM events e WHERE e.tenant_id = d.tenant_id AND e.id = d.event_id AND e.redacted_at IS NOT NULL
				AND d.agent_id = $1 AND d.target = $2 AND d.state = 'pending' AND d.available_at <= now()
				AND d.event_seq <= coalesce((SELECT event_seq FROM deliveries WHERE agent_id = $1 AND target = $2 AND state = 'pending'
					AND available_at <= now() ORDER BY event_seq OFFSET $3 LIMIT 1), 9223372036854775807)
				RETURNING d.id::text, d.event_seq`, st.sub.agent, st.sub.session, pushBatch-1)
			redacted, err := pgx.CollectRows(rows, pgx.RowToStructByPos[struct {
				ID  string
				Seq int64
			}])
			if err != nil {
				return err
			}
			for _, r := range redacted {
				frames = append(frames, pushFrame{id: r.ID, seq: r.Seq})
			}
			list, err := deliveries.Claim(ctx, tx, st.sub.agent, st.sub.session, st.holder, s.DeliveryLease(), pushBatch)
			if err != nil {
				return err
			}
			batch = len(list) + len(redacted)
			if len(list) < pushBatch {
				if next, err = deliveries.NextAvailable(ctx, tx, st.sub.agent, st.sub.session); err != nil {
					return err
				}
			}
			var gaps []string
			for _, d := range list {
				occ, err := mcpevents.OccurrenceJSON(d, st.name, st.app.ID)
				if errors.Is(err, events.ErrRedacted) || errors.Is(err, mcpevents.ErrTooLarge) {
					gaps = append(gaps, d.ID)
					frames = append(frames, pushFrame{id: d.ID, seq: int64(d.Event.Seq)})
					continue
				}
				if err != nil {
					return err
				}
				frames = append(frames, pushFrame{id: d.ID, seq: int64(d.Event.Seq), occ: occ})
			}
			slices.SortFunc(frames, func(a, b pushFrame) int { return cmp.Compare(a.seq, b.seq) })
			if len(gaps) > 0 {
				_, err = tx.Exec(ctx, `UPDATE deliveries SET state = 'dead', finished_at = now(), lease_until = NULL, lease_holder = NULL,
					last_error = 'skipped by a push stream (too large, or its content deleted)' WHERE id = ANY($1::uuid[]) AND state = 'in_flight'`, gaps)
			}
			return err
		})
		if err != nil || batch == 0 {
			return next, err
		}
		var unwritten []string
		for i, f := range frames {
			if err := st.interrupted(); err != nil {
				unwritten = idsOf(frames[i:])
				st.unclaim(ctx, unwritten)
				return time.Time{}, err
			}
			if f.occ == nil {
				// A gap: the client is told some were skipped, with the
				// cursor past it.
				st.pos = max(st.pos, f.seq)
				if err := st.writeActive(true); err != nil {
					st.unclaim(ctx, idsOf(frames[i+1:]))
					return time.Time{}, err
				}
				continue
			}
			if err := st.writeEvent(f); err != nil {
				st.unclaim(ctx, idsOf(frames[i+1:]))
				return time.Time{}, err
			}
			st.unconfirmed = append(st.unconfirmed, pushSent{f.id, time.Now()})
			st.pos = max(st.pos, f.seq)
		}
		if batch < pushBatch {
			return next, nil
		}
	}
}

func idsOf(frames []pushFrame) []string {
	out := make([]string, 0, len(frames))
	for _, f := range frames {
		if f.occ != nil {
			out = append(out, f.id)
		}
	}
	return out
}

// unclaim gives back what was claimed and never written, without counting
// the attempt.
func (st *pushStream) unclaim(ctx context.Context, ids []string) {
	if len(ids) == 0 {
		return
	}
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := st.tx(fctx, func(tx pgx.Tx) error { return deliveries.Unclaim(fctx, tx, st.holder, ids) }); err != nil {
		slog.Warn("mcp events: giving back a push batch failed (its leases expire)", "holder", st.holder, "err", err)
	}
}

// interrupted reports, without blocking, whether the stream must stop
// writing: a newer one took it, or the instance is draining.
func (st *pushStream) interrupted() error {
	select {
	case <-st.replaced:
		return streams.ErrDisplaced
	case <-st.a.Streams.Draining():
		return streams.ErrDisplaced
	default:
		return nil
	}
}

// write writes one frame and flushes it. A write the client doesn't take
// within two heartbeat intervals ends the stream.
func (st *pushStream) write(b []byte) error {
	st.rc.SetWriteDeadline(time.Now().Add(2 * st.grant))
	_, err := st.w.Write(append(b, '\n'))
	if err == nil {
		err = st.rc.Flush()
	}
	if err == nil {
		st.lastWrite = time.Now()
	}
	return err
}

func (st *pushStream) writeJSON(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return st.write(b)
}

func (st *pushStream) writeActive(truncated bool) error {
	return st.writeJSON(map[string]any{"type": "active", "cursor": st.cursor(st.pos), "truncated": truncated, "heartbeat_ms": st.grant.Milliseconds()})
}

// writeFirstActive is the stream's first frame: for an open that made a
// new subscription, created (and for a voucher's, via and tool), which the
// SDK reads to run the app's onSubscribe and never passes to the client.
func (st *pushStream) writeFirstActive() error {
	f := map[string]any{"type": "active", "cursor": st.cursor(st.pos), "truncated": st.sub.truncated, "heartbeat_ms": st.grant.Milliseconds()}
	if st.sub.created {
		f["created"] = true
		if st.sub.tool != "" {
			f["via"], f["tool"] = madeByToolPrefix+st.sub.tool, st.sub.tool
		}
	}
	return st.writeJSON(f)
}

// writeEvent writes an occurrence with its cursor (the delivery's
// position), its payload as the app sent it.
func (st *pushStream) writeEvent(f pushFrame) error {
	occ := []byte(f.occ)
	if len(occ) < 2 || occ[len(occ)-1] != '}' {
		return errors.New("mcp events: an occurrence isn't an object")
	}
	b := make([]byte, 0, len(occ)+200)
	b = append(b, `{"type":"event","occurrence":`...)
	b = append(b, occ[:len(occ)-1]...)
	if len(occ) > 2 {
		b = append(b, ',')
	}
	b = append(b, `"cursor":`...)
	b = strconv.AppendQuote(b, st.cursor(max(st.pos, f.seq)))
	b = append(b, "}}"...)
	return st.write(b)
}

// heartbeat writes the stream's position: never behind a cursor it
// wrote, never past a delivery it hasn't.
func (st *pushStream) heartbeat() error {
	return st.writeJSON(map[string]any{"type": "heartbeat", "cursor": st.cursor(st.pos)})
}

// confirm marks delivered what the stream has outlived: written at least
// a heartbeat interval before its last write, which went through. A
// delivery to a client that wakes the conversation holding the watch
// (opencode) counts as a wake. That count trusts the client's own name
// (clientInfo.name, as the SDK passes it): it's a log line for metrics
// only, never billing or anything a client could gain by claiming it.
func (st *pushStream) confirm(ctx context.Context, log *slog.Logger) {
	if len(st.unconfirmed) == 0 {
		return
	}
	cut := st.lastWrite.Add(-st.grant)
	var ids []string
	var keep []pushSent
	for _, w := range st.unconfirmed {
		if w.at.After(cut) {
			keep = append(keep, w)
		} else {
			ids = append(ids, w.id)
		}
	}
	if len(ids) == 0 {
		return
	}
	if err := st.tx(ctx, func(tx pgx.Tx) error { return deliveries.Delivered(ctx, tx, st.holder, ids) }); err != nil {
		if ctx.Err() == nil {
			log.Warn("mcp events: confirming push deliveries failed (the next write tries again)", "err", err)
		}
		return
	}
	st.unconfirmed = keep
	harness, path := "", "native-push"
	if st.opencode {
		harness = harnessOpencode
	}
	log.Info("mcp events: push delivered", "count", len(ids), "wake", st.opencode, "harness", harness, "path", path)
}

// check re-checks, in one transaction, what the stream stands on: with
// renew, its hold (ErrDisplaced), its leases and its subscription's lease;
// always the app and its key, the subscription, its grant, its session's
// owner, the event and a voucher's ttl. done says the stream ends, and e
// how.
func (st *pushStream) check(ctx context.Context, renew bool) (e pushEnd, done bool, err error) {
	s := st.a.Streams
	err = st.tx(ctx, func(tx pgx.Tx) error {
		if renew {
			if err := streams.RenewPush(ctx, tx, st.sub.agent, st.sub.session, st.holder, s.DeliveryLease()); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE push_stream_holds SET renewed_at = now() WHERE holder = $1`, st.holder); err != nil {
				return err
			}
		}
		// The app and the key its assertion was signed with: a suspended
		// app, or a revoked key, closes the stream (a reconnect meets the
		// refusal at open).
		var appOK bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM apps WHERE id = $1 AND deleted_at IS NULL AND suspended_at IS NULL)
			AND ($2 = '' OR EXISTS (SELECT 1 FROM app_keys WHERE app_id = $1 AND key_id = $2 AND revoked_at IS NULL))`,
			st.app.ID, st.app.KeyID).Scan(&appOK); err != nil {
			return err
		}
		if !appOK {
			e, done = pushClose, true
			return nil
		}
		var reason string
		var ended, live bool
		var owner string
		err := tx.QueryRow(ctx, `SELECT coalesce(es.ended_reason, ''), es.ended_at IS NOT NULL,
			EXISTS (SELECT 1 FROM topic_subscriptions ts JOIN subscriptions s ON s.tenant_id = ts.tenant_id AND s.id = ts.subscription_id
				WHERE ts.subscription_id = es.subscription_id AND ts.ended_at IS NULL AND ts.expires_at > now() AND s.status = 'active'),
			coalesce((SELECT agent_id FROM sessions WHERE id = es.session_id), '')
			FROM event_subscriptions es WHERE es.id = $1::uuid`, st.sub.id).Scan(&reason, &ended, &live, &owner)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			e, done = pushRevoked, true // its workspace was erased
			return nil
		case err != nil:
			return err
		case ended:
			e, done = endFor(reason), true
			return nil
		case !live:
			// Its routing ended without the record (its ttl, or a once
			// subscription's one delivery, recorded by the next pass).
			e, done = pushFinished, true
			return nil
		case owner != st.sub.agent:
			e, done = pushRevoked, true
			return nil
		}
		if _, err := grants.Live(ctx, tx, st.app.ID, st.sub.agent); errors.Is(err, grants.ErrNotFound) {
			e, done = pushRevoked, true
			return nil
		} else if err != nil {
			return err
		}
		var offered bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM apps a, jsonb_array_elements(CASE WHEN jsonb_typeof(a.manifest->'events') = 'array'
			THEN a.manifest->'events' ELSE '[]'::jsonb END) ev WHERE a.id = $1 AND ev->>'name' = $2)`, st.app.ID, st.name).Scan(&offered); err != nil {
			return err
		}
		if !offered {
			e, done = pushRemoved, true
			return nil
		}
		if !st.sub.voucherEnds.IsZero() && !st.a.now().Before(st.sub.voucherEnds) {
			list, err := queryEventSubs(ctx, tx, `es.id = $1::uuid AND es.ended_at IS NULL FOR UPDATE OF es`, st.sub.id)
			if err != nil {
				return err
			}
			if err := endEventSubs(ctx, tx, list, evEndExpired, audit.App(st.app.ID), true); err != nil {
				return err
			}
			e, done = pushTTLEnded, true
			return nil
		}
		if renew {
			return renewPushLease(ctx, tx, st.sub, false)
		}
		return nil
	})
	return e, done, err
}

// end ends the stream for e: terminated (the subscription is over), or
// close (the client reconnects with its cursor). What was written and not
// confirmed goes back (the stream's release), so a reconnect gets it.
func (st *pushStream) end(ctx context.Context, e pushEnd) {
	if e.terminated {
		data := e.data
		if data == nil {
			data = map[string]any{}
		}
		st.writeJSON(map[string]any{"type": "terminated", "error": map[string]any{"name": e.name, "message": e.msg, "data": data}})
		return
	}
	st.writeJSON(map[string]string{"type": "close"})
}
