package coretest_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/wakeshift/server/internal/coretest"
	"github.com/wakeshift/server/pkg/wakeshift"
)

// What a call that passes a forged or unknown session_key gets. The key is
// a selector inside the user's workspace when the call names a user, and a
// bearer for the conversation's own workspace when it doesn't; either way
// it never binds the call to a conversation it doesn't own, never reads
// another conversation's held messages and never ends its subscriptions.
// A result that would otherwise say nothing says the key isn't known.

const unknownKey = "session_key you passed isn't known"

// victim is a conversation with a subscription and a message held for it.
type victim struct {
	call caller
	sub  noteSub
}

// victims subscribes one conversation per client (Claude Code, Codex, an
// unknown client), with and without a user, to ci/acme, and holds a
// message for each.
func victims(t *testing.T, c *coretest.Core, app *wakeshift.Client) map[string]victim {
	t.Helper()
	out := map[string]victim{}
	for _, user := range []string{"ada", ""} {
		for _, client := range []string{"claude-code", "codex-mcp-client", "acme-agent"} {
			v := caller{User: user, Client: client}
			if client == "codex-mcp-client" {
				v.Meta = codexMeta("victim-" + user)
			}
			s, err := subscribe(app, v, "ci/acme", subOpts{})
			if err != nil {
				t.Fatal(err)
			}
			v.Session = s.Session
			out[client+"/"+user] = victim{v, s}
		}
	}
	if p, err := emitNote(app, []string{"ci/acme"}, note{Text: "secret for the victims"}); err != nil || p.Matched != len(out) {
		t.Fatalf("emit: %+v %v", p, err)
	}
	return out
}

// checkVictims: every victim's subscription is live and its held message
// is still waiting for its own listener: nothing anyone called handed it
// over (no result does, since round 2: messages wait for a listener).
// read names victims whose key's bearer acted as them, by design.
func checkVictims(t *testing.T, c *coretest.Core, app *wakeshift.Client, vs map[string]victim, read map[string]bool) {
	t.Helper()
	for name, v := range vs {
		if !noteLive(t, c, v.sub.ID) {
			t.Fatalf("%s's subscription ended", name)
		}
		tenant, sub := topicSubRow(t, c, v.sub.ID)
		waitFor(t, name+"'s held message", func() bool {
			return count(t, c, tenant, `SELECT count(*) FROM deliveries WHERE subscription_id = $1 AND state = 'pending'`, sub) == 1
		})
		if n, _ := annotate(app, v.call); strings.Contains(n.Text, "secret for the victims") {
			t.Fatalf("%s's own result handed its message over: %q", name, n.Text)
		}
	}
}

// unsubscribeNote is an unsubscribe's ended and the note it adds.
func unsubscribeNote(t *testing.T, app *wakeshift.Client, call caller, topic string) (bool, string, error) {
	t.Helper()
	parts := strings.Split(topic, "/")
	args := map[string]any{"a": parts[0], "b": parts[1]}
	var out struct {
		Ended    bool `json:"ended"`
		Addition struct {
			Text string `json:"text"`
		} `json:"addition"`
	}
	err := coretest.RawApp(app, "POST", "/api/apps/v1/event-subscriptions/unsubscribe", map[string]any{"call": call, "name": "note.two", "arguments": args}, &out)
	return out.Ended, out.Addition.Text, err
}

