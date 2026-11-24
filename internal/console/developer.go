package console

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/appapi"
	"github.com/wakeshift/server/internal/apps"
	"github.com/wakeshift/server/internal/audit"
	"github.com/wakeshift/server/internal/auth"
	"github.com/wakeshift/server/internal/httpx"
	"github.com/wakeshift/server/internal/orgs"
)

// The apps people build. Apps are platform-level and belong to an
// organization; only its members see them, their keys and log, or what
// happens in the workspaces the app keeps for its users (appconsole.go),
// and each member's role says what they may change. Every route runs the
// one check, apps.Registry.Access, with its action: a non-member gets 404,
// as for an app that doesn't exist, and a member whose role doesn't allow
// the action 403. The registry writes each change to the app's log in the
// same transaction as the change.

func (a *API) registerDeveloper(route func(string, http.HandlerFunc)) {
	route("GET /api/v1/developer/apps", a.listOwnedApps)
	route("POST /api/v1/developer/apps", a.createOwnedApp)
	route("POST /api/v1/developer/apps/register", a.registerApp)
	route("GET /api/v1/developer/apps/{id}", a.getOwnedApp)
	route("PATCH /api/v1/developer/apps/{id}", a.updateOwnedApp)
	route("DELETE /api/v1/developer/apps/{id}", a.deleteOwnedApp)
	route("GET /api/v1/developer/apps/{id}/log", a.appLog)
	route("GET /api/v1/developer/apps/{id}/keys", a.listKeys)
	route("POST /api/v1/developer/apps/{id}/keys", a.createKey)
	route("POST /api/v1/developer/apps/{id}/keys/register", a.registerKey)
	route("PATCH /api/v1/developer/apps/{id}/keys/{key}", a.renameKey)
	route("POST /api/v1/developer/apps/{id}/keys/{key}/revoke", a.revokeKey)
	route("POST /api/v1/developer/apps/{id}/keys/{key}/rotate", a.rotateKey)
	a.registerAppConsole(route)
}

// platformTx runs fn in a transaction with no workspace: apps, their keys
// and logs are platform-level.
func (a *API) platformTx(r *http.Request, fn func(ctx context.Context, tx pgx.Tx) error) error {
	ctx := r.Context()
	return pgx.BeginFunc(ctx, a.DB.Pool, func(tx pgx.Tx) error { return fn(ctx, tx) })
}

func me(r *http.Request) string { return auth.UserFrom(r.Context()).ID }

// listOwnedApps lists an organization's apps (?org=, or without it the
// user's default organization, for a console from before organizations).
func (a *API) listOwnedApps(w http.ResponseWriter, r *http.Request) {
	var list []apps.Owned
	var org string
	err := a.platformTx(r, func(ctx context.Context, tx pgx.Tx) (err error) {
		list, org, err = a.Apps.ListOwned(ctx, tx, me(r), r.URL.Query().Get("org"))
		return err
	})
	a.respondApps(w, map[string]any{"apps": list, "org_id": org}, err)
}

func (a *API) createOwnedApp(w http.ResponseWriter, r *http.Request) {
	var in apps.NewApp
	if !httpx.Decode(w, r, &in) {
		return
	}
	if err := in.Validate(); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	var out apps.Owned
	var k apps.Key
	var key string
	err := a.platformTx(r, func(ctx context.Context, tx pgx.Tx) (err error) {
		out, k, key, err = a.Apps.Create(ctx, tx, me(r), in.OrgID, in)
		return err
	})
	a.respondApps(w, map[string]any{"app": out, "key": key, "key_info": k}, err)
}

func (a *API) getOwnedApp(w http.ResponseWriter, r *http.Request) {
	var out apps.Owned
	err := a.platformTx(r, func(ctx context.Context, tx pgx.Tx) (err error) {
		out, err = a.Apps.GetOwned(ctx, tx, me(r), r.PathValue("id"))
		return err
	})
	a.respondApps(w, out, err)
}

// updateOwnedApp changes the app's name or MCP URL (Settings).
func (a *API) updateOwnedApp(w http.ResponseWriter, r *http.Request) {
	var in apps.Settings
	if !httpx.Decode(w, r, &in) {
		return
	}
	var out apps.Owned
	err := a.platformTx(r, func(ctx context.Context, tx pgx.Tx) (err error) {
		out, err = a.Apps.Update(ctx, tx, me(r), r.PathValue("id"), in)
		return err
	})
	a.respondApps(w, out, err)
}

