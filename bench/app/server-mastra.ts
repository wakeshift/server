// The toy app's MCP endpoint on Mastra (WB_MCP_SDK=mastra), the way a Mastra
// app serves one: an MCPServer (@mastra/mcp) of Mastra tools (createTool),
// served over Streamable HTTP with startHTTP, with Wakeshift through
// @wakeshift/sdk/mastra (attachMastra). legacy: "stateless" answers clients
// that open with initialize (Codex) as well as 2026-07-28 ones (Claude Code).
// The tools, the event and the control API are server.ts's; only the MCP
// route differs.
//
// Mastra writes its answer to a Node response itself, so each lane's
// MCPServer is served on a loopback server of its own, and the route
// forwards each exchange there and records each call from the result the
// client receives (Wakeshift's block added). The exchange's ID reaches the
// tools as the request's AuthInfo (req.auth, which Mastra passes on), so
// what a handler notes is found again by exchange and JSON-RPC request ID.

import { randomUUID } from "node:crypto";
import { appendFileSync } from "node:fs";
import { createServer, type IncomingMessage, type Server, type ServerResponse } from "node:http";
import type { AddressInfo } from "node:net";
import { MCPServer } from "@mastra/mcp";
import { createTool } from "@mastra/core/tools";
import type { CallToolResult } from "@modelcontextprotocol/server";
import { z } from "zod";
import type { ToolCall, Wakeshift, WakeEvent } from "@wakeshift/sdk";
import { attachMastra, type MastraContext } from "@wakeshift/sdk/mastra";
import { redactPlain } from "../src/redact.js";
// Used only once a request runs (server.ts imports this module).
import { WATCHING, short } from "./server.js";

const EXCHANGE = "x-wakebench-exchange";

/** What the Mastra route needs of the toy app (server.ts's benchApp). */
export interface MastraDeps {
  wake: Wakeshift;
  benchEvent: WakeEvent<"bench.event", any, any, false>;
  argsOf(lane: string): { run: string; lane: string };
  subs: Map<string, Set<string>>;
  seenPush(lane: string, ids: string[]): void;
  /** Records a call from its final result; extra is what the handler noted. */
  record(lane: string, tool: string, args: Record<string, unknown>, extra: Record<string, unknown>, result: CallToolResult, client: string): void;
  /** Rewrites watch's final result for WB_CLAUDE_TRANSPORT (server.ts's withTransport). */
  finishWatch(lane: string, harness: string | null | undefined, result: CallToolResult): CallToolResult;
  readJson(req: IncomingMessage): Promise<unknown>;
  log?(line: string): void;
  serverName?: string;
  /** attachMastra serves the wait tool. */
  wait?: boolean;
}

interface Note {
  extra: Record<string, unknown>;
  harness?: string | null;
  own: number;
}

