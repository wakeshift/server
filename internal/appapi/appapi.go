// Package appapi is the API apps call, under /api/apps/v1.
//
// App-level calls (manifest, events) carry the app's signed
// assertion. Calls for an agent carry a short-lived token naming the app,
// one of the workspaces it keeps and the agent's grant there (from the
// headless app API, oauth/headless.go). Everything an app does for an agent
// stays inside that grant.
package appapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/agenthooks"
	"github.com/wakeshift/server/internal/apps"
	"github.com/wakeshift/server/internal/config"
	"github.com/wakeshift/server/internal/db"
	"github.com/wakeshift/server/internal/dblimit"
	"github.com/wakeshift/server/internal/downloads"
	"github.com/wakeshift/server/internal/httpx"
	"github.com/wakeshift/server/internal/mcpevents"
	"github.com/wakeshift/server/internal/streams"
	"github.com/wakeshift/server/internal/waittask"
)

// API serves the app API.
type API struct {
	DB   *db.DB
	Apps *apps.Registry
	// SupportEmail is where refusals tell app owners to write
	// (WAKESHIFT_SUPPORT_EMAIL; empty: apps.DefaultSupportEmail).
	SupportEmail string
	Streams      *streams.Server
	Issuer       string
	// SiteURL is the public site's URL ("" if none is known): where the
	// docs a refusal links to are (callbackhint.go).
	SiteURL string
	// Keys returns a public signing key by ID, to verify agent tokens.
	Keys func(ctx context.Context, kid string) ([]byte, bool)
	// Downloads says how to install the connector a session needs.
	Downloads *downloads.Server
	// Sign signs connector beacons (Wakeshift's OAuth signing keys).
	Sign func(ctx context.Context, typ string, claims any) (string, error)
	// Events seals MCP Events webhook secrets (events.go); EventsClient
	// verifies callbacks (default: mcpevents.Client, public addresses only).
	Events       *mcpevents.Sealer
	EventsClient *http.Client
	// PublishSecrets seals publish URLs' signing secrets (publishsigning.go):
	// a key of its own, not Events'.
	PublishSecrets *mcpevents.Sealer
	// Tasks makes and opens MCP wait tasks' IDs (tasks.go); nil turns the
	// task path off: conversations get their zero-install path instead.
	Tasks *waittask.Codec
	// Hooks makes and reads agent-hosted webhooks' registration links,
	// HookSeal seals their URLs (a key of its own), and HookWake has a
	// newly registered destination's waiting messages sent at once
	// (agenthooks.go). Hooks nil: Capy gets its fallback command alone.
	Hooks    *agenthooks.Links
	HookSeal *mcpevents.Sealer
	HookWake func(tenant, agent string)
	// Grok turns Grok Bot's webhook wake on (grokhooks.go; the core flag
	// WAKESHIFT_GROK_WEBHOOKS): nil, Grok Bot waits in the foreground only,
	// and the setup and proof routes answer 404.
	Grok *GrokHooks
	// SubscribeVouchers turns subscribe vouchers on (vouchers.go; the core
	// flag WAKESHIFT_SUBSCRIBE_VOUCHERS).
	SubscribeVouchers bool
	// RedeemByToolCall turns redeem by tool call ID on (callredeem.go; the
	// core flag WAKESHIFT_REDEEM_BY_TOOL_CALL, off by default): off, the routes
	// answers 404 and tickets aren't indexed by call.
	RedeemByToolCall bool
	// InlineSessionSteps: Claude Code's install offer carries its
	// session-only steps (the link file, the Monitor) inline, as before,
	// rather than a one-time URL the agent fetches once the user picks that
	// option (sessionsetup.go).
	InlineSessionSteps bool
	// CodexElicit: which apps' Codex conversations may get the install
	// offer as a client request, a form the app's SDK shows in Codex's own
	// dialog (clientrequests.go; config.Config.CodexElicit).
	CodexElicit config.Allowlist
	// Push serves MCP Events push (eventspush.go; the core flag
	// WAKESHIFT_MCP_EVENTS_PUSH): off, POST /events/stream answers 501
	// unsupported and the manifest response doesn't report the feature.
	// PushCursorKey MACs push cursors (never logged).
	Push          bool
	PushCursorKey []byte
	// CallLimits counts redeem by call's limits per client address
	// (registrations and claims store a row for any key; callredeem.go);
	// nil: no limits (tests). CallProxyHops: as GrokHooks.ProxyHops.
	CallLimits    *dblimit.Counter
	CallProxyHops int
	// VerifyLimits counts MCP Events callback verifications per callback
	// host (VerifyPerHost, verifycallback.go); nil: no limit (tests).
	VerifyLimits *dblimit.Counter
	Now          func() time.Time
}

