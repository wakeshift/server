package coretest_test

import (
	"strings"
	"testing"
	"time"

	"github.com/wakeshift/server/internal/coretest"
	"github.com/wakeshift/server/internal/harnesses"
)

// Unsubscribing and subscribing again: the
// conversation keeps one session only if it passes its session key again.
// For Claude Code nothing else names the conversation (its MCP calls carry
// no conversation ID, and a stateless 2026-07-28 server has no MCP
// session), so the core never guesses: a call without the key is a new
// conversation. What it can do is say the key stays the same, end the old
// wait at once, and keep its own state honest.

// The result of the call that unsubscribed says the session key stays the
// same, once, for a conversation that passes its key back; other results
// and conversations a harness names by ID don't say it.
func TestUnsubscribingSaysTheSessionKeyStays(t *testing.T) {
	c := coretest.Start(t)
	app := noteApp(t, c, "ci")
	for _, tc := range []struct {
		name string
		call caller
	}{
		{"task path", taskCaller("ada")},
		{"Monitor", caller{User: "bob", Client: "claude-code"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sub, err := subscribe(app, tc.call, "ci/x", subOpts{})
			if err != nil {
				t.Fatal(err)
			}
			call := tc.call
			call.Session = sub.Session
			if _, err := subscribe(app, call, "ci/y", subOpts{}); err != nil {
				t.Fatal(err)
			}
			before, _ := annotate(app, call)
			if strings.Contains(before.Text, "after unsubscribing") {
				t.Fatalf("before unsubscribing: %s", before.Text)
			}
			// One of two: still watching the other.
			if n, err := unsubscribe(app, call, "ci/x"); err != nil || n != 1 {
				t.Fatal(n, err)
			}
			want := harnesses.KeepSessionNote(sub.Session)
			if got, _ := annotate(app, call); !strings.HasPrefix(got.Text, want) {
				t.Fatalf("after unsubscribing from one: %q", got.Text)
			}
			// The last.
			if n, err := unsubscribe(app, call, "ci/y"); err != nil || n != 1 {
				t.Fatal(n, err)
			}
			if got, _ := annotate(app, call); !strings.HasPrefix(got.Text, want) {
				t.Fatalf("after unsubscribing from the last: %q", got.Text)
			}
			// Once: the next result doesn't say it again.
			if got, _ := annotate(app, call); strings.Contains(got.Text, "after unsubscribing") {
				t.Fatalf("the next result: %q", got.Text)
			}
		})
	}

	// An SDK's own unsubscribe_events tool gets the note in the
	// unsubscribe's answer (and adds it to its reply), only when it ended
	// something; it asks for no block, so no later result repeats it.
	rawUnsubscribe := func(call caller, topic string) (ended bool, text string) {
		t.Helper()
		a, b, _ := strings.Cut(topic, "/")
		name, args := "note.two", map[string]any{"a": a, "b": b} // as coretest.Unsubscribe names a two-segment topic
		var out struct {
			Ended    bool `json:"ended"`
			Addition struct {
				Text string `json:"text"`
			} `json:"addition"`
		}
		if err := coretest.RawApp(app, "POST", "/api/apps/v1/event-subscriptions/unsubscribe", map[string]any{"call": call, "name": name, "arguments": args}, &out); err != nil {
			t.Fatal(err)
		}
		return out.Ended, out.Addition.Text
	}
	dee := caller{User: "dee", Client: "claude-code", Tasks: true, Tool: "unsubscribe_events"}
	sub, err := subscribe(app, dee, "ci/x", subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	dee.Session = sub.Session
	if ended, text := rawUnsubscribe(dee, "ci/x"); !ended || text != harnesses.KeepSessionNote(sub.Session) {
		t.Fatalf("the unsubscribe's answer: %v %q", ended, text)
	}
	if ended, text := rawUnsubscribe(dee, "ci/x"); ended || text != "" {
		t.Fatalf("nothing left to end: %v %q", ended, text)
	}
	if got, _ := annotate(app, dee); strings.Contains(got.Text, "after unsubscribing") {
		t.Fatalf("a result after unsubscribe_events: %q", got.Text)
	}
	// A wrapped tool's unsubscribe answers without it: its block says it.
	wrapped := dee
	wrapped.Tool = "leave"
	if _, err := subscribe(app, wrapped, "ci/x", subOpts{}); err != nil {
		t.Fatal(err)
	}
	if ended, text := rawUnsubscribe(wrapped, "ci/x"); !ended || text != "" {
		t.Fatalf("a wrapped tool's unsubscribe: %v %q", ended, text)
	}

	// A Codex thread in a user's workspace is found by its thread, so it's
	// never told to pass a key.
	codex := caller{User: "cy", Client: "codex-mcp-client", Meta: map[string]any{"x-codex-turn-metadata": map[string]any{"thread_id": "thread-1"}}}
	if _, err := subscribe(app, codex, "ci/x", subOpts{}); err != nil {
		t.Fatal(err)
	}
	if ended, text := rawUnsubscribe(codex, "ci/x"); !ended || text != "" {
		t.Fatalf("a Codex thread's unsubscribe: %v %q", ended, text)
	}
	if got, _ := annotate(app, codex); strings.Contains(got.Text, "after unsubscribing") {
		t.Fatalf("a Codex thread: %q", got.Text)
	}
}

// An unsubscribe that leaves the conversation nothing to wait for ends its
// running wait at once, naming the session key, and the session stops
// counting as following a wait: subscribing again with the key says to
// call the wait tool again, which waits on the same session.
func TestUnsubscribingEndsTheWaitAtOnce(t *testing.T) {
	c := coretest.Start(t)
	app := noteApp(t, c, "ci")
	call, sub, task := subscribedTask(t, app, taskCaller("ada"), "ci/x")
	held := make(chan taskAnswer, 1)
	go func() { held <- taskGet(t, app, call, task.id(), 20) }()
	time.Sleep(300 * time.Millisecond)
	start := time.Now()
	if _, err := unsubscribe(app, call, "ci/x"); err != nil {
		t.Fatal(err)
	}
	var got taskAnswer
	select {
	case got = <-held:
	case <-time.After(5 * time.Second):
		t.Fatal("the wait went on holding after the conversation's last subscription ended")
	}
	if got.status() != "completed" || !strings.Contains(got.text(), "ended for good") || !strings.Contains(got.text(), `session_key="`+sub.Session+`"`) {
		t.Fatalf("the held poll, %v after unsubscribing: %+v", time.Since(start), got)
	}
	// Polled again (Claude Code after --continue): the same.
	if again := taskGet(t, app, call, task.id(), 0); again.status() != "completed" || again.text() != got.text() {
		t.Fatalf("polled again: %+v", again)
	}

	resub, err := subscribe(app, call, "ci/x", subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if resub.Session != sub.Session {
		t.Fatal("subscribing again with the key started another session")
	}
	if !strings.Contains(resub.Addition.Text, "Your Wakeshift wait isn't running") || !strings.Contains(resub.Addition.Text, `session_key="`+sub.Session+`"`) {
		t.Fatalf("subscribing again: %s", resub.Addition.Text)
	}
	next := waitTask(t, app, call)
	if next.status() != "working" {
		t.Fatalf("the wait tool again: %+v", next)
	}
	emitNote(app, []string{"ci/x"}, note{Text: "after the re-watch"})
	var done taskAnswer
	waitFor(t, "completed", func() bool { done = taskGet(t, app, call, next.id(), 2); return done.status() == "completed" })
	if !strings.Contains(done.text(), "after the re-watch") {
		t.Fatalf("the new wait: %+v", done)
	}

	// Unsubscribing from one of two topics leaves the wait running.
	call2, _, task2 := subscribedTask(t, app, taskCaller("bob"), "ci/a")
	if _, err := subscribe(app, call2, "ci/b", subOpts{}); err != nil {
		t.Fatal(err)
	}
	held2 := make(chan taskAnswer, 1)
	go func() { held2 <- taskGet(t, app, call2, task2.id(), 3) }()
	time.Sleep(300 * time.Millisecond)
	if _, err := unsubscribe(app, call2, "ci/a"); err != nil {
		t.Fatal(err)
	}
	if got := <-held2; got.status() != "working" {
		t.Fatalf("with a subscription left: %+v", got)
	}
}

// Subscribing again without the session key never reaches the earlier
// session, however much else matches: the same app, user and client, even
// while that session's wait runs. Such a call is a new conversation, as a
// second Claude Code conversation of the same user would be, and it learns
// nothing of the first: not its key, not its task, not its messages.
func TestSubscribingAgainWithoutTheKeyIsANewConversation(t *testing.T) {
	c := coretest.Start(t)
	app := noteApp(t, c, "ci")
	call, sub, task := subscribedTask(t, app, taskCaller("ada"), "ci/x")

	keyless, err := subscribe(app, taskCaller("ada"), "ci/x", subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if keyless.Session == sub.Session {
		t.Fatal("a call without the key reached the conversation's session")
	}
	if strings.Contains(keyless.Addition.Text, sub.Session) || strings.Contains(keyless.Addition.Text, "Your Wakeshift wait is running") {
		t.Fatalf("the new conversation was told of the first: %s", keyless.Addition.Text)
	}
	other := taskCaller("ada")
	other.Session = keyless.Session
	// Its wait call starts its own task, which neither replaces nor
	// confirms the first's: both conversations get the message.
	second := waitTask(t, app, other)
	emitNote(app, []string{"ci/x"}, note{Text: "to both"})
	var first, its taskAnswer
	waitFor(t, "the first", func() bool { first = taskGet(t, app, call, task.id(), 2); return first.status() == "completed" })
	waitFor(t, "the second", func() bool { its = taskGet(t, app, other, second.id(), 2); return its.status() == "completed" })
	if strings.Contains(its.text(), sub.Session) || !strings.Contains(its.text(), keyless.Session) {
		t.Fatalf("the second conversation's result names the first's key: %s", its.text())
	}

	// Another user passing the first conversation's key, or another client
	// of the same user, doesn't reach it either.
	bob := taskCaller("bob")
	bob.Session = sub.Session
	if s, err := subscribe(app, bob, "ci/x", subOpts{}); err != nil || s.Session == sub.Session {
		t.Fatalf("another user with the key: %v %v", s.Session == sub.Session, err)
	}
	cursor := caller{User: "ada", Client: "Cursor", Session: sub.Session, Tasks: true}
	if s, err := subscribe(app, cursor, "ci/x", subOpts{}); err == nil && s.Session == sub.Session {
		t.Fatal("another client with the key reached the session")
	}
	if got := waitTask(t, app, cursor); got.id() != "" {
		t.Fatalf("another client's wait call with the key: %+v", got)
	}
	// Nor does another app.
	otherApp := noteApp(t, c, "other")
	if s, err := subscribe(otherApp, call, "ci/x", subOpts{}); err == nil && s.Session == sub.Session {
		t.Fatal("another app with the key reached the session")
	}

	// A userless conversation's key names its own workspace: a call
	// without it is another conversation, in a workspace of its own.
	lone, err := subscribe(app, taskCaller(""), "ci/z", subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	again, err := subscribe(app, taskCaller(""), "ci/z", subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	t1, _ := topicSubRow(t, c, lone.ID)
	t2, _ := topicSubRow(t, c, again.ID)
	if again.Session == lone.Session || t1 == t2 {
		t.Fatal("two userless calls without a key shared a conversation")
	}
}

// A harness that names its conversation (a Codex thread) needs no key:
// subscribing again from the same thread finds the same session, and
// another thread doesn't.
func TestSubscribingAgainFromTheSameThread(t *testing.T) {
	c := coretest.Start(t)
	app := noteApp(t, c, "ci")
	thread := func(id string) caller {
		return caller{User: "ada", Client: "codex-mcp-client", Meta: map[string]any{"x-codex-turn-metadata": map[string]any{"thread_id": id}}}
	}
	first, err := subscribe(app, thread("t-1"), "ci/x", subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := unsubscribe(app, thread("t-1"), "ci/x"); err != nil {
		t.Fatal(err)
	}
	again, err := subscribe(app, thread("t-1"), "ci/x", subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if again.Session != first.Session {
		t.Fatal("the same thread got a second session")
	}
	other, err := subscribe(app, thread("t-2"), "ci/x", subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if other.Session == first.Session {
		t.Fatal("another thread reached the first thread's session")
	}
}
