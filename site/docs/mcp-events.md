# Agents that speak MCP Events

Some agent programs subscribe to events themselves, with MCP Events, the Model Context Protocol's extension for events. Your app serves them with the code you already have: `attach` answers their requests, and Wakeshift holds each subscription and delivers every occurrence. You write nothing for any of these clients, and the same events still reach every other agent through `subscribe_events` and your own tools.

## Which agents, and what happens on an event

| Agent | How it subscribes | What happens when an event arrives | What the user sets up |
| --- | --- | --- | --- |
| ChatGPT (Work chats) | The user asks ChatGPT to watch something | A new run in the chat | Nothing beyond adding your app |
| OpenClaw (2026.10.10 beta and later) | An automation with an MCP event source, made in the Automation editor or by asking the agent | A new, isolated automation run, with the event as outside data | The `mcp-events` plugin, with a public HTTPS address for its gateway |
| Hermes Agent (v0.22.5 and later) | The agent subscribes when asked, after the user approves each subscription in chat | A new, unattended run that follows the instructions given at subscribe time | `events: true` on your server in Hermes's config, and its webhook listener on a public HTTPS address |
| opencode (2.4.0 and later, or 2.3 with its `mcp_events` flag) | The agent watches when asked, with an opencode automation that lasts until the agent ends it | The session that's open wakes with it | No Wakeshift plugin (with the plugin installed, Wakeshift wakes the session through it instead); on 2.3, the `mcp_events` flag |
| Mastra (`@mastra/mcp` 2.6 and later) | In code, or with its tools for the agent | In code, the subscription's iterator yields it. With Mastra's signal provider, it's added to the thread that subscribed | Nothing for poll or push; a public URL for webhooks |
| TanStack AI (`@tanstack/ai-mcp` 1.0) | In code | Your callback runs | A public HTTPS URL for its webhook handler |
| Vercel AI SDK (`@ai-sdk/mcp` 2.0) | In code | Your callback runs | A public HTTPS URL for its webhook handler |
| mcpkit (Go) | In code | Your handler runs | A public HTTPS URL for webhooks; nothing for poll or push |

ChatGPT reads your server's list of events once, when the user connects your app. An event that appears in it later (one you add, say) reaches a user who connected before only after they choose "Refresh tools" for your app in ChatGPT.

These clients start a new run for each event, apart from opencode and Mastra's signal provider. To wake the conversation a person already has open, their agent subscribes with `subscribe_events` or one of your tools instead, and Wakeshift wakes it the way that agent can be woken ([agents](/docs/agents)). OpenClaw and Hermes Agent can do both, so pick the path for the job:
- MCP Events for an automation that runs on its own.
- `subscribe_events` and Wakeshift's install for the conversation the user is in.

Claude Code, Codex, Cursor, Copilot CLI, Antigravity CLI, ChatGPT's desktop app, the agents in OpenClaw's and Hermes Agent's chats, and opencode before 2.4.0 without the `mcp_events` flag subscribe through your tools, and Wakeshift wakes them as the [agents](/docs/agents) page describes. Wakeshift tells the two kinds of client apart from what each one's requests declare; there's nothing to configure.

### How each client receives occurrences

| Client | Version tested | Webhook | Poll | Push |
| --- | --- | --- | --- | --- |
| ChatGPT (Work chats) | Hosted | Not tested live; matches its recorded requests | — | — |
| OpenClaw | 2026.11.3 | Yes | — | — |
| Hermes Agent | v0.23.1 | Yes | — | — |
| opencode | 2.4.0 (in 2.3.0, behind its `mcp_events` flag, off by default) | — | — | Yes |
| Mastra (`@mastra/mcp`) | 2.8.0 | Yes | Yes | Yes |
| TanStack AI (`@tanstack/ai-mcp`) | 1.2.0 | Yes | — | — |
| Vercel AI SDK (`@ai-sdk/mcp`) | 2.0.97 | Yes | — | — |
| mcpkit (Go) | v0.8.21 | Not tested | Not tested | Yes |

"Yes" means tested live against Wakeshift (November 12-13, 2026); "—" means the client doesn't use that mode. Mastra picks push when your server offers it, and poll otherwise, unless your code gives it a webhook.

## How occurrences arrive

Each event lists the modes it's offered in (`webhook`, `poll`, `push`), and the client picks one.

