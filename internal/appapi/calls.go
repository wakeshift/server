package appapi

// One block per tool call: the pending addition. An SDK's wrapped tool
// subscribes with defer_addition, so no subscribe writes instructions or
// mints a beacon; the call's first request that names a session makes a
// pending-addition record (its first: had the conversation subscribed
// before?), later subscribes in the call update it (armed, their notes),
// and at the end the wrapper asks /calls/addition once for the call's one
// block: what the conversation's harness needs to be woken (one beacon),
// and one line per event a native client should subscribe to itself.
//
//	POST /api/apps/v1/calls/session   {call, label?}                → {session, pending, agent}: session null when unfindable
//	POST /api/apps/v1/calls/addition  {call, pending?, native?}     → {text, trailer, meta, agent}
//
// The record is the app's and the session's: /calls/addition mints only
// for an unused, unexpired record of the same app, whose session the call
// names. A call with an agent token (call.agent_token, wakeshift ActAs) is an
// app acting for one of its agents: its session is found or started by
// agentSession.

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/apps"
	"github.com/wakeshift/server/internal/grants"
	"github.com/wakeshift/server/internal/harnesses"
	"github.com/wakeshift/server/internal/httpx"
	"github.com/wakeshift/server/internal/sessions"
)

// pendingTTL is how long a pending-addition record can be read: a tool
// call's length, with room.
const pendingTTL = 10 * time.Minute

// Modes of a pending-addition record.
const (
	modeTopic = "topic" // the app's user's conversation (findConversation)
	modeAgent = "agent" // an app acting for one of its agents (agent token)
)

var pendingRE = regexp.MustCompile(`^pa_[A-Za-z0-9_-]{22}$`)

// errBadPending: the pending record named isn't this call's (another app's
// or session's, used, expired, or made up).
var errBadPending = badRequest{errors.New("pending isn't a record of this call (it was used, expired, or names another conversation): start the call's requests again without it")}

// pendingRecord is a pending-addition record.
type pendingRecord struct {
	ID, App, Session, Agent, Mode string
	First, Armed                  bool
	Notes                         []string
}

// newPending makes the call's pending-addition record for cv's session,
// computing first before the call subscribes to anything: no earlier topic
// subscription of the session.
func newPending(ctx context.Context, tx pgx.Tx, cv *conversation, mode string) (pendingRecord, error) {
	var told bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM topic_subscriptions WHERE session_id = $1)`,
		cv.session.ID).Scan(&told); err != nil {
		return pendingRecord{}, err
	}
	b := make([]byte, 16)
	rand.Read(b)
	p := pendingRecord{ID: "pa_" + base64.RawURLEncoding.EncodeToString(b), App: cv.app.ID, Session: cv.session.ID, Agent: cv.act.agent, Mode: mode, First: !told}
	if _, err := tx.Exec(ctx, `INSERT INTO pending_additions (tenant_id, id, app_id, session_id, agent_id, mode, first, expires_at)
		VALUES (current_tenant(), $1, $2, $3, $4, $5, $6, now() + make_interval(secs => $7))`,
		p.ID, p.App, p.Session, p.Agent, p.Mode, p.First, pendingTTL.Seconds()); err != nil {
		return p, err
	}
	_, err := tx.Exec(ctx, `INSERT INTO pending_addition_ids (id, app_id, tenant_id) VALUES ($1, $2, current_tenant())`, p.ID, p.App)
	return p, err
}

// loadPending is the transaction's workspace's unused, unexpired record id
// of app, locked (errBadPending if there's none).
func loadPending(ctx context.Context, tx pgx.Tx, app, id string) (pendingRecord, error) {
	p := pendingRecord{ID: id}
	if !pendingRE.MatchString(id) {
		return p, errBadPending
	}
	err := tx.QueryRow(ctx, `SELECT app_id, session_id::text, agent_id, mode, first, armed, notes FROM pending_additions
		WHERE id = $1 AND app_id = $2 AND used_at IS NULL AND expires_at > now() FOR UPDATE`, id, app).
		Scan(&p.App, &p.Session, &p.Agent, &p.Mode, &p.First, &p.Armed, &p.Notes)
	if errors.Is(err, pgx.ErrNoRows) {
		return p, errBadPending
	}
	return p, err
}

// pendingFor is the call's record for cv's session: the one named, which
// must be this session's, or a new one.
func pendingFor(ctx context.Context, tx pgx.Tx, cv *conversation, id, mode string) (pendingRecord, error) {
	if id == "" {
		return newPending(ctx, tx, cv, mode)
	}
	p, err := loadPending(ctx, tx, cv.app.ID, id)
	if err == nil && (p.Session != cv.session.ID || p.Mode != mode) {
		err = errBadPending
	}
	return p, err
}

// armPending records that a subscribe in the call made or renewed a
// subscription, with what its result would have said first.
func armPending(ctx context.Context, tx pgx.Tx, id string, notes ...string) error {
	var add []string
	for _, n := range notes {
		if n != "" {
			add = append(add, n)
		}
	}
	if add == nil {
		add = []string{}
	}
	_, err := tx.Exec(ctx, `UPDATE pending_additions SET armed = true, notes = notes || $2::text[] WHERE id = $1`, id, add)
	return err
}

// PrunePendingAdditions deletes the transaction's workspace's
// pending-addition records an hour past their expiry, with their IDs, for
// the expiry pass: batch at a time, at most expiryBatches batches, so a
// backlog drains over passes instead of failing every pass on the job's
// statement timeout.
func PrunePendingAdditions(ctx context.Context, tx pgx.Tx, batch int) (int64, error) {
	var n int64
	for range expiryBatches {
		rows, _ := tx.Query(ctx, `DELETE FROM pending_additions WHERE (tenant_id, id) IN (
			SELECT tenant_id, id FROM pending_additions WHERE expires_at < now() - interval '1 hour' LIMIT $1 FOR UPDATE SKIP LOCKED) RETURNING id`, batch)
		ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil || len(ids) == 0 {
			return n, err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM pending_addition_ids WHERE tenant_id = current_tenant() AND id = ANY($1)`, ids); err != nil {
			return n, err
		}
		n += int64(len(ids))
		if len(ids) < batch {
			break
		}
	}
	return n, nil
}

