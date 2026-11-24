# Tools registered elsewhere

Most servers register a tool that subscribes through the attachment (`attached.registerTool`, `@attached.tool`, `wakeshift.AddTool`), as the [quickstart](/docs/quickstart) does. When your tools are registered some other way, name the ones that subscribe in `attach`'s `wrap`, and find the call from inside the handler with `wake.currentCall()`. That covers a low-level `Server` with hand-written `tools/list` and `tools/call`, vendor middleware that registers tools for you, a catalog generated from an OpenAPI spec, and registrations such as `server.tool(...)`. A runtime with no MCP SDK server at all uses [`@wakeshift/sdk/tools`](#a-runtime-with-no-mcp-sdk-server).

## TypeScript

Set your handlers, then attach, naming each tool that subscribes with the events it subscribes to. Inside the handler, anywhere in its async call chain, `wake.currentCall()` is the call:

```ts
import { randomUUID } from "node:crypto"
import { Server, createMcpHandler } from "@modelcontextprotocol/server"
import { z } from "zod"
import { Wakeshift } from "@wakeshift/sdk"

const wake = new Wakeshift()

const crawlFinished = wake.defineEvent("crawl.finished", {
  description: "A crawl finished",
  args: z.object({ job: z.string() }),
  payload: z.object({ status: z.string() }),
})

function makeServer() {
  const server = new Server({ name: "crawler", version: "1.0.0" }, { capabilities: { tools: {} } })
  server.setRequestHandler("tools/list", async () => ({
    tools: [{ name: "start_crawl", description: "Crawl a site.", inputSchema: { type: "object", properties: { url: { type: "string" } }, required: ["url"] } }],
  }))
  server.setRequestHandler("tools/call", async (request) => {
    const { url } = request.params.arguments as { url: string }
    const job = randomUUID()
    await wake.currentCall().subscribe(crawlFinished, { job }, { once: true })
    await startJob(url, job)
    return { content: [{ type: "text", text: `Crawl ${job} started.` }] }
  })
  wake.attach(server, { wrap: { start_crawl: [crawlFinished] } })
  return server
}

const handler = createMcpHandler(makeServer)
```

On `@modelcontextprotocol/sdk` 1.x it's the same, with 1.x's request schemas. Code that lost the async context (a callback queue, a worker thread) passes the request context its handler got: `wake.currentCall(extra)` (2.x: `ctx`).

```ts
import { Server } from "@modelcontextprotocol/sdk/server/index.js"
import { CallToolRequestSchema, ListToolsRequestSchema } from "@modelcontextprotocol/sdk/types.js"

const lowServer = new Server({ name: "crawler", version: "1.0.0" }, { capabilities: { tools: {} } })
lowServer.setRequestHandler(ListToolsRequestSchema, async () => ({ tools: [/* your catalog */] }))
lowServer.setRequestHandler(CallToolRequestSchema, async (request, extra) => {
  const job = String(request.params.arguments?.job)
  await wake.currentCall(extra).subscribe(crawlFinished, { job })
  return { content: [{ type: "text", text: `Watching crawl ${job}.` }] }
})
wake.attach(lowServer, { wrap: { watch_crawl: [crawlFinished] } })
```

`wrap` works the same on an `McpServer` whose tools another library registers. On the `wrap` path TypeScript can't see the declared events at compile time, so `subscribe` checks them when it runs.

## Python

On the low-level `Server` (`mcp.server.lowlevel.Server`), `wrap` names the tool, and `wake.current_call()` finds its call:

