# Events

An event is something your app offers to wake agents with, such as `crawl.finished` or `chat.message`. You define each event once, let your tools subscribe the conversations that call them, and emit an occurrence whenever it happens. Every conversation subscribed with matching arguments wakes up.

Events follow MCP Events, the Model Context Protocol's standard for this. Clients that speak it, such as ChatGPT, can subscribe natively when the event allows it; every other agent subscribes through your tools, and Wakeshift wakes it the way that agent can be woken. Your code is the same for all of them.

## Define an event

```ts
const message = wake.defineEvent("chat.message", {
  description: "A new message in a channel",
  args: z.object({ channel: z.string(), from: z.string().optional() }),
  payload: z.object({ from: z.string(), text: z.string() }),
  authorize: (subscriber, { channel }) => canRead(subscriber.user, channel),
  text: (m, { channel }) => `${m.from} in #${channel}: ${m.text}`,
})
```

| Option | What it is |
| --- | --- |
| name | What clients subscribe to: lowercase words joined by dots (`chat.message`, `game.turn`), at most 64 characters, not starting with `wakeshift.`. |
| `description` | What happened, for the agent and for the person who sees the subscription. |
| `args` | The event's arguments: what a subscriber picks, as a zod object (Python: a pydantic model or a JSON Schema; Go: a type). Its **required** string, integer (`z.number().int()`) and boolean fields route occurrences: an occurrence reaches the subscribers who picked the same values. An **optional** field filters: a subscriber who sets it only gets occurrences whose payload field of the same name has that value. |
| `payload` | What each occurrence carries. `emit` checks it before sending. |
| `text` | Optional: the line the agent reads first, from the payload and the arguments. Without it, the agent reads the description and the arguments: "A new message in a channel (channel eng)". |
| `personal`, `authorize`, `public` | Who may subscribe beyond your own tools. With none of them, only your tools can. See [who may subscribe](/docs/who-may-subscribe). |
| `deleteOnDelivery` | Wakeshift deletes each occurrence's text and payload once it's delivered, keeping only that it happened (when, and where it went). An agent can't have it again after that. Use it for messages people wouldn't expect a third party to keep, such as chat messages. |

### Writing event descriptions

Write the description as a plain statement of what happened: "A build finished: it passed, failed or was canceled.", "A new message in a channel.", "It's your move at a table." Agents read it in the description of every tool that subscribes to the event, as "This tool is how Shipyard notifies you in this conversation of this event: A build finished: it passed, failed or was canceled.", followed by a sentence on how Shipyard delivers it ([the whole text](#subscribe-from-your-own-tools)). So don't write it as instructions to the agent ("Subscribe after join, with the game's ID"): say how to subscribe in the tool's own description.

### Argument values

Routing values are `a-z 0-9 . _ -`, at most 100 characters, and Wakeshift never rewrites one: a value with anything else is refused. **Never lowercase or trim a case-sensitive ID** to fit: `Run-A1` and `run-a1` would become one. Encode it instead, on every path that subscribes or emits:

```ts
import { encodeArgValue } from "@wakeshift/sdk"

