package appapi

// MCP wait tasks: Claude Code's no-install wake path. For a client that takes MCP
// tasks (protocol 2026-07-28 with the tasks extension), an app's SDK lists
// Wakeshift's wait tool (harnesses.WaitTool) and passes its calls, and the
// client's tasks/get and tasks/cancel for the task it answers with, here:
//
//	POST /api/apps/v1/calls/task         {call}                       → {result}
//	POST /api/apps/v1/calls/task/get     {task, call, hold_seconds?}  → {result} | {rpc_error}
//	POST /api/apps/v1/calls/task/cancel  {task, call}                 → {result} | {rpc_error}
//
// result is the MCP result the SDK answers with (adding nothing but the
// JSON-RPC envelope), so every SDK says the same; rpc_error is a JSON-RPC
// error it answers with instead (an ID that isn't a task of this caller's).
//
// A task is only its ID (internal/waittask: sealed, naming the
// conversation's workspace and session, bound to the app), so any instance
// answers for it after a deploy, and Claude Code, which follows a task
// across /compact, /exit and --continue, never finds it forgotten. Each
// tasks/get is a round of the conversation's one-shot wait
// (streams.TaskWait), held up to hold_seconds, under the task's own lease
// holder: what it hands out is the task's batch, found again from the ID
// on every later poll (streams.TaskBatch), and confirmed when the agent
// calls the wait tool again (callsTask acks the previous task's holder), so
// a result that never reached the model comes again. The session keeps
// its newest task's holder (sessions.wait_task): an older task is
// replaced, and can't take the stream back. It also keeps each app's
// newest (sessions.wait_tasks), which is what that app's next call
// confirms: a call of one app never confirms another app's batch, which
// that app's agent may not have read.
//
// Every use checks that the caller is the conversation the task is for:
// the ID opens only for the app it was made for, its workspace must be the
// caller's (the app's user, the agent token's, or for a conversation's own
// workspace, no user), the client must be the session's agent, and the
// session must still be the app's with the agent's consent live. An ID
// that fails any of these reads as unknown, the same as a made-up one.

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/apps"
	"github.com/wakeshift/server/internal/deliveries"
	"github.com/wakeshift/server/internal/grants"
	"github.com/wakeshift/server/internal/harnesses"
	"github.com/wakeshift/server/internal/httpx"
	"github.com/wakeshift/server/internal/sessions"
	"github.com/wakeshift/server/internal/streams"
	"github.com/wakeshift/server/internal/waittask"
)

const (
	// taskPollMs is how long the client waits after each tasks/get answer
	// before asking again (Claude Code clamps it to 250-30000 ms): the hold
	// does the waiting.
	taskPollMs = 1000
	// taskHold is how long a tasks/get holds a working task's answer by
	// default; taskMaxHold, at most (under the SDKs' request timeouts).
	taskHold    = 20 * time.Second
	taskMaxHold = 25 * time.Second
	// taskIdle: a conversation whose client hasn't asked about its task
	// (or started one) for this long isn't waiting: its next wait confirms
	// nothing the last one handed out (confirmPrevious), and hands it out
	// again.
	taskIdle = 3 * time.Minute
	// taskWorking is a working task's status message.
	taskWorking = "Waiting for Wakeshift messages."
)

// ConversationTaskMaxAge is how long a wait task's ID works in a
// conversation's own workspace (no user), where nothing but the ID says
// the caller is the conversation (tasks/get carries no session key): a
// day, not waittask.MaxAge, so an ID read from a transcript doesn't
// collect the conversation's messages for a month. Past it, the task
// fails as an expired one does, and the conversation calls the wait tool
// again (with its session key).
const ConversationTaskMaxAge = 24 * time.Hour

// errUnknownTask: the ID isn't a task of this caller's (forged, another
// app's, workspace's, client's or conversation's, or malformed).
var errUnknownTask = errors.New("unknown task")

// errTaskReplaced: a newer task took the conversation's wait (or it was
// stopped) while a round of this one was starting.
var errTaskReplaced = errors.New("the wait task was replaced")

