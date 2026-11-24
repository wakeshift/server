package appapi

// What an app's owner can do to it from the console (internal/console),
// with the same code the app's own calls use: ending one of its
// subscriptions, forgetting one of its users, and making and revoking its
// publish URLs. by is who asked: "app:<id>" for the app's own calls,
// "user:<id>" for its owner in the console (who the console has already
// checked owns the app). The owner's actions are also written to the app's
// log, in the same transaction.

import (
	"cmp"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	mathrand "math/rand/v2"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/wakeshift/server/internal/apps"
	"github.com/wakeshift/server/internal/audit"
	"github.com/wakeshift/server/internal/db"
	"github.com/wakeshift/server/internal/sessions"
	"github.com/wakeshift/server/internal/streams"
)

// ErrNoPublishURL is a publish URL the app doesn't have.
var ErrNoPublishURL = errors.New("no such publish URL")

// ErrNoSubscription is a subscription ID that isn't one of the app's.
var ErrNoSubscription = errors.New("no such subscription")

// BadRequest reports whether err is a mistake in what was asked (a bad
// topic or user ID), with what to say.
func BadRequest(err error) (string, bool) {
	var b badRequest
	if errors.As(err, &b) {
		return b.Error(), true
	}
	return "", false
}

// ConnectedVia is how a conversation is connected now: "stream", "poll",
// "hooks", or "" if it isn't.
func ConnectedVia(s sessions.Session) string { return connectedVia(s) }

// ConversationID names a conversation (its session, by ID) where people
// can see it, as the console shows it: stable, and no help in finding its
// session key or handle, which act as the conversation (a handle in a
// user's workspace carries the key's token).
func ConversationID(sessionID string) string {
	sum := sha256.Sum256([]byte("wakeshift/conversation\x00" + sessionID))
	return "conv_" + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(sum[:10]))
}

func byOwner(by string) bool { return strings.HasPrefix(by, "user:") }

// EndEventSubscription ends one of the app's live event subscriptions by
// its ID, looked for in tenants (the console's page of the app's
// workspaces), as the app's event.end does, recording who did it.
// ended is false if it had already ended; ErrNoSubscription if none of
// tenants has it.
func (a *API) EndEventSubscription(ctx context.Context, app string, tenants []string, id, by string) (ended bool, err error) {
	if !uuidRE.MatchString(id) {
		return false, ErrNoSubscription
	}
	found := false
	for _, tenant := range tenants {
		err = a.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
			list, err := queryEventSubs(ctx, tx, `es.id = $1::uuid AND es.source_kind = $2 AND es.source_id = $3 FOR UPDATE OF es`,
				id, sourceApp, app)
			if err != nil || len(list) == 0 {
				return err
			}
			found = true
			s := list[0]
			if s.EndedAt != nil {
				return nil
			}
			if err := endUnasked(ctx, tx, s, by); err != nil {
				return err
			}
			ended = true
			if !byOwner(by) {
				return nil
			}
			return apps.Log(ctx, tx, apps.LogEntry{AppID: app, Actor: by, Action: apps.LogSubscriptionEnded,
				Detail: map[string]string{"subscription": id, "event": s.Event}})
		})
		if err != nil || found {
			break
		}
	}
	if err == nil && !found {
		err = ErrNoSubscription
	}
	return ended && err == nil, err
}

// appUser says whether u can be the app's ID for a person: 1 to 200
// characters, not starting with "~", which marks the workspace of a
// conversation without a user (conversationPrefix).
func appUser(u string) bool { return u != "" && len(u) <= 200 && !strings.HasPrefix(u, "~") }

// ForgetUser deletes the workspace the app keeps for one of its users and
// everything in it (see forgetUser). forgotten is false if there was
// nothing to forget.
func (a *API) ForgetUser(ctx context.Context, app, user, by string) (forgotten bool, err error) {
	user = strings.TrimSpace(user)
	if !appUser(user) {
		return false, badRequest{errBadForgetUser}
	}
	return forget(ctx, a.DB, app, user, by, 0)
}

var errBadForgetUser = errors.New("user is the app's ID for the person")

// IdleFor is how long a conversation without a user may go unused
// before it's forgotten (activity is noted at most hourly, db.Touch).
const IdleFor = 31 * 24 * time.Hour

