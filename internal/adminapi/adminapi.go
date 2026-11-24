// Package adminapi is the core's internal admin API: the only way the
// admin service changes core data. It runs the same registry code as the
// console, and each action writes the app's log (actor apps.AdminActor)
// and an admin_log entry naming the person, in one transaction. A refused
// action writes nothing.
//
// Every request needs two Google-signed tokens:
//
//  1. Authorization: Bearer <ID token> of the admin service's account,
//     from the metadata server, for audience <public URL>/internal/admin,
//     with that account's email.
//  2. X-Admin-Assertion: the person's IAP assertion, forwarded by the
//     admin service, for the admin backend's audience, with an email
//     exactly on WAKESHIFT_ADMIN_EMAILS (internal/iap).
//
// Anything less is 404, the same as with the API turned off. So a stolen
// service-account token can't act without a person's assertion (which
// lasts about ten minutes), and an assertion can't act without the
// service account.
package adminapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"unicode"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/appapi"
	"github.com/wakeshift/server/internal/apps"
	"github.com/wakeshift/server/internal/db"
	"github.com/wakeshift/server/internal/httpx"
	"github.com/wakeshift/server/internal/iap"
	"github.com/wakeshift/server/internal/mail"
)

// AssertionHeader carries the person's IAP assertion.
const AssertionHeader = "X-Admin-Assertion"

// Prefix is where the API's routes are.
const Prefix = "/internal/admin/"

// GoogleIssuer issues the admin service's ID tokens.
const GoogleIssuer = "https://accounts.google.com"

// GoogleKeysURL is Google's ID token signing keys.
const GoogleKeysURL = "https://www.googleapis.com/oauth2/v3/certs"

// IDTokenVerifier checks the admin service's ID token (an
// *oidc.IDTokenVerifier).
type IDTokenVerifier interface {
	Verify(ctx context.Context, raw string) (*oidc.IDToken, error)
}

// API is the internal admin API. With any of Service, ServiceAccount or IAP
// unset, it's off: every path under Prefix is 404.
type API struct {
	DB     *db.DB
	Apps   *apps.Registry
	AppAPI *appapi.API
	// Service verifies the admin service's ID token, for Audience.
	Service IDTokenVerifier
	// ServiceAccount is the admin service's account's email.
	ServiceAccount string
	// IAP verifies the forwarded assertion and the allowlist.
	IAP *iap.Verifier
	// Mail sends the waitlist's invitation emails (nil: none); ConsoleURL
	// is where they send people to sign in.
	Mail       mail.Sender
	ConsoleURL string
}

// Audience is the admin service's ID tokens' audience for a core at
// publicURL.
func Audience(publicURL string) string { return strings.TrimSuffix(publicURL, "/") + "/internal/admin" }

// NewGoogleVerifier verifies Google-signed ID tokens for audience.
func NewGoogleVerifier(ctx context.Context, audience string) IDTokenVerifier {
	return oidc.NewVerifier(GoogleIssuer, oidc.NewRemoteKeySet(ctx, GoogleKeysURL), &oidc.Config{ClientID: audience})
}

func (a *API) on() bool {
	return a != nil && a.Service != nil && a.ServiceAccount != "" && a.IAP != nil
}

// Register adds the routes.
func (a *API) Register(mux *http.ServeMux) {
	mux.HandleFunc(Prefix, func(w http.ResponseWriter, r *http.Request) { notFound(w) })
	if !a.on() {
		return
	}
	route := func(pattern string, h func(http.ResponseWriter, *http.Request, string)) {
		mux.HandleFunc(pattern, a.authenticated(h))
	}
	route("POST /internal/admin/v1/apps/{id}/suspend", a.suspend)
	route("POST /internal/admin/v1/apps/{id}/unsuspend", a.unsuspend)
	route("PUT /internal/admin/v1/apps/{id}/workspace-cap", a.workspaceCap)
	route("POST /internal/admin/v1/apps/{id}/delete", a.deleteApp)
	route("POST /internal/admin/v1/apps/{id}/users/forget", a.forgetUser)
	// The emergency stop for subscribe vouchers: every app's, or one's.
	route("POST /internal/admin/v1/voucher-subscriptions/end", a.endVoucherSubscriptions)
	route("POST /internal/admin/v1/apps/{id}/voucher-subscriptions/end", a.endVoucherSubscriptions)
	a.registerSignups(route)
}