// rpcInvalidParams is JSON-RPC's code for an unknown task.
const rpcInvalidParams = -32602

// taskPath reports whether cv is woken by the wait tool's task rather than
// its harness's zero-install path: its SDK serves the wait tool for its
// client, which takes MCP tasks (callIn.Tasks), its harness has the task
// path (harnesses.Harness.Tasks), and this core makes tasks.
func (a *API) taskPath(cv *conversation) bool {
	if a.Tasks == nil || !cv.call.Tasks {
		return false
	}
	h, ok := harnesses.Get(cv.harness)
	return ok && h.Tasks
}

// taskLive reports whether the session's client is following a wait task:
// it started or asked about one within taskIdle.
func taskLive(ctx context.Context, tx pgx.Tx, session string) (bool, error) {
	var live bool
	err := tx.QueryRow(ctx, `SELECT wait_task IS NOT NULL AND coalesce(wait_task_seen_at > now() - make_interval(secs => $2), false) FROM sessions WHERE id = $1`,
		session, taskIdle.Seconds()).Scan(&live)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return live, err
}

// claudeCodeTaskResult is claudeCodeResult for a conversation on the task
// path: the wait tool instead of the Monitor. Subscribing says to call it
// (unless its task is followed now); other results add no instructions.
// Until Wakeshift's hooks
// connect, results carry a beacon, as on the Monitor path, so hooks
// installed since connect (and end the wait).
func (a *API) claudeCodeTaskResult(ctx context.Context, tx pgx.Tx, cv *conversation, h harnesses.Harness, why resultFor) (toolResult, error) {
	s := cv.session
	var out toolResult
	live, err := taskLive(ctx, tx, s.ID)
	if err != nil {
		return out, err
	}
	if live {
		if why == forSubscribe {
			out.Text = join(harnesses.TaskRunningNote(cv.handle), passSession(cv))
		} else if recent, err := recentTicket(ctx, tx, cv); err != nil || recent {
			return out, err
		}
		return out, a.withBeacon(ctx, tx, cv, h, &out)
	}
	if why == forAnnotate {
		// Nothing to say (what arrives waits for the session): only a
		// beacon, for hooks the user may have installed since.
		if watching, err := watchingTopics(ctx, tx, s.ID); err != nil || !watching {
			cv.noBeacon = "not_watching"
			return out, err
		}
		if recent, err := recentTicket(ctx, tx, cv); err != nil || recent {
			cv.noBeacon = "recent_ticket"
			return out, err
		}
		return out, a.withBeacon(ctx, tx, cv, h, &out)
	}
	before, err := cv.toldBefore(ctx, tx)
	if err != nil {
		return out, err
	}
	if before {
		again := harnesses.TaskAgainNote(appName(cv), cv.handle)
		note := harnesses.PluginPendingNote(withTools(h, cv), a.base(), true, again)
		if why == forSubscribe {
			if c, err := connecting(ctx, tx, s.ID); err != nil {
				return out, err
			} else if c {
				// No new beacon: the one connecting is enough, and a fresh
				// one each call would keep this answer going for good.
				out.Text = join(harnesses.PluginConnectingNote(h, a.base(), true), passSession(cv))
				cv.noBeacon = "connecting"
				return out, nil
			}
		}
		out.Text = join(note, passSession(cv))
	} else {
		// The plugin first, the wait only after a no (harnesses.PluginFirstNote).
		wait := join(harnesses.TaskNote(appName(cv), cv.handle),
			"If Wakeshift's plugin for Claude Code connects this conversation later, "+harnesses.WaitTool+"'s result says \""+harnesses.Connected+"\": then don't call it again, since the plugin delivers instead.")
		out.Text = join(harnesses.PluginFirstNote(withTools(h, cv), a.base(), appName(cv), true, wait), passSession(cv))
	}
	return out, a.withBeacon(ctx, tx, cv, h, &out)
}

