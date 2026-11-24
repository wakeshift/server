package streams

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/connectorversion"
	"github.com/wakeshift/server/internal/harnesses"
	"github.com/wakeshift/server/internal/httpx"
)

// Connector versions (internal/connectorversion). A link a connector
// redeemed a ticket for (Link.Connector) is checked on every stream, poll
// and wait: one used by a connector older than Server.Connectors serves is
// refused with 426 update_required, and nothing is handed to it, so the
// session's deliveries stay pending (its next tool result shows them, and
// asks for the update). Acks are recorded whatever the version: what an
// outdated connector already has is confirmed, not sent again. Links an
// agent holds itself (a Monitor, a wait command: no Link.Connector) are
// checked only if their requests name a connector.

// Minimums are the connector versions served now: SetMinimums's, else
// Server.Connectors.
func (s *Server) Minimums() connectorversion.Minimums {
	if m := s.minimums.Load(); m != nil {
		return *m
	}
	return s.Connectors
}

// SetMinimums changes the versions served while the server runs: new
// requests are checked against them, and a live connection whose connector
// they no longer serve ends at its next renewal (within Server.Renew).
func (s *Server) SetMinimums(m connectorversion.Minimums) { s.minimums.Store(&m) }

// current returns an *UpdateRequired if the connector this connection
// recorded when it connected (Link.Connector) is older than the core
// serves now: the minimums may have been raised since.
func (st *stream) current() error {
	c, ok := connectorversion.Parse(st.l.Connector)
	if ok && st.s.Minimums().Outdated(c) {
		return &UpdateRequired{Client: c}
	}
	return nil
}

// endOutdated ends the connection if err is an *UpdateRequired:
// {"type":"hub.closed","reason":"update_required","note":…} (on a
// WebSocket, close code CodeUpdateRequired). What it was handed and
// didn't confirm goes back, held for the session's tool results.
func (st *stream) endOutdated(err error, log *slog.Logger) bool {
	var u *UpdateRequired
	if !errors.As(err, &u) {
		return false
	}
	log.Info("streams: the connector is out of date; closing", "connector", u.Client.String())
	st.t.end(endUpdateRequired, Ending{"update_required", u.Message(st.s.PublicURL)})
	return true
}

// UpdateRequired is why a connector's request was refused: it's older
// than the core serves.
type UpdateRequired struct {
	Client connectorversion.Client
}

func (e *UpdateRequired) Error() string {
	return "streams: connector " + e.Client.String() + " is out of date"
}

// Message is what the connector shows its agent (harnesses.UpdateRequired).
func (e *UpdateRequired) Message(base string) string {
	h, ok := harnesses.Get(e.Client.Harness)
	if !ok {
		h = harnesses.Harness{ID: e.Client.Harness}
	}
	return harnesses.UpdateRequired(h, base)
}

// seen records the connector a request on l names (header), or for a
// connector's link without one, version 0 of its connector: on the link
// and its session, if they say otherwise. With check, an outdated one is
// refused (returned, after it's recorded). Nothing for a link an agent
// holds itself whose request names no connector.
//
// The session's row is written before the link's: the order a grace
// renewal (grace.go) and an unsubscribe stopping the session (appapi's
// lockWatching, then RevokeSessionLinks) lock them in, so none of them
// waits on another in a circle. Call it before anything else in the
// transaction writes the link's row (use does).
func (s *Server) seen(ctx context.Context, tx pgx.Tx, l *Link, header string, check bool) (refused error, err error) {
	if header == "" && (l.Connector == "" || !check) {
		return nil, nil // an agent's own link, or an unversioned ack: nothing to record
	}
	harness := ""
	if was, ok := connectorversion.Parse(l.Connector); ok {
		harness = was.Harness
	}
	c := connectorversion.Identify(header, harness)
	now := c.String()
	if uuidRE.MatchString(l.Target) {
		if _, err := tx.Exec(ctx, `UPDATE sessions SET connector = $2 WHERE id = $1::uuid AND connector IS DISTINCT FROM $2`, l.Target, now); err != nil {
			return nil, err
		}
	}
	if now != l.Connector {
		if _, err := tx.Exec(ctx, `UPDATE stream_links SET connector = $2 WHERE id = $1`, l.ID, now); err != nil {
			return nil, err
		}
		l.Connector = now
	}
	if check && s.Minimums().Outdated(c) {
		return &UpdateRequired{Client: c}, nil
	}
	return nil, nil
}

// writeUpdateRequired writes a refusal for an outdated connector (426
// update_required), if err is one.
func (s *Server) writeUpdateRequired(w http.ResponseWriter, err error) bool {
	var u *UpdateRequired
	if !errors.As(err, &u) {
		return false
	}
	httpx.WriteError(w, http.StatusUpgradeRequired, "update_required", u.Message(s.PublicURL))
	return true
}
