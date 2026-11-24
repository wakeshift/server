package console

// An app's pages in the console: its overview, subscriptions, activity,
// users and publish URLs, for the members of the app's organization (each
// route checks the role its action needs).
//
// What happens for an app happens in the workspaces it keeps for its
// users, each behind row-level security. These pages read
// them with db.ReadTenants: one read-only transaction that sets each
// workspace in turn, so every query sees only the workspace it's set to,
// exactly as inside db.InTenant, and nothing can be written. Each read is
// bounded: at most maxScan of the app's workspaces (the newest, or the
// ones with subscriptions), each queried for at most one page, and the
// response says when there were more workspaces than it read. Changes
// (ending a subscription, forgetting a user) run in the workspace's own
// InTenant transaction, through the same code as the app's own calls
// (appapi/owner.go), and are written to the workspace's audit log and the
// app's log.

import (
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/appapi"
	"github.com/wakeshift/server/internal/apps"
	"github.com/wakeshift/server/internal/connectorversion"
	"github.com/wakeshift/server/internal/httpx"
	"github.com/wakeshift/server/internal/orgs"
	"github.com/wakeshift/server/internal/sessions"
)

// maxScan is the most of an app's workspaces one request reads.
const maxScan = 200

// maxPage is the most rows a list returns at once.
const maxPage = 100

func (a *API) registerAppConsole(route func(string, http.HandlerFunc)) {
	route("GET /api/v1/developer/apps/{id}/overview", a.appOverview)
	route("GET /api/v1/developer/apps/{id}/subscriptions", a.appSubscriptions)
	route("POST /api/v1/developer/apps/{id}/subscriptions/{sub}/end", a.endAppSubscription)
	route("GET /api/v1/developer/apps/{id}/activity", a.appActivity)
	route("GET /api/v1/developer/apps/{id}/users", a.appUsers)
	route("POST /api/v1/developer/apps/{id}/users/forget", a.forgetAppUser)
	route("GET /api/v1/developer/apps/{id}/publish-urls", a.appPublishURLs)
	route("POST /api/v1/developer/apps/{id}/publish-urls", a.createAppPublishURL)
	route("POST /api/v1/developer/apps/{id}/publish-urls/{url}/revoke", a.revokeAppPublishURL)
	route("PUT /api/v1/developer/apps/{id}/publish-urls/{url}/signing-secret", a.putAppPublishURLSecret)
	route("DELETE /api/v1/developer/apps/{id}/publish-urls/{url}/signing-secret", a.deleteAppPublishURLSecret)
}

// access is the app named in the path, if the signed-in user is a member
// of its organization whose role allows action: ErrNotOwner (404) for a
// non-member, or no such app; orgs.ErrForbidden (403) for a role that
// doesn't allow it.
func (a *API) access(r *http.Request, action orgs.Action) (apps.Owned, error) {
	var out apps.Owned
	err := a.platformTx(r, func(ctx context.Context, tx pgx.Tx) (err error) {
		out, err = a.Apps.Access(ctx, tx, me(r), r.PathValue("id"), action)
		return err
	})
	return out, err
}

// workspace is one the app keeps: for a user (User) or, without one, for a
// single conversation (Harness says which kind, when the harness named it).
type workspace struct {
	ID        string
	External  string
	CreatedAt time.Time
}

// user is the app's ID for the workspace's person, or "" for a
// conversation's own workspace (its external ID starts with ~ and may name
// a harness thread, which isn't shown).
func (w workspace) user() string {
	if strings.HasPrefix(w.External, "~") {
		return ""
	}
	return w.External
}

// harness is the harness a conversation's own workspace was named by
// (~codex:<thread> is "codex"), or "" for a user's or an unnamed one.
func (w workspace) harness() string {
	rest, ok := strings.CutPrefix(w.External, "~")
	if !ok {
		return ""
	}
	h, _, _ := strings.Cut(rest, ":")
	if h == "conversation" {
		return ""
	}
	return h
}

// workspaces lists up to maxScan of the app's workspaces, newest first,
// and how many it has. With subscribed, only ones where the app has a live
// subscription (topic_members); with external, only that one.
func (a *API) workspaces(ctx context.Context, app string, subscribed bool, external *string) ([]workspace, int, error) {
	where := `t.app_id = $1`
	args := []any{app}
	if subscribed {
		where += ` AND EXISTS (SELECT 1 FROM topic_members m WHERE m.tenant_id = t.id AND m.app_id = $1)`
	}
	if external != nil {
		where += ` AND t.external_id = $2`
		args = append(args, *external)
	}
	var total int
	if err := a.DB.Pool.QueryRow(ctx, `SELECT count(*) FROM tenants t WHERE `+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := a.DB.Pool.Query(ctx, `SELECT t.id::text, t.external_id, t.created_at FROM tenants t WHERE `+where+
		` ORDER BY t.created_at DESC, t.id LIMIT `+strconv.Itoa(maxScan), args...)
	if err != nil {
		return nil, 0, err
	}
	list, err := pgx.CollectRows(rows, pgx.RowToStructByPos[workspace])
	return list, total, err
}

func ids(list []workspace) []string {
	out := make([]string, len(list))
	for i, w := range list {
		out[i] = w.ID
	}
	return out
}

func byID(list []workspace) map[string]workspace {
	out := make(map[string]workspace, len(list))
	for _, w := range list {
		out[w.ID] = w
	}
	return out
}

// scanned says how much of the app a response read.
type scanned struct {
	Workspaces int `json:"workspaces_scanned"`
	Total      int `json:"workspaces_total"`
}

// ── Cursors ──────────────────────────────────────────────────────────────

// A cursor is the last row of a page: its time and ID, opaque to callers.
type cursor struct {
	At time.Time
	ID string
}

func (c cursor) String() string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(c.At.UnixNano(), 10) + "|" + c.ID))
}