// taskResult is an MCP result: a task's state (tasks/get's, and the wait
// tool's call's with resultType "task"), or a plain tool result.
type taskResult map[string]any

// taskView is a task's state as tasks/get answers it.
func taskView(id string, created, updated time.Time, status, message string) taskResult {
	v := taskResult{
		"resultType":     "complete",
		"taskId":         id,
		"status":         status,
		"createdAt":      created.UTC().Format(time.RFC3339),
		"lastUpdatedAt":  updated.UTC().Format(time.RFC3339),
		"ttlMs":          nil,
		"pollIntervalMs": taskPollMs,
	}
	switch status {
	case "completed":
		v["result"] = map[string]any{"content": []any{map[string]any{"type": "text", "text": message}}}
	case "failed":
		v["statusMessage"] = message
		v["error"] = map[string]any{"code": -32603, "message": message}
	default:
		v["statusMessage"] = message
	}
	return v
}

// plainResult is a tool result with one text block, an error if isError.
func plainResult(text string, isError bool) taskResult {
	r := taskResult{"resultType": "complete", "content": []any{map[string]any{"type": "text", "text": text}}}
	if isError {
		r["isError"] = true
	}
	return r
}

// callsTask is the wait tool's call: a new task for the calling
// conversation, which confirms what the conversation's previous task
// handed out (the agent read that result, and called again) and replaces
// it. A conversation that can't wait this way (its client doesn't take
// tasks, nothing is subscribed, its hooks deliver, or it isn't known) gets
// a plain result saying what to do instead.
func (a *API) callsTask(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Call callIn `json:"call"`
	}
	if !httpx.Decode(w, r, &in) {
		return
	}
	app := AppFrom(r.Context())
	ctx := r.Context()
	if a.Tasks == nil || !in.Call.Tasks {
		a.respond(w, map[string]any{"result": plainResult("This MCP client can't run "+harnesses.WaitTool+" as a background task. Follow the instructions in the result of the app's tool that subscribed you instead.", true)}, nil)
		return
	}
	var cv *conversation
	var handle string
	var err error
	agentMode := in.Call.AgentToken != ""
	if agentMode {
		cv, handle, err = a.agentCall(ctx, app, in.Call)
	} else {
		cv, err = a.findConversation(ctx, app, in.Call, false)
	}
	if errors.Is(err, errNoConversation) || errors.Is(err, errNoSession) {
		a.respond(w, map[string]any{"result": plainResult(noTaskSession, true)}, nil)
		return
	}
	if err != nil {
		a.respond(w, nil, err)
		return
	}
	var out taskResult
	err = a.DB.InTenant(ctx, cv.tenant, func(tx pgx.Tx) error {
		if agentMode {
			if err := agentResolve(ctx, tx, cv, handle, "", false); err != nil {
				return err
			}
		} else if err := a.resolve(ctx, tx, cv, false); err != nil {
			return err
		}
		s, err := sessions.Get(ctx, tx, cv.session.ID) // fresh: its hooks
		if err != nil {
			return err
		}
		if !s.Joined(app.ID) {
			return errNoSession
		}
		if hooks, err := hooksDeliver(ctx, tx, s); err != nil || hooks {
			out = plainResult(harnesses.TaskHooks, false)
			return err
		}
		var subscribed bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM topic_subscriptions WHERE session_id = $1 AND ended_at IS NULL AND expires_at > now())
			OR EXISTS (SELECT 1 FROM subscriptions s WHERE s.session_id = $1 AND s.status = 'active' AND NOT EXISTS (SELECT 1 FROM topic_subscriptions t WHERE t.subscription_id = s.id))`,
			s.ID).Scan(&subscribed); err != nil {
			return err
		}
		if !subscribed {
			out = plainResult("Nothing is subscribed for this conversation, so there's nothing to wait for.", true)
			return nil
		}
		now := a.now()
		id, err := a.Tasks.Mint(app.ID, waittask.Task{Tenant: cv.tenant, Session: s.ID, Issued: now})
		if err != nil {
			return err
		}
		prev, err := confirmPrevious(ctx, tx, app.ID, s.Agent, s.ID)
		if err != nil {
			return err
		}
		if prev != nil {
			// A round of the previous task in flight ends now (replaced),
			// rather than holding the stream until its hold runs out.
			if _, err := tx.Exec(ctx, `SELECT pg_notify($1, $2)`, streams.TakeoverChannel, *prev); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE sessions SET wait_task = $2, wait_tasks = wait_tasks || jsonb_build_object($3::text, $2::text),
			wait_task_seen_at = now(), last_active_at = now() WHERE id = $1`, s.ID, a.Tasks.Holder(id), app.ID); err != nil {
			return err
		}
		out = taskView(id, now, now, "working", taskWorking)
		out["resultType"] = "task"
		return nil
	})
	if errors.Is(err, errNoConversation) || errors.Is(err, errNoSession) || errors.Is(err, errOtherClient) {
		a.respond(w, map[string]any{"result": plainResult(noTaskSession, true)}, nil)
		return
	}
	a.respond(w, map[string]any{"result": out}, err)
}

