package coretest_test

import (
	"bufio"
	"encoding/json"
	"io"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/wakeshift/server/internal/beacon"
	"github.com/wakeshift/server/internal/coretest"
	"github.com/wakeshift/server/pkg/wakeshift"
)

// Hermes Agent is recognized by the MCP Python SDK's default client name,
// "mcp", which other Python clients send too: its result says who the
// install note is for, carries an unbound beacon in _meta, and the agent
// keeps the client's own name.
func TestHermesResult(t *testing.T) {
	c := coretest.Start(t)
	app := noteApp(t, c, "ci")
	sub, err := subscribe(app, caller{User: "hu", Client: "mcp", UserAgent: "python-httpx2/2.12.0"}, "ci/acme/web", subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if got := sub.Addition.Agent; got.Harness != "hermes" || got.Via != "client" {
		t.Fatalf("agent %+v", got)
	}
	text := sub.Addition.Text
	for _, want := range []string{"If you're Hermes Agent (Nous Research):", "Ask the user", "hermes plugins install wakeshift/hermes-plugin --enable", "If not: Wakeshift can't wake this agent,"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q: %s", want, text)
		}
	}
	tok, _ := sub.Addition.Meta[beacon.MetaKey].(string)
	claims, err := beacon.VerifyBound(tok, beacon.Binding{Conversation: "hermes-session", Unbound: true}, []string{c.URL}, coreKeys(t, c), time.Now())
	if err != nil || claims.Thread != "" || claims.Call != "" {
		t.Fatalf("beacon: %+v %v", claims, err)
	}
	if _, err := beacon.VerifyBound(tok, beacon.Binding{Conversation: "hermes-session"}, []string{c.URL}, coreKeys(t, c), time.Now()); err == nil {
		t.Fatal("an unbound beacon accepted by a hook that isn't unbound")
	}
}

