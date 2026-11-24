package appapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/deliveries"
	"github.com/wakeshift/server/internal/httpx"
	"github.com/wakeshift/server/internal/sessions"
	"github.com/wakeshift/server/internal/streams"
)

// Pull delivery for apps: a wait tool (an MCP tool that blocks until the
// calling conversation has messages) and catch-up on other tool results.
// It's the session's stream long-polled (streams.LongPoll) under the
// agent's grant, so it follows the stream's rules: at least once, the
// newest poller holds the stream, a waiting session counts as connected.
// Unlike a link's poll, it never takes the stream from a live WebSocket (a
// Monitor or a connector already wakes that conversation). A connector that
// delivers only between turns (Codex's) is the exception: the conversation
// is in a turn and asked for its messages, which that connector would hold
// until the turn ends, so the wait takes them from it (streams.TakeHeld)
// and answers "woken": the session is woken once the turn ends.

// wait long-polls one of the agent's sessions for deliveries.
func (a *API) wait(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Session      string   `json:"session"`      // a handle the agent holds
		Conversation string   `json:"conversation"` // or the harness conversation's ID (a Codex thread)
		Label        string   `json:"label"`        // or a named session; default "wait"
		WaitSeconds  *int     `json:"wait_seconds"` // 0 to 55; default 25
		Limit        int      `json:"limit"`        // 1 to 100; default 20
		Ack          []string `json:"ack"`          // delivery IDs to ack first
	}
	if !httpx.Decode(w, r, &in) {
		return
	}
	wait := streams.DefaultPollWait
	if in.WaitSeconds != nil {
		if *in.WaitSeconds < 0 {
			httpx.WriteError(w, http.StatusBadRequest, "bad_request", "wait_seconds can't be negative")
			return
		}
		wait = min(time.Duration(*in.WaitSeconds)*time.Second, streams.MaxPollWait)
	}
	if in.Limit < 0 || in.Limit > streams.MaxPollBatch || len(in.Ack) > streams.MaxPollAcks {
		httpx.WriteError(w, http.StatusBadRequest, "bad_request", "limit is 1 to 100, with at most 100 acks")
		return
	}
	byDefault := in.Session == "" && in.Conversation == "" && in.Label == ""
	if byDefault {
		in.Label = "wait"
	}
	act := actorFrom(r.Context())
	var s sessions.Session
	stream := false // a live stream (WebSocket) delivers to it already
	turns := false  // and it's a connector that delivers only between turns
	subscribed := true
	err := a.tx(r, func(ctx context.Context, tx pgx.Tx) (err error) {
		n := sessions.New{Agent: act.agent, Resume: in.Session, Label: in.Label, Conversation: in.Conversation, CreatedBy: "app:" + act.app}
		s, err = sessions.Register(ctx, tx, n)
		if byDefault && errors.Is(err, sessions.ErrLabelTaken) {
			// Another app's wait has the agent's wait session: this app's
			// is its own (each app's wait takes only its own deliveries).
			n.Label = waitLabel(act.app)
			s, err = sessions.Register(ctx, tx, n)
		}
		if errors.Is(err, sessions.ErrNotFound) {
			return errNoSession
		}
		if err != nil {
			return badRequest{err}
		}
		if s.ConnectedVia == "stream" {
			stream = true
			if s.Runtime != sessions.Connector {
				turns, err = streams.HeldBetweenTurns(ctx, tx, act.agent, s.ID)
			}
			return err
		}
		// Nothing subscribed for this session (an agent's default wait
		// session it never watched with, say): nothing new can come, so the
		// wait takes what's left and answers at once, saying so, rather
		// than having the agent wait again and again for nothing. A topic
		// subscription counts by its own expiry, not its subscriptions row,
		// which stays active until the expiry pass ends it. Other
		// subscriptions don't expire (subscriptions.expires_at is unset, and
		// goes in a later release).
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM subscriptions s WHERE s.agent_id = $2 AND s.session_id = $1 AND s.status = 'active'
				AND NOT EXISTS (SELECT 1 FROM topic_subscriptions t WHERE t.subscription_id = s.id))
			OR EXISTS (SELECT 1 FROM topic_subscriptions WHERE session_id = $1 AND ended_at IS NULL AND expires_at > now())`, s.ID, act.agent).Scan(&subscribed); err != nil {
			return err
		}
		// A conversation delivered on the agent's own stream (a connector
		// session), with nothing holding that: it's waiting itself, so its
		// deliveries are its own from now on (as when a connector redeems).
		if s.Runtime == sessions.Connector {
			if _, err := tx.Exec(ctx, `UPDATE sessions SET runtime = 'stream' WHERE id = $1`, s.ID); err != nil {
				return err
			}
			err = deliveries.ToOwnStream(ctx, tx, s.Agent, s.ID)
		}
		return err
	})
	if err != nil {
		a.respond(w, nil, err)
		return
	}
	type answer struct {
		streams.Batch
		// StreamConnected: a live stream (a Monitor, a connector) already
		// delivers this session's messages, so nothing was waited for.
		StreamConnected bool `json:"stream_connected,omitempty"`
		// Woken: a connector that delivers between turns wakes this
		// session once the turn ends; what it held was taken for this wait.
		Woken bool `json:"woken,omitempty"`
		// Unsubscribed: nothing is subscribed for this session, so nothing
		// new can come to it; the wait didn't wait.
		Unsubscribed bool `json:"unsubscribed,omitempty"`
	}
	target := s.ID
	if turns {
		batch, err := a.Streams.TakeHeld(r.Context(), streams.PollRequest{Tenant: act.tenant, Agent: act.agent, Target: target,
			Valid: a.grantValid(act), Acks: in.Ack, Wait: wait, Limit: in.Limit, App: act.app})
		if a.writeWaitError(w, err) {
			return
		}
		batch.Session = s.Handle
		httpx.WriteJSON(w, http.StatusOK, answer{Batch: batch, Woken: true})
		return
	}
	if stream && s.Runtime == sessions.Connector {
		target = "" // acks are for the agent's connector stream
	}
	if stream {
		acked, err := a.Streams.AckBatch(r.Context(), act.tenant, act.agent, target, act.app, a.grantValid(act), in.Ack)
		if a.writeWaitError(w, err) {
			return
		}
		httpx.WriteJSON(w, http.StatusOK, answer{Batch: streams.Batch{Deliveries: []streams.Frame{}, Acked: acked, Session: s.Handle}, StreamConnected: true})
		return
	}
	if !subscribed {
		wait = 0
	}
	batch, err := a.Streams.LongPoll(r.Context(), streams.PollRequest{Tenant: act.tenant, Agent: act.agent, Target: target,
		Valid: a.grantValid(act), Acks: in.Ack, Wait: wait, Limit: in.Limit, Yield: true, App: act.app})
	if errors.Is(err, streams.ErrStreamConnected) {
		httpx.WriteJSON(w, http.StatusOK, answer{Batch: batch, StreamConnected: true})
		return
	}
	if a.writeWaitError(w, err) {
		return
	}
	batch.Session = s.Handle
	httpx.WriteJSON(w, http.StatusOK, answer{Batch: batch, Unsubscribed: !subscribed})
}

// waitLabel is an app's own default wait session's label, when another
// app's holds agent@wait: wait-<8 hex digits of the app's ID's hash>.
func waitLabel(app string) string {
	h := sha256.Sum256([]byte(app))
	return "wait-" + hex.EncodeToString(h[:4])
}

// ackDeliveries records acks for one of the agent's sessions.
func (a *API) ackDeliveries(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Session     string   `json:"session"`
		DeliveryIDs []string `json:"delivery_ids"`
	}
	if !httpx.Decode(w, r, &in) {
		return
	}
	if len(in.DeliveryIDs) == 0 || len(in.DeliveryIDs) > streams.MaxPollAcks {
		httpx.WriteError(w, http.StatusBadRequest, "bad_request", `send {"session": "…", "delivery_ids": [...]}, 1 to 100`)
		return
	}
	act := actorFrom(r.Context())
	var s sessions.Session
	err := a.tx(r, func(ctx context.Context, tx pgx.Tx) (err error) {
		s, err = ownSession(ctx, tx, act, in.Session)
		return err
	})
	if err != nil {
		a.respond(w, nil, err)
		return
	}
	target := s.ID
	if s.Runtime == sessions.Connector {
		target = ""
	}
	acked, err := a.Streams.AckBatch(r.Context(), act.tenant, act.agent, target, act.app, a.grantValid(act), in.DeliveryIDs)
	if a.writeWaitError(w, err) {
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"session": s.Handle, "acked": acked})
}

// seen acknowledges what's still waiting for one of the agent's sessions
// among the messages the app just showed that conversation another way (a
// tool's result: Agent Chat's history), named by their events' data "id",
// so they don't wake it again (deliveries.AckSeen: only this app's, and
// only what's pending or held by a between-turns connector, which is told).
func (a *API) seen(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Session string   `json:"session"`
		IDs     []string `json:"ids"`
	}
	if !httpx.Decode(w, r, &in) {
		return
	}
	if len(in.IDs) == 0 || len(in.IDs) > streams.MaxPollAcks {
		httpx.WriteError(w, http.StatusBadRequest, "bad_request", `send {"session": "…", "ids": [...]}, 1 to 100`)
		return
	}
	for _, id := range in.IDs {
		if id == "" || len(id) > 200 {
			httpx.WriteError(w, http.StatusBadRequest, "bad_request", "each id is 1 to 200 characters")
			return
		}
	}
	act := actorFrom(r.Context())
	acked := []string{}
	var handle string
	err := a.tx(r, func(ctx context.Context, tx pgx.Tx) error {
		if err := a.grantValid(act)(ctx, tx); err != nil {
			return err
		}
		s, err := ownSession(ctx, tx, act, in.Session)
		if err != nil {
			return err
		}
		handle = s.Handle
		if s.Runtime == sessions.Connector {
			return nil // delivered on the agent's own stream, which acks its own
		}
		ids, took, err := deliveries.AckSeen(ctx, tx, act.agent, s.ID, act.app, in.IDs)
		if err != nil {
			return err
		}
		acked = append(acked, ids...)
		if took {
			_, err = tx.Exec(ctx, `SELECT pg_notify($1, $2)`, deliveries.NotifyChannel, act.tenant+":"+act.agent)
		}
		return err
	})
	if errors.Is(err, streams.ErrRevoked) {
		a.writeWaitError(w, err)
		return
	}
	a.respond(w, map[string]any{"session": handle, "acked": acked}, err)
}

// inbox says what's waiting for one of the agent's sessions, without
// delivering it: for an app to mention on its other tool results.
func (a *API) inbox(w http.ResponseWriter, r *http.Request) {
	act := actorFrom(r.Context())
	var out struct {
		Session      string   `json:"session"`
		Pending      int      `json:"pending"`
		From         []string `json:"from"`
		ConnectedVia string   `json:"connected_via,omitempty"`
	}
	err := a.tx(r, func(ctx context.Context, tx pgx.Tx) error {
		s, err := ownSession(ctx, tx, act, r.PathValue("handle"))
		if err != nil {
			return err
		}
		out.Session, out.ConnectedVia = s.Handle, s.ConnectedVia
		// Only this app's: what other apps delivered to the conversation is
		// theirs to tell (deliveries.OfApp).
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM deliveries WHERE agent_id = $1 AND target = $2 AND state = 'pending' AND available_at <= now()
			AND `+deliveries.OfApp("deliveries", "$3"), act.agent, s.ID, act.app).Scan(&out.Pending); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT f FROM (SELECT coalesce(nullif(e.from_handle, ''), e.from_id) AS f, min(d.event_seq) AS first
			FROM deliveries d JOIN events e ON e.tenant_id = d.tenant_id AND e.id = d.event_id
			WHERE d.agent_id = $1 AND d.target = $2 AND d.state = 'pending' AND d.available_at <= now() AND `+deliveries.OfApp("d", "$3")+`
			GROUP BY 1) x ORDER BY first LIMIT 10`, act.agent, s.ID, act.app)
		if err != nil {
			return err
		}
		out.From, err = pgx.CollectRows(rows, pgx.RowTo[string])
		return err
	})
	if out.From == nil {
		out.From = []string{}
	}
	a.respond(w, out, err)
}

// grantValid is a wait's streams.PollRequest.Valid: the agent's grant for
// the app is still live.
func (a *API) grantValid(act actor) func(context.Context, pgx.Tx) error {
	return func(ctx context.Context, tx pgx.Tx) error {
		var live bool
		err := tx.QueryRow(ctx, `SELECT revoked_at IS NULL AND expires_at > now() FROM grants WHERE id = $1`, act.grant.ID).Scan(&live)
		if errors.Is(err, pgx.ErrNoRows) || err == nil && !live {
			return streams.ErrRevoked
		}
		return err
	}
}

func (a *API) writeWaitError(w http.ResponseWriter, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, streams.ErrRevoked):
		httpx.WriteError(w, http.StatusForbidden, "no_grant", "the agent's consent for this app was revoked or has expired")
	case errors.Is(err, streams.ErrReplaced):
		httpx.WriteError(w, http.StatusConflict, "replaced", "a newer wait (or connection) for this session took over")
	case errors.Is(err, streams.ErrDraining):
		httpx.WriteError(w, http.StatusServiceUnavailable, "restarting", "server restarting; try again")
	case errors.Is(err, context.Canceled):
	default:
		slog.Error("appapi: waiting failed", "err", err)
		httpx.WriteError(w, http.StatusServiceUnavailable, "unavailable", "try again")
	}
	return true
}
