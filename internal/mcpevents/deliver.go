package mcpevents

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/db"
	"github.com/wakeshift/server/internal/deliveries"
	"github.com/wakeshift/server/internal/events"
	"github.com/wakeshift/server/internal/framing"
	"github.com/wakeshift/server/internal/mcpevents/event"
	"github.com/wakeshift/server/internal/wakegate"
)

// Deliverer POSTs each live webhook's session's deliveries to its callback,
// in order and one at a time: a 2xx acks it; anything else is retried with
// Backoff, and nothing after it goes until it's acknowledged or abandoned
// (MaxAttempts, RetryWindow; 410 and 413 aren't retried). Several instances
// can run it: a webhook is held by one at a time (busy_holder). Before
// delivering it checks the app's grant for the session's agent is live,
// and at least every ReauthEvery that the session is still the agent's;
// if not, the webhook ends, with a terminated envelope.
//
// New deliveries go out as soon as they're made: the pipeline calls Wake
// for each agent it made them for, on the instance that made them. The
// pass every Poll is the fallback (deliveries made elsewhere, a retry
// that's due, an ending), and looks only at webhooks with something to do.
type Deliverer struct {
	DB   *db.DB
	Seal *Sealer
	// Poll is how often a pass looks (default 10s), on one instance at a
	// time: the fallback, for retries that are due (every Backoff is far
	// longer), endings and deliveries no Wake announced.
	Poll time.Duration
	// Client calls the callbacks (default: Client()).
	Client *http.Client
	Now    func() time.Time

	once   sync.Once
	holder string
	sends  *shares // the send slots, shared fairly between apps and workspaces
	woken  *wakeQueue
	// gate holds Wake's agents until it's read that their workspace has a
	// webhook (mcp_event_targets), so a workspace without one costs no
	// claim transaction.
	gate *wakegate.Gate
}

// sendSlots is how many webhooks an instance sends to at once: a slow
// callback holds up only its own webhook. Of them, one app takes at most
// appSends, and one workspace (one of the app's users) at most
// workspaceSends, so a few slow callbacks can't take every slot: the other
// apps' and workspaces' webhooks are claimed and sent meanwhile.
const (
	sendSlots      = 32
	appSends       = 8
	workspaceSends = 4
)

// wakeWorkers is how many woken agents are claimed for at once, and
// passClaimers how many workspaces a pass claims in at once: each holds a
// database connection, and the core's pool is small.
const wakeWorkers = 2

// perPass is how many deliveries one webhook sends in a pass: within the
// lease even if each takes the whole timeout.
const perPass = 5

// holderPrefix starts every Deliverer's holder (a webhook's busy_holder,
// its deliveries' lease_holder).
const holderPrefix = "webhook_"

// lease is how long a webhook (and its claimed deliveries) is held while
// they're sent.
const lease = 90 * time.Second

func (d *Deliverer) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

func (d *Deliverer) init() {
	d.once.Do(func() {
		b := make([]byte, 8)
		rand.Read(b)
		d.holder = holderPrefix + hex.EncodeToString(b)
		if d.Client == nil {
			d.Client = Client()
		}
		d.sends = newShares()
		d.woken = newWakeQueue()
		d.gate = wakegate.New()
	})
}

// Wake has agent's new deliveries in tenant sent now, not on the next
// pass (if Run is running), if the workspace has a webhook (wakegate).
func (d *Deliverer) Wake(tenant, agent string) {
	d.init()
	d.gate.Add(tenant, agent)
}