// ExpireIdle forgets, as ForgetUser does, up to limit topics'
// conversations without a user ("~conversation:…") that no app has acted
// for in idle (tenants.active_at, else since they were made) and no live
// subscription still delivers to, oldest first, each recorded in its app's
// log as forgotten by Wakeshift. It returns how many it forgot.
func ExpireIdle(ctx context.Context, d *db.DB, idle time.Duration, limit int) (int, error) {
	type conversation struct {
		App, User string
	}
	// The prefix written out, so the planner can use the partial index
	// tenants_idle_conversations (057).
	rows, _ := d.Pool.Query(ctx, `SELECT app_id, external_id FROM tenants
		WHERE kind = 'app' AND starts_with(external_id, '`+conversationPrefix+`') AND coalesce(active_at, created_at) < now() - $1::interval
		ORDER BY coalesce(active_at, created_at) LIMIT $2`, idle.String(), limit)
	list, err := pgx.CollectRows(rows, pgx.RowToStructByPos[conversation])
	if err != nil {
		return 0, err
	}
	n := 0
	var failed error
	for _, g := range list {
		if ctx.Err() != nil {
			return n, ctx.Err()
		}
		ok, err := forget(ctx, d, g.App, g.User, audit.Hub, idle)
		if err != nil {
			// The rest go on: oldest first, a workspace that can't be erased
			// would otherwise stop every pass, and none after it would
			// ever be forgotten. It's tried again on the next pass, and the
			// first failure is returned (the pass logs it).
			slog.Warn("idle: forgetting an idle workspace failed; going on with the rest", "app", g.App, "err", err)
			failed = cmp.Or(failed, err)
			continue
		}
		if ok {
			n++
		}
	}
	return n, failed
}

// forget deletes the workspace app keeps for user, and everything in it,
// recording it in the app's log (forgetUser says what goes). With idle
// set, only if the workspace is still unused for that long once it's held:
// an app acting for it meanwhile keeps it.
func forget(ctx context.Context, d *db.DB, app, user, by string, idle time.Duration) (forgotten bool, err error) {
	return forgetWith(ctx, d, app, user, by, idle, nil)
}