// confirmPrevious confirms what app's previous wait task in the session
// handed out (the agent read its result, and called app's wait tool
// again), locking the session's row so two calls at once replace in turn,
// and returns the session's newest task's holder (of any app), which the
// new task replaces. It confirms only app's deliveries, held by its own
// previous task (sessions.wait_tasks) or by the session's newest (a task
// made before wait_tasks recorded it): another app's batch, which that
// app's agent may not have read, waits for that app's next call. A client
// that stopped following its task (taskLive: Claude Code closed, the
// result never read) confirms nothing: the batch goes back to pending, and
// this task hands it out again (no tool result hands it over).
func confirmPrevious(ctx context.Context, tx pgx.Tx, app, agent, session string) (*string, error) {
	var prev, prevOwn *string
	if err := tx.QueryRow(ctx, `SELECT wait_task, wait_tasks->>$2 FROM sessions WHERE id = $1 FOR UPDATE`, session, app).Scan(&prev, &prevOwn); err != nil {
		return nil, err
	}
	live, err := taskLive(ctx, tx, session)
	if err != nil {
		return nil, err
	}
	var holders []string
	if prevOwn != nil {
		holders = append(holders, *prevOwn)
	}
	if prev != nil && (prevOwn == nil || *prev != *prevOwn) {
		holders = append(holders, *prev)
	}
	for _, h := range holders {
		var err error
		switch {
		case live:
			err = deliveries.AckHolderFor(ctx, tx, agent, session, app, h)
		case prevOwn != nil && h == *prevOwn:
			// Only the app's own previous task's batch: another app's waits
			// for that app's next call.
			err = deliveries.Release(ctx, tx, h)
		}
		if err != nil {
			return nil, err
		}
	}
	return prev, nil
}

// taskCall is a tasks/get or tasks/cancel as the SDK passes it.
type taskCall struct {
	Task        string `json:"task"`
	Call        callIn `json:"call"`
	HoldSeconds *int   `json:"hold_seconds"`
}

// openTask is the caller's task: the ID opens for the app, and the caller
// is the conversation it was made for (its workspace, and client). The
// session's checks are the transaction's (taskSession). handle is the
// session key the conversation passes (for its results).
type openTask struct {
	id     string
	task   waittask.Task
	holder string
	agent  string // the agent the caller's client is, which must be the session's
	mode   string // modeTopic or modeAgent
	conv   bool   // a conversation's own workspace (no user)
	ext    string // the workspace's external ID
}

