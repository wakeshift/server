// Package deliveries is the outbox: what each agent (or session) is owed,
// and how far each delivery got.
//
//	pending → in_flight → delivered | acked
//	              ↓ lease expires
//	           pending (another attempt) … dead after MaxAttempts
//	pending | in_flight → canceled (subscription canceled or revoked)
//
// A stream claims an agent's pending deliveries (leasing them), writes them,
// then waits for an ack, or for consumers that can't acknowledge, for the
// connection to outlive the write (internal/streams), before marking them
// delivered: at least once, either way. If the stream dies, it releases
// them, or the lease expires and Reap puts them back.
//
// An event whose app asks for it (events.Event.DeleteOnDelivery) loses its
// content once it's delivered: see RedactDelivered.
package deliveries

import (
	"cmp"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/events"
)

// MaxAttempts is how many times a delivery is leased before it's dead.
const MaxAttempts = 10

// NotifyChannel is the Postgres channel notified with "tenant:agent" when
// new deliveries for that agent commit.
const NotifyChannel = "wakeshift_deliveries"

// Delivery is one event owed to an agent (or one of its sessions).
type Delivery struct {
	ID             string       `json:"delivery_id"`
	AgentID        string       `json:"agent_id"`
	Target         string       `json:"target,omitempty"`
	SubscriptionID string       `json:"subscription_id"`
	State          string       `json:"state"`
	Attempts       int          `json:"attempt"`
	Event          events.Event `json:"event"`
	// DeliverTo is set on the agent's connector stream for a delivery that
	// belongs to one of its connector sessions (e.g. a Codex thread).
	DeliverTo *Target `json:"deliver_to,omitempty"`
	// AlsoWoken are the agent's other sessions this event woke (each
	// joined): so the conversations can see each other and not double up.
	AlsoWoken []string `json:"also_woken,omitempty"`
}

// Target names the session a connector delivery is for.
type Target struct {
	Session      string `json:"session"` // its handle (codex#2)
	Conversation string `json:"conversation,omitempty"`
}

// inQueue is the SQL condition for the deliveries of the stream with target
// target, whose placeholder is n: a session's own (not connector-delivered
// ones), or, for the agent's stream (""), its own plus every connector
// session's.
//
// Each branch is its own SQL, chosen here rather than by a CASE on n: pgx
// prepares statements, and a generic plan (which Postgres may cache after
// five runs) doesn't fold a CASE on a parameter, so it loses target as an
// index condition and walks every delivery the agent ever had. The agent's
// branch still names n (target = n is target = ” there), since Postgres
// can't type a parameter the statement never uses.
func inQueue(target, n string) string {
	if target == "" {
		return `(target = ` + n + ` OR deliver_to IS NOT NULL)`
	}
	return `(target = ` + n + ` AND deliver_to IS NULL)`
}

// claimable is inQueue for claiming, going by how a connector delivery's
// session is delivered to now rather than when it was queued: once its
// connector redeems a ticket the session holds its own stream, and gets what
// was still left for the agent's connector stream (in flight at the time,
// or queued by a runner step that began before). Claiming one for the
// session clears deliver_to, so the session can ack it. Like inQueue, each
// branch is its own SQL.
func claimable(target, n string) string {
	connector := `EXISTS (SELECT 1 FROM sessions s WHERE s.tenant_id = deliveries.tenant_id AND s.id = deliveries.deliver_to AND s.runtime = 'connector')`
	if target == "" {
		return `(target = ` + n + ` OR deliver_to IS NOT NULL AND ` + connector + `)`
	}
	return `(target = ` + n + ` AND (deliver_to IS NULL OR NOT ` + connector + `))`
}

// OfApp is the SQL condition for the deliveries one app's subscriptions
// made (app is its placeholder, e.g. "$4", empty for every app's), on
// the deliveries table or alias. A session's queue holds every app's
// deliveries for that conversation: the agent's own channels (its stream
// links, its connector) take them all, but an app reading or acknowledging
// for the session itself (its wait tool, its inbox, a webhook it
// registered) sees only its own.
func OfApp(alias, app string) string {
	return `(` + app + `::text = '' OR EXISTS (SELECT 1 FROM subscriptions sub JOIN grants g ON g.tenant_id = sub.tenant_id AND g.id = sub.grant_id
		WHERE sub.tenant_id = ` + alias + `.tenant_id AND sub.id = ` + alias + `.subscription_id AND g.app_id = ` + app + `::text))`
}

// ID is a delivery's deterministic ID: the same agent, event and target
// always give the same ID, so redone work inserts nothing new and consumers
// can recognize a redelivery.
func ID(agentID, eventID, target string) string {
	h := sha256.Sum256([]byte("delivery\x00" + agentID + "\x00" + eventID + "\x00" + target))
	h[6] = h[6]&0x0f | 0x80 // version 8 (custom), RFC 9562
	h[8] = h[8]&0x3f | 0x80 // variant
	return fmt.Sprintf("%x-%x-%x-%x-%x", h[0:4], h[4:6], h[6:8], h[8:10], h[10:16])
}

