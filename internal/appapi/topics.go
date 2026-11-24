package appapi

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/agents"
	"github.com/wakeshift/server/internal/apps"
	"github.com/wakeshift/server/internal/deliveries"
	"github.com/wakeshift/server/internal/events"
	"github.com/wakeshift/server/internal/framing"
	"github.com/wakeshift/server/internal/grants"
	"github.com/wakeshift/server/internal/harnesses"
	"github.com/wakeshift/server/internal/httpx"
	"github.com/wakeshift/server/internal/orgs"
	"github.com/wakeshift/server/internal/pipeline"
	"github.com/wakeshift/server/internal/pluginmark"
	"github.com/wakeshift/server/internal/sessions"
	"github.com/wakeshift/server/internal/subscriptions"
)

// Topics: how the core routes an app's events (eventsubs.go). An event's
// arguments pick a topic; a conversation that calls one of the app's tools
// is subscribed to it, and an emit publishes to it. The app never sees
// topics, workspaces, agents, sessions or harnesses. Behind the scenes:
//
//   - the caller is a session in a workspace the app owns:
//     one per app user, or, without a user, one per conversation; its agent
//     is named after the harness, with the app's grant made at once;
//   - a topic subscription is an ordinary subscription there, to the app's
//     event type topics.<app>, filtered to the topic (an attribute
//     "$topic:<topic>") and to the data fields the app asked for, so the
//     pipeline, pacing, deliveries and streams work unchanged;
//   - a publish is copied into the log of each workspace with a subscriber
//     (in one transaction per workspace, in workspace order), naming
//     only the topics that workspace subscribes to.
//
// The type is outside the app's namespace (app.<app>.), so no manifest
// scope reaches it: agents can't subscribe or post to it through the agent
// API, and an app can't publish into it with source events.

const (
	maxTopicText    = 1000 // bytes of an occurrence's text: longer is trimmed (trimText)
	maxTopicData    = 64 << 10
	maxFilterFields = 20
	maxReplay       = 24 * time.Hour
	defaultTopicTTL = 30 * 24 * time.Hour
	maxTopicTTL     = 365 * 24 * time.Hour
	minTopicTTL     = time.Minute
	// A publish URL takes at most this many posts a minute.
	publishURLPerMinute = 60
)

var topicRE = regexp.MustCompile(`^[a-z0-9._-]+(/[a-z0-9._-]+)*$`)

// checkTopic says what's wrong with a topic, if anything.
func checkTopic(t string) error {
	switch {
	case t == "":
		return badRequest{errors.New("topic is required")}
	case len(t) > 200:
		return badRequest{errors.New("a topic is at most 200 characters")}
	case t == "u":
		return badRequest{errors.New(`"u" alone isn't a topic: a private topic is u/<user> or u/<user>/…`)}
	case topicRE.MatchString(t):
		return nil
	case strings.HasPrefix(t, "/") || strings.HasSuffix(t, "/") || strings.Contains(t, "//"):
		return badRequest{fmt.Errorf("topic %q has an empty segment: segments are joined by single '/'", t)}
	}
	return badRequest{fmt.Errorf("topic %q: topic segments must be lowercase a-z 0-9 . _ -", t)}
}

// privateTo is the user a u/<user>/… topic is private to.
func privateTo(topic string) (string, bool) {
	rest, ok := strings.CutPrefix(topic, "u/")
	if !ok {
		return "", false
	}
	user, _, _ := strings.Cut(rest, "/")
	return user, true
}

// topicsType is the event type of an app's topic events in each workspace.
func topicsType(app string) string { return "topics." + app }

// topicAttr is the attribute naming a topic on an event, and the filter
// that subscribes to it.
func topicAttr(topic string) string { return "$topic:" + topic }

// duration is a JSON duration: seconds, or a string like "90s", "5m", "2h"
// or "30d".
type duration time.Duration

// maxDuration bounds a JSON duration, so converting one can't overflow (and
// wrap into a valid-looking one); the caller's own limits are far smaller.
const maxDuration = 100 * 365 * 24 * time.Hour

func (d *duration) UnmarshalJSON(b []byte) error {
	var secs float64
	if json.Unmarshal(b, &secs) == nil {
		if !(secs >= -maxDuration.Seconds() && secs <= maxDuration.Seconds()) { // NaN too
			return errors.New("duration out of range")
		}
		*d = duration(time.Duration(secs * float64(time.Second)))
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return errors.New("a duration is seconds or a string like \"5m\"")
	}
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil {
			return fmt.Errorf("bad duration %q", s)
		}
		if n < 0 || time.Duration(n) > maxDuration/(24*time.Hour) {
			return fmt.Errorf("duration %q out of range", s)
		}
		*d = duration(time.Duration(n) * 24 * time.Hour)
		return nil
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("bad duration %q", s)
	}
	*d = duration(v)
	return nil
}

// ── The caller ───────────────────────────────────────────────────────────

// callIn is what an app's SDK packs from the tool call (ws.call).
type callIn struct {
	User      string         `json:"user"`       // the app's own ID for the person, if it has one
	Client    string         `json:"client"`     // the MCP client's name (initialize's clientInfo.name; a stateless server has none)
	Meta      map[string]any `json:"meta"`       // the tool call's _meta (Codex sends its thread there)
	Session   string         `json:"session"`    // the session handle a previous result gave the agent
	UserAgent string         `json:"user_agent"` // the MCP request's HTTP User-Agent, if the app has it
	// ClientVersion is the MCP client's version (initialize's
	// clientInfo.version), if the app has it: with the User-Agent, which
	// commands an offer gives (harnesses.WithClient). A hint, as the
	// client name is.
	ClientVersion string `json:"client_version"`
	// Conversation is the harness's conversation ID as the agent passed it
	// (the tool's conversation_id argument), for a harness that tells only
	// the agent (harnesses.Harness.ConversationEnv: Cursor's
	// CURSOR_CONVERSATION_ID). Others' is ignored, as is one that isn't an
	// ID (an unexpanded "$CURSOR_CONVERSATION_ID").
	Conversation string `json:"conversation"`
	// Harness is the program the agent says it runs in (the tool's harness
	// argument): a hint that refines what the call was recognized as, never
	// a credential (harnesses.Refine).
	Harness string `json:"harness"`
	// Plugin is the mark Wakeshift's plugin for Claude Code put in the
	// harness argument (internal/pluginmark), taken out of Harness as the
	// call is read (UnmarshalJSON); nil if there's none. A hint, as the
	// claim is: it only leaves the install offer and the Monitor out of the
	// result (pluginMarked).
	Plugin *pluginmark.Mark `json:"-"`
	// AgentToken is the app's token for one of its agents (wakeshift ActAs):
	// it names the subscriber (eventsubs_agent.go). A credential: never
	// logged.
	AgentToken string `json:"agent_token"`
	// Tool is the MCP tool the call is to (its name as the app's server
	// lists it): a beacon in its result names it (beacon.Claims.Tool), and
	// the hooks act on the beacon only from that tool's result. The SDKs
	// fill it in; one that isn't a tool name is ignored (beaconTool).
	Tool string `json:"tool"`
	// ClientRequests are the kinds of client request (clientrequests.go)
	// the app's SDK can make of this call's MCP client now: it supports the
	// kind, the client declared the capability, and the transport carries
	// a server-to-client request during the call. A hint the core never
	// trusts for anything but whether to describe a request.
	ClientRequests []string `json:"client_requests"`
	// Tasks: the call's client takes MCP tasks (its request is protocol
	// 2026-07-28 with the tasks extension in its capabilities) and the
	// app's SDK serves Wakeshift's wait tool (harnesses.WaitTool) for it, so
	// a harness with harnesses.Harness.Tasks is told to wait with that tool
	// rather than its zero-install path (tasks.go). An SDK sets it only
	// when both hold; it's a hint that picks instructions, never a
	// credential.
	Tasks bool `json:"tasks"`
	// TransportSession is the SHA-256 (hex) of the MCP request's
	// Mcp-Session-Id header, when it had one: what an anonymous subscribe
	// voucher is bound to (vouchers.go). It's not a credential by itself.
	TransportSession string `json:"transport_session"`
	// EventsExt: the calling request declared the MCP Events extension (its
	// _meta capabilities, or its session's initialize capabilities). With
	// push served, an opencode caller the plugin doesn't listen for gets a
	// native push line (nativePush). A hint, never a credential.
	EventsExt bool `json:"events_ext"`
}

