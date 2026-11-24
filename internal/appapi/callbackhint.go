package appapi

import (
	"errors"
	"net/http"

	"github.com/wakeshift/server/internal/harnesses"
	"github.com/wakeshift/server/internal/httpx"
	"github.com/wakeshift/server/internal/mcpevents"
	"github.com/wakeshift/server/internal/mcpevents/event"
)

// A webhook events/subscribe refused because nothing can reach its
// callback (the URL isn't https, or its verification failed: the address
// isn't public, no answer, a TLS error, a timeout, a 4xx or 5xx, a wrong
// challenge) keeps its standard answer (bad_request, or callback_failed
// with its reason) and adds a hint: what the client can use instead. The
// SDKs pass the hint into the JSON-RPC error's data, and the message, as
// they are (sdk/typescript/test/mcp-events-wire.json core_errors):
//
//	{"error": {"code": "callback_failed", "message": "…", "reason": "connection_refused",
//	  "hint": {"kind": "callback_unreachable", "alternatives": ["plugin"], "install": "<docs>"}}}
//
// alternatives lists plugin only with install: a client recognized
// (harnesses.Recognize, surely) as an agent Wakeshift installs into, on a
// Wakeshift that knows its site (SiteURL). It lists poll, and push when
// this Wakeshift serves it (API.Push: WAKESHIFT_MCP_EVENTS_PUSH), only for a client
// that isn't known to take events by webhook alone: the MCP Events client
// table's webhook_only entries (event.EvaluateClient: ChatGPT, OpenClaw's
// events plugin, Hermes Agent) can't poll or push, so they're never told to. Neither
// the hint nor the message ever repeats the callback, its host or its
// address. Nothing else gets a hint: not a refused caller, a bad secret, a
// limit or an unknown event.

// hintUnreachable is the hint's kind.
const hintUnreachable = "callback_unreachable"

// unreachableError is a refusal for an unreachable callback, with its hint.
type unreachableError struct {
	err error // badRequest{mcpevents.NotHTTPSError} or *mcpevents.CallbackError
	// install is the docs page that installs kit, what Wakeshift installs
	// into the client ("" for neither).
	install, kit string
	// webhookOnly: the client takes events by webhook alone, so poll and
	// push aren't offered; push: this Wakeshift serves push.
	webhookOnly, push bool
}

func (e unreachableError) Error() string { return e.err.Error() }
func (e unreachableError) Unwrap() error { return e.err }

// unreachable is err with a hint for call's client (speaking dialect) if
// err says its callback can't be reached, else err.
func (a *API) unreachable(call callIn, dialect string, err error) error {
	var bad badRequest
	notHTTPS := errors.As(err, &bad) && errors.As(bad.error, new(mcpevents.NotHTTPSError))
	if !notHTTPS && !errors.As(err, new(*mcpevents.CallbackError)) {
		return err
	}
	// Webhook alone: by the client table, or by the dialect the app's SDK
	// names (ChatGPT's, which it may know from what the table doesn't see).
	webhookOnly := dialect == event.DialectChatGPT ||
		event.EvaluateClient(event.ClientInput{Client: call.Client, UserAgent: call.UserAgent, Meta: call.Meta, ServesPush: a.Push}).WebhookOnly
	e := unreachableError{err: err, webhookOnly: webhookOnly, push: a.Push}
	h, _, ok := harnesses.Recognize(call.Client, call.Meta, call.UserAgent)
	if !ok || h.Unsure != "" || !h.Installable || a.SiteURL == "" {
		return e
	}
	e.install, e.kit = a.SiteURL+"/docs/agents#the-one-time-install", harnesses.KitFor(h, a.Issuer)
	if h.ID == "hermes" {
		e.install = a.SiteURL + "/docs/hermes"
	}
	return e
}

// hint is the error's data.hint.
func (e unreachableError) hint() map[string]any {
	alts := []string{}
	h := map[string]any{"kind": hintUnreachable}
	if e.install != "" {
		alts = append(alts, "plugin")
		h["install"] = e.install
	}
	if !e.webhookOnly {
		alts = append(alts, "poll")
		if e.push {
			alts = append(alts, "push")
		}
	}
	h["alternatives"] = alts
	return h
}

// message is the refusal's message, then what the client can do instead.
func (e unreachableError) message() string {
	help := "This client needs a public HTTPS callback for webhook delivery."
	switch {
	case e.install != "" && e.webhookOnly:
		help += " To be woken without one, install " + e.kit + " (" + e.install + ")."
	case e.install != "":
		help += " To be woken without one, install " + e.kit + " (" + e.install + "), or " + e.pollOr() + "."
	case !e.webhookOnly:
		help += " Without one, " + e.pollOr() + "."
	}
	return e.err.Error() + ". " + help
}

// pollOr is the message's other ways to take events, as this Wakeshift
// serves them.
func (e unreachableError) pollOr() string {
	if e.push {
		return "use poll or push if the client supports them"
	}
	return "use poll if the client supports it"
}

// write answers the refusal: bad_request or callback_failed (with its
// reason), as without the hint, and the hint.
func (e unreachableError) write(w http.ResponseWriter) {
	out := map[string]any{"code": "bad_request", "message": e.message(), "hint": e.hint()}
	var cb *mcpevents.CallbackError
	if errors.As(e.err, &cb) {
		out["code"], out["reason"] = "callback_failed", cb.Reason
	}
	httpx.WriteJSON(w, http.StatusBadRequest, map[string]any{"error": out})
}