// openFor opens a task ID for the calling app and caller: errUnknownTask if
// it isn't theirs, waittask.ErrExpired if it's too old.
func (a *API) openFor(ctx context.Context, app apps.App, in taskCall) (openTask, error) {
	t, err := a.Tasks.Open(app.ID, in.Task, a.now())
	expired := errors.Is(err, waittask.ErrExpired)
	if err != nil && !expired {
		return openTask{}, errUnknownTask
	}
	o := openTask{id: in.Task, task: t, holder: a.Tasks.Holder(in.Task), mode: modeTopic}
	if err := a.callerOf(ctx, app, in, &o); err != nil {
		return o, err
	}
	// In a conversation's own workspace (no user) nothing but the ID says
	// who's asking, so it works for ConversationTaskMaxAge, not MaxAge.
	if o.conv && o.mode == modeTopic && a.now().Sub(t.Issued) > ConversationTaskMaxAge {
		expired = true
	}
	if expired {
		// Said only to the conversation the task was for: its session's
		// agent and app, as taskSession checks a live one.
		err := a.DB.InTenant(ctx, t.Tenant, func(tx pgx.Tx) error {
			s, err := sessions.Get(ctx, tx, t.Session)
			if err != nil {
				return err
			}
			if s.Agent != o.agent || !s.Joined(app.ID) {
				return errUnknownTask
			}
			return nil
		})
		if errors.Is(err, sessions.ErrNotFound) {
			return o, errUnknownTask
		}
		if err != nil {
			return o, err
		}
		return o, waittask.ErrExpired
	}
	return o, nil
}

// callerOf checks that the caller is the conversation o's task was made
// for (its workspace) and records its agent: errUnknownTask if not.
func (a *API) callerOf(ctx context.Context, app apps.App, in taskCall, o *openTask) error {
	t := o.task
	if in.Call.AgentToken != "" {
		cv, err := a.agentConversation(ctx, app, in.Call)
		if err != nil {
			return errUnknownTask
		}
		if cv.tenant != t.Tenant {
			return errUnknownTask
		}
		o.agent, o.mode = cv.act.agent, modeAgent
		return nil
	}
	user := strings.TrimSpace(in.Call.User)
	var owner *string
	err := a.DB.Pool.QueryRow(ctx, `SELECT app_id, external_id FROM tenants WHERE id = $1::uuid`, t.Tenant).Scan(&owner, &o.ext)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && (owner == nil || *owner != app.ID) {
		return errUnknownTask
	}
	if err != nil {
		return err
	}
	o.conv = strings.HasPrefix(o.ext, conversationPrefix)
	switch {
	case o.conv && user != "": // a user's call can't follow a userless conversation's task
		return errUnknownTask
	case !o.conv && (user == "" || user != o.ext): // another user's (or no user's)
		return errUnknownTask
	}
	_, o.agent, _, _, _ = harnessOf(in.Call)
	return nil
}

// taskSession is the task's session, in its workspace's transaction:
// errUnknownTask unless it's the caller's agent's and the app's;
// sessions.ErrNotFound if it has ended; errGrantGone without the agent's
// consent. It records that the client asked.
func (a *API) taskSession(ctx context.Context, tx pgx.Tx, app apps.App, o openTask) (sessions.Session, error) {
	s, err := sessions.Get(ctx, tx, o.task.Session)
	if err != nil {
		return s, err
	}
	if s.Agent != o.agent || !s.Joined(app.ID) {
		return s, errUnknownTask
	}
	if _, err := grants.Live(ctx, tx, app.ID, s.Agent); errors.Is(err, grants.ErrNotFound) {
		return s, errGrantGone
	} else if err != nil {
		return s, err
	}
	return s, nil
}

// taskHandle is the session key the task's conversation passes: its own
// workspace's token, or the session's handle.
func taskHandle(o openTask, s sessions.Session) string {
	if o.mode == modeAgent {
		return s.Handle
	}
	if o.conv {
		return sessionTokenPrefix + strings.TrimPrefix(o.ext, conversationPrefix)
	}
	return handleOf(s)
}

// taskRefusal answers a task request that failed before it got to the
// task's state: an unknown task as a JSON-RPC error, anything else as the
// task failing (expired) or an API error.
func (a *API) taskRefusal(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errUnknownTask):
		a.respond(w, map[string]any{"rpc_error": map[string]any{"code": rpcInvalidParams, "message": "Unknown task: it isn't one of this conversation's."}}, nil)
	default:
		a.respond(w, nil, err)
	}
}