const jobArg = encodeArgValue("Run-A1") // ".run-.a1": one value per ID
encodeArgValue("dep-abc123") // "dep-abc123": lowercase IDs stay as they are
```

`encodeArgValue` (Python `encode_arg_value`, Go `wakeshift.EncodeArgValue`) keeps `a-z 0-9 - _` as they are, so ordinary IDs read the same; it writes a capital as `.` and its lowercase letter, and every other byte, `.` included, as `..` and two hex digits. A result over 100 characters becomes `..h` and 40 hex digits of its SHA-256. `decodeArgValue` reverses it, except for the hashed form. Use encoded values on events only your tools subscribe to, or personal ones; on an event with `authorize`, refuse any value that isn't one `encodeArgValue` would produce, and check ownership of the decoded ID. Encoding isn't secrecy. IDs that are case-insensitive, such as GitHub's owner and repo names, can be lowercased.

An agent reads argument values as they are, in the event's default text ("A deploy finished (deploy .dep-.a1)") and in its subscriptions, and an encoded one isn't an ID your other tools take. So when an ID needs encoding, put the raw ID in the payload too (or in `text`), and let agents use that one with your tools.

### Routing

An occurrence's topic is `ev/<name>/` followed by its required arguments' values, in alphabetical order of the arguments' names. A personal event's topic is each subscriber's own: Wakeshift puts it under a private prefix made from your user ID, `u/p<hash of the user ID>/ev/<name>/…`, which is what a Monitor line's `topic` shows, and which takes 34 of a topic's 200 characters. If that doesn't fit, `topic` and `filter` set your own. A `filter` without a `topic` adds to the optional fields' filters; one that names the same payload field as an optional field is an error from `defineEvent`, which names the field.

A `topic` is segments joined by `/`, with `{argument}` placeholders for required arguments: `builds/{pipeline}/{number}`. Wakeshift refuses a manifest (`400 ambiguous_topic`, naming the topics) where one topic could reach the wrong subscribers:

- **One placeholder per segment.** A value can contain `.`, `_` and `-`, so `jobs/{org}-{job}` makes `jobs/acme-prod-42` from both `{org: "acme-prod", job: "42"}` and `{org: "acme", job: "prod-42"}`. Write `jobs/{org}/{job}`. Fixed text beside a single placeholder is fine: `jobs/job-{job}`.
- **No two events on topics that can be the same.** `acct/{id}` for `invoice.paid` and `acct/{id}` for `acct.activity` would each get the other's occurrences, bypassing `invoice.paid`'s `authorize`; so would `acct/{id}` and `acct/all`. Give each event a fixed segment of its own, as the default topic (`ev/<name>/…`) does: `acct/{id}/invoices` and `acct/{id}/activity`.

Define every event before `attach`. Your MCP server adds your events to your app's manifest the first time it needs them, and updates any whose definition changed. It never removes one, so several processes can share your key, each with its own events. A process that only emits, such as a webhook worker or a script, defines the events it emits, and sends them only if Wakeshift doesn't know one yet: when Wakeshift answers an emit with `unknown_event`, the SDK adds the process's events, then emits again. So you can emit at startup, before any agent has connected to your server. To remove an event, stop defining it in every process, then publish your whole manifest ([advanced](/docs/advanced#the-manifest)).

## Serve them

```ts
const attached = wake.attach(server, { identify: (ctx) => ({ user: userOf(ctx.http?.authInfo) }) })
```

`attach` adds three tools after your own, and answers MCP Events requests (`events/list`, `events/subscribe`, `events/poll`, `events/stream`) for clients that speak it:

- `subscribe_events`, listed only when one of your events is open to it (`personal`, `authorize` or `public`), for agents that don't subscribe natively. A server whose events are all tools-only gets none.
- `unsubscribe_events` and `list_subscriptions`, for every agent, so a person can always see and stop what they're subscribed to by asking their agent. With a signed-in user ([who a subscriber is](/docs/users#who-a-subscriber-is)) they list and end that user's subscriptions, from any conversation; without one, the calling conversation's own, found by the `session_key` the agent passes (both tools list it).

A server where nothing can make a subscription lists none of them: no tool of it is declared with `subscribes` (registered through the attachment or named in `wrap`, even with an empty list) and no event is open to `subscribe_events`. That's decided at each `tools/list`, so a server that serves only part of your tools (one per product, say) doesn't carry them where they'd do nothing.

Turn one off with `builtinTools` if your own tools cover it, or all of them with `builtinTools: false` (Python `builtin_tools=False`, Go `BuiltinTools: wakeshift.BuiltinTools{DisableAll: true}`):

```ts
const noSubscribeTool = wake.attach(server, { identify: (ctx) => ({ user: userOf(ctx.http?.authInfo) }), builtinTools: { subscribe: false } })
```

Python takes `builtin_tools={"subscribe": False}` and Go `BuiltinTools: wakeshift.BuiltinTools{DisableSubscribe: true}`. Turn off `list` or `unsubscribe` only if your own tools let a person see and stop their subscriptions. The same option turns on Claude Code's wait tool, which is off by default ([the wait tool for Claude Code](/docs/agents#the-wait-tool-for-claude-code)). The built-in tools' names are exported, for tests and for code that filters your tool list: `BUILTIN_TOOL_NAMES` in TypeScript and Python, `wakeshift.BuiltinToolNames()` in Go (`subscribe_events`, `unsubscribe_events`, `list_subscriptions` and `wakeshift_wait`).

**When to turn off `subscribe_events` on a personal event.** A personal event is open to `subscribe_events`, so an agent can subscribe to it with any arguments. That's right when your app emits to the user for anything they could name (their messages, their turn at any table). It's wrong when only your own tool knows the arguments an emit will use, such as a run ID your background check mints after the call: a subscription made through `subscribe_events` would name arguments no emit ever uses, and never fire. Then turn it off (`builtinTools: { subscribe: false }`) and subscribe from your tool.

**Your app's listing.** The built-in tools are tools of your server, so every client sees them in `tools/list`, ChatGPT included: an app directory's copy of your tool list, or a submission that names your tools, includes them while Wakeshift is on.

The tools' wording is fixed, and the same in every SDK, as are their annotations and output schemas: each answers with `structuredContent` too, with Wakeshift's text in a `wakeshift` field, so hosts and app stores that require an output schema of every tool accept them. Call `attach` once on each server, before it takes requests: in the function that makes each server for `createMcpHandler`, or before connecting it to a transport. It returns the attachment your own tools [register through](#subscribe-from-your-own-tools).

`user` is your own ID for the person behind a request, from your own sign-in ([your users](/docs/users)); subscriptions belong to that user. Personal events need a user, and so do native subscriptions a client makes on its own, without a voucher from your tool.

## Emit

```ts
const { matched, duplicate } = await message.emit(
  { channel: "eng" },
  { from: "ada", text: "shipped it" },
  { id: deliveryId },
)
```

- The routing arguments say who gets it: here, everyone subscribed to `eng`. Optional (filter) arguments aren't passed to `emit`; they match against the payload.
- `id` makes retries safe. A retry of the same emit (the same event, arguments, recipients and payload) with the same `id` within a day sends nothing new and returns the first emit's result with `duplicate: true`, including that emit's `matched`. The same `id` on a different emit throws `id_reused` and sends nothing, so give each occurrence its own `id`: a timestamp or a commit SHA that two occurrences can share isn't one. An `id` is at most 200 bytes; a longer one is refused. Clients that follow the MCP Events standard get your `id` as the occurrence's `eventId`, so pass the upstream's own ID when there is one (a Stripe `evt_…`, a GitHub delivery GUID). An `id` with spaces or characters outside printable ASCII still dedupes your emits, and those clients get Wakeshift's delivery ID instead ([MCP Events](/docs/mcp-events#the-standard-and-where-wakeshift-differs)).
- `matched` counts the subscriptions the occurrence reached: each conversation is woken now, or gets it as soon as its agent can take it. It doesn't count agents awake right now.
- Wakeshift trims text over 1000 bytes to 1000, cut between characters and ending with "…", and the emit still goes, so there's no need to trim it yourself. The result then says `textTrimmed: true` (Python `text_trimmed`, Go `TextTrimmed`), and the SDK logs `text_trimmed` once per event. The payload is at most 64 KB; a larger one is refused. The whole request may be 512 KB, room for a list of 1,000 recipients.

**Why text is capped.** The text is the wake's headline, the line the agent reads first; the details belong in the payload and your tools. Every wake costs the user context and tokens, some agents cut long text short when they're woken, and text from your app that enters an agent's context should stay bounded. For Claude Code the tighter limit is its Monitor line: about 250 bytes for the text and the payload together, when an argument is a UUID ([the Monitor line's budget](/docs/testing#claude-code)).

## Personal events

A personal event goes only to the subscribers you name, so an occurrence can carry something only they may see: their cards in a game, their own messages.

```ts
const myTurn = wake.defineEvent("game.turn", {
  description: "It's your move at a table",
  args: z.object({ table: z.string() }),
  payload: z.object({ hole: z.array(z.string()), toCall: z.number() }),
  text: (t) => `Your move: ${t.toCall} to call`,
  personal: true,
})

