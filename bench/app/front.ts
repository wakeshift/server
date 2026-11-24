// The front for a toy-app port (bench/app/CONTRACT.md): a Go or Python toy
// app (WB_APP=go-sdk, py-mcpserver, …) serves MCP and emits through its own
// SDK, and this process stands in front of it, so the bench sees the same
// toy app whatever the port is written in:
//
//   - /mcp/<lane> is passed through to the port's /mcp, untouched but for
//     two headers it adds (the lane as a bearer token, whose user every
//     caller of the lane's server is, and a call ID per exchange), and each
//     call of the app's three tools is recorded from the result as it
//     leaves (Wakeshift's block added), as server.ts records its own. SSE
//     responses are rewritten event by event as they stream, never held.
//   - /control/* is server.ts's control API: publish (the port emits),
//     lane, status (the port reads its SDK's status and statusOf).
//
// What the wire doesn't show (the subscription a watch made, a hash of the
// session it gave, what Wakeshift recognized) the port's handler files
// under the exchange's call ID, and the front reads it back (/bench/note)
// before the result goes on. A call whose handler didn't run (it threw)
// leaves no note and isn't recorded, as in server.ts.

import { randomUUID } from "node:crypto";
import { spawn, type ChildProcess } from "node:child_process";
import { readFileSync } from "node:fs";
import { createServer, type IncomingMessage, type Server, type ServerResponse } from "node:http";
import { redactPlain } from "../src/redact.js";
import { CODE, codeNonce, readJson, short, type CallRecord, type DeliveryRow, type EventRecord, type SeenRecord } from "./server.js";

/** contract.json, as the front reads it. */
export interface Contract {
  server: { name: string; secondName: string; version: string };
  event: { name: string; description: string; topic: string; text: string; textTagged: string };
  subscribeTtl: string;
  tools: {
    watch: { description: string; text: string };
    unwatch: { description: string; ended: string; notWatching: string };
    report_seen: { description: string; idsDescription: string; text: string };
  };
}

export const APP_TOOLS = ["watch", "unwatch", "report_seen"] as const;

/** A note a port's handler filed for its call (toy.go's Note, toy.py's notes). */
export interface PortNote {
  tool: string;
  lane: string;
  subscription?: string;
  session?: string;
  agent?: { harness?: string | null; via?: string | null; claimed?: string | null } | null;
}

export interface FrontOptions {
  /** The port's base URL (http://127.0.0.1:<port>). */
  port: string;
  run: string;
  appId: string;
  contract: Contract;
  controlToken: string;
  slot?: 1 | 2;
  deliveries?: (appId: string, topic: string) => Promise<DeliveryRow[]>;
  now?: () => number;
  log?: (line: string) => void;
}

/** The texts a tool's own reply can be, so what Wakeshift added is the rest. */
export function ownTexts(c: Contract, tool: string, args: Record<string, unknown>): string[] {
  if (tool === "watch") return [c.tools.watch.text];
  if (tool === "unwatch") return [c.tools.unwatch.ended, c.tools.unwatch.notWatching];
  if (tool === "report_seen") {
    const n = Array.isArray(args.ids) ? args.ids.length : 0;
    return [c.tools.report_seen.text.replace("{n}", String(n))];
  }
  return [];
}

/**
 * What Wakeshift added to a result: its text after the tool's own reply,
 * whether the SDK put its block in a content block of its own (Go,
 * TypeScript) or joined it to a reply returned as a string (Python).
 */
export function addedText(text: string, own: string[]): string {
  for (const o of own) if (o && text.startsWith(o)) return text.slice(o.length).trim();
  return text.trim();
}

function textOf(content: unknown): string {
  if (!Array.isArray(content)) return "";
  return content
    .map((c) => (c && typeof c === "object" && (c as { type?: unknown }).type === "text" ? String((c as { text?: unknown }).text ?? "") : ""))
    .filter((t) => t !== "")
    .join("\n");
}