```python lowlevel_server.py
from uuid import uuid4

import mcp_types as types
from mcp.server.lowlevel import Server
from pydantic import BaseModel

from wakeshift import Identity, Request, Wakeshift

wake = Wakeshift()


class Job(BaseModel):
    job: str


class Crawl(BaseModel):
    status: str


crawl_finished = wake.define_event("crawl.finished", description="A crawl finished", args=Job, payload=Crawl)


async def list_tools(ctx, params) -> types.ListToolsResult:
    schema = {"type": "object", "properties": {"url": {"type": "string"}}, "required": ["url"]}
    return types.ListToolsResult(tools=[types.Tool(name="start_crawl", description="Crawl a site.", input_schema=schema)])


async def call_tool(ctx, params: types.CallToolRequestParams) -> types.CallToolResult:
    job = str(uuid4())
    await wake.current_call().subscribe(crawl_finished, {"job": job}, once=True)
    return types.CallToolResult(content=[types.TextContent(type="text", text=f"Crawl {job} started.")])


def identify(r: Request) -> Identity | None:
    """Your own sign-in: user_of is your code, from the verified token."""
    return Identity(user=user_of(r.token)) if r.token else None


server = Server("crawler", on_list_tools=list_tools, on_call_tool=call_tool)
wake.attach(server, identify=identify, wrap={"start_crawl": [crawl_finished]})

app = server.streamable_http_app()
```

