package streams

// Long polling: a stream taken in batches, for clients that can make an
// HTTP request but can't hold a WebSocket (hooks, CLIs, SDKs, wait tools).
//
//	GET  /v1/stream/poll?wait=25s&limit=20&ack=<delivery_id>…
//	POST /v1/stream/ack   {"delivery_ids": ["…"]}
//
// authenticated with the same signed link the WebSocket uses: its token
// (the link= value of the stream URL) as "Authorization: Bearer <token>"
// (preferred) or as ?link=. A poll answers as soon as the stream has
// deliveries, or after wait with none:
//
//	{"deliveries": [Frame…], "acked": ["…"], "session": "codex#2",
//	 "rotate": {"link": "…", "url": "wss://…", "expires_at": "…"}}
//
// It's the stream's rules in batches:
//   - At least once, always (whatever the link's ack setting: a poller can
//     always ack). Deliveries a poll returns stay leased until acked, with
//     ack= on the next poll or POST /v1/stream/ack; acked lists the ones
//     now recorded. A poll that doesn't ack what the previous one returned
//     gets it again (attempt+1), as a reconnecting stream would; so does
//     anyone polling after the lease expires.
//   - One poller (or connection) holds a stream at a time, the newest: a
//     poll takes it, and an older poll still waiting answers 409 replaced
//     (a WebSocket on it closes with 4000).
//   - A revoked link answers 401 link_revoked, at once if it's revoked
//     while a poll waits. An expired one answers 401 link_expired, unless
//     it's a session's link that expired while nothing polled it (a laptop
//     asleep), within a day: then it's renewed, once (grace.go), and the
//     poll answers at once with no deliveries and the successor in rotate.
//   - A session counts as connected while polls keep coming: during a poll,
//     and for a minute after the last one.
//   - A session's link rotates: within a few minutes of its expiry a poll's
//     answer carries the next link (same session, same position) in
//     rotate; use it from then on.
//   - A poll doesn't take the stream from the agent's live wait command
//     (wait.go), which wakes the conversation itself. It waits instead,
//     and takes the stream as soon as the wait stops counting as live (its
//     command died: what it took comes to the poll); if the wait is still
//     live when wait runs out, it answers with nothing and "background":
//     true. With background=stop it answers that at once (a stop hook
//     that shouldn't hold the turn open). A long-lived stream (lines with
//     max=) never makes it stand down: behind a front end, one whose
//     client died can look live for many minutes.
//
// A waiting poll holds no database connection: it waits on the notify hub,
// as the stream does (with the same slow safety poll and paced timer).

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/connectorversion"
	"github.com/wakeshift/server/internal/deliveries"
	"github.com/wakeshift/server/internal/httpx"
)

// Poll limits.
const (
	MaxPollWait      = 55 * time.Second // under common proxy and tool-call timeouts
	DefaultPollWait  = 25 * time.Second // under Copilot CLI's 30s tool timeout
	MaxPollBatch     = 100
	DefaultPollBatch = 20
	MaxPollAcks      = 100
)

// Why a poll ended without a batch.
var (
	// ErrReplaced: a newer poll or connection took the stream.
	ErrReplaced = errDisplaced
	// ErrRevoked: the poller may no longer read the stream (its link or
	// grant was revoked or expired).
	ErrRevoked = errors.New("streams: no longer allowed to read this stream")
	// ErrStreamConnected: a live WebSocket holds the stream, and the poll
	// asked to yield to one (PollRequest.Yield).
	ErrStreamConnected = errors.New("streams: a live connection delivers this stream")
	// ErrDraining: the server is restarting; poll again.
	ErrDraining = errDraining
)

// PollRequest is one poll of a stream (an agent's, or one of its
// sessions').
type PollRequest struct {
	Tenant, Agent, Target string
	// Valid checks, in the tenant, that the poller may still read the
	// stream, returning ErrRevoked if not. It's checked before anything is
	// acked or handed out.
	Valid func(ctx context.Context, tx pgx.Tx) error
	Acks  []string // recorded first
	Wait  time.Duration
	Limit int
	// Yield: don't take the stream from a live WebSocket (ErrStreamConnected).
	Yield bool
	// Connector, if set, is the connector polling (Link.Connector): what
	// it's handed records it (deliveries.HandedTo).
	Connector string
	// App, if set, is the app polling for the agent (its wait tool): it
	// takes and acknowledges only what its own subscriptions delivered
	// (deliveries.OfApp). The agent's own links leave it empty.
	App string
}

