package auth

// The waitlist page's side of sign-in (accounts.WaitlistedError): a
// sign-in past the sign-up cap makes no account and no session, so a
// short-lived signed cookie names the person's waitlist entry instead. The
// session endpoint shows the entry with it, and the person can add a note
// on what they'll build. Signing in again finds the same entry.
//
// Opened from @wakeshift/cli's /new-app link, the page also holds the
// link's public key against the entry (waitlistKey, apps.HoldPendingKey),
// so the CLI, polling with that key, hears it's waitlisted.

import (
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/wakeshift/server/internal/accounts"
	"github.com/wakeshift/server/internal/appkey"
	"github.com/wakeshift/server/internal/apps"
	"github.com/wakeshift/server/internal/httpx"
)

const (
	waitlistCookie = "ws_waitlist"
	waitlistPath   = "/api/v1"
	waitlistTTL    = 7 * 24 * time.Hour
)

type waitlistClaims struct {
	Entry string `json:"w"`
	Exp   int64  `json:"e"`
}

// onWaitlist keeps the person's entry for the waitlist page.
func (c *Console) onWaitlist(w http.ResponseWriter, entry string) {
	exp := c.now().Add(waitlistTTL)
	http.SetCookie(w, c.cookie(waitlistCookie, waitlistPath, c.sign("waitlist", waitlistClaims{Entry: entry, Exp: exp.Unix()}), exp, http.SameSiteStrictMode))
}

// waitlistEntry is the entry the request's cookie names, if it's valid.
func (c *Console) waitlistEntry(r *http.Request) (string, bool) {
	ck, err := r.Cookie(waitlistCookie)
	var cl waitlistClaims
	if err != nil || !c.verify("waitlist", ck.Value, &cl) || cl.Entry == "" || c.now().Unix() > cl.Exp {
		return "", false
	}
	return cl.Entry, true
}

// waitlist is the entry for the session endpoint: nil without one (none,
// removed, or signed up since).
func (c *Console) waitlist(r *http.Request) *accounts.WaitlistEntry {
	id, ok := c.waitlistEntry(r)
	if !ok {
		return nil
	}
	e, err := c.Accounts.WaitlistEntry(r.Context(), id)
	if err != nil {
		if !errors.Is(err, accounts.ErrNoEntry) {
			slog.Warn("auth: reading a waitlist entry failed", "err", err)
		}
		return nil
	}
	return &e
}

// waitlistNote sets the note on the person's entry: what they'll build.
func (c *Console) waitlistNote(w http.ResponseWriter, r *http.Request) {
	if !c.sameOrigin(r) {
		httpx.WriteError(w, http.StatusForbidden, "bad_origin", "request must come from the console")
		return
	}
	id, ok := c.waitlistEntry(r)
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthenticated", "sign in again to see your place on the waitlist")
		return
	}
	var in struct {
		Note string `json:"note"`
	}
	if !httpx.Decode(w, r, &in) {
		return
	}
	switch err := c.Accounts.SetWaitlistNote(r.Context(), id, in.Note); {
	case err == nil:
		httpx.WriteJSON(w, http.StatusOK, map[string]bool{"saved": true})
	case errors.Is(err, accounts.ErrBadNote):
		httpx.WriteError(w, http.StatusBadRequest, "bad_request", "keep it to 500 characters")
	case errors.Is(err, accounts.ErrNoEntry):
		httpx.WriteError(w, http.StatusNotFound, "not_found", "you're not on the waitlist any more; sign in again")
	default:
		slog.Warn("auth: saving a waitlist note failed", "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, "internal", "couldn't save that; try again")
	}
}

// waitlistKey holds the public key from @wakeshift/cli's link against the
// person's entry for a day (apps.HoldPendingKey), so the CLI's polls with
// that key get 403 waitlisted. Only the public key is sent; nothing is
// created, and the key authenticates nothing.
func (c *Console) waitlistKey(w http.ResponseWriter, r *http.Request) {
	if !c.sameOrigin(r) {
		httpx.WriteError(w, http.StatusForbidden, "bad_origin", "request must come from the console")
		return
	}
	id, ok := c.waitlistEntry(r)
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthenticated", "sign in again to see your place on the waitlist")
		return
	}
	var in struct {
		PublicKey string `json:"public_key"`
	}
	if !httpx.Decode(w, r, &in) {
		return
	}
	pub, err := apps.ParsePublicKey(in.PublicKey)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "invalid_key", "the link's key isn't valid: run the command again for a new link")
		return
	}
	fresh, err := apps.HoldPendingKey(r.Context(), c.Accounts.Pool, id, pub, c.now())
	switch {
	case err == nil:
		if fresh {
			slog.Info("auth: a waitlist entry holds a key from the CLI", "entry", id, "key_id", appkey.ShortID(pub))
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]bool{"held": true})
	case errors.Is(err, apps.ErrKeyUsed):
		httpx.WriteError(w, http.StatusConflict, "key_used", "that key is taken: run the command again for a new link")
	case errors.Is(err, apps.ErrTooManyPendingKeys):
		httpx.WriteError(w, http.StatusTooManyRequests, "rate_limited", "your terminal has asked too often today; try again tomorrow")
	case errors.Is(err, apps.ErrNoPendingEntry):
		httpx.WriteError(w, http.StatusNotFound, "not_found", "you're not on the waitlist any more; sign in again")
	default:
		slog.Warn("auth: holding a waitlist key failed", "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, "internal", "couldn't tell your terminal; try again")
	}
}