// Notes a deferred subscribe adds to the call's block.
const (
	onceNote = "This subscription ends after its first message."
)

// notesLast puts a call's notes (onceNote, staleNote) after the block's
// text, before its beacon trailer if it has one (which stays last): what
// the agent should do comes first. Codex quoted a block that opened with
// onceNote as if that line were all of it, and asked nothing (vendor
// bench, November 6, 2026).
func notesLast(text string, notes ...string) string {
	n := join(notes...)
	if n == "" {
		return text
	}
	body, trailer := splitTrailer(text)
	if trailer != "" {
		trailer = "\n" + trailer // a blank line before it, as withBeacon writes it
	}
	return strings.TrimSpace(body+"\n\n"+n) + trailer
}

// staleNote is what a subscribe that started a new session, because the
// handle passed names nothing, says first.
func staleNote() string {
	return "The " + harnesses.SessionArg + " you passed isn't known (it may have ended), so this conversation has a new one."
}

// unknownKeyNote is what a result that would otherwise say nothing says
// when the key the call passed names no conversation (errUnknownKey):
// nothing was found by it, and nothing was done for it.
func unknownKeyNote() string {
	return "The " + harnesses.SessionArg + " you passed isn't known (it may have ended)."
}

// agentCall is the calling conversation of an app acting for one of its
// agents, from its agent token (agentConversation checks it), with the
// handle it passed. Its user is the token's, <workspace>/<agent>.
func (a *API) agentCall(ctx context.Context, app apps.App, call callIn) (*conversation, string, error) {
	base, err := a.agentConversation(ctx, app, call)
	if err != nil {
		return nil, "", err
	}
	handle := strings.TrimSpace(call.Session)
	cv := agentCaller(app, base.act, call)
	cv.call.User = base.call.User
	return cv, handle, nil
}

// holdGrant reads cv's grant again, FOR SHARE, as tx does for agent calls:
// a revocation then waits for this transaction.
func holdGrant(ctx context.Context, tx pgx.Tx, cv *conversation) error {
	var live bool
	err := tx.QueryRow(ctx, `SELECT g.revoked_at IS NULL AND g.expires_at > now() AND a.deleted_at IS NULL
		FROM grants g JOIN apps a ON a.id = g.app_id WHERE g.id = $1 FOR SHARE OF g`, cv.act.grant.ID).Scan(&live)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && !live {
		return errGrantGone
	}
	return err
}

