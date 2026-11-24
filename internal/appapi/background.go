package appapi

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/deliveries"
	"github.com/wakeshift/server/internal/harnesses"
	"github.com/wakeshift/server/internal/sessions"
	"github.com/wakeshift/server/internal/streams"
)

// backgroundResult: Cursor and Antigravity CLI conversations are woken by a
// command the agent starts in the background itself (harnesses.Background):
// Cursor's Shell with notify_on_output on a loop of the session's one-shot
// wait (harnesses.CursorLoop), Antigravity's run_command on one that ends
// when messages arrive (harnesses.UntilLoop). Nothing to install. The
// command is the same for the session (backgroundURL hands out its link
// again). Subscribing gives the command (and says to keep it running if it
// is); other results add no instructions, even while it isn't running
// (what arrives waits for the session until the command runs again).
// Wakeshift's hooks are an extra (they deliver when a turn ends, and
// confirm), not a replacement: results carry a beacon for them as
// hooksResult's do.
//
// A Cursor conversation passes its CURSOR_CONVERSATION_ID back
// (conversation_id), which the session learns, and says which program it
// is (harness): Cursor's MCP traffic is also Grok Bot's and its cloud
// agents' (cv.variant). A cloud agent (claimed, or an ID starting with
// bc-) can't be woken yet: subscribing says so, and nothing else. Grok
// Bot waits in the foreground (harnesses.ForegroundNote). Until the agent
// says, the watch result asks it to (harnesses.AskNote), with Cursor's
// instructions for a tool that has no harness argument.
func (a *API) backgroundResult(ctx context.Context, tx pgx.Tx, cv *conversation, h harnesses.Harness, why resultFor) (toolResult, error) {
	s := cv.session
	var out toolResult
	app := appName(cv)
	// A variant of the family with a wake path of its own (Harness.Wake).
	switch v := cv.variant; {
	case v.ID != "" && v.Wake == harnesses.Pull:
		note := harnesses.CantWake(v.Name)
		if v.ID == "cursor-cloud" {
			note = harnesses.CursorCloudNote()
		}
		return cantWakeResult(cv, note, why), nil
	case v.ID != "" && v.Wake == harnesses.Foreground && a.grokOn():
		return a.grokResult(ctx, tx, cv, v, why) // its webhook: grokhooks.go
	case v.ID != "" && v.Wake == harnesses.Foreground:
		return a.foregroundResult(ctx, tx, cv, v, why)
	}
	live, err := backgroundLive(ctx, tx, s)
	if err != nil {
		return out, err
	}
	if cmd := waitCommand(s); cmd != "" {
		return a.waitResult(ctx, tx, cv, h, why, cmd, live)
	}
	if why == forSubscribe && !live && a.copilotPluginReady(cv, h) {
		// Wakeshift's plugin for Copilot CLI (or the hooks its installer
		// wrote) marked the call, and its preMcpToolCall hook named the
		// conversation: its postToolUse hook connects the conversation
		// from this result's beacon and gives the wait command, which its
		// permissionRequest hook approves. No install to offer, no question
		// and no shell loop on the session's link.
		out.Text = join(harnesses.PluginReadyNote(h, app, cv.call.Plugin.Hooks), passSession(cv))
		return out, a.withBeacon(ctx, tx, cv, h, &out)
	}
	// A command that's running is Cursor's own (local) one: no need to ask.
	askHarness := why == forSubscribe && h.ID == "cursor" && !cv.said && !live
	switch {
	case why == forAnnotate:
		// No instructions, whether or not the command runs (it's the
		// agent's to start again, and messages wait for it). The command
		// may have died a moment ago with the harness: the restart that
		// loads Wakeshift's hooks (Copilot CLI, Cursor, agy) kills it. The
		// hooks connect the conversation only from a result's beacon, and
		// the agent's first call after the restart is often another of the
		// app's tools, within seconds. So until the hooks have connected
		// it, the result carries a beacon, as the Monitor's does, at most
		// one per tool every remintAfter.
		if !s.Watching {
			return out, nil
		}
		if listening, err := hooksListening(ctx, tx, s); err != nil || listening {
			cv.noBeacon = "connected"
			return out, err
		}
		if recent, err := recentTicket(ctx, tx, cv); err != nil || recent {
			cv.noBeacon = "recent_ticket"
			return out, err
		}
		return out, a.withBeacon(ctx, tx, cv, h, &out)
	case live && !askHarness:
		url, claim, err := a.backgroundCommandURL(ctx, tx, cv, h)
		if err != nil {
			return out, err
		}
		out.Text = join(harnesses.BackgroundRunningNote(h, app, url, claim), passSession(cv))
		return out, a.withBeacon(ctx, tx, cv, h, &out)
	}
	before, err := cv.toldBefore(ctx, tx)
	if err != nil {
		return out, err
	}
	url, claim, err := a.backgroundCommandURL(ctx, tx, cv, h)
	if err != nil {
		return out, err
	}
	ask := h.ConversationEnv != "" && s.Conversation == "" && !askHarness
	if before {
		out.Text = join(harnesses.BackgroundAgainNote(h, app, url, claim, ask, hooksLinked(s)), passSession(cv))
	} else {
		upgrade := ""
		if !hooksLinked(s) {
			upgrade = harnesses.BackgroundUpgradeNote(withTools(h, cv), a.base())
		}
		out.Text = join(harnesses.BackgroundNote(h, app, url, claim, ask), passSession(cv), upgrade)
	}
	if askHarness {
		// Cursor's traffic is also Grok Bot's and its cloud agents': the
		// harness argument on later calls tells them apart.
		out.Text = join("If you're Grok Bot or a Cursor cloud agent, pass "+harnesses.HarnessArg+" (\"grok-bot\" or \"cursor-cloud\") on your calls to this app's tools, and don't start the command below. Here's how to be woken in "+h.Name+" ("+h.ClaimHint+"):", out.Text)
	}
	return out, a.withBeacon(ctx, tx, cv, h, &out)
}