// UnmarshalJSON reads a call as the SDK packs it, taking the plugin's mark
// (pluginmark.Split) out of the harness argument, so everything else reads
// the claim alone.
func (c *callIn) UnmarshalJSON(b []byte) error {
	type plain callIn
	var p plain
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	*c = callIn(p)
	if claim, m, ok := pluginmark.Split(c.Harness); ok {
		c.Harness, c.Plugin = claim, &m
	}
	return nil
}

// toolNameRE is what an MCP tool name looks like (the spec's characters,
// and / and : some servers use).
var toolNameRE = regexp.MustCompile(`^[A-Za-z0-9_.:/-]{1,128}$`)

// beaconTool is the tool a beacon may name: tool if it's a tool name, else
// "" (an app on an SDK from before Tool, or one that sent something else:
// its beacons name no tool, which hooks accept only from a result's own
// _meta).
func beaconTool(tool string) string {
	if toolNameRE.MatchString(tool) {
		return tool
	}
	return ""
}

// conversationArgRE is what a conversation ID an agent passes looks like.
var conversationArgRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,199}$`)

// argConversation is the conversation an agent passed for its harness,
// if the harness takes one that way.
func argConversation(harness string, call callIn) string {
	h, ok := harnesses.Get(harness)
	if !ok || h.ConversationEnv == "" || !conversationArgRE.MatchString(call.Conversation) {
		return ""
	}
	return call.Conversation
}

// recognized is what the core made of the caller, as /calls/session and
// /calls/addition report it (agent): the harness and the signal that named it
// (harnesses.Recognize; "claim" when the agent's harness argument named it
// alone), both null when nothing did, so an SDK can warn only then; and
// what the agent claimed (harnesses.ClaimName), null if it didn't. A claim
// that differs from harness was ignored.
type recognized struct {
	Harness *string `json:"harness"`
	Via     *string `json:"via"`
	Claimed *string `json:"claimed"`
}

// viaClaim: the agent's harness argument named the harness (nothing else
// recognized the call).
const viaClaim = "claim"

// recognize is the call's harness: what its client name, _meta or
// User-Agent say (harnesses.Recognize), refined by what its agent claims
// (harnesses.Refine), and what the claim did.
func recognize(call callIn) (harnesses.Harness, recognized, harnesses.Outcome) {
	det, via, ok := harnesses.Recognize(call.Client, call.Meta, call.UserAgent)
	h, outcome := harnesses.Refine(det, ok, call.Harness)
	var rec recognized
	if name := harnesses.ClaimName(call.Harness); name != "" {
		rec.Claimed = &name
	}
	switch {
	case outcome == harnesses.Decided:
		v := viaClaim
		rec.Harness, rec.Via = &h.ID, &v
	case h.ID != "":
		v := string(via)
		rec.Harness, rec.Via = &h.ID, &v
	}
	return h, rec, outcome
}

// Harnesses with their own wake path (internal/harnesses has the others
// Wakeshift's hooks reach: Cursor, Copilot CLI, opencode, Antigravity).
// Anything else can't be woken (cantWakeResult).
const (
	harnessCodex      = "codex"
	harnessClaudeCode = "claude-code"
	harnessCopilot    = "copilot"
	harnessOpenClaw   = "openclaw"
	harnessChatGPT    = "chatgpt"       // woken by MCP Events (events.go)
	harnessCowork     = "claude-cowork" // woken by a Monitor alone (cowork.go)
)

// harnessOf is which harness a caller is (by its client name, else its
// _meta, else its User-Agent: harnesses.Recognize) and the agent it's named
// as (the harness's ID, as wakeshift.HarnessAgent names it; harnesses.AgentID of the
// client name for anything else). The harness
// is "" (no wake path of its own) unless it's Codex with a thread, or one
// Wakeshift can reach (its hooks or plugin, or a background command alone:
// Capy, Muse), or a retired one, so its agent is told so.
//
// The agent's claim (harnesses.Refine) can name a harness of the
// recognized one's family (Grok Bot, a Cursor cloud agent: variant), or the
// harness of a call nothing recognized. It never changes the agent the
// conversation is named as (a conversation's calls with and without it
// stay one conversation), nor the harness its workspace and session are
// keyed by (the family's: Cursor), so a claim reaches nothing that stating
// a client name wouldn't.
func harnessOf(call callIn) (harness, agent string, rec recognized, variant harnesses.Harness, outcome harnesses.Outcome) {
	h, rec, outcome := recognize(call)
	if rec.Harness == nil {
		return "", harnesses.AgentID(call.Client), rec, variant, outcome
	}
	if h.Family != "" {
		variant = h
		h, _ = harnesses.Get(h.Family)
	}
	// A variant of a client name ("cursor-nightly") names the agent, but only
	// the name a harness really sends (or registered with, a tag aside:
	// harnesses.ExactClient) gets its hooks' result: a variant may be
	// another product, with other hooks.
	_, exact := harnesses.ExactClient(call.Client)
	exact = exact || *rec.Via != string(harnesses.ByClient) || outcome == harnesses.Confirmed || outcome == harnesses.Refined
	agent = h.ID
	if harnesses.NamedByClient(h) || outcome == harnesses.Decided { // Hermes Agent ("mcp" or "hermes-agent"), or a claim alone
		agent = harnesses.AgentID(call.Client)
	}
	switch {
	case h.ID == harnessCodex && codexThread(call.Meta) == "": // nothing to wake
		return "", agent, rec, variant, outcome
	case h.ID == harnessCodex, h.ID == harnessClaudeCode, h.ID == harnessChatGPT, exact && (h.Installable || h.ReplacedBy != "" || h.ZeroInstall == harnesses.Background || h.Wake == harnesses.Monitor):
		return h.ID, agent, rec, variant, outcome
	}
	return "", agent, rec, variant, outcome
}

// threadOf is the conversation a tool call names in its _meta, for a
// harness that names it there: a Codex thread, or the conversation
// Antigravity (always) or Copilot CLI (once Wakeshift's hooks are
// installed) puts there.
func threadOf(harness string, meta map[string]any) string {
	if harness == harnessCodex {
		return codexThread(meta)
	}
	if h, ok := harnesses.Get(harness); ok && h.Binding == harnesses.ByConversation {
		return harnesses.Conversation(h, meta)
	}
	return ""
}

// codexThread is the Codex thread a tool call came from, from its _meta:
// x-codex-turn-metadata.thread_id (0.160+), or threadId (earlier). The
// same parsing as wakeshift.CodexThread.
func codexThread(meta map[string]any) string {
	id, _ := meta["threadId"].(string)
	if turn, ok := meta["x-codex-turn-metadata"].(map[string]any); ok {
		if t, _ := turn["thread_id"].(string); t != "" {
			id = t
		}
	}
	if len(id) > 200 {
		return ""
	}
	return id
}

// embeddedThread is the Codex thread an OpenClaw call came from when
// OpenClaw ran the turn in Codex's app-server (harnesses.Embedded: Codex's
// MCP client, the thread marked as OpenClaw's), in a user's workspace; ""
// otherwise. It names the conversation's session there for the thread's
// calls that pass no session_key (issue #1125): OpenClaw's plugin for
// Codex turns, installed mid-turn, is hooked into the thread only from
// its next turn, and agents leave session_key off tools that subscribe to
// nothing, so without it nothing that turn carries a beacon. Never in a
// conversation's own workspace (no user), which only its key names
// (findConversation).
func embeddedThread(cv *conversation) string {
	if cv.harness != harnessOpenClaw || cv.external == "" || strings.HasPrefix(cv.external, conversationPrefix) {
		return ""
	}
	h, ok := harnesses.Get(harnessOpenClaw)
	if !ok || !harnesses.Embedded(h, cv.call.Meta) {
		return ""
	}
	turn, _ := cv.call.Meta["x-codex-turn-metadata"].(map[string]any)
	t, _ := turn["thread_id"].(string)
	if len(t) > 200 {
		return ""
	}
	return t
}

// codexThreadSession is agent's session that subscribed from Codex thread
// thread (embeddedThread) and still watches, the latest if several, and
// hasn't been connected by a plugin yet (a connected one is its
// plugin's conversation, and needs no beacon); sessions.ErrNotFound if
// none.
func codexThreadSession(ctx context.Context, tx pgx.Tx, agent, thread string) (sessions.Session, error) {
	var id string
	err := tx.QueryRow(ctx, `SELECT id::text FROM sessions WHERE agent_id = $1 AND codex_thread = $2 AND watching AND conversation IS NULL
		ORDER BY last_active_at DESC LIMIT 1`, agent, thread).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return sessions.Session{}, sessions.ErrNotFound
	}
	if err != nil {
		return sessions.Session{}, err
	}
	return sessions.Get(ctx, tx, id)
}

// conversation is the calling conversation, as Wakeshift keeps it.
type conversation struct {
	app     apps.App
	call    callIn
	harness string
	thread  string
	// passedThread: the agent passed thread itself (callIn.Conversation),
	// rather than the harness naming it in _meta.
	passedThread bool
	recognized   recognized // what the call said about its harness (agent in responses)
	// variant is the harness of harness's family the agent claimed (Grok
	// Bot, a Cursor cloud agent; harnesses.Harness.Family), if it did;
	// claim is what its claim did.
	variant harnesses.Harness
	claim   harnesses.Outcome
	// said: the conversation said which harness it is, in this call or an
	// earlier one (settleHarness), so its result doesn't ask.
	said     bool
	external string // the workspace's external ID
	tenant   string
	act      actor
	session  sessions.Session
	handle   string // what the app passes back as call.session
	// staleHandle: the call passed a handle Wakeshift doesn't know (it
	// ended, or was mistyped or made up), so subscribing started a new
	// session instead.
	staleHandle bool
	// agentHandles: an app acting for one of its agents (agentwatch.go,
	// agent-token event subscribes): its sessions are found and made by
	// agentSession.
	agentHandles bool
	// told, if set, is whether the conversation had the full instructions
	// before this call: a pending-addition record's !first (calls.go),
	// computed before the call subscribed, where toldBefore would count the
	// call's own subscriptions.
	told *bool
	// noBeacon is why the result being made has no beacon, where its path
	// decided that (logBeacon).
	noBeacon string
	// pushLine: the block being made carries the native push line for
	// this conversation (nativePush), so its subscribing part offers
	// no install (hooksResult): the push line is what wakes it.
	pushLine bool
}

var (
	// errNoConversation: nothing is kept for this caller yet.
	errNoConversation = errors.New("no such conversation")
	// errOtherClient: the handle is another MCP client's conversation's,
	// in the caller's own workspace (another workspace's is errNoSession).
	errOtherClient = errors.New("another client's session")
	convTokenRE    = regexp.MustCompile(`^[A-Za-z0-9_-]{22}$`)
	sessionTokenRE = regexp.MustCompile(`^[a-z2-7]{26}$`)
)

// Session handles are random, so one conversation can't name another's
// and a handle never recurs: wsc_<token>. Without a user, the token names
// the conversation's own workspace (22 base64url characters); the "~" keeps
// those apart from the app's own user IDs. A harness's conversation ID (a
// Codex thread) never names a workspace: harnesses send it to every MCP
// server, so it isn't a secret. With a user, it names a session in the
// user's workspace (26 lowercase base32 characters: the session's label, so
// its internal handle is <agent>@<token>). Sessions a harness names by
// conversation get one too, for the app to show.
const (
	conversationPrefix = "~conversation:"
	sessionTokenPrefix = "wsc_"
	conversationLabel  = "conversation" // its one session: <agent>@conversation
)

// newSessionToken is a session's random label (128 bits).
func newSessionToken() string {
	b := make([]byte, 16)
	rand.Read(b)
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b))
}

// handleOf is the handle the app passes back for a session in a user's
// workspace (or one a harness names by conversation).
func handleOf(s sessions.Session) string {
	if sessionTokenRE.MatchString(s.Label) {
		return sessionTokenPrefix + s.Label
	}
	return s.Handle
}

// handleSession is the session in the workspace the call's handle names:
// errNoSession if none does, errOtherClient if it's another agent's (another
// MCP client's conversation in this workspace). The check runs inside the
// workspace, so a handle from another user's workspace (or another app's)
// is errNoSession, as a made-up one is: a call can't learn that a handle
// names a session elsewhere, and never attaches to it.
func handleSession(ctx context.Context, tx pgx.Tx, cv *conversation) (sessions.Session, error) {
	tok, ok := strings.CutPrefix(cv.call.Session, sessionTokenPrefix)
	if !ok || !sessionTokenRE.MatchString(tok) {
		return sessions.Session{}, errUnknownKey
	}
	s, err := sessions.ByHandle(ctx, tx, cv.act.agent+"@"+tok)
	if errors.Is(err, sessions.ErrNotFound) {
		var other bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM sessions WHERE label = $1)`, tok).Scan(&other); err != nil {
			return s, err
		}
		if other {
			return s, errOtherClient
		}
		return s, errUnknownKey
	}
	if err != nil {
		return s, err
	}
	return heirOf(ctx, tx, s)
}

