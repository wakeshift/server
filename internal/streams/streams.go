// Package streams delivers an agent's events over WebSocket.
//
// An agent gets a link: a URL with a signed reference to a stream_links row
// naming its tenant, agent and target. The link works until it expires or is
// revoked, and only while whoever created it is still a member of the
// workspace; that's checked on connect, at every renewal (Renew), and at once when the
// agent's links are revoked. The stream claims the agent's deliveries from
// the outbox, in event order, and writes one JSON message per event:
//
//	{"type":"event","delivery_id":"…","attempt":1,"subscription_id":"…","event":{…}}
//
// Links made with ack=true expect {"type":"ack","delivery_id":"…"} for each
// delivery, and confirm each once it's recorded with
// {"type":"acked","delivery_id":"…"}; deliveries without a confirmed ack are
// redelivered on reconnect, or if the connection stops renewing its lease
// (at-least-once). An ack can be recorded and its receipt lost (the
// connection drops in between): the delivery then isn't redelivered, so a
// client sends its unconfirmed acks again after reconnecting (acks are
// idempotent and always get a receipt). Once a receipt is written, the
// connection doesn't write that delivery again, even one it had claimed
// before the ack came.
//
// Without ack (consumers like Monitor that can't reply), delivery is still
// at least once, by keepalives rather than acks. A write going through
// says nothing: a proxy in front takes it whether or not its client is
// still there, and notices a vanished client only when it next writes to it.
// So every PingEvery the stream writes a keepalive (a WebSocket ping, which
// waits for the pong; an SSE comment; an empty line, which Monitor skips),
// and a delivery stays in flight, leased to the connection, until a
// keepalive written at least PingEvery after it has gone through. A
// connection that ends before then hands it back, and the next connection
// (or the next tool result, for a session nothing is connected to) gets it
// again, marked as a redelivery (attempt > 1). A connection that's lost
// (the client went away, a write failed) also hands back what it confirmed
// in the last lostWithin, for proxies that notice late. The cost is the
// occasional repeat, which the event and delivery IDs let a client spot.
//
// What a keepalive going through proves: the bytes before it left the
// server and the connection still stood. Not that the client read them.
// So this is at least once to the front end, and to the client only when
// the front end notices a vanished client within lostWithin of the write
// (by a failed write or a closed connection). A client that goes without a
// packet (a laptop asleep behind NAT) can leave a front end retrying for
// minutes; what's written then is confirmed and not sent again. Links
// with acks have no such gap.
//
// Keepalives also keep presence honest: a session counts as connected while
// its connection renews its lease (every Renew), and stops the moment
// the connection is found dead, within about two PingEvery behind a proxy.
//
// One connection holds a stream at a time; the newest wins, and the older
// one is closed with code 4000.
//
// The same link also reads the stream as Server-Sent Events or JSON lines
// (transport.go), and by long polling (poll.go).
package streams

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/audit"
	"github.com/wakeshift/server/internal/connectorversion"
	"github.com/wakeshift/server/internal/db"
	"github.com/wakeshift/server/internal/deliveries"
	"github.com/wakeshift/server/internal/events"
	"github.com/wakeshift/server/internal/framing"
	"github.com/wakeshift/server/internal/httpx"
	"github.com/wakeshift/server/internal/notify"
)

// Postgres channels the stream listens on (besides deliveries.NotifyChannel).
const (
	// TakeoverChannel is notified with the holder ID of a connection that a
	// newer one replaced.
	TakeoverChannel = "wakeshift_stream_takeover"
	// RevokedChannel is notified with "tenant:agent" when an agent's links
	// are revoked.
	RevokedChannel = "wakeshift_stream_revoked"
)

// Close codes, besides the standard ones.
const (
	CodeReplaced websocket.StatusCode = 4000 // a newer connection took the stream
	// CodeRevoked: the link was revoked or expired, or its creator left;
	// or the session has nothing left to receive, which revokes a
	// connector's links as it closes (stream.ending), since connectors
	// take any 4001 as revoked.
	CodeRevoked websocket.StatusCode = 4001
	// CodeUpdateRequired: the connector on it is older than Wakeshift
	// serves now (versions.go); its reason is "update_required".
	CodeUpdateRequired websocket.StatusCode = 4002
)

// maxAcksPerSecond bounds the database work one connection can cause: the
// frames a client sends a second, acks or anything else.
const maxAcksPerSecond = 200

// Server serves GET /v1/stream (WebSocket, SSE or lines), and with
// ServePoll and ServeAck, GET /v1/stream/poll and POST /v1/stream/ack.
type Server struct {
	DB        *db.DB
	Key       []byte // signs links
	PublicURL string
	Notify    *notify.Hub // must listen on deliveries.NotifyChannel, TakeoverChannel, RevokedChannel and PushEndedChannel

	Lease time.Duration // how long an unacknowledged delivery is held without renewal (default 60s)
	// MaxAge: connections are closed after this, to reconnect (default 50m,
	// under Cloud Run's 60-minute request limit). A session's stream sends
	// the next link first (hub.rotate).
	MaxAge time.Duration
	// OneWayMaxAge caps MaxAge for a one-way stream without acks (SSE,
	// lines; default DefaultOneWayMaxAge): behind a front end, one whose
	// client died can look connected while its keepalives go through, so it
	// ends (with hub.rotate for a session's) by then and lets go.
	OneWayMaxAge time.Duration
	// PingEvery: the keepalive interval (default 15s). A keepalive is a
	// write, no database work, except that one going through confirms the
	// deliveries it outlived (without acks; see the package doc).
	PingEvery time.Duration
	// Renew: how often a connection renews its leases (presence),
	// re-validates its link and checks for deliveries it wasn't told
	// about, in one transaction (default 30s: half the lease, and of the
	// minute that counts as connected). The check is only a safety net:
	// new and handed-back deliveries notify, the notify hub wakes every
	// stream when it reconnects, and paced deliveries set their own timer.
	Renew time.Duration
	// CursorWebhooks: Grok Bot's webhook wake is on (WAKESHIFT_GROK_WEBHOOKS),
	// so a foreground wait on a session whose webhook is bound ends
	// (webhookwait.go). Off, the deliverer skips those destinations, and
	// the foreground wait delivers their messages as before.
	CursorWebhooks bool
	// Poll: how often a waiting long poll checks for deliveries without a
	// notification (default 30s), the same safety net.
	Poll time.Duration
	// WaitLease: how long what a one-shot wait returned stays leased to it,
	// waiting to be confirmed by the next wait (default DefaultWaitLease).
	WaitLease time.Duration
	// WaitKeepalive: how often a one-shot wait writes an empty line while
	// it waits (default 30s).
	WaitKeepalive time.Duration
	// LoopPause: how long a wait loop's round (waitloop.go) waits before a
	// refusal or an empty answer it gives at once (default 2s; negative:
	// none, for tests).
	LoopPause time.Duration
	// WaitGrace: how long a session counts as connected after a wait ends,
	// for its command to run the next (default DefaultWaitGrace; at most a
	// minute). A wait counts as connected while it's waiting.
	WaitGrace time.Duration
	// RotateLead: how long before a session link expires a poll's answer
	// carries the next one (default 2m).
	RotateLead time.Duration
	// Ending, if set, says why a session's stream ends, in words for its
	// agent, in the link's tenant. With valid false the link stopped
	// working, and it says why (ok false: no more than that); with valid
	// true the stream is idle and still allowed, and it says whether the
	// session has nothing left to receive (its last subscription ended),
	// which ends the stream. appapi.API.StreamEnding is the one in use.
	Ending func(ctx context.Context, tx pgx.Tx, l Link, valid bool) (e Ending, ok bool, err error)
	// Connectors are the oldest connector versions served
	// (connectorversion.Minimums): a connector's link used by an older one
	// is refused with 426 update_required and given nothing, so what's
	// waiting stays held for the session. None: every version is served.
	// SetMinimums changes them while the server runs; read them with
	// Minimums.
	Connectors connectorversion.Minimums
	minimums   atomic.Pointer[connectorversion.Minimums]
	// MaxStreams caps the connections and long polls this instance holds
	// at once (0: no cap). Past it, new ones get 503 with Retry-After, so
	// a full instance refuses quickly rather than timing out; set it under
	// the platform's per-instance request limit to leave room for the API.
	MaxStreams int

	active   atomic.Int64 // connections and polls held now
	refusals refusals     // recent at-capacity refusals, per link
	// pushes: push streams held now, in all and per app (push.go:
	// AdmitPush), under connsMu.
	pushes      int
	pushesByApp map[string]int
	initOnce    sync.Once
	drainOnce   sync.Once
	connsMu     sync.Mutex // orders entering (conns.Add) before Drain's Wait
	drain       chan struct{}
	conns       sync.WaitGroup

	// streamsByApp: the other streams and polls held now for an app's
	// links and waits (enterApp), under connsMu.
	streamsByApp map[string]int
}

// claims is what a link's URL carries: a signed reference to its row.
type claims struct {
	Tenant string `json:"t"`
	ID     string `json:"l"`
	Exp    int64  `json:"e"`
}

// Link is a stored stream link.
type Link struct {
	ID        string
	Agent     string
	Target    string
	Ack       bool
	CreatedBy string
	GrantID   string
	ExpiresAt time.Time
	// SlideUntil, if set, is how long the link can keep sliding (NewLink.Slide):
	// its token's expiry.
	SlideUntil time.Time
	// Connector, for a link a connector redeemed a ticket for (NewLink.Connector),
	// is the connector that last used it (connectorversion.Client); "" for
	// a link an agent holds itself.
	Connector string
}

// expiry is when the link's token stops working, whatever its use.
func (l Link) expiry() time.Time {
	if !l.SlideUntil.IsZero() {
		return l.SlideUntil
	}
	return l.ExpiresAt
}

// SessionLinkTTL is how long a session's link can be used to connect: a
// copied link stops working within the TTL. A connection made in time stays
// open past it (re-checked every Renew for revocation), so an idle
// Monitor isn't woken just to move to a new link; when a connection does
// have to end (MaxAge, a restart), the stream sends {"type":"hub.rotate",
// "url":…} with the next link (same session, same position) first. One
// with acks over HTTP (SSE, lines), whose client acks with the token, ends
// that way before the token expires.
const SessionLinkTTL = 25 * time.Minute

// SessionLinkMax is how long a session link made with NewLink.Slide can
// keep working while it's used: each use moves its expiry SessionLinkTTL
// ahead, up to this long after it was made. So the command an agent runs
// with it stays the same for its session (a harness's "always allow"
// keeps matching), and a copy stops working SessionLinkTTL after the last
// use, as before. (Following a wait's hub.next, a copy could always be
// kept going; revoking the session's links ends it.)
const SessionLinkMax = 24 * time.Hour

// rotateLead is how long before a session link expires a poll gets the
// next one.
const rotateLead = 2 * time.Minute

// endCheckEvery: an idle session's stream checks whether the session is
// over every this many renewals (5 minutes by default; see stream.wrote).
const endCheckEvery = 10

