package console

// Registering a key made on the developer's machine: @wakeshift/cli
// generates the key, keeps it in .env and opens the console at
// /new-app#pk=… (or /apps/<id>/keys#pk=…). The page, with the person signed
// in, posts the public key here. The routes are the console's like every
// developer route: a session cookie, from the console's own origin
// (auth.Console), and apps.Registry's one check of the person's role.
// Nothing secret comes back: the key never left their machine.
//
// Each registration is recorded twice in its transaction: in the app's log
// (app.created and app.key_created with via "cli"), and in the person's
// own audit log (audit.KeyRegistered), since approving a key the console
// didn't make is a consent. Request bodies are never logged.

import (
	"context"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/apps"
	"github.com/wakeshift/server/internal/audit"
	"github.com/wakeshift/server/internal/httpx"
)

// registerApp creates an app whose only key is the public key in the body
// (public_key: 43 characters of base64url), with the fields of a new app.
func (a *API) registerApp(w http.ResponseWriter, r *http.Request) {
	var in struct {
		apps.NewApp
		PublicKey string `json:"public_key"`
	}
	if !httpx.Decode(w, r, &in) {
		return
	}
	pub, err := apps.ParsePublicKey(in.PublicKey)
	if err == nil {
		err = in.NewApp.Validate()
	}
	if err != nil {
		a.respondApps(w, nil, err)
		return
	}
	user := me(r)
	var out apps.Owned
	var k apps.Key
	err = a.platformTx(r, func(ctx context.Context, tx pgx.Tx) (err error) {
		if out, k, err = a.Apps.CreateWithKey(ctx, tx, user, in.OrgID, in.NewApp, pub, apps.ViaCLI); err != nil {
			return err
		}
		return recordKeyRegistered(ctx, tx, user, out, k, true)
	})
	a.respondApps(w, map[string]any{"app": out, "key_info": k}, err)
}

// registerKey adds the public key in the body to the app, named name, as
// making a key does (the same role, limits and log), returning only what's
// kept of it.
func (a *API) registerKey(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name      string `json:"name"`
		PublicKey string `json:"public_key"`
	}
	if !httpx.Decode(w, r, &in) {
		return
	}
	pub, err := apps.ParsePublicKey(in.PublicKey)
	if err != nil {
		a.respondApps(w, nil, err)
		return
	}
	user, id := me(r), r.PathValue("id")
	var k apps.Key
	err = a.platformTx(r, func(ctx context.Context, tx pgx.Tx) (err error) {
		if k, err = a.Apps.RegisterKey(ctx, tx, user, id, in.Name, pub, apps.ViaCLI); err != nil {
			return err
		}
		o, err := a.Apps.GetOwned(ctx, tx, user, id)
		if err != nil {
			return err
		}
		return recordKeyRegistered(ctx, tx, user, o, k, false)
	})
	a.respondApps(w, map[string]any{"key_info": k}, err)
}

// recordKeyRegistered writes audit.KeyRegistered in user's personal
// workspace, in tx. It scopes tx to that workspace, so it comes last.
func recordKeyRegistered(ctx context.Context, tx pgx.Tx, user string, app apps.Owned, k apps.Key, newApp bool) error {
	var ws string
	if err := tx.QueryRow(ctx, `SELECT t.id::text FROM memberships m JOIN tenants t ON t.id = m.tenant_id
		WHERE m.user_id = $1 AND t.kind = 'personal' ORDER BY t.created_at LIMIT 1`, user).Scan(&ws); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, ws); err != nil {
		return err
	}
	return audit.Record(ctx, tx, audit.Entry{Actor: audit.User(user), Action: audit.KeyRegistered, AppID: app.ID,
		Detail: map[string]any{"org_id": app.OrgID, "key_id": k.ID, "via": apps.ViaCLI, "new_app": newApp}})
}
