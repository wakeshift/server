package appapi

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/framing"
	"github.com/wakeshift/server/internal/streams"
)

// StreamEnding is streams.Server.Ending: why a session's stream ends, in
// words for its agent. A Monitor prints the last line and stops, and an
// agent does what it says, so only an expired link says to get a new one
// (streams does that); when the session's subscriptions are over, it says
// there's nothing more to do, so the agent doesn't subscribe again.
//
// valid true (the stream is idle and still allowed): it ends the stream
// once the session has nothing left to receive, that is no active
// subscription and no delivery pending or in flight, after at least one topic subscription (a session
// that never had one, such as an Agent Chat member's, is left alone).
// valid false (the link stopped working): it says why, from how the
// session's last topic subscription ended.
func (a *API) StreamEnding(ctx context.Context, tx pgx.Tx, l streams.Link, valid bool) (streams.Ending, bool, error) {
	// A session's target is its ID, compared as a uuid so the indexes serve
	// it; anything else isn't a session.
	if !uuidRE.MatchString(l.Target) {
		return streams.Ending{}, false, nil
	}
	if valid {
		var active bool
		// Nor while a delivery waits for it (the notice that its last
		// subscription ended, written in the same transaction as the end),
		// or was written but not yet acknowledged or confirmed (in flight:
		// a once subscription's one delivery ends it as it's made, and
		// ending the stream before the ack would hand it back, to wait for
		// the agent's next tool call).
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM subscriptions WHERE session_id = $3::uuid AND status = 'active')
			OR EXISTS (SELECT 1 FROM deliveries WHERE agent_id = $2 AND target = $1 AND state IN ('pending', 'in_flight'))`, l.Target, l.Agent, l.Target).Scan(&active); err != nil || active {
			return streams.Ending{}, false, err
		}
	}
	var topic, reason, app, sub string
	err := tx.QueryRow(ctx, `SELECT ts.topic, ts.subscription_id::text, coalesce(a.name, ts.app_id),
			CASE WHEN s.status = 'active' AND ts.ended_at IS NULL THEN ''
				ELSE coalesce(ts.ended_reason, CASE WHEN ts.once THEN $2 ELSE $3 END) END
		FROM topic_subscriptions ts
		JOIN subscriptions s ON s.tenant_id = ts.tenant_id AND s.id = ts.subscription_id
		LEFT JOIN apps a ON a.id = ts.app_id
		WHERE ts.session_id = $1::uuid
		ORDER BY (s.status = 'active' AND ts.ended_at IS NULL) DESC, coalesce(ts.ended_at, s.cancelled_at) DESC NULLS LAST, ts.created_at DESC
		LIMIT 1`, l.Target, endOnce, endRevoked).Scan(&topic, &sub, &app, &reason)
	if errors.Is(err, pgx.ErrNoRows) {
		if valid {
			return streams.Ending{}, false, nil
		}
		// Not a topics session: say whose access ended, if an app made the link.
		if id, ok := strings.CutPrefix(l.CreatedBy, "app:"); ok {
			if err := tx.QueryRow(ctx, `SELECT name FROM apps WHERE id = $1`, id).Scan(&app); err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return streams.Ending{}, false, err
			}
		}
		return revokedEnding(app), true, nil
	}
	if err != nil {
		return streams.Ending{}, false, err
	}
	if reason == "" { // still subscribed: the link itself was revoked
		if valid {
			return streams.Ending{}, false, nil
		}
		return revokedEnding(app), true, nil
	}
	what, err := topicSubName(ctx, tx, sub, topic)
	if err != nil {
		return streams.Ending{}, false, err
	}
	return topicEnding(reason, what, framing.Name(app)), true, nil
}

// topicEnding is the last line for a stream whose session's last topic
// subscription ended, by reason (endUnsubscribed, …).
func topicEnding(reason, topic, app string) streams.Ending {
	const done = "so this stream has ended. Nothing more to do"
	switch reason {
	case endUnsubscribed:
		return streams.Ending{Reason: "unsubscribed", Note: "You unsubscribed from everything you watched in " + app + ", " + done + ": don't start it again unless the user asks."}
	case endClosed:
		return streams.Ending{Reason: "closed", Note: app + " closed " + topic + ", the last topic you watched there, " + done + "."}
	case endTTL:
		return streams.Ending{Reason: "ttl", Note: "Your subscription to " + topic + " in " + app + " ended after nothing arrived on it for a while, " + done + " unless the user wants to watch it again."}
	case endOnce:
		return streams.Ending{Reason: "once", Note: "Your subscription to " + topic + " in " + app + " got the one message it was for, " + done + "."}
	case endRemoved:
		return streams.Ending{Reason: "removed", Note: app + " ended your subscription to " + topic + ", the last thing you watched there, " + done + "."}
	}
	return revokedEnding(app)
}

// span is a duration in words: "30 days", "2 hours", "5 minutes".
func span(d time.Duration) string {
	unit := func(n int, one string) string {
		if n == 1 {
			return "1 " + one
		}
		return strconv.Itoa(n) + " " + one + "s"
	}
	switch {
	case d >= 24*time.Hour && d%(24*time.Hour) == 0:
		return unit(int(d/(24*time.Hour)), "day")
	case d >= time.Hour && d%time.Hour == 0:
		return unit(int(d/time.Hour), "hour")
	case d >= time.Minute:
		return unit(int(d/time.Minute), "minute")
	}
	return unit(int(d/time.Second), "second")
}

func revokedEnding(app string) streams.Ending {
	if app == "" {
		app = "the app"
	}
	return streams.Ending{Reason: "revoked", Note: "Access to " + framing.Name(app) + " was revoked, so this stream has ended. Nothing more to do unless the user asks you to watch again."}
}
