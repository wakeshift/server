package appapi

// Event subscriptions by agent token. An app acting for its agents (ActAs:
// Agent Chat) subscribes as the agent the call's agent token names (the
// app's token for one of its workspaces, wakeshift ActAs), in that agent's own
// workspace (one the app keeps). Its subscriber is "<workspace>/<agent>"
// (agentUser), taken from the token, never from the app.
//
// (Account mode, where an app's subscriber was an agent in a person's own
// workspace under the consent they gave on Wakeshift's screen, is turned
// off.)

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/apps"
	"github.com/wakeshift/server/internal/grants"
)

// agentUser is the subscriber of a route made as one of the app's agents:
// the workspace and the agent.
func agentUser(tenant, agent string) string { return tenant + "/" + agent }

// agentConversation is the agent the call's token names, in the workspace
// the app keeps for it. The app may pass call.user, but only as the user
// the token makes it.
func (a *API) agentConversation(ctx context.Context, app apps.App, call callIn) (*conversation, error) {
	call.User, call.Session = strings.TrimSpace(call.User), strings.TrimSpace(call.Session)
	if len(call.Session) > 100 {
		return nil, badRequest{errors.New("call.session is at most 100 characters")}
	}
	act, err := a.agentActor(ctx, call.AgentToken)
	switch {
	case errors.Is(err, grants.ErrNotFound):
		return nil, errGrantGone
	case err != nil:
		return nil, err
	case act.app != app.ID:
		return nil, errBadAgentToken
	}
	user := agentUser(act.tenant, act.agent)
	if call.User != "" && call.User != user {
		return nil, badRequest{errors.New("call.user, if passed, is the agent token's: <workspace>/<agent>")}
	}
	call.User = user
	cv := &conversation{app: app, call: call, tenant: act.tenant, act: act}
	cv.harness, _, cv.recognized, cv.variant, cv.claim = harnessOf(call)
	return cv, nil
}

// userWorkspace is where one of an app's users' event subscriptions live:
// a workspace, and, if set, only those of one agent there, or of one
// principal.
type userWorkspace struct {
	tenant, agent, principal string
}

// userWorkspaces are where an app's user's event subscriptions live: the
// user's own workspace (by external ID), and, for a user of the form
// "<workspace>/<agent>" naming a workspace the app keeps, that agent's
// subscriptions there, made as that user.
func (a *API) userWorkspaces(ctx context.Context, app apps.App, user string) ([]userWorkspace, error) {
	ws, ag, isAgent := strings.Cut(user, "/")
	isAgent = isAgent && uuidRE.MatchString(ws) && ag != ""
	var out []userWorkspace
	var tenant string
	err := a.DB.Pool.QueryRow(ctx, `SELECT id::text FROM tenants WHERE app_id = $1 AND external_id = $2`, app.ID, user).Scan(&tenant)
	switch {
	case err == nil:
		out = append(out, userWorkspace{tenant: tenant})
	case !errors.Is(err, pgx.ErrNoRows):
		return nil, err
	}
	if !isAgent {
		return out, nil
	}
	err = a.DB.Pool.QueryRow(ctx, `SELECT id::text FROM tenants WHERE id = $1::uuid AND app_id = $2`, ws, app.ID).Scan(&tenant)
	switch {
	case err == nil:
		out = append(out, userWorkspace{tenant: tenant, agent: ag, principal: user})
	case !errors.Is(err, pgx.ErrNoRows):
		return nil, err
	}
	return out, nil
}