// DefaultOneWayMaxAge is Server.OneWayMaxAge's default.
const DefaultOneWayMaxAge = 10 * time.Minute

// lostWithin is how far back a lost connection hands back what it
// confirmed (see the package doc).
const lostWithin = time.Minute

// NewLink is what Mint needs.
type NewLink struct {
	Agent     string
	Target    string
	Ack       bool
	CreatedBy string // "user:<id>": the link stops working if they leave the workspace; "app:<id>"
	GrantID   string // the grant an app made it under; revoking the grant revokes it
	TTL       time.Duration
	// Slide, if set: the link keeps working while it's used, each use
	// moving its expiry TTL ahead, up to Slide after now (SessionLinkMax).
	Slide time.Duration
	// Connector, for a link redeemed by a connector: the connector
	// (connectorversion.Client.String()). Its requests are checked against
	// Server.Connectors; its successors keep it.
	Connector string
}

// Mint records a link in the transaction's tenant and returns its URL.
// For a link with Slide, the time is its token's expiry (it expires sooner
// if it isn't used).
func (s *Server) Mint(ctx context.Context, tx pgx.Tx, tenant string, n NewLink) (string, time.Time, error) {
	_, u, exp, err := s.MintID(ctx, tx, tenant, n)
	return u, exp, err
}

// MintID is Mint, also returning the link's ID (for an audit entry, which
// names the link by its ID, never its URL).
func (s *Server) MintID(ctx context.Context, tx pgx.Tx, tenant string, n NewLink) (id, url string, exp time.Time, err error) {
	l, err := mint(ctx, tx, n)
	if err != nil {
		return "", "", time.Time{}, err
	}
	return l.ID, s.urlOf(tenant, l), l.expiry(), nil
}

// mint records a link in the transaction's tenant (Mint), returning it.
func mint(ctx context.Context, tx pgx.Tx, n NewLink) (Link, error) {
	now := time.Now()
	exp := now.Add(n.TTL).Truncate(time.Second)
	var slide *time.Time
	if n.Slide > 0 {
		until := now.Add(max(n.Slide, n.TTL)).Truncate(time.Second)
		slide = &until
	}
	var id string
	var grant, connector *string
	if n.GrantID != "" {
		grant = &n.GrantID
	}
	if n.Connector != "" {
		connector = &n.Connector
	}
	err := tx.QueryRow(ctx, `INSERT INTO stream_links (tenant_id, agent_id, target, ack, created_by, grant_id, expires_at, slide_until, connector)
		VALUES (current_tenant(), $1, $2, $3, $4, $5, $6, $7, $8) RETURNING id`, n.Agent, n.Target, n.Ack, n.CreatedBy, grant, exp, slide, connector).Scan(&id)
	if err != nil {
		return Link{}, err
	}
	l := Link{ID: id, Agent: n.Agent, Target: n.Target, Ack: n.Ack, CreatedBy: n.CreatedBy, GrantID: n.GrantID, ExpiresAt: exp, Connector: n.Connector}
	if slide != nil {
		l.SlideUntil = *slide
	}
	return l, nil
}

// urlOf is a stored link's URL: its token signs the link's ID and its
// token's expiry (Link.expiry), so the same link always gives the same URL.
func (s *Server) urlOf(tenant string, l Link) string {
	body, _ := json.Marshal(claims{Tenant: tenant, ID: l.ID, Exp: l.expiry().Unix()})
	b := base64.RawURLEncoding.EncodeToString(body)
	token := b + "." + base64.RawURLEncoding.EncodeToString(s.mac(b))
	base := strings.TrimSuffix(s.PublicURL, "/")
	base = "ws" + strings.TrimPrefix(base, "http") // http→ws, https→wss
	return base + "/v1/stream?" + url.Values{"link": {token}}.Encode()
}

// SessionLink is the URL of a session's sliding link (NewLink.Slide) that
// an app (createdBy, under grantID) made for it and that still works for a
// while (an hour of its SessionLinkMax left, and used or made within
// SessionLinkTTL, less a minute), in the transaction's tenant; "" if
// there's none. Handing it out again keeps the session's command the same.
// Links with acks (a hook's) aren't handed out.
func (s *Server) SessionLink(ctx context.Context, tx pgx.Tx, tenant, agent, target, createdBy, grantID string) (string, error) {
	var l Link
	err := tx.QueryRow(ctx, `SELECT id, expires_at, slide_until FROM stream_links
		WHERE target = $1 AND agent_id = $2 AND created_by = $3 AND coalesce(grant_id::text, '') = $4 AND NOT ack
			AND revoked_at IS NULL AND slide_until > now() + interval '1 hour' AND expires_at > now() + interval '1 minute'
		ORDER BY created_at DESC LIMIT 1`, target, agent, createdBy, grantID).Scan(&l.ID, &l.ExpiresAt, &l.SlideUntil)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	// Still a member, if a user made it (an app's links: always).
	if _, err := loadTx(ctx, tx, l.ID, false); errors.Is(err, errLinkInvalid) {
		return "", nil
	} else if err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx, `UPDATE stream_links SET handed_at = now() WHERE id = $1`, l.ID); err != nil {
		return "", err
	}
	return s.urlOf(tenant, l), nil
}

// URLOf is the URL of the stored link id in the transaction's tenant (as
// Mint and SessionLink give it), if it still works: ErrLinkInvalid if it's
// revoked or expired, or its creator left the workspace. For a link claim
// (appapi), which hands over the link a result named.
func (s *Server) URLOf(ctx context.Context, tx pgx.Tx, tenant, id string) (string, Link, error) {
	l, err := loadTx(ctx, tx, id, false)
	if err != nil {
		return "", l, err
	}
	return s.urlOf(tenant, l), l, nil
}

// RevokeLinks revokes every link for an agent in the transaction's tenant;
// open streams on them close (4001) once it commits, and none of its
// streams counts as connected from then on (no grace: what read them
// can't read them again).
func RevokeLinks(ctx context.Context, tx pgx.Tx, agent string) (int64, error) {
	tag, err := tx.Exec(ctx, `UPDATE stream_links SET revoked_at = now() WHERE agent_id = $1 AND revoked_at IS NULL`, agent)
	if err != nil {
		return 0, err
	}
	// Setup URLs not yet used (appapi, sessionsetup.go) would mint the
	// sessions new links: used up with them.
	if _, err := tx.Exec(ctx, `UPDATE session_setups SET used_at = now() WHERE used_at IS NULL AND session_id IN (SELECT id FROM sessions WHERE agent_id = $1)`, agent); err != nil {
		return 0, err
	}
	// Agent-hosted webhook destinations were registered with the links'
	// authority (internal/agenthooks): they end with them.
	if _, err := RemoveAgentWebhooks(ctx, tx, "user", "links_revoked", `w.agent_id = $1`, agent); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, `UPDATE stream_leases SET renewed_at = NULL WHERE agent_id = $1`, agent); err != nil {
		return 0, err
	}
	_, err = tx.Exec(ctx, `SELECT pg_notify($1, current_tenant()::text || ':' || $2)`, RevokedChannel, agent)
	return tag.RowsAffected(), err
}

// RemoveAgentWebhooks deletes the transaction's workspace's agent-hosted
// webhook destinations (Capy's, Grok Bot's) matching where (on the alias
// w), recording an agent_webhook.unregistered audit entry for each, in the
// same transaction, with actor and reason. It returns how many it
// removed. Every path that removes a destination goes through it, or
// records its own entry.
func RemoveAgentWebhooks(ctx context.Context, tx pgx.Tx, actor, reason, where string, args ...any) (int, error) {
	type removed struct{ Agent, App, Grant, Kind, Handle string }
	rows, _ := tx.Query(ctx, `DELETE FROM agent_webhooks w WHERE `+where+`
		RETURNING w.agent_id, w.app_id, coalesce(w.grant_id::text, ''), w.kind, coalesce((SELECT handle FROM sessions s WHERE s.id = w.session_id), '')`, args...)
	gone, err := pgx.CollectRows(rows, pgx.RowToStructByPos[removed])
	if err != nil {
		return 0, err
	}
	for _, r := range gone {
		if err := audit.Record(ctx, tx, audit.Entry{Actor: actor, Action: audit.AgentWebhookUnregistered, AgentID: r.Agent, AppID: r.App, GrantID: r.Grant,
			Detail: map[string]any{"session": r.Handle, "kind": r.Kind, "reason": reason}}); err != nil {
			return 0, err
		}
	}
	return len(gone), nil
}

// RevokeSessionLinks revokes the links for one of an agent's sessions (its
// target); open streams on them close (4001) once it commits, and the
// session stops counting as connected at once (no wait's grace: the
// command on them can't go on, and the next watch must give a new one).
func RevokeSessionLinks(ctx context.Context, tx pgx.Tx, agent, target string) (int64, error) {
	tag, err := tx.Exec(ctx, `UPDATE stream_links SET revoked_at = now() WHERE agent_id = $1 AND target = $2 AND revoked_at IS NULL`, agent, target)
	if err != nil {
		return 0, err
	}
	if uuidRE.MatchString(target) { // the session's setup URLs, used up with its links
		if _, err := tx.Exec(ctx, `UPDATE session_setups SET used_at = now() WHERE used_at IS NULL AND session_id = $1::uuid`, target); err != nil {
			return 0, err
		}
	}
	// The session's agent-hosted webhook destination ends with its links
	// (internal/agenthooks). Only a session's target is a uuid, compared
	// as one so the key serves it.
	if uuidRE.MatchString(target) {
		if _, err := RemoveAgentWebhooks(ctx, tx, "agent", "unwatched", `w.agent_id = $1 AND w.session_id = $2::uuid`, agent, target); err != nil {
			return 0, err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE stream_leases SET renewed_at = NULL WHERE agent_id = $1 AND target = $2`, agent, target); err != nil {
		return 0, err
	}
	_, err = tx.Exec(ctx, `SELECT pg_notify($1, current_tenant()::text || ':' || $2)`, RevokedChannel, agent)
	return tag.RowsAffected(), err
}

// LockRedeems takes, until tx ends, the lock that orders a connector's
// redeems for session (a session's ID, in the transaction's workspace:
// appapi's redeemProven) with each other and with a connector's stream on
// the session ending (stream.ending), so a redeem sees the session's
// connector links either all live or all revoked by that end.
func LockRedeems(ctx context.Context, tx pgx.Tx, session string) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('redeem:' || current_tenant()::text || ':' || $1))`, session)
	return err
}

// RevokeGrantLinks revokes the links an app made under a grant; open streams
// on them close once it commits. by is who ended the grant, for the audit
// entries of the destinations that end with it.
func RevokeGrantLinks(ctx context.Context, tx pgx.Tx, grantID, by string) error {
	if _, err := tx.Exec(ctx, `UPDATE session_setups SET used_at = now() WHERE used_at IS NULL AND grant_id = $1`, grantID); err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `UPDATE stream_links SET revoked_at = now() WHERE grant_id = $1 AND revoked_at IS NULL RETURNING agent_id`, grantID)
	if err != nil {
		return err
	}
	agents, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	// Agent-hosted webhook destinations registered under the grant end
	// with it (internal/agenthooks).
	if _, err := RemoveAgentWebhooks(ctx, tx, by, "grant_ended", `w.grant_id = $1`, grantID); err != nil {
		return err
	}
	// The streams read on them stop counting as connected now, unless a
	// link that still works is left for the same stream.
	if _, err := tx.Exec(ctx, `UPDATE stream_leases l SET renewed_at = NULL
		WHERE EXISTS (SELECT 1 FROM stream_links k WHERE k.grant_id = $1 AND k.agent_id = l.agent_id AND k.target = l.target)
		AND NOT EXISTS (SELECT 1 FROM stream_links k WHERE k.agent_id = l.agent_id AND k.target = l.target AND k.revoked_at IS NULL AND k.expires_at > now())`, grantID); err != nil {
		return err
	}
	for _, a := range agents {
		if _, err := tx.Exec(ctx, `SELECT pg_notify($1, current_tenant()::text || ':' || $2)`, RevokedChannel, a); err != nil {
			return err
		}
	}
	return nil
}

