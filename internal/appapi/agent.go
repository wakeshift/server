package appapi

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/apps"
	"github.com/wakeshift/server/internal/grants"
	"github.com/wakeshift/server/internal/httpx"
	"github.com/wakeshift/server/internal/oauth"
	"github.com/wakeshift/server/internal/sessions"
	"github.com/wakeshift/server/internal/streams"
	"github.com/wakeshift/server/internal/subscriptions"
)

// actor is an app acting for an agent, under a grant.
type actor struct {
	app    string
	tenant string
	agent  string
	grant  grants.Grant
}

type actorKey struct{}

func actorFrom(ctx context.Context) actor {
	a, _ := ctx.Value(actorKey{}).(actor)
	return a
}

// requireAgent authenticates an agent token (an app acting for one of its
// agents, from the headless app API) and checks its grant is still live.
func (a *API) requireAgent(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok {
			httpx.WriteError(w, http.StatusUnauthorized, "unauthenticated", "send the app's token for the agent as a Bearer token")
			return
		}
		act, err := a.agentActor(r.Context(), token)
		switch {
		case errors.Is(err, errBadAgentToken):
			httpx.WriteError(w, http.StatusUnauthorized, "unauthenticated", "invalid or expired agent token; get a new one (ActAs)")
			return
		case errors.Is(err, apps.ErrSuspended):
			httpx.WriteError(w, http.StatusForbidden, "suspended", apps.SuspendedMessage(a.SupportEmail))
			return
		case err != nil:
			httpx.WriteError(w, http.StatusForbidden, "no_grant", "the agent's consent for this app was revoked or has expired")
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), actorKey{}, act)))
	}
}

// errBadAgentToken: not an agent token (or an expired one).
var errBadAgentToken = errors.New("invalid agent token")

// agentActor is the app acting for the agent an agent token names,
// under the grant it names: errBadAgentToken if the token doesn't verify,
// grants.ErrNotFound if the grant was revoked or has expired (or isn't the
// app's grant for that agent), apps.ErrSuspended if the app is suspended.
func (a *API) agentActor(ctx context.Context, token string) (actor, error) {
	keys := func(kid string) (ed25519.PublicKey, bool) {
		k, ok := a.Keys(ctx, kid)
		return k, ok
	}
	c, err := oauth.VerifyAccessToken(token, a.Issuer, strings.TrimSuffix(a.Issuer, "/")+"/api/apps", keys, a.now())
	if err != nil || c.Actor == nil || !strings.HasPrefix(c.Actor.Subject, "app:") || c.Grant == "" {
		return actor{}, errBadAgentToken
	}
	act := actor{app: strings.TrimPrefix(c.Actor.Subject, "app:"), tenant: c.Tenant, agent: c.Agent}
	err = a.DB.InTenant(ctx, act.tenant, func(tx pgx.Tx) error {
		g, err := grants.Get(ctx, tx, c.Grant)
		if err != nil {
			return err
		}
		if g.RevokedAt != nil || a.now().After(g.ExpiresAt) || g.AppID != act.app || g.AgentID != act.agent || g.Scope.Agent == nil {
			return grants.ErrNotFound
		}
		// A deleted app's grants stay live until its deletion reaches this
		// workspace (FinishAppDeletions); its tokens stop now. And an app
		// acts only in workspaces it keeps: a grant left in a person's own
		// workspace from account mode (turned off) is no grant. A suspended
		// app's tokens stop at once too, as its keys do: a token minted just
		// before the suspension would otherwise act for its agent until it
		// expires.
		var deleted, owned, suspended bool
		if err := tx.QueryRow(ctx, `SELECT a.deleted_at IS NOT NULL, t.kind = 'app' AND t.app_id IS NOT DISTINCT FROM a.id, a.suspended_at IS NOT NULL
			FROM apps a, tenants t WHERE a.id = $1 AND t.id = current_tenant()`, act.app).Scan(&deleted, &owned, &suspended); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return grants.ErrNotFound
			}
			return err
		}
		if deleted || !owned {
			return grants.ErrNotFound
		}
		if suspended {
			return apps.ErrSuspended
		}
		act.grant = g
		return nil
	})
	return act, err
}

// tx runs fn in the actor's workspace, after re-reading its grant FOR
// SHARE: a revocation (which locks the grant) then waits for this
// transaction, and its cascade sees what fn wrote.
func (a *API) tx(r *http.Request, fn func(ctx context.Context, tx pgx.Tx) error) error {
	ctx := r.Context()
	act := actorFrom(ctx)
	return a.DB.InTenant(ctx, act.tenant, func(tx pgx.Tx) error {
		var live, suspended bool
		err := tx.QueryRow(ctx, `SELECT g.revoked_at IS NULL AND g.expires_at > now() AND a.deleted_at IS NULL, a.suspended_at IS NOT NULL
			FROM grants g JOIN apps a ON a.id = g.app_id WHERE g.id = $1 FOR SHARE OF g`, act.grant.ID).Scan(&live, &suspended)
		if errors.Is(err, pgx.ErrNoRows) || err == nil && !live {
			return errGrantGone
		}
		if err != nil {
			return err
		}
		if suspended { // since requireAgent checked
			return apps.ErrSuspended
		}
		return fn(ctx, tx)
	})
}

