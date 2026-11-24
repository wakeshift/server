package appapi

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/apps"
	"github.com/wakeshift/server/internal/httpx"
	"github.com/wakeshift/server/internal/mcpevents"
)

// Signed publish URLs (site/docs/publish-urls.md, "Signed webhooks"): a
// publish URL with a signing secret, a Standard Webhooks whsec_ secret from
// the sender (a Svix endpoint's, say), takes only POSTs signed with it
// (mcpevents.VerifySigned), and dedupes them by their signed webhook-id.
// The secret is sealed at rest (PublishSecrets), bound to the URL's ID,
// never logged and never returned: only its fingerprint is. Setting a new
// one keeps the previous one working for SigningGrace, so the sender can
// roll over.

// SigningGrace is how long a publish URL's previous signing secret is
// still accepted after a new one is set.
const SigningGrace = 24 * time.Hour

// Signing is a publish URL's signing secret, as it's shown: its
// fingerprint (the first 12 hex digits of the SHA-256 of the whsec_ string,
// so the developer can tell which one is set), when it was set, and until
// when the previous one is still accepted.
type Signing struct {
	Fingerprint        string     `json:"fingerprint"`
	SetAt              time.Time  `json:"set_at"`
	PreviousValidUntil *time.Time `json:"previous_valid_until,omitempty"`
}

// SigningSecretError is a signing secret that isn't a Standard Webhooks
// whsec_ secret. Its message never carries the secret.
type SigningSecretError struct{ Msg string }

func (e SigningSecretError) Error() string { return e.Msg }

// ErrPublishURLRevoked is a change to a revoked publish URL.
var ErrPublishURLRevoked = errors.New("this publish URL was revoked")

// signingBinding is what a publish URL's sealed secret is bound to.
func signingBinding(id string) string { return "publish-url-signing:" + id }

// parseSigningSecret is a whsec_ secret's key and fingerprint.
func parseSigningSecret(s string) ([]byte, string, error) {
	s = strings.TrimSpace(s)
	key, err := mcpevents.ParseSecret(s)
	if err != nil {
		return nil, "", SigningSecretError{"signing_secret must be a Standard Webhooks secret: whsec_ and the base64 of 24 to 64 bytes"}
	}
	sum := sha256.Sum256([]byte(s))
	return key, hex.EncodeToString(sum[:6]), nil
}

// setSigningSecret sets publish URL id's signing secret in tx (the caller
// holds its row): a different secret from the one set rotates it, keeping
// the old one for SigningGrace; the same one changes nothing. action is
// what to log ("" for nothing changed).
func (a *API) setSigningSecret(ctx context.Context, tx pgx.Tx, id string, key []byte, fp string, now time.Time) (Signing, string, error) {
	if a.PublishSecrets == nil {
		return Signing{}, "", errors.New("appapi: no key to seal publish URLs' signing secrets")
	}
	var cur []byte
	var setAt, prevUntil *time.Time
	if err := tx.QueryRow(ctx, `SELECT signing_secret, signing_set_at, previous_signing_until FROM topic_publish_urls WHERE id = $1`, id).
		Scan(&cur, &setAt, &prevUntil); err != nil {
		return Signing{}, "", err
	}
	if cur == nil {
		_, err := tx.Exec(ctx, `UPDATE topic_publish_urls SET signing_secret = $2, signing_fingerprint = $3, signing_set_at = $4,
			previous_signing_secret = NULL, previous_signing_until = NULL WHERE id = $1`, id, a.PublishSecrets.Seal(signingBinding(id), key), fp, now)
		return Signing{Fingerprint: fp, SetAt: now}, apps.LogPublishURLSecretSet, err
	}
	old, err := a.PublishSecrets.Open(signingBinding(id), cur)
	if err != nil {
		return Signing{}, "", err
	}
	if subtle.ConstantTimeCompare(old, key) == 1 {
		s := Signing{Fingerprint: fp, SetAt: *setAt}
		if prevUntil != nil && prevUntil.After(now) {
			s.PreviousValidUntil = prevUntil
		}
		return s, "", nil
	}
	until := now.Add(SigningGrace)
	_, err = tx.Exec(ctx, `UPDATE topic_publish_urls SET previous_signing_secret = signing_secret, previous_signing_until = $5,
		signing_secret = $2, signing_fingerprint = $3, signing_set_at = $4 WHERE id = $1`, id, a.PublishSecrets.Seal(signingBinding(id), key), fp, now, until)
	return Signing{Fingerprint: fp, SetAt: now, PreviousValidUntil: &until}, apps.LogPublishURLSecretRotated, err
}

// lockPublishURL holds one of app's live publish URLs (FOR UPDATE) until tx
// ends, and returns its event: ErrNoPublishURL if the app has none by that
// ID, ErrPublishURLRevoked if it was revoked.
func lockPublishURL(ctx context.Context, tx pgx.Tx, app, id string) (string, error) {
	if !uuidRE.MatchString(id) {
		return "", ErrNoPublishURL
	}
	if err := liveApp(ctx, tx, app); err != nil {
		return "", err
	}
	var event string
	var revoked bool
	err := tx.QueryRow(ctx, `SELECT coalesce(event, ''), revoked_at IS NOT NULL FROM topic_publish_urls WHERE id = $1 AND app_id = $2 FOR UPDATE`, id, app).Scan(&event, &revoked)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return "", ErrNoPublishURL
	case err != nil:
		return "", err
	case revoked:
		return "", ErrPublishURLRevoked
	}
	return event, nil
}

