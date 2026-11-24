package appapi

// Client requests: a subscribing call's /calls/addition answer can carry
// one request for the app's SDK to make of the MCP client during the call.
// The SDK offers the kinds it can run (callIn.ClientRequests), and runs one
// only within fixed bounds it enforces itself. The first kind is an
// elicitation form: Codex's install offer in Codex's own dialog, so the
// user runs the install with no model step (GPT-6.2 Sol, Codex's default
// since 0.169.1, never relays the text offer). Each outcome the SDK can see
// has its own result text; the SDK reports the outcome with the request's
// token:
//
//	POST /api/apps/v1/requests/outcome {"token": "wse_<tenant>_<secret>", "outcome": "choice=setup"}
//
// The token is one request's, for the app that got it: looked up by its
// SHA-256, in the workspace it names, within RequestTTL. The first outcome
// wins (the same one again is 200, another is 409); every refusal
// (malformed, unknown, expired, another app's or workspace's) is the same
// 404. A forged outcome can only change what later results in that
// conversation say: nothing is installed or granted on any outcome.

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/audit"
	"github.com/wakeshift/server/internal/harnesses"
	"github.com/wakeshift/server/internal/httpx"
)

// RequestElicitForm is the elicitation form kind; RequestTTL is how long a
// request's token takes its outcome.
const (
	RequestElicitForm = "elicit.form"
	RequestTTL        = 15 * time.Minute
)

// The outcomes a form's SDK reports: an answer (choice=<value>), or what it
// saw instead. decline and cancel can come from the client by itself
// (codex exec declines, claude -p cancels), so neither is "Not now".
const (
	OutcomeDecline     = "decline"
	OutcomeCancel      = "cancel"
	OutcomeTimeout     = "timeout"
	OutcomeUnsupported = "unsupported"
)

var (
	outcomeSetup     = harnesses.ElicitField + "=" + harnesses.ElicitSetup
	outcomeLater     = harnesses.ElicitField + "=" + harnesses.ElicitLater
	codexOutcomes    = []string{outcomeSetup, outcomeLater, OutcomeDecline, OutcomeCancel, OutcomeTimeout, OutcomeUnsupported}
	requestTokenRE   = regexp.MustCompile(`^wse_([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})_[A-Za-z0-9_-]{43}$`)
	errRequestGone   = errors.New("appapi: client request unknown, expired or another app's")
	errOutcomeTaken  = errors.New("appapi: client request already has another outcome")
	maxOfferedKinds  = 8
	codexTurnTrigger = "x-codex-turn-metadata.turn_trigger"
	codexModel       = "x-codex-turn-metadata.model"
)

// clientRequest is one request in /calls/addition's answer.
type clientRequest struct {
	Token    string                    `json:"token"`
	Kind     string                    `json:"kind"`
	Message  string                    `json:"message"`
	Schema   harnesses.FormSchema      `json:"schema"`
	Outcomes map[string]requestOutcome `json:"outcomes"`
}

// placement is where an outcome's text goes, relative to the app's own
// content; beaconUse is whether the result keeps its beacon.
type (
	placement string
	beaconUse string
)

const (
	placeBefore placement = "before"
	placeAfter  placement = "after"
	beaconKeep  beaconUse = "keep"
)

// requestOutcome is the result for one outcome: the text that replaces
// Wakeshift's own (never the app's), where it goes, and the beacon.
type requestOutcome struct {
	Text      string    `json:"text"`
	Placement placement `json:"placement"`
	Beacon    beaconUse `json:"beacon"`
}

// offers says whether the call's SDK can run kind.
func offers(call callIn, kind string) bool {
	return len(call.ClientRequests) <= maxOfferedKinds && slices.Contains(call.ClientRequests, kind)
}

