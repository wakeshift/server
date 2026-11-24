# Test your integration

Your tests can play each kind of agent against your own server: Claude Code, woken through a command its Monitor tool runs; Codex, woken through a WebSocket its connector holds; and a client that subscribes to your events itself (MCP Events). Each test connects as that agent, calls your tool that subscribes, emits the event, and checks what arrives. They need a key: create a second app in the [console]({{APP}}) for tests, and set its key as `WAKESHIFT_KEY` where they run.

## Unit tests

Without `WAKESHIFT_KEY` the client is [disabled](/docs/sdk#without-a-key): your server runs as it would without Wakeshift, nothing is sent, and `call.subscribe` answers `state: "disabled"`. So your tools' own logic tests as it does today, with no core and nothing to fake.

**Keep the client off in tests when `WAKESHIFT_KEY` is set** in your shell or CI. A client made with `new Wakeshift()` reads the variable, so either run the tests without it (`WAKESHIFT_KEY= npm test`), or make the client with an explicit empty key, which never reads the environment: `new Wakeshift({ key: "" })` in TypeScript, `Wakeshift("")` in Python, `wakeshift.New(wakeshift.WithKey(""))` in Go.

**A snapshot of `tools/list` or of your capabilities changes when the key is set.** With a key, Wakeshift adds its arguments, the sentence on each tool that subscribes, the `wakeshift` field in their output schemas, its built-in tools and, when you define any event, the MCP Events capability (in your `initialize` answer too, whatever protocol version the client asks for); the first `tools/list` also makes one request to Wakeshift, for your app's name. Take those snapshots with the key unset, or with the test client below, which answers that request itself.

### A tool's call, without a core

Each SDK has a test module: `@wakeshift/sdk/testing`, `wakeshift.testing` and `github.com/wakeshift/wakeshift-go/wakeshifttest` ([the Go tests](#go) use it too). Its stand-in call is typed as the real one, records what your tool subscribes to, and answers each subscribe with a full `Subscription` (`created`, then `renewed` for the same event and arguments), so handlers that read `sub.state` or `sub.id` run as they do in production:

```ts tab:TypeScript
import { test } from "node:test"
import assert from "node:assert/strict"
import { createToolCall } from "@wakeshift/sdk/testing"

test("watch_crawl subscribes to its job", async () => {
  const call = createToolCall({ tool: "watch_crawl", user: "ada" })
  await watchCrawl({ job: "j1" }, call.ctx, call) // your handler, as you registered it
  assert.deepEqual(call.subscriptions.map((s) => [s.event, s.args]), [["crawl.finished", { job: "j1" }]])
})
```

```python tab:Python
from wakeshift.testing import tool_call


async def test_watch_crawl_subscribes():
    call = tool_call(tool="watch_crawl", user="ada")
    await watch_crawl("j1", call)  # your tool function, as you registered it
    assert [(s.event, s.args) for s in call.subscriptions] == [("crawl.finished", {"job": "j1"})]
```

`createToolCall({ tool, user, native, ctx, events })` (Python `tool_call(tool=, user=, session=, native=, context=, events=)`, Go `wakeshifttest.ToolCallOptions`) sets what the handler reads: `native: true` answers `state: "native"`, as for ChatGPT, and `events` makes `subscribe` refuse any other event with `undeclared_subscribe`, as your tool's `subscribes` does. `call.wakes` is true once a subscribe holds a subscription. Test your fallback branch (your own polling or waiting, as without Wakeshift) with `createToolCall({ wakes: false })` (Python `tool_call(wakes=False)`, Go `ToolCallOptions{NoWakes: true}`; for the test client's attached server, Go `wakeshifttest.NoWakes()`). `call.callLimit` is undefined unless you give one: `createToolCall({ callLimit: 60 })` (Python `tool_call(call_limit=60)`, Go `ToolCallOptions{CallLimit: time.Minute}`) tests `call.shouldSkipWait` as for Claude Code.

More of the stand-in call, the same in every SDK:

- **Make a subscribe fail.** `call.failNext("subscribe", "unreachable")` (Python `call.fail_next(...)`, Go `rec.FailNext(...)`) makes the next `subscribe` (or `"unsubscribe"`) throw as it would in production: `"unreachable"` is no answer (`APIConnectionError`, code `unreachable`), `"refused"` is Wakeshift refusing the key (`unauthenticated`). The test client takes the same: `wake.failNext(op, error)` (Python `wake.fail_next`, Go `wake.FailNext`).
- **Run `onSubscribe`.** A real call runs the event's `onSubscribe` after it succeeds, with the `context` your tool passed to `call.subscribe`. The stand-in does when you `await call.finish()` (Go `rec.Finish(ctx)`), which waits for it; `withCall` (Python `async with with_call(...)`) runs it when your function returns without throwing. Python's plain `with with_call(...)` can't wait, so it doesn't run `on_subscribe` (or the watches it starts): it warns (`RuntimeWarning`) when the call subscribed to an event that has one. Use `async with` there. Each subscribe the stand-in records carries the `context` it was given (TypeScript in its `options`, Python `context`, Go `Context`).
- **ChatGPT and vouchers.** `createToolCall({ native: true, client: wake })` (Python `tool_call(native=True, wake=wake)`, Go `ToolCallOptions{Native: true, Client: wake}`) with a test client hands out a voucher for each subscribe to a tools-only event, listed in `call.vouchers` (Go `rec.Vouchers()`). `await wake.redeem(voucher, webhookUrl)` (Go `wake.Redeem(ctx, voucher, url)`) subscribes with it as ChatGPT would, running `onSubscribe` with the call's `context`; using it again with the same webhook renews it, and with another is refused (`bad_voucher`), as Wakeshift does. A tool you call through the test client's attached server hands out the same vouchers, one line each in the text Wakeshift adds to the result, in a fixed form you can match: `To be woken by <event> with <arguments as compact JSON>, subscribe to it (MCP Events) with wakeshift_voucher set to wsv_… in its arguments.`

A tool registered elsewhere finds its call with `wake.currentCall()`. Run its handler inside `withCall`, and that's the call it finds (Go: `wakeshifttest.ContextWithCall(ctx, call)`, after which `wakeshift.ToolCallFrom(ctx)` returns `call`):

```ts tab:TypeScript
import assert from "node:assert/strict"
import { createToolCall, withCall } from "@wakeshift/sdk/testing"

const call = createToolCall({ tool: "start_crawl" })
await withCall(wake, call, () => wrappedHandler()) // your tool's handler, as your tools/call handler runs it
assert.equal(call.subscriptions[0].event, "crawl.finished")
```

```python tab:Python
from wakeshift.testing import tool_call, with_call

async def test_start_crawl_subscribes():
    call = tool_call(tool="start_crawl")
    async with with_call(wake, call):  # async with: it runs on_subscribe after the block, as a real call does
        await call_tool(ctx, params)  # your own tools/call handler
    assert call.subscriptions[0].event == "crawl.finished"
```

`authorize` is your own function, so test it directly: define it as a named function and call it with the subscriber and arguments you want to check. In Python, `event.authorize` is the function you gave `define_event` (or `None`), so `await event.authorize(subscriber, args)` works on the defined event too (await it only if it's async).

### A test client, enabled and offline

To test your server with Wakeshift on (what `tools/list` lists, a call's result, what you emit) without a key or a network, make your client with the test module's test client. It's enabled, answers Wakeshift's requests itself, never reads `WAKESHIFT_KEY` and never sends anything, and records every subscribe and emit (Go: `wakeshifttest.NewClient()`, which embeds an enabled `*wakeshift.Client`, with `Emits()`, `Subscriptions()` and `Unsubscribes()`):

```ts tab:TypeScript
import assert from "node:assert/strict"
import { createTestClient } from "@wakeshift/sdk/testing"

const testWake = createTestClient({ app: "Crawler" }) // a Wakeshift, enabled and offline
const finished = testWake.defineEvent("crawl.finished", {
  description: "A crawl finished",
  args: z.object({ job: z.string() }),
  payload: z.object({ pages: z.number() }),
})
await finished.emit({ job: "j1" }, { pages: 3 })
assert.deepEqual(testWake.emits.map((e) => [e.event, e.args, e.payload]), [["crawl.finished", { job: "j1" }, { pages: 3 }]])
```

```python tab:Python
from wakeshift.testing import TestClient


async def test_emit_is_recorded():
    wake = TestClient(app="Crawler")  # a Wakeshift, enabled and offline
    finished = wake.define_event("crawl.finished", description="A crawl finished", args=Job, payload=Crawl)
    await finished.emit({"job": "j1"}, {"pages": 3})
    assert [(e.event, e.args, e.payload) for e in wake.emits] == [("crawl.finished", {"job": "j1"}, {"pages": 3})]
```

It records `emits`, `subscriptions` (each with the event, arguments, tool, user and options), `unsubscribes` and publish URLs; `clear()` (Go `Clear()`) empties them. Attach to it as you would in production: `tools/list`, a tool call with its block, the built-in tools, `emit`, `createPublishUrl` and `status` all answer. Have the code that builds your server take the client as an argument, so tests can pass this one; a client made at module level with `new Wakeshift()` stays the production one.

A [`watch`](/docs/events#wait-on-work-you-can-only-poll) on the test client runs its first check at once, so a check that's already done emits straight away: `await wake.close()` (Python `await wake.aclose()`, Go `wake.Close()`) waits for checks in flight and their emits, then stops the watches, so `emits` shows it after that.

## The examples

Each test below runs your server in process, with a tool `watch_crawl` that subscribes the calling conversation to `crawl.finished` for a job, and the event's text `Crawl <job> finished: <pages> pages`:

```ts server.ts
import { McpServer } from "@modelcontextprotocol/server"
import { z } from "zod"
import { Wakeshift } from "@wakeshift/sdk"

export const wake = new Wakeshift()
export const crawlFinished = wake.defineEvent("crawl.finished", {
  description: "A crawl finished",
  args: z.object({ job: z.string() }),
  payload: z.object({ pages: z.number() }),
  text: (p, a) => `Crawl ${a.job} finished: ${p.pages} pages`,
})

export function makeServer() {
  const server = new McpServer({ name: "crawler", version: "1.0.0" })
  const attached = wake.attach(server)
  attached.registerTool(
    "watch_crawl",
    { description: "Watch a crawl.", inputSchema: z.object({ job: z.string() }), subscribes: [crawlFinished] },
    async ({ job }, _ctx, call) => {
      await call.subscribe(crawlFinished, { job })
      return `Watching crawl ${job}.`
    },
  )
  return server
}
```

The TypeScript examples use the MCP TypeScript SDK 2.x, as the [quickstart](/docs/quickstart) does, and its client: `npm install --save-dev @modelcontextprotocol/client` (the server packages, `@modelcontextprotocol/server` and `@modelcontextprotocol/node`, you have already). On 1.x, take `Client` from `@modelcontextprotocol/sdk/client/index.js`, `InMemoryTransport` from `@modelcontextprotocol/sdk/inMemory.js` and `StdioClientTransport` from `@modelcontextprotocol/sdk/client/stdio.js`, and serve each HTTP request with 1.x's `StreamableHTTPServerTransport` without sessions ([on 1.x](/docs/sdk#using-modelcontextprotocolsdk-1x)). They run commands in a process group, so they need macOS or Linux.

**Use fresh IDs in each run.** The examples' job IDs (`j1`, `j2`…) are short to read; yours should be new each run (`` `j1-${randomUUID()}` ``). Your tests' app keeps what an earlier run left: a subscription that hasn't reached its `ttl`, or a `once` one that was never delivered, still matches the same arguments, and an emit with `replay` can deliver an earlier run's occurrence. With IDs reused, a test can pass on a stale wake, or lose its own delivery to a leftover subscription.

## Claude Code

Claude Code is recognized by its client name, `claude-code`: from `initialize` on a server with sessions and over stdio, as in these in-memory examples. A stateless server (one built for each request, as clients on protocol 2026-07-28 connect) never sees `initialize`, so there Wakeshift recognizes Claude Code by the client info in each request's `_meta` on 2026-07-28, and on older revisions by its HTTP `User-Agent`, which starts with `claude-code/`: Claude Code sends `claude-code/<version> (external, cli)`. A test of a stateless server sends the same, for example `User-Agent: claude-code/2.1.0 (external, cli)`.

The tool's result asks Claude Code to start its Monitor tool with a command: the command is in the result's text, after `with this command: ` and up to ` .`, both in the first result and in a later one that asks it to start the Monitor again. When the Monitor is already running, the result says so ("Your Wakeshift Monitor is already running") and still carries the command, for the agent to start only if the Monitor has ended since: a test that matches the command alone can't tell the two results apart, so check for "already running" first. Run the command as Monitor does: each line it prints is one JSON message.

```ts wakes.test.ts
import { test } from "node:test"
import assert from "node:assert/strict"
import { spawn } from "node:child_process"
import { createInterface } from "node:readline"
import { Client, InMemoryTransport } from "@modelcontextprotocol/client"
import { makeServer, crawlFinished } from "./server.js"

async function connectAs(name: string) {
  const [serverSide, clientSide] = InMemoryTransport.createLinkedPair()
  await makeServer().connect(serverSide)
  const client = new Client({ name, version: "1.0.0" })
  await client.connect(clientSide)
  return client
}
const text = (result: any) => result.content.map((c: any) => c.text ?? "").join("\n")

test("Claude Code is woken through its Monitor", async () => {
  const client = await connectAs("claude-code")
  const result = await client.callTool({ name: "watch_crawl", arguments: { job: "j1" } })
  // The result asks Claude Code to run a command in its Monitor tool.
  const command = text(result).match(/with this command: (u='.*?; done) \./s)?.[1]
  assert.ok(command, text(result))
  // Run it as Monitor does: each line it prints is one JSON message.
  const monitor = spawn("sh", ["-c", command], { detached: true, stdio: ["ignore", "pipe", "ignore"] })
  const lines = createInterface({ input: monitor.stdout })
  try {
    await crawlFinished.emit({ job: "j1" }, { pages: 12 })
    for await (const line of lines) {
      const message = JSON.parse(line)
      if (message.type !== "event") continue
      assert.equal(message.subject, "Crawl j1 finished: 12 pages")
      assert.deepEqual(message.data, { pages: 12 })
      break
    }
  } finally {
    process.kill(-monitor.pid!, "SIGKILL") // the command and its curl
    await client.close()
  }
})
```

A line is `{"type":"event", "topic", "from", "app", "trust", "subject", "data", "id", …}`: `subject` is the event's text (for a [publish URL](/docs/publish-urls)'s post, the event's default text), and `data` its payload. `redelivered: true` means it may have come before. Other lines (`"type":"hub.restart"`, `"hub.closed"`) are notices from Wakeshift.

`subject` is your own `text`, so assert it, as the test does. What not to assert is Wakeshift's wording: the instructions it adds to your tool's result, and how it frames a message for each agent program, change as agents change.

**The Monitor line's budget.** A line is at most 480 bytes, since Claude Code's Monitor shows only the start of a long line. When the event doesn't fit, `truncated` is `true` and `data` is left out; Wakeshift puts the payload's JSON in `data_preview` instead and cuts it to fit, each cut field ending in "…": first `data_preview`, down to its first 40 bytes, then `subject`, then the rest of `data_preview`. So a line with a long `subject` still starts its payload, with a shorter `subject`; only a line whose fixed fields leave no room for both loses `data_preview` (a field cut below about 8 bytes is left out). A preview is the start of the JSON, not valid JSON: a test that reads the payload from a Monitor line should handle `truncated` lines, or keep the event small enough not to get one. The fixed fields (`type`, `topic`, `from`, `app`, `trust` and `id`) take about 210 bytes when an argument is a UUID, which leaves about 250 bytes for `subject` and `data` together. So for Claude Code, that budget binds long before the 1,000-byte limit on `text`: keep the text to one short line and the payload to IDs and a status, and let your tools return the rest. Shorter argument values leave more room.

The result may also offer the person a one-time install that wakes Claude Code without the Monitor, as [agents](/docs/agents) describes. Which install it offers depends on the Wakeshift it comes from, so a test checks for the Monitor command, not for the offer's wording.

### More than one call in a conversation

A real conversation calls your tools more than once, and passes the `session_key` an earlier result gave it, so Wakeshift knows the calls come from the same conversation. The result gives it as `session_key="…"`. A later result may ask Claude Code to start the Monitor again, with a command; started again, it can print an event the first one already printed. Dedupe by the line's `id`, as Claude Code does:

```ts wakes.test.ts
test("two calls in one conversation, deduped by event id", async () => {
  const client = await connectAs("claude-code")
  const first = text(await client.callTool({ name: "watch_crawl", arguments: { job: "j4" } }))
  const sessionKey = first.match(/session_key="([^"]+)"/)?.[1]
  assert.ok(sessionKey, first)
  const second = text(await client.callTool({ name: "watch_crawl", arguments: { job: "j5", session_key: sessionKey } }))

  // Run every Monitor command the results gave, as Claude Code would.
  const commands = [first, second].map((t) => t.match(/with this command: (u='.*?; done) \./s)?.[1]).filter((c): c is string => !!c)
  const monitors = commands.map((c) => spawn("sh", ["-c", c], { detached: true, stdio: ["ignore", "pipe", "ignore"] }))
  const seen = new Map<string, string>() // event id -> subject
  const both = new Promise<void>((resolve) => {
    for (const m of monitors) {
      createInterface({ input: m.stdout! }).on("line", (line) => {
        const message = JSON.parse(line)
        if (message.type === "event" && !seen.has(message.id)) seen.set(message.id, message.subject)
        if (seen.size === 2) resolve()
      })
    }
  })
  try {
    await crawlFinished.emit({ job: "j4" }, { pages: 1 })
    await crawlFinished.emit({ job: "j5" }, { pages: 2 })
    await both
    assert.deepEqual([...seen.values()].sort(), ["Crawl j4 finished: 1 pages", "Crawl j5 finished: 2 pages"])
  } finally {
    for (const m of monitors) process.kill(-m.pid!, "SIGKILL")
    await client.close()
  }
})
```

### A watch

A tool whose event a [`watch`](/docs/events#wait-on-work-you-can-only-poll) emits is tested the same way, with your API's status call answered by the test. The event starts its watch in `onSubscribe`, and the test says when the crawl is done:

```ts server.ts
// A crawl your API reports only by status: the test sets what the status call returns.
export const crawlStatus = new Map<string, number>() // job -> pages, once done
export const crawlPolled = wake.defineEvent("crawl.polled", {
  description: "A crawl finished",
  args: z.object({ job: z.string() }),
  payload: z.object({ pages: z.number() }),
  text: (p, a) => `Crawl ${a.job} finished: ${p.pages} pages`,
  onSubscribe: ({ args }) => watchPolled(args.job),
})

function watchPolled(job: string) {
  crawlPolled.watch(
    { job },
    async () => {
      const pages = crawlStatus.get(job) // your API's status call
      return pages === undefined ? undefined : { pages } // undefined: keep waiting
    },
    { every: "1s", timeout: "2m" },
  )
}

export function makeWatchServer() {
  const server = new McpServer({ name: "crawler", version: "1.0.0" })
  wake.attach(server).registerTool(
    "watch_polled_crawl",
    { description: "Watch a crawl.", inputSchema: z.object({ job: z.string() }), subscribes: [crawlPolled] },
    async ({ job }, _ctx, call) => {
      await call.subscribe(crawlPolled, { job }, { once: true })
      return `Watching crawl ${job}.`
    },
  )
  return server
}
```

```ts wakes.test.ts
import { randomUUID } from "node:crypto"
import { crawlStatus, makeWatchServer, wake } from "./server.js"

test("a watch wakes Claude Code once the status says done", async () => {
  const [serverSide, clientSide] = InMemoryTransport.createLinkedPair()
  await makeWatchServer().connect(serverSide)
  const client = new Client({ name: "claude-code", version: "1.0.0" })
  await client.connect(clientSide)
  const job = `j-${randomUUID()}` // fresh each run
  const result = await client.callTool({ name: "watch_polled_crawl", arguments: { job } })
  const command = text(result).match(/with this command: (u='.*?; done) \./s)?.[1]
  assert.ok(command, text(result))
  const monitor = spawn("sh", ["-c", command], { detached: true, stdio: ["ignore", "pipe", "ignore"] })
  try {
    const event = new Promise<any>((resolve) =>
      createInterface({ input: monitor.stdout! }).on("line", (line) => {
        const message = JSON.parse(line)
        if (message.type === "event") resolve(message)
      }),
    )
    crawlStatus.set(job, 5) // the crawl is done: the watch's next check emits
    assert.equal((await event).subject, `Crawl ${job} finished: 5 pages`)
  } finally {
    process.kill(-monitor.pid!, "SIGKILL")
    await client.close()
    await wake.close() // stops the watches still running
  }
})
```

### Over HTTP, as the quickstart serves it

The in-memory examples connect with `initialize`. The [quickstart](/docs/quickstart)'s server is stateless instead: `createMcpHandler` builds one from `makeServer` for each request, and never sees `initialize`. Test it the same way your users reach it: serve `makeServer` over HTTP on a free port, and call it as Claude Code does, with its name (which goes in each request's `_meta` on protocol 2026-07-28) and its `User-Agent` (which older revisions are recognized by):

```ts http.test.ts
import { test } from "node:test"
import assert from "node:assert/strict"
import { once } from "node:events"
import { createServer } from "node:http"
import type { AddressInfo } from "node:net"
import { createMcpHandler } from "@modelcontextprotocol/server"
import { toNodeHandler } from "@modelcontextprotocol/node"
import { Client, StreamableHTTPClientTransport } from "@modelcontextprotocol/client"
import { makeServer } from "./server.js"

const text = (result: any) => result.content.map((c: any) => c.text ?? "").join("\n")

test("Claude Code over HTTP, on a stateless server", async () => {
  const http = createServer(toNodeHandler(createMcpHandler(makeServer))).listen(0, "127.0.0.1")
  await once(http, "listening")
  const url = new URL(`http://127.0.0.1:${(http.address() as AddressInfo).port}/mcp`)
  const client = new Client({ name: "claude-code", version: "2.1.0" })
  const transport = new StreamableHTTPClientTransport(url, {
    requestInit: { headers: { "User-Agent": "claude-code/2.1.0 (external, cli)" } },
  })
  await client.connect(transport)
  try {
    const result = await client.callTool({ name: "watch_crawl", arguments: { job: "j7" } })
    assert.match(text(result), /with this command: u='/)
  } finally {
    await client.close()
    http.close()
  }
})
```

From here, run the Monitor command and emit, as the in-memory test does.

### Over stdio

A server that users run as a command (`npx your-server`, `uvx your-server`) is best tested the same way: start it as a child process, as Claude Code does, and talk to it over its stdin and stdout. That also covers a server that starts when its file runs, with nothing to import, and it fails if anything writes to stdout besides the JSON-RPC:

```ts stdio.test.ts
import { test } from "node:test"
import assert from "node:assert/strict"
import { Client } from "@modelcontextprotocol/client"
import { StdioClientTransport } from "@modelcontextprotocol/client/stdio"

const text = (result: any) => result.content.map((c: any) => c.text ?? "").join("\n")

test("over stdio, started as Claude Code starts it", async () => {
  const transport = new StdioClientTransport({
    command: "node",
    args: ["dist/stdio.js"], // your server's entry point, as users run it
    env: { ...process.env } as Record<string, string>, // WAKESHIFT_KEY included
    stderr: "inherit",
  })
  const client = new Client({ name: "claude-code", version: "1.0.0" })
  await client.connect(transport)
  try {
    const result = await client.callTool({ name: "watch_crawl", arguments: { job: "j6" } })
    assert.match(text(result), /with this command: u='/)
  } finally {
    await client.close()
  }
})
```

Then run the command and emit from the test process, as in the first test: the same key and event definition reach the same subscription. `StdioClientTransport` passes only a few environment variables unless you give `env`, so pass the key through. In Python, `Client(StdioServerParameters(command=sys.executable, args=["server.py"], env=dict(os.environ)), client_info=Implementation(name="claude-code", version="1.0.0"))` does the same.

## Codex

Codex is recognized by its client name, `codex-mcp-client`, and each call carries its thread in `_meta`, under `x-codex-turn-metadata`. The result's `_meta["wakeshift/connector"]` is a beacon for Wakeshift's connector for Codex: a signed token whose claims give a URL to redeem it at, and a ticket. Redeem it with `{ticket, thread}` for the thread's stream link, open a WebSocket on it, and acknowledge each event.

```ts wakes.test.ts
test("Codex is woken through its connector", async () => {
  const client = await connectAs("codex-mcp-client")
  const thread = "thread-test-1"
  const result = await client.callTool({
    name: "watch_crawl",
    arguments: { job: "j2" },
    _meta: { "x-codex-turn-metadata": { thread_id: thread } }, // the Codex thread
  })
  // The connector's beacon: a signed token whose claims say where to redeem it.
  const beacon = result._meta?.["wakeshift/connector"] as string
  const { redeem, ticket } = JSON.parse(Buffer.from(beacon.split(".")[1], "base64url").toString())
  const redeemed = await fetch(redeem, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ ticket, thread }) })
  const { url } = (await redeemed.json()) as { url: string }
  const socket = new WebSocket(url)
  await new Promise((resolve, reject) => ((socket.onopen = resolve), (socket.onerror = reject)))
  try {
    const frame = new Promise<any>((resolve) => {
      socket.onmessage = (m) => {
        const f = JSON.parse(String(m.data))
        if (f.type !== "event") return
        socket.send(JSON.stringify({ type: "ack", delivery_id: f.delivery_id }))
        resolve(f)
      }
    })
    await crawlFinished.emit({ job: "j2" }, { pages: 3 })
    const f = await frame
    assert.equal(f.event.subject, "Crawl j2 finished: 3 pages")
    assert.deepEqual(f.event.data, { pages: 3 })
  } finally {
    socket.close()
    await client.close()
  }
})
```

A WebSocket frame isn't a Monitor line: it's `{"type":"event", "delivery_id", "attempt", "subscription_id", "topic", "app", "event": {"id", "subject", "data", "from": {"kind", "id"}, "trust", …}}`, with the whole payload in `event.data` and nothing cut. Acknowledge it with `{"type":"ack","delivery_id":…}`, or it comes again (`attempt` over 1). A redeem without `ticket` or `thread` is refused with `bad_request`; a ticket is good once.

**The WebSocket client.** The stream link accepts a client that sends no `Origin` header, or one naming the link's own host. Node's global `WebSocket` and the `ws` package, Python's `websockets`, and Go's `github.com/coder/websocket` and gorilla's dialer send none by default. Go's `golang.org/x/net/websocket` always sends one, so give it the link's own origin (`https://` and its host) or use another client: a mismatched `Origin` is refused with a 403, which x/net reports only as "bad status", in an error whose text includes the link. The link is a credential, so don't log it, or errors that carry it.

## MCP Events clients

A client that speaks MCP Events (protocol 2026-07-28) subscribes to your events itself. It sends every request with the headers `MCP-Protocol-Version: 2026-07-28` and `Mcp-Method` (the request's method), plus `Mcp-Name` (its `params.name`) on `tools/call` and `events/*`, and, in `params._meta`, its protocol version, client info and capabilities. Send the same headers in every raw 2026-07-28 request your tests make: FastMCP 4 and go-sdk refuse a request without `Mcp-Method` (FastMCP answers `-32020`). A subscription a client makes on its own, to an event that's `personal`, `authorize` or `public`, belongs to a signed-in person, so your server's `identify` must give a user: without one, `events/poll` and `events/subscribe` are refused with MCP Events' forbidden error, "this app couldn't tell who you are". The example reads a test header in `identify`; yours reads your own sign-in. The forbidden error is `-32012` for ChatGPT (client name or User-Agent `openai-mcp`) and `-32024` for every other client ([every code](/docs/errors#what-mcp-events-clients-get)).

| Method | Params | Result |
| --- | --- | --- |
| `events/list` | none | `events`: the events open to native clients (personal, `authorize` or public) and, while Wakeshift offers vouchers, your tools-only events, each with `name`, `description`, `delivery`, `inputSchema` and `payloadSchema` |
| `events/poll` | `name`, `arguments`, `cursor` (`null` the first time), `maxAgeMs?`, `maxEvents?` | `events` (each with `eventId`, `name`, `timestamp`, `data` and `_meta["ai.wakeshift/text"]`), `cursor`, `truncated`, `hasMore`, `nextPollMs` |
| `events/subscribe` | `name`, `arguments`, `delivery` (`{mode: "webhook", url, secret}`), `cursor?`, `ttlMs?`, `maxAgeMs?` | `id`, `refreshBefore`, `cursor`, `truncated` |
| `events/unsubscribe` | `name`, `arguments`, `delivery` | |
| `events/stream` (push, where your server offers it) | `name`, `arguments`, `cursor?`, `maxAgeMs?`, `heartbeatMs?` | Notifications on the open request: `notifications/events/active` (`cursor`, `truncated`, `heartbeatMs`), then `notifications/events/event` per occurrence and `notifications/events/heartbeat` while quiet; `notifications/events/terminated` when it's refused or ended; then the final result |

`events/subscribe`'s `delivery.secret` is a Standard Webhooks secret, `whsec_` and the base64 of 24 to 64 bytes, and before delivering anything Wakeshift POSTs a signed `{"type": "verification", "challenge": …}` to the URL, which must answer with a 2xx whose JSON body echoes it, `{"challenge": …}` ([`callback_failed`](/docs/errors#callback_failed)). Wakeshift delivers only to a public HTTPS URL: it refuses `http`, private, loopback and link-local addresses, and redirects, so a test webhook needs a tunnel to the port it listens on. Polling needs none, which is why the example polls.

A tools-only event is listed only to MCP Events clients, with a required `wakeshift_voucher` argument. Its `delivery` is `webhook`, plus `push` where your server offers it, since a voucher works only with `events/subscribe` and a webhook, or `events/stream`: `events/poll` of a tools-only event is always refused, voucher or not (`-32024` here, "can't be polled: it's subscribed to only through this app's tools, with a webhook or by push"), and so is `events/subscribe` without a voucher ("can be subscribed only through this app's tools, or with the voucher one of them handed you"). That refusal is the check that only your tools subscribe to it. [A ChatGPT-shaped client](#a-chatgpt-shaped-client) tests the voucher. Run the server on a stateless transport, as 2026-07-28 clients connect, and poll until the event arrives:

```ts native.test.ts
import { test } from "node:test"
import assert from "node:assert/strict"
import { createServer } from "node:http"
import { createMcpHandler } from "@modelcontextprotocol/server"
import { toNodeHandler } from "@modelcontextprotocol/node"

const META = {
  "io.modelcontextprotocol/protocolVersion": "2026-07-28",
  "io.modelcontextprotocol/clientInfo": { name: "native-test", version: "1.0.0" },
  "io.modelcontextprotocol/clientCapabilities": { extensions: { "io.modelcontextprotocol/events": {} } },
}

test("a native client polls an open event, and is refused a tools-only one", async (t) => {
  // A server for each request, as the quickstart serves it; identify reads the test's X-User header.
  const handler = createMcpHandler(() => makeNativeServer({ identify: (ctx) => ({ user: ctx.http?.req?.headers.get("x-user") ?? undefined }) }))
  const http = createServer(toNodeHandler(handler))
  await new Promise<void>((resolve) => http.listen(0, "127.0.0.1", resolve))
  t.after(() => (http.closeAllConnections(), http.close()))
  const url = `http://127.0.0.1:${(http.address() as { port: number }).port}/mcp`
  const rpc = async (method: string, params: Record<string, unknown> = {}) => {
    const res = await fetch(url, {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        Accept: "application/json, text/event-stream",
        "MCP-Protocol-Version": "2026-07-28",
        "Mcp-Method": method,
        ...(typeof params.name === "string" ? { "Mcp-Name": params.name } : {}),
        "X-User": "ada",
      },
      body: JSON.stringify({ jsonrpc: "2.0", id: 1, method, params: { ...params, _meta: META } }),
    })
    const body = await res.text()
    return JSON.parse(body.startsWith("{") ? body : body.split("\n").find((l) => l.startsWith("data: "))!.slice(6))
  }

  const poll = { name: "site.updated", arguments: { site: "example.com" } } // a public event
  let { cursor } = (await rpc("events/poll", { ...poll, cursor: null })).result
  await siteUpdated.emit({ site: "example.com" }, { pages: 7 })
  let events: any[] = []
  while (events.length === 0) {
    const r = (await rpc("events/poll", { ...poll, cursor })).result
    ;({ events, cursor } = r)
    if (events.length === 0) await new Promise((resolve) => setTimeout(resolve, 500))
  }
  assert.deepEqual(events[0].data, { pages: 7 })

  const refused = await rpc("events/poll", { name: "crawl.finished", arguments: { job: "j1" }, cursor: null })
  assert.equal(refused.error.code, -32024)
})
```

`makeNativeServer` and `siteUpdated` stand for your own: a server like `makeServer` with `identify`, and an event that's open to native clients. `_meta["ai.wakeshift/text"]` is the line the agent reads, inside Wakeshift's framing of a message from outside.

**Don't reuse this `_meta` for a Claude Code test.** It declares the MCP Events extension in its capabilities, and a client that declares it subscribes natively, whatever its name: a call with `clientInfo.name` `claude-code` and these capabilities gets no Monitor command, and `call.subscribe` answers `native` or `unavailable`. A raw 2026-07-28 request that plays Claude Code sends its client info without the extension:

```ts
const CLAUDE_CODE_META = {
  "io.modelcontextprotocol/protocolVersion": "2026-07-28",
  "io.modelcontextprotocol/clientInfo": { name: "claude-code", version: "2.1.0" },
  "io.modelcontextprotocol/clientCapabilities": {},
}
```

### MCP Events over stdio

A stdio server answers MCP Events on protocol 2026-07-28 too, in TypeScript and Python (Go serves them only over HTTP). There are no headers on stdio: each request is one JSON line on the server's stdin, with the same `_meta`, and each answer one line on its stdout. A client on stdio has no webhook a public address reaches, so it polls:

```ts stdio.test.ts
import { spawn } from "node:child_process"
import { createInterface } from "node:readline"

test("an MCP Events client over stdio lists and polls", async () => {
  const child = spawn("node", ["dist/stdio.js"], { env: process.env, stdio: ["pipe", "pipe", "inherit"] })
  const answers = new Map<number, (message: any) => void>()
  createInterface({ input: child.stdout! }).on("line", (line) => {
    const message = JSON.parse(line)
    answers.get(message.id)?.(message)
  })
  let id = 0
  const rpc = (method: string, params: Record<string, unknown> = {}) =>
    new Promise<any>((resolve) => {
      answers.set(++id, resolve)
      const _meta = {
        "io.modelcontextprotocol/protocolVersion": "2026-07-28",
        "io.modelcontextprotocol/clientInfo": { name: "native-test", version: "1.0.0" },
        "io.modelcontextprotocol/clientCapabilities": { extensions: { "io.modelcontextprotocol/events": {} } },
      }
      child.stdin!.write(JSON.stringify({ jsonrpc: "2.0", id, method, params: { ...params, _meta } }) + "\n")
    })
  try {
    const listed = await rpc("events/list")
    assert.ok(listed.result.events.length > 0, JSON.stringify(listed))
    const first = await rpc("events/poll", { name: "site.updated", arguments: { site: "example.com" }, cursor: null })
    assert.ok(first.result.cursor, JSON.stringify(first))
  } finally {
    child.kill()
  }
})
```

A native subscription needs a user from `identify` on stdio too, where there's no HTTP request to read: return the user your server already knows from its own configuration (the API key in its environment, say). This test's server does, and `site.updated` is open to native clients; without a user, `events/poll` gets the forbidden error. Poll again with the `cursor` after emitting, as [above](#mcp-events-clients).

### A ChatGPT-shaped client

When your tool subscribes ChatGPT to a tools-only event, the tool's result hands it a one-time voucher, and ChatGPT subscribes with it ([how a voucher works](/docs/who-may-subscribe#chatgpt-and-other-clients-that-subscribe-natively)). A test plays ChatGPT: a 2026-07-28 client named `openai-mcp`, with no sign-in. It calls `watch_crawl`, reads the voucher from the result, subscribes with it and a webhook, and receives the emit there.

The voucher is in the text Wakeshift adds to the result, `wsv_` and 32 characters (`/wsv_[a-z2-7]{32}/`), and in `structuredContent.wakeshift` too when your tool returns structured content; that field holds the same text, so match it there the same way. The webhook needs a public HTTPS URL, as every webhook Wakeshift delivers to does: point a tunnel at the port the test listens on and set its URL as `WEBHOOK_URL`. Wakeshift's verification challenge reaches the webhook before `events/subscribe` answers, so it has to be listening first.

```ts chatgpt.test.ts
import { test } from "node:test"
import assert from "node:assert/strict"
import { randomBytes } from "node:crypto"
import { createServer } from "node:http"
import { createMcpHandler } from "@modelcontextprotocol/server"
import { toNodeHandler } from "@modelcontextprotocol/node"
import { crawlFinished, makeServer } from "./server.js"

const WEBHOOK_URL = process.env.WEBHOOK_URL // a public HTTPS address that forwards to port 8788 here

// ChatGPT's MCP client is named openai-mcp.
const CHATGPT_META = {
  "io.modelcontextprotocol/protocolVersion": "2026-07-28",
  "io.modelcontextprotocol/clientInfo": { name: "openai-mcp", version: "1.0.0" },
  "io.modelcontextprotocol/clientCapabilities": {},
}

test("ChatGPT subscribes with the voucher its tool call handed it", { skip: !WEBHOOK_URL && "set WEBHOOK_URL" }, async (t) => {
  // The MCP server, built for each request as the quickstart serves it, with no sign-in.
  const http = createServer(toNodeHandler(createMcpHandler(() => makeServer())))
  await new Promise<void>((resolve) => http.listen(0, "127.0.0.1", resolve))
  t.after(() => (http.closeAllConnections(), http.close()))
  const url = `http://127.0.0.1:${(http.address() as { port: number }).port}/mcp`
  const rpc = async (method: string, params: Record<string, unknown> = {}) => {
    const res = await fetch(url, {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        Accept: "application/json, text/event-stream",
        "MCP-Protocol-Version": "2026-07-28",
        "Mcp-Method": method,
        ...(typeof params.name === "string" ? { "Mcp-Name": params.name } : {}),
      },
      body: JSON.stringify({ jsonrpc: "2.0", id: 1, method, params: { ...params, _meta: CHATGPT_META } }),
    })
    const body = await res.text()
    return JSON.parse(body.startsWith("{") ? body : body.split("\n").find((l) => l.startsWith("data: "))!.slice(6))
  }

  // ChatGPT's webhook: answers the verification challenge, then takes occurrences.
  let delivered!: (occurrence: any) => void
  const occurrence = new Promise<any>((resolve) => (delivered = resolve))
  const hook = createServer(async (req, res) => {
    let raw = ""
    for await (const chunk of req) raw += chunk
    const body = JSON.parse(raw)
    res.setHeader("Content-Type", "application/json")
    if (body.type === "verification") return void res.end(JSON.stringify({ challenge: body.challenge }))
    res.end("{}")
    delivered(body)
  })
  await new Promise<void>((resolve) => hook.listen(8788, resolve))
  t.after(() => (hook.closeAllConnections(), hook.close()))

  // 1. ChatGPT calls the tool.
  const called = await rpc("tools/call", { name: "watch_crawl", arguments: { job: "j9" } })
  // 2. The voucher is in the result's text.
  const text = called.result.content.map((c: { text?: string }) => c.text ?? "").join("\n")
  const voucher = text.match(/wsv_[a-z2-7]{32}/)?.[0]
  assert.ok(voucher, text)

  // 3. It subscribes with the voucher, as the event argument wakeshift_voucher, and its webhook.
  const delivery = { mode: "webhook", url: WEBHOOK_URL, secret: "whsec_" + randomBytes(32).toString("base64") }
  const subscribed = await rpc("events/subscribe", { name: "crawl.finished", arguments: { job: "j9", wakeshift_voucher: voucher }, delivery })
  assert.ok(subscribed.result?.id, JSON.stringify(subscribed))

  // 4. The emit arrives at the webhook.
  await crawlFinished.emit({ job: "j9" }, { pages: 4 })
  const timeout = new Promise<never>((_, reject) => setTimeout(() => reject(new Error("no delivery in 30s")), 30_000).unref())
  const got = await Promise.race([occurrence, timeout])
  assert.equal(got.name, "crawl.finished")
  assert.deepEqual(got.data, { pages: 4 })

  // Without a voucher, ChatGPT is refused with its forbidden error, -32012.
  const refused = await rpc("events/subscribe", { name: "crawl.finished", arguments: { job: "j10" }, delivery })
  assert.equal(refused.error.code, -32012)
})
```

Each occurrence is one POST with `eventId`, `name`, `timestamp`, `data` (your payload) and `_meta["ai.wakeshift/text"]`, signed with [Standard Webhooks](https://www.standardwebhooks.com/) with the delivery's secret. A test can skip checking the signature; a real client's webhook checks it.

The voucher's subscription keeps what `watch_crawl`'s `call.subscribe` asked for (`once`, `replay` and `ttl`), and starts with the latest occurrence since the tool handed the voucher out, so an emit right after the tool call isn't missed. Subscribing again with the same voucher and webhook URL renews that subscription rather than making another ([vouchers](/docs/who-may-subscribe#chatgpt-and-other-clients-that-subscribe-natively)). A signed-in user can have at most 20 live webhook subscriptions to your app; a `once` one stops counting when it delivers, so a suite that subscribes with `once` and delivers doesn't pile up. Subscriptions without `once` count until they end or lapse: end them in the test (`events/unsubscribe`), or give them a short `ttl`.

## Python

The same tests with pytest, `pytest-asyncio` (`asyncio_mode = auto`) and `websockets`, on the `mcp` SDK's in-process client:

```python
# server.py
from mcp.server.mcpserver import MCPServer
from pydantic import BaseModel

from wakeshift import ToolCall, Wakeshift

wake = Wakeshift()  # reads WAKESHIFT_KEY


class Job(BaseModel):
    job: str


class Crawl(BaseModel):
    pages: int


crawl_finished = wake.define_event(
    "crawl.finished",
    description="A crawl finished",
    args=Job,
    payload=Crawl,
    text=lambda p, a: f"Crawl {a.job} finished: {p.pages} pages",
)


def make_server(identify=None) -> MCPServer:
    server = MCPServer("crawler")
    attached = wake.attach(server, identify=identify)

    @attached.tool(subscribes=[crawl_finished])
    async def watch_crawl(job: str, call: ToolCall) -> str:
        """Watch a crawl."""
        await call.subscribe(crawl_finished, {"job": job})
        return f"Watching crawl {job}."

    return server
```

```python
# test_wakes.py
import asyncio
import base64
import contextlib
import json
import os
import re
import signal

import httpx2
import websockets
from mcp import Client
from mcp.types import Implementation

from server import crawl_finished, make_server


def connect_as(name: str) -> Client:
    return Client(make_server(), client_info=Implementation(name=name, version="1.0.0"))


def text(result) -> str:
    return "\n".join(getattr(c, "text", "") for c in result.content)


async def test_claude_code_is_woken_through_its_monitor():
    async with connect_as("claude-code") as client:
        result = await client.call_tool("watch_crawl", {"job": "j1"})
    # The result asks Claude Code to run a command in its Monitor tool.
    match = re.search(r"with this command: (u='.*?; done) \.", text(result), re.S)
    assert match, text(result)
    # Run it as Monitor does: each line it prints is one JSON message.
    monitor = await asyncio.create_subprocess_exec(
        "sh", "-c", match[1], stdout=asyncio.subprocess.PIPE, start_new_session=True
    )
    try:
        await crawl_finished.emit({"job": "j1"}, {"pages": 12})
        async with asyncio.timeout(30):
            while (message := json.loads(await monitor.stdout.readline())).get("type") != "event":
                pass
        assert message["subject"] == "Crawl j1 finished: 12 pages"
        assert message["data"] == {"pages": 12}
    finally:
        # The command and its curl. On macOS, killpg raises PermissionError once the
        # group's processes have exited (a once subscription ends the command itself).
        with contextlib.suppress(ProcessLookupError, PermissionError):
            os.killpg(monitor.pid, signal.SIGKILL)
        await monitor.wait()


async def test_codex_is_woken_through_its_connector():
    thread = "thread-test-1"
    async with connect_as("codex-mcp-client") as client:
        result = await client.call_tool(
            "watch_crawl", {"job": "j2"}, meta={"x-codex-turn-metadata": {"thread_id": thread}}
        )
    # The connector's beacon: a signed token whose claims say where to redeem it.
    beacon = result.meta["wakeshift/connector"]
    claims = json.loads(base64.urlsafe_b64decode(beacon.split(".")[1] + "=="))
    async with httpx2.AsyncClient() as http:
        redeemed = await http.post(claims["redeem"], json={"ticket": claims["ticket"], "thread": thread})
    async with websockets.connect(redeemed.json()["url"]) as socket:
        await crawl_finished.emit({"job": "j2"}, {"pages": 3})
        async with asyncio.timeout(30):
            while (frame := json.loads(await socket.recv())).get("type") != "event":
                pass
        await socket.send(json.dumps({"type": "ack", "delivery_id": frame["delivery_id"]}))
        assert frame["event"]["subject"] == "Crawl j2 finished: 3 pages"
        assert frame["event"]["data"] == {"pages": 3}
```

**Against a server you already run.** `Client` takes a URL too, so a test can call your server over HTTP as Claude Code reaches it, with the server started as you serve it (`uvicorn server:app --port 3000`): `Client("http://127.0.0.1:3000/mcp", client_info=Implementation(name="claude-code", version="1.0.0"))` in place of `connect_as`. Emit from the test process with the same key and event definition, and run the Monitor command as above.

For an MCP Events client, send the same JSON-RPC as above, with the same headers (`Mcp-Method` included), with `httpx2` (it comes with `wakeshift`) through `httpx2.ASGITransport` to `make_server(identify).streamable_http_app(stateless_http=True, json_response=True)`, inside the app's lifespan, with `base_url="http://127.0.0.1:8000"`: the MCP SDK refuses requests whose `Host` it doesn't expect.

[A ChatGPT-shaped client](#a-chatgpt-shaped-client) in Python, with the webhook on Python's own HTTP server and `WEBHOOK_URL` a public HTTPS address that forwards to it:

```python
# test_chatgpt.py
import asyncio
import base64
import json
import os
import queue
import re
import secrets
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import httpx2
import pytest

from server import crawl_finished, make_server

WEBHOOK_URL = os.environ.get("WEBHOOK_URL")  # a public HTTPS address that forwards to port 8788 here
# ChatGPT's MCP client is named openai-mcp.
CHATGPT_META = {
    "io.modelcontextprotocol/protocolVersion": "2026-07-28",
    "io.modelcontextprotocol/clientInfo": {"name": "openai-mcp", "version": "1.0.0"},
    "io.modelcontextprotocol/clientCapabilities": {},
}
occurrences: queue.Queue = queue.Queue()


class Webhook(BaseHTTPRequestHandler):
    """ChatGPT's webhook: answers the verification challenge, then takes occurrences."""

    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        answer = {"challenge": body["challenge"]} if body.get("type") == "verification" else {}
        out = json.dumps(answer).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(out)))
        self.end_headers()
        self.wfile.write(out)
        if not answer:
            occurrences.put(body)


