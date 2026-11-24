package coretest_test

import (
	"strings"
	"testing"
	"time"

	"github.com/wakeshift/server/internal/beacon"
	"github.com/wakeshift/server/internal/coretest"
	"github.com/wakeshift/server/internal/harnesses"
)

// #1272: a Claude Code process that redeems and has no waiter (claude -p,
// a one-shot Agent SDK query, or an old plugin in an SDK host) isn't
// counted as delivering: the conversation's next subscribe isn't told the
// hooks deliver (no "you don't need a Monitor"), and its wakes stays
// pending, until the hooks' waiter polls their link. Then both say so.
func TestClaudeCodeRedeemAloneIsNoListener(t *testing.T) {
	c := coretest.Start(t)
	app := noteApp(t, c, "ci")
	const conv = "5e551011-0000-4000-8000-0000000012f2"
	call := caller{User: "bo", Client: "claude-code", Meta: map[string]any{"claudecode/toolUseId": "toolu_1"}}
	sub, err := subscribe(app, call, "ci/acme/web", subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	tok, _ := sub.Addition.Meta[beacon.MetaKey].(string)
	claims, err := beacon.VerifyBound(tok, beacon.Binding{Conversation: conv, Call: "toolu_1"}, []string{c.URL}, coreKeys(t, c), time.Now())
	if err != nil {
		t.Fatalf("beacon: %v", err)
	}
	code, out := redeemTicket(t, claims.Redeem, claims.Ticket, conv)
	if code != 200 {
		t.Fatalf("redeem: %d %v", code, out)
	}

	call.Session = sub.Session
	call.Meta = map[string]any{"claudecode/toolUseId": "toolu_2"}
	alone, err := subscribe(app, call, "ci/acme/api", subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if alone.Wakes || strings.Contains(alone.Addition.Text, "hooks for Claude Code deliver") || strings.Contains(alone.Addition.Text, "You don't need a Monitor") {
		t.Fatalf("a redeem alone counted as listening: wakes %v: %s", alone.Wakes, alone.Addition.Text)
	}

	waiterPolls(t, c, out)
	call.Meta = map[string]any{"claudecode/toolUseId": "toolu_3"}
	polled, err := subscribe(app, call, "ci/acme/db", subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if !polled.Wakes || !strings.Contains(polled.Addition.Text, "hooks for Claude Code deliver") || !strings.Contains(polled.Addition.Text, harnesses.Connected) {
		t.Fatalf("once the waiter polled: wakes %v: %s", polled.Wakes, polled.Addition.Text)
	}
}