// Batch is what a poll returns.
type Batch struct {
	Deliveries []Frame `json:"deliveries"`
	// Acked are the acks sent with the poll that are recorded (repeats
	// included); others weren't this stream's.
	Acked []string `json:"acked"`
	// Session is the stream's session handle (codex#2), if it's a session's.
	Session string `json:"session,omitempty"`
}

var uuidRE = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// ackInOrder acks the delivery IDs in ids worth trying (UUIDs) with ack,
// each once, in one order whatever order they came in: acking locks each
// delivery's row, so two requests acking overlapping IDs lock them in the
// same order and can't deadlock. It returns the ones recorded as they were
// listed (repeats included).
func ackInOrder(ids []string, ack func(id string) (bool, error)) ([]string, error) {
	order := make([]string, 0, len(ids))
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		key := strings.ToLower(id)
		if !uuidRE.MatchString(id) || seen[key] {
			continue
		}
		seen[key] = true
		order = append(order, key)
	}
	slices.Sort(order)
	recorded := make(map[string]bool, len(order))
	for _, key := range order {
		ok, err := ack(key)
		if err != nil {
			return nil, err
		}
		recorded[key] = ok
	}
	acked := []string{}
	for _, id := range ids {
		if recorded[strings.ToLower(id)] {
			acked = append(acked, id)
		}
	}
	return acked, nil
}

// LongPoll records p's acks, takes the stream, and returns its next deliveries,
// waiting up to p.Wait for the first. What it returns stays leased to it
// until acked; the next poll (anyone's) takes back what isn't.
func (s *Server) LongPoll(ctx context.Context, p PollRequest) (Batch, error) {
	return s.longPoll(ctx, p, pollOpts{})
}

// pollOpts are how a one-shot wait (wait.go) or a hook's poll differs from
// a plain poll.
type pollOpts struct {
	// wait: a one-shot wait. Its holder is a wait's (isWait), it may wait
	// up to MaxWaitTimeout, and what it returns stays leased for WaitLease.
	wait bool
	// holder, if set, is given the wait's holder (its batch's name).
	holder *string
	// keepalive is called every keepalive interval while it waits.
	keepalive func() error
	// yieldBackground: don't take the stream from a live wait (isWait). A
	// hook's poll mustn't stop the command that wakes the conversation.
	yieldBackground bool
	// patient, with yieldBackground: wait for the wait to stop being live
	// (until the poll's wait runs out), then take the stream.
	patient bool
	// untilHooks: a wait that ends once Wakeshift's hooks connect the
	// session (errHooksConnected, taking nothing).
	untilHooks bool
	// untilWait: a wait that ends once Wakeshift's hooks connect the
	// session with a wait command (a *WaitInstalled, taking nothing).
	untilWait bool
	// fixedHolder, with wait, is the wait's holder instead of a random one:
	// an MCP wait task's, the same for every round of the task (TaskWait).
	fixedHolder string
	// loop, with wait: a wait loop's round (waitloop.go). It may refuse the
	// round or confirm the loop's last batch as it takes the stream.
	loop *waitLoop
	// taken, if set, is called once the stream is taken, before waiting.
	taken func()
}

// WaitInstalled: an until=wait wait found the session's hooks connected
// with Wakeshift's wait command, Command, which the agent runs instead.
type WaitInstalled struct{ Command string }

func (e *WaitInstalled) Error() string { return "streams: the session has a wait command" }

// waitCommandOf is the wait command of a session Wakeshift's hooks
// connected ("" if none, or they haven't).
func waitCommandOf(ctx context.Context, tx pgx.Tx, target string) (string, error) {
	var cmd string
	err := tx.QueryRow(ctx, `SELECT coalesce(wait_command, '') FROM sessions WHERE id = $1::uuid AND runtime = 'stream' AND connector_seen_at IS NOT NULL`,
		target).Scan(&cmd)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return cmd, err
}

// errHooksConnected: an until=hooks wait found the session's hooks
// connected as it took the stream.
var errHooksConnected = errors.New("streams: the session's hooks are connected")

// ErrHooksConnected is what TaskWait returns once Wakeshift's hooks have
// connected the session: they deliver to it instead, and nothing was taken.
var ErrHooksConnected = errHooksConnected