/** Recognized as server.ts records it: nulls, not empty strings. */
export function recognized(a: PortNote["agent"]): { harness: string | null; via: string | null; claimed: string | null } | undefined {
  if (!a) return undefined;
  const v = (x: unknown) => (typeof x === "string" && x !== "" ? x : null);
  return { harness: v(a.harness), via: v(a.via), claimed: v(a.claimed) };
}

interface Pending {
  tool: string;
  args: Record<string, unknown>;
  client: string;
}

/** Hop-by-hop and other headers the front doesn't pass on. */
const DROP_REQUEST = new Set(["host", "connection", "content-length", "transfer-encoding", "authorization", "keep-alive", "upgrade", "x-wakebench-call"]);
const DROP_RESPONSE = new Set(["content-length", "transfer-encoding", "connection", "content-encoding", "keep-alive"]);

export function front(o: FrontOptions): Server {
  const now = o.now ?? Date.now;
  const iso = () => new Date(now()).toISOString();
  const calls: CallRecord[] = [];
  const events: EventRecord[] = [];
  const seen: SeenRecord[] = [];
  const subs = new Map<string, Set<string>>();
  const counter = new Map<string, number>();
  const nonce = new Map<string, string>();
  // The client's name: by MCP session (initialize's), else the lane's last
  // initialize (a stateless server has no session), else the request's _meta.
  const sessionClient = new Map<string, string>();
  const laneClient = new Map<string, string>();
  const topic = (lane: string) => `bench/${o.run}/${lane}`;
  const control = (path: string, init?: RequestInit) =>
    fetch(o.port + path, { ...init, headers: { authorization: `Bearer ${o.controlToken}`, "content-type": "application/json", ...(init?.headers ?? {}) } });

  async function noteOf(call: string): Promise<PortNote | null> {
    try {
      const r = await control(`/bench/note?call=${encodeURIComponent(call)}`);
      if (!r.ok) return null;
      return ((await r.json()) as { note: PortNote | null }).note;
    } catch {
      return null;
    }
  }

  // finish records one response to a tools/call of the app's tools, from
  // the result the client receives.
  async function finish(lane: string, call: string, pending: Map<string | number, Pending>, m: unknown): Promise<void> {
    if (!m || typeof m !== "object") return;
    const msg = m as { id?: string | number; result?: { content?: unknown; isError?: boolean; _meta?: Record<string, unknown> } };
    if (msg.id === undefined || !pending.has(msg.id)) return;
    const p = pending.get(msg.id)!;
    pending.delete(msg.id);
    if (!msg.result) return;
    const note = await noteOf(call);
    if (!note || note.tool !== p.tool) return;
    const result = msg.result;
    const text = textOf(result.content);
    const added = result.isError ? "" : addedText(text, ownTexts(o.contract, p.tool, p.args));
    const meta = (result._meta ?? {}) as Record<string, unknown>;
    const agent = recognized(note.agent);
    calls.push({
      at: iso(),
      tool: p.tool,
      lane,
      client: p.client,
      args: {
        harness: typeof p.args.harness === "string" ? p.args.harness : null,
        conversation_id: typeof p.args.conversation_id === "string" ? p.args.conversation_id : null,
        session_key: typeof p.args.session_key === "string" && p.args.session_key ? short(p.args.session_key) : null,
        ...(Array.isArray(p.args.ids) ? { ids: p.args.ids.map(String) } : {}),
      },
      events_in_result: [...new Set(text.match(CODE) ?? [])],
      beacon: Object.keys(meta).some((k) => k.startsWith("wakeshift")) || /wakeshift-beacon:/.test(text),
      ...(agent ? { agent } : {}),
      ...(note.subscription ? { subscription: note.subscription } : {}),
      ...(p.tool === "watch" ? { session: note.session || undefined } : {}),
      added: redactPlain(added),
    });
    if (p.tool === "watch" && note.subscription) {
      if (!subs.has(lane)) subs.set(lane, new Set());
      subs.get(lane)!.add(note.subscription);
    }
    if (p.tool === "report_seen") seen.push({ at: iso(), lane, ids: Array.isArray(p.args.ids) ? p.args.ids.map(String) : [] });
  }

  async function mcp(lane: string, req: IncomingMessage, res: ServerResponse) {
    const raw = req.method === "POST" ? await readRaw(req) : undefined;
    let body: unknown;
    try {
      body = raw && raw.length ? JSON.parse(raw.toString("utf8")) : undefined;
    } catch {
      body = undefined;
    }
    const sid = typeof req.headers["mcp-session-id"] === "string" ? req.headers["mcp-session-id"] : "";
    const pending = new Map<string | number, Pending>();
    let initClient = "";
    for (const x of Array.isArray(body) ? body : [body]) {
      const r = x as { method?: string; id?: string | number; params?: { name?: unknown; arguments?: unknown; clientInfo?: { name?: unknown }; _meta?: Record<string, unknown> } } | undefined;
      if (!r || typeof r !== "object") continue;
      if (r.method === "initialize" && typeof r.params?.clientInfo?.name === "string") initClient = r.params.clientInfo.name;
      if (r.method !== "tools/call" || r.id === undefined || typeof r.params?.name !== "string") continue;
      if (!(APP_TOOLS as readonly string[]).includes(r.params.name)) continue;
      const args = r.params.arguments && typeof r.params.arguments === "object" && !Array.isArray(r.params.arguments) ? (r.params.arguments as Record<string, unknown>) : {};
      const info = r.params._meta?.["io.modelcontextprotocol/clientInfo"] as { name?: unknown } | undefined;
      const client = (typeof info?.name === "string" && info.name) || (sid && sessionClient.get(sid)) || laneClient.get(lane) || "";
      pending.set(r.id, { tool: r.params.name, args, client });
    }
    if (initClient) laneClient.set(lane, initClient);

    const call = randomUUID();
    const headers: Record<string, string> = {};
    for (const [k, v] of Object.entries(req.headers)) {
      if (DROP_REQUEST.has(k) || v === undefined) continue;
      headers[k] = Array.isArray(v) ? v.join(", ") : v;
    }
    headers.authorization = `Bearer ${lane}`;
    headers["x-wakebench-call"] = call;
    const ac = new AbortController();
    res.on("close", () => ac.abort());
    // How long the port takes over each of the app's tool calls (names and times only).
    if (pending.size) {
      const t0 = now();
      const tools = [...pending.values()].map((p) => p.tool).join(",");
      res.on("finish", () => o.log?.(`${iso()} ${lane} ${tools} ${now() - t0}ms`));
    }
    let up: Response;
    try {
      up = await fetch(`${o.port}/mcp`, { method: req.method, headers, body: raw && raw.length ? new Uint8Array(raw) : undefined, signal: ac.signal });
    } catch (err) {
      if (!res.headersSent) json(res, 502, { jsonrpc: "2.0", error: { code: -32603, message: `the toy app's port: ${redactPlain(String(err))}` }, id: null });
      return;
    }
    const newSid = up.headers.get("mcp-session-id");
    if (newSid && initClient) sessionClient.set(newSid, initClient);
    if (req.method === "DELETE" && sid) sessionClient.delete(sid);
    const out: Record<string, string> = {};
    up.headers.forEach((v, k) => {
      if (!DROP_RESPONSE.has(k)) out[k] = v;
    });
    const type = up.headers.get("content-type") ?? "";
    if (pending.size && type.includes("application/json")) {
      const text = await up.text();
      try {
        const j = JSON.parse(text);
        for (const m of Array.isArray(j) ? j : [j]) await finish(lane, call, pending, m);
      } catch {
        // Not JSON after all: passed on as it came.
      }
      res.writeHead(up.status, out);
      res.end(text);
      return;
    }
    res.writeHead(up.status, out);
    if (!up.body) return void res.end();
    if (pending.size && type.includes("text/event-stream")) {
      // Event by event, as each arrives: a response is recorded before the
      // event that carries it goes on, and nothing is held back.
      const sse = new SSELines(async (event) => {
        const data = event.filter((l) => l.startsWith("data:")).map((l) => l.slice(5).replace(/^ /, ""));
        if (data.length) {
          try {
            await finish(lane, call, pending, JSON.parse(data.join("\n")));
          } catch {
            // Not JSON: passed on as it came.
          }
        }
        res.write(event.join("\n") + "\n\n");
      });
      try {
        for await (const chunk of up.body) await sse.push(Buffer.from(chunk as Uint8Array).toString("utf8"));
        await sse.end((rest) => res.write(rest));
      } catch {
        // The client or the port went away.
      }
      return void res.end();
    }
    try {
      for await (const chunk of up.body) res.write(chunk as Uint8Array);
    } catch {
      // The client or the port went away.
    }
    res.end();
  }

  async function controlApi(url: URL, req: IncomingMessage, res: ServerResponse) {
    if (!authorized(req, o.controlToken)) return json(res, 401, { error: { code: "unauthenticated", message: "control token" } });
    const lane = url.searchParams.get("lane") ?? "";
    if (req.method === "POST" && url.pathname === "/control/publish") {
      const b = (await readJson(req)) as { lane?: string; count?: number; tag?: string };
      if (!b?.lane || !/^[a-z0-9-]+$/.test(b.lane)) return json(res, 400, { error: { code: "bad_request", message: "lane" } });
      const n = b.count === undefined ? 1 : b.count;
      if (typeof n !== "number" || !Number.isInteger(n) || n < 1 || n > 20) return json(res, 400, { error: { code: "bad_request", message: "count is a whole number from 1 to 20" } });
      let nn = nonce.get(b.lane);
      if (!nn) nonce.set(b.lane, (nn = codeNonce(o.run, b.lane, o.slot ?? 1)));
      const done: EventRecord[] = [];
      for (let i = 0; i < n; i++) {
        const seq = (counter.get(b.lane) ?? 0) + 1;
        counter.set(b.lane, seq);
        const code = `WB-${nn}-${seq}`;
        const at = iso();
        const id = `${o.run}:${b.lane}:${seq}`;
        const r = await control("/bench/emit", { method: "POST", body: JSON.stringify({ lane: b.lane, id, payload: { code, seq, sent_at: at, ...(b.tag ? { tag: b.tag } : {}) } }) });
        const j = (await r.json()) as { matched?: number; error?: { message?: string } };
        if (!r.ok) return json(res, 502, { error: { code: "emit", message: redactPlain(j.error?.message ?? `HTTP ${r.status}`) } });
        const e = { id, code, lane: b.lane, at, accepted_at: iso(), matched: j.matched ?? 0 };
        events.push(e);
        done.push(e);
      }
      return json(res, 200, { events: done });
    }
    if (req.method === "GET" && url.pathname === "/control/lane") {
      let deliveries: DeliveryRow[] | null = null;
      if (o.deliveries) {
        try {
          deliveries = await o.deliveries(o.appId, topic(lane));
        } catch {
          deliveries = null;
        }
      }
      return json(res, 200, {
        lane,
        topic: topic(lane),
        calls: calls.filter((c) => c.lane === lane),
        events: events.filter((e) => e.lane === lane),
        seen: seen.filter((s) => s.lane === lane),
        subscriptions: [...(subs.get(lane) ?? [])],
        deliveries,
      });
    }
    if (req.method === "GET" && url.pathname === "/control/status") {
      const r = await control("/bench/status", { method: "POST", body: JSON.stringify({ lane, subscriptions: [...(subs.get(lane) ?? [])] }) });
      const j = (await r.json()) as { event?: { subscriptions: number; connected: number; via?: Record<string, number> | null }; subscriptions?: unknown[]; error?: { message?: string } };
      if (!r.ok || !j.event) return json(res, 502, { error: { code: "status", message: redactPlain(j.error?.message ?? `HTTP ${r.status}`) } });
      return json(res, 200, { event: { ...j.event, via: j.event.via ?? {} }, subscriptions: j.subscriptions ?? [] });
    }
    return json(res, 404, { error: { code: "not_found", message: "no such control call" } });
  }

  return createServer((req, res) => {
    const url = new URL(req.url ?? "/", "http://bench");
    const m = /^\/mcp\/([a-z0-9-]{1,40})$/.exec(url.pathname);
    const work = m ? mcp(m[1], req, res) : url.pathname.startsWith("/control/") ? controlApi(url, req, res) : Promise.resolve(json(res, url.pathname === "/health" ? 200 : 404, { ok: url.pathname === "/health" }));
    work.catch((err) => {
      o.log?.(`front: ${redactPlain(String(err))}`);
      if (!res.headersSent) json(res, 500, { error: { code: "internal", message: redactPlain(String(err)) } });
      else res.end();
    });
  });
}

