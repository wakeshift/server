package streams

// A one-shot wait: a stream read by a command that exits when something
// arrives, for agents woken by a background command ending (Antigravity's
// run_command) or that loop on it (Claude Code's Monitor):
//
//	GET /v1/stream/wait?link=<token>[&timeout=45m][&after=<cursor>][&until=hooks]   (or Authorization: Bearer <token>)
//	GET /v1/stream/wait?link=<token>&after=<cursor>&confirm=1
//
// It answers 200 at once and then writes, as lines (format=lines: the
// same compact JSON line per event, marked as outside data), the stream's
// waiting deliveries as soon as there's at least one, then a cursor line
// naming them, and a last line saying what to do next, and closes:
//
//	{"type":"cursor","after":"<cursor>"}       pass after=<cursor> next time: it confirms these
//	{"type":"hub.next","url":"…","note":"…"}   run it again with url
//	{"type":"hub.closed","reason":…,"note":…}  don't: the note says why
//
// With nothing for timeout (default DefaultWaitTimeout), it writes only the
// last line. While it waits, an empty line every WaitKeepalive. Each is
// one line the server writes: an event's data is inside its line, so a
// line that starts {"type":"hub.closed" or {"type":"cursor" is always the
// server's own (a shell loop matches whole lines, never inside one).
//
// after=<cursor> confirms exactly the batch the cursor names, which a wait
// on this stream handed out (still leased, or handed back since): the
// client printed it. The command keeps the cursor in a shell variable, so
// the command (and its url) is the same every time, and a harness's
// "always allow" for it keeps matching. Without after=, nothing is
// confirmed: what an earlier wait returned that its client never printed
// (it was killed, or the answer was lost behind a front end) is handed
// back as the wait takes the stream, and comes again (redelivered). With
// confirm=1 it only confirms, and answers at once with the last line (a
// command that ends when messages arrive confirms them before it exits).
// What isn't confirmed is also handed back when its lease runs out, or
// when something else takes the stream (a poll, a connection).
// ack=<cursor> (earlier versions' url) is after=.
//
// The url carries the next link once this one is close to its last expiry
// (session links slide while they're used: SessionLinkMax), and the wait's
// timeout and until.
//
// Like a poll, a wait takes the stream (the newest wins: an older wait
// ends with hub.closed "replaced"), counts as connected while it waits and
// for WaitGrace after, and ends at once if its link is revoked. Hooks'
// polls don't take the stream from a live wait. When Wakeshift's hooks
// connect the session, they take over (TakeOver): a wait they take the
// stream from says to run it again, and the command's next wait takes it
// back; with until=hooks (Claude Code, whose hooks replace the command) it
// ends instead, with hub.closed "hooks" and the note that they're
// connected.

import (
	"context"
	"encoding/hex"
	"errors"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/connectorversion"
	"github.com/wakeshift/server/internal/deliveries"
)

// Wait limits.
const (
	DefaultWaitTimeout = 45 * time.Minute
	MaxWaitTimeout     = 50 * time.Minute // MaxAge: under Cloud Run's 60-minute request limit
	// DefaultWaitLease: how long what a wait returned waits to be
	// confirmed before it's handed back.
	DefaultWaitLease = 15 * time.Minute
	// DefaultWaitGrace: a session counts as connected for this long after
	// a wait ends (Server.WaitGrace). A loop runs the next at once; an agent
	// woken by the exit runs it first thing.
	DefaultWaitGrace = 30 * time.Second
	// waitRotateLead: a wait ending within this of its link's expiry hands
	// over the next link, so the command run again still works.
	waitRotateLead = 15 * time.Minute
)

// WaitURL is a stream URL (wss://…/v1/stream?link=…) as its one-shot
// wait: https://…/v1/stream/wait?link=…, the same url every time.
func WaitURL(stream string) string {
	return waitURL(stream, "")
}