@pytest.mark.skipif(not WEBHOOK_URL, reason="set WEBHOOK_URL")
async def test_chatgpt_subscribes_with_the_voucher():
    hook = ThreadingHTTPServer(("127.0.0.1", 8788), Webhook)
    threading.Thread(target=hook.serve_forever, daemon=True).start()
    app = make_server().streamable_http_app(stateless_http=True, json_response=True)  # no sign-in
    try:
        async with app.router.lifespan_context(app):
            async with httpx2.AsyncClient(
                transport=httpx2.ASGITransport(app=app), base_url="http://127.0.0.1:8000"
            ) as http:

                async def rpc(method: str, params: dict) -> dict:
                    headers = {
                        "Accept": "application/json, text/event-stream",
                        "MCP-Protocol-Version": "2026-07-28",
                        "Mcp-Method": method,
                        "Mcp-Name": params["name"],
                    }
                    body = {"jsonrpc": "2.0", "id": 1, "method": method, "params": {**params, "_meta": CHATGPT_META}}
                    return (await http.post("/mcp", json=body, headers=headers)).json()

                # 1. ChatGPT calls the tool.
                called = await rpc("tools/call", {"name": "watch_crawl", "arguments": {"job": "j9"}})
                # 2. The voucher is in the result's text.
                text = "\n".join(c.get("text", "") for c in called["result"]["content"])
                voucher = re.search(r"wsv_[a-z2-7]{32}", text)
                assert voucher, text

                # 3. It subscribes with the voucher, as the event argument wakeshift_voucher, and its webhook.
                secret = "whsec_" + base64.b64encode(secrets.token_bytes(32)).decode()
                delivery = {"mode": "webhook", "url": WEBHOOK_URL, "secret": secret}
                subscribed = await rpc(
                    "events/subscribe",
                    {"name": "crawl.finished", "arguments": {"job": "j9", "wakeshift_voucher": voucher[0]}, "delivery": delivery},
                )
                assert subscribed.get("result", {}).get("id"), subscribed

                # 4. The emit arrives at the webhook.
                await crawl_finished.emit({"job": "j9"}, {"pages": 4})
                got = await asyncio.to_thread(occurrences.get, timeout=30)
                assert got["name"] == "crawl.finished"
                assert got["data"] == {"pages": 4}

                # Without a voucher, ChatGPT is refused with its forbidden error, -32012.
                refused = await rpc("events/subscribe", {"name": "crawl.finished", "arguments": {"job": "j10"}, "delivery": delivery})
                assert refused["error"]["code"] == -32012
    finally:
        hook.shutdown()
        hook.server_close()