await myTurn.emit({ table: "juniper" }, { hole: ["As", "Kd"], toCall: 2 }, { to: "player-abc123" })
```

- `to` is required for a personal event, and refused for any other. A user, the `user` that `attach`'s `identify` gave for them, reaches every subscription they have to the event with these arguments. To reach several people at once, or one conversation, see [emit to many](#emit-to-many).
- **Take `to` from your own records** of who owns what (the seating, the membership list), never from the subscriber list or from anything the caller sent.
- Nothing in the arguments names the subscriber, so nobody can subscribe to someone else's: Wakeshift takes who's subscribing from their own sign-in.

## Subscribe from your own tools

A tool like "join a table" or "watch this repo" subscribes the conversation that called it, in the same step. Register it through the attachment, name the events it subscribes to, and call `call.subscribe` inside:

```ts
attached.registerTool(
  "join",
  { description: "Take a seat at a table.", inputSchema: z.object({ table: z.string() }), subscribes: [turn] },
  async ({ table }, ctx, call) => {
    const sub = await call.subscribe(turn, { table }, { reason: `Playing at ${table}` })
    if (sub.state === "created" || sub.state === "renewed") await seatPlayer(sub.user, table, sub.id)
    return `You're seated at ${table}.`
  },
)
```

- **`subscribes`** lists the events the tool subscribes to (Python `@attached.tool(subscribes=[…])`, Go `wakeshift.Subscribes(…)`). `call.subscribe` takes only those. `tools/list` adds two sentences to the tool's description, from each event's own description and with your app named as the console has it: "This tool is how Shipyard notifies you in this conversation of this event: A build finished: it passed, failed or was canceled. Shipyard delivers its notifications through Wakeshift; the result may ask you to start a background listener and to offer the user a one-time install. Follow it." That's 289 characters: 206 of fixed text, your app's name twice, and the events' descriptions (" of this event: …", or " of these events: …; …" for several), so most tools get about 270 to 320. Claude Code keeps only the first 2048 characters of a description, so budget for the addition: keep a subscribing tool's own description under about 1700 characters, or the end of Wakeshift's text is cut off (the SDK logs `description_too_long`). An agent reads Wakeshift's part of a result as part of your tool only when the tool's description says so; without it, agents often set that part aside as something the user didn't ask for, and aren't woken. Your description stays as you wrote it, without trailing whitespace; the sentence follows a space, or a blank line after a description of several lines. A one-line description that doesn't end its last sentence (with `.`, `!`, `?` or `…`, before any closing quote or parenthesis) gets a period first, so the sentence doesn't run on.
- **Your input schema stays yours.** `tools/list` adds Wakeshift's arguments (`session_key`, which tells Wakeshift which conversation is calling, and `harness` and `conversation_id`, which tell it which agent program), and takes them out of each call before your handler runs.
- **`call.subscribe(event, args, { reason?, ttl?, label?, replay?, once?, context? })`** subscribes the calling conversation, or renews its subscription with those arguments. `once: true` ends it after its first delivery, for "tell me when this finishes". `reason` is shown in the console. More options are on [advanced](/docs/advanced#more-of-callsubscribe). `context` is a value of yours for the event's `onSubscribe` (the caller's credential, say), never sent to Wakeshift ([below](#wait-on-work-you-can-only-poll)). A personal event needs `call.user`.
- **Your tool decides who may subscribe**: `authorize` isn't consulted here. When the caller names a resource (a table, a repo), check they may see it first. When your tool minted the ID, there's nothing to check.
- **Subscribe before you start the work.** An occurrence emitted before the subscription exists reaches nobody, so a fast job can finish unseen. Subscribe first, then start the job; or pass `replay` ([subscribe before you start the work](/docs/low-level#subscribe-before-you-start-the-work)).
- **The reply.** When the handler returns, Wakeshift adds what the agent needs to be woken to whatever it returned ([exactly what](#what-wakeshift-adds-to-a-result)). One addition per call, however many subscribes the call made. You write nothing for it. A call that subscribes and then unsubscribes the same subscription before returning gets no addition: nothing of it is left to wake for.
- **`call.unsubscribe(event, args)`** ends the calling conversation's subscription with those arguments, for a tool like "leave". It resolves to whether one ended.

Await each `call.subscribe` and `call.unsubscribe` before the handler returns. A `call` used after that throws `call_ended`. Tools registered some other way use `wrap` and `wake.currentCall()`: [tools registered elsewhere](/docs/low-level).

`call.subscribe` resolves to a subscription with a `state`:

| `state` | What it means |
| --- | --- |
| `created`, `renewed` | The conversation is subscribed, newly or again. `id` and `user` say which subscription, and whose: store both to [emit to this conversation](#emit-to-many) later, or to end it with `event.end(user, id)`. |
| `native` | The caller subscribes itself ([below](#clients-that-subscribe-natively)): nothing was made, and your tool's reply tells it what to subscribe to. |
| `unavailable` | Nothing happened, and `reason` says why: `client_unsupported`, `no_user` or `refused` ([below](#clients-that-subscribe-natively)). |
| `disabled` | Wakeshift is off: there's no key. |

### When the call fails

If the tool call fails, the subscriptions it made end, and the agent isn't told about them: it never learned of them. A call fails when the handler throws, returns `isError: true` or output that doesn't match the tool's `outputSchema`, or the request is canceled. A result shaped `{ "error": … }` that isn't `isError` counts as success. A renewal isn't undone.

### When Wakeshift fails

A Wakeshift failure never changes your tool's result. If Wakeshift's addition can't be had once your handler returns (an error, a timeout, Wakeshift unreachable, a refused key), the agent gets your result exactly as you returned it, never an error and never a request to call again: your tool may have started paid or visible work, and a repeated call would repeat it. What the call subscribed ends, and the SDK logs `block_failed`. `call.subscribe` itself throws when Wakeshift can't subscribe: catch it if your tool should go on without wakes. Wakeshift's part of a call is bounded: a subscribe gives up after 5 seconds and the addition after 1, neither is retried, and after 5 failures in a row tool calls skip Wakeshift for 30 seconds ([inside a tool call](/docs/sdk#inside-a-tool-call)). Whatever your handler returns or throws reaches the agent unchanged.

### Tools that wait, return a task, or tell the agent to poll

- **A tool that waits for the work and only sometimes returns a job ID** (it waits up to a limit, then returns the result, or the ID if the work is still running) subscribes only on the path that returns the ID: when the wait runs out, `call.subscribe` with `once` and `replay` (`"10m"`), so an occurrence emitted between the end of the wait and the subscribe still arrives. A call that returns the finished result subscribes nothing, and its reply goes back with nothing added. If you'd rather subscribe before the wait, as for any job, `call.unsubscribe` when the work finishes within the call: a call that ends everything it subscribed gets no addition.
- **A tool that returns an MCP task** (`execution.taskSupport`, answered with a task the client follows through `tasks/get`) doesn't subscribe: the client already follows the task to its result, and a subscription would tell it twice. Subscribe in tools whose reply is a job ID the agent would otherwise have to check on.
- **Your descriptions and replies that tell the agent to poll** ("call `get_crawl_status` every 30 seconds until it's done") contradict Wakeshift's sentence on a tool that subscribes. Keep the status tool, which agents still use to check on demand, and reword the hint: "Returns a job ID; you're notified when the crawl finishes. `get_crawl_status` returns its progress meanwhile." In the reply, keep a polling hint unless `call.wakes` is true.

### Skip your own wait only when `call.wakes`

A tool that polls or waits for its work without Wakeshift (it holds the call until the job finishes, or tells the agent to check back) can drop that wait once Wakeshift wakes the conversation instead. Drop it only when `call.wakes` (Python `call.wakes`, Go `call.Wakes()`) is true after `call.subscribe`; otherwise behave exactly as without Wakeshift. Your own wait is then the fallback when Wakeshift is down.

`call.wakes` reflects Wakeshift's decision for this conversation. It's true only once a wake is confirmed: Wakeshift has a working way to wake the idle agent that needs nothing installed, no further call to your tools and no choice from the user still to come:

- Claude Code's Monitor once the user has picked it, or a background command the agent starts in the same turn as your result tells it;
- hooks or a connector already installed, Wakeshift's plugin for Claude Code included;
- a ChatGPT Work chat on the web, which subscribes itself with MCP Events, so its subscription comes back `native`.

It's false, and your tool behaves as without Wakeshift:

- while the user's choice of how to be notified is pending: the first time Claude Code watches something, it asks the user, and nothing runs until they answer. A later call returns true once their pick runs, and stays false if they chose not to be notified;
- on a client with no key;
- when any `call.subscribe` of the call threw, even after one that succeeded: an outage, a timeout, or tool calls skipping Wakeshift after repeated failures;
- when the subscription came back `unavailable`;
- when the agent's program can't be woken now: ChatGPT's desktop app, which waits in the foreground; ChatGPT reached through a tool; a Cursor cloud agent; a retired program or one Wakeshift doesn't know; one whose connector or hooks aren't installed yet;
- once the call unsubscribed what it subscribed.

```ts
try {
  await call.subscribe(crawlFinished, { job: id })
} catch {
  // go on without wakes
}
if (call.wakes) return `Started crawl ${id}.`
return await waitForCrawl(id) // as without Wakeshift
```

Don't promise the wake in your own text: Wakeshift's addition tells the agent. Keep the subscription when `call.wakes` is false. Your result carries what the agent needs to be woken, such as Claude Code's question to the user, and that only goes out with a subscription. Unsubscribe it, and the agent is never asked, so `call.wakes` never turns true.

If the addition to your result fails after your handler returns, which is rare and bounded at 1 second, the subscription is rolled back even though your tool skipped its wait. Keep your status tool for that case.

Whether the agent asked for a wait (an argument such as `wait: true`) stays your tool's decision: honor it as your tool would without Wakeshift, except for a wait too long for one tool call (next). If your schema gives that argument a default, your handler may not be able to tell an explicit value equal to the default from the default itself. Leave the argument without a default if the difference matters to you.

### A wait longer than the agent's program allows

Agents often ask for a longer wait (`wait_seconds: 300`) than their program lets one tool call run. Claude Code gives up on a remote server's call after 60 seconds, so a 300-second wait is cut off, the agent never sees its result, and it falls back to polling. When Wakeshift wakes the conversation, return at once instead:

- `call.callLimit` (Python `call.call_limit`, Go `call.CallLimit()`) is how many seconds the agent's program lets one tool call take: 60 for Claude Code, 300 for Codex. It's set by `call.subscribe`, and undefined (`None`, `0`) when the program's limit isn't known.
- `call.shouldSkipWait(seconds)` (Python `call.should_skip_wait(seconds)`, Go `call.ShouldSkipWait(d)`) is true only when `call.wakes` is true and the wait reaches `call.callLimit`. Otherwise the wait runs, as without Wakeshift.

```ts
try {
  await call.subscribe(crawlFinished, { job: id })
} catch {
  // go on without wakes
}
if (call.shouldSkipWait(waitSeconds)) return `Started crawl ${id}.`
return await waitForCrawl(id, waitSeconds) // as without Wakeshift
```

Don't promise the wake in your own text: Wakeshift's addition tells the agent. Keep the subscription either way. The limits are each program's defaults: a user who raised theirs (Claude Code's `MCP_TOOL_TIMEOUT`, Codex's `tool_timeout_sec`) still gets the early return, and the wake brings the result.

### Retry your backend's hook

When the work finishes, your backend calls the route that emits (a webhook handler, a worker). If Wakeshift is down then, the emit fails, and the route should answer `502` so the sender retries ([in a webhook receiver](/docs/errors#in-a-webhook-receiver)). Retry with backoff for up to an hour, and give each emit the sender's delivery ID or your own ID for the change as its `id`: a retry then wakes once. If your own code calls that route, retry it the same way.

### Clients that subscribe natively

A client that speaks MCP Events, such as ChatGPT, subscribes itself, with its own webhook, by polling, or by push. For such a caller `call.native` is `true`, and `call.subscribe` makes nothing itself: the reply tells the client which event and arguments to subscribe to (`state: "native"`). For an event only your tools may subscribe to (the default), the reply also hands it a one-time subscribe voucher for exactly that event and those arguments, signed in or not; with sign-in, only the same user can use it ([how a voucher works](/docs/who-may-subscribe#chatgpt-and-other-clients-that-subscribe-natively)). A `native` subscription's `user` is `""` for an anonymous caller. Otherwise `call.subscribe` returns `unavailable` and the client is told nothing:

- `client_unsupported`: only your tools may subscribe to the event, and Wakeshift couldn't hand this client a voucher for it.
- `no_user`: the event is `personal`, `authorize` or `public`, and the caller has no signed-in user.
- `refused`: the event's `authorize` refused this caller.

The SDK logs `native_unavailable` once per tool and reason. [Who may subscribe](/docs/who-may-subscribe#chatgpt-and-other-clients-that-subscribe-natively) shows which events each client gets. `call.unsubscribe` ends that user's own subscriptions to the event with those arguments. Your tool is the same for every client.

### What Wakeshift adds to a result

The addition is text for the agent, an optional last line for Wakeshift's hook on the user's computer, and `_meta` entries for the agent's program. Where it goes depends on what your handler returned:

| Your handler returned | The result |
| --- | --- |
| A string | One text item: your text, a blank line, Wakeshift's text, then, where there is one, a blank line and the hook's line |
| A `CallToolResult` | Your content unchanged and in order, then Wakeshift's text as one more text item, then the hook's line as another |
| Either | `_meta` gains Wakeshift's entries beside yours (`wakeshift/connector`, the beacon Codex's connector reads, among them); your own keys stay |
| An object in `structuredContent` | It gains a `wakeshift` field: a string, Wakeshift's text without the hook's line. `tools/list` adds `wakeshift: { "type": "string" }`, optional, to the tool's output schema |

A line at the end of your own text that looks like the hook's (a message you relay, say) is dropped: only Wakeshift's own may end a result's text. A call that names no conversation and subscribed nothing gets no addition at all. If your tests or clients check results against a strict contract (an exact content length, `additionalProperties: false` on your output), allow for the extra text item and the `wakeshift` field, or test with the key unset.

### Structured output

Some hosts show the model only a tool's `structuredContent` when the tool has an `outputSchema`. So the structured result of a tool that subscribes gets a `wakeshift` field with what Wakeshift adds for the agent, and `tools/list` adds that field to the tool's output schema. The addition stays in the content too. You declare nothing: write your output schema without it. An output schema that already names `wakeshift` is refused at registration.

## Emit to many

Your app keeps its own membership: who is in a channel, who is seated at a table. Each member's conversation subscribes once, from your tool, and you store the subscription. To send an occurrence, emit it to the subscriptions of the members who should hear it, in one request:

```ts
const channelNote = wake.defineEvent("chat.notification", {
  description: "A message for you in a channel you joined",
  args: z.object({}),
  payload: z.object({ channel: z.string(), from: z.string(), text: z.string() }),
  text: (m) => `${m.from} in #${m.channel}: ${m.text}`,
  personal: true,
  deleteOnDelivery: true,
})

