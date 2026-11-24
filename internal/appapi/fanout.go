package appapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/db"
	"github.com/wakeshift/server/internal/events"
	"github.com/wakeshift/server/internal/mcpevents"
	"github.com/wakeshift/server/internal/subscriptions"
)

// expireOnPublish is the most of an app's day-old publishes one publish
// deletes: what's left goes at its next, or in the expiry pass
// (ExpireOldPublishes).
const expireOnPublish = 500

// publishTopics copies an event into every workspace with a subscriber to
// one of topics, ending subscriptions found expired, and records it for
// replay. A repeated event ID within a day returns what the first publish
// matched (and finishes its copies, if the first was interrupted), if it's
// the same emit (checkRepeat); after a day, the same ID publishes again.
//
// The publish is recorded first, with the workspaces it goes to and their
// subscribers at that moment (topic_fanout), in one short transaction; then
// each workspace gets its copy in a transaction of its own (fanOut). So a
// publish to many workspaces holds no workspace's event sequence for longer
// than its own copy, and a failure in one (a deadlock, a timeout) undoes
// none of the others: its copy is made again by a retry of the publish with
// the same ID, or by the expiry pass (FinishFanouts). The call returns once
// every copy is made, so an app's publishes, one after another, arrive in
// that order in each workspace.
func (a *API) publishTopics(ctx context.Context, app string, topics []string, ev topicEvent, urlID string) (matched int, dup bool, err error) {
	slices.Sort(topics)
	topics = slices.Compact(topics)
	var sourceID string
	err = a.appTx(ctx, func(tx pgx.Tx) error {
		if ev.ID != "" {
			sourceID = ev.ID
			found, err := checkRepeat(ctx, tx, app, ev)
			if err != nil {
				return err
			}
			if found {
				dup = true
				return nil
			}
		}
		// Shared with other publishes, not with a subscribe to one of the
		// topics in progress (lockTopic), in topic order: a subscribe
		// either finished before this (and its subscription is listed
		// below) or starts after (and replays this).
		for _, t := range topics {
			if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock_shared(hashtext('topic-members:' || $1 || ':' || $2))`, app, t); err != nil {
				return err
			}
		}
		if urlID != "" {
			if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('publish-url:' || $1))`, urlID); err != nil {
				return err
			}
			var recent int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM topic_events WHERE publish_url = $1 AND published_at > now() - interval '1 minute'`, urlID).Scan(&recent); err != nil {
				return err
			}
			if recent >= publishURLPerMinute {
				return errRateLimited
			}
		}
		var eventID string
		if err := tx.QueryRow(ctx, `SELECT gen_random_uuid()::text`).Scan(&eventID); err != nil {
			return err
		}
		if sourceID == "" {
			sourceID = eventID
		}
		var data []byte
		if len(ev.Data) > 0 {
			data = ev.Data
		}
		var url *string
		if urlID != "" {
			url = &urlID
		}
		if _, err := tx.Exec(ctx, `INSERT INTO topic_events (app_id, source_id, event_id, topics, text, data, matched, publish_url, delete_on_delivery, fingerprint, text_fingerprint)
			VALUES ($1, $2, $3, $4, $5, $6, 0, $7, $8, $9, $10)`, app, sourceID, eventID, topics, ev.Text, data, url, ev.deleteOnDelivery, ev.fingerprint, ev.textFingerprint); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO topic_fanout (app_id, source_id, tenant_id, subscriptions)
			SELECT $1, $2, tenant_id, array_agg(subscription_id) FROM topic_members WHERE app_id = $1 AND topic = ANY($3) GROUP BY tenant_id`,
			app, sourceID, topics); err != nil {
			return err
		}
		// The app's publishes over a day old, as many as one publish's
		// share: rows another publish (or the expiry pass) is deleting are
		// skipped, not waited for. Two of the app's publishes at once each
		// deleted the same rows, in their own order, and deadlocked.
		_, err := tx.Exec(ctx, `DELETE FROM topic_events WHERE ctid IN (SELECT ctid FROM topic_events
			WHERE app_id = $1 AND published_at < now() - interval '1 day' LIMIT $2 FOR UPDATE SKIP LOCKED)`, app, expireOnPublish)
		return err
	})
	if err != nil {
		return 0, false, err
	}
	err = fanOut(ctx, a.DB, app, sourceID)
	if qerr := a.DB.Pool.QueryRow(ctx, `SELECT matched FROM topic_events WHERE app_id = $1 AND source_id = $2`, app, sourceID).Scan(&matched); qerr != nil && err == nil {
		err = qerr
	}
	return matched, dup, err
}