// agentResolve finds or starts an agent call's session (agentSession) in
// the transaction, holding its grant.
func agentResolve(ctx context.Context, tx pgx.Tx, cv *conversation, handle, label string, create bool) error {
	if err := holdGrant(ctx, tx, cv); err != nil {
		return err
	}
	s, err := agentSession(ctx, tx, cv, handle, label, create)
	if err != nil {
		return err
	}
	cv.session, cv.handle = s, s.Handle
	return settleHarness(ctx, tx, cv)
}

// callsSession finds or starts the calling conversation's session and makes
// the call's pending-addition record, without subscribing it to anything or
// marking it watching (wakeshift ToolCall.ResolveSession). For a call whose
// conversation can't be found by what it gives (unfindable) the session is
// null, with no record.
func (a *API) callsSession(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Call  callIn `json:"call"`
		Label string `json:"label"`
	}
	if !httpx.Decode(w, r, &in) {
		return
	}
	app := AppFrom(r.Context())
	var out struct {
		Session *string    `json:"session"`
		Pending string     `json:"pending"`
		Agent   recognized `json:"agent"`
	}
	ctx := r.Context()
	var cv *conversation
	var handle string
	var err error
	mode := modeTopic
	if in.Call.AgentToken == "" && unfindable(in.Call) {
		// Its conversation is one Wakeshift can't find by what the call
		// gives: session null, and nothing made.
		_, _, out.Agent, _, _ = harnessOf(in.Call)
		a.respond(w, out, nil)
		return
	}
	if in.Call.AgentToken != "" {
		mode = modeAgent
		cv, handle, err = a.agentCall(ctx, app, in.Call)
	} else {
		cv, err = a.findConversation(ctx, app, in.Call, true)
	}
	if err == nil {
		err = a.DB.InTenant(ctx, cv.tenant, func(tx pgx.Tx) error {
			if mode == modeAgent {
				if err := agentResolve(ctx, tx, cv, handle, in.Label, true); err != nil {
					return err
				}
			} else if err := a.resolve(ctx, tx, cv, true); err != nil {
				return err
			}
			p, err := newPending(ctx, tx, cv, mode)
			if err != nil {
				return err
			}
			if cv.staleHandle {
				if err := armPendingNotes(ctx, tx, p.ID, staleNote()); err != nil {
					return err
				}
			}
			out.Session, out.Pending, out.Agent = &cv.handle, p.ID, cv.recognized
			return nil
		})
	}
	a.respond(w, out, err)
}

// unfindable: the call names its conversation (a Codex thread, a
// conversation ID in its _meta or passed by its agent) but gives neither a
// user nor a session key. Without a user, a conversation's own workspace is
// found only by the key Wakeshift handed it (findConversation): the ID its
// harness reports goes to every MCP server the user has, so it never finds
// a session. Finding or starting would start a new conversation alongside
// the one the call is in, and a session for it that no subscribe of that
// conversation uses, so /calls/session answers none instead (an SDK's
// resolveSession returns empty). A call that names nothing is a new
// conversation, as an SDK from before 0.11 asks for one.
func unfindable(call callIn) bool {
	if strings.TrimSpace(call.User) != "" || strings.TrimSpace(call.Session) != "" {
		return false
	}
	h, _, _, _, _ := harnessOf(call)
	return threadOf(h, call.Meta) != "" || argConversation(h, call) != ""
}

// armPendingNotes adds notes to a record without arming it.
func armPendingNotes(ctx context.Context, tx pgx.Tx, id string, notes ...string) error {
	_, err := tx.Exec(ctx, `UPDATE pending_additions SET notes = notes || $2::text[] WHERE id = $1`, id, notes)
	return err
}

// nativeLine is one event and arguments a native client should subscribe
// to itself (its own MCP Events subscription), for /calls/addition.
type nativeLine struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
	// call.subscribe's options, kept on a tools-only event's voucher and
	// applied when it's redeemed (vouchers.go).
	Once   bool      `json:"once"`
	TTL    *duration `json:"ttl"`
	Replay *duration `json:"replay"`
}

