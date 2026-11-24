# MCP Events in the Wakeshift server

This server is an independent implementation of the server side of MCP Events, [SEP-3415](https://github.com/modelcontextprotocol/modelcontextprotocol/pull/3415), as merged with status Final on 2026-11-19 at commit `8a18b4b`. That revision is pinned in one place, `internal/mcpevents/event/spec.go` (`Revision`), and in the same constant in each SDK.

This page says what is implemented, how it behaves, and where it deliberately departs from the text, with the reason for each departure. The developer-facing version, without the internals, is [wakeshift.ai/docs/mcp-events](https://wakeshift.ai/docs/mcp-events).

## Who does what

An MCP server attaches one of the SDKs ([TypeScript](https://github.com/wakeshift/wakeshift-node), [Python](https://github.com/wakeshift/wakeshift-python), [Go](https://github.com/wakeshift/wakeshift-go)). The SDK answers the `events/*` methods on the MCP server's own transport, and this server (the core) does the rest:

| Method or job | Answered by | Where |
| --- | --- | --- |
| Capability on `server/discover` and `initialize` | The SDK | Each SDK's `attach` |
| `events/list` | The SDK, from the app's event definitions | Each SDK |
| `events/subscribe`, `events/unsubscribe` (webhook) | The SDK checks the request and relays it; the core stores, verifies and delivers | `internal/appapi` (`eventsubs.go`), `internal/mcpevents` |
| `events/poll` | Relayed to the core, which keeps each subscription's queue | `internal/appapi/eventspoll.go` |
| `events/stream` (push) | The SDK holds the client's request open; the core streams to it | `internal/appapi/eventspush.go` |
| Authorization (`identify`, the event's `authorize`) | The app's code, called by the SDK at subscribe time and again later | Each SDK |

The client never talks to the core directly, except to receive webhook deliveries from it.

## What is implemented

### Negotiation

- The extension capability, `extensions["io.modelcontextprotocol/events"]`, on `server/discover` and `initialize`, with `listChanged: false`. The SDKs also send the top-level `events` key from the earlier sketch, which some clients still read (below).
- `resultType: "complete"` on every result for protocol `2026-07-28`. The TypeScript SDK adds `server/discover` and `2026-07-28` to servers on MCP SDK 1.x when they run a stateless transport.
- `Mcp-Name` is checked against `params.name` on `events/poll`, `events/stream`, `events/subscribe` and `events/unsubscribe`. A mismatch is refused with HTTP 400 and `HeaderMismatch` (`-32020`). A missing header is accepted (below).
- `Mcp-Param-{Name}` headers: an argument whose schema property carries `x-mcp-header` is checked against the header of that name, and a mismatch is refused the same way. Each SDK checks the `x-mcp-header` annotations when an event is defined.

### Webhook delivery

- `events/subscribe` is an idempotent upsert keyed by (principal, callback URL, event name, canonical arguments), with a derived subscription ID. The client's `whsec_` secret (24 to 64 bytes) is stored sealed; on rotation both secrets sign for a while.
- The callback must be `https` and resolve to a public address. The core verifies the callback before the first delivery, caches that verification per (principal, URL), and limits verification requests per host. It refuses private, loopback, link-local and other reserved addresses at subscribe time and again on every connection, and never follows a redirect.
- Grants last at most 24 hours. `ttlMs: null` gets a finite grant; a suggestion below 5 minutes is raised to 5 minutes.
- Each occurrence is one POST, signed per Standard Webhooks, with `X-MCP-Subscription-Id`. A fresh timestamp and signature on every attempt. At most 5 attempts within 15 minutes; `410` and `413` aren't retried. A delivery given up on becomes a `gap` envelope.
- Ordering is stricter than the text asks: one delivery in flight per subscription.
- `deliveryStatus` on refresh: `active`, `lastDeliveryAt`, a `lastError` category, `failedSince`. There's no `throttled`, because the core doesn't throttle outbound delivery.
- Bodies are capped at 256 KiB; `_meta` is dropped first when an occurrence would exceed it.
- A refresh with a cursor changes nothing. A fresh `events/subscribe` with the client's last cursor, after the subscription lapsed or ended, replays what the app published meanwhile (kept for a day), or says `truncated: true` when it can't be complete.
- Removing an event type ends its webhook subscriptions within an hour, with `terminated` and `NotFound` (`data.kind: "event"`). A revoked consent or a refused re-authorization does the same: delivery re-checks authorization at least hourly.

### Poll

- The first `events/poll` for (principal, client, event, arguments) creates the subscription, with the same consent record and audit entry a webhook gets. The app's `authorize` runs on every poll.
- Cursors are positions in the subscription's own durable queue. A poll with cursor `c` acknowledges everything up to `c`, so delivery is at least once: a lost response is answered again by the same poll. `cursor: null` starts from now.
- `maxEvents` (at most 100), `hasMore`, `nextPollMs` (10 seconds, and 0 while more is waiting), `maxAgeMs` with `truncated`.
- A subscription nobody polls for 10 minutes lapses; an old cursor after that starts a new one with `truncated: true`. A subscription the user or the app ended answers its next poll with `Forbidden`, on every retry, so a client's loop stops rather than quietly subscribing again.

### Push

- `events/stream` holds one request per subscription and sends `notifications/events/*` messages on it, each carrying the request's ID in `_meta["io.modelcontextprotocol/subscriptionId"]`.
- The stream opens with `active`, sends each occurrence in order, and a heartbeat every 30 seconds whether events arrived or not. The client may ask for 10 to 45 seconds.
- A reconnect with a cursor replays what the client missed from the last hour, up to 1,000 occurrences; from further back, `active` says `truncated: true`.
- `identify` and the event's `authorize` run again every 5 minutes on an open stream (configurable, 1 to 50 minutes). A refusal ends the stream with `terminated`. When they can't answer, the stream stays open; if that lasts 15 minutes, the stream ends and the client reconnects.
- Limits: 20 push subscriptions per user per app, 2,000 open streams per app. Past them, `ResourceExhausted`.
- Push is on by default only when the server's public URL is on `localhost`. Elsewhere, set `WAKESHIFT_MCP_EVENTS_PUSH=1` ([self-hosting](self-hosting.md#push)). While it's off, `events/list` offers `webhook` and `poll` only, and `events/stream` answers `Unsupported` with `value: "push"`.
- The SDKs offer push only on hosts that can stream: a server in JSON-response mode gets webhook and poll alone. On serverless hosts with a request cap, the SDK ends each stream before the cap and the client resumes with its cursor.

### Errors

The spec's codes: `NotFound` `-32023`, `Forbidden` `-32024`, `ResourceExhausted` `-32025`, `Unsupported` `-32026`, callback failure `-32027`, `HeaderMismatch` `-32020`, `InvalidParams` `-32602`, with `data.kind`, `limit`, `feature`, `value` and `reason` as the text defines them. ChatGPT gets its own codes (below).

### On the wire beyond the text

- Each occurrence carries `_meta["ai.wakeshift/text"]`, a rendered line for clients that show events as text. The payload in `data` is the app's, untouched.
- An event that only the app's own tools may subscribe to carries a required `wakeshift_voucher` argument in its `inputSchema`, for native clients only. A tool's result hands the client a voucher for exactly that event and those arguments.
- `events/list` descriptors carry a `title`. Every client checked ignores it.

## Per-client dialects

Some clients that shipped before the text was final reject a delivery that follows it. The core picks a dialect for each webhook subscription when it's made, from a table of known clients (`internal/mcpevents/event/mcp-events-clients.json`: the client's `clientInfo.name`, User-Agent products and `_meta`), and keeps it for the subscription's life:

| Dialect | Clients | `webhook-id` | `gap`, `terminated` | Errors |
| --- | --- | --- | --- | --- |
| `spec` | Every client not listed below | Its own for each message, kept for that message's retries | Sent | The spec's |
| `draft` | TanStack AI, the Vercel AI SDK | The body's `eventId` | Sent | The spec's |
| `openclaw` | OpenClaw | The body's `eventId` | Not sent; every body carries `cursor` | The spec's |
| `chatgpt` | ChatGPT | The body's `eventId` | Not sent | ChatGPT's |

Why they exist: ChatGPT, the Vercel AI SDK 2.0.97, TanStack AI 1.2.0 and OpenClaw 2026.11.3 answer HTTP 400 to a delivery whose `webhook-id` differs from its `eventId`, as [OpenAI's guide](https://developers.openai.com/plugins/build/mcp-events) says for ChatGPT and the others' published code shows. OpenClaw also answers `gap` and `terminated` with 400 and refuses a body without `cursor`. A server that follows the text to the letter loses those clients; a server that doesn't loses the spec's guarantees for everyone else. The table confines the compromise to the clients that need it.

In the `spec` dialect, `eventId` is the app's own ID for the occurrence: the `id` passed to `emit`, or a publish URL's idempotency key, so a client can deduplicate the same upstream event however it arrives. Without one, it's the core's delivery ID. In the other three dialects `eventId` is the core's delivery ID, because `webhook-id` has to equal it and has to be unique per message.

Capabilities decide the rest. A client is treated as native when it declares the extension, or is ChatGPT by name. The table also lists clients that must never count as native whatever they declare, because nothing they do with an event reaches the conversation: Claude Code, Codex, Cursor, Copilot CLI, Antigravity CLI and the others in [harnesses.md](harnesses.md) are woken through their bridges instead.

## Deviations and why

| Where | What the text says | What this server does | Why |
| --- | --- | --- | --- |
| `Mcp-Name` | Required on `events/poll`, `events/stream`, `events/subscribe` and `events/unsubscribe`; a server refuses a request without it | A missing header is accepted from every client. A wrong one is refused with HTTP 400 and `-32020` | ChatGPT, OpenClaw, the Vercel AI SDK, Hermes Agent and Mastra don't send it |
| `webhook-id` | Unique per message and never derived from `eventId` | Equal to `eventId` for the `draft`, `openclaw` and `chatgpt` dialects | Those clients refuse a delivery where the two differ (above). Their `eventId` is then the core's per-subscription delivery ID, which meets every other requirement on `webhook-id` |
| `eventId` | Should be the upstream's stable ID | The app's ID in the `spec` dialect; the core's delivery ID in the others | A consequence of the row above |
| `gap`, `terminated` | Sent to every webhook | Not sent to OpenClaw or ChatGPT | OpenClaw answers both with 400; ChatGPT has no handling for them |
| Error codes | The spec's | ChatGPT gets `-32011`, `-32012`, `-32015`, and `InvalidParams` where it has no code | ChatGPT understands only its own codes; no other client checked understands them |
| Grants | `ttlMs: null` may get a grant with no expiry | Every grant ends within 24 hours | Bounded subscriptions; OpenClaw rejects `null` in any case |
| Principal | Webhook `events/subscribe` and `events/unsubscribe` must have an authenticated principal | A subscription made with a voucher, on a server without sign-in, has none | The voucher stands in for the principal: it binds the event and arguments the app's tool chose, and refreshing or unsubscribing needs the same voucher and secret, which closes the risk the rule guards against |
| Schema changes | An incompatible change to an event's schema ends its subscriptions with `Unsupported` (`schema_changed`) | Subscriptions continue | The core doesn't yet tell an incompatible change from an additive one. Give a changed event a new name |
| `events/list` | Paginated | One page, never a `nextCursor` | Allowed; catalogs are small |
| `-32601` | A server without events answers `events/*` with method not found | The TypeScript and Go SDKs answer `events/*` even with no events defined, while advertising nothing | Open |
| Egress | A server without a Server Card documents its egress ranges | The core has no fixed egress addresses; receivers check the signature | No Server Card yet. The signature is the boundary the text relies on when ranges aren't stable |
| Server identity (`v1a,`) | Optional | Not sent | Optional; no Server Card |

## Testing

- **Core tests** (`internal/coretest`, `internal/mcpevents`, `internal/appapi`) cover webhook delivery and its order, retries and `gap`, replay, `terminated`, re-authorization, `Mcp-Name`, each dialect, poll, push (replay, heartbeats, re-checks, limits) and SSRF refusals. They run against a real Postgres.
- **Shared wire fixtures** in the TypeScript SDK's tests (`mcp-events-wire.json`, `mcp-events-interop.json`, the client table) are run by all three SDKs, so they answer the same bytes.
- **The proposed server conformance suite** for SEP-3415 ([modelcontextprotocol/conformance#521](https://github.com/modelcontextprotocol/conformance/pull/521), pinned at `76fef32`, which targets the SEP's `b08e4f9` draft) runs against a test app on each SDK (TypeScript on MCP SDK 2.x, Go on the official go-sdk, Python on `MCPServer`), with a list of expected failures ([bench/mcp-events/conformance-expected.yaml](../bench/mcp-events/conformance-expected.yaml); how to run it: [bench/README.md](../bench/README.md#mcp-events-conformance)). What it doesn't grade:
  - the missing-`Mcp-Name` rows, which fail by design (above);
  - most webhook rows: the suite subscribes with callbacks on `conformance.invalid`, which never resolves, and this server verifies a callback inside `events/subscribe`, so no subscription is made. Webhook delivery is tested by the core tests and against real clients instead;
  - `schema_changed`, which isn't implemented;
  - rows that need test-app controls the suite defines and our test app doesn't have yet (changing the catalog mid-run, forcing a gap or a quota refusal), and a row that needs DNS answers to change between subscribe and delivery. Core tests cover each of those behaviors.
- **Real clients**, each at the version named, against a local server (November 12–23, 2026):

| Client | Version | Webhook | Poll | Push |
| --- | --- | --- | --- | --- |
| ChatGPT (Work chats) | Hosted | Matches its recorded requests; not tested live | — | — |
| OpenClaw | 2026.11.3 | Yes | — | — |
| Hermes Agent | v0.23.1 | Yes | — | — |
| opencode | 2.4.0, with no flag (and `dev` 6dd40633, behind its `mcp_events` flag) | — | — | Yes |
| Mastra (`@mastra/mcp`) | 2.8.0 | Yes | Yes | Yes |
| TanStack AI (`@tanstack/ai-mcp`) | 1.2.0 | Yes | — | — |
| Vercel AI SDK (`@ai-sdk/mcp`) | 2.0.97 | Yes | — | — |
| mcpkit (Go) | v0.8.21 | Not tested | Not tested | Yes |

"—" means the client doesn't use that mode.

## What clients do with an event

Most clients that subscribe natively start a new run for each event (ChatGPT, OpenClaw automations, Hermes Agent), or run the application's callback (TanStack AI, the Vercel AI SDK). opencode 2.4 and later (2.3 with its `mcp_events` flag) wakes the session that's open, and Mastra's signal provider adds the event to the thread that subscribed. To wake the conversation a person already has open in the other agents, use the bridges in [harnesses.md](harnesses.md).
