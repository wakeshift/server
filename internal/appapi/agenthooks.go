package appapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/agenthooks"
	"github.com/wakeshift/server/internal/audit"
	"github.com/wakeshift/server/internal/framing"
	"github.com/wakeshift/server/internal/harnesses"
	"github.com/wakeshift/server/internal/httpx"
	"github.com/wakeshift/server/internal/sessions"
	"github.com/wakeshift/server/internal/streams"
)

// Agent-hosted webhooks (internal/agenthooks): a Capy thread is woken by a
// webhook watch it creates on itself, whose URL it registers with the
// registration link its subscribing result gives (webhookResult). The link
// is the only credential the registration route takes: it names the
// session's stream link and the harness it was handed to, and works only
// while that stream link does (revoked with the session's links, the
// grant, or an unwatch).

// webhookResult: Capy (harnesses.Webhook). Subscribing says where the
// thread's watch stands: registered and live, nothing to do; none yet, or
// dead (the watch expired or was stopped), create one and register its URL
// with this link, or else run the one-shot command (Harness.Fallback).
// Other results add nothing: what arrives waits for the session until a
// watch or the command takes it, and no result asks for another call.
func (a *API) webhookResult(ctx context.Context, tx pgx.Tx, cv *conversation, h harnesses.Harness, why resultFor) (toolResult, error) {
	var out toolResult
	cv.noBeacon = "nothing_to_install" // no hooks to read one
	if why == forAnnotate {
		return out, nil
	}
	app := appName(cv)
	d, registered, err := agenthooks.Get(ctx, tx, cv.session.ID)
	if err != nil {
		return out, err
	}
	if registered && d.State == agenthooks.Live {
		out.Text = join(harnesses.CapyRegisteredNote(app), passSession(cv))
		return out, nil
	}
	link, err := a.sessionLink(ctx, tx, cv)
	if err != nil {
		return out, err
	}
	register, err := a.registrationLink(ctx, tx, cv, link)
	if err != nil {
		return out, err
	}
	live, err := backgroundLive(ctx, tx, cv.session)
	if err != nil {
		return out, err
	}
	url, err := a.backgroundURL(ctx, tx, cv, h) // the same link: the fallback command's
	if err != nil {
		return out, err
	}
	if registered {
		out.Text = join(harnesses.CapyDeadNote(app, register, url, live), passSession(cv))
		return out, nil
	}
	before, err := cv.toldBefore(ctx, tx)
	if err != nil {
		return out, err
	}
	out.Text = join(harnesses.CapyWebhookNote(app, register, url, before, live), passSession(cv))
	return out, nil
}

// registrationLink is the registration link for the session's stream link
// (its URL, link): the stream link is kept a full SessionLinkTTL from now
// (the registration works only while it does), and the registration link
// expires with the stream link's slide at the latest.
func (a *API) registrationLink(ctx context.Context, tx pgx.Tx, cv *conversation, link string) (string, error) {
	tenant, id, ok := a.Streams.Held(link)
	if !ok || tenant != cv.tenant {
		return "", errors.New("appapi: the session's link isn't one this server made")
	}
	if err := streams.KeepLink(ctx, tx, id); err != nil {
		return "", err
	}
	l, err := streams.LinkByID(ctx, tx, id)
	if err != nil {
		return "", err
	}
	exp := l.SlideUntil
	if exp.IsZero() {
		exp = l.ExpiresAt
	}
	return a.Hooks.URL(a.base(), tenant, id, agenthooks.KindCapy, exp), nil
}

// errors the registration route answers with.
var (
	errHookLink    = errors.New("this registration link isn't valid, or no longer works (it expired, or the conversation stopped listening)")
	errHookHarness = errors.New("this conversation's harness can't register a webhook")
)