// codexElicitSkip is why a Codex conversation's first ask stays the text
// offer, or "" if it gets the form: the app isn't on the setting, the SDK
// doesn't offer the form, the turn isn't the user's (codex exec answers a
// form by itself), or the model relays the text offer and runs the install
// on a yes (GPT-6.1 Sol).
func (a *API) codexElicitSkip(cv *conversation) string {
	switch {
	case !a.CodexElicit.Allows(cv.app.ID):
		return "setting"
	case !offers(cv.call, RequestElicitForm):
		return "not_offered"
	case harnesses.MetaString(cv.call.Meta, codexTurnTrigger) != "user":
		return "turn_trigger"
	case strings.HasPrefix(harnesses.MetaString(cv.call.Meta, codexModel), "gpt-6.1-"):
		return "model"
	}
	return ""
}

// mintCodexElicit stores a request's token for the conversation and
// returns the form, with each outcome's text: the answers' own, and
// fallback (today's text offer) for everything else.
func (a *API) mintCodexElicit(ctx context.Context, tx pgx.Tx, cv *conversation, fallback string) (clientRequest, error) {
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return clientRequest{}, err
	}
	token := "wse_" + cv.tenant + "_" + base64.RawURLEncoding.EncodeToString(secret)
	if _, err := tx.Exec(ctx, `INSERT INTO client_requests (tenant_id, token_hash, session_id, app_id, harness, kind, expires_at)
		VALUES (current_tenant(), $1, $2, $3, $4, $5, now() + $6::interval)`, ticketHash(token), cv.session.ID, cv.app.ID, harnessCodex, RequestElicitForm, RequestTTL.String()); err != nil {
		return clientRequest{}, err
	}
	codex, _ := harnesses.Get(harnessCodex)
	thread := harnesses.Conversation(codex, cv.call.Meta)
	after := func(text string) requestOutcome {
		return requestOutcome{Text: text, Placement: placeAfter, Beacon: beaconKeep}
	}
	return clientRequest{
		Token:   token,
		Kind:    RequestElicitForm,
		Message: harnesses.CodexElicitMessage(a.base(), a.SiteURL, appName(cv), thread),
		Schema:  harnesses.CodexElicitSchema(),
		Outcomes: map[string]requestOutcome{
			outcomeSetup:       {Text: join(harnesses.CodexElicitSetupText(a.base(), thread), passSession(cv)), Placement: placeBefore, Beacon: beaconKeep},
			outcomeLater:       after(join(harnesses.CodexElicitLaterText(), passSession(cv))),
			OutcomeDecline:     after(fallback),
			OutcomeCancel:      after(fallback),
			OutcomeTimeout:     after(fallback),
			OutcomeUnsupported: after(fallback),
		},
	}, nil
}