var (
	errLinkInvalid = errors.New("this stream link no longer works: it was revoked or expired, or its creator left the workspace")
	errLinkExpired = errors.New("this stream link expired")
)

func (s *Server) parse(token string) (claims, error) {
	var c claims
	b, sig, ok := strings.Cut(token, ".")
	if !ok {
		return c, errors.New("malformed link")
	}
	got, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil || !hmac.Equal(got, s.mac(b)) {
		return c, errors.New("invalid link")
	}
	body, err := base64.RawURLEncoding.DecodeString(b)
	if err != nil || json.Unmarshal(body, &c) != nil || c.Tenant == "" || c.ID == "" {
		return c, errors.New("invalid link")
	}
	if time.Now().Unix() > c.Exp {
		return c, errLinkExpired
	}
	return c, nil
}

// Held is the stored link a stream link (its URL, or the token) names, in
// its tenant, if Wakeshift signed it: revoked or expired, it still proves
// that whoever presents it held the link. ok is false for anything else.
// For a hook that hands back the link it held for a conversation when it
// connects that conversation again (the redeem's supersedes).
func (s *Server) Held(link string) (tenant, id string, ok bool) {
	token := link
	if u, err := url.Parse(link); err == nil && u.Query().Get("link") != "" {
		token = u.Query().Get("link")
	}
	c, err := s.parse(token)
	if err != nil && !errors.Is(err, errLinkExpired) {
		return "", "", false
	}
	return c.Tenant, c.ID, true
}

// LinkByID is a stored link in the transaction's tenant, revoked or
// expired (ErrNoLink if there's none).
func LinkByID(ctx context.Context, tx pgx.Tx, id string) (Link, error) {
	var l Link
	var slide *time.Time
	err := tx.QueryRow(ctx, `SELECT id, agent_id, target, ack, created_by, coalesce(grant_id::text, ''), expires_at, slide_until, coalesce(connector, '') FROM stream_links WHERE id = $1`, id).
		Scan(&l.ID, &l.Agent, &l.Target, &l.Ack, &l.CreatedBy, &l.GrantID, &l.ExpiresAt, &slide, &l.Connector)
	if errors.Is(err, pgx.ErrNoRows) {
		return l, ErrNoLink
	}
	if slide != nil {
		l.SlideUntil = *slide
	}
	return l, err
}

// ErrNoLink: no stored link has that ID here.
var ErrNoLink = errors.New("no such stream link")

// load returns the link if it's still good: not revoked or expired, and
// its creator still belongs to the workspace. Each load marks it used (a
// stream, poll or wait opened on it: used_at), and a sliding link's
// (NewLink.Slide) moves its expiry SessionLinkTTL ahead (slide).
//
// r is the request that opened it: the connector it names is recorded in
// the same transaction, and with check, one older than Server.Connectors
// serves is refused (*UpdateRequired) once that's recorded (versions.go).
func (s *Server) load(ctx context.Context, c claims, r *http.Request, check bool) (Link, error) {
	header := r.Header.Get(connectorversion.Header)
	return s.use(ctx, c, false, func(tx pgx.Tx, l *Link) (error, error) {
		return s.seen(ctx, tx, l, header, check)
	})
}

// loadAs is load for a connection made while the link was good (live:
// the link's expiry doesn't end it; SessionLinkTTL), re-checking it.
func (s *Server) loadAs(ctx context.Context, c claims, live bool) (Link, error) {
	return s.use(ctx, c, live, nil)
}

// use loads the link (loadTx), runs also if it's set (seen), then marks
// the link used and slides it, all in one transaction: a refusal also
// returns (committed with the rest) is load's error.
//
// also runs first because seen writes the session's row before the
// link's, the order a grace renewal and an unsubscribe stopping the
// session take them in; marking the link used locks its row, so it comes
// after. loadTx read the link without a lock, so marking it used checks
// again that it isn't revoked: an unsubscribe that revoked it while this
// waited for the session's row has its way (errLinkInvalid).
func (s *Server) use(ctx context.Context, c claims, live bool, also func(tx pgx.Tx, l *Link) (refused, err error)) (Link, error) {
	var l Link
	var refused error
	err := s.DB.InTenant(ctx, c.Tenant, func(tx pgx.Tx) (err error) {
		if l, err = loadTx(ctx, tx, c.ID, live); err != nil {
			return err
		}
		if also != nil {
			if refused, err = also(tx, &l); err != nil {
				return err
			}
		}
		tag, err := tx.Exec(ctx, `UPDATE stream_links SET used_at = now() WHERE id = $1 AND revoked_at IS NULL`, c.ID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return errLinkInvalid
		}
		return slide(ctx, tx, &l)
	})
	if err == nil {
		err = refused
	}
	return l, err
}

// slide moves a sliding link's expiry SessionLinkTTL ahead (up to its
// SlideUntil), inside its tenant: it's in use.
func slide(ctx context.Context, tx pgx.Tx, l *Link) error {
	if l.SlideUntil.IsZero() {
		return nil
	}
	return tx.QueryRow(ctx, `UPDATE stream_links SET expires_at = greatest(expires_at, least(slide_until, now() + $2::interval))
		WHERE id = $1 RETURNING expires_at`, l.ID, SessionLinkTTL.String()).Scan(&l.ExpiresAt)
}

// loadTx is load inside the link's tenant (live: see loadAs).
func loadTx(ctx context.Context, tx pgx.Tx, id string, live bool) (Link, error) {
	var l Link
	var revoked, slide *time.Time
	err := tx.QueryRow(ctx, `SELECT id, agent_id, target, ack, created_by, coalesce(grant_id::text, ''), expires_at, revoked_at, slide_until, coalesce(connector, '') FROM stream_links WHERE id = $1`, id).
		Scan(&l.ID, &l.Agent, &l.Target, &l.Ack, &l.CreatedBy, &l.GrantID, &l.ExpiresAt, &revoked, &slide, &l.Connector)
	if errors.Is(err, pgx.ErrNoRows) {
		return l, errLinkInvalid
	}
	if err != nil {
		return l, err
	}
	if slide != nil {
		l.SlideUntil = *slide
	}
	if revoked != nil || !live && time.Now().After(l.ExpiresAt) {
		return l, errLinkInvalid
	}
	if user, ok := strings.CutPrefix(l.CreatedBy, "user:"); ok {
		var member bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM memberships WHERE tenant_id = current_tenant() AND user_id = $1)`, user).Scan(&member); err != nil {
			return l, err
		}
		if !member {
			return l, errLinkInvalid
		}
	}
	// A link made under a grant ends with it: at its end date, not only
	// once the expiry pass revokes it.
	if l.GrantID != "" {
		var live bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM grants WHERE id = $1 AND revoked_at IS NULL AND expires_at > now())`, l.GrantID).Scan(&live); err != nil {
			return l, err
		}
		if !live {
			return l, errLinkInvalid
		}
	}
	return l, nil
}

func (s *Server) mac(b string) []byte {
	m := hmac.New(sha256.New, s.Key)
	m.Write([]byte("stream-link:" + b))
	return m.Sum(nil)
}

// Drain closes every connection with "service restart" so clients reconnect
// to another instance, releasing what they held. Call it before shutting
// the HTTP server down; it waits (until ctx ends) for the connections to
// finish.
func (s *Server) Drain(ctx context.Context) {
	s.connsMu.Lock()
	s.drainOnce.Do(func() { close(s.drainCh()) })
	s.connsMu.Unlock()
	done := make(chan struct{})
	go func() { s.conns.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
	}
}

// errBusy: the instance holds MaxStreams already.
var errBusy = errors.New("streams: at capacity")

// enter counts a connection or poll in, unless the server is draining
// (errDraining) or full (errBusy, with a wait that grows each time the
// same key, a link or stream, is refused); call s.leave when it ends.
func (s *Server) enter(key string) error {
	s.connsMu.Lock()
	defer s.connsMu.Unlock()
	if s.draining() {
		return errDraining
	}
	if s.MaxStreams > 0 && s.active.Load() >= int64(s.MaxStreams) {
		return busyError{after: s.refusals.refused(key, time.Now())}
	}
	s.refusals.admitted(key)
	s.active.Add(1)
	s.conns.Add(1)
	return nil
}

// atCapacity is enter's capacity check alone, in memory, for a wait or a
// loop's round before it does any database work: errBusy, with the wait
// for key's refusal (counted as enter counts them), if the instance is
// full. enter checks again as the round takes its place; key's refusals
// are forgotten only once it got in (admittedAs).
func (s *Server) atCapacity(key string) error {
	if s.MaxStreams > 0 && s.active.Load() >= int64(s.MaxStreams) {
		d, n := s.refusals.refusedN(key, time.Now())
		return busyError{after: d, first: n == 1}
	}
	return nil
}

// admittedAs forgets key's refusals (atCapacity) once its wait or round
// got past enter (err isn't errBusy).
func (s *Server) admittedAs(key string, err error) {
	if s.MaxStreams > 0 && !errors.Is(err, errBusy) {
		s.refusals.admitted(key)
	}
}

func (s *Server) leave() {
	s.active.Add(-1)
	s.conns.Done()
}

// An app's share of an instance's MaxStreams, for the streams and polls on
// links an app made and its wait tool's (push streams have their own,
// PushAppShare): an app holding AppShare of MaxStreams or more gets
// another only while the instance has more than AppReserve of it free. So
// one app's users (or one app minting links) can't take every place: the
// last AppReserve is kept for the other apps and for agents' own links,
// while an app alone on a quiet instance isn't held to its share.
const (
	AppShare   = 0.25
	AppReserve = 0.25
)

// appCaps are an instance's per-app share and reserve for maxStreams (0:
// none).
func appCaps(maxStreams int) (share, reserve int) {
	if maxStreams <= 0 {
		return 0, 0
	}
	return max(int(float64(maxStreams)*AppShare), 1), max(int(float64(maxStreams)*AppReserve), 1)
}