func parseCursor(s string) (*cursor, bool) {
	if s == "" {
		return nil, true
	}
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, false
	}
	at, id, ok := strings.Cut(string(b), "|")
	n, err := strconv.ParseInt(at, 10, 64)
	if !ok || err != nil || len(id) > 100 {
		return nil, false
	}
	return &cursor{At: time.Unix(0, n), ID: id}, true
}

// listPage reads ?cursor= and ?limit= (1-100, default 50).
func listPage(w http.ResponseWriter, r *http.Request) (*cursor, int, bool) {
	q := r.URL.Query()
	c, ok := parseCursor(q.Get("cursor"))
	if !ok {
		httpx.WriteError(w, http.StatusBadRequest, "bad_request", "cursor is the next_cursor of the page before")
		return nil, 0, false
	}
	limit := 50
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxPage {
			httpx.WriteError(w, http.StatusBadRequest, "bad_request", "limit must be 1-100")
			return nil, 0, false
		}
		limit = n
	}
	return c, limit, true
}

// args for a SQL condition on (time, id) < cursor: nil times read as no
// cursor.
func cursorArgs(c *cursor) (*time.Time, string) {
	if c == nil {
		return nil, ""
	}
	return &c.At, c.ID
}

func sortNewest[T any](rows []T, key func(T) (time.Time, string)) {
	slices.SortFunc(rows, func(x, y T) int {
		xa, xi := key(x)
		ya, yi := key(y)
		if c := ya.Compare(xa); c != 0 {
			return c
		}
		return cmp.Compare(yi, xi)
	})
}

// keepNewest keeps the newest n of rows as a page merges workspace by
// workspace, so it holds at most n rows. Once it has n, floor is the
// oldest kept: the next workspaces are read only for rows newer than it
// (nil until then).
func keepNewest[T any](rows []T, n int, key func(T) (time.Time, string)) ([]T, *cursor) {
	sortNewest(rows, key)
	if len(rows) < n {
		return rows, nil
	}
	rows = rows[:n]
	at, id := key(rows[n-1])
	return rows, &cursor{At: at, ID: id}
}

// newestFirst sorts rows by time and ID, newest first, and keeps limit,
// returning the cursor for the next page ("" if there's none).
func newestFirst[T any](rows []T, limit int, key func(T) (time.Time, string)) ([]T, string) {
	sortNewest(rows, key)
	if len(rows) <= limit {
		return rows, ""
	}
	rows = rows[:limit]
	at, id := key(rows[limit-1])
	return rows, cursor{At: at, ID: id}.String()
}

// ── Conversations ────────────────────────────────────────────────────────

// conversation is one of the app's conversations, as its owner sees it.
type conversation struct {
	Harness string `json:"harness"` // the agent: claude-code, codex, cursor…
	// ID names it (appapi.ConversationID): never its handle, which in a
	// user's workspace carries its session key.
	ID        string    `json:"id"`
	Connected bool      `json:"connected"`
	Via       string    `json:"via,omitempty"` // stream, poll or hooks
	LastSeen  time.Time `json:"last_active_at"`
}

func conversationOf(s sessions.Session) conversation {
	via := appapi.ConnectedVia(s)
	return conversation{Harness: s.Agent, ID: appapi.ConversationID(s.ID), Connected: via != "", Via: via, LastSeen: s.LastActiveAt}
}

// ── Overview ─────────────────────────────────────────────────────────────

// connectorUse is one version of a Wakeshift connector in use for the app:
// how many of its conversations were last seen with it.
type connectorUse struct {
	Harness       string `json:"harness"`
	Connector     string `json:"connector"` // wakeshift, wakeshift-hermes, wakeshift-openclaw
	Version       string `json:"version"`   // "0": from before connectors said their version
	Conversations int    `json:"conversations"`
	Outdated      bool   `json:"outdated"` // older than Wakeshift serves: it's refused
}

// connectorsActive: the connectors of conversations active this recently
// count as in use.
const connectorsActive = "7 days"

type failure struct {
	id    string    // the delivery: the tiebreak when two failed at once
	At    time.Time `json:"at"`
	Text  string    `json:"text"`
	Error string    `json:"error,omitempty"`
	User  string    `json:"user,omitempty"`
}

