# Wakeshift server

Wakeshift lets an MCP server wake the AI agents that use it.

Your MCP server has events: a build finished, a reply arrived, a job is done. MCP Events (SEP-3415) is the standard way to offer them, and few agents subscribe to it yet (opencode 2.4 does): Claude Code, Codex, Cursor and most others never hear about your event. This server closes that gap. Run it next to your MCP server and:

- **Clients that speak MCP Events get the standard.** The server holds each subscription and delivers every occurrence by webhook, poll or push (`events/stream`), with the per-client compatibility today's clients need ([docs/mcp-events.md](docs/mcp-events.md)).
- **Clients that don't are woken anyway.** Claude Code, Codex, Cursor, GitHub Copilot CLI, opencode, pi, Antigravity CLI, OpenClaw, Hermes Agent and others are woken in the conversation that subscribed, through a bridge built for each one ([docs/harnesses.md](docs/harnesses.md)).
- **Webhooks become events.** Point GitHub, Stripe, CI or any other webhook sender at a publish URL, and each POST reaches every subscribed agent, by push or by wake. You write no handler.

Your code is the same for every agent. The SDKs attach to the MCP server you already have.

## Run it

You need Docker. In a clone of this repository:

```sh
docker compose run --build --rm --no-deps --entrypoint /appkey server -app example
export WAKESHIFT_APPS='<the JSON after WAKESHIFT_APPS= in what it printed>'
export WAKESHIFT_SECRET_KEY=$(openssl rand -base64 48)
docker compose up --build
```

The first command makes your app's key and prints two lines:

- `WAKESHIFT_KEY=wst_…`: your app's private key. Put it in your MCP server's environment, with `WAKESHIFT_CORE=http://localhost:8370`, and keep it out of version control.
- `WAKESHIFT_APPS=[…]`: the app's public key, which registers it with the server.

`docker compose up` then starts Postgres 17 and the server at `http://localhost:8370`, both on this machine's loopback only. The server migrates the database at startup, and MCP Events push is on. To sign in to the console on your own machine, use `make run` instead ([self-hosting](docs/self-hosting.md#signing-in-to-the-console)); to run the server anywhere else, see [docs/self-hosting.md](docs/self-hosting.md).

### 1. Attach the SDK