// Your join tool: remember which conversation joined.
attached.registerTool(
  "join_channel",
  { description: "Join a channel.", inputSchema: z.object({ channel: z.string() }), subscribes: [channelNote] },
  async ({ channel }, ctx, call) => {
    const sub = await call.subscribe(channelNote, {}, { reason: `Member of #${channel}` })
    if (sub.state === "native") await db.addMember(channel, { native: sub.user })
    else if (sub.state === "created" || sub.state === "renewed") await db.addMember(channel, { user: sub.user, subscription: sub.id })
    return `You joined #${channel}.`
  },
)

// When someone posts: emit to the channel's members.
const members = await db.members(channel)
const { ended } = await channelNote.emit({}, { channel, from, text }, { to: members, id: messageId })
await db.removeMembers(channel, ended)
```

`to` takes one recipient or a list of up to 1,000, mixed:

| Recipient | Reaches |
| --- | --- |
| `"u1"` | Every subscription that user has to the event with these arguments. |
| `{ user: "u1", subscription: id }` | Exactly that subscription, if it's that user's: the `id` and `user` that `call.subscribe` returned. |
| `{ native: "u1" }` | Only that user's native (MCP Events) subscriptions to the event with these arguments. |

- One emit is one delivery to each conversation it reaches, even when `to` names that conversation more than once. Delivery is at least once: a redelivery is marked `"redelivered": true`, with the same `id`.
- `matched` counts the subscriptions reached. `ended` lists the subscription recipients that are no longer live: forget them. `unmatched` lists the users and native recipients nothing reached, whether `to` names one or many. A retry of the same emit with its `id` returns the first emit's lists.
- An app's emits to lists may name 10,000 recipients a minute in all; past that, `emit` throws `rate_limited`. A single user isn't counted.
- When a member leaves, end their subscription with `call.unsubscribe` from their own tool call, or with `event.end(user, id)` (TypeScript and Python; the Go SDK doesn't have it yet) when someone else removes them, which tells their conversation it ended ([when a subscription ends](#when-a-subscription-ends)).

## Who is subscribed

```ts
const { subscriptions, connected, via } = await message.status({ channel: "eng" })
const [one] = await notification.statusOf([{ user: member.user, subscription: member.subscription }])
```

- `event.status(args, { to? })` counts the conversations subscribed with those arguments (for a personal event, those of the one user `to` names), how many are connected now, and how they're reached: `via` counts them as `stream` (woken as soon as an occurrence comes), `poll` (woken when the agent's program next asks for its occurrences), `hooks` (woken through what the user installed for their agent program) and `webhook` (a client that subscribed itself, with MCP Events).
- `event.statusOf(subs)` reads up to 1,000 subscriptions by `user` and `subscription`. Each comes back with `live` (it hasn't ended, and it's that user's subscription to this event), `connected` and `via`.
- Both only read: they deliver and end nothing.

## Emit from a webhook

When a service that can only send webhooks should emit one of your events, give it an event's publish URL. Each POST to it emits the event with the arguments you made the URL for:

```ts
const { id, url } = await message.createPublishUrl({ channel: "eng" })
```

- The body is the payload: a JSON object that matches the event's `payload` schema. Anything else gets `400 bad_payload`, and nothing is emitted.
- Your `text` function doesn't run, since your code isn't involved. The agent reads the event's default text, its description with the URL's arguments (`A message in a channel (channel eng)`), and the payload as the event's data: `{"from":"ci","text":"deployed"}`.
- For a personal event, pass `{ to }`: the URL emits to that one user.
- `wake.revokePublishUrl(id)` turns it off; it answers `410` from then on, and also once your app stops offering the event.
- Each call makes a new URL, and the URL is a credential: [use them safely](/docs/publish-urls#use-them-safely).

When the sender's body doesn't match your payload, receive the webhook in your own handler and call `emit` from there. A publish URL takes the body as the payload, whole, and can't pick or rename fields, and it checks only [Standard Webhooks](/docs/publish-urls#signed-webhooks) signatures. So use your own route when the sender posts its full result (every page of a crawl, say: too big for a notification, and more than an agent should get unasked), or signs some other way, such as a hex HMAC of the body in a header of its own. The route verifies the signature, keeps the few fields an agent needs, and emits those, with an `id` that makes the sender's retries emit once:

```ts
import { createHmac, timingSafeEqual } from "node:crypto"

