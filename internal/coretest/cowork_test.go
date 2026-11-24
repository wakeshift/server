package coretest_test

import (
	"strings"
	"testing"

	"github.com/wakeshift/server/internal/coretest"
	"github.com/wakeshift/server/internal/harnesses"
	"github.com/wakeshift/server/pkg/wakeshift"
)

// A Cowork-shaped call (a connector called from Anthropic's servers, with
// clientInfo "claude-ai", no _meta and no tool-use ID) is recognized as
// Claude Cowork, and subscribing gives the Monitor's command, the check
// that the task's shell reaches the core, what to tell the user if it
// doesn't, and what to do without a Monitor tool (claude.ai chat sends the
// same name): no install, no plugin, no beacon. The command wakes it: a
// message emitted while it runs is its next line. Subscribing again while
// it runs says so; once it's stopped, the short note gives the same
// command; other results add nothing.
func TestCoworkMonitor(t *testing.T) {
	c := coretest.Start(t)
	app := noteApp(t, c, "ci")
	call := caller{User: "cw", Client: "claude-ai"}
	sub, err := subscribe(app, call, "ci/acme/web", subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	call.Session = sub.Session
	if sub.Addition.Agent != (wakeshift.Recognized{Harness: "claude-cowork", Via: "client"}) {
		t.Fatalf("recognized as %+v", sub.Addition.Agent)
	}
	text := sub.Addition.Text
	mon := monitorRE.FindStringSubmatch(text)
	if mon == nil || mon[1] != harnesses.MonitorLoop(strings.SplitN(mon[2], "&until=hooks", 2)[0]) {
		t.Fatalf("no Monitor command: %s", text)
	}
	wait := strings.SplitN(mon[2], "&until=hooks", 2)[0]
	if text != harnesses.CoworkNote("Ci", wait)+` Pass session_key="`+sub.Session+`" to this app's tools from now on (keep it to this conversation; don't share or log it).` {
		t.Fatalf("result: %s", text)
	}
	host := harnesses.CoworkHost(wait)
	if host == "" || !strings.Contains(c.URL, host) {
		t.Fatalf("host %q for core %s", host, c.URL)
	}
	for _, want := range []string{"If you have a Monitor tool", "curl -sS -o /dev/null -w '%{http_code}\\n' " + strings.TrimSuffix(c.URL, "/") + "/health`",
		`turn on "Use the internet while making files" in the Claude app's Settings > Capabilities, then click Manage next to "Allowed websites" and add ` + host + " (on a Team or Enterprise plan, an owner does this under Organization settings > Capabilities)",
		"If you have no Monitor tool (as in claude.ai chat), don't run anything", "nothing will notify you here"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q: %s", want, text)
		}
	}
	for _, not := range []string{"wakeshift-beacon", "install", "plugin", "run_in_background", "call this app's tools again", "call this again"} {
		if strings.Contains(text, not) {
			t.Fatalf("result has %q: %s", not, text)
		}
	}
	if len(sub.Addition.Meta) != 0 {
		t.Fatalf("meta: %v", sub.Addition.Meta)
	}
	if r, _ := annotate(app, call); r.Text != "" || len(r.Meta) != 0 {
		t.Fatalf("annotate before the Monitor: %+v", r)
	}

	s, kill := runMonitor(t, mon[1])
	waitFor(t, "connected", func() bool {
		st, _ := noteStatus(app, "ci/acme/web")
		return st.Connected == 1
	})
	running, err := subscribe(app, call, "ci/acme/web", subOpts{})
	if err != nil || running.Addition.Text != harnesses.MonitorRunningNote() {
		t.Fatalf("again while it runs: %+v %v", running, err)
	}
	if r, _ := annotate(app, call); r.Text != "" || len(r.Meta) != 0 {
		t.Fatalf("annotate while it runs: %+v", r)
	}
	emitNote(app, []string{"ci/acme/web"}, note{Text: "deploy finished"})
	f := s.expect("deploy finished")
	// Killed once the next round, which confirmed it, holds the stream (as
	// TestMonitorGoneMessagesWaitForTheNextMonitor does).
	tenant, _ := topicSubRow(t, c, sub.ID)
	waitFor(t, "confirmed by the next round", func() bool {
		return count(t, c, tenant, `SELECT count(*) FROM deliveries WHERE event_id = $1 AND state = 'acked'`, f.Event.ID) == 1
	})
	waitFor(t, "the next round holding the stream", func() bool {
		return count(t, c, tenant, `SELECT count(*) FROM stream_leases WHERE holder LIKE 'poll\_wait\_%'
			AND holder <> (SELECT lease_holder FROM deliveries WHERE event_id = $1)`, f.Event.ID) == 1
	})
	kill()
	waitFor(t, "not connected", func() bool {
		st, _ := noteStatus(app, "ci/acme/web")
		return st.Connected == 0
	})
	again, err := subscribe(app, call, "ci/acme/web", subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	m := monitorRE.FindStringSubmatch(again.Addition.Text)
	if m == nil || m[1] != mon[1] || again.Addition.Text != harnesses.CoworkAgainNote("Ci", wait) {
		t.Fatalf("again once stopped: %s", again.Addition.Text)
	}
}

// What names Cowork besides its client name: a plugin's local server's
// client name, and the agent's own word, for a call nothing else names (a
// stateless server sees no client name). A claim never moves a call
// another client name names: Claude Code stays Claude Code.
func TestCoworkRecognized(t *testing.T) {
	c := coretest.Start(t)
	app := noteApp(t, c, "ci")
	for i, tc := range []struct {
		call caller
		want wakeshift.Recognized
	}{
		{caller{Client: "local-agent-mode-plugin:acme:ci"}, wakeshift.Recognized{Harness: "claude-cowork", Via: "client"}},
		{caller{Client: ""}.WithHarness("claude-ai"), wakeshift.Recognized{Harness: "claude-cowork", Via: "claim", Claimed: "claude-cowork"}},
		{caller{Client: ""}.WithHarness("Claude app (claude.ai)"), wakeshift.Recognized{Harness: "claude-cowork", Via: "claim", Claimed: "claude-cowork"}},
		{caller{Client: ""}.WithHarness("Cowork"), wakeshift.Recognized{Harness: "claude-cowork", Via: "claim", Claimed: "claude-cowork"}},
		{caller{Client: "claude-ai"}.WithHarness("claude cowork"), wakeshift.Recognized{Harness: "claude-cowork", Via: "claim", Claimed: "claude-cowork"}},
		{caller{Client: "claude-code", Meta: map[string]any{"claudecode/toolUseId": "toolu_cw"}}.WithHarness("claude-ai"), wakeshift.Recognized{Harness: "claude-code", Via: "client", Claimed: "claude-cowork"}},
	} {
		tc.call.User = "cr" + string(rune('a'+i))
		sub, err := subscribe(app, tc.call, "ci/acme/web", subOpts{})
		if err != nil {
			t.Fatal(err)
		}
		if sub.Addition.Agent != tc.want {
			t.Errorf("%+v: recognized as %+v", tc.call, sub.Addition.Agent)
		}
		cowork := tc.want.Harness == "claude-cowork"
		if got := strings.HasPrefix(sub.Addition.Text, "Subscribed, but that alone won't tell you when something arrives: Ci delivers its notifications through Wakeshift, which reaches this conversation only through a Monitor you start."); got != cowork {
			t.Errorf("%+v: %s", tc.call, sub.Addition.Text)
		}
	}
}