// enterApp counts a stream or poll enter let in as app's ("": none, not
// counted), unless app holds its share (AppShare) and the instance has no
// more than AppReserve free (errBusy, refused as key, as enter refuses);
// call s.leaveApp(app) when it ends, before s.leave.
func (s *Server) enterApp(key, app string) error {
	if app == "" {
		return nil
	}
	s.connsMu.Lock()
	defer s.connsMu.Unlock()
	share, reserve := appCaps(s.MaxStreams)
	// active counts this one already (enter): refused if the others leave
	// no more than the reserve.
	if s.MaxStreams > 0 && s.streamsByApp[app] >= share && s.active.Load()-1 >= int64(s.MaxStreams-reserve) {
		slog.Warn("streams: an app's streams past their share of a busy instance", "app", app, "app_streams", s.streamsByApp[app],
			"app_share", share, "active", s.active.Load())
		return busyError{after: s.refusals.refused(key, time.Now())}
	}
	if s.streamsByApp == nil {
		s.streamsByApp = map[string]int{}
	}
	s.streamsByApp[app]++
	return nil
}

// leaveApp counts out a stream enterApp counted in as app's.
func (s *Server) leaveApp(app string) {
	if app == "" {
		return
	}
	s.connsMu.Lock()
	if s.streamsByApp[app]--; s.streamsByApp[app] <= 0 {
		delete(s.streamsByApp, app)
	}
	s.connsMu.Unlock()
}

// linkApp is the app that made l ("app:<id>"), or "" for an agent's own
// link (the console's, a user's).
func linkApp(l Link) string {
	if app, ok := strings.CutPrefix(l.CreatedBy, "app:"); ok {
		return app
	}
	return ""
}

// Active is how many connections and long polls this instance holds.
func (s *Server) Active() int64 { return s.active.Load() }

// turnAway writes a refusal to open a stream: restarting or at capacity.
func turnAway(w http.ResponseWriter, err error) {
	if errors.Is(err, errBusy) {
		w.Header().Set("Retry-After", retryAfterOf(err))
		httpx.WriteError(w, http.StatusServiceUnavailable, "at_capacity", "this server is at capacity; retry after the Retry-After seconds")
		return
	}
	w.Header().Set("Retry-After", "1")
	httpx.WriteError(w, http.StatusServiceUnavailable, "restarting", "server restarting; reconnect")
}

func (s *Server) drainCh() chan struct{} {
	s.initOnce.Do(func() { s.drain = make(chan struct{}) })
	return s.drain
}

