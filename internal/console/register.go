package console

// Registering a key made on the developer's machine: @wakeshift/cli
// generates the key, keeps it in .env and opens the console at
// /new-app#pk=… (or /apps/<id>/keys#pk=…). The page, with the person signed
// in, posts the public key here. The routes are the console's like every
// developer route: a session cookie, from the console's own origin
// (auth.Console), and apps.Registry's one check of the person's role.
// Nothing secret comes back: the key never left their machine.
//
// A link can be someone else's: a phisher runs the CLI, sends the link,
// and whoever confirms it puts the phisher's key on their app. So:
//   - the person types the key code their terminal shows (key_code), and the
//     page never shows it, so confirming takes having the terminal;
//   - only owners and admins add a link's key to an app that exists
//     (orgs.AppLinkKey; apps.RegisterKey);
//   - the key is marked via cli_link, and the organization's owners and
//     admins are told: an entry in the organization's log, in the same
//     transaction, and an email each when invite email is set up.
//
// Each registration is also recorded in the app's log (app.created and
// app.key_created with via "cli") and in the person's own audit log
// (audit.KeyRegistered), since approving a key the console didn't make is
// a consent. Request bodies are never logged.

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/apps/email"
	"github.com/wakeshift/server/internal/apps"
	"github.com/wakeshift/server/internal/audit"
	"github.com/wakeshift/server/internal/httpx"
	"github.com/wakeshift/server/internal/mail"
	"github.com/wakeshift/server/internal/orgs"
)

// LogKeyFromLink is the organization's log entry for a key added from
// @wakeshift/cli's link, to a new app or one that exists.
const LogKeyFromLink = "org.app_key_from_link"

var (
	// errKeyCodeRequired and errWrongKeyCode refuse a registration without
	// the key code typed from the terminal, or with another key's (400).
	errKeyCodeRequired = errors.New("type the key code your terminal shows")
	errWrongKeyCode    = errors.New("that isn't the key code for this link; type the one your terminal shows, and only if you ran the command yourself")
)

// checkKeyCode says whether typed is pub's key code (apps.KeyID, which the
// CLI prints as "Key code: …"), ignoring surrounding spaces and case:
// nil, errKeyCodeRequired or errWrongKeyCode.
func checkKeyCode(typed string, pub ed25519.PublicKey) error {
	typed = strings.TrimSpace(typed)
	switch {
	case typed == "":
		return errKeyCodeRequired
	case !strings.EqualFold(typed, apps.KeyID(pub)):
		return errWrongKeyCode
	}
	return nil
}

// respondKeyCode writes checkKeyCode's refusal, reporting whether it did.
func respondKeyCode(w http.ResponseWriter, err error) bool {
	switch {
	case errors.Is(err, errKeyCodeRequired):
		httpx.WriteError(w, http.StatusBadRequest, "key_code_required", err.Error())
	case errors.Is(err, errWrongKeyCode):
		httpx.WriteError(w, http.StatusBadRequest, "wrong_key_code", err.Error())
	default:
		return false
	}
	return true
}

// registerApp creates an app whose only key is the public key in the body
// (public_key: 43 characters of base64url), with the fields of a new app,
// once the person has typed its key code (key_code).
func (a *API) registerApp(w http.ResponseWriter, r *http.Request) {
	var in struct {
		apps.NewApp
		PublicKey string `json:"public_key"`
		KeyCode   string `json:"key_code"`
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
	if respondKeyCode(w, checkKeyCode(in.KeyCode, pub)) {
		return
	}
	user := me(r)
	var out apps.Owned
	var k apps.Key
	var notice linkNotice
	err = a.platformTx(r, func(ctx context.Context, tx pgx.Tx) (err error) {
		if out, k, err = a.Apps.CreateWithKey(ctx, tx, user, in.OrgID, in.NewApp, pub, apps.ViaCLI); err != nil {
			return err
		}
		notice, err = recordLinkRegistration(ctx, tx, user, out, k, true)
		return err
	})
	if err == nil {
		a.mailKeyFromLink(r.Context(), notice)
	}
	a.respondApps(w, map[string]any{"app": out, "key_info": k}, err)
}

// registerKey adds the public key in the body to the app, named name, as
// making a key does (the same limits and log), once the person has typed
// its key code, returning only what's kept of it. Owners and admins only.
func (a *API) registerKey(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name      string `json:"name"`
		PublicKey string `json:"public_key"`
		KeyCode   string `json:"key_code"`
	}
	if !httpx.Decode(w, r, &in) {
		return
	}
	pub, err := apps.ParsePublicKey(in.PublicKey)
	if err != nil {
		a.respondApps(w, nil, err)
		return
	}
	if respondKeyCode(w, checkKeyCode(in.KeyCode, pub)) {
		return
	}
	user, id := me(r), r.PathValue("id")
	var k apps.Key
	var notice linkNotice
	err = a.platformTx(r, func(ctx context.Context, tx pgx.Tx) (err error) {
		if k, err = a.Apps.RegisterKey(ctx, tx, user, id, in.Name, pub, apps.ViaCLI); err != nil {
			return err
		}
		o, err := a.Apps.GetOwned(ctx, tx, user, id)
		if err != nil {
			return err
		}
		notice, err = recordLinkRegistration(ctx, tx, user, o, k, false)
		return err
	})
	if err == nil {
		a.mailKeyFromLink(r.Context(), notice)
	}
	a.respondApps(w, map[string]any{"key_info": k}, err)
}