/**
 * SSE split into events as text streams in (an event's lines, without the
 * blank line that ends it), each handed on in order.
 */
export class SSELines {
  private buf = "";
  private lines: string[] = [];
  constructor(private onEvent: (lines: string[]) => Promise<void>) {}
  async push(text: string): Promise<void> {
    this.buf += text;
    for (;;) {
      const i = this.buf.indexOf("\n");
      if (i < 0) return;
      const line = this.buf.slice(0, i).replace(/\r$/, "");
      this.buf = this.buf.slice(i + 1);
      if (line === "") {
        if (this.lines.length) {
          const ev = this.lines;
          this.lines = [];
          await this.onEvent(ev);
        }
      } else this.lines.push(line);
    }
  }
  /** Whatever is left when the stream ends, passed on as it came. */
  async end(write: (rest: string) => void): Promise<void> {
    const rest = [...this.lines, ...(this.buf ? [this.buf] : [])];
    this.lines = [];
    this.buf = "";
    if (rest.length) write(rest.join("\n"));
  }
}

async function readRaw(req: IncomingMessage): Promise<Buffer> {
  const chunks: Buffer[] = [];
  let size = 0;
  for await (const c of req) {
    size += (c as Buffer).length;
    if (size > 4 << 20) throw new Error("body too large");
    chunks.push(c as Buffer);
  }
  return Buffer.concat(chunks);
}