```

### FastMCP

On FastMCP (2.12+, 3 and 4), connect FastMCP's own client to the server in process, with the client info of the agent you play. The rest of each test is the same as above:

```python
# test_fastmcp_wakes.py
import mcp_types  # FastMCP 4; on FastMCP 2 and 3: from mcp import types as mcp_types
from fastmcp import Client

from server import crawl_finished, mcp  # your FastMCP server, attached


def connect_as(name: str) -> Client:
    return Client(mcp, client_info=mcp_types.Implementation(name=name, version="1.0.0"))


async def test_claude_code_on_fastmcp():
    async with connect_as("claude-code") as client:
        result = await client.call_tool("watch_crawl", {"job": "j1"})
    text = "\n".join(getattr(c, "text", "") for c in result.content)
    assert "with this command: u='" in text  # then run it and emit, as above


async def test_codex_on_fastmcp():
    async with connect_as("codex-mcp-client") as client:
        result = await client.call_tool(
            "watch_crawl", {"job": "j2"}, meta={"x-codex-turn-metadata": {"thread_id": "thread-test-1"}}
        )
    assert result.meta["wakeshift/connector"]  # the beacon: redeem it, as above
```

On FastMCP 2 and 3 every client is served through Wakeshift's tools (they have no native MCP Events), so there's no MCP Events test to write there.

## Go

The same tests with `go test`, go-sdk's in-memory transports and `github.com/coder/websocket`:

```go file
// server.go
package crawler