// recordLinkRegistration records a key registered from a link, in tx, the
// same way for a new app and an existing one: the organization's log
// (noteKeyFromLink) and the person's own audit log (recordKeyRegistered).
// It returns the notice to email once tx commits (mailKeyFromLink).
func recordLinkRegistration(ctx context.Context, tx pgx.Tx, user string, app apps.Owned, k apps.Key, newApp bool) (linkNotice, error) {
	n, err := noteKeyFromLink(ctx, tx, user, app, k, newApp)
	if err != nil {
		return n, err
	}
	return n, recordKeyRegistered(ctx, tx, user, app, k, newApp)
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

// linkNotice is what the owners and admins are emailed about a key added
// from a link: who added it (their email address), to which app, the key's
// code, and the addresses to tell.
type linkNotice struct {
	by, app, keyID, org string
	newApp              bool
	to                  []string
}

// noteKeyFromLink writes LogKeyFromLink in app's organization's log, in tx,
// and returns the notice to email once tx commits.
func noteKeyFromLink(ctx context.Context, tx pgx.Tx, user string, app apps.Owned, k apps.Key, newApp bool) (linkNotice, error) {
	n := linkNotice{app: app.ID, keyID: k.ID, newApp: newApp}
	if err := orgs.Log(ctx, tx, orgs.Entry{Org: app.OrgID, Actor: orgs.UserActor(user), Action: LogKeyFromLink,
		Detail: map[string]any{"app_id": app.ID, "key_id": k.ID, "via": apps.ViaCLILink, "new_app": newApp}}); err != nil {
		return n, err
	}
	if err := tx.QueryRow(ctx, `SELECT u.email, o.name FROM users u, organizations o WHERE u.id = $1 AND o.id = $2`, user, app.OrgID).
		Scan(&n.by, &n.org); err != nil {
		return n, err
	}
	rows, err := tx.Query(ctx, `SELECT u.email FROM organization_members m JOIN users u ON u.id = m.user_id
		WHERE m.org_id = $1 AND m.role IN ('owner', 'admin') AND u.email <> '' ORDER BY m.created_at`, app.OrgID)
	if err != nil {
		return n, err
	}
	n.to, err = pgx.CollectRows(rows, pgx.RowTo[string])
	return n, err
}

// mailKeyFromLink emails n to each of the organization's owners and admins,
// when email is set up. A failure is logged (never the message or its
// recipient) and doesn't undo the registration: the organization's log
// has it either way.
func (a *API) mailKeyFromLink(ctx context.Context, n linkNotice) {
	m := a.mailer()
	if len(n.to) == 0 || !m.Ready(ctx) {
		return
	}
	keys := strings.TrimSuffix(a.Auth.PublicURL, "/") + "/apps/" + url.PathEscape(n.app) + "/keys"
	for _, to := range n.to {
		if err := m.Send(ctx, keyFromLinkMail(n, to, keys)); err != nil {
			slog.Warn("console: emailing a key-from-link notice failed", "app", n.app, "err", err)
		}
	}
}

// keyFromLinkSubject is the notice's subject: fixed (mailsafe.go).
const keyFromLinkSubject = "A key was added to your organization's app from a CLI link"

// keyFromLinkMail is the notice's email to one owner or admin.
func keyFromLinkMail(n linkNotice, to, keys string) mail.Message {
	in := ""
	if name := mailName(n.org); name != "" {
		in = " in " + name
	}
	what := fmt.Sprintf("%s added key %s to the app %s%s, from a link the Wakeshift CLI made.", oneLine(n.by), n.keyID, n.app, in)
	if n.newApp {
		what = fmt.Sprintf("%s created the app %s%s from a link the Wakeshift CLI made, with key %s.", oneLine(n.by), n.app, in, n.keyID)
	}
	r := email.Render(email.Wakeshift, email.Email{
		Subject:   keyFromLinkSubject,
		Preheader: "If nobody in your organization just ran the Wakeshift CLI, revoke the key.",
		Kicker:    "Security notice",
		Heading:   "A key was added from a CLI link",
		Body: []string{what,
			"The key works for the app's API now. If nobody in your organization just ran the Wakeshift CLI and confirmed it, someone may have sent them their own link: revoke the key."},
		Button: email.Button{Label: "Review the app's keys", URL: keys},
		Notes:  []string{"Wakeshift emails every owner and admin when a key is added from a CLI link."},
		Reason: "You're an owner or admin of the organization the app belongs to.",
	})
	return mail.Message{To: to, Subject: keyFromLinkSubject, Text: r.Text, HTML: r.HTML}
}
