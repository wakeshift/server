# MCP frameworks

Wakeshift's SDKs work inside the MCP server you already have, with one way in for each kind of server. On a supported framework, your server gets everything on the [events](/docs/events) page: your own tools that subscribe, the built-in tools, MCP Events for clients that speak it, and the wake paths for every agent.

| Your server | TypeScript | Python | Go |
| --- | --- | --- | --- |
| The MCP SDK's high-level server | `wake.attach(server)` and `attached.registerTool` | `wake.attach(server)` and `@attached.tool` | `wake.Attach(srv, opts)`, `wakeshift.AddTool` and `attached.Handler` |
| Tools registered elsewhere: a low-level `Server`, vendor middleware, a generated catalog | `wake.attach(server, { wrap })` and `wake.currentCall()` | `wake.attach(server, wrap=…)` and `wake.current_call()` | `wake.Attach(srv, wakeshift.AttachOptions{Wrap: …})` and `wakeshift.ToolCallFrom(ctx)` |
| A server built for each request | Attach in the function that builds it | Attach in the function that builds it | `wake.Attach` on each server, and one `wake.Handler(next, opts)` around the HTTP handler |
| A process that only emits (a worker, a webhook handler) | `new Wakeshift()`, `defineEvent`, `emit` | `Wakeshift()`, `define_event`, `emit` | `wakeshift.New()`, `DefineEvent`, `Emit` |

[Tools registered elsewhere](/docs/low-level) covers the second row in all three languages. Not listed, or not supported yet? The [HTTP API](/docs/other-languages) works from any server.