// A well-formed (or malformed) key that names no conversation, from Claude
// Code, Codex or an unknown client, with or without a user: ordinary
// results and unsubscribes say it isn't known and touch nothing;
// subscribing starts a new conversation, never one named by the key. A
// Codex thread with a user is found by its thread, so its key is ignored.
func TestUnknownSessionKeyNeverBinds(t *testing.T) {
	c := coretest.Start(t)
	app := noteApp(t, c, "ci")
	vs := victims(t, c, app)
	known := map[string]bool{}
	for _, v := range vs {
		known[v.sub.Session] = true
	}
	forged := []string{"wsc_" + strings.Repeat("a", 26), "wsc_" + strings.Repeat("A", 22), "wsc_", "claude-code#1", "codex#1", "acme-agent@conversation", "hello world"}
	i := 0
	for _, user := range []string{"ada", "bob", ""} {
		for _, client := range []string{"claude-code", "codex-mcp-client", "acme-agent"} {
			for _, key := range forged {
				i++
				g := caller{User: user, Client: client, Session: key}
				byThread := false
				if client == "codex-mcp-client" {
					g.Meta = codexMeta(fmt.Sprintf("attacker-%d", i))
					byThread = user != ""
				}
				name := fmt.Sprintf("%s/%s with %q", client, user, key)
				n, err := annotate(app, g)
				if err != nil || strings.Contains(n.Text, "secret") {
					t.Fatalf("%s: annotate: %+v %v", name, n, err)
				}
				if said := strings.Contains(n.Text, unknownKey); said == byThread {
					t.Fatalf("%s: annotate says %q", name, n.Text)
				}
				ended, msg, err := unsubscribeNote(t, app, g, "ci/acme")
				if err != nil || ended {
					t.Fatalf("%s: unsubscribe: %v %v", name, ended, err)
				}
				if said := strings.Contains(msg, unknownKey); said == byThread {
					t.Fatalf("%s: unsubscribe says %q", name, msg)
				}
				s, err := subscribe(app, g, "ci/acme", subOpts{})
				if err != nil || s.Session == key || known[s.Session] || strings.Contains(s.Addition.Text, "secret") {
					t.Fatalf("%s: subscribe: %+v %v", name, s, err)
				}
				if said := strings.Contains(s.Addition.Text, unknownKey); said == byThread {
					t.Fatalf("%s: subscribe says %q", name, s.Addition.Text)
				}
				known[s.Session] = true
			}
		}
	}
	checkVictims(t, c, app, vs, nil)
}

// Another conversation's real key. Another user's is unknown in the
// caller's own workspace (it can't learn the key exists); with no user, a
// user's key names nothing either; another client's in the same workspace
// is refused (other_client), its ordinary results saying nothing. With no
// user the key is the conversation's bearer: the same client holding it is
// that conversation, by design.
func TestAnotherConversationsSessionKey(t *testing.T) {
	c := coretest.Start(t)
	app := noteApp(t, c, "ci")
	vs := victims(t, c, app)
	clients := []string{"claude-code", "codex-mcp-client", "acme-agent"}
	read := map[string]bool{}
	i := 0
	for vname, v := range vs {
		for _, user := range []string{"ada", "bob", ""} {
			for _, client := range clients {
				i++
				g := caller{User: user, Client: client, Session: v.sub.Session}
				if client == "codex-mcp-client" {
					g.Meta = codexMeta(fmt.Sprintf("other-%d", i))
				}
				name := fmt.Sprintf("%s's key from %s/%s", vname, client, user)
				owner := user == v.call.User && client == v.call.Client
				sameWorkspace := user == v.call.User && (user == "" || client != "codex-mcp-client")
				switch {
				case owner && user == "":
					// The bearer: it is that conversation.
					s, err := subscribe(app, g, "ci/acme", subOpts{})
					if err != nil || s.Session != v.sub.Session {
						t.Fatalf("%s: the bearer: %+v %v", name, s, err)
					}
					read[vname] = true
					continue
				case owner:
					continue // the same user's own conversation: the key selects it, by design
				case sameWorkspace:
					// Another client's conversation in the same workspace: refused.
					if _, err := subscribe(app, g, "ci/acme", subOpts{}); !wakeshift.IsCode(err, "other_client") {
						t.Fatalf("%s: subscribe: %v", name, err)
					}
					if _, _, err := unsubscribeNote(t, app, g, "ci/acme"); !wakeshift.IsCode(err, "other_client") {
						t.Fatalf("%s: unsubscribe: %v", name, err)
					}
					if n, err := annotate(app, g); err != nil || n.Text != "" {
						t.Fatalf("%s: annotate: %+v %v", name, n, err)
					}
					continue
				}
				byThread := client == "codex-mcp-client" && user != ""
				n, err := annotate(app, g)
				if err != nil || strings.Contains(n.Text, "secret") || strings.Contains(n.Text, unknownKey) == byThread {
					t.Fatalf("%s: annotate: %+v %v", name, n, err)
				}
				if ended, msg, err := unsubscribeNote(t, app, g, "ci/acme"); err != nil || ended || strings.Contains(msg, unknownKey) == byThread {
					t.Fatalf("%s: unsubscribe: %v %q %v", name, ended, msg, err)
				}
				s, err := subscribe(app, g, "ci/acme", subOpts{})
				if err != nil || s.Session == v.sub.Session || strings.Contains(s.Addition.Text, "secret") || strings.Contains(s.Addition.Text, unknownKey) == byThread {
					t.Fatalf("%s: subscribe: %+v %v", name, s, err)
				}
				if tenant, _ := topicSubRow(t, c, s.ID); tenant == workspaceOfSub(t, c, v.sub) && user != v.call.User {
					t.Fatalf("%s: its subscription landed in the victim's workspace", name)
				}
			}
		}
	}
	checkVictims(t, c, app, vs, read)
}