type ctxKey struct{}

// AppFrom returns the authenticated app.
func AppFrom(ctx context.Context) apps.App {
	a, _ := ctx.Value(ctxKey{}).(apps.App)
	return a
}

// Register adds the routes.
func (a *API) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/apps/v1/app", a.requireApp(a.getApp))
	mux.HandleFunc("PUT /api/apps/v1/manifest", a.requireApp(a.putManifest))
	mux.HandleFunc("GET /api/apps/v1/manifest", a.requireApp(a.getManifest))
	// The SDKs' automatic send: merged into the stored manifest, never
	// removing an event (mergeManifest).
	mux.HandleFunc("POST /api/apps/v1/manifest/merge", a.requireApp(a.mergeManifest))

	mux.HandleFunc("POST /api/apps/v1/users/forget", a.requireApp(a.forgetUser))
	// MCP Events (events.go): the app's MCP server passes on a client's
	// events/subscribe and events/unsubscribe.
	mux.HandleFunc("POST /api/apps/v1/events/subscribe", a.requireApp(a.eventsSubscribe))
	mux.HandleFunc("POST /api/apps/v1/events/unsubscribe", a.requireApp(a.eventsUnsubscribe))
	mux.HandleFunc("POST /api/apps/v1/events/poll", a.requireApp(a.eventsPoll))
	// Push (eventspush.go): a stream per client stream, and stdio's cancel.
	mux.HandleFunc("POST /api/apps/v1/events/stream", a.requireApp(a.eventsStream))
	mux.HandleFunc("POST /api/apps/v1/events/stream/end", a.requireApp(a.eventsStreamEnd))
	// Event types (eventsubs.go): subscribing by event name and arguments,
	// listing a user's subscriptions, and emitting.
	mux.HandleFunc("POST /api/apps/v1/event-subscriptions", a.requireApp(a.eventSubscribe))
	mux.HandleFunc("POST /api/apps/v1/event-subscriptions/unsubscribe", a.requireApp(a.eventUnsubscribe))
	mux.HandleFunc("POST /api/apps/v1/event-subscriptions/list", a.requireApp(a.eventSubsList))
	mux.HandleFunc("POST /api/apps/v1/event-subscriptions/end", a.requireApp(a.eventSubEnd))
	mux.HandleFunc("POST /api/apps/v1/events/emit", a.requireApp(a.eventEmit))
	mux.HandleFunc("POST /api/apps/v1/events/status", a.requireApp(a.eventStatus))
	// One block per tool call (calls.go): an SDK's wrapped tool subscribes
	// with defer_addition, and asks once for what its result carries.
	mux.HandleFunc("POST /api/apps/v1/calls/session", a.requireApp(a.callsSession))
	mux.HandleFunc("POST /api/apps/v1/calls/addition", a.requireApp(a.callsAddition))
	mux.HandleFunc("POST /api/apps/v1/requests/outcome", a.requireApp(a.requestOutcome))
	// Wakeshift's wait tool answered with an MCP task (tasks.go): the SDK
	// passes on the tool call, tasks/get and tasks/cancel.
	mux.HandleFunc("POST /api/apps/v1/calls/task", a.requireApp(a.callsTask))
	mux.HandleFunc("POST /api/apps/v1/calls/task/get", a.requireApp(a.callsTaskGet))
	mux.HandleFunc("POST /api/apps/v1/calls/task/cancel", a.requireApp(a.callsTaskCancel))
	// An event's publish URL; revoking takes any of the app's publish URLs.
	mux.HandleFunc("POST /api/apps/v1/events/publish-urls", a.requireApp(a.createEventPublishURL))
	mux.HandleFunc("DELETE /api/apps/v1/events/publish-urls/{id}", a.requireApp(a.revokePublishURL))
	mux.HandleFunc("PUT /api/apps/v1/events/publish-urls/{id}/signing-secret", a.requireApp(a.putPublishURLSecret))
	mux.HandleFunc("DELETE /api/apps/v1/events/publish-urls/{id}/signing-secret", a.requireApp(a.deletePublishURLSecret))
	// A publish URL is its own credential.
	mux.HandleFunc("POST /v1/publish/{token}", a.publishByURL)
	// Anything else under it (a GET from a link unfurler or a browser, a
	// trailing slash) is answered here rather than by the web app, whose
	// route would log the path, which holds the token.
	mux.HandleFunc("/v1/publish/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Allow", http.MethodPost)
		httpx.WriteError(w, http.StatusMethodNotAllowed, "method_not_allowed", "a publish URL takes a POST with the event as its body")
	})

	agent := func(pattern string, h http.HandlerFunc) { mux.HandleFunc(pattern, a.requireAgent(h)) }
	agent("POST /api/apps/v1/agent/stream-links", a.streamLink)
	agent("GET /api/apps/v1/agent/sessions", a.listSessions)
	agent("POST /api/apps/v1/agent/sessions", a.registerSession)
	agent("POST /api/apps/v1/agent/sessions/{handle}/unwatch", a.unwatch)
	// Pull delivery (wait.go): a wait tool, its acks, and catch-up.
	agent("POST /api/apps/v1/agent/wait", a.wait)
	agent("POST /api/apps/v1/agent/acks", a.ackDeliveries)
	agent("POST /api/apps/v1/agent/seen", a.seen)
	agent("GET /api/apps/v1/agent/sessions/{handle}/inbox", a.inbox)
	// An agent-hosted webhook's registration link (agenthooks.go): no other
	// credential. Under /v1/stream, whose query strings no log keeps.
	mux.HandleFunc("POST "+agenthooks.RegisterPath, a.agentWebhook)
	mux.HandleFunc("DELETE "+agenthooks.RegisterPath, a.agentWebhook)
	// Grok Bot's webhook setup (grokhooks.go): a challenge or a setup token
	// in the body is the only credential.
	mux.HandleFunc("POST "+GrokProofPath, a.grokProof)
	mux.HandleFunc("POST "+GrokFetchPath, a.grokFetch)
	mux.HandleFunc("POST "+GrokDescribePath, a.grokDescribe)
	mux.HandleFunc("POST "+GrokSavePath, a.grokSetupSave)
	// The connector's hook redeems a beacon's ticket: no other credential.
	// Session-only setup, fetched by the agent once the user picks it
	// (sessionsetup.go).
	mux.HandleFunc("GET "+SetupPath, a.serveSetup)
	// A background command claims its link with the one-time code it
	// carries (linkclaim.go): the code in the body is the only credential.
	mux.HandleFunc("POST "+LinkClaimPath, a.serveLinkClaim)
	mux.HandleFunc("POST /v1/connector/redeem", a.redeemConnector)
	// Redeem by call (callredeem.go): a plugin claims a call before it goes out,
	// and redeems its ticket by the call's ID, with the session's key.
	mux.HandleFunc("POST /v1/connector/register", a.registerCallSession)
	mux.HandleFunc("POST /v1/connector/claim", a.claimCall)
	mux.HandleFunc("POST /v1/connector/redeem-call", a.redeemByCall)
}

