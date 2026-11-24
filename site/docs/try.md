# Try it without an account

The try app gives you a webhook URL that wakes your agent. Point curl, GitHub, Stripe or your own code at it, and each POST wakes the agent with what was sent. It takes about two minutes, and you don't sign up for anything.

**You need:** Claude Code, Codex, Claude Cowork, ChatGPT (a Work chat), Cursor, Antigravity, opencode, Copilot CLI, OpenClaw, Hermes Agent or pi. Any other MCP client that speaks Streamable HTTP can connect too.

## 1. Get a link

Open [the try app]({{TRY}}) and get a link. The page's address is your link, so bookmark it. Anyone with it can connect an agent to your webhook and see what's posted.

## 2. Connect your agent

The page has a tab for each agent, with your link's URL filled in. It looks like this:

```sh tab:Claude Code
claude mcp add --transport http webhooks {{TRY}}/t/<code>/mcp
```

```sh tab:Codex
codex mcp add webhooks --url {{TRY}}/t/<code>/mcp
```

```text tab:Claude Cowork
{{TRY}}/t/<code>/mcp
```

```text tab:ChatGPT
{{TRY}}/t/<code>/mcp
```

```json tab:Cursor
{
  "mcpServers": {
    "webhooks": {
      "url": "{{TRY}}/t/<code>/mcp"
    }
  }
}
```

```sh tab:Antigravity
agy mcp add webhooks {{TRY}}/t/<code>/mcp
```

```sh tab:opencode
opencode mcp add webhooks --url {{TRY}}/t/<code>/mcp
```

```sh tab:Copilot CLI
copilot mcp add --transport http webhooks {{TRY}}/t/<code>/mcp
```

```sh tab:OpenClaw
openclaw config set mcp.servers.webhooks '{"url":"{{TRY}}/t/<code>/mcp","transport":"streamable-http"}' --strict-json
```

```sh tab:Hermes Agent
hermes mcp add webhooks --url {{TRY}}/t/<code>/mcp
```

```sh tab:pi
pi mcp add webhooks --url {{TRY}}/t/<code>/mcp
```

```text tab:Other MCP clients
{{TRY}}/t/<code>/mcp
```

For Claude Cowork, open Settings in the Claude app, then Connectors, choose Add custom connector, paste the URL and choose Connect. For ChatGPT, open Settings, then Apps & Connectors, then Advanced settings, and turn on Developer mode; back in Apps & Connectors, choose Create, paste the URL and choose No authentication. For Cursor, the JSON goes in `~/.cursor/mcp.json`, which the IDE and `cursor-agent` both read; the page also has an **Add to Cursor** button that opens Cursor's MCP settings with the server filled in. For OpenClaw, run the command where its gateway runs (in Docker, `docker exec` into its container). It works while the gateway is running, and new MCP servers are used from the next turn. (`openclaw mcp add` needs the gateway stopped.) For Hermes Agent, run the command where Hermes runs and answer no when it asks about authentication (or add the server under `mcp_servers` in `$HERMES_HOME/config.yaml`, `~/.hermes/config.yaml` unless `HERMES_HOME` is set); `/reload-mcp` picks it up in a running chat, or start a new one. For pi, type `/reload` in a pi session that's already running to pick it up. Other clients take the URL as a Streamable HTTP server.

There's no sign-in: the code in the URL is the credential. Then ask your agent:

```text Prompt
Watch for webhooks and tell me when one arrives.
```

What happens next depends on the agent:

- **Claude Code** first asks whether to install Wakeshift's plugin for Claude Code, showing the exact command. Say yes, type `/reload-plugins` and send any message: each post then wakes the agent within seconds, even when it's idle, with nothing more to approve. If you say no, it starts a background monitor instead (in manual permission mode, it asks you to approve it), with the same wakes and nothing installed; Claude Code ends a monitor after 30 minutes, so the agent starts it again every 25 by itself, and in manual permission mode each restart asks for your approval.
- **Codex**: the first time, the agent asks whether to install Wakeshift's plugin for Codex, showing the exact command, and runs it only if you say yes. Afterwards, quit Codex, start it again with `codex resume --last`, choose "Trust all and continue" to trust the new hook, and send any message. In the ChatGPT desktop app's Codex mode, trust the hooks in the composer instead: click "Review hooks", then "Allow all", and send any message. Then each post wakes the agent within seconds.
- **Claude Cowork**: the agent starts a background listener in the task. Each post then wakes it within seconds while Cowork is open, even when it's idle. Nothing to install. If the agent says it can't reach Wakeshift, open Settings > Capabilities in the Claude app, turn on "Use the internet while making files", click Manage next to "Allowed websites" and add `app.wakeshift.ai` (on Team and Enterprise plans, an owner does this under Organization settings > Capabilities). Then tell the agent. In a Claude chat outside Cowork, the agent isn't woken: posts arrive the next time it uses the webhook's tools.
- **ChatGPT**: in a Work chat (ChatGPT Business or Enterprise; on desktop, choose Cloud), turn on the connector and say "Watch this webhook." ChatGPT subscribes to it, and each post starts a new run that sees what was sent; a run takes longer to start than other agents' wakes. In personal chats ChatGPT isn't woken: posts arrive the next time it uses the webhook's tools.
- **Cursor** (the IDE and `cursor-agent`): the agent starts a background command that waits for posts (Cursor may ask you to allow it). Each post wakes the agent within seconds, even when it's idle. Nothing to install; the agent also asks once whether to install Wakeshift's package for Cursor, which is optional. Cursor's cloud agents aren't woken: posts arrive with their next tool call.
- **Antigravity**: the agent starts a background command that waits for posts (`agy` may ask you to allow it). Each post wakes the agent within seconds, even when it's idle. Nothing to install; the agent also asks once whether to install Wakeshift's plugin for Antigravity, which is optional.
- **opencode**: the agent offers a one-time install of Wakeshift's plugin. Say yes, restart opencode (`opencode --continue` reopens the session) and ask the agent something that uses the webhook, such as checking for new posts. Then each post wakes the agent, even when it's idle.
- **Copilot CLI**: the agent starts a background command that waits for posts. Each post wakes the agent within seconds, even when it's idle. Nothing to install. The agent also asks once whether to install Wakeshift's plugin for Copilot CLI, which is optional. Copilot asks you to approve that command each time the agent goes back to waiting, so each wake needs one "Yes"; running `copilot` with `--allow-all-tools` skips that, and approves every tool. With Wakeshift's plugin installed, the agent waits with Wakeshift's own wait command, which the plugin approves, so wakes ask nothing.
- **OpenClaw**: the agent offers a one-time install of Wakeshift's plugin for OpenClaw. Say yes. The plugin connects the conversation the next time the agent uses the webhook, so ask it something that does, such as checking for new posts. After that, each post wakes the agent within seconds, even when it's idle, as long as the gateway runs.
- **Hermes Agent**: the agent offers a one-time install of Wakeshift's plugin for Hermes (Hermes may ask you to approve the command once). Say yes. If the install says the running gateway loaded the plugin, a chat through the gateway (its API server, Telegram or Discord) just needs any message. Otherwise, and always in `hermes chat`, Hermes loads it when a chat starts or is resumed, so resume the chat (exit and run the `hermes --resume <session>` command Hermes prints, or `hermes chat --continue`; `hermes chat --tui --continue` in the TUI; or restart the gateway) or start a new one, and send any message. After that, each post wakes the agent within seconds, even when it's idle, as long as Hermes runs (its API server, CLI, TUI or Desktop app, or a Telegram or Discord direct message with its bot).
- **pi**: the agent offers a one-time install of Wakeshift's extension for pi. Say yes, then type `/reload` in pi and ask the agent something that uses the webhook, such as checking for new posts. After that, each post wakes the agent within seconds, even when it's idle, as long as pi is open. While it's busy, it gets each post once its turn ends.
- **Other MCP clients**: the agent is told that nothing will notify it in that conversation. Posts wait for it, and reach it only if it's later connected another way.

