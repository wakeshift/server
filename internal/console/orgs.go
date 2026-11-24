package console

// Organizations' own routes: the organization switcher, members, roles,
// leaving, settings and the organization's log, under /api/v1/orgs. Like
// the developer API, they're platform-level, read membership on every
// request, answer 404 to a non-member (as for an organization that doesn't
// exist) and 403 to a member whose role doesn't allow the change and need
// the console's origin. The orgs package writes each change to org_log in
// the same transaction.

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/httpx"
	"github.com/wakeshift/server/internal/orgs"
)

func (a *API) registerOrgs(route func(string, http.HandlerFunc)) {
	route("GET /api/v1/orgs", a.listOrgs)
	route("POST /api/v1/orgs", a.createOrg)
	route("GET /api/v1/orgs/{org}", a.getOrg)
	route("PATCH /api/v1/orgs/{org}", a.renameOrg)
	route("DELETE /api/v1/orgs/{org}", a.deleteOrg)
	route("GET /api/v1/orgs/{org}/members", a.listMembers)
	route("PATCH /api/v1/orgs/{org}/members/{user}", a.setMemberRole)
	route("DELETE /api/v1/orgs/{org}/members/{user}", a.removeMember)
	route("GET /api/v1/orgs/{org}/members/{user}/made", a.memberMade)
	route("GET /api/v1/orgs/{org}/log", a.orgLog)
}

func (a *API) listOrgs(w http.ResponseWriter, r *http.Request) {
	var list []orgs.Org
	err := a.platformTx(r, func(ctx context.Context, tx pgx.Tx) (err error) {
		list, err = orgs.List(ctx, tx, me(r))
		return err
	})
	respondOrgs(w, map[string]any{"orgs": list}, err)
}

func (a *API) createOrg(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name string `json:"name"`
	}
	if !httpx.Decode(w, r, &in) {
		return
	}
	var out orgs.Org
	err := a.platformTx(r, func(ctx context.Context, tx pgx.Tx) (err error) {
		out, err = orgs.Create(ctx, tx, me(r), in.Name)
		return err
	})
	respondOrgs(w, out, err)
}

func (a *API) getOrg(w http.ResponseWriter, r *http.Request) {
	var out orgs.Org
	err := a.platformTx(r, func(ctx context.Context, tx pgx.Tx) (err error) {
		out, err = orgs.Get(ctx, tx, me(r), r.PathValue("org"))
		return err
	})
	respondOrgs(w, out, err)
}

func (a *API) renameOrg(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name string `json:"name"`
	}
	if !httpx.Decode(w, r, &in) {
		return
	}
	var out orgs.Org
	err := a.platformTx(r, func(ctx context.Context, tx pgx.Tx) (err error) {
		out, err = orgs.Rename(ctx, tx, me(r), r.PathValue("org"), in.Name)
		return err
	})
	respondOrgs(w, out, err)
}

func (a *API) deleteOrg(w http.ResponseWriter, r *http.Request) {
	err := a.platformTx(r, func(ctx context.Context, tx pgx.Tx) error {
		return orgs.Delete(ctx, tx, me(r), r.PathValue("org"))
	})
	respondOrgs(w, map[string]bool{"deleted": true}, err)
}

func (a *API) listMembers(w http.ResponseWriter, r *http.Request) {
	var list []orgs.Member
	err := a.platformTx(r, func(ctx context.Context, tx pgx.Tx) (err error) {
		list, err = orgs.Members(ctx, tx, me(r), r.PathValue("org"))
		return err
	})
	respondOrgs(w, map[string]any{"members": list}, err)
}

func (a *API) setMemberRole(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Role orgs.Role `json:"role"`
	}
	if !httpx.Decode(w, r, &in) {
		return
	}
	err := a.platformTx(r, func(ctx context.Context, tx pgx.Tx) error {
		return orgs.SetRole(ctx, tx, me(r), r.PathValue("org"), r.PathValue("user"), in.Role)
	})
	respondOrgs(w, map[string]any{"role": in.Role}, err)
}

// removeMember removes a member, or with your own ID, leaves.
func (a *API) removeMember(w http.ResponseWriter, r *http.Request) {
	err := a.platformTx(r, func(ctx context.Context, tx pgx.Tx) error {
		return orgs.Remove(ctx, tx, me(r), r.PathValue("org"), r.PathValue("user"))
	})
	respondOrgs(w, map[string]bool{"removed": true}, err)
}

// memberMade lists the live keys and publish URLs a member made, for the
// removal dialog.
func (a *API) memberMade(w http.ResponseWriter, r *http.Request) {
	var out orgs.Made
	err := a.platformTx(r, func(ctx context.Context, tx pgx.Tx) (err error) {
		out, err = orgs.MadeBy(ctx, tx, me(r), r.PathValue("org"), r.PathValue("user"))
		return err
	})
	respondOrgs(w, out, err)
}

func (a *API) orgLog(w http.ResponseWriter, r *http.Request) {
	before, limit, ok := page(w, r, orgs.MaxLogLimit)
	if !ok {
		return
	}
	var list []orgs.LogItem
	err := a.platformTx(r, func(ctx context.Context, tx pgx.Tx) (err error) {
		list, err = orgs.ListLog(ctx, tx, me(r), r.PathValue("org"), before, limit)
		return err
	})
	respondOrgs(w, map[string]any{"entries": list}, err)
}

func respondOrgs(w http.ResponseWriter, v any, err error) {
	var invalid *orgs.ValidationError
	switch {
	case errors.Is(err, orgs.ErrNotMember):
		httpx.WriteError(w, http.StatusNotFound, "not_found", "no such organization of yours")
	case errors.Is(err, orgs.ErrNoMember):
		httpx.WriteError(w, http.StatusNotFound, "not_found", "no such member")
	case errors.Is(err, orgs.ErrForbidden):
		httpx.WriteError(w, http.StatusForbidden, "forbidden", "your role in this organization doesn't allow that")
	case errors.Is(err, orgs.ErrLastOwner):
		httpx.WriteError(w, http.StatusConflict, "last_owner", "an organization needs an owner: make someone else an owner first")
	case errors.Is(err, orgs.ErrHasApps):
		httpx.WriteError(w, http.StatusConflict, "has_apps", "the organization still has apps: delete them first")
	case errors.Is(err, orgs.ErrTooMany):
		httpx.WriteError(w, http.StatusBadRequest, "too_many", fmt.Sprintf("you can make at most %d organizations; delete one first", orgs.MaxCreated))
	case errors.As(err, &invalid):
		httpx.WriteError(w, http.StatusBadRequest, "bad_request", invalid.Message)
	default:
		respond(w, v, err)
	}
}
