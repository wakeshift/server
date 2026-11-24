package appapi

// Event status: who is subscribed to an event, and whether given
// subscriptions are live and connected.
//
//	POST /api/apps/v1/events/status {name, arguments?, to?}
//	    → {subscriptions, connected, via}: the conversations subscribed to the
//	      event with these arguments (a personal event's: to's), how many are
//	      connected now, and how
//	POST /api/apps/v1/events/status {name, subscriptions: [{user, id}]}
//	    → {subscriptions: [{id, user, live, connected, via}]}: each of the
//	      app's users' subscriptions by ID, found in that user's workspaces
//	      (as eventSubEnd finds one)
//
// It only reads (in a read-only transaction): nothing is ended, minted or
// drained. An ID that isn't the user's, the app's or the event's, or isn't
// live, reports live: false, as one that names nothing does: a caller can't
// tell another app's or user's subscription from a made-up one.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/apps"
	"github.com/wakeshift/server/internal/httpx"
	"github.com/wakeshift/server/internal/sessions"
)

// maxStatusIDs is the most subscriptions one status call reads.
const maxStatusIDs = 1000

// statusRef is a subscription a status call asks about.
type statusRef struct {
	User string `json:"user"`
	ID   string `json:"id"`
}

// subStatus is one subscription's status.
type subStatus struct {
	ID        string `json:"id"`
	User      string `json:"user"`
	Live      bool   `json:"live"`
	Connected bool   `json:"connected"`
	Via       string `json:"via,omitempty"`
}

func (a *API) eventStatus(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name          string          `json:"name"`
		Arguments     json.RawMessage `json:"arguments"`
		To            string          `json:"to"`
		Subscriptions []statusRef     `json:"subscriptions"`
	}
	if !httpx.DecodeLimit(w, r, &in, maxListBody) {
		return
	}
	app := AppFrom(r.Context())
	et, err := app.Manifest.EventType(in.Name)
	if err != nil {
		a.respondEventSub(w, nil, err)
		return
	}
	if in.Subscriptions == nil {
		topic, err := emitTopic(et, in.Arguments, in.To)
		if err != nil {
			a.respondEventSub(w, nil, err)
			return
		}
		out, err := a.topicCounts(r.Context(), app.ID, topic)
		a.respond(w, out, err)
		return
	}
	switch {
	case len(in.Subscriptions) == 0 || len(in.Subscriptions) > maxStatusIDs:
		a.respond(w, nil, badRequest{fmt.Errorf("subscriptions is 1 to %d {user, id}", maxStatusIDs)})
		return
	case len(in.Arguments) > 0 || in.To != "":
		a.respond(w, nil, badRequest{errors.New("pass subscriptions, or arguments (and to), not both")})
		return
	}
	for i, s := range in.Subscriptions {
		in.Subscriptions[i].User = strings.TrimSpace(s.User)
		if !appUser(in.Subscriptions[i].User) {
			a.respond(w, nil, badRequest{errors.New("each subscription's user is the app's ID for the person (at most 200 characters, not starting with ~)")})
			return
		}
	}
	out, err := a.subscriptionStatuses(r.Context(), app, et.Name, in.Subscriptions)
	a.respond(w, map[string]any{"subscriptions": out}, err)
}