- **Webhook.** The client gives a URL, and Wakeshift checks that the URL agrees to receive events before delivering anything. Then each occurrence is one POST, signed with [Standard Webhooks](https://www.standardwebhooks.com/), in order and retried for up to 15 minutes. If one is given up on, the client is told it missed something (ChatGPT and OpenClaw take no such notice). A subscription lasts at most a day, and the client renews it before then. Mastra, OpenClaw and Hermes Agent renew on their own; with TanStack AI and the Vercel AI SDK, your code renews. A client whose subscription lapsed and that subscribes again with its last cursor gets what was published meanwhile, or is told that some of it is gone.
- **Polling.** This is for clients that can't take webhooks, such as an agent on a laptop or Mastra's poll mode. The first poll subscribes, and each later poll returns what happened since the last one, in order. Wakeshift asks clients to poll every 10 seconds. Nothing is lost between polls or when a poll fails, because the next poll returns it again. A subscription nobody has polled for 10 minutes ends.
- **Push.** The client keeps one request open to your server, and each occurrence arrives on it as it happens, in order, with no public URL and no polling. A quiet stream carries a heartbeat every 30 seconds (a client may ask for 10 to 45). When the stream drops, the client reconnects with its cursor and gets what it missed from the last hour, up to 1,000 occurrences; from further back, it's told that some of it is gone.

Every occurrence carries your `payload` exactly as you emitted it, and the line your event's `text` renders, for clients that show one. Keep payloads to what an agent needs to decide what to do, and let it fetch the rest with your tools: these clients hand the payload to a model as outside data.

### No inbound HTTPS?

A webhook needs a public HTTPS URL that answers Wakeshift's verification. A private, LAN or loopback address, or a URL that isn't `https`, is refused ([`callback_failed`](/docs/errors#callback_failed)). An agent that can't take inbound HTTPS still gets your events:
- **Wakeshift's install for that agent**, which wakes the conversation without any inbound address: [the one-time install](/docs/agents#the-one-time-install), and [Hermes Agent](/docs/hermes) for Hermes.
- **Poll or push**, when its client supports them (the table above). An event only your tools subscribe to is never polled, and takes push only from a signed-in caller or one that keeps a session with your server ([consent and control](#consent-and-control)).

The refusal's message says the client needs a public HTTPS callback for webhook delivery, and how to be woken without one, by client:
- **Hermes Agent and OpenClaw** are pointed to Wakeshift's plugin for them, with its install link.
- **ChatGPT** is told the requirement alone.
- **Any other client** is told to use poll, or push where your server offers it, if the client supports them.

Hermes Agent shows the subscribe error to its model, so the user hears it. OpenClaw shows a failed subscribe only as a code in its own diagnostics, so its users learn about Wakeshift's install from your next tool result instead.

### Where webhooks come from

Wakeshift sends webhook deliveries and verification requests from Google Cloud addresses that change and are shared with other Google Cloud customers. Wakeshift has no fixed egress IP ranges, so a firewall can't allowlist them. Check each request's Standard Webhooks signature instead, with the secret the client gave when it subscribed. A valid signature shows the request was signed with that subscription's secret, which only the client and Wakeshift hold, so keep the secret private. In the MCP Events standard, the signature is the security boundary, and a source address is only defense in depth. A receiver that admits only listed addresses can still get your events by poll or push, when its client supports them.

Wakeshift calls only public addresses: it refuses a callback that resolves to a private, loopback, link-local or other reserved address, checks again on every connection, and never follows a redirect.

## Consent and control

- **Only what you allow.** These clients subscribe on their own only to an event that declares `personal`, `authorize` or `public`. An event with none of them is for your own tools: when one of your tools subscribes such a client, its result hands the client a one-time voucher for exactly that event and those arguments, when the client is signed in or keeps a session with your server, and the client subscribes with it, within 4 hours, by webhook or by push. Without a voucher it's refused ([who may subscribe](/docs/who-may-subscribe#chatgpt-and-other-clients-that-subscribe-natively)).
- **Each subscription belongs to your user, when there is one.** A subscription a client makes on its own needs your sign-in (attach's `identify`). A voucher needs none; with sign-in, only the same user can use it. Your event's `authorize` runs before a subscription starts, again on every poll, and every 5 minutes while a push stream is open, together with `identify` ([push options](#push-options)). A refusal, or a stream whose user is now gone or someone else, ends the stream, and the client is told it ended; when `authorize` or `identify` can't answer, the stream stays open. If they still can't answer once the stream's last successful check is 15 minutes old (three checks, if you set a longer interval), the stream ends and the client reconnects with its cursor. While they can't answer, a reconnect gets a retryable error rather than a refusal, so the client keeps trying and the stream resumes once they answer. The SDK logs each failed check as `push_recheck_failed`, and each refused reconnect as `push_open_check_failed`. Each of your users can have up to 20 webhook subscriptions and 20 push subscriptions to your app at once, and your app can have up to 2,000 push streams open at once.
- **Your users can stop them.** `list_subscriptions` shows a user's subscriptions, native ones included, and `unsubscribe_events` with an ID ends one. Nothing more is delivered. A polling client's next poll is refused, so it doesn't quietly subscribe again, a push stream is ended with a final notice, and a webhook gets a final notice if its client takes one (ChatGPT and OpenClaw don't).
- **Removing an event from your app ends its subscriptions.** Webhook subscriptions end within an hour, with a final notice that the event is gone, and a poller is told on its next poll.

## Your server

MCP Events is defined for MCP protocol 2026-07-28. Every SDK also answers `events/*` and advertises the capability on the older handshake (`initialize`), so most clients above reach your events either way; OpenClaw speaks only 2026-07-28, and opencode 2.4 offers events only on a 2026-07-28 connection, so they need a server that does:
- **TypeScript:** MCP SDK 2.x speaks 2026-07-28 on any transport. On 1.x, Wakeshift adds it on a stateless Streamable HTTP transport (no sessions) and on stdio; a 1.x server with sessions serves every client above except OpenClaw and opencode 2.4.
- **Go:** MCP Events are served over HTTP, by `attached.Handler`, and not over stdio. go-sdk v1.8 and later speak 2026-07-28. On mcp-go, or a server that pins the protocol versions it accepts, a 2026-07-28 client connects only if your transport accepts that version; clients on `initialize` are served either way.
- **Python:** on mcp 2.x hosts (MCPServer, FastMCP 4, the low-level `Server`). FastMCP 2 and 3 serve no MCP Events.

**A server that keeps state per session** (a workspace the user picked earlier in the session, say) gets none on 2026-07-28: those requests carry no `Mcp-Session-Id`, and each one stands alone. Before serving 2026-07-28, key that state on something every request carries (the user's credential), or keep 2026-07-28 off and serve clients through `initialize`.

### Push on your host

Push holds a request to your server open for as long as the client listens, so it needs a host that streams its responses:
- **A host that answers with JSON only** gets no push: the MCP SDK's JSON-response mode (TypeScript `enableJsonResponse: true` on 1.x, `responseMode: "json"` on 2.x; Python `json_response=True`). The SDK sees the setting and offers webhook and poll alone. Where something else in front of your server forces JSON answers (mcp-use 1.x with the [middleware for 2026-07-28](/docs/frameworks#mcp-use), say), turn push off yourself: `attach(server, { push: false })` (Python `push=False`, Go `Push: wakeshift.PushOff`).
- **Serverless hosts** cap a request's duration. On Vercel Functions, AWS Lambda and Cloud Run, the SDK ends each stream before the platform's default cap. A function you've configured with a shorter limit is cut off by the host first. Either way the client reconnects with its cursor, and nothing is lost. To end streams sooner, or on another host with a cap, set the longest a stream may stay open (below).
- **Each open stream is one request in flight.** Leave `events/stream` out of any limit you put on concurrent requests or request duration, and keep a load balancer's idle timeout above the heartbeat (45 seconds at most), or lower the heartbeat (below).

### Push options

`push` in `attach`'s options turns push off, or sets how its streams behave:

| TypeScript | Python | Go | What it does |
| --- | --- | --- | --- |
| `push: false` | `push=False` | `Push: wakeshift.PushOff` | No push: clients use webhook or poll |
| `push: { maxStreamMs }` | `push={"max_stream_ms": …}` | `Push: wakeshift.PushOptions{MaxStream: …}` | The longest a stream stays open before it ends and the client reconnects with its cursor. On Vercel, AWS Lambda and Cloud Run the SDK sets it under the platform's default cap |
| `push: { maxHeartbeatMs }` | `push={"max_heartbeat_ms": …}` | `Push: wakeshift.PushOptions{MaxHeartbeat: …}` | The longest gap between heartbeats, for a load balancer with a shorter idle timeout. Otherwise the client's choice, or 30 seconds |
| `push: { reauthorizeMs }` | `push={"reauthorize_ms": …}` | `Push: wakeshift.PushOptions{Reauthorize: …}` | How often an open stream asks `identify` and your event's `authorize` again: 5 minutes by default, 1 to 50 minutes |

In Go, a zero field keeps the default.

## The standard, and where Wakeshift differs

Wakeshift follows MCP Events as published (SEP-3415, final on 2026-11-19). Where a client above differs from the standard, Wakeshift stays compatible with that client:
- **ChatGPT** gets each webhook's `webhook-id` equal to the occurrence's `eventId`, no notices of missed or ended subscriptions, and its own error codes ([errors](/docs/errors#what-mcp-events-clients-get)).
- **OpenClaw** gets `webhook-id` equal to `eventId`, no notices of missed or ended subscriptions, and a `cursor` key on every occurrence.
- **The Vercel AI SDK and TanStack AI** get `webhook-id` equal to `eventId`.
- **Every other client** gets the standard: a `webhook-id` of its own for each message, the same across that message's retries, and as `eventId` the `id` you passed to `emit` (the upstream's ID), or for a [publish URL](/docs/publish-urls) the sender's idempotency key, so the client can deduplicate the same event however it arrives. Without an `id`, `eventId` is Wakeshift's delivery ID. Poll and push results carry your `id` the same way.

ChatGPT, OpenClaw, the Vercel AI SDK and TanStack AI get Wakeshift's delivery ID as `eventId`, because their `webhook-id` equals it, and a `webhook-id` has to be unique to each message.

The deviations we know of apply to every client:
- **A missing `Mcp-Name` header is accepted.** On protocol 2026-07-28, a request whose `Mcp-Name` header doesn't match the event it names is refused with HTTP 400 and `HeaderMismatch` (`-32020`), and a request without the header is accepted.
- **A breaking change to an event's schema isn't detected.** The standard ends such subscriptions with `Unsupported` (`schema_changed`); Wakeshift keeps them. When an event's arguments or payload change incompatibly, give it a new name.

The official conformance suite can't grade webhook delivery against Wakeshift; the clients above are tested directly.