// waitCommand is the wait command the session's hooks reported
// (sessions.WaitCommand), once they've connected it: "" if none.
func waitCommand(s sessions.Session) string {
	if !hooksLinked(s) || !harnesses.WaitCommandOK(s.WaitCommand) {
		return ""
	}
	return s.WaitCommand
}

// waitResult is backgroundResult once Wakeshift's hooks connected the
// conversation and said its wait command (harnesses.WaitNote): the agent
// runs that, the same in every conversation and with no link in it, rather
// than a loop on the session's wait URL, so no link is handed out here.
// Subscribing gives the command (and says to keep it running if it is);
// other results add nothing.
func (a *API) waitResult(ctx context.Context, tx pgx.Tx, cv *conversation, h harnesses.Harness, why resultFor, cmd string, live bool) (toolResult, error) {
	s := cv.session
	var out toolResult
	app := appName(cv)
	switch {
	case why == forAnnotate:
		return out, nil
	case live:
		out.Text = join(harnesses.WaitRunningNote(h, app, cmd), passSession(cv))
		return out, a.withBeacon(ctx, tx, cv, h, &out)
	}
	before, err := cv.toldBefore(ctx, tx)
	if err != nil {
		return out, err
	}
	ask := h.ConversationEnv != "" && s.Conversation == ""
	if before {
		out.Text = join(harnesses.WaitAgainNote(h, app, cmd, ask), passSession(cv))
	} else {
		out.Text = join(harnesses.WaitNote(h, app, cmd, ask), passSession(cv))
	}
	return out, a.withBeacon(ctx, tx, cv, h, &out)
}

// foregroundResult: Grok Bot and ChatGPT's Codex mode can't be woken by a
// background command, so they wait in the foreground on the session's
// one-shot wait (Wake Foreground; with the harness's ForegroundTimeout, if
// it has one, which the command asks for itself: harnesses.ForegroundLoop),
// and run it again after each batch, quiet round or interruption. url is
// the plain wait URL, since the link file must hold just the link
// (LinkStep, linkRead). Subscribing gives the command; other results add
// nothing.
func (a *API) foregroundResult(ctx context.Context, tx pgx.Tx, cv *conversation, h harnesses.Harness, why resultFor) (toolResult, error) {
	s := cv.session
	var out toolResult
	app := appName(cv)
	if why == forAnnotate {
		return out, nil
	}
	url, err := a.backgroundURL(ctx, tx, cv, h)
	if err != nil {
		return out, err
	}
	ask := h.ConversationEnv != "" && s.Conversation == ""
	out.Text = join(harnesses.ForegroundNote(h, app, url, ask), a.codexAppsNote(cv), passSession(cv))
	return out, nil
}