// claimCodexAsk records the conversation's one full ask atomically, so of
// parallel subscribing calls (Codex makes parallel tool calls) only one
// asks; with form, the choice is pending until the SDK reports it.
func claimCodexAsk(ctx context.Context, tx pgx.Tx, cv *conversation, form bool) (bool, error) {
	var id string
	err := tx.QueryRow(ctx, `UPDATE sessions SET asked_harness = $2,
		install_choice = CASE WHEN $3 THEN 'pending' ELSE install_choice END,
		install_choice_at = CASE WHEN $3 THEN now() ELSE install_choice_at END
		WHERE id = $1 AND asked_harness IS DISTINCT FROM $2 RETURNING id::text`, cv.session.ID, cv.harness, form).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// installChoice is the conversation's answer to the dialog, and whether a
// pending one is still within RequestTTL (the dialog may still be open).
func installChoice(ctx context.Context, tx pgx.Tx, session string) (choice string, fresh bool, err error) {
	err = tx.QueryRow(ctx, `SELECT coalesce(install_choice, ''), coalesce(install_choice_at > now() - $2::interval, false) FROM sessions WHERE id = $1`,
		session, RequestTTL.String()).Scan(&choice, &fresh)
	return choice, fresh, err
}

// codexAskedNote is a later subscribing result's text for a conversation
// that was asked: by the answer to its dialog, else today's AskedNote.
func (a *API) codexAskedNote(ctx context.Context, tx pgx.Tx, cv *conversation) (string, error) {
	codex, _ := harnesses.Get(harnessCodex)
	choice, fresh, err := installChoice(ctx, tx, cv.session.ID)
	if err != nil {
		return "", err
	}
	if choice == "pending" && fresh {
		return harnesses.CodexElicitPendingNote(), nil
	}
	if n, ok := harnesses.CodexElicitAskedNote(choice, a.base(), harnesses.Conversation(codex, cv.call.Meta)); ok {
		return n, nil
	}
	return harnesses.AskedNote(codex, a.base()), nil
}

// requestOutcome serves POST /api/apps/v1/requests/outcome.
func (a *API) requestOutcome(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Token   string `json:"token"`
		Outcome string `json:"outcome"`
	}
	if !httpx.Decode(w, r, &in) {
		return
	}
	if !slices.Contains(codexOutcomes, in.Outcome) {
		httpx.WriteError(w, http.StatusBadRequest, "bad_request", "outcome must be one of "+strings.Join(codexOutcomes, ", "))
		return
	}
	app := AppFrom(r.Context()).ID
	m := requestTokenRE.FindStringSubmatch(in.Token)
	if m == nil {
		httpx.WriteError(w, http.StatusNotFound, "unknown_request", "no request of this app's has that token, or it expired")
		return
	}
	var harness string
	err := a.DB.InTenant(r.Context(), m[1], func(tx pgx.Tx) error {
		var err error
		harness, err = recordOutcome(r.Context(), tx, app, ticketHash(m[0]), in.Outcome)
		return err
	})
	switch {
	case errors.Is(err, errRequestGone):
		httpx.WriteError(w, http.StatusNotFound, "unknown_request", "no request of this app's has that token, or it expired")
	case errors.Is(err, errOutcomeTaken):
		httpx.WriteError(w, http.StatusConflict, "outcome_taken", "this request already has another outcome")
	case err != nil:
		slog.Error("appapi: client request outcome failed", "app", app, "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, "internal", "something went wrong")
	default:
		slog.Info("appapi: client request outcome", "app", app, "harness", harness, "outcome", in.Outcome)
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"recorded": true})
	}
}

// recordOutcome takes the request's first outcome and stores the answer on
// its conversation's session, with an audit entry for an answer the user
// gave (the app reports it: actor app:<id>).
func recordOutcome(ctx context.Context, tx pgx.Tx, app string, hash []byte, outcome string) (string, error) {
	var session, harness string
	var created time.Time
	err := tx.QueryRow(ctx, `UPDATE client_requests SET outcome = $3, outcome_at = now()
		WHERE token_hash = $1 AND app_id = $2 AND outcome IS NULL AND expires_at > now()
		RETURNING session_id::text, harness, created_at`, hash, app, outcome).Scan(&session, &harness, &created)
	if errors.Is(err, pgx.ErrNoRows) {
		var had *string
		err := tx.QueryRow(ctx, `SELECT outcome, harness FROM client_requests WHERE token_hash = $1 AND app_id = $2`, hash, app).Scan(&had, &harness)
		switch {
		case errors.Is(err, pgx.ErrNoRows), err == nil && had == nil:
			return "", errRequestGone // unknown, another app's, or expired unanswered
		case err != nil:
			return "", err
		case *had == outcome:
			return harness, nil // the same report again
		default:
			return "", errOutcomeTaken
		}
	}
	if err != nil {
		return "", err
	}
	choice := strings.TrimPrefix(outcome, harnesses.ElicitField+"=")
	// Only the pending choice this request set (claimCodexAsk sets it in the
	// transaction that makes the request, so at its created_at): a late
	// outcome never overwrites a newer request's.
	if _, err := tx.Exec(ctx, `UPDATE sessions SET install_choice = $2, install_choice_at = now()
		WHERE id = $1 AND install_choice = 'pending' AND install_choice_at = $3`, session, choice, created); err != nil {
		return "", err
	}
	if choice == harnesses.ElicitSetup || choice == harnesses.ElicitLater {
		if err := audit.Record(ctx, tx, audit.Entry{Actor: audit.App(app), Action: audit.InstallElicit, AppID: app,
			Detail: map[string]any{"harness": harness, "outcome": choice, "reported_by": "app"}}); err != nil {
			return "", err
		}
	}
	return harness, nil
}
