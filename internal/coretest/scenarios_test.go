// End-to-end scenarios ported from the spike (internal/hub/*_test.go on the
// spike branch) that the feature tests don't already cover: each runs a
// whole core, with real HTTP and the app API, and checks
// what users, agents and apps would see.
package coretest_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/apps/agentkit"
	"github.com/wakeshift/server/internal/beacon"
	"github.com/wakeshift/server/internal/coretest"
	"github.com/wakeshift/server/pkg/wakeshift"
)

var ctx = context.Background()

const chatMCP = "https://chat.test/mcp"

// chatApp registers an Agent Chat-like app: it acts for its agents, and
// wakes their conversations with chat.line, a personal event per channel
// that a conversation subscribes to with its agent's token (lineSub) and
// the app emits to (postLine).
func chatApp(t *testing.T, c *coretest.Core) *wakeshift.Client {
	t.Helper()
	app, _ := c.AppClient("agentchat", "Agent Chat", chatMCP)
	if err := app.PublishManifest(ctx, wakeshift.Manifest{Description: "Channels for agents", OthersCanWake: true,
		Agent: &wakeshift.AgentScope{Wake: []wakeshift.ScopeEvent{}, Post: []wakeshift.ScopeEvent{}, TTLDays: 90},
		Events: []wakeshift.ManifestEvent{{Name: "chat.line", Description: "A line in a channel.", Topic: "lines/{channel}", Personal: true,
			Arguments: json.RawMessage(`{"type":"object","properties":{"channel":{"type":"string"}},"required":["channel"],"additionalProperties":false}`),
			Payload:   json.RawMessage(`{"type":"object"}`)}}}); err != nil {
		t.Fatal(err)
	}
	return app
}

// lineSub subscribes one of ag's conversations (session, its handle; ""
// for a new one) to chat.line in #general, with ag's token, as a wrapped
// tool of the app's does (no block yet), returning the subscription's ID.
func lineSub(t *testing.T, c *coretest.Core, app *wakeshift.Client, ag *wakeshift.Agent, session string) string {
	t.Helper()
	call := map[string]any{"agent_token": agentToken(t, c, app, ag), "client": "test", "tool": "join"}
	if session != "" {
		call["session"] = session
	}
	var out deferredOut
	if code, e := appAPI(t, c, app, "/api/apps/v1/event-subscriptions", map[string]any{"call": call, "name": "chat.line",
		"arguments": map[string]any{"channel": "general"}, "defer_addition": true}, &out); code != 200 {
		t.Fatalf("%s's %s subscribes: %d %s", ag.ID, session, code, e)
	}
	return out.EventSubscription.ID
}

// lineUnsub ends the chat.line subscription of one of ag's conversations.
func lineUnsub(t *testing.T, c *coretest.Core, app *wakeshift.Client, ag *wakeshift.Agent, session string) {
	t.Helper()
	call := map[string]any{"agent_token": agentToken(t, c, app, ag), "client": "test", "tool": "leave", "session": session}
	if code, e := appAPI(t, c, app, "/api/apps/v1/event-subscriptions/unsubscribe", map[string]any{"call": call, "name": "chat.line",
		"arguments": map[string]any{"channel": "general"}}, nil); code != 200 {
		t.Fatalf("%s's %s unsubscribes: %d %s", ag.ID, session, code, e)
	}
}

// postLine emits subject to every conversation of ag subscribed to
// chat.line in #general.
func postLine(t *testing.T, c *coretest.Core, app *wakeshift.Client, ag *wakeshift.Agent, subject string) {
	t.Helper()
	if code, e := appAPI(t, c, app, "/api/apps/v1/events/emit", map[string]any{"name": "chat.line", "arguments": map[string]any{"channel": "general"},
		"payload": map[string]any{}, "text": subject, "to": ag.User()}, nil); code != 200 {
		t.Fatalf("post %q: %d %s", subject, code, e)
	}
}

