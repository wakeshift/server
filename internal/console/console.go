// Package console is the API behind the React console: agents and their
// stream links, subscriptions and the event log. Every route requires a signed-in
// user and works inside their current workspace.
package console

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/agents"
	"github.com/wakeshift/server/internal/appapi"
	"github.com/wakeshift/server/internal/apps"
	"github.com/wakeshift/server/internal/audit"
	"github.com/wakeshift/server/internal/auth"
	"github.com/wakeshift/server/internal/db"
	"github.com/wakeshift/server/internal/events"
	"github.com/wakeshift/server/internal/grants"
	"github.com/wakeshift/server/internal/httpx"
	"github.com/wakeshift/server/internal/mail"
	"github.com/wakeshift/server/internal/streams"
	"github.com/wakeshift/server/internal/subscriptions"
)

// API serves the console's endpoints under /api/v1.
type API struct {
	DB   *db.DB
	Auth *auth.Console
	// SupportEmail is where refusals tell app owners to write
	// (WAKESHIFT_SUPPORT_EMAIL; empty: apps.DefaultSupportEmail).
	SupportEmail string
	Streams      *streams.Server
	Apps         *apps.Registry
	// AppAPI does what an app's owner asks of it in the console (ending a
	// subscription, forgetting a user, publish URLs) as the app's own calls
	// do.
	AppAPI *appapi.API
	// Mail sends organization invites (nil: none; the console shows the
	// links to copy).
	Mail mail.Sender
}

// Register adds the console's routes.
func (a *API) Register(mux *http.ServeMux) {
	route := func(pattern string, h http.HandlerFunc) { mux.HandleFunc(pattern, a.Auth.Require(h)) }
	route("GET /api/v1/agents", a.listAgents)
	route("POST /api/v1/agents", a.createAgent)
	route("POST /api/v1/agents/{id}/stream-link", a.streamLink)
	route("POST /api/v1/agents/{id}/stream-links/revoke", a.revokeLinks)
	route("GET /api/v1/subscriptions", a.listSubscriptions)
	route("POST /api/v1/subscriptions", a.createSubscription)
	route("POST /api/v1/subscriptions/{id}/cancel", a.cancelSubscription)
	route("GET /api/v1/events", a.listEvents)
	route("POST /api/v1/events/test", a.sendTestEvent)
	route("GET /api/v1/apps", a.listApps)
	route("POST /api/v1/grants/{id}/revoke", a.revokeGrant)
	route("GET /api/v1/event-subscriptions", a.listEventSubs)
	route("POST /api/v1/event-subscriptions/{id}/revoke", a.revokeEventSub)
	route("GET /api/v1/audit", a.listAudit)
	a.registerDeveloper(route)
	a.registerOrgs(route)
	a.registerInvites(route)
	a.registerJoinPreview(mux)
}

// tx runs fn in the user's current workspace.
func (a *API) tx(r *http.Request, fn func(ctx context.Context, tx pgx.Tx) error) error {
	ctx := r.Context()
	return a.DB.InTenant(ctx, auth.TenantFrom(ctx), func(tx pgx.Tx) error { return fn(ctx, tx) })
}

func (a *API) listAgents(w http.ResponseWriter, r *http.Request) {
	var list []agents.Agent
	err := a.tx(r, func(ctx context.Context, tx pgx.Tx) (err error) {
		list, err = agents.List(ctx, tx)
		return err
	})
	respond(w, map[string]any{"agents": list}, err)
}

func (a *API) createAgent(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID          string `json:"id"`
		DisplayName string `json:"display_name"`
	}
	if !httpx.Decode(w, r, &in) {
		return
	}
	if !agents.ValidID(in.ID) {
		httpx.WriteError(w, http.StatusBadRequest, "bad_request", "agent IDs are 2-40 lowercase letters, digits and dashes")
		return
	}
	var out agents.Agent
	err := a.tx(r, func(ctx context.Context, tx pgx.Tx) (err error) {
		out, err = agents.Create(ctx, tx, in.ID, in.DisplayName)
		return err
	})
	respond(w, out, err)
}

// streamLink returns a URL that streams an agent's deliveries, to paste
// into the agent (e.g. Claude Code's Monitor). Apps hand out links the same
// way; this is the console's.
func (a *API) streamLink(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Ack bool `json:"ack"`
	}
	if !httpx.Decode(w, r, &in) {
		return
	}
	agent := r.PathValue("id")
	var u string
	var exp time.Time
	err := a.tx(r, func(ctx context.Context, tx pgx.Tx) error {
		ok, err := agents.Exists(ctx, tx, agent)
		if err == nil && !ok {
			err = agents.ErrNotFound
		}
		if err != nil {
			return err
		}
		u, exp, err = a.Streams.Mint(ctx, tx, auth.TenantFrom(ctx), streams.NewLink{
			Agent: agent, Ack: in.Ack, CreatedBy: "user:" + auth.UserFrom(ctx).ID, TTL: streamLinkTTL,
		})
		return err
	})
	if err != nil {
		respond(w, nil, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"url": u, "expires_at": exp.UTC().Format(time.RFC3339)})
}