func (a *API) appOverview(w http.ResponseWriter, r *http.Request) {
	app, err := a.access(r, orgs.AppRead)
	if err != nil {
		a.respondApps(w, nil, err)
		return
	}
	ctx := r.Context()
	out := struct {
		App    apps.Owned `json:"app"`
		Users  int        `json:"users"`
		NoUser int        `json:"conversation_workspaces"` // workspaces for one conversation, without a user
		// Conversations is every conversation using the app: the sessions
		// in the workspaces read (Scanned), plus one for each conversation's
		// own workspace beyond them.
		Conversations int            `json:"conversations"`
		Subscriptions int            `json:"subscriptions"`
		Connected     int            `json:"connected"`
		ByHarness     map[string]int `json:"connected_by_harness"`
		Via           map[string]int `json:"connected_via"`
		Publishes24h  int            `json:"publishes_24h"`
		Wakes24h      int            `json:"wakes_24h"`
		Failures24h   int            `json:"failures_24h"`
		Recent        []failure      `json:"recent_failures"`
		Connectors    []connectorUse `json:"connectors"`
		PublishURLs   int            `json:"publish_urls"`
		LastActivity  *time.Time     `json:"last_activity_at,omitempty"`
		Scanned       scanned        `json:"scanned"`
	}{App: app, ByHarness: map[string]int{}, Via: map[string]int{}, Recent: []failure{}, Connectors: []connectorUse{}}
	uses := map[connectorversion.Client]int{}
	err = a.DB.Pool.QueryRow(ctx, `SELECT
			(SELECT count(*) FROM tenants WHERE app_id = $1 AND NOT starts_with(external_id, '~')),
			(SELECT count(*) FROM tenants WHERE app_id = $1 AND starts_with(external_id, '~')),
			(SELECT count(*) FROM topic_members WHERE app_id = $1),
			(SELECT count(*) FROM topic_events WHERE app_id = $1 AND published_at > now() - interval '1 day'),
			(SELECT max(published_at) FROM topic_events WHERE app_id = $1),
			(SELECT count(*) FROM topic_publish_urls WHERE app_id = $1 AND revoked_at IS NULL)`, app.ID).
		Scan(&out.Users, &out.NoUser, &out.Subscriptions, &out.Publishes24h, &out.LastActivity, &out.PublishURLs)
	if err != nil {
		respond(w, nil, err)
		return
	}
	list, total, err := a.workspaces(ctx, app.ID, false, nil)
	if err != nil {
		respond(w, nil, err)
		return
	}
	out.Scanned = scanned{len(list), total}
	out.Conversations = out.NoUser
	for _, w := range list {
		if w.user() == "" {
			out.Conversations-- // counted by its sessions instead
		}
	}
	names := byID(list)
	later := func(t *time.Time) {
		if t != nil && (out.LastActivity == nil || t.After(*out.LastActivity)) {
			out.LastActivity = t
		}
	}
	err = a.DB.ReadTenants(ctx, ids(list), func(tenant string, tx pgx.Tx) error {
		var wakes, failed int
		var last *time.Time
		if err := tx.QueryRow(ctx, `SELECT count(*) FILTER (WHERE state IN ('delivered', 'acked')), count(*) FILTER (WHERE state = 'dead'), max(created_at)
			FROM deliveries WHERE created_at > now() - interval '1 day'`).Scan(&wakes, &failed, &last); err != nil {
			return err
		}
		out.Wakes24h += wakes
		out.Failures24h += failed
		later(last)
		if failed > 0 {
			rows, err := tx.Query(ctx, `SELECT d.id::text, d.created_at, e.subject, coalesce(d.last_error, '') FROM deliveries d JOIN events e ON e.tenant_id = d.tenant_id AND e.id = d.event_id
				WHERE d.state = 'dead' AND d.created_at > now() - interval '1 day' ORDER BY d.created_at DESC, d.id DESC LIMIT 5`)
			if err != nil {
				return err
			}
			fs, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (failure, error) {
				f := failure{User: names[tenant].user()}
				return f, row.Scan(&f.id, &f.At, &f.Text, &f.Error)
			})
			if err != nil {
				return err
			}
			out.Recent = append(out.Recent, fs...)
		}
		var active *time.Time
		var sessionCount int
		if err := tx.QueryRow(ctx, `SELECT count(*), max(last_active_at) FROM sessions`).Scan(&sessionCount, &active); err != nil {
			return err
		}
		out.Conversations += sessionCount
		later(active)
		rows, err := tx.Query(ctx, `SELECT connector, count(*) FROM sessions WHERE connector IS NOT NULL
			AND last_active_at > now() - interval '`+connectorsActive+`' GROUP BY connector`)
		if err != nil {
			return err
		}
		var raw string
		var n int
		if _, err := pgx.ForEachRow(rows, []any{&raw, &n}, func() error {
			if c, ok := connectorversion.Parse(raw); ok {
				uses[c] += n
			}
			return nil
		}); err != nil {
			return err
		}
		ss, err := sessions.ConnectedNow(ctx, tx)
		if err != nil {
			return err
		}
		for _, s := range ss {
			if c := conversationOf(s); c.Connected {
				out.Connected++
				out.ByHarness[c.Harness]++
				out.Via[c.Via]++
			}
		}
		return nil
	})
	if err != nil {
		respond(w, nil, err)
		return
	}
	out.Recent, _ = newestFirst(out.Recent, 5, func(f failure) (time.Time, string) { return f.At, f.id })
	for c, n := range uses {
		out.Connectors = append(out.Connectors, connectorUse{Harness: c.Harness, Connector: c.Name, Version: c.Version, Conversations: n,
			Outdated: a.Streams != nil && a.Streams.Minimums().Outdated(c)})
	}
	// By harness, newest version first.
	slices.SortFunc(out.Connectors, func(x, y connectorUse) int {
		return cmp.Or(cmp.Compare(x.Harness, y.Harness), cmp.Compare(x.Connector, y.Connector),
			-connectorversion.Compare(x.Version, y.Version), cmp.Compare(y.Version, x.Version))
	})
	respond(w, out, nil)
}