// New is what Insert needs.
type New struct {
	AgentID        string
	Target         string // a session ID, or "" for the agent
	DeliverTo      string // for a connector session: its ID (Target too); delivered on the agent's stream
	SubscriptionID string
	Event          events.Event
	AvailableAt    time.Time // not before (paced wake-ups); zero: now
}

// Insert adds a pending delivery, unless the agent (target) already has one
// for the event. It reports whether it inserted.
func Insert(ctx context.Context, tx pgx.Tx, n New) (bool, error) {
	if n.Event.Redacted {
		return false, nil // delivered once, its content gone: never an empty wake-up
	}
	var at *time.Time
	if !n.AvailableAt.IsZero() {
		at = &n.AvailableAt
	}
	tag, err := tx.Exec(ctx, `INSERT INTO deliveries (tenant_id, id, agent_id, target, event_id, event_seq, subscription_id, deliver_to, available_at)
		VALUES (current_tenant(), $1, $2, $3, $4, $5, $6, nullif($7, '')::uuid, coalesce($8, now()))
		ON CONFLICT (tenant_id, agent_id, event_id, target) DO NOTHING`,
		ID(n.AgentID, n.Event.ID, n.Target), n.AgentID, n.Target, n.Event.ID, n.Event.Seq, n.SubscriptionID, n.DeliverTo, at)
	return tag.RowsAffected() == 1, err
}

// ToOwnStream makes what's pending for an agent's connector session
// (delivered on the agent's stream: deliver_to) the session's own: it
// waits, or holds a stream, itself from now on. It reads the session's
// queue (agent and target) by index, not the workspace's.
func ToOwnStream(ctx context.Context, tx pgx.Tx, agentID, session string) error {
	_, err := tx.Exec(ctx, toOwnStreamSQL, agentID, session)
	return err
}

const toOwnStreamSQL = `UPDATE deliveries SET deliver_to = NULL WHERE agent_id = $1 AND target = $2 AND deliver_to IS NOT NULL AND state = 'pending'`

// HandedTo records the connector (connectorversion.Client.String()) the
// deliveries were just claimed for, for the delivery activity; nothing
// for "" (a stream an agent holds itself).
func HandedTo(ctx context.Context, tx pgx.Tx, ds []Delivery, connector string) error {
	if connector == "" || len(ds) == 0 {
		return nil
	}
	ids := make([]string, len(ds))
	for i, d := range ds {
		ids[i] = d.ID
	}
	_, err := tx.Exec(ctx, `UPDATE deliveries SET connector = $2 WHERE id = ANY($1::uuid[]) AND connector IS DISTINCT FROM $2`, ids, connector)
	return err
}

// Claim leases up to limit of an agent's (target's) pending deliveries, in
// event order, to holder until now+lease, counting an attempt on each.
// Deliveries another holder has leased are skipped.
func Claim(ctx context.Context, tx pgx.Tx, agentID, target, holder string, lease time.Duration, limit int) ([]Delivery, error) {
	return ClaimFor(ctx, tx, agentID, target, "", holder, lease, limit)
}

// ClaimFor is Claim of only the deliveries app's subscriptions made (OfApp;
// "" for every app's).
func ClaimFor(ctx context.Context, tx pgx.Tx, agentID, target, app, holder string, lease time.Duration, limit int) ([]Delivery, error) {
	out, _, err := claim(ctx, tx, agentID, target, app, holder, lease, limit, false)
	return out, err
}

// TurnHolderPrefix starts the lease holder of a connection that delivers
// only between its conversation's turns (Codex's connector: it starts a
// turn once the thread is idle, and holds what comes while it's busy).
const TurnHolderPrefix = "conn_turn_"

// turnHeld is the SQL condition for a delivery such a connection holds and
// hasn't delivered: written to it, not acknowledged.
const turnHeld = `state = 'in_flight' AND lease_holder LIKE 'conn\_turn\_%'`

// ClaimTurnHeld is ClaimFor that also takes what a connection delivering
// only between turns holds and hasn't delivered (TurnHolderPrefix): the
// conversation is in a turn and asked for its messages (an app's wait
// tool), so they're handed over now rather than after the turn. Taking one
// from such a connection doesn't count an attempt (the conversation never
// had it). took reports whether any was taken from one: the caller tells
// its stream (NotifyChannel), which tells the connection.
func ClaimTurnHeld(ctx context.Context, tx pgx.Tx, agentID, target, app, holder string, lease time.Duration, limit int) (_ []Delivery, took bool, err error) {
	return claim(ctx, tx, agentID, target, app, holder, lease, limit, true)
}