// revokeLinks revokes every stream link for an agent; open streams on them
// close at once.
func (a *API) revokeLinks(w http.ResponseWriter, r *http.Request) {
	var n int64
	err := a.tx(r, func(ctx context.Context, tx pgx.Tx) (err error) {
		if n, err = streams.RevokeLinks(ctx, tx, r.PathValue("id")); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{Actor: audit.User(auth.UserFrom(ctx).ID), Action: audit.StreamLinksRevoked,
			AgentID: r.PathValue("id"), Detail: map[string]int64{"revoked": n}})
	})
	respond(w, map[string]int64{"revoked": n}, err)
}

var (
	uuidRE     = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	testTypeRE = regexp.MustCompile(`^test(\.[a-z0-9][a-z0-9_-]*)+$`)
)

// streamLinkTTL is how long a console-made link works.
const streamLinkTTL = 24 * time.Hour

func (a *API) listSubscriptions(w http.ResponseWriter, r *http.Request) {
	var list []subscriptions.Subscription
	err := a.tx(r, func(ctx context.Context, tx pgx.Tx) (err error) {
		list, err = subscriptions.List(ctx, tx, r.URL.Query().Get("agent"))
		return err
	})
	respond(w, map[string]any{"subscriptions": list}, err)
}

func (a *API) createSubscription(w http.ResponseWriter, r *http.Request) {
	var in struct {
		AgentID   string            `json:"agent_id"`
		EventType string            `json:"event_type"`
		Filters   map[string]string `json:"filters"`
		Reason    string            `json:"reason"`
	}
	if !httpx.Decode(w, r, &in) {
		return
	}
	var out subscriptions.Subscription
	err := a.tx(r, func(ctx context.Context, tx pgx.Tx) (err error) {
		out, err = subscriptions.Create(ctx, tx, subscriptions.New{
			AgentID: in.AgentID, EventType: in.EventType, Filters: in.Filters, Reason: in.Reason, CreatedBy: auth.UserFrom(ctx).Email,
		})
		if err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{Actor: audit.User(auth.UserFrom(ctx).ID), Action: audit.SubscriptionCreated,
			AgentID: out.AgentID, Detail: map[string]any{"subscription_id": out.ID, "event_type": out.EventType, "filters": out.Filters}})
	})
	respond(w, out, err)
}

func (a *API) cancelSubscription(w http.ResponseWriter, r *http.Request) {
	if !uuidRE.MatchString(r.PathValue("id")) {
		respond(w, nil, subscriptions.ErrNotFound)
		return
	}
	var out subscriptions.Subscription
	err := a.tx(r, func(ctx context.Context, tx pgx.Tx) (err error) {
		if out, err = subscriptions.Cancel(ctx, tx, r.PathValue("id")); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{Actor: audit.User(auth.UserFrom(ctx).ID), Action: audit.SubscriptionCanceled,
			AgentID: out.AgentID, Detail: map[string]any{"subscription_id": out.ID, "event_type": out.EventType}})
	})
	respond(w, out, err)
}

func (a *API) listEvents(w http.ResponseWriter, r *http.Request) {
	var before uint64
	if v := r.URL.Query().Get("before"); v != "" {
		var err error
		if before, err = strconv.ParseUint(v, 10, 64); err != nil {
			httpx.WriteError(w, http.StatusBadRequest, "bad_request", "before must be an event number")
			return
		}
	}
	var list []events.Event
	err := a.tx(r, func(ctx context.Context, tx pgx.Tx) (err error) {
		list, err = events.Recent(ctx, tx, before, 50)
		return err
	})
	respond(w, map[string]any{"events": list}, err)
}

// sendTestEvent appends an event from the console, to try subscriptions and
// streams without connecting a source. Its type must start with "test." so
// the console can't impersonate an app's or a source's events.
func (a *API) sendTestEvent(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Type    string            `json:"type"`
		Subject string            `json:"subject"`
		Body    string            `json:"body"`
		Attrs   map[string]string `json:"attrs"`
	}
	if !httpx.Decode(w, r, &in) {
		return
	}
	if len(in.Type) > 100 || !testTypeRE.MatchString(in.Type) {
		httpx.WriteError(w, http.StatusBadRequest, "bad_request", `test event types start with "test.", e.g. test.ping`)
		return
	}
	var out events.Event
	err := a.tx(r, func(ctx context.Context, tx pgx.Tx) (err error) {
		out, _, err = events.Append(ctx, tx, events.Event{
			Type: in.Type, Source: "console", Subject: in.Subject, Body: in.Body, Attrs: in.Attrs,
			From: events.Party{Kind: "user", ID: auth.UserFrom(ctx).Email}, Trust: events.Internal,
		})
		return err
	})
	respond(w, out, err)
}