func waitURL(stream, timeout string) string {
	u, err := url.Parse(stream)
	if err != nil {
		return stream
	}
	switch u.Scheme {
	case "wss":
		u.Scheme = "https"
	case "ws":
		u.Scheme = "http"
	}
	u.Path = strings.TrimSuffix(u.Path, "/") + "/wait"
	q := "link=" + url.QueryEscape(u.Query().Get("link"))
	if timeout != "" {
		q += "&timeout=" + url.QueryEscape(timeout)
	}
	u.RawQuery = q
	return u.String()
}

// Notes a wait ends with.
const (
	waitQuiet     = "Nothing arrived."
	waitRestart   = "The server is restarting."
	waitReplaced  = "A newer wait for this conversation started, so this one stopped. Nothing to do here: the newer one gets the messages."
	waitTaken     = "Something else read this conversation's messages for a moment."
	waitConfirmed = "Confirmed."
	waitWentWrong = "Something went wrong. Run the same command again, in the background as before."
	waitBusy      = "Nothing was handed out: the server was busy."
)

// ServeWait serves GET /v1/stream/wait.
func (s *Server) ServeWait(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Has("loop") {
		s.serveLoop(w, r) // loop mode (waitloop.go)
		return
	}
	timeout := DefaultWaitTimeout
	tp := q.Get("timeout")
	if tp != "" {
		d, err := time.ParseDuration(tp)
		if err != nil {
			n, nerr := strconv.Atoi(tp)
			if nerr != nil {
				httpx400(w, `timeout is a duration ("45m") or seconds`)
				return
			}
			d = time.Duration(n) * time.Second
		}
		if d < 0 {
			httpx400(w, "timeout can't be negative")
			return
		}
		timeout = min(d, MaxWaitTimeout)
	}
	// after: the cursor of the batch the client printed (ack=, from earlier
	// versions). One that names no batch confirms nothing.
	after := q.Get("after")
	if after == "" {
		after = q.Get("ack")
	}
	acked, _ := holderOfBatch(after)
	keep := url.Values{}
	if tp != "" {
		keep.Set("timeout", tp)
	}
	if q.Get("say") == "command" {
		keep.Set("say", "command")
	}
	untilHooks, untilWait := q.Get("until") == "hooks", q.Get("until") == "wait"
	if untilHooks || untilWait {
		keep.Set("until", q.Get("until"))
	}
	tok := linkToken(r)
	// A confirm-only request (a command about to exit) doesn't renew an
	// expired link: its answer is thrown away, and with it the successor.
	renew := q.Get("confirm") == ""
	c, err := s.parse(tok)
	if errors.Is(err, errLinkExpired) {
		if !renew || !s.renewWait(w, r, c, keep) {
			s.refuse(w, r, FormatLines, c)
		}
		return
	}
	if err != nil {
		writeLine(w, http.StatusUnauthorized, closedFrame(Ending{"invalid", "This isn't a working Wakeshift link, so this command can't wait for messages. Nothing more to do here."}))
		return
	}
	// At capacity, refused before any database work (a confirm-only
	// request takes no place, and goes on): held, then answered as a wait
	// that had nothing to hand out, with the same url to run next. Its
	// command goes round as after any quiet wait, so the agent isn't woken
	// for it, and what after= names is confirmed by a later wait.
	if renew {
		if err := s.atCapacity(refusalKey(c)); err != nil {
			s.holdRefused(r.Context(), err, max(timeout, time.Nanosecond)) // timeout=0: no longer than the pause
			writeLine(w, http.StatusOK, s.busyNext(tok, keep, after))
			return
		}
	}
	l, err := s.load(r.Context(), c, r, true)
	if errors.Is(err, errLinkInvalid) {
		if !renew || !s.renewWait(w, r, c, keep) {
			s.refuse(w, r, FormatLines, c)
		}
		return
	}
	if s.writeUpdateRequired(w, err) {
		return
	}
	if err != nil {
		slog.Error("streams: loading link", "err", err)
		w.Header().Set("Retry-After", "2")
		writeLine(w, http.StatusServiceUnavailable, closedFrame(Ending{"error", waitWentWrong}))
		return
	}
	ctx := r.Context()
	// The batch the cursor names was printed: confirmed, whatever holds the
	// stream now (if it was handed back meanwhile, too).
	if acked != "" {
		if err := s.DB.InTenant(ctx, c.Tenant, func(tx pgx.Tx) error { // the link was checked just now (load)
			return deliveries.AckHolder(ctx, tx, l.Agent, l.Target, acked)
		}); err != nil {
			slog.Error("streams: confirming a wait's batch", "err", err)
			w.Header().Set("Retry-After", "2")
			writeLine(w, http.StatusServiceUnavailable, closedFrame(Ending{"error", waitWentWrong}))
			return
		}
	}
	t := newHTTPTransport(w, FormatLines)
	if q.Get("confirm") != "" {
		t.send(ctx, s.waitNext(ctx, c.Tenant, l, tok, keep, "", waitConfirmed), "")
		return
	}
	// The link was good when the wait started: it stays good until it ends,
	// unless revoked (its expiry aside, as a stream's).
	valid := func(ctx context.Context, tx pgx.Tx) error {
		_, err := loadTx(ctx, tx, l.ID, true)
		if errors.Is(err, errLinkInvalid) {
			return ErrRevoked
		}
		return err
	}
	// A session whose last subscription ended has nothing more to wait
	// for: say so rather than wait.
	if why, done := s.waitDone(ctx, c.Tenant, l); done {
		t.end(endRevoked, why)
		return
	}
	// Claude Code's command ends once its hooks connect the session (they
	// replace it), however the rounds and the hooks' first poll interleave:
	// the wait checks as it takes the stream (errHooksConnected).
	var holder string
	batch, err := s.longPoll(ctx, PollRequest{Tenant: c.Tenant, Agent: l.Agent, Target: l.Target, Valid: valid, Wait: timeout, Connector: l.Connector},
		pollOpts{wait: true, holder: &holder, untilHooks: untilHooks, untilWait: untilWait, keepalive: func() error { return t.ping(ctx) }, app: linkApp(l)})
	s.admittedAs(refusalKey(c), err)
	var installed *WaitInstalled
	switch {
	case ctx.Err() != nil:
		return
	case errors.As(err, &installed):
		if _, err := s.hooksConnected(ctx, c.Tenant, l); err != nil { // said now: no longer due
			slog.Warn("streams: checking a session's hooks failed", "err", err)
		}
		t.end(endReplaced, Ending{"wait", WaitInstalledNote(installed.Command)})
		return
	case errors.Is(err, errWebhookBound):
		t.end(endReplaced, Ending{"webhook", WebhookNote})
		return
	case errors.Is(err, errHooksConnected):
		if _, err := s.hooksConnected(ctx, c.Tenant, l); err != nil { // said now: no longer due
			slog.Warn("streams: checking a session's hooks failed", "err", err)
		}
		t.end(endReplaced, Ending{"hooks", HooksConnected})
		return
	case errors.Is(err, ErrRevoked):
		t.end(endRevoked, s.endingOf(ctx, c, true))
		return
	case errors.Is(err, ErrReplaced):
		s.replacedWait(ctx, t, c.Tenant, l, tok, keep, untilHooks, untilWait)
		return
	case errors.Is(err, ErrDraining):
		t.send(ctx, s.waitNext(ctx, c.Tenant, l, tok, keep, "", waitRestart), "")
		return
	case errors.Is(err, errBusy):
		// Full since the check above: as there. Never an error note: the
		// agent would spend a turn on it, and nothing went wrong.
		s.holdRefused(ctx, err, max(timeout, time.Nanosecond))
		t.send(ctx, s.busyNext(tok, keep, after), "")
		return
	case err != nil:
		slog.Warn("streams: a wait failed", "err", err)
		t.end(endError, Ending{"error", waitWentWrong})
		return
	}
	for _, f := range batch.Deliveries {
		if err := t.send(ctx, f, f.DeliveryID); err != nil {
			return // they stay leased, and come again
		}
	}
	if len(batch.Deliveries) == 0 {
		// Its last subscription may have ended meanwhile.
		if why, done := s.waitDone(ctx, c.Tenant, l); done {
			t.end(endRevoked, why)
			return
		}
		t.send(ctx, s.waitNext(ctx, c.Tenant, l, tok, keep, "", waitQuiet), "")
		return
	}
	if err := t.send(ctx, cursorFrame{Type: "cursor", After: batchOf(holder)}, ""); err != nil {
		return
	}
	t.send(ctx, s.waitNext(ctx, c.Tenant, l, tok, keep, batchOf(holder), ""), "")
}