// backgroundCommandURL is backgroundURL, and for Cursor a one-time code
// its command claims that link with (mintLinkClaim, #1152), so no text it
// is given holds the link. For other harnesses, the zero Claim
// (harnesses.LinkStep).
func (a *API) backgroundCommandURL(ctx context.Context, tx pgx.Tx, cv *conversation, h harnesses.Harness) (string, harnesses.Claim, error) {
	url, err := a.backgroundURL(ctx, tx, cv, h)
	if err != nil || !claimsLink(h.ID) {
		return url, harnesses.Claim{}, err
	}
	claim, err := a.mintLinkClaim(ctx, tx, cv, url)
	return url, claim, err
}

// backgroundLive: a background command holds the session's stream (a wait,
// or an older command's lines stream with max=, renewed in the last
// minute: it's waiting, or just returned and the agent is starting it
// again), on a link that still works (one on a revoked or expired link,
// after unwatch, is about to end: streams.LiveOnWorkingLink).
func backgroundLive(ctx context.Context, tx pgx.Tx, s sessions.Session) (bool, error) {
	return streams.LiveOnWorkingLink(ctx, tx, s.Agent, s.ID, true)
}

// backgroundURL gives the session's one-shot wait URL (streams.WaitURL),
// which the harness's command reads: the session's link again if it has
// one that still works (sessionLink), so the command stays the same, else
// a new one. A session delivered on the agent's connector stream (one a
// harness named by its conversation) gets its own deliveries from now on,
// as when a connector redeems.
func (a *API) backgroundURL(ctx context.Context, tx pgx.Tx, cv *conversation, h harnesses.Harness) (string, error) {
	s := cv.session
	if s.Runtime == sessions.Connector {
		if _, err := tx.Exec(ctx, `UPDATE sessions SET runtime = 'stream' WHERE id = $1`, s.ID); err != nil {
			return "", err
		}
		if err := deliveries.ToOwnStream(ctx, tx, s.Agent, s.ID); err != nil {
			return "", err
		}
		cv.session.Runtime = sessions.Stream
	}
	link, err := a.sessionLink(ctx, tx, cv)
	if err != nil {
		return "", err
	}
	return streams.WaitURL(link), nil
}

// sessionLink is the session's sliding stream link (streams.NewLink.Slide)
// that this app made under the conversation's grant, if one still works
// (streams.Server.SessionLink), else a new one: the URL an agent's wait
// command reads, the same for the session while it's used.
func (a *API) sessionLink(ctx context.Context, tx pgx.Tx, cv *conversation) (string, error) {
	return a.sessionLinkFor(ctx, tx, cv.tenant, cv.session, cv.app.ID, cv.act.grant.ID)
}

// sessionLinkFor is sessionLink for session s of tenant, made by app
// under grant.
func (a *API) sessionLinkFor(ctx context.Context, tx pgx.Tx, tenant string, s sessions.Session, app, grant string) (string, error) {
	createdBy := "app:" + app
	link, err := a.Streams.SessionLink(ctx, tx, tenant, s.Agent, s.ID, createdBy, grant)
	if err != nil || link != "" {
		return link, err
	}
	link, _, err = a.Streams.Mint(ctx, tx, tenant, streams.NewLink{Agent: s.Agent, Target: s.ID, CreatedBy: createdBy,
		GrantID: grant, TTL: streams.SessionLinkTTL, Slide: streams.SessionLinkMax})
	return link, err
}

// copilotPluginReady: a Copilot CLI call that Wakeshift's plugin (or its
// installer's hooks) marked (pluginMarked), from a session where those
// hooks can connect the conversation from the result: their
// preMcpToolCall hook named it in the call's _meta (the
// harness's ConversationMeta), which is what their postToolUse hook binds
// a beacon to. A mark without it (the model passed the mark where the
// hooks don't run: another program, a session started before the plugin)
// gets the result an unmarked call gets, so it's never told that something
// connects it when nothing can.
func (a *API) copilotPluginReady(cv *conversation, h harnesses.Harness) bool {
	return h.ID == harnessCopilot && cv.variant.ID == "" && a.pluginMarked(cv) &&
		harnesses.Conversation(h, cv.call.Meta) != "" && cv.session.Conversation == harnesses.Conversation(h, cv.call.Meta)
}