func notFound(w http.ResponseWriter) {
	httpx.WriteError(w, http.StatusNotFound, "not_found", "no such API endpoint")
}

// authenticated runs h with the person's email only when both tokens are
// valid; anything else is 404.
func (a *API) authenticated(h func(http.ResponseWriter, *http.Request, string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		admin, ok := a.check(r)
		if !ok {
			notFound(w)
			return
		}
		h(w, r, admin)
	}
}

func (a *API) check(r *http.Request) (string, bool) {
	raw, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || raw == "" || len(raw) > 8192 {
		return "", false
	}
	tok, err := a.Service.Verify(r.Context(), raw)
	if err != nil {
		return "", false
	}
	var claims struct {
		Email    string `json:"email"`
		Verified bool   `json:"email_verified"`
	}
	if err := tok.Claims(&claims); err != nil || !claims.Verified || claims.Email != a.ServiceAccount {
		return "", false
	}
	id, err := a.IAP.Verify(r.Context(), r.Header.Get(AssertionHeader))
	if err != nil {
		return "", false
	}
	return id.Email, true
}

// logAdmin appends to admin_log in tx: who, what, which app, why.
func logAdmin(ctx context.Context, tx pgx.Tx, admin, action, app, reason string, detail any) error {
	return logAdminTarget(ctx, tx, admin, action, "app", app, reason, detail)
}

// logAdminTarget appends to admin_log in tx: who, what, which target (an
// app, the sign-up cap, a waitlist entry), why.
func logAdminTarget(ctx context.Context, tx pgx.Tx, admin, action, kind, target, reason string, detail any) error {
	b := []byte("{}")
	if detail != nil {
		var err error
		if b, err = json.Marshal(detail); err != nil {
			return err
		}
	}
	_, err := tx.Exec(ctx, `INSERT INTO admin_log (actor, action, target_kind, target_id, reason, detail) VALUES ($1, $2, $3, $4, nullif($5, ''), $6)`,
		admin, action, kind, target, reason, b)
	return err
}

// The admin_log actions this API writes: the app log's, for the same
// change, except forgetting a user, which the app's log records as
// app.user_forgotten too.
const (
	ActionSuspended       = apps.LogSuspended
	ActionUnsuspended     = apps.LogUnsuspended
	ActionWorkspaceCapSet = apps.LogWorkspaceCapSet
	ActionDeleted         = "app.deleted"
	ActionUserForgotten   = "app.user_forgotten"
	// ActionVoucherSubscriptionsEnded: the emergency stop ended live
	// subscribe-voucher subscriptions, of one app (target app) or of
	// every app (target "platform", "*").
	ActionVoucherSubscriptionsEnded = "subscribe_vouchers.subscriptions_ended"
)

func (a *API) tx(r *http.Request, fn func(ctx context.Context, tx pgx.Tx) error) error {
	ctx := r.Context()
	return pgx.BeginFunc(ctx, a.DB.Pool, func(tx pgx.Tx) error { return fn(ctx, tx) })
}

type reasoned struct {
	Reason string `json:"reason"`
}

func decode[T any](w http.ResponseWriter, r *http.Request, in *T, reason *string) bool {
	if !httpx.Decode(w, r, in) {
		return false
	}
	clean, err := apps.ValidReason(*reason)
	if err != nil {
		respond(w, nil, err)
		return false
	}
	*reason = clean
	return true
}

