package appapi

// Multi-recipient emit: one occurrence of a personal event to many recipients in one
// request, each one of
//
//	"user"                          every subscription that user has to the event with these arguments
//	{"user": u, "subscription": id} exactly that subscription (the ID a subscribe returned), if it's u's
//	{"native": u}                   only u's native (webhook) subscriptions to it with these arguments
//
// User recipients go to their personal topics, as a single-user emit does,
// under striped locks instead of one lock per topic. Subscription and native
// recipients are direct: each gets its own event in its workspace, limited
// to its subscription (subscriptions.ForAttr), found through its user's
// workspaces (userWorkspaces, as eventSubEnd finds an ID): there's no
// platform-level index of event subscriptions. The emit is recorded once
// (topic_events, with its recipients, so a retry uses them) and copied into
// each workspace in a transaction of its own (topic_fanout, fanOut), as a
// publish is.
//
// Locks, in order: the dedupe lock, the app's counter row, the stripes in
// ascending order, then rows.
// A subscribe to a personal topic takes its topic lock, then its stripe.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/apps"
)

// maxRecipients is the most recipients one emit may name.
const maxRecipients = 1000

// maxListBody is the request body /events/emit and /events/status read,
// instead of httpx's 64 KB: room for their lists of 1,000. 1,000
// {"user", "subscription"} recipients with the longest user IDs (200
// characters) are ~265 KB, and the emit's payload (maxTopicData, 64 KB) and
// text come on top. A 1,001st recipient is still refused by count
// (maxRecipients, maxStatusIDs), and the payload by its own limit.
const maxListBody = 512 << 10

// topicStripes is how many stripe locks an app's personal topics share.
const topicStripes = 64

// recipient is one of a multi-recipient emit's recipients, as recorded.
type recipient struct {
	User         string `json:"user"`
	Subscription string `json:"subscription,omitempty"`
	Native       bool   `json:"native,omitempty"`
	// Topic is a user or native recipient's personal topic for the emit's
	// arguments (apps.PersonalTopic).
	Topic string `json:"topic,omitempty"`
}

// key names the recipient in an emit's results.
func (r recipient) key() string {
	switch {
	case r.Subscription != "":
		return "s:" + r.Subscription + "/" + r.User
	case r.Native:
		return "n:" + r.User
	}
	return "u:" + r.User
}

// direct reports whether the recipient is reached by direct copies.
func (r recipient) direct() bool { return r.Subscription != "" || r.Native }

// emitRecord is topic_events.recipients for a multi-recipient emit.
type emitRecord struct {
	Event string      `json:"event"`
	To    []recipient `json:"to"`
}

// emitResults is topic_events.results: the recipients its copies reached.
type emitResults struct {
	Found []string `json:"found"`
}

// directEntry is one direct recipient to look for in a workspace
// (topic_fanout.direct), with the filters its user's workspace takes
// (userWorkspace).
type directEntry struct {
	recipient
	Agent     string `json:"agent,omitempty"`
	Principal string `json:"principal,omitempty"`
}

// emitRateError: the app's multi-recipient emits named as many recipients
// in the last minute as it may. Refusals answer with message, which names
// the server's support address.
type emitRateError struct{ limit int }

func (e emitRateError) Error() string {
	return fmt.Sprintf("this app's emits named %d recipients in the last minute, its limit", e.limit)
}

func (e emitRateError) message(support string) string {
	return fmt.Sprintf("this app's emits named %d recipients in the last minute, its limit; try again in a minute, or write to %s to raise it", e.limit, apps.Support(support))
}