import (
	"context"
	"fmt"
	"log"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/wakeshift/wakeshift-go"
)

type crawlArgs struct {
	Job string `json:"job"`
}

type crawl struct {
	Pages int `json:"pages"`
}

var (
	wake          = must(wakeshift.New()) // reads WAKESHIFT_KEY
	crawlFinished = must(wakeshift.DefineEvent(wake, "crawl.finished", wakeshift.EventDef[crawlArgs, crawl]{
		Description: "A crawl finished",
		Text:        func(p crawl, a crawlArgs) string { return fmt.Sprintf("Crawl %s finished: %d pages", a.Job, p.Pages) },
	}))
)

func newServer() *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{Name: "crawler", Version: "1.0.0"}, nil)
	attached := wake.Attach(srv, wakeshift.AttachOptions{})
	wakeshift.AddTool(attached, &mcp.Tool{Name: "watch_crawl", Description: "Watch a crawl."}, watchCrawl, wakeshift.Subscribes(crawlFinished))
	return srv
}

func watchCrawl(ctx context.Context, call *wakeshift.ToolCall, in crawlArgs) (*mcp.CallToolResult, any, error) {
	if _, err := crawlFinished.Subscribe(ctx, call, crawlArgs{Job: in.Job}); err != nil {
		return nil, nil, err
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "Watching crawl " + in.Job + "."}}}, nil, nil
}