// claimSQL is claim's statement for target ($2; agent $1, holder $3, lease
// $4, limit $5, app $6).
func claimSQL(target string, held bool) string {
	which := `state = 'pending' AND available_at <= now()`
	if held {
		which = `(` + which + ` OR ` + turnHeld + `)`
	}
	return `
		WITH next AS (
			SELECT id, state = 'in_flight' AS held FROM deliveries
			WHERE agent_id = $1 AND ` + claimable(target, "$2") + ` AND ` + which + ` AND ` + OfApp("deliveries", "$6") + `
			ORDER BY event_seq LIMIT $5
			FOR UPDATE SKIP LOCKED
		)
		UPDATE deliveries d SET state = 'in_flight', attempts = CASE WHEN next.held THEN d.attempts ELSE d.attempts + 1 END,
			lease_holder = $3, lease_until = now() + $4::interval,
			deliver_to = CASE WHEN $2 = '' THEN d.deliver_to END
		FROM next WHERE d.tenant_id = current_tenant() AND d.id = next.id
		RETURNING d.id, d.event_id, d.subscription_id, d.attempts, d.target, coalesce(d.deliver_to::text, ''), next.held`
}

func claim(ctx context.Context, tx pgx.Tx, agentID, target, app, holder string, lease time.Duration, limit int, held bool) ([]Delivery, bool, error) {
	rows, err := tx.Query(ctx, claimSQL(target, held), agentID, target, holder, lease.String(), limit, app)
	if err != nil {
		return nil, false, err
	}
	type claimed struct {
		ID, EventID, SubscriptionID string
		Attempts                    int
		Target, DeliverTo           string
		Held                        bool
	}
	list, err := pgx.CollectRows(rows, pgx.RowToStructByPos[claimed])
	if err != nil {
		return nil, false, err
	}
	took := slices.ContainsFunc(list, func(c claimed) bool { return c.Held })
	if len(list) > 0 {
		// Leased: the sweep reaps them if the lease runs out.
		if err := events.MarkDue(ctx, tx); err != nil {
			return nil, false, err
		}
	}
	out := make([]Delivery, 0, len(list))
	for _, c := range list {
		e, err := events.Get(ctx, tx, c.EventID)
		if err != nil {
			return nil, false, err
		}
		if e.Redacted {
			if err := finishRedacted(ctx, tx, c.ID); err != nil {
				return nil, false, err
			}
			continue
		}
		d := Delivery{ID: c.ID, AgentID: agentID, Target: c.Target, SubscriptionID: c.SubscriptionID,
			State: "in_flight", Attempts: c.Attempts, Event: e}
		rows, _ := tx.Query(ctx, `SELECT s.handle FROM deliveries o JOIN sessions s ON s.tenant_id = o.tenant_id AND s.id::text = o.target
			WHERE o.agent_id = $1 AND o.event_id = $2 AND o.id <> $3 ORDER BY s.handle`, agentID, c.EventID, c.ID)
		if d.AlsoWoken, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
			return nil, false, err
		}
		if c.DeliverTo != "" {
			d.DeliverTo = &Target{}
			if err := tx.QueryRow(ctx, `SELECT handle, coalesce(conversation, '') FROM sessions WHERE id = $1`, c.DeliverTo).
				Scan(&d.DeliverTo.Session, &d.DeliverTo.Conversation); err != nil {
				return nil, false, err
			}
		}
		out = append(out, d)
	}
	// UPDATE … RETURNING doesn't promise order; streams need event order.
	slices.SortFunc(out, func(a, b Delivery) int { return cmp.Compare(a.Event.Seq, b.Event.Seq) })
	return out, took, nil
}

// Pending reports whether an agent (target) has deliveries a stream could
// claim now.
func Pending(ctx context.Context, tx pgx.Tx, agentID, target string) (bool, error) {
	var ok bool
	err := tx.QueryRow(ctx, pendingSQL(target), agentID, target).Scan(&ok)
	return ok, err
}

// pendingSQL is Pending's statement for target ($2; agent $1).
func pendingSQL(target string) string {
	return `SELECT EXISTS (SELECT 1 FROM deliveries
		WHERE agent_id = $1 AND ` + claimable(target, "$2") + ` AND state = 'pending' AND available_at <= now())`
}

// NextAvailable is when the earliest of an agent's (target's) paced
// deliveries, pending but not available yet, becomes available: zero if
// there's none. A stream sets a timer for it rather than polling.
//
// It's on this instance's clock: the wait is measured on the database's
// (which Claim compares available_at with), so a clock that's off doesn't
// wake a stream too early, again and again, or too late.
func NextAvailable(ctx context.Context, tx pgx.Tx, agentID, target string) (time.Time, error) {
	return NextAvailableFor(ctx, tx, agentID, target, "")
}

// NextAvailableFor is NextAvailable of only app's deliveries (OfApp).
func NextAvailableFor(ctx context.Context, tx pgx.Tx, agentID, target, app string) (time.Time, error) {
	var in *float64 // seconds from now, on the database's clock
	err := tx.QueryRow(ctx, `SELECT extract(epoch FROM min(available_at) - clock_timestamp())::float8 FROM deliveries
		WHERE agent_id = $1 AND `+claimable(target, "$2")+` AND state = 'pending' AND available_at > now() AND `+OfApp("deliveries", "$3"),
		agentID, target, app).Scan(&in)
	if err != nil || in == nil {
		return time.Time{}, err
	}
	return time.Now().Add(time.Duration(max(*in, 0) * float64(time.Second))), nil
}

