package coretest_test

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/core"
	"github.com/wakeshift/server/internal/coretest"
	"github.com/wakeshift/server/internal/harnesses"
	"github.com/wakeshift/server/internal/jws"
	"github.com/wakeshift/server/pkg/wakeshift"
)

// MCP wait tasks (internal/appapi/tasks.go): Claude Code's no-install wake
// path, for a client that takes MCP tasks. These call the app API's routes
// as an SDK passes the wait tool's call, tasks/get and tasks/cancel.

// taskAnswer is a /calls/task* answer: the MCP result, or a JSON-RPC error.
type taskAnswer struct {
	Result   map[string]any `json:"result"`
	RPCError *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"rpc_error"`
}

func (a taskAnswer) status() string { s, _ := a.Result["status"].(string); return s }
func (a taskAnswer) id() string     { s, _ := a.Result["taskId"].(string); return s }

// text is a completed task's result text, or a plain result's.
func (a taskAnswer) text() string {
	content, _ := a.Result["content"].([]any)
	if r, ok := a.Result["result"].(map[string]any); ok {
		content, _ = r["content"].([]any)
	}
	if len(content) == 0 {
		return ""
	}
	b, _ := content[0].(map[string]any)
	s, _ := b["text"].(string)
	return s
}

func (a taskAnswer) message() string { s, _ := a.Result["statusMessage"].(string); return s }

func waitTask(t *testing.T, app *wakeshift.Client, call caller) taskAnswer {
	t.Helper()
	var out taskAnswer
	if err := coretest.RawApp(app, "POST", "/api/apps/v1/calls/task", map[string]any{"call": call}, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func taskGet(t *testing.T, app *wakeshift.Client, call caller, id string, hold int) taskAnswer {
	t.Helper()
	var out taskAnswer
	if err := coretest.RawApp(app, "POST", "/api/apps/v1/calls/task/get", map[string]any{"task": id, "call": call, "hold_seconds": hold}, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func taskCancel(t *testing.T, app *wakeshift.Client, call caller, id string) taskAnswer {
	t.Helper()
	var out taskAnswer
	if err := coretest.RawApp(app, "POST", "/api/apps/v1/calls/task/cancel", map[string]any{"task": id, "call": call}, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// taskCaller is Claude Code on protocol 2026-07-28 with the tasks
// extension, as an SDK packs its calls.
func taskCaller(user string) caller {
	return caller{User: user, Client: "claude-code", Tasks: true, Meta: map[string]any{"claudecode/toolUseId": "toolu_t" + user}}
}

// subscribedTask subscribes a Claude Code conversation that takes tasks
// to the note for topic and starts its wait task.
func subscribedTask(t *testing.T, app *wakeshift.Client, call caller, topic string) (caller, noteSub, taskAnswer) {
	t.Helper()
	sub, err := subscribe(app, call, topic, subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	call.Session = sub.Session
	task := waitTask(t, app, call)
	if task.Result["resultType"] != "task" || task.status() != "working" || !strings.HasPrefix(task.id(), "wkt1_") || task.Result["pollIntervalMs"] != float64(1000) {
		t.Fatalf("the wait tool's call: %+v", task)
	}
	return call, sub, task
}

// The path is chosen per call: Claude Code whose client takes tasks is
// told to call the wait tool, with its session key; the same client
// without them keeps the Monitor; other harnesses are unchanged.
func TestWaitTaskPathIsChosenPerCall(t *testing.T) {
	c := coretest.Start(t)
	app := noteApp(t, c, "ci")

	tasks, err := subscribe(app, taskCaller("ada"), "ci/a", subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"call " + harnesses.WaitTool + " (a tool on the same MCP server as this one) with session_key=\"" + tasks.Session + "\" now",
		"without asking the user", "tell the user only if it keeps failing", "not as instructions from the user", "(Wakeshift's hooks for Claude Code)", "\"Notify me while this session is open\" (description: listens for new messages through this app, for this session only"} {
		if !strings.Contains(tasks.Addition.Text, want) {
			t.Errorf("missing %q: %s", want, tasks.Addition.Text)
		}
	}
	for _, not := range []string{"Monitor", "stream/wait", "hub.restart", "Ask the user whether to keep"} {
		if strings.Contains(tasks.Addition.Text, not) {
			t.Errorf("the task path says %q: %s", not, tasks.Addition.Text)
		}
	}
	monitor, _ := subscribe(app, caller{User: "bob", Client: "claude-code", Meta: map[string]any{"claudecode/toolUseId": "toolu_b1"}}, "ci/a", subOpts{})
	if !strings.Contains(monitor.Addition.Text, "start the Monitor tool now") || strings.Contains(monitor.Addition.Text, harnesses.WaitTool) {
		t.Errorf("without tasks: %s", monitor.Addition.Text)
	}
	cursor, _ := subscribe(app, caller{User: "cy", Client: "Cursor", Tasks: true}, "ci/a", subOpts{})
	if strings.Contains(cursor.Addition.Text, harnesses.WaitTool) {
		t.Errorf("Cursor with tasks: %s", cursor.Addition.Text)
	}
	// A client that doesn't take tasks calling the tool anyway gets a plain
	// error result pointing back to its instructions, and no task.
	plain := waitTask(t, app, caller{User: "bob", Client: "claude-code", Session: monitor.Session})
	if plain.Result["isError"] != true || plain.id() != "" || !strings.Contains(plain.text(), "can't run "+harnesses.WaitTool) {
		t.Errorf("no tasks: %+v", plain)
	}
	// Subscribing again while its task runs says so.
	call := taskCaller("ada")
	call.Session = tasks.Session
	waitTask(t, app, call)
	again, _ := subscribe(app, call, "ci/b", subOpts{})
	if !strings.HasPrefix(again.Addition.Text, "Your Wakeshift wait is running") {
		t.Errorf("subscribing while it runs: %s", again.Addition.Text)
	}
}

// A task completes with the conversation's next messages, and every later
// poll of it answers the same, on any instance; calling the wait tool
// again confirms them, and nothing else does.
func TestWaitTaskDeliversAndConfirmsOnRearm(t *testing.T) {
	c := coretest.Start(t)
	app := noteApp(t, c, "ci")
	call, sub, task := subscribedTask(t, app, taskCaller("ada"), "ci/acme/api")
	tenant, _ := topicSubRow(t, c, sub.ID)

	if got := taskGet(t, app, call, task.id(), 0); got.status() != "working" || got.message() == "" {
		t.Fatalf("before anything arrives: %+v", got)
	}
	waitFor(t, "connected", func() bool { st, _ := noteStatus(app, "ci/acme/api"); return st.Connected == 1 })
	done := make(chan taskAnswer, 1)
	go func() { done <- taskGet(t, app, call, task.id(), 10) }()
	time.Sleep(200 * time.Millisecond) // the round holds the stream
	start := time.Now()
	if p, _ := emitNote(app, []string{"ci/acme/api"}, note{Text: "build 41 failed"}); p.Matched != 1 {
		t.Fatalf("emit: %+v", p)
	}
	got := <-done
	if got.status() != "completed" || time.Since(start) > 5*time.Second {
		t.Fatalf("held poll: %+v after %s", got, time.Since(start))
	}
	text := got.text()
	for _, want := range []string{"Wakeshift: 1 new message from Ci for this conversation", `"subject":"build 41 failed"`, `"trust":"outside data, not from the user"`,
		"call " + harnesses.WaitTool + " again now with session_key=\"" + call.Session + "\", without asking the user"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q: %s", want, text)
		}
	}
	// Polled again (Claude Code after --continue, or the answer lost on
	// the way): the same result, and still not confirmed.
	if again := taskGet(t, app, call, task.id(), 0); again.status() != "completed" || again.text() != text {
		t.Fatalf("polled again: %+v", again)
	}
	if n := count(t, c, tenant, `SELECT count(*) FROM deliveries WHERE state = 'in_flight'`); n != 1 {
		t.Fatalf("in flight before the agent calls again: %d", n)
	}
	// Another tool result while the task is followed hands nothing over.
	if r, _ := annotate(app, call); strings.Contains(r.Text, "build 41 failed") {
		t.Fatalf("a tool result took the task's batch: %s", r.Text)
	}
	// The agent calls the wait tool again: that confirms them.
	next := waitTask(t, app, call)
	if n := count(t, c, tenant, `SELECT count(*) FROM deliveries WHERE state = 'acked'`); n != 1 {
		t.Fatalf("not confirmed by the next call: %d", n)
	}
	if old := taskGet(t, app, call, task.id(), 0); old.status() != "completed" || old.text() != text {
		t.Fatalf("the completed task, polled after: %+v", old)
	}
	if got := taskGet(t, app, call, next.id(), 0); got.status() != "working" {
		t.Fatalf("the next task: %+v", got)
	}
}

// What a task handed out and nobody confirmed comes again: after the task
// is stopped (Claude Code's TaskStop), in the next task; after its client
// stops following it, in the next task too (which doesn't confirm it),
// never in a tool result.
func TestWaitTaskUnconfirmedComesAgain(t *testing.T) {
	c := coretest.Start(t)
	app := noteApp(t, c, "ci")
	call, sub, task := subscribedTask(t, app, taskCaller("ada"), "ci/x")
	tenant, _ := topicSubRow(t, c, sub.ID)
	emitNote(app, []string{"ci/x"}, note{Text: "first"})
	waitFor(t, "completed", func() bool { return taskGet(t, app, call, task.id(), 2).status() == "completed" })

	if got := taskCancel(t, app, call, task.id()); got.status() != "cancelled" {
		t.Fatalf("cancel: %+v", got)
	}
	next := waitTask(t, app, call)
	if n := count(t, c, tenant, `SELECT count(*) FROM deliveries WHERE state = 'acked'`); n != 0 {
		t.Fatalf("a stopped task's batch was confirmed: %d", n)
	}
	var got taskAnswer
	waitFor(t, "the next task", func() bool { got = taskGet(t, app, call, next.id(), 2); return got.status() == "completed" })
	if !strings.Contains(got.text(), `"subject":"first"`) || !strings.Contains(got.text(), `"redelivered":true`) {
		t.Fatalf("not again: %s", got.text())
	}

	// Its client stops following it (Claude Code closed, its result never
	// read): the next tool result says nothing (the next wait gets it).
	if err := c.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE sessions SET wait_task_seen_at = now() - interval '10 minutes'`)
		if err == nil {
			_, err = tx.Exec(ctx, `UPDATE stream_leases SET renewed_at = now() - interval '10 minutes'`)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	r, err := annotate(app, call)
	if err != nil {
		t.Fatal(err)
	}
	if r.Text != "" {
		t.Fatalf("the next result: %s", r.Text)
	}
	again := waitTask(t, app, call)
	waitFor(t, "the wait after", func() bool { got = taskGet(t, app, call, again.id(), 2); return got.status() == "completed" })
	if !strings.Contains(got.text(), `"subject":"first"`) {
		t.Fatalf("the wait after: %s", got.text())
	}
}

