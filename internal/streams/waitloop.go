package streams

// A wait's loop mode: the same one-shot wait, for a shell loop that runs it
// again and again with nothing kept between rounds, so the command can be
// one short line that's correct as written:
//
//	chmod 700 "$HOME/.wakeshift/links"; while curl -sN --fail-with-body "$(cat "$HOME/.wakeshift/links/<name>")&loop=$$"; do :; done
//
// The server keeps what today's loop keeps in shell variables (wait.go):
//
//   - The next round confirms the last. A round's batch is confirmed by the
//     next round of the same loop (loop=<id>: the shell's $$, the same for
//     every round of one loop and different for the next loop the agent
//     starts), and only if its answer was written in full (loop_flushed),
//     nothing else has taken the stream since, and it's still leased (in
//     flight, within WaitLease). Anything else is handed back as today:
//     another loop, a poll, a hook, or a loop started later gets it again,
//     marked redelivered. With `while curl`, the same loop's next round
//     runs only after curl exited 0, having written the whole answer to
//     the Monitor: a batch the loop didn't print is never confirmed by it.
//   - The server follows its own rotation. A round on a link that a
//     renewal replaced (the grace path, successor_id) is served on the
//     newest link of its chain, and one on a link that expired while
//     nothing used it renews it there and then. So the link file is
//     written once and never changes. The chain still ends with the
//     session, a revocation, or RenewGrace after the link's token expired.
//   - Closed is a status. A round that can't go on (the link no longer
//     works, the session has nothing left, Wakeshift's hooks connected it,
//     or a newer loop holds the stream) answers 410 at once, its body the
//     hub.closed line saying why: curl --fail-with-body prints it and
//     exits non-zero, so the while ends. Something found while the round
//     waits (its 200 is already out) ends it with nothing more, and the
//     next round answers 410. A bad link is 401; a bad request 400.
//   - The server picks the timeout: LoopTimeout for Claude Code's sessions
//     (their Monitor stops after 30 minutes), whose rounds also end once
//     Wakeshift's hooks connect the session (until=hooks); DefaultWaitTimeout
//     otherwise. timeout= and until= still override.
//   - The body holds only events (and empty keepalive lines, which a Monitor
//     skips), or the hub.closed line: no cursor line, no hub.next.
//
// Two loops on one link (a stale Monitor and a new one): the newest takes
// the stream as today, and the loop it took it from is recorded
// (loop_replaced). That loop's next round, while the newer one is live, is
// refused (410, "replaced"), so the two never take the stream from each
// other in turn.
//
// Loop mode is opt-in per request: without loop=, the wait is as it was.

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/connectorversion"
	"github.com/wakeshift/server/internal/deliveries"
)

// LoopTimeout is how long a Claude Code loop runs before it ends itself
// (hub.restart): well inside the 30 minutes after which Claude Code stops a
// Monitor.
const LoopTimeout = 25 * time.Minute

// LoopRound is a loop-mode round's default timeout for Claude Code's
// sessions. Short rounds bound how long a round whose connection hung
// silently (behind a front end) can hold a loop: the command's curl gives
// up a little after it (harnesses.LoopRoundMax) and goes round again.
const LoopRound = 5 * time.Minute

// loopIDRE is what a loop's id may be: the shell's $$, or any short word.
var loopIDRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)

// errLoopReplaced: a loop's round while a newer loop holds the stream.
var errLoopReplaced = errors.New("streams: a newer wait loop holds the stream")

// Notes a loop's round ends with (hub.closed lines, 410).
var (
	loopNoLinkFile = Ending{"link_file", "This command found no link in its link file. Write the link to the file the steps name, at its full path (with $HOME replaced by your home directory's full path), then start the Monitor again the same way."}
	loopReplaced   = Ending{"replaced", "A newer copy of this command is running for this conversation, so this one stopped. Nothing to do here: the newer one gets the messages."}
	// A renewed link a round couldn't follow (its token is past
	// RenewGrace) reads as one that expired.
	loopExpired = Ending{"expired", "This command's link has expired, so it can't wait for messages any more. Tell the user that notifications for this conversation have stopped, and that they can ask you to start watching when they want them back."}
)