// opencodeWatchNote follows the native lines for opencode: it names
// how opencode subscribes, which the generic line doesn't. Without it, a
// model read "subscribe to it (MCP Events)" as done by the app's own tool
// and told the user it was watching, with nothing that would wake it. Even
// with the tool named, one run in five still did, so the note says this
// call didn't subscribe.
//
// opencode 2.4.0 has no watch_mcp_event tool: an event watch is an
// automation (automation.create with a trigger of type "event"), whose
// action runs in the session when the event comes. The 2.3 flag build had
// watch_mcp_event. One note serves both: the core can't tell them apart
// (2.4.0's User-Agent, "opencode/latest/2.4.0/cli", isn't a product/version
// token, and the SDKs don't pass clientInfo.version). With the old note,
// 2.4.0's models looked for watch_mcp_event, failed, and found
// automation.create themselves (wakebench push lane, 2026-11-23). The note
// says nothing about the session key: the voucher in the arguments is
// what the subscription needs.
const opencodeWatchNote = `In opencode, that's automation.create with an event trigger ({"type": "event", "server": this MCP server's name, "event": the event's name, "args": those arguments}) and an action for when it fires; before opencode 2.4, the watch_mcp_event tool, with the same server, event and arguments. This tool call didn't subscribe you: nothing wakes this conversation until you subscribe there.`

// maxNativeLines is the most native lines one call asks for.
const maxNativeLines = 20

// callsAddition is the call's one block: native lines first, then, with a
// pending record that a subscribe armed, what subscribing gives the
// conversation's harness (the full instructions the first time, the short
// form after; one beacon), with the record's notes before it; otherwise
// what any other tool result of the app's carries (annotateAddition). The
// record is used once. With native lines for a conversation the push line
// wakes (nativePush: opencode without its plugin), the subscribing part
// offers no install.
func (a *API) callsAddition(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Call    callIn       `json:"call"`
		Pending string       `json:"pending"`
		Native  []nativeLine `json:"native"`
	}
	if !httpx.Decode(w, r, &in) {
		return
	}
	app := AppFrom(r.Context())
	ctx := r.Context()
	var out struct {
		Text    string         `json:"text"`
		Trailer string         `json:"trailer,omitempty"`
		Meta    map[string]any `json:"meta,omitempty"`
		Agent   recognized     `json:"agent"`
		// Requests: a client request for the SDK to run (clientrequests.go),
		// each outcome's text in place of Text.
		Requests []clientRequest `json:"requests,omitempty"`
	}
	_, out.Agent, _ = recognize(in.Call)
	if len(in.Native) > maxNativeLines {
		a.respond(w, nil, badRequest{errors.New("at most 20 native lines in one call")})
		return
	}
	lines, err := a.nativeLines(ctx, app, in.Call, in.Native)
	if err != nil {
		a.respondEventSub(w, nil, err)
		return
	}
	if h, _, _, _, _ := harnessOf(in.Call); len(lines) > 0 && a.Push && h == harnessOpencode {
		lines = append(lines, opencodeWatchNote)
	}
	var res toolResult
	var rec recognized
	var found bool
	if in.Pending != "" {
		res, rec, err = a.pendingAddition(ctx, app, in.Call, in.Pending, len(lines) > 0)
		found = err == nil
	} else {
		res, rec, found, err = a.annotateAddition(ctx, app, in.Call)
	}
	if err != nil {
		a.respond(w, nil, err)
		return
	}
	if found {
		out.Agent = rec
	}
	note, trailer := splitTrailer(res.Text)
	out.Text = strings.TrimSpace(strings.Join(lines, " ") + "\n\n" + note)
	out.Trailer, out.Meta = trailer, res.Meta
	for _, req := range res.Requests {
		for k, o := range req.Outcomes {
			o.Text = strings.TrimSpace(strings.Join(lines, " ") + "\n\n" + o.Text)
			req.Outcomes[k] = o
		}
		out.Requests = append(out.Requests, req)
	}
	a.respond(w, out, nil)
}

