package coretest_test

import (
	"strings"
	"testing"

	"github.com/wakeshift/server/internal/coretest"
	"github.com/wakeshift/server/internal/harnesses"
	"github.com/wakeshift/server/pkg/wakeshift"
)

// rawUnsubscribe ends the note for a two-segment topic (as
// coretest.Unsubscribe names it) and returns the core's whole answer.
func rawUnsubscribe(t *testing.T, app *wakeshift.Client, call caller, topic string, extra map[string]any) (ended bool, text string) {
	t.Helper()
	a, b, _ := strings.Cut(topic, "/")
	body := map[string]any{"call": call, "name": "note.two", "arguments": map[string]any{"a": a, "b": b}}
	for k, v := range extra {
		body[k] = v
	}
	var out struct {
		Ended    bool `json:"ended"`
		Addition struct {
			Text string `json:"text"`
		} `json:"addition"`
	}
	if err := coretest.RawApp(app, "POST", "/api/apps/v1/event-subscriptions/unsubscribe", body, &out); err != nil {
		t.Fatal(err)
	}
	return out.Ended, out.Addition.Text
}

// An unsubscribe without the session key (wakebench S7, py-mcpserver on
// Claude Code's Monitor path: the agent then watched again without it and
// got every message twice) ends nothing, and its answer says why: call
// again with the key, and don't subscribe again before. For any tool, the
// SDKs' own unsubscribe_events or a wrapped tool's call.unsubscribe (whose
// block carries it). Only conversations that only their key names are told:
// a harness that names the conversation (Codex, Antigravity, Copilot with
// our hook) isn't, nor is a call that passed a key.
func TestUnsubscribingWithoutTheKeySaysToPassIt(t *testing.T) {
	c := coretest.Start(t)
	app := noteApp(t, c, "ci")
	for _, tc := range []struct {
		name string
		call caller
	}{
		{"task path", taskCaller("ada")},
		{"Monitor", caller{User: "bob", Client: "claude-code"}},
		{"Monitor, stateless (by _meta)", caller{User: "cy", Meta: map[string]any{"claudecode/toolUseId": "toolu_1"}}},
		{"no user", taskCaller("")},
		{"Cursor", caller{User: "dee", Client: "Cursor"}},
		{"an unrecognized client", caller{User: "eve", Client: "acme-agent"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sub, err := subscribe(app, tc.call, "ci/x", subOpts{})
			if err != nil {
				t.Fatal(err)
			}
			for _, tool := range []string{"unsubscribe_events", "unwatch"} {
				keyless := tc.call
				keyless.Tool = tool
				ended, text := rawUnsubscribe(t, app, keyless, "ci/x", nil)
				if ended || text != harnesses.KeylessUnsubscribeNote {
					t.Fatalf("%s without the key: %v %q", tool, ended, text)
				}
				if strings.Contains(text, sub.Session) {
					t.Fatal("the note names a session key")
				}
				if !noteLive(t, c, sub.ID) {
					t.Fatalf("%s without the key ended the subscription", tool)
				}
			}
			// A rollback (a failed call's own subscription) isn't the agent's
			// doing: nothing to tell it.
			if _, text := rawUnsubscribe(t, app, tc.call, "ci/x", map[string]any{"reason": "rolled_back"}); text != "" {
				t.Fatalf("a rollback without the key: %q", text)
			}
			// With the key: ended, and not told it lacks one.
			keyed := tc.call
			keyed.Session = sub.Session
			if ended, text := rawUnsubscribe(t, app, keyed, "ci/x", nil); !ended || strings.Contains(text, "nothing was unsubscribed") {
				t.Fatalf("with the key: %v %q", ended, text)
			}
			// With the key and nothing left to end: not told to pass it.
			if ended, text := rawUnsubscribe(t, app, keyed, "ci/x", nil); ended || text != "" {
				t.Fatalf("with the key, nothing left: %v %q", ended, text)
			}
			// A key that names nothing isn't a keyless call: it's told the key
			// isn't known (#927), not to pass one.
			stale := tc.call
			stale.Session = "wsc_aaaaaaaaaaaaaaaaaaaaaaaaaa"
			if ended, text := rawUnsubscribe(t, app, stale, "ci/x", nil); ended || text != "The session_key you passed isn't known (it may have ended)." {
				t.Fatalf("a stale key: %v %q", ended, text)
			}
		})
	}

	// Conversations a harness names in _meta find themselves without a key:
	// never told to pass one, whether or not anything ended.
	for _, tc := range []struct {
		name string
		call caller
	}{
		{"Codex", caller{User: "fay", Client: "codex-mcp-client", Meta: map[string]any{"x-codex-turn-metadata": map[string]any{"thread_id": "thread-9"}}}},
		{"Codex, no user", caller{Client: "codex-mcp-client", Meta: map[string]any{"x-codex-turn-metadata": map[string]any{"thread_id": "thread-8"}}}},
		{"Antigravity", caller{Client: "antigravity-client", Meta: map[string]any{"antigravity.google/conversation_id": "conv-1"}}},
		{"Copilot with the hook", caller{User: "gus", Client: "copilot-cli", Meta: map[string]any{"wakeshift/conversation": "conv-2"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, text := rawUnsubscribe(t, app, tc.call, "ci/x", nil); text != "" {
				t.Fatalf("%q", text)
			}
		})
	}
}

// End to end through the Go SDK: a wrapped tool's Unsubscribe without the
// key returns false, and the call's block says why, after the app's own
// text; the subscription stays. With the key it ends, and the block says
// the key stays. A Codex thread's keyless leave is found by its thread.
func TestWrappedUnsubscribeWithoutTheKey(t *testing.T) {
	c := coretest.Start(t)
	w := newWrappedApp(t, c, "cards")
	const ua = "claude-code/2.1.280"
	if res := w.call(t, "u:ada", ua, "join", map[string]any{"table": "oak"}, nil); res.IsError {
		t.Fatalf("join: %s", res.text())
	}
	key := string(w.subsOf("u:ada")[0].Session)
	res := w.call(t, "u:ada", ua, "leave", map[string]any{"table": "oak"}, nil)
	if res.IsError || res.text() != "ended: false\n\n"+harnesses.KeylessUnsubscribeNote {
		t.Fatalf("a keyless leave: %q", res.text())
	}
	if got, err := w.turn.Emit(ctx, tableArgs{Table: "oak"}, turn{ToCall: "2"}, wakeshift.To("ada")); err != nil || got.Matched != 1 {
		t.Fatalf("after a keyless leave, the emit reached %+v %v", got, err)
	}
	res = w.call(t, "u:ada", ua, "leave", map[string]any{"table": "oak", "session_key": key}, nil)
	if res.IsError || !strings.HasPrefix(res.text(), "ended: true\n\n"+harnesses.KeepSessionNote(key)) || strings.Contains(res.text(), "nothing was unsubscribed") {
		t.Fatalf("a leave with the key: %q", res.text())
	}

	if res := w.call(t, "u:bo", "codex_exec/0.50.0", "join", map[string]any{"table": "oak"}, codexMeta("thread-1")); res.IsError {
		t.Fatalf("Codex join: %s", res.text())
	}
	res = w.call(t, "u:bo", "codex_exec/0.50.0", "leave", map[string]any{"table": "oak"}, codexMeta("thread-1"))
	if res.IsError || !strings.HasPrefix(res.text(), "ended: true") || strings.Contains(res.text(), "nothing was unsubscribed") {
		t.Fatalf("Codex leave: %q", res.text())
	}
}

// A keyless unsubscribe is never matched to a session by anything else:
// with the same app, user and client as live conversations (one's wait
// running), it ends none of them, they still get the next message, and its
// answer names no conversation's key.
func TestUnsubscribingWithoutTheKeyEndsNoOneElses(t *testing.T) {
	c := coretest.Start(t)
	app := noteApp(t, c, "ci")
	call1, sub1, task1 := subscribedTask(t, app, taskCaller("ada"), "ci/x")
	sub2, err := subscribe(app, taskCaller("ada"), "ci/x", subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if sub1.Session == sub2.Session {
		t.Fatal("two keyless subscribes shared a session")
	}
	call2 := taskCaller("ada")
	call2.Session = sub2.Session
	monitor := caller{User: "ada", Client: "claude-code"}
	sub3, err := subscribe(app, monitor, "ci/x", subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	// Another user's conversation, and a userless one.
	bob, err := subscribe(app, taskCaller("bob"), "ci/x", subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	lone, err := subscribe(app, taskCaller(""), "ci/x", subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	all := []noteSub{sub1, sub2, sub3, bob, lone}
	for _, keyless := range []caller{taskCaller("ada"), monitor, taskCaller("bob"), taskCaller(""), {User: "ada", Client: "claude-code", Tool: "unsubscribe_events"}} {
		ended, text := rawUnsubscribe(t, app, keyless, "ci/x", nil)
		if ended || text != harnesses.KeylessUnsubscribeNote {
			t.Fatalf("keyless %+v: %v %q", keyless, ended, text)
		}
		for _, s := range all {
			if strings.Contains(text, s.Session) {
				t.Fatal("the answer names a conversation's key")
			}
		}
	}
	for _, s := range all {
		if !noteLive(t, c, s.ID) {
			t.Fatalf("a keyless unsubscribe ended %s", s.ID)
		}
	}
	// Both of ada's task conversations still get the next message.
	second := waitTask(t, app, call2)
	if p, err := emitNote(app, []string{"ci/x"}, note{Text: "still here"}); err != nil || p.Matched < len(all) {
		t.Fatalf("the emit: %+v %v", p, err)
	}
	var first, its taskAnswer
	waitFor(t, "the first", func() bool { first = taskGet(t, app, call1, task1.id(), 2); return first.status() == "completed" })
	waitFor(t, "the second", func() bool { its = taskGet(t, app, call2, second.id(), 2); return its.status() == "completed" })
	if !strings.Contains(first.text(), "still here") || !strings.Contains(its.text(), "still here") {
		t.Fatalf("after a keyless unsubscribe: %q, %q", first.text(), its.text())
	}
}