// callsTaskGet is tasks/get: the task's batch if it handed one out;
// otherwise, unless it was replaced or the conversation has nothing left to
// wait for, a round of the conversation's wait, held up to hold_seconds.
func (a *API) callsTaskGet(w http.ResponseWriter, r *http.Request) {
	var in taskCall
	if !httpx.Decode(w, r, &in) {
		return
	}
	if a.Tasks == nil {
		a.taskRefusal(w, errUnknownTask)
		return
	}
	app := AppFrom(r.Context())
	ctx := r.Context()
	o, err := a.openFor(ctx, app, in)
	if errors.Is(err, waittask.ErrExpired) {
		a.respond(w, map[string]any{"result": taskView(in.Task, o.task.Issued, a.now(), "failed", "This wait is too old to follow any more. Call "+harnesses.WaitTool+" again.")}, nil)
		return
	}
	if err != nil {
		a.taskRefusal(w, err)
		return
	}
	hold := taskHold
	if in.HoldSeconds != nil {
		hold = min(max(time.Duration(*in.HoldSeconds)*time.Second, 0), taskMaxHold)
	}
	view := func(status, message string) {
		a.respond(w, map[string]any{"result": taskView(o.id, o.task.Issued, a.now(), status, message)}, nil)
	}
	name := appName(&conversation{app: app})
	var s sessions.Session
	var done bool // answered from the transaction (status, message)
	var status, message string
	err = a.DB.InTenant(ctx, o.task.Tenant, func(tx pgx.Tx) error {
		var err error
		if s, err = a.taskSession(ctx, tx, app, o); err != nil {
			return err
		}
		// Its batch, if it handed one out: the same result on every poll.
		frames, err := streams.TaskBatch(ctx, tx, s.Agent, s.ID, o.holder, taskBatchMax)
		if err != nil {
			return err
		}
		if len(frames) > 0 {
			done, status, message = true, "completed", harnesses.TaskResult(name, taskHandle(o, s), taskLines(frames))
			_, err := tx.Exec(ctx, `UPDATE sessions SET wait_task_seen_at = now() WHERE id = $1 AND wait_task = $2`, s.ID, o.holder)
			return err
		}
		var cur *string
		if err := tx.QueryRow(ctx, `UPDATE sessions SET wait_task_seen_at = now() WHERE id = $1 AND wait_task = $2 RETURNING wait_task`, s.ID, o.holder).Scan(&cur); errors.Is(err, pgx.ErrNoRows) {
			done, status, message = true, "cancelled", harnesses.TaskReplaced
			if err := tx.QueryRow(ctx, `SELECT wait_task FROM sessions WHERE id = $1`, s.ID).Scan(&cur); err == nil && cur == nil {
				message = harnesses.TaskStopped
			}
			return nil
		} else if err != nil {
			return err
		}
		if hooks, err := hooksDeliver(ctx, tx, s); err != nil || hooks {
			done, status, message = true, "completed", harnesses.TaskHooks
			return err
		}
		// Nothing left to wait for (its last subscription ended): the
		// stream's own ending says why.
		note, over, err := a.taskOver(ctx, tx, s)
		if err != nil || !over {
			return err
		}
		done, status, message = true, "completed", harnesses.TaskEnded(note, taskHandle(o, s))
		return taskDone(ctx, tx, s.ID, o.holder)
	})
	switch {
	case errors.Is(err, sessions.ErrNotFound):
		view("failed", harnesses.TaskSessionGone)
		return
	case errors.Is(err, errGrantGone):
		view("failed", harnesses.TaskConsentGone(name))
		return
	case err != nil:
		a.taskRefusal(w, err)
		return
	case done:
		view(status, message)
		return
	}
	valid := func(ctx context.Context, tx pgx.Tx) error {
		var cur *string
		if err := tx.QueryRow(ctx, `SELECT wait_task FROM sessions WHERE id = $1`, s.ID).Scan(&cur); errors.Is(err, pgx.ErrNoRows) {
			return errTaskReplaced
		} else if err != nil {
			return err
		}
		if cur == nil || *cur != o.holder {
			return errTaskReplaced
		}
		if _, err := grants.Live(ctx, tx, app.ID, s.Agent); errors.Is(err, grants.ErrNotFound) {
			return streams.ErrRevoked
		} else if err != nil {
			return err
		}
		// Its last subscription ended while this round waits (an
		// unsubscribe stops the session's listening, which wakes the
		// round): it ends now, rather than holding the request for the
		// rest of the round.
		note, over, err := a.taskOver(ctx, tx, s)
		if err != nil {
			return err
		}
		if over {
			return taskOverErr{note: note}
		}
		return nil
	}
	batch, err := a.Streams.TaskWait(ctx, streams.PollRequest{Tenant: o.task.Tenant, Agent: s.Agent, Target: s.ID, Valid: valid, Wait: hold, Limit: taskBatchMax, App: app.ID}, o.holder)
	var ended taskOverErr
	switch {
	case ctx.Err() != nil:
		return
	case errors.As(err, &ended):
		// The round's transaction rolled back: recorded on its own.
		if err := a.DB.InTenant(ctx, o.task.Tenant, func(tx pgx.Tx) error { return taskDone(ctx, tx, s.ID, o.holder) }); err != nil {
			a.taskRefusal(w, err)
			return
		}
		view("completed", harnesses.TaskEnded(ended.note, taskHandle(o, s)))
	case errors.Is(err, errTaskReplaced):
		view("cancelled", harnesses.TaskReplaced)
	case errors.Is(err, streams.ErrHooksConnected):
		view("completed", harnesses.TaskHooks)
	case errors.Is(err, streams.ErrRevoked):
		view("failed", harnesses.TaskConsentGone(name))
	case errors.Is(err, streams.ErrReplaced) && a.taskReplaced(ctx, o, s):
		view("cancelled", harnesses.TaskReplaced)
	case errors.Is(err, streams.ErrReplaced), errors.Is(err, streams.ErrDraining), errors.Is(err, streams.ErrStreamConnected):
		// Something else took the stream for a moment, or this instance is
		// stopping: still working; the client asks again (anywhere).
		view("working", taskWorking)
	case err != nil:
		a.writeWaitError(w, err)
	case len(batch.Deliveries) > 0:
		view("completed", harnesses.TaskResult(name, taskHandle(o, s), taskLines(batch.Deliveries)))
	default:
		view("working", taskWorking)
	}
}