// ── Subscriptions ────────────────────────────────────────────────────────

type appSubscription struct {
	ID        string          `json:"id"` // the event subscription's, as the app sees it
	Event     string          `json:"event"`
	Arguments json.RawMessage `json:"arguments"`
	// Destination is "conversation" (a bridged conversation) or "webhook"
	// (a client that subscribed natively).
	Destination  string        `json:"destination"`
	Reason       string        `json:"reason,omitempty"`
	Once         bool          `json:"once"`
	CreatedAt    time.Time     `json:"created_at"`
	ExpiresAt    time.Time     `json:"expires_at"`
	User         string        `json:"user,omitempty"`
	Conversation *conversation `json:"conversation,omitempty"`
	tenant       string
	session      *string
}

// appSubscriptions lists the app's live event subscriptions, newest first,
// in the workspaces where it has any.
func (a *API) appSubscriptions(w http.ResponseWriter, r *http.Request) {
	c, limit, ok := listPage(w, r)
	if !ok {
		return
	}
	app, err := a.access(r, orgs.AppRead)
	if err != nil {
		a.respondApps(w, nil, err)
		return
	}
	ctx := r.Context()
	list, total, err := a.workspaces(ctx, app.ID, true, nil)
	if err != nil {
		respond(w, nil, err)
		return
	}
	names := byID(list)
	at, id := cursorArgs(c)
	var rows []appSubscription
	var floor *cursor
	subKey := func(s appSubscription) (time.Time, string) { return s.CreatedAt, s.ID }
	err = a.DB.ReadTenants(ctx, ids(list), func(tenant string, tx pgx.Tx) error {
		fat, fid := cursorArgs(floor)
		q, err := tx.Query(ctx, `SELECT es.id::text, es.event, es.arguments, es.destination, coalesce(es.reason, ''), ts.once, es.created_at, ts.expires_at, es.session_id::text
			FROM event_subscriptions es JOIN topic_subscriptions ts ON ts.tenant_id = es.tenant_id AND ts.subscription_id = es.subscription_id
			JOIN subscriptions s ON s.tenant_id = es.tenant_id AND s.id = es.subscription_id
			WHERE es.source_kind = 'app' AND es.source_id = $1 AND es.ended_at IS NULL
			AND ts.ended_at IS NULL AND s.status = 'active' AND ts.expires_at > now()
			AND ($2::timestamptz IS NULL OR (es.created_at, es.id::text) < ($2, $3))
			AND ($5::timestamptz IS NULL OR (es.created_at, es.id::text) > ($5, $6))
			ORDER BY es.created_at DESC, es.id::text DESC LIMIT $4`, app.ID, at, id, limit+1, fat, fid)
		if err != nil {
			return err
		}
		got, err := pgx.CollectRows(q, func(row pgx.CollectableRow) (appSubscription, error) {
			s := appSubscription{tenant: tenant, User: names[tenant].user()}
			var args string
			err := row.Scan(&s.ID, &s.Event, &args, &s.Destination, &s.Reason, &s.Once, &s.CreatedAt, &s.ExpiresAt, &s.session)
			s.Arguments = json.RawMessage(args)
			return s, err
		})
		rows, floor = keepNewest(append(rows, got...), limit+1, subKey)
		return err
	})
	if err != nil {
		respond(w, nil, err)
		return
	}
	rows, next := newestFirst(rows, limit, subKey)
	// Then the conversations of the ones on this page.
	tenants := []string{}
	for _, s := range rows {
		if !slices.Contains(tenants, s.tenant) {
			tenants = append(tenants, s.tenant)
		}
	}
	err = a.DB.ReadTenants(ctx, tenants, func(tenant string, tx pgx.Tx) error {
		for i := range rows {
			s := &rows[i]
			if s.tenant != tenant || s.session == nil || s.Destination != "conversation" {
				continue
			}
			sess, err := sessions.Get(ctx, tx, *s.session)
			if errors.Is(err, sessions.ErrNotFound) {
				continue
			}
			if err != nil {
				return err
			}
			cv := conversationOf(sess)
			s.Conversation = &cv
		}
		return nil
	})
	if rows == nil {
		rows = []appSubscription{}
	}
	respond(w, map[string]any{"subscriptions": rows, "next_cursor": next, "scanned": scanned{len(list), total}}, err)
}