func workspaceOfSub(t *testing.T, c *coretest.Core, s noteSub) string {
	t.Helper()
	tenant, _ := topicSubRow(t, c, s.ID)
	return tenant
}

// A key from another app names nothing at this one, with or without a
// user: a new conversation in this app's own workspace.
func TestAnotherAppsSessionKey(t *testing.T) {
	c := coretest.Start(t)
	app := noteApp(t, c, "ci")
	other := noteApp(t, c, "other")
	vs := victims(t, c, app)
	for vname, v := range vs {
		for _, user := range []string{"ada", ""} {
			g := v.call
			g.User = user
			if g.Meta != nil {
				g.Meta = codexMeta("elsewhere-" + vname)
			}
			byThread := g.Meta != nil && user != ""
			n, err := annotate(other, g)
			if err != nil || strings.Contains(n.Text, "secret") || strings.Contains(n.Text, unknownKey) == byThread {
				t.Fatalf("%s's key at another app (user %q): annotate: %+v %v", vname, user, n, err)
			}
			s, err := subscribe(other, g, "ci/acme", subOpts{})
			if err != nil || s.Session == v.sub.Session || strings.Contains(s.Addition.Text, unknownKey) == byThread {
				t.Fatalf("%s's key at another app (user %q): subscribe: %+v %v", vname, user, s, err)
			}
			if tenant, _ := topicSubRow(t, c, s.ID); tenant == workspaceOfSub(t, c, v.sub) {
				t.Fatalf("%s's key at another app: landed in the victim's workspace", vname)
			}
		}
	}
	checkVictims(t, c, app, vs, nil)
}

// A key over 100 characters is refused as a bad request; one with spaces
// around it is the same key.
func TestMalformedSessionKey(t *testing.T) {
	c := coretest.Start(t)
	app := noteApp(t, c, "ci")
	for _, user := range []string{"ada", ""} {
		own, err := subscribe(app, caller{User: user, Client: "claude-code"}, "ci/acme", subOpts{})
		if err != nil {
			t.Fatal(err)
		}
		long := caller{User: user, Client: "claude-code", Session: own.Session + strings.Repeat("x", 101)}
		if _, err := subscribe(app, long, "ci/acme", subOpts{}); !wakeshift.IsCode(err, "bad_request") {
			t.Fatalf("user %q: a long key's subscribe: %v", user, err)
		}
		if _, err := annotate(app, long); !wakeshift.IsCode(err, "bad_request") {
			t.Fatalf("user %q: a long key's annotate: %v", user, err)
		}
		if _, _, err := unsubscribeNote(t, app, long, "ci/acme"); !wakeshift.IsCode(err, "bad_request") {
			t.Fatalf("user %q: a long key's unsubscribe: %v", user, err)
		}
		if code, e := appAPI(t, c, app, "/api/apps/v1/calls/session", map[string]any{"call": long}, nil); code != 400 || e != "bad_request" {
			t.Fatalf("user %q: a long key's calls/session: %d %s", user, code, e)
		}
		padded := caller{User: user, Client: "claude-code", Session: " " + own.Session + "\n"}
		if s, err := subscribe(app, padded, "ci/acme", subOpts{}); err != nil || s.Session != own.Session {
			t.Fatalf("user %q: a padded key: %+v %v", user, s, err)
		}
	}
}

