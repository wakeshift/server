package oauth

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/agents"
	"github.com/wakeshift/server/internal/apps"
	"github.com/wakeshift/server/internal/grants"
	"github.com/wakeshift/server/internal/httpx"
)

// Headless apps: the app identifies its own users, and Wakeshift keeps a
// workspace for each, owned by the app. The app creates the workspace and
// the agent, which gets its grant at once, and then acts
// for the agent with a token from here.
// There's no Wakeshift screen: adding the app to the agent is the user's
// consent, as it would be for an app that delivered its own events.

func (s *Server) registerHeadless(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/apps/v1/workspaces", s.requireHeadlessApp(s.createWorkspace))
	mux.HandleFunc("POST /api/apps/v1/workspaces/{workspace}/agents", s.requireHeadlessApp(s.addAgent))
	mux.HandleFunc("POST /api/apps/v1/workspaces/{workspace}/agents/{agent}/token", s.requireHeadlessApp(s.agentToken))
}

type headlessAppKey struct{}

func (s *Server) requireHeadlessApp(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok {
			httpx.WriteError(w, http.StatusUnauthorized, "unauthenticated", "send the app's signed assertion as a Bearer token")
			return
		}
		app, err := s.Apps.Authenticate(r.Context(), token, s.now())
		if errors.Is(err, apps.ErrUnauthenticated) {
			httpx.WriteError(w, http.StatusUnauthorized, "unauthenticated", err.Error())
			return
		}
		if errors.Is(err, apps.ErrSuspended) {
			httpx.WriteError(w, http.StatusForbidden, "suspended", apps.SuspendedMessage(s.SupportEmail))
			return
		}
		if errors.Is(err, apps.ErrWaitlisted) {
			// A key held on the waitlist (apps/pending.go): only its holder
			// gets here, past the signature check.
			httpx.WriteError(w, http.StatusForbidden, "waitlisted", err.Error())
			return
		}
		if err != nil {
			slog.Error("oauth: authenticating an app", "err", err)
			httpx.WriteError(w, http.StatusInternalServerError, "internal", "try again")
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), headlessAppKey{}, app)))
	}
}

func headlessApp(ctx context.Context) apps.App {
	app, _ := ctx.Value(headlessAppKey{}).(apps.App)
	return app
}