func (s *Server) draining() bool {
	select {
	case <-s.drainCh():
		return true
	default:
		return false
	}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	format, err := streamFormat(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	limit, err := streamMax(r, format)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// The link's token: ?link=, else Authorization: Bearer (as the poll
	// takes it), which keeps it out of URLs and proxies' logs.
	tok := r.URL.Query().Get("link")
	if tok == "" {
		tok = linkToken(r)
	}
	c, err := s.parse(tok)
	if errors.Is(err, errLinkExpired) {
		if !s.renewStream(w, r, format, limit, c) {
			s.refuse(w, r, format, c)
		}
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	// Counted in before any database work, so a full instance refuses
	// without any.
	if err := s.enter("link:" + c.ID); err != nil {
		turnAway(w, err)
		return
	}
	defer s.leave()
	l, err := s.load(r.Context(), c, r, true)
	if errors.Is(err, errLinkInvalid) {
		if !s.renewStream(w, r, format, limit, c) {
			s.refuse(w, r, format, c)
		}
		return
	}
	if s.writeUpdateRequired(w, err) {
		return
	}
	if err != nil {
		slog.Error("streams: loading link", "err", err)
		w.Header().Set("Retry-After", "2")
		http.Error(w, "try again", http.StatusServiceUnavailable)
		return
	}
	// An app's link: within the app's share of the instance (enterApp).
	app := linkApp(l)
	if err := s.enterApp("link:"+c.ID, app); err != nil {
		turnAway(w, err)
		return
	}
	defer s.leaveApp(app)
	if format != "" {
		t := newHTTPTransport(w, format)
		t.acks = l.Ack
		st := &stream{s: s, t: t, claims: c, l: l, holder: newHolder()}
		if limit > 0 {
			t.max, st.max, st.holder = limit, limit, holderID(backgroundPrefix)
		}
		if format == FormatSSE {
			st.lastEventID = r.Header.Get("Last-Event-ID")
		}
		st.run(r.Context())
		return
	}
	ws, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer ws.CloseNow()
	ws.SetReadLimit(4 << 10)
	holder := newHolder()
	if betweenTurns(l) {
		holder = holderID(deliveries.TurnHolderPrefix)
	}
	(&stream{s: s, t: wsTransport{ws}, claims: c, l: l, holder: holder}).run(r.Context())
}

// renewStream answers a connection on an expired session link that can be
// renewed (renewExpired) with the hub.rotate frame naming its successor,
// in the connection's form (format, and a one-way stream's max), and ends
// it, as a stream that reached its age limit does; false, having written
// nothing, if it can't be renewed (the caller refuses it as before).
func (s *Server) renewStream(w http.ResponseWriter, r *http.Request, format string, limit int, c claims) bool {
	next, exp, _, err := s.renewExpired(r.Context(), c, r.Header.Get(connectorversion.Header), true)
	switch {
	case errors.Is(err, errNoGrace):
		return false
	case s.writeUpdateRequired(w, err):
		return true
	case err != nil:
		slog.Error("streams: renewing an expired link", "err", err)
		w.Header().Set("Retry-After", "2")
		http.Error(w, "try again", http.StatusServiceUnavailable)
		return true
	}
	u, _ := url.Parse(next)
	var t transport
	if format != "" {
		ht := newHTTPTransport(w, format)
		ht.max = limit
		t = ht
	} else {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return true
		}
		defer ws.CloseNow()
		t = wsTransport{ws}
	}
	t.send(r.Context(), rotateFrame{Type: "hub.rotate", URL: t.url(next), Link: u.Query().Get("link"), ExpiresAt: exp.UTC().Format(time.RFC3339), Note: renewalNote}, "")
	t.end(endRotated, Ending{})
	return true
}

// betweenTurns reports whether a WebSocket on l is a connector that
// delivers only between its conversation's turns: a connector's link with
// acks (Codex's connector, the only connector that holds a WebSocket,
// starts a turn once the thread is idle and acks then). What it holds
// while the conversation is busy can be taken in the turn (an app's wait
// tool: Server.TakeHeld), and the connection is told (announceTaken).
func betweenTurns(l Link) bool { return l.Ack && l.Connector != "" }

// refuse answers a connection on a link that no longer works (401), saying
// why: for a one-way format, as the hub.closed line a stream would end with
// (curl prints it, so Monitor shows it).
func (s *Server) refuse(w http.ResponseWriter, r *http.Request, format string, c claims) {
	why := s.endingOf(r.Context(), c, false)
	if format == "" {
		http.Error(w, why.Note, http.StatusUnauthorized)
		return
	}
	b, _ := json.Marshal(closedFrame(why))
	w.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
	w.WriteHeader(http.StatusUnauthorized)
	w.Write(append(b, '\n'))
}

// endingOf is why a link's stream can't go on: the session's own reason
// (Server.Ending, valid false), else that the link expired or was revoked.
// live: the link's expiry doesn't count (a connection made in time).
func (s *Server) endingOf(ctx context.Context, c claims, live bool) Ending {
	why := endingRevoked
	err := s.DB.InTenant(ctx, c.Tenant, func(tx pgx.Tx) error {
		var l Link
		var revoked, renewed bool
		err := tx.QueryRow(ctx, `SELECT id, agent_id, target, ack, created_by, coalesce(grant_id::text, ''), expires_at, revoked_at IS NOT NULL, renewed_at IS NOT NULL
			FROM stream_links WHERE id = $1`, c.ID).
			Scan(&l.ID, &l.Agent, &l.Target, &l.Ack, &l.CreatedBy, &l.GrantID, &l.ExpiresAt, &revoked, &renewed)
		if errors.Is(err, pgx.ErrNoRows) {
			why = endingExpired // deleted a day after it expired
			// Or with its workspace (an app forgetting its user).
			held := false
			if uuidRE.MatchString(c.Tenant) {
				if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM tenants WHERE id = $1::uuid)`, c.Tenant).Scan(&held); err != nil {
					return err
				}
			}
			if !held {
				why = endingForgotten
			}
			return nil
		}
		if err != nil {
			return err
		}
		if !revoked && !live && time.Now().After(l.ExpiresAt) {
			why = endingExpired
		}
		if renewed {
			why = endingRenewed // the grace path renewed it (grace.go)
		}
		if s.Ending == nil || l.Target == "" {
			return nil
		}
		e, ok, err := s.Ending(ctx, tx, l, false)
		// A link that only expired says so (get a new one), unless the
		// session itself is over; "revoked" is all the session can add then.
		if ok && !((why == endingExpired || why == endingRenewed) && e.Reason == endingRevoked.Reason) {
			why = e
		}
		return err
	})
	if err != nil {
		slog.Warn("streams: explaining a link's end failed", "err", err)
	}
	return why
}

type stream struct {
	s      *Server
	t      transport
	claims claims
	l      Link
	holder string
	// prev is the connection this one took the stream from.
	prev string
	// unconfirmed: written on a link without acks, not yet outlived by a
	// keepalive (see the package doc), oldest first.
	unconfirmed []sent
	// lost: the connection ended without the server ending it (the client
	// went away, or a write failed).
	lost bool
	// wrote: something was written since the session's end was last
	// checked (Server.Ending). A session ends with a last delivery (the
	// notice that its subscription ended), so an idle stream checks only
	// every endCheckEvery renewals, in case one ended without.
	wrote    bool
	renewals int
	// ackedCh: an ack was just recorded (readAcks), so the session's end is
	// checked again at once. A session whose last subscription ended keeps
	// its stream while what it wrote is unacknowledged (Server.Ending
	// counts it as still owed), and ends right after the last ack.
	ackedCh chan struct{}

	replaced <-chan struct{}
	// yield: don't take the stream from a live connection or wait (a wait
	// tool mustn't stop a Monitor, connector or background command that
	// wakes the session).
	yield bool
	// yieldBackground: don't take it from a live one-shot wait (isWait):
	// the agent's own wait command, which wakes the session. Not from a
	// long-lived background stream (a lines stream with max=): behind a
	// front end, a stream whose client died can look live for many
	// minutes, and a hook mustn't stand down for that.
	yieldBackground bool
	// yieldUntil: when take yielded, when the holder it yielded to stops
	// counting as live (unless it renews).
	yieldUntil time.Time
	// max: a one-way stream with ?max= ends after this many events (0: no
	// limit), so a command that wakes its agent on output exits, and wakes
	// it again, before the harness stops listening to it. count is how many
	// it has written; full, that it reached max (at fullAt) and only waits
	// to confirm them before it ends (settled).
	max, count int
	full       bool
	fullAt     time.Time
	// lastEventID: the last SSE event a reconnecting client saw.
	lastEventID string
	// held: for a between-turns connection (betweenTurns), what it was
	// written and hasn't acked, which another path may take meanwhile
	// (announceTaken). readAcks removes from it too, so heldMu guards it.
	heldMu sync.Mutex
	held   map[string]bool
	// acked: what readAcks acknowledged since the current batch was
	// claimed. A client that reconnects sends again the acks it had no
	// receipt for, so one can land between the claim of a delivery handed
	// back by its last connection and the write: the frame then isn't
	// written, since its receipt may already have gone. ackMu orders the
	// two: readAcks records an ack and writes its receipt under it, and
	// pump checks and writes each frame under it.
	ackMu sync.Mutex
	acked map[string]bool
}

// turns reports whether this is a between-turns connection (betweenTurns).
func (st *stream) turns() bool { return strings.HasPrefix(st.holder, deliveries.TurnHolderPrefix) }

// sent is a delivery written to the connection, and when.
type sent struct {
	id string
	at time.Time
}

func newHolder() string { return holderID("conn_") }

func holderID(prefix string) string {
	b := make([]byte, 8)
	rand.Read(b)
	return prefix + hex.EncodeToString(b)
}

// isConnection reports whether a lease holder is a connection (WebSocket,
// SSE or lines) rather than a poll.
func isConnection(holder string) bool { return strings.HasPrefix(holder, "conn_") }

// Holders of background commands an agent starts itself to be woken: a
// lines stream with ?max= (Cursor's Shell with notify_on_output) and a
// one-shot wait (Antigravity's run_command). A wait is a poll to presence
// ("poll_…"), a capped stream a connection.
const (
	backgroundPrefix = "conn_bg_"
	waitPrefix       = "poll_wait_"
)

// isWait reports whether a lease holder is a one-shot wait (wait.go).
func isWait(holder string) bool { return strings.HasPrefix(holder, waitPrefix) }

func (st *stream) tx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	return st.s.DB.InTenant(ctx, st.claims.Tenant, fn)
}

// finishing gives cleanup work its own short deadline, even after the
// connection's context has ended.
func finishing(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
}

func (st *stream) run(reqCtx context.Context) {
	ctx, cancel := context.WithCancel(reqCtx)
	defer cancel()
	log := slog.With("tenant", st.claims.Tenant, "agent", st.l.Agent, "target", st.l.Target, "link", st.l.ID, "holder", st.holder, "ack", st.l.Ack)

	// Wait before taking the lease, so no notification is missed between.
	key := st.claims.Tenant + ":" + st.l.Agent
	wake, stopWake := st.s.Notify.Wait(deliveries.NotifyChannel, key)
	defer stopWake()
	replaced, stopReplaced := st.s.Notify.Wait(TakeoverChannel, st.holder)
	defer stopReplaced()
	st.replaced = replaced
	revoked, stopRevoked := st.s.Notify.Wait(RevokedChannel, key)
	defer stopRevoked()
	// The link was checked before these waiters existed; check again (in
	// the lease's transaction), so a revocation in between isn't missed.
	switch err := st.takeLease(ctx, st.validLink, nil); {
	case errors.Is(err, errLinkInvalid):
		why := st.s.endingOf(ctx, st.claims, true)
		log.Info("streams: link no longer valid; closing", "reason", why.Reason)
		st.t.end(endRevoked, why)
		return
	case err != nil:
		log.Warn("streams: taking the lease failed", "err", err)
		st.t.end(endError, endingError)
		return
	}
	// Whatever this connection still holds goes back to pending; if it was
	// lost, so does what it confirmed lately.
	defer func() {
		fctx, fcancel := finishing(ctx)
		defer fcancel()
		if err := st.tx(fctx, func(tx pgx.Tx) error {
			// No longer present (unless a newer connection holds it).
			if _, err := tx.Exec(fctx, `UPDATE stream_leases SET renewed_at = NULL WHERE agent_id = $1 AND target = $2 AND holder = $3`,
				st.l.Agent, st.l.Target, st.holder); err != nil {
				return err
			}
			if st.lost && !st.l.Ack {
				if _, err := deliveries.ResendLost(fctx, tx, st.l.Agent, st.holder, lostWithin); err != nil {
					return err
				}
			}
			return deliveries.Release(fctx, tx, st.holder)
		}); err != nil {
			log.Warn("streams: releasing failed (the lease will expire)", "err", err)
		}
	}()
	if st.lastEventID != "" && !st.l.Ack {
		if err := st.tx(ctx, func(tx pgx.Tx) error {
			_, err := deliveries.Resend(ctx, tx, st.l.Agent, st.l.Target, st.lastEventID, st.prev, resendWithin)
			return err
		}); err != nil {
			log.Warn("streams: resending after Last-Event-ID failed", "err", err)
		}
	}
	log.Info("streams: connected")

	if ws, ok := st.t.(wsTransport); ok {
		st.ackedCh = make(chan struct{}, 1)
		go st.readAcks(ctx, cancel, ws.c)
	}

	ping := time.NewTicker(st.s.pingEvery())
	defer ping.Stop()
	// The first renewal at a random point within the interval, then every
	// interval: streams that connected together (after a deploy, a
	// failover) otherwise renew in step for the rest of the day.
	renew := time.NewTimer(phase(st.s.renew()))
	defer renew.Stop()
	// Fires when the earliest paced delivery becomes available.
	paced := time.NewTimer(time.Hour)
	paced.Stop()
	defer paced.Stop()
	// Ages shortened by up to a fifth at random (jitterDown), so streams
	// that connected together don't all end and reconnect together.
	age := jitterDown(st.s.maxAge())
	if _, ok := st.t.(*httpTransport); ok && !st.l.Ack {
		// A one-way stream without acks (SSE, lines): behind a front end, a
		// client that died can look connected while keepalives go through,
		// holding the stream. It ends (handing over its next link) within
		// OneWayMaxAge, so a dead one lets go by then.
		age = min(age, jitterDown(st.s.oneWayMaxAge()))
	}
	if _, ok := st.t.(*httpTransport); ok && st.l.Ack && st.l.Target != "" {
		// A session's stream with acks over HTTP (SSE, lines: Hermes's and
		// OpenClaw's plugins) is acked through POST /v1/stream/ack with the
		// link's token, which stops working at the link's expiry while the
		// connection could stay open until MaxAge: a delivery after that
		// couldn't be acked (401 link_expired, which the plugins read as a
		// link that's gone). So it hands over the next link (hub.rotate)
		// rotateLead before the token expires. A WebSocket acks over the
		// connection itself, and a sliding link's token lasts its Slide.
		age = min(age, max(time.Until(st.l.expiry())-st.s.rotateLead(), time.Second))
	}
	maxAge := time.NewTimer(age)
	defer maxAge.Stop()
	// Fires when a failed claim transaction is due to be retried.
	retryPump := time.NewTimer(time.Hour)
	retryPump.Stop()
	defer retryPump.Stop()
	pumpFailures := 0

	// The stream checks for deliveries when it connects, when notified,
	// when a paced one comes due, and when a renewal finds some it wasn't
	// told about (the safety net). Otherwise only renewals touch the
	// database, and a keepalive that confirms deliveries it outlived.
	due := true
	for {
		if due {
			due = false
			st.announceTaken(ctx, log)
			next, err := st.pump(ctx)
			switch {
			case errors.Is(err, errDisplaced):
				st.t.end(endReplaced, endingReplaced)
				return
			case errors.Is(err, errDraining):
				st.restart(ctx, log)
				return
			case err != nil:
				if ctx.Err() == nil {
					// Its workspace may have gone meanwhile (an app forgetting
					// its user): then the link is invalid, and the stream says so.
					if !st.stillValid(ctx, log) {
						return
					}
					// The database failed (a failover, a pool briefly full):
					// retried, keeping the connection, as a renewal is.
					var dbErr pumpDBError
					if errors.As(err, &dbErr) && pumpFailures < pumpRetries {
						wait := jitter(pumpRetryWait << pumpFailures)
						pumpFailures++
						log.Warn("streams: delivering failed; retrying (keeping the connection)", "err", err, "attempt", pumpFailures, "wait", wait)
						retryPump.Reset(wait)
						break
					}
					log.Warn("streams: delivering failed", "err", err)
				}
				st.lost = true
				return
			default:
				pumpFailures = 0
			}
			if !next.IsZero() {
				paced.Reset(max(time.Until(next), 0) + 10*time.Millisecond)
			}
		}
		if st.full && st.settled(ctx) {
			st.capped(ctx, log)
			return
		}
		select {
		case <-ctx.Done():
			st.lost = true
			return
		case <-st.s.drainCh():
			st.restart(ctx, log)
			return
		case <-replaced:
			st.t.end(endReplaced, endingReplaced)
			return
		case <-maxAge.C:
			if st.l.Target != "" {
				st.rotate(ctx, log, "This connection reached its age limit: start the same watch again with this url. Nothing was missed.")
				return
			}
			st.t.end(endReconnect, endingReconnect)
			return
		case <-revoked:
			if !st.stillValid(ctx, log) {
				return
			}
		case <-ping.C:
			if err := st.t.ping(ctx); err != nil {
				st.lost = true
				return
			}
			// What the connection has now outlived counts as delivered.
			if why, done := st.confirm(ctx, log); done {
				log.Info("streams: nothing left for the session; closing", "reason", why.Reason)
				st.t.end(endRevoked, why)
				return
			}
			if st.full && st.settled(ctx) {
				st.capped(ctx, log)
				return
			}
		case <-renew.C:
			renew.Reset(st.s.renew())
			r, err := st.renew(ctx)
			due = r.pending
			st.announceTaken(ctx, log) // the safety net, for a missed notification
			switch {
			case errors.Is(err, errDisplaced):
				st.t.end(endReplaced, endingReplaced)
				return
			case errors.Is(err, errLinkInvalid):
				why := st.s.endingOf(ctx, st.claims, true)
				log.Info("streams: link no longer valid; closing", "reason", why.Reason)
				st.t.end(endRevoked, why)
				return
			case st.endOutdated(err, log):
				return
			case err != nil && ctx.Err() == nil:
				log.Warn("streams: renewing leases failed (keeping the connection)", "err", err)
			case r.done:
				log.Info("streams: nothing left for the session; closing", "reason", r.why.Reason)
				st.t.end(endRevoked, r.why)
				return
			}
		case <-wake:
			due = true
		case <-paced.C:
			due = true
		case <-retryPump.C:
			due = true
		case <-st.ackedCh:
			if why, done := st.ended(ctx, log); done {
				log.Info("streams: nothing left for the session; closing", "reason", why.Reason)
				st.t.end(endRevoked, why)
				return
			}
		}
	}
}

// ended checks, after an ack, whether the session has nothing left to
// receive (Server.Ending: nothing active, pending or still in flight), so
// a stream whose last delivery was just acknowledged ends at once rather
// than at a later renewal.
func (st *stream) ended(ctx context.Context, log *slog.Logger) (why Ending, done bool) {
	if st.s.Ending == nil || st.l.Target == "" || len(st.unconfirmed) > 0 {
		return why, false
	}
	if err := st.tx(ctx, func(tx pgx.Tx) (err error) {
		if err := st.lockEnd(ctx, tx); err != nil {
			return err
		}
		why, done, err = st.ending(ctx, tx)
		return err
	}); err != nil {
		if ctx.Err() == nil {
			log.Warn("streams: checking the session's end failed (the next renewal checks again)", "err", err)
		}
		return why, false
	}
	return why, done
}

// revokesAtEnd: this stream is a connector's WebSocket on a link it
// redeemed (Link.Connector), whose end with nothing left to receive
// (4001) the connector takes as its link revoked, and forgets it (Codex's:
// codexconnector.ErrRevoked). Those links end with it (ending).
func (st *stream) revokesAtEnd() bool {
	_, ws := st.t.(wsTransport)
	return ws && st.l.Connector != "" && st.l.Target != ""
}

// lockEnd takes, first thing in a transaction that may end the stream
// (ending), the lock the session's redeems take (LockRedeems), for a
// stream whose links end with it (revokesAtEnd): before any row lock, as
// a redeem takes it before the leases and deliveries it changes, so
// neither waits on the other while holding what the other needs.
func (st *stream) lockEnd(ctx context.Context, tx pgx.Tx) error {
	if !st.revokesAtEnd() {
		return nil
	}
	return LockRedeems(ctx, tx, st.l.Target)
}

// ending is Server.Ending for an idle stream that's still allowed (valid),
// in tx, after lockEnd. On a stream whose links end with it (revokesAtEnd),
// once the session has nothing left (done), the session's links that a
// connector redeemed are revoked in the same transaction: the connector is
// told 4001 and forgets its link, so the core mustn't go on counting it as
// held (a redeem that shows none would be refused, conversation_taken,
// until it expired, and the thread's next subscriptions would wake
// nothing). It's what endListening does when a conversation unsubscribes
// from the last thing it watched: here the subscriptions ended another
// way (once, ttl, the app), and what they were owed is delivered.
//
// It holds the session's redeem lock (lockEnd), so a redeem racing it
// either finishes first (and its subscribe, made before it, keeps the
// session from being done) or sees every one of those links revoked.
// While anything is left to receive, nothing is revoked: a redeem
// without a link stays refused while the connector holds one.
func (st *stream) ending(ctx context.Context, tx pgx.Tx) (Ending, bool, error) {
	why, done, err := st.s.Ending(ctx, tx, st.l, true)
	if err != nil || !done || !st.revokesAtEnd() {
		return why, done, err
	}
	if _, err := tx.Exec(ctx, `UPDATE stream_links SET revoked_at = now() WHERE agent_id = $1 AND target = $2
		AND coalesce(connector, '') <> '' AND revoked_at IS NULL`, st.l.Agent, st.l.Target); err != nil {
		return why, done, err
	}
	// The session stops counting as connected at once, as with
	// RevokeSessionLinks, not when this connection's cleanup runs.
	if _, err := tx.Exec(ctx, `UPDATE stream_leases SET renewed_at = NULL WHERE agent_id = $1 AND target = $2`, st.l.Agent, st.l.Target); err != nil {
		return why, done, err
	}
	// Any other stream on one of them closes too (stillValid).
	_, err = tx.Exec(ctx, `SELECT pg_notify($1, current_tenant()::text || ':' || $2)`, RevokedChannel, st.l.Agent)
	return why, done, err
}

// announceTaken tells a between-turns connection (betweenTurns) which of
// the deliveries it holds were taken another way since (an app's wait
// tool, or the app showing the conversation those messages:
// deliveries.ClaimTurnHeld, deliveries.AckSeen), so it doesn't start a turn
// with them: {"type":"taken","delivery_id"} for each. They're no longer
// leased to it. It's told on the agent's NotifyChannel, and checks at each
// renewal too.
func (st *stream) announceTaken(ctx context.Context, log *slog.Logger) {
	if !st.turns() {
		return
	}
	st.heldMu.Lock()
	ids := make([]string, 0, len(st.held))
	for id := range st.held {
		ids = append(ids, id)
	}
	st.heldMu.Unlock()
	if len(ids) == 0 {
		return
	}
	var taken []string
	err := st.tx(ctx, func(tx pgx.Tx) error {
		rows, _ := tx.Query(ctx, `SELECT id::text FROM deliveries WHERE id = ANY($1::uuid[]) AND agent_id = $2 AND lease_holder IS DISTINCT FROM $3`,
			ids, st.l.Agent, st.holder)
		var err error
		taken, err = pgx.CollectRows(rows, pgx.RowTo[string])
		return err
	})
	if err != nil {
		if ctx.Err() == nil {
			log.Warn("streams: checking for taken deliveries failed", "err", err)
		}
		return
	}
	for _, id := range taken {
		if err := st.t.send(ctx, map[string]string{"type": "taken", "delivery_id": id}, ""); err != nil {
			return // the connection is going; what it holds goes back when it does
		}
		st.heldMu.Lock()
		delete(st.held, id)
		st.heldMu.Unlock()
	}
}

// confirm marks delivered what the connection has outlived: written at
// least a keepalive interval before the keepalive that just went through.
// Once nothing written is unconfirmed, it checks in the same transaction
// whether the session has anything left to receive (done, why: see
// renewal), so a stream whose last subscription ended (its notice the last
// thing written) ends at once rather than at the next renewal.
func (st *stream) confirm(ctx context.Context, log *slog.Logger) (why Ending, done bool) {
	if st.l.Ack || len(st.unconfirmed) == 0 {
		return why, false
	}
	cut := time.Now().Add(-st.s.pingEvery())
	var ids []string
	var keep []sent
	for _, w := range st.unconfirmed {
		if w.at.After(cut) {
			keep = append(keep, w)
		} else {
			ids = append(ids, w.id)
		}
	}
	if len(ids) == 0 {
		return why, false
	}
	check := st.s.Ending != nil && st.l.Target != "" && len(keep) == 0
	if err := st.tx(ctx, func(tx pgx.Tx) (err error) {
		if check {
			if err := st.lockEnd(ctx, tx); err != nil {
				return err
			}
		}
		if err := deliveries.Delivered(ctx, tx, st.holder, ids); err != nil {
			return err
		}
		if check {
			why, done, err = st.ending(ctx, tx)
		}
		return err
	}); err != nil {
		if ctx.Err() == nil {
			log.Warn("streams: confirming deliveries failed (next keepalive tries again)", "err", err)
		}
		return why, false
	}
	st.unconfirmed = keep
	if len(keep) == 0 {
		st.wrote = false
	}
	return why, done
}

// validLink checks, in the link's tenant, that the link still works (its
// expiry aside: the connection was made in time).
func (st *stream) validLink(ctx context.Context, tx pgx.Tx) error {
	_, err := loadTx(ctx, tx, st.l.ID, true)
	return err
}

// settled reports whether a stream that wrote its ?max= events can end:
// what it wrote is confirmed (without acks: outlived by a keepalive) or,
// on a link with acks, acknowledged (none of its leases is still in
// flight). Ending before the acks came would hand the deliveries back, so
// the command started again would get them again, an attempt each time.
// It waits for acks a lease (Server.Lease) at most: what's still
// unacknowledged then goes back, as on a disconnect.
func (st *stream) settled(ctx context.Context) bool {
	if !st.l.Ack {
		return len(st.unconfirmed) == 0
	}
	if time.Since(st.fullAt) >= st.s.lease() {
		return true
	}
	var held bool
	err := st.tx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM deliveries WHERE lease_holder = $1 AND state = 'in_flight')`, st.holder).Scan(&held)
	})
	return err == nil && !held
}

// capped ends a stream that wrote its ?max= events (once they're
// confirmed): a session's hands over its next link, in the same form.
func (st *stream) capped(ctx context.Context, log *slog.Logger) {
	n := strconv.Itoa(st.max) + " messages"
	if st.max == 1 {
		n = "1 message"
	}
	log.Info("streams: reached max; closing", "max", st.max)
	if st.l.Target != "" {
		st.rotate(ctx, log, "This command ended after "+n+", its limit: start it again the same way with this url. Nothing was missed.")
		return
	}
	st.t.end(endReconnect, Ending{"max", "This command ended after " + n + ", its limit: start it again the same way. Nothing was missed."})
}

// restart ends the connection for a server restart: a session's stream
// hands over its next link, since the one it connected with may have
// expired by now.
func (st *stream) restart(ctx context.Context, log *slog.Logger) {
	if st.l.Target == "" {
		st.t.end(endRestarting, endingRestarting)
		return
	}
	st.rotate(ctx, log, "The server is restarting: start the same watch again with this url. Nothing was missed.")
}

// rotate sends the session's next link, with a note saying why, and
// closes. It's only minted while this link (and the grant it was made
// under) still works.
func (st *stream) rotate(ctx context.Context, log *slog.Logger, note string) {
	if !st.stillValid(ctx, log) {
		return
	}
	next, exp, err := st.s.next(ctx, st.claims.Tenant, st.l)
	if errors.Is(err, errLinkInvalid) {
		st.t.end(endRevoked, st.s.endingOf(ctx, st.claims, true))
		return
	}
	if err != nil {
		log.Warn("streams: rotating the link failed", "err", err)
		st.t.end(endError, endingError)
		return
	}
	u, _ := url.Parse(next)
	st.t.send(ctx, rotateFrame{Type: "hub.rotate", URL: st.t.url(next), Link: u.Query().Get("link"), ExpiresAt: exp.UTC().Format(time.RFC3339), Note: note}, "")
	log.Info("streams: link rotated")
	st.t.end(endRotated, Ending{})
}

// next mints a session link's successor (same session, same position), only
// while the grant it was made under is still live.
func (s *Server) next(ctx context.Context, tenant string, l Link) (next string, exp time.Time, err error) {
	err = s.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		if l.GrantID != "" {
			var live bool
			if err := tx.QueryRow(ctx, `SELECT revoked_at IS NULL AND expires_at > now() FROM grants WHERE id = $1`, l.GrantID).Scan(&live); err != nil || !live {
				return errLinkInvalid
			}
		}
		n := NewLink{Agent: l.Agent, Target: l.Target, Ack: l.Ack, CreatedBy: l.CreatedBy, GrantID: l.GrantID, TTL: SessionLinkTTL, Connector: l.Connector}
		if !l.SlideUntil.IsZero() {
			n.Slide = SessionLinkMax
		}
		var err error
		next, exp, err = s.Mint(ctx, tx, tenant, n)
		return err
	})
	return next, exp, err
}