// Hermes Agent 0.22.5 names itself, "hermes-agent": with no claim from the
// model, its result carries the unbound beacon its plugin connects from,
// and the note doesn't hedge. Where the app sees no client name, the
// plugin's own claim (harness "hermes", plugins/hermes fill_args) does
// the same. Either way the agent keeps the client's own name.
func TestHermesRecognizedWithoutTheModelsClaim(t *testing.T) {
	c := coretest.Start(t)
	app := noteApp(t, c, "ci")
	for _, call := range []caller{
		{User: "h1", Client: "hermes-agent", UserAgent: "python-httpx2/2.12.0"},
		{User: "h2", Harness: "hermes", UserAgent: "python-httpx2/2.12.0"},
	} {
		sub, err := subscribe(app, call, "ci/acme/web", subOpts{})
		if err != nil {
			t.Fatal(err)
		}
		want := "client"
		if call.Client == "" {
			want = "claim"
		}
		if got := sub.Addition.Agent; got.Harness != "hermes" || got.Via != want {
			t.Fatalf("%+v: agent %+v", call, got)
		}
		text := sub.Addition.Text
		if !strings.Contains(text, "hermes plugins install wakeshift/hermes-plugin --enable") || strings.Contains(text, "If not:") || strings.Contains(text, "If you're") {
			t.Fatalf("%+v: %s", call, text)
		}
		tok, _ := sub.Addition.Meta[beacon.MetaKey].(string)
		claims, err := beacon.VerifyBound(tok, beacon.Binding{Conversation: "hermes-session", Unbound: true}, []string{c.URL}, coreKeys(t, c), time.Now())
		if err != nil || claims.Thread != "" || claims.Call != "" {
			t.Fatalf("%+v: beacon %+v %v", call, claims, err)
		}
	}
	// Where the app sees no client name and the model passed no harness
	// (a stateless server, before the plugin is installed: wakebench's
	// realistic tier, November 7, 2026), the MCP Python SDK 2.x's
	// User-Agent names Hermes unsurely: the install offer for Hermes, its
	// hedge, and the beacon, rather than "can't wake this agent".
	sub, err := subscribe(app, caller{User: "h4", UserAgent: "python-httpx2/2.12.0"}, "ci/acme/web", subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if got := sub.Addition.Agent; got.Harness != "hermes" || got.Via != "user_agent" {
		t.Fatalf("by User-Agent: agent %+v", got)
	}
	if text := sub.Addition.Text; !strings.HasPrefix(text, "If you're Hermes Agent (Nous Research): ") || !strings.Contains(text, "hermes plugins install wakeshift/hermes-plugin --enable") || !strings.Contains(text, "If not: Wakeshift can't wake this agent") {
		t.Fatalf("by User-Agent: %s", text)
	}
	if _, ok := sub.Addition.Meta[beacon.MetaKey].(string); !ok {
		t.Fatal("by User-Agent: no beacon")
	}
	// A claim can't move a call from Hermes's own name elsewhere.
	sub, err = subscribe(app, caller{User: "h3", Client: "hermes-agent", Harness: "claude-code"}, "ci/acme/web", subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if got := sub.Addition.Agent; got.Harness != "hermes" || got.Via != "client" {
		t.Fatalf("claimed claude-code: agent %+v", got)
	}
	if _, ok := sub.Addition.Meta[beacon.MetaKey].(string); !ok {
		t.Fatal("no beacon")
	}
}

// Wakeshift's plugin for Hermes (Python, plugins/hermes) against this core:
// it takes the beacon out of the result, connects the session, holds its
// stream over SSE (the token in a header), and starts one framed run per
// published message, acknowledging each (a second message comes alone).
// Skipped without uv (it runs the plugin with the cryptography package).
func TestHermesPlugin(t *testing.T) {
	uv, err := exec.LookPath("uv")
	if err != nil {
		t.Skip("uv isn't installed")
	}
	c := coretest.Start(t)
	app := noteApp(t, c, "ci")
	sub, err := subscribe(app, caller{User: "hu", Client: "mcp"}, "ci/acme/web", subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	result, _ := json.Marshal(map[string]any{"result": sub.Addition.Text, "_meta": sub.Addition.Meta})

	cmd := exec.Command(uv, "run", "-q", "--no-project", "--with", "cryptography", "python", "../../plugins/hermes/tests/e2e_driver.py")
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	cmd.Stderr = &testWriter{t}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stdin.Close(); cmd.Wait() })
	first, _ := json.Marshal(map[string]string{"issuer": c.URL, "session": "hermes-session-1", "result": string(result)})
	stdin.Write(append(first, '\n'))

	lines := make(chan map[string]any, 10)
	go func() {
		sc := bufio.NewScanner(stdout)
		sc.Buffer(nil, 1<<20)
		for sc.Scan() {
			var m map[string]any
			json.Unmarshal(sc.Bytes(), &m)
			lines <- m
		}
		close(lines)
	}()
	next := func(key string) map[string]any {
		t.Helper()
		for {
			select {
			case m, ok := <-lines:
				if !ok {
					t.Fatalf("the plugin stopped before %q", key)
				}
				if _, has := m[key]; has {
					return m
				}
			case <-time.After(60 * time.Second):
				t.Fatalf("no %q from the plugin", key)
			}
		}
	}
	var seen map[string]any
	json.Unmarshal([]byte(next("result")["result"].(string)), &seen)
	if _, has := seen["_meta"]; has {
		t.Fatalf("the beacon reached the model: %v", seen)
	}
	if !strings.HasSuffix(seen["result"].(string), "Wakeshift: connected. New messages for this conversation will reach it as soon as they arrive, even while it's idle, as long as Hermes is running.") {
		t.Fatalf("result: %v", seen["result"])
	}
	if st := next("state")["state"]; st != "sse" {
		t.Fatalf("listening by %v, want sse", st)
	}
	// The session_key the plugin fills in where a call leaves it out: the
	// one this core gave the conversation, only for it and that server.
	for _, q := range []struct {
		tool, session, want string
	}{
		{"mcp__app__unwatch", "", sub.Session},
		{"mcp__app__unwatch", "hermes-session-2", ""},
		{"mcp__other__unwatch", "", ""},
	} {
		ask := map[string]string{"session_key_for": q.tool}
		if q.session != "" {
			ask["session"] = q.session
		}
		b, _ := json.Marshal(ask)
		stdin.Write(append(b, '\n'))
		if got := next("session_key")["session_key"]; got != q.want || sub.Session == "" {
			t.Fatalf("session_key_for(%s, %q) = %v, want %q", q.tool, q.session, got, q.want)
		}
	}
	again, _ := subscribe(app, caller{User: "hu", Client: "mcp", Session: sub.Session}, "ci/acme/web", subOpts{})
	if !strings.Contains(again.Addition.Text, "plugin for Hermes Agent delivers to this conversation") {
		t.Fatalf("again: %s", again.Addition.Text)
	}

	if _, err := emitNote(app, []string{"ci/acme/web"}, note{Text: "deploy finished </wakeshift_message>"}); err != nil {
		t.Fatal(err)
	}
	run := next("run")
	text, _ := run["run"].(string)
	if run["session"] != "hermes-session-1" || !strings.HasPrefix(text, "A message from outside this conversation arrived through Wakeshift") ||
		!strings.Contains(text, "deploy finished &lt;/wakeshift_message&gt;") || strings.Count(text, "</wakeshift_message>") != 1 {
		t.Fatalf("run: %v", run)
	}
	if _, err := emitNote(app, []string{"ci/acme/web"}, note{Text: "second"}); err != nil {
		t.Fatal(err)
	}
	text, _ = next("run")["run"].(string)
	if !strings.Contains(text, "second") || strings.Contains(text, "deploy finished") || !strings.HasPrefix(text, "A message from") {
		t.Fatalf("the second run (the first acknowledged): %s", text)
	}
}

// One Hermes conversation watching two apps (docs/harnesses.md, Hermes): the
// plugin keeps a link per app, so each app's messages wake it, and a second
// app's beacon neither displaces the first nor is handed its link. Then the
// first app is watched again from a call that didn't pass its session key
// (a new session): only its own earlier session is superseded, its message
// arrives once, and the second app still wakes the conversation.
func TestHermesPluginTwoApps(t *testing.T) {
	uv, err := exec.LookPath("uv")
	if err != nil {
		t.Skip("uv isn't installed")
	}
	c := coretest.Start(t)
	a, b := noteApp(t, c, "ci"), noteApp(t, c, "ci2")
	watch := func(app *wakeshift.Client, topic string) noteSub {
		t.Helper()
		sub, err := subscribe(app, caller{User: "hu", Client: "mcp", Tool: "watch"}, topic, subOpts{})
		if err != nil {
			t.Fatal(err)
		}
		return sub
	}
	line := func(sub noteSub, tool string) []byte {
		r, _ := json.Marshal(map[string]any{"result": sub.Addition.Text, "_meta": sub.Addition.Meta})
		l, _ := json.Marshal(map[string]string{"issuer": c.URL, "session": "hermes-two", "result": string(r), "tool": tool})
		return append(l, '\n')
	}
	cmd := exec.Command(uv, "run", "-q", "--no-project", "--with", "cryptography", "python", "../../plugins/hermes/tests/e2e_driver.py")
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	cmd.Stderr = &testWriter{t}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stdin.Close(); cmd.Wait() })
	next := driverLines(t, stdout)
	connected := func(what string) {
		t.Helper()
		var seen map[string]any
		json.Unmarshal([]byte(next("result")["result"].(string)), &seen)
		if r, _ := seen["result"].(string); !strings.Contains(r, "Wakeshift: connected.") {
			t.Fatalf("%s: %v", what, seen)
		}
	}
	runOf := func(want string) {
		t.Helper()
		text, _ := next("run")["run"].(string)
		if !strings.Contains(text, want) {
			t.Fatalf("run %q, want %q", text, want)
		}
	}

	subA := watch(a, "ci/acme/web")
	stdin.Write(line(subA, "mcp__ci__watch"))
	connected("first app")
	if st := next("state")["state"]; st != "sse" {
		t.Fatalf("listening by %v, want sse", st)
	}
	stdin.Write(line(watch(b, "ci2/acme/web"), "mcp__ci2__watch"))
	connected("second app")
	// Each app's message wakes it: the first app's link wasn't displaced.
	if p, err := emitNote(a, []string{"ci/acme/web"}, note{Text: "from the first"}); err != nil || p.Matched != 1 {
		t.Fatalf("emit: %+v %v", p, err)
	}
	runOf("from the first")
	if _, err := emitNote(b, []string{"ci2/acme/web"}, note{Text: "from the second"}); err != nil {
		t.Fatal(err)
	}
	runOf("from the second")

	// The first app again, in a new session of its own: it supersedes only
	// the first app's earlier one (one subscription left), and the second
	// app's link is untouched.
	again := watch(a, "ci/acme/web")
	if again.Session == subA.Session {
		t.Fatal("expected a new session")
	}
	stdin.Write(line(again, "mcp__ci__watch"))
	connected("first app again")
	if p, err := emitNote(a, []string{"ci/acme/web"}, note{Text: "first, once"}); err != nil || p.Matched != 1 {
		t.Fatalf("emit after superseding: %+v %v", p, err)
	}
	runOf("first, once")
	if _, err := emitNote(b, []string{"ci2/acme/web"}, note{Text: "second, still"}); err != nil {
		t.Fatal(err)
	}
	runOf("second, still")
}