// SetPublishURLSecret sets or rotates one of app's publish URLs' signing
// secret, and logs it in the app's log: from then on a POST must be signed
// with it (or, for SigningGrace after a rotation, with the previous one).
func (a *API) SetPublishURLSecret(ctx context.Context, app, id, secret, by string) (Signing, error) {
	key, fp, err := parseSigningSecret(secret)
	if err != nil {
		return Signing{}, err
	}
	var out Signing
	err = pgx.BeginFunc(ctx, a.DB.Pool, func(tx pgx.Tx) error {
		event, err := lockPublishURL(ctx, tx, app, id)
		if err != nil {
			return err
		}
		s, action, err := a.setSigningSecret(ctx, tx, id, key, fp, a.now())
		out = s
		if err != nil || action == "" {
			return err
		}
		return apps.Log(ctx, tx, apps.LogEntry{AppID: app, Actor: by, Action: action,
			Detail: map[string]string{"publish_url": id, "event": event, "fingerprint": fp}})
	})
	return out, err
}

// RemovePublishURLSecret removes one of app's publish URLs' signing secret,
// the previous one too, and logs it: from then on its POSTs aren't checked.
// removed is false if it had none.
func (a *API) RemovePublishURLSecret(ctx context.Context, app, id, by string) (removed bool, err error) {
	err = pgx.BeginFunc(ctx, a.DB.Pool, func(tx pgx.Tx) error {
		event, err := lockPublishURL(ctx, tx, app, id)
		if err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `UPDATE topic_publish_urls SET signing_secret = NULL, signing_fingerprint = NULL, signing_set_at = NULL,
			previous_signing_secret = NULL, previous_signing_until = NULL WHERE id = $1 AND signing_secret IS NOT NULL`, id)
		if err != nil || tag.RowsAffected() == 0 {
			return err
		}
		removed = true
		return apps.Log(ctx, tx, apps.LogEntry{AppID: app, Actor: by, Action: apps.LogPublishURLSecretRemoved,
			Detail: map[string]string{"publish_url": id, "event": event}})
	})
	return removed, err
}

// signingSecrets are the keys a POST to publish URL id may be signed with:
// its secret, and the previous one until its grace ends. None: unsigned
// POSTs are taken.
func (a *API) signingSecrets(id string, cur, prev []byte, prevUntil *time.Time, now time.Time) ([][]byte, error) {
	if cur == nil {
		return nil, nil
	}
	if a.PublishSecrets == nil {
		return nil, errors.New("appapi: no key to open publish URLs' signing secrets")
	}
	key, err := a.PublishSecrets.Open(signingBinding(id), cur)
	if err != nil {
		return nil, err
	}
	keys := [][]byte{key}
	if prev != nil && prevUntil != nil && prevUntil.After(now) {
		// One that won't open is left out, which only refuses more: the
		// current secret still works, and the failure is logged.
		old, err := a.PublishSecrets.Open(signingBinding(id), prev)
		if err != nil {
			slog.Error("appapi: opening a publish URL's previous signing secret", "publish_url", id, "err", err)
		} else {
			keys = append(keys, old)
		}
	}
	return keys, nil
}

// putPublishURLSecret is PUT /events/publish-urls/{id}/signing-secret:
// set or rotate the URL's signing secret.
func (a *API) putPublishURLSecret(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Secret string `json:"secret"`
	}
	if !httpx.Decode(w, r, &in) {
		return
	}
	app := AppFrom(r.Context()).ID
	s, err := a.SetPublishURLSecret(r.Context(), app, r.PathValue("id"), in.Secret, "app:"+app)
	if !writeSigningError(w, err) {
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"signing": s})
	}
}

// deletePublishURLSecret is DELETE /events/publish-urls/{id}/signing-secret.
func (a *API) deletePublishURLSecret(w http.ResponseWriter, r *http.Request) {
	app := AppFrom(r.Context()).ID
	removed, err := a.RemovePublishURLSecret(r.Context(), app, r.PathValue("id"), "app:"+app)
	if !writeSigningError(w, err) {
		httpx.WriteJSON(w, http.StatusOK, map[string]bool{"removed": removed})
	}
}

// writeSigningError writes the app API's error response for a change to a
// publish URL's signing secret, and says whether there was one.
func writeSigningError(w http.ResponseWriter, err error) bool {
	var bad SigningSecretError
	switch {
	case err == nil:
		return false
	case errors.As(err, &bad):
		httpx.WriteError(w, http.StatusBadRequest, "bad_signing_secret", bad.Error())
	case errors.Is(err, ErrNoPublishURL):
		httpx.WriteError(w, http.StatusNotFound, "unknown_publish_url", "no such publish URL")
	case errors.Is(err, ErrPublishURLRevoked):
		httpx.WriteError(w, http.StatusGone, "revoked", err.Error())
	case errors.Is(err, apps.ErrDeleted), errors.Is(err, apps.ErrNotFound):
		httpx.WriteError(w, http.StatusNotFound, "not_found", "no such app")
	default:
		slog.Error("appapi: changing a publish URL's signing secret", "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, "internal", "try again")
	}
	return true
}
