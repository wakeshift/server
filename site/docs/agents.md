# Agents

You don't write code for any agent. Wakeshift recognizes which agent is subscribing and wakes it the way that agent can be woken. Clients that speak MCP Events, such as ChatGPT, OpenClaw's automations, Hermes Agent and opencode 2.4 and later, subscribe natively ([agents that speak MCP Events](/docs/mcp-events)); every other agent subscribes with the `subscribe_events` tool, or through a tool of yours that subscribes it ([subscribe from your own tools](/docs/events#subscribe-from-your-own-tools)).

## How each agent wakes

| Agent | Wakes while idle | Install | What the user approves | Typical wake |
| --- | --- | --- | --- | --- |
| Claude Code | Yes | None (hooks are an optional upgrade) | Nothing with the hooks. Without them, in manual permission mode, a background monitor when the agent starts watching, and again each time it restarts it (every 25 minutes, since Claude Code ends a monitor after 30); where your app serves [the wait tool](#the-wait-tool-for-claude-code), the first call to it instead (Claude Code offers "don't ask again"), and no restarts | Seconds |
| Codex | Yes | Once | Trusting the new hook (restart Codex with `codex resume --last`, then "Trust all and continue"; in the ChatGPT desktop app's Codex mode, "Review hooks", then "Allow all", in the composer) | Seconds |
| Claude Cowork | Yes, while Cowork is open, through a background listener the agent starts in the task | None | Nothing, if the workspace lets Cowork reach Wakeshift (see below) | Seconds |
| Cursor (CLI and IDE) | Yes | None (hooks are optional) | Cursor asks before it starts a background command, which keeps running across wakes: choose "Run (once)" | Seconds |
| Cursor cloud agents | No: it's told, when it subscribes, that nothing will notify it, and checks with your tools as it would without Wakeshift | None | Nothing | — |
| ChatGPT (Work chats on the web) | Yes: each occurrence starts a run in the chat | None | The user asks ChatGPT to monitor the event, in a Work chat | Slower: each one starts a new run |
| ChatGPT desktop (Codex mode, cloud) | Only while it waits for events in the foreground, which keeps the chat busy: a message from the user interrupts the wait, and the agent starts it again after answering. Each wait ends within 90 seconds and the agent starts the next | None | Nothing | Seconds, while it waits |
| GitHub Copilot CLI | Yes | None (hooks are optional) | Without Wakeshift's hooks, one "Yes" each time it goes back to waiting, so once per wake, unless `copilot` runs with `--allow-all-tools` (which approves every tool). With them, nothing: they approve Wakeshift's wait command, and only that command | Seconds |
| opencode | Yes | Once | Nothing; restart opencode after the install | Seconds |
| pi | Yes, while pi is open; while it's busy, once its turn ends | Once | Nothing; type `/reload` in pi after the install | Seconds |
| Antigravity CLI | Yes | None (hooks are optional) | `agy` may ask to allow a background command; allowing it in the conversation is enough | Seconds |
| OpenClaw | Yes, while the gateway runs | Once | Nothing; on a gateway that listens beyond this computer, the agent's question says OpenClaw's hooks endpoint is reachable there too | Seconds |
| Hermes Agent | Yes, while Hermes runs (its API server, CLI, TUI and Desktop chats, and Telegram and Discord direct messages with its bot) | Once | Hermes may ask to approve the install command; in a chat through its API server, where nobody can approve it, the agent gives the user the command to run | Seconds |
| Capy | Yes, once the agent has set up a webhook on its thread: each occurrence wakes the thread, even after its machine has gone to sleep | None | Nothing | Not measured yet |
| Muse (Meta) | Yes, while its background task waits: the task ends with each wake, and the agent starts it again after acting | None | Nothing | About 15 seconds |
| Grok Bot | Yes, once its webhook is set up: the agent creates a routine with a webhook trigger in the bot, and each batch of occurrences then starts a run there, even while the bot is idle. Until then, only while it's waiting for events in the foreground, and talking to it interrupts the wait | Once per bot, the agent installs Wakeshift's connector in the bot's machine with the command the result gives | Pasting the routine's webhook URL and key on a one-time Wakeshift page, once per bot | Seconds after the run starts |
| Goose, Cline | No: it's told, when it subscribes, that nothing will notify it, and checks with your tools as it would without Wakeshift | None | Nothing | — |
| Any other MCP client (your own, or one Wakeshift doesn't know) | No: it's told, when it subscribes, that nothing will notify it, and checks with your tools as it would without Wakeshift | None | Nothing | — |

"Seconds" is the time to the agent's first words; most of it is the model starting its turn ([measured wake times](#measured-wake-times)). One wake brings at most 20 occurrences; if more are waiting, the next wake follows as soon as the agent goes back to waiting.

**Claude Code.** The first time the agent watches something, it asks the user once how to be notified, with three answers: in this session only (nothing is installed), in every session (Wakeshift's plugin), or not at all. Nothing runs before the answer, and what arrives meanwhile is held. With the plugin, nothing more is approved and nothing restarts. For this session only, or if the plugin doesn't install, Claude Code wakes through a background monitor, as in the table, or through your app's wait tool if you turn it on (below). With "not at all", nothing runs, and what arrives waits for the agent's next call to your app's tools.

**Claude Cowork.** When it subscribes, the agent starts a background listener in the Cowork task, which wakes it while Cowork is open. The listener runs in Cowork's sandbox, whose network access the workspace may limit: if the agent says it can't reach Wakeshift, open Settings > Capabilities in the Claude app, turn on "Use the internet while making files", click Manage next to "Allowed websites" and add `app.wakeshift.ai` (on Team and Enterprise plans, an owner does this under Organization settings > Capabilities). Then tell the agent, and it starts listening. Claude chats outside Cowork aren't woken: the agent is told that nothing will notify it, and checks with your tools as it would without Wakeshift.

**opencode.** opencode 2.4 and later (2.3 with its `mcp_events` flag on) subscribes to your events itself, by push, when your server offers push ([agents that speak MCP Events](/docs/mcp-events)): the session that's open wakes with each occurrence, with nothing to install. With Wakeshift's plugin installed, or where your server doesn't offer push, opencode is woken through the plugin, as in the table, and so is opencode 1.x, which its default install still gives. A wake by push is a confirmed wake, so `call.wakes` is true for it ([skip your own wait only when `call.wakes`](/docs/events#skip-your-own-wait-only-when-callwakes)).

**Capy.** When it subscribes, the agent creates a webhook watch on its own thread in Capy and registers the watch's address with Wakeshift, with one command the result gives. Each occurrence then wakes the thread, even when it's idle and its machine is asleep, until the watch ends (at most 30 days); the next time the agent subscribes after that, the result offers to set up a new one. An agent that can't set up the watch waits on a command in the background instead, as Muse does. Wakeshift sends only to the address Capy gave the watch, and keeps it encrypted.

**Grok Bot.** The first time a bot subscribes, the agent sets up wake-ups once for that bot. It runs three commands the result gives, exactly as given: one installs Wakeshift's connector in the bot's machine (checked against its published checksum before it's kept), one makes the bot's signing key in a protected file (the key never enters the agent's memory or its replies), and one proves the key to Wakeshift. Nothing an app sends or a message carries goes into these commands. The agent then creates an Automations routine named "Wakeshift wake-ups" with a webhook trigger, and gives the user a one-time Wakeshift link. On that page, which leads with your app's name, the user pastes the routine's webhook URL and key from its settings. Wakeshift sends a check through the webhook, the routine answers it with the bot's key, and wake-ups turn on: each batch of occurrences starts a run in the bot, which counts toward the user's Grok usage, so Wakeshift sends at most one every 20 seconds, carrying everything waiting. When the same bot subscribes through another app, it joins the same webhook, with no second routine and no second visit to the page, and the bot says so in a run. Once they're on, the page can be closed. To stop them, the user asks the bot to stop watching (that app's wake-ups end), or deletes the routine in Grok (every app's end). Until the webhook is set up, or if the user doesn't want it, the bot waits in the foreground. If the bot's machine is reset, its key and the connector go with it: the routine's run tells the user that wake-ups are off, and when they ask the bot to set them up again, it runs the same three commands for a new key and gives a new link for the webhook.

### The wait tool for Claude Code

With `builtinTools: { wait: true }` (Python: `builtin_tools={"wait": True}`; Go: `BuiltinTools: wakeshift.BuiltinTools{Wait: true}`), `attach` serves Wakeshift's wait tool, `wakeshift_wait`, to a recent Claude Code in the terminal on an account where Anthropic has turned on MCP tasks. It's off unless you turn it on. Your server needs `attach` on a stateless transport (Go: `attached.Handler` in front of a stateless handler).

What changes for a user who declines the plugin:

| | Wait tool off (the default) | Wait tool on |
| --- | --- | --- |
| How it waits | A background monitor, which Claude Code ends after 30 minutes, so the agent starts it again every 25 | A call to `wakeshift_wait`, which returns at once and waits in the background for as long as the conversation lives |
| Approvals in manual permission mode | The monitor, and each restart | The first call (Claude Code offers "don't ask again") |
| `/compact`, closing and `claude --continue` | The monitor stops with the session; the agent starts another when it's next asked to watch | The wait carries on through all three; what arrived meanwhile wakes the conversation when it's back |
| Your server | Not involved in waiting | Holds one request per waiting conversation (below) |

Elsewhere (the VS Code extension, the desktop app, subagents, or an account without MCP tasks), Claude Code uses the monitor either way. Users who install the plugin never use either.

**What it costs your server.** While a conversation waits, its Claude Code keeps one request open to your server for about 20 seconds at a time, and sends the next about a second later, whether or not anything arrives: about three requests a minute, each held about 20 seconds, for every waiting conversation. On a serverless host, that's one function invocation running for each of them (about 130,000 a month for a conversation that waits all month). Give the function a timeout of at least 30 seconds (on Vercel, `maxDuration`): with less, each request is cut off before Wakeshift answers it, and the conversation isn't woken.

Wakeshift recognizes agents from what their MCP clients send. Where calls look alike (Grok Bot and Cursor's cloud agents call as Cursor does), the agent says which program it runs in, and Wakeshift uses that only to choose between the programs the call could be.

### Measured wake times

Measured on Wakeshift's test bench, from the `emit` to the agent's response in the woken conversation. Each time includes the agent's program starting its turn and the model replying:

| Agent | p50 | p90 |
| --- | --- | --- |
| Claude Code | 5.4 s | 6.3 s |
| Codex | 4.2 s | 6.1 s |

Wakeshift's own part of that is about 0.2 seconds; the rest is the agent's. The other agents that wake while idle take seconds too, as in the table above.

## The one-time install

Where an agent needs an install, the flow is the same for every agent:

1. Your tool's result tells the agent that wake-ups need a one-time install.
2. The agent asks its user. It runs the install only if they agree. Where it can't run the command (its program blocks it, or nobody can approve it), it gives the user the command to run themselves.
3. The installer doesn't ask again. It changes only the files described below, keeps a backup of each one it edits, and then prints what it changed and the agent's own trust step, if it has one.
4. Once it's running, the agent sees a note that starts with `Wakeshift: connected` (for Codex, `Wakeshift connector: connected`).

Neither the agent nor the installer shows your users a legal notice. Your own terms carry the Connector Terms and the Privacy Policy to them: see [Your terms and privacy policy](/docs/users#your-terms-and-privacy-policy).

The install command has this shape:

```sh
curl -fsSL {{APP}}/install/<agent>.sh | sh
```

It puts `wakeshift` in `~/.wakeshift/bin`, trusts only this Wakeshift, and adds what the agent needs to its settings. For Claude Code, the agent offers Claude Code's own plugin installer instead, `claude plugin install wakeshift --marketplace wakeshift/agent-plugins` (Claude Code 2.1.292 or later; for an earlier version, `claude plugin marketplace add wakeshift/agent-plugins && claude plugin install wakeshift@wakeshift`), which installs Wakeshift's plugin from [github.com/wakeshift/agent-plugins](https://github.com/wakeshift/agent-plugins); the first time its hooks run, they download Wakeshift's connector to `~/.wakeshift/bin`, checked against the SHA-256 the plugin carries. For OpenClaw, the agent offers OpenClaw's own plugin installer instead, run where the gateway runs (in its container, if it's in one): `openclaw plugins install npm:@wakeshift/openclaw@<version> --force --accept-capabilities && openclaw plugins enable wakeshift --accept-capabilities`. It installs the npm package [@wakeshift/openclaw](https://www.npmjs.com/package/@wakeshift/openclaw), which is published with npm provenance from [github.com/wakeshift/connectors](https://github.com/wakeshift/connectors), and npm checks its integrity. `--force` is what OpenClaw asks for to install a plugin from npm rather than ClawHub, and `--accept-capabilities` stands in for a prompt nobody can answer. The plugin trusts Wakeshift's own service. For another Wakeshift, the command first runs `openclaw config set plugins.entries.wakeshift.config.issuer '"<that Wakeshift>"' --strict-json`. For Hermes Agent, the agent offers Hermes's own plugin installer instead, `hermes plugins install wakeshift/hermes-plugin --enable`, with `hermes config set plugins.entries.wakeshift.allow_gateway_injection true` (the plugin wakes no Hermes chat until it's on), run where Hermes runs; the plugin can also be installed ahead of time, or shipped with your own Hermes plugin ([Hermes Agent](/docs/hermes)). OpenClaw's plugin turns on OpenClaw's own hooks endpoint, with a token of its own and for the conversations that connect through Wakeshift only; on a gateway that listens beyond this computer, that endpoint is reachable wherever the gateway is, so the agent says so in the question it asks before installing. The plugin turns nothing on there until you agree: after your yes, the agent's command starts with `openclaw config set plugins.entries.wakeshift.config.allowNetworkHooks true --strict-json` (the installer script, run by hand, stops to ask first; run it with `WAKESHIFT_OPENCLAW_NETWORK_HOOKS=1` to agree). Each installer leaves the rest of the agent's settings as they were. The script checks the download against a SHA-256 written into the script; both come from the same Wakeshift, so this catches a broken download, not a replaced one.

For Codex and GitHub Copilot CLI, the agent offers the program's own plugin commands instead:

```sh
codex plugin marketplace add wakeshift/agent-plugins && codex plugin add wakeshift@wakeshift
copilot plugin marketplace add wakeshift/agent-plugins && copilot plugin install wakeshift@wakeshift
```

They install Wakeshift's plugin from [github.com/wakeshift/agent-plugins](https://github.com/wakeshift/agent-plugins). The first time its hooks run, they download Wakeshift's connector into `~/.wakeshift/bin`. They check the download against a SHA-256 the plugin carries, in a repository Wakeshift's servers can't change, and install nothing if it doesn't match. Codex's sandbox blocks the command, so the agent runs it with escalated permissions, which you approve. Copilot CLI asks once to run the command, and its plugin's hooks need no approval.

For opencode, the agent offers opencode's own plugin command instead:

```sh
opencode plugin @wakeshift/opencode@<version> --global --force
```

opencode adds Wakeshift's plugin, the npm package `@wakeshift/opencode`, to the `plugin` list in `~/.config/opencode/opencode.json`, keeping the rest of the file, and downloads it with Wakeshift's connector for the computer. The plugin trusts https://app.wakeshift.ai. Wakeshift's npm packages are published with npm provenance from [github.com/wakeshift/connectors](https://github.com/wakeshift/connectors).

For pi, the agent offers pi's own package command:

```sh
pi install npm:@wakeshift/pi@<version>
```

pi adds Wakeshift's extension, the npm package `@wakeshift/pi`, to the `packages` list in `~/.pi/agent/settings.json`, keeping the rest of the file, and downloads it with Wakeshift's connector for the computer. The extension trusts https://app.wakeshift.ai.

For Antigravity CLI, the agent offers agy's own plugin installer:

```sh
agy plugin install https://github.com/wakeshift/antigravity-plugin
```

It installs Wakeshift's plugin from [github.com/wakeshift/antigravity-plugin](https://github.com/wakeshift/antigravity-plugin), and running it again updates the plugin. As with Codex, the first time its hooks run they download Wakeshift's connector into `~/.wakeshift/bin` and check it against the SHA-256 the plugin carries.

For Cursor (the app and `cursor-agent`), the agent offers Wakeshift's npm package instead:

```sh
npx -y @wakeshift/connect@<version> cursor
```

It puts `wakeshift` in `~/.wakeshift/bin`, checking it against the SHA-256 the package carries, and adds the same three hooks the script does. Cursor asks once to run the command. It needs Node 18 or later.

The same install works through npm for every agent the script covers: `npx -y @wakeshift/connect <agent>` puts `wakeshift` in `~/.wakeshift/bin`, checking it against the SHA-256 the package carries, and runs the same install (Node 18 or later).

## What the installer changes

| Agent | What it adds |
| --- | --- |
| Codex | Wakeshift's plugin, which Codex records in `~/.codex/config.toml`. Its hooks run Wakeshift's connector, `~/.wakeshift/bin/wakeshift`, which they download. If an earlier install added a hook to `~/.codex/hooks.json`, the plugin removes that hook the first time it runs. |
| Claude Code | Wakeshift's plugin, with its three hooks, and a rule in `~/.claude/settings.json` that lets Claude Code run Wakeshift's wait command without asking. Moving from the installer's hooks to the plugin removes those hooks. |
| Cursor | Three hooks in `~/.cursor/hooks.json`. |
| GitHub Copilot CLI | Wakeshift's plugin, which Copilot records in `~/.copilot/settings.json`. Its hooks run Wakeshift's connector, `~/.wakeshift/bin/wakeshift`, which they download. If an earlier install wrote `~/.copilot/hooks/wakeshift.json`, the plugin removes it the first time it runs. |
| opencode | Wakeshift's plugin, `@wakeshift/opencode`, in the `plugin` list of `~/.config/opencode/opencode.json`. An earlier install wrote its own plugin file, `~/.config/opencode/plugins/wakeshift.ts`, instead; the npm plugin removes that file when it first loads. |
| pi | Wakeshift's extension, `@wakeshift/pi`, in the `packages` list of `~/.pi/agent/settings.json`. An install with the script writes its own extension file, `~/.pi/agent/extensions/wakeshift.ts`, instead; the npm package removes that file when it first loads. |
| Antigravity CLI | Wakeshift's plugin, in `~/.gemini/config/plugins/wakeshift`. It takes out the `wakeshift` entry an earlier install added to `~/.gemini/config/hooks.json`, so the hooks don't run twice. |
| OpenClaw | Wakeshift's plugin, installed and enabled by OpenClaw, with its settings in `plugins.entries.wakeshift`. When a conversation connects, the plugin turns on OpenClaw's hooks endpoint (`hooks.*` in `openclaw.json`) with a token of its own, and records what it changed in `~/.openclaw/wakeshift`. |
| Hermes Agent | Wakeshift's plugin, as above. |

The wait command is how an agent that's woken by a command in the background (Cursor, Antigravity CLI, Copilot CLI) waits for occurrences once Wakeshift is installed: `~/.wakeshift/bin/wakeshift wait`, written with your home folder's full path, for example `/Users/ada/.wakeshift/bin/wakeshift wait`. It's the same command in every conversation, takes no arguments, and has no private address in it. Cursor and Antigravity CLI ask before it starts (in Antigravity CLI, allowing it in the conversation is enough), and in Copilot CLI, Wakeshift's hooks approve it, so it doesn't ask. If you deny it in Copilot CLI (`--deny-tool shell`, or a `--deny-tool` rule for the command), or your organization's Copilot policy does, the hooks don't approve it, and your rule applies. opencode, pi, Hermes Agent and OpenClaw don't ask, and Codex is woken by its hook. Before Wakeshift is installed, these agents wait with a longer command, and their program asks before each start.

## Fewer approval prompts for your tools

Most approval prompts in a long conversation are for your own tools, not for Wakeshift: the agent's program asks before each call to a tool it hasn't been told to trust. Three things help:

- Mark tools that only read with the MCP annotation `readOnlyHint: true`. Codex, in its default mode, doesn't ask before calling such a tool. Wakeshift's own `list_subscriptions` is marked.
- Name the tools that are safe to run unasked in your manifest's `preapprove_tools` (below). Wakeshift then offers them to the user, by name, when it connects Claude Code or Cursor CLI.
- On your app's own "connect your agent" page, you can suggest rules that allow your tools by name, for the server name the user gave your app. Name each tool; don't suggest a rule for the whole server unless every tool on it is safe to run unasked.

### `preapprove_tools`

List the tools by the names your MCP server gives them (letters, digits, `_` and `-`; at most 20):

```json
{ "description": "…", "preapprove_tools": ["list_channels", "history", "post"] }
```

`wakeshift.Manifest.PreapproveTools` in Go, `preapprove_tools` in TypeScript and Python. List tools that only read, and routine ones the agent calls on every wake that reach only what it already joined (Agent Chat's `post` writes only to channels the conversation joined, and joining still asks). Never list a tool that deletes something, spends money, or reaches people the user hasn't chosen.

When the user connects Cursor CLI, the install question also asks to allow those tools ("…; it also lets Cursor run Agent Chat's list_channels, history and post tools without asking each time"). Claude Code's plugin install can't carry that answer, so there, and wherever the hooks are installed already, a conversation the hooks newly connect to your app asks the same, until the user has answered once. With a yes, Wakeshift's hooks add one exact rule per tool, for the name the user gave your server:

| Agent | Where | The rule (server `agentchat`, tool `post`) |
| --- | --- | --- |
| Claude Code | `permissions.allow` in `~/.claude/settings.json` | `mcp__agentchat__post` |
| Cursor CLI | `permissions.allow` in `~/.cursor/cli-config.json` | `Mcp(agentchat:post)` |

The hooks learn the server's name from the agent's own program when it connects a conversation to your app, and write rules only if its MCP settings agree that the name is one server. They never write a rule for a tool the user wasn't asked about, a tool your manifest stops naming, or a whole server. `~/.wakeshift/bin/wakeshift allow --harness claude-code --app <your app> --none` removes them; uninstalling removes them all. Nothing is written for the other agents: Antigravity CLI's tool rules aren't confirmed to match one tool exactly, Copilot CLI has no rules that last beyond one folder, and Codex asks once per tool for a whole session (and never for `readOnlyHint` tools).

### Rules the user adds

For the other agents, or tools you don't list, your own connect page can show the user the rule for each tool:

| Agent | Where the user adds it | A rule for one tool (server `myapp`, tool `post`) |
| --- | --- | --- |
| Claude Code | `permissions.allow` in `~/.claude/settings.json` | `mcp__myapp__post` |
| Codex | `~/.codex/config.toml` | `[mcp_servers.myapp.tools.post]` with `approval_mode = "approve"` |
| Cursor CLI | `permissions.allow` in `~/.cursor/cli-config.json` | `Mcp(myapp:post)` |
| Antigravity CLI | `permissions.allow` in `~/.gemini/antigravity-cli/settings.json` | `mcp(myapp/post)` |
| GitHub Copilot CLI | `copilot --allow-tool 'myapp(post)'` when it starts | `myapp(post)` |

Beyond `preapprove_tools`, Wakeshift adds no rules for your tools: suggesting them, and the user adding them, is up to you and them.

## Trust steps

| Agent | After the install | After a later restart |
| --- | --- | --- |
| Codex | Quit Codex, start it again with `codex resume --last` and choose "Trust all and continue" to trust the new hook, then send any message so the agent connects the conversation. In the ChatGPT desktop app's Codex mode, the hooks don't run until you trust them: in the composer, click "Review hooks", then "Allow all", and send any message. | Once the thread is open again, send any message. |
| Claude Code | Nothing to approve, but Claude Code loads a newly installed plugin only when a session starts: type `/reload-plugins` (or restart with `claude --continue`), then send any message so the agent connects the conversation. Until then, the conversation is woken the way it was before the install. | Nothing: it reconnects by itself. |
| Cursor | Nothing to approve. Restart `cursor-agent`, reopen the chat (`cursor-agent --continue`) and send any message. | Reopen the chat and send any message. |
| GitHub Copilot CLI | Restart `copilot`, reopen the session (`copilot --continue`) and send any message. | Reopen the session and send any message. |
| opencode | Restart `opencode`, reopen the session (`opencode --continue`) and ask the agent something that uses the app, such as checking for anything new. A message that doesn't use the app doesn't connect the conversation. | Nothing: it reconnects by itself. |
| pi | Nothing to approve: type `/reload` in pi (or restart it with `pi --continue`), then ask the agent something that uses the app, such as checking for anything new. A message that doesn't use the app doesn't connect the conversation. | Nothing: it reconnects by itself. |
| Antigravity CLI | Restart `agy`, reopen the conversation (`agy --continue`) and send any message. | Reopen the conversation and send any message. |
| OpenClaw | Nothing to approve, and no gateway restart, whether OpenClaw runs the turns itself or in Codex. The plugin connects the conversation the next time the agent uses your app's tools; messages that arrive before then wait, and are delivered once it's connected. | Nothing: it reconnects by itself when the gateway starts. |
| Hermes Agent | Nothing to approve, but Hermes loads plugins only when a chat starts or is resumed, so Wakeshift can't wake a chat that was running during the install until you resume it: exit and run the `hermes --resume <session>` command Hermes prints as it exits, or `hermes chat --continue` (`hermes chat --tui --continue` in the TUI; through Hermes's gateway or API server, restart the gateway and continue the session), then send any message. A new chat works too. If the installer couldn't enable the plugin, run `hermes plugins enable wakeshift` where Hermes runs. | In `hermes chat`, send any message once you've resumed it; conversations through Hermes's API server reconnect by themselves, and a Telegram or Discord direct message reconnects with its next message. |

## Updates

Wakeshift's hooks and plugins report their version. When Wakeshift needs a newer one, your tool's result tells the agent to ask its user to update, with the same install command; until then, events are held for your tools' results. Running the install again replaces the hooks or plugin and keeps the agent's settings and connected conversations. For Codex, the update is Codex's own plugin commands again: `codex plugin marketplace add wakeshift/agent-plugins && codex plugin marketplace upgrade wakeshift && codex plugin add wakeshift@wakeshift`. Codex then asks you to trust the plugin's hooks again: restart it with `codex resume --last` and choose "Trust all and continue" (in the ChatGPT desktop app's Codex mode, click "Review hooks" in the composer, then "Allow all"). Wakeshift updates Codex's plugin only when the plugin has to change. For Copilot CLI, the update is `copilot plugin update wakeshift`, then a restart. For opencode, the update is opencode's plugin command with the new version; restart opencode afterwards. For pi, the update is pi's package command with the new version; type `/reload` in pi afterwards. Claude Code's plugin updates with `claude plugin marketplace update wakeshift && claude plugin update wakeshift@wakeshift`, then `/reload-plugins`. For Antigravity CLI, the update is the install command again: agy replaces the installed plugin, with nothing to approve, then restart agy. For OpenClaw, that's the same `openclaw plugins install` command, which also replaces a plugin installed by the earlier `openclaw.sh` installer. For Cursor, the update is the npx command with the new version, and there's nothing to restart.

Wakeshift's hooks connect a conversation only from the tool result Wakeshift made the connection for, so a message someone else posts, which a read tool shows the agent, can't connect your conversation to their session. Wakeshift also keeps a connected conversation where it is: a request to move it to another person's session in the same app is refused. Your tools need the current SDK for these hooks to connect from their results.

## Uninstall

To turn Wakeshift's hooks off without uninstalling anything, run:

```sh
~/.wakeshift/bin/wakeshift disable
```

This works for Claude Code, Codex, Cursor, GitHub Copilot CLI and Antigravity CLI, through their plugins or the hooks the install command added, in every session on this computer, including ones already running. Every hook then exits at once and does nothing: no network request, no download, no output. Nothing wakes a conversation, and a Codex thread's listener stops before its next wake. Conversations that were connected keep their link, and their messages wait: run `~/.wakeshift/bin/wakeshift enable` to turn the hooks on again, and a conversation gets what waited once you next send it a message.

To turn them off for one run of an agent instead, start it with `WAKESHIFT_DISABLED=1` (or `true`) in its environment, for example `WAKESHIFT_DISABLED=1 claude`. Any other value, or none, leaves the hooks on. The variable reaches only hooks that run in the agent's own environment, so it doesn't work for Codex, whose threads run in a background server that the first `codex` you started keeps running, or for the Cursor app when it's already open: use `wakeshift disable` for those. A hook that was already running when you started the agent carries on until it ends.

To remove Wakeshift, first remove its plugin with the agent's own commands (the table below), then run:

```sh
~/.wakeshift/bin/wakeshift uninstall --harness <agent>
```

`<agent>` is `codex`, `claude-code`, `cursor`, `copilot`, `opencode`, `pi`, `antigravity` or `hermes`. It removes Wakeshift's hooks (for Hermes, its plugin), the rule for the wait command that the install added (a rule that was there before the install stays), and Wakeshift's files for that agent in `~/.wakeshift`. Once no other agent on the computer uses Wakeshift, it also removes Wakeshift's program, `~/.wakeshift/bin/wakeshift`, and then `~/.wakeshift` if nothing else is in it. While another agent still uses Wakeshift, the program stays, and the command says which agent uses it. Remove the plugin first: a plugin that's still installed downloads the program again the next time its hooks run.

| Agent | First, run | What stays, and why |
| --- | --- | --- |
| Claude Code | `claude plugin uninstall wakeshift@wakeshift; claude plugin marketplace remove wakeshift`, then type `/reload-plugins` in any running session | Claude Code keeps a copy of the plugin in `~/.claude/plugins/cache/wakeshift`, marked as no longer in use, and deletes it itself later. |
| Codex | `codex plugin remove wakeshift@wakeshift; codex plugin marketplace remove wakeshift` | Codex's record that you trusted the plugin's hooks: the `[hooks.state."wakeshift@wakeshift:…"]` entries in `~/.codex/config.toml` and, for some installs, a file in `~/.codex/.hook-trust`. Codex has no command that removes them, and they do nothing once the plugin is gone. You can delete the `hooks.state` entries yourself. |
| GitHub Copilot CLI | `copilot plugin uninstall wakeshift; copilot plugin marketplace remove wakeshift` | Copilot's record that the plugin is turned off: `"wakeshift@wakeshift": false` in `~/.copilot/settings.json`. Copilot has no command that removes it, and it does nothing once the plugin is gone. You can delete that line yourself. |
| Antigravity CLI | `agy plugin uninstall wakeshift` | Nothing of Wakeshift's. |
| Cursor | Nothing: the command above removes the hooks | Nothing of Wakeshift's. |
| opencode | opencode has no command that removes a plugin. Delete the `@wakeshift/opencode@…` entry from the `plugin` list in `~/.config/opencode/opencode.json`, then restart opencode. | opencode's own copy of the package, in `~/.cache/opencode/packages/@wakeshift`, which opencode doesn't remove: delete that folder, and the folder `~/.wakeshift/opencode`. The command above is needed only for an earlier install's plugin file, which it removes. |
| pi | `pi remove npm:@wakeshift/pi`, then type `/reload` in pi | The folder `~/.wakeshift/pi`: delete it. The command above is needed only for the extension file an install with the script wrote, which it removes. |
| Hermes Agent | Nothing. `hermes plugins remove wakeshift` works instead of the command above, without Wakeshift's program. | After `hermes plugins remove`, the plugin's state and the setting that let it start turns: see [Hermes Agent](/docs/hermes#uninstall). |
| OpenClaw | Instead of the command above, `openclaw wakeshift reset && openclaw plugins uninstall wakeshift --force`, where the gateway runs | Nothing of Wakeshift's. |

For OpenClaw, the reset puts OpenClaw's hooks settings back as the plugin found them (its token goes with them), removes your agreement to hooks on a gateway that listens beyond this computer, and deletes `~/.openclaw/wakeshift`; OpenClaw's uninstall then removes the plugin and its settings. A plugin installed by the installer script, before the reset command existed, can be updated first, or removed with `curl -fsSL https://app.wakeshift.ai/install/openclaw-uninstall.sh | sh`.

**Backups.** Each time Wakeshift changes a settings file, it first saves a copy of it next to the file, named after it with `.before-wakeshift-` and the date and time, for example `~/.claude/settings.json.before-wakeshift-20261109-170548`. The uninstall keeps them, so you can go back to an earlier version; delete them when you no longer need them. A settings file is deleted only if the install created it and nothing else has been added to it since.

To remove everything Wakeshift keeps on the computer at once, for every agent, delete `~/.wakeshift` after removing each agent's plugin and hooks. That doesn't remove the hooks and rules in your agents' settings, or the backups next to them.

## Agents Wakeshift doesn't know

An agent Wakeshift doesn't recognize is treated as one it [can't wake](#agents-that-cant-be-woken): its result says so in one line, with the conversation's session key, and asks for nothing else.

## Agents that can't be woken

Some MCP clients have no way to be woken from outside. When one of your tools subscribes such a conversation, the result tells the agent that nothing will notify it there and not to say it's watching ("Wakeshift can't wake this agent, so nothing will notify you here when something arrives. Tell the user so plainly, and don't say you're watching or that you'll let them know."), so it doesn't promise the user a notification, and gives it the session key to pass to your tools. Nothing asks it to call a tool again: your own tools are how it checks on the work, exactly as without Wakeshift. What arrives for the conversation waits for it, and reaches it if a listener connects later (an install, a restarted Monitor); otherwise it expires. No result of your tools carries held occurrences, and Wakeshift changes no tool or result you don't wrap.

A listener survives the computer going to sleep or offline: when it's back within a day, the listener carries on where it left off and gets what arrived meanwhile. After longer, an agent with Wakeshift's plugin or hooks reconnects with the next result of your tools it gets, and a Claude Code monitor is set up again the next time the agent watches.

Wherever a listener does deliver, delivery is at least once: an occurrence the agent may already have seen (its connection dropped before it confirmed one, say) comes again with `"redelivered": true` and the same `id`.
