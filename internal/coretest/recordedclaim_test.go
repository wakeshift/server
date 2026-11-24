package coretest_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/wakeshift/server/internal/beacon"
	"github.com/wakeshift/server/internal/coretest"
)

// An agent says its harness on the call that subscribes; a later result of
// a tool that subscribes to nothing, without the claim, still takes the
// harness's own path when only the claim named it (no client name, or only
// one others send too): its session's recorded harness stands in for the
// claim. Once Hermes Agent's or OpenClaw's plugin has connected the
// conversation, its new messages go to the plugin's stream, so another
// result doesn't hand them over too, where the generic path would.
func TestRecordedClaimPicksTheResultPath(t *testing.T) {
	c := coretest.Start(t)
	app := noteApp(t, c, "ci")
	keys := coreKeys(t, c)
	codexMeta := map[string]any{"x-codex-turn-metadata": map[string]any{"thread_id": "01a155a9-3a11-7830-aa96-233228455669", "thread_source": "openclaw"}}
	for i, tc := range []struct {
		name string
		call caller
		want string
	}{
		{"hermes on the shared mcp client", caller{User: "h1", Client: "mcp", Harness: "hermes"}, "hermes"},
		{"hermes, no client name", caller{User: "h2", Harness: "hermes"}, "hermes"},
		{"openclaw, no client name", caller{User: "o1", Harness: "openclaw"}, "openclaw"},
		{"openclaw in codex", caller{User: "o2", Client: "codex-mcp-client", Meta: codexMeta, Harness: "openclaw"}, "openclaw"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			topic := fmt.Sprintf("ci/acme/web%d", i)
			sub, err := subscribe(app, tc.call, topic, subOpts{})
			if err != nil {
				t.Fatal(err)
			}
			if got := sub.Addition.Agent.Harness; got != tc.want {
				t.Fatalf("subscribe: agent %+v", sub.Addition.Agent)
			}
			later := tc.call
			later.Session, later.Harness = sub.Session, ""

			// Not connected yet: the same path as with the claim.
			r, err := annotate(app, later)
			if err != nil {
				t.Fatal(err)
			}
			if r.Agent.Harness != tc.want || r.Agent.Claimed != "" {
				t.Fatalf("annotate without the claim: agent %+v", r.Agent)
			}

			// The plugin connects the conversation from the subscribe's beacon.
			tok, _ := sub.Addition.Meta[beacon.MetaKey].(string)
			if tok == "" {
				tok = beacon.InText(sub.Addition.Text)
			}
			cl, err := beacon.VerifyBound(tok, beacon.Binding{Conversation: "x", Unbound: true}, []string{c.URL}, keys, time.Now())
			if err != nil {
				t.Fatalf("beacon: %v", err)
			}
			if code, out := redeemTicket(t, cl.Redeem, cl.Ticket, fmt.Sprintf("%s/0123456789abcdef/agent:main:%d", tc.want, i)); code != 200 {
				t.Fatalf("redeem: %d %v", code, out)
			}
			if _, err := emitNote(app, []string{topic}, note{Text: "deploy for " + tc.call.User}); err != nil {
				t.Fatal(err)
			}
			r, err = annotate(app, later)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(r.Text, "deploy for") {
				t.Fatalf("connected, annotate without the claim handed over the plugin's message: %s", r.Text)
			}
			if r.Agent.Harness != tc.want {
				t.Fatalf("connected, annotate without the claim: agent %+v", r.Agent)
			}
		})
	}

	// An unknown client on the shared "mcp" name that said it's Cursor
	// stays Cursor, rather than becoming Hermes Agent (and its beacon).
	sub, err := subscribe(app, caller{User: "u1", Client: "mcp", Harness: "cursor"}, "ci/acme/web", subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	r, err := annotate(app, caller{User: "u1", Client: "mcp", Session: sub.Session})
	if err != nil || r.Agent.Harness != "cursor" || r.Meta[beacon.MetaKey] != nil {
		t.Fatalf("cursor on mcp: %+v %v", r, err)
	}
}