// renewWait answers a wait on an expired session link that can be renewed
// (renewExpired) with the hub.next line naming its successor (with keep,
// the wait's timeout and until), at once: the command runs again with it
// and gets what's waiting. Nothing is confirmed (an after= cursor's batch
// comes again, unless the next wait passes it). false, having written
// nothing, if it can't be renewed (the caller refuses it as before).
func (s *Server) renewWait(w http.ResponseWriter, r *http.Request, c claims, keep url.Values) bool {
	next, exp, _, err := s.renewExpired(r.Context(), c, r.Header.Get(connectorversion.Header), true)
	switch {
	case errors.Is(err, errNoGrace):
		return false
	case s.writeUpdateRequired(w, err):
		return true
	case err != nil:
		slog.Error("streams: renewing an expired link", "err", err)
		w.Header().Set("Retry-After", "2")
		writeLine(w, http.StatusServiceUnavailable, closedFrame(Ending{"error", waitWentWrong}))
		return true
	}
	u := waitURL(next, "")
	if len(keep) > 0 {
		u += "&" + keep.Encode()
	}
	writeLine(w, http.StatusOK, nextFrame{Type: "hub.next", URL: u, ExpiresAt: exp.UTC().Format(time.RFC3339), Note: renewalNote})
	return true
}