// endAppSubscription ends one of the app's event subscriptions, as the
// app's event.end does.
func (a *API) endAppSubscription(w http.ResponseWriter, r *http.Request) {
	app, err := a.access(r, orgs.AppEndSubscription)
	if err != nil {
		a.respondApps(w, nil, err)
		return
	}
	// The workspaces the page lists, then the app's newest: one already
	// ended (its workspace no longer has a live subscription) is found
	// there, and says so.
	live, _, err := a.workspaces(r.Context(), app.ID, true, nil)
	if err != nil {
		respond(w, nil, err)
		return
	}
	newest, _, err := a.workspaces(r.Context(), app.ID, false, nil)
	if err != nil {
		respond(w, nil, err)
		return
	}
	tenants := ids(live)
	for _, t := range ids(newest) {
		if !slices.Contains(tenants, t) {
			tenants = append(tenants, t)
		}
	}
	ended, err := a.AppAPI.EndEventSubscription(r.Context(), app.ID, tenants, r.PathValue("sub"), "user:"+me(r))
	a.respondApps(w, map[string]bool{"ended": ended}, err)
}

// ── Activity ─────────────────────────────────────────────────────────────

// activity is one thing that happened for the app: an emit (kept a
// day), or a delivery to one conversation (a wake, one
// waiting, failed or canceled), or a notice Wakeshift sent a conversation
// (its subscription ended).
type activity struct {
	ID           string          `json:"id"`
	Kind         string          `json:"kind"` // publish, delivery or notice
	At           time.Time       `json:"at"`
	Status       string          `json:"status"`
	Topics       []string        `json:"topics,omitempty"`
	Text         string          `json:"text"`
	Data         json.RawMessage `json:"data,omitempty"`
	Matched      *int            `json:"matched,omitempty"`
	PublishURL   string          `json:"publish_url,omitempty"`
	User         string          `json:"user,omitempty"`
	Conversation *conversation   `json:"conversation,omitempty"`
	Attempts     int             `json:"attempts,omitempty"`
	Error        string          `json:"error,omitempty"`
	FinishedAt   *time.Time      `json:"finished_at,omitempty"`
	// Connector: the Wakeshift connector it was last handed to
	// ("wakeshift/2026.10.20.1435+91faaf7 codex"), if one took it.
	Connector string `json:"connector,omitempty"`
	// ContentDeleted: its text and data were deleted once it was delivered,
	// as its event type asks (apps.EventType.DeleteOnDelivery).
	ContentDeleted bool `json:"content_deleted,omitempty"`
}

// Activity statuses: a publish matched subscribers or none; a delivery
// woke its conversation, is waiting, failed or was canceled.
var deliveryStates = map[string][]string{
	"woke":     {"delivered", "acked"},
	"waiting":  {"pending", "in_flight"},
	"failed":   {"dead"},
	"canceled": {"cancelled"}, // the deliveries table keeps the British spelling
}

func deliveryStatus(state string) string {
	for k, v := range deliveryStates {
		if slices.Contains(v, state) {
			return k
		}
	}
	return state
}