// parseRecipients reads an emit's to: one user (as before), or a list of
// users, {user, subscription} and {native} recipients. single is set when
// it's one plain user, which keeps the single-user path exactly.
func parseRecipients(raw json.RawMessage) (list []recipient, single bool, err error) {
	raw = json.RawMessage(strings.TrimSpace(string(raw)))
	if len(raw) == 0 || string(raw) == "null" || string(raw) == `""` {
		return nil, false, nil
	}
	var items []json.RawMessage
	switch raw[0] {
	case '"':
		var u string
		if err := json.Unmarshal(raw, &u); err != nil {
			return nil, false, apps.EventArgsError{Msg: "to is a user, a recipient or a list of them"}
		}
		return []recipient{{User: strings.TrimSpace(u)}}, true, nil
	case '[':
		if err := json.Unmarshal(raw, &items); err != nil {
			return nil, false, apps.EventArgsError{Msg: "to is a user, a recipient or a list of them"}
		}
		if len(items) == 0 {
			return nil, false, apps.EventArgsError{Msg: "to is empty: name at least one recipient"}
		}
	default:
		items = []json.RawMessage{raw}
	}
	if len(items) > maxRecipients {
		return nil, false, apps.EventArgsError{Msg: fmt.Sprintf("at most %d recipients in one emit", maxRecipients)}
	}
	seen := map[string]bool{}
	for _, it := range items {
		r, err := parseRecipient(it)
		if err != nil {
			return nil, false, err
		}
		if seen[r.key()] {
			continue
		}
		seen[r.key()] = true
		list = append(list, r)
	}
	return list, len(list) == 1 && !list[0].direct(), nil
}