// actAs adds agent to the workspace app keeps for user (its own ID for
// them), and acts for it.
func actAs(t *testing.T, app *wakeshift.Client, user, agent string) *wakeshift.Agent {
	t.Helper()
	ws, err := app.Workspace(ctx, user, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.AddAgent(ctx, ws, agent, ""); err != nil {
		t.Fatal(err)
	}
	ag, err := app.ActAs(ctx, ws, agent)
	if err != nil {
		t.Fatal(err)
	}
	return ag
}

// owedTo lists the subjects delivered to one session of agent (by handle),
// or to the agent as a whole (""), once the runner has caught up.
func owedTo(t *testing.T, c *coretest.Core, workspace, agent, handle string) []string {
	t.Helper()
	var out []string
	caughtUp(t, c, workspace)
	c.DB.InTenant(ctx, workspace, func(tx pgx.Tx) error {
		rows, _ := tx.Query(ctx, `SELECT e.subject FROM deliveries d JOIN events e ON e.tenant_id = d.tenant_id AND e.id = d.event_id
			LEFT JOIN sessions s ON s.tenant_id = d.tenant_id AND s.id::text = d.target
			WHERE d.agent_id = $1 AND coalesce(s.handle, '') = $2 ORDER BY e.seq`, agent, handle)
		var err error
		out, err = pgx.CollectRows(rows, pgx.RowTo[string])
		return err
	})
	return out
}

// caughtUp waits until the runner has read every event in workspace for
// each of its active subscriptions, so what they're owed so far is in
// deliveries (a cursor moves in the transaction that makes them). A fixed
// sleep can be too short on a loaded CI runner, and read nothing.
func caughtUp(t *testing.T, c *coretest.Core, workspace string) {
	t.Helper()
	waitFor(t, "the runner to catch up", func() bool {
		var behind bool
		if err := c.DB.InTenant(ctx, workspace, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM subscription_cursors c
				JOIN subscriptions s ON s.tenant_id = c.tenant_id AND s.id = c.subscription_id
				WHERE s.status = 'active' AND c.last_seq < (SELECT coalesce(max(last_seq), 0) FROM event_counters))`).Scan(&behind)
		}); err != nil {
			t.Fatal(err)
		}
		return !behind
	})
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	// Long enough for a CI runner under load (go test -race across every
	// package); what's waited for normally comes in milliseconds.
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for " + what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func getJSON(t *testing.T, u string, out any) {
	t.Helper()
	resp, err := http.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("GET %s: %d", u, resp.StatusCode)
	}
	json.NewDecoder(resp.Body).Decode(out)
}

func postForm(t *testing.T, u string, v url.Values) (int, map[string]any) {
	t.Helper()
	resp, err := http.PostForm(u, v)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// A subscription follows the conversation that made it, and another
// conversation's subscription wakes that one too; ending one stops only its
// conversation (spike: TestDeliveryFollowsTheConversationThatJoined).
func TestDeliveryFollowsTheConversationThatJoined(t *testing.T) {
	c := coretest.Start(t)
	app := chatApp(t, c)
	codex := actAs(t, app, "ada", "codex")
	a, _ := agentkit.SessionLink(ctx, codex, agentkit.LinkRequest{Conversation: "thread-A"})
	b, _ := agentkit.SessionLink(ctx, codex, agentkit.LinkRequest{Conversation: "thread-B"})
	if again, _ := agentkit.SessionLink(ctx, codex, agentkit.LinkRequest{Conversation: "thread-A"}); again.Session != a.Session || a.Session == b.Session {
		t.Fatalf("sessions %q %q %q", a.Session, b.Session, again.Session)
	}

	lineSub(t, c, app, codex, a.Session)
	postLine(t, c, app, codex, "one")
	if got := owedTo(t, c, codex.Workspace, "codex", a.Session); !slices.Equal(got, []string{"one"}) {
		t.Fatalf("A (subscribed) got %v", got)
	}
	// B subscribes too: each conversation that subscribed is woken.
	lineSub(t, c, app, codex, b.Session)
	postLine(t, c, app, codex, "two")
	if got := owedTo(t, c, codex.Workspace, "codex", b.Session); !slices.Equal(got, []string{"two"}) {
		t.Fatalf("B (subscribed) got %v", got)
	}
	if got := owedTo(t, c, codex.Workspace, "codex", a.Session); !slices.Equal(got, []string{"one", "two"}) {
		t.Fatalf("A (still subscribed) got %v", got)
	}
	// A unsubscribes; only B is woken.
	lineUnsub(t, c, app, codex, a.Session)
	postLine(t, c, app, codex, "three")
	if got := owedTo(t, c, codex.Workspace, "codex", a.Session); !slices.Equal(got, []string{"one", "two"}) {
		t.Fatalf("A after unsubscribing got %v", got)
	}
	if got := owedTo(t, c, codex.Workspace, "codex", b.Session); !slices.Equal(got, []string{"two", "three"}) {
		t.Fatalf("B got %v", got)
	}
}

// A deleted app's agent tokens stop at once, though its grants
// stay live until its deletion reaches each workspace.
func TestDeletedAppsAgentTokensStop(t *testing.T) {
	c := coretest.Start(t)
	app := chatApp(t, c)
	claude := actAs(t, app, "ada", "claude-code")
	if _, err := agentkit.Sessions(ctx, claude); err != nil {
		t.Fatal(err)
	}
	if _, err := c.DB.Pool.Exec(ctx, `WITH o AS (INSERT INTO organizations (name) VALUES ('Acme') RETURNING id)
		UPDATE apps SET deleted_at = now(), org_id = (SELECT id FROM o) WHERE id = 'agentchat'`); err != nil {
		t.Fatal(err)
	}
	if _, err := agentkit.Sessions(ctx, claude); err == nil {
		t.Fatal("a deleted app's agent token still works")
	}
}

// agentToken is the app's token for ag, as ActAs gets it.
func agentToken(t *testing.T, c *coretest.Core, app *wakeshift.Client, ag *wakeshift.Agent) string {
	t.Helper()
	var out struct {
		AccessToken string `json:"access_token"`
	}
	if code, e := appAPI(t, c, app, "/api/apps/v1/workspaces/"+ag.Workspace+"/agents/"+ag.ID+"/token", nil, &out); code != 200 {
		t.Fatalf("%s's token: %d %s", ag.ID, code, e)
	}
	return out.AccessToken
}

// watched is what a Codex thread's tool result carries from Wakeshift:
// its text and its connector beacon ("" for none).
type watched struct {
	Text, Beacon string
}

// threadCall is a call from ag's Codex thread to tool.
func threadCall(t *testing.T, c *coretest.Core, app *wakeshift.Client, ag *wakeshift.Agent, thread, tool string) map[string]any {
	return map[string]any{"agent_token": agentToken(t, c, app, ag), "client": "codex-mcp-client", "tool": tool,
		"meta": map[string]any{"x-codex-turn-metadata": map[string]any{"thread_id": thread}}}
}

// threadWatch subscribes ag's Codex thread to chat.line in #general, as the
// app's wrapped watch tool does, returning what its result carries: a
// subscribe's result always has a fresh beacon.
func threadWatch(t *testing.T, c *coretest.Core, app *wakeshift.Client, ag *wakeshift.Agent, thread string) (watched, error) {
	t.Helper()
	var out struct {
		Addition struct {
			Text string         `json:"text"`
			Meta map[string]any `json:"meta"`
		} `json:"addition"`
	}
	if code, e := appAPI(t, c, app, "/api/apps/v1/event-subscriptions", map[string]any{"call": threadCall(t, c, app, ag, thread, "watch"),
		"name": "chat.line", "arguments": map[string]any{"channel": "general"}}, &out); code != 200 {
		t.Fatalf("%s's thread watches: %d %s", ag.ID, code, e)
	}
	b, _ := out.Addition.Meta[beacon.MetaKey].(string)
	return watched{Text: out.Addition.Text, Beacon: b}, nil
}

// threadResult is what Wakeshift adds to another tool result of the
// thread's (/calls/addition): a beacon only while it watches and its
// connector hasn't taken the last one.
func threadResult(t *testing.T, c *coretest.Core, app *wakeshift.Client, ag *wakeshift.Agent, thread string) (watched, error) {
	t.Helper()
	var out additionOut
	if code, e := appAPI(t, c, app, "/api/apps/v1/calls/addition", map[string]any{"call": threadCall(t, c, app, ag, thread, "post")}, &out); code != 200 {
		t.Fatalf("%s's thread's result: %d %s", ag.ID, code, e)
	}
	b, _ := out.Meta[beacon.MetaKey].(string)
	return watched{Text: out.Text, Beacon: b}, nil
}