// cursorFrame is the line after a wait's messages that names them:
// after=<After> on the next wait (or a confirm) confirms them.
type cursorFrame struct {
	Type  string `json:"type"`
	After string `json:"after"`
}

// batchOf is a wait's batch name (its cursor) for its holder, and back.
func batchOf(holder string) string { return strings.TrimPrefix(holder, waitPrefix) }

func holderOfBatch(batch string) (string, bool) {
	if b, err := hex.DecodeString(batch); err != nil || len(b) != 8 {
		return "", false
	}
	return waitPrefix + batch, true
}

// replacedWait ends a wait something else took the stream from: a newer
// wait (the newer copy of the command gets the messages: this one stops),
// or not (Wakeshift's hooks took over, TakeOver: run it again, and its
// next wait takes the stream back; with untilHooks, and the session's hooks
// connected, it stops instead, saying they're connected; with untilWait,
// and the session's hooks connected with a wait command, it stops saying
// to run that).
func (s *Server) replacedWait(ctx context.Context, t *httpTransport, tenant string, l Link, tok string, keep url.Values, untilHooks, untilWait bool) {
	var holder, cmd string
	var bound bool
	err := s.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT holder FROM stream_leases WHERE agent_id = $1 AND target = $2`, l.Agent, l.Target).Scan(&holder); err != nil {
			return err
		}
		if s.CursorWebhooks {
			var err error
			if bound, err = webhookBound(ctx, tx, l.Target); err != nil || bound {
				return err
			}
		}
		if !isWait(holder) && untilWait && uuidRE.MatchString(l.Target) {
			var err error
			cmd, err = waitCommandOf(ctx, tx, l.Target)
			return err
		}
		return nil
	})
	hooks := false
	if err == nil && !isWait(holder) && (untilHooks || cmd != "") {
		hooks, err = s.hooksConnected(ctx, tenant, l)
	}
	switch {
	case err != nil:
		slog.Warn("streams: explaining a replaced wait failed", "err", err)
		t.end(endReplaced, Ending{"replaced", waitReplaced})
	case bound:
		t.end(endReplaced, Ending{"webhook", WebhookNote})
	case isWait(holder):
		t.end(endReplaced, Ending{"replaced", waitReplaced})
	case cmd != "":
		t.end(endReplaced, Ending{"wait", WaitInstalledNote(cmd)})
	case hooks:
		t.end(endReplaced, Ending{"hooks", HooksConnected})
	default:
		t.send(ctx, s.waitNext(ctx, tenant, l, tok, keep, "", waitTaken), "")
	}
}

// hooksConnected reports whether Wakeshift's hooks deliver the link's
// session's messages (it delivers on its own stream, a connector redeemed
// for it, and their waiter polled lately: hooksDeliverSQL), and if so,
// that the agent has been told (connected_note_due: the wait's last line
// says so).
func (s *Server) hooksConnected(ctx context.Context, tenant string, l Link) (bool, error) {
	if !uuidRE.MatchString(l.Target) {
		return false, nil
	}
	var hooks bool
	err := s.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `UPDATE sessions s SET connected_note_due = false WHERE s.id = $1::uuid AND `+hooksDeliverSQL+`
			RETURNING true`, l.Target).Scan(&hooks)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		// The waits it ends stop counting as live (their last round's
		// grace too), so the hooks' waiter, whose poll yields to a live
		// wait, takes the stream at once (waitsGone).
		_, err = tx.Exec(ctx, `UPDATE stream_leases SET renewed_at = NULL WHERE agent_id = $1 AND target = $2 AND starts_with(holder, $3)`, l.Agent, l.Target, waitPrefix)
		return err
	})
	return hooks, err
}

// HooksConnected is a Claude Code wait command's last line once Wakeshift's
// hooks connect its session (until=hooks). It starts with
// harnesses.Connected, the note agents are told to look for.
const HooksConnected = "Wakeshift: connected. Wakeshift's hooks deliver this conversation's messages now, as soon as they arrive, so this command has stopped: don't start it again."

// WaitInstalledNote is a background command's last line (until=wait) once
// Wakeshift's hooks connect its session with Wakeshift's wait command,
// cmd: run that instead, the same way. It starts with harnesses.Connected.
func WaitInstalledNote(cmd string) string {
	return "Wakeshift: connected. Wakeshift's hooks are installed here, with Wakeshift's own wait command, so this command has stopped: don't start it again. " +
		"From now on, start `" + cmd + "` instead, the same way you started this one (the same tool and settings, with that as the command). " +
		"It's the same in every conversation, and installing Wakeshift may have pre-approved it."
}

// waitDone reports whether a session's stream has nothing left to receive
// (Server.Ending), and why: not while something is waiting for it, or a
// wait's batch isn't confirmed (this one gets that again).
func (s *Server) waitDone(ctx context.Context, tenant string, l Link) (Ending, bool) {
	if s.Ending == nil || l.Target == "" {
		return Ending{}, false
	}
	var why Ending
	var done bool
	err := s.DB.InTenant(ctx, tenant, func(tx pgx.Tx) (err error) {
		if why, done, err = s.Ending(ctx, tx, l, true); err != nil || !done {
			return err
		}
		// What's still waiting (the notice that it ended, last) comes first.
		var pending bool
		if pending, err = deliveries.Pending(ctx, tx, l.Agent, l.Target); err != nil || pending {
			done = false
			return err
		}
		// A wait before it that wasn't confirmed: this one gets that again.
		err = tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM deliveries d JOIN stream_leases s ON s.tenant_id = d.tenant_id AND d.lease_holder = s.holder
			WHERE s.agent_id = $1 AND s.target = $2 AND s.holder LIKE 'poll\_wait\_%' AND d.state = 'in_flight')`, l.Agent, l.Target).Scan(&pending)
		if done = done && !pending; !done || err != nil {
			return err
		}
		// The command stops at this hub.closed: it no longer counts as
		// connected (no wait's grace), so subscribing again right after
		// gives a command to start, not "already running".
		_, err = tx.Exec(ctx, `UPDATE stream_leases SET renewed_at = NULL WHERE agent_id = $1 AND target = $2 AND holder LIKE 'poll\_wait\_%'`, l.Agent, l.Target)
		return err
	})
	if err != nil {
		slog.Warn("streams: checking a wait's session failed", "err", err)
		return Ending{}, false
	}
	return why, done
}