// forgetWith is forget, also running also (if set) in the erasure's
// transaction, once there's something to forget.
func forgetWith(ctx context.Context, d *db.DB, app, user, by string, idle time.Duration, also func(pgx.Tx) error) (forgotten bool, err error) {
	var tenant string
	err = d.Pool.QueryRow(ctx, `SELECT id::text FROM tenants WHERE app_id = $1 AND external_id = $2`, app, user).Scan(&tenant)
	if errors.Is(err, pgx.ErrNoRows) {
		// No workspace, but maybe vouchers handed out to them, unused.
		return false, forgetVouchers(ctx, d.Pool, app, user)
	}
	if err != nil {
		return false, err
	}
	erase := func(tx pgx.Tx) error {
		held, idleFor, err := holdAppWorkspace(ctx, tx, app)
		if err != nil || !held {
			return err // or forgotten meanwhile
		}
		if idle > 0 && idleFor < idle {
			return nil // in use again
		}
		if idle > 0 && strings.HasPrefix(user, conversationPrefix) {
			// A conversation a live subscription still delivers to is in
			// use, though the app hasn't acted for it: noted, so the next
			// pass doesn't pick it again before its subscriptions end.
			var live bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM topic_subscriptions WHERE ended_at IS NULL AND expires_at > now())`).Scan(&live); err != nil {
				return err
			}
			if live {
				_, err := tx.Exec(ctx, `UPDATE tenants SET active_at = now() WHERE id = current_tenant()`)
				return err
			}
		}
		var subs, convs int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM topic_subscriptions ts JOIN subscriptions s ON s.tenant_id = ts.tenant_id AND s.id = ts.subscription_id
			WHERE ts.ended_at IS NULL AND s.status = 'active'`).Scan(&subs); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM sessions`).Scan(&convs); err != nil {
			return err
		}
		if err := eraseAndClose(ctx, tx, tenant); err != nil {
			return err
		}
		// Vouchers handed out to them (subscribe_vouchers): the unused ones;
		// the workspace's used ones went with it.
		if err := forgetVouchers(ctx, tx, app, user); err != nil {
			return err
		}
		// Their private topics (u/<user>/…), and their personal events'
		// (u/p<hash of user>/…, apps.PersonalTopic).
		own, under, personal := "u/"+user, "u/"+user+"/", apps.PersonalTopic("", user)
		// A multi-recipient emit that also names others keeps its record,
		// so their copies still to be made (topic_fanout, which cascades
		// from it) are made, without them: their entries in its
		// recipients (where a subscription or native recipient is named
		// only by their ID, emitToMany), topics and results go, and its
		// fingerprint, which covered them, so a retry of its id still
		// matches it.
		if _, err := tx.Exec(ctx, `UPDATE topic_events SET
				recipients = jsonb_set(recipients, '{to}', (SELECT coalesce(jsonb_agg(e), '[]') FROM jsonb_array_elements(recipients->'to') e
					WHERE e->>'user' IS DISTINCT FROM $5)),
				topics = ARRAY(SELECT t FROM unnest(topics) t WHERE NOT (t = $2 OR starts_with(t, $3) OR starts_with(t, $4))),
				results = CASE WHEN results ? 'found' THEN jsonb_set(results, '{found}', (SELECT coalesce(jsonb_agg(f), '[]') FROM jsonb_array_elements_text(results->'found') f
					WHERE f NOT IN ('u:' || $5, 'n:' || $5) AND NOT (starts_with(f, 's:') AND substr(f, position('/' IN f) + 1) = $5))) ELSE results END,
				fingerprint = NULL
			WHERE app_id = $1 AND jsonb_typeof(recipients->'to') = 'array'
				AND (recipients->'to' @> jsonb_build_array(jsonb_build_object('user', $5::text))
					OR EXISTS (SELECT 1 FROM unnest(topics) t WHERE t = $2 OR starts_with(t, $3) OR starts_with(t, $4)))
				AND EXISTS (SELECT 1 FROM jsonb_array_elements(recipients->'to') e WHERE e->>'user' IS DISTINCT FROM $5)`,
			app, own, under, personal, user); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM topic_events WHERE app_id = $1 AND EXISTS (SELECT 1 FROM unnest(topics) t WHERE t = $2 OR starts_with(t, $3) OR starts_with(t, $4))`,
			app, own, under, personal); err != nil {
			return err
		}
		// One that named only them, by their ID alone (a subscription or
		// native recipient: emitToMany).
		if _, err := tx.Exec(ctx, `DELETE FROM topic_events WHERE app_id = $1 AND recipients IS NOT NULL
			AND recipients->'to' @> jsonb_build_array(jsonb_build_object('user', $2::text))`, app, user); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM topic_publish_urls WHERE app_id = $1 AND (topic = $2 OR starts_with(topic, $3) OR starts_with(topic, $4))`,
			app, own, under, personal); err != nil {
			return err
		}
		forgotten = true
		// Never the user's ID, nor a hash of it: an ID that can be guessed
		// could be matched to its hash (096_forgotten_user_hash.sql).
		detail := map[string]any{"subscriptions_ended": subs, "conversations": convs}
		if idle > 0 {
			detail["reason"], detail["idle_days"] = "idle", int(idleFor/(24*time.Hour))
		}
		if also != nil {
			if err := also(tx); err != nil {
				return err
			}
		}
		return apps.Log(ctx, tx, apps.LogEntry{AppID: app, Actor: by, Action: apps.LogUserForgotten, Detail: detail})
	}
	// Erasing locks most of the workspace's rows, so a request that locks
	// some first (an agent's grant or session) and then what the erasure
	// already holds (appending an event) can deadlock with it. Postgres
	// cancels one of the two; when it's the erasure, it's tried again, from
	// a fresh transaction, after a short pause.
	for try := 1; ; try++ {
		forgotten = false
		err = d.InTenant(ctx, tenant, erase)
		if try == forgetTries || !deadlocked(err) {
			break
		}
		select {
		case <-ctx.Done():
			return false, err
		case <-time.After(time.Duration(try)*forgetRetryPause + mathrand.N(forgetRetryPause)):
		}
	}
	return forgotten && err == nil, err
}

// How often forgetting a user is tried when it deadlocks, and the pause
// between tries (growing, with as much again at random).
const (
	forgetTries      = 3
	forgetRetryPause = 50 * time.Millisecond
)

// deadlocked reports whether err is Postgres canceling a deadlock (40P01).
func deadlocked(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "40P01"
}

// holdAppWorkspace locks the transaction's workspace, which app keeps, for
// erasing: held is false if it's gone (forgotten meanwhile); idleFor is
// how long no app has acted for it. A concurrent call for the same user
// waits, then finds it gone (and makes a new one).
func holdAppWorkspace(ctx context.Context, tx pgx.Tx, app string) (held bool, idleFor time.Duration, err error) {
	// The publishes still to be copied into the workspace first, as making
	// a copy takes its row (then the publish's, then the event sequence):
	// erasing deletes them, so taken after the sequence, a forget and a copy
	// deadlock.
	if _, err := tx.Exec(ctx, `SELECT 1 FROM topic_fanout WHERE tenant_id = current_tenant() FOR UPDATE`); err != nil {
		return false, 0, err
	}
	// Then the workspace's event sequence, as an event append takes it
	// (then the workspace's row, for its foreign key): taken the other way
	// round, a forget and a publish into the workspace deadlock.
	if _, err := tx.Exec(ctx, `SELECT 1 FROM event_counters WHERE tenant_id = current_tenant() FOR UPDATE`); err != nil {
		return false, 0, err
	}
	var idleSecs float64
	err = tx.QueryRow(ctx, `SELECT extract(epoch FROM now() - coalesce(active_at, created_at))::float8
		FROM tenants WHERE id = current_tenant() AND app_id = $1 FOR UPDATE`, app).Scan(&idleSecs)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, 0, nil
	}
	if err != nil {
		return false, 0, err
	}
	return true, time.Duration(idleSecs * float64(time.Second)), nil
}

// eraseAndClose erases the transaction's workspace (tenant), held by
// holdAppWorkspace, and tells its open streams and polls, which re-check
// their link when told: with the links gone, they close.
func eraseAndClose(ctx context.Context, tx pgx.Tx, tenant string) error {
	rows, err := tx.Query(ctx, `SELECT DISTINCT agent_id FROM stream_links WHERE revoked_at IS NULL`)
	if err != nil {
		return err
	}
	linked, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	// Push subscriptions' streams hold no link: each is told by its ID.
	rows, err = tx.Query(ctx, `SELECT id::text FROM event_subscriptions WHERE destination = 'push' AND ended_at IS NULL`)
	if err != nil {
		return err
	}
	pushed, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	if err := eraseWorkspace(ctx, tx); err != nil {
		return err
	}
	for _, id := range pushed {
		if err := notifyPushEnded(ctx, tx, id); err != nil {
			return err
		}
	}
	for _, agent := range linked {
		if _, err := tx.Exec(ctx, `SELECT pg_notify($1, $2::text || ':' || $3)`, streams.RevokedChannel, tenant, agent); err != nil {
			return err
		}
	}
	return nil
}

// liveApp holds the app's row (FOR SHARE) until tx ends, so deleting it
// waits: apps.ErrDeleted if it was deleted first, apps.ErrNotFound if
// there's no such app.
func liveApp(ctx context.Context, tx pgx.Tx, app string) error {
	var deleted bool
	err := tx.QueryRow(ctx, `SELECT deleted_at IS NOT NULL FROM apps WHERE id = $1 FOR SHARE`, app).Scan(&deleted)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return apps.ErrNotFound
	case err != nil:
		return err
	case deleted:
		return apps.ErrDeleted
	}
	return nil
}

// notSuspended refuses (apps.ErrSuspended) something new for an app we've
// suspended: a publish URL, say. Call it after liveApp, which locks the row.
func notSuspended(ctx context.Context, tx pgx.Tx, app string) error {
	var suspended bool
	if err := tx.QueryRow(ctx, `SELECT suspended_at IS NOT NULL FROM apps WHERE id = $1`, app).Scan(&suspended); err != nil {
		return err
	}
	if suspended {
		return apps.ErrSuspended
	}
	return nil
}

// PublishURL is a new publish URL: its URL is shown once (only its hash is
// kept).
type PublishURL struct {
	ID    string `json:"id"`
	Event string `json:"event"` // the event a POST emits
	URL   string `json:"url"`
	// Signing is set when it was made with a signing secret: POSTs must be
	// signed with it (publishsigning.go).
	Signing *Signing `json:"signing,omitempty"`
}

// CreateEventPublishURL makes a publish URL for one of the app's events (a
// POST's body is an occurrence's payload, emitted with these arguments and,
// for a personal event, to that one subscriber), and logs it in the app's
// log: the console's no-code publish URL, and the app API's
// (createEventPublishURL). With a signing secret (a Standard Webhooks
// whsec_ secret), it takes only POSTs signed with it. Bad arguments are
// apps.EventArgsError, an event the app doesn't offer
// apps.ErrUnknownEventType, a bad secret SigningSecretError.
func (a *API) CreateEventPublishURL(ctx context.Context, app apps.App, name string, args json.RawMessage, to, signingSecret, by string) (PublishURL, error) {
	var sig *signingKey
	if strings.TrimSpace(signingSecret) != "" {
		key, fp, err := parseSigningSecret(signingSecret)
		if err != nil {
			return PublishURL{}, err
		}
		sig = &signingKey{key: key, fingerprint: fp}
	}
	et, err := app.Manifest.EventType(name)
	if err != nil {
		return PublishURL{}, err
	}
	topic, err := emitTopic(et, args, to)
	if err != nil {
		return PublishURL{}, err
	}
	canon, err := apps.Canonical(argsOrEmpty(args))
	if err != nil {
		return PublishURL{}, apps.EventArgsError{Msg: err.Error()}
	}
	ev := eventURL{name: et.Name, arguments: canon}
	if et.Personal {
		ev.subscriber = strings.TrimSpace(to)
	}
	return a.newPublishURL(ctx, app.ID, topic, ev, sig, by)
}

// signingKey is a parsed signing secret.
type signingKey struct {
	key         []byte
	fingerprint string
}

// argsOrEmpty is arguments, {} when there are none.
func argsOrEmpty(args json.RawMessage) json.RawMessage {
	if s := strings.TrimSpace(string(args)); s == "" || s == "null" {
		return json.RawMessage("{}")
	}
	return args
}

// eventURL is what an event's publish URL emits: the event, its arguments
// (canonical JSON) and a personal event's one subscriber.
type eventURL struct {
	name, subscriber string
	arguments        []byte
}

// newPublishURL makes a publish URL whose POST emits ev's event (topic is
// where its occurrences go), its body checked against the event's payload
// schema.
func (a *API) newPublishURL(ctx context.Context, app, topic string, ev eventURL, sig *signingKey, by string) (PublishURL, error) {
	event := ev.name
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return PublishURL{}, err
	}
	token := "wsp_" + base64.RawURLEncoding.EncodeToString(secret)
	// A personal event's topic is derived from its subscriber: the app is
	// told only the event.
	out := PublishURL{Event: event, URL: strings.TrimSuffix(a.Issuer, "/") + "/v1/publish/" + token}
	detail := map[string]string{"event": event}
	err := pgx.BeginFunc(ctx, a.DB.Pool, func(tx pgx.Tx) error {
		if err := liveApp(ctx, tx, app); err != nil {
			return err
		}
		if err := notSuspended(ctx, tx, app); err != nil {
			return err
		}
		var args *string
		if ev.arguments != nil {
			s := string(ev.arguments)
			args = &s
		}
		if err := tx.QueryRow(ctx, `INSERT INTO topic_publish_urls (app_id, topic, event, token_hash, created_by, arguments, subscriber)
			VALUES ($1, $2, nullif($3, ''), $4, $5, $6::jsonb, nullif($7, '')) RETURNING id::text`,
			app, topic, event, ticketHash(token), by, args, ev.subscriber).Scan(&out.ID); err != nil {
			return err
		}
		detail["publish_url"] = out.ID
		if err := apps.Log(ctx, tx, apps.LogEntry{AppID: app, Actor: by, Action: apps.LogPublishURLCreated, Detail: detail}); err != nil {
			return err
		}
		if sig == nil {
			return nil
		}
		s, action, err := a.setSigningSecret(ctx, tx, out.ID, sig.key, sig.fingerprint, a.now())
		if err != nil {
			return err
		}
		out.Signing = &s
		return apps.Log(ctx, tx, apps.LogEntry{AppID: app, Actor: by, Action: action,
			Detail: map[string]string{"publish_url": out.ID, "event": event, "fingerprint": sig.fingerprint}})
	})
	return out, err
}

// RevokePublishURL stops one of the app's publish URLs working (410 from
// then on). Revoking a revoked one changes nothing; one the app doesn't
// have is ErrNoPublishURL; a deleted app's is apps.ErrDeleted.
func (a *API) RevokePublishURL(ctx context.Context, app, id, by string) error {
	if !uuidRE.MatchString(id) {
		return ErrNoPublishURL
	}
	return pgx.BeginFunc(ctx, a.DB.Pool, func(tx pgx.Tx) error {
		// The same lock as making one: a deleted app's URLs were all
		// revoked with it (apps.Registry.Delete), so it has none to change.
		if err := liveApp(ctx, tx, app); err != nil {
			return err
		}
		// The log names the event, as making one does, never the topic
		// inside the core: a personal event's is derived from its subscriber.
		var event string
		var revoked bool
		err := tx.QueryRow(ctx, `SELECT coalesce(event, ''), revoked_at IS NOT NULL FROM topic_publish_urls WHERE id = $1 AND app_id = $2 FOR UPDATE`, id, app).Scan(&event, &revoked)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNoPublishURL
		}
		if err != nil || revoked {
			return err
		}
		// Its signing secrets go with it: nothing can use them now.
		if _, err := tx.Exec(ctx, `UPDATE topic_publish_urls SET revoked_at = now(), signing_secret = NULL, signing_fingerprint = NULL,
			previous_signing_secret = NULL, previous_signing_until = NULL WHERE id = $1`, id); err != nil {
			return err
		}
		return apps.Log(ctx, tx, apps.LogEntry{AppID: app, Actor: by, Action: apps.LogPublishURLRevoked,
			Detail: map[string]string{"publish_url": id, "event": event}})
	})
}