// heirOf is the session s's subscriptions went to if it was merged into
// another (mergeInto: its conversation's hook connected a sibling), the
// same agent's in the same workspace, else s: its session_key still finds
// what it subscribed to.
func heirOf(ctx context.Context, tx pgx.Tx, s sessions.Session) (sessions.Session, error) {
	for range 8 {
		var next *string
		if err := tx.QueryRow(ctx, `SELECT merged_into::text FROM sessions WHERE id = $1`, s.ID).Scan(&next); err != nil || next == nil {
			return s, err
		}
		h, err := sessions.Get(ctx, tx, *next)
		if errors.Is(err, sessions.ErrNotFound) || err == nil && h.Agent != s.Agent {
			return s, nil
		}
		if err != nil {
			return s, err
		}
		s = h
	}
	return s, nil
}

// newSession starts a session with a random handle (for a conversation the
// harness names, that conversation's).
func newSession(ctx context.Context, tx pgx.Tx, cv *conversation, conversation string) (sessions.Session, error) {
	n := sessions.New{Agent: cv.act.agent, Label: newSessionToken(), CreatedBy: "app:" + cv.app.ID}
	if conversation != "" {
		n.Conversation, n.Runtime = conversation, sessions.Connector
	}
	return sessions.Register(ctx, tx, n)
}

// findConversation works out the caller's workspace, creating it (with
// create) if new. It only identifies the workspace: resolve does the rest,
// inside it.
func (a *API) findConversation(ctx context.Context, app apps.App, call callIn, create bool) (*conversation, error) {
	call.User, call.Session = strings.TrimSpace(call.User), strings.TrimSpace(call.Session)
	if len(call.User) > 200 || strings.HasPrefix(call.User, "~") || len(call.Client) > 200 || len(call.Session) > 100 {
		return nil, badRequest{errors.New("call.user is at most 200 characters (not starting with ~), call.client 200, call.session 100")}
	}
	cv := &conversation{app: app, call: call}
	var agent string
	cv.harness, agent, cv.recognized, cv.variant, cv.claim = harnessOf(call)
	cv.thread = threadOf(cv.harness, call.Meta)
	if cv.thread == "" {
		cv.thread = argConversation(cv.harness, call)
		cv.passedThread = cv.thread != ""
	}
	// Without a user, a conversation's own workspace is named only by the
	// random token Wakeshift handed it (its session key), never by the
	// conversation ID the call reports (a Codex thread, Antigravity's
	// conversation ID): harnesses send that ID to every MCP server the user
	// has, so it can name a conversation but never act as it. A call without
	// the key starts a new conversation, whose session records the ID
	// (resolve) for its hook's beacons.
	mustExist := false
	switch {
	case call.User != "":
		cv.external = call.User
	case call.Session != "":
		if tok, ok := strings.CutPrefix(call.Session, sessionTokenPrefix); ok && convTokenRE.MatchString(tok) {
			cv.external, cv.handle, mustExist = conversationPrefix+tok, call.Session, true
			break
		}
		// Not a handle Wakeshift made: subscribing starts a new
		// conversation; anything else has none to find.
		if !create {
			return nil, errUnknownKey
		}
		cv.staleHandle = true
		cv.external, cv.handle = newConversation()
	case create:
		cv.external, cv.handle = newConversation()
	default:
		return nil, errNoConversation
	}
	var err error
	if mustExist {
		err = a.DB.Pool.QueryRow(ctx, `SELECT id::text FROM tenants WHERE app_id = $1 AND external_id = $2`, app.ID, cv.external).Scan(&cv.tenant)
		if errors.Is(err, pgx.ErrNoRows) {
			if !create {
				return nil, errUnknownKey
			}
			// A handle that names nothing (any more): a new conversation,
			// never one named by the handle it passed.
			cv.staleHandle, mustExist = true, false
			cv.external, cv.handle = newConversation()
		} else if err != nil {
			return nil, err
		}
	}
	switch {
	case mustExist: // found above
	case create:
		name := app.Name + " user"
		if call.User == "" {
			name = app.Name + " conversation"
		}
		if err := apps.CheckWorkspaceCap(ctx, a.DB.Pool, app, cv.external); err != nil {
			return nil, err
		}
		err = a.DB.Pool.QueryRow(ctx, `INSERT INTO tenants (name, kind, app_id, external_id) VALUES ($1, 'app', $2, $3)
			ON CONFLICT (app_id, external_id) WHERE app_id IS NOT NULL DO UPDATE SET name = tenants.name
			RETURNING id::text`, name, app.ID, cv.external).Scan(&cv.tenant)
	default:
		err = a.DB.Pool.QueryRow(ctx, `SELECT id::text FROM tenants WHERE app_id = $1 AND external_id = $2`, app.ID, cv.external).Scan(&cv.tenant)
		if errors.Is(err, pgx.ErrNoRows) && call.Session != "" && cv.thread == "" {
			// The user has no workspace here, so the key the call passed
			// names nothing (a conversation the harness names is found by
			// that, not by the key).
			return nil, errUnknownKey
		}
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, errNoConversation
		}
	}
	if err != nil {
		return nil, err
	}
	// The app is acting for it: it's in use (idle conversations are forgotten).
	if err := a.DB.Touch(ctx, cv.tenant); err != nil {
		return nil, err
	}
	cv.act = actor{app: app.ID, tenant: cv.tenant, agent: agent}
	return cv, nil
}