// TaskWait is one round of an MCP wait task (internal/appapi, tasks.go):
// a one-shot wait, as a Monitor's round is (until=hooks), under holder, the
// task's own lease holder (waittask.Codec.Holder), so every round of the
// task, on any instance, takes the stream as the same holder, and what it
// hands out is the task's batch, found again by its holder
// (deliveries.HeldBy). What it returns stays leased for WaitLease, until
// the next task confirms it (deliveries.AckHolder) or something else takes
// the stream. It counts as connected while it waits and for WaitGrace
// after; ErrReplaced if a newer task (or anything else) takes the stream.
func (s *Server) TaskWait(ctx context.Context, p PollRequest, holder string) (Batch, error) {
	if !isWait(holder) {
		return Batch{}, errors.New("streams: a task's holder must be a wait's")
	}
	return s.longPoll(ctx, p, pollOpts{wait: true, fixedHolder: holder, untilHooks: true})
}

func (s *Server) longPoll(ctx context.Context, p PollRequest, o pollOpts) (_ Batch, err error) {
	out := Batch{Deliveries: []Frame{}, Acked: []string{}}
	if err := s.enter(p.Tenant + ":" + p.Agent + ":" + p.Target); err != nil {
		return out, err
	}
	defer s.leave()
	limit := p.Limit
	if limit <= 0 || limit > MaxPollBatch {
		limit = DefaultPollBatch
	}
	wait := min(max(p.Wait, 0), MaxPollWait)
	holder, lease := holderID("poll_"), s.lease()
	if o.wait {
		wait, holder, lease = min(max(p.Wait, 0), MaxWaitTimeout), holderID(waitPrefix), s.waitLease()
		if o.fixedHolder != "" {
			holder = o.fixedHolder
		}
	}
	st := &stream{s: s, claims: claims{Tenant: p.Tenant}, l: Link{Agent: p.Agent, Target: p.Target}, holder: holder, yield: p.Yield,
		yieldBackground: o.yieldBackground}
	if o.holder != nil {
		*o.holder = holder
	}
	// A revoked link ends presence at once, even if a renewal raced the
	// revocation: what read it can't read it again. So does a wait whose
	// client went away, wherever it noticed: waiting, or in a transaction
	// (a read on a tick or wake, a renewal), which then fails with the
	// request's context; else the session would count as connected for up
	// to a minute after.
	defer func() {
		if errors.Is(err, ErrRevoked) || (o.wait && ctx.Err() != nil) {
			st.absent(ctx)
		}
	}()

	// Wait before taking the lease, so no notification is missed between.
	key := p.Tenant + ":" + p.Agent
	wake, stopWake := s.Notify.Wait(deliveries.NotifyChannel, key)
	defer stopWake()
	replaced, stopReplaced := s.Notify.Wait(TakeoverChannel, st.holder)
	defer stopReplaced()
	st.replaced = replaced
	revoked, stopRevoked := s.Notify.Wait(RevokedChannel, key)
	defer stopRevoked()

	deadline := time.Now().Add(wait)
	var batch []Frame
	var next time.Time
	var last bool
	// claim takes what's waiting, inside a transaction holding the stream.
	claim := func(ctx context.Context, tx pgx.Tx) (err error) {
		batch, next, last = nil, time.Time{}, !time.Now().Before(deadline)
		claimed, err := deliveries.ClaimFor(ctx, tx, p.Agent, p.Target, p.App, st.holder, lease, limit)
		if err != nil {
			return err
		}
		if err := deliveries.HandedTo(ctx, tx, claimed, p.Connector); err != nil {
			return err
		}
		if batch, err = framesOf(ctx, tx, claimed); err != nil {
			return err
		}
		if len(batch) > 0 || last {
			if o.wait {
				// Present for WaitGrace after this wait: its command runs it
				// again at once (a loop) or when it's woken.
				return st.presentFor(ctx, tx, s.waitGrace())
			}
			// Present until a minute after this poll.
			return st.present(ctx, tx)
		}
		next, err = deliveries.NextAvailableFor(ctx, tx, p.Agent, p.Target, p.App)
		return err
	}
	// One transaction: the checks and acks, the lease, the first claim. A
	// patient poll that yields tries again when the holder stops counting
	// as live, until its deadline.
	for {
		err := st.takeLease(ctx, func(ctx context.Context, tx pgx.Tx) error {
			if err := p.Valid(ctx, tx); err != nil {
				return err
			}
			if uuidRE.MatchString(p.Target) { // compared as a uuid, so the primary key serves it
				if err := tx.QueryRow(ctx, `SELECT handle FROM sessions WHERE id = $1::uuid`, p.Target).Scan(&out.Session); err != nil && !errors.Is(err, pgx.ErrNoRows) {
					return err
				}
			}
			// Acks first, so what they confirm isn't handed out again below.
			acked, err := ackInOrder(p.Acks, func(id string) (bool, error) { return deliveries.AckFor(ctx, tx, p.Agent, p.Target, p.App, id) })
			out.Acked = append(out.Acked, acked...)
			if err != nil || o.loop == nil {
				return err
			}
			return o.loop.before(ctx, tx, p.Agent, p.Target)
		}, func(ctx context.Context, tx pgx.Tx) error {
			if o.loop != nil {
				if err := o.loop.after(ctx, tx, p.Agent, p.Target); err != nil {
					return err
				}
			}
			// Checked in the transaction that takes the stream, after take
			// has updated the session's row: the hooks' redeem changes that
			// row and takes over (TakeOver) in one transaction, so this
			// waits for it and sees it. A wait that started just before
			// they connected doesn't take the stream back after their
			// takeover, or the hooks would stand down for it. It takes
			// nothing (the rollback). The hooks count only once their
			// waiter has polled (HooksDeliver): until then a wait takes the
			// stream back after the redeem's takeover, since a redeem with
			// no waiter (claude -p, a one-shot SDK query) wakes nothing.
			if o.untilHooks && uuidRE.MatchString(p.Target) {
				hooks, err := HooksDeliver(ctx, tx, p.Target)
				if err != nil {
					return err
				}
				if hooks {
					return errHooksConnected
				}
			}
			// A Grok Bot's foreground wait stops once its webhook is
			// bound (webhookwait.go).
			if o.wait && s.CursorWebhooks {
				if bound, err := webhookBound(ctx, tx, p.Target); err != nil || bound {
					if err == nil {
						err = errWebhookBound
					}
					return err
				}
			}
			if o.untilWait && uuidRE.MatchString(p.Target) {
				cmd, err := waitCommandOf(ctx, tx, p.Target)
				if err != nil {
					return err
				}
				if cmd != "" {
					return &WaitInstalled{Command: cmd}
				}
			}
			return claim(ctx, tx)
		})
		if err == nil {
			break
		}
		if errors.Is(err, errHooksConnected) {
			st.waitsGone(ctx)
		}
		if !errors.Is(err, ErrStreamConnected) || !o.patient || !time.Now().Before(deadline) {
			return out, err
		}
		// A pending wait renews its presence as it waits, so it may end
		// (and its grace run out) well before the time it looks live until:
		// look again at least every Poll (at most 5s), and when a delivery
		// is announced (the wait may have just taken it).
		if !st.waitOut(ctx, earliest(earliest(deadline, st.yieldUntil), time.Now().Add(min(s.poll(), 5*time.Second))), revoked, wake) {
			return out, err
		}
		out.Acked = out.Acked[:0] // recorded again: acks are idempotent
	}
	if o.taken != nil {
		o.taken()
	}

	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	// One check every Poll (or Renew, if sooner), and the stream's
	// presence renewed in that check's transaction once Renew is (about)
	// due: an idle poll or wait costs one transaction a renewal, not a
	// claim and a renewal each in their own (finding 5 of the scalability
	// review: two transactions every 30 s per waiting conversation).
	tick := time.NewTicker(min(s.poll(), s.renew()))
	defer tick.Stop()
	presentAt := time.Now() // taking the stream marked it connected
	paced := time.NewTimer(time.Hour)
	paced.Stop()
	defer paced.Stop()
	var keepalive <-chan time.Time
	if o.keepalive != nil {
		k := time.NewTicker(s.waitKeepalive())
		defer k.Stop()
		keepalive = k.C
	}
	for first := true; ; first = false {
		if !first {
			if err := st.interrupted(); errors.Is(err, errDraining) {
				return out, nil // nothing's held: poll again (elsewhere)
			} else if err != nil {
				return out, err
			}
			presented := false
			err := st.tx(ctx, func(tx pgx.Tx) error {
				if err := st.holding(ctx, tx); err != nil {
					return err
				}
				if err := p.Valid(ctx, tx); err != nil {
					return err
				}
				if presenceDue(presentAt, s.renew()) {
					if err := st.present(ctx, tx); err != nil {
						return err
					}
					presented = true
				}
				// The hooks' waiter may first poll while this wait holds
				// the stream (a patient poll yields to it): it ends then,
				// taking nothing, rather than at the end of its round.
				if o.untilHooks && uuidRE.MatchString(p.Target) {
					if hooks, err := HooksDeliver(ctx, tx, p.Target); err != nil || hooks {
						if err == nil {
							err = errHooksConnected
						}
						return err
					}
				}
				return claim(ctx, tx)
			})
			if errors.Is(err, errHooksConnected) {
				st.waitsGone(ctx) // not live any more: the waiter takes the stream at once
			}
			if err != nil {
				return out, err
			}
			if presented {
				presentAt = time.Now()
			}
		}
		if len(batch) > 0 || last {
			out.Deliveries = append(out.Deliveries, batch...)
			return out, nil
		}
		if !next.IsZero() {
			paced.Reset(max(time.Until(next), 0) + 10*time.Millisecond)
		}
		select {
		case <-ctx.Done():
			return out, ctx.Err() // its client went away: not connected (above)
		case <-s.drainCh():
		case <-replaced:
			return out, ErrReplaced
		case <-revoked: // re-checked above
		case <-keepalive:
			if err := o.keepalive(); err != nil {
				st.absent(ctx) // the client went away
				return out, err
			}
		case <-wake:
		case <-tick.C:
		case <-paced.C:
		case <-timer.C:
		}
	}
}