**Push needs a streaming answer.** MCP Events' push (`events/stream`) keeps a request open while the client listens, so a server in the MCP SDK's JSON-response mode (TypeScript `enableJsonResponse: true` on 1.x or `responseMode: "json"` on 2.x, Python `json_response=True`) offers its events by webhook and poll only. The SDK reads the setting where it can; where something in front of your server forces JSON answers, turn push off with `push: false` (Python `push=False`, Go `Push: wakeshift.PushOff`). [Push on your host](/docs/mcp-events#push-on-your-host) has the rest, and [push options](/docs/mcp-events#push-options) the settings for long-lived streams.

## TypeScript

| Framework | Wakeshift | How |
| --- | --- | --- |
| `@modelcontextprotocol/server` 2.x (2.3 or later), `McpServer` | Supported | [TypeScript SDK](/docs/sdk): `wake.attach` and `attached.registerTool` |
| `@modelcontextprotocol/sdk` 1.x (1.23 or later), `McpServer` | Supported | [Using @modelcontextprotocol/sdk 1.x](/docs/sdk#using-modelcontextprotocolsdk-1x) |
| Either MCP SDK's low-level `Server` | Supported | [Tools registered elsewhere](/docs/low-level#typescript): `wrap` and `wake.currentCall()` |
| Vercel `mcp-handler` | Supported | `wake.attach` on the server its `createMcpHandler` hands you, as with 2.x's |
| Cloudflare `agents`, `createMcpHandler` | Supported, with `nodejs_compat` | [Cloudflare Workers](/docs/sdk#cloudflare-workers) |
| Mastra `MCPServer` (`@mastra/mcp` 2.7 or later) | Supported | [Mastra](/docs/sdk#mastra): `attachMastra` from `@wakeshift/sdk/mastra` |
| xmcp | Supported, without MCP Events | [xmcp](/docs/sdk#xmcp): `attachTools` from `@wakeshift/sdk/tools`. Clients that only subscribe natively, such as ChatGPT, can't subscribe there |
| A runtime with no MCP SDK server (hand-rolled JSON-RPC) | Supported, without MCP Events | [`attachTools`](/docs/low-level#a-runtime-with-no-mcp-sdk-server) from `@wakeshift/sdk/tools` |
| Cloudflare `agents`, `McpAgent` | Not supported | It keeps a session per client, so it refuses MCP protocol 2026-07-28. Use `createMcpHandler` |
| `fastmcp` (punkpeye) 4.x | Supported, served with its `connect` | [fastmcp](#fastmcp-punkpeye): attach each session's `server`, and serve HTTP with `server.connect(transport)` |
| `mcp-use` 1.x, `MCPServer` | Supported | [mcp-use](#mcp-use): attach to the server its `getServerForSession` returns, and a middleware for 2026-07-28 |
| `mcp-use` 2.x, `MCPServer` | Supported, without MCP Events | [mcp-use](#mcp-use): `attachTools` from `@wakeshift/sdk/tools`. Clients that only subscribe natively, such as ChatGPT, can't subscribe there |

### fastmcp (punkpeye)

fastmcp builds a low-level `Server` for each session and hands it out in its `connect` event, before the client's first request: attach there, naming your tools that subscribe in `wrap`, and find the call with `wake.currentCall()` in `execute`.

fastmcp's own `start()` refuses MCP protocol 2026-07-28: its HTTP layer (mcp-proxy) serves that revision only from an MCP SDK 2.x server, and fastmcp builds 1.x ones. So serve HTTP yourself with fastmcp's `server.connect(transport)`, one stateless transport per request, which `attach` teaches 2026-07-28. Every client connects, old and new.

```ts
import { createServer } from "node:http";
import { FastMCP } from "fastmcp";
import { StreamableHTTPServerTransport } from "@modelcontextprotocol/sdk/server/streamableHttp.js";
import { z } from "zod";
import { Wakeshift } from "@wakeshift/sdk";

const wake = new Wakeshift();
const buildFinished = wake.defineEvent("build.finished", {
  description: "A build finished",
  args: z.object({ repo: z.string() }),
  payload: z.object({ status: z.string() }),
});

const server = new FastMCP({ name: "ci", version: "1.0.0" });
server.on("connect", ({ session }) => wake.attach(session.server, { wrap: { watch_build: [buildFinished] } }));

server.addTool({
  name: "watch_build",
  description: "Watch a repository's builds.",
  parameters: z.object({ repo: z.string() }),
  execute: async ({ repo }) => {
    await wake.currentCall().subscribe(buildFinished, { repo });
    return `Watching ${repo}.`;
  },
});

createServer(async (req, res) => {
  if (new URL(req.url!, "http://localhost").pathname !== "/mcp") return res.writeHead(404).end();
  if (req.method !== "POST") return res.writeHead(405, { Allow: "POST" }).end();
  const transport = new StreamableHTTPServerTransport({ sessionIdGenerator: undefined });
  const session = await server.connect(transport);
  res.on("close", () => session.close());
  await transport.handleRequest(req, res);
}).listen(8080);
```

Install `@modelcontextprotocol/sdk` 1.x beside fastmcp for the transport. With fastmcp's `authenticate`, pass what it returns: `server.connect(transport, await authenticate(req))`. Serving HTTP yourself drops what `start()` adds around it (CORS, its OAuth routes, `/health`): add those your server needs. Each request gets a stateless transport, so a tool can't ask the client for input or for a sampling call mid-request (elicitation and sampling need a session); tools that do should keep a session transport of their own. If `npm ls @modelcontextprotocol/sdk` shows a second copy nested under fastmcp, pin yours to the version fastmcp uses, so the transport you import and fastmcp's server come from one copy. Over stdio, keep fastmcp's own `start({ transportType: "stdio" })` and attach in its `connect` event as above. fastmcp can answer the client's first `tools/list` before that event fires; fastmcp 4 declares `tools.listChanged`, so `attach` then sends `notifications/tools/list_changed` and the client lists again, with Wakeshift's part (Claude Code and Codex do). fastmcp 3 doesn't declare it, so a client that lists within about 100 ms of connecting keeps a list without Wakeshift's arguments: use fastmcp 4. Over stdio fastmcp negotiates up to 2025-11-25, never 2026-07-28.

### mcp-use

**mcp-use 1.x** builds an official MCP SDK 1.x `McpServer` for each session, and for each stateless request, in its `getServerForSession`. Wrap that method and attach to the server it returns, naming your tools that subscribe in `wrap`; each finds its call with `wake.currentCall()`.

```ts
import { MCPServer, getRequestContext, text } from "mcp-use/server"
import { z } from "zod"
import { Wakeshift } from "@wakeshift/sdk"

const wake = new Wakeshift()
const jobFinished = wake.defineEvent("job.finished", {
  description: "A job finished",
  args: z.object({ job: z.string() }),
  payload: z.object({ status: z.string() }),
})

const server = new MCPServer({ name: "jobs", version: "1.0.0", oauth })

server.tool({ name: "start_job", description: "Start a job.", schema: z.object({ name: z.string() }) }, async ({ name }) => {
  const job = await runJob(name)
  await wake.currentCall().subscribe(jobFinished, { job }, { once: true })
  return text(`Job ${job} started.`)
})

// mcp-use's bearer middleware keeps the verified token in its own request
// context, as "auth"; its transport passes nothing to identify as authInfo.
function identify() {
  const user = getRequestContext()?.get("auth")?.user?.userId
  return user ? { user: String(user) } : undefined
}

const sessionServer = server.getServerForSession.bind(server)
server.getServerForSession = (sessionId?: string) => {
  const mcp = sessionServer(sessionId)
  // push: false, since the middleware below makes 2026-07-28 answers JSON.
  wake.attach(mcp, { identify, wrap: { start_job: [jobFinished] }, push: false })
  return mcp
}

// Protocol 2026-07-28 has no session. mcp-use answers a request without one
// statelessly, through its own /mcp handler and its own auth, when the
// request doesn't accept an event stream: this only rewrites that header.
server.app.use("/mcp", async (c, next) => {
  if (c.req.method === "POST" && !c.req.header("mcp-session-id") && c.req.header("mcp-protocol-version") === "2026-07-28")
    c.req.raw.headers.set("accept", "application/json")
  await next()
})

await server.listen(3000)
```

- **The user comes from mcp-use's request context.** mcp-use verifies the bearer token itself and keeps the result, with what your provider's `getUserInfo` returned, in its request context (`getRequestContext()?.get("auth")`), which is what a tool's `ctx.auth` reads. Its transport doesn't pass it on as 1.x's `extra.authInfo`, so an `identify` that reads `extra.authInfo` finds no user, and every call is anonymous (refused, with `requireUser`). Read it as above.
- **Protocol 2026-07-28 stays behind mcp-use's auth.** The middleware changes no answer of its own: the request goes on to mcp-use's `/mcp` handler and its OAuth like any other, so a request without a valid token gets mcp-use's 401 on every protocol. Don't answer 2026-07-28 from a transport of your own in a middleware: registered there, it runs ahead of mcp-use's OAuth and serves `tools/list` to anyone ([serving 2026-07-28 beside sessions](/docs/sdk#serving-protocol-2026-07-28-beside-sessions)). Check it: a 2026-07-28 `tools/list` without a token must get a 401.
- **The middleware turns off push.** It makes 2026-07-28 requests take JSON answers, which can't hold a push stream open, so attach with `push: false`, as above: MCP Events clients then subscribe by webhook or poll.
- **Without the middleware**, clients on older revisions work as before, and clients that speak only 2026-07-28 can't connect.

**mcp-use 2.x** builds its MCP server privately for each request, from a copy of the MCP SDK bundled inside it, and never hands it to your code, so `attach` has nothing to attach to. Use `attachTools` from `@wakeshift/sdk/tools`, as for [xmcp](/docs/sdk#xmcp): make each tool that subscribes with `tools.tool`, and register it, and each of `tools.builtins()`, with mcp-use's own `server.tool`. There are no MCP Events there, so clients that only subscribe natively, such as ChatGPT, can't subscribe; every other agent is woken as usual.

```ts
import { AsyncLocalStorage } from "node:async_hooks"
import { MCPServer } from "mcp-use"
import { z } from "zod"
import { Wakeshift } from "@wakeshift/sdk"
import { attachTools, type ToolsTool } from "@wakeshift/sdk/tools"

const wake = new Wakeshift()
const jobFinished = wake.defineEvent("job.finished", {
  description: "A job finished",
  args: z.object({ job: z.string() }),
  payload: z.object({ status: z.string() }),
})

// ctx is what register passes below: auth is mcp-use's verified sign-in.
const tools = attachTools(wake, {
  identify: (ctx) => {
    const user = (ctx.auth as { user?: { userId?: string } } | undefined)?.user?.userId
    return user ? { user } : undefined
  },
})
const server = new MCPServer({ name: "jobs", version: "1.0.0", oauth })

// A tool's callback doesn't get the call's _meta (where Claude Code and
// Codex name themselves and their thread); mcp-use's tools/call middleware does.
const callMeta = new AsyncLocalStorage<Record<string, unknown> | undefined>()
server.use("mcp:tools/call", (ctx, next) => callMeta.run(ctx.params._meta, next))
const register = (t: ToolsTool) =>
  server.tool({ name: t.metadata.name, description: t.metadata.description, inputSchema: z.object(t.schema), annotations: t.metadata.annotations }, (args, ctx) =>
    t.handler(args, {
      clientInfo: ctx.client.info(),
      headers: Object.fromEntries(ctx.req?.raw.headers ?? []),
      signal: ctx.signal,
      meta: callMeta.getStore(),
      auth: ctx.auth,
    }),
  )

register(
  tools.tool("start_job", { description: "Start a job.", inputSchema: { name: z.string() }, subscribes: [jobFinished] }, async ({ name }, ctx, call) => {
    const job = await runJob(name)
    await call.subscribe(jobFinished, { job }, { once: true })
    return `Job ${job} started.`
  }),
)
for (const b of tools.builtins()) register(b)

await server.listen(3000)
```

mcp-use 2 serves protocol 2026-07-28 itself, behind its own auth, so there's nothing to add for it.

## Python

| Framework | Wakeshift | How |
| --- | --- | --- |
| `mcp` 2.x (2.3 or later), `MCPServer` | Supported | [Python SDK](/docs/python): `pip install 'wakeshift[mcp]'`, `wake.attach` and `@attached.tool` |
| `mcp` 2.x, low-level `Server` | Supported | [Tools registered elsewhere](/docs/low-level#python): `wrap` and `wake.current_call()` |
| FastMCP 4 (`fastmcp` 4.1 or later) | Supported | [FastMCP](/docs/python#fastmcp): `pip install 'wakeshift[fastmcp]'` |
| FastMCP 2.x (2.12 or later) and 3.x | Supported, without native MCP Events | Works on FastMCP 2.12+ and 3.x; native MCP Events need 4. Install plain `wakeshift` (never `[mcp]`): `wake.attach`, `@attached.tool` and `wrap` work as on FastMCP 4, and every client subscribes through Wakeshift's tools ([FastMCP 2 and 3](/docs/python#fastmcp-2-and-3)) |

## Go

| Framework | Wakeshift | How |
| --- | --- | --- |
| `github.com/modelcontextprotocol/go-sdk` (v1.8 or later) | Supported | [Go SDK](/docs/go): `wake.Attach`, `wakeshift.AddTool` and `attached.Handler` |
| `github.com/mark3labs/mcp-go` (v1.0 or later) | Supported | [mcp-go](/docs/go#mcp-go): `mcpgo.Attach` and `mcpgo.AddTool` |