type appView struct {
	apps.App
	Grants []grants.Grant `json:"grants"` // this workspace's live grants for the app
}

// listApps lists the apps with live grants in this workspace, with them,
// so they can be revoked. Since account mode was turned off, no app gets
// a grant in a person's own workspace: these are grants from before, until
// they expire.
func (a *API) listApps(w http.ResponseWriter, r *http.Request) {
	var live []grants.Grant
	err := a.tx(r, func(ctx context.Context, tx pgx.Tx) (err error) {
		live, err = grants.List(ctx, tx)
		return err
	})
	out := []appView{}
	byApp := map[string]int{}
	for _, g := range live {
		if err != nil {
			break
		}
		i, ok := byApp[g.AppID]
		if !ok {
			var app apps.App
			if app, err = a.Apps.Get(r.Context(), g.AppID); errors.Is(err, apps.ErrNotFound) {
				err = nil // deleted: its grants go with its deletion
				continue
			}
			i = len(out)
			byApp[g.AppID] = i
			out = append(out, appView{App: app, Grants: []grants.Grant{}})
		}
		out[i].Grants = append(out[i].Grants, g)
	}
	respond(w, map[string]any{"apps": out}, err)
}

func (a *API) revokeGrant(w http.ResponseWriter, r *http.Request) {
	if !uuidRE.MatchString(r.PathValue("id")) {
		respond(w, nil, grants.ErrNotFound)
		return
	}
	var g grants.Grant
	err := a.tx(r, func(ctx context.Context, tx pgx.Tx) (err error) {
		g, err = grants.Revoke(ctx, tx, r.PathValue("id"), audit.User(auth.UserFrom(ctx).ID))
		return err
	})
	respond(w, g, err)
}

// listEventSubs lists the workspace's event subscriptions: what each app
// may wake its agents for, by event and arguments, and where it goes.
func (a *API) listEventSubs(w http.ResponseWriter, r *http.Request) {
	var list []appapi.EventSubscription
	err := a.tx(r, func(ctx context.Context, tx pgx.Tx) (err error) {
		list, err = appapi.ListEventSubscriptions(ctx, tx)
		return err
	})
	if list == nil {
		list = []appapi.EventSubscription{}
	}
	respond(w, map[string]any{"subscriptions": list}, err)
}

// revokeEventSub ends one event subscription at once, as the person.
func (a *API) revokeEventSub(w http.ResponseWriter, r *http.Request) {
	if !uuidRE.MatchString(r.PathValue("id")) {
		respond(w, nil, appapi.ErrNoEventSubscription)
		return
	}
	var s appapi.EventSubscription
	err := a.tx(r, func(ctx context.Context, tx pgx.Tx) (err error) {
		s, err = appapi.RevokeEventSubscription(ctx, tx, r.PathValue("id"), audit.User(auth.UserFrom(ctx).ID))
		return err
	})
	respond(w, s, err)
}

// respond writes v, or maps a store error to an API error.
func respond(w http.ResponseWriter, v any, err error) {
	switch {
	case err == nil:
		httpx.WriteJSON(w, http.StatusOK, v)
	case errors.Is(err, agents.ErrExists):
		httpx.WriteError(w, http.StatusConflict, "exists", "an agent with this ID already exists")
	case errors.Is(err, agents.ErrNotFound):
		httpx.WriteError(w, http.StatusNotFound, "not_found", "no such agent")
	case errors.Is(err, subscriptions.ErrNotFound):
		httpx.WriteError(w, http.StatusNotFound, "not_found", "no such subscription")
	case errors.Is(err, apps.ErrNotFound):
		httpx.WriteError(w, http.StatusNotFound, "not_found", "no such app")
	case errors.Is(err, grants.ErrNotFound):
		httpx.WriteError(w, http.StatusNotFound, "not_found", "no such grant")
	case errors.Is(err, appapi.ErrNoEventSubscription):
		httpx.WriteError(w, http.StatusNotFound, "not_found", "no such event subscription")
	case errors.Is(err, subscriptions.ErrBadType):
		httpx.WriteError(w, http.StatusBadRequest, "bad_request", err.Error())
	default:
		slog.Error("console: request failed", "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, "internal", "something went wrong")
	}
}