// waitLoop is a loop-mode round's part in taking the stream (longPoll).
type waitLoop struct {
	id string
	// prev: the loop whose wait held the stream before this round.
	prev string
}

// before runs in the transaction that takes the stream, its lease locked
// (lockLease), before take: it refuses a loop a newer one replaced while
// that one is live.
func (wl *waitLoop) before(ctx context.Context, tx pgx.Tx, agent, target string) error {
	var cur, replaced string
	var live bool
	err := tx.QueryRow(ctx, `SELECT coalesce(loop_id, ''), coalesce(loop_replaced, ''), coalesce(renewed_at > now() - interval '1 minute', false)
		FROM stream_leases WHERE agent_id = $1 AND target = $2`, agent, target).Scan(&cur, &replaced, &live)
	if err != nil {
		return err
	}
	if replaced == wl.id && cur != "" && cur != wl.id && live {
		return errLoopReplaced
	}
	wl.prev = cur
	return nil
}

// after runs once this round has taken the stream: it's this loop's now,
// since now if it wasn't already, and the loop it took it from (if
// another) is recorded as replaced.
func (wl *waitLoop) after(ctx context.Context, tx pgx.Tx, agent, target string) error {
	_, err := tx.Exec(ctx, `UPDATE stream_leases SET loop_id = $3, loop_flushed = NULL,
			loop_since = CASE WHEN $4 = $3 AND loop_since IS NOT NULL THEN loop_since ELSE now() END,
			loop_replaced = CASE WHEN $4 <> '' AND $4 <> $3 THEN $4 ELSE loop_replaced END
		WHERE agent_id = $1 AND target = $2`, agent, target, wl.id, wl.prev)
	return err
}

// loopState is what a loop's round found as it started (confirmLoop).
type loopState struct {
	// mine: the stream's last holder was this loop's; since: when the loop
	// first took it.
	mine  bool
	since time.Time
}

// confirmLoop runs before a loop's round does anything else: if the
// stream's last holder was this loop's last round, its answer was written
// in full, nothing has taken the stream since, and this round came within
// WaitGrace of it (the stream still counts as connected: presentFor), it
// confirms what that round handed out and still has leased. A loop's next
// round comes at once; one much later, from a loop with the same id, is
// someone else (or a PID used again): what it holds is handed back
// instead. The session's row, then the lease's, are locked first, in
// takeLease's order.
func (s *Server) confirmLoop(ctx context.Context, tenant string, l Link, id string) (loopState, error) {
	var st loopState
	err := s.DB.InTenant(ctx, tenant, func(tx pgx.Tx) (err error) {
		st, err = confirmLoopTx(ctx, tx, l, id)
		return err
	})
	return st, err
}

// confirmLoopTx is confirmLoop in tx.
func confirmLoopTx(ctx context.Context, tx pgx.Tx, l Link, id string) (loopState, error) {
	var st loopState
	err := func() error {
		if uuidRE.MatchString(l.Target) {
			if _, err := tx.Exec(ctx, `SELECT 1 FROM sessions WHERE id = $1::uuid FOR NO KEY UPDATE`, l.Target); err != nil {
				return err
			}
		}
		var holder, cur, flushed string
		var since *time.Time
		var live bool
		err := tx.QueryRow(ctx, `SELECT holder, coalesce(loop_id, ''), coalesce(loop_flushed, ''), loop_since, coalesce(renewed_at > now() - interval '1 minute', false)
			FROM stream_leases WHERE agent_id = $1 AND target = $2 FOR UPDATE`, l.Agent, l.Target).Scan(&holder, &cur, &flushed, &since, &live)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		st.mine = cur == id
		if since != nil {
			st.since = *since
		}
		if st.mine && flushed != "" && flushed == holder && live {
			return deliveries.AckLeased(ctx, tx, l.Agent, l.Target, holder)
		}
		return nil
	}()
	return st, err
}

