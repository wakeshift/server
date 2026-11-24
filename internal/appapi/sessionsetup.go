package appapi

// Session-only setup, sent when it's picked. Claude
// Code's install offer asks the user how to be notified: in every session
// (Wakeshift's plugin), only while this session is open, or not at all. The
// steps for "this session" (the link file, the Monitor's command, how to
// treat what arrives) aren't in the offer: a private link, a file to
// write and a command, offered before the user has chosen, read to agents
// as an injection and the offer was set aside. The offer carries a one-time URL instead,
// which the agent fetches with curl only once the user picks this session:
//
//	GET /v1/stream/setup?token=wsu_<tenant>_<secret>
//
// It answers once (200, text/plain) with this session's steps, for the
// session the token was minted for, with that session's link; within
// SetupTTL of the offer. Every refusal (malformed, unknown, another
// workspace's, used, expired) is the same 410, byte for byte: it says
// what to do and reveals nothing. Under /v1/stream, whose query strings
// no request log keeps; the token is never logged.
//
// It's a fetch of instructions from Wakeshift, not a call to the app's
// tools: the app's tool, which may have side effects, is never called again
// (no result asks the agent for another call).

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/harnesses"
	"github.com/wakeshift/server/internal/sessions"
	"github.com/wakeshift/server/internal/streams"
)

// SetupPath is the session-only setup's route.
const SetupPath = "/v1/stream/setup"

// SetupTTL is how long a session-only setup URL works after the offer that
// gave it: an hour, so a user who steps away before answering the question
// still gets the steps when they pick "this session". It's a bearer token
// on a command line until it's used, so not longer: single use, one
// conversation's steps, never the stream link itself (an hour was chosen
// over 15 minutes).
const SetupTTL = time.Hour

var setupTokenRE = regexp.MustCompile(`^wsu_([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})_[A-Za-z0-9_-]{43}$`)

// SetupRefused is every refused setup URL's answer.
const SetupRefused = "This Wakeshift setup link has expired or was already used (it works once, for an hour after the result that gave it), so it can't print the steps again. " +
	"If you already started this session's listener from it, it's running, and nothing more is needed. " +
	"Otherwise, tell the user that notifications for this session couldn't be set up from this link. " +
	"The next result from the app's tools that subscribes this conversation carries a new one, and installing Wakeshift's plugin (the other option) still works.\n"

// mintSetup stores a one-time setup token for the conversation's session
// and returns its URL.
func (a *API) mintSetup(ctx context.Context, tx pgx.Tx, cv *conversation) (string, error) {
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return "", err
	}
	token := "wsu_" + cv.tenant + "_" + base64.RawURLEncoding.EncodeToString(secret)
	if _, err := tx.Exec(ctx, `INSERT INTO session_setups (tenant_id, token_hash, session_id, app_id, grant_id, expires_at)
		VALUES (current_tenant(), $1, $2, $3, $4, now() + $5::interval)`, ticketHash(token), cv.session.ID, cv.app.ID, cv.act.grant.ID, SetupTTL.String()); err != nil {
		return "", err
	}
	return strings.TrimSuffix(a.base(), "/") + SetupPath + "?token=" + token, nil
}

// serveSetup serves GET /v1/stream/setup.
func (a *API) serveSetup(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodHead {
		// A link preview or scanner's HEAD never uses the token up.
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		return
	}
	tok := r.URL.Query().Get("token")
	if len(tok) > 128 { // a token is 84 characters
		tok = ""
	}
	m := setupTokenRE.FindStringSubmatch(tok)
	if m == nil {
		setupAnswer(w, http.StatusGone, SetupRefused)
		return
	}
	tenant := m[1]
	var text string
	err := a.DB.InTenant(r.Context(), tenant, func(tx pgx.Tx) error {
		var err error
		text, err = a.setupFor(r.Context(), tx, tenant, ticketHash(m[0]))
		return err
	})
	switch {
	case errors.Is(err, errSetupGone):
		slog.Info("appapi: session setup refused") // never the token, nor whose
		setupAnswer(w, http.StatusGone, SetupRefused)
	case err != nil:
		slog.Error("appapi: session setup failed", "err", err)
		w.Header().Set("Retry-After", "2")
		setupAnswer(w, http.StatusServiceUnavailable, "Wakeshift couldn't answer just now. Run the same curl again in a few seconds.\n")
	default:
		slog.Info("appapi: session setup served", "tenant", tenant)
		setupAnswer(w, http.StatusOK, text)
	}
}

var errSetupGone = errors.New("appapi: setup token unknown, used or expired")

// setupFor uses up the setup token whose hash is hash, in the transaction's
// workspace, and returns its session's steps.
func (a *API) setupFor(ctx context.Context, tx pgx.Tx, tenant string, hash []byte) (string, error) {
	var session, app, grant string
	err := tx.QueryRow(ctx, `UPDATE session_setups SET used_at = now() WHERE token_hash = $1 AND link_id IS NULL AND used_at IS NULL AND expires_at > now()
		RETURNING session_id::text, app_id, grant_id::text`, hash).Scan(&session, &app, &grant)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", errSetupGone
	}
	if err != nil {
		return "", err
	}
	name := appNameTx(ctx, tx, app)
	// The grant, then the session's row, held (as a redeem holds the
	// grant): a revocation or an unwatch in progress is waited for, so it
	// can't miss a link minted below.
	if _, err := tx.Exec(ctx, `SELECT 1 FROM grants WHERE id = $1 FOR SHARE`, grant); err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx, `SELECT 1 FROM sessions WHERE id = $1 FOR SHARE`, session); err != nil {
		return "", err
	}
	s, err := sessions.Get(ctx, tx, session)
	if errors.Is(err, sessions.ErrNotFound) {
		return harnesses.SetupEnded(name), nil
	}
	if err != nil {
		return "", err
	}
	// Only hooks whose waiter listens: a redeem by a session that can't be
	// woken (claude -p, a one-shot Agent SDK query) gets the Monitor (#1272).
	if hooks, err := hooksDeliver(ctx, tx, s); err != nil {
		return "", err
	} else if hooks {
		return harnesses.SetupPluginConnected(name), nil
	}
	var live bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM grants WHERE id = $1 AND revoked_at IS NULL AND expires_at > now())`, grant).Scan(&live); err != nil {
		return "", err
	}
	if !s.Watching || !live {
		return harnesses.SetupEnded(name), nil
	}
	link, err := a.sessionLinkFor(ctx, tx, tenant, s, app, grant)
	if err != nil {
		return "", err
	}
	return harnesses.SessionSetupNote(name, streams.WaitURL(link)), nil
}

func setupAnswer(w http.ResponseWriter, status int, text string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	w.Write([]byte(text))
}
