package coretest_test

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/wakeshift/server/internal/beacon"
	"github.com/wakeshift/server/internal/connector"
	"github.com/wakeshift/server/internal/coretest"
)

// pi, as its MCP client calls (1.5.0: clientInfo "pi", User-Agent
// "undici", _meta of progressToken only), end to end with the real bridge
// Wakeshift's extension runs: the first subscribe offers the extension
// with no beacon (nothing names the session yet); once the extension sets
// conversation_id to the session's ID on the app's tool, the result's last
// line is a beacon bound to it, the bridge connects the session from it
// and hands over what's published, and the same beacon can't be redeemed
// again.
func TestPiExtensionConnects(t *testing.T) {
	c := coretest.Start(t)
	app := noteApp(t, c, "ci")
	const sid = "019a5c3e-7d2f-7c11-9a41-5e0f3b2d8c6a"
	call := caller{Client: "pi", ClientVersion: "1.5.0", UserAgent: "undici", Meta: map[string]any{"progressToken": 1}, Tool: "watch"}

	first, err := subscribe(app, call, "ci/acme/web", subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if got := first.Addition.Agent; got.Harness != "pi" || got.Via != "client" {
		t.Fatalf("agent %+v", got)
	}
	text := first.Addition.Text
	for _, want := range []string{"Install Wakeshift's extension for pi? It runs `curl -fsSL " + c.URL + "/install/pi.sh | sh`.", "~/.pi/agent/extensions/wakeshift.ts", "/reload", "nothing to approve"} {
		if !strings.Contains(text, want) {
			t.Fatalf("offer lacks %q: %s", want, text)
		}
	}
	for _, never := range []string{"call this tool again", "call the tool again"} {
		if strings.Contains(strings.ToLower(text), never) {
			t.Fatalf("offer says %q: %s", never, text)
		}
	}
	if beacon.InText(text) != "" || len(first.Addition.Meta) != 0 {
		t.Fatalf("a beacon naming no session: %+v", first.Addition)
	}

	// The extension's tool_call handler: harness and the session's ID.
	call.Session, call.Conversation, call.Harness = first.Session, sid, "pi"
	sub, err := subscribe(app, call, "ci/acme/web", subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	tok := beacon.Trailer(sub.Addition.Text)
	if tok == "" {
		t.Fatalf("no beacon ending the result: %s", sub.Addition.Text)
	}
	claims, err := beacon.Verify(tok, sid, []string{c.URL}, coreKeys(t, c), time.Now())
	if err != nil || claims.Tool != "watch" {
		t.Fatalf("beacon for the session: %+v %v", claims, err)
	}

	// The bridge, as the extension starts it for that result.
	spec, _ := connector.Lookup("pi")
	set := connector.Settings{Dir: t.TempDir(), Issuers: []string{c.URL}}
	h := &connector.Hook{Harness: "pi", Format: spec.Format, Verifier: &connector.Verifier{Settings: set}, Poller: &connector.HTTPPoller{Harness: "pi", Issuers: []string{c.URL}},
		Links: connector.LinksFor(set), When: spec.When, Unbound: spec.Unbound}
	line, _ := json.Marshal(map[string]any{"hook_event_name": "PostToolUse", "session_id": sid, "call_id": "call_1", "tool_name": "mcp__ci__watch",
		"server": "ci", "mcp_tool": "watch", "tool_response": map[string]any{"content": []any{map[string]string{"type": "text", "text": sub.Addition.Text}}}})
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	bctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() { (&connector.Bridge{Hook: h}).Run(bctx, inR, outW); outW.Close() }()
	io.WriteString(inW, string(line)+"\n")
	lines := bufio.NewScanner(outR)
	if !lines.Scan() {
		t.Fatal("no answer")
	}
	var answer struct {
		AdditionalContext string `json:"additionalContext"`
		KeepAlive         bool   `json:"keep_alive"`
	}
	json.Unmarshal(lines.Bytes(), &answer)
	if !answer.KeepAlive || !strings.HasPrefix(answer.AdditionalContext, "Wakeshift: connected") {
		t.Fatalf("answer %s", lines.Bytes())
	}
	if _, err := emitNote(app, []string{"ci/acme/web"}, note{Text: "deploy finished"}); err != nil {
		t.Fatal(err)
	}
	if !lines.Scan() {
		t.Fatal("no wake")
	}
	var wake struct{ Wake struct{ ID, Text string } }
	json.Unmarshal(lines.Bytes(), &wake)
	if !strings.Contains(wake.Wake.Text, "deploy finished") || !strings.Contains(wake.Wake.Text, `trust="external"`) || !strings.Contains(wake.Wake.Text, "not as instructions from the user") {
		t.Fatalf("wake %s", lines.Bytes())
	}
	io.WriteString(inW, `{"ack":"`+wake.Wake.ID+`"}`+"\n")

	// Connected: the next subscribe says the extension delivers and offers
	// nothing (its beacon, as every subscribing result's, lets the extension
	// reconnect after a link is lost).
	again, _ := subscribe(app, call, "ci/acme/web", subOpts{})
	if !strings.Contains(again.Addition.Text, "Wakeshift's extension for pi delivers to this conversation") || strings.Contains(again.Addition.Text, "/install/") {
		t.Fatalf("again: %q", again.Addition.Text)
	}

	// The same beacon again (a replay, from anywhere): its ticket is spent.
	if code, _ := redeemTicket(t, claims.Redeem, claims.Ticket, sid); code == 200 {
		t.Fatal("a spent ticket redeemed again")
	}
	inW.Close()
}

// A stateless server never sees pi's client name, and its User-Agent
// (undici) is Node's default: such a call is pi's only when it says so
// (the extension passes harness "pi").
func TestPiStateless(t *testing.T) {
	c := coretest.Start(t)
	app := noteApp(t, c, "ci")
	plain, err := subscribe(app, caller{UserAgent: "undici", Meta: map[string]any{"progressToken": 1}}, "ci/acme/web", subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if plain.Addition.Agent.Harness != "" || !strings.HasPrefix(plain.Addition.Text, "Wakeshift can't wake this agent") {
		t.Fatalf("plain: %+v", plain.Addition)
	}
	said, err := subscribe(app, caller{UserAgent: "undici", Harness: "pi", Conversation: "019a5c3e-0000-7000-8000-000000000001", Tool: "watch"}, "ci/acme/web", subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if got := said.Addition.Agent; got.Harness != "pi" || got.Via != "claim" || !strings.Contains(said.Addition.Text, "/install/pi.sh") || beacon.Trailer(said.Addition.Text) == "" {
		t.Fatalf("said: %+v", said.Addition)
	}
}