// taskOverErr: a round of a wait task found its conversation with nothing
// left to wait for (its last subscription ended); note says why.
type taskOverErr struct{ note string }

func (e taskOverErr) Error() string {
	return "the wait task's conversation has nothing left to wait for"
}

// taskOver reports whether session s has nothing left to wait for (its
// last subscription ended, and nothing waits to be delivered), and the
// stream's own ending note saying why.
func (a *API) taskOver(ctx context.Context, tx pgx.Tx, s sessions.Session) (string, bool, error) {
	end, over, err := a.StreamEnding(ctx, tx, streams.Link{Agent: s.Agent, Target: s.ID}, true)
	return end.Note, over, err
}

// taskDone records that the session's task (holder) has ended for good:
// the session no longer counts as following a wait (taskLive), so
// subscribing again says to call the wait tool again rather than that
// its wait still runs.
func taskDone(ctx context.Context, tx pgx.Tx, session, holder string) error {
	_, err := tx.Exec(ctx, `UPDATE sessions SET wait_task_seen_at = NULL WHERE id = $1 AND wait_task = $2`, session, holder)
	return err
}

// taskReplaced reports whether o's task is no longer its session's newest
// (a newer wait task took over, or it was stopped).
func (a *API) taskReplaced(ctx context.Context, o openTask, s sessions.Session) bool {
	var replaced bool
	err := a.DB.InTenant(ctx, o.task.Tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT wait_task IS DISTINCT FROM $2 FROM sessions WHERE id = $1`, s.ID, o.holder).Scan(&replaced)
	})
	return err == nil && replaced
}

// taskLines are a batch's messages as a wait task's result lists them: the
// lines a one-shot wait prints (streams.MonitorLine).
func taskLines(frames []streams.Frame) []string {
	out := make([]string, len(frames))
	for i, f := range frames {
		out[i] = string(streams.MonitorLine(f))
	}
	return out
}

// callsTaskCancel is tasks/cancel: the conversation stops waiting (if the
// task is its newest), what the task handed out and nobody confirmed is
// handed back, and the session stops counting as connected.
func (a *API) callsTaskCancel(w http.ResponseWriter, r *http.Request) {
	var in taskCall
	if !httpx.Decode(w, r, &in) {
		return
	}
	if a.Tasks == nil {
		a.taskRefusal(w, errUnknownTask)
		return
	}
	app := AppFrom(r.Context())
	ctx := r.Context()
	o, err := a.openFor(ctx, app, in)
	if errors.Is(err, waittask.ErrExpired) {
		a.respond(w, map[string]any{"result": taskView(in.Task, o.task.Issued, a.now(), "cancelled", harnesses.TaskStopped)}, nil)
		return
	}
	if err != nil {
		a.taskRefusal(w, err)
		return
	}
	err = a.DB.InTenant(ctx, o.task.Tenant, func(tx pgx.Tx) error {
		s, err := a.taskSession(ctx, tx, app, o)
		if errors.Is(err, errGrantGone) || errors.Is(err, sessions.ErrNotFound) {
			return nil // nothing to stop
		}
		if err != nil {
			return err
		}
		// Locked first, as the wait tool's call locks it (confirmPrevious),
		// so a cancel and a new task of the app take turns. Only a task that
		// is still its app's newest (wait_tasks) or the session's
		// (wait_task) stops: one a newer task of the same app replaced was
		// confirmed or handed back by that one, and its holder no longer
		// stands for anything. A task that stops is no longer its app's to
		// confirm on its next call: what it handed out is handed back.
		var newest, own bool
		if err := tx.QueryRow(ctx, `SELECT coalesce(wait_task = $2, false), coalesce(wait_tasks->>$3::text = $2, false) FROM sessions WHERE id = $1 FOR UPDATE`,
			s.ID, o.holder, app.ID).Scan(&newest, &own); err != nil || !newest && !own {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE sessions SET wait_task = CASE WHEN wait_task = $2 THEN NULL ELSE wait_task END,
			wait_tasks = CASE WHEN wait_tasks->>$3::text = $2 THEN wait_tasks - $3::text ELSE wait_tasks END WHERE id = $1`, s.ID, o.holder, app.ID); err != nil {
			return err
		}
		if err := deliveries.Release(ctx, tx, o.holder); err != nil {
			return err
		}
		if !newest {
			return nil // another app's task holds the stream now
		}
		if _, err := tx.Exec(ctx, `UPDATE stream_leases SET renewed_at = NULL WHERE agent_id = $1 AND target = $2 AND holder = $3`, s.Agent, s.ID, o.holder); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `SELECT pg_notify($1, $2)`, streams.TakeoverChannel, o.holder)
		return err
	})
	if err != nil {
		a.taskRefusal(w, err)
		return
	}
	a.respond(w, map[string]any{"result": taskView(o.id, o.task.Issued, a.now(), "cancelled", harnesses.TaskStopped)}, nil)
}

// taskBatchMax is how many messages one wait task's result hands over.
const taskBatchMax = 20

// noTaskSession is the wait tool's result for a call whose conversation
// Wakeshift doesn't know: nothing to wait for, and no call to make (the
// tool that subscribes may have side effects).
const noTaskSession = "Wakeshift doesn't know this conversation (or the " + harnesses.SessionArg + " passed isn't this conversation's), so there's nothing to wait for. " +
	"Only a result that subscribed this conversation gives a " + harnesses.SessionArg + " for " + harnesses.WaitTool + "."