// Drain hands over up to limit of one session's (target's) pending
// deliveries at once, in event order, marking them delivered: for a
// conversation with nothing to wake it, which sees them in the result of
// its next tool call (at-most-once, like a stream without acks). It takes
// what's queued for the session whichever stream it would go to (a
// connector session's too).
func Drain(ctx context.Context, tx pgx.Tx, agentID, target string, limit int) ([]Delivery, error) {
	return DrainFor(ctx, tx, agentID, target, "", limit)
}

// DrainFor is Drain of only app's deliveries (OfApp): an app's tool result
// hands over only what its own subscriptions made.
func DrainFor(ctx context.Context, tx pgx.Tx, agentID, target, app string, limit int) ([]Delivery, error) {
	rows, err := tx.Query(ctx, `
		WITH next AS (
			SELECT id FROM deliveries
			WHERE agent_id = $1 AND target = $2 AND state = 'pending' AND available_at <= now() AND `+OfApp("deliveries", "$4")+`
			ORDER BY event_seq LIMIT $3
			FOR UPDATE SKIP LOCKED
		)
		UPDATE deliveries d SET state = 'delivered', attempts = attempts + 1, finished_at = now(), deliver_to = NULL, lease_holder = NULL
		FROM next WHERE d.tenant_id = current_tenant() AND d.id = next.id
		RETURNING d.id, d.event_id, d.subscription_id, d.attempts`, agentID, target, limit, app)
	if err != nil {
		return nil, err
	}
	type drained struct {
		ID, EventID, SubscriptionID string
		Attempts                    int
	}
	list, err := pgx.CollectRows(rows, pgx.RowToStructByPos[drained])
	if err != nil {
		return nil, err
	}
	out := make([]Delivery, 0, len(list))
	ids := make([]string, 0, len(list))
	for _, c := range list {
		e, err := events.Get(ctx, tx, c.EventID)
		if err != nil {
			return nil, err
		}
		if e.Redacted {
			if err := finishRedacted(ctx, tx, c.ID); err != nil {
				return nil, err
			}
			continue
		}
		ids = append(ids, c.EventID)
		out = append(out, Delivery{ID: c.ID, AgentID: agentID, Target: target, SubscriptionID: c.SubscriptionID, State: "delivered", Attempts: c.Attempts, Event: e})
	}
	slices.SortFunc(out, func(a, b Delivery) int { return cmp.Compare(a.Event.Seq, b.Event.Seq) })
	// Handed over in the tool result, at most once (nothing confirms a tool
	// result, and Drain never resends: no lease holder, so neither
	// ResendLost nor RedactDelivered's resend window counts it): an event
	// that asks loses its content now if this was its last copy owed, or
	// at the expiry pass once the others are done.
	if err := redact(ctx, tx, ids); err != nil {
		return nil, err
	}
	return out, nil
}

// finishRedacted finishes a delivery whose event's content was deleted
// (RedactDelivered) after an earlier copy was delivered: it isn't sent
// again, empty. Only a race with a resend gets one this far.
func finishRedacted(ctx context.Context, tx pgx.Tx, id string) error {
	_, err := tx.Exec(ctx, `UPDATE deliveries SET state = 'delivered', finished_at = now(), lease_until = NULL, lease_holder = NULL,
		last_error = 'content deleted after delivery' WHERE id = $1`, id)
	return err
}

// Delivered marks leased deliveries as written to a consumer that can't
// acknowledge. Only the lease holder can.
func Delivered(ctx context.Context, tx pgx.Tx, holder string, ids []string) error {
	_, err := tx.Exec(ctx, `UPDATE deliveries SET state = 'delivered', finished_at = now(), lease_until = NULL
		WHERE id = ANY($1::uuid[]) AND state = 'in_flight' AND lease_holder = $2`, ids, holder)
	return err
}

// Ack records that an agent's (target's) consumer received a delivery, and
// reports whether it's now acknowledged: true again for a repeated ack (so
// a client can retry safely), false for a delivery that isn't this
// consumer's or was finished another way (canceled, dead). It works after
// the lease expired.
func Ack(ctx context.Context, tx pgx.Tx, agentID, target, id string) (bool, error) {
	return AckFor(ctx, tx, agentID, target, "", id)
}