// Calling the wait tool again replaces the running task: the old one's
// polls answer cancelled at once and can't take the stream back.
func TestWaitTaskReplaced(t *testing.T) {
	c := coretest.Start(t)
	app := noteApp(t, c, "ci")
	call, _, first := subscribedTask(t, app, taskCaller("ada"), "ci/x")
	held := make(chan taskAnswer, 1)
	go func() { held <- taskGet(t, app, call, first.id(), 10) }()
	time.Sleep(300 * time.Millisecond)
	second := waitTask(t, app, call)
	select {
	case got := <-held:
		if got.status() != "cancelled" || got.message() != harnesses.TaskReplaced {
			t.Fatalf("the replaced task's held poll: %+v", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the replaced task's held poll went on")
	}
	done := make(chan taskAnswer, 1)
	go func() { done <- taskGet(t, app, call, second.id(), 10) }()
	time.Sleep(300 * time.Millisecond)
	if got := taskGet(t, app, call, first.id(), 5); got.status() != "cancelled" {
		t.Fatalf("the old task polled again: %+v", got)
	}
	emitNote(app, []string{"ci/x"}, note{Text: "to the newer one"})
	if got := <-done; got.status() != "completed" || !strings.Contains(got.text(), "to the newer one") {
		t.Fatalf("the newer task: %+v", got)
	}
}

// Task IDs are unforgeable and scoped to the conversation: an ID that's
// made up, altered, another app's, another user's, a userless
// conversation's from a user's call, another client's, or one whose
// session or consent is gone never polls, stops or confirms anything.
func TestWaitTaskIDsAreScopedToTheirConversation(t *testing.T) {
	c := coretest.Start(t)
	app := noteApp(t, c, "ci")
	other := noteApp(t, c, "other")
	call, sub, task := subscribedTask(t, app, taskCaller("ada"), "ci/x")
	tenant, _ := topicSubRow(t, c, sub.ID)
	id := task.id()

	unknown := func(what string, a taskAnswer) {
		t.Helper()
		if a.RPCError == nil || a.RPCError.Code != -32602 || a.Result != nil {
			t.Errorf("%s: %+v", what, a)
		}
	}
	raw, _ := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(id, "wkt1_"))
	flipped := append([]byte(nil), raw...)
	flipped[len(flipped)/2] ^= 0x10
	for what, bad := range map[string]string{
		"made up":  "wkt1_" + base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{7}, len(raw))),
		"altered":  "wkt1_" + base64.RawURLEncoding.EncodeToString(flipped),
		"empty":    "",
		"garbage":  "not-a-task",
		"too long": id + strings.Repeat("A", 100),
	} {
		unknown(what+" (get)", taskGet(t, app, call, bad, 0))
		unknown(what+" (cancel)", taskCancel(t, app, call, bad))
	}
	// Another app, even with a user of the same ID.
	unknown("another app", taskGet(t, other, call, id, 0))
	unknown("another app (cancel)", taskCancel(t, other, call, id))
	// Another user of the same app, or no user.
	bob := call
	bob.User = "bob"
	unknown("another user", taskGet(t, app, bob, id, 0))
	unknown("another user (cancel)", taskCancel(t, app, bob, id))
	anon := call
	anon.User, anon.Session = "", ""
	unknown("no user", taskGet(t, app, anon, id, 0))
	// The same user's other agent program.
	cursor := call
	cursor.Client = "Cursor"
	unknown("another client", taskGet(t, app, cursor, id, 0))

	// A userless conversation's task: only a call without a user follows it.
	lone, _, loneTask := subscribedTask(t, app, taskCaller(""), "ci/y")
	if got := taskGet(t, app, lone, loneTask.id(), 0); got.status() != "working" {
		t.Fatalf("the userless conversation's own poll: %+v", got)
	}
	unknown("a user's call, a userless conversation's task", taskGet(t, app, call, loneTask.id(), 0))

	// One conversation's call can't confirm or replace another's task: the
	// same user's second conversation starts its own.
	emitNote(app, []string{"ci/x"}, note{Text: "for the first conversation"})
	waitFor(t, "completed", func() bool { return taskGet(t, app, call, id, 2).status() == "completed" })
	second, _, _ := subscribedTask(t, app, taskCaller("ada"), "ci/z")
	if second.Session == call.Session {
		t.Fatal("one session")
	}
	waitTask(t, app, second)
	if n := count(t, c, tenant, `SELECT count(*) FROM deliveries WHERE state = 'in_flight'`); n != 1 {
		t.Fatalf("another conversation's call confirmed this one's batch: %d in flight", n)
	}
	// A session key that isn't the conversation's gets no task.
	wrong := call
	wrong.Session = "wsc_" + strings.Repeat("a", 26)
	if got := waitTask(t, app, wrong); got.id() != "" || got.Result["isError"] != true {
		t.Fatalf("a made-up session key: %+v", got)
	}

	// Consent revoked: the task fails, and says so.
	if err := c.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE grants SET revoked_at = now() WHERE app_id = 'ci'`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	next := taskGet(t, app, call, id, 0)
	if next.status() != "failed" || !strings.Contains(next.message(), "consent") {
		t.Fatalf("after revoking: %+v", next)
	}
}

// The conversation's hooks connect, or its last subscription ends: the
// task ends, saying so, and the wait tool gives no new task.
func TestWaitTaskEnds(t *testing.T) {
	c := coretest.Start(t)
	app := noteApp(t, c, "ci")
	call, sub, task := subscribedTask(t, app, taskCaller("ada"), "ci/x")
	if _, err := unsubscribe(app, call, "ci/x"); err != nil {
		t.Fatal(err)
	}
	var got taskAnswer
	waitFor(t, "ended", func() bool {
		got = taskGet(t, app, call, task.id(), 1)
		return got.status() == "completed" && strings.Contains(got.text(), "ended for good")
	})
	if again := waitTask(t, app, call); again.id() != "" || !strings.Contains(again.text(), "Nothing is subscribed") {
		t.Fatalf("after unsubscribing: %+v", again)
	}

	call2, sub2, task2 := subscribedTask(t, app, taskCaller("bob"), "ci/y")
	tenant, _ := topicSubRow(t, c, sub2.ID)
	_ = sub
	// The hooks redeemed: not enough (#1272), the task goes on until their
	// waiter polls.
	if err := c.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE sessions SET runtime = 'stream', connector_seen_at = now()`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if got := taskGet(t, app, call2, task2.id(), 0); got.status() != "working" {
		t.Fatalf("a redeem alone: %+v", got)
	}
	if err := c.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error { return hooksPolledSQL(tx, "true", "0 seconds") }); err != nil {
		t.Fatal(err)
	}
	if got := taskGet(t, app, call2, task2.id(), 0); got.status() != "completed" || got.text() != harnesses.TaskHooks {
		t.Fatalf("hooks connected: %+v", got)
	}
	if again := waitTask(t, app, call2); again.id() != "" || again.text() != harnesses.TaskHooks {
		t.Fatalf("the wait tool once the hooks connect: %+v", again)
	}
}