// loopRestart is a Claude Code loop's last line once it has run for
// LoopTimeout (410, so the while ends): Claude Code stops a Monitor after
// 30 minutes, so the loop ends first and the agent starts it again.
var loopRestart = struct {
	Type string `json:"type"`
	Note string `json:"note"`
}{"hub.restart", "Routine restart: Claude Code stops a Monitor after 30 minutes, so this command ends by itself every 25. " +
	"Start the Monitor tool again now with the same command and timeout_ms 1800000, without asking the user: this is expected, and nothing was missed. " +
	"Tell the user only if it fails to start."}

// serveLoop serves a wait in loop mode (ServeWait with loop=).
func (s *Server) serveLoop(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	ctx := r.Context()
	id := q.Get("loop")
	if !loopIDRE.MatchString(id) {
		s.loopRefuse(ctx, w, http.StatusBadRequest, Ending{"bad_request", "loop is a short word: letters, digits, _ and -"})
		return
	}
	var timeout time.Duration
	tp := q.Get("timeout")
	if tp != "" {
		d, err := time.ParseDuration(tp)
		if err != nil {
			n, nerr := strconv.Atoi(tp)
			if nerr != nil {
				s.loopRefuse(ctx, w, http.StatusBadRequest, Ending{"bad_request", `timeout is a duration ("25m") or seconds`})
				return
			}
			d = time.Duration(n) * time.Second
		}
		timeout = min(max(d, time.Second), MaxWaitTimeout)
	}
	// At capacity, refused before any database work: held, then a 200 with
	// nothing (refusedRound), so a loop that goes round at once still comes
	// back only after the refusal's wait, and costs nothing meanwhile.
	tok := linkToken(r)
	if c, err := s.parse(tok); tok != "" && (err == nil || errors.Is(err, errLinkExpired)) {
		if err := s.atCapacity(refusalKey(c)); err != nil {
			s.confirmRefused(ctx, c, id, err)
			s.refusedRound(ctx, w, err, timeout)
			return
		}
	}
	c, l, ok := s.loopLink(w, r, tok)
	if !ok {
		return
	}
	untilHooks, untilWait := q.Get("until") == "hooks", q.Get("until") == "wait"
	claude, err := s.claudeSession(ctx, c.Tenant, l)
	if err != nil {
		slog.Error("streams: reading a wait loop's session", "err", err)
		w.Header().Set("Retry-After", "2")
		s.loopRetry(ctx, w)
		return
	}
	if tp == "" {
		timeout = DefaultWaitTimeout
		if claude {
			timeout = jitterDown(LoopRound) // rounds that started together end apart
		}
	}
	if q.Get("until") == "" && claude {
		untilHooks = true
	}
	// The loop's last round's batch, first: what's done below sees it
	// confirmed.
	st, err := s.confirmLoop(ctx, c.Tenant, l, id)
	if err != nil {
		slog.Error("streams: confirming a wait loop's batch", "err", err)
		w.Header().Set("Retry-After", "2")
		s.loopRetry(ctx, w)
		return
	}
	// Closed before it starts: said with the status, so the loop ends.
	if why, done := s.waitDone(ctx, c.Tenant, l); done {
		s.loopRefuse(ctx, w, http.StatusGone, why)
		return
	}
	if untilHooks {
		if hooks, err := s.hooksConnected(ctx, c.Tenant, l); err != nil {
			slog.Warn("streams: checking a session's hooks failed", "err", err)
		} else if hooks {
			s.loopRefuse(ctx, w, http.StatusGone, Ending{"hooks", HooksConnected})
			return
		}
	}
	// A Claude Code loop ends itself within LoopTimeout of its start,
	// before Claude Code stops its Monitor mid-round.
	if claude && st.mine && !st.since.IsZero() {
		left := LoopTimeout - time.Since(st.since)
		if left <= 0 {
			s.pause(ctx)
			writeLine(w, http.StatusGone, loopRestart)
			return
		}
		timeout = min(timeout, left)
	}
	valid := func(ctx context.Context, tx pgx.Tx) error {
		_, err := loadTx(ctx, tx, l.ID, true)
		if errors.Is(err, errLinkInvalid) {
			return ErrRevoked
		}
		return err
	}
	// The 200 goes out once the round holds the stream: until then, a
	// refusal can still be a status.
	var t *httpTransport
	start := func() {
		if t == nil {
			t = newHTTPTransport(w, FormatLines)
		}
	}
	var holder string
	wl := &waitLoop{id: id}
	batch, err := s.longPoll(ctx, PollRequest{Tenant: c.Tenant, Agent: l.Agent, Target: l.Target, Valid: valid, Wait: timeout, Connector: l.Connector},
		pollOpts{wait: true, holder: &holder, untilHooks: untilHooks, untilWait: untilWait, loop: wl, taken: start, app: linkApp(l),
			keepalive: func() error { start(); return t.ping(ctx) }})
	s.admittedAs(refusalKey(c), err)
	// The link is in use: it slides, so the next round still finds it
	// working after a round as long as the link's TTL.
	defer func() {
		bg := context.WithoutCancel(ctx)
		if err := s.DB.InTenant(bg, c.Tenant, func(tx pgx.Tx) error { return slide(bg, tx, &l) }); err != nil {
			slog.Warn("streams: extending a wait loop's link failed", "err", err)
		}
	}()
	var installed *WaitInstalled
	switch {
	case ctx.Err() != nil:
		return
	case err == nil:
	case t != nil:
		// The 200 is out: end it with nothing more. The next round says
		// why, with the status (or carries on, if it can).
		if errors.Is(err, ErrRevoked) {
			return
		}
		if !errors.Is(err, ErrReplaced) && !errors.Is(err, ErrDraining) && !errors.Is(err, errHooksConnected) && !errors.As(err, &installed) {
			slog.Warn("streams: a wait loop's round failed", "err", err)
		}
		return
	case errors.Is(err, errLoopReplaced):
		s.loopRefuse(ctx, w, http.StatusGone, loopReplaced)
		return
	case errors.As(err, &installed):
		if _, err := s.hooksConnected(ctx, c.Tenant, l); err != nil {
			slog.Warn("streams: checking a session's hooks failed", "err", err)
		}
		s.loopRefuse(ctx, w, http.StatusGone, Ending{"wait", WaitInstalledNote(installed.Command)})
		return
	case errors.Is(err, errHooksConnected):
		if _, err := s.hooksConnected(ctx, c.Tenant, l); err != nil {
			slog.Warn("streams: checking a session's hooks failed", "err", err)
		}
		s.loopRefuse(ctx, w, http.StatusGone, Ending{"hooks", HooksConnected})
		return
	case errors.Is(err, errWebhookBound):
		s.loopRefuse(ctx, w, http.StatusGone, Ending{"webhook", WebhookNote})
		return
	case errors.Is(err, ErrRevoked):
		s.loopRefuse(ctx, w, http.StatusGone, s.loopEnding(ctx, c))
		return
	case errors.Is(err, ErrDraining):
		// A 200 with nothing, after a pause: the loop goes round, likely to
		// another instance.
		s.pause(ctx)
		start()
		return
	case errors.Is(err, errBusy):
		// Full since the check above: as there.
		s.refusedRound(ctx, w, err, timeout)
		return
	default:
		slog.Warn("streams: a wait loop's round failed", "err", err)
		w.Header().Set("Retry-After", "2")
		s.loopRetry(ctx, w)
		return
	}
	start()
	for _, f := range batch.Deliveries {
		if err := t.send(ctx, f, f.DeliveryID); err != nil {
			return // they stay leased, unconfirmed, and come again
		}
	}
	if len(batch.Deliveries) == 0 {
		return // nothing arrived (or the session ended: the next round says so)
	}
	// Written in full: the loop's next round confirms it.
	bg := context.WithoutCancel(ctx)
	if err := s.DB.InTenant(bg, c.Tenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(bg, `UPDATE stream_leases SET loop_flushed = $4 WHERE agent_id = $1 AND target = $2 AND loop_id = $3 AND holder = $4`,
			l.Agent, l.Target, id, holder)
		return err
	}); err != nil {
		slog.Warn("streams: recording a wait loop's batch failed", "err", err) // it comes again
	}
}