// newConversation names a new conversation's own workspace (no user, no
// conversation ID from the harness), and its handle.
func newConversation() (external, handle string) {
	b := make([]byte, 16)
	rand.Read(b)
	tok := base64.RawURLEncoding.EncodeToString(b)
	return conversationPrefix + tok, sessionTokenPrefix + tok
}

// resolve finds (with create, makes) the caller's agent, its grant and its
// session in the conversation's workspace: the Codex thread's session, else
// the session handle passed back, else (with a user) a new session. The
// grant is held FOR SHARE, so a revocation waits for this transaction.
func (a *API) resolve(ctx context.Context, tx pgx.Tx, cv *conversation, create bool) error {
	agent := cv.act.agent
	if ok, err := agents.Exists(ctx, tx, agent); err != nil {
		return err
	} else if !ok {
		if !create {
			return noAgentYet(ctx, tx, cv)
		}
		// The workspace's first calls may come in parallel (an agent's
		// first turn calling two of the app's tools at once): each makes
		// the agent unless the other did.
		if err := agents.Ensure(ctx, tx, agent); err != nil {
			return err
		}
	}
	var g grants.Grant
	var err error
	if create {
		g, err = grants.EnsureByApp(ctx, tx, topicsApp(cv.app), agent)
	} else {
		g, err = grants.Live(ctx, tx, cv.app.ID, agent)
		if errors.Is(err, grants.ErrNotFound) {
			return errNoConversation
		}
	}
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `SELECT 1 FROM grants WHERE id = $1 FOR SHARE`, g.ID); err != nil {
		return err
	}
	cv.act.grant = g
	var s sessions.Session
	switch {
	case strings.HasPrefix(cv.external, conversationPrefix):
		// The conversation's own workspace has one session, of the client
		// that made it: another client passing its handle is refused, not
		// given a second session there.
		var other bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM sessions WHERE label = $1 AND agent_id <> $2)`, conversationLabel, agent).Scan(&other); err != nil {
			return err
		}
		if other {
			return errOtherClient
		}
		if create {
			s, err = sessions.Register(ctx, tx, sessions.New{Agent: agent, Label: conversationLabel, CreatedBy: "app:" + cv.app.ID})
		} else {
			s, err = sessions.ByHandle(ctx, tx, agent+"@"+conversationLabel)
		}
		if err == nil && cv.thread != "" && s.Conversation == "" {
			// The call names its conversation (its harness did, or its agent
			// passed the ID): the session learns it, once, so its hook's
			// beacons name it. The key found the session; the ID only names
			// it.
			if _, err = tx.Exec(ctx, `UPDATE sessions SET conversation = $2 WHERE id = $1 AND conversation IS NULL`, s.ID, cv.thread); err == nil {
				s.Conversation = cv.thread
			}
		}
	case cv.thread != "":
		if create {
			s, err = adoptSession(ctx, tx, cv)
			if errors.Is(err, sessions.ErrNotFound) {
				s, err = newSession(ctx, tx, cv, cv.thread)
			}
		} else if cv.passedThread && cv.call.Session != "" {
			// It passed its ID with its handle: the session learns the ID
			// if it has none, as a subscribe teaches it (adoptSession), so
			// a result of a tool that subscribes nothing can carry a
			// beacon naming it. pi's extension passes both on the app's
			// tools after the reload that loads it, for a conversation
			// that subscribed before it was there and so never named
			// itself (issue #1106). adoptSession finds the session by the
			// handle (handleSession: this agent's, in this workspace) before
			// it changes anything. Otherwise its handle still names it.
			s, err = adoptSession(ctx, tx, cv)
			if errors.Is(err, sessions.ErrNotFound) {
				s, err = handleSession(ctx, tx, cv)
			}
		} else {
			s, err = sessions.ByConversation(ctx, tx, agent, cv.thread)
		}
	case cv.call.Session != "":
		s, err = handleSession(ctx, tx, cv)
		switch {
		case err == nil:
			err = sessions.Touch(ctx, tx, s.ID)
		case errors.Is(err, errNoSession) && create:
			// A handle that names nothing here (it ended, or was mistyped
			// or made up): a new session, with a handle of its own.
			cv.staleHandle = true
			s, err = newSession(ctx, tx, cv, "")
		}
	case create:
		s, err = newSession(ctx, tx, cv, "")
	case embeddedThread(cv) != "":
		// OpenClaw running the turn in Codex, with no session_key: the
		// session that subscribed from the same Codex thread, if one
		// still watches (codexThreadSession).
		s, err = codexThreadSession(ctx, tx, agent, embeddedThread(cv))
	default:
		return errNoConversation
	}
	if errors.Is(err, sessions.ErrNotFound) {
		if create {
			return errNoSession
		}
		return errNoConversation
	}
	if err != nil {
		return err
	}
	if t := embeddedThread(cv); create && t != "" && s.Conversation == "" {
		// It subscribes from a Codex thread, not yet connected: the session
		// records the thread, so the thread's later calls find it without
		// a session_key (codexThreadSession).
		if _, err := tx.Exec(ctx, `UPDATE sessions SET codex_thread = $2 WHERE id = $1 AND codex_thread IS DISTINCT FROM $2`, s.ID, t); err != nil {
			return err
		}
	}
	cv.session = s
	if cv.handle == "" {
		cv.handle = handleOf(s)
	}
	return settleHarness(ctx, tx, cv)
}

// settleHarness works out the conversation's variant of its harness's
// family (cv.variant: Grok Bot, a Cursor cloud agent) and records it on
// the session (sessions.harness), for its later calls that don't say
// (and, for Grok Bot, the delivery credentials that come later). In
// order: what the agent claims, if the call accepted it; else what the
// session recorded, if it's of the family the call was recognized as;
// and where that leaves plain Cursor, a cloud agent's conversation ID
// (bc-…) makes it a Cursor cloud agent (agents don't always know they're
// in the cloud). The record is a hint from the conversation itself (the
// caller holds its handle, or names its conversation): it only picks the
// instructions, and grants nothing.
func settleHarness(ctx context.Context, tx pgx.Tx, cv *conversation) error {
	var recorded *string
	if err := tx.QueryRow(ctx, `SELECT harness FROM sessions WHERE id = $1`, cv.session.ID).Scan(&recorded); err != nil {
		return err
	}
	accepted := cv.claim == harnesses.Confirmed || cv.claim == harnesses.Refined || cv.claim == harnesses.Decided
	if !accepted && recorded != nil && cv.harness != "" {
		r, ok := harnesses.Get(*recorded)
		switch {
		case ok && r.ID == cv.harness:
			cv.said = true
		case ok && r.Family == cv.harness:
			cv.variant, cv.said = r, true
		}
	}
	// An accepted claim replaces the record: the agent may correct itself
	// (it said cursor, it's Grok Bot). Only a caller that holds the
	// session's key (or names its conversation) gets here, a claim can't
	// leave the family the calls are recognized as (Refine), and the record
	// only picks instructions for this session: nothing to protect from the
	// session's own agent. A claim that contradicts recognition isn't
	// accepted, so it never overwrites the record.
	record := ""
	if accepted {
		record, cv.said = *cv.recognized.Harness, true
	}
	if cv.variant.ID == "" && cv.harness == "cursor" && strings.HasPrefix(cv.session.Conversation, "bc-") {
		// Cursor (claimed, recorded or neither) with a cloud agent's ID.
		cv.variant, _ = harnesses.Get("cursor-cloud")
		record, cv.said = cv.variant.ID, true
	}
	if cv.variant.ID != "" {
		cv.recognized.Harness = &cv.variant.ID
	}
	if record == "" || recorded != nil && *recorded == record {
		return nil
	}
	_, err := tx.Exec(ctx, `UPDATE sessions SET harness = $2 WHERE id = $1`, cv.session.ID, record)
	return err
}

// noAgentYet is why a call that creates nothing finds no conversation when
// the workspace has no agent for its client: the handle it passed is
// another client's (errOtherClient), or names nothing (errNoSession), or it
// passed none (errNoConversation).
func noAgentYet(ctx context.Context, tx pgx.Tx, cv *conversation) error {
	switch {
	case cv.thread != "" && !strings.HasPrefix(cv.external, conversationPrefix):
		return errNoConversation
	case strings.HasPrefix(cv.external, conversationPrefix):
		var taken bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM sessions WHERE label = $1)`, conversationLabel).Scan(&taken); err != nil {
			return err
		}
		if taken {
			return errOtherClient
		}
		return errNoConversation
	case cv.call.Session != "":
		if _, err := handleSession(ctx, tx, cv); err != nil {
			return err
		}
	}
	return errNoConversation
}