// agentWebhook is the registration route (agenthooks.RegisterPath): POST
// {"url": …} registers the conversation's watch URL (replacing any), and
// DELETE removes it. The URL is never logged, echoed or put in the audit
// entry.
func (a *API) agentWebhook(w http.ResponseWriter, r *http.Request) {
	if a.Hooks == nil || a.HookSeal == nil {
		httpx.WriteError(w, http.StatusNotFound, "not_found", "agent-hosted webhooks aren't enabled here")
		return
	}
	reg, err := a.Hooks.Parse(r.URL.Query().Get("link"), a.now())
	if err != nil {
		httpx.WriteError(w, http.StatusUnauthorized, "invalid_link", errHookLink.Error())
		return
	}
	if reg.Kind != agenthooks.KindCapy {
		httpx.WriteError(w, http.StatusForbidden, "unsupported_harness", errHookHarness.Error())
		return
	}
	var in struct {
		URL string `json:"url"`
	}
	if r.Method == http.MethodPost {
		b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4<<10))
		if err != nil || json.Unmarshal(b, &in) != nil {
			httpx.WriteError(w, http.StatusBadRequest, "bad_request", `the body must be JSON: {"url": "<the watch URL>"}, at most 4 KB`)
			return
		}
		in.URL = strings.TrimSpace(in.URL)
		if err := agenthooks.CheckURL(in.URL); err != nil {
			httpx.WriteError(w, http.StatusBadRequest, "invalid_url", err.Error())
			return
		}
	}
	var agent, app string
	var removed bool
	err = a.DB.InTenant(r.Context(), reg.Tenant, func(tx pgx.Tx) error {
		ctx := r.Context()
		l, err := streams.UsableLink(ctx, tx, reg.Link)
		if err != nil || l.Target == "" || l.Ack || !strings.HasPrefix(l.CreatedBy, "app:") {
			return errHookLink
		}
		s, err := sessions.Get(ctx, tx, l.Target)
		if errors.Is(err, sessions.ErrNotFound) || err == nil && s.Agent != l.Agent {
			return errHookLink
		}
		if err != nil {
			return err
		}
		agent, app = s.Agent, strings.TrimPrefix(l.CreatedBy, "app:")
		entry := audit.Entry{Actor: "conversation", AgentID: s.Agent, AppID: app, GrantID: l.GrantID,
			Detail: map[string]any{"session": s.Handle, "kind": reg.Kind}}
		if r.Method == http.MethodDelete {
			if removed, err = agenthooks.Unregister(ctx, tx, s.ID); err != nil || !removed {
				return err
			}
			entry.Action = audit.AgentWebhookUnregistered
			return audit.Record(ctx, tx, entry)
		}
		replaced, err := agenthooks.Register(ctx, tx, a.HookSeal, reg.Tenant, agenthooks.Destination{Session: s.ID, Agent: s.Agent, Kind: reg.Kind,
			App: app, Grant: l.GrantID}, in.URL)
		if err != nil {
			return err
		}
		entry.Action = audit.AgentWebhookRegistered
		entry.Detail = map[string]any{"session": s.Handle, "kind": reg.Kind, "replaced": replaced}
		return audit.Record(ctx, tx, entry)
	})
	switch {
	case errors.Is(err, errHookLink):
		httpx.WriteError(w, http.StatusUnauthorized, "invalid_link", errHookLink.Error())
		return
	case errors.Is(err, agenthooks.ErrBadURL):
		httpx.WriteError(w, http.StatusBadRequest, "invalid_url", err.Error())
		return
	case err != nil:
		a.respond(w, nil, err) // logs the error: never the URL (pgx errors don't carry arguments)
		return
	}
	if r.Method == http.MethodDelete {
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"registered": false, "removed": removed})
		return
	}
	if a.HookWake != nil {
		a.HookWake(reg.Tenant, agent) // what's waiting goes at once
	}
	name := app
	if ap, err := a.Apps.Get(r.Context(), app); err == nil {
		name = framing.Name(ap.Name)
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"registered": true, "message": harnesses.CapyRegistered(name)})
}