// loopRefuse answers a loop's round that can't go on with status and the
// hub.closed line saying why, after a pause: a loop that goes round on a
// refusal anyway (written without --fail-with-body, or by hand) can't
// spin.
func (s *Server) loopRefuse(ctx context.Context, w http.ResponseWriter, status int, why Ending) {
	s.pause(ctx)
	writeLine(w, status, closedFrame(why))
}

// loopRetry answers a round the server couldn't serve (a database error)
// with a 200 and nothing, after the pause: the loop goes round, rather than
// ending on an error status as a refusal would. Nothing was handed out, so
// the next round has nothing to confirm.
func (s *Server) loopRetry(ctx context.Context, w http.ResponseWriter) {
	s.pause(ctx)
	writeEmpty(w)
}

// writeEmpty answers a loop's round with a 200 and nothing: it goes round.
func writeEmpty(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("\n"))
}

// refusedRound answers a loop's round refused at capacity: held for the
// refusal's wait (holdRefused), then a 200 with nothing, as a round that
// had nothing to hand out. Not an error status, which would end the loop,
// and not an answer at once, which it would come straight back from.
func (s *Server) refusedRound(ctx context.Context, w http.ResponseWriter, err error, limit time.Duration) {
	s.holdRefused(ctx, err, limit)
	writeEmpty(w)
}