## 3. Post to your webhook

The page shows your webhook URL and a `curl` line for it, which sends JSON. To send plain text:

```sh
curl -H 'Content-Type: text/plain' --data-binary 'CI failed on main' <your webhook URL>
```

Your agent wakes with a line that says what came and how much, then as much of the body as fits. JSON, forms and text all work; the agent gets the parsed body as the event's data. Events posted to a GitHub, Stripe or Linear URL (see below) get a line of their own, such as `GitHub workflow_run completed: failure on main (tests) in acme/api` or `Stripe invoice.payment_failed: in_…, USD 20.00, open (evt_…)`. Anyone with the open URL can send what those services send, so your agent reads a post there as unverified, whatever it says it is. The page lists each post as it arrives, how many watching conversations it reached, and whether an agent is connected right now.

If a sender retries, send the same `Idempotency-Key` header and your agent is woken once. GitHub's, Stripe's and Linear's own retries of an event are recognized without it.

## Check who can post

The webhook URL is open by default: anyone who has it can post. Before you give it to a service, pick a check on the page. Each check gives you a different URL:

- **GitHub.** Requests must carry `X-Hub-Signature-256`. In GitHub, open Settings, then Webhooks, and add one with this URL, content type `application/json` and the page's secret.
- **Stripe.** Requests must carry `Stripe-Signature`, made with your endpoint's signing secret, from the last 5 minutes. Stripe makes the secret, so add the endpoint in Stripe first (with the open URL), copy its signing secret (`whsec_…`) into the page, and change the endpoint's URL to the one the page gives you.
- **Linear.** Requests must carry `Linear-Signature`, made with your webhook's signing secret, and a `webhookTimestamp` from the last minute. As with Stripe: add the webhook in Linear, paste its signing secret into the page, then change the webhook's URL to the one you get.
- **Bearer token.** Requests must carry `Authorization: Bearer <secret>`.

Anyone with the open URL can wake your agent, up to 60 times an hour and 500 a day. If the sender can sign its requests or send a bearer token, give it a checked URL instead. If the open URL got out, choose **Get a new URL** on the page's Open tab: the old URL stops working at once, and your agent keeps watching, with the same history and checked URLs.

## What the agent can do

The try app gives your agent `watch` and `unwatch`, `recent_posts` (the last 10 posts in full) and `webhook_info` (the URL, and how to test it), plus `subscribe_events`, `unsubscribe_events` and `list_subscriptions`. Each post is a `webhook.received` [event](/docs/events), so clients that speak MCP Events can subscribe to it natively.

## Limits

- 60 posts an hour and 500 a day for each URL. Refused posts don't count; retries do, though your agent is woken once for them. These are lower than an app's [publish URLs](/docs/publish-urls), which take 60 a minute.
- 1,000 posts an hour from one address, across all webhooks.
- Bodies up to 64 KB, sent within 10 seconds.
- One wake brings your agent at most 20 posts. If more arrive in a burst, the rest come in the next wake, as soon as the agent goes back to waiting. `recent_posts` gives the last 10 in full.
- A link expires after a week without use.
- The live view lists only the first few refused or rate-limited posts each minute one by one, so a flood of them doesn't push real posts out of view. The rest are counted in one line once the minute ends, with the statuses they got.
- The page keeps the last 50 lines of its live view and the last 10 posts in full. A webhook's posts are deleted after a day without use when no agent is watching, or after 7 days without use.

## Next steps

The try app is for trying Wakeshift. A lasting webhook takes code: to build your own app, [sign up]({{APP}}) and follow the [quickstart](/docs/quickstart). In your own app, emit an [event](/docs/events) from your webhook handler, or point the sender at a [publish URL](/docs/publish-urls). The try app is built on the same SDK.
