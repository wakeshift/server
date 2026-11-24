package coretest_test

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/apps/agentkit"
	"github.com/wakeshift/server/internal/coretest"
	"github.com/wakeshift/server/internal/grants"
	"github.com/wakeshift/server/pkg/wakeshift"
)

const relayMCP = "https://relay.test/mcp"

// relayApp registers a second Agent Chat-like app.
func relayApp(t *testing.T, c *coretest.Core) *wakeshift.Client {
	t.Helper()
	app, _ := c.AppClient("relay", "Relay", relayMCP)
	scope := []wakeshift.ScopeEvent{{Pattern: "app.relay.channel.*", Description: "Channels it joins"}}
	if err := app.PublishManifest(ctx, wakeshift.Manifest{Description: "More channels for agents",
		Agent: &wakeshift.AgentScope{Wake: scope, Post: scope, TTLDays: 90}}); err != nil {
		t.Fatal(err)
	}
	return app
}

// Each app keeps its own workspace for a user, and acts only there: another
// app can't add agents to it, act for its agents or get a grant in it, and
// the same person's workspaces are separate per app. (Two apps' deliveries
// in one conversation were possible only in people's own workspaces, under
// account mode, which is turned off.)
func TestAppsReachOnlyTheirOwnWorkspaces(t *testing.T) {
	c := coretest.Start(t)
	chat, relay := chatApp(t, c), relayApp(t, c)
	codexChat := actAs(t, chat, "ada", "codex")
	codexRelay := actAs(t, relay, "ada", "codex")
	if codexChat.Workspace == codexRelay.Workspace {
		t.Fatal("two apps share a user's workspace")
	}
	pipe, err := agentkit.SessionLink(ctx, codexChat, agentkit.LinkRequest{Label: "pipe"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := relay.AddAgent(ctx, codexChat.Workspace, "claude-code", ""); !wakeshift.IsCode(err, "unknown_workspace") {
		t.Fatalf("adding an agent to another app's workspace: %v", err)
	}
	if _, err := relay.ActAs(ctx, codexChat.Workspace, "codex"); !wakeshift.IsCode(err, "unknown_workspace") {
		t.Fatalf("acting in another app's workspace: %v", err)
	}
	if _, err := agentkit.SessionLink(ctx, codexRelay, agentkit.LinkRequest{Session: pipe.Session}); !wakeshift.IsCode(err, "unknown_session") {
		t.Fatalf("a link for another app's session: %v", err)
	}
	relayApp, err := c.Apps.Get(ctx, "relay")
	if err != nil {
		t.Fatal(err)
	}
	err = c.DB.InTenant(ctx, codexChat.Workspace, func(tx pgx.Tx) error {
		_, err := grants.Create(ctx, tx, grants.New{App: relayApp, AgentID: "codex"})
		return err
	})
	if !errors.Is(err, grants.ErrWrongMode) {
		t.Fatalf("a grant in another app's workspace: %v", err)
	}
}