// confirmRefused confirms what the loop's last round printed, on the first
// of a run of refused rounds only (busyError.first), in one transaction:
// the last round's lease stays live only a minute, less than a refusal's
// hold, so the round that gets in after a refusal couldn't confirm it any
// more, and it would come again. The refused rounds after the first have
// nothing to confirm (none handed anything out), and do no database work.
func (s *Server) confirmRefused(ctx context.Context, c claims, id string, refusal error) {
	var b busyError
	if !errors.As(refusal, &b) || !b.first {
		return
	}
	err := s.DB.InTenant(ctx, c.Tenant, func(tx pgx.Tx) error {
		// Only a conversation's own wait link confirms a loop's batch, as
		// loopLink requires of a round that gets in (loopKind).
		var loop bool
		if err := tx.QueryRow(ctx, loopKindSQL, c.ID).Scan(&loop); err != nil || !loop {
			return nil // gone, or not a loop's link: the next round says why
		}
		l, err := loadTx(ctx, tx, c.ID, true)
		if err != nil {
			return nil // gone or revoked: nothing of its to confirm; the next round says why
		}
		_, err = confirmLoopTx(ctx, tx, l, id)
		return err
	})
	if err != nil && ctx.Err() == nil {
		slog.Warn("streams: confirming a refused wait loop's last batch failed", "err", err)
	}
}

// refusalKey is what a wait's or loop's refusals are counted by before its
// link is loaded: the link's stored ID, the same every round.
func refusalKey(c claims) string { return c.Tenant + ":link:" + c.ID }

// holdRefused holds a wait or a loop's round refused at capacity for the
// refusal's wait (busyError: 15s, then 30s, then 60s, each spread up to half
// again), at most limit (0: no limit), doing no database work. Its client
// then comes back no sooner however fast it goes round, and the request
// held meanwhile counts toward the instance's concurrency, which is what
// the platform scales out on. It ends early if the request ends or the
// server drains. With LoopPause negative (tests), it doesn't hold.
func (s *Server) holdRefused(ctx context.Context, err error, limit time.Duration) {
	if s.LoopPause < 0 {
		return
	}
	t := time.NewTimer(refusalHold(err, limit, or(s.LoopPause, 2*time.Second)))
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-s.drainCh():
	case <-t.C:
	}
}