// subscriptionStatuses is each ref's status, in order.
func (a *API) subscriptionStatuses(ctx context.Context, app apps.App, event string, refs []statusRef) ([]subStatus, error) {
	out := make([]subStatus, len(refs))
	var users []string
	for i, ref := range refs {
		out[i] = subStatus{ID: ref.ID, User: ref.User}
		if uuidRE.MatchString(ref.ID) && !slices.Contains(users, ref.User) {
			users = append(users, ref.User)
		}
	}
	where, err := statusWorkspaces(ctx, a.DB.Pool, app.ID, users)
	if err != nil || len(where) == 0 {
		return out, err
	}
	// Each workspace's asks: the refs whose user has it, with that user's
	// filters there.
	type ask struct {
		i  int
		uw userWorkspace
	}
	asks := map[string][]ask{}
	for i, ref := range refs {
		if !uuidRE.MatchString(ref.ID) {
			continue
		}
		for _, uw := range where[ref.User] {
			asks[uw.tenant] = append(asks[uw.tenant], ask{i, uw})
		}
	}
	tenants := make([]string, 0, len(asks))
	for t := range asks {
		tenants = append(tenants, t)
	}
	slices.Sort(tenants)
	err = a.DB.ReadTenants(ctx, tenants, func(tenant string, tx pgx.Tx) error {
		list := asks[tenant]
		ids := make([]string, 0, len(list))
		for _, q := range list {
			ids = append(ids, refs[q.i].ID)
		}
		rows, err := tx.Query(ctx, `SELECT es.id::text, es.agent_id, es.principal, ts.session_id::text
			FROM event_subscriptions es
			JOIN topic_subscriptions ts ON ts.tenant_id = es.tenant_id AND ts.subscription_id = es.subscription_id
			JOIN subscriptions s ON s.tenant_id = ts.tenant_id AND s.id = ts.subscription_id
			WHERE es.id = ANY($1::uuid[]) AND es.source_kind = $2 AND es.source_id = $3 AND es.event = $4 AND es.ended_at IS NULL
			AND ts.app_id = $3 AND ts.ended_at IS NULL AND ts.expires_at > now() AND s.status = 'active'`,
			ids, sourceApp, app.ID, event)
		if err != nil {
			return err
		}
		type live struct{ ID, Agent, Principal, Session string }
		found, err := pgx.CollectRows(rows, pgx.RowToStructByPos[live])
		if err != nil {
			return err
		}
		for _, l := range found {
			for _, q := range list {
				if refs[q.i].ID != l.ID || (q.uw.agent != "" && q.uw.agent != l.Agent) || (q.uw.principal != "" && q.uw.principal != l.Principal) {
					continue
				}
				st := &out[q.i]
				st.Live = true
				s, err := sessions.Get(ctx, tx, l.Session)
				if errors.Is(err, sessions.ErrNotFound) {
					continue
				}
				if err != nil {
					return err
				}
				st.Via = connectedVia(s)
				st.Connected = st.Via != ""
			}
		}
		return nil
	})
	return out, err
}

// statusWorkspaces is userWorkspaces for many users at once, in two
// queries: each user's own workspace (by external ID), and for a user of
// the form "<workspace>/<agent>" naming a workspace the app keeps, that
// agent's subscriptions there, made as that user.
func statusWorkspaces(ctx context.Context, q interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}, app string, users []string) (map[string][]userWorkspace, error) {
	out := map[string][]userWorkspace{}
	if len(users) == 0 {
		return out, nil
	}
	rows, _ := q.Query(ctx, `SELECT id::text, external_id FROM tenants WHERE app_id = $1 AND external_id = ANY($2)`, app, users)
	type own struct{ Tenant, User string }
	owns, err := pgx.CollectRows(rows, pgx.RowToStructByPos[own])
	if err != nil {
		return nil, err
	}
	for _, o := range owns {
		out[o.User] = append(out[o.User], userWorkspace{tenant: o.Tenant})
	}
	agentsOf := map[string][]string{}
	var spaces []string
	for _, u := range users {
		ws, ag, ok := strings.Cut(u, "/")
		if !ok || !uuidRE.MatchString(ws) || ag == "" {
			continue
		}
		if agentsOf[ws] == nil {
			spaces = append(spaces, ws)
		}
		agentsOf[ws] = append(agentsOf[ws], u)
	}
	if len(spaces) == 0 {
		return out, nil
	}
	rows, _ = q.Query(ctx, `SELECT id::text FROM tenants WHERE id = ANY($1::uuid[]) AND app_id = $2`, spaces, app)
	kept, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	for _, t := range kept {
		for _, u := range agentsOf[t] {
			_, ag, _ := strings.Cut(u, "/")
			out[u] = append(out[u], userWorkspace{tenant: t, agent: ag, principal: u})
		}
	}
	return out, nil
}