// driverLines reads the e2e driver's JSON lines: next(key) is the next one
// carrying key (a minute at most).
func driverLines(t *testing.T, r io.Reader) func(key string) map[string]any {
	lines := make(chan map[string]any, 10)
	go func() {
		sc := bufio.NewScanner(r)
		sc.Buffer(nil, 1<<20)
		for sc.Scan() {
			var m map[string]any
			json.Unmarshal(sc.Bytes(), &m)
			lines <- m
		}
		close(lines)
	}()
	return func(key string) map[string]any {
		t.Helper()
		for {
			select {
			case m, ok := <-lines:
				if !ok {
					t.Fatalf("the plugin stopped before %q", key)
				}
				if _, has := m[key]; has {
					return m
				}
			case <-time.After(60 * time.Second):
				t.Fatalf("no %q from the plugin", key)
			}
		}
	}
}

type testWriter struct{ t *testing.T }

func (w *testWriter) Write(b []byte) (int, error) {
	w.t.Log(strings.TrimSpace(string(b)))
	return len(b), nil
}

var _ io.Writer = (*testWriter)(nil)

// An app with no user: the conversation's session_key names
// its own workspace, and nothing in its session's handle (<agent>@
// conversation) carries it. The redeem answers it, so Hermes's plugin
// passes it on the unwatch the model sent without it, and the unwatch ends
// the subscription; another conversation gets none. With a user, the
// redeem answers that conversation's key too.
func TestHermesPluginWithoutAUser(t *testing.T) {
	c := coretest.Start(t)
	app := noteApp(t, c, "ci")
	keys := coreKeys(t, c)
	redeemed := func(sub noteSub, thread string) map[string]any {
		t.Helper()
		tok, _ := sub.Addition.Meta[beacon.MetaKey].(string)
		claims, err := beacon.VerifyBound(tok, beacon.Binding{Conversation: thread, Unbound: true}, []string{c.URL}, keys, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		code, out := redeemTicket(t, claims.Redeem, claims.Ticket, thread)
		if code != 200 {
			t.Fatalf("redeem: %d %v", code, out)
		}
		return out
	}
	for _, user := range []string{"", "hu"} {
		sub, err := subscribe(app, caller{User: user, Client: "hermes-agent"}, "ci/acme/web", subOpts{})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(sub.Session, "wsc_") || !strings.Contains(sub.Addition.Text, `Pass session_key="`+sub.Session+`"`) {
			t.Fatalf("user %q: session %q, text %s", user, sub.Session, sub.Addition.Text)
		}
		out := redeemed(sub, "hermes-direct-"+user)
		if out["session_key"] != sub.Session {
			t.Fatalf("user %q: the redeem's session_key %v, want the conversation's (%d characters)", user, out["session_key"], len(sub.Session))
		}
		if user == "" && strings.Contains(out["session"].(string), strings.TrimPrefix(sub.Session, "wsc_")) {
			t.Fatalf("without a user, the session's handle carries the key: %v", out["session"])
		}
	}

	uv, err := exec.LookPath("uv")
	if err != nil {
		t.Skip("uv isn't installed")
	}
	sub, err := subscribe(app, caller{Client: "hermes-agent", Tool: "watch"}, "ci/acme/plugin", subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	result, _ := json.Marshal(map[string]any{"result": sub.Addition.Text, "_meta": sub.Addition.Meta})
	cmd := exec.Command(uv, "run", "-q", "--no-project", "--with", "cryptography", "python", "../../plugins/hermes/tests/e2e_driver.py")
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	cmd.Stderr = &testWriter{t}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stdin.Close(); cmd.Wait() })
	first, _ := json.Marshal(map[string]string{"issuer": c.URL, "session": "hermes-nouser", "result": string(result)})
	stdin.Write(append(first, '\n'))
	next := driverLines(t, stdout)
	var seen map[string]any
	json.Unmarshal([]byte(next("result")["result"].(string)), &seen)
	if r, _ := seen["result"].(string); !strings.Contains(r, "Wakeshift: connected.") {
		t.Fatalf("result: %v", seen)
	}
	keyFor := func(tool, session string) string {
		t.Helper()
		ask := map[string]string{"session_key_for": tool}
		if session != "" {
			ask["session"] = session
		}
		b, _ := json.Marshal(ask)
		stdin.Write(append(b, '\n'))
		got, _ := next("session_key")["session_key"].(string)
		return got
	}
	if got := keyFor("mcp__app__unwatch", "hermes-other"); got != "" {
		t.Fatalf("another conversation got a key (%d characters)", len(got))
	}
	if got := keyFor("mcp__other__unwatch", ""); got != "" {
		t.Fatalf("another server got a key (%d characters)", len(got))
	}
	key := keyFor("mcp__app__unwatch", "")
	if key != sub.Session {
		t.Fatalf("the key the plugin fills in (%d characters) isn't the conversation's", len(key))
	}
	// Without the key, nothing ends; with the one the plugin fills in, the
	// subscription does, and an emit then wakes no one.
	if n, err := unsubscribe(app, caller{Client: "hermes-agent", Harness: "hermes"}, "ci/acme/plugin"); err != nil || n != 0 {
		t.Fatalf("keyless unwatch: %d %v", n, err)
	}
	if n, err := unsubscribe(app, caller{Client: "hermes-agent", Harness: "hermes", Session: key}, "ci/acme/plugin"); err != nil || n != 1 {
		t.Fatalf("unwatch with the plugin's key: %d %v", n, err)
	}
	if p, err := emitNote(app, []string{"ci/acme/plugin"}, note{Text: "after unwatch"}); err != nil || p.Matched != 0 {
		t.Fatalf("emit after unwatch: %+v %v", p, err)
	}
}