// refusalHold is how long holdRefused holds: the refusal's wait, at most
// limit (if set), and at least floor.
func refusalHold(err error, limit, floor time.Duration) time.Duration {
	d := refusalWaits[0]
	var b busyError
	if errors.As(err, &b) {
		d = b.after
	}
	if limit > 0 {
		d = min(d, limit)
	}
	return max(d, floor)
}

// pause waits LoopPause (default 2s) before a loop's refusal or empty
// answer, unless the request ends first.
func (s *Server) pause(ctx context.Context) {
	d := or(s.LoopPause, 2*time.Second)
	if s.LoopPause < 0 {
		return
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// loopEnding is why a loop's link can't go on (endingOf), in loop mode's
// words: a renewed link it couldn't follow has expired.
func (s *Server) loopEnding(ctx context.Context, c claims) Ending {
	why := s.endingOf(ctx, c, false)
	if why.Reason == endingRenewed.Reason {
		why = loopExpired
	}
	return why
}

// loopKind reports whether c's stored link is one loop mode serves: an
// agent's own session link (sliding, without acks, not a connector's).
// A link that's gone reports true: it's refused as gone, below.
// loopKindSQL is whether a link ($1) is a conversation's own wait link:
// the only kind a loop may wait on, or confirm a batch with.
const loopKindSQL = `SELECT NOT ack AND connector IS NULL AND slide_until IS NOT NULL FROM stream_links WHERE id = $1`

func (s *Server) loopKind(ctx context.Context, c claims) (bool, error) {
	ok := true
	err := s.DB.InTenant(ctx, c.Tenant, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, loopKindSQL, c.ID).Scan(&ok)
		if errors.Is(err, pgx.ErrNoRows) {
			ok = true
			return nil
		}
		return err
	})
	return ok, err
}

// loopLink is the link a loop's round waits on: the one its token names,
// or, if a renewal replaced that one (successor_id), the newest of its
// chain, renewed there and then if it expired while nothing used it (the
// grace path). Each is loaded (load: checked, marked used, slid) as a
// wait's link is. Only an agent's own session link (loopKind): a
// connector's link (which the hooks keep, and whose holder follows its
// rotations itself) gets 400. false: the round was answered (401 for a
// token that isn't a link, 410 for one that can't go on).
func (s *Server) loopLink(w http.ResponseWriter, r *http.Request, tok string) (claims, Link, bool) {
	ctx := r.Context()
	c, err := s.parse(tok)
	if tok == "" {
		// The command read nothing from its link file: missing, empty, or
		// written somewhere else ($HOME not expanded).
		s.loopRefuse(ctx, w, http.StatusUnauthorized, loopNoLinkFile)
		return c, Link{}, false
	}
	if err != nil && !errors.Is(err, errLinkExpired) {
		s.pause(ctx)
		writeLine(w, http.StatusUnauthorized, closedFrame(Ending{"invalid", "This isn't a working Wakeshift link, so this command can't wait for messages. Nothing more to do here."}))
		return c, Link{}, false
	}
	if ok, kerr := s.loopKind(ctx, c); kerr != nil {
		return c, Link{}, s.loopLoadFailed(ctx, w, kerr)
	} else if !ok {
		s.loopRefuse(ctx, w, http.StatusBadRequest, Ending{"bad_request", "loop= is only for a conversation's own wait link."})
		return c, Link{}, false
	}
	at := c
	if err == nil {
		l, err := s.load(ctx, c, r, true)
		if err == nil {
			return c, l, true
		}
		if !errors.Is(err, errLinkInvalid) {
			return c, Link{}, s.loopLoadFailed(ctx, w, err)
		}
	}
	// Expired or replaced: follow the chain from it, within RenewGrace of
	// its token's expiry (as long as a renewal of it could have come).
	if time.Since(time.Unix(c.Exp, 0)) > RenewGrace {
		s.loopRefuse(ctx, w, http.StatusGone, s.loopEnding(ctx, c))
		return c, Link{}, false
	}
	newest, err := s.newestOf(ctx, c)
	if err != nil {
		return c, Link{}, s.loopLoadFailed(ctx, w, err)
	}
	if newest.ID != c.ID {
		l, err := s.load(ctx, newest, r, true)
		if err == nil {
			return newest, l, true
		}
		if !errors.Is(err, errLinkInvalid) {
			return c, Link{}, s.loopLoadFailed(ctx, w, err)
		}
		at = newest
	}
	// The newest expired while nothing used it: renew it (single use, as
	// any renewal), and wait on its successor.
	next, _, _, err := s.renewExpired(ctx, at, r.Header.Get(connectorversion.Header), true)
	switch {
	case errors.Is(err, errNoGrace):
		s.loopRefuse(ctx, w, http.StatusGone, s.loopEnding(ctx, at))
		return c, Link{}, false
	case err != nil:
		return c, Link{}, s.loopLoadFailed(ctx, w, err)
	}
	nc, err := s.parse(linkOf(next))
	if err != nil {
		return c, Link{}, s.loopLoadFailed(ctx, w, err)
	}
	l, err := s.load(ctx, nc, r, true)
	if err != nil {
		if errors.Is(err, errLinkInvalid) {
			s.loopRefuse(ctx, w, http.StatusGone, s.loopEnding(ctx, nc))
			return c, Link{}, false
		}
		return c, Link{}, s.loopLoadFailed(ctx, w, err)
	}
	return nc, l, true
}