// waitOut waits, for a poll that yielded, until the time to look again
// whether the holder still counts as live (or the link may have been
// revoked, which the next try checks, or a delivery was announced). false:
// the poll should stop (its client went away, or the server is draining).
func (st *stream) waitOut(ctx context.Context, until time.Time, revoked, wake <-chan struct{}) bool {
	t := time.NewTimer(max(time.Until(until), 0) + 20*time.Millisecond)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-st.s.drainCh():
		return false
	case <-revoked:
	case <-wake:
	case <-t.C:
	}
	return true
}

func earliest(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

// presenceDue reports whether a poll that last marked its stream connected
// at presentAt should mark it again in this check: once Renew has (all but
// a tenth) passed, so a tick that comes a little early still renews.
func presenceDue(presentAt time.Time, renew time.Duration) bool {
	return time.Since(presentAt) >= renew-renew/10
}

// present marks the stream connected now (while this poll holds it).
func (st *stream) present(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, `UPDATE stream_leases SET renewed_at = now() WHERE agent_id = $1 AND target = $2 AND holder = $3`,
		st.l.Agent, st.l.Target, st.holder)
	return err
}

// presentFor marks the stream connected for d more (connected is renewed
// in the last minute).
func (st *stream) presentFor(ctx context.Context, tx pgx.Tx, d time.Duration) error {
	_, err := tx.Exec(ctx, `UPDATE stream_leases SET renewed_at = now() - interval '1 minute' + $4::interval
		WHERE agent_id = $1 AND target = $2 AND holder = $3`, st.l.Agent, st.l.Target, st.holder, min(d, time.Minute).String())
	return err
}