func must[T any](v T, err error) T {
	if err != nil {
		log.Fatal(err)
	}
	return v
}
```

```go file
// wakes_test.go
package crawler

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os/exec"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// connectAs connects a client with the given name to a fresh server, in memory.
func connectAs(t *testing.T, name string) *mcp.ClientSession {
	t.Helper()
	serverSide, clientSide := mcp.NewInMemoryTransports()
	if _, err := newServer().Connect(t.Context(), serverSide, nil); err != nil {
		t.Fatal(err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: name, Version: "1.0.0"}, nil)
	session, err := client.Connect(t.Context(), clientSide, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close() })
	return session
}

func text(res *mcp.CallToolResult) string {
	var parts []string
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			parts = append(parts, tc.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func TestClaudeCodeWokenThroughMonitor(t *testing.T) {
	session := connectAs(t, "claude-code")
	res, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: "watch_crawl", Arguments: map[string]any{"job": "j1"}})
	if err != nil {
		t.Fatal(err)
	}
	// The result asks Claude Code to run a command in its Monitor tool.
	m := regexp.MustCompile(`(?s)with this command: (u='.*?; done) \.`).FindStringSubmatch(text(res))
	if m == nil {
		t.Fatalf("no Monitor command in: %s", text(res))
	}
	// Run it as Monitor does: each line it prints is one JSON message.
	monitor := exec.Command("sh", "-c", m[1])
	monitor.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout, _ := monitor.StdoutPipe()
	if err := monitor.Start(); err != nil {
		t.Fatal(err)
	}
	defer syscall.Kill(-monitor.Process.Pid, syscall.SIGKILL) // the command and its curl

	if _, err := crawlFinished.Emit(t.Context(), crawlArgs{Job: "j1"}, crawl{Pages: 12}); err != nil {
		t.Fatal(err)
	}
	lines := bufio.NewScanner(stdout)
	for lines.Scan() {
		var msg struct {
			Type    string `json:"type"`
			Subject string `json:"subject"`
			Data    crawl  `json:"data"`
		}
		if json.Unmarshal(lines.Bytes(), &msg) != nil || msg.Type != "event" {
			continue
		}
		if msg.Subject != "Crawl j1 finished: 12 pages" || msg.Data.Pages != 12 {
			t.Fatalf("event: %+v", msg)
		}
		return
	}
	t.Fatal("the Monitor command ended without the event")
}