/** The MCP route for /mcp/<lane> on Mastra. */
export function mastraRoute(d: MastraDeps): (lane: string, req: IncomingMessage, res: ServerResponse) => Promise<void> {
  const notes = new Map<string, Map<string | number, Note>>();
  const servers = new Map<string, MCPServer>();

  const serverFor = (lane: string): MCPServer => {
    let server = servers.get(lane);
    if (server) return server;
    // Every caller of a lane's server is that lane's user. Its lanes use only their own tools.
    const attached = attachMastra(d.wake, {
      identify: () => ({ user: lane }),
      builtinTools: { subscribe: false, list: false, unsubscribe: false, wait: !!d.wait },
    });
    const note = (call: ToolCall<MastraContext, never>, n: Note) => {
      const exchange = call.ctx.http?.authInfo?.token;
      const id = call.ctx.mcpReq?.id;
      if (exchange && id !== undefined) notes.get(exchange)?.set(id, n);
    };
    const watch = createTool({
      id: "watch",
      description: "Wake this conversation when a bench event is published.",
      inputSchema: attached.inputs(z.object({})),
      execute: attached.events("watch", { subscribes: [d.benchEvent] }, async (_args, call) => {
        const sub = await call.subscribe(d.benchEvent, d.argsOf(lane), { ttl: "1d" });
        const created = sub.state === "created" || sub.state === "renewed" ? sub : undefined;
        if (!d.subs.has(lane)) d.subs.set(lane, new Set());
        if (created) d.subs.get(lane)!.add(created.id);
        const recognized = sub.state === "unavailable" || sub.state === "disabled" ? undefined : sub.recognized;
        note(call, { extra: { agent: recognized, subscription: created?.id, session: created ? short(created.session) : undefined }, harness: recognized?.harness, own: 1 });
        return WATCHING;
      }),
    });
    const unwatch = createTool({
      id: "unwatch",
      description: "Stop waking this conversation for bench events.",
      inputSchema: attached.inputs(z.object({})),
      execute: attached.events("unwatch", { subscribes: [] }, async (_args, call) => {
        const ended = await call.unsubscribe(d.benchEvent, d.argsOf(lane));
        note(call, { extra: {}, own: 1 });
        return ended ? "Stopped watching bench events." : "This conversation wasn't watching bench events.";
      }),
    });
    const reportSeen = createTool({
      id: "report_seen",
      description: "Tell the bench which event codes (WB-…) you've received since you last reported.",
      inputSchema: attached.inputs(z.object({ ids: z.array(z.string()).describe("The event codes, e.g. WB-ab12-3") })),
      execute: attached.events("report_seen", { subscribes: [] }, async ({ ids }: { ids: string[] }, call) => {
        d.seenPush(lane, ids);
        note(call, { extra: {}, own: 1 });
        return `Noted ${ids.length} event(s).`;
      }),
    });
    server = attached.serve(
      new MCPServer({ id: `wakebench-${lane}`, name: d.serverName ?? "wakebench", version: "0.1.0", legacy: "stateless", tools: { watch, unwatch, report_seen: reportSeen } }),
    );
    servers.set(lane, server);
    return server;
  };

  // The loopback server Mastra answers on: /mcp/<lane>, the exchange's ID as req.auth.
  let inner: Promise<{ http: Server; base: string }> | undefined;
  const innerServer = () =>
    (inner ??= new Promise((resolve) => {
      const http = createServer((req, res) => {
        const url = new URL(req.url ?? "/", "http://mastra");
        const m = /^\/mcp\/([a-z0-9-]{1,40})$/.exec(url.pathname);
        if (!m) return void res.writeHead(404).end();
        const exchange = String(req.headers[EXCHANGE] ?? "");
        delete req.headers[EXCHANGE];
        (req as IncomingMessage & { auth?: unknown }).auth = { token: exchange, clientId: m[1], scopes: [] };
        serverFor(m[1])
          .startHTTP({ url, httpPath: url.pathname, req, res })
          .catch((err) => {
            d.log?.(`mastra ${m[1]}: ${redactPlain(String(err))}`);
            if (!res.headersSent) res.writeHead(500).end();
          });
      });
      http.listen(0, "127.0.0.1", () => resolve({ http, base: `http://127.0.0.1:${(http.address() as AddressInfo).port}` }));
    }));

  return async (lane, req, res) => {
    const body = req.method === "POST" ? await d.readJson(req) : undefined;
    if (process.env.WB_MCP_LOG) {
      // Methods, protocol versions and declared capabilities only: never arguments or results.
      const b = (Array.isArray(body) ? body[0] : body) as { method?: string; params?: { _meta?: Record<string, unknown> } } | undefined;
      const caps = JSON.stringify(b?.params?._meta?.["io.modelcontextprotocol/clientCapabilities"] ?? null);
      const at = () => new Date().toISOString();
      appendFileSync(
        process.env.WB_MCP_LOG,
        `${at()} ${lane} ${req.method} ${b?.method ?? "-"} header=${req.headers["mcp-protocol-version"] ?? "-"} meta=${b?.params?._meta?.["io.modelcontextprotocol/protocolVersion"] ?? "-"} caps=${caps} method-header=${req.headers["mcp-method"] ?? "-"} sdk=mastra\n`,
      );
      res.on("finish", () => appendFileSync(process.env.WB_MCP_LOG!, `${at()} ${lane} -> ${res.statusCode} ${b?.method ?? "-"} ${String(res.getHeader("content-type") ?? "")}\n`));
    }
    const exchange = randomUUID();
    const pending = new Map<string | number, { tool: string; args: Record<string, unknown>; client: string }>();
    for (const m of Array.isArray(body) ? body : [body]) {
      const r = m as { method?: string; id?: string | number; params?: { name?: unknown; arguments?: unknown; _meta?: Record<string, unknown> } } | undefined;
      if (r?.method !== "tools/call" || r.id === undefined || typeof r.params?.name !== "string") continue;
      const args = r.params.arguments && typeof r.params.arguments === "object" && !Array.isArray(r.params.arguments) ? (r.params.arguments as Record<string, unknown>) : {};
      const info = r.params._meta?.["io.modelcontextprotocol/clientInfo"] as { name?: unknown } | undefined;
      pending.set(r.id, { tool: r.params.name, args, client: typeof info?.name === "string" ? info.name : "" });
    }
    notes.set(exchange, new Map());
    try {
      const { base } = await innerServer();
      const headers = new Headers();
      for (const [k, v] of Object.entries(req.headers)) {
        if (v === undefined || k === "host" || k === "content-length" || k === "connection" || k === EXCHANGE) continue;
        headers.set(k, Array.isArray(v) ? v.join(", ") : v);
      }
      headers.set(EXCHANGE, exchange);
      const search = new URL(req.url ?? "/", "http://bench").search;
      const response = await fetch(`${base}/mcp/${lane}${search}`, { method: req.method, headers, body: body === undefined ? undefined : JSON.stringify(body) });
      const type = response.headers.get("content-type") ?? "";
      let text = await response.text();
      const finish = (m: any) => {
        const p = m && m.id !== undefined ? pending.get(m.id) : undefined;
        const n = p ? notes.get(exchange)?.get(m.id) : undefined;
        if (!p || !n || !m.result) return m;
        let result = m.result as CallToolResult;
        const added = result.isError ? "" : textOf((result.content ?? []).slice(n.own));
        if (p.tool === "watch") result = d.finishWatch(lane, n.harness, result);
        d.record(lane, p.tool, p.args, { ...n.extra, added: redactPlain(added) }, result, p.client);
        return { ...m, result };
      };
      if (type.includes("application/json") && text.trim()) {
        const j = JSON.parse(text);
        text = JSON.stringify(Array.isArray(j) ? j.map(finish) : finish(j));
      } else if (type.includes("text/event-stream")) {
        text = text
          .split("\n")
          .map((line) => (line.startsWith("data: ") ? "data: " + JSON.stringify(finish(JSON.parse(line.slice(6)))) : line))
          .join("\n");
      }
      const out: Record<string, string> = {};
      response.headers.forEach((v, k) => {
        if (k !== "content-length") out[k] = v;
      });
      res.writeHead(response.status, out);
      res.end(text);
    } finally {
      notes.delete(exchange);
    }
  };
}

function textOf(content: CallToolResult["content"]): string {
  return content.map((c) => (c.type === "text" ? c.text : "")).filter((t) => t !== "").join("\n");
}
