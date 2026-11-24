package appapi

// Link claims (#1152). A background
// command reads its conversation's link from a file (harnesses.LinkStep).
// Cursor's command carries a one-time code, and the first time it runs,
// finding no link file, it exchanges the code for the link and writes the
// file itself (harnesses.Claim), so the agent never handles the link:
//
//	POST /v1/stream/claim   token=wso_<tenant>_<secret>[&conversation=<id>]
//
// It answers once (200, text/plain) with the wait URL of the link the
// result named (never a newer one: the file's name is that link's), within
// SetupTTL of the result, while the link works and the session still
// watches under a live grant, and only while no background command waits
// on the session: a code can't take the link from a command that's
// listening on it. Every refusal is the same
// 410, byte for byte. The code is a session_setups row with link_id set
// (migration 160), so it's used up when the session's links are revoked,
// and cleaned up, as a setup URL is.
//
// The command also sends the conversation's ID from its environment
// (Cursor's CURSOR_CONVERSATION_ID): a session that doesn't know its
// conversation yet learns it here, once, as a call that names it would
// teach it. In a workspace the app's user is signed in to, that's what
// later calls find the conversation by (passesKey): its ID, not a key.

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/harnesses"
	"github.com/wakeshift/server/internal/sessions"
	"github.com/wakeshift/server/internal/streams"
)

// LinkClaimPath is the link claim's route.
const LinkClaimPath = "/v1/stream/claim"

// LinkClaimPrefix starts every link claim's code.
const LinkClaimPrefix = "wso_"

var linkClaimRE = regexp.MustCompile(`^wso_([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})_[A-Za-z0-9_-]{43}$`)

// LinkClaimRefused is every refused claim's answer. The command doesn't
// print it (it prints its own hub.closed line); it's for a person who
// tries the request by hand.
const LinkClaimRefused = "This Wakeshift code has expired or was already used (it works once, within an hour of the result that gave it).\n"

// mintLinkClaim stores a one-time code that hands over link (a stream or
// wait URL the session's result names) to the command it's in.
func (a *API) mintLinkClaim(ctx context.Context, tx pgx.Tx, cv *conversation, link string) (harnesses.Claim, error) {
	tenant, id, ok := a.Streams.Held(link)
	if !ok || tenant != cv.tenant {
		return harnesses.Claim{}, errors.New("appapi: link claim for a link that isn't this workspace's")
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return harnesses.Claim{}, err
	}
	token := LinkClaimPrefix + cv.tenant + "_" + base64.RawURLEncoding.EncodeToString(secret)
	if _, err := tx.Exec(ctx, `INSERT INTO session_setups (tenant_id, token_hash, session_id, app_id, grant_id, expires_at, link_id)
		VALUES (current_tenant(), $1, $2, $3, $4, now() + $5::interval, $6)`, ticketHash(token), cv.session.ID, cv.app.ID, cv.act.grant.ID, SetupTTL.String(), id); err != nil {
		return harnesses.Claim{}, err
	}
	return harnesses.Claim{URL: strings.TrimSuffix(a.base(), "/") + LinkClaimPath, Token: token}, nil
}

// serveLinkClaim serves POST /v1/stream/claim.
func (a *API) serveLinkClaim(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1024)
	if err := r.ParseForm(); err != nil {
		setupAnswer(w, http.StatusGone, LinkClaimRefused)
		return
	}
	m := linkClaimRE.FindStringSubmatch(r.PostForm.Get("token"))
	if m == nil {
		setupAnswer(w, http.StatusGone, LinkClaimRefused)
		return
	}
	conversation := strings.TrimSpace(r.PostForm.Get("conversation"))
	if !conversationArgRE.MatchString(conversation) {
		conversation = ""
	}
	tenant := m[1]
	var url string
	err := a.DB.InTenant(r.Context(), tenant, func(tx pgx.Tx) error {
		var err error
		url, err = a.linkClaimFor(r.Context(), tx, tenant, ticketHash(m[0]), conversation)
		return err
	})
	switch {
	case errors.Is(err, errSetupGone) || err == nil && url == "":
		slog.Info("appapi: link claim refused") // never the code, nor whose
		setupAnswer(w, http.StatusGone, LinkClaimRefused)
	case err != nil:
		slog.Error("appapi: link claim failed", "err", err)
		w.Header().Set("Retry-After", "2")
		setupAnswer(w, http.StatusServiceUnavailable, "Wakeshift couldn't answer just now.\n")
	default:
		slog.Info("appapi: link claimed", "tenant", tenant)
		setupAnswer(w, http.StatusOK, url+"\n")
	}
}

// linkClaimFor uses up the claim whose hash is hash, in the transaction's
// workspace, and returns the wait URL of the link it names: errSetupGone
// for a code that isn't there (unknown, used or expired), and "" for one
// it won't hand over, with no error, so the transaction commits and the
// code is used up all the same: a code is tried once.
func (a *API) linkClaimFor(ctx context.Context, tx pgx.Tx, tenant string, hash []byte, conversation string) (string, error) {
	var session, grant, link string
	err := tx.QueryRow(ctx, `UPDATE session_setups SET used_at = now() WHERE token_hash = $1 AND link_id IS NOT NULL AND used_at IS NULL AND expires_at > now()
		RETURNING session_id::text, grant_id::text, link_id::text`, hash).Scan(&session, &grant, &link)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", errSetupGone
	}
	if err != nil {
		return "", err
	}
	// The grant, then the session's row, held (as a redeem holds the
	// grant): a revocation or an unwatch in progress is waited for.
	if _, err := tx.Exec(ctx, `SELECT 1 FROM grants WHERE id = $1 FOR SHARE`, grant); err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx, `SELECT 1 FROM sessions WHERE id = $1 FOR UPDATE`, session); err != nil {
		return "", err
	}
	s, err := sessions.Get(ctx, tx, session)
	if errors.Is(err, sessions.ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	var live bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM grants WHERE id = $1 AND revoked_at IS NULL AND expires_at > now())`, grant).Scan(&live); err != nil {
		return "", err
	}
	if !s.Watching || !live {
		return "", nil
	}
	url, l, err := a.Streams.URLOf(ctx, tx, tenant, link)
	if errors.Is(err, streams.ErrLinkInvalid) || err == nil && (l.Target != s.ID || l.Agent != s.Agent) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	// A command is listening on the session: the code came from somewhere
	// else (a screen, a transcript), or from a second copy of a command
	// that's already running and has its file.
	if listening, err := backgroundLive(ctx, tx, s); err != nil {
		return "", err
	} else if listening {
		return "", nil
	}
	if conversation != "" && s.Conversation == "" {
		// The command runs in the conversation: its ID, once (a later
		// one never moves it), unless another session of the agent has it.
		if _, err := tx.Exec(ctx, `UPDATE sessions SET conversation = $2 WHERE id = $1 AND conversation IS NULL
			AND NOT EXISTS (SELECT 1 FROM sessions o WHERE o.agent_id = sessions.agent_id AND o.conversation = $2)`, s.ID, conversation); err != nil {
			return "", err
		}
	}
	return streams.WaitURL(url), nil
}