// adoptSession: a call that names its conversation (in _meta) and passes
// back a handle may be the conversation that handle was made for before the
// harness named it: a Copilot conversation that subscribed before
// Wakeshift's preMcpToolCall hook was installed, then resumed with the
// hook. If no session has the conversation yet and the handle is one of
// this agent's sessions that has none, the session learns it (once, as a
// connector redeem would teach it), so the conversation keeps one session
// rather than a second one publishes also match. A session that already
// has a conversation is never taken. sessions.ErrNotFound: nothing to
// adopt.
func adoptSession(ctx context.Context, tx pgx.Tx, cv *conversation) (sessions.Session, error) {
	agent := cv.act.agent
	s, err := sessions.ByConversation(ctx, tx, agent, cv.thread)
	if err == nil {
		return s, sessions.Touch(ctx, tx, s.ID)
	}
	if !errors.Is(err, sessions.ErrNotFound) || cv.call.Session == "" {
		return s, err
	}
	s, err = handleSession(ctx, tx, cv)
	if errors.Is(err, errNoSession) || errors.Is(err, errOtherClient) || err == nil && s.Conversation != "" {
		return sessions.Session{}, sessions.ErrNotFound
	}
	if err != nil {
		return s, err
	}
	tag, err := tx.Exec(ctx, `UPDATE sessions SET conversation = $2 WHERE id = $1 AND conversation IS NULL
		AND NOT EXISTS (SELECT 1 FROM sessions o WHERE o.agent_id = sessions.agent_id AND o.conversation = $2)`, s.ID, cv.thread)
	if err != nil {
		return s, err
	}
	if tag.RowsAffected() == 0 {
		return sessions.Session{}, sessions.ErrNotFound
	}
	s.Conversation = cv.thread
	return s, sessions.Touch(ctx, tx, s.ID)
}

// topicsApp is the app as its topic grants record it: an app needs no agent
// scope in its manifest for topics (they're outside it), so without one
// the grant has none, for as long as a grant can last.
func topicsApp(app apps.App) apps.App {
	if app.Manifest.Agent == nil {
		app.Manifest.Agent = &apps.AgentScope{TTLDays: 365}
	}
	return app
}

// ── Subscriptions ────────────────────────────────────────────────────────

// topicFilters is a subscription's filters for a topic and the app's filter
// on its events' data (equality on top-level fields).
func topicFilters(topic string, filter map[string]json.RawMessage, once bool) (map[string]string, error) {
	if len(filter) > maxFilterFields {
		return nil, badRequest{fmt.Errorf("at most %d filter fields", maxFilterFields)}
	}
	f := map[string]string{topicAttr(topic): "1"}
	for k, raw := range filter {
		v, ok := subscriptions.CanonicalJSON(raw)
		if k == "" || len(k) > 64 || !ok || len(v) > 256 {
			return nil, badRequest{errors.New("filter is {field: value}: top-level fields of the event's data (at most 64 characters), each equal to a string, number, boolean or null (at most 256 bytes)")}
		}
		f[subscriptions.DataFilterPrefix+k] = v
	}
	if once {
		f[subscriptions.OnceFilter] = "true"
	}
	return f, nil
}

