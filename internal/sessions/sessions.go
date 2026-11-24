// Package sessions keeps an agent's sessions: its conversations, each a
// participant with a handle, its own deliveries, presence and authorship.
//
// Handles are agent#n (numbered in order) or agent@label. A mention can name
// the agent ("@codex": its most recently active connected session gets it)
// or one session ("@codex#2"). A session's own posts never wake it.
package sessions

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Runtimes.
const (
	Stream    = "stream"    // holds its own stream link
	Connector = "connector" // delivered on the agent's connector stream
)

// Session is one conversation of an agent. Connected: its stream (for a
// connector session, the agent's connector stream) renewed in the last
// minute (streams renew with every ping, every 20s; polls while they wait
// and as they answer).
type Session struct {
	ID           string    `json:"id"`
	Agent        string    `json:"agent"`
	Handle       string    `json:"handle"`
	Label        string    `json:"label,omitempty"`
	Runtime      string    `json:"runtime"`
	Conversation string    `json:"conversation,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	LastActiveAt time.Time `json:"last_active_at"`
	Connected    bool      `json:"connected"`
	// ConnectedVia is how: "stream" (a stream held open: a WebSocket, a
	// connector, a background command's lines) or "poll" (long polls, waits
	// such as Claude Code's Monitor, or a wait tool, within the last minute),
	// or "webhook" (an MCP Events subscription that hasn't expired:
	// ChatGPT's, internal/mcpevents; or a live agent-hosted webhook
	// destination: Capy's watch, internal/agenthooks).
	ConnectedVia string `json:"connected_via,omitempty"`
	// Watching: the conversation asked to be woken (it subscribed) and
	// hasn't stopped (an unsubscribe of its last subscription, or unwatch).
	Watching bool `json:"watching"`
	// ConnectorSeenAt: when its connector last redeemed a ticket (so it's
	// installed and trusted where the conversation runs).
	ConnectorSeenAt *time.Time `json:"connector_seen_at,omitempty"`
	// Connector, for a connector session: what delivers to it, and, while
	// it isn't running, how to install it (set by the app API).
	Connector *ConnectorStatus `json:"connector,omitempty"`
	// ConnectorClient is the latest Wakeshift connector seen for it
	// (connectorversion.Client.String(): "wakeshift/2026.10.20.1435+91faaf7
	// codex"), "" if none; ConnectorNoted, the one a tool result last said
	// was out of date.
	ConnectorClient string `json:"-"`
	ConnectorNoted  string `json:"-"`
	// WaitCommand is Wakeshift's wait command on the machine whose hooks
	// connected it (`/Users/<name>/.wakeshift/bin/wakeshift wait`), as the
	// connector said when it redeemed: what a harness woken by a command
	// in the background is told to run (harnesses.WaitNote). "" if none.
	WaitCommand string `json:"-"`
	// CreatedBy and AppIDs: who started it ("app:<id>") and the apps it
	// belongs to (Joined).
	CreatedBy string   `json:"-"`
	AppIDs    []string `json:"-"`
}

// Joined reports whether the session is one of app's: the app started it,
// or registered it by its harness conversation's ID. An app acts only on
// its own sessions; another app's handle or label names nothing it may use.
func (s Session) Joined(app string) bool {
	return s.CreatedBy == "app:"+app || slices.Contains(s.AppIDs, app)
}

// appOf is the app a New's CreatedBy names ("" for none).
func appOf(createdBy string) string {
	app, _ := strings.CutPrefix(createdBy, "app:")
	if app == createdBy {
		return ""
	}
	return app
}

// ConnectorStatus is the local app a connector session is delivered
// through, and whether it's running.
type ConnectorStatus struct {
	Name    string `json:"name"`
	Running bool   `json:"running"`
	Install any    `json:"install,omitempty"` // downloads.Install while it isn't running
	Note    string `json:"note,omitempty"`    // for the agent: what to tell its user
}

// New is what Register needs.
type New struct {
	Agent        string
	Resume       string // a handle the agent already holds: reuse it
	Label        string // names a new session agent@label (reused if it exists)
	Runtime      string // default Stream
	Conversation string // the harness's conversation ID: reuse its session
	// CreatedBy is who's registering: "app:<id>" for an app, which may
	// resume or reuse (by label) only its own sessions (Joined), and joins
	// the session for a conversation it names.
	CreatedBy string
}

var (
	ErrNotFound = errors.New("sessions: no such session")
	// ErrLabelTaken: the label names another app's session.
	ErrLabelTaken = errors.New("that session label is taken by another app's session; choose another label")
	labelRE       = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,29}$`)
)

