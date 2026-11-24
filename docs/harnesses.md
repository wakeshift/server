# How each harness is woken

An agent harness is the program a conversation runs in: Claude Code, Codex, Cursor and so on. Few of them can be woken from outside, and each one that can offers a different way in. This page says how the server reaches each one, what runs on the user's machine to do it (the bridge), and which versions it was tested on.

Code: `internal/harnesses` (what the server knows about each harness: names, wake path, install offer), `internal/appapi` (the server's side), `internal/streams` (stream links, waits and polls), and on users' machines the connector in [wakeshift/connectors](https://github.com/wakeshift/connectors) (`cmd/wakeshift-connector`, `internal/connector`, `internal/codexconnector`). The published copy of those packages is also in this repository, since the server builds and serves the connector. The developer-facing summary is [wakeshift.ai/docs/agents](https://wakeshift.ai/docs/agents).

## The model

Every bridge follows the same pattern:

1. **An app's tool subscribes the conversation.** The tool's result tells the agent how it will be woken. Where that needs a one-time install, the result says so; the agent asks the user and runs the install only if they agree. The install trusts exactly one server, the one that offered it.
2. **The result carries a beacon.** A beacon is a short JWS, signed with the server's OAuth keys, holding a one-time ticket (valid for a minute) and naming the conversation or the tool call it answers, the app and the tool. The harness hands the result to a hook, and the hook acts only on a beacon signed by the server it trusts, for its own conversation or call, from the tool the beacon names. It redeems the ticket (`POST /v1/connector/redeem`) for the conversation's own stream link, and says "Wakeshift: connected" in the conversation.
3. **The bridge listens and hands messages over.** It long-polls the link (`GET /v1/stream/poll`), or holds a stream, and hands each message to the harness through whatever non-user channel it offers: a system reminder, a tool output, a system message. It acknowledges a message only once it's handed over (`POST /v1/stream/ack`); anything unacknowledged comes back.

Two things vary per harness and decide everything else:

- **Where the beacon can travel.** Codex and opencode give hooks the MCP result with its `_meta`. Claude Code doesn't, but writes `_meta` to its transcript next to the tool call ID. Cursor, Copilot CLI and Antigravity CLI give hooks only the result's text, so there the beacon is the result's last line; Cursor's and Copilot's hooks remove it before the model sees it.
- **How the beacon binds to the conversation.** The server can name only what the harness tells it: the conversation (a Codex thread, an Antigravity conversation), the tool call (Claude Code), or nothing (opencode). Where it's nothing, the harness handing the result to the calling conversation's hook is what binds it.

Harnesses with no hook support at all are woken without an install, by a command the agent itself runs in the background, which ends or prints when a message arrives ("Background commands", below).

## Rules every bridge follows

- **Never deliver as the user.** Messages are framed as information from outside the conversation, in a channel the harness doesn't attribute to the user.
- **Never interrupt a running turn.** Deliver between tool calls or after the turn.
- **Acknowledge honestly.** A message counts as delivered when the harness has it (handed to a hook, a turn started), and comes again otherwise. Delivery is at least once; a repeat carries `"redelivered": true` and the same ID.
- **Ask before installing anything,** and record the install. Each installer backs up every file it edits, keeps everything else as written, and its uninstall removes only what it added.
- **Trust one server.** A hook trusts only the issuer pinned at install. It never follows a redirect with a ticket or a link, and dials a stream link only on that issuer's own origin.
- **No secret in a command.** No command the server gives an agent holds a stream link: loops read the link from a file the agent writes with its file tool, so an approval the harness saves for the command holds no secret.

## Recognizing the harness

The server tells a call's harness from three signals, in order (`harnesses.Recognize`), the first that names one winning:

1. **The client name** (`clientInfo.name` from `initialize`).
2. **A `_meta` key only that harness sends:** Claude Code `claudecode/toolUseId`, Codex `x-codex-turn-metadata.thread_id`, Antigravity `antigravity.google/conversation_id`, Copilot CLI `wakeshift/conversation` (added by Wakeshift's hook).
3. **A product token in the User-Agent:** `claude-code`, `codex-mcp-client`, `opencode`, `Cursor`, `copilot-cli`.

The fallbacks exist for stateless MCP servers, which never see `initialize`. Where calls look alike (Grok Bot and Cursor's cloud agents call as Cursor does), the subscribe tool takes an optional `harness` argument the agent fills in, used only to choose between the harnesses the call could be.

Every signal is self-asserted. A spoofed name gets nothing that stating that harness's name wouldn't. What protects a conversation is the binding (a beacon names its conversation or call, the hook checks it, and the server refuses a redeem for any other), not the recognition.

## Per harness

| Harness | How it's woken | The bridge | Acknowledged |
| --- | --- | --- | --- |
| **Claude Code** | Wakeshift's plugin: an `asyncRewake` hook waits on the conversation's link and exits with the message, which wakes an idle session within a second as a system reminder. Without the plugin: a Monitor the agent starts, running the conversation's wait in a loop; or, where the client takes MCP tasks and the app turns it on, Wakeshift's wait tool answered with a task | [agent-plugins](https://github.com/wakeshift/agent-plugins) (`plugins/claude-code`), or nothing | On hand-over |
| **Codex** | A hook reads the beacon from the result's `_meta` and starts a per-thread connector, which starts a turn through Codex's app server with the message as a tool output, once the thread is idle. For the Codex desktop app's threads, through the app's IPC to the window that owns the thread, or a background app-server turn | [agent-plugins](https://github.com/wakeshift/agent-plugins) (`plugins/codex`) | When the turn starts |
| **Claude Cowork** | Claude Code's Monitor loop inside the task's VM. Plugin hooks don't run in Cowork tasks, so there's no install. The workspace must allow the server's host in its network settings | Nothing | Confirmed by the loop's next round |
| **Cursor** (CLI and IDE) | A background command the agent starts, which ends when messages arrive and wakes the conversation. Hooks are an optional extra that deliver when a turn ends | Nothing, or `npx @wakeshift/connect cursor`, or the Cursor Marketplace plugin | On hand-over |
| **GitHub Copilot CLI** | A background command, as Cursor's; hooks are optional and approve the wait command | Nothing, or [agent-plugins](https://github.com/wakeshift/agent-plugins) (`plugins/copilot`) | On hand-over |
| **Antigravity CLI** | A background command, as Cursor's | Nothing, or [antigravity-plugin](https://github.com/wakeshift/antigravity-plugin) | On hand-over |
| **opencode** | Wakeshift's plugin starts a prompt on `session.idle`. Or, from opencode 2.4 (2.3 with its `mcp_events` flag) and with no plugin, MCP Events push: opencode subscribes natively and the open session wakes. Its default install is still 1.x, which has neither | `@wakeshift/opencode` on npm | Once opencode stores it |
| **pi** | Wakeshift's extension starts a run while pi is idle, with the messages in that run's system prompt; while pi is busy, it waits for the turn to end | `@wakeshift/pi` on npm | When the run that carries it starts |
| **OpenClaw** | Wakeshift's plugin holds the stream and starts a session-targeted turn through OpenClaw's hooks endpoint, whether OpenClaw runs the turn itself or in Codex | `@wakeshift/openclaw` on npm | Once the woken turn has run |
| **Hermes Agent** | Wakeshift's Python plugin holds the stream and starts a run in the same chat while it's idle (`/v1/runs` or `inject_message`), only once the user allows it (`allow_gateway_injection`) | [hermes-plugin](https://github.com/wakeshift/hermes-plugin) | When Hermes accepts the run |
| **ChatGPT** (Work chats) | Its own MCP Events webhooks; each occurrence starts a run in the chat | Nothing | On ChatGPT's 2xx |
| **ChatGPT desktop, Codex mode (cloud)** | A wait the agent runs in the foreground, which keeps the chat busy; each wait ends within 90 seconds and the agent starts the next | Nothing | Confirmed by the next wait |
| **Capy** | The agent creates a webhook watch on its own thread and registers its URL with the server, which POSTs each message there; Capy wakes the thread even when idle. Falls back to a one-shot background command | Nothing | When Capy answers 2xx |
| **Muse** (Meta) | A background task on the sandbox VM waits once, prints what arrived and finishes; the finished task's output starts a turn | Nothing | By the command, once printed |
| **Grok Bot** | Once per bot, the agent sets up an Automations routine with a webhook trigger, and the user pastes its URL and key on a one-time page; each batch of messages starts a run. Until then, a foreground wait | The connector, in the bot's machine | When the routine answers |
| **Cursor cloud agents, Goose, Cline, unknown clients** | Not woken. The result tells the agent that nothing will notify it, and the agent checks with the app's tools, as it would without Wakeshift | — | — |

### Claude Code

Monitor is the zero-install path. The first time a conversation subscribes, the agent asks the user how to be notified: in this session only (nothing installed), in every session (the plugin), or not at all. Nothing runs before the answer, and what arrives meanwhile is held.

- **The plugin.** Hooks on `SessionStart` and `Stop` run a waiter with `asyncRewake: true` and a 24-hour timeout (without an explicit timeout Claude Code kills it at 600 seconds). The waiter long-polls; on a message it writes it to stderr and exits 2, which wakes an idle session, or reaches a busy one between tool calls. One waiter per Claude process; a newer one makes the older stop, which covers `/clear`. A `PostToolUse` hook reads the beacon from the transcript (Claude Code doesn't give hooks `_meta`) and connects the conversation. Each wake is labeled "Woken by Wakeshift" in the terminal.
- **The Monitor.** Claude Code caps every Monitor at 30 minutes, so the loop ends itself after 25 and prints a line telling the agent to start it again with the same command. Each round confirms exactly what it printed, by passing the cursor back, so a Monitor that died stops confirming and what it took comes again.
- **Agent SDK hosts.** Claude Code run through the Agent SDK runs `asyncRewake` hooks in the background, so the waiters wake idle SDK sessions too. `claude -p` with a text prompt runs them as blocking hooks, so there Wakeshift's hooks do nothing.
- **MCP tasks.** Where Claude Code turns MCP tasks on and the app's server speaks protocol 2026-07-28, a call to Wakeshift's wait tool answered with a task starts a turn when the task completes: no install, no 30-minute limit, and it survives `/compact` and `claude --continue`.
- Claude Code is never treated as a native MCP Events client, whatever it declares: it doesn't act on events.

### Codex

Codex delivers nothing from MCP notifications or `events/stream` to the model, and a background command that finishes doesn't wake an idle thread. What works is Codex's own app server: a connector starts a turn with the message as a tool output (`turn/start` with `toolOutput`), which the model sees as a tool result, never as the user.

- The hook runs on `PostToolUse` (matcher `^mcp__`), `SessionStart` and `UserPromptSubmit`. It reads the beacon from the result's `_meta`, redeems it, and starts the connector detached; on later prompts it restarts a stopped connector with the link it kept.
- The connector stops when the window showing the thread closes or moves on (read from Codex's log), when the thread is archived or deleted, when the link is revoked, or when nobody has used the thread for an hour and it no longer holds the stream.
- Codex asks the user to trust new or changed hooks. After an install, the user restarts Codex in the conversation (`codex resume --last`) and chooses "Trust all and continue".
- A model failure leaves a thread in `systemError`. The connector starts a turn there too, and acknowledges the batch only when a turn ends `idle`, retrying with backoff.

### Background commands

Cursor, Copilot CLI, Antigravity CLI, Capy and Muse can't be woken by a hook while idle, but each wakes the conversation when a background command the agent started prints or finishes. The subscribing result gives the agent one command, the same in every conversation: before the install, a shell loop around `GET /v1/stream/wait` that reads the link from a file; after it, `~/.wakeshift/bin/wakeshift wait`. The harness asks before running it (Copilot's hooks approve exactly that command). The command confirms what it printed, so a message is lost only if the harness fails between reading the output and acting on it.

### opencode, pi, OpenClaw and Hermes Agent

These run plugins or extensions in-process, which hold the conversation's link themselves (opencode's and pi's through the connector's bridge) and start a turn through the harness's own API while it's idle. Each plugin trusts `https://app.wakeshift.ai` by default and takes a list of trusted servers in its own settings ([self-hosting](self-hosting.md#point-the-sdks-cli-and-plugins-at-your-server)).

OpenClaw and Hermes Agent can also subscribe natively with MCP Events, which starts a new, isolated run for each event rather than waking the chat the user has open. Pick the path for the job: MCP Events for an automation, the plugin for the conversation.

## Results

The harness runner that produced these is in [bench/](../bench/README.md): it drives real Claude Code, Codex and the other terminal harnesses, each in a temporary home directory, against a server built from this checkout. Its pins are `bench/harness-pins.json`.

### Release checks

Each new release of Claude Code and Codex runs the bench's scenarios (subscribe, idle wake with and without the install, the install yes and no, bursts, a killed listener, unsubscribe and subscribe again) before the bench moves to it. The latest that passed, as of November 23, 2026 (`bench/harness-pins.json` always has the current pins):

| Harness | Latest passed | Oldest supported |
| --- | --- | --- |
| Claude Code | 2.1.331 | 2.1.280 (before 2.1.292, its plugin installs in two steps) |
| Claude Agent SDK | 0.3.323 | |
| Codex | 0.169.0 | 0.163.0 |
| opencode | 1.19.5 (2.4 and later subscribe natively, through MCP Events push) | |

### Verified per harness

The version each harness's wake path was verified on, against the real binary: its client name captured from a logging MCP server, hook inputs captured from real sessions, and the install and wake checked end to end.

| Harness | Version | Client name | Verified with |
| --- | --- | --- | --- |
| Codex | 0.163.0 to 0.169.0 | `codex-mcp-client` | Real binary and model |
| Claude Code | 2.1.280 to 2.1.331 | `claude-code` | Real binary and model |
| Claude Cowork | Claude desktop, November 2026 | `claude-ai` (shared with claude.ai chat) | Real app, assisted |
| Cursor | cursor-agent 2026.10.22; IDE 3.25.9 | `Cursor`, `cursor-vscode` | Real binary and model (CLI); IDE from its docs |
| GitHub Copilot CLI | 1.0.105 | `copilot-cli` | Real binary and model |
| opencode | 1.19.3 | `opencode` | Real binary, free model |
| pi | 1.5.0 | `pi` | Real binary and model |
| Antigravity CLI | 1.4.12 | `antigravity-client` | Real binary and model |
| OpenClaw | 2026.10.4 | `openclaw-bundle-mcp`; `codex-mcp-client` when it runs turns in Codex | Real gateway in Docker, both runtimes |
| Hermes Agent | 0.22.5 | `hermes-agent` | Real image and model |
| ChatGPT desktop, Codex mode | November 2026 | `openai-mcp (Codex)` | Probe server; the wait in tests |
| Capy | November 2026 | `capy` | Probe server; the webhook against a stand-in in tests |
| Muse | November 2026 | `muse-custom-connector` | Probe server and a real wake |

### Wake times

From the `emit` to the agent's first words in the woken conversation, on the bench, idle sessions with the install:

| Harness | p50 | p90 |
| --- | --- | --- |
| Claude Code | 5.4 s | 6.3 s |
| Codex | 4.2 s | 6.1 s |

The server's own part is about 0.2 seconds; the rest is the harness starting its turn and the model replying. The other harnesses that wake while idle take seconds too; Muse took about 15.

### What models do with the offer

Whether a conversation gets woken depends on the model agreeing to set it up. In runs where a realistic user asked for real work and never mentioned Wakeshift, the harnesses subscribed through the app's own tool in nearly every run. Whether the agent then offered the install depended on the wording: Claude Code and Codex set an unexplained offer aside as unrequested, and asked nearly every time once the tool's description said it was how the app notifies the agent and the result answered why. Antigravity CLI, opencode and Cursor CLI followed the offer in nearly every run. Once woken, the harnesses named the event and finished the task they were on.

## Not built

| Harness | Why |
| --- | --- |
| Goose | `PostToolUse` gets no tool result, so no beacon can reach a hook |
| Cline | No session ID reaches MCP servers; the VS Code extension has no stop hook |
| Amp | Its client name and tool result shape are undocumented |
| Windsurf, Devin | Cascade has no hook; Devin's stop hook would work with a text beacon |
| VS Code Copilot (agent mode) | A stop hook with a text beacon would work; not built |

## Asks of harness vendors

- **Cursor:** for `notify_on_output`, notify a match at once when nothing was notified in the last debounce window, or honor a `debounce_ms` below 5000: that window is about 5 seconds of every idle wake. For cloud agents, API keys scoped to one agent.
- **Google (Antigravity):** give hooks a token that lets a hook wake its own conversation.
- **Cursor and xAI (Grok Bot):** give the agent its routine's webhook URL and key when it creates one (today only the user sees them, so the setup needs a page), or a supported way to send a message into a Grok Bot conversation with a credential scoped to it.
- **Every harness:** a client for MCP Events that delivers an event into the conversation that subscribed would make every bridge on this page unnecessary for it.
