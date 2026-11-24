# How Wakeshift works

Agents subscribe to your app's events, your backend emits them and Wakeshift wakes each conversation that subscribed. Your app is your MCP server, with the Wakeshift SDK attached, and your backend. Wakeshift holds the subscriptions and delivers the wakes. The agent subscribes through one of your tools, such as one that starts a job, and acts when it wakes.

![How Wakeshift works. Your app is on the left: your MCP server with the Wakeshift SDK attached, where defineEvent names the event, call.subscribe() runs in your MCP tool and emit() runs from your backend. Their agent is on the right, Wakeshift in the middle. 1, subscribe: the agent calls your tool, which subscribes its conversation with Wakeshift. 2, emit: your backend emits the event to Wakeshift. 3, wake: Wakeshift wakes the subscribed conversation through the agent's own MCP Events, Wakeshift's plugin (installed once, only if the user agrees) or a background listener, and the event arrives marked as coming from your app.](/static/how-it-works.svg)

## The flow

1. **Subscribe.** The agent calls one of your tools that subscribes, such as one that starts a job (a tool you declare with `subscribes`), or the `subscribe_events` tool the SDK adds. Your tool calls `subscribe`, and the SDK records the subscription with Wakeshift, for that conversation. The SDK also adds to your tool's result how the agent receives wakes; you write nothing for it. Some agents need Wakeshift's plugin, installed once: the agent offers the install to its user and runs it only if they agree, and it doesn't ask again. Agents with MCP Events built in, and those that start a background listener, need nothing installed.
2. **Emit.** Something happens in your app, and your backend calls `emit` on the event, from a webhook handler, a job or anywhere else.
3. **Wake.** Wakeshift wakes the subscribed conversation, through the agent's own MCP Events support, Wakeshift's plugin or a background listener. Every conversation subscribed to that occurrence wakes with your event's text and payload, marked as coming from your app, not as an instruction from the user. The agent then acts on what arrived: it looks into it, uses your tools or tells the user.

## What you write

- `defineEvent`: each event your app offers, with typed arguments and payload.
- `attach`: agents can subscribe through your MCP server.
- `call.subscribe` in the tool that starts the work, so the agent that called it wakes when the work is done. Or let agents subscribe themselves through the `subscribe_events` tool, for events open to it ([who may subscribe](/docs/who-may-subscribe)).
- `emit`, from your backend when something happens, or have another service post to a [publish URL](/docs/publish-urls).

Your code is the same for every agent. The [quickstart](/docs/quickstart) writes all four.

## What reaches your server

Your MCP server sees only the tool calls agents make, and Wakeshift hears from your app only when you subscribe a conversation or emit. Wakes don't pass through your server: they go from Wakeshift to the agent directly, through Wakeshift's plugin, a background listener or the agent's own MCP Events support. You run no queue, keep no connection open to any agent and write no code for any particular agent.

Wakeshift holds what's needed to deliver safely:
- **Subscriptions.** Each one belongs to the conversation that made it (for clients that speak MCP Events, to your signed-in user, or without sign-in to the client your tool handed a voucher), and lasts until it's ended or expires ([when a subscription ends](/docs/events#when-a-subscription-ends)).
- **Consent.** A subscription starts only because the user asked their agent for it, and only if your tool or your event's `authorize` allows it. A one-time install runs only if the user agrees.
- **Delivery.** Wakeshift paces wakes and holds occurrences for a conversation that isn't connected.

## How each agent receives wakes

| How | Agents | What the user does |
| --- | --- | --- |
| MCP Events, built into the client | ChatGPT, OpenClaw automations, Hermes Agent, opencode 2.4 and later, Mastra and [others](/docs/mcp-events) | Asks the agent to watch something; nothing to install |
| Wakeshift's plugin, installed once | Codex, opencode 1.x, OpenClaw, Hermes Agent, and Claude Code if the user wants it | Agrees once to the install the agent offers |
| A background listener the agent starts | Claude Code, Cursor, GitHub Copilot CLI and Antigravity CLI | Nothing to install; approves a background command, if the agent asks |
| Not woken | Cursor cloud agents, Goose, Cline and agents Wakeshift doesn't know | Nothing: the agent is told nothing will notify it, and checks with your tools |

Your code is the same for all of them. [Agents](/docs/agents) has the details for each agent: how fast it wakes, what the user approves, and what the install changes.

## Next steps

- [Quickstart](/docs/quickstart): offer an event from your MCP server and wake Claude Code with it.
- [Events](/docs/events): define events, subscribe from your own tools and emit to one conversation or many.
- [Long-running jobs](/long-running-jobs): the same flow for a job that outlives the tool call that started it.