func (a *API) suspend(w http.ResponseWriter, r *http.Request, admin string) {
	var in reasoned
	if !decode(w, r, &in, &in.Reason) {
		return
	}
	id := r.PathValue("id")
	err := a.tx(r, func(ctx context.Context, tx pgx.Tx) error {
		if err := a.Apps.Suspend(ctx, tx, id, admin, in.Reason); err != nil {
			return err
		}
		return logAdmin(ctx, tx, admin, ActionSuspended, id, in.Reason, nil)
	})
	respond(w, map[string]any{"suspended": true}, err)
}

func (a *API) unsuspend(w http.ResponseWriter, r *http.Request, admin string) {
	var in reasoned
	if !decode(w, r, &in, &in.Reason) {
		return
	}
	id := r.PathValue("id")
	err := a.tx(r, func(ctx context.Context, tx pgx.Tx) error {
		if err := a.Apps.Unsuspend(ctx, tx, id, in.Reason); err != nil {
			return err
		}
		return logAdmin(ctx, tx, admin, ActionUnsuspended, id, in.Reason, nil)
	})
	respond(w, map[string]any{"suspended": false}, err)
}

func (a *API) workspaceCap(w http.ResponseWriter, r *http.Request, admin string) {
	var in struct {
		Cap    *int   `json:"cap"`
		Reason string `json:"reason"`
	}
	if !decode(w, r, &in, &in.Reason) {
		return
	}
	id := r.PathValue("id")
	err := a.tx(r, func(ctx context.Context, tx pgx.Tx) error {
		old, err := a.Apps.SetWorkspaceCap(ctx, tx, id, in.Cap)
		if err != nil {
			return err
		}
		return logAdmin(ctx, tx, admin, ActionWorkspaceCapSet, id, in.Reason, map[string]any{"cap": in.Cap, "old": old})
	})
	respond(w, map[string]any{"cap": in.Cap}, err)
}