func parseRecipient(raw json.RawMessage) (recipient, error) {
	bad := apps.EventArgsError{Msg: `each recipient is a user ("user"), {"user": …, "subscription": …} or {"native": user}`}
	var r recipient
	if len(raw) > 0 && raw[0] == '"' {
		if err := json.Unmarshal(raw, &r.User); err != nil {
			return r, bad
		}
	} else {
		var in struct {
			User         *string `json:"user"`
			Subscription *string `json:"subscription"`
			Native       *string `json:"native"`
		}
		dec := json.NewDecoder(strings.NewReader(string(raw)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&in); err != nil {
			return r, bad
		}
		switch {
		case in.Native != nil && in.User == nil && in.Subscription == nil:
			r.User, r.Native = *in.Native, true
		case in.Native == nil && in.User != nil:
			r.User = *in.User
			if in.Subscription != nil {
				r.Subscription = strings.TrimSpace(*in.Subscription)
				if !uuidRE.MatchString(r.Subscription) {
					return r, apps.EventArgsError{Msg: "a recipient's subscription is the id a subscribe returned"}
				}
			}
		default:
			return r, bad
		}
	}
	r.User = strings.TrimSpace(r.User)
	if !appUser(r.User) {
		return r, apps.EventArgsError{Msg: "each recipient's user is the app's ID for the person (at most 200 characters, not starting with ~)"}
	}
	return r, nil
}

// emitOut is a multi-recipient emit's response.
type emitOut struct {
	Matched   int  `json:"matched"`
	Duplicate bool `json:"duplicate"`
	// Unmatched: user and native recipients with no live subscription
	// reached ("user", or {"native": user}).
	Unmatched []any `json:"unmatched"`
	// Ended: subscription recipients that aren't live (or aren't that
	// user's, or the event's).
	Ended []map[string]string `json:"ended"`
	// TextTrimmed: the text was over maxTopicText bytes, so Wakeshift cut
	// it (trimText). Omitted otherwise.
	TextTrimmed bool `json:"text_trimmed,omitempty"`
}

// emitToMany records a multi-recipient emit of et (base: the topic its
// arguments render) and makes its copies; a repeated id within a day
// returns what the first matched, and finishes its copies, if it's the same
// emit (checkRepeat).
func (a *API) emitToMany(ctx context.Context, app apps.App, et apps.EventType, base string, list []recipient, ev topicEvent) (emitOut, error) {
	var topics []string
	for i := range list {
		if list[i].Subscription != "" {
			continue
		}
		list[i].Topic = apps.PersonalTopic(base, list[i].User)
		if err := checkTopic(list[i].Topic); err != nil {
			return emitOut{}, err
		}
		if !list[i].Native {
			topics = append(topics, list[i].Topic)
		}
	}
	slices.Sort(topics)
	topics = slices.Compact(topics)
	var out emitOut
	var sourceID string
	err := a.appTx(ctx, func(tx pgx.Tx) error {
		if ev.ID != "" {
			sourceID = ev.ID
			// The same lock and record as publishTopics: an id dedupes a
			// publish, a single-user emit and a multi-recipient one alike.
			found, err := checkRepeat(ctx, tx, app.ID, ev)
			if err != nil {
				return err
			}
			if found {
				out.Duplicate = true
				return nil
			}
		}
		// The app's counter: refused here, the transaction rolls back, so a
		// refused emit isn't counted and records nothing. A transaction that
		// began in the minute before the row's never moves its window back.
		var n int
		if err := tx.QueryRow(ctx, `INSERT INTO app_emit_counters (app_id, window_start, recipients) VALUES ($1, date_trunc('minute', now()), $2)
			ON CONFLICT (app_id) DO UPDATE SET recipients = CASE WHEN excluded.window_start > app_emit_counters.window_start
				THEN excluded.recipients ELSE app_emit_counters.recipients + excluded.recipients END,
				window_start = greatest(app_emit_counters.window_start, excluded.window_start)
			RETURNING recipients`, app.ID, len(list)).Scan(&n); err != nil {
			return err
		}
		if limit := apps.EmitRecipientsPerMinute(app); n > limit {
			return emitRateError{limit}
		}
		if err := lockStripes(ctx, tx, app.ID, topics); err != nil {
			return err
		}
		direct, err := a.directEntries(ctx, tx, app, list)
		if err != nil {
			return err
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
		record, _ := json.Marshal(emitRecord{Event: et.Name, To: list})
		if topics == nil {
			topics = []string{}
		}
		if _, err := tx.Exec(ctx, `INSERT INTO topic_events (app_id, source_id, event_id, topics, text, data, matched, delete_on_delivery, recipients, results, fingerprint, text_fingerprint)
			VALUES ($1, $2, $3, $4, $5, $6, 0, $7, $8, '{"found": []}', $9, $10)`, app.ID, sourceID, eventID, topics, ev.Text, data, ev.deleteOnDelivery, record, ev.fingerprint, ev.textFingerprint); err != nil {
			return err
		}
		if len(topics) > 0 {
			if _, err := tx.Exec(ctx, `INSERT INTO topic_fanout (app_id, source_id, tenant_id, subscriptions)
				SELECT $1, $2, tenant_id, array_agg(subscription_id) FROM topic_members WHERE app_id = $1 AND topic = ANY($3) GROUP BY tenant_id`,
				app.ID, sourceID, topics); err != nil {
				return err
			}
		}
		tenants := make([]string, 0, len(direct))
		for t := range direct {
			tenants = append(tenants, t)
		}
		slices.Sort(tenants)
		for _, t := range tenants {
			b, _ := json.Marshal(direct[t])
			if _, err := tx.Exec(ctx, `INSERT INTO topic_fanout (app_id, source_id, tenant_id, subscriptions, direct) VALUES ($1, $2, $3, '{}', $4)
				ON CONFLICT (app_id, source_id, tenant_id) DO UPDATE SET direct = excluded.direct`, app.ID, sourceID, t, b); err != nil {
				return err
			}
		}
		// The app's publishes over a day old, as publishTopics deletes them.
		_, err = tx.Exec(ctx, `DELETE FROM topic_events WHERE ctid IN (SELECT ctid FROM topic_events
			WHERE app_id = $1 AND published_at < now() - interval '1 day' LIMIT $2 FOR UPDATE SKIP LOCKED)`, app.ID, expireOnPublish)
		return err
	})
	if err != nil {
		return emitOut{}, err
	}
	err = fanOut(ctx, a.DB, app.ID, sourceID)
	res, rerr := emitResult(ctx, a.DB.Pool, app.ID, sourceID)
	res.Duplicate = out.Duplicate
	res.TextTrimmed = ev.trimmed
	if err == nil {
		err = rerr
	}
	return res, err
}

// emitResult is what a recorded emit matched, from its record: the same
// for a duplicate as for the first.
func emitResult(ctx context.Context, q interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}, app, sourceID string) (emitOut, error) {
	out := emitOut{Unmatched: []any{}, Ended: []map[string]string{}}
	var record, results []byte
	if err := q.QueryRow(ctx, `SELECT matched, recipients, results FROM topic_events WHERE app_id = $1 AND source_id = $2`, app, sourceID).
		Scan(&out.Matched, &record, &results); err != nil {
		return out, err
	}
	if record == nil {
		return out, nil // a publish or a single-user emit with this id
	}
	var rec emitRecord
	var res emitResults
	if err := json.Unmarshal(record, &rec); err != nil {
		return out, err
	}
	if results != nil {
		if err := json.Unmarshal(results, &res); err != nil {
			return out, err
		}
	}
	for _, r := range rec.To {
		if slices.Contains(res.Found, r.key()) {
			continue
		}
		switch {
		case r.Subscription != "":
			out.Ended = append(out.Ended, map[string]string{"user": r.User, "subscription": r.Subscription})
		case r.Native:
			out.Unmatched = append(out.Unmatched, map[string]string{"native": r.User})
		default:
			out.Unmatched = append(out.Unmatched, r.User)
		}
	}
	return out, nil
}

// lockStripes takes the stripe locks of topics, shared, in ascending order:
// a subscribe to one of them takes its stripe's exclusively (lockStripe),
// so it either finished before this (and its subscription is listed) or
// starts after (and replays this).
func lockStripes(ctx context.Context, tx pgx.Tx, app string, topics []string) error {
	if len(topics) == 0 {
		return nil
	}
	rows, _ := tx.Query(ctx, `SELECT DISTINCT abs(hashtext(t)::bigint) % $2 FROM unnest($1::text[]) t ORDER BY 1`, topics, topicStripes)
	stripes, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return err
	}
	for _, s := range stripes {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock_shared(hashtext('topic-stripe:' || $1 || ':' || $2))`, app, strconv.FormatInt(s, 10)); err != nil {
			return err
		}
	}
	return nil
}

// personalTopicRE is a personal event's topic (apps.PersonalTopic).
var personalTopicRE = regexp.MustCompile(`^u/p[0-9a-f]{30}/`)

// lockStripe takes a personal topic's stripe lock exclusively, after its
// topic lock (lockStripes says why); other topics have none.
func lockStripe(ctx context.Context, tx pgx.Tx, app, topic string) error {
	if !personalTopicRE.MatchString(topic) {
		return nil
	}
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('topic-stripe:' || $1 || ':' || (abs(hashtext($2)::bigint) % $3)::text))`, app, topic, topicStripes)
	return err
}