// createWorkspace returns the workspace the app keeps for one of its users
// (external_id is the app's own ID for them), creating it on first use.
func (s *Server) createWorkspace(w http.ResponseWriter, r *http.Request) {
	app := headlessApp(r.Context())
	var in struct {
		ExternalID string `json:"external_id"`
		Name       string `json:"name"`
	}
	if !httpx.Decode(w, r, &in) {
		return
	}
	in.ExternalID, in.Name = strings.TrimSpace(in.ExternalID), strings.TrimSpace(in.Name)
	if in.ExternalID == "" || len(in.ExternalID) > 200 || len(in.Name) > 100 {
		httpx.WriteError(w, http.StatusBadRequest, "bad_request", "external_id is required (at most 200 characters); name at most 100")
		return
	}
	if strings.HasPrefix(in.ExternalID, "~") {
		// "~" names topics' conversations (~conversation:…), never an app's user.
		httpx.WriteError(w, http.StatusBadRequest, "bad_request", "external_id can't start with ~")
		return
	}
	if in.Name == "" {
		in.Name = app.Name + " user"
	}
	var limit apps.WorkspaceLimitError
	if err := apps.CheckWorkspaceCap(r.Context(), s.DB.Pool, app, in.ExternalID); errors.As(err, &limit) {
		httpx.WriteError(w, http.StatusForbidden, "workspace_limit", limit.Message(s.SupportEmail))
		return
	} else if apps.IsWorkspaceRate(err) {
		httpx.WriteError(w, http.StatusTooManyRequests, "workspace_rate_limited", err.Error())
		return
	} else if err != nil {
		slog.Error("oauth: counting an app's workspaces", "app", app.ID, "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, "internal", "couldn't create the workspace")
		return
	}
	var id string
	var created bool
	err := s.DB.Pool.QueryRow(r.Context(), `INSERT INTO tenants (name, kind, app_id, external_id) VALUES ($1, 'app', $2, $3)
		ON CONFLICT (app_id, external_id) WHERE app_id IS NOT NULL DO UPDATE SET active_at = now()
		RETURNING id::text, xmax = 0`, in.Name, app.ID, in.ExternalID).Scan(&id, &created)
	if err != nil {
		slog.Error("oauth: creating a headless workspace", "app", app.ID, "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, "internal", "couldn't create the workspace")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"workspace": id, "created": created})
}

// ownedWorkspace checks that the path's workspace is one the app keeps.
func (s *Server) ownedWorkspace(w http.ResponseWriter, r *http.Request, app apps.App) (string, bool) {
	ws := r.PathValue("workspace")
	var owner string
	err := s.DB.Pool.QueryRow(r.Context(), `SELECT coalesce(app_id, '') FROM tenants WHERE id = $1::uuid`, ws).Scan(&owner)
	if err != nil || !uuidRE.MatchString(ws) || owner != app.ID {
		httpx.WriteError(w, http.StatusNotFound, "unknown_workspace", "no such workspace for this app")
		return "", false
	}
	// The app is acting for it: it's in use (idle conversations are forgotten).
	if err := s.DB.Touch(r.Context(), ws); err != nil {
		slog.Warn("oauth: noting a workspace in use", "app", app.ID, "err", err)
	}
	return ws, true
}

// addAgent creates an agent in one of the app's workspaces (if new) and its
// grant for the app's manifest scope, at once.
func (s *Server) addAgent(w http.ResponseWriter, r *http.Request) {
	app := headlessApp(r.Context())
	ws, ok := s.ownedWorkspace(w, r, app)
	if !ok {
		return
	}
	var in struct {
		Agent       string `json:"agent"`
		DisplayName string `json:"display_name"`
	}
	if !httpx.Decode(w, r, &in) {
		return
	}
	if app.Manifest.Agent == nil {
		httpx.WriteError(w, http.StatusBadRequest, "bad_request", "the app's manifest has no agent scope")
		return
	}
	if !agents.ValidID(in.Agent) || len(in.DisplayName) > 100 {
		httpx.WriteError(w, http.StatusBadRequest, "bad_request", "agent is 2-40 lowercase letters, digits and dashes")
		return
	}
	var grant string
	err := s.DB.InTenant(r.Context(), ws, func(tx pgx.Tx) error {
		ctx := r.Context()
		if ok, err := agents.Exists(ctx, tx, in.Agent); err != nil {
			return err
		} else if !ok {
			if _, err := agents.Create(ctx, tx, in.Agent, in.DisplayName); err != nil {
				return err
			}
		}
		g, err := grants.EnsureByApp(ctx, tx, app, in.Agent)
		grant = g.ID
		return err
	})
	if err != nil {
		slog.Error("oauth: adding a headless agent", "app", app.ID, "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, "internal", "couldn't add the agent")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"agent": in.Agent, "grant": grant})
}

// agentToken is a short-lived app API token for acting for one of the
// app's agents, while its grant is live.
func (s *Server) agentToken(w http.ResponseWriter, r *http.Request) {
	app := headlessApp(r.Context())
	ws, ok := s.ownedWorkspace(w, r, app)
	if !ok {
		return
	}
	agent := r.PathValue("agent")
	var grantID string
	err := s.DB.InTenant(r.Context(), ws, func(tx pgx.Tx) error {
		g, err := grants.Live(r.Context(), tx, app.ID, agent)
		grantID = g.ID
		return err
	})
	if errors.Is(err, grants.ErrNotFound) {
		httpx.WriteError(w, http.StatusForbidden, "no_grant", "the agent has no live grant for this app (add it first)")
		return
	}
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "internal", "try again")
		return
	}
	key, err := s.keys.signer(r.Context())
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "internal", "try again")
		return
	}
	now := s.now()
	exp := now.Add(exchangeTTL)
	token, err := signJWT(key, AccessClaims{
		Issuer: s.Issuer, Subject: "app:" + app.ID + ":" + ws, Audience: s.APIAudience(), ClientID: "app:" + app.ID,
		IssuedAt: now.Unix(), Expires: exp.Unix(), ID: randomString(12), Tenant: ws, Agent: agent,
		Actor: &Actor{Subject: "app:" + app.ID}, Grant: grantID,
	})
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "internal", "try again")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"access_token": token, "expires_in": int(exchangeTTL.Seconds()), "workspace": ws, "agent": agent})
}