// nextFrame is a wait's last line when it should run again:
// {"type":"hub.next","url":…,"note":…}, type and url first (a shell loop
// reads the url from the start of the line).
type nextFrame struct {
	Type      string `json:"type"`
	URL       string `json:"url"`
	ExpiresAt string `json:"expires_at,omitempty"` // a new link's
	Note      string `json:"note"`
}

// waitNext is a wait's last line: the url to run next (the same, with the
// session's next link if this one can't slide much longer), with keep (its
// timeout and until). returned: it returned messages, which the cursor
// confirms. what says what happened ("" for messages).
func (s *Server) waitNext(ctx context.Context, tenant string, l Link, tok string, keep url.Values, after string, what string) any {
	// The link is in use: it slides (a wait can outlast SessionLinkTTL).
	if err := s.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error { return slide(ctx, tx, &l) }); err != nil {
		slog.Warn("streams: extending a wait's link failed", "err", err)
	}
	stream, exp := "", ""
	if l.Target != "" && time.Until(l.expiry()) < waitRotateLead {
		next, e, err := s.next(ctx, tenant, l)
		switch {
		case err == nil:
			stream, exp = next, e.UTC().Format(time.RFC3339)
		case errors.Is(err, errLinkInvalid):
			return closedFrame(endingRevoked)
		default:
			slog.Warn("streams: rotating a wait's link failed", "err", err)
		}
	}
	return s.nextLine(tok, stream, exp, keep, after, what)
}