// directEntries are the direct recipients of list by workspace: each user's
// workspaces (userWorkspaces, for many users at once), where its copy's
// transaction looks for its subscription. A user with no workspace is in
// none: its subscription is reported ended, its native recipient unmatched.
func (a *API) directEntries(ctx context.Context, tx pgx.Tx, app apps.App, list []recipient) (map[string][]directEntry, error) {
	var users []string
	for _, r := range list {
		if r.direct() && !slices.Contains(users, r.User) {
			users = append(users, r.User)
		}
	}
	if len(users) == 0 {
		return nil, nil
	}
	where, err := usersWorkspaces(ctx, tx, app, users)
	if err != nil {
		return nil, err
	}
	out := map[string][]directEntry{}
	for _, r := range list {
		if !r.direct() {
			continue
		}
		for _, uw := range where[r.User] {
			out[uw.tenant] = append(out[uw.tenant], directEntry{recipient: r, Agent: uw.agent, Principal: uw.principal})
		}
	}
	return out, nil
}

// usersWorkspaces is userWorkspaces for many users, in two queries.
func usersWorkspaces(ctx context.Context, tx pgx.Tx, app apps.App, users []string) (map[string][]userWorkspace, error) {
	out := map[string][]userWorkspace{}
	rows, _ := tx.Query(ctx, `SELECT id::text, external_id FROM tenants WHERE app_id = $1 AND external_id = ANY($2)`, app.ID, users)
	type own struct{ Tenant, User string }
	owns, err := pgx.CollectRows(rows, pgx.RowToStructByPos[own])
	if err != nil {
		return nil, err
	}
	for _, o := range owns {
		out[o.User] = append(out[o.User], userWorkspace{tenant: o.Tenant})
	}
	// A user of the form "<workspace>/<agent>" naming a workspace the app
	// keeps: that agent's subscriptions there, made as that user.
	agentOf := map[string][]string{}
	var spaces []string
	for _, u := range users {
		ws, ag, ok := strings.Cut(u, "/")
		if !ok || !uuidRE.MatchString(ws) || ag == "" {
			continue
		}
		agentOf[ws] = append(agentOf[ws], u)
		if !slices.Contains(spaces, ws) {
			spaces = append(spaces, ws)
		}
	}
	if len(spaces) == 0 {
		return out, nil
	}
	rows, _ = tx.Query(ctx, `SELECT id::text FROM tenants WHERE id = ANY($1::uuid[]) AND app_id = $2`, spaces, app.ID)
	kept, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	for _, t := range kept {
		for _, u := range agentOf[t] {
			_, ag, _ := strings.Cut(u, "/")
			out[u] = append(out[u], userWorkspace{tenant: t, agent: ag, principal: u})
		}
	}
	return out, nil
}