// absent marks the stream not connected (while this poll holds it), even
// after its context ended.
func (st *stream) absent(ctx context.Context) {
	fctx, cancel := finishing(ctx)
	defer cancel()
	if err := st.tx(fctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(fctx, `UPDATE stream_leases SET renewed_at = NULL WHERE agent_id = $1 AND target = $2 AND holder = $3`,
			st.l.Agent, st.l.Target, st.holder)
		return err
	}); err != nil {
		slog.Warn("streams: marking a wait gone failed", "err", err)
	}
}

// waitsGone, once an until=hooks wait finds the hooks delivering (taking
// nothing), ends the presence of any wait holding the session's stream:
// its last round's, which counts as live for WaitGrace after it, or this
// one's. Otherwise the hooks' waiter, whose poll yields to a live wait,
// would leave messages to nothing for that long. A Claude Code session's
// waits are its Monitor's, which the hooks replace (as the redeem's
// TakeOver did when a redeem counted as listening).
func (st *stream) waitsGone(ctx context.Context) {
	fctx, cancel := finishing(ctx)
	defer cancel()
	if err := st.tx(fctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(fctx, `UPDATE stream_leases SET renewed_at = NULL WHERE agent_id = $1 AND target = $2 AND starts_with(holder, $3)`,
			st.l.Agent, st.l.Target, waitPrefix)
		return err
	}); err != nil {
		slog.Warn("streams: ending a replaced wait's presence failed", "err", err)
	}
}

// AckBatch records acks for a stream without polling (and without taking
// the stream), returning the ones recorded: for app (PollRequest.App), only
// its own deliveries.
func (s *Server) AckBatch(ctx context.Context, tenant, agent, target, app string, valid func(context.Context, pgx.Tx) error, ids []string) ([]string, error) {
	acked := []string{}
	err := s.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		if err := valid(ctx, tx); err != nil {
			return err
		}
		var err error
		acked, err = ackInOrder(ids, func(id string) (bool, error) { return deliveries.AckFor(ctx, tx, agent, target, app, id) })
		return err
	})
	return acked, err
}