// AckFor is Ack of only app's deliveries (OfApp): an app can't acknowledge,
// and so hide from the conversation, what another app's subscriptions
// delivered.
func AckFor(ctx context.Context, tx pgx.Tx, agentID, target, app, id string) (bool, error) {
	var state, eventID string
	err := tx.QueryRow(ctx, `
		WITH acked AS (
			UPDATE deliveries SET state = 'acked', finished_at = now(), lease_until = NULL
			WHERE id = $1 AND agent_id = $2 AND `+inQueue(target, "$3")+` AND state IN ('in_flight', 'pending', 'dead') AND `+OfApp("deliveries", "$4")+`
			RETURNING state, event_id::text
		)
		SELECT state, event_id FROM acked
		UNION ALL
		SELECT state, event_id::text FROM deliveries WHERE id = $1 AND agent_id = $2 AND `+inQueue(target, "$3")+` AND state = 'acked' AND `+OfApp("deliveries", "$4")+`
		LIMIT 1`, id, agentID, target, app).Scan(&state, &eventID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := redact(ctx, tx, []string{eventID}); err != nil {
		return false, err
	}
	return true, nil
}

// SeenHolder is the lease holder AckSeen leaves on what it acknowledges,
// so a between-turns connection that held one sees it's no longer its own.
const SeenHolder = "seen"

// AckSeen acknowledges app's deliveries (OfApp) for an agent's session
// (target) whose event's data has an "id" in ids, while they still wait
// for the conversation: pending, or held by a connection that delivers
// only between turns (TurnHolderPrefix). The app showed the conversation
// those messages another way (in a tool's result), so they mustn't wake it
// again. What's in flight to any other consumer (a Monitor, a hook's poll)
// is left alone: it's being handed over already. It returns the deliveries
// acknowledged, and whether any was taken from a between-turns connection
// (the caller notifies NotifyChannel, as for ClaimTurnHeld).
func AckSeen(ctx context.Context, tx pgx.Tx, agentID, target, app string, ids []string) (acked []string, took bool, err error) {
	if target == "" || len(ids) == 0 {
		return nil, false, nil
	}
	rows, err := tx.Query(ctx, `
		WITH seen AS (
			SELECT d.id, d.state = 'in_flight' AS held FROM deliveries d
			WHERE d.agent_id = $1 AND `+inQueue(target, "$2")+` AND (d.state = 'pending' OR `+turnHeld+`) AND `+OfApp("d", "$4")+`
				AND EXISTS (SELECT 1 FROM events e WHERE e.tenant_id = d.tenant_id AND e.id = d.event_id AND e.data->>'id' = ANY($3::text[]))
			ORDER BY d.id
			FOR UPDATE SKIP LOCKED
		)
		UPDATE deliveries d SET state = 'acked', finished_at = now(), lease_until = NULL, lease_holder = $5
		FROM seen WHERE d.tenant_id = current_tenant() AND d.id = seen.id
		RETURNING d.id::text, d.event_id::text, seen.held`, agentID, target, ids, app, SeenHolder)
	if err != nil {
		return nil, false, err
	}
	type seen struct {
		ID, EventID string
		Held        bool
	}
	list, err := pgx.CollectRows(rows, pgx.RowToStructByPos[seen])
	if err != nil {
		return nil, false, err
	}
	acked = make([]string, 0, len(list))
	evs := make([]string, 0, len(list))
	for _, s := range list {
		acked = append(acked, s.ID)
		evs = append(evs, s.EventID)
		took = took || s.Held
	}
	if err := redact(ctx, tx, evs); err != nil {
		return nil, false, err
	}
	return acked, took, nil
}

// AckHolder acknowledges what holder had of an agent's (target's)
// deliveries, still leased or handed back since: its consumer showed it
// received them all (a wait's batch, named by its cursor: after=).
func AckHolder(ctx context.Context, tx pgx.Tx, agentID, target, holder string) error {
	rows, err := tx.Query(ctx, `UPDATE deliveries SET state = 'acked', finished_at = now(), lease_until = NULL
		WHERE lease_holder = $3 AND agent_id = $1 AND `+inQueue(target, "$2")+` AND state IN ('in_flight', 'pending')
		RETURNING event_id::text`, agentID, target, holder)
	if err != nil {
		return err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	return redact(ctx, tx, ids)
}

// AckLeased acknowledges what holder still has leased of an agent's
// (target's) deliveries: in flight, with its lease not yet run out. A
// wait loop's next round confirms its last batch this way (streams,
// loop mode): unlike AckHolder, nothing handed back or past its lease
// counts, so a batch the loop may not have printed comes again.
func AckLeased(ctx context.Context, tx pgx.Tx, agentID, target, holder string) error {
	rows, err := tx.Query(ctx, `UPDATE deliveries SET state = 'acked', finished_at = now(), lease_until = NULL
		WHERE lease_holder = $3 AND agent_id = $1 AND `+inQueue(target, "$2")+` AND state = 'in_flight' AND lease_until > now()
		RETURNING event_id::text`, agentID, target, holder)
	if err != nil {
		return err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	return redact(ctx, tx, ids)
}

// AckHolderFor is AckHolder for an app's wait (deliveries.OfApp): it
// acknowledges only what holder had of the app's own deliveries, so an
// app's wait never confirms another app's.
func AckHolderFor(ctx context.Context, tx pgx.Tx, agentID, target, app, holder string) error {
	rows, err := tx.Query(ctx, `UPDATE deliveries SET state = 'acked', finished_at = now(), lease_until = NULL
		WHERE lease_holder = $3 AND agent_id = $1 AND `+inQueue(target, "$2")+` AND state IN ('in_flight', 'pending') AND `+OfApp("deliveries", "$4")+`
		RETURNING event_id::text`, agentID, target, holder, app)
	if err != nil {
		return err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	return redact(ctx, tx, ids)
}

// Unclaim returns leased deliveries that were never written to pending,
// without counting the attempt: the consumer never saw them.
func Unclaim(ctx context.Context, tx pgx.Tx, holder string, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := tx.Exec(ctx, `UPDATE deliveries SET state = 'pending', attempts = greatest(attempts - 1, 0),
		lease_until = NULL, lease_holder = NULL
		WHERE id = ANY($1::uuid[]) AND lease_holder = $2 AND state = 'in_flight'`, ids, holder)
	return err
}

// Renew extends a holder's leases, so a consumer that's still connected
// isn't sent the same delivery again just because it's slow to ack.
func Renew(ctx context.Context, tx pgx.Tx, holder string, lease time.Duration) error {
	_, err := tx.Exec(ctx, `UPDATE deliveries SET lease_until = now() + $2::interval
		WHERE lease_holder = $1 AND state = 'in_flight'`, holder, lease.String())
	return err
}

// Release returns a holder's unfinished deliveries to pending (the stream
// closed before they were confirmed or acknowledged). They keep the holder
// as who had them last, so a reconnecting client's Last-Event-ID can
// confirm the one it saw last (Resend). The agents whose deliveries are
// pending again are notified (on commit): a stream that connected meanwhile
// doesn't poll, so it takes them only when told.
func Release(ctx context.Context, tx pgx.Tx, holder string) error {
	_, err := tx.Exec(ctx, `WITH released AS (
			UPDATE deliveries SET state = CASE WHEN attempts >= $2 THEN 'dead' ELSE 'pending' END,
				finished_at = CASE WHEN attempts >= $2 THEN now() END, lease_until = NULL
			WHERE lease_holder = $1 AND state = 'in_flight'
			RETURNING agent_id, state
		)
		SELECT pg_notify($3, current_tenant()::text || ':' || agent_id) FROM (SELECT DISTINCT agent_id FROM released WHERE state = 'pending') a`,
		holder, MaxAttempts, NotifyChannel)
	return err
}

// ResendLost returns to pending what holder (a connection without acks that
// was lost) marked delivered to agent within the last within: the
// connection may have died before it passed them on. It returns how many,
// and notifies the agent (on commit) if any, as Release does.
func ResendLost(ctx context.Context, tx pgx.Tx, agentID, holder string, within time.Duration) (int64, error) {
	within = min(within, RedeliverWindow) // past it, RedactDelivered may have taken the content
	tag, err := tx.Exec(ctx, `UPDATE deliveries SET state = 'pending', finished_at = NULL
		WHERE agent_id = $1 AND lease_holder = $2 AND state = 'delivered' AND finished_at > now() - $3::interval AND `+notRedacted,
		agentID, holder, within.String())
	if err != nil || tag.RowsAffected() == 0 {
		return 0, err
	}
	_, err = tx.Exec(ctx, `SELECT pg_notify($1, current_tenant()::text || ':' || $2)`, NotifyChannel, agentID)
	return tag.RowsAffected(), err
}

var uuidRE = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// Resend is a reconnecting client saying the delivery with ID after was the
// last it saw (SSE's Last-Event-ID), for a stream without acks: if prev
// (the connection it reconnects from) handed that one back unconfirmed, it
// was seen, so it's delivered; what prev wrote after it, within the last
// within, goes back to pending. Only that one is confirmed: a connection
// doesn't always write in event order (a paced delivery comes due after
// later events), so an earlier event isn't proof of an earlier write, and
// whatever else prev handed back comes again. Only what prev wrote goes
// again: what another connection or a tool result handed over (drained,
// with no holder) was received there. Without prev (the connection left
// no lease to take over), the connection is the one that wrote after,
// which keeps its holder; one that names no delivery written by a
// connection sends nothing again. It returns how many go again.
func Resend(ctx context.Context, tx pgx.Tx, agentID, target, after, prev string, within time.Duration) (int64, error) {
	within = min(within, RedeliverWindow)
	// A client's word for an ID: compared as a uuid (so the primary key
	// serves it), and anything else names no delivery.
	if !uuidRE.MatchString(after) {
		return 0, nil
	}
	if prev == "" {
		err := tx.QueryRow(ctx, `SELECT coalesce(lease_holder, '') FROM deliveries WHERE id = $3::uuid AND agent_id = $1 AND `+inQueue(target, "$2"),
			agentID, target, after).Scan(&prev)
		if errors.Is(err, pgx.ErrNoRows) || err == nil && prev == "" {
			return 0, nil
		}
		if err != nil {
			return 0, err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE deliveries SET state = 'delivered', finished_at = now()
		WHERE id = $3::uuid AND agent_id = $1 AND `+inQueue(target, "$2")+` AND state = 'pending' AND lease_holder = $4`,
		agentID, target, after, prev); err != nil {
		return 0, err
	}
	tag, err := tx.Exec(ctx, `UPDATE deliveries SET state = 'pending', finished_at = NULL
		WHERE agent_id = $1 AND `+inQueue(target, "$2")+` AND state = 'delivered' AND finished_at > now() - $4::interval
		AND lease_holder = $5 AND `+notRedacted+`
		AND event_seq > (SELECT event_seq FROM deliveries WHERE id = $3::uuid AND agent_id = $1 AND `+inQueue(target, "$2")+`)`,
		agentID, target, after, within.String(), prev)
	return tag.RowsAffected(), err
}

// Reap returns deliveries whose lease expired to pending, or marks them dead
// after MaxAttempts. Like Release, it keeps the holder as who had them
// last: a wait's cursor (AckHolder) or a reconnecting client's
// Last-Event-ID (Resend) still confirms what that holder handed out,
// however long the agent took to come back. Every query that acts for a
// holder also checks the state, so a stale holder on a pending row lets it
// confirm only that. It returns how many it changed, and notifies the
// agents whose deliveries are pending again (on commit), so their streams
// take them without polling.
func Reap(ctx context.Context, tx pgx.Tx) (int64, error) {
	var n int64
	err := tx.QueryRow(ctx, `WITH reaped AS (
			UPDATE deliveries SET state = CASE WHEN attempts >= $1 THEN 'dead' ELSE 'pending' END,
				finished_at = CASE WHEN attempts >= $1 THEN now() END,
				last_error = 'lease expired', lease_until = NULL
			WHERE state = 'in_flight' AND lease_until < now()
			RETURNING agent_id, state
		), woken AS (
			SELECT pg_notify($2, current_tenant()::text || ':' || agent_id) FROM (SELECT DISTINCT agent_id FROM reaped WHERE state = 'pending') a
		)
		SELECT (SELECT count(*) FROM reaped), (SELECT count(*) FROM woken)`, MaxAttempts, NotifyChannel).Scan(&n, new(int64))
	return n, err
}

// Get returns a delivery's state.
func Get(ctx context.Context, tx pgx.Tx, id string) (Delivery, error) {
	var d Delivery
	var eventID string
	err := tx.QueryRow(ctx, `SELECT id, agent_id, target, subscription_id, state, attempts, event_id FROM deliveries WHERE id = $1`, id).
		Scan(&d.ID, &d.AgentID, &d.Target, &d.SubscriptionID, &d.State, &d.Attempts, &eventID)
	if err != nil {
		return d, err
	}
	d.Event, err = events.Get(ctx, tx, eventID)
	return d, err
}

// Retention is how long finished deliveries (delivered, acked, dead,
// canceled) and old events are kept: long enough for the console's activity
// and event log to explain recent wake-ups, and far past any redelivery
// window (Resend and ResendLost look back minutes). It's also how long a
// message is kept for an agent that never takes it: past it, its deliveries
// still owed and the event go (Prune). An event that asks
// (DeleteOnDelivery) loses its content much sooner: RedactDelivered.
const Retention = 30 * 24 * time.Hour

// RedeliverWindow is how far back Resend and ResendLost send again what
// was written to a consumer that can't acknowledge (state delivered, with
// the connection that wrote it as lease holder): the connection may have
// died before passing it on.
const RedeliverWindow = 5 * time.Minute

// notRedacted is the condition, on a deliveries row (in an UPDATE of
// deliveries), that its event still has its content: an event whose
// content was deleted after delivery is never queued again.
const notRedacted = `NOT EXISTS (SELECT 1 FROM events e WHERE e.tenant_id = deliveries.tenant_id AND e.id = deliveries.event_id AND e.redacted_at IS NOT NULL)`

// RedactDelivered deletes the content (subject, body, data) of up to limit
// of the workspace's events that ask for it (events.Event.DeleteOnDelivery)
// and have been delivered, keeping their metadata: IDs (for dedupe, cursors
// and the console's activity), type, source, times and who they were from.
// It returns how many it redacted. The expiry pass runs it; acks and
// Drain redact their own events at once (redact).
//
// An event is delivered when every subscription to its type has read past
// it (the pipeline makes no more deliveries of it), none of its deliveries is still
// owed (pending or in flight), at least one reached its consumer (acked,
// or delivered), and none written to a consumer that can't acknowledge is
// still within RedeliverWindow (it may be sent again). Deliveries that
// all ended without reaching anyone (dead, canceled) leave the content to
// Retention, as any undelivered event's.
func RedactDelivered(ctx context.Context, tx pgx.Tx, limit int) (int64, error) {
	return redactWhere(ctx, tx, `true`, nil, limit)
}

// redact is RedactDelivered for the events named (just acknowledged or
// handed over), in the caller's transaction. Another delivery of one of
// them finishing concurrently may leave it to the expiry pass.
func redact(ctx context.Context, tx pgx.Tx, eventIDs []string) error {
	if len(eventIDs) == 0 {
		return nil
	}
	_, err := redactWhere(ctx, tx, `e.id = ANY($3::uuid[])`, eventIDs, len(eventIDs))
	return err
}

func redactWhere(ctx context.Context, tx pgx.Tx, where string, ids []string, limit int) (int64, error) {
	args := []any{RedeliverWindow.String(), limit}
	if ids != nil {
		args = append(args, ids)
	}
	tag, err := tx.Exec(ctx, `WITH done AS (
			SELECT e.tenant_id, e.seq FROM events e
			WHERE e.delete_on_delivery AND e.redacted_at IS NULL AND `+where+`
			AND EXISTS (SELECT 1 FROM deliveries d WHERE d.tenant_id = e.tenant_id AND d.event_id = e.id AND d.state IN ('delivered', 'acked'))
			AND NOT EXISTS (SELECT 1 FROM deliveries d WHERE d.tenant_id = e.tenant_id AND d.event_id = e.id
				AND (d.state IN ('pending', 'in_flight') OR d.state = 'delivered' AND d.lease_holder IS NOT NULL AND d.finished_at > now() - $1::interval))
			AND NOT EXISTS (SELECT 1 FROM subscription_cursors c
				JOIN subscriptions s ON s.tenant_id = c.tenant_id AND s.id = c.subscription_id
				WHERE s.status = 'active' AND c.last_seq < e.seq
				AND (s.event_type = e.type OR right(s.event_type, 1) = '*' AND starts_with(e.type, left(s.event_type, -1))))
			ORDER BY e.seq LIMIT $2
			FOR UPDATE OF e SKIP LOCKED
		)
		UPDATE events SET subject = '', body = '', data = NULL, redacted_at = now()
		FROM done WHERE events.tenant_id = done.tenant_id AND events.seq = done.seq`, args...)
	return tag.RowsAffected(), err
}

// Prune deletes what's past keep in the workspace, up to limit of each kind:
// deliveries that finished more than keep ago; deliveries still owed
// (pending, or in flight with a lapsed lease) whose event arrived more than
// keep ago, so a message nobody received goes keep after it arrived; and
// events that arrived more than keep ago that no delivery names, whether or
// not every subscription has read them (a subscription that lags that far
// skips them). A delivery leased right now is left to finish or lapse
// first: rows are locked as they're chosen (skipping any a stream holds),
// so a lease renewed or a claim made meanwhile is seen, not deleted. It
// returns how many deliveries and events it deleted.
func Prune(ctx context.Context, tx pgx.Tx, keep time.Duration, limit int) (deliveries, evs int64, err error) {
	tag, err := tx.Exec(ctx, `DELETE FROM deliveries WHERE (tenant_id, id) IN (
			SELECT tenant_id, id FROM deliveries
			WHERE state IN ('delivered', 'acked', 'dead', 'cancelled') AND finished_at < now() - $1::interval
			LIMIT $2 FOR UPDATE SKIP LOCKED)`, keep.String(), limit)
	if err != nil {
		return 0, 0, err
	}
	deliveries = tag.RowsAffected()
	tag, err = tx.Exec(ctx, `DELETE FROM deliveries WHERE (tenant_id, id) IN (
			SELECT d.tenant_id, d.id FROM deliveries d JOIN events e ON e.tenant_id = d.tenant_id AND e.id = d.event_id
			WHERE (d.state = 'pending' OR d.state = 'in_flight' AND d.lease_until < now())
			AND e.received_at < now() - $1::interval
			LIMIT $2 FOR UPDATE OF d SKIP LOCKED)`, keep.String(), limit)
	if err != nil {
		return deliveries, 0, err
	}
	deliveries += tag.RowsAffected()
	tag, err = tx.Exec(ctx, `DELETE FROM events WHERE (tenant_id, seq) IN (
			SELECT e.tenant_id, e.seq FROM events e
			WHERE e.received_at < now() - $1::interval
			AND NOT EXISTS (SELECT 1 FROM deliveries d WHERE d.tenant_id = e.tenant_id AND d.event_id = e.id)
			ORDER BY e.received_at LIMIT $2)`, keep.String(), limit)
	if err != nil {
		return deliveries, 0, err
	}
	return deliveries, tag.RowsAffected(), nil
}

// HeldBy is what holder (an MCP wait task's lease holder) was handed from
// the stream with target, oldest first, at most limit: in flight, or
// confirmed or delivered since, without content that's gone (redacted).
// The task's result, for every poll after it completed: a task is nothing
// but its ID, so its batch is found again by its holder.
func HeldBy(ctx context.Context, tx pgx.Tx, agentID, target, holder string, limit int) ([]Delivery, error) {
	rows, err := tx.Query(ctx, `SELECT id::text, event_id::text, subscription_id::text, attempts, state FROM deliveries
		WHERE agent_id = $1 AND target = $2 AND lease_holder = $3 AND state IN ('in_flight', 'acked', 'delivered')
		ORDER BY event_seq LIMIT $4`, agentID, target, holder, limit)
	if err != nil {
		return nil, err
	}
	type held struct {
		ID, EventID, SubscriptionID string
		Attempts                    int
		State                       string
	}
	list, err := pgx.CollectRows(rows, pgx.RowToStructByPos[held])
	if err != nil {
		return nil, err
	}
	out := make([]Delivery, 0, len(list))
	for _, h := range list {
		e, err := events.Get(ctx, tx, h.EventID)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if e.Redacted {
			continue
		}
		out = append(out, Delivery{ID: h.ID, AgentID: agentID, Target: target, SubscriptionID: h.SubscriptionID, State: h.State, Attempts: h.Attempts, Event: e})
	}
	return out, nil
}
