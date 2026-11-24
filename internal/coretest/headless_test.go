package coretest_test

import (
	"net/url"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/apps/agentkit"
	"github.com/wakeshift/server/internal/coretest"
	"github.com/wakeshift/server/pkg/wakeshift"
)

// headlessApp registers an app with a headless manifest (the default),
// saying whether other people's agents can wake its users' agents.
func headlessApp(t *testing.T, c *coretest.Core, id string, othersCanWake bool) *wakeshift.Client {
	t.Helper()
	app := c.App(id, "https://"+id+".test/mcp")
	scope := []wakeshift.ScopeEvent{{Pattern: "app." + id + ".game.*", Description: "Games it joins"}}
	defineNotes(t, app)
	if err := app.PublishManifest(ctx, wakeshift.Manifest{OthersCanWake: othersCanWake, Agent: &wakeshift.AgentScope{Wake: scope, Post: scope, TTLDays: 30}}); err != nil {
		t.Fatal(err)
	}
	return app
}

// A headless app needs no Wakeshift account for its users: it makes a
// workspace per user, adds their agent, and acts for it.
func TestHeadlessApp(t *testing.T) {
	c := coretest.Start(t)
	app := headlessApp(t, c, "quiz", false)
	ws, err := app.Workspace(ctx, "guest-1", "")
	if err != nil {
		t.Fatal(err)
	}
	if again, _ := app.Workspace(ctx, "guest-1", ""); again != ws {
		t.Fatalf("the same user got another workspace: %s, %s", ws, again)
	}
	// Granted at once: adding the app was the user's consent.
	if grant, err := app.AddAgent(ctx, ws, "claude-code", "Claude Code"); err != nil || grant == "" {
		t.Fatalf("add agent: %q %v", grant, err)
	}
	ag, err := app.ActAs(ctx, ws, "claude-code")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := agentkit.Sessions(ctx, ag); err != nil {
		t.Fatalf("acting for the agent: %v", err)
	}

	// Another app can't use this app's workspace.
	other := headlessApp(t, c, "trivia", false)
	if _, err := other.AddAgent(ctx, ws, "claude-code", ""); !wakeshift.IsCode(err, "unknown_workspace") {
		t.Fatalf("another app's workspace: %v", err)
	}
	if _, err := other.ActAs(ctx, ws, "claude-code"); !wakeshift.IsCode(err, "unknown_workspace") {
		t.Fatalf("acting in another app's workspace: %v", err)
	}
}

// No app signs in Wakeshift users (account mode is turned off): client
// registration, the authorize page (for an MCP client asking for the app's
// MCP URL, or the app signing users in as app:<id>) and the token
// endpoint, the token exchange included, all refuse, even for a signed-in
// person and one of our apps (approved for accounts before).
func TestNoAppSignsInWakeshiftUsers(t *testing.T) {
	c := coretest.Start(t)
	ada := c.SignIn("ada@example.com")
	const mcp = "https://chat.test/mcp"
	app, _ := c.AppClient("chat", "Chat", mcp)
	scope := []wakeshift.ScopeEvent{{Pattern: "app.chat.room.*", Description: "Rooms"}}
	if err := app.PublishManifest(ctx, wakeshift.Manifest{Agent: &wakeshift.AgentScope{Wake: scope, Post: scope, TTLDays: 30}}); err != nil {
		t.Fatal(err)
	}
	var reg struct{ Error string }
	if code := ada.Do("POST", "/oauth/register", map[string]any{"client_name": "Codex", "redirect_uris": []string{"http://127.0.0.1:9/cb"}}, &reg); code != 400 || reg.Error != "invalid_client_metadata" {
		t.Fatalf("register: %d %+v", code, reg)
	}
	for _, client := range []string{"wsk_any", "app:chat"} {
		query := "response_type=code&client_id=" + client + "&redirect_uri=http%3A%2F%2F127.0.0.1%3A9%2Fcb&code_challenge=" +
			strings.Repeat("a", 43) + "&code_challenge_method=S256&resource=" + mcp
		var view struct {
			Error struct{ Code, Message string }
		}
		if code := ada.Do("GET", "/api/v1/oauth/authorize?"+query, nil, &view); code != 400 || view.Error.Code != "invalid_target" {
			t.Fatalf("signing in to %s: %d %+v", client, code, view)
		}
	}
	code, out := postForm(t, c.URL+"/oauth/token", url.Values{"grant_type": {"urn:ietf:params:oauth:grant-type:token-exchange"},
		"subject_token": {"x"}, "subject_token_type": {"urn:ietf:params:oauth:token-type:access_token"}})
	if code != 400 || out["error"] != "unauthorized_client" {
		t.Fatalf("token exchange: %d %v", code, out)
	}
}