// The crawler's webhook: the whole result, signed with a hex HMAC-SHA256 of the body.
export async function crawlWebhook(req: Request): Promise<Response> {
  const body = await req.text()
  const expected = createHmac("sha256", process.env.CRAWLER_WEBHOOK_SECRET!).update(body).digest()
  const given = Buffer.from(req.headers.get("x-signature") ?? "", "hex")
  if (given.length !== expected.length || !timingSafeEqual(given, expected)) return new Response(null, { status: 401 })
  const result = JSON.parse(body) // { id, status, pages: [ every page ] }
  await crawlFinished.emit({ job: result.id }, { status: result.status, pages: result.pages.length }, { id: `crawl-${result.id}` })
  return new Response(null, { status: 204 })
}
```

The agent fetches the rest with your tools. Event and workflow tools fit the same way: Composio's triggers deliver an app's events to a webhook URL you control, so call `emit` from that handler, and in Inngest or Trigger.dev, call `emit` from the last step of the job a tool started.

## Wait on work you can only poll

When the service behind your tool has no webhook you can use, only a status call, `event.watch` checks it for you and emits once when it's done. Give it the event's arguments and a check: the check returns the payload when the work is done, or nothing to keep waiting. Start it where a subscription starts, in the event's `onSubscribe`, which runs for every new subscription however it was made (your tool, `subscribe_events`, or a client that subscribes natively):

```ts tab:TypeScript
const crawlFinished = wake.defineEvent("crawl.finished", {
  description: "A crawl finished",
  args: z.object({ job: z.string() }),
  payload: z.object({ status: z.string(), pages: z.number() }),
  onSubscribe: ({ args }) => watchCrawl(args.job), // every new subscription, however it was made
})