func (a *API) appActivity(w http.ResponseWriter, r *http.Request) {
	c, limit, ok := listPage(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	topic, user, status := q.Get("topic"), q.Get("user"), q.Get("status")
	if status == "cancelled" {
		// Accepted for one release after "canceled" replaced it.
		status = "canceled"
	}
	switch {
	case len(topic) > 200 || len(user) > 200:
		httpx.WriteError(w, http.StatusBadRequest, "bad_request", "topic and user are at most 200 characters")
		return
	case status != "" && status != "published" && status != "unmatched" && deliveryStates[status] == nil:
		httpx.WriteError(w, http.StatusBadRequest, "bad_request", "status is published, unmatched, woke, waiting, failed or canceled")
		return
	}
	app, err := a.access(r, orgs.AppRead)
	if err != nil {
		a.respondApps(w, nil, err)
		return
	}
	ctx := r.Context()
	at, id := cursorArgs(c)
	var rows []activity
	actKey := func(e activity) (time.Time, string) { return e.At, e.ID }
	// Publishes are the app's, not a user's.
	if user == "" && (status == "" || status == "published" || status == "unmatched") {
		q, err := a.DB.Pool.Query(ctx, `SELECT event_id::text, published_at, topics, text, data, matched, coalesce(publish_url::text, ''), redacted_at IS NOT NULL
			FROM topic_events WHERE app_id = $1 AND ($2::timestamptz IS NULL OR (published_at, 'p' || event_id::text) < ($2, $3))
			AND ($4 = '' OR $4 = ANY(topics)) AND ($5 = '' OR ($5 = 'published') = (matched > 0))
			ORDER BY published_at DESC, event_id DESC LIMIT $6`, app.ID, at, id, topic, status, limit+1)
		if err != nil {
			respond(w, nil, err)
			return
		}
		got, err := pgx.CollectRows(q, func(row pgx.CollectableRow) (activity, error) {
			var e activity
			var matched int
			err := row.Scan(&e.ID, &e.At, &e.Topics, &e.Text, &e.Data, &matched, &e.PublishURL, &e.ContentDeleted)
			e.ID, e.Kind, e.Matched, e.Status = "p"+e.ID, "publish", &matched, "published"
			if matched == 0 {
				e.Status = "unmatched"
			}
			return e, err
		})
		if err != nil {
			respond(w, nil, err)
			return
		}
		rows = append(rows, got...)
	}
	var sc scanned
	if status != "published" && status != "unmatched" {
		var only *string
		if user != "" {
			only = &user
		}
		list, total, err := a.workspaces(ctx, app.ID, false, only)
		if err != nil {
			respond(w, nil, err)
			return
		}
		sc = scanned{len(list), total}
		names := byID(list)
		states := append([]string{}, deliveryStates[status]...) // never nil: nil is SQL NULL
		var floor *cursor
		rows, floor = keepNewest(rows, limit+1, actKey)
		err = a.DB.ReadTenants(ctx, ids(list), func(tenant string, tx pgx.Tx) error {
			fat, fid := cursorArgs(floor)
			q, err := tx.Query(ctx, `SELECT d.id::text, d.created_at, d.state, d.attempts, coalesce(d.last_error, ''), d.finished_at,
				e.subject, e.data, coalesce(e.attrs->>'topic', ''), e.source, se.agent_id, se.id::text, coalesce(d.connector, ''), e.redacted_at IS NOT NULL
				FROM deliveries d JOIN events e ON e.tenant_id = d.tenant_id AND e.id = d.event_id
				LEFT JOIN sessions se ON se.tenant_id = d.tenant_id AND se.id::text = coalesce(d.deliver_to::text, nullif(d.target, ''))
				WHERE ($1::timestamptz IS NULL OR (d.created_at, 'd' || d.id::text) < ($1, $2))
				AND ($3 = '' OR e.attrs ? ('$topic:' || $3)) AND (cardinality($4::text[]) = 0 OR d.state = ANY($4))
				AND ($6::timestamptz IS NULL OR (d.created_at, 'd' || d.id::text) > ($6, $7))
				ORDER BY d.created_at DESC, d.id DESC LIMIT $5`, at, id, topic, states, limit+1, fat, fid)
			if err != nil {
				return err
			}
			got, err := pgx.CollectRows(q, func(row pgx.CollectableRow) (activity, error) {
				e := activity{Kind: "delivery", User: names[tenant].user()}
				var state, topics, source string
				var harness, session *string
				err := row.Scan(&e.ID, &e.At, &state, &e.Attempts, &e.Error, &e.FinishedAt, &e.Text, &e.Data, &topics, &source, &harness, &session, &e.Connector, &e.ContentDeleted)
				e.ID, e.Status = "d"+e.ID, deliveryStatus(state)
				if topics != "" {
					e.Topics = strings.Fields(topics)
				}
				if source == "hub" {
					e.Kind = "notice"
				}
				if harness != nil {
					e.Conversation = &conversation{Harness: *harness, ID: appapi.ConversationID(*session)}
				}
				return e, err
			})
			rows, floor = keepNewest(append(rows, got...), limit+1, actKey)
			return err
		})
		if err != nil {
			respond(w, nil, err)
			return
		}
	}
	rows, next := newestFirst(rows, limit, actKey)
	if rows == nil {
		rows = []activity{}
	}
	respond(w, map[string]any{"activity": rows, "next_cursor": next, "scanned": sc}, nil)
}

// ── Users ────────────────────────────────────────────────────────────────

type appUser struct {
	User          string     `json:"user,omitempty"`    // the app's ID for them; absent for a conversation's own workspace
	Harness       string     `json:"harness,omitempty"` // for a conversation's own workspace, the harness that named it
	CreatedAt     time.Time  `json:"created_at"`
	Conversations int        `json:"conversations"`
	Connected     int        `json:"connected"`
	Subscriptions int        `json:"subscriptions"`
	LastActivity  *time.Time `json:"last_activity_at,omitempty"`
}

// appUsers lists the workspaces the app keeps, newest first: one per user,
// or per conversation for calls without a user.
func (a *API) appUsers(w http.ResponseWriter, r *http.Request) {
	c, limit, ok := listPage(w, r)
	if !ok {
		return
	}
	app, err := a.access(r, orgs.AppRead)
	if err != nil {
		a.respondApps(w, nil, err)
		return
	}
	ctx := r.Context()
	at, id := cursorArgs(c)
	rows, err := a.DB.Pool.Query(ctx, `SELECT id::text, external_id, created_at FROM tenants WHERE app_id = $1
		AND ($2::timestamptz IS NULL OR (created_at, id::text) < ($2, $3)) ORDER BY created_at DESC, id::text DESC LIMIT $4`, app.ID, at, id, limit+1)
	if err != nil {
		respond(w, nil, err)
		return
	}
	list, err := pgx.CollectRows(rows, pgx.RowToStructByPos[workspace])
	if err != nil {
		respond(w, nil, err)
		return
	}
	next := ""
	if len(list) > limit {
		list = list[:limit]
		last := list[limit-1]
		next = cursor{At: last.CreatedAt, ID: last.ID}.String()
	}
	out := make([]appUser, 0, len(list))
	byTenant := map[string]int{}
	for i, w := range list {
		out = append(out, appUser{User: w.user(), Harness: w.harness(), CreatedAt: w.CreatedAt})
		byTenant[w.ID] = i
	}
	err = a.DB.ReadTenants(ctx, ids(list), func(tenant string, tx pgx.Tx) error {
		u := &out[byTenant[tenant]]
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM topic_subscriptions ts JOIN subscriptions s ON s.tenant_id = ts.tenant_id AND s.id = ts.subscription_id
			WHERE ts.ended_at IS NULL AND s.status = 'active' AND ts.expires_at > now()`).Scan(&u.Subscriptions); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT count(*), max(last_active_at) FROM sessions`).Scan(&u.Conversations, &u.LastActivity); err != nil {
			return err
		}
		ss, err := sessions.ConnectedNow(ctx, tx)
		u.Connected = len(ss)
		return err
	})
	respond(w, map[string]any{"users": out, "next_cursor": next}, err)
}