// pendingAddition is the block a pending record makes, for the call that
// names its session; the record is marked used in the same transaction.
// native: the block carries native lines, so a subscribe's part for a
// conversation the push line wakes (nativePush) offers no install.
func (a *API) pendingAddition(ctx context.Context, app apps.App, call callIn, id string, native bool) (toolResult, recognized, error) {
	if !pendingRE.MatchString(id) {
		return toolResult{}, recognized{}, errBadPending
	}
	var tenant string
	err := a.DB.Pool.QueryRow(ctx, `SELECT tenant_id::text FROM pending_addition_ids WHERE id = $1 AND app_id = $2`, id, app.ID).Scan(&tenant)
	if errors.Is(err, pgx.ErrNoRows) {
		return toolResult{}, recognized{}, errBadPending
	}
	if err != nil {
		return toolResult{}, recognized{}, err
	}
	// The call must name the record's conversation: its workspace, from its
	// own identity (the app's user and session key, or its agent token).
	var cv *conversation
	var handle string
	agentMode := call.AgentToken != ""
	if agentMode {
		cv, handle, err = a.agentCall(ctx, app, call)
	} else {
		cv, err = a.findConversation(ctx, app, call, false)
	}
	switch {
	case errors.Is(err, errNoConversation) || errors.Is(err, errNoSession):
		return toolResult{}, recognized{}, errBadPending
	case err != nil:
		return toolResult{}, recognized{}, err
	case cv.tenant != tenant:
		return toolResult{}, recognized{}, errBadPending
	}
	var res toolResult
	err = a.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		p, err := loadPending(ctx, tx, app.ID, id)
		if err != nil {
			return err
		}
		if p.Agent != cv.act.agent || (p.Mode == modeAgent) != agentMode {
			return errBadPending
		}
		// The call must name the record's own session, not another
		// conversation of the same agent: found as the call's subscribe
		// found it, without starting one.
		if own, err := a.callSessionID(ctx, tx, cv, handle, agentMode); err != nil {
			return err
		} else if own != p.Session {
			return errBadPending
		}
		s, err := sessions.Get(ctx, tx, p.Session)
		if errors.Is(err, sessions.ErrNotFound) {
			return errBadPending
		}
		if err != nil {
			return err
		}
		if s.Agent != cv.act.agent || !s.Joined(app.ID) {
			return errBadPending
		}
		if agentMode {
			if err := holdGrant(ctx, tx, cv); err != nil {
				return err
			}
			cv.handle = s.Handle
		} else {
			g, err := grants.Live(ctx, tx, app.ID, cv.act.agent)
			if errors.Is(err, grants.ErrNotFound) {
				return errGrantGone
			}
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `SELECT 1 FROM grants WHERE id = $1 FOR SHARE`, g.ID); err != nil {
				return err
			}
			cv.act.grant = g
			if cv.handle == "" || !strings.HasPrefix(cv.external, conversationPrefix) {
				cv.handle = handleOf(s)
			}
		}
		cv.session = s
		if agentMode && !p.Armed {
			// As annotateAddition: a later result goes by the harness the
			// agent said when it subscribed, when nothing recognizes the
			// call (useRecordedClaim for the app's own users).
			if err := useRecordedHarness(ctx, tx, cv); err != nil {
				return err
			}
		} else if !p.Armed {
			if err := useRecordedClaim(ctx, tx, cv); err != nil {
				return err
			}
		}
		if err := settleHarness(ctx, tx, cv); err != nil {
			return err
		}
		if p.Armed {
			told := !p.First
			cv.told = &told
			if native && !agentMode {
				// Decided from the conversation's state now, as the call's
				// subscribe decided native_line.
				if cv.pushLine, err = a.nativePush(ctx, tx, cv); err != nil {
					return err
				}
			}
			res, err = a.harnessResult(ctx, tx, cv, forSubscribe)
		} else if agentMode {
			res, err = a.agentAddition(ctx, tx, cv)
		} else {
			res, err = a.harnessResult(ctx, tx, cv, forAnnotate)
		}
		if err != nil {
			return err
		}
		res.Text = notesLast(res.Text, p.Notes...)
		_, err = tx.Exec(ctx, `UPDATE pending_additions SET used_at = now() WHERE id = $1`, id)
		return err
	})
	return res, cv.recognized, err
}