func (a *API) listKeys(w http.ResponseWriter, r *http.Request) {
	var list []apps.Key
	err := a.platformTx(r, func(ctx context.Context, tx pgx.Tx) (err error) {
		list, err = a.Apps.ListKeys(ctx, tx, me(r), r.PathValue("id"))
		return err
	})
	a.respondApps(w, map[string]any{"keys": list, "max": apps.MaxKeys}, err)
}

// createKey makes a new key, returned once; the app's other keys keep
// working.
func (a *API) createKey(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name string `json:"name"`
	}
	if !httpx.Decode(w, r, &in) {
		return
	}
	var k apps.Key
	var key string
	err := a.platformTx(r, func(ctx context.Context, tx pgx.Tx) (err error) {
		k, key, err = a.Apps.CreateKey(ctx, tx, me(r), r.PathValue("id"), in.Name)
		return err
	})
	a.respondApps(w, map[string]any{"key": key, "key_info": k}, err)
}

// renameKey changes one key's name; a revoked key keeps its own.
func (a *API) renameKey(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name string `json:"name"`
	}
	if !httpx.Decode(w, r, &in) {
		return
	}
	var k apps.Key
	err := a.platformTx(r, func(ctx context.Context, tx pgx.Tx) (err error) {
		k, err = a.Apps.RenameKey(ctx, tx, me(r), r.PathValue("id"), r.PathValue("key"), in.Name)
		return err
	})
	a.respondApps(w, map[string]any{"key_info": k}, err)
}

// revokeKey stops one key at once.
func (a *API) revokeKey(w http.ResponseWriter, r *http.Request) {
	var k apps.Key
	err := a.platformTx(r, func(ctx context.Context, tx pgx.Tx) (err error) {
		k, err = a.Apps.RevokeKey(ctx, tx, me(r), r.PathValue("id"), r.PathValue("key"))
		return err
	})
	a.respondApps(w, map[string]any{"key_info": k}, err)
}

// rotateKey replaces one key with a new one of the same name, returned
// once; the old one stops at once.
func (a *API) rotateKey(w http.ResponseWriter, r *http.Request) {
	var k apps.Key
	var key string
	err := a.platformTx(r, func(ctx context.Context, tx pgx.Tx) (err error) {
		k, key, err = a.Apps.RotateKey(ctx, tx, me(r), r.PathValue("id"), r.PathValue("key"))
		return err
	})
	a.respondApps(w, map[string]any{"key": key, "key_info": k}, err)
}