// stillValid re-checks the link (its expiry aside: the connection was made
// in time), closing the connection (4001) with why if it no longer works.
func (st *stream) stillValid(ctx context.Context, log *slog.Logger) bool {
	_, err := st.s.loadAs(ctx, st.claims, true)
	if err == nil {
		err = st.current()
	}
	switch {
	case st.endOutdated(err, log):
		return false
	case errors.Is(err, errLinkInvalid):
		why := st.s.endingOf(ctx, st.claims, true)
		log.Info("streams: link no longer valid; closing", "reason", why.Reason)
		st.t.end(endRevoked, why)
		return false
	case err != nil:
		log.Warn("streams: re-checking the link failed; keeping the connection", "err", err)
	}
	return true
}

var (
	errDisplaced = errors.New("stream taken by a newer connection")
	errDraining  = errors.New("server draining")
)

// takeLease makes this connection the stream's holder, returning the
// previous holder's deliveries to pending and telling it to close. The row
// is created first (if new) and then locked, so two connections arriving at
// once for a new stream still see each other. In the same transaction,
// first runs before (validity checks; a poll's acks: an error from it takes
// nothing), and then after, once the stream is held (a poll's first claim).
// A poll that yields to a live connection (ErrStreamConnected) takes
// nothing either, but what first did (its acks) commits.
//
// The session's and the lease's rows are locked before first runs: a
// poll's acks lock deliveries, which taking over a stream (a connection,
// the hooks' TakeOver, a renewal or release) locks after those two rows,
// so taking them in the same order here can't deadlock with it.
func (st *stream) takeLease(ctx context.Context, first, then func(context.Context, pgx.Tx) error) error {
	yielded := false
	err := st.tx(ctx, func(tx pgx.Tx) (err error) {
		held, err := st.lockLease(ctx, tx)
		if err != nil {
			return err
		}
		if first != nil {
			if err := first(ctx, tx); err != nil {
				return err
			}
		}
		if yielded, err = st.take(ctx, tx, held); err != nil || yielded {
			return err
		}
		if then != nil {
			return then(ctx, tx)
		}
		return nil
	})
	if err == nil && yielded {
		return ErrStreamConnected
	}
	return err
}

