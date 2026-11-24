// The bench's toy app on the official MCP TypeScript SDK 1.x
// (@modelcontextprotocol/sdk), wrapped with @wakeshift/sdk (WB_APP=ts-v1;
// bench/app/CONTRACT.md), served as the toy app was before it moved to 2.x:
// an McpServer and Streamable HTTP transport per MCP session for clients
// that initialize, and a server per request for protocol 2026-07-28, as a
// 1.x app on a stateless transport serves it. It runs behind front.ts, like
// the Go and Python ports, and reads the same environment (toy.go).
import { createHash, randomUUID, timingSafeEqual } from "node:crypto";
import { readFileSync, statSync } from "node:fs";
import http from "node:http";
import { McpServer } from "@modelcontextprotocol/sdk/server/mcp.js";
import { StreamableHTTPServerTransport } from "@modelcontextprotocol/sdk/server/streamableHttp.js";
import { SUPPORTED_PROTOCOL_VERSIONS, isInitializeRequest } from "@modelcontextprotocol/sdk/types.js";
import { z } from "zod";
import { Wakeshift } from "@wakeshift/sdk";

process.stdin.on("end", () => process.exit(0));
process.stdin.resume();

function secretFile(name) {
  const path = process.env[name];
  if (!path) throw new Error(`set ${name}`);
  if ((statSync(path).mode & 0o077) !== 0) throw new Error(`${name} must be readable only by you (chmod 600)`);
  return readFileSync(path, "utf8").trim();
}

const c = JSON.parse(readFileSync(process.env.BENCH_CONTRACT, "utf8"));
const control = secretFile("BENCH_CONTROL_FILE");
const toStderr = (m) => console.error(m);
const wake = new Wakeshift({ key: secretFile("BENCH_KEY_FILE"), core: process.env.WAKESHIFT_CORE, logger: { error: toStderr, warn: toStderr, info: toStderr } });
await wake.app();
if (process.env.BENCH_REQUIRE_APP && wake.appId !== process.env.BENCH_REQUIRE_APP) {
  console.error(`refused: the key is for ${wake.appId}, not ${process.env.BENCH_REQUIRE_APP}`);
  process.exit(3);
}
const run = process.env.BENCH_RUN || "manual";
const name = process.env.BENCH_MCP_NAME || c.server.name;
const benchEvent = wake.defineEvent(c.event.name, {
  description: c.event.description,
  args: z.object({ run: z.string(), lane: z.string() }),
  payload: z.object({ code: z.string(), seq: z.number().int(), sent_at: z.string(), tag: z.string().optional() }),
  text: (p) => (p.tag ? c.event.textTagged.replace("{code}", p.code).replace("{tag}", p.tag) : c.event.text.replace("{code}", p.code)),
  topic: c.event.topic,
  public: true,
});
const argsOf = (lane) => ({ run, lane });
const short = (s) => (s ? createHash("sha256").update(s).digest("hex").slice(0, 12) : "");
const LANE = /^[a-z0-9-]{1,40}$/;
const notes = new Map();

/** The lane (the front's bearer token) and the exchange's call ID, from a request's headers. */
function laneOf(headers) {
  const m = /^Bearer (.+)$/.exec(String(headers?.authorization ?? ""));
  return m && LANE.test(m[1]) ? m[1] : "";
}
const noted = (call, note) => {
  const id = call.ctx?.requestInfo?.headers?.["x-wakebench-call"];
  if (typeof id === "string" && id) notes.set(id, note);
};

function mcpServer(lane) {
  const server = new McpServer({ name, version: c.server.version });
  // Every caller of a lane's server is that lane's user; its lanes use only
  // their own tools, and the wait tool only with BENCH_WAIT=1.
  const attached = wake.attach(server, {
    identify: () => ({ user: lane }),
    builtinTools: { subscribe: false, list: false, unsubscribe: false, wait: process.env.BENCH_WAIT === "1" },
  });
  attached.registerTool(
    "watch",
    { description: c.tools.watch.description, inputSchema: {}, subscribes: [benchEvent] },
    async (_args, _extra, call) => {
      const sub = await call.subscribe(benchEvent, argsOf(lane), { ttl: c.subscribeTtl });
      noted(call, { tool: "watch", lane, subscription: sub.id, session: short(sub.session), agent: sub.recognized });
      return { content: [{ type: "text", text: c.tools.watch.text }] };
    },
  );
  attached.registerTool(
    "unwatch",
    { description: c.tools.unwatch.description, inputSchema: {}, subscribes: [] },
    async (_args, _extra, call) => {
      const ended = await call.unsubscribe(benchEvent, argsOf(lane));
      noted(call, { tool: "unwatch", lane });
      return { content: [{ type: "text", text: ended ? c.tools.unwatch.ended : c.tools.unwatch.notWatching }] };
    },
  );
  attached.registerTool(
    "report_seen",
    { description: c.tools.report_seen.description, inputSchema: { ids: z.array(z.string()).describe(c.tools.report_seen.idsDescription) }, subscribes: [] },
    async ({ ids }, _extra, call) => {
      noted(call, { tool: "report_seen", lane });
      return { content: [{ type: "text", text: c.tools.report_seen.text.replace("{n}", String(ids.length)) }] };
    },
  );
  return server;
}