// callSessionID is the ID of the session a /calls/addition call names (its
// harness's conversation, its session key or handle), found as its
// subscribe found it but without starting one, on a copy of cv;
// errBadPending if it names none. What finding it writes (a session
// learning the conversation ID the call names, as the call's subscribe
// would) is in tx, so a refusal rolls it back.
func (a *API) callSessionID(ctx context.Context, tx pgx.Tx, cv *conversation, handle string, agentMode bool) (string, error) {
	probe := *cv
	var s sessions.Session
	var err error
	if agentMode {
		s, err = agentSession(ctx, tx, &probe, handle, "", false)
	} else if err = a.resolve(ctx, tx, &probe, false); err == nil {
		s = probe.session
	}
	switch {
	case errors.Is(err, errNoSession) || errors.Is(err, errNoConversation) || errors.Is(err, sessions.ErrNotFound):
		return "", errBadPending
	case err != nil:
		return "", err
	}
	return s.ID, nil
}

// annotateAddition is what a wrapped tool that subscribed to nothing adds
// for the calling conversation: harnessResult's for any other result, or,
// for an app acting for one of its agents, agentAddition's (nothing for a
// session that isn't watching, so nothing re-arms one after unwatch).
// Nothing is made: a call that names no conversation gets nothing (found
// false). Cursor's and Grok Bot's calls name theirs by session key or
// conversation_id: one of a user's that passes neither is never given the
// user's most recent conversation of that client, which may be another
// chat's. The harness, though, the session's record gives
// (useRecordedClaim), so an agent needs to say it only when it subscribes.
func (a *API) annotateAddition(ctx context.Context, app apps.App, call callIn) (toolResult, recognized, bool, error) {
	var res toolResult
	if call.AgentToken != "" {
		cv, handle, err := a.agentCall(ctx, app, call)
		if err != nil {
			return res, recognized{}, false, err
		}
		found := false
		err = a.DB.InTenant(ctx, cv.tenant, func(tx pgx.Tx) error {
			if err := holdGrant(ctx, tx, cv); err != nil {
				return err
			}
			s, err := agentSession(ctx, tx, cv, handle, "", false)
			if errors.Is(err, errUnknownKey) {
				res.Text = unknownKeyNote()
				return nil
			}
			if errors.Is(err, errNoSession) {
				return nil
			}
			if err != nil {
				return err
			}
			found = true
			cv.session = s
			if err := useRecordedHarness(ctx, tx, cv); err != nil {
				return err
			}
			if err := settleHarness(ctx, tx, cv); err != nil {
				return err
			}
			res, err = a.agentAddition(ctx, tx, cv)
			return err
		})
		return res, cv.recognized, found, err
	}
	cv, err := a.findConversation(ctx, app, call, false)
	if err != nil {
		if errors.Is(err, errUnknownKey) {
			res.Text = unknownKeyNote()
		}
		if errors.Is(err, errNoConversation) || errors.Is(err, errNoSession) {
			err = nil
		}
		return res, recognized{}, false, err
	}
	err = a.DB.InTenant(ctx, cv.tenant, func(tx pgx.Tx) error {
		if err := a.resolve(ctx, tx, cv, false); err != nil {
			return err
		}
		if err := useRecordedClaim(ctx, tx, cv); err != nil {
			return err
		}
		var err error
		res, err = a.harnessResult(ctx, tx, cv, forAnnotate)
		return err
	})
	if errors.Is(err, errUnknownKey) {
		return toolResult{Text: unknownKeyNote()}, cv.recognized, false, nil
	}
	if errors.Is(err, errNoConversation) || errors.Is(err, errNoSession) || errors.Is(err, errOtherClient) {
		return toolResult{}, cv.recognized, false, nil
	}
	return res, cv.recognized, err == nil, err
}