In your MCP server (TypeScript here; [Python](https://github.com/wakeshift/wakeshift-python) and [Go](https://github.com/wakeshift/wakeshift-go) work the same way):

```sh
npm install @wakeshift/sdk @modelcontextprotocol/server zod
```

```ts
import { Wakeshift } from "@wakeshift/sdk"
import { z } from "zod"

const wake = new Wakeshift() // reads WAKESHIFT_KEY and WAKESHIFT_CORE from the environment

export const buildFinished = wake.defineEvent("build.finished", {
  description: "A CI build finished",
  args: z.object({ repo: z.string() }),
  payload: z.object({ status: z.string() }),
  public: true, // open to subscribe_events and to MCP Events clients
})

const attached = wake.attach(server) // your McpServer
```

`attach` answers `events/list`, `events/subscribe`, `events/poll` and `events/stream`, and adds a `subscribe_events` tool for agents whose clients don't speak MCP Events. Tools of your own can subscribe the conversation that called them too ([events](https://wakeshift.ai/docs/events)), and [who may subscribe](https://wakeshift.ai/docs/who-may-subscribe) covers `personal`, `authorize` and the default, where only your tools subscribe.

### 2. Point your webhooks at it

Make a publish URL for the event, and give it to the service that sends the webhooks:

```ts
const { url } = await buildFinished.createPublishUrl({ repo: "api" })
```

Each POST to `url` emits one occurrence, with the body as its payload. Or emit from your own code: `await buildFinished.emit({ repo: "api" }, { status: "passed" })`. [Publish URLs](https://wakeshift.ai/docs/publish-urls) covers signed senders (Svix and Standard Webhooks), retries and revoking.

### 3. Get push, and wake an agent

- **A client that speaks MCP Events** (Mastra, opencode 2.4 and later) subscribes to `build.finished` itself and gets each occurrence on its `events/stream` request, in order, with no public URL. A native subscription to a `public` event belongs to a user, so it needs your server's sign-in (`attach`'s `identify`); without sign-in, a tool of yours hands the client a one-time voucher instead ([who may subscribe](https://wakeshift.ai/docs/who-may-subscribe)).
- **Claude Code** subscribes with `subscribe_events` when you ask it to watch the build, and wakes in the same conversation when the webhook arrives, with nothing installed. Codex, Cursor and the others wake the way [docs/harnesses.md](docs/harnesses.md) describes; their one-time installs trust only your server, once it serves the connector or a plugin marketplace rendered for it ([self-hosting](docs/self-hosting.md#the-connector-and-installs)).

Each wake arrives marked as information from your app, never as the user speaking.

## Which agents it wakes

| Agent | How it's woken | What the user installs |
| --- | --- | --- |
| Claude Code | Wakeshift's plugin (hooks that wake an idle session), or a background Monitor the agent starts | Nothing, or the plugin |
| Codex | A hook that starts a per-thread connector, which starts a turn through Codex's app server | The plugin, once |
| Cursor, GitHub Copilot CLI, Antigravity CLI | A background command the agent starts, which ends when an event arrives | Nothing (hooks are optional) |
| opencode | Wakeshift's plugin, or MCP Events push from opencode 2.4 (its default install is still 1.x, which has none) | The plugin, once, or nothing on 2.4 and later |
| pi | Wakeshift's extension | The extension, once |
| OpenClaw, Hermes Agent | Wakeshift's plugin, which starts a run in the same chat; or their own MCP Events webhooks, which start a new run | The plugin, once, or nothing |
| Claude Cowork | A background Monitor inside the task | Nothing |
| ChatGPT (Work chats), Mastra, TanStack AI, Vercel AI SDK | Their own MCP Events support | Nothing |

[docs/harnesses.md](docs/harnesses.md) has every harness, the bridge for each, and the versions tested. To reproduce the matrix with real agents on your machine, see [bench/README.md](bench/README.md).

## SDKs and plugins

Everything an app or an agent's machine runs is open source too, under the same license:

- **SDKs:** [TypeScript](https://github.com/wakeshift/wakeshift-node) (`@wakeshift/sdk`), [Python](https://github.com/wakeshift/wakeshift-python) (`wakeshift`), [Go](https://github.com/wakeshift/wakeshift-go). Any other language works over the [HTTP API](https://wakeshift.ai/docs/http-api).
- **The connector and the bridges:** [wakeshift/connectors](https://github.com/wakeshift/connectors): the `wakeshift` program with its hooks for each harness, the OpenClaw plugin, the npm packages for opencode, pi and Cursor, and `wakeshift-cli`.
- **Plugins:** [wakeshift/agent-plugins](https://github.com/wakeshift/agent-plugins) (Claude Code, Codex, Copilot CLI, Cursor), [wakeshift/antigravity-plugin](https://github.com/wakeshift/antigravity-plugin), [wakeshift/hermes-plugin](https://github.com/wakeshift/hermes-plugin).
- **Example apps:** [wakeshift/apps](https://github.com/wakeshift/apps): Agent Chat, Webhooks, the games, Slackline.

The published plugins trust `https://app.wakeshift.ai`. [Self-hosting](docs/self-hosting.md#point-the-sdks-cli-and-plugins-at-your-server) says how to point each one at your server.

## Or use the hosted service

[wakeshift.ai](https://wakeshift.ai) runs this server for you: no database, no scaling, no certificates, and the published plugins already trust it. Your code is the same either way; only `WAKESHIFT_CORE` changes.

## Docs

- [docs/self-hosting.md](docs/self-hosting.md): settings, sign-in, Postgres, proxies and push in production.
- [docs/mcp-events.md](docs/mcp-events.md): what the server implements of SEP-3415, and where it deviates.
- [docs/harnesses.md](docs/harnesses.md): how each agent is woken.
- [How it works](https://wakeshift.ai/docs/how-it-works): how a subscription and a wake flow between your app, the server and the agent.
- [bench/README.md](bench/README.md): the harness runner, to reproduce the matrix with real agents.
- [docs/codebase.md](docs/codebase.md) and [AGENTS.md](AGENTS.md): where things are, and the rules for changing them.
- [wakeshift.ai/docs](https://wakeshift.ai/docs): the developer documentation (SDKs, events, publish URLs, the HTTP API). `site/docs/` is that site's source: its links are site paths and some of it is template placeholders, which don't render on GitHub, so read it there.

## Contributing and security

Issues and pull requests are welcome; see [CONTRIBUTING](CONTRIBUTING.md). Commits are signed off under the Developer Certificate of Origin. Report a security problem privately, not in an issue: see [SECURITY](SECURITY.md).

## License

Apache License 2.0; see [LICENSE](LICENSE) and [NOTICE](NOTICE). The Wakeshift name and logos aren't licensed: see [TRADEMARKS](TRADEMARKS.md).