// idReusedError: an emit repeated an id the app used within the last day
// for a different emit (another event, arguments, recipients or payload).
type idReusedError struct{ event string }

func (e idReusedError) Error() string {
	return fmt.Sprintf("%s: this id was already used within the last day for a different emit (another event, arguments, recipients, payload or text), "+
		"so this one was not sent. Retry with exactly the same emit, or give a new occurrence a new id", e.event)
}

// checkRepeat takes ev.ID's dedupe lock and reports whether the app
// recorded it within the last day: a retry, whose first result stands. A
// record with a different fingerprint, or text fingerprint, is another
// emit, refused (idReusedError); a record or an emit without one (a publish
// by URL, a row from before fingerprints or written by a revision without
// text fingerprints, a redacted one) matches on it.
func checkRepeat(ctx context.Context, tx pgx.Tx, app string, ev topicEvent) (bool, error) {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('topic-event:' || $1 || ':' || $2))`, app, ev.ID); err != nil {
		return false, err
	}
	// An id dedupes for a day: an older record of it (not pruned yet)
	// doesn't count.
	if _, err := tx.Exec(ctx, `DELETE FROM topic_events WHERE app_id = $1 AND source_id = $2 AND published_at < now() - interval '1 day'`, app, ev.ID); err != nil {
		return false, err
	}
	var stored, storedText []byte
	err := tx.QueryRow(ctx, `SELECT fingerprint, text_fingerprint FROM topic_events WHERE app_id = $1 AND source_id = $2`, app, ev.ID).Scan(&stored, &storedText)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	differs := func(a, b []byte) bool { return a != nil && b != nil && !bytes.Equal(a, b) }
	if differs(stored, ev.fingerprint) || differs(storedText, ev.textFingerprint) {
		return false, idReusedError{ev.event}
	}
	return true, nil
}

// emitFingerprint is what makes two emits with one id the same emit: its
// event, the topic its arguments render (before any recipient), its
// recipients as a set (recipient.key, so "u" and ["u"] are one) and its
// payload, as canonical JSON (keys sorted, numbers as written), hashed.
// Its text is fingerprinted apart (textFingerprint), in a column of its
// own, so a revision that compares only this one still reads it.
func emitFingerprint(event, target string, list []recipient, payload json.RawMessage) ([]byte, error) {
	var p any
	if len(payload) > 0 {
		dec := json.NewDecoder(bytes.NewReader(payload))
		dec.UseNumber()
		if err := dec.Decode(&p); err != nil {
			return nil, err
		}
	}
	to := make([]string, 0, len(list))
	for _, r := range list {
		to = append(to, r.key())
	}
	slices.Sort(to)
	to = slices.Compact(to)
	b, err := json.Marshal(struct {
		Event   string   `json:"event"`
		Target  string   `json:"target"`
		To      []string `json:"to"`
		Payload any      `json:"payload"`
	}{event, target, to, p})
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(b)
	return sum[:], nil
}

// textFingerprint is an emit's text, hashed (topic_events.text_fingerprint).
// The SDKs render the text from the payload, so their retries match; an
// HTTP caller's own text that differs makes another emit.
func textFingerprint(text string) []byte {
	sum := sha256.Sum256([]byte(text))
	return sum[:]
}