- Attach after the server has its handlers. Wakeshift lists its own tools after yours, and gives each tool in `wrap` its arguments.
- It also answers the server's `get_tool_input_schema` with the schema `tools/list` shows, Wakeshift's arguments included, so the MCP SDK's header validation on protocol 2026-07-28 accepts what the client was shown.
- `wrap` works on `MCPServer` and FastMCP (4, 3.x and 2.12+) too. A tool named in `wrap` may be sync, but only an `async def` can `await call.subscribe`.
- Code that lost the async context passes the request context it got: `wake.current_call(ctx)` (a tool's `Context` works too).
- `@attached.tool` raises `TypeError` on a low-level server: it has no tools to register.

## Go

On go-sdk, a tool your code registers with `srv.AddTool`, or from a generated catalog, is named in `Wrap`; its handler finds the call with `wakeshift.ToolCallFrom(ctx)`:

```go
type jobArgs struct {
	Job string `json:"job"`
}
type crawl struct {
	Status string `json:"status"`
}

wake, err := wakeshift.New()
if err != nil {
	log.Fatal(err)
}
crawlFinished, err := wakeshift.DefineEvent(wake, "crawl.finished", wakeshift.EventDef[jobArgs, crawl]{Description: "A crawl finished"})
if err != nil {
	log.Fatal(err)
}

catalog := mcp.NewServer(&mcp.Implementation{Name: "crawler", Version: "1.0.0"}, nil)
attached := wake.Attach(catalog, wakeshift.AttachOptions{
	Wrap: map[string][]wakeshift.ToolOption{"start_crawl": {wakeshift.Subscribes(crawlFinished)}},
})
catalog.AddTool(&mcp.Tool{
	Name:        "start_crawl",
	Description: "Crawl a site.",
	InputSchema: map[string]any{"type": "object", "properties": map[string]any{"url": map[string]any{"type": "string"}}},
}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	job := strings.ToLower(rand.Text())
	if _, err := crawlFinished.Subscribe(ctx, wakeshift.ToolCallFrom(ctx), jobArgs{Job: job}, wakeshift.Once()); err != nil {
		return nil, err
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "Crawl " + job + " started."}}}, nil
})
mux.Handle("/mcp", attached.Handler(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return catalog }, nil)))
```

`mcpgo.Attach` takes the same `Wrap` (and `WrapFunc`) for an mcp-go server, whose tools you register with `srv.AddTool`. `ToolCallFrom` outside a tool call returns nil, and `Subscribe` with it returns `CodeNoCall`.

**On stdio, serve stdin through the attachment.** Over HTTP, `attached.Handler` takes Wakeshift's arguments out of every call before go-sdk or mcp-go parses it, so no middleware ever sees them. On stdio, `attached.Stdin` does the same to each line:

- **go-sdk:** `srv.Run(ctx, attached.StdioTransport())`, go-sdk's stdio transport with `attached.Stdin` in front, which never closes your process's stdout ([the Go SDK's stdio server](/docs/go#stdio-servers)). With `&mcp.StdioTransport{}`, receiving middleware added after `Attach` runs first and sees `session_key`.
- **mcp-go:** `mcpgo.ServeStdio(attached)`, or `attached.Stdin(os.Stdin)` passed to `Listen`. Without it, Wakeshift's arguments are taken out in a `BeforeCallTool` hook `Attach` adds: tool middleware never sees them, but `BeforeAny`, `OnRequestInitialization` and `BeforeCallTool` hooks added before `Attach` do.

## A runtime with no MCP SDK server

A hand-rolled JSON-RPC runtime, or a framework that takes tool definitions and writes `tools/list` itself (xmcp), uses `attachTools` from `@wakeshift/sdk/tools`. It works through your tools alone:

```ts
import { attachTools } from "@wakeshift/sdk/tools"

const tools = attachTools(wake, { identify: (ctx) => ({ user: userOf(ctx.authInfo) }), wrap: { start_crawl: [crawlFinished] } })

const catalog = [{ name: "start_crawl", description: "Crawl a site.", inputSchema: { type: "object", properties: { url: { type: "string" } } } }]

// Your JSON-RPC runtime's tools/list and tools/call:
async function answer(method: string, params: any, headers: Record<string, string>) {
  const context = { meta: params?._meta, headers }
  if (method === "tools/list") return { tools: await tools.list(catalog, context) } // your tools amended, plus the built-ins
  return tools.call(params, context, async (p) => {
    const job = crypto.randomUUID()
    await wake.currentCall().subscribe(crawlFinished, { job }, { once: true })
    startCrawl(String(p.arguments?.url), job)
    return { content: [{ type: "text", text: `Crawl ${job} started.` }] }
  })
}
```

- `tools.list(catalog, context)` returns your catalog with Wakeshift's arguments and the subscribes sentence on each tool in `wrap`, plus the built-in tools.
- `tools.call(params, context, dispatch)` answers a built-in itself, and passes every other call to `dispatch` with Wakeshift's arguments taken out. A tool in `wrap` runs as a call that subscribes, and its result gets Wakeshift's block.
- Every field of the context is optional: `meta` (the call's `_meta`) and `headers` tell Wakeshift which agent program is calling; `clientInfo` (`initialize`'s) names the client; `authInfo` is the verified token, which `identify` gets. Pass only a token your runtime already verified: Wakeshift doesn't check it.
- **Errors are yours to send.** Throw from `dispatch` to answer with a JSON-RPC error, or to refuse at your HTTP layer (a 401): `tools.call` rethrows what `dispatch` throws, unchanged, after rolling back anything the call subscribed.
- **Protocol 2026-07-28.** On a 2026-07-28 request, `tools.call` gives a result without `resultType` (the built-ins', or one `dispatch` returned) `resultType: "complete"`, which the official 2.x clients require. The list differs by caller, so answer `tools/list` with `cacheScope: "private"` and `ttlMs: 0`, whatever an upstream list said.

### A proxy in front of a hosted MCP server

A proxy (a Worker, a gateway) that forwards to an MCP server you host elsewhere adds Wakeshift at the proxy: amend the upstream's `tools/list`, and answer `tools/call` with the upstream as `dispatch`.

```ts
import { attachTools } from "@wakeshift/sdk/tools"

const proxyTools = attachTools(wake, { identify: (ctx) => ({ user: userOf(ctx.authInfo) }), wrap: { start_crawl: [crawlFinished] } })

// upstream(method, params) forwards one JSON-RPC request and returns its result, or throws its error.
async function proxy(method: string, params: any, headers: Record<string, string>, upstream: (method: string, params: unknown) => Promise<any>) {
  const context = { meta: params?._meta, headers }
  if (method === "tools/list") {
    const listed = await upstream("tools/list", params)
    return { ...listed, tools: await proxyTools.list(listed.tools, context), cacheScope: "private", ttlMs: 0 }
  }
  if (method === "tools/call") {
    return proxyTools.call(params, context, async (p) => {
      const job = crypto.randomUUID()
      if (p.name === "start_crawl") await wake.currentCall().subscribe(crawlFinished, { job }, { once: true })
      return upstream("tools/call", { ...p, arguments: { ...p.arguments, job } })
    })
  }
  return upstream(method, params)
}
```

The upstream never sees Wakeshift's arguments, and its results come back with Wakeshift's block. Keep the upstream's `_meta` on results as they pass through ([results keep their `_meta`](#results-keep-their-meta)).

This proxy gives up two things. **It serves no MCP Events**, so ChatGPT and other clients that subscribe natively can't subscribe through it. **And it forwards no credential**: `upstream` is yours to write, so pass the caller's `Authorization` header on, and answer a request the upstream refuses with its 401 (and `WWW-Authenticate`) at your HTTP layer, so the client signs in again.

**A proxy that keeps ChatGPT** is an MCP server of its own: a low-level `Server` whose `tools/list` and `tools/call` forward to the upstream with the caller's credential, attached like any server. Wakeshift then serves MCP Events and vouchers at the proxy, and amends the upstream's list:

```ts
import { randomUUID } from "node:crypto"
import { Server, createMcpHandler, type ServerContext } from "@modelcontextprotocol/server"

const UPSTREAM = "https://mcp.example.com/mcp" // your hosted server, without sessions

// One JSON-RPC request to the upstream, with the caller's own credential.
async function upstreamRpc(method: string, params: unknown, ctx: ServerContext) {
  const authorization = ctx.http?.req?.headers.get("authorization")
  const res = await fetch(UPSTREAM, {
    method: "POST",
    headers: { "Content-Type": "application/json", Accept: "application/json, text/event-stream", ...(authorization ? { Authorization: authorization } : {}) },
    body: JSON.stringify({ jsonrpc: "2.0", id: 1, method, params }),
  })
  const body = await res.text()
  const answer = JSON.parse(body.startsWith("{") ? body : body.split("\n").find((l) => l.startsWith("data: "))!.slice(6))
  if (answer.error) throw new Error(answer.error.message)
  return answer.result
}

function makeProxyServer() {
  const proxyServer = new Server({ name: "crawler", version: "1.0.0" }, { capabilities: { tools: {} } })
  proxyServer.setRequestHandler("tools/list", (request, ctx) => upstreamRpc("tools/list", request.params, ctx))
  proxyServer.setRequestHandler("tools/call", async (request, ctx) => {
    if (request.params.name !== "start_crawl") return upstreamRpc("tools/call", request.params, ctx)
    const job = randomUUID()
    await wake.currentCall().subscribe(crawlFinished, { job }, { once: true })
    return upstreamRpc("tools/call", { ...request.params, arguments: { ...request.params.arguments, job } }, ctx)
  })
  wake.attach(proxyServer, { identify: (ctx) => ({ user: userOf(ctx.http?.authInfo) }), wrap: { start_crawl: [crawlFinished] } })
  return proxyServer
}

const proxyHandler = createMcpHandler(makeProxyServer) // behind your auth, which checks the caller's token first
```

Your auth runs in front of the handler, as on any server: refuse a token the upstream would refuse with a 401 there, before the MCP handler, since a JSON-RPC error can't ask the client to sign in again.

A framework that owns `tools/list`, such as xmcp, takes each tool as a definition instead:

```ts
import { attachTools } from "@wakeshift/sdk/tools"

// src/wake.ts: attach once, after your events.
const xmcpTools = attachTools(wake, { identify: (ctx) => ({ user: userOf(ctx.authInfo) }) })

// src/tools/start_crawl.ts
const startCrawlTool = xmcpTools.tool(
  "start_crawl",
  { description: "Crawl a site.", inputSchema: { url: z.string() }, subscribes: [crawlFinished] },
  async ({ url }, ctx, call) => {
    const job = crypto.randomUUID()
    await call.subscribe(crawlFinished, { job }, { once: true })
    startCrawl(url, job)
    return `Crawl ${job} started.`
  },
)
// xmcp takes the file's exports: export const schema = startCrawlTool.schema, metadata = startCrawlTool.metadata; export default startCrawlTool.handler
```

`xmcpTools.builtins()` gives the built-in tools as the same kind of definition, one file each. This path answers no MCP Events and serves no wait tool, so clients that only subscribe natively, such as ChatGPT, can't subscribe here. Claude Code, Codex and every other agent subscribe through your tools and are woken as on any other server.

## Generated tools

A catalog generated from an OpenAPI spec has no handler of yours to subscribe in. Subscribe in the one place every generated call passes through, and name the tools that subscribe with `wrap` as a function, which also covers tools added at run time:

```ts
// Which generated operations subscribe, and to what: your own table.
const watches: Record<string, { event: typeof crawlFinished; args: (result: any) => { job: string } }> = {
  start_crawl: { event: crawlFinished, args: (result) => ({ job: String(result.structuredContent?.job) }) },
}

wake.attach(server, { wrap: (tool) => (watches[tool] ? [watches[tool].event] : undefined) })

// The dispatcher your generator calls for every operation.
async function callGenerated(name: string, run: () => Promise<any>) {
  const result = await run()
  const watch = watches[name]
  if (watch && !result.isError) await wake.currentCall().subscribe(watch.event, watch.args(result), { replay: "10m", once: true })
  return result
}
```

When the job's ID exists only once the operation has run, subscribe after it with `replay`, so an occurrence emitted in between still arrives ([subscribe before you start the work](#subscribe-before-you-start-the-work)). In Python, `wrap=` takes the same function and `wake.current_call()` finds the call; in Go, `AttachOptions.WrapFunc` and `wakeshift.ToolCallFrom(ctx)`.

## Results keep their `_meta` {#results-keep-their-meta}

Wakeshift's addition to a result is text for the agent and a `_meta` entry for its program ([exactly what](/docs/events#what-wakeshift-adds-to-a-result)). Codex, and Claude Code with Wakeshift's plugin, read what they need only from the result's own `_meta`, never from its text. If a proxy or framework between your server and the client rebuilds tool results, it must pass `_meta` through, or those agents aren't woken, and nothing reports it. The addition goes in after your handler and your result validation, so your own checks never see it.

## Subscribe before you start the work

Subscribe the conversation first, then start the job. An occurrence emitted before the subscription exists reaches nobody, and a fast job (a cached crawl) can finish in that gap. If the job can't start, throw or return `isError`: the subscription ends with the call.

When you can't subscribe first, because the job's ID exists only once it's started, pass `replay`, which also delivers the latest occurrence with these arguments from that far back (at most 24 hours), with `once`. That ID is unique to the job, so the only occurrence `replay` can find is this job's own. On arguments that repeat (a repo, a branch), leave `replay` off: an earlier occurrence would end a `once` subscription at once.

```ts
const started = await startCrawl("https://example.com")
await wake.currentCall().subscribe(crawlFinished, { job: started }, { replay: "10m", once: true })
```

When the work is paid for and the subscription is only a convenience, do the work first, then subscribe in a `try`, so a Wakeshift failure never fails a call the user paid for.

## What each tool gets

- Each tool in `wrap` is listed with Wakeshift's arguments added to its input schema (`session_key`, and `harness` and `conversation_id` on a tool that subscribes to something), a sentence in its description saying it's how your app notifies the agent of those events (after a period, if a one-line description lacks one), and a `wakeshift` field in its output schema.
- Its calls reach your handler without those arguments, so a strict schema never refuses them. Every other tool's calls have them taken out too, so `session_key` never reaches code that doesn't expect it.
- A tool in `wrap` whose input schema already names `session_key`, `harness` or `conversation_id`, or whose output schema names `wakeshift`, is left unwrapped, and the SDK logs `wrap_arg_conflict` at error level. A name in `wrap` that the server never lists logs `wrap_unknown_tool`.
- A call that throws, returns `isError`, returns output its schema refuses, or is canceled ends the subscriptions it made. A result shaped `{ "error": … }` that isn't `isError` counts as success.

## How Wakeshift recognizes the agent

Wakeshift picks the wake path for each agent program from what its requests carry, in this order: the client's name from `initialize` (a server with sessions); the client's info in the request's `_meta` (protocol 2026-07-28 sends it on every request); the keys a program puts in `_meta`; the `User-Agent` header; the `client` your `identify` returns; and last, the agent's own `harness` argument, which only picks between programs whose requests look alike. All of these are asserted by the client, so they choose how to wake an agent, never who may see what. Over stateless HTTP, Claude Code, Codex, Antigravity, opencode, Cursor and GitHub Copilot CLI are recognized (Claude Code by a `User-Agent` that starts with `claude-code/`, such as `claude-code/2.1.0 (external, cli)`); for another client, return its name from `identify` as `client`. Whether a client subscribes natively is decided first: one that declares the MCP Events extension in its capabilities (in `initialize`, or in a 2026-07-28 request's `_meta`) is served as a native client, whatever its name.
