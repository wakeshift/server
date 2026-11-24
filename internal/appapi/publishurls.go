package appapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/apps"
	"github.com/wakeshift/server/internal/httpx"
	"github.com/wakeshift/server/internal/mcpevents"
)

// Publish URLs: a capability URL that emits one of the app's events, with
// the arguments it was made for, for services that can only send webhooks.
// Anyone with the URL can emit that occurrence (and only that one) until the
// app revokes it, within a size and rate limit; its body is the payload,
// checked against the event's schema. The token is stored only as its hash.

var publishTokenRE = regexp.MustCompile(`^wsp_[A-Za-z0-9_-]{43}$`)

// createEventPublishURL makes a publish URL for one of the app's events:
// a POST's body is an occurrence's payload, emitted with these arguments
// (and, for a personal event, to that one subscriber).
func (a *API) createEventPublishURL(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
		To        string          `json:"to"`
		// A Standard Webhooks whsec_ secret: POSTs must be signed with it.
		SigningSecret string `json:"signing_secret"`
	}
	if !httpx.Decode(w, r, &in) {
		return
	}
	app := AppFrom(r.Context())
	out, err := a.CreateEventPublishURL(r.Context(), app, in.Name, in.Arguments, in.To, in.SigningSecret, "app:"+app.ID)
	if bad := (SigningSecretError{}); errors.As(err, &bad) {
		httpx.WriteError(w, http.StatusBadRequest, "bad_signing_secret", bad.Error())
		return
	}
	a.respondEventSub(w, out, err)
}

// revokePublishURL stops one of the app's publish URLs working (410 from
// then on).
func (a *API) revokePublishURL(w http.ResponseWriter, r *http.Request) {
	app := AppFrom(r.Context()).ID
	err := a.RevokePublishURL(r.Context(), app, r.PathValue("id"), "app:"+app)
	if errors.Is(err, ErrNoPublishURL) {
		httpx.WriteError(w, http.StatusNotFound, "unknown_publish_url", "no such publish URL")
		return
	}
	a.respond(w, map[string]bool{"revoked": true}, err)
}

var uuidRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// publishByURL is POST /v1/publish/{token}: the body becomes an
// occurrence of the URL's event (eventByURL), emitted with its arguments.
// An Idempotency-Key (or GitHub's X-GitHub-Delivery, or Standard Webhooks'
// webhook-id) header makes retries publish once. A URL with a signing
// secret takes only POSTs signed with it, deduped by their signed
// webhook-id alone (publishsigning.go).
func (a *API) publishByURL(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	if !publishTokenRE.MatchString(token) {
		httpx.WriteError(w, http.StatusNotFound, "unknown_publish_url", "no such publish URL")
		return
	}
	var id, app, topic, event, args string
	var revoked, suspended bool
	var secret, prevSecret []byte
	var prevUntil *time.Time
	err := a.DB.Pool.QueryRow(r.Context(), `SELECT u.id::text, u.app_id, u.topic, coalesce(u.event, ''), coalesce(u.arguments::text, '{}'),
		u.revoked_at IS NOT NULL, a.suspended_at IS NOT NULL, u.signing_secret, u.previous_signing_secret, u.previous_signing_until
		FROM topic_publish_urls u JOIN apps a ON a.id = u.app_id WHERE u.token_hash = $1`,
		ticketHash(token)).Scan(&id, &app, &topic, &event, &args, &revoked, &suspended, &secret, &prevSecret, &prevUntil)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		httpx.WriteError(w, http.StatusNotFound, "unknown_publish_url", "no such publish URL")
		return
	case err != nil:
		slog.Error("appapi: looking up a publish URL", "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, "internal", "try again")
		return
	case revoked:
		httpx.WriteError(w, http.StatusGone, "revoked", "this publish URL was revoked")
		return
	case suspended:
		httpx.WriteError(w, http.StatusForbidden, "suspended", apps.SuspendedMessage(a.SupportEmail))
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxTopicData+1))
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "bad_request", "couldn't read the body")
		return
	}
	if len(body) > maxTopicData {
		httpx.WriteError(w, http.StatusRequestEntityTooLarge, "too_large", "the body is at most 64 KB")
		return
	}
	if event == "" {
		// A URL for a topic, made before publish URLs moved onto events.
		httpx.WriteError(w, http.StatusGone, "retired", "publish URLs for topics were retired: make one for one of the app's events")
		return
	}
	now := a.now()
	keys, err := a.signingSecrets(id, secret, prevSecret, prevUntil, now)
	if err != nil {
		slog.Error("appapi: opening a publish URL's signing secret", "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, "internal", "try again")
		return
	}
	// Unsigned, any of the headers senders retry with; signed, only the
	// signed webhook-id, so a captured request can't be replayed with a
	// fresh key.
	key := cmpOr(cmpOr(r.Header.Get("Idempotency-Key"), r.Header.Get("X-GitHub-Delivery")), cmpOr(r.Header.Get("webhook-id"), r.Header.Get("svix-id")))
	if keys != nil {
		signedID, err := mcpevents.VerifySigned(r.Header, body, now, keys...)
		if err != nil {
			httpx.WriteError(w, http.StatusUnauthorized, "invalid_signature", err.Error())
			return
		}
		key = signedID
	}
	ev, ok := a.eventByURL(r.Context(), w, app, event, []byte(args), body)
	if !ok {
		return
	}
	if key != "" && len(key) <= 150 {
		ev.ID = "url:" + id + ":" + key
	}
	if len(ev.Data) > maxTopicData { // wrapped: a little larger than the body
		httpx.WriteError(w, http.StatusRequestEntityTooLarge, "too_large", "the body is at most 64 KB")
		return
	}
	matched, dup, err := a.publishTopics(r.Context(), app, []string{topic}, ev, id)
	if errors.Is(err, errRateLimited) {
		w.Header().Set("Retry-After", "60")
		httpx.WriteError(w, http.StatusTooManyRequests, "rate_limited", "at most 60 posts a minute to a publish URL")
		return
	}
	if err != nil {
		slog.Error("appapi: publishing by URL", "app", app, "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, "internal", "try again")
		return
	}
	a.DB.Pool.Exec(r.Context(), `UPDATE topic_publish_urls SET last_used_at = now() WHERE id = $1`, id)
	out := map[string]any{"matched": matched, "duplicate": dup}
	if ev.trimmed {
		out["text_trimmed"] = true
	}
	httpx.WriteJSON(w, http.StatusAccepted, out)
}

// eventByURL is an event publish URL's body as an occurrence of event,
// with the arguments the URL was made for: the body is its payload, checked
// against the event's schema in the app's manifest as it is now, and the
// text is publishText's. An event the app no longer offers is a 410.
func (a *API) eventByURL(ctx context.Context, w http.ResponseWriter, app, event string, args, body []byte) (topicEvent, bool) {
	ap, err := a.Apps.Get(ctx, app)
	var et apps.EventType
	if err == nil {
		et, err = ap.Manifest.EventType(event)
	}
	switch {
	case errors.Is(err, apps.ErrNotFound) || errors.Is(err, apps.ErrUnknownEventType):
		httpx.WriteError(w, http.StatusGone, "unknown_event", "the app no longer offers this event")
		return topicEvent{}, false
	case err != nil:
		slog.Error("appapi: reading an event publish URL's app", "app", app, "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, "internal", "try again")
		return topicEvent{}, false
	}
	payload := bytes.TrimSpace(body)
	if len(payload) == 0 || payload[0] != '{' || !json.Valid(payload) {
		httpx.WriteError(w, http.StatusBadRequest, "bad_payload", "the body is the event's payload: a JSON object")
		return topicEvent{}, false
	}
	if err := et.CheckPayload(payload); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "bad_payload", err.Error())
		return topicEvent{}, false
	}
	ev := topicEvent{Data: payload, Text: publishText(et, args), deleteOnDelivery: et.DeleteOnDelivery}
	ev.Text, ev.trimmed = trimText(ev.Text) // a long description
	return ev, true
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// publishText is the line the agent reads for a publish URL's occurrence,
// since the app's own text function doesn't run: the event's default text,
// as every SDK writes it for an event defined without text (its description
// with the URL's arguments, "A crawl finished (job j1)":
// sdk/typescript/test/default-text.json), or its name when it has no
// description. Never the payload: that is the event's data, and a Monitor
// line (streams.MonitorLineMax) keeps the data whole only while the subject
// doesn't repeat it.
func publishText(et apps.EventType, args []byte) string {
	if t := defaultText(et.Description, args); t != "" {
		return t
	}
	return et.Name
}

// defaultText is an event's line without text, the same as the SDKs'
// (pkg/wakeshift defaultText): the description, its whitespace collapsed
// and trailing periods and spaces trimmed, with the arguments in name
// order: null and empty strings skipped, strings raw, other values compact
// JSON as JavaScript writes it.
func defaultText(description string, args []byte) string {
	d := strings.TrimRight(strings.Join(strings.Fields(description), " "), ". ")
	var obj map[string]any
	dec := json.NewDecoder(bytes.NewReader(args))
	dec.UseNumber()
	if dec.Decode(&obj) != nil || len(obj) == 0 {
		return d
	}
	var parts []string
	for _, k := range slices.Sorted(maps.Keys(obj)) {
		switch v := obj[k].(type) {
		case nil:
		case string:
			if v != "" {
				parts = append(parts, k+" "+v)
			}
		default:
			parts = append(parts, k+" "+jsJSON(v))
		}
	}
	if len(parts) == 0 {
		return d
	}
	return d + " (" + strings.Join(parts, ", ") + ")"
}

// jsJSON is v (decoded with UseNumber) as JavaScript's JSON.stringify
// writes it: <, > and & unescaped, a number with a fraction or an exponent
// as a float64 (1.0 is 1), integers with every digit.
func jsJSON(v any) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(jsNumbers(v)); err != nil {
		return ""
	}
	return strings.TrimSuffix(buf.String(), "\n")
}

func jsNumbers(v any) any {
	switch v := v.(type) {
	case json.Number:
		if strings.ContainsAny(string(v), ".eE") {
			if f, err := v.Float64(); err == nil {
				return f
			}
		}
	case []any:
		out := make([]any, len(v))
		for i, x := range v {
			out[i] = jsNumbers(x)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, x := range v {
			out[k] = jsNumbers(x)
		}
		return out
	}
	return v
}