// heldLease is the stream's lease as lockLease found it.
type heldLease struct {
	holder string
	live   bool // renewed in the last minute (what counts as connected)
	until  time.Time
}

// lockLease, in takeLease's transaction, locks the session's row (if it's
// a session's stream), then the lease's, creating the lease if it's new.
// Connecting is activity, for a session. Locks go session row, then lease
// row, the order the paths that change both take (the hooks' redeem,
// unwatch's revocation), so none deadlocks with them.
func (st *stream) lockLease(ctx context.Context, tx pgx.Tx) (heldLease, error) {
	var h heldLease
	if uuidRE.MatchString(st.l.Target) {
		if _, err := tx.Exec(ctx, `UPDATE sessions SET last_active_at = now() WHERE id = $1::uuid`, st.l.Target); err != nil {
			return h, err
		}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO stream_leases (tenant_id, agent_id, target, holder)
		VALUES (current_tenant(), $1, $2, $3) ON CONFLICT DO NOTHING`, st.l.Agent, st.l.Target, st.holder); err != nil {
		return h, err
	}
	err := tx.QueryRow(ctx, `SELECT holder, coalesce(renewed_at > now() - interval '1 minute', false), coalesce(renewed_at + interval '1 minute', now())
		FROM stream_leases WHERE agent_id = $1 AND target = $2 FOR UPDATE`, st.l.Agent, st.l.Target).Scan(&h.holder, &h.live, &h.until)
	return h, err
}

// take is takeLease inside its transaction, with the lease locked (held);
// yielded reports that it took nothing, yielding to a live connection.
func (st *stream) take(ctx context.Context, tx pgx.Tx, held heldLease) (yielded bool, err error) {
	prev, live, until := held.holder, held.live, held.until
	if prev != st.holder && live && (st.yield && (isConnection(prev) || isWait(prev)) || st.yieldBackground && isWait(prev)) {
		st.yieldUntil = until
		return true, nil
	}
	// Connecting is presence.
	if prev == st.holder {
		_, err = tx.Exec(ctx, `UPDATE stream_leases SET renewed_at = now() WHERE agent_id = $1 AND target = $2`, st.l.Agent, st.l.Target)
		return false, err
	}
	st.prev = prev
	// A new holder isn't a wait loop's until that loop says so (wait.go,
	// loop mode): what the last loop printed is no longer its to confirm.
	if _, err := tx.Exec(ctx, `UPDATE stream_leases SET holder = $3, fence = fence + 1, taken_at = now(), renewed_at = now(), loop_id = NULL, loop_flushed = NULL
		WHERE agent_id = $1 AND target = $2`, st.l.Agent, st.l.Target, st.holder); err != nil {
		return false, err
	}
	// Taking the stream hands back what the last holder didn't confirm (a
	// wait's batch its client printed is confirmed by its cursor first:
	// wait.go).
	if err := deliveries.Release(ctx, tx, prev); err != nil {
		return false, err
	}
	_, err = tx.Exec(ctx, `SELECT pg_notify($1, $2)`, TakeoverChannel, prev)
	return false, err
}

// TakeOver, in the transaction's tenant, stops whatever holds a session's
// stream (agent, target) counting as live, and hands back what it hasn't
// confirmed: Wakeshift's hooks (or a connector or plugin) just connected
// the session, and they take over delivery whatever an older stream or
// wait seems to be doing (behind a front end, one whose client died can
// look live for minutes). What the old holder had goes to the new path.
// A connection is told to close; a wait that's really running comes back
// within its timeout and takes the stream again (the newest wins), and one
// a hook's poll takes the stream from is told to run again (wait.go).
func TakeOver(ctx context.Context, tx pgx.Tx, agent, target string) error {
	var holder string
	err := tx.QueryRow(ctx, `UPDATE stream_leases SET renewed_at = NULL WHERE agent_id = $1 AND target = $2 RETURNING holder`, agent, target).Scan(&holder)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := deliveries.Release(ctx, tx, holder); err != nil {
		return err
	}
	if isConnection(holder) {
		_, err = tx.Exec(ctx, `SELECT pg_notify($1, $2)`, TakeoverChannel, holder)
	}
	return err
}

// LiveOnWorkingLink reports, in the transaction's tenant, whether something
// holds a session's stream (agent, target) live, reading it on a link that
// still works: not revoked, not expired (a connection, which outlives its
// link, aside), and used as the holder took the stream. With background,
// only a background command counts (a wait, or a lines stream with max=).
// A holder whose link was revoked or expired is about to stop (its next
// round is refused), so a watch gives a command to start rather than
// saying one is running.
func LiveOnWorkingLink(ctx context.Context, tx pgx.Tx, agent, target string, background bool) (bool, error) {
	var live bool
	err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM stream_leases l
		WHERE l.agent_id = $1 AND l.target = $2 AND l.renewed_at > now() - interval '1 minute'
		AND (NOT $3 OR l.holder LIKE 'conn\_bg\_%' OR l.holder LIKE 'poll\_wait\_%')
		AND EXISTS (SELECT 1 FROM stream_links k WHERE k.agent_id = l.agent_id AND k.target = l.target AND k.revoked_at IS NULL
			AND (k.expires_at > now() OR l.holder LIKE 'conn\_%') AND k.used_at >= l.taken_at - interval '1 minute'))`,
		agent, target, background).Scan(&live)
	return live, err
}

// holding checks, inside tx, that this connection still holds the stream.
func (st *stream) holding(ctx context.Context, tx pgx.Tx) error {
	var holder string
	err := tx.QueryRow(ctx, `SELECT holder FROM stream_leases WHERE agent_id = $1 AND target = $2 FOR SHARE`,
		st.l.Agent, st.l.Target).Scan(&holder)
	if errors.Is(err, pgx.ErrNoRows) {
		// The lease is gone (with its workspace, when an app forgets its
		// user): the link is likely gone too, else the stream was given up.
		if err := st.validLink(ctx, tx); err != nil {
			return err
		}
		return errDisplaced
	}
	if err != nil {
		return err
	}
	if holder != st.holder {
		return errDisplaced
	}
	return nil
}

// renewal is what a renewal found.
type renewal struct {
	// pending: deliveries are waiting for the stream, which a notification
	// should have announced.
	pending bool
	// done: the session has nothing left to receive (its last subscription
	// ended), why says so, and the stream ends. Only checked after a write
	// (see stream.wrote; for links without acks, confirm checks first), and
	// while nothing written is unconfirmed: ending then would hand it back,
	// to a stream that's over.
	done bool
	why  Ending
}

// renew extends this connection's leases while it still holds the stream,
// and re-validates its link (errLinkInvalid if it no longer works: the
// creator left the workspace, or a revocation's notification was missed),
// in one transaction; see renewal for what else it checks.
func (st *stream) renew(ctx context.Context) (r renewal, err error) {
	st.renewals++
	check := st.s.Ending != nil && st.l.Target != "" && len(st.unconfirmed) == 0 && (st.wrote || st.renewals%endCheckEvery == 0)
	err = st.tx(ctx, func(tx pgx.Tx) error {
		if check {
			if err := st.lockEnd(ctx, tx); err != nil {
				return err
			}
		}
		// Renewing the lease is the check that this connection still holds
		// the stream; if it renews nothing, holding says why.
		tag, err := tx.Exec(ctx, `UPDATE stream_leases SET renewed_at = now() WHERE agent_id = $1 AND target = $2 AND holder = $3`,
			st.l.Agent, st.l.Target, st.holder)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			if err := st.holding(ctx, tx); err != nil {
				return err
			}
			return errDisplaced
		}
		if err := st.validLink(ctx, tx); err != nil {
			return err
		}
		if err := st.current(); err != nil {
			return err
		}
		if r.pending, err = deliveries.Pending(ctx, tx, st.l.Agent, st.l.Target); err != nil {
			return err
		}
		if err := deliveries.Renew(ctx, tx, st.holder, st.s.lease()); err != nil {
			return err
		}
		if check {
			r.why, r.done, err = st.ending(ctx, tx)
		}
		return err
	})
	if err == nil && check {
		st.wrote = false
	}
	return r, err
}

// interrupted reports, without blocking, whether the connection should stop
// writing: it was replaced, or the server is draining.
func (st *stream) interrupted() error {
	select {
	case <-st.replaced:
		return errDisplaced
	default:
	}
	if st.s.draining() {
		return errDraining
	}
	return nil
}