const sessions = new Map(); // MCP session ID → { lane, transport }
const isModern = (b) => !!b && typeof b === "object" && !Array.isArray(b) && !!b.params?._meta?.["io.modelcontextprotocol/protocolVersion"];

async function mcp(req, res) {
  const lane = laneOf(req.headers);
  if (!lane) return json(res, 401, { error: "no lane" });
  const body = req.method === "POST" ? await readJson(req) : undefined;
  if (req.method === "POST" && isModern(body)) {
    // Protocol 2026-07-28: a server of its own per request, no session.
    const versions = SUPPORTED_PROTOCOL_VERSIONS;
    if (!versions.includes("2026-07-28")) versions.unshift("2026-07-28");
    const server = mcpServer(lane);
    const transport = new StreamableHTTPServerTransport({ sessionIdGenerator: undefined, enableJsonResponse: true });
    res.on("close", () => {
      void transport.close();
      void server.close();
    });
    await server.connect(transport);
    return transport.handleRequest(req, res, body);
  }
  const sid = req.headers["mcp-session-id"];
  let s = typeof sid === "string" ? sessions.get(sid) : undefined;
  if (s && s.lane !== lane) return json(res, 404, { jsonrpc: "2.0", error: { code: -32001, message: "no such MCP session for this lane" }, id: null });
  if (!s) {
    if (req.method !== "POST" || !isInitializeRequest(body)) return json(res, 400, { jsonrpc: "2.0", error: { code: -32000, message: "no MCP session: initialize first" }, id: null });
    const transport = new StreamableHTTPServerTransport({ sessionIdGenerator: () => randomUUID(), onsessioninitialized: (id) => sessions.set(id, { lane, transport }) });
    transport.onclose = () => transport.sessionId && sessions.delete(transport.sessionId);
    await mcpServer(lane).connect(transport);
    s = { lane, transport };
  }
  await s.transport.handleRequest(req, res, body);
}

async function side(url, req, res) {
  const got = Buffer.from(String(req.headers.authorization ?? ""));
  const want = Buffer.from(`Bearer ${control}`);
  if (got.length !== want.length || !timingSafeEqual(got, want)) return json(res, 401, { error: { code: "unauthenticated", message: "control token" } });
  if (req.method === "GET" && url.pathname === "/bench/note") {
    const id = url.searchParams.get("call") ?? "";
    const note = notes.get(id) ?? null;
    notes.delete(id);
    return json(res, 200, { note });
  }
  const b = await readJson(req);
  if (!LANE.test(b?.lane ?? "")) return json(res, 400, { error: { code: "bad_request", message: "lane" } });
  if (req.method === "POST" && url.pathname === "/bench/emit") {
    try {
      const out = await benchEvent.emit(argsOf(b.lane), b.payload, { id: b.id });
      return json(res, 200, { matched: out.matched });
    } catch (err) {
      return json(res, 502, { error: { code: "emit", message: String(err) } });
    }
  }
  if (req.method === "POST" && url.pathname === "/bench/status") {
    const event = await benchEvent.status(argsOf(b.lane));
    const ids = Array.isArray(b.subscriptions) ? b.subscriptions.map(String) : [];
    let each = [];
    if (ids.length) {
      try {
        each = await benchEvent.statusOf(ids.map((id) => ({ user: b.lane, subscription: id })));
      } catch (err) {
        each = ids.map((id) => ({ id, error: String(err) }));
      }
    }
    return json(res, 200, { event, subscriptions: each });
  }
  return json(res, 404, { error: { code: "not_found", message: "no such call" } });
}

function json(res, status, body) {
  res.writeHead(status, { "content-type": "application/json" });
  res.end(JSON.stringify(body));
}

async function readJson(req) {
  const chunks = [];
  for await (const x of req) chunks.push(x);
  const s = Buffer.concat(chunks).toString("utf8");
  return s ? JSON.parse(s) : undefined;
}

http
  .createServer((req, res) => {
    const url = new URL(req.url ?? "/", "http://port");
    const work = url.pathname === "/mcp" ? mcp(req, res) : url.pathname.startsWith("/bench/") ? side(url, req, res) : Promise.resolve(json(res, url.pathname === "/health" ? 200 : 404, { ok: url.pathname === "/health" }));
    work.catch((err) => {
      console.error(String(err));
      if (!res.headersSent) json(res, 500, { error: { code: "internal", message: String(err) } });
      else res.end();
    });
  })
  .listen(Number(process.env.PORT), "127.0.0.1", () => console.error(`listening on ${process.env.PORT} (app ${wake.appId})`));