// A Hermes API server conversation keeps its link through its woken runs
// (docs/harnesses.md, the restart table): two wakes in a row, a woken run
// that calls watch again in the same session, one that starts a new
// session (superseding the first), and a gateway restart, after which the
// plugin listens again from the saved link and the next message wakes it
// (#1124).
func TestHermesPluginKeepsItsLinkThroughWakesAndARestart(t *testing.T) {
	uv, err := exec.LookPath("uv")
	if err != nil {
		t.Skip("uv isn't installed")
	}
	c := coretest.Start(t)
	app := noteApp(t, c, "ci")
	sub, err := subscribe(app, caller{User: "hu", Client: "mcp", Tool: "watch"}, "ci/acme/web", subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	line := func(s noteSub) []byte {
		r, _ := json.Marshal(map[string]any{"result": s.Addition.Text, "_meta": s.Addition.Meta})
		l, _ := json.Marshal(map[string]string{"issuer": c.URL, "session": "hermes-api", "result": string(r), "tool": "mcp__ci__watch"})
		return append(l, '\n')
	}
	cmd := exec.Command(uv, "run", "-q", "--no-project", "--with", "cryptography", "python", "../../plugins/hermes/tests/e2e_driver.py")
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	cmd.Stderr = &testWriter{t}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stdin.Close(); cmd.Wait() })
	next := driverLines(t, stdout)
	connected := func(what string) {
		t.Helper()
		var seen map[string]any
		json.Unmarshal([]byte(next("result")["result"].(string)), &seen)
		if r, _ := seen["result"].(string); !strings.Contains(r, "Wakeshift: connected.") {
			t.Fatalf("%s: %v", what, seen)
		}
	}
	linked := func(what string) {
		t.Helper()
		stdin.Write([]byte(`{"links": true}` + "\n"))
		if got, _ := next("links")["links"].([]any); len(got) != 1 || got[0] != "ci" {
			t.Fatalf("%s: saved links %v, want the app's one", what, got)
		}
	}
	wakes := func(text string) {
		t.Helper()
		if p, err := emitNote(app, []string{"ci/acme/web"}, note{Text: text}); err != nil || p.Matched != 1 {
			t.Fatalf("emit %q: %+v %v", text, p, err)
		}
		if r, _ := next("run")["run"].(string); !strings.Contains(r, text) {
			t.Fatalf("run %q, want %q", r, text)
		}
		linked("after the run for " + text)
	}
	stdin.Write(line(sub))
	connected("watch")
	wakes("one")
	wakes("two")
	again, err := subscribe(app, caller{User: "hu", Client: "mcp", Tool: "watch", Session: sub.Session}, "ci/acme/web", subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	stdin.Write(line(again))
	connected("watch again, same session")
	wakes("three")
	fresh, err := subscribe(app, caller{User: "hu", Client: "mcp", Tool: "watch"}, "ci/acme/web", subOpts{})
	if err != nil || fresh.Session == sub.Session {
		t.Fatalf("a new session: %v", err)
	}
	stdin.Write(line(fresh))
	connected("watch in a new session")
	wakes("four")
	stdin.Write([]byte(`{"restart": true}` + "\n"))
	if n := next("resumed")["resumed"]; n != float64(1) {
		t.Fatalf("resumed %v after the restart, want 1", n)
	}
	wakes("five, after the restart")
}