// Rotation is a session link's successor, in a poll's answer.
type Rotation struct {
	Link      string `json:"link"` // the token, for Authorization: Bearer
	URL       string `json:"url"`  // the stream URL (wss://…/v1/stream?link=…)
	ExpiresAt string `json:"expires_at"`
}

type pollAnswer struct {
	Batch
	Rotate *Rotation `json:"rotate,omitempty"`
	// Background: a background command the agent started holds the stream
	// and wakes the conversation, so the poll took nothing.
	Background bool `json:"background,omitempty"`
}

// linkToken is the link a poll or ack sent: Authorization: Bearer, else
// ?link=. Either may be the whole stream URL.
func linkToken(r *http.Request) string {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		tok = r.URL.Query().Get("link")
	}
	tok = strings.TrimSpace(tok)
	if strings.Contains(tok, "://") {
		if u, err := url.Parse(tok); err == nil {
			tok = u.Query().Get("link")
		}
	}
	return tok
}

// authorize resolves a poll's or ack's link, writing the error if it's no
// good. check: refuse an outdated connector (a poll; an ack is recorded
// whatever the version). renew: a poll's, which renews a session link that
// expired while nothing polled it (renewExpired), answering with its
// successor in rotate and nothing else.
func (s *Server) authorize(w http.ResponseWriter, r *http.Request, check, renew bool) (claims, Link, bool) {
	tok := linkToken(r)
	if tok == "" {
		httpx.WriteError(w, http.StatusUnauthorized, "invalid_link", "send the stream link's token as Authorization: Bearer (or as ?link=)")
		return claims{}, Link{}, false
	}
	c, err := s.parse(tok)
	if errors.Is(err, errLinkExpired) {
		if !renew || !s.renewPoll(w, r, c, check) {
			httpx.WriteError(w, http.StatusUnauthorized, "link_expired", err.Error())
		}
		return c, Link{}, false
	}
	if err != nil {
		httpx.WriteError(w, http.StatusUnauthorized, "invalid_link", err.Error())
		return c, Link{}, false
	}
	l, err := s.load(r.Context(), c, r, check)
	if errors.Is(err, errLinkInvalid) {
		if !renew || !s.renewPoll(w, r, c, check) {
			httpx.WriteError(w, http.StatusUnauthorized, "link_revoked", errLinkInvalid.Error())
		}
		return c, l, false
	}
	if s.writeUpdateRequired(w, err) {
		return c, l, false
	}
	if err != nil {
		slog.Error("streams: loading link", "err", err)
		httpx.WriteError(w, http.StatusServiceUnavailable, "unavailable", "try again")
		return c, l, false
	}
	return c, l, true
}

// renewPoll answers a poll on an expired session link that can be renewed
// (renewExpired) with its successor in rotate and no deliveries, as a poll
// near its link's end would have; false, having written nothing, if it
// can't be (the caller refuses it as before).
func (s *Server) renewPoll(w http.ResponseWriter, r *http.Request, c claims, check bool) bool {
	next, exp, _, err := s.renewExpired(r.Context(), c, r.Header.Get(connectorversion.Header), check)
	switch {
	case errors.Is(err, errNoGrace):
		return false
	case s.writeUpdateRequired(w, err):
		return true
	case err != nil:
		slog.Error("streams: renewing an expired link", "err", err)
		w.Header().Set("Retry-After", "2")
		httpx.WriteError(w, http.StatusServiceUnavailable, "unavailable", "try again")
		return true
	}
	u, _ := url.Parse(next)
	httpx.WriteJSON(w, http.StatusOK, pollAnswer{Batch: Batch{Deliveries: []Frame{}, Acked: []string{}},
		Rotate: &Rotation{Link: u.Query().Get("link"), URL: next, ExpiresAt: exp.UTC().Format(time.RFC3339)}})
	return true
}

// linkValid is a link's PollRequest.Valid.
func linkValid(id string) func(context.Context, pgx.Tx) error {
	return func(ctx context.Context, tx pgx.Tx) error {
		_, err := loadTx(ctx, tx, id, false)
		if errors.Is(err, errLinkInvalid) {
			return ErrRevoked
		}
		return err
	}
}