// insertTopicSub makes a topic subscription for the conversation's session
// (the caller holds the topic's members lock), returning its ID.
func insertTopicSub(ctx context.Context, tx pgx.Tx, cv *conversation, topic string, filters map[string]string, once bool, ttl time.Duration) (string, error) {
	sub, err := subscriptions.Create(ctx, tx, subscriptions.New{AgentID: cv.act.agent, EventType: topicsType(cv.app.ID), Filters: filters,
		Reason: "Topic " + topic, CreatedBy: "app:" + cv.app.ID, GrantID: cv.act.grant.ID, SessionID: cv.session.ID})
	if err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO topic_subscriptions (tenant_id, subscription_id, app_id, topic, session_id, once, ttl_seconds, expires_at)
		VALUES (current_tenant(), $1, $2, $3, $4, $5, $6::int, now() + make_interval(secs => $6::int))`,
		sub.ID, cv.app.ID, topic, cv.session.ID, once, int(ttl.Seconds())); err != nil {
		return "", err
	}
	_, err = tx.Exec(ctx, `INSERT INTO topic_members (app_id, topic, tenant_id, subscription_id) VALUES ($1, $2, current_tenant(), $3)`,
		cv.app.ID, topic, sub.ID)
	return sub.ID, err
}

// refreshTopicSub sets a live topic subscription's topic, filters, once
// and ttl, and starts its ttl again. The topic changes only for an event
// subscription recorded before its topic was worked out as it is now (a
// number argument written 1.0 was once on .../1.0, where an emit of 1
// never went): it moves to the topic its arguments name, whose members
// lock the caller holds. The old topic's isn't taken (a second lock, out
// of the order publishes take them in): a publish to it racing the move
// reaches the subscription once more, as it would have a moment earlier.
//
// errRoutingEnded: the subscription ended (or its routing did) before this
// got its row, so there's nothing to refresh, and nothing was changed.
func refreshTopicSub(ctx context.Context, tx pgx.Tx, subID, topic string, filters map[string]string, once bool, ttl time.Duration) error {
	tag, err := tx.Exec(ctx, `UPDATE subscriptions SET filters = $2 WHERE id = $1 AND status = 'active'`, subID, filters)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return errRoutingEnded
	}
	tag, err = tx.Exec(ctx, `UPDATE topic_subscriptions SET topic = $2, once = $3, ttl_seconds = $4::int, expires_at = now() + make_interval(secs => $4::int)
		WHERE subscription_id = $1 AND ended_at IS NULL`, subID, topic, once, int(ttl.Seconds()))
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return errRoutingEnded
	}
	_, err = tx.Exec(ctx, `UPDATE topic_members SET topic = $2 WHERE tenant_id = current_tenant() AND subscription_id = $1 AND topic <> $2`, subID, topic)
	return err
}

// slideTopicSub starts a topic subscription's ttl again: something was
// published to it (a subscription's ttl runs without a delivery or a fresh
// subscribe). Not a webhook's, a poll's or a push stream's routing (a push
// subscription's lease moves only while a stream holds it): an MCP Events subscription never
// outlives its refresh_before without a refresh, and its webhook lapses then
// however busy the topic is. Routing that outlived it would queue deliveries
// for a session nothing delivers, for as long as anything is published (a
// house agent's game table playing on after it left).
func slideTopicSub(ctx context.Context, tx pgx.Tx, subID string) error {
	_, err := tx.Exec(ctx, `UPDATE topic_subscriptions ts SET expires_at = now() + make_interval(secs => ts.ttl_seconds)
		WHERE ts.subscription_id = $1 AND NOT EXISTS (SELECT 1 FROM event_subscriptions es
			WHERE es.tenant_id = ts.tenant_id AND es.subscription_id = ts.subscription_id AND es.destination IN ('webhook', 'poll', 'push'))`, subID)
	return err
}

// webhookGone is true of an event subscription es whose webhook no longer
// delivers: lapsed, ending or removed (mcpevents.Exists, as SQL). The one
// definition for endTopicSub and CancelStranded.
const webhookGone = `NOT EXISTS (SELECT 1 FROM mcp_event_webhooks w WHERE w.tenant_id = es.tenant_id AND w.id = es.webhook_id
	AND w.expires_at > now() AND w.ending_reason IS NULL)`

// deadWebhookRouting reports whether a topic subscription is a webhook's
// routing whose webhook no longer delivers (lapsed, ending or gone), or a
// poll or push subscription's routing (asked only as it ends). Only that
// webhook, those polls, or its stream deliver its session, so what waits
// there never goes.
func deadWebhookRouting(ctx context.Context, tx pgx.Tx, subID string) (bool, error) {
	var dead bool
	err := tx.QueryRow(ctx, `SELECT es.destination IN ('poll', 'push') OR `+webhookGone+`
		FROM event_subscriptions es WHERE es.subscription_id = $1 AND es.destination IN ('webhook', 'poll', 'push') ORDER BY es.created_at DESC LIMIT 1`, subID).Scan(&dead)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return dead, err
}

// replay delivers to a new subscription the latest event published to its
// topic within the window before it began that passes its filter, if any.
func (a *API) replay(ctx context.Context, tx pgx.Tx, app apps.App, topic, subID string, filters map[string]string, before time.Time, window time.Duration) error {
	rows, err := tx.Query(ctx, `SELECT `+pastPublishColumns+` FROM topic_events
		WHERE app_id = $1 AND $2 = ANY(topics) AND published_at > now() - make_interval(secs => $3) AND published_at < $4
		ORDER BY published_at DESC LIMIT 50`, app.ID, topic, window.Seconds(), before)
	if err != nil {
		return err
	}
	list, err := pgx.CollectRows(rows, pgx.RowToStructByPos[pastPublish])
	if err != nil {
		return err
	}
	for _, p := range list {
		if p.Redacted {
			// Delivered, and its content deleted (it asked): the latest isn't
			// there to replay, and an older one isn't the latest.
			return nil
		}
		if !dataMatches(app.ID, topic, filters, p.Data) {
			continue
		}
		_, err := copyPublish(ctx, tx, app, topic, subID, p)
		return err
	}
	return nil
}

// pastPublish is one of the app's recent publishes (topic_events), as a
// replay reads it (pastPublishColumns).
type pastPublish struct {
	EventID, SourceID, Text string
	Data                    []byte
	At                      time.Time
	DeleteOnDelivery        bool
	Redacted                bool
	PublishURL              string // the publish URL it came through, or ""
}

const pastPublishColumns = `event_id::text, source_id, text, data, published_at, delete_on_delivery, redacted_at IS NOT NULL, coalesce(publish_url::text, '')`

// copyPublish copies a past publish into the transaction's workspace for
// the subscription subID alone (a replay). It reports false, copying
// nothing, when the publish's content is gone: deleted once delivered, or
// since it was read.
func copyPublish(ctx context.Context, tx pgx.Tx, app apps.App, topic, subID string, p pastPublish) (bool, error) {
	if p.Redacted {
		return false, nil
	}
	if p.DeleteOnDelivery {
		// Its copy here is the one delivered: the publish keeps no
		// content. Its row is locked before the copy is appended, in
		// the order a publish's own copy takes them (copyInto: the row,
		// then the workspace's event counter). Not replayed if a copy
		// redacted it since it was read, nor while any of its own copies
		// is still to be made: its content is its subscribers' until
		// the last copy deletes it, and redacted here, those copies
		// would have nothing to make.
		var gone bool
		if err := tx.QueryRow(ctx, `SELECT redacted_at IS NOT NULL OR EXISTS (SELECT 1 FROM topic_fanout f WHERE f.app_id = t.app_id AND f.source_id = t.source_id)
			FROM topic_events t WHERE t.app_id = $1 AND t.source_id = $2 FOR UPDATE OF t`, app.ID, p.SourceID).Scan(&gone); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return false, nil // deleted since it was read
			}
			return false, err
		}
		if gone {
			return false, nil
		}
		if err := redactPublish(ctx, tx, app.ID, p.SourceID); err != nil {
			return false, err
		}
	}
	attrs := map[string]string{"app": app.ID, "topic": topic, "replay": "true", topicAttr(topic): "1", subscriptions.ForAttr: subID}
	withAppEventID(attrs, p.SourceID, p.EventID, p.PublishURL) // the same eventId as the publish's own copies
	_, _, err := events.Append(ctx, tx, events.Event{Type: topicsType(app.ID), Source: "app:" + app.ID, SourceEventID: "replay:" + subID + ":" + p.SourceID,
		OccurredAt: p.At, From: events.Party{Kind: "app", ID: app.ID}, Subject: p.Text, Data: p.Data, Trust: events.External,
		Attrs: attrs, DeleteOnDelivery: p.DeleteOnDelivery})
	return err == nil, err
}

// dataMatches reports whether an event with data passes a topic
// subscription's filters, as the pipeline will judge it.
func dataMatches(app, topic string, filters map[string]string, data []byte) bool {
	typ := topicsType(app)
	s := subscriptions.Subscription{Status: subscriptions.Active, EventType: typ, Filters: filters}
	return s.Matches(events.Event{Seq: 1, Type: typ, Data: data, Attrs: map[string]string{topicAttr(topic): "1"}, From: events.Party{Kind: "app", ID: app}})
}

// liveTopicSub is a topic subscription that hasn't ended (as far as
// topic_subscriptions knows: its subscription may have, once or revoked).
type liveTopicSub struct {
	ID, Topic, Session string
	Expired            bool
	TTLSeconds         int
	Filters            map[string]string
	Status             string
}

const liveTopicSubColumns = `ts.subscription_id::text, ts.topic, ts.session_id::text, ts.expires_at <= now(), ts.ttl_seconds, s.filters, s.status
	FROM topic_subscriptions ts JOIN subscriptions s ON s.tenant_id = ts.tenant_id AND s.id = ts.subscription_id`

func liveTopicSubs(ctx context.Context, tx pgx.Tx, where string, args ...any) ([]liveTopicSub, error) {
	rows, err := tx.Query(ctx, `SELECT `+liveTopicSubColumns+` WHERE ts.ended_at IS NULL AND `+where+` ORDER BY ts.created_at`, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[liveTopicSub])
}

// Why Wakeshift ended a subscription.
const (
	endUnsubscribed = "unsubscribed"
	endTTL          = "ttl"
	endClosed       = "closed"
	endRemoved      = "removed" // the app ended it (event.end), or a person in the console
	endOnce         = "once"    // after its one delivery (the pipeline ended it)
	endRevoked      = "revoked" // with its grant
)

// endedWithoutUs is why a subscription that ended outside this file did.
func endedWithoutUs(s liveTopicSub) string {
	if s.Filters[subscriptions.OnceFilter] == "true" {
		return endOnce
	}
	return endRevoked
}

// lapsed is true of a topic subscription that's to be ended when found:
// its subscription ended (once, or with its grant), or its ttl ran out.
func lapsed(s liveTopicSub) bool { return s.Status != subscriptions.Active || s.Expired }

// lockEnding locks the topic subscriptions a transaction is about to end
// (endTopicSub), all of them before the first is ended, in one order every
// such transaction shares: their cursors, subscriptions and
// topic_subscriptions rows, each by ID. Ending one appends its notice,
// which holds the workspace's event counter until commit; locking the next
// only after that, two transactions ending the same subscriptions in
// different orders (a copy ends the topic's, then the direct recipients';
// the expiry pass, its batch oldest first) each wait on the other.
func lockEnding(ctx context.Context, tx pgx.Tx, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	ids = slices.Compact(slices.Sorted(slices.Values(ids))) // the queries order them; this just drops repeats
	for _, q := range []string{
		`SELECT 1 FROM subscription_cursors WHERE subscription_id = ANY($1::uuid[]) ORDER BY subscription_id FOR UPDATE`,
		`SELECT 1 FROM subscriptions WHERE id = ANY($1::uuid[]) ORDER BY id FOR NO KEY UPDATE`,
		`SELECT 1 FROM topic_subscriptions WHERE subscription_id = ANY($1::uuid[]) ORDER BY subscription_id FOR NO KEY UPDATE`,
	} {
		if _, err := tx.Exec(ctx, q, ids); err != nil {
			return err
		}
	}
	return nil
}

// topicSubIDs are the subscriptions' IDs.
func topicSubIDs(list []liveTopicSub) []string {
	ids := make([]string, len(list))
	for i, s := range list {
		ids[i] = s.ID
	}
	return ids
}

// topicSubName is how Wakeshift names a topic subscription to its
// conversation: for an event subscription's routing, the event and its
// arguments (`chat.message {channel:eng}`), not the topic inside the core
// (a personal event's starts with its user's hash); else the topic.
func topicSubName(ctx context.Context, tx pgx.Tx, sub, topic string) (string, error) {
	var event, args string
	err := tx.QueryRow(ctx, `SELECT event, arguments::text FROM event_subscriptions WHERE tenant_id = current_tenant() AND subscription_id = $1::uuid ORDER BY created_at DESC LIMIT 1`,
		sub).Scan(&event, &args)
	if errors.Is(err, pgx.ErrNoRows) {
		return topic, nil
	}
	if err != nil {
		return "", err
	}
	return eventSubName(event, args), nil
}

// eventSubName is an event subscription's name in Wakeshift's sentences:
// the event (a manifest's name) and its arguments, with quotes and angle
// brackets left out (framing.Name), so app-supplied values can't pose as
// Wakeshift's own text.
func eventSubName(event, args string) string {
	if args == "" || args == "{}" || args == "null" {
		return event
	}
	return event + " " + framing.Name(args)
}

// endTopicSub ends a topic subscription in the transaction's workspace and,
// with notice, tells its conversation why (a delivery of its own, straight
// to its session: the subscription no longer matches anything). With
// notice, what was published to it before it ended still arrives, first:
// only the conversation's own unsubscribe drops what's waiting.
func endTopicSub(ctx context.Context, tx pgx.Tx, app string, s liveTopicSub, reason string, notice bool) error {
	dead, err := deadWebhookRouting(ctx, tx, s.ID)
	if err != nil {
		return err
	}
	if dead {
		// A webhook's routing whose webhook is gone (a lapsed one ends by
		// ttl with it): what waits for its session can never be sent, so
		// it's canceled rather than left waiting, even if the
		// subscription ended already; and there's no conversation to tell.
		if _, err := subscriptions.Cancel(ctx, tx, s.ID); err != nil {
			return err
		}
		notice = false
	} else if s.Status == subscriptions.Active {
		if notice {
			if err := catchUp(ctx, tx, s.ID); err != nil {
				return err
			}
			if err := subscriptions.Finish(ctx, tx, s.ID); err != nil {
				return err
			}
		} else if _, err := subscriptions.Cancel(ctx, tx, s.ID); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE topic_subscriptions SET ended_reason = $2, ended_at = now() WHERE subscription_id = $1 AND ended_at IS NULL`, s.ID, reason); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM topic_members WHERE tenant_id = current_tenant() AND subscription_id = $1`, s.ID); err != nil {
		return err
	}
	if !notice || s.Status != subscriptions.Active {
		return nil
	}
	var name string
	if err := tx.QueryRow(ctx, `SELECT name FROM apps WHERE id = $1`, app).Scan(&name); err != nil {
		return err
	}
	name = framing.Name(name)
	why := map[string]string{
		endTTL:     "nothing arrived on it for " + span(time.Duration(s.TTLSeconds)*time.Second),
		endClosed:  name + " closed the topic",
		endRemoved: name + " ended it",
		endRevoked: "it was revoked in Wakeshift",
	}[reason]
	sess, err := sessions.Get(ctx, tx, s.Session)
	if errors.Is(err, sessions.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	what, err := topicSubName(ctx, tx, s.ID, s.Topic)
	if err != nil {
		return err
	}
	e, _, err := events.Append(ctx, tx, events.Event{Type: topicsType(app), Source: "hub", From: events.Party{Kind: "hub", ID: "hub"},
		Subject: fmt.Sprintf("Your subscription to %s in %s ended (%s). Its events won't reach this conversation any more.", what, name, why),
		Trust:   events.Internal, Attrs: map[string]string{"app": app, "topic": s.Topic, "ended": reason, subscriptions.ForAttr: s.ID}})
	if err != nil {
		return err
	}
	d := deliveries.New{AgentID: sess.Agent, Target: sess.ID, SubscriptionID: s.ID, Event: e}
	if sess.Runtime == sessions.Connector {
		d.DeliverTo = sess.ID
	}
	if _, err := deliveries.Insert(ctx, tx, d); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `SELECT pg_notify($1, current_tenant()::text || ':' || $2)`, deliveries.NotifyChannel, sess.Agent)
	return err
}

// catchUp makes the deliveries a subscription is owed from events the
// pipeline hasn't read for it yet, as a pipeline step would (unpaced: it's
// ending), so ending it loses none of them. It takes the subscription's
// cursor first, so a runner mid-step on it finishes (and its deliveries
// count) or skips it. Under a grant that's revoked or past its end it
// makes none, as live delivery makes none (pipeline.ConsentEnded): the
// consent ended, whether or not the expiry pass has revoked it yet.
func catchUp(ctx context.Context, tx pgx.Tx, id string) error {
	var last uint64
	err := tx.QueryRow(ctx, `SELECT last_seq FROM subscription_cursors WHERE subscription_id = $1 FOR UPDATE`, id).Scan(&last)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if ended, err := pipeline.ConsentEnded(ctx, tx, id); err != nil || ended {
		return err
	}
	sub, err := subscriptions.Get(ctx, tx, id)
	if err != nil {
		return err
	}
	once := sub.Filters[subscriptions.OnceFilter] == "true"
	const batch = 200
	created := false
	for done := false; !done; {
		evs, err := events.ReadAfter(ctx, tx, last, []string{sub.EventType}, batch)
		if err != nil {
			return err
		}
		done = len(evs) < batch
		for _, e := range evs {
			last = e.Seq
			targets, err := pipeline.Targets(ctx, tx, sub, e)
			if err != nil {
				return err
			}
			for _, t := range targets {
				inserted, err := deliveries.Insert(ctx, tx, deliveries.New{AgentID: sub.AgentID, Target: t.Target, DeliverTo: t.DeliverTo, SubscriptionID: sub.ID, Event: e})
				if err != nil {
					return err
				}
				created = created || inserted
			}
			if once && len(targets) > 0 {
				done = true
				break
			}
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE subscription_cursors SET last_seq = $2, updated_at = now() WHERE subscription_id = $1`, id, last); err != nil {
		return err
	}
	if created {
		_, err = tx.Exec(ctx, `SELECT pg_notify($1, current_tenant()::text || ':' || $2)`, deliveries.NotifyChannel, sub.AgentID)
	}
	return err
}

