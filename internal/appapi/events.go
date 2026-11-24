package appapi

import (
	"context"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/harnesses"
	"github.com/wakeshift/server/internal/httpx"
	"github.com/wakeshift/server/internal/mcpevents"
	"github.com/wakeshift/server/internal/mcpevents/event"
)

// MCP Events (internal/mcpevents): a client that speaks it (ChatGPT)
// subscribes to one of the app's event types (apps.EventType) through the
// app's MCP server (pkg/wakeshift Attach, the TypeScript SDK's attach),
// which passes the subscription on here; eventsubs.go does the work:
//
//	POST /api/apps/v1/events/subscribe   {call, name, arguments, delivery: {mode, url, secret}, ttl_ms, cursor, max_age_ms, dialect}
//	  → {id, refresh_before, cursor, truncated, delivery_status?, created}
//	POST /api/apps/v1/events/unsubscribe {call, name, arguments, delivery: {url}} → {ended}
//
// Both take voucher, a subscribe voucher (vouchers.go): the only way a
// tools-only event is subscribed to natively. A redemption's answer adds
// via ("tool:<name>") and tool.
//
// dialect is what the client speaks, as the app's SDK tells by the client
// table (an older SDK: draft or chatgpt). With call.client,
// call.user_agent and call.meta it picks the webhook's dialect
// (event.WebhookDialect): its webhook-id, and whether the callback gets
// gap and terminated envelopes.

type eventsIn struct {
	Call      callIn         `json:"call"`
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
	Delivery  struct {
		Mode   string `json:"mode"`
		URL    string `json:"url"`
		Secret string `json:"secret"`
	} `json:"delivery"`
	TTLMs    *int64  `json:"ttl_ms"`
	Cursor   *string `json:"cursor"`
	MaxAgeMs *int64  `json:"max_age_ms"`
	Dialect  string  `json:"dialect"`
	// Voucher is a subscribe voucher (vouchers.go), which the client wrote
	// as the argument wakeshift_voucher and the SDK took out of the
	// arguments. A credential: never logged.
	Voucher string `json:"voucher"`
}

// errUnsupportedMode is a delivery mode Wakeshift doesn't offer (only
// webhook).
type errUnsupportedMode struct{ mode string }

func (e errUnsupportedMode) Error() string {
	return `delivery.mode must be "webhook": the only delivery Wakeshift offers for MCP Events`
}

// dialect is the dialect a new webhook is made with (event.WebhookDialect):
// the SDK's when it names one other than spec or draft (an older SDK says
// draft for every client but ChatGPT), else the client table's for the
// client's name, User-Agent and _meta.
func (in *eventsIn) dialect() string {
	return event.WebhookDialect(in.Dialect, in.Call.Client, in.Call.UserAgent, in.Call.Meta)
}

func (a *API) eventsSubscribe(w http.ResponseWriter, r *http.Request) {
	var in eventsIn
	if httpx.Decode(w, r, &in) {
		a.eventsSubscribeNative(w, r, in) // eventsubs.go
	}
}

func (a *API) eventsUnsubscribe(w http.ResponseWriter, r *http.Request) {
	var in eventsIn
	if httpx.Decode(w, r, &in) {
		a.eventsUnsubscribeNative(w, r, in)
	}
}

// respondEvents is respond, with the errors only these calls make.
func (a *API) respondEvents(w http.ResponseWriter, v any, err error) {
	var cb *mcpevents.CallbackError
	var mode errUnsupportedMode
	var un unreachableError
	switch {
	case errors.As(err, &un):
		un.write(w) // callbackhint.go
	case errors.As(err, &mode):
		httpx.WriteJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]string{"code": "unsupported", "message": mode.Error(),
			"feature": "deliveryMode", "value": mode.mode}})
	case errors.As(err, &cb):
		httpx.WriteJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]string{"code": "callback_failed", "message": cb.Error(), "reason": cb.Reason}})
	case errors.Is(err, mcpevents.ErrTooManyWebhooks):
		httpx.WriteError(w, http.StatusForbidden, "subscription_limit", err.Error())
	default:
		a.respond(w, v, err)
	}
}

func (a *API) eventsClient() *http.Client {
	if a.EventsClient != nil {
		return a.EventsClient
	}
	return mcpevents.Client()
}

// chatgptResult: nothing wakes a ChatGPT conversation for what it
// subscribed to with an app's tools (ChatGPT is woken only by the app's
// own event types, natively), so it's a conversation nothing can wake
// (cantWakeResult). ChatGPT's desktop app (the chatgpt-codex variant:
// harnesses.ChatGPTDesktop) waits in the foreground instead, as Grok Bot
// does (foregroundResult).
//
// A call that carries the mark of Wakeshift's Codex plugin is a Codex
// thread calling through ChatGPT's hosted connector (codex_apps), which
// the plugin's hook can't connect: its result never says the plugin
// connects it, and adds how to get background wakes (codexAppsNote).
func (a *API) chatgptResult(ctx context.Context, tx pgx.Tx, cv *conversation, why resultFor) (toolResult, error) {
	if v := cv.variant; v.ID != "" && v.Wake == harnesses.Foreground {
		return a.foregroundResult(ctx, tx, cv, v, why)
	}
	return cantWakeResult(cv, join(ChatGPTNote, a.codexAppsNote(cv)), why), nil
}

// codexAppsNote is harnesses.CodexAppsNote for a ChatGPT call whose
// harness argument carries the mark of Wakeshift's Codex plugin from a
// connector that trusts this Wakeshift and that the core serves (so its
// hook could connect the thread from the app's results if the app were
// added to Codex directly: markServed), else "". The mark is the agent's
// word, as a claim is: it adds a sentence, and grants nothing.
func (a *API) codexAppsNote(cv *conversation) string {
	if cv.harness != harnessChatGPT || harnesses.ClaimName(cv.call.Harness) != harnessCodex || !a.markServed(cv.call.Plugin, harnessCodex) {
		return ""
	}
	return harnesses.CodexAppsNote(appName(cv))
}

// ChatGPTNote is what a ChatGPT conversation is told when it subscribes
// with an app's tools: what to tell the user, and not to claim it's
// watching (ChatGPT said "I'm watching" with the bare fact alone). Native
// MCP Events subscriptions get nativeLines' lines, not this.
const ChatGPTNote = "ChatGPT can't be woken for this subscription: tell the user plainly that you won't be notified here when something arrives, and that they can ask you to check with this app's tools. Don't say you're watching or that you'll let them know."
