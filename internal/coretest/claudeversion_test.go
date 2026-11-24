package coretest_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/wakeshift/server/internal/beacon"
	"github.com/wakeshift/server/internal/connectorversion"
	"github.com/wakeshift/server/internal/core"
	"github.com/wakeshift/server/internal/coretest"
	"github.com/wakeshift/server/internal/harnesses"
	"github.com/wakeshift/server/internal/pluginmark"
)

// withMarketplace makes this core's offers give Wakeshift's plugin from a
// local marketplace, as the bench's core does (WAKESHIFT_AGENT_PLUGINS),
// and returns the one command and the two steps for its address.
func withMarketplace(t *testing.T, c *coretest.Core) (one, two string) {
	t.Helper()
	const src = "/tmp/wb/agent-plugins"
	harnesses.SetAgentPlugins(src)
	t.Cleanup(func() { harnesses.SetAgentPlugins("") })
	return "claude plugin install wakeshift --marketplace " + src,
		"claude plugin marketplace add " + src + " && claude plugin install wakeshift@wakeshift"
}

// The subscribe result offers the install command the conversation's
// Claude Code runs: `claude plugin install … --marketplace` from 2.1.292,
// and before it, or when neither the User-Agent nor clientInfo says, the
// marketplace first and the install after (2.1.280 has no --marketplace).
func TestClaudeCodeInstallOfferByVersion(t *testing.T) {
	c := coretest.Start(t)
	one, two := withMarketplace(t, c)
	app := noteApp(t, c, "ci")
	for i, tc := range []struct {
		ua, version string
		new         bool
	}{
		{"claude-code/2.1.280 (cli)", "", false},
		{"claude-code/2.1.291 (sdk-cli)", "", false},
		{"claude-code/2.1.292 (cli)", "", true},
		{"claude-code/2.1.318 (cli)", "", true},
		{"", "2.1.280", false},
		{"", "2.1.318", true},
		{"", "", false},
		{"claude-code/nightly", "", false},
	} {
		call := caller{User: "v" + string(rune('a'+i)), Client: "claude-code", UserAgent: tc.ua, ClientVersion: tc.version, Meta: map[string]any{"claudecode/toolUseId": "toolu_v" + string(rune('a'+i))}}
		sub, err := subscribe(app, call, "ci/acme/web", subOpts{})
		if err != nil {
			t.Fatal(err)
		}
		want, not := two, one
		if tc.new {
			want, not = one, two
		}
		text := sub.Addition.Text
		if !strings.Contains(text, "\"Notify me in every session\" (description: runs `"+want+"` (") || !strings.Contains(text, "`! "+want+"`") || strings.Contains(text, not) {
			t.Fatalf("%q %q: %s", tc.ua, tc.version, text)
		}
		if !tc.new && strings.Contains(text, "--marketplace") {
			t.Fatalf("%q %q: --marketplace for a Claude Code without it: %s", tc.ua, tc.version, text)
		}
	}
}