// ── Publishing ───────────────────────────────────────────────────────────

// topicEvent is what an app publishes.
type topicEvent struct {
	ID   string          `json:"id"`
	Text string          `json:"text"`
	Data json.RawMessage `json:"data"`
	// deleteOnDelivery: an occurrence of an event type that asks
	// (apps.EventType.DeleteOnDelivery); never the caller's to set.
	deleteOnDelivery bool
	// event and fingerprint: an emit's event type and its fingerprint
	// (emitFingerprint), so a repeated id with a different emit is refused
	// (checkRepeat). A publish by URL has neither.
	event           string
	fingerprint     []byte
	textFingerprint []byte // its text's as sent, before check trims it (textFingerprint)
	// trimmed: check cut the text to maxTopicText (trimText); the
	// response says so (text_trimmed).
	trimmed bool
}

// check refuses what an occurrence can't be, and trims a text over
// maxTopicText (trimText) rather than refusing it.
func (e *topicEvent) check() error {
	if string(e.Data) == "null" {
		e.Data = nil
	}
	switch {
	case strings.TrimSpace(e.Text) == "":
		return badRequest{errors.New("text is required")}
	case len(e.ID) > 200:
		return badRequest{errors.New("id is at most 200 characters")}
	case len(e.Data) > maxTopicData:
		return badRequest{fmt.Errorf("payload is at most %d bytes", maxTopicData)}
	case len(e.Data) > 0 && (strings.TrimSpace(string(e.Data))[0] != '{' || !json.Valid(e.Data)):
		return badRequest{errors.New("payload is a JSON object")}
	}
	e.Text, e.trimmed = trimText(e.Text)
	return nil
}

// textEllipsis ends a trimmed text.
const textEllipsis = "…"

// trimText is text cut to at most maxTopicText bytes, ellipsis included,
// at a character boundary, and whether it was cut. The text is the wake's
// headline; the details belong in the payload.
func trimText(text string) (string, bool) {
	if len(text) <= maxTopicText {
		return text, false
	}
	cut := maxTopicText - len(textEllipsis)
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut] + textEllipsis, true
}

// ── Status ───────────────────────────────────────────────────────────────