// fanOut makes the copies of a recorded publish that are still to be made,
// one workspace per transaction, in workspace order. Every workspace is
// tried; the first failure is returned (its copy stays to be made).
func fanOut(ctx context.Context, d *db.DB, app, sourceID string) error {
	rows, _ := d.Pool.Query(ctx, `SELECT tenant_id::text FROM topic_fanout WHERE app_id = $1 AND source_id = $2 ORDER BY tenant_id`, app, sourceID)
	tenants, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	var first error
	for _, t := range tenants {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		err := pgx.BeginFunc(ctx, d.Pool, func(tx pgx.Tx) error { return copyInto(ctx, tx, app, sourceID, t) })
		if err != nil {
			slog.Warn("topics: copying a publish into a workspace failed (made again by a retry or the expiry pass)", "app", app, "tenant", t, "err", err)
			if first == nil {
				first = err
			}
		}
	}
	return first
}

// copyInto makes a publish's copy in one workspace, if it's still to be
// made: the workspace's topic_fanout row is held until this commits (a
// concurrent retry waits, then finds it gone), and deleted with it. For a
// multi-recipient emit it also makes the direct copies (topic_fanout.direct):
// one event per subscription found, limited to it (ForAttr), unless the
// topic copy or another direct copy already wakes its conversation.
//
// Its steps keep the order the deadlock fixes need: the platform rows
// first (claimCopy), then the workspace's subscriptions; every lapsed one
// locked before the first ends; the ttls of those it reaches slid before
// anything is appended (a copy, or a lapsed one's notice); then the lapsed
// ones end, the copies are appended, and the publish's results recorded.
func copyInto(ctx context.Context, tx pgx.Tx, app, sourceID, tenant string) error {
	p, ok, err := claimCopy(ctx, tx, app, sourceID, tenant)
	if err != nil || !ok {
		return err
	}
	if err := setTenant(ctx, tx, tenant); err != nil {
		return err
	}
	list, err := liveTopicSubs(ctx, tx, `ts.app_id = $1 AND ts.topic = ANY($2) AND ts.subscription_id = ANY($3::uuid[])`, app, p.topics, p.subs)
	if err != nil {
		return err
	}
	// The direct recipients' subscriptions, found before anything is
	// ended or appended.
	named := make([][]liveTopicSub, len(p.direct))
	for i, d := range p.direct {
		if named[i], err = directSubs(ctx, tx, app, p.rec.Event, d); err != nil {
			return err
		}
	}
	r := p.route(list, named)
	if err := lockEnding(ctx, tx, topicSubIDs(r.lapsed)); err != nil {
		return err
	}
	if len(r.lapsed) > 0 {
		// Read again now they're locked: one renewed since (a subscribe
		// started its ttl again) is live, and reached; one ended since is
		// left out.
		now, err := liveNow(ctx, tx, topicSubIDs(r.lapsed))
		if err != nil {
			return err
		}
		current := func(l []liveTopicSub) []liveTopicSub {
			out := make([]liveTopicSub, 0, len(l))
			for _, s := range l {
				if lapsed(s) {
					var ok bool
					if s, ok = now[s.ID]; !ok {
						continue
					}
				}
				out = append(out, s)
			}
			return out
		}
		list = current(list)
		for i := range named {
			named[i] = current(named[i])
		}
		r = p.route(list, named) // its lapsed ones: those still lapsed, all locked above
	}
	// The ttls of the subscriptions it reaches start again before anything
	// is appended (a copy, or a lapsed one's notice, below): slid after an
	// append, the workspace's event counter is held while waiting on a
	// subscription's row that ending it (a revoke, event.end, the expiry
	// pass) holds while waiting on the counter for its notice.
	for _, s := range slices.Concat(r.reached, r.directs) {
		if err := slideTopicSub(ctx, tx, s.ID); err != nil {
			return err
		}
	}
	// Then the lapsed ones end, telling their conversations. (A session it
	// reaches is woken once, so nothing above was slid twice either.)
	for _, s := range r.lapsed {
		reason, notice := endTTL, true
		if s.Status != subscriptions.Active { // ended once, or with its grant
			reason, notice = endedWithoutUs(s), false
		}
		if err := endTopicSub(ctx, tx, app, s, reason, notice); err != nil {
			return err
		}
	}
	if err := p.appendCopies(ctx, tx, r.here, r.directs); err != nil {
		return err
	}
	return p.finish(ctx, tx, len(r.woken), r.found)
}