// /calls/session (resolveSession) with a key that names nothing starts a
// new conversation, and the call's block says the key wasn't known.
func TestCallsSessionUnknownKey(t *testing.T) {
	c := coretest.Start(t)
	app := noteApp(t, c, "ci")
	for _, user := range []string{"ada", ""} {
		for _, key := range []string{"wsc_" + strings.Repeat("a", 26), "wsc_" + strings.Repeat("A", 22), "nonsense"} {
			call := caller{User: user, Client: "claude-code", Session: key}
			var s struct{ Session, Pending string }
			if code, e := appAPI(t, c, app, "/api/apps/v1/calls/session", map[string]any{"call": call}, &s); code != 200 || s.Session == key || s.Session == "" {
				t.Fatalf("user %q, key %q: %d %s %+v", user, key, code, e, s)
			}
			call.Session = s.Session
			var block additionOut
			if code, e := appAPI(t, c, app, "/api/apps/v1/calls/addition", map[string]any{"call": call, "pending": s.Pending}, &block); code != 200 || !strings.Contains(block.Text, unknownKey) {
				t.Fatalf("user %q, key %q: block %d %s %q", user, key, code, e, block.Text)
			}
		}
	}
}

// An app acting for one of its agents: a handle that isn't one of the
// token's agent's sessions (made up, or another workspace's agent's) is
// refused with unknown_session where the call would use it (subscribe,
// /calls/session), and said to be unknown where it would say nothing
// (an ordinary result, an unsubscribe).
func TestAgentTokenUnknownSessionKey(t *testing.T) {
	c := coretest.Start(t)
	app, ada := agentsApp(t, c, "chat")
	bob := actAs(t, app, "bob", "codex")
	adaTok, bobTok := agentToken(t, c, app, ada), agentToken(t, c, app, bob)
	var sub deferredOut
	if code, e := appAPI(t, c, app, "/api/apps/v1/event-subscriptions", map[string]any{"call": map[string]any{"agent_token": adaTok, "harness": "claude-code", "tool": "watch"},
		"name": "game.turn", "arguments": map[string]any{"table": "juniper"}, "label": "main"}, &sub); code != 200 {
		t.Fatalf("ada's agent subscribes: %d %s", code, e)
	}
	for _, key := range []string{sub.Session, "codex@nope", "wsc_" + strings.Repeat("a", 26), "nonsense"} {
		call := map[string]any{"agent_token": bobTok, "harness": "claude-code", "tool": "join", "session": key}
		if code, e := appAPI(t, c, app, "/api/apps/v1/event-subscriptions", map[string]any{"call": call, "name": "game.turn", "arguments": map[string]any{"table": "juniper"}}, nil); code != 404 || e != "unknown_session" {
			t.Fatalf("subscribe with %q: %d %s", key, code, e)
		}
		if code, e := appAPI(t, c, app, "/api/apps/v1/calls/session", map[string]any{"call": call}, nil); code != 404 || e != "unknown_session" {
			t.Fatalf("calls/session with %q: %d %s", key, code, e)
		}
		var block additionOut
		if code, e := appAPI(t, c, app, "/api/apps/v1/calls/addition", map[string]any{"call": call}, &block); code != 200 || block.Text != "The "+unknownKey+" (it may have ended)." {
			t.Fatalf("addition with %q: %d %s %q", key, code, e, block.Text)
		}
		var un struct {
			Ended    bool
			Addition struct{ Text string }
		}
		if code, e := appAPI(t, c, app, "/api/apps/v1/event-subscriptions/unsubscribe", map[string]any{"call": call, "name": "game.turn", "arguments": map[string]any{"table": "juniper"}}, &un); code != 200 || un.Ended || !strings.Contains(un.Addition.Text, unknownKey) {
			t.Fatalf("unsubscribe with %q: %d %s %+v", key, code, e, un)
		}
	}
	if !noteLive(t, c, sub.EventSubscription.ID) {
		t.Fatal("ada's agent's subscription ended")
	}
}