// forgetAppUser forgets one of the app's users, as the app's forgetUser
// does: their workspace and everything in it is deleted.
func (a *API) forgetAppUser(w http.ResponseWriter, r *http.Request) {
	var in struct {
		User string `json:"user"`
	}
	if !httpx.Decode(w, r, &in) {
		return
	}
	app, err := a.access(r, orgs.AppForgetUser)
	if err != nil {
		a.respondApps(w, nil, err)
		return
	}
	forgotten, err := a.AppAPI.ForgetUser(r.Context(), app.ID, in.User, "user:"+me(r))
	a.respondApps(w, map[string]bool{"forgotten": forgotten}, err)
}

// ── Publish URLs ─────────────────────────────────────────────────────────

type publishURL struct {
	ID    string `json:"id"`
	Topic string `json:"topic"`
	// Event, Arguments and User are an event's URL's: the event a POST
	// emits, with these arguments, and to this one subscriber for a
	// personal event. Arguments is null for one made before they were kept.
	Event        string          `json:"event,omitempty"`
	Arguments    json.RawMessage `json:"arguments,omitempty"`
	User         string          `json:"user,omitempty"`
	CreatedAt    time.Time       `json:"created_at"`
	LastUsedAt   *time.Time      `json:"last_used_at,omitempty"`
	RevokedAt    *time.Time      `json:"revoked_at,omitempty"`
	Publishes24h int             `json:"publishes_24h"`
	// Signing is its signing secret's fingerprint, never the secret: set
	// when POSTs must be signed.
	Signing *appapi.Signing `json:"signing,omitempty"`
}

func (a *API) appPublishURLs(w http.ResponseWriter, r *http.Request) {
	app, err := a.access(r, orgs.AppRead)
	if err != nil {
		a.respondApps(w, nil, err)
		return
	}
	// An event's URL shows the event, and its topic unless that's a
	// personal event's (derived from the subscriber, so it means nothing).
	rows, err := a.DB.Pool.Query(r.Context(), `SELECT u.id::text,
		CASE WHEN u.event IS NULL THEN u.topic WHEN u.topic LIKE 'u/p%' THEN u.event || ' (personal)' ELSE u.event || ' → ' || u.topic END,
		coalesce(u.event, ''), u.arguments, coalesce(u.subscriber, ''),
		u.created_at, u.last_used_at, u.revoked_at,
		(SELECT count(*) FROM topic_events e WHERE e.publish_url = u.id AND e.published_at > now() - interval '1 day'),
		u.signing_fingerprint, u.signing_set_at, CASE WHEN u.previous_signing_until > now() THEN u.previous_signing_until END
		FROM topic_publish_urls u WHERE u.app_id = $1 ORDER BY u.revoked_at IS NOT NULL, coalesce(u.revoked_at, u.created_at) DESC LIMIT 200`, app.ID)
	if err != nil {
		respond(w, nil, err)
		return
	}
	list, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (publishURL, error) {
		var u publishURL
		var fp *string
		var setAt, prevUntil *time.Time
		err := row.Scan(&u.ID, &u.Topic, &u.Event, &u.Arguments, &u.User, &u.CreatedAt, &u.LastUsedAt, &u.RevokedAt, &u.Publishes24h, &fp, &setAt, &prevUntil)
		if fp != nil && setAt != nil {
			u.Signing = &appapi.Signing{Fingerprint: *fp, SetAt: *setAt, PreviousValidUntil: prevUntil}
		}
		return u, err
	})
	if list == nil {
		list = []publishURL{}
	}
	if err != nil {
		respond(w, nil, err)
		return
	}
	// The events a new one can emit, for the page's picker: each one's
	// arguments (their names, JSON types and whether they're required).
	events := []publishEvent{}
	if full, gerr := a.Apps.Get(r.Context(), app.ID); gerr == nil {
		for _, et := range full.Manifest.Events {
			events = append(events, publishEvent{Name: et.Name, Description: et.Description, Personal: et.Personal, Arguments: argumentFields(et.Arguments)})
		}
	} else if !errors.Is(gerr, apps.ErrNotFound) {
		respond(w, nil, gerr)
		return
	}
	respond(w, map[string]any{"publish_urls": list, "events": events}, nil)
}