// A task is only its ID: another core instance on the same database and
// key (a deploy, a restart) answers for it.
func TestWaitTaskSurvivesARestart(t *testing.T) {
	c := coretest.Start(t)
	app := noteApp(t, c, "ci")
	call, _, task := subscribedTask(t, app, taskCaller("ada"), "ci/x")
	key := func(purpose string) []byte {
		sum := sha256.Sum256([]byte("coretest/" + purpose))
		return sum[:]
	}
	again, err := core.New(ctx, core.Options{DB: c.DB, PublicURL: c.URL, Key: key, Web: fstest.MapFS{}, Poll: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	other := httptest.NewServer(again.Handler)
	defer other.Close()
	emitNote(app, []string{"ci/x"}, note{Text: "after the restart"})
	var got taskAnswer
	waitFor(t, "the other instance", func() bool {
		got = rawTaskGet(t, app, other.URL, c.URL, call, task.id())
		return got.status() == "completed"
	})
	if !strings.Contains(got.text(), "after the restart") {
		t.Fatalf("from the other instance: %+v", got)
	}
}

// rawTaskGet is tasks/get sent to base, with app's assertion for the core
// at issuer.
func rawTaskGet(t *testing.T, app *wakeshift.Client, base, issuer string, call caller, id string) taskAnswer {
	t.Helper()
	jti := make([]byte, 16)
	rand.Read(jti)
	now := time.Now()
	tok, err := jws.Sign(coretest.CredsOf(app).Key, "", "app+jwt", map[string]any{"iss": coretest.CredsOf(app).ID, "sub": coretest.CredsOf(app).ID, "aud": issuer + "/api/apps",
		"iat": now.Unix(), "exp": now.Add(time.Minute).Unix(), "jti": base64.RawURLEncoding.EncodeToString(jti)})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(map[string]any{"task": id, "call": call, "hold_seconds": 1})
	req, _ := http.NewRequestWithContext(ctx, "POST", base+"/api/apps/v1/calls/task/get", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("%d: %s", resp.StatusCode, body)
	}
	var out taskAnswer
	json.Unmarshal(body, &out)
	return out
}

// The core's answers have the shape the SDKs' shared wire file shows
// (sdk/typescript/test/mcp-tasks-wire.json): the same keys for a started,
// a working, a completed and a canceled task, and an unknown task's error.
func TestWaitTaskAnswersMatchTheSDKsWireFile(t *testing.T) {
	b, err := os.ReadFile("../../sdk/typescript/test/mcp-tasks-wire.json")
	if err != nil {
		t.Skip("the TypeScript SDK isn't in this tree")
	}
	var g struct {
		Cases []struct {
			Name         string     `json:"name"`
			CoreResponse taskAnswer `json:"core_response"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(b, &g); err != nil {
		t.Fatal(err)
	}
	keys := func(m map[string]any) string {
		var ks []string
		for k := range m {
			ks = append(ks, k)
		}
		slices.Sort(ks)
		return strings.Join(ks, ",")
	}
	c := coretest.Start(t)
	app := noteApp(t, c, "ci")
	call, _, started := subscribedTask(t, app, taskCaller("ada"), "ci/x")
	working := taskGet(t, app, call, started.id(), 0)
	emitNote(app, []string{"ci/x"}, note{Text: "hi"})
	var completed taskAnswer
	waitFor(t, "completed", func() bool {
		completed = taskGet(t, app, call, started.id(), 2)
		return completed.status() == "completed"
	})
	canceled := taskCancel(t, app, call, started.id())
	unknown := taskGet(t, app, call, "wkt1_nope", 0)
	for i, got := range []taskAnswer{started, working, completed, unknown, canceled} {
		want := g.Cases[i].CoreResponse
		if keys(got.Result) != keys(want.Result) || (got.RPCError == nil) != (want.RPCError == nil) {
			t.Errorf("%s: %v / %+v, the wire file has %v / %+v", g.Cases[i].Name, keys(got.Result), got.RPCError, keys(want.Result), want.RPCError)
		}
		if want.RPCError != nil && (got.RPCError.Code != want.RPCError.Code || got.RPCError.Message != want.RPCError.Message) {
			t.Errorf("%s: %+v", g.Cases[i].Name, got.RPCError)
		}
	}
}

// An app acting for its agents (Agent Chat) gets the task path too: the
// agent's token names the conversation's workspace, and another agent's
// token can't follow its task.
func TestWaitTaskForAnAppActingForItsAgents(t *testing.T) {
	c := coretest.Start(t)
	app := chatApp(t, c)
	ada := actAs(t, app, "ada", "claude-code")
	bob := actAs(t, app, "bob", "claude-code")
	call := map[string]any{"agent_token": agentToken(t, c, app, ada), "client": "claude-code", "tool": "join", "tasks": true}
	var sub deferredOut
	if code, e := appAPI(t, c, app, "/api/apps/v1/event-subscriptions", map[string]any{"call": call, "name": "chat.line",
		"arguments": map[string]any{"channel": "general"}, "defer_addition": true}, &sub); code != 200 {
		t.Fatalf("subscribe: %d %s", code, e)
	}
	call["session"] = sub.Session
	var add struct{ Text string }
	if code, e := appAPI(t, c, app, "/api/apps/v1/calls/addition", map[string]any{"call": call, "pending": sub.Pending}, &add); code != 200 {
		t.Fatalf("addition: %d %s", code, e)
	}
	if !strings.Contains(add.Text, harnesses.WaitTool+" (a tool on the same MCP server as this one) with session_key=\""+sub.Session+"\"") {
		t.Fatalf("the block: %s", add.Text)
	}
	var task, got, other taskAnswer
	if code, e := appAPI(t, c, app, "/api/apps/v1/calls/task", map[string]any{"call": call}, &task); code != 200 || task.status() != "working" {
		t.Fatalf("task: %d %s %+v", code, e, task)
	}
	bobCall := map[string]any{"agent_token": agentToken(t, c, app, bob), "client": "claude-code", "tasks": true}
	if code, _ := appAPI(t, c, app, "/api/apps/v1/calls/task/get", map[string]any{"call": bobCall, "task": task.id(), "hold_seconds": 0}, &other); code != 200 || other.RPCError == nil {
		t.Fatalf("another agent's token: %d %+v", code, other)
	}
	postLine(t, c, app, ada, "hello from #general")
	waitFor(t, "completed", func() bool {
		appAPI(t, c, app, "/api/apps/v1/calls/task/get", map[string]any{"call": call, "task": task.id(), "hold_seconds": 1}, &got)
		return got.status() == "completed"
	})
	if !strings.Contains(got.text(), "hello from #general") || !strings.Contains(got.text(), "session_key=\""+sub.Session+"\"") {
		t.Fatalf("result: %s", got.text())
	}
}