// pendingCopy is a publish's copy still to be made in one workspace, as
// claimCopy read it.
type pendingCopy struct {
	app, sourceID, tenant string
	subs                  []string      // the topic's subscriptions here, found at publish
	direct                []directEntry // a multi-recipient emit's direct recipients here
	rec                   emitRecord    // a multi-recipient emit's record
	eventID, text         string
	topics                []string
	data                  []byte
	deleteOnDelivery      bool
	publishURL            string // the publish URL it came through, or ""
}

// claimCopy locks the workspace's topic_fanout row, then the publish's
// topic_events row. ok is false if there's no copy to make: it was made
// meanwhile, or the publish's content expired (ExpirePublishContent)
// first, when the row is deleted with nothing copied, never an empty
// message.
func claimCopy(ctx context.Context, tx pgx.Tx, app, sourceID, tenant string) (p pendingCopy, ok bool, err error) {
	p = pendingCopy{app: app, sourceID: sourceID, tenant: tenant}
	var direct, record []byte
	err = tx.QueryRow(ctx, `SELECT subscriptions::text[], direct FROM topic_fanout WHERE app_id = $1 AND source_id = $2 AND tenant_id = $3 FOR UPDATE`,
		app, sourceID, tenant).Scan(&p.subs, &direct)
	if errors.Is(err, pgx.ErrNoRows) {
		return p, false, nil // made meanwhile
	}
	if err != nil {
		return p, false, err
	}
	var redacted bool
	// Locked: the copies of a publish whose content goes once they're made
	// (redactPublish) are made one at a time, so the last one sees it's last;
	// and so are an emit's results, which each copy adds to.
	if err := tx.QueryRow(ctx, `SELECT event_id::text, topics, text, data, delete_on_delivery, redacted_at IS NOT NULL, recipients, coalesce(publish_url::text, '')
		FROM topic_events WHERE app_id = $1 AND source_id = $2 FOR UPDATE`, app, sourceID).
		Scan(&p.eventID, &p.topics, &p.text, &p.data, &p.deleteOnDelivery, &redacted, &record, &p.publishURL); err != nil {
		return p, false, err
	}
	if redacted {
		_, err := tx.Exec(ctx, `DELETE FROM topic_fanout WHERE app_id = $1 AND source_id = $2 AND tenant_id = $3`, app, sourceID, tenant)
		return p, false, err
	}
	if record != nil {
		// A multi-recipient emit.
		if err := json.Unmarshal(record, &p.rec); err != nil {
			return p, false, err
		}
		if direct != nil {
			if err := json.Unmarshal(direct, &p.direct); err != nil {
				return p, false, err
			}
		}
	}
	return p, true, nil
}

// copyRoutes is who a publish's copy in one workspace reaches.
type copyRoutes struct {
	lapsed  []liveTopicSub  // to end, each once (the topic's and a direct recipient's may be the same)
	here    []string        // the topics the topic copy names
	reached []liveTopicSub  // the topic's subscribers the topic copy reaches
	directs []liveTopicSub  // the direct recipients' subscriptions that get a copy of their own
	woken   map[string]bool // the sessions woken
	found   []string        // the emit's recipients reached (recipient.key)
}