// deleteApp deletes the app as its owner could, by us: confirm must be its
// ID. Its keys and publish URLs stop at once, its grants in the
// workspaces it kept are revoked, and its users' data goes within 30 days
// (appapi.FinishAppDeletions).
func (a *API) deleteApp(w http.ResponseWriter, r *http.Request, admin string) {
	var in struct {
		Confirm string `json:"confirm"`
		Reason  string `json:"reason"`
	}
	if !decode(w, r, &in, &in.Reason) {
		return
	}
	id := r.PathValue("id")
	if in.Confirm != id {
		httpx.WriteError(w, http.StatusBadRequest, "confirm", "type the app's ID in confirm to delete it")
		return
	}
	var already bool
	err := a.tx(r, func(ctx context.Context, tx pgx.Tx) error {
		was, err := a.Apps.DeleteAsAdmin(ctx, tx, id)
		if err != nil {
			return err
		}
		if already = was; already {
			return nil // finishing a deletion: nothing new to log
		}
		return logAdmin(ctx, tx, admin, ActionDeleted, id, in.Reason, nil)
	})
	if err != nil {
		respond(w, nil, err)
		return
	}
	workspaces, err := a.Apps.Workspaces(r.Context(), id)
	var revoked, in2 int
	if err == nil {
		revoked, in2, err = appapi.RevokeGrantsIn(r.Context(), a.DB, id, apps.AdminActor, workspaces)
	}
	if revoked > 0 || err != nil {
		if lerr := a.Apps.LogGrantsRevokedBy(r.Context(), apps.AdminActor, id, revoked, in2, err == nil); lerr != nil {
			slog.Error("adminapi: logging a deleted app's revoked grants", "app", id, "err", lerr)
		}
	}
	if err != nil {
		slog.Error("adminapi: revoking a deleted app's grants", "app", id, "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, "internal", "the app is deleted, but some of its grants are still live; delete it again")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"deleted": true, "already": already, "grants_revoked": revoked})
}

// forgetUser forgets one of the app's users, as the app or its owners can,
// by us: their workspace and everything in it. request is the reference of
// the request that asked for it (a support ticket, say).
func (a *API) forgetUser(w http.ResponseWriter, r *http.Request, admin string) {
	var in struct {
		User    string `json:"user"`
		Request string `json:"request"`
		Reason  string `json:"reason"`
	}
	if !decode(w, r, &in, &in.Reason) {
		return
	}
	id := r.PathValue("id")
	in.Request = strings.TrimSpace(in.Request)
	if in.Request == "" || len(in.Request) > 100 || strings.ContainsFunc(in.Request, unicode.IsControl) {
		httpx.WriteError(w, http.StatusBadRequest, "bad_request", "request is the reference of the request that asked for this (at most 100 characters)")
		return
	}
	if _, err := a.Apps.Get(r.Context(), id); err != nil {
		respond(w, nil, err)
		return
	}
	user := strings.TrimSpace(in.User)
	forgotten, err := a.AppAPI.ForgetUserLogged(r.Context(), id, user, apps.AdminActor, func(tx pgx.Tx) error {
		// Never the user's ID, which can be an email, nor a hash of it,
		// which a guessable ID can be matched to: the request names them.
		return logAdmin(r.Context(), tx, admin, ActionUserForgotten, id, in.Reason,
			map[string]string{"request": in.Request})
	})
	if err == nil && !forgotten {
		httpx.WriteError(w, http.StatusNotFound, "not_found", "the app has no such user")
		return
	}
	respond(w, map[string]any{"forgotten": true}, err)
}

// endVoucherSubscriptions ends every live subscription a subscribe voucher
// made (appapi.EndVoucherSubscriptions), of the app in the path or of
// every app, each recorded in its workspace's audit log, and the action in
// admin_log with how many it ended. Turning WAKESHIFT_SUBSCRIBE_VOUCHERS
// off stops new ones; this ends the live ones now.
func (a *API) endVoucherSubscriptions(w http.ResponseWriter, r *http.Request, admin string) {
	var in reasoned
	if !decode(w, r, &in, &in.Reason) {
		return
	}
	id := r.PathValue("id")
	kind, target := "app", id
	if id == "" {
		kind, target = "platform", "*"
	} else if _, err := a.Apps.Get(r.Context(), id); err != nil {
		respond(w, nil, err)
		return
	}
	ended, workspaces, err := a.AppAPI.EndVoucherSubscriptions(r.Context(), id, apps.AdminActor)
	detail := map[string]any{"ended": ended, "workspaces": workspaces, "complete": err == nil}
	if lerr := a.tx(r, func(ctx context.Context, tx pgx.Tx) error {
		return logAdminTarget(ctx, tx, admin, ActionVoucherSubscriptionsEnded, kind, target, in.Reason, detail)
	}); lerr != nil {
		slog.Error("adminapi: logging ended voucher subscriptions", "err", lerr)
		if err == nil {
			err = lerr
		}
	}
	if err != nil {
		slog.Error("adminapi: ending voucher subscriptions", "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, "internal", "some voucher subscriptions are still live; run it again")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ended": ended, "workspaces": workspaces})
}

func respond(w http.ResponseWriter, v any, err error) {
	var invalid *apps.ValidationError
	msg, bad := appapi.BadRequest(err)
	switch {
	case err == nil:
		httpx.WriteJSON(w, http.StatusOK, v)
	case errors.Is(err, apps.ErrNotFound) || errors.Is(err, apps.ErrNotOwner):
		httpx.WriteError(w, http.StatusNotFound, "not_found", "no such app")
	case errors.Is(err, apps.ErrDeleted):
		httpx.WriteError(w, http.StatusConflict, "deleted", "the app is deleted")
	case errors.Is(err, apps.ErrNoChange):
		httpx.WriteError(w, http.StatusConflict, "no_change", "that's how the app is already")
	case errors.Is(err, apps.ErrOurs):
		httpx.WriteError(w, http.StatusConflict, "ours", err.Error())
	case errors.As(err, &invalid):
		httpx.WriteError(w, http.StatusBadRequest, "bad_request", invalid.Message)
	case bad:
		httpx.WriteError(w, http.StatusBadRequest, "bad_request", msg)
	default:
		slog.Error("adminapi: an action failed", "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, "internal", "try again")
	}
}