// hasTargets reports which of tenants have a webhook, expired ones
// included (a claim removes those). mcp_event_targets is platform-level,
// read by its primary key.
func (d *Deliverer) hasTargets(ctx context.Context, tenants []string) (map[string]bool, error) {
	rows, _ := d.DB.Pool.Query(ctx, `SELECT DISTINCT tenant_id::text FROM mcp_event_targets WHERE tenant_id = ANY($1::uuid[])`, tenants)
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
	var wg sync.WaitGroup
	defer wg.Wait()
	wg.Go(func() {
		d.gate.Run(ctx, d.hasTargets, func(tenant, agent string) { d.woken.add(wake{tenant: tenant, agent: agent}) })
	})
	for range wakeWorkers {
		wg.Go(func() {
			for {
				w, ok := d.woken.next(ctx)
				if !ok {
					return
				}
				d.deliverNow(ctx, w)
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
		// One instance at a time: a pass visits every workspace with a
		// webhook, and each instance's own wakes send what it makes.
		var err error
		if _, lerr := d.DB.Exclusive(ctx, passLock, func() { err = d.Pass(ctx) }); lerr != nil {
			err = lerr
		}
		if err != nil && ctx.Err() == nil {
			slog.Warn("mcp events: delivery pass failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// passLock is the advisory lock key under which one instance at a time
// runs a pass.
const passLock = 0x77616b6504 // "wake" 4

// wake is an agent with new deliveries in a workspace (retry: again,
// after a claim that failed).
type wake struct {
	tenant, agent string
	retry         bool
}

// deliverNow claims what's waiting for one agent's webhooks in one
// workspace and sends it, without waiting for the sends to finish.
func (d *Deliverer) deliverNow(ctx context.Context, w wake) {
	got, gone, err := d.claim(ctx, w.tenant, w.agent)
	if err != nil {
		if ctx.Err() == nil {
			slog.Warn("mcp events: claiming deliveries failed; trying again", "tenant", w.tenant, "err", err)
			// Once more shortly, then it's the pass's.
			if !w.retry {
				time.AfterFunc(time.Second, func() { d.woken.add(wake{tenant: w.tenant, agent: w.agent, retry: true}) })
			}
		}
		return
	}
	d.dispatch(ctx, got, gone)
}

// dispatch sends what was claimed: each webhook's deliveries, and the
// terminated envelopes, each in the send slot claim took for it, so a slow
// callback holds up only its own webhook. A webhook that got through
// everything it was sending is looked at again at once: more may have come
// while it was held (a Wake for it then found it busy).
func (d *Deliverer) dispatch(ctx context.Context, got []claimed, gone []ended) {
	for _, e := range gone {
		d.spawn(e.hook, func() { d.control(ctx, e.hook, EnvelopeTerminated, TerminatedBody(e.dialect, e.reason)) })
	}
	for _, c := range got {
		d.spawn(c.hook, func() {
			if d.send(ctx, c) {
				d.woken.add(wake{tenant: c.tenant, agent: c.agent})
			}
		})
	}
}

// spawn runs fn in a goroutine, in the send slot claim took for h, and
// frees the slot after. It never waits: the slot was taken before h was
// claimed, so a claimed webhook's lease doesn't run down in a queue, and
// claiming never waits on a slow app's sends.
func (d *Deliverer) spawn(h hook, fn func()) {
	go func() {
		defer d.freeSlot(h)
		fn()
	}()
}

// freeSlot gives back h's send slot, and has the webhooks that found no slot
// free (shares.take) looked at again.
func (d *Deliverer) freeSlot(h hook) {
	for _, w := range d.sends.give(h.tenant, h.appID) {
		d.woken.add(w)
	}
}

// shares counts the sends in flight on this instance, in all, for each app
// and for each workspace, within sendSlots, appSends and workspaceSends.
// A webhook is claimed only once its slot is taken (take); one that finds
// its app, its workspace or every slot full is left unclaimed, and looked
// at again when a send there ends (give), or by the next pass.
type shares struct {
	mu      sync.Mutex
	all     int
	byKey   map[string]int
	waiting map[string]map[wake]bool // by what was full: allSlots, an app's key or a workspace's
}

const allSlots = "*"

func newShares() *shares {
	return &shares{byKey: map[string]int{}, waiting: map[string]map[wake]bool{}}
}

func appKey(app string) string          { return "app:" + app }
func workspaceKey(tenant string) string { return "workspace:" + tenant }

// take takes a send slot for one of app's webhooks in tenant, reporting
// whether there was one; if not, w is woken when one there is given back.
func (s *shares) take(tenant, app string, w wake) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ws := appKey(app), workspaceKey(tenant)
	full := ""
	switch {
	case s.all >= sendSlots:
		full = allSlots
	case s.byKey[a] >= appSends:
		full = a
	case s.byKey[ws] >= workspaceSends:
		full = ws
	}
	if full != "" {
		if s.waiting[full] == nil {
			s.waiting[full] = map[wake]bool{}
		}
		s.waiting[full][w] = true
		return false
	}
	s.all++
	s.byKey[a]++
	s.byKey[ws]++
	return true
}

// give gives back a slot take took, returning the wakes now to be tried
// again: those that found every slot, this app or this workspace full.
func (s *shares) give(tenant, app string) []wake {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.all--
	var out []wake
	for _, k := range []string{appKey(app), workspaceKey(tenant)} {
		if s.byKey[k]--; s.byKey[k] <= 0 {
			delete(s.byKey, k)
		}
	}
	for _, k := range []string{allSlots, appKey(app), workspaceKey(tenant)} {
		for w := range s.waiting[k] {
			out = append(out, w)
		}
		delete(s.waiting, k)
	}
	return out
}

// wakeQueue is the set of notified agents not yet claimed for. One is with
// one worker at a time; notified again meanwhile, it's claimed for again
// after.
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
		q.signal()
	}
}

func (q *wakeQueue) signal() {
	select {
	case q.ready <- struct{}{}:
	default:
	}
}

// next waits for an agent to claim for, until ctx ends; call done for it
// once claimed.
func (q *wakeQueue) next(ctx context.Context) (wake, bool) {
	for {
		q.mu.Lock()
		if len(q.pending) > 0 {
			w := q.pending[0]
			q.pending = q.pending[1:]
			delete(q.queued, w)
			q.busy[w] = true
			if len(q.pending) > 0 {
				q.signal()
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

// hook is what a pass knows about one webhook.
type hook struct {
	tenant, id, url string
	handle, agent   string
	session         string
	appID           string
	app             string // its display name
	dialect         string
	event           string // the event type it's subscribed to
	secrets         [][]byte
}

// claimed is one webhook's deliveries for a pass.
type claimed struct {
	hook
	list     []deliveries.Delivery
	replay   bool // already acknowledged or abandoned: replayed from a cursor
	attempts int  // failed attempts at the head delivery so far
	since    *time.Time
	// messages are the deliveries' webhook-ids, by delivery ID, in the
	// spec's dialect (in the others, webhook-id is the delivery's ID, its
	// eventId).
	messages map[string]string
}

// messageID is a claimed delivery's webhook-id: its ID (its eventId) in
// the dialects that want them equal, else the message ID claimOne gave it.
// A spec-dialect delivery without one is an error (a retry), never sent
// with its eventId.
func (c claimed) messageID(dl deliveries.Delivery) (string, error) {
	if event.WebhookIDIsEventID(c.dialect) {
		return dl.ID, nil
	}
	if id := c.messages[dl.ID]; id != "" {
		return id, nil
	}
	return "", errNoMessageID
}

// errNoMessageID is a spec-dialect delivery claimed without a webhook-id.
var errNoMessageID = errors.New("mcpevents: a delivery has no message ID")

// ended is a webhook a pass ended, to be told (terminated).
type ended struct {
	hook
	reason string
}

// Pass claims what's waiting for every live webhook, once, and starts sending
// it (the sends go on after it returns, each webhook held meanwhile).
func (d *Deliverer) Pass(ctx context.Context) error {
	d.init()
	start := time.Now()
	// Workspaces with an expired webhook are visited too: claim removes it
	// (and its target) there. Visiting only those with a live one left a
	// workspace's expired webhooks behind for good once its last one
	// lapsed, still counted as failing if it lapsed mid-retry.
	rows, _ := d.DB.Pool.Query(ctx, `SELECT DISTINCT tenant_id::text FROM mcp_event_targets`)
	tenants, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	// Workspaces are claimed a few at a time, each in its own transaction,
	// and what's claimed goes out at once (a webhook held while the rest of
	// the pass claims would hold up its newest deliveries too).
	var claimedHooks atomic.Int64
	var claiming sync.WaitGroup
	next := make(chan string)
	for range passClaimers {
		claiming.Go(func() {
			for tenant := range next {
				got, end, err := d.claim(ctx, tenant, "")
				if err != nil {
					if ctx.Err() == nil {
						slog.Warn("mcp events: claiming deliveries failed", "tenant", tenant, "err", err)
					}
					continue
				}
				claimedHooks.Add(int64(len(got) + len(end)))
				d.dispatch(ctx, got, end)
			}
		})
	}
	for _, t := range tenants {
		if ctx.Err() != nil {
			break
		}
		next <- t
	}
	close(next)
	claiming.Wait()
	// A target its workspace's claims couldn't remove (they kept failing)
	// goes an hour after it lapsed.
	if _, err := d.DB.Pool.Exec(ctx, `DELETE FROM mcp_event_targets WHERE expires_at < now() - interval '1 hour'`); err != nil && ctx.Err() == nil {
		return err
	}
	if took := time.Since(start); took > time.Second {
		slog.Warn("mcp events: a delivery pass's claims are slow", "tenants", len(tenants), "webhooks", claimedHooks.Load(), "took", took.Round(time.Millisecond))
	}
	return nil
}

const passClaimers = 2

// webhookRow is a webhook as a pass reads it.
type webhookRow struct {
	ID, App, Agent, Session, Handle, URL string
	Secret, Previous                     []byte
	Rotating                             bool
	AppName, Dialect                     string
	Reauth                               bool // last checked more than ReauthEvery ago
	ReplayAfter, ReplayThrough           *int64
	Attempts                             int
	Since                                *time.Time
	RetryDelivery                        *string // the delivery Attempts and Since are for
	Event, Ending                        string
}

// claim takes the workspace's webhooks (only agent's, if it's set) that
// are free (no other instance sending, none waiting out a retry) and have
// something to do (deliveries waiting, a replay, an ending, a grant no
// longer live, or a recheck of the session that's due), after ending the
// expired ones, and claims each in turn (claimOne). So an idle webhook
// costs a pass no queries of its own.
//
// Each webhook takes its send slot (shares.take) before it's claimed, and
// keeps it for dispatch; one that finds none free stays unclaimed, to be
// looked at when one frees up. A slot taken for nothing (nothing to send,
// or the transaction failed) is given back at once, so the next webhook
// here can take it (were it kept to the end, a webhook with nothing to
// send could keep its neighbor waiting, and the wake that slot hands
// back would claim the two again, and again); the wakes it hands back go
// once the transaction is over, so they find its rows unlocked.
func (d *Deliverer) claim(ctx context.Context, tenant, agent string) (out []claimed, gone []ended, err error) {
	var again []wake
	defer func() {
		if err != nil {
			for _, c := range out {
				again = append(again, d.sends.give(tenant, c.appID)...)
			}
			for _, e := range gone {
				again = append(again, d.sends.give(tenant, e.appID)...)
			}
			out, gone = nil, nil
		}
		for _, w := range again {
			d.woken.add(w)
		}
	}()
	err = d.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		// Expired webhooks go, and finished ones (Finish) once nothing
		// waits for them: their last delivery sent (or abandoned), and any
		// replay done. Only a minute after they finished: the delivery that
		// finished one commits with it, and a statement whose snapshot is
		// older wouldn't see it waiting (a webhook row it waited for is
		// rechecked as it is now, its subquery isn't).
		if _, err := tx.Exec(ctx, pruneWebhooksSQL); err != nil {
			return err
		}
		rows, _ := tx.Query(ctx, dueWebhooksSQL, ReauthEvery.String(), agent)
		list, err := pgx.CollectRows(rows, pgx.RowToStructByPos[webhookRow])
		if err != nil {
			return err
		}
		for _, w := range list {
			if !d.sends.take(tenant, w.App, wake{tenant: tenant, agent: w.Agent}) {
				continue // no slot free: left as it is, to be looked at again
			}
			c, e, err := d.claimOne(ctx, tx, tenant, w)
			switch {
			case err != nil:
				again = append(again, d.sends.give(tenant, w.App)...)
				return err
			case e != nil:
				gone = append(gone, *e)
			case c != nil:
				out = append(out, *c)
			default:
				again = append(again, d.sends.give(tenant, w.App)...)
			}
		}
		return nil
	})
	return out, gone, err
}

// claimOne is one webhook's part of a pass, in its workspace's transaction:
// its access checked (ending it, to be told, if it's gone), then what it
// sends next leased to this instance: what's to be replayed first, then
// what's waiting. Neither (nil, nil): nothing to send, or its secret is
// unreadable.
func (d *Deliverer) claimOne(ctx context.Context, tx pgx.Tx, tenant string, w webhookRow) (*claimed, *ended, error) {
	h := hook{tenant: tenant, id: w.ID, url: w.URL, handle: w.Handle, agent: w.Agent, session: w.Session, appID: w.App, app: w.AppName, dialect: w.Dialect,
		event: w.Event}
	secret, err := d.Seal.open(w.ID, w.Secret)
	if err != nil {
		slog.Error("mcp events: can't read a webhook's secret", "tenant", tenant, "webhook", w.ID, "err", err)
		return nil, nil, nil
	}
	h.secrets = [][]byte{secret}
	if w.Rotating && len(w.Previous) > 0 {
		if prev, err := d.Seal.open(w.ID, w.Previous); err == nil {
			h.secrets = append(h.secrets, prev)
		}
	}
	why := w.Ending // ended (Terminate): tell the receiver and remove it
	if why == "" {
		if why, err = authorized(ctx, tx, w.App, w.Agent, w.Session, w.Reauth); err != nil {
			return nil, nil, err
		}
	}
	if why == "" && w.Reauth {
		// At the same recheck: an event type the app's manifest dropped ends
		// its webhooks (nothing more can be emitted to them), telling the
		// receiver NotFound, so it lists the events again.
		if why, err = offered(ctx, tx, w.App, w.Event); err != nil {
			return nil, nil, err
		}
	}
	if why != "" {
		slog.Info("mcp events: access ended; webhook ended", "tenant", tenant, "webhook", w.ID, "why", why)
		if _, err := Unsubscribe(ctx, tx, w.ID); err != nil {
			return nil, nil, err
		}
		return nil, &ended{hook: h, reason: why}, nil
	}
	if w.Reauth {
		if _, err := tx.Exec(ctx, `UPDATE mcp_event_webhooks SET authorized_at = now() WHERE id = $1`, w.ID); err != nil {
			return nil, nil, err
		}
	}
	c := claimed{hook: h, attempts: w.Attempts, since: w.Since}
	if w.ReplayAfter != nil && w.ReplayThrough != nil && *w.ReplayAfter < *w.ReplayThrough {
		c.replay = true
		if c.list, err = replayable(ctx, tx, w.ID, w.App, w.Agent, w.Session, *w.ReplayAfter, *w.ReplayThrough); err != nil {
			return nil, nil, err
		}
		if len(c.list) == 0 { // nothing left in the range (pruned): done
			if _, err := tx.Exec(ctx, `UPDATE mcp_event_webhooks SET replay_after = NULL, replay_through = NULL WHERE id = $1`, w.ID); err != nil {
				return nil, nil, err
			}
			c.replay = false
		}
	}
	if !c.replay {
		// One a webhook deliverer claimed and didn't finish (an instance
		// that stopped mid-send) still comes first: nothing is claimed
		// past it until the sweep returns it to waiting once its lease
		// lapses (deliveries.Reap).
		var held bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM deliveries dl WHERE dl.agent_id = $1 AND dl.target = $2 AND dl.state = 'in_flight'
			AND left(dl.lease_holder, length($3)) = $3 AND `+deliveries.OfApp("dl", "$4")+`)`, w.Agent, w.Session, holderPrefix, w.App).Scan(&held); err != nil {
			return nil, nil, err
		}
		if held {
			return nil, nil, nil
		}
		// Only the app's own deliveries: its webhook's callback is whatever
		// the app said, so it gets what the app's subscriptions made, as
		// the app's wait tool does (deliveries.OfApp).
		if c.list, err = deliveries.ClaimFor(ctx, tx, w.Agent, w.Session, w.App, d.holder, lease, perPass); err != nil {
			return nil, nil, err
		}
	}
	if len(c.list) == 0 {
		return nil, nil, nil
	}
	if !event.WebhookIDIsEventID(w.Dialect) {
		if c.messages, err = messageIDs(ctx, tx, c.list); err != nil {
			return nil, nil, err
		}
	}
	// The retries so far are another delivery's (it ended while it was
	// being retried: canceled with its subscription): this one starts
	// afresh, with its own MaxAttempts and RetryWindow.
	if w.RetryDelivery == nil || *w.RetryDelivery != c.list[0].ID {
		c.attempts, c.since = 0, nil
	}
	if _, err := tx.Exec(ctx, `UPDATE mcp_event_webhooks SET busy_holder = $2, busy_until = now() + $3::interval WHERE id = $1`,
		w.ID, d.holder, lease.String()); err != nil {
		return nil, nil, err
	}
	return &c, nil, nil
}

// messageIDs gives each delivery in list a webhook-id of its own, if it
// has none yet (MessageID), and returns them all by delivery ID: one made
// for an earlier attempt (a retry, a resend after an instance stopped
// mid-send) is kept.
func messageIDs(ctx context.Context, tx pgx.Tx, list []deliveries.Delivery) (map[string]string, error) {
	ids, fresh := make([]string, len(list)), make([]string, len(list))
	for i, dl := range list {
		ids[i], fresh[i] = dl.ID, MessageID()
	}
	// Only the deliveries without one are written; the rest are read. (The
	// SELECT sees the rows as they were before the UPDATE, so a row given
	// one here comes back once, from the UPDATE.)
	rows, err := tx.Query(ctx, `WITH given AS (
			UPDATE deliveries d SET message_id = f.fresh
			FROM unnest($1::uuid[], $2::text[]) AS f(id, fresh)
			WHERE d.id = f.id AND d.message_id IS NULL
			RETURNING d.id::text AS id, d.message_id)
		SELECT id, message_id FROM given
		UNION ALL
		SELECT id::text, message_id FROM deliveries WHERE id = ANY($1::uuid[]) AND message_id IS NOT NULL`, ids, fresh)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(list))
	var id, msg string
	_, err = pgx.ForEachRow(rows, []any{&id, &msg}, func() error {
		out[id] = msg
		return nil
	})
	return out, err
}

// outcome is what became of one attempt.
type outcome int

const (
	acked outcome = iota
	retry
	abandoned // given up; a gap unless the receiver refused it (410)
)

// send POSTs one webhook's claimed deliveries, in order, one at a time,
// and records each outcome; a failure stops it (the rest wait their turn).
// finished reports that it got through all of them (so more may be sent
// now), not stopped by a failure or ctx.
func (d *Deliverer) send(ctx context.Context, c claimed) (finished bool) {
	defer d.free(ctx, c)
	for i, dl := range c.list {
		cursor := Cursor(int64(dl.Event.Seq))
		body, err := EventBody(dl, c.event, c.app, cursor, c.dialect)
		var sent Sent
		began := time.Now()
		var msgID string
		if err == nil {
			msgID, err = c.messageID(dl)
		}
		if err == nil {
			sent, err = Send(ctx, d.Client, c.url, c.id, msgID, body, d.now(), c.secrets...)
		}
		took := time.Since(began)
		if ctx.Err() != nil {
			// Stopping (a deploy): what isn't recorded goes back to waiting
			// now, this one too (a 2xx it had gets it sent again: same
			// eventId), rather than when its lease runs out.
			if !c.replay {
				d.release(ctx, c, c.list[i:])
			}
			return false
		}
		o, gap, why := acked, false, ""
		category := CategoryOf(sent.Status, err)
		switch {
		case errors.Is(err, errTooLarge):
			o, gap, why = abandoned, true, "over 256 KiB"
		case errors.Is(err, events.ErrRedacted):
			o, gap, why = abandoned, true, "content deleted after delivery"
		case errors.Is(err, errPrivate):
			o, gap, why = abandoned, true, "the callback's address isn't public"
		case err != nil:
			o, why = retry, category
		case sent.Status/100 == 2:
		case sent.Status == http.StatusGone: // the receiver doesn't want it: not a gap
			o, why = abandoned, "HTTP 410"
		case sent.Status == http.StatusRequestEntityTooLarge:
			o, gap, why = abandoned, true, "HTTP 413"
		default:
			o, why = retry, category
		}
		if o == retry && (c.attempts+1 >= MaxAttempts || c.since != nil && d.now().Sub(*c.since) >= RetryWindow) {
			o, gap = abandoned, true
		}
		// Recorded whatever happens to ctx now: the attempt happened.
		bg := context.WithoutCancel(ctx)
		var wm int64
		err = d.DB.InTenant(bg, c.tenant, func(tx pgx.Tx) error {
			if err := d.record(bg, tx, c, dl, o, gap, why, category, sent.Status); err != nil {
				return err
			}
			if gap {
				var err error
				wm, err = Watermark(bg, tx, c.id)
				return err
			}
			return nil
		})
		if err != nil {
			// Nothing about this attempt is recorded: stop here, so the
			// order holds and no gap or cursor runs ahead of what's in the
			// database. This one and the rest go back to waiting; a 2xx it
			// had gets it sent again (at least once: same eventId).
			slog.Warn("mcp events: recording a delivery failed; stopping this webhook's pass", "tenant", c.tenant, "webhook", c.id, "err", err)
			if !c.replay {
				d.release(ctx, c, c.list[i:])
			}
			return false
		}
		switch o {
		case acked:
			// age_ms: from the event's occurrence to its acknowledgment.
			var age int64
			if !dl.Event.OccurredAt.IsZero() {
				age = d.now().Sub(dl.Event.OccurredAt).Milliseconds()
			}
			slog.Info("mcp events: delivered", "tenant", c.tenant, "webhook", c.id, "delivery", dl.ID, "status", sent.Status, "replay", c.replay,
				"age_ms", age, "send_ms", took.Milliseconds())
			c.attempts, c.since = 0, nil
		case abandoned:
			slog.Warn("mcp events: delivery abandoned", "tenant", c.tenant, "webhook", c.id, "delivery", dl.ID, "why", why, "attempts", c.attempts+1)
			c.attempts, c.since = 0, nil
			if gap {
				d.control(ctx, c.hook, EnvelopeGap, GapBody(Cursor(wm)))
			}
		case retry:
			slog.Warn("mcp events: delivery failed; retrying", "tenant", c.tenant, "webhook", c.id, "delivery", dl.ID, "why", why, "attempt", c.attempts+1)
			// Nothing after it goes until it's acknowledged or abandoned:
			// the rest go back to waiting, untouched.
			if !c.replay {
				d.release(ctx, c, c.list[i+1:])
			}
			return false
		}
	}
	return true
}

// control POSTs a control envelope (gap, terminated) once. ChatGPT takes
// neither, and OpenClaw answers both with 400, so they get none
// (event.TakesEnvelopes).
func (d *Deliverer) control(ctx context.Context, h hook, typ string, body []byte) {
	if !event.TakesEnvelopes(h.dialect) {
		return
	}
	sent, err := Send(ctx, d.Client, h.url, h.id, ControlID(typ), body, d.now(), h.secrets...)
	if err != nil || sent.Status/100 != 2 {
		slog.Warn("mcp events: an envelope wasn't accepted", "tenant", h.tenant, "webhook", h.id, "type", typ, "why", CategoryOf(sent.Status, err))
	}
}

var errTooLarge = errors.New("mcpevents: body over 256 KiB")

// EventBody is a delivery for a subscription to one of the app's event
// types (its manifest's events), as the callback gets it: an occurrence
// with that event's name and the app's payload as data, untouched (the
// emit checked it against the event's payloadSchema), the cursor after
// it, and in _meta the rendered line: the emit's text, framed as outside
// information (the payload isn't repeated there). The rendered line goes
// first if the body would pass MaxBody; a payload that still doesn't fit
// can't be cut without changing it, so it's errTooLarge (a gap). Its
// eventId is the app's (AppEventID) only in a dialect whose webhook-id
// isn't the eventId: in the others (draft, openclaw, chatgpt) the
// webhook-id is the eventId, so it stays the delivery's own ID, never a
// value the app chose or one shared by two subscriptions.
func EventBody(dl deliveries.Delivery, name, app, cursor, dialect string) ([]byte, error) {
	occ, err := Occurrence(dl, name, app, !event.WebhookIDIsEventID(dialect))
	if err != nil {
		return nil, err
	}
	occ["cursor"] = cursor
	b, err := marshalRaw(occ)
	if err == nil && len(b) > MaxBody {
		delete(occ, "_meta")
		b, err = marshalRaw(occ)
	}
	if err == nil && len(b) > MaxBody {
		return nil, errTooLarge
	}
	return b, err
}

// AppEventIDAttr is the attribute an event copied from an app's emit or
// publish carries the app's own ID for it in (the emit's id, or a publish
// URL's signed webhook-id), when the app gave one AppEventID accepts.
const AppEventIDAttr = "app_event_id"

// MaxAppEventID is the longest app ID passed on as an eventId (an emit's
// id is at most 200 characters).
const MaxAppEventID = 200

// AppEventID reports whether an app's ID for an event can be an
// occurrence's eventId (SEP-3415, Event Occurrences: the upstream's stable
// identifier, when it has one): 1 to MaxAppEventID printable ASCII
// characters, no spaces, as Stripe's, GitHub's and Kafka's IDs are. An ID
// outside that still dedupes the app's emits; its occurrences keep the
// delivery's ID.
func AppEventID(id string) bool {
	if id == "" || len(id) > MaxAppEventID {
		return false
	}
	for i := 0; i < len(id); i++ {
		if id[i] < 0x21 || id[i] > 0x7e {
			return false
		}
	}
	return true
}

// Occurrence is a delivery as an EventOccurrence (SEP-3415), without a
// cursor (a webhook's body has one after each event, a poll's result one
// for the batch). Its eventId, what a client deduplicates on within a
// subscription, is the same on every resend: with upstream, the app's own
// ID for the event when it gave one (AppEventIDAttr), as the spec says,
// so the same event reached by an emit and by a replay carries one
// eventId; otherwise, and without upstream, the delivery's ID, unique to
// the subscription and header-safe, so it can be the webhook-id too in
// the dialects whose clients require the two to be equal
// (event.WebhookIDIsEventID). data is the app's payload, untouched, and
// _meta has the rendered line. events.ErrRedacted for an event whose
// content was deleted once delivered: it's never sent empty.
func Occurrence(dl deliveries.Delivery, name, app string, upstream bool) (map[string]any, error) {
	e := dl.Event
	if e.Redacted {
		return nil, events.ErrRedacted // never sent empty
	}
	from := e.From.ID
	if e.FromHandle != "" {
		from = e.FromHandle
	}
	text := framing.Frame(Reply, framing.Event{ID: e.ID, Topic: e.Attrs["topic"], Text: e.Subject, Body: e.Body, Mentions: e.Mentions, ParentID: e.ParentID,
		At: e.OccurredAt, FromKind: e.From.Kind, FromHandle: from, App: app, Trust: string(e.Trust)}.Message())
	data := json.RawMessage(e.Data)
	if len(data) == 0 || string(data) == "null" {
		data = json.RawMessage(`{}`)
	}
	at := e.OccurredAt
	if at.IsZero() {
		at = time.Now()
	}
	id := dl.ID
	if own := e.Attrs[AppEventIDAttr]; upstream && AppEventID(own) {
		id = own
	}
	return map[string]any{"eventId": id, "name": name, "timestamp": at.UTC().Format(time.RFC3339), "data": data,
		"_meta": map[string]any{event.TextMeta: text}}, nil
}

// OccurrenceJSON is Occurrence as JSON, as a poll's result and a push
// stream carry it (with the app's eventId: neither has a webhook-id): the
// rendered line left out if the occurrence would pass MaxBody, and
// errTooLarge (ErrTooLarge) if it still would.
func OccurrenceJSON(dl deliveries.Delivery, name, app string) (json.RawMessage, error) {
	occ, err := Occurrence(dl, name, app, true)
	if err != nil {
		return nil, err
	}
	b, err := marshalRaw(occ)
	if err == nil && len(b) > MaxBody {
		delete(occ, "_meta")
		b, err = marshalRaw(occ)
	}
	if err == nil && len(b) > MaxBody {
		return nil, errTooLarge
	}
	return b, err
}

// ErrTooLarge is an occurrence over MaxBody even without its rendered line.
var ErrTooLarge = errTooLarge

// marshalRaw is json.Marshal without HTML escaping, so the app's payload
// goes out as it came (no < for <).
func marshalRaw(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// Reply is how a woken conversation is told to answer.
const Reply = "If you answer, answer with this app's tools."

// The deliverer's two reads of each webhook's conversation's deliveries
// compare target with (SELECT w.session_id::text), not w.session_id::text:
// under row-level security, Postgres 17.12 and later won't use an I/O cast
// of an outer column (not leakproof) as an index condition, and would read
// every delivery the agent has in the workspace for each webhook. The
// sub-select is a parameter to the planner, so the read goes by
// deliveries_queue (TestDelivererReadsByIndex).

// pruneWebhooksSQL removes expired webhooks, and finished ones once nothing
// waits for them, and the targets of both.
var pruneWebhooksSQL = `WITH gone AS (DELETE FROM mcp_event_webhooks w WHERE w.expires_at <= now()
				OR w.finished_at < now() - interval '1 minute' AND NOT (w.replay_after IS NOT NULL AND w.replay_through IS NOT NULL AND w.replay_after < w.replay_through)
				AND NOT EXISTS (SELECT 1 FROM deliveries dl WHERE dl.tenant_id = w.tenant_id AND dl.agent_id = w.agent_id
					AND dl.target = (SELECT w.session_id::text) AND dl.state IN ('pending', 'in_flight') AND ` + deliveries.OfApp("dl", "w.app_id") + `)
				RETURNING id)
			DELETE FROM mcp_event_targets WHERE tenant_id = current_tenant() AND (webhook_id IN (SELECT id FROM gone)
				OR NOT EXISTS (SELECT 1 FROM mcp_event_webhooks w WHERE w.id = mcp_event_targets.webhook_id))`

// dueWebhooksSQL claims the webhooks with something to do ($1: ReauthEvery;
// $2: one agent's, or "" for all).
var dueWebhooksSQL = `SELECT w.id, w.app_id, w.agent_id, w.session_id::text, w.session_handle, w.url, w.secret,
			coalesce(w.previous_secret, ''::bytea), coalesce(w.previous_until > now(), false), a.name, w.dialect,
			w.authorized_at < now() - $1::interval, w.replay_after, w.replay_through, w.retry_attempts, w.retrying_since, w.retry_delivery_id::text,
			w.name, coalesce(w.ending_reason, '')
			FROM mcp_event_webhooks w JOIN apps a ON a.id = w.app_id
			WHERE (w.busy_until IS NULL OR w.busy_until < now())
				AND (w.next_attempt_at IS NULL OR w.next_attempt_at <= now() OR w.ending_reason IS NOT NULL)
				AND ($2 = '' OR w.agent_id = $2)
				AND (w.ending_reason IS NOT NULL OR w.authorized_at < now() - $1::interval
					OR w.replay_after IS NOT NULL AND w.replay_through IS NOT NULL AND w.replay_after < w.replay_through
					OR NOT EXISTS (SELECT 1 FROM grants g WHERE g.tenant_id = w.tenant_id AND g.app_id = w.app_id AND g.kind = 'agent'
						AND g.agent_id = w.agent_id AND g.revoked_at IS NULL AND g.expires_at > now())
					OR EXISTS (SELECT 1 FROM deliveries dl WHERE dl.tenant_id = w.tenant_id AND dl.agent_id = w.agent_id
						AND dl.target = (SELECT w.session_id::text) AND dl.state = 'pending' AND dl.available_at <= now() AND ` + deliveries.OfApp("dl", "w.app_id") + `))
			ORDER BY w.id FOR UPDATE OF w SKIP LOCKED`