// loopLoadFailed answers a round whose link couldn't be loaded: false.
func (s *Server) loopLoadFailed(ctx context.Context, w http.ResponseWriter, err error) bool {
	if s.writeUpdateRequired(w, err) {
		return false
	}
	slog.Error("streams: loading a wait loop's link", "err", err)
	w.Header().Set("Retry-After", "2")
	s.loopRetry(ctx, w)
	return false
}

// maxChain bounds following a chain of renewals: one a link, renewed at
// most every SessionLinkTTL, can't be longer within its RenewGrace.
const maxChain = int(2*RenewGrace/SessionLinkTTL) + 2

// newestOf is the newest link of c's chain of renewals (successor_id), as
// claims for it (c's own if nothing replaced it), in c's tenant.
func (s *Server) newestOf(ctx context.Context, c claims) (claims, error) {
	out := c
	err := s.DB.InTenant(ctx, c.Tenant, func(tx pgx.Tx) error {
		var id string
		var exp time.Time
		err := tx.QueryRow(ctx, `WITH RECURSIVE chain (id, successor_id, depth) AS (
				SELECT id, successor_id, 0 FROM stream_links WHERE id = $1
				UNION ALL
				SELECT k.id, k.successor_id, c.depth + 1 FROM stream_links k JOIN chain c ON k.id = c.successor_id WHERE c.depth < $2
			)
			SELECT k.id, coalesce(k.slide_until, k.expires_at) FROM chain c JOIN stream_links k ON k.id = c.id ORDER BY c.depth DESC LIMIT 1`,
			c.ID, maxChain).Scan(&id, &exp)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		out = claims{Tenant: c.Tenant, ID: id, Exp: exp.Unix()}
		return nil
	})
	return out, err
}

// claudeSession reports whether a link's session is Claude Code's.
func (s *Server) claudeSession(ctx context.Context, tenant string, l Link) (bool, error) {
	if !uuidRE.MatchString(l.Target) {
		return false, nil
	}
	var claude bool
	err := s.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `SELECT coalesce(harness = 'claude-code', false) FROM sessions WHERE id = $1::uuid`, l.Target).Scan(&claude)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return err
	})
	return claude, err
}

// linkOf is a stream URL's token.
func linkOf(stream string) string {
	u, err := url.Parse(stream)
	if err != nil {
		return ""
	}
	return u.Query().Get("link")
}
