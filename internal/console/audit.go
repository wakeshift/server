package console

import (
	"context"
	"fmt"
	"net/http"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/audit"
	"github.com/wakeshift/server/internal/httpx"
)

// listAudit returns the workspace's audit log, newest first: up to limit
// entries (at most 100) older than the entry ID before.
func (a *API) listAudit(w http.ResponseWriter, r *http.Request) {
	before, limit, ok := page(w, r, audit.MaxLimit)
	if !ok {
		return
	}
	var list []audit.Item
	err := a.tx(r, func(ctx context.Context, tx pgx.Tx) (err error) {
		list, err = audit.List(ctx, tx, before, limit)
		return err
	})
	respond(w, map[string]any{"entries": list}, err)
}

// page reads a log's ?before= (an entry ID) and ?limit= (1-max; 50 if
// absent), answering 400 for bad ones.
func page(w http.ResponseWriter, r *http.Request, max int) (before int64, limit int, ok bool) {
	q := r.URL.Query()
	limit = 50
	if v := q.Get("before"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			httpx.WriteError(w, http.StatusBadRequest, "bad_request", "before must be an entry ID")
			return 0, 0, false
		}
		before = n
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > max {
			httpx.WriteError(w, http.StatusBadRequest, "bad_request", fmt.Sprintf("limit must be 1-%d", max))
			return 0, 0, false
		}
		limit = n
	}
	return before, limit, true
}