function watchCrawl(job: string) {
  crawlFinished.watch(
    { job },
    async ({ signal }) => {
      const s = await api.status(job, { signal }) // your API's status call
      return s.state === "running" ? undefined : { status: s.state, pages: s.pages } // undefined: keep waiting
    },
    { every: "15s", timeout: "2h" },
  )
}
```

```python tab:Python
from wakeshift import Subscribed


def on_crawl_subscribed(s: Subscribed[Job]) -> None:
    """Every new subscription, however it was made."""
    watch_crawl(s.args.job)


crawl_finished = wake.define_event(
    "crawl.finished", description="A crawl finished", args=Job, payload=Crawl, on_subscribe=on_crawl_subscribed
)


def watch_crawl(job: str) -> None:
    async def check() -> Crawl | None:
        s = await api.crawl_status(job)  # your API's status call
        return None if s.state == "running" else Crawl(status=s.state, pages=s.pages)  # None: keep waiting

    crawl_finished.watch({"job": job}, check, every="15s", timeout="2h")
```

```go tab:Go
type jobArgs struct {
	Job string `json:"job"`
}
type crawl struct {
	Status string `json:"status"`
	Pages  int    `json:"pages"`
}

// crawlStatus is your API's status call: the payload, and whether the crawl is done.
crawlStatus := func(ctx context.Context, job string) (crawl, bool, error) { return crawl{Status: "completed"}, true, nil }