var errGrantGone = errors.New("grant revoked")

// serialize takes a transaction-scoped lock on key, for check-then-write
// steps that have no constraint behind them. The lock is the workspace's:
// agents are named per workspace, so one workspace's claude-code never waits
// for another's.
func serialize(ctx context.Context, tx pgx.Tx, key string) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext(coalesce(current_tenant()::text, '') || ':' || $1))`, key)
	return err
}

// errRateLimited: a publish URL posted to more than it may a minute.
var errRateLimited = errors.New("rate limited")

// streamLink gives the agent a link to its deliveries, revoked with the
// grant.
func (a *API) streamLink(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Ack          bool   `json:"ack"`
		Session      string `json:"session"`      // resume a session the agent holds (its handle)
		Label        string `json:"label"`        // or name a new one (agent@label)
		Conversation string `json:"conversation"` // the harness's conversation ID, if known
	}
	if !httpx.Decode(w, r, &in) {
		return
	}
	act := actorFrom(r.Context())
	var u string
	var exp time.Time
	var sess sessions.Session
	err := a.tx(r, func(ctx context.Context, tx pgx.Tx) (err error) {
		// Each link is a session: a participant with its own handle,
		// deliveries, position and presence. Its link rotates in-band.
		sess, err = sessions.Register(ctx, tx, sessions.New{Agent: act.agent, Resume: in.Session, Label: in.Label,
			Conversation: in.Conversation, CreatedBy: "app:" + act.app})
		if errors.Is(err, sessions.ErrNotFound) {
			return errNoSession
		}
		if err != nil {
			return badRequest{err}
		}
		u, exp, err = a.Streams.Mint(ctx, tx, act.tenant, streams.NewLink{Agent: act.agent, Target: sess.ID, Ack: in.Ack,
			CreatedBy: "app:" + act.app, GrantID: act.grant.ID, TTL: streams.SessionLinkTTL})
		return err
	})
	a.respond(w, map[string]string{"url": u, "expires_at": exp.UTC().Format(time.RFC3339), "session": sess.Handle}, err)
}

// listSessions lists the agent's sessions, with presence.
func (a *API) listSessions(w http.ResponseWriter, r *http.Request) {
	act := actorFrom(r.Context())
	var list []sessions.Session
	err := a.tx(r, func(ctx context.Context, tx pgx.Tx) (err error) {
		list, err = sessions.List(ctx, tx, act.agent)
		return err
	})
	for i := range list {
		if !list[i].Joined(act.app) {
			// Another app's conversation: its handle (to mention it) and
			// presence, but not the harness's conversation ID, which would
			// let this app register it as its own.
			list[i].Conversation, list[i].ConnectorSeenAt, list[i].ConnectorClient = "", nil, ""
			continue
		}
		a.describeConnector(&list[i])
	}
	a.respond(w, map[string]any{"sessions": list}, err)
}

// describeConnector says, for a harness conversation's session (a Codex
// thread), what delivers to it and, while nothing is, what the agent should
// tell its user: how to install Wakeshift's connector for Codex, or that it
// restarts by itself. A connector session (one not yet taken over by the
// hooks or a wait) is delivered on the agent's own stream, a link minted in
// the console or by an app (target ""), while one is held.
func (a *API) describeConnector(s *sessions.Session) {
	if s.Conversation == "" {
		return
	}
	// Only a held stream is a connector: a session that polls (a wait tool)
	// is present, but nothing wakes it.
	held := s.ConnectedVia == "stream"
	if s.Runtime == sessions.Connector && held {
		s.Connector = &sessions.ConnectorStatus{Name: "the agent's stream", Running: true}
		return
	}
	c := &sessions.ConnectorStatus{Name: "Wakeshift connector for Codex", Running: s.Runtime == sessions.Stream && held && s.Watching}
	if a.Downloads != nil {
		c.Install = a.Downloads.HarnessInstall("codex")
	}
	switch {
	case c.Running:
	case s.ConnectorSeenAt != nil:
		c.Note = "Wakeshift's connector for this Codex thread isn't running, so new messages won't wake it; you'll see them when you next use this app's tools. " +
			"It starts again by itself on your next call to this app's tools while you're watching."
	case a.Downloads != nil:
		in := a.Downloads.HarnessInstall("codex")
		c.Note = "Nothing delivers Wakeshift messages to this Codex thread yet, so new messages won't wake it; you'll see them when you next use this app's tools. " +
			"Ask the user whether to install Wakeshift's connector for Codex, as a yes/no question that quotes this exact command: `" + in.Command + "`. Only if they say yes, run exactly that command right away. " +
			"Then tell them to quit Codex, start it again with `codex resume --last`, and trust the Wakeshift hooks when Codex asks (\"Trust all and continue\"). If they've already installed it, only that step is needed."
	}
	s.Connector = c
}

// registerSession registers (or finds) the session for a harness
// conversation, e.g. a Codex thread an app saw in a tool call's _meta. A
// connector session is delivered on the agent's own stream (target ""),
// with deliver_to naming the conversation, until the hooks or a wait take
// it over.
func (a *API) registerSession(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Conversation string `json:"conversation"`
		Runtime      string `json:"runtime"` // "connector" (default) or "stream"
	}
	if !httpx.Decode(w, r, &in) {
		return
	}
	if in.Conversation == "" {
		httpx.WriteError(w, http.StatusBadRequest, "bad_request", "conversation is required")
		return
	}
	if in.Runtime == "" {
		in.Runtime = sessions.Connector
	}
	act := actorFrom(r.Context())
	var s sessions.Session
	err := a.tx(r, func(ctx context.Context, tx pgx.Tx) (err error) {
		s, err = sessions.Register(ctx, tx, sessions.New{Agent: act.agent, Conversation: in.Conversation, Runtime: in.Runtime,
			CreatedBy: "app:" + act.app})
		if err != nil {
			return badRequest{err}
		}
		return nil
	})
	a.describeConnector(&s)
	a.respond(w, s, err)
}

// ownSession resolves a handle the acting agent holds, in a session that's
// the acting app's (sessions.Session.Joined): another app's session is no
// session of this app's, so it can't watch, link, subscribe or read it.
func ownSession(ctx context.Context, tx pgx.Tx, act actor, handle string) (sessions.Session, error) {
	s, err := sessions.ByHandle(ctx, tx, strings.TrimPrefix(handle, "@"))
	if errors.Is(err, sessions.ErrNotFound) || err == nil && (s.Agent != act.agent || !s.Joined(act.app)) {
		return sessions.Session{}, errUnknownKey
	}
	return s, err
}

var errNoSession = errors.New("no such session")

// errUnknownKey is errNoSession because of the key the call passed: it
// names no session the caller may use (made up, malformed, ended, another
// workspace's or another app's). A result that would otherwise say nothing
// says so (unknownKeyNote).
var errUnknownKey = fmt.Errorf("%w: the session key passed names none", errNoSession)

// errRoutingEnded: a refresh found its subscription ended (refreshTopicSub).
var errRoutingEnded = errors.New("the subscription ended before it was refreshed")

type badRequest struct{ error }

func (a *API) respond(w http.ResponseWriter, v any, err error) {
	var limit apps.WorkspaceLimitError
	switch {
	case err == nil:
		httpx.WriteJSON(w, http.StatusOK, v)
	case errors.Is(err, errNoSession):
		httpx.WriteError(w, http.StatusNotFound, "unknown_session", "the agent has no session with that handle")
	case errors.As(err, new(badRequest)):
		httpx.WriteError(w, http.StatusBadRequest, "bad_request", err.Error())
	case errors.Is(err, errGrantGone):
		httpx.WriteError(w, http.StatusForbidden, "no_grant", "the agent's consent for this app was revoked or has expired")
	case errors.Is(err, apps.ErrSuspended):
		httpx.WriteError(w, http.StatusForbidden, "suspended", apps.SuspendedMessage(a.SupportEmail))
	case errors.Is(err, errBadAgentToken):
		httpx.WriteError(w, http.StatusForbidden, "bad_agent_token", "call.agent_token isn't this app's token for one of its agents (or it expired); get a new one (ActAs)")
	case errors.Is(err, errRoutingEnded):
		httpx.WriteError(w, http.StatusConflict, "subscription_ended", "the subscription ended while this refreshed it; subscribe again")
	case errors.Is(err, errOtherClient):
		httpx.WriteError(w, http.StatusForbidden, "other_client", "that session key (call.session) is a conversation in another MCP client; call again without it")
	case errors.Is(err, errNoConversation):
		httpx.WriteError(w, http.StatusBadRequest, "unknown_conversation", "nothing identifies this conversation: pass call.session (the handle a subscribe result gave it)")
	case errors.As(err, &limit):
		httpx.WriteError(w, http.StatusForbidden, "workspace_limit", limit.Message(a.SupportEmail))
	case apps.IsWorkspaceRate(err):
		httpx.WriteError(w, http.StatusTooManyRequests, "workspace_rate_limited", err.Error())
	case errors.Is(err, subscriptions.ErrBadType):
		httpx.WriteError(w, http.StatusBadRequest, "bad_request", err.Error())
	default:
		slog.Error("appapi: request failed", "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, "internal", "something went wrong")
	}
}
