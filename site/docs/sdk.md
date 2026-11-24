# TypeScript SDK

`@wakeshift/sdk` for Node 22.12 or later (Node 20.19 or later works too), from an ES module or, through `require()`, a CommonJS one. It works with either MCP TypeScript SDK: `@modelcontextprotocol/server` 2.x (2.3 or later), which this page uses, or `@modelcontextprotocol/sdk` 1.x (1.23 or later; [below](#using-modelcontextprotocolsdk-1x)), with `zod` 4.2 or later on 2.x and 3.25 or later on 1.x. [MCP frameworks](/docs/frameworks) lists the server frameworks it works with, [Cloudflare Workers](#cloudflare-workers), [Mastra](#mastra) and [xmcp](#xmcp) among them. The [Python](/docs/python) and [Go](/docs/go) SDKs have the same calls. Its source is at [github.com/wakeshift/wakeshift-node](https://github.com/wakeshift/wakeshift-node), under the Apache License 2.0.

> **Using a coding agent?** Paste the prompt below, add [AGENTS.md](/AGENTS.md) to your repository, or point your agent at [llms.txt](/llms.txt), which links every docs page as Markdown.

```text Prompt for your coding agent
Add Wakeshift to this MCP server following {{SITE}}/AGENTS.md
```

```sh
npm install @wakeshift/sdk @modelcontextprotocol/server @modelcontextprotocol/node zod
```

`@modelcontextprotocol/node` serves your MCP server on Node's `http`. On 1.x, install `@wakeshift/sdk @modelcontextprotocol/sdk zod` instead.

`createMcpHandler` makes a server for each request, so attach in the function that makes it, and define your events once, outside it:

```ts
import { createServer } from "node:http"
import { McpServer, createMcpHandler } from "@modelcontextprotocol/server"
import { toNodeHandler } from "@modelcontextprotocol/node"
import { z } from "zod"
import { Wakeshift } from "@wakeshift/sdk"

const wake = new Wakeshift() // reads WAKESHIFT_KEY; without one, Wakeshift is off

const crawlFinished = wake.defineEvent("crawl.finished", {
  description: "A crawl finished",
  args: z.object({ job: z.string() }),
  payload: z.object({ status: z.string(), pages: z.number() }),
})

const mcp = createMcpHandler(() => {
  const server = new McpServer({ name: "crawler", version: "1.0.0" })
  const attached = wake.attach(server)
  attached.registerTool(
    "start_crawl",
    { description: "Crawl a site.", inputSchema: z.object({ url: z.string() }), subscribes: [crawlFinished] },
    async ({ url }, ctx, call) => {
      const job = crypto.randomUUID()
      await call.subscribe(crawlFinished, { job }, { once: true })
      startCrawl(url, job)
      return `Crawl ${job} started.`
    },
  )
  return server
})

createServer(toNodeHandler(mcp)).listen(3000)
```

- **Request context.** Your tool's handler, `identify` and `isNativeClient` get the request's context, `ctx`: the verified token is `ctx.http?.authInfo` (`toNodeHandler` passes on the `req.auth` your auth middleware sets), and the HTTP request is `ctx.http?.req`.
- **Protocol.** `createMcpHandler` answers clients on MCP protocol 2026-07-28 and on older revisions, so every client connects with nothing more to set up.
- **Caching.** Wakeshift lists its tools for each request's client, so `tools/list` is cached by that client only, whatever cache hint you set for it.

## The client

```text
new Wakeshift({ key?, required?, core?, fetch?, timeoutMs?, maxRetries?, callTimeouts?, logger?, logLevel? })
```

| Option | What it is |
| --- | --- |
| `key` | Your app's key: `wst_` and 50 characters. Default: `WAKESHIFT_KEY` from the environment. It's a private key and never goes over the wire: each request carries a fresh assertion signed with it. `""` or whitespace counts as no key. Older `wst_test_…` and `wst_live_…` keys keep working. |
| `required` | Throw when there's no key, instead of running disabled: for a hosted server that must have one. |
| `core` | The address of the console the key came from. Default: `WAKESHIFT_CORE` from the environment, else `https://app.wakeshift.ai`. It must be `https://`, except on `localhost`, `127.0.0.1` and `::1`. An older `wst_test_` key has no default and needs one of them. |
| `fetch`, `timeoutMs`, `maxRetries` | The `fetch` to use, the timeout for each request (default 20 seconds), and how many times a refused or failed connection is retried (default 2). Without `fetch`, each request uses `globalThis.fetch` as it is when the request is made, so a test that stubs the global `fetch` after creating the client is seen. |
| `callTimeouts` | `{ subscribeMs?, blockMs? }`: the budget for the requests a tool that subscribes makes inside its call. `subscribeMs` (default 5000) covers `call.subscribe`, `call.unsubscribe` and `call.resolveSession`; `blockMs` (default 1000) covers the addition to the result and the rollback of a failed call. These requests are never retried, and `timeoutMs` and `maxRetries` don't apply to them: [inside a tool call](#inside-a-tool-call). |
| `logger`, `logLevel` | Where the SDK logs, and from which level: [logging and secrets](#logging-and-secrets). |

- **Creating the client makes no network request**, in any runtime. The environment is read through `globalThis.process?.env`, so runtimes without `process` don't throw.
- **A malformed key, or an option or value the SDK doesn't know** (`builtinTools: { subcribe: false }`), throws a `UsageError` with code [`config`](/docs/errors#config). The message never contains any part of the key.
- **In tests, `new Wakeshift({ key: "" })`** is a disabled client even where `WAKESHIFT_KEY` is set: an explicit empty key never reads the environment. `createTestClient()` from `@wakeshift/sdk/testing` is an enabled one that sends nothing ([test your integration](/docs/testing#unit-tests)).
- **`await wake.close()`** stops every [`watch`](/docs/events#wait-on-work-you-can-only-poll) and waits for `onSubscribe` work in flight; call it on shutdown. A watch never keeps the process alive, and on a stdio server the client closes itself when stdin ends or the stdio transport closes; with a framework adapter (`attachTools`, Mastra), call `await wake.close()` yourself.
- **`wake.enabled`** says whether the client has a key. `await wake.app()` returns `{ id, name }`, which app the key is, asked once; `wake.appId` is the app's ID once known, else `undefined`.

### Without a key

A missing key gives a disabled client, logged once at debug level (`no_key`), so a server without a key prints nothing it didn't before. It keeps its shape, so nothing in your code needs a guard:

| On a disabled client | What happens |
| --- | --- |
| `defineEvent` | Works, fully typed; schemas and options are checked |
| `attach` | Returns an attachment that adds nothing: no built-in tools, no Wakeshift arguments, no MCP Events |
| `attached.registerTool` | Registers the tool as you wrote it |
| `identify` | Never runs; `call.user` is `undefined` |
| `wake.currentCall()` | Returns a call that does nothing, anywhere |
| `call.subscribe` | Resolves to `{ state: "disabled" }` |
| `event.emit` | Checks the payload, sends nothing, resolves to `{ matched: 0, … }` |
| `event.createPublishUrl`, `status`, `statusOf`, `end`, `wake.forgetUser` | Throw a `UsageError` with code [`disabled`](/docs/errors#disabled) |

The rule: the calls on your hot paths (`emit`, `subscribe`, `currentCall`) do nothing, and the calls that must return Wakeshift's answer throw. Guard those with `if (wake.enabled)`.

## Events

```text
wake.defineEvent(name, { description, args, payload, text?, personal?, authorize?, public?, onSubscribe?, deleteOnDelivery?, title?, topic?, filter? })
```

| Option | What it is |
| --- | --- |
| name | What clients subscribe to: lowercase words joined by dots (`crawl.finished`), at most 64 characters, not starting with `wakeshift.`. |
| `description` | What happened, as a plain statement: agents read it in the description of every tool that subscribes to it. |
| `args` | What a subscriber picks, as a zod object: the event's arguments. Required fields route occurrences; optional ones filter on the payload ([events](/docs/events#define-an-event)). |
| `payload` | What each occurrence carries, as a zod object. `emit` checks it before sending. |
| `text(payload, args)` | Optional: the line the agent reads first. Without it, the agent reads the description and the arguments: "A crawl finished (job 3f2a…)". |
| `personal`, `authorize(subscriber, args)`, `public` | Who may subscribe beyond your own tools. None of them: only your tools. See [who may subscribe](/docs/who-may-subscribe). `public` can't be combined with the other two. |
| `onSubscribe({ args, user, via, tool, context })` | Runs once for each new subscription to the event, however it was made, in the background: where a [`watch`](/docs/events#wait-on-work-you-can-only-poll) starts. `context` is what the subscribing call passed to `call.subscribe(event, args, { context })`, never sent to Wakeshift; for a tool it runs inside the call's async context ([details](/docs/events#wait-on-work-you-can-only-poll)). Annotate the parameter as `Subscribed<Args, YourContext>` to type `context` without a cast. A watch already running for the same event and arguments keeps its first subscriber's `context`; a later subscriber's doesn't reach it. |
| `deleteOnDelivery` | Wakeshift deletes each occurrence's text and payload once it's delivered. |
| `title`, `topic`, `filter` | A short title for clients that show one, and your own routing when the default doesn't fit ([events](/docs/events#define-an-event)). |

Define every event at startup, before `attach`. `defineEvent` returns a `WakeEvent`; `EventArgs<typeof crawlFinished>` and `EventPayload<typeof crawlFinished>` are its argument and payload types. For code that takes any of your events, `AnyEvent` is any event, and `SharedEvent` any event that isn't personal, whose `emit` takes no `to`.

| Call | What it does |
| --- | --- |
| `event.emit(args, payload, { to?, id? })` | Sends an occurrence: [emit](#emit). |
| `event.watch(args, check, { every?, timeout?, maxErrors?, isFinal?, id?, to?, signal? })` | Calls `check` until it returns the payload, then emits once: [wait on work you can only poll](/docs/events#wait-on-work-you-can-only-poll). |
| `event.createPublishUrl(args, { to?, signingSecret? })` | A URL that emits the event when a webhook posts to it: [publish URLs](#publish-urls). |
| `event.status(args, { to? })` | How many conversations are subscribed with those arguments, how many are connected now, and how: `{ subscriptions, connected, via }` ([who is subscribed](/docs/events#who-is-subscribed)). |
| `event.statusOf([{ user, subscription }])` | Up to 1,000 subscriptions by ID, each `{ id, user, live, connected, via? }`. |
| `event.end(user, id)` | Ends one of a user's subscriptions by its ID, and tells its conversation. `true` if there was one. |

Every async call takes `{ signal?, timeoutMs?, maxRetries? }` last, beside its own options. A `signal` you abort rethrows its reason, unchanged.

The SDK sends your app's manifest itself: your events, added to the ones Wakeshift has. Once `attach` has run, it's sent on the first call about events. A process that only emits sends it when Wakeshift answers an emit with `unknown_event`, then emits again, so you can emit at startup or from a worker before any agent connects.

## Attach

```text
wake.attach(server, { identify?, requireUser?, wrap?, builtinTools?, isNativeClient?, push? })
```

| Option | What it is | Default |
| --- | --- | --- |
| `identify(ctx)` | Who is behind a request: `{ user?, client?, agent? }`, from your own sign-in. May be async. [Sign-in](#sign-in-identify-and-authorize). | No user |
| `requireUser` | Refuse Wakeshift's part of a request from a caller with no user. | Anonymous allowed |
| `wrap` | Your tools that subscribe, registered elsewhere, by name: `{ start_crawl: [crawlFinished] }`. For tools named at run time, a function from a tool's name to its events (`undefined` for one that doesn't subscribe): `(tool) => (tool.startsWith("run_") ? [runFinished] : undefined)`. [Tools registered elsewhere](/docs/low-level). | None |
| `builtinTools` | Which built-in tools to add: `{ subscribe?, list?, unsubscribe?, wait? }`, or `false` for none. | `subscribe`, `list`, `unsubscribe` on (where something can subscribe); `wait` off |
| `isNativeClient(ctx)` | For your own agents only: whether a caller subscribes natively. [Advanced](/docs/advanced#your-own-agents-that-subscribe-natively). | The SDK's own checks |
| `push` | `false` to offer MCP Events clients webhook and poll only, never push (`events/stream`), for a host where something in front of your server forces JSON answers; or `PushOptions` `{ maxStreamMs?, maxHeartbeatMs?, reauthorizeMs? }` ([push options](/docs/mcp-events#push-options)). | Push offered where your transport can stream (not in JSON-response mode) |

`attach` returns the attachment your tools register through. It adds:

- **The built-in tools**, listed where `attach` registers them (tools you register after `attach` come after them): `subscribe_events` (listed only when an event is open to it), `unsubscribe_events` and `list_subscriptions`, so a person can always see and stop what they're subscribed to. Without `identify` those two act on the calling conversation, found by the `session_key` the agent passes; with a user, on that user's subscriptions. A server where nothing can subscribe (no tool declared with `subscribes`, no event open to `subscribe_events`) lists none of them. `builtinTools: { wait: true }` adds Claude Code's [wait tool](/docs/agents#the-wait-tool-for-claude-code); `builtinTools: false` turns them all off. `BUILTIN_TOOL_NAMES` lists their names. `list_subscriptions` answers with a line per subscription (its id, event, arguments, where it's delivered and the day it started) and `structuredContent` `{ subscriptions: [{ id, event, arguments, destination, since }] }`: `destination` is `conversation` (it wakes a conversation), `webhook` or `poll` (an MCP Events client's), and `since` an RFC 3339 time.
- **A `tools/list` hook of your own** that adds fields to every tool (`securitySchemes` for ChatGPT, say): on `McpServer` the built-in tools are registered tools, so a hook that wraps the `tools/list` handler sees them. On a low-level `Server`, Wakeshift adds them after your handler answers, so your hook doesn't see them: add the fields to them yourself or turn them off with `builtinTools: false`.
- **MCP Events** (`events/list`, `events/subscribe`, `events/poll`, and `events/stream` for push where your transport can stream) for clients that subscribe natively, advertised whenever you define an event, tools-only ones included (a client subscribes to those only with the voucher your tool's result hands it), unless your server already serves its own MCP Events: then it keeps them, Wakeshift serves none there, and the SDK logs `events_capability_kept`. That's decided when you attach, from the events capability and the `events/*` handlers the server has then, so set your own before `attach`. A handler you set after `attach` replaces Wakeshift's for that method, while Wakeshift's capability stays.
- **Wakeshift's arguments taken out of every tool call** whose tool doesn't declare them, before the MCP SDK validates it and before your middleware runs.

Attach once per server: a second `attach` on the same server throws. Pass the `McpServer` itself, not its inner `.server`. A handler set again after `attach` (`setRequestHandler`) keeps Wakeshift, including one that wraps the handler it replaces: Wakeshift amends each `tools/list` and strips each call once.

## Tools that subscribe

```text
attached.registerTool(name, { ...your MCP SDK's tool config, subscribes }, (args, ctx, call) => result)
```

`registerTool` is your MCP SDK's own `registerTool`, with `subscribes` added to the config and `call` appended to the handler, so an existing tool moves over unchanged:

```ts
attached.registerTool(
  "watch_ci",
  { description: "Watch CI on a repo.", inputSchema: z.object({ owner: z.string(), repo: z.string() }), subscribes: [ciFinished] },
  async ({ owner, repo }, ctx, call) => {
    if (!(await canSeeRepo(ctx.http?.authInfo, `${owner}/${repo}`))) {
      return { isError: true, content: [{ type: "text", text: "You can't see that repo." }] }
    }
    await call.subscribe(ciFinished, { owner, repo }, { reason: `Watching ${owner}/${repo}` })
    return `Watching CI on ${owner}/${repo}.`
  },
)
```

- **`subscribes`** lists the events the tool subscribes to. `call.subscribe` takes only those: anything else is a type error, and a `UsageError` ([`undeclared_subscribe`](/docs/errors#undeclared_subscribe)) when it runs. `subscribes: []`, or no `subscribes` at all, is a tool that subscribes to nothing: it registers as it would on your server, with or without a key, and with a key it still takes `session_key`, so `call.unsubscribe` can end what another tool subscribed.
- **The config is your MCP SDK's**: `title`, `description`, `inputSchema`, `outputSchema`, `annotations`, `_meta`, and any other field it takes, passed through unchanged. `inputSchema` is whatever your MCP SDK accepts, including any Standard Schema (ArkType, Valibot) on 2.x, and the handler's arguments are typed from it.
- **The handler** is `(args, ctx, call)`, or `(ctx, call)` for a tool without an `inputSchema`. It returns a string or a `CallToolResult` (a bare string works here, not on your server's own `registerTool`, which takes only a `CallToolResult`); with an `outputSchema`, a `CallToolResult` whose `structuredContent` matches it.
- **It returns your MCP SDK's `RegisteredTool`**, so `.update()`, `.disable()` and `.remove()` work.
- **What Wakeshift adds.** `tools/list` gives the tool Wakeshift's arguments, two sentences after your description (which stays as you wrote it) saying it's how your app notifies the agent of its events and that the result may ask for a listener (about 300 characters: [the text](/docs/events#subscribe-from-your-own-tools); keep your own description under about 1700), and a `wakeshift` field in its output schema. The sentence names your app as the console has it: the first `tools/list` asks Wakeshift for the name once, waiting at most 3 seconds (meanwhile it says "this app"), and remembers it; construction sends nothing. Keep descriptions under 2048 characters with the sentence, or Claude Code cuts it off (the SDK logs `description_too_long`). A one-line description that doesn't end its last sentence gets a period before the sentence, so the two don't run together. Its calls reach your handler without those arguments, and its result gets what the agent needs to be woken, after your result's validation: [exactly what](/docs/events#what-wakeshift-adds-to-a-result).
- **A failed call** (it throws, returns `isError`, returns output its schema refuses, or is canceled) ends the subscriptions it made. A result shaped `{ "error": … }` that isn't `isError` counts as success.
- **Conflicts throw at registration:** a tool named like a built-in tool, an input schema that names `session_key`, `harness` or `conversation_id`, or an output schema that names `wakeshift`.

Your tool decides who may subscribe to what: `call.subscribe` doesn't consult `authorize`. When the caller names a resource, as here, check they can see it first; when your tool minted the ID, there's nothing to check.

### The tool call

| | What it is |
| --- | --- |
| `call.subscribe(event, args, options?)` | Subscribes the calling conversation: [subscribing](#subscribing). |
| `call.unsubscribe(event, args)` | Ends the calling conversation's subscription with those arguments. `true` if one ended. |
| `call.resolveSession()` | Finds or starts the conversation's session without subscribing it ([advanced](/docs/advanced#more-of-callsubscribe)); `null`, with no request to Wakeshift, when nothing names the conversation. |
| `call.user` | Who `identify` says is calling, or `undefined`. Wakeshift's view of the caller: your own authorization rests on your own verified credential. |
| `call.tool` | The tool being called. |
| `call.native` | The caller subscribes to your events itself (MCP Events: ChatGPT). `call.nativeConversation` is its conversation, when it names one. |
| `call.recognized` | Which agent program Wakeshift recognized: `{ harness, via, claimed }`, or `null` before a subscribe. |
| `call.wakes` | Whether Wakeshift wakes this conversation for what the call subscribed, as Wakeshift decides for it: `true` only after a `call.subscribe` the call still holds, once a wake is confirmed: the idle agent is woken with nothing to install, no further call to your tools and no choice from the user still to come (Claude Code's Monitor once the user picked it, installed hooks, plugin or connector, a ChatGPT Work chat on the web subscribing natively). `false` before, while the user's choice of how to be notified is pending (Claude Code asks it the first time), on a disabled client, after a subscribe that threw or came back `unavailable`, and when the agent's program can't be woken now (ChatGPT's desktop app, a Cursor cloud agent, nothing installed yet). Skip your own polling or waiting only when it's `true` ([skip your own wait](/docs/events#skip-your-own-wait-only-when-callwakes)). |
| `call.callLimit` | How many seconds the agent's program lets one tool call take (Claude Code: 60, Codex: 300), from what Wakeshift recognized it as; set by `call.subscribe`, `undefined` before and when it isn't known. |
| `call.shouldSkipWait(seconds)` | Whether to skip a wait the agent asked for and return at once: `true` only when `call.wakes` is `true` and `seconds` reaches `call.callLimit` ([a wait longer than the program allows](/docs/events#a-wait-longer-than-the-agents-program-allows)). Keep the subscription either way. |
| `call.session` | The conversation's session, once Wakeshift has resolved it: after a `call.subscribe` that reached Wakeshift, or `call.resolveSession()`; `null` before that. It's never the raw `session_key` argument, which an agent can forge, so key nothing of your own on that. A secret: it doesn't print, and you shouldn't show, store or log it. |
| `call.ctx` | The request context your MCP SDK passed the handler. It doesn't print. |

Await each call before the handler returns: a `call` used after that throws [`call_ended`](/docs/errors#call_ended). For a tool registered elsewhere, `wake.currentCall()` is the call ([tools registered elsewhere](/docs/low-level)). Outside a call of a tool that subscribes it throws a `UsageError` with code `no_call` (check with `isWakeshiftError(err, "no_call")`); with no key it returns a call that does nothing.

### Inside a tool call

A slow or unreachable Wakeshift adds at most seconds to a tool call, so the agent's own tool timeout never runs out on Wakeshift's account:

- **Each request a call makes has its own budget, and none is retried.** `call.subscribe`, `call.unsubscribe` and `call.resolveSession` get 5 seconds; the addition to the result, and the rollback of a call that failed, get 1 second. Change them with the client's `callTimeouts` option. Past the budget, `call.subscribe` throws [`timeout`](/docs/errors#timeout), and a result goes out unchanged, as on any Wakeshift failure.
- **After 5 failures in a row, tool calls skip Wakeshift for 30 seconds.** A failure is no answer, a timeout or a 5xx; a 4xx is an answer and never counts. While skipped, `call.subscribe` throws [`unreachable`](/docs/errors#unreachable) at once, results go out unchanged, and the SDK logs [`core_unavailable`](/docs/errors#log-codes). After 30 seconds, one call goes through; if Wakeshift answers it, tool calls use Wakeshift again.
- **Emits, watches and every other request** keep `timeoutMs` and `maxRetries`, and are never skipped.

Catch `call.subscribe`'s error if your tool should go on without wakes, and skip your own polling or waiting only when `call.wakes`; otherwise behave as without Wakeshift, so your own wait covers an outage:

```ts
import { APIError } from "@wakeshift/sdk"

try {
  await call.subscribe(crawlFinished, { crawl: id })
} catch (err) {
  // No answer, a timeout, a 5xx or a refusal: go on without wakes.
  if (!(err instanceof APIError)) throw err
}
if (call.wakes) return `Started crawl ${id}.`
return await waitForCrawl(id) // as without Wakeshift
```

Don't promise the wake in your own text: Wakeshift's addition tells the agent.

## Subscribing

```text
await call.subscribe(event, args, { once?, replay?, ttl?, reason?, label? })
```

- **`once`** ends the subscription after its first delivery.
- **`replay`** also delivers the latest occurrence with these arguments from that far back, more than 0 and at most 24 hours. One rule covers when: subscribe before you start the work, without `replay`, whenever you have the ID first (your tool minted it); pass `replay` with `once` when the ID exists only once the work has started, since the work can finish before you subscribe ([subscribe before you start the work](/docs/low-level#subscribe-before-you-start-the-work)). That ID is unique, so the only occurrence `replay` can find is this job's. Leave `replay` off on arguments that repeat (a repo, a branch): an earlier occurrence would arrive at once and end a `once` subscription ([`replay` with `once`](/docs/advanced#more-of-callsubscribe)).
- **`ttl`** ends it after that long without a delivery or a renewal (default 30 days). Durations are seconds or a string: `"90s"`, `"10m"`, `"2h"`, `"30d"`.
- **`reason`** is shown to the person in the console; **`label`** names a session for an agent your app acts for ([advanced](/docs/advanced#more-of-callsubscribe)).

Subscribing again with the same arguments renews the subscription. It resolves to a `Subscription`, a union on `state`:

| `state` | Fields | Meaning |
| --- | --- | --- |
| `created`, `renewed` | `id`, `user`, `session`, `recognized` | A conversation subscription, new or renewed. Store `id` and `user` to [emit to it](/docs/advanced#reaching-one-conversation) or end it later. |
| `native` | `user`, `recognized` | The client subscribes itself through MCP Events; your tool's reply tells it what to subscribe to, with a one-time voucher when only your tools may subscribe to the event. `user` is `""` for an anonymous caller. |
| `unavailable` | `reason`: `client_unsupported`, `no_user` or `refused` | Nothing happened: an MCP Events client and an event only your tools may subscribe to that Wakeshift couldn't hand it a voucher for, such a client with no signed-in user on a `personal`, `authorize` or `public` event, or the event's `authorize` refused this caller ([who may subscribe](/docs/who-may-subscribe#chatgpt-and-other-clients-that-subscribe-natively)). |
| `disabled` | | The client has no key. |

When the work is paid for and the subscription is a convenience, do the work first and subscribe in a `try`, so a Wakeshift failure never fails a call the user paid for:

```ts
try {
  await call.subscribe(crawlFinished, { job: "j1" }, { replay: "1h", once: true })
} catch (err) {
  console.error("couldn't subscribe", err instanceof Error ? err.name : err)
}
```

## Emit

```text
await event.emit(args, payload, { to?, id? })
```

```ts
const { matched, duplicate } = await crawlFinished.emit({ job: "3f2a" }, { status: "completed", pages: 42 }, { id: "crawl-3f2a" })
```

- **`id`** makes retries emit once, for a day: a retry with the same `id` returns the first emit's result with `duplicate: true`. The same `id` on a different emit throws `IdReusedError` ([`id_reused`](/docs/errors#id_reused)). When the SDK retries an emit itself, it never wakes twice.
- **`to`** is required for a personal event, and refused for any other; the types enforce both when `personal` is a literal. It takes a user, `{ user, subscription }` or `{ native: user }`, or a list of up to 1,000. **Take it from your own record of who owns what**, never from the subscriber list.
- **The result**: `matched` counts the subscriptions the occurrence reached, each woken now or as soon as its agent can take it, not agents awake now. `unmatched` lists the users nothing reached, `ended` the subscriptions that are no longer live, and `textTrimmed` says Wakeshift trimmed a text over 1000 bytes. The payload is at most 64 KB.

## Publish URLs

```ts
if (wake.enabled) {
  const hook = await ciFinished.createPublishUrl({ owner: "acme", repo: "api" }, { signingSecret: process.env.SVIX_SECRET })
  await registerWebhook(hook.url, hook.id)
}
```

- `event.createPublishUrl(args, { to?, signingSecret? })` mints a new URL that emits the event with those arguments each time something posts to it. `to` is the one user a personal event's URL is for. `signingSecret` is the sender's Standard Webhooks secret (`whsec_…`): the URL then accepts only posts signed with it, and the result's `signing` carries its fingerprint, never the secret.
- `wake.revokePublishUrl(id)` turns it off; it answers `410` from then on.
- The URL is a credential: `JSON.stringify` and `util.inspect` of a `PublishUrl` leave out its token. Pass `.url` explicitly where it's meant to go.
- On a client without a key, `createPublishUrl` throws `disabled`, so guard it with `wake.enabled` where a tool offers notifications.

How each post becomes an occurrence, and how to use them safely: [publish URLs](/docs/publish-urls).

## Sign-in: identify and authorize

```ts
const signedIn = wake.attach(server, {
  identify: async (ctx) => {
    const user = await userOf(ctx.http?.authInfo) // your own sign-in, from the token your auth verified
    return user ? { user } : undefined
  },
  requireUser: true, // refuse anonymous callers: a session_key is never a bearer
})
```

- **`identify(ctx)`** returns `{ user?, client?, agent? }`: your ID for the person; on a stateless server, the client's name as your sign-in knows it ([your users](/docs/users#who-a-subscriber-is)); and an agent your app acts for ([advanced](/docs/advanced)). It gets the request's context: on 2.x the verified token is `ctx.http?.authInfo`, on 1.x `extra.authInfo`.
- **Your own auth, as it is.** `identify` reads whatever your HTTP layer verified, without touching the request: the token your auth middleware put in `req.auth` (the MCP SDK's convention, which `requireBearerAuth` follows), or the headers themselves. `toNodeHandler` passes `req.auth` on as `ctx.http?.authInfo`, and 1.x's `StreamableHTTPServerTransport` as `extra.authInfo` when your code hands it the request. A framework that runs the transport for you may keep the verified token in its own request context instead and pass nothing as `authInfo` (mcp-use 1.x does): then `identify` reads the user from that context ([mcp-use](/docs/frameworks#mcp-use)). An `identify` that finds no user makes the call anonymous, so check it against a signed-in request. An API key your server checks itself:

  ```ts
  wake.attach(server, {
    // 2.x: ctx.http?.req is the request; on 1.x, extra.requestInfo?.headers["x-api-key"]
    identify: async (ctx) => {
      const account = await accountForApiKey(ctx.http?.req?.headers.get("x-api-key")) // your own check
      return account ? { user: account.id } : undefined
    },
  })
  ```

- **It runs only when Wakeshift needs it**, at most once per request: a call of a tool that subscribes, a built-in tool, an MCP Events request. Never for `initialize`, `tools/list` or your other tools.
- **If it throws, rejects or takes over 10 seconds, Wakeshift's part of the request is refused**: `call.subscribe` throws [`identify_failed`](/docs/errors#identify_failed), the built-in tools answer `isError`, and MCP Events requests are refused. Your tool's own code still runs.
- **`requireUser: true`** refuses an anonymous caller the same way, with [`user_required`](/docs/errors#user_required) in place of `identify_failed`. Without a user, a `session_key` is a bearer for its conversation. Set it whenever your server requires sign-in.
- **`authorize(subscriber, args)`** on an event decides `subscribe_events` and native subscriptions. `subscriber` is `{ user, client, native, authInfo?, headers?, ctx }`: `authInfo` is the caller's verified token, the same shape on 1.x and 2.x. Never decide on `client` or `native`, which the client asserts. A throw or a run over 10 seconds refuses. See [who may subscribe](/docs/who-may-subscribe#shared-resources-authorize).

## Logging and secrets

The SDK logs to stderr by default (`console.error` and `console.warn`, for every level). `logger` takes `{ error, warn, info?, debug? }`, each called as `(message, fields)`, and `logLevel` (`"debug"`, `"info"`, `"warn"`, `"error"` or `"off"`, default `"info"`) sets the threshold. Every line's fields carry its `code` ([log codes](/docs/errors#log-codes)), and the `event` or `tool` it's about. pino takes `(fields, message)`, so adapt it in one line:

```ts
import pino from "pino"

const log = pino()
const quiet = new Wakeshift({
  logger: { warn: (m, f) => log.warn(f, m), error: (m, f) => log.error(f, m), info: (m, f) => log.info(f, m) },
})
```

- **A Wakeshift failure never changes your tool's result.** If Wakeshift's addition to a result can't be had (an error, a timeout, Wakeshift unreachable, a refused key), the agent gets your result exactly as your handler returned it, never an error and never a request to call again; what the call subscribed is ended, and `block_failed` is logged, at most once per five minutes.
- **Never logged:** keys, session keys, your callers' tokens and headers, signing secrets, publish URL tokens, payloads, tool arguments, request bodies, and your own code's error messages. Lines carry a code and an error's type.
- **Nothing prints its secret.** `JSON.stringify` and `util.inspect` of the client show `Wakeshift { app, core }`; `call.session` and a `Subscription`'s `session` don't print; `call.ctx`, a subscriber's `authInfo`, `headers` and `ctx` don't print; a `PublishUrl` prints without its token, and an `Agent` without its token.
- **`redactSecrets(text)`** replaces every Wakeshift secret format in a string with `[redacted]`, for logs the SDK can't reach: your own request logging, a proxy, Sentry's `beforeSend`.

Wakeshift takes its arguments out of every tool call before your code runs, so `session_key` never reaches your handlers, middleware or logs unless you log raw request bodies. Don't. An agent can pass any `session_key`, so never key your own data on the raw argument: `call.session` is set only once Wakeshift has resolved the conversation (after a `call.subscribe` that reached it, or `call.resolveSession()`), and is `null` before.

### Stdio servers

A stdio MCP server's stdout is its JSON-RPC stream, so the SDK never writes to stdout: its default logger writes every level to stderr, with or without `WAKESHIFT_KEY`. Without a key it logs nothing at the default level (`no_key` is debug), so the server's output is exactly what it was. A `logger` you pass must keep to stderr too. Everything on this page works over stdio, on 1.x protocol 2026-07-28 included; of the [frameworks](/docs/frameworks), punkpeye's `fastmcp` over stdio needs fastmcp 4 and stays on 2025-11-25 ([why](/docs/frameworks#fastmcp-punkpeye)). `attach` on 1.x adds 2026-07-28 to the MCP SDK's own list of supported versions, which the whole process shares, so every 1.x server in the process accepts it, stdio ones included. That's intended.

**What a stdio server can't do.** It runs only while the agent has it open, so a [`watch`](/docs/events#wait-on-work-you-can-only-poll) or any background check of yours ends when the agent closes the server, and a job that finishes hours later may lose its wake: emit long jobs from a webhook or a hosted server. And ChatGPT connects only to remote servers over HTTPS, so it can't reach a stdio server; serve one over HTTP to reach it.

**Where the key lives when your stdio server is a package.** A stdio server runs on each user's machine, so never ship your app key inside the package: anyone could read it and act as your app. Either each person who runs it creates their own app in the console and sets their own `WAKESHIFT_KEY` (without one, the server runs as it would without Wakeshift), or you serve Wakeshift from your hosted HTTP server, where the key stays with you, and the package talks to that.

## Errors

Every error is a `WakeshiftError` with a `code`:

| Class | When |
| --- | --- |
| `APIError` | Wakeshift answered with an error: `status`, `code`, the message, and `requestId` when the answer has one. |
| `APIConnectionError` | An `APIError` with `status: null`: no answer. `unreachable`, or `timeout`, whose outcome is unknown: retry with the same `id`. |
| `IdReusedError` | An `APIError`: an emit `id` used for a different emit. |
| `UsageError` | A mistake caught before sending: bad arguments or payload (`issues`: `{ message, path }[]`), configuration, an undeclared subscribe, a disabled client, no call, a failed `identify`. |

`isWakeshiftError(err, code?)` checks one: its `code` accepts only known codes, so a typo doesn't compile, and it narrows to that code's class. It works across duplicate installs of the SDK.

```ts
try {
  await notification.emit({}, payload, { to: members, id: messageId })
} catch (err) {
  if (isWakeshiftError(err, "rate_limited")) return retryLater(messageId)
  throw err
}
```

Each message ends with a link to its code on [errors](/docs/errors), which lists every code with its fix. A `signal` you abort rethrows its own reason, never a `WakeshiftError`. In a webhook receiver, answer the sender by what the emit did:

| The emit | Answer the sender |
| --- | --- |
| Succeeded, or `duplicate` | 2xx |
| `id_reused`, `bad_arguments`, `unknown_event` | 2xx or 422, and log it: a retry won't help |
| `rate_limited` | 503 with a `Retry-After` |
| `unreachable`, `timeout`, or a 5xx from Wakeshift | 502 or 503, so the sender retries with the same delivery ID as `id` |
| A bad signature, before emitting | 401 |

## Cloudflare Workers

The SDK needs the `nodejs_compat` compatibility flag on Workers: it signs with `node:crypto` and finds each tool call with `AsyncLocalStorage`. Without the flag, `attach` throws, naming it. Cloudflare's `createMcpHandler` from `agents/mcp/server` takes the same kind of factory as 2.x's, so attach inside it, and make the client from the Worker's `env`:

```ts
import { env } from "cloudflare:workers"
import { createMcpHandler } from "agents/mcp/server"
import { McpServer } from "@modelcontextprotocol/server"
import { z } from "zod"
import { Wakeshift } from "@wakeshift/sdk"

const wake = new Wakeshift({ key: env.WAKESHIFT_KEY })

const crawlFinished = wake.defineEvent("crawl.finished", {
  description: "A crawl finished",
  args: z.object({ job: z.string() }),
  payload: z.object({ status: z.string() }),
})

const handler = createMcpHandler(() => {
  const server = new McpServer({ name: "crawler", version: "1.0.0" })
  const attached = wake.attach(server)
  attached.registerTool(
    "start_crawl",
    { description: "Crawl a site.", inputSchema: z.object({ url: z.string() }), subscribes: [crawlFinished] },
    async ({ url }, ctx, call) => {
      const job = crypto.randomUUID()
      await call.subscribe(crawlFinished, { job }, { once: true })
      return `Crawl ${job} of ${url} started.`
    },
  )
  return server
})

export default {
  fetch: (request: Request, env: Env, ctx: ExecutionContext) => handler(request, env, ctx),
}
```

```toml wrangler.toml
compatibility_flags = ["nodejs_compat"]
```

A [`watch`](/docs/events#wait-on-work-you-can-only-poll) doesn't survive on Workers: a Worker lives only as long as its request (`waitUntil` adds about 30 seconds), so a check that runs for minutes is lost. Run the check from a Durable Object instead, one per job, whose alarm checks the job and sets itself again until the job is done, then emits:

```ts
import { DurableObject, env } from "cloudflare:workers"

// One per job: its alarm checks the job every 15 seconds, and emits once it's done.
export class CrawlWatch extends DurableObject<Env> {
  async start(job: string) {
    await this.ctx.storage.put("job", job)
    await this.ctx.storage.setAlarm(Date.now() + 15_000)
  }

  async alarm() {
    const job = await this.ctx.storage.get<string>("job")
    if (!job) return
    const s = await api.status(job) // your API's status call
    if (!s.done) {
      await this.ctx.storage.setAlarm(Date.now() + 15_000) // not yet: check again
      return
    }
    // A throw here makes Cloudflare retry the alarm; the id makes a retry emit once.
    await crawlFinished.emit({ job }, { status: s.state, pages: s.pages }, { id: `crawl-${job}` })
    await this.ctx.storage.deleteAll()
  }
}

// In start_crawl, after call.subscribe, before the tool replies:
async function watchCrawl(job: string) {
  await env.CRAWL_WATCH.get(env.CRAWL_WATCH.idFromName(job)).start(job)
}
```

Bind the class in `wrangler.toml` (`[[durable_objects.bindings]]` with `name = "CRAWL_WATCH"`, `class_name = "CrawlWatch"`, and a migration that creates it). Start it from the tool that mints the job, and await it: `onSubscribe` runs in the background, which a Worker doesn't wait for. For a tools-only event that covers every subscription, a ChatGPT voucher's included, since each comes from your tool's call. An alarm survives restarts and deploys, so the check carries on for hours if the job takes them.

Cloudflare's older `McpAgent` keeps a session per client, so it refuses MCP protocol 2026-07-28 and can't serve the clients that speak only that revision. Use `createMcpHandler`.

## Using @modelcontextprotocol/sdk 1.x

`attach` takes an `McpServer` from either MCP TypeScript SDK and tells them apart by the server you pass, so everything on this page works the same on `@modelcontextprotocol/sdk` 1.x (1.23 or later), with zod 3.25 or later. `attached.registerTool` takes 1.x's config: its `inputSchema` is a zod shape.

```sh
npm install @wakeshift/sdk @modelcontextprotocol/sdk zod
```

1.x serves HTTP with its `StreamableHTTPServerTransport`. With sessions, each session gets its own `McpServer`: attach before connecting it.

```ts
import { createServer } from "node:http"
import { randomUUID } from "node:crypto"
import { McpServer } from "@modelcontextprotocol/sdk/server/mcp.js"
import { StreamableHTTPServerTransport } from "@modelcontextprotocol/sdk/server/streamableHttp.js"
import { z } from "zod"

function newSessionServer() {
  const server = new McpServer({ name: "ci-bot", version: "1.0.0" })
  // identify gets 1.x's extra: the verified token is extra.authInfo.
  const attached = wake.attach(server, { identify: (extra) => ({ user: myUser(extra.authInfo) }) })
  attached.registerTool(
    "watch_ci",
    { description: "Watch CI on a repo.", inputSchema: { owner: z.string(), repo: z.string() }, subscribes: [ciFinished] },
    async ({ owner, repo }, extra, call) => {
      await call.subscribe(ciFinished, { owner, repo }, { reason: `Watching ${owner}/${repo}` })
      return `Watching CI on ${owner}/${repo}.`
    },
  )
  return server
}

const sessions = new Map<string, StreamableHTTPServerTransport>()

createServer(async (req, res) => {
  const id = req.headers["mcp-session-id"] as string | undefined
  let transport = id ? sessions.get(id) : undefined
  if (!transport) {
    if (id) return void res.writeHead(404).end()
    const created = new StreamableHTTPServerTransport({
      sessionIdGenerator: () => randomUUID(),
      onsessioninitialized: (sid) => void sessions.set(sid, created),
    })
    created.onclose = () => void (created.sessionId && sessions.delete(created.sessionId))
    await newSessionServer().connect(created)
    transport = created
  }
  await transport.handleRequest(req, res)
}).listen(3000)
```

| On 1.x | |
| --- | --- |
| Request context | Your handler, `identify` and `isNativeClient` get 1.x's `extra` instead of `ctx`: the verified token is `extra.authInfo` when your transport passes one: 1.x's `StreamableHTTPServerTransport` copies `req.auth` into it when your code calls its `handleRequest`. A framework that runs the transport for you may not (mcp-use 1.x doesn't): read the user from the framework's own request context in `identify`, as [mcp-use](/docs/frameworks#mcp-use) does. The HTTP headers are `extra.requestInfo?.headers`. |
| Schemas | `inputSchema` and `outputSchema` are zod shapes, as 1.x's `registerTool` takes them. |
| Protocol | With sessions, clients connect with `initialize`. Clients that speak only MCP protocol 2026-07-28, such as OpenClaw, connect over stdio or to a stateless transport (no `sessionIdGenerator`, a server for each request), where `attach` answers that revision for 1.x. A server with sessions can serve them too: [below](#serving-protocol-2026-07-28-beside-sessions). |
| Node | 1.x loads through `require(esm)`, on by default from Node 22.12 (and 20.19). Without it, `attach` throws, naming the Node version it needs. |
| Versions | The peer range takes any 1.x, so installing never fails; `attach` needs 1.23 or later, and logs `mcp_patch_unavailable` on an older one. |
| Recognizing the client | On a stateless server, tool calls on older revisions don't carry `initialize`'s client name, so Wakeshift recognizes Claude Code and the others by the request's `User-Agent` (Claude Code's starts with `claude-code/`), which the transport passes in `extra.requestInfo.headers`. A transport of your own must pass the request's headers there. |

### Serving protocol 2026-07-28 beside sessions

Protocol 2026-07-28 has no sessions: its requests come without `Mcp-Session-Id`, with `MCP-Protocol-Version: 2026-07-28`. Answer those with a server of their own on a stateless transport, and keep your sessions for the rest.

**Your auth runs first, on every protocol.** This branch answers MCP requests, `tools/list` and `tools/call` included, so put it where your auth has already run: inside the handler your auth guards, after the check, never ahead of it. Registered ahead of your framework's auth (a Hono or Express middleware added before the framework's OAuth middleware, say), it answers 2026-07-28 requests with no token at all; in a test, such a request listed every tool. In the handler above:

```ts
import { createServer } from "node:http"
import { StreamableHTTPServerTransport } from "@modelcontextprotocol/sdk/server/streamableHttp.js"

createServer(async (req, res) => {
  // Your auth first: verify the token, set req.auth, and answer 401 yourself.
  // A request it refuses never reaches MCP, whatever its protocol.
  if (!(await authenticate(req, res))) return
  const id = req.headers["mcp-session-id"] as string | undefined
  if (!id && req.headers["mcp-protocol-version"] === "2026-07-28") {
    const server = newSessionServer()
    const stateless = new StreamableHTTPServerTransport({ sessionIdGenerator: undefined })
    res.on("close", () => void server.close())
    await server.connect(stateless)
    return stateless.handleRequest(req, res)
  }
  // The session lookup above, unchanged.
}).listen(3000)
```

`authenticate` stands for your own sign-in. Check the result before you ship: a 2026-07-28 `tools/list` without a token must get the same 401 an older client gets.

**If your framework answers stateless requests itself, use that instead.** Then its own handler, behind its own auth, serves 2026-07-28, and you add no transport of your own. mcp-use 1.x, for one, answers a request without `Mcp-Session-Id` statelessly when the request doesn't accept `text/event-stream`, so a middleware that only rewrites a 2026-07-28 request's `Accept` header is all it takes, and the request still goes through mcp-use's OAuth: see [mcp-use](/docs/frameworks#mcp-use). Rewriting a header lets nothing through that the framework's auth wouldn't let through anyway.

**If your server shapes its tools at connect**, this server for each request doesn't get that. A server built per request sees no `initialize`, so whatever your session server decides from it (tools enabled for the client's capabilities, a confirmation step added for clients that support elicitation, tools hidden by the client's name) has to be decided again from what each request carries, or those clients get the default tool set. Check `tools/list` on a 2026-07-28 request against a session's before you ship it. Elicitation and sampling need the client to answer back on the same connection, so a stateless request can't use them.

### Moving from 1.x to 2.x

Moving a server from 1.x to 2.x moves where your own code reads a request's fields, in `identify`, `isNativeClient` and your tool handlers. Fields read the 1.x way don't fail on 2.x. They come back `undefined`, and code that doesn't type its context as 2.x's still compiles, so a user your sign-in recognized on 1.x silently goes unrecognized. Check each read:

| What you read | On 1.x (`extra`) | On 2.x (`ctx`) |
| --- | --- | --- |
| The verified token | `extra.authInfo` | `ctx.http?.authInfo` |
| An HTTP header | `extra.requestInfo?.headers[name]` | `ctx.http?.req?.headers.get(name)` |
| Your own `_meta` keys | `extra._meta` | `ctx.mcpReq._meta` |
| The reserved `io.modelcontextprotocol/*` `_meta` keys (protocol version, client info, client capabilities) | `extra._meta` | `ctx.mcpReq.envelope`, on protocol 2026-07-28: 2.x moves them out of `ctx.mcpReq._meta` |
| The request's cancellation | `extra.signal` | `ctx.mcpReq.signal` |

Wakeshift reads both shapes itself, so what it sees doesn't change: only your own reads move. Write your schemas, events' included, with zod 4 (4.2 or later), which 2.x requires.

## Mastra

A Mastra `MCPServer` (`@mastra/mcp` 2.7 or later) builds its MCP server itself, so it has an entry point of its own, `@wakeshift/sdk/mastra`. Wrap the `execute` of each Mastra tool that subscribes, and serve your `MCPServer` with the attachment:

```ts
import { createTool } from "@mastra/core/tools"
import { MCPServer } from "@mastra/mcp"
import { z } from "zod"
import { attachMastra } from "@wakeshift/sdk/mastra"

// identify gets the request's MCP context: the verified token is ctx.http?.authInfo.
const mastra = attachMastra(wake, { identify: (ctx) => ({ user: myUser(ctx.http?.authInfo) }) })

const watchCi = createTool({
  id: "watch_ci",
  description: "Watch CI on a repo",
  inputSchema: mastra.inputs(z.object({ owner: z.string(), repo: z.string() })),
  execute: mastra.events("watch_ci", { subscribes: [ciFinished] }, async ({ owner, repo }: { owner: string; repo: string }, call) => {
    await call.subscribe(ciFinished, { owner, repo }, { reason: `Watching ${owner}/${repo}` })
    return `Watching CI on ${owner}/${repo}.`
  }),
})

const mcp = mastra.serve(new MCPServer({ id: "ci-bot", name: "CI bot", version: "1.0.0", tools: { watch_ci: watchCi } }))
```

| Call | What it does |
| --- | --- |
| `attachMastra(wake, { identify?, requireUser?, builtinTools?, isNativeClient? })` | `attach`'s options, after your last `defineEvent`. `identify(ctx)` gets the request's MCP context, as `attach`'s does. |
| `mastra.inputs(schema)`, `mastra.output(schema)` | Your tool's input and output schemas with Wakeshift's arguments and `wakeshift` field, as Mastra's tools declare them. |
| `mastra.events(name, { subscribes }, handler)` | Wraps a Mastra tool's `execute`, for the tool your `MCPServer` serves under `name` (its key in `tools`). `handler(args, call, context)` returns what a Mastra tool returns. Outside an MCP client's call (an agent, a workflow or Studio runs the tool), `call.subscribe` throws. |
| `mastra.serve(server)` | Serves Wakeshift from your `MCPServer`, and returns it. Call it before the server takes requests. It throws for an `@mastra/mcp` outside 2.7–2.x. |

Serve the `MCPServer` as Mastra's docs do: `startHTTP`, `startStdio`, or on your `Mastra` instance. It gets everything `attach` gives a server. Your own MCP Events (`MCPServer`'s `events`) keep working beside Wakeshift's. A wrapped tool answers in one call: if it becomes a Mastra task or asks the client for input, what it subscribed ends.

## xmcp

xmcp builds its MCP server from your tool files and writes `tools/list` itself, so Wakeshift comes in through each tool's definition, with `attachTools` from `@wakeshift/sdk/tools`: `tools.tool(name, { description, inputSchema, subscribes }, handler)` returns xmcp's `{ schema, metadata, handler }`, and `tools.builtins()` the built-in tools, one file each. [A runtime with no MCP SDK server](/docs/low-level#a-runtime-with-no-mcp-sdk-server) shows both. xmcp has no way to serve MCP methods of an app's own, so an xmcp app has no MCP Events, and clients that only subscribe natively, such as ChatGPT, can't subscribe there.

## The CLI

`@wakeshift/cli` sets up your app's key and checks your server, from the terminal (Node 22 or later):

```sh
npx -y @wakeshift/cli init
npx -y @wakeshift/cli verify --url http://localhost:3000/mcp --tool watch_ci --args '{"owner":"acme","repo":"wsverify-3f9a"}'
```

- **`init`** makes your app's key on your computer, writes it to `.env` as `WAKESHIFT_KEY` (and makes sure git ignores `.env`), and prints a link. Open it, sign in and confirm: that creates the app, named `<project>-dev` unless you pass `--name`. Wakeshift only ever gets the key's public half, and the key is never printed.
- **`verify`**, with your server running, acts as an agent would: it calls the tool that subscribes, checks its description and reply, emits a test event (or runs `--trigger tool:<name>` or `--trigger cmd:<command>`) only if nobody else is subscribed with those arguments, waits for the wake, unsubscribes, and says what to fix for anything that failed. A trigger can use what the subscribing tool returned: `{result.<path>}` is a value in the JSON its text starts with, `{structured.<path>}` one in its `structuredContent` (`--trigger 'tool:cancel_build:{"number":"{result.number}"}'`). Only an occurrence after the trigger counts as the wake, not one a subscription with `replay` delivers from earlier.
- **`status`** shows the app, its key and the events Wakeshift has from your server; **`keys new`** adds a new key the same way as `init`.

Its source is in [github.com/wakeshift/connectors](https://github.com/wakeshift/connectors) (`sdk/cli`), and `npx -y @wakeshift/cli --help` lists every option.

## Users

| Call | What it does |
| --- | --- |
| `wake.forgetUser(user)` | Ends the user's subscriptions and deletes what Wakeshift keeps for them. `true` if there was something to delete, `false` after that. See [forgetting a user](/docs/users#forgetting-a-user). |
| `wake.publishManifest(manifest)` | Replaces your app's manifest: `description`, `agent`, `othersCanWake`, `preapproveTools`, and events defined elsewhere. Optional, and the way to remove an event ([advanced](/docs/advanced#the-manifest)). |

Acting for your users' agents (`wake.workspace`, `wake.addAgent`, `wake.actAs`, `wake.checkAgent`) is on [advanced](/docs/advanced).