// Hooks from Wakeshift's installer, out of date: the update offer is the
// plugin (which takes their place, keeping the conversation's link), in the
// form the conversation's Claude Code runs.
func TestClaudeCodeOutdatedHooksUpdateByVersion(t *testing.T) {
	c := coretest.StartWith(t, func(o *core.Options) {
		o.Connectors = connectorversion.Minimums{"wakeshift": "2026.10.20"}
	})
	one, two := withMarketplace(t, c)
	const tail = " && claude plugin marketplace update wakeshift && claude plugin update wakeshift@wakeshift"
	app := noteApp(t, c, "ci")
	for _, tc := range []struct {
		ua, want, not string
		session       string
	}{
		{"claude-code/2.1.280 (cli)", two + tail, one, "0ddc1a00-0000-4000-8000-000000000001"},
		{"claude-code/2.1.318 (cli)", one + tail, two, "0ddc1a00-0000-4000-8000-000000000002"},
	} {
		call := caller{User: "o" + tc.session[len(tc.session)-1:], Client: "claude-code", UserAgent: tc.ua, Meta: map[string]any{"claudecode/toolUseId": "toolu_o" + tc.session[len(tc.session)-1:]}}
		sub, err := subscribe(app, call, "ci/acme/web", subOpts{})
		if err != nil {
			t.Fatal(err)
		}
		// The hooks redeem its beacon, as an install from before connectors
		// said their version (counted as version 0): refused.
		tok, _ := sub.Addition.Meta[beacon.MetaKey].(string)
		claims, err := beacon.VerifyBound(tok, beacon.Binding{Conversation: tc.session, Call: call.Meta["claudecode/toolUseId"].(string)}, []string{c.URL}, coreKeys(t, c), time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if code, out := redeemTicket(t, claims.Redeem, claims.Ticket, tc.session); code != http.StatusUpgradeRequired {
			t.Fatalf("redeem: %d %v", code, out)
		}
		call.Session = sub.Session
		call.Meta = map[string]any{"claudecode/toolUseId": call.Meta["claudecode/toolUseId"].(string) + "2"}
		again, err := subscribe(app, call, "ci/acme/api", subOpts{})
		if err != nil {
			t.Fatal(err)
		}
		text := again.Addition.Text
		if !strings.HasPrefix(text, "Wakeshift's hooks for Claude Code are out of date.") || !strings.Contains(text, "runs `"+tc.want+"`") ||
			!strings.Contains(text, "in place of the hooks Wakeshift's installer wrote to ~/.claude/settings.json") || strings.Contains(text, tc.not+" ") || strings.Contains(text, "uninstall") {
			t.Fatalf("%s: %s", tc.ua, text)
		}
	}
}

// The hooks Wakeshift's installer wrote (no plugin) tell the model their
// mark too, from their session-context hook: the first subscribe of a new
// conversation asks nothing and offers no plugin on top of them, and once
// they've connected the conversation, no result offers it either.
func TestClaudeCodeHooksInstallMark(t *testing.T) {
	t.Setenv("CLAUDE_CODE_SESSION_ATTENDED", "")
	c := coretest.Start(t)
	_, _ = withMarketplace(t, c)
	app := noteApp(t, c, "ci")
	mark := sessionMark(t, c, false)
	claim, m, ok := pluginmark.Split(mark)
	if !ok || claim != "claude-code" || !m.Hooks || !m.TrustsIssuer(c.URL) {
		t.Fatalf("mark: %q", mark)
	}
	const session = "5e551011-0000-4000-8000-000000000001"
	call := caller{User: "hk", Client: "claude-code", UserAgent: "claude-code/2.1.280 (cli)", Meta: map[string]any{"claudecode/toolUseId": "toolu_h1"}}.WithHarness(mark)
	sub, err := subscribe(app, call, "ci/acme/web", subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	noOffer := func(name, text string) {
		t.Helper()
		for _, no := range []string{"claude plugin install", "claude plugin marketplace", "AskUserQuestion", "Monitor tool", "curl", "/install/", "Notify me in every session"} {
			if strings.Contains(text, no) {
				t.Fatalf("%s says %q: %s", name, no, text)
			}
		}
	}
	noOffer("the marked subscribe", sub.Addition.Text)
	if !strings.HasPrefix(sub.Addition.Text, "Subscribed. Wakeshift's hooks for Claude Code are installed here") || !strings.Contains(sub.Addition.Text, "they connect this conversation when this turn ends") {
		t.Fatalf("text: %s", sub.Addition.Text)
	}
	// The hooks connect it from the result's beacon.
	tok, _ := sub.Addition.Meta[beacon.MetaKey].(string)
	claims, err := beacon.VerifyBound(tok, beacon.Binding{Conversation: session, Call: "toolu_h1"}, []string{c.URL}, coreKeys(t, c), time.Now())
	if err != nil {
		t.Fatalf("beacon: %v", err)
	}
	code, out := redeemTicket(t, claims.Redeem, claims.Ticket, session)
	if code != 200 {
		t.Fatalf("redeem: %d %v", code, out)
	}
	waiterPolls(t, c, out) // the hooks deliver once their waiter listens
	// Hooks-connected: later subscribes, marked or not, offer nothing.
	for i, cl := range []caller{call, {User: "hk", Client: "claude-code", UserAgent: "claude-code/2.1.280 (cli)"}} {
		cl.Session = sub.Session
		cl.Meta = map[string]any{"claudecode/toolUseId": "toolu_h2" + string(rune('a'+i))}
		again, err := subscribe(app, cl, "ci/acme/api"+string(rune('a'+i)), subOpts{})
		if err != nil {
			t.Fatal(err)
		}
		noOffer("a hooks-connected subscribe", again.Addition.Text)
		if !strings.Contains(again.Addition.Text, "Wakeshift's plugin for Claude Code") && !strings.Contains(again.Addition.Text, harnesses.Connected) {
			t.Fatalf("connected: %s", again.Addition.Text)
		}
	}
}
