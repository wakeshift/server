/**
 * WB_CONNECTOR_ROUTE=1: the toy app as Claude Code reaches an app the user
 * added as a claude.ai connector, through Anthropic's MCP proxy. The bench
 * can't add a real claude.ai connector (that's the user's claude.ai
 * account), so a proxy in front of the app's MCP URL does to each call
 * what that route did on November 9, 2026 (the Claude desktop app's Code
 * tab, a watch through a claude.ai connector), as far as the app and the
 * core can tell:
 *
 * - the client states Anthropic's name, "claude-ai" (initialize's
 *   clientInfo, and 2026-07-28's per-request clientInfo in _meta), with
 *   Anthropic's own User-Agent;
 * - a request's _meta keeps only the protocol's own keys (and a
 *   progressToken): Claude Code's claudecode/toolUseId doesn't arrive, so
 *   nothing a beacon could be bound to does;
 * - a result's _meta keeps only the protocol's own keys (the transcript's
 *   mcpMeta held the server's serverInfo alone).
 *
 * Everything else passes through as it is.
 */
import { createServer, request as httpRequest, type IncomingMessage, type Server, type ServerResponse } from "node:http";
import type { AddressInfo } from "node:net";

/** The client name Anthropic's servers state (harnesses.ConnectorProxyClients). */
export const CONNECTOR_CLIENT = "claude-ai";

/** The protocol's own _meta keys, which the route keeps. */
function own(key: string): boolean {
  return key.startsWith("io.modelcontextprotocol/") || key === "progressToken";
}

function keepOwn(meta: unknown): Record<string, unknown> | undefined {
  if (!meta || typeof meta !== "object" || Array.isArray(meta)) return undefined;
  const out: Record<string, unknown> = {};
  for (const [k, v] of Object.entries(meta as Record<string, unknown>)) if (own(k)) out[k] = v;
  return out;
}

/** One JSON-RPC request as the route sends it on. */
export function rewriteRequest(msg: unknown): unknown {
  if (Array.isArray(msg)) return msg.map(rewriteRequest);
  if (!msg || typeof msg !== "object") return msg;
  const m = { ...(msg as Record<string, unknown>) };
  if (!m.params || typeof m.params !== "object") return m;
  const p = { ...(m.params as Record<string, unknown>) };
  if (p.clientInfo && typeof p.clientInfo === "object") p.clientInfo = { ...(p.clientInfo as object), name: CONNECTOR_CLIENT, version: "1.0.0" };
  if ("_meta" in p) {
    const meta = keepOwn(p._meta) ?? {};
    const ci = meta["io.modelcontextprotocol/clientInfo"];
    if (ci && typeof ci === "object") meta["io.modelcontextprotocol/clientInfo"] = { ...(ci as object), name: CONNECTOR_CLIENT, version: "1.0.0" };
    p._meta = meta;
  }
  m.params = p;
  return m;
}

/** One JSON-RPC response as the route hands it back. */
export function rewriteResponse(msg: unknown): unknown {
  if (Array.isArray(msg)) return msg.map(rewriteResponse);
  if (!msg || typeof msg !== "object") return msg;
  const m = { ...(msg as Record<string, unknown>) };
  if (m.result && typeof m.result === "object" && "_meta" in (m.result as object)) {
    const r = { ...(m.result as Record<string, unknown>) };
    r._meta = keepOwn(r._meta) ?? {};
    m.result = r;
  }
  return m;
}

/** A response body (JSON, or SSE whose data lines are JSON) rewritten. */
function rewriteBody(body: string, contentType: string): string {
  if (contentType.includes("text/event-stream")) {
    return body
      .split("\n")
      .map((line) => {
        if (!line.startsWith("data:")) return line;
        try {
          return "data: " + JSON.stringify(rewriteResponse(JSON.parse(line.slice(5))));
        } catch {
          return line;
        }
      })
      .join("\n");
  }
  try {
    return JSON.stringify(rewriteResponse(JSON.parse(body)));
  } catch {
    return body;
  }
}

function readAll(req: IncomingMessage): Promise<Buffer> {
  return new Promise((resolve, reject) => {
    const chunks: Buffer[] = [];
    req.on("data", (c: Buffer) => chunks.push(c));
    req.on("end", () => resolve(Buffer.concat(chunks)));
    req.on("error", reject);
  });
}

/** Starts the route in front of target (the app's MCP URL for a lane). */
export async function startConnectorRoute(target: (lane: string) => string): Promise<{ mcpUrl: (lane: string) => string; stop: () => Promise<void> }> {
  const srv: Server = createServer(async (req: IncomingMessage, res: ServerResponse) => {
    const lane = decodeURIComponent((req.url ?? "").replace(/^\/mcp\//, "").split("?")[0] ?? "");
    const to = new URL(target(lane));
    const raw = await readAll(req);
    let body = raw;
    if (req.method === "POST" && raw.length > 0) {
      try {
        body = Buffer.from(JSON.stringify(rewriteRequest(JSON.parse(raw.toString("utf8")))));
      } catch {
        body = raw;
      }
    }
    const headers: Record<string, string> = {};
    for (const [k, v] of Object.entries(req.headers)) {
      if (typeof v !== "string" || k === "host" || k === "content-length" || k === "accept-encoding") continue;
      headers[k] = v;
    }
    headers["user-agent"] = "Anthropic-MCP-Proxy/1.0";
    headers["content-length"] = String(body.length);
    const up = httpRequest({ hostname: to.hostname, port: to.port, path: to.pathname + to.search, method: req.method, headers }, (ur) => {
      const ct = String(ur.headers["content-type"] ?? "");
      const out: Record<string, string | string[]> = {};
      for (const [k, v] of Object.entries(ur.headers)) if (v !== undefined && k !== "content-length" && k !== "transfer-encoding") out[k] = v;
      if (req.method !== "POST") {
        // A GET stream (notifications): passed through as it comes.
        res.writeHead(ur.statusCode ?? 502, out);
        ur.pipe(res);
        return;
      }
      readAll(ur).then(
        (b) => {
          const text = rewriteBody(b.toString("utf8"), ct);
          res.writeHead(ur.statusCode ?? 502, { ...out, "content-length": String(Buffer.byteLength(text)) });
          res.end(text);
        },
        () => res.destroy(),
      );
    });
    up.on("error", () => {
      if (!res.headersSent) res.writeHead(502);
      res.end();
    });
    up.end(body);
  });
  await new Promise<void>((resolve) => srv.listen(0, "127.0.0.1", resolve));
  const { port } = srv.address() as AddressInfo;
  return {
    mcpUrl: (lane) => `http://127.0.0.1:${port}/mcp/${encodeURIComponent(lane)}`,
    stop: () => new Promise<void>((resolve) => srv.close(() => resolve())),
  };
}