// route works out who the copy reaches from the topic's live subscriptions
// (list) and each direct recipient's (named, by p.direct): it reads and
// writes nothing.
func (p pendingCopy) route(list []liveTopicSub, named [][]liveTopicSub) copyRoutes {
	r := copyRoutes{woken: map[string]bool{}}
	for _, s := range slices.Concat(append([][]liveTopicSub{list}, named...)...) {
		if lapsed(s) && !slices.ContainsFunc(r.lapsed, func(l liveTopicSub) bool { return l.ID == s.ID }) {
			r.lapsed = append(r.lapsed, s)
		}
	}
	for _, s := range list {
		if lapsed(s) || !dataMatches(p.app, s.Topic, s.Filters, p.data) {
			continue
		}
		r.woken[s.Session] = true
		if !slices.Contains(r.here, s.Topic) {
			r.here = append(r.here, s.Topic)
		}
		r.reached = append(r.reached, s)
	}
	for _, h := range r.here {
		// A multi-recipient emit's user recipients reached here.
		for _, rc := range p.rec.To {
			if !rc.direct() && rc.Topic == h {
				r.found = append(r.found, rc.key())
			}
		}
	}
	for i, d := range p.direct {
		for _, s := range named[i] {
			if lapsed(s) || d.Native && !dataMatches(p.app, s.Topic, s.Filters, p.data) {
				// A native recipient is reached as a user recipient would
				// be, only on its webhooks: their filters apply.
				continue
			}
			if !slices.Contains(r.found, d.key()) {
				r.found = append(r.found, d.key())
			}
			if r.woken[s.Session] {
				continue // the topic copy, or another direct copy, wakes it already
			}
			r.woken[s.Session] = true
			r.directs = append(r.directs, s)
		}
	}
	return r
}

// appendCopies appends the topic copy, naming the topics here, and a copy
// for each direct subscription.
func (p pendingCopy) appendCopies(ctx context.Context, tx pgx.Tx, here []string, directs []liveTopicSub) error {
	app := p.app
	if len(here) > 0 {
		// Only the topics this workspace subscribes to: another user's
		// private topic in the same publish isn't named here.
		attrs := map[string]string{"app": app, "topic": strings.Join(here, " ")}
		for _, h := range here {
			attrs[topicAttr(h)] = "1"
		}
		withAppEventID(attrs, p.sourceID, p.eventID, p.publishURL)
		// Keyed by this publish, not the app's id: topic_events dedupes
		// that for a day, and after it the same id publishes again (keyed
		// by the app's id, events_source_key would drop the copy in every
		// workspace that had it, while matched still counted them).
		if _, _, err := events.Append(ctx, tx, events.Event{ID: p.eventID, Type: topicsType(app), Source: "app:" + app, SourceEventID: "topic:" + p.eventID,
			From: events.Party{Kind: "app", ID: app}, Subject: p.text, Data: p.data, Attrs: attrs, Trust: events.External, DeleteOnDelivery: p.deleteOnDelivery}); err != nil {
			return err
		}
	}
	for _, s := range directs {
		// Its own event, with its own ID and source ID, so one per
		// subscription is kept; $emit names the emit, for billing.
		attrs := map[string]string{"app": app, "topic": s.Topic, topicAttr(s.Topic): "1", subscriptions.ForAttr: s.ID, EmitAttr: p.eventID}
		withAppEventID(attrs, p.sourceID, p.eventID, p.publishURL)
		if _, _, err := events.Append(ctx, tx, events.Event{Type: topicsType(app), Source: "app:" + app, SourceEventID: "emit:" + p.eventID + ":" + s.ID,
			From: events.Party{Kind: "app", ID: app}, Subject: p.text, Data: p.data, Trust: events.External, DeleteOnDelivery: p.deleteOnDelivery,
			Attrs: attrs}); err != nil {
			return err
		}
	}
	return nil
}

// withAppEventID records in a copy's attrs the upstream's own ID for the
// publish, if it gave one that can be an eventId (mcpevents.AppEventID): a
// spec client's occurrences carry it (mcpevents.Occurrence). It's the
// emit's id (sourceID), or for a post to a publish URL (publishURL, its
// ID) the sender's delivery ID, which sourceID holds after Wakeshift's
// "url:<publish URL>:" prefix. A publish with neither has its eventID as
// its sourceID.
func withAppEventID(attrs map[string]string, sourceID, eventID, publishURL string) {
	id := sourceID
	if publishURL != "" {
		var ok bool
		if id, ok = strings.CutPrefix(sourceID, "url:"+publishURL+":"); !ok {
			return
		}
	}
	if id != eventID && mcpevents.AppEventID(id) {
		attrs[mcpevents.AppEventIDAttr] = id
	}
}