// busyNext is a refused wait's last line (busy: at capacity), made with no
// database work: the same url, nothing new to confirm. A foreground wait
// (say=command) is told the command to run with after, the cursor it came
// with, which this one didn't confirm; a command keeps its own cursor.
func (s *Server) busyNext(tok string, keep url.Values, after string) nextFrame {
	if after != "" && keep.Get("say") == "command" {
		keep = maps.Clone(keep)
		keep.Set("after", after)
	}
	return s.nextLine(tok, "", "", keep, "", waitBusy)
}

// nextLine is a wait's hub.next line: the url to run next (stream, the
// next link expiring at exp; or "" for tok's own) with keep, and its note.
func (s *Server) nextLine(tok, stream, exp string, keep url.Values, after, what string) nextFrame {
	if stream == "" {
		stream = strings.TrimSuffix(s.PublicURL, "/") + "/v1/stream?link=" + url.QueryEscape(tok)
	}
	note := "Run the command again now, as before."
	if exp != "" {
		note = "Run the command again now, as before, with this url (a new link)."
	}
	if after != "" {
		note += " Pass the cursor line's after= with it: that confirms the messages above (ones you don't confirm come again)."
	}
	u := waitURL(stream, "")
	if len(keep) > 0 {
		u += "&" + keep.Encode()
	}
	// Its parts are query-escaped, so it never holds a single quote; one
	// that did (a public URL with one) gets the plain note instead.
	if keep.Get("say") == "command" && !strings.Contains(u, "'") {
		// A foreground wait the agent runs by hand (say=command): the note
		// is the exact command to run next, the cursor's after= in it.
		run := u
		if after != "" {
			run += "&after=" + url.QueryEscape(after)
		}
		note = "Run this again now, in the foreground as before: curl -sN '" + run + "'"
		if after != "" {
			note += " (after you've handled the messages above; it confirms them)."
		} else {
			note += " ."
		}
	}
	return nextFrame{Type: "hub.next", URL: u, ExpiresAt: exp, Note: strings.TrimSpace(what + " " + note)}
}

// writeLine answers with one JSON line (a URL in it keeps its &: a shell
// loop reads it from the line as it is).
func writeLine(w http.ResponseWriter, status int, v any) {
	b, _ := marshalRaw(v)
	w.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
	w.WriteHeader(status)
	w.Write(append(b, '\n'))
}

func httpx400(w http.ResponseWriter, msg string) {
	writeLine(w, http.StatusBadRequest, closedFrame(Ending{"bad_request", msg}))
}