const columns = `s.id::text, s.agent_id, s.handle, s.label, s.runtime, coalesce(s.conversation, ''), s.created_at, s.last_active_at, s.watching, s.connector_seen_at,
	coalesce(s.connector, ''), coalesce(s.connector_noted, ''), coalesce(s.wait_command, ''), s.created_by, s.app_ids,
	coalesce((SELECT CASE WHEN l.renewed_at > now() - interval '1 minute' THEN CASE WHEN l.holder LIKE 'poll\_%' THEN 'poll' ELSE 'stream' END END
		FROM stream_leases l WHERE l.tenant_id = s.tenant_id AND l.agent_id = s.agent_id
		AND l.target = CASE s.runtime WHEN 'connector' THEN '' ELSE s.id::text END),
		(SELECT 'webhook' FROM mcp_event_webhooks w WHERE w.tenant_id = s.tenant_id AND w.session_id = s.id AND w.expires_at > now() AND w.ending_reason IS NULL LIMIT 1),
		(SELECT 'webhook' FROM agent_webhooks h WHERE h.tenant_id = s.tenant_id AND h.session_id = s.id AND h.state = 'live' AND h.needs_setup_at IS NULL), '')`

func scan(row pgx.Row) (Session, error) {
	var s Session
	err := row.Scan(&s.ID, &s.Agent, &s.Handle, &s.Label, &s.Runtime, &s.Conversation, &s.CreatedAt, &s.LastActiveAt, &s.Watching, &s.ConnectorSeenAt, &s.ConnectorClient, &s.ConnectorNoted, &s.WaitCommand,
		&s.CreatedBy, &s.AppIDs, &s.ConnectedVia)
	s.Connected = s.ConnectedVia != ""
	if errors.Is(err, pgx.ErrNoRows) {
		return s, ErrNotFound
	}
	return s, err
}