// An app whose users' agents other people's agents can wake (OthersCanWake)
// needs no Wakeshift screen either: the grant is made at once, as the app's
// (confirmed_by "app", audited), and the app acts for the agent.
func TestHeadlessOthersCanWakeGrantedAtOnce(t *testing.T) {
	c := coretest.Start(t)
	app := headlessApp(t, c, "nightfall", true)
	ws, _ := app.Workspace(ctx, "guest-1", "")
	grant, err := app.AddAgent(ctx, ws, "claude-code", "")
	if err != nil || grant == "" {
		t.Fatalf("add agent: %q %v", grant, err)
	}
	if again, _ := app.AddAgent(ctx, ws, "claude-code", ""); again != grant {
		t.Fatalf("adding it again made another grant: %s, %s", grant, again)
	}
	var confirmedBy, actor string
	c.DB.InTenant(ctx, ws, func(tx pgx.Tx) error {
		tx.QueryRow(ctx, `SELECT confirmed_by FROM grants WHERE id = $1`, grant).Scan(&confirmedBy)
		return tx.QueryRow(ctx, `SELECT string_agg(actor, ',') FROM audit_log WHERE grant_id = $1 AND action = 'grant.created'`, grant).Scan(&actor)
	})
	if confirmedBy != "app" || actor != "app:nightfall" { // recorded once, as the app
		t.Fatalf("grant confirmed_by %q, audit actors %q", confirmedBy, actor)
	}
	ag, err := app.ActAs(ctx, ws, "claude-code")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := agentkit.Sessions(ctx, ag); err != nil {
		t.Fatalf("acting for the agent: %v", err)
	}
}

// Suspending an app stops it at once, including the agent tokens it minted
// before the suspension (they last 15 minutes); unsuspending lets them
// work again.
func TestSuspendStopsAgentTokens(t *testing.T) {
	c := coretest.Start(t)
	app := headlessApp(t, c, "quiz", false)
	ws, err := app.Workspace(ctx, "guest-1", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.AddAgent(ctx, ws, "claude-code", "Claude Code"); err != nil {
		t.Fatal(err)
	}
	ag, err := app.ActAs(ctx, ws, "claude-code")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := agentkit.Sessions(ctx, ag); err != nil {
		t.Fatalf("before: %v", err)
	}
	if _, err := c.DB.Pool.Exec(ctx, `UPDATE apps SET suspended_at = now(), suspended_by = 'test', suspended_reason = 'test' WHERE id = 'quiz'`); err != nil {
		t.Fatal(err)
	}
	if _, err := agentkit.Sessions(ctx, ag); err == nil || !strings.Contains(err.Error(), "suspended") {
		t.Fatalf("a suspended app's agent token minted before the suspension: %v, want refused as suspended", err)
	}
	if _, err := c.DB.Pool.Exec(ctx, `UPDATE apps SET suspended_at = NULL, suspended_by = NULL, suspended_reason = NULL WHERE id = 'quiz'`); err != nil {
		t.Fatal(err)
	}
	if _, err := agentkit.Sessions(ctx, ag); err != nil {
		t.Fatalf("after unsuspending: %v", err)
	}
}