// publishEvent is one of the app's events, as the publish URL page offers
// it.
type publishEvent struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Personal    bool            `json:"personal"`
	Arguments   []argumentField `json:"arguments"`
}

// argumentField is one argument of an event's arguments schema.
type argumentField struct {
	Name     string `json:"name"`
	Type     string `json:"type"` // string, number, integer, boolean, or "" (anything else: entered as JSON)
	Required bool   `json:"required"`
}

// argumentFields are the top-level properties of an arguments schema, in
// name order.
func argumentFields(schema json.RawMessage) []argumentField {
	var s struct {
		Properties map[string]struct {
			Type any `json:"type"`
		} `json:"properties"`
		Required []string `json:"required"`
	}
	out := []argumentField{}
	if json.Unmarshal(schema, &s) != nil {
		return out
	}
	for name, p := range s.Properties {
		typ, _ := p.Type.(string)
		out = append(out, argumentField{Name: name, Type: typ, Required: slices.Contains(s.Required, name)})
	}
	slices.SortFunc(out, func(a, b argumentField) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// createAppPublishURL makes a publish URL for one of the app's events: its
// arguments, and a personal event's user.
func (a *API) createAppPublishURL(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Event     string          `json:"event"`
		Arguments json.RawMessage `json:"arguments"`
		User      string          `json:"user"`
		// A Standard Webhooks whsec_ secret: POSTs must be signed with it.
		SigningSecret string `json:"signing_secret"`
	}
	if !httpx.Decode(w, r, &in) {
		return
	}
	app, err := a.access(r, orgs.AppPublishURLs)
	if err == nil && app.DeletedAt != nil {
		err = apps.ErrDeleted
	}
	if err != nil {
		a.respondApps(w, nil, err)
		return
	}
	full, err := a.Apps.Get(r.Context(), app.ID)
	if errors.Is(err, apps.ErrNotFound) {
		err = apps.ErrDeleted
	}
	if err != nil {
		a.respondApps(w, nil, err)
		return
	}
	out, err := a.AppAPI.CreateEventPublishURL(r.Context(), full, strings.TrimSpace(in.Event), in.Arguments, in.User, in.SigningSecret, "user:"+me(r))
	var bad apps.EventArgsError
	switch {
	case errors.Is(err, apps.ErrUnknownEventType):
		httpx.WriteError(w, http.StatusBadRequest, "unknown_event", "the app's manifest offers no event by that name")
	case errors.As(err, &bad):
		httpx.WriteError(w, http.StatusBadRequest, "bad_arguments", bad.Error())
	default:
		a.respondApps(w, out, err)
	}
}

func (a *API) revokeAppPublishURL(w http.ResponseWriter, r *http.Request) {
	app, err := a.access(r, orgs.AppPublishURLs)
	if err != nil {
		a.respondApps(w, nil, err)
		return
	}
	err = a.AppAPI.RevokePublishURL(r.Context(), app.ID, r.PathValue("url"), "user:"+me(r))
	a.respondApps(w, map[string]bool{"revoked": true}, err)
}

// putAppPublishURLSecret sets or rotates a publish URL's signing secret
// (the previous one still works for appapi.SigningGrace). The response has
// only its fingerprint.
func (a *API) putAppPublishURLSecret(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Secret string `json:"secret"`
	}
	if !httpx.Decode(w, r, &in) {
		return
	}
	app, err := a.access(r, orgs.AppPublishURLs)
	if err != nil {
		a.respondApps(w, nil, err)
		return
	}
	s, err := a.AppAPI.SetPublishURLSecret(r.Context(), app.ID, r.PathValue("url"), in.Secret, "user:"+me(r))
	a.respondApps(w, map[string]any{"signing": s}, err)
}

// deleteAppPublishURLSecret removes a publish URL's signing secret: its
// POSTs are no longer checked.
func (a *API) deleteAppPublishURLSecret(w http.ResponseWriter, r *http.Request) {
	app, err := a.access(r, orgs.AppPublishURLs)
	if err != nil {
		a.respondApps(w, nil, err)
		return
	}
	removed, err := a.AppAPI.RemovePublishURLSecret(r.Context(), app.ID, r.PathValue("url"), "user:"+me(r))
	a.respondApps(w, map[string]bool{"removed": removed}, err)
}