function authorized(req: IncomingMessage, token: string): boolean {
  const got = String(req.headers.authorization ?? "");
  const want = `Bearer ${token}`;
  if (got.length !== want.length) return false;
  let diff = 0;
  for (let i = 0; i < got.length; i++) diff |= got.charCodeAt(i) ^ want.charCodeAt(i);
  return diff === 0;
}

function json(res: ServerResponse, status: number, body: unknown) {
  res.writeHead(status, { "content-type": "application/json" });
  res.end(JSON.stringify(body));
}

/** Starts a port (its command from BENCH_PORT_CMD) and waits until it answers /health. */
export async function startPort(o: {
  cmd: { argv: string[]; cwd?: string };
  env: Record<string, string>;
  port: number;
  timeoutMs: number;
  log: (line: string) => void;
}): Promise<ChildProcess> {
  const child = spawn(o.cmd.argv[0], o.cmd.argv.slice(1), { cwd: o.cmd.cwd, env: { ...o.env, PORT: String(o.port) }, stdio: ["pipe", "pipe", "pipe"] });
  child.stdout!.on("data", (b: Buffer) => o.log(`port: ${redactPlain(b.toString("utf8").trimEnd())}`));
  child.stderr!.on("data", (b: Buffer) => o.log(`port: ${redactPlain(b.toString("utf8").trimEnd())}`));
  let exited: number | string | null = null;
  child.on("exit", (code, sig) => (exited = code ?? sig ?? "exited"));
  const deadline = Date.now() + o.timeoutMs;
  for (;;) {
    if (exited !== null) throw new Error(`the port exited (${exited}) before it answered`);
    try {
      const r = await fetch(`http://127.0.0.1:${o.port}/health`, { signal: AbortSignal.timeout(2000) });
      if (r.ok) return child;
    } catch {
      // not up yet
    }
    if (Date.now() > deadline) {
      child.kill("SIGTERM");
      throw new Error(`the port didn't answer within ${Math.round(o.timeoutMs / 1000)}s`);
    }
    await new Promise((r) => setTimeout(r, 200));
  }
}

/** contract.json, next to this file's source. */
export function loadContract(path: string): Contract {
  return JSON.parse(readFileSync(path, "utf8")) as Contract;
}