// appTx runs fn in a transaction with no workspace: for the app's own
// platform-level requests, which set one where they need it (setTenant).
func (a *API) appTx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	return pgx.BeginFunc(ctx, a.DB.Pool, fn)
}

// setTenant sets the workspace the rest of tx reads and writes.
func setTenant(ctx context.Context, tx pgx.Tx, tenant string) error {
	_, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenant)
	return err
}

func (a *API) requireApp(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok {
			httpx.WriteError(w, http.StatusUnauthorized, "unauthenticated", "send the app's signed assertion as a Bearer token")
			return
		}
		app, err := a.Apps.Authenticate(r.Context(), token, a.now())
		if errors.Is(err, apps.ErrUnauthenticated) {
			httpx.WriteError(w, http.StatusUnauthorized, "unauthenticated", err.Error())
			return
		}
		if errors.Is(err, apps.ErrSuspended) {
			httpx.WriteError(w, http.StatusForbidden, "suspended", apps.SuspendedMessage(a.SupportEmail))
			return
		}
		if errors.Is(err, apps.ErrWaitlisted) {
			// A key held on the waitlist (apps/pending.go): only its holder
			// gets here, past the signature check.
			httpx.WriteError(w, http.StatusForbidden, "waitlisted", err.Error())
			return
		}
		if err != nil {
			slog.Error("appapi: authenticating an app", "err", err)
			httpx.WriteError(w, http.StatusInternalServerError, "internal", "try again")
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, app)))
	}
}