// finish adds what the copy reached to the publish's results, deletes the
// workspace's topic_fanout row, and redacts the publish once its last copy
// is made, if its content goes once delivered.
func (p pendingCopy) finish(ctx context.Context, tx pgx.Tx, woken int, found []string) error {
	app, sourceID := p.app, p.sourceID
	if woken > 0 || len(found) > 0 {
		f, _ := json.Marshal(found)
		if _, err := tx.Exec(ctx, `UPDATE topic_events SET matched = matched + $3,
			results = CASE WHEN results IS NULL THEN NULL ELSE jsonb_set(results, '{found}', coalesce(results->'found', '[]') || $4::jsonb) END
			WHERE app_id = $1 AND source_id = $2`, app, sourceID, woken, f); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `DELETE FROM topic_fanout WHERE app_id = $1 AND source_id = $2 AND tenant_id = $3`, app, sourceID, p.tenant); err != nil || !p.deleteOnDelivery {
		return err
	}
	// The last copy made, into at least one workspace: the content lives on
	// only in those copies, each deleted once delivered. A publish nobody got
	// keeps it for replay to a later subscriber, for its day.
	var last bool
	if err := tx.QueryRow(ctx, `SELECT NOT EXISTS (SELECT 1 FROM topic_fanout WHERE app_id = $1 AND source_id = $2)
		AND (SELECT matched > 0 FROM topic_events WHERE app_id = $1 AND source_id = $2)`, app, sourceID).Scan(&last); err != nil || !last {
		return err
	}
	return redactPublish(ctx, tx, app, sourceID)
}

// EmitAttr, on a multi-recipient emit's direct copy, is the emit's event
// ID: the copies of one emit to several conversations of an agent are
// billed once (deliveries.RollUpUsage). Wakeshift's ("$"): no app sets it.
const EmitAttr = "$emit"

// directSubs are the topic subscriptions in the transaction's workspace
// that a direct recipient names and that haven't ended as far as
// topic_subscriptions knows: its subscription (the app's, to event, and its
// user's, as d's workspace filters say), or its user's native (webhook, poll, push)
// subscriptions on its personal topic. copyInto ends those found ended
// (lapsed, revoked, its one delivery made) and checks a native recipient's
// filters.
func directSubs(ctx context.Context, tx pgx.Tx, app, event string, d directEntry) ([]liveTopicSub, error) {
	var ids []string
	var err error
	if d.Subscription != "" {
		rows, _ := tx.Query(ctx, `SELECT es.subscription_id::text FROM event_subscriptions es WHERE es.id = $1::uuid AND es.source_kind = $2 AND es.source_id = $3
			AND es.event = $4 AND es.ended_at IS NULL AND ($5 = '' OR es.agent_id = $5) AND ($6 = '' OR es.principal = $6)`,
			d.Subscription, sourceApp, app, event, d.Agent, d.Principal)
		ids, err = pgx.CollectRows(rows, pgx.RowTo[string])
	} else {
		rows, _ := tx.Query(ctx, `SELECT es.subscription_id::text FROM event_subscriptions es
			JOIN topic_subscriptions ts ON ts.tenant_id = es.tenant_id AND ts.subscription_id = es.subscription_id
			WHERE es.source_kind = $1 AND es.source_id = $2 AND es.event = $3 AND es.destination IN ('webhook', 'poll', 'push') AND es.ended_at IS NULL
			AND es.principal = $4 AND ($5 = '' OR es.agent_id = $5) AND ts.topic = $6 ORDER BY es.created_at`,
			sourceApp, app, event, d.User, d.Agent, d.Topic)
		ids, err = pgx.CollectRows(rows, pgx.RowTo[string])
	}
	if err != nil || len(ids) == 0 {
		return nil, err
	}
	return liveTopicSubs(ctx, tx, `ts.app_id = $1 AND ts.subscription_id = ANY($2::uuid[])`, app, ids)
}

