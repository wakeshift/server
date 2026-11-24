package agenthooks

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/db"
	"github.com/wakeshift/server/internal/deliveries"
	"github.com/wakeshift/server/internal/mcpevents"
	"github.com/wakeshift/server/internal/streams"
	"github.com/wakeshift/server/internal/wakegate"
)

// Limits.
const (
	// MaxBody is the longest body sent: Capy hands the agent the first
	// 8,000 characters of a webhook's body, so a message whose whole line
	// doesn't fit goes as its preview (streams.EventLine, marked
	// "truncated"), whole JSON the agent can read to the end, rather than
	// cut off mid-way. Capy refuses bodies over 256 KB (413); this is far
	// under.
	MaxBody = 8000
	// Timeout is a POST's whole budget; connecting gets less.
	Timeout = 10 * time.Second
	// lease is how long a destination (and the deliveries claimed for it)
	// is held while they're sent: perPass sends at Timeout each, spaced,
	// fit in it.
	lease   = 90 * time.Second
	perPass = 5
	// sendSlots is how many destinations an instance sends to at once.
	sendSlots = 16
	// maxBackoff caps the wait between failed attempts.
	maxBackoff = 10 * time.Minute
)

// defaultSpacing and backoffBase are DefaultSpacing's and BackoffBase's
// values, as nanoseconds: atomic, since the deliverer's goroutines read
// them while a test may change them (SetTestTimings).
var defaultSpacing, backoffBase atomic.Int64

func init() {
	defaultSpacing.Store(int64(time.Second))
	backoffBase.Store(int64(15 * time.Second))
}

// DefaultSpacing is Deliverer.Spacing's default.
func DefaultSpacing() time.Duration { return time.Duration(defaultSpacing.Load()) }

// BackoffBase is the wait after a destination's first failure; each next
// one waits twice as long, up to maxBackoff.
func BackoffBase() time.Duration { return time.Duration(backoffBase.Load()) }

// SetTestTimings shortens BackoffBase and DefaultSpacing for a test, until
// the returned func restores them.
func SetTestTimings(backoff, spacing time.Duration) (restore func()) {
	b, s := backoffBase.Swap(int64(backoff)), defaultSpacing.Swap(int64(spacing))
	return func() { backoffBase.Store(b); defaultSpacing.Store(s) }
}

// Backoff is the wait after a destination's n-th consecutive failure.
func Backoff(n int) time.Duration {
	if n > 10 {
		return maxBackoff
	}
	return min(BackoffBase()<<max(n-1, 0), maxBackoff)
}

// holderPrefix starts every Deliverer's holder (a destination's
// busy_holder, its deliveries' lease_holder).
const holderPrefix = "agenthook_"

// passLock is the advisory lock key under which one instance at a time
// runs a pass.
const passLock = 0x77616b6510 // "wake" 16

var errPrivate = errors.New("agenthooks: the address isn't public")