func TestCodexWokenThroughConnector(t *testing.T) {
	session := connectAs(t, "codex-mcp-client")
	const thread = "thread-test-1"
	res, err := session.CallTool(t.Context(), &mcp.CallToolParams{
		Name:      "watch_crawl",
		Arguments: map[string]any{"job": "j2"},
		Meta:      mcp.Meta{"x-codex-turn-metadata": map[string]any{"thread_id": thread}}, // the Codex thread
	})
	if err != nil {
		t.Fatal(err)
	}
	// The connector's beacon: a signed token whose claims say where to redeem it.
	beacon, _ := res.Meta["wakeshift/connector"].(string)
	parts := strings.Split(beacon, ".")
	if len(parts) != 3 {
		t.Fatalf("no beacon in _meta: %v", res.Meta)
	}
	claims, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var c struct{ Redeem, Ticket string }
	if err := json.Unmarshal(claims, &c); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]string{"ticket": c.Ticket, "thread": thread})
	resp, err := http.Post(c.Redeem, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var redeemed struct{ URL string }
	if err := json.NewDecoder(resp.Body).Decode(&redeemed); err != nil || redeemed.URL == "" {
		t.Fatalf("redeem: %d %v", resp.StatusCode, err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, redeemed.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()

	if _, err := crawlFinished.Emit(ctx, crawlArgs{Job: "j2"}, crawl{Pages: 3}); err != nil {
		t.Fatal(err)
	}
	for {
		var frame struct {
			Type       string `json:"type"`
			DeliveryID string `json:"delivery_id"`
			Event      struct {
				Subject string `json:"subject"`
				Data    crawl  `json:"data"`
			} `json:"event"`
		}
		if err := wsjson.Read(ctx, conn, &frame); err != nil {
			t.Fatal(err)
		}
		if frame.Type != "event" {
			continue
		}
		wsjson.Write(ctx, conn, map[string]string{"type": "ack", "delivery_id": frame.DeliveryID})
		if frame.Event.Subject != "Crawl j2 finished: 3 pages" || frame.Event.Data.Pages != 3 {
			t.Fatalf("event: %+v", frame.Event)
		}
		return
	}
}
```

They need Go 1.25 or later, as the SDK does (they use `t.Context()`). For an MCP Events client, send the JSON-RPC above, with the same headers (go-sdk refuses a 2026-07-28 request without `Mcp-Method`), to `attached.Handler(…)` on an `httptest.Server`.

[A ChatGPT-shaped client](#a-chatgpt-shaped-client) in Go, with `WEBHOOK_URL` a public HTTPS address that forwards to port 8788:

```go file
// chatgpt_test.go
package crawler

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/wakeshift/wakeshift-go"
)

// chatGPTMeta is ChatGPT's: its MCP client is named openai-mcp.
var chatGPTMeta = map[string]any{
	"io.modelcontextprotocol/protocolVersion":    "2026-07-28",
	"io.modelcontextprotocol/clientInfo":         map[string]any{"name": "openai-mcp", "version": "1.0.0"},
	"io.modelcontextprotocol/clientCapabilities": map[string]any{},
}

type rpcReply struct {
	Result struct {
		ID      string `json:"id"`
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	} `json:"result"`
	Error *struct {
		Code int `json:"code"`
	} `json:"error"`
}

func TestChatGPTSubscribesWithVoucher(t *testing.T) {
	webhookURL := os.Getenv("WEBHOOK_URL") // a public HTTPS address that forwards to port 8788 here
	if webhookURL == "" {
		t.Skip("set WEBHOOK_URL")
	}
	// The MCP server, built for each request, with no sign-in.
	opts := wakeshift.AttachOptions{}
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return newServer() }, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	srv := httptest.NewServer(wake.Handler(h, opts))
	t.Cleanup(srv.Close)
	rpc := func(method, name string, params map[string]any) rpcReply {
		t.Helper()
		params["name"], params["_meta"] = name, chatGPTMeta
		body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
		req, _ := http.NewRequest("POST", srv.URL, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("MCP-Protocol-Version", "2026-07-28")
		req.Header.Set("Mcp-Method", method)
		req.Header.Set("Mcp-Name", name)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out rpcReply
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
		return out
	}

	// ChatGPT's webhook: answers the verification challenge, then takes occurrences.
	occurrences := make(chan map[string]any, 1)
	ln, err := net.Listen("tcp", "127.0.0.1:8788")
	if err != nil {
		t.Fatal(err)
	}
	hook := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		if body["type"] == "verification" {
			json.NewEncoder(w).Encode(map[string]any{"challenge": body["challenge"]})
			return
		}
		w.Write([]byte("{}"))
		select {
		case occurrences <- body:
		default:
		}
	})}
	go hook.Serve(ln)
	t.Cleanup(func() { hook.Close() })

	// 1. ChatGPT calls the tool.
	called := rpc("tools/call", "watch_crawl", map[string]any{"arguments": map[string]any{"job": "j9"}})
	// 2. The voucher is in the result's text.
	var parts []string
	for _, c := range called.Result.Content {
		parts = append(parts, c.Text)
	}
	text := strings.Join(parts, "\n")
	voucher := regexp.MustCompile(`wsv_[a-z2-7]{32}`).FindString(text)
	if voucher == "" {
		t.Fatalf("no voucher in: %s", text)
	}

	// 3. It subscribes with the voucher, as the event argument wakeshift_voucher, and its webhook.
	key := make([]byte, 32)
	rand.Read(key)
	delivery := map[string]any{"mode": "webhook", "url": webhookURL, "secret": "whsec_" + base64.StdEncoding.EncodeToString(key)}
	subscribed := rpc("events/subscribe", "crawl.finished", map[string]any{
		"arguments": map[string]any{"job": "j9", "wakeshift_voucher": voucher},
		"delivery":  delivery,
	})
	if subscribed.Error != nil || subscribed.Result.ID == "" {
		t.Fatalf("subscribe: %+v", subscribed)
	}

	// 4. The emit arrives at the webhook.
	if _, err := crawlFinished.Emit(t.Context(), crawlArgs{Job: "j9"}, crawl{Pages: 4}); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-occurrences:
		data, _ := got["data"].(map[string]any)
		if got["name"] != "crawl.finished" || data["pages"] != float64(4) {
			t.Fatalf("occurrence: %v", got)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("no delivery in 30s")
	}

	// Without a voucher, ChatGPT is refused with its forbidden error, -32012.
	refused := rpc("events/subscribe", "crawl.finished", map[string]any{"arguments": map[string]any{"job": "j10"}, "delivery": delivery})
	if refused.Error == nil || refused.Error.Code != -32012 {
		t.Fatalf("without a voucher: %+v", refused)
	}
}
```

The unit tests from [the top of this page](#unit-tests), with `wakeshifttest`: a stand-in call for `watchCrawl`, and a test client that records emits:

```go file
// unit_test.go
package crawler