// useRecordedClaim is useRecordedHarness for a call of the app's own
// users: a call whose agent claims no harness, and that nothing but the
// claim would name (no client name, _meta or User-Agent of its own, or
// only a client name others send too: Hermes Agent's "mcp"), is given the
// harness its session recorded (an accepted claim of an earlier call:
// settleHarness), as if it had claimed it again. So an agent says its
// harness on the call that subscribes, and a later result of a tool that
// subscribes to nothing still takes its harness's path (the
// background-command reminder, the beacon where its hooks read it).
//
// It's found only once resolve has found the session by the call's own
// key or conversation, in its own workspace, for its own agent: the record
// is that conversation's own, and the claim it stands in for never changes
// the agent the call acts as (harnessOf), so the call gets nothing that
// passing the claim itself wouldn't. A call that claims something, or that
// its client name, _meta or User-Agent already name, keeps what it says.
func useRecordedClaim(ctx context.Context, tx pgx.Tx, cv *conversation) error {
	if harnesses.ClaimName(cv.call.Harness) != "" {
		return nil
	}
	var recorded *string
	if err := tx.QueryRow(ctx, `SELECT harness FROM sessions WHERE id = $1`, cv.session.ID).Scan(&recorded); err != nil {
		return err
	}
	if recorded == nil {
		return nil
	}
	call := cv.call
	call.Harness = *recorded
	h, agent, rec, variant, outcome := harnessOf(call)
	if outcome != harnesses.Decided || agent != cv.act.agent {
		return nil
	}
	rec.Claimed = nil // the agent claimed nothing in this call
	cv.harness, cv.recognized, cv.variant, cv.claim = h, rec, variant, outcome
	return settleHarness(ctx, tx, cv)
}

// nativeLines are the sentences a native client is given for the events it
// should subscribe to itself: "subscribe to …", or, when its user has a
// live webhook or poll subscription to the event with those arguments,
// that it has.
// A tools-only event's line hands out a subscribe voucher (voucherLine).
func (a *API) nativeLines(ctx context.Context, app apps.App, call callIn, list []nativeLine) ([]string, error) {
	if len(list) == 0 {
		return nil, nil
	}
	user := strings.TrimSpace(call.User)
	if call.AgentToken != "" {
		cv, err := a.agentConversation(ctx, app, call)
		if err != nil {
			return nil, err
		}
		user = cv.call.User
	}
	var where []userWorkspace
	if appUser(user) {
		var err error
		if where, err = a.userWorkspaces(ctx, app, user); err != nil {
			return nil, err
		}
	}
	var out []string
	for _, n := range list {
		et, err := app.Manifest.EventType(n.Name)
		if err != nil {
			return nil, err
		}
		if et.ToolsOnly() {
			// Only through a voucher (vouchers.go), which never fails the
			// call: no line if none can be handed out.
			if line := a.voucherLine(ctx, app, call, user, where, et, n); line != "" {
				out = append(out, line)
			}
			continue
		}
		key, err := apps.ArgumentsKey(n.Arguments)
		if err != nil {
			return nil, err
		}
		canon := "{}"
		if len(n.Arguments) > 0 && string(n.Arguments) != "null" {
			c, err := apps.Canonical(n.Arguments)
			if err != nil {
				return nil, apps.EventArgsError{Msg: err.Error()}
			}
			canon = string(c)
		}
		// Not a push subscription: it reaches only the stream that holds
		// it, which may be another conversation's of the same user and
		// agent, or none (opencode's watch may end after one event: the
		// model ends its automation, or the watch_mcp_event of the 2.3 flag
		// build does; and the subscription outlives it by PushLease). A
		// client that pushes subscribes again each time; opencode admits a
		// repeated event once.
		live := false
		for _, uw := range where {
			if live {
				break
			}
			if err := a.DB.InTenant(ctx, uw.tenant, func(tx pgx.Tx) error {
				return tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM event_subscriptions es WHERE es.source_kind = $1 AND es.source_id = $2
					AND es.event = $3 AND es.arguments_key = $4 AND es.destination IN ('webhook', 'poll') AND es.ended_at IS NULL AND es.principal = $5
					AND ($6 = '' OR es.agent_id = $6) AND (es.destination = 'poll' OR NOT `+webhookGone+`))`, sourceApp, app.ID, n.Name, key, user, uw.agent).Scan(&live)
			}); err != nil {
				return nil, err
			}
		}
		if live {
			out = append(out, "You're subscribed to "+n.Name+" with "+canon+": its events reach you.")
		} else {
			out = append(out, "To be woken by "+n.Name+" with "+canon+", subscribe to it (MCP Events).")
		}
	}
	return out, nil
}