// Register returns the session to use: the one resumed, the one for the
// conversation or label, or a new one (agent#n). It counts as activity.
//
// For an app (CreatedBy "app:<id>"): a handle it resumes must be one of its
// sessions (ErrNotFound if not), as must a label's (ErrLabelTaken); the
// session for a conversation it names becomes one of its (the harness
// told it the conversation's ID, so it's in that conversation).
func Register(ctx context.Context, tx pgx.Tx, n New) (Session, error) {
	app := appOf(n.CreatedBy)
	if n.Runtime == "" {
		n.Runtime = Stream
	}
	if n.Runtime != Stream && n.Runtime != Connector {
		return Session{}, errors.New("sessions: unknown runtime")
	}
	if n.Label != "" && !labelRE.MatchString(n.Label) {
		return Session{}, errors.New("session labels are 1-30 lowercase letters, digits and dashes")
	}
	if len(n.Conversation) > 200 {
		return Session{}, errors.New("conversation IDs are at most 200 characters")
	}
	var existing Session
	var err error
	switch {
	case n.Resume != "":
		existing, err = ByHandle(ctx, tx, strings.TrimPrefix(n.Resume, "@"))
		if err == nil && (existing.Agent != n.Agent || app != "" && !existing.Joined(app)) {
			err = ErrNotFound
		}
		if err != nil {
			return Session{}, err
		}
	case n.Conversation != "":
		existing, err = scan(tx.QueryRow(ctx, `SELECT `+columns+` FROM sessions s WHERE s.agent_id = $1 AND s.conversation = $2`, n.Agent, n.Conversation))
	case n.Label != "":
		existing, err = ByHandle(ctx, tx, n.Agent+"@"+n.Label)
	default:
		err = ErrNotFound
	}
	if err == nil {
		return join(ctx, tx, existing, app, n.Conversation != "")
	}
	if !errors.Is(err, ErrNotFound) {
		return Session{}, err
	}
	// New: numbered under a lock per agent, so concurrent registrations
	// don't pick the same number.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('sessions:' || current_tenant()::text || ':' || $1))`, n.Agent); err != nil {
		return Session{}, err
	}
	// A concurrent registration for the same conversation may have made it
	// while this one waited for the lock.
	if n.Conversation != "" {
		existing, err = scan(tx.QueryRow(ctx, `SELECT `+columns+` FROM sessions s WHERE s.agent_id = $1 AND s.conversation = $2`, n.Agent, n.Conversation))
		if err == nil {
			return join(ctx, tx, existing, app, true)
		}
		if !errors.Is(err, ErrNotFound) {
			return Session{}, err
		}
	}
	handle := n.Agent + "@" + n.Label
	if n.Label == "" {
		var count int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM sessions WHERE agent_id = $1 AND label = ''`, n.Agent).Scan(&count); err != nil {
			return Session{}, err
		}
		handle = fmt.Sprintf("%s#%d", n.Agent, count+1)
	}
	var conv *string
	if n.Conversation != "" {
		conv = &n.Conversation
	}
	apps := []string{}
	if app != "" {
		apps = append(apps, app)
	}
	var id string
	err = tx.QueryRow(ctx, `INSERT INTO sessions (tenant_id, agent_id, handle, label, runtime, conversation, created_by, app_ids)
		VALUES (current_tenant(), $1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (tenant_id, handle) DO UPDATE SET last_active_at = now() RETURNING id::text`,
		n.Agent, handle, n.Label, n.Runtime, conv, n.CreatedBy, apps).Scan(&id)
	if err != nil {
		return Session{}, err
	}
	s, err := Get(ctx, tx, id)
	if err == nil && app != "" && !s.Joined(app) {
		return Session{}, ErrLabelTaken // made meanwhile, by another app
	}
	return s, err
}

// join returns an existing session to app: one of its own; for a
// conversation it named, after making it one of its own; else (a label
// another app's session holds) ErrLabelTaken. It counts as activity.
func join(ctx context.Context, tx pgx.Tx, s Session, app string, conversation bool) (Session, error) {
	switch {
	case app == "" || s.Joined(app):
	case conversation:
		if _, err := tx.Exec(ctx, `UPDATE sessions SET app_ids = array_append(app_ids, $2) WHERE id = $1 AND NOT ($2 = ANY(app_ids))`, s.ID, app); err != nil {
			return Session{}, err
		}
		s.AppIDs = append(s.AppIDs, app)
	default:
		return Session{}, ErrLabelTaken
	}
	return s, Touch(ctx, tx, s.ID)
}

// Get returns a session by ID.
func Get(ctx context.Context, tx pgx.Tx, id string) (Session, error) {
	return scan(tx.QueryRow(ctx, `SELECT `+columns+` FROM sessions s WHERE s.id = $1`, id))
}

// ByHandle returns a session by handle (codex#2, claude-code@pipe).
func ByHandle(ctx context.Context, tx pgx.Tx, handle string) (Session, error) {
	return scan(tx.QueryRow(ctx, `SELECT `+columns+` FROM sessions s WHERE s.handle = $1`, handle))
}

// ByConversation returns an agent's session for a harness conversation (a
// Codex thread), without registering one.
func ByConversation(ctx context.Context, tx pgx.Tx, agent, conversation string) (Session, error) {
	return scan(tx.QueryRow(ctx, `SELECT `+columns+` FROM sessions s WHERE s.agent_id = $1 AND s.conversation = $2`, agent, conversation))
}

// List returns an agent's sessions, most recently active first.
func List(ctx context.Context, tx pgx.Tx, agent string) ([]Session, error) {
	return list(ctx, tx, `SELECT `+columns+` FROM sessions s WHERE s.agent_id = $1 ORDER BY s.last_active_at DESC LIMIT 100`, agent)
}

// MaxConnected is the most sessions ConnectedNow returns.
const MaxConnected = 10000

// ConnectedNow returns the workspace's sessions, of every agent, that are
// connected now (as Connected says), most recently active first: all of
// them, up to MaxConnected.
func ConnectedNow(ctx context.Context, tx pgx.Tx) ([]Session, error) {
	return list(ctx, tx, `SELECT `+columns+` FROM sessions s WHERE EXISTS (SELECT 1 FROM stream_leases l
		WHERE l.tenant_id = s.tenant_id AND l.agent_id = s.agent_id AND l.target = CASE s.runtime WHEN 'connector' THEN '' ELSE s.id::text END
		AND l.renewed_at > now() - interval '1 minute')
		OR EXISTS (SELECT 1 FROM mcp_event_webhooks w WHERE w.tenant_id = s.tenant_id AND w.session_id = s.id AND w.expires_at > now() AND w.ending_reason IS NULL)
		OR EXISTS (SELECT 1 FROM agent_webhooks h WHERE h.tenant_id = s.tenant_id AND h.session_id = s.id AND h.state = 'live' AND h.needs_setup_at IS NULL)
		ORDER BY s.last_active_at DESC LIMIT $1`, MaxConnected)
}

func list(ctx context.Context, tx pgx.Tx, sql string, args ...any) ([]Session, error) {
	rows, err := tx.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	out := []Session{}
	for rows.Next() {
		s, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// Touch records activity (connecting, posting, joining).
func Touch(ctx context.Context, tx pgx.Tx, id string) error {
	_, err := tx.Exec(ctx, `UPDATE sessions SET last_active_at = now() WHERE id = $1`, id)
	return err
}

// IsHandle reports whether a mention names a session (agent#n or
// agent@label) rather than an agent, and of which agent.
func IsHandle(mention string) (agent string, ok bool) {
	if i := strings.IndexAny(mention, "#@"); i > 0 {
		return mention[:i], true
	}
	return "", false
}

// Pick is the session an event for agent goes to when it names the agent,
// not a session: the most recently active connected session, else follow
// (the session that joined), else the most recently active session; except
// never the sender's own session. "" means the agent as a whole.
func Pick(ctx context.Context, tx pgx.Tx, agent, follow, except string) (Session, error) {
	list, err := List(ctx, tx, agent)
	if err != nil {
		return Session{}, err
	}
	var first *Session
	for i := range list {
		s := &list[i]
		if s.ID == except {
			continue
		}
		if s.Connected {
			return *s, nil
		}
		if first == nil {
			first = s
		}
	}
	if follow != "" && follow != except {
		if s, err := Get(ctx, tx, follow); err == nil {
			return s, nil
		}
	}
	if first != nil {
		return *first, nil
	}
	return Session{}, ErrNotFound
}

// SetWatching records whether the session's conversation wants to be woken
// (it subscribed; it unsubscribed from everything, or unwatched).
func SetWatching(ctx context.Context, tx pgx.Tx, id string, watching bool) error {
	_, err := tx.Exec(ctx, `UPDATE sessions SET watching = $2 WHERE id = $1`, id, watching)
	return err
}