import (
	"testing"

	"github.com/wakeshift/wakeshift-go"
	"github.com/wakeshift/wakeshift-go/wakeshifttest"
)

func TestWatchCrawlSubscribes(t *testing.T) {
	call, rec := wakeshifttest.NewToolCall(wakeshifttest.ToolCallOptions{Tool: "watch_crawl", User: "ada"})
	if _, _, err := watchCrawl(t.Context(), call, crawlArgs{Job: "j1"}); err != nil {
		t.Fatal(err)
	}
	if subs := rec.Subscriptions(); len(subs) != 1 || subs[0].Event != "crawl.finished" {
		t.Fatalf("subscribed: %+v", subs)
	}
}

func TestEmitRecorded(t *testing.T) {
	tc := wakeshifttest.NewClient() // embeds an enabled, offline *wakeshift.Client
	done, err := wakeshift.DefineEvent(tc.Client, "crawl.finished", wakeshift.EventDef[crawlArgs, crawl]{Description: "A crawl finished"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := done.Emit(t.Context(), crawlArgs{Job: "j1"}, crawl{Pages: 3}); err != nil {
		t.Fatal(err)
	}
	if emits := tc.Emits(); len(emits) != 1 || emits[0].Event != "crawl.finished" {
		t.Fatalf("emitted: %+v", emits)
	}
}
```

A handler registered with `srv.AddTool` and named in `Wrap` finds its call with `wakeshift.ToolCallFrom(ctx)`: give it `wakeshifttest.ContextWithCall(t.Context(), call)`.

### mcp-go

On mcp-go, connect its in-process client to your attached server, with the client info of the agent you play. The Monitor and connector steps are the same as above:

```go file
// mcpgo_test.go
package crawler

import (
	"context"
	"strings"
	"testing"

	mcpgoclient "github.com/mark3labs/mcp-go/client"
	mcpgomcp "github.com/mark3labs/mcp-go/mcp"
	mcpgoserver "github.com/mark3labs/mcp-go/server"
	"github.com/wakeshift/wakeshift-go"
	"github.com/wakeshift/wakeshift-go/mcpgo"
)

// newMCPGoServer is the crawler on mcp-go.
func newMCPGoServer() *mcpgoserver.MCPServer {
	srv := mcpgoserver.NewMCPServer("crawler", "1.0.0")
	attached := mcpgo.Attach(wake, srv, wakeshift.AttachOptions{})
	mcpgo.AddTool(attached, mcpgomcp.NewTool("watch_crawl", mcpgomcp.WithDescription("Watch a crawl."), mcpgomcp.WithString("job", mcpgomcp.Required())),
		func(ctx context.Context, call *wakeshift.ToolCall, _ mcpgomcp.CallToolRequest, in crawlArgs) (*mcpgomcp.CallToolResult, error) {
			if _, err := crawlFinished.Subscribe(ctx, call, in); err != nil {
				return nil, err
			}
			return mcpgomcp.NewToolResultText("Watching crawl " + in.Job + "."), nil
		}, wakeshift.Subscribes(crawlFinished))
	return srv
}

func TestClaudeCodeOnMCPGo(t *testing.T) {
	c, err := mcpgoclient.NewInProcessClient(newMCPGoServer())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	var init mcpgomcp.InitializeRequest
	init.Params.ProtocolVersion = "2025-06-18" // initialize carries the client's name
	init.Params.ClientInfo = mcpgomcp.Implementation{Name: "claude-code", Version: "1.0.0"} // the agent you play
	if _, err := c.Initialize(t.Context(), init); err != nil {
		t.Fatal(err)
	}
	var call mcpgomcp.CallToolRequest
	call.Params.Name = "watch_crawl"
	call.Params.Arguments = map[string]any{"job": "j1"}
	res, err := c.CallTool(t.Context(), call)
	if err != nil {
		t.Fatal(err)
	}
	var parts []string
	for _, content := range res.Content {
		if tc, ok := mcpgomcp.AsTextContent(content); ok {
			parts = append(parts, tc.Text)
		}
	}
	if !strings.Contains(strings.Join(parts, "\n"), "with this command: u='") {
		t.Fatalf("no Monitor command in: %v", parts) // then run it and emit, as above
	}
}
```

For Codex, set `call.Params.Meta = &mcpgomcp.Meta{AdditionalFields: map[string]any{"x-codex-turn-metadata": map[string]any{"thread_id": "thread-test-1"}}}` and read the beacon from `res.Meta`.