func (a *API) putManifest(w http.ResponseWriter, r *http.Request) {
	var m apps.Manifest
	if !httpx.Decode(w, r, &m) {
		return
	}
	stored, changed, err := a.Apps.PublishManifest(r.Context(), AppFrom(r.Context()).ID, AppFrom(r.Context()).KeyID, m, a.subscribeChanged)
	switch {
	case errors.As(err, new(apps.AmbiguousTopicError)):
		httpx.WriteError(w, http.StatusBadRequest, "ambiguous_topic", err.Error())
		return
	case errors.Is(err, apps.ErrInvalidManifest):
		httpx.WriteError(w, http.StatusBadRequest, "bad_manifest", err.Error())
		return
	case errors.Is(err, apps.ErrNotFound):
		httpx.WriteError(w, http.StatusNotFound, "not_found", "the app was deleted")
		return
	case err != nil:
		slog.Error("appapi: storing a manifest", "app", AppFrom(r.Context()).ID, "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, "internal", "try again")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, manifestOut{stored, changed, a.features()})
}

// manifestOut is a published manifest as stored (each event's subscribe
// setting filled in from the stored one where the request left it out),
// with changed: the events whose subscribe setting it changed, and how
// many subscriptions each close ended (apps.SubscribeChange).
type manifestOut struct {
	apps.Manifest
	Changed []apps.SubscribeChange `json:"changed"`
	// Features are what this core offers that an SDK acts on when it
	// attaches: subscribe_vouchers while vouchers are on (an SDK then lists
	// tools-only events to native clients, with wakeshift_voucher); push
	// while push is served (an SDK then lists push in events/list).
	Features []string `json:"features"`
}

// features is manifestOut.Features.
func (a *API) features() []string {
	out := []string{}
	if a.SubscribeVouchers {
		out = append(out, FeatureSubscribeVouchers)
	}
	if a.Push {
		out = append(out, FeaturePush)
	}
	return out
}

// mergeManifest merges the body into the app's stored manifest
// (apps.ManifestPatch): the SDKs' automatic send, from each server that
// serves the app's tools. Several processes share a key, each with the
// events it defines, so a send never removes an event or clears a
// setting it leaves out; only PUT /manifest, the app's explicit publish,
// does. Answers with the manifest as stored.
func (a *API) mergeManifest(w http.ResponseWriter, r *http.Request) {
	var p apps.ManifestPatch
	if !httpx.Decode(w, r, &p) {
		return
	}
	m, changed, err := a.Apps.MergeManifest(r.Context(), AppFrom(r.Context()).ID, AppFrom(r.Context()).KeyID, p, a.subscribeChanged)
	switch {
	case errors.As(err, new(apps.AmbiguousTopicError)):
		httpx.WriteError(w, http.StatusBadRequest, "ambiguous_topic", err.Error())
		return
	case errors.Is(err, apps.ErrInvalidManifest):
		httpx.WriteError(w, http.StatusBadRequest, "bad_manifest", err.Error())
		return
	case errors.Is(err, apps.ErrNotFound):
		httpx.WriteError(w, http.StatusNotFound, "not_found", "the app was deleted")
		return
	case err != nil:
		slog.Error("appapi: merging a manifest", "app", AppFrom(r.Context()).ID, "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, "internal", "try again")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, manifestOut{m, changed, a.features()})
}

// getApp says which app the assertion's key belongs to, and which key it
// is: how an SDK with a key that names no app learns its ID. Called once
// and cached.
func (a *API) getApp(w http.ResponseWriter, r *http.Request) {
	app := AppFrom(r.Context())
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"id": app.ID, "name": app.Name,
		"key": map[string]string{"key_id": app.KeyID, "name": app.KeyName}})
}

func (a *API) getManifest(w http.ResponseWriter, r *http.Request) {
	app, err := a.Apps.Get(r.Context(), AppFrom(r.Context()).ID)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "internal", "couldn't load the manifest")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, app.Manifest)
}

func (a *API) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}