// ParseWait reads a poll's wait: a duration ("25s") or whole seconds
// ("25"); empty is DefaultPollWait. It's capped at MaxPollWait.
func ParseWait(v string) (time.Duration, error) {
	if v == "" {
		return DefaultPollWait, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		n, nerr := strconv.Atoi(v)
		if nerr != nil {
			return 0, errors.New(`wait is a duration ("25s") or seconds ("25")`)
		}
		d = time.Duration(n) * time.Second
	}
	if d < 0 {
		return 0, errors.New("wait can't be negative")
	}
	return min(d, MaxPollWait), nil
}

// ServePoll serves GET /v1/stream/poll.
func (s *Server) ServePoll(w http.ResponseWriter, r *http.Request) {
	c, l, ok := s.authorize(w, r, true, true)
	if !ok {
		return
	}
	q := r.URL.Query()
	wait, err := ParseWait(q.Get("wait"))
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	limit := DefaultPollBatch
	if v := q.Get("limit"); v != "" {
		if limit, err = strconv.Atoi(v); err != nil || limit < 1 || limit > MaxPollBatch {
			httpx.WriteError(w, http.StatusBadRequest, "bad_request", "limit is 1 to "+strconv.Itoa(MaxPollBatch))
			return
		}
	}
	var acks []string
	for _, v := range q["ack"] {
		for id := range strings.SplitSeq(v, ",") {
			if id = strings.TrimSpace(id); id != "" {
				acks = append(acks, id)
			}
		}
	}
	if len(acks) > MaxPollAcks {
		httpx.WriteError(w, http.StatusBadRequest, "bad_request", "at most "+strconv.Itoa(MaxPollAcks)+" acks a poll")
		return
	}
	batch, err := s.longPoll(r.Context(), PollRequest{Tenant: c.Tenant, Agent: l.Agent, Target: l.Target, Valid: linkValid(l.ID),
		Acks: acks, Wait: wait, Limit: limit, Connector: l.Connector}, pollOpts{yieldBackground: true, patient: q.Get("background") != "stop"})
	background := errors.Is(err, ErrStreamConnected)
	if background {
		// The agent's wait command holds the stream (wait.go) and wakes the
		// conversation, so this poll (a hook's) left it be: with
		// background=stop it says so at once (the hook stops listening);
		// otherwise it waited as long as it was asked to.
		err = nil
	}
	if writePollError(w, r, err) {
		return
	}
	ans := pollAnswer{Batch: batch, Background: background}
	// A session's link: its successor, in time to switch before it expires.
	if l.Target != "" && time.Until(l.expiry()) < s.rotateLead()+MaxPollWait {
		next, exp, err := s.next(r.Context(), c.Tenant, l)
		switch {
		case err == nil:
			u, _ := url.Parse(next)
			ans.Rotate = &Rotation{Link: u.Query().Get("link"), URL: next, ExpiresAt: exp.UTC().Format(time.RFC3339)}
		case !errors.Is(err, errLinkInvalid):
			slog.Warn("streams: rotating a poll's link failed", "err", err)
		}
	}
	httpx.WriteJSON(w, http.StatusOK, ans)
}

// ServeAck serves POST /v1/stream/ack.
func (s *Server) ServeAck(w http.ResponseWriter, r *http.Request) {
	c, l, ok := s.authorize(w, r, false, false)
	if !ok {
		return
	}
	var in struct {
		DeliveryIDs []string `json:"delivery_ids"`
	}
	if !httpx.Decode(w, r, &in) {
		return
	}
	if len(in.DeliveryIDs) == 0 || len(in.DeliveryIDs) > MaxPollAcks {
		httpx.WriteError(w, http.StatusBadRequest, "bad_request", `send {"delivery_ids": [...]}, 1 to `+strconv.Itoa(MaxPollAcks))
		return
	}
	acked, err := s.AckBatch(r.Context(), c.Tenant, l.Agent, l.Target, "", linkValid(l.ID), in.DeliveryIDs)
	if writePollError(w, r, err) {
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"acked": acked})
}

// writePollError writes a poll's (or ack's) error, if there is one.
func writePollError(w http.ResponseWriter, r *http.Request, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, ErrRevoked):
		httpx.WriteError(w, http.StatusUnauthorized, "link_revoked", errLinkInvalid.Error())
	case errors.Is(err, ErrReplaced):
		httpx.WriteError(w, http.StatusConflict, "replaced", "a newer poll or connection took this stream")
	case errors.Is(err, ErrDraining), errors.Is(err, errBusy):
		turnAway(w, err)
	case r.Context().Err() != nil:
		// The client went away.
	default:
		slog.Error("streams: poll failed", "err", err)
		w.Header().Set("Retry-After", "2")
		httpx.WriteError(w, http.StatusServiceUnavailable, "unavailable", "try again")
	}
	return true
}