// redactPublish deletes the content of a publish whose event type asks for
// that once delivered (apps.EventType.DeleteOnDelivery), once it has been
// copied to whoever gets it: its row stays (event ID, topics, matched) for
// dedupe and the app's activity.
func redactPublish(ctx context.Context, tx pgx.Tx, app, sourceID string) error {
	_, err := tx.Exec(ctx, `UPDATE topic_events SET text = '', data = NULL, fingerprint = NULL, text_fingerprint = NULL, redacted_at = now()
		WHERE app_id = $1 AND source_id = $2 AND delete_on_delivery AND redacted_at IS NULL`, app, sourceID)
	return err
}

// ExpirePublishContent deletes the content of publishes that asked for it
// to go once delivered (apps.EventType.DeleteOnDelivery) and that nobody
// got within their day (replay reads at most a day back): what publishTopics
// would delete with the row at the app's next publish, if it ever comes.
func ExpirePublishContent(ctx context.Context, d *db.DB) (int64, error) {
	tag, err := d.Pool.Exec(ctx, `UPDATE topic_events SET text = '', data = NULL, fingerprint = NULL, text_fingerprint = NULL, redacted_at = now()
		WHERE delete_on_delivery AND redacted_at IS NULL AND published_at < now() - interval '1 day'`)
	return tag.RowsAffected(), err
}

// ExpireOldPublishes deletes publishes over a day old (replay and dedupe
// read at most a day back), whose copies are all made: what publishTopics
// deletes at the app's next publish, for apps that stop publishing, so a
// publish's content never outlives the day. A copy still not made after
// two days (FinishFanouts failing on it every pass) is given up, so it
// can't keep its publish. It deletes at most limit publishes (through
// topic_events_recent, per app).
func ExpireOldPublishes(ctx context.Context, d *db.DB, limit int) (int64, error) {
	// Rows a publish or another instance's pass is deleting are skipped
	// (FOR UPDATE SKIP LOCKED), not waited for: two deletes taking the same
	// rows in different orders deadlock.
	if _, err := d.Pool.Exec(ctx, `DELETE FROM topic_fanout WHERE ctid IN (
		SELECT ctid FROM topic_fanout WHERE created_at < now() - interval '2 days' LIMIT $1 FOR UPDATE SKIP LOCKED)`, limit); err != nil {
		return 0, err
	}
	tag, err := d.Pool.Exec(ctx, `DELETE FROM topic_events WHERE ctid IN (
		SELECT t.ctid FROM apps a JOIN topic_events t ON t.app_id = a.id AND t.published_at < now() - interval '1 day'
		WHERE NOT EXISTS (SELECT 1 FROM topic_fanout f WHERE f.app_id = t.app_id AND f.source_id = t.source_id)
		LIMIT $1 FOR UPDATE OF t SKIP LOCKED)`, limit)
	return tag.RowsAffected(), err
}

// FinishFanouts makes the copies of publishes interrupted part-way (for a
// minute or more: one in progress is left to finish), for the expiry pass.
// It returns how many publishes it finished. One that keeps failing doesn't
// stop the rest: it goes on to the others and returns the first failure.
func FinishFanouts(ctx context.Context, d *db.DB) (int, error) {
	type pending struct{ App, SourceID string }
	rows, _ := d.Pool.Query(ctx, `SELECT DISTINCT app_id, source_id FROM topic_fanout WHERE created_at < now() - $1::interval LIMIT 100`, time.Minute.String())
	list, err := pgx.CollectRows(rows, pgx.RowToStructByPos[pending])
	if err != nil {
		return 0, err
	}
	done := 0
	var first error
	for _, p := range list {
		if ctx.Err() != nil {
			return done, ctx.Err()
		}
		if err := fanOut(ctx, d, p.App, p.SourceID); err != nil {
			slog.Warn("topics: finishing an interrupted publish failed (retried on the next pass)", "app", p.App, "err", err)
			if first == nil {
				first = err
			}
			continue
		}
		done++
	}
	return done, first
}
