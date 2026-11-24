package coretest_test

import (
	"strings"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/wakeshift/server/internal/beacon"
	"github.com/wakeshift/server/internal/core"
	"github.com/wakeshift/server/internal/coretest"
	"github.com/wakeshift/server/internal/telemetry"
)

// TestStreamsWithTracing: with tracing on, the handler is wrapped, and
// streams still deliver: Codex's WebSocket (a hijacked connection) and
// Claude Code's Monitor lines (flushes and write deadlines through
// http.ResponseController).
func TestStreamsWithTracing(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(telemetry.Scrub(exp)))
	c := coretest.StartWith(t, func(o *core.Options) { o.Tracing = tp })
	app := noteApp(t, c, "ci")
	const thread = "019a14bd-bbbc-7b70-a723-0abb9f38118e"
	codexCall := caller{User: "ada", Client: "codex-mcp-client", Meta: map[string]any{"x-codex-turn-metadata": map[string]any{"thread_id": thread}}}
	claudeCall := caller{User: "bob", Client: "claude-code"}

	codexSub, err := subscribe(app, codexCall, "ci/acme/api", subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	tok, _ := codexSub.Addition.Meta[beacon.MetaKey].(string)
	claims, err := beacon.Verify(tok, thread, []string{c.URL}, coreKeys(t, c), time.Now())
	if err != nil {
		t.Fatalf("beacon: %v", err)
	}
	code, redeemed := redeemTicket(t, claims.Redeem, claims.Ticket, thread)
	if code != 200 {
		t.Fatalf("redeem: %d %v", code, redeemed)
	}
	codex := dialStream(t, redeemed["url"].(string), true)

	claudeSub, err := subscribe(app, claudeCall, "ci/acme/api", subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	mon := monitorRE.FindStringSubmatch(claudeSub.Addition.Text)
	if mon == nil {
		t.Fatalf("claude code result: %+v", claudeSub)
	}
	claude := readLines(t, mon[1])
	waitFor(t, "connected", func() bool {
		s, _ := subscribe(app, caller{User: "bob", Client: "claude-code", Session: claudeSub.Session}, "ci/acme/api", subOpts{})
		return strings.Contains(s.Addition.Text, "already running")
	})

	if p, err := emitNote(app, []string{"ci/acme/api"}, note{Text: "CI failed on main: build"}); err != nil || p.Matched != 2 {
		t.Fatalf("publish: %+v %v", p, err)
	}
	codex.expect("CI failed on main: build")
	claude.expect("CI failed on main: build")
	if len(exp.GetSpans()) == 0 {
		t.Fatal("no spans: tracing wasn't on")
	}
}