// pump writes everything the stream is owed, batch by batch. next is when
// the earliest paced delivery still waiting becomes available (zero: none).
func (st *stream) pump(ctx context.Context) (next time.Time, err error) {
	for {
		limit := pumpBatch
		if st.max > 0 {
			if limit = min(limit, st.max-st.count); limit <= 0 {
				st.full = true
				return time.Time{}, nil
			}
		}
		var batch []deliveries.Delivery
		var frames []Frame
		// An ack that commits after the claim is recorded after this (and
		// one that commits before, the claim doesn't take).
		st.ackMu.Lock()
		st.acked = nil
		st.ackMu.Unlock()
		err := st.tx(ctx, func(tx pgx.Tx) error {
			if err := st.holding(ctx, tx); err != nil {
				return err
			}
			if f := beforeClaim.Load(); f != nil {
				if err := (*f)(); err != nil {
					return err
				}
			}
			var err error
			if batch, err = deliveries.Claim(ctx, tx, st.l.Agent, st.l.Target, st.holder, st.s.lease(), limit); err != nil {
				return err
			}
			if err := deliveries.HandedTo(ctx, tx, batch, st.l.Connector); err != nil {
				return err
			}
			if len(batch) < limit {
				// That's all there is now: no transaction just to find none.
				// (What comes later is notified.)
				if next, err = deliveries.NextAvailable(ctx, tx, st.l.Agent, st.l.Target); err != nil {
					return err
				}
			}
			if len(batch) == 0 {
				return nil
			}
			frames, err = framesOf(ctx, tx, batch)
			return err
		})
		if err != nil {
			return next, pumpDBError{err}
		}
		if len(batch) == 0 {
			return next, nil
		}
		if f := afterClaim.Load(); f != nil {
			(*f)()
		}
		if st.turns() {
			// Held from the claim, before any is written: one taken in
			// between is announced after its frame, on the next check.
			st.heldMu.Lock()
			if st.held == nil {
				st.held = map[string]bool{}
			}
			for _, f := range frames {
				st.held[f.DeliveryID] = true
			}
			st.heldMu.Unlock()
		}
		written := make([]string, 0, len(batch))
		for i, f := range frames {
			err := st.interrupted()
			sent := false
			if err == nil {
				sent, err = st.sendUnlessAcked(ctx, f)
			}
			if err != nil {
				st.settle(ctx, written, batch[i:])
				return time.Time{}, err
			}
			if !sent {
				continue // acknowledged already: nothing to settle
			}
			written = append(written, f.DeliveryID)
			st.wrote = true
			st.count++
		}
		st.settle(ctx, written, nil)
		if st.max > 0 && st.count >= st.max {
			st.full, st.fullAt = true, time.Now()
			return time.Time{}, nil
		}
		if len(batch) < limit {
			return next, nil
		}
	}
}

// afterClaim, if set, runs right after a stream claims a batch, before it
// writes any (tests: export_test.go).
var afterClaim atomic.Pointer[func()]

// beforeClaim, if set, runs in a stream's claim transaction before it
// claims; an error from it fails the transaction, as the database failing
// would (tests: export_test.go).
var beforeClaim atomic.Pointer[func() error]

// pumpDBError is pump's claim transaction failing: the database, not the
// connection. The stream retries it (pumpRetries) rather than ending.
type pumpDBError struct{ err error }

func (e pumpDBError) Error() string { return e.err.Error() }
func (e pumpDBError) Unwrap() error { return e.err }

// pumpRetries is how many times in a row a stream retries a failed claim
// transaction, after pumpRetryWait doubling each time (with jitter: about
// 31 s in all), before it ends as it did before. A database failover or a
// pool briefly full then costs a delay, not every stream on the instance
// ending and reconnecting into the same window (~3 transactions each).
// A variable, so tests can shorten the wait.
const pumpRetries = 5

var pumpRetryWait = time.Second

// sendUnlessAcked writes a claimed frame, unless the client acknowledged
// it since the claim (see stream.acked): it had it already, and may have
// its receipt too. sent reports whether it wrote it.
func (st *stream) sendUnlessAcked(ctx context.Context, f Frame) (sent bool, err error) {
	st.ackMu.Lock()
	defer st.ackMu.Unlock()
	if st.acked[f.DeliveryID] {
		return false, nil
	}
	return true, st.t.send(ctx, f, f.DeliveryID)
}

// pumpBatch is how many deliveries a stream claims at once.
const pumpBatch = 20

// settle records how a batch ended, even if the connection's context has:
// what was written waits for its ack, or without acks for a keepalive to
// confirm it (confirm); what was never written goes back without counting
// the attempt.
func (st *stream) settle(ctx context.Context, written []string, unwritten []deliveries.Delivery) {
	if !st.l.Ack {
		now := time.Now()
		for _, id := range written {
			st.unconfirmed = append(st.unconfirmed, sent{id, now})
		}
	}
	if len(unwritten) == 0 {
		return
	}
	fctx, cancel := finishing(ctx)
	defer cancel()
	ids := make([]string, len(unwritten))
	for i, d := range unwritten {
		ids[i] = d.ID
	}
	if err := st.tx(fctx, func(tx pgx.Tx) error { return deliveries.Unclaim(fctx, tx, st.holder, ids) }); err != nil {
		slog.Warn("streams: recording a batch failed (leases will expire)", "holder", st.holder, "err", err)
	}
}

// Frame is what a consumer sees: the event, and what it needs to
// acknowledge it or recognize a redelivery. A poll returns the same.
type Frame struct {
	Type           string `json:"type"`
	DeliveryID     string `json:"delivery_id"`
	Attempt        int    `json:"attempt"` // more than 1: it may have been seen before
	SubscriptionID string `json:"subscription_id"`
	// Topic is the topic (or topics, space-separated) the event was
	// published to, if it's a topic event: so an agent watching several can
	// tell which fired.
	Topic string `json:"topic,omitempty"`
	// App is the display name of the app the event came through, if any
	// (framing.Name: one plain line).
	App   string       `json:"app,omitempty"`
	Event events.Event `json:"event"`
	// On the agent's connector stream: which session (and harness
	// conversation) the delivery is for.
	DeliverTo *deliveries.Target `json:"deliver_to,omitempty"`
	// The agent's other sessions the same event woke.
	AlsoWoken []string `json:"also_woken,omitempty"`
}

// FrameOf is a delivery as a consumer sees it (without the app's name:
// framesOf adds it).
func FrameOf(d deliveries.Delivery) Frame {
	return Frame{Type: "event", DeliveryID: d.ID, Attempt: d.Attempts, SubscriptionID: d.SubscriptionID, Topic: d.Event.Attrs["topic"],
		Event: d.Event, DeliverTo: d.DeliverTo, AlsoWoken: d.AlsoWoken}
}

// framesOf is deliveries as a consumer sees them, with the display names
// of the apps they came through.
func framesOf(ctx context.Context, tx pgx.Tx, ds []deliveries.Delivery) ([]Frame, error) {
	out := make([]Frame, len(ds))
	var ids []string
	for i, d := range ds {
		out[i] = FrameOf(d)
		if id := d.Event.Attrs["app"]; id != "" && !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := tx.Query(ctx, `SELECT id, name FROM apps WHERE id = ANY($1)`, ids)
	if err != nil {
		return nil, err
	}
	names := map[string]string{}
	for rows.Next() {
		var id, name string
		if err := rows.Scan(&id, &name); err != nil {
			rows.Close()
			return nil, err
		}
		names[id] = framing.Name(name)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		out[i].App = names[out[i].Event.Attrs["app"]]
	}
	return out, nil
}

// resendWithin bounds what Last-Event-ID gets again: what was written in
// the last few minutes.
const resendWithin = deliveries.RedeliverWindow

// readAcks handles messages from the client (acks), and notices when it
// goes away.
func (st *stream) readAcks(ctx context.Context, cancel context.CancelFunc, c *websocket.Conn) {
	defer cancel()
	window, count := time.Now(), 0
	for {
		_, b, err := c.Read(ctx)
		if err != nil {
			return
		}
		// Every frame counts, acks or not: reading and parsing one is work
		// too.
		if time.Since(window) > time.Second {
			window, count = time.Now(), 0
		}
		if count++; count > maxAcksPerSecond {
			st.t.end(endTooManyAcks, endingTooMany)
			return
		}
		var m struct {
			Type       string `json:"type"`
			DeliveryID string `json:"delivery_id"`
		}
		if json.Unmarshal(b, &m) != nil || m.Type != "ack" || m.DeliveryID == "" {
			continue
		}
		var acked bool
		err = st.tx(ctx, func(tx pgx.Tx) (err error) {
			acked, err = deliveries.Ack(ctx, tx, st.l.Agent, st.l.Target, m.DeliveryID)
			return err
		})
		if err != nil {
			if ctx.Err() == nil {
				slog.Warn("streams: ack failed", "delivery", m.DeliveryID, "err", err)
			}
			continue
		}
		st.heldMu.Lock()
		delete(st.held, m.DeliveryID)
		st.heldMu.Unlock()
		if acked {
			// The receipt: the client can stop expecting a redelivery. A
			// frame claimed before the ack isn't written after it (pump).
			b, _ := json.Marshal(map[string]string{"type": "acked", "delivery_id": m.DeliveryID})
			st.ackMu.Lock()
			if st.acked == nil {
				st.acked = map[string]bool{}
			}
			st.acked[m.DeliveryID] = true
			wctx, wcancel := context.WithTimeout(ctx, 10*time.Second)
			c.Write(wctx, websocket.MessageText, b)
			wcancel()
			st.ackMu.Unlock()
			select {
			case st.ackedCh <- struct{}{}:
			default: // a check is due already
			}
		}
	}
}

func (s *Server) lease() time.Duration         { return or(s.Lease, 60*time.Second) }
func (s *Server) waitLease() time.Duration     { return or(s.WaitLease, DefaultWaitLease) }
func (s *Server) waitKeepalive() time.Duration { return or(s.WaitKeepalive, 30*time.Second) }
func (s *Server) waitGrace() time.Duration     { return or(s.WaitGrace, DefaultWaitGrace) }
func (s *Server) maxAge() time.Duration        { return or(s.MaxAge, 50*time.Minute) }
func (s *Server) oneWayMaxAge() time.Duration  { return or(s.OneWayMaxAge, DefaultOneWayMaxAge) }
func (s *Server) pingEvery() time.Duration     { return or(s.PingEvery, 15*time.Second) }
func (s *Server) poll() time.Duration          { return or(s.Poll, 30*time.Second) }
func (s *Server) rotateLead() time.Duration    { return or(s.RotateLead, rotateLead) }
func (s *Server) renew() time.Duration         { return or(s.Renew, 30*time.Second) }

func or(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}

// FramesOf is framesOf for consumers outside this package that push
// deliveries rather than hold a stream (agent-hosted webhooks:
// internal/agenthooks): the deliveries as a stream's consumer sees them,
// with their apps' display names.
func FramesOf(ctx context.Context, tx pgx.Tx, ds []deliveries.Delivery) ([]Frame, error) {
	return framesOf(ctx, tx, ds)
}

// UsableLink is the stored link with this ID in the transaction's tenant
// if it still works (not revoked or expired, its grant live, its creator
// still a member), else an error: for a capability derived from a session's
// link (an agent-hosted webhook's registration link), which works only
// while the link does.
func UsableLink(ctx context.Context, tx pgx.Tx, id string) (Link, error) {
	return loadTx(ctx, tx, id, false)
}

// KeepLink moves a sliding link's expiry SessionLinkTTL ahead (up to its
// slide_until), as a use does: a capability derived from it was just
// handed out, so it works for at least that long.
func KeepLink(ctx context.Context, tx pgx.Tx, id string) error {
	_, err := tx.Exec(ctx, `UPDATE stream_links SET expires_at = greatest(expires_at, least(slide_until, now() + $2::interval))
		WHERE id = $1 AND slide_until IS NOT NULL AND revoked_at IS NULL`, id, SessionLinkTTL.String())
	return err
}