wake, err := wakeshift.New()
if err != nil {
	log.Fatal(err)
}
var crawlFinished *wakeshift.Event[jobArgs, crawl]
crawlFinished, err = wakeshift.DefineEvent(wake, "crawl.finished", wakeshift.EventDef[jobArgs, crawl]{
	Description: "A crawl finished",
	// Every new subscription, however it was made. ctx outlives the request.
	OnSubscribe: func(ctx context.Context, s wakeshift.Subscribed[jobArgs]) error {
		go crawlFinished.Watch(ctx, s.Args, func(ctx context.Context) (crawl, bool, error) {
			return crawlStatus(ctx, s.Args.Job)
		}, wakeshift.Every(15*time.Second), wakeshift.Timeout(2*time.Hour))
		return nil
	},
})
if err != nil {
	log.Fatal(err)
}
```

How a watch runs:

- **The check** runs at once, then every `every` (default 30 seconds, at least 1) while it returns nothing. After a check that throws, the next waits longer, doubling up to 5 minutes. TypeScript's check gets `{ signal, attempt }`, and the signal aborts when the watch stops; Python's is a function with no arguments, sync or async, canceled with the watch's task; Go's gets a `ctx` that ends with the watch.
- **It stops** when the check returns a payload, which it emits once; at `timeout` (default 1 hour), which covers the checks and the emit together; after `maxErrors` errors in a row (Python `max_errors`, Go `wakeshift.MaxErrors(n)`, default 5); at once on an error your `isFinal(err)` marks final (Python `is_final`, Go `wakeshift.FinalIf`), such as "no such job"; or when you stop it.
- **The emit** is retried while retrying can help (no answer, a timeout, a 429 or a 5xx), within the timeout, and every attempt sends the same `id`: yours, or one the SDK makes for the watch. An emit error a retry wouldn't fix stops the watch. If a webhook can also report the same work, emit from both with the same `id`: whichever comes second is a duplicate, and the watch ends as emitted. The watch takes its `id` when it starts, before any check, so it can't come from what the check finds: use the job's own ID (``id: `crawl-${job}` ``), and have the webhook emit with that one too. If the same arguments can finish more than once (a rebuild under the same build ID), one fixed `id` would make the later finishes duplicates: make the two sources exclusive instead, watching only work the webhook doesn't report. Pass `to` for a personal event, as with `emit`.
- **One watch per event and arguments.** Watching the same event with the same arguments while a watch runs returns the running one, so starting it from both your tool and `onSubscribe` runs one check loop. It keeps the check it started with: a later subscriber's `onSubscribe` that watches the same arguments gets the running watch, whose check still uses the first subscriber's `context` (their credential, say), not its own.
- **At most 10 checks run at once** per client; the others wait their turn.
- **It knows when there's nothing to wake.** On a client without a key it does nothing, and never calls the check. Inside a tool call whose subscription to those arguments came back `unavailable`, it doesn't start; if the call then fails and its subscriptions end, a watch it started for them stops.
- **What it returns.** TypeScript's `watch` returns `{ done, stop() }`; `done` resolves, never rejects, to `{ outcome }`: `emitted` (with `emitted`, `null` when another emit with the same `id` got there first), `timeout`, `failed` (with `reason`: `check`, `errors` or `emit`, and the `error`), `stopped` or `skipped`. `await wake.close()` stops every watch. Python's returns the `asyncio.Task` running it, whose result is a `WatchResult(outcome, emitted, reason, error)`; cancel the task to stop it, and `await wake.aclose()` cancels them all. It needs a running event loop. Go's `Watch` blocks until it's done, so run it in a goroutine with a context that outlives the request (`context.WithoutCancel(ctx)`, or `OnSubscribe`'s): it returns nil once it emitted (or had nothing to do), the context's error when stopped, an error matching `context.DeadlineExceeded` at its timeout, and otherwise the check's or the emit's error.
- **Logs.** A watch that stops without emitting logs `watch_failed` (a warning), with the reason and the error's type, never its message; one stopped by an error your `isFinal` marks final logs `watch_stopped` at info, since it stopped as asked; one that times out logs `watch_timeout`, once per event.

**A watch lives in your process.** A restart, a deploy or a serverless function that freezes between requests loses it, and nothing resumes it. Use it on a long-running server. On Cloudflare Workers, run the check from a Durable Object alarm, which survives between requests (`waitUntil` keeps a Worker alive only about 30 seconds; [the recipe](/docs/sdk#cloudflare-workers)); on Vercel or Lambda, from a queue or a cron job that calls `emit` itself. Where the service has a webhook, a [publish URL](#emit-from-a-webhook) or your own receiver needs no process at all.

**On a stdio server, the process is the agent's.** The agent starts your server when it opens it and ends it when it closes it, so a watch, or any background check of your own, ends with the agent's session: a job that runs for hours may finish after nothing is left to emit, and that wake is lost. For long jobs, emit from somewhere that outlives the agent: the service's webhook through a publish URL, or a hosted server of yours.

**`onSubscribe`** (Python `on_subscribe`, Go `OnSubscribe`) gets `{ args, user, via, tool, context }` (Python `Subscribed`, Go `wakeshift.Subscribed[A]`): the subscription's arguments, its user if there is one, how it was made (`tool`, `subscribe_events` or `native`) and, for `tool`, the tool's name. It runs once for each new subscription, never for a renewal, in the background: the request doesn't wait for it. For a tool, it runs after the tool's call succeeded, so a call that fails and ends its subscriptions never fires it. For a client that subscribes natively, it runs when the client's own `events/subscribe` or first `events/poll` makes the subscription. If it throws, the subscription stands, and the SDK logs `on_subscribe_failed`. A client without a key never calls it. `context` is the value the subscribing call passed to `call.subscribe(event, args, { context })` (Python `context=`, Go `wakeshift.Context(v)`): your own, such as the caller's credential for your API, never sent to Wakeshift or logged. It arrives for a subscription your tool made, and for a voucher this process handed out and the client redeemed (kept in memory until the voucher expires or is used). For a tool, `onSubscribe` also runs inside the call's own context: TypeScript's async context (your `AsyncLocalStorage` values), Python's `contextvars`, Go's `ctx` values from `Subscribe` (without its cancellation). Start a watch that needs the caller's credentials from `onSubscribe` with `context`. If `context` is missing (another instance redeemed the voucher, or the subscription came from `subscribe_events` or a native client), the watch can't run with the caller's credentials: say so in a log, or fall back to your status tool. A watch already running for the same event and arguments keeps its first subscriber's `context`; a later subscriber's doesn't reach it.

`args` are the arguments the subscription was made with, so an [encoded value](#argument-values) arrives encoded. `decodeArgValue` (Python `decode_arg_value`, Go `wakeshift.DecodeArgValue`) gives the raw ID back, except for the hashed form (`..h` and 40 hex digits). When your IDs can be long enough to be hashed and `onSubscribe` needs the raw one (to start a watch, say), keep your own map from the encoded value to the raw ID, written where your tool encodes it, and look it up in `onSubscribe`. Put the raw ID in the payload too, so agents get it with each occurrence.

## When a subscription ends

| Ended by | What happens |
| --- | --- |
| The agent calls `unsubscribe_events` with an event, your tool calls `call.unsubscribe`, or a native client unsubscribes | It's the conversation's own request: it ends at once, occurrences still waiting for it are dropped, and the agent learns it from the tool's reply. |
| Your code calls `event.end`, or the agent calls `unsubscribe_events` with an ID | Occurrences emitted before it ended still arrive, then the conversation is told it ended and why. |
| The tool call that made it fails | It ends at once. The agent never learned of it, so it isn't told. |
| `once`, after its first delivery | It ends with that delivery. |
| No delivery or fresh subscribe for 30 days (or the `ttl` you gave) | It expires, and the conversation is told. |
| A native client stops renewing it | It ends at most a day after the client last renewed it, however many occurrences there are. |
| You [forget the user](/docs/users#forgetting-a-user) | Their subscriptions end and their streams close. |

So "emit the result, then `event.end`" works, even for an agent that's offline. To stop a conversation hearing anything more, unsubscribe it from its own tool call instead.

When a conversation's last subscription ends, it stops being woken, and whatever its agent started to be woken (Claude Code's background monitor, say) stops too, saying why and that there's nothing more to do. Ending one of several subscriptions leaves the others waking it. Subscribing again, even straight after, starts afresh: the reply gives the agent what it needs to be woken again.

## How occurrences reach the agent

Each subscription has one destination, set by how it was made:

- **A native subscription** (ChatGPT, or any client that speaks MCP Events: [which ones](/docs/mcp-events)) delivers to the webhook the client gave: one POST per occurrence, signed with [Standard Webhooks](https://www.standardwebhooks.com/), in order, retried for up to 15 minutes. A client without a public URL polls for occurrences instead, or keeps a request open to your server and gets each one on it as it happens (push). The occurrence carries your `payload` unchanged. Each of your users can have up to 20 native webhook subscriptions and 20 push subscriptions to your app at once; past that, a new one is refused with MCP Events' `ResourceExhausted` error (`InvalidParams` for ChatGPT, which has no such code) until one ends or lapses. A `once` subscription ends with its first delivery, so it stops counting then. Renewing one is never refused.
- **Every other subscription** delivers to the conversation that made it, however that agent is woken (see [agents](/docs/agents)). An agent that can't be woken is told so when it subscribes, and checks with your tools.

An occurrence is marked as information from outside, never as an instruction from the agent's user, and stamped with your app as the sender. If the conversation isn't connected, it waits. Wakeshift paces wake-ups for each agent, and one wake brings the conversation at most 20 occurrences. When more are waiting, the rest come in the next wake, as soon as the agent goes back to waiting; an agent that gets them with its next call to your tools is told how many more are waiting. How quickly each agent wakes, and what its user approves, is on the [agents](/docs/agents) page.

Your users have no Wakeshift account, so there's nothing for them to revoke in Wakeshift. To stop, they ask the agent to call `unsubscribe_events` or your own leave tool. If they remove your MCP server from their agent instead, its subscriptions end when their `ttl` passes without a delivery.

## Testing your integration

Test what reaches the agent, not how Wakeshift words it. Assert that a woken conversation got your event's name, its arguments and your payload, and your `text` where you give one: a Monitor line's `subject` is exactly your `text`, so assert it. Don't assert the wording Wakeshift adds around it, or the instructions it adds to your tool's reply: Wakeshift words those for each agent program, and changes the wording as agents change. [Test your integration](/docs/testing) has the tests. Run your own tests with `WAKESHIFT_KEY` unset too: everything works, and nothing is sent.