// How a conversation is connected right now, for an app's status calls:
const (
	viaStream = "stream" // a stream link held open (Cursor's background command, an SSE or WebSocket client)
	viaPoll   = "poll"   // long polls or waits (Claude Code's Monitor), or a wait tool, within the last minute
	viaHooks  = "hooks"  // Wakeshift's hooks or connector on the user's machine
	// viaWebhook: an MCP Events subscription (ChatGPT); Wakeshift POSTs to it.
	viaWebhook = "webhook"
)

// connectedVia is how a session is connected ("" if it isn't).
func connectedVia(s sessions.Session) string {
	switch {
	case !s.Connected:
		return ""
	case s.Runtime == sessions.Stream && s.ConnectorSeenAt != nil:
		return viaHooks
	case s.ConnectedVia == "webhook":
		return viaWebhook
	case s.ConnectedVia == "poll":
		return viaPoll
	}
	return viaStream
}

// topicCount is how many conversations are subscribed to a topic, and how
// many of them are connected now, and how.
type topicCount struct {
	Subscriptions int            `json:"subscriptions"` // conversations subscribed
	Connected     int            `json:"connected"`     // of those, connected now
	Via           map[string]int `json:"via"`
}

// topicCounts counts the conversations subscribed to one of the app's
// topics (an event's, for eventStatus). It only reads.
func (a *API) topicCounts(ctx context.Context, app, topic string) (topicCount, error) {
	out := topicCount{Via: map[string]int{viaStream: 0, viaPoll: 0, viaHooks: 0, viaWebhook: 0}}
	err := a.appTx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT DISTINCT tenant_id::text FROM topic_members WHERE app_id = $1 AND topic = $2 ORDER BY 1`, app, topic)
		if err != nil {
			return err
		}
		tenants, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return err
		}
		for _, t := range tenants {
			if err := setTenant(ctx, tx, t); err != nil {
				return err
			}
			rows, err := tx.Query(ctx, `SELECT DISTINCT ts.session_id::text FROM topic_subscriptions ts
				JOIN subscriptions s ON s.tenant_id = ts.tenant_id AND s.id = ts.subscription_id
				WHERE ts.app_id = $1 AND ts.topic = $2 AND ts.ended_at IS NULL AND ts.expires_at > now() AND s.status = 'active'`, app, topic)
			if err != nil {
				return err
			}
			ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
			if err != nil {
				return err
			}
			for _, id := range ids {
				s, err := sessions.Get(ctx, tx, id)
				if errors.Is(err, sessions.ErrNotFound) {
					continue
				}
				if err != nil {
					return err
				}
				out.Subscriptions++
				if via := connectedVia(s); via != "" {
					out.Connected++
					out.Via[via]++
				}
			}
		}
		return nil
	})
	return out, err
}

// ── Forget, expire ───────────────────────────────────────────────────────

// forgetUser deletes the workspace the app keeps for one of its users (their
// account was deleted, or they asked to be forgotten) and everything in it:
// sessions, subscriptions, events, deliveries, stream links (open streams
// close), grants, its audit log, and the user's ID itself. Also gone: the
// app's recent publishes to the user's private topics (kept for replay)
// and its publish URLs for them. The app's log records that a user was
// forgotten, with a hash in place of their ID. forgotten is false if there
// was nothing to forget.
func (a *API) forgetUser(w http.ResponseWriter, r *http.Request) {
	var in struct {
		User string `json:"user"`
	}
	if !httpx.Decode(w, r, &in) {
		return
	}
	app := AppFrom(r.Context())
	// Owners and admins only, as in the console: a key a developer made
	// can't (orgs.AppForgetUser).
	if !app.KeyMay(orgs.AppForgetUser) {
		httpx.WriteError(w, http.StatusForbidden, "forbidden",
			"forgetting a user takes a key that an owner or admin of the app's organization made; make one in the console")
		return
	}
	forgotten, err := a.ForgetUser(r.Context(), app.ID, in.User, "app:"+app.ID)
	a.respond(w, map[string]bool{"forgotten": forgotten}, err)
}

// eraseWorkspace deletes the transaction's workspace and everything in it,
// table by table inside it (row-level security applies), then the
// workspace itself. TestForgetUserDeletesEverything checks that no table
// keeps a row for it, so a new table with workspace data must be added
// here.
func eraseWorkspace(ctx context.Context, tx pgx.Tx) error {
	// Wait for a pipeline runner mid-step on one of its subscriptions, as
	// subscriptions.Cancel does, so nothing it writes lands after.
	if _, err := tx.Exec(ctx, `UPDATE subscription_cursors SET updated_at = now()`); err != nil {
		return err
	}
	for _, table := range []string{
		"connector_tickets", "session_setups", "client_requests", "pending_additions", "deliveries", "subscription_cursors", "event_subscriptions", "topic_subscriptions", "subscriptions",
		"stream_leases", "stream_links", "oauth_codes", "oauth_refresh_tokens", "grant_paces", "grants", "mcp_event_webhooks", "agent_webhooks",
		"agent_webhook_setups", "bot_challenges", "bot_keys", "sessions",
		"events", "event_counters", "devices", "agents",
		// Platform-level tables that name the workspace, its daily usage
		// counts included (deliveries.RollUpUsage holds the workspace's row
		// while it writes them, so none land after this).
		"topic_members", "topic_fanout", "memberships", "mcp_event_targets", "agent_webhook_targets", "usage_daily", "pending_addition_ids",
		"subscribe_vouchers", "bot_webhooks", "bot_setup_targets",
		// Redeem by call (callredeem.go): its conversation keys, and the call index.
		"connector_conversation_keys", "connector_call_index",
	} {
		if _, err := tx.Exec(ctx, `DELETE FROM `+table+` WHERE tenant_id = current_tenant()`); err != nil {
			return fmt.Errorf("erasing %s: %w", table, err)
		}
	}
	// Then the audit log, which is append-only and which the server's role
	// can't delete from, and the workspace itself, together: erase_tenant
	// (170_erase_tenant.sql) does both as the tables' owner, and only for a
	// workspace nothing else is left of.
	_, err := tx.Exec(ctx, `SELECT erase_tenant()`)
	return err
}

// expiredTopicsPerPass is how many lapsed topic subscriptions ExpireTopics
// ends at most.
const expiredTopicsPerPass = 200

// ExpireTopics ends the transaction's workspace's topic subscriptions whose
// ttl ran out, telling each conversation, the longest lapsed first, at most
// expiredTopicsPerPass: the expiry pass's transaction is bounded, and the
// next pass takes the rest. (Publishing and sending end them as they find
// them too.) The whole batch is locked first (lockEnding), as every
// transaction that ends several does, so a copy ending some of them can't
// deadlock with it.
func ExpireTopics(ctx context.Context, tx pgx.Tx) (int, error) {
	rows, err := tx.Query(ctx, `SELECT ts.app_id, `+liveTopicSubColumns+` WHERE ts.subscription_id IN (SELECT subscription_id FROM topic_subscriptions
		WHERE ended_at IS NULL AND expires_at <= now() ORDER BY expires_at LIMIT $1) ORDER BY ts.created_at`, expiredTopicsPerPass)
	if err != nil {
		return 0, err
	}
	type due struct {
		App string
		liveTopicSub
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (due, error) {
		var d due
		err := r.Scan(&d.App, &d.ID, &d.Topic, &d.Session, &d.Expired, &d.TTLSeconds, &d.Filters, &d.Status)
		return d, err
	})
	if err != nil {
		return 0, err
	}
	ids := make([]string, len(list))
	for i, d := range list {
		ids[i] = d.ID
	}
	if err := lockEnding(ctx, tx, ids); err != nil {
		return 0, err
	}
	now, err := liveNow(ctx, tx, ids)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, d := range list {
		s, ok := now[d.ID]
		if !ok || !lapsed(s) {
			continue // ended, or renewed, since it was found
		}
		if err := endTopicSub(ctx, tx, d.App, s, endTTL, true); err != nil {
			return 0, err
		}
		n++
	}
	return n, nil
}

// liveNow reads again, once they're locked (lockEnding), the topic
// subscriptions a transaction found lapsed with an unlocked read: those not
// ended, as they are now, by ID. One a subscribe renewed in between (its
// ttl started again) is no longer lapsed; one another transaction ended
// isn't there.
func liveNow(ctx context.Context, tx pgx.Tx, ids []string) (map[string]liveTopicSub, error) {
	out := map[string]liveTopicSub{}
	if len(ids) == 0 {
		return out, nil
	}
	list, err := liveTopicSubs(ctx, tx, `ts.subscription_id = ANY($1::uuid[])`, ids)
	if err != nil {
		return nil, err
	}
	for _, s := range list {
		out[s.ID] = s
	}
	return out, nil
}
