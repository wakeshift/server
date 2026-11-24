package appapi

// Harness-aware results for an app acting for one of its agents (ActAs),
// such as Agent Chat: the same per-harness instructions, install offer and
// beacon a subscription's result carries (harnessResult), for the agent's
// own session. A wrapped tool's calls carry the agent's token, so
// /event-subscriptions, /calls/session and /calls/addition (calls.go) use
// what's here: agentCaller, agentSession, useRecordedHarness and
// agentAddition.

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/apps"
	"github.com/wakeshift/server/internal/beacon"
	"github.com/wakeshift/server/internal/harnesses"
	"github.com/wakeshift/server/internal/sessions"
)

// agentCaller is the calling conversation of an app acting for one of its
// agents: the harness its call says (harnessOf, refined by its claim), and
// the conversation the harness names (a Codex thread, Antigravity's or
// Copilot's conversation) or the agent passed (Cursor's). The agent is the
// token's, never the call's.
func agentCaller(app apps.App, act actor, call callIn) *conversation {
	call.AgentToken, call.User, call.Session = "", "", ""
	cv := &conversation{app: app, call: call, tenant: act.tenant, act: act, agentHandles: true}
	cv.harness, _, cv.recognized, cv.variant, cv.claim = harnessOf(call)
	cv.thread = threadOf(cv.harness, call.Meta)
	if cv.thread == "" {
		cv.thread = argConversation(cv.harness, call)
		cv.passedThread = cv.thread != ""
	}
	return cv
}

// agentSession is the calling conversation's session: the session of the
// conversation its harness names (or its agent passed), which wins over a
// handle passed; else the handle passed; else, with create, the session
// labeled label, or a new one. A conversation no session has yet is
// adopted by the session the agent named (by handle, or with create by
// label) if that session has no conversation yet: it watched before its
// harness named the conversation (Copilot before Wakeshift's hook was
// installed, which an agent whose user ran the installer may never watch
// again after, so annotating adopts too). Otherwise, with create, it gets a
// session of its own. Without create, nothing is made: errNoSession if
// there's no session to find.
func agentSession(ctx context.Context, tx pgx.Tx, cv *conversation, handle, label string, create bool) (sessions.Session, error) {
	act := cv.act
	createdBy := "app:" + act.app
	var named *sessions.Session
	if handle != "" {
		s, err := ownSession(ctx, tx, act, handle)
		if err != nil {
			return s, err
		}
		named = &s
	}
	if cv.thread != "" {
		s, err := sessions.ByConversation(ctx, tx, act.agent, cv.thread)
		switch {
		case err == nil && create:
			// Made the app's own if it isn't yet: the harness named the
			// conversation, so the app is in it.
			return registered(sessions.Register(ctx, tx, sessions.New{Agent: act.agent, Conversation: cv.thread, Runtime: sessions.Connector, CreatedBy: createdBy}))
		case err == nil && s.Joined(act.app):
			return s, nil
		case err == nil:
			return sessions.Session{}, errNoSession
		case !errors.Is(err, sessions.ErrNotFound):
			return s, err
		}
		if named == nil && label != "" && create {
			s, err := registered(sessions.Register(ctx, tx, sessions.New{Agent: act.agent, Label: label, CreatedBy: createdBy}))
			if err != nil {
				return s, err
			}
			named = &s
		}
		if named != nil && named.Conversation == "" {
			tag, err := tx.Exec(ctx, `UPDATE sessions SET conversation = $2 WHERE id = $1 AND conversation IS NULL`, named.ID, cv.thread)
			if err != nil {
				return *named, err
			}
			if tag.RowsAffected() == 1 {
				named.Conversation = cv.thread
				return *named, nil
			}
		}
		if !create {
			return sessions.Session{}, errNoSession
		}
		return registered(sessions.Register(ctx, tx, sessions.New{Agent: act.agent, Conversation: cv.thread, Runtime: sessions.Connector, CreatedBy: createdBy}))
	}
	switch {
	case named != nil && create:
		return *named, sessions.Touch(ctx, tx, named.ID)
	case named != nil:
		return *named, nil
	case !create:
		return sessions.Session{}, errNoSession
	}
	return registered(sessions.Register(ctx, tx, sessions.New{Agent: act.agent, Label: label, CreatedBy: createdBy}))
}

// registered is sessions.Register's result, with its errors as the API's.
func registered(s sessions.Session, err error) (sessions.Session, error) {
	switch {
	case errors.Is(err, sessions.ErrNotFound):
		return s, errNoSession
	case err != nil && (errors.Is(err, sessions.ErrLabelTaken) || strings.HasPrefix(err.Error(), "session")):
		return s, badRequest{err}
	}
	return s, err
}

// splitTrailer splits a result's text into what the model reads and the
// beacon line that ends it, if there is one (withBeacon puts it last).
// The SDKs add the trailer as a text item of its own, after the app's
// content and Wakeshift's text. It starts with a newline: Copilot CLI hands
// its hooks a result's text items joined with nothing between them, and a
// hook accepts a beacon only on a line of its own (beacon.Trailer).
func splitTrailer(text string) (note, trailer string) {
	note = beacon.StripText(text)
	if beacon.Trailer(text) != "" {
		trailer = "\n" + strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(text), note))
	}
	return strings.TrimSpace(note), trailer
}

// useRecordedHarness gives a call nothing recognizes (a stateless server
// sees no client name, and some harnesses send no _meta or User-Agent of
// their own: OpenClaw's own runtime, Hermes Agent) the harness its agent
// said at watch (settleHarness recorded it), for a later result's
// addition. A hint for where the beacon goes, as the claim was: it grants
// nothing.
func useRecordedHarness(ctx context.Context, tx pgx.Tx, cv *conversation) error {
	if cv.harness != "" {
		return nil
	}
	var recorded *string
	if err := tx.QueryRow(ctx, `SELECT harness FROM sessions WHERE id = $1`, cv.session.ID).Scan(&recorded); err != nil {
		return err
	}
	if recorded != nil {
		if h, ok := harnesses.Get(*recorded); ok && h.Family == "" {
			cv.harness = h.ID
		}
	}
	return nil
}

// agentAddition is what /calls/addition adds for an agent's conversation
// that subscribed to nothing in the call (annotateAddition): no
// instructions (a result never asks for another call, and a listener that
// stopped isn't restarted from an unrelated result; the app's pull path
// shows what's waiting), only the beacon, if its hooks need one.
func (a *API) agentAddition(ctx context.Context, tx pgx.Tx, cv *conversation) (toolResult, error) {
	var out toolResult
	s := cv.session
	h, ok := harnesses.Get(cv.harness)
	if !ok || !s.Watching || !h.Installable {
		return out, nil
	}
	if cv.harness == harnessCodex {
		if s.Runtime == sessions.Stream && s.Connected {
			return out, nil // its connector runs
		}
	} else if hooksLinked(s) {
		return out, nil
	}
	if recent, err := recentTicket(ctx, tx, cv); err != nil || recent {
		return out, err
	}
	return out, a.withBeacon(ctx, tx, cv, h, &out)
}