// Client is the HTTP client for destinations: public addresses only,
// checked on every connection (so DNS can't point it inside; a test's
// loopback server only with AllowTestOrigin), no proxy, no redirects
// followed, short timeouts.
func Client() *http.Client {
	d := &net.Dialer{Timeout: 5 * time.Second, Control: func(_, address string, _ syscall.RawConn) error {
		ap, err := netip.ParseAddrPort(address)
		if err != nil {
			return errPrivate
		}
		if mcpevents.PublicAddr(ap.Addr()) || testOrigin.Load() != nil && ap.Addr().Unmap().IsLoopback() {
			return nil
		}
		return errPrivate
	}}
	t := &http.Transport{DialContext: d.DialContext, Proxy: nil, TLSHandshakeTimeout: 5 * time.Second,
		ResponseHeaderTimeout: Timeout, MaxIdleConnsPerHost: 4, IdleConnTimeout: time.Minute}
	return &http.Client{Transport: t, Timeout: Timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// Deliverer POSTs each live destination's session's deliveries to it, in
// order and one at a time.
//
// Acknowledgement is explicit: a delivery is acknowledged (deliveries.Ack,
// state acked) only when the destination answers 2xx and its body isn't
// "no-match" (Capy: 202 accepted or duplicate), which means Capy has taken
// the wake for the thread; not when the agent reads it. Anything else
// leaves the delivery waiting, unacknowledged, with the same delivery ID
// (the Idempotency-Key) for its next attempt, so delivery is at least
// once, and Capy dedupes a retry of one it did accept:
//
//   - 5xx, 429, other 4xx, a 3xx (never followed), a timeout or a network
//     error: the destination waits Backoff, then tries again;
//   - 404 or 410 (the watch is gone), or 202 no-match (its conditions
//     filter the message out): the destination is dead, nothing more goes
//     there, and messages wait until the conversation registers again (or
//     its fallback command takes them);
//   - 413: the same message goes again at once as its Monitor-line
//     preview (streams.MonitorLine, "truncated"); a 413 for that too is a
//     failure, as above.
//
// A delivery is leased while it's sent; one an instance never finished
// returns to waiting when its lease runs out (deliveries.Reap). Several
// instances can run it: a destination is held by one at a time
// (busy_holder).
type Deliverer struct {
	DB   *db.DB
	Seal *mcpevents.Sealer
	// Poll is how often a pass looks (default 10s), on one instance at a
	// time: for retries that are due and deliveries no Wake announced.
	Poll time.Duration
	// Client calls the destinations (default: Client()).
	Client *http.Client
	// Spacing is the least time between two POSTs to one destination
	// (default DefaultSpacing): a rate limit per destination, on top of one at a time.
	// Negative: none (tests).
	Spacing time.Duration
	// Cursor turns Cursor Automations destinations (Grok Bot) on, and
	// paces them: off (nil), the deliverer skips them, as with the core's
	// WAKESHIFT_GROK_WEBHOOKS off.
	Cursor *CursorPacing

	once   sync.Once
	holder string
	slots  chan struct{}
	// sends are the sends deliver started, which Run waits for before it
	// returns, so none outlives it.
	sends sync.WaitGroup
	woken *wakeQueue
	// gate holds Wake's agents until it's read that their workspace has a
	// destination (agent_webhook_targets), so a workspace without one
	// costs no claim transaction.
	gate *wakegate.Gate
}

func (d *Deliverer) init() {
	d.once.Do(func() {
		b := make([]byte, 8)
		rand.Read(b)
		d.holder = holderPrefix + hex.EncodeToString(b)
		if d.Client == nil {
			d.Client = Client()
		}
		d.slots = make(chan struct{}, sendSlots)
		d.woken = newWakeQueue()
		d.gate = wakegate.New()
	})
}

func (d *Deliverer) spacing() time.Duration {
	switch {
	case d.Spacing < 0:
		return 0
	case d.Spacing == 0:
		return DefaultSpacing()
	}
	return d.Spacing
}

// Wake has agent's deliveries in tenant sent now (if Run is running), not
// on the next pass: for new deliveries, and a destination just registered
// (what's waiting goes at once). Only if the workspace has a destination
// (wakegate).
func (d *Deliverer) Wake(tenant, agent string) {
	d.init()
	d.gate.Add(tenant, agent)
}

// hasTargets reports which of tenants have a destination.
// agent_webhook_targets is platform-level, read by its primary key.
func (d *Deliverer) hasTargets(ctx context.Context, tenants []string) (map[string]bool, error) {
	rows, _ := d.DB.Pool.Query(ctx, `SELECT DISTINCT tenant_id::text FROM agent_webhook_targets WHERE tenant_id = ANY($1::uuid[])`, tenants)
	with, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(with))
	for _, t := range with {
		out[t] = true
	}
	return out, nil
}

// Run delivers until ctx ends: at once for each Wake, and in a pass every
// Poll.
func (d *Deliverer) Run(ctx context.Context) {
	d.init()
	// Its workers first (no new sends), then the sends they started: those
	// see ctx end, and only record what happened.
	defer d.sends.Wait()
	var wg sync.WaitGroup
	defer wg.Wait()
	wg.Go(func() {
		d.gate.Run(ctx, d.hasTargets, func(tenant, agent string) { d.woken.add(wake{tenant: tenant, agent: agent}) })
	})
	for range 2 {
		wg.Go(func() {
			for {
				w, ok := d.woken.next(ctx)
				if !ok {
					return
				}
				d.deliver(ctx, w.tenant, w.agent)
				d.woken.done(w)
			}
		})
	}
	every := d.Poll
	if every <= 0 {
		every = 10 * time.Second
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		var err error
		if _, lerr := d.DB.Exclusive(ctx, passLock, func() { err = d.Pass(ctx) }); lerr != nil {
			err = lerr
		}
		if err != nil && ctx.Err() == nil {
			slog.Warn("agent webhooks: delivery pass failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Pass claims what's waiting for every live destination, once, and starts
// sending it.
func (d *Deliverer) Pass(ctx context.Context) error {
	d.init()
	rows, _ := d.DB.Pool.Query(ctx, `SELECT DISTINCT tenant_id::text FROM agent_webhook_targets`)
	tenants, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	for _, t := range tenants {
		if ctx.Err() != nil {
			break
		}
		d.deliver(ctx, t, "")
	}
	return nil
}

// deliver claims what's waiting for the workspace's destinations (only
// agent's, if set) and sends it, without waiting for the sends.
func (d *Deliverer) deliver(ctx context.Context, tenant, agent string) {
	got, err := d.claim(ctx, tenant, agent)
	if err != nil {
		if ctx.Err() == nil {
			slog.Warn("agent webhooks: claiming deliveries failed", "tenant", tenant, "err", err)
		}
		return
	}
	for _, c := range got {
		d.sends.Add(1)
		go func() {
			defer d.sends.Done()
			defer func() { <-d.slots }()
			if d.send(ctx, c) {
				d.woken.add(wake{tenant: c.tenant, agent: c.agent})
			}
		}()
	}
}

// claimed is one destination's deliveries for a pass. url is the opened
// URL: it lives only in memory, for the sends.
type claimed struct {
	tenant, session, agent string
	url                    string
	failures               int
	list                   []deliveries.Delivery
	frames                 []streams.Frame
	// A Cursor destination's: its key, the bot, the app's name, and
	// whether this send is the join's announcement.
	cursor   bool
	key      string
	bot, app string
	join     bool
}

// claim takes the workspace's live destinations (only agent's, if set)
// that are free (no other instance sending, no backoff running) and have
// deliveries waiting, after removing those whose grant ended, and leases
// each one's next deliveries to this instance. Each takes a send slot
// first; one that finds none is left for the next wake or pass.
func (d *Deliverer) claim(ctx context.Context, tenant, agent string) (out []claimed, err error) {
	defer func() {
		if err != nil {
			for range out {
				<-d.slots
			}
			out = nil
		}
	}()
	err = d.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		// The grant the registration link was made under ended (revoked,
		// expired): its destinations go, as its stream links do.
		ended, err := streams.RemoveAgentWebhooks(ctx, tx, "hub", "grant_ended", `($1 = '' OR w.agent_id = $1) AND w.grant_id IS NOT NULL
			AND NOT EXISTS (SELECT 1 FROM grants g WHERE g.tenant_id = w.tenant_id AND g.id = w.grant_id AND g.revoked_at IS NULL AND g.expires_at > now())`, agent)
		if err != nil {
			return err
		}
		if ended > 0 {
			slog.Info("agent webhooks: access ended; destinations removed", "tenant", tenant, "count", ended)
		}
		if agent == "" || ended > 0 {
			if _, err := tx.Exec(ctx, `DELETE FROM agent_webhook_targets t WHERE t.tenant_id = current_tenant()
				AND NOT EXISTS (SELECT 1 FROM agent_webhooks w WHERE w.tenant_id = t.tenant_id AND w.session_id = t.session_id)`); err != nil {
				return err
			}
		}
		type row struct {
			Session, Agent, Kind string
			URL                  []byte
			Failures             int
			JoinDue              bool
			Bot, App             string
			HourPosts, DayPosts  int
			HourEnd, DayEnd      *time.Time
		}
		// Each kind only where this revision knows it (a Cursor destination
		// only with Cursor on, and not while it needs setup); a Cursor
		// destination also for its join's announcement.
		rows, _ := tx.Query(ctx, dueWebhooksSQL, agent, d.Cursor != nil)
		list, err := pgx.CollectRows(rows, pgx.RowToStructByPos[row])
		if err != nil {
			return err
		}
		for _, w := range list {
			if w.Kind == KindCursor {
				// Past the hour's or the day's cap: the next window.
				var until *time.Time
				window := ""
				switch {
				case w.HourPosts >= d.Cursor.perHour():
					until, window = w.HourEnd, "hour"
				case w.DayPosts >= d.Cursor.perDay():
					until, window = w.DayEnd, "day"
				}
				if until != nil {
					slog.Info("agent webhooks: destination at its cap; messages wait", "tenant", tenant, "session", w.Session, "reason", "paced",
						"cap", window, "kind", KindCursor, "app", w.App)
					if _, err := tx.Exec(ctx, `UPDATE agent_webhooks SET next_attempt_at = $2 WHERE session_id = $1`, w.Session, *until); err != nil {
						return err
					}
					continue
				}
			}
			select {
			case d.slots <- struct{}{}:
			default:
				return nil // every slot is busy: the rest wait for the next wake or pass
			}
			var c *claimed
			var err error
			if w.Kind == KindCursor {
				c, err = d.claimCursor(ctx, tx, tenant, w.Session, w.Agent, w.URL, w.Failures, w.JoinDue, w.Bot, w.App)
			} else {
				c, err = d.claimOne(ctx, tx, tenant, w.Session, w.Agent, w.URL, w.Failures)
			}
			if err != nil || c == nil {
				<-d.slots
				if err != nil {
					return err
				}
				continue
			}
			out = append(out, *c)
		}
		return nil
	})
	return out, err
}

// claimOne opens a destination's URL and leases its next deliveries; nil
// if there's nothing to send.
func (d *Deliverer) claimOne(ctx context.Context, tx pgx.Tx, tenant, session, agent string, sealed []byte, failures int) (*claimed, error) {
	raw, err := d.Seal.Open(binding(tenant, session), sealed)
	if err == nil && CheckURL(string(raw)) != nil {
		err = ErrBadURL // not accepted any more (a test's origin, gone): never sent to
	}
	if err != nil {
		slog.Error("agent webhooks: a destination's URL can't be used; destination marked dead", "tenant", tenant, "session", session)
		_, err := tx.Exec(ctx, `UPDATE agent_webhooks SET state = 'dead', dead_reason = $2, dead_at = now() WHERE session_id = $1`, session, ReasonUnreadable)
		return nil, err
	}
	list, err := deliveries.Claim(ctx, tx, agent, session, d.holder, lease, perPass)
	if err != nil || len(list) == 0 {
		return nil, err
	}
	frames, err := streams.FramesOf(ctx, tx, list)
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `UPDATE agent_webhooks SET busy_holder = $2, busy_until = now() + $3::interval WHERE session_id = $1`,
		session, d.holder, lease.String()); err != nil {
		return nil, err
	}
	return &claimed{tenant: tenant, session: session, agent: agent, url: string(raw), failures: failures, list: list, frames: frames}, nil
}

// outcome is what became of one delivery's attempt.
type outcome int

const (
	accepted outcome = iota // 2xx, not no-match: acknowledged
	failed                  // retried after Backoff
	dead                    // the destination is dead
)

// Sent is what a destination answered.
type Sent struct {
	Status int
	Body   []byte // its first 4 KiB
}

// Body is a delivery's body: its Monitor line (type, topic, from, trust,
// subject, body, data, id, …) whole if it fits in MaxBody, else its
// preview marked "truncated" (preview true).
func Body(f streams.Frame) (body []byte, preview bool) {
	if whole := streams.EventLine(f, 1<<30); len(whole) <= MaxBody {
		return whole, false
	}
	return streams.EventLine(f, MaxBody), true
}

// Post sends one body to a destination: Content-Type application/json and
// Idempotency-Key the delivery's ID, the same on every attempt. Its error
// may hold the URL (net/http puts it in *url.Error): never log it; log
// Category instead.
func Post(ctx context.Context, c *http.Client, url, deliveryID string, body []byte) (Sent, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return Sent{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Wakeshift")
	req.Header.Set("Idempotency-Key", deliveryID)
	resp, err := c.Do(req)
	if err != nil {
		return Sent{}, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10)) // reuse the connection, within reason
	return Sent{Status: resp.StatusCode, Body: b}, nil
}

// Category is a failure's category: the status class or the network
// error's kind, never the endpoint's own words or the error's text (which
// can carry the URL).
func Category(status int, err error) string {
	var ne net.Error
	switch {
	case err != nil && errors.Is(err, errPrivate):
		return "address_not_public"
	case err != nil && errors.As(err, &ne) && ne.Timeout():
		return "timeout"
	case err != nil && (strings.Contains(err.Error(), "tls:") || strings.Contains(err.Error(), "x509:")):
		return "tls_error"
	case err != nil:
		return "connection_failed"
	case status/100 == 3:
		return "http_3xx"
	case status/100 == 4:
		return "http_4xx"
	case status/100 == 5:
		return "http_5xx"
	}
	return ""
}

// noMatch reports whether a 2xx answer says the watch's conditions
// filtered the message out (Capy: 202 with "no-match").
func noMatch(body []byte) bool {
	b := bytes.ToLower(body)
	return bytes.Contains(b, []byte("no-match")) || bytes.Contains(b, []byte("no_match"))
}

// judge is what an answer means for the delivery and the destination.
func judge(s Sent, err error) (outcome, string) {
	switch {
	case err != nil:
		return failed, ""
	case s.Status/100 == 2 && noMatch(s.Body):
		return dead, ReasonNoMatch
	case s.Status/100 == 2:
		return accepted, ""
	case s.Status == http.StatusNotFound:
		return dead, ReasonNotFound
	case s.Status == http.StatusGone:
		return dead, ReasonGone
	}
	return failed, ""
}

// send POSTs one destination's claimed deliveries, in order, one at a
// time, and records each outcome; a failure or a dead destination stops it
// (the rest go back to waiting, untouched). finished reports that it got
// through all of them (so more may be sent now).
func (d *Deliverer) send(ctx context.Context, c claimed) (finished bool) {
	if c.cursor {
		return d.sendCursor(ctx, c)
	}
	defer d.free(ctx, c)
	for i, dl := range c.list {
		if i > 0 {
			select {
			case <-ctx.Done():
			case <-time.After(d.spacing()):
			}
		}
		if ctx.Err() != nil {
			d.release(ctx, c, c.list[i:])
			return false
		}
		f := c.frames[i]
		body, preview := Body(f)
		began := time.Now()
		sent, err := Post(ctx, d.Client, c.url, dl.ID, body)
		if err == nil && sent.Status == http.StatusRequestEntityTooLarge && !preview {
			// Too large for it after all: the Monitor line's preview.
			sent, err = Post(ctx, d.Client, c.url, dl.ID, streams.MonitorLine(f))
			preview = true
		}
		took := time.Since(began)
		if ctx.Err() != nil {
			// Stopping (a deploy): back to waiting now, this one too (a 2xx
			// it had gets it sent again, deduplicated by its key).
			d.release(ctx, c, c.list[i:])
			return false
		}
		o, reason := judge(sent, err)
		category := Category(sent.Status, err)
		bg := context.WithoutCancel(ctx)
		rerr := d.DB.InTenant(bg, c.tenant, func(tx pgx.Tx) error { return d.record(bg, tx, c, dl, o, reason, category, sent.Status) })
		if rerr != nil {
			// Nothing about this attempt is recorded: it and the rest go
			// back to waiting (a 2xx it had gets it sent again: at least
			// once, deduplicated by its key).
			slog.Warn("agent webhooks: recording a delivery failed; stopping this destination's pass", "tenant", c.tenant, "session", c.session, "err", rerr)
			d.release(ctx, c, c.list[i:])
			return false
		}
		switch o {
		case accepted:
			var age int64
			if !dl.Event.OccurredAt.IsZero() {
				age = time.Since(dl.Event.OccurredAt).Milliseconds()
			}
			slog.Info("agent webhooks: delivered", "tenant", c.tenant, "session", c.session, "delivery", dl.ID, "status", sent.Status,
				"preview", preview, "age_ms", age, "send_ms", took.Milliseconds())
			c.failures = 0
		case dead:
			slog.Warn("agent webhooks: destination dead; messages wait", "tenant", c.tenant, "session", c.session, "delivery", dl.ID,
				"status", sent.Status, "reason", reason)
			d.release(ctx, c, c.list[i+1:])
			return false
		case failed:
			slog.Warn("agent webhooks: delivery failed; retrying", "tenant", c.tenant, "session", c.session, "delivery", dl.ID,
				"status", sent.Status, "why", category, "failures", c.failures+1, "retry_in", Backoff(c.failures+1).String())
			d.release(ctx, c, c.list[i+1:])
			return false
		}
	}
	return true
}

// record writes one attempt's outcome: the delivery's state and the
// destination's, the destination locked first (as claiming does).
func (d *Deliverer) record(ctx context.Context, tx pgx.Tx, c claimed, dl deliveries.Delivery, o outcome, reason, category string, status int) error {
	if _, err := tx.Exec(ctx, `SELECT 1 FROM agent_webhooks WHERE session_id = $1 FOR NO KEY UPDATE`, c.session); err != nil {
		return err
	}
	var err error
	switch o {
	case accepted:
		// The acknowledgement: the destination took it (Capy queued the
		// thread's wake).
		if _, err = deliveries.Ack(ctx, tx, c.agent, c.session, dl.ID); err == nil {
			_, err = tx.Exec(ctx, `UPDATE agent_webhooks SET delivered = delivered + 1, last_delivery_at = now(), last_status = $2, last_error = NULL,
				failures = 0, next_attempt_at = NULL WHERE session_id = $1`, c.session, status)
		}
	case dead:
		// Not acknowledged: it waits, with the same ID.
		if err = deliveries.Unclaim(ctx, tx, d.holder, []string{dl.ID}); err == nil {
			_, err = tx.Exec(ctx, `UPDATE agent_webhooks SET state = 'dead', dead_reason = $2, dead_at = now(), last_status = $3, last_error = NULL
				WHERE session_id = $1`, c.session, reason, status)
		}
	default:
		// Not acknowledged, and the attempt isn't counted against the
		// delivery (it's the destination that's failing): it waits, with
		// the same ID, for the destination's next attempt or another way of
		// waking the conversation.
		if err = deliveries.Unclaim(ctx, tx, d.holder, []string{dl.ID}); err == nil {
			_, err = tx.Exec(ctx, `UPDATE agent_webhooks SET failures = failures + 1, next_attempt_at = now() + $2::interval,
				last_status = nullif($3, 0), last_error = nullif($4, '') WHERE session_id = $1`,
				c.session, Backoff(c.failures+1).String(), status, category)
		}
	}
	return err
}

// free lets other passes take the destination again.
func (d *Deliverer) free(ctx context.Context, c claimed) {
	bg := context.WithoutCancel(ctx)
	err := d.DB.InTenant(bg, c.tenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(bg, `UPDATE agent_webhooks SET busy_holder = NULL, busy_until = NULL WHERE session_id = $1 AND busy_holder = $2`, c.session, d.holder)
		return err
	})
	if err != nil {
		slog.Warn("agent webhooks: freeing a destination failed", "tenant", c.tenant, "session", c.session, "err", err)
	}
}

// release returns claimed deliveries to waiting without counting the
// attempt.
func (d *Deliverer) release(ctx context.Context, c claimed, rest []deliveries.Delivery) {
	if len(rest) == 0 {
		return
	}
	ids := make([]string, 0, len(rest))
	for _, dl := range rest {
		ids = append(ids, dl.ID)
	}
	bg := context.WithoutCancel(ctx)
	err := d.DB.InTenant(bg, c.tenant, func(tx pgx.Tx) error { return deliveries.Unclaim(bg, tx, d.holder, ids) })
	if err != nil {
		slog.Warn("agent webhooks: releasing deliveries failed", "tenant", c.tenant, "session", c.session, "err", err)
	}
}

// wake is an agent with something to send in a workspace.
type wake struct{ tenant, agent string }

// wakeQueue is the set of woken agents not yet claimed for; one woken
// again while it's being claimed for is claimed for again after.
type wakeQueue struct {
	mu      sync.Mutex
	pending []wake
	queued  map[wake]bool
	busy    map[wake]bool
	again   map[wake]bool
	ready   chan struct{}
}

func newWakeQueue() *wakeQueue {
	return &wakeQueue{queued: map[wake]bool{}, busy: map[wake]bool{}, again: map[wake]bool{}, ready: make(chan struct{}, 1)}
}

func (q *wakeQueue) add(w wake) {
	q.mu.Lock()
	defer q.mu.Unlock()
	switch {
	case q.busy[w]:
		q.again[w] = true
	case !q.queued[w]:
		q.queued[w] = true
		q.pending = append(q.pending, w)
		select {
		case q.ready <- struct{}{}:
		default:
		}
	}
}

func (q *wakeQueue) next(ctx context.Context) (wake, bool) {
	for {
		q.mu.Lock()
		if len(q.pending) > 0 {
			w := q.pending[0]
			q.pending = q.pending[1:]
			delete(q.queued, w)
			q.busy[w] = true
			if len(q.pending) > 0 {
				select {
				case q.ready <- struct{}{}:
				default:
				}
			}
			q.mu.Unlock()
			return w, true
		}
		q.mu.Unlock()
		select {
		case <-ctx.Done():
			return wake{}, false
		case <-q.ready:
		}
	}
}

func (q *wakeQueue) done(w wake) {
	q.mu.Lock()
	delete(q.busy, w)
	again := q.again[w]
	delete(q.again, w)
	q.mu.Unlock()
	if again {
		q.add(w)
	}
}

// dueWebhooksSQL claims the live destinations with something to send ($1:
// one agent's, or "" for all; $2: Cursor on). The read of each one's
// conversation's deliveries compares target with (SELECT
// w.session_id::text): under row-level security, Postgres 17.12 and later
// won't use an I/O cast of an outer column (not leakproof) as an index
// condition, and would read every delivery of the agent's for each
// destination (TestDueWebhooksReadByIndex).
var dueWebhooksSQL = `SELECT w.session_id::text, w.agent_id, w.kind, w.url, w.failures, w.join_due, coalesce(w.bot_id, ''), coalesce(a.name, w.app_id),
				CASE WHEN w.hour_start > now() - interval '1 hour' THEN w.hour_posts ELSE 0 END,
				CASE WHEN w.day_start > now() - interval '1 day' THEN w.day_posts ELSE 0 END,
				w.hour_start + interval '1 hour', w.day_start + interval '1 day'
			FROM agent_webhooks w LEFT JOIN apps a ON a.id = w.app_id
			WHERE w.state = 'live' AND (w.busy_until IS NULL OR w.busy_until < now()) AND (w.next_attempt_at IS NULL OR w.next_attempt_at <= now())
				AND ($1 = '' OR w.agent_id = $1)
				AND (w.kind = 'capy' OR w.kind = 'cursor-automations' AND $2 AND w.needs_setup_at IS NULL)
				AND (w.kind = 'cursor-automations' AND w.join_due
					OR EXISTS (SELECT 1 FROM deliveries dl WHERE dl.tenant_id = w.tenant_id AND dl.agent_id = w.agent_id AND dl.target = (SELECT w.session_id::text)
						AND dl.deliver_to IS NULL AND dl.state = 'pending' AND dl.available_at <= now()))
			ORDER BY w.session_id FOR UPDATE OF w SKIP LOCKED`
