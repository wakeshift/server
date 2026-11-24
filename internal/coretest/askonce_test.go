package coretest_test

import (
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/beacon"
	"github.com/wakeshift/server/internal/coretest"
	"github.com/wakeshift/server/internal/harnesses"
)

// Ask once per conversation, action first (in testing, a status tool that subscribes on every poll repeated the whole ask, and
// Claude Code set the copies aside as a repeated injected request; Codex
// quoted a block that opened with "This subscription ends after its first
// message." as if that were all of it, and asked nothing). The full ask
// comes once per conversation; a later subscribing call while nothing
// wakes it points back to it in one sentence, and one that something wakes
// says only that it's subscribed. Notes such as the once note come last.

const onceText = "This subscription ends after its first message."

// A Claude Code conversation that passes its key back: the full question
// once, then one sentence pointing back to it, with the beacon still
// there; another conversation gets the full question; a running Monitor
// gets one line and no command.
func TestClaudeCodeAsksOncePerConversation(t *testing.T) {
	c := coretest.Start(t)
	app := noteApp(t, c, "ci")
	call := func(session, toolUse string) caller {
		return caller{Client: "claude-code", Session: session, Tool: "watch", Meta: map[string]any{"claudecode/toolUseId": toolUse}}
	}
	first, err := subscribe(app, call("", "toolu_1"), "ci/acme", subOpts{Once: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(first.Addition.Text, "Subscribed, but that alone won't tell you when something arrives") || !strings.HasSuffix(first.Addition.Text, onceText) ||
		monitorRE.FindStringSubmatch(first.Addition.Text) == nil || !strings.Contains(first.Addition.Text, `session_key="`+first.Session+`"`) {
		t.Fatalf("the first result: %s", first.Addition.Text)
	}

	// The agent polls the app's status tool, which subscribes too.
	claudeCode, _ := harnesses.Get("claude-code")
	tenant, _ := topicSubRow(t, c, first.ID)
	for i, tool := range []string{"toolu_2", "toolu_3"} {
		// Polls a while apart: no earlier result's beacon is still
		// connecting (PluginConnectingNote).
		if err := c.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE connector_tickets SET created_at = created_at - interval '1 minute'`)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		again, err := subscribe(app, call(first.Session, tool), "ci/acme", subOpts{Once: true})
		if err != nil {
			t.Fatal(err)
		}
		want := harnesses.PluginAskedNote(claudeCode, c.URL, "Ci") + "\n\n" + onceText
		if again.Addition.Text != want {
			t.Fatalf("poll %d:\n%s\nwant\n%s", i, again.Addition.Text, want)
		}
		if strings.Contains(again.Addition.Text, "with this command") || strings.Contains(again.Addition.Text, "AskUserQuestion") || len(again.Addition.Text) > 900 {
			t.Fatalf("poll %d repeats the ask: %s", i, again.Addition.Text)
		}
		if i == 0 {
			// The beacon stays: a plugin installed since connects from it.
			if again.Addition.Meta[beacon.MetaKey] == nil {
				t.Fatalf("poll %d has no beacon: %+v", i, again.Addition)
			}
		}
	}

	// Another conversation (no key) is asked in full.
	other, err := subscribe(app, call("", "toolu_9"), "ci/acme", subOpts{})
	if err != nil || other.Session == first.Session || !strings.HasPrefix(other.Addition.Text, "Subscribed, but that alone won't tell you") {
		t.Fatalf("another conversation: %+v %v", other, err)
	}

	// The user picks "this session only": the Monitor runs, so a later call
	// says only that it's subscribed.
	readLines(t, monitorRE.FindStringSubmatch(first.Addition.Text)[1])
	waitFor(t, "the Monitor connected", func() bool {
		st, _ := noteStatus(app, "ci/acme")
		return st.Connected >= 1
	})
	running, err := subscribe(app, call(first.Session, "toolu_4"), "ci/acme", subOpts{Once: true})
	if err != nil || running.Addition.Text != harnesses.MonitorRunningNote()+"\n\n"+onceText {
		t.Fatalf("while the Monitor runs: %+v %v", running, err)
	}
}

// Once the Monitor has run and stopped, a later call needs its command
// again to be woken: that result carries it (and doesn't ask the question).
func TestClaudeCodeStoppedMonitorGetsItsCommand(t *testing.T) {
	c := coretest.Start(t)
	c.Streams.WaitGrace = 300 * time.Millisecond
	app := noteApp(t, c, "ci")
	call := caller{Client: "claude-code", Meta: map[string]any{"claudecode/toolUseId": "toolu_1"}}
	first, err := subscribe(app, call, "ci/acme", subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	m := monitorRE.FindStringSubmatch(first.Addition.Text)
	_, kill := runMonitor(t, strings.Replace(m[1], "timeout="+harnesses.MonitorWait, "timeout=3s", 1))
	waitFor(t, "the Monitor connected", func() bool {
		st, _ := noteStatus(app, "ci/acme")
		return st.Connected == 1
	})
	kill()
	waitFor(t, "the Monitor gone", func() bool {
		st, _ := noteStatus(app, "ci/acme")
		return st.Connected == 0
	})
	call.Session = first.Session
	again, err := subscribe(app, call, "ci/acme", subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if f := monitorRE.FindStringSubmatch(again.Addition.Text); f == nil || f[2] != m[2] || !strings.Contains(again.Addition.Text, "Your Wakeshift Monitor isn't running") ||
		strings.Contains(again.Addition.Text, "AskUserQuestion") {
		t.Fatalf("after the Monitor stopped: %s", again.Addition.Text)
	}
}

// A conversation subscribed before while nothing could be asked (its
// harness wasn't known yet) was never asked: its first subscribe as
// Claude Code gets the full form, not the repeat. Its calls name no tool
// call (no client name, no _meta: only the agent's claim), so no beacon
// could reach Wakeshift's plugin from them, and the full form is the
// Monitor's, without the plugin's question (pluginConnectable).
func TestNeverAskedIsAskedInFull(t *testing.T) {
	c := coretest.Start(t)
	app := noteApp(t, c, "ci")
	anon, err := subscribe(app, caller{UserAgent: "node"}, "ci/anon", subOpts{})
	if err != nil || !strings.HasPrefix(anon.Addition.Text, "Wakeshift can't wake this agent") {
		t.Fatalf("anon: %+v %v", anon, err)
	}
	decided, err := subscribe(app, caller{UserAgent: "node", Session: anon.Session, Harness: "claude-code"}, "ci/anon", subOpts{})
	if err != nil || decided.Session != anon.Session || !strings.HasPrefix(decided.Addition.Text, harnesses.UnconnectableNote) || !strings.Contains(decided.Addition.Text, "start the Monitor tool now") {
		t.Fatalf("decided: %+v %v", decided, err)
	}
}

// Codex, through the one-block-per-call path a wrapped tool takes: the
// full ask once with the once note last, then one sentence; the beacon in
// every block; another thread is asked in full.
func TestCodexAsksOncePerConversation(t *testing.T) {
	c := coretest.Start(t)
	app := noteApp(t, c, "ci")
	block := func(call map[string]any) (additionOut, string) {
		t.Helper()
		var sub deferredOut
		body := map[string]any{"call": call, "name": "note.two", "arguments": map[string]any{"a": "ci", "b": "x"}, "defer_addition": true, "once": true}
		if code, e := appAPI(t, c, app, "/api/apps/v1/event-subscriptions", body, &sub); code != 200 {
			t.Fatalf("subscribe: %d %s", code, e)
		}
		ac := map[string]any{}
		for k, v := range call {
			ac[k] = v
		}
		ac["session"] = sub.Session
		var b additionOut
		if code, e := appAPI(t, c, app, "/api/apps/v1/calls/addition", map[string]any{"call": ac, "pending": sub.Pending}, &b); code != 200 {
			t.Fatalf("addition: %d %s", code, e)
		}
		if b.Meta[beacon.MetaKey] == nil {
			t.Fatalf("no beacon: %+v", b)
		}
		return b, sub.Session
	}
	first, session := block(codexCall("", "thread-1", ""))
	if !strings.HasPrefix(first.Text, "Wakeshift can't wake this conversation yet. Ask the user now") || !strings.HasSuffix(first.Text, onceText) {
		t.Fatalf("the first block: %s", first.Text)
	}
	again, _ := block(codexCall("", "thread-1", session))
	codex, _ := harnesses.Get("codex")
	if want := harnesses.AskedNote(codex, c.URL) + "\n\n" + onceText; again.Text != want {
		t.Fatalf("the next block:\n%s\nwant\n%s", again.Text, want)
	}
	if strings.Contains(again.Text, "codex.sh") || strings.Contains(again.Text, "session_key") {
		t.Fatalf("the next block repeats the ask or the key it was passed: %s", again.Text)
	}
	other, _ := block(codexCall("", "thread-2", ""))
	if !strings.HasPrefix(other.Text, "Wakeshift can't wake this conversation yet. Ask the user now") {
		t.Fatalf("another thread: %s", other.Text)
	}
}