// The recorded harness is the conversation's own: another conversation of
// the same user and client, another user, or another client passing the
// key gets nothing of it. A call its client name already names keeps that
// harness, and a call that claims one keeps its claim.
func TestRecordedClaimStaysInItsConversation(t *testing.T) {
	c := coretest.Start(t)
	app := noteApp(t, c, "ci")
	const topic = "ci/acme/web"
	// A conversation the record doesn't reach is recognized as nothing,
	// as before it: no harness, no beacon.
	generic := func(t *testing.T, what string, r coretest.Addition) {
		t.Helper()
		if r.Agent.Harness != "" || r.Agent.Via != "" || r.Meta[beacon.MetaKey] != nil || beacon.InText(r.Text) != "" {
			t.Fatalf("%s: agent %+v, meta %v: %s", what, r.Agent, r.Meta, r.Text)
		}
	}

	// One user, two conversations of one client: only the one that said
	// OpenClaw is OpenClaw's.
	a, err := subscribe(app, caller{User: "ada", Harness: "openclaw"}, topic, subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	b, err := subscribe(app, caller{User: "ada"}, topic, subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if a.Session == b.Session {
		t.Fatal("one session for two conversations")
	}
	r, _ := annotate(app, caller{User: "ada", Session: b.Session})
	generic(t, "ada's other conversation", r)
	if r, _ := annotate(app, caller{User: "ada", Session: a.Session}); r.Agent.Harness != "openclaw" {
		t.Fatalf("ada's openclaw conversation: %+v", r.Agent)
	}

	// Another user with the same client gets its own (generic) path.
	if _, err := subscribe(app, caller{User: "bob"}, topic, subOpts{}); err != nil {
		t.Fatal(err)
	}
	bobSub, _ := subscribe(app, caller{User: "bob"}, topic, subOpts{})
	r, _ = annotate(app, caller{User: "bob", Session: bobSub.Session})
	generic(t, "bob", r)

	// Another user, or another client, passing ada's key finds nothing:
	// no harness, only that the key isn't known (#927), and her messages
	// stay hers.
	for _, other := range []caller{
		{User: "bob", Session: a.Session},
		{User: "ada", Client: "other-client", Session: a.Session},
	} {
		r, err := annotate(app, other)
		if err != nil || r.Agent.Harness != "" || r.Text != "" && r.Text != "The session_key you passed isn't known (it may have ended)." || r.Meta != nil {
			t.Fatalf("%+v with ada's key: %+v %v", other, r, err)
		}
	}
	// No key at all names no conversation, even with one recorded for the
	// user: a user's conversations of one client are told apart only by
	// key (or conversation ID), never by which was last.
	if r, err := annotate(app, caller{User: "ada"}); err != nil || r.Agent.Harness != "" || r.Text != "" {
		t.Fatalf("ada without a key: %+v %v", r, err)
	}

	// A claim in the call wins over the record (and replaces it, as on
	// any call: the agent may correct itself).
	if r, _ := annotate(app, caller{User: "ada", Session: a.Session, Harness: "hermes"}); r.Agent.Harness != "hermes" || r.Agent.Claimed != "hermes" {
		t.Fatalf("claim over the record: %+v", r.Agent)
	}

	// A call its client name names keeps its harness: a record can't move
	// Claude Code's calls elsewhere.
	cc, err := subscribe(app, caller{User: "cy", Client: "claude-code", Harness: "openclaw"}, topic, subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if r, _ := annotate(app, caller{User: "cy", Client: "claude-code", Session: cc.Session}); r.Agent.Harness != "claude-code" {
		t.Fatalf("claude-code: %+v", r.Agent)
	}
}

// Cursor and Grok Bot name their conversation by conversation_id, and only
// on a call that passes it (or the session key): a call of the same user
// and client that passes neither finds no conversation and gets nothing,
// rather than the user's most recent conversation's messages or command.
func TestKeylessCursorFindsNothing(t *testing.T) {
	c := coretest.Start(t)
	app := noteApp(t, c, "ci")
	const topic = "ci/acme/web"
	for _, call := range []caller{
		{User: "cu", Client: "cursor-vscode", Conversation: "c-cursor-1"},
		{User: "gb", Harness: "grok-bot", Conversation: "c-grok-1"},
	} {
		if _, err := subscribe(app, call, topic, subOpts{}); err != nil {
			t.Fatal(err)
		}
		if _, err := emitNote(app, []string{topic}, note{Text: "for " + call.User}); err != nil {
			t.Fatal(err)
		}
		bare := caller{User: call.User, Client: call.Client}
		if r, err := annotate(app, bare); err != nil || r.Text != "" || r.Meta != nil {
			t.Fatalf("%+v without key or conversation: %+v %v", bare, r, err)
		}
	}
}