// deleteOwnedApp deletes the app (its keys stop at once), then revokes its
// grants in the workspaces it kept for its users, which ends its agents'
// subscriptions and stream links.
func (a *API) deleteOwnedApp(w http.ResponseWriter, r *http.Request) {
	user := me(r)
	var out apps.Owned
	err := a.platformTx(r, func(ctx context.Context, tx pgx.Tx) (err error) {
		out, _, err = a.Apps.Delete(ctx, tx, user, r.PathValue("id"))
		return err
	})
	if err != nil {
		a.respondApps(w, nil, err)
		return
	}
	revoked, workspaces, err := a.revokeAppGrants(r.Context(), out.ID, audit.User(user))
	if revoked > 0 || err != nil {
		if lerr := a.Apps.LogGrantsRevoked(r.Context(), user, out.ID, revoked, workspaces, err == nil); lerr != nil {
			slog.Error("console: logging a deleted app's revoked grants", "app", out.ID, "err", lerr)
		}
	}
	if err != nil {
		// The keys are gone already, so no new agent tokens; ones already
		// out last at most 15 minutes. Deleting again finishes this.
		slog.Error("console: revoking a deleted app's grants", "app", out.ID, "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, "internal", "the app is deleted, but some of its grants are still live; delete it again")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"deleted": true, "grants_revoked": revoked})
}

// revokeAppGrants revokes an app's live grants in every workspace it keeps
// (a test app has grants nowhere else): how many, and in how many
// workspaces.
func (a *API) revokeAppGrants(ctx context.Context, appID, by string) (revoked, inWorkspaces int, err error) {
	workspaces, err := a.Apps.Workspaces(ctx, appID)
	if err != nil {
		return 0, 0, err
	}
	return appapi.RevokeGrantsIn(ctx, a.DB, appID, by, workspaces)
}

// appLog is the app's log, newest first, for its owner only (even after
// it's deleted): up to limit entries (at most 100) older than the entry ID
// before, whose action starts with action (e.g. app.key_).
func (a *API) appLog(w http.ResponseWriter, r *http.Request) {
	before, limit, ok := page(w, r, apps.MaxLogLimit)
	if !ok {
		return
	}
	action := r.URL.Query().Get("action")
	if len(action) > 64 {
		httpx.WriteError(w, http.StatusBadRequest, "bad_request", "action is at most 64 characters")
		return
	}
	var list []apps.LogItem
	err := a.platformTx(r, func(ctx context.Context, tx pgx.Tx) (err error) {
		list, err = a.Apps.ListLog(ctx, tx, me(r), r.PathValue("id"), before, limit, action)
		return err
	})
	a.respondApps(w, map[string]any{"entries": list}, err)
}

func (a *API) respondApps(w http.ResponseWriter, v any, err error) {
	var invalid *apps.ValidationError
	var limited *apps.RateLimitError
	msg, bad := appapi.BadRequest(err)
	switch {
	case errors.Is(err, apps.ErrNotOwner):
		httpx.WriteError(w, http.StatusNotFound, "not_found", "no such app of yours")
	case errors.Is(err, orgs.ErrNotMember):
		httpx.WriteError(w, http.StatusNotFound, "not_found", "no such organization of yours")
	case errors.Is(err, orgs.ErrForbidden):
		httpx.WriteError(w, http.StatusForbidden, "forbidden", "your role in this organization doesn't allow that; ask an owner or admin")
	case errors.Is(err, apps.ErrNoKey):
		httpx.WriteError(w, http.StatusNotFound, "not_found", "no such key")
	case errors.Is(err, apps.ErrKeyRevoked):
		httpx.WriteError(w, http.StatusConflict, "revoked", "the key is revoked; it keeps its name")
	case errors.Is(err, appapi.ErrNoPublishURL):
		httpx.WriteError(w, http.StatusNotFound, "not_found", "no such publish URL")
	case errors.Is(err, appapi.ErrPublishURLRevoked):
		httpx.WriteError(w, http.StatusGone, "revoked", err.Error())
	case errors.As(err, new(appapi.SigningSecretError)):
		httpx.WriteError(w, http.StatusBadRequest, "bad_signing_secret", err.Error())
	case errors.Is(err, appapi.ErrNoSubscription):
		httpx.WriteError(w, http.StatusNotFound, "not_found", "no such subscription")
	case errors.Is(err, apps.ErrDeleted):
		httpx.WriteError(w, http.StatusConflict, "deleted", "the app is deleted")
	case errors.Is(err, apps.ErrSuspended):
		httpx.WriteError(w, http.StatusForbidden, "suspended", apps.SuspendedMessage(a.SupportEmail))
	case errors.Is(err, apps.ErrTaken):
		httpx.WriteError(w, http.StatusConflict, "exists", "that app ID is taken; pick another")
	case errors.Is(err, apps.ErrTooMany):
		httpx.WriteError(w, http.StatusBadRequest, "too_many", fmt.Sprintf("an organization can have at most %d apps; delete one first", apps.MaxOwned))
	case errors.Is(err, apps.ErrTooManyKeys):
		httpx.WriteError(w, http.StatusBadRequest, "too_many", fmt.Sprintf("an app can have at most %d keys; revoke one first", apps.MaxKeys))
	case errors.Is(err, apps.ErrBadPublicKey):
		httpx.WriteError(w, http.StatusBadRequest, "invalid_key", "the link's public key isn't a valid key; copy the whole link from your terminal, or run the command again")
	case errors.Is(err, apps.ErrKeyUsed):
		httpx.WriteError(w, http.StatusConflict, "key_used", "that key is registered already; run the command again for a new one")
	case errors.As(err, &limited):
		httpx.WriteError(w, http.StatusTooManyRequests, "rate_limited", limited.Message)
	case errors.As(err, &invalid):
		httpx.WriteError(w, http.StatusBadRequest, "bad_request", invalid.Message)
	case bad:
		httpx.WriteError(w, http.StatusBadRequest, "bad_request", msg)
	default:
		respond(w, v, err)
	}
}