// HeldBetweenTurns reports, in the transaction's tenant, whether a
// between-turns connector (Codex's: betweenTurns) holds a session's stream
// (agent, target) live: it delivers what comes while the conversation is in
// a turn only once the turn ends, so a wait in the turn takes from it
// (TakeHeld) rather than standing aside.
func HeldBetweenTurns(ctx context.Context, tx pgx.Tx, agent, target string) (bool, error) {
	var held bool
	err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM stream_leases WHERE agent_id = $1 AND target = $2
		AND renewed_at > now() - interval '1 minute' AND holder LIKE 'conn\_turn\_%')`, agent, target).Scan(&held)
	return held, err
}

// TakeHeld is an app's wait (PollRequest.App) for a session whose stream a
// between-turns connector holds (HeldBetweenTurns). The conversation is in
// a turn and asked for its messages, so it takes, without taking the
// stream, what's pending and what the connector holds and hasn't delivered
// (deliveries.ClaimTurnHeld), waiting up to p.Wait for the first. What it
// returns stays leased to it until acked, as a poll's does. The connector
// is told what was taken (NotifyChannel, then its stream's "taken"
// frames), so the same messages don't start a turn after this one; the
// session stays connected through it.
func (s *Server) TakeHeld(ctx context.Context, p PollRequest) (Batch, error) {
	out := Batch{Deliveries: []Frame{}, Acked: []string{}}
	if err := s.enter(p.Tenant + ":" + p.Agent + ":" + p.Target); err != nil {
		return out, err
	}
	defer s.leave()
	limit := p.Limit
	if limit <= 0 || limit > MaxPollBatch {
		limit = DefaultPollBatch
	}
	holder := holderID("poll_take_")
	key := p.Tenant + ":" + p.Agent
	wake, stopWake := s.Notify.Wait(deliveries.NotifyChannel, key)
	defer stopWake()
	revoked, stopRevoked := s.Notify.Wait(RevokedChannel, key)
	defer stopRevoked()
	deadline := time.Now().Add(min(max(p.Wait, 0), MaxPollWait))
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	tick := time.NewTicker(s.poll())
	defer tick.Stop()
	acks := p.Acks
	for {
		var batch []Frame
		last := !time.Now().Before(deadline)
		err := s.DB.InTenant(ctx, p.Tenant, func(tx pgx.Tx) error {
			if err := p.Valid(ctx, tx); err != nil {
				return err
			}
			if out.Session == "" && uuidRE.MatchString(p.Target) {
				if err := tx.QueryRow(ctx, `SELECT handle FROM sessions WHERE id = $1::uuid`, p.Target).Scan(&out.Session); err != nil && !errors.Is(err, pgx.ErrNoRows) {
					return err
				}
			}
			if len(acks) > 0 {
				acked, err := ackInOrder(acks, func(id string) (bool, error) { return deliveries.AckFor(ctx, tx, p.Agent, p.Target, p.App, id) })
				if err != nil {
					return err
				}
				out.Acked = append(out.Acked, acked...)
			}
			claimed, took, err := deliveries.ClaimTurnHeld(ctx, tx, p.Agent, p.Target, p.App, holder, s.lease(), limit)
			if err != nil {
				return err
			}
			if took {
				if _, err := tx.Exec(ctx, `SELECT pg_notify($1, $2)`, deliveries.NotifyChannel, key); err != nil {
					return err
				}
			}
			batch, err = framesOf(ctx, tx, claimed)
			return err
		})
		if err != nil {
			return out, err
		}
		acks = nil
		if len(batch) > 0 || last {
			out.Deliveries = append(out.Deliveries, batch...)
			return out, nil
		}
		select {
		case <-ctx.Done():
			return out, ctx.Err()
		case <-s.drainCh():
			return out, nil // nothing's held: wait again (elsewhere)
		case <-revoked: // re-checked above
		case <-wake:
		case <-tick.C:
		case <-timer.C:
		}
	}
}

// TaskBatch is what an MCP wait task's holder was handed (deliveries.HeldBy),
// as frames: the task's result, found again from its ID alone.
func TaskBatch(ctx context.Context, tx pgx.Tx, agent, target, holder string, limit int) ([]Frame, error) {
	ds, err := deliveries.HeldBy(ctx, tx, agent, target, holder, limit)
	if err != nil || len(ds) == 0 {
		return nil, err
	}
	return framesOf(ctx, tx, ds)
}
