package appapi

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/apps"
	"github.com/wakeshift/server/internal/mcpevents"
)

// lapseSlack is how far before a cursor's event reached the workspace a
// replay after a lapse (replaySince) looks for the copies the client has.
const lapseSlack = time.Minute

// copySlack is how far before the newest publish the client has (or, with
// none, its cursor's event) the replay starts reading the app's publishes:
// a publish's copy into a workspace can land a little after it, and after a
// later publish's, so one published just before may not have reached the
// client. Kept short: a publish from before that never reached this
// workspace (published before the client subscribed, or to another user's
// workspace only) is replayed too, if it matches.
const copySlack = 5 * time.Second

// replaySince replays to a new MCP Events webhook subscription (subID, in
// a session of its own) what was published to its topic after the client's
// cursor and before the subscription began (before), reporting whether
// anything may have been skipped (truncated). It's the replay a subscribe
// owes a client whose subscription lapsed and that presents its last
// cursor (SEP-3415: the cursor is the replay point of a lapsed or lost
// subscription): what was published in between reached no session here,
// so it's read from the app's recent publishes.
//
// The cursor is a position in the workspace's event log. The replay starts
// at the newest publish the client has on this topic (its copy here is at
// or before the cursor), or with none at when the cursor's event arrived
// here, less copySlack, skipping the publishes whose copy here is at or
// before the cursor: the client has those. A replay is at least once: a
// publish its old subscription got after the cursor comes again, as a new
// occurrence.
//
// truncated: the cursor can't be placed (c1.0, or its events are gone),
// it's older than the publishes kept (maxReplay), maxAge cut a publish the
// subscription would get, more than MaxReplay were published to its topic
// since (filtered or not: only that many are read), or one's content was
// deleted once delivered.
func (a *API) replaySince(ctx context.Context, tx pgx.Tx, app apps.App, topic, subID string, filters map[string]string, before time.Time,
	cursor int64, maxAge time.Duration) (bool, error) {
	var at, now time.Time
	err := tx.QueryRow(ctx, `SELECT received_at, now() FROM events WHERE seq <= $1 ORDER BY seq DESC LIMIT 1`, cursor).Scan(&at, &now)
	if errors.Is(err, pgx.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	truncated := false
	seenFrom := at.Add(-lapseSlack)
	var floor time.Time
	if maxAge > 0 {
		floor = now.Add(-maxAge)
	}
	// The client has the publishes copied here at or before its cursor:
	// a topic copy has the publish's event ID, a direct copy names it
	// (EmitAttr), and a replay's source ID ends with the publish's.
	rows, err := tx.Query(ctx, `SELECT id::text, coalesce(attrs->>$3, ''), coalesce(source_event_id, '') FROM events
		WHERE type = $1 AND seq <= $2 AND received_at >= $4`, topicsType(app.ID), cursor, EmitAttr, seenFrom)
	if err != nil {
		return false, err
	}
	seenEvents, seenSources := map[string]bool{}, map[string]bool{}
	var id, emit, source string
	if _, err := pgx.ForEachRow(rows, []any{&id, &emit, &source}, func() error {
		seenEvents[id] = true
		if emit != "" {
			seenEvents[emit] = true
		}
		if rest, ok := strings.CutPrefix(source, "replay:"); ok {
			if _, src, ok := strings.Cut(rest, ":"); ok {
				seenSources[src] = true
			}
		}
		return nil
	}); err != nil {
		return false, err
	}
	// The newest publish the client has: the replay starts there, so what
	// was published before the client's subscription isn't sent.
	anchor := at
	ids, sources := make([]string, 0, len(seenEvents)), make([]string, 0, len(seenSources))
	for id := range seenEvents {
		ids = append(ids, id)
	}
	for src := range seenSources {
		sources = append(sources, src)
	}
	if len(ids)+len(sources) > 0 {
		var newest *time.Time
		if err := tx.QueryRow(ctx, `SELECT max(published_at) FROM topic_events WHERE app_id = $1 AND $2 = ANY(topics)
			AND (event_id::text = ANY($3) OR source_id = ANY($4))`, app.ID, topic, ids, sources).Scan(&newest); err != nil {
			return false, err
		}
		if newest != nil {
			anchor = *newest
		}
	}
	from := anchor.Add(-copySlack)
	if kept := now.Add(-maxReplay); from.Before(kept) {
		truncated, from = true, kept
	}
	rows, err = tx.Query(ctx, `SELECT `+pastPublishColumns+` FROM topic_events WHERE app_id = $1 AND $2 = ANY(topics) AND published_at >= $3 AND published_at < $4
		ORDER BY published_at DESC, source_id DESC LIMIT $5`, app.ID, topic, from, before, mcpevents.MaxReplay+1)
	if err != nil {
		return false, err
	}
	past, err := pgx.CollectRows(rows, pgx.RowToStructByPos[pastPublish])
	if err != nil {
		return false, err
	}
	var due []pastPublish
	for i, p := range past {
		if i == mcpevents.MaxReplay {
			truncated = true // older ones weren't read
			break
		}
		if seenEvents[p.EventID] || seenSources[p.SourceID] || !dataMatches(app.ID, topic, filters, p.Data) {
			continue
		}
		if !floor.IsZero() && p.At.Before(floor) {
			truncated = true
			continue
		}
		due = append(due, p)
	}
	// Oldest first, so they're delivered in the order they were published.
	slices.Reverse(due)
	for _, p := range due {
		ok, err := copyPublish(ctx, tx, app, topic, subID, p)
		if err != nil {
			return false, err
		}
		if !ok {
			truncated = true
		}
	}
	return truncated, nil
}
