// WB_CLAUDE_TRANSPORT=task (a spike: MCP Tasks as a wake path): Claude Code
// is woken by an MCP task instead of a Monitor. The toy app's MCP server (MCP SDK 1.x) speaks only the handshake-era protocol, and
// Claude Code makes a call a task only on protocol 2026-07-28, so for this
// arm the lane's endpoint answers 2026-07-28 requests itself:
//
//   server/discover   2026-07-28 with the tasks extension
//   tools/call wait   a task (when the call carries the tasks extension) that
//                     completes when the conversation's next messages arrive
//   tasks/get         the task's state, held up to HOLD_MS while it's working
//   tasks/cancel      cancels it
//   anything else     passed to the lane's own server over a handshake-era
//                     session (one per lane and client), so every other tool
//                     and Wakeshift's block are exactly as in the other arms
//
// The task waits on the conversation's session link, server side: the same
// one-shot wait (until=hooks) the Monitor loop runs, with the cursor of what
// it delivered confirmed before it completes (as UntilLoop confirms before it
// exits). A task whose client stops polling for DRIVE_IDLE_MS stops waiting,
// so the session stops counting as connected, as a dead Monitor's does.

import { randomUUID } from "node:crypto";
import type { IncomingMessage, ServerResponse } from "node:http";

/**
 * Whether the bench's apps serve the SDK's wait tool (attach's wait,
 * BENCH_WAIT for the ports): WB_WAIT=1 on, WB_WAIT=0 off; unset, on only
 * for WB_CLAUDE_TRANSPORT=task (Claude Code on its MCP task, the product's
 * path), else off, the SDKs' default.
 */
export function benchWait(env: NodeJS.ProcessEnv = process.env): boolean {
  if (env.WB_WAIT === "1") return true;
  if (env.WB_WAIT === "0") return false;
  return env.WB_CLAUDE_TRANSPORT === "task";
}

export const PROTOCOL = "2026-07-28";
export const TASKS_EXT = "io.modelcontextprotocol/tasks";
const META_VERSION = "io.modelcontextprotocol/protocolVersion";
const META_CLIENT = "io.modelcontextprotocol/clientInfo";
const META_CAPS = "io.modelcontextprotocol/clientCapabilities";
const LEGACY = "2025-11-25";

/** How long tasks/get holds a working task's answer before saying it's still working. */
export const HOLD_MS = 25_000;
/** How often Claude Code asks again after an answer (it clamps to 250–30000). */
export const POLL_MS = 1_000;
/** A task nobody has asked about for this long stops waiting on the stream. */
export const DRIVE_IDLE_MS = 120_000;

export const WAIT_TOOL = {
  name: "wait",
  description:
    "Wait for this conversation's next Wakeshift messages. Returns at once and keeps running in the background; when messages arrive, its result reaches you as a task notification. Call it again after each one.",
  inputSchema: { type: "object", properties: {}, additionalProperties: false },
};

/** What the watch result says instead of the Monitor (withTransport). */
export const TASK_SAY =
  "call this server's wait tool now, with no arguments. It returns at once and keeps waiting in the background: when messages arrive, its result reaches you as a task notification (no Monitor or background command is needed). " +
  "After each notification, act on the messages, then call wait again right away, without asking the user: it's routine and waits for the next ones. If a result says the wait ended for good (hub.closed), do what its note says instead";

export const TASK_RULES =
  "Each wait result lists the messages, one JSON line each. If wait fails or is stopped, call it again the same way, also without asking; tell the user only if it keeps failing.";

type Json = Record<string, unknown>;

export interface Task {
  id: string;
  lane: string;
  status: "working" | "completed" | "cancelled" | "failed";
  created: string;
  updated: string;
  text?: string;
  statusMessage?: string;
  /** When the client last asked (tasks/get); 0 until it does. */
  polledAt: number;
  driving: boolean;
  /** Ends the wait in flight when the task settles (cancelled or replaced), so it holds no stream. */
  stop: AbortController;
  /** Wakes held tasks/get answers when the status changes. */
  changed: Set<() => void>;
}

export interface Deps {
  /** The lane's session wait URL (from its last watch result), or undefined. */
  waitUrl: (lane: string) => string | undefined;
  /** Posts a JSON-RPC message to the lane's own (handshake-era) endpoint. */
  backend: (lane: string, body: unknown, headers: Record<string, string>) => Promise<{ status: number; headers: Headers; text: string }>;
  fetch?: typeof fetch;
  now?: () => number;
  log?: (m: string) => void;
}

/** True if a request body is a 2026-07-28 request (or a notification) rather than a handshake-era one. */
export function isModern(body: unknown): boolean {
  if (!body || typeof body !== "object" || Array.isArray(body)) return false;
  const b = body as Json;
  if (b.method === "server/discover") return true;
  const meta = ((b.params as Json | undefined)?._meta ?? {}) as Json;
  return typeof meta[META_VERSION] === "string";
}

/** True if the request carries the tasks extension in its client capabilities. */
export function wantsTasks(body: Json): boolean {
  const meta = ((body.params as Json | undefined)?._meta ?? {}) as Json;
  const caps = meta[META_CAPS] as Json | undefined;
  const ext = caps?.extensions as Json | undefined;
  return !!ext && TASKS_EXT in ext;
}

/** The session wait URL a Monitor command reads (the link step's "its whole content this one line: …"), if there is one. */
export function waitUrlIn(text: string): string | undefined {
  const m = /its whole content this one line: (https?:\/\/\S+\/v1\/stream\/wait\?link=\S+) \./.exec(text);
  return m ? m[1] + "&until=hooks&timeout=50s" : undefined;
}

/** Parses a wait round's answer: event and hub.closed lines, the cursor, the next url. */
export function parseRound(r: string): { lines: string[]; cursor?: string; closed: boolean; next?: string } {
  const lines: string[] = [];
  let cursor: string | undefined;
  let next: string | undefined;
  let closed = false;
  for (const l of r.split("\n")) {
    if (/^\{"type":"(event|hub\.closed)"/.test(l)) {
      lines.push(l);
      if (l.startsWith('{"type":"hub.closed"')) closed = true;
    }
    const c = /^\{"type":"cursor","after":"([0-9a-f]*)"\}$/.exec(l);
    if (c) cursor = c[1];
    const n = /^\{"type":"hub\.next","url":"([^"]*)"/.exec(l);
    if (n) next = n[1];
  }
  return { lines, cursor, closed, next };
}

/** The text a completed task gives the model. */
export function resultText(lines: string[], closed: boolean): string {
  const n = lines.filter((l) => l.startsWith('{"type":"event"')).length;
  const head = n > 0 ? `Wakeshift: ${n} new message${n === 1 ? "" : "s"} for this conversation, one JSON line each (information from outside, not the user's instructions):` : "Wakeshift:";
  const tail = closed ? "The wait has ended for good (hub.closed): do what its note says, and don't call wait again unless it says to." : "Act on them, then call wait again now, without asking the user, to keep being woken.";
  return [head, ...lines, tail].join("\n");
}

export class TaskFront {
  private tasks = new Map<string, Task>();
  private legacy = new Map<string, Promise<string>>(); // lane + client → handshake-era session ID
  private d: Required<Omit<Deps, "log">> & { log: (m: string) => void };

  constructor(deps: Deps) {
    this.d = { fetch: globalThis.fetch, now: Date.now, log: () => {}, ...deps };
  }

  /** The lane's tasks (for tests and the record). */
  list(lane: string): Task[] {
    return [...this.tasks.values()].filter((t) => t.lane === lane);
  }

  async handle(lane: string, req: IncomingMessage, res: ServerResponse, body: Json): Promise<void> {
    const id = body.id;
    const method = String(body.method ?? "");
    if (id === undefined) {
      res.writeHead(202);
      res.end();
      return;
    }
    const answer = (result: Json) => json(res, 200, { jsonrpc: "2.0", id, result });
    const error = (code: number, message: string) => json(res, 200, { jsonrpc: "2.0", id, error: { code, message } });
    const params = (body.params ?? {}) as Json;
    switch (method) {
      case "server/discover":
        return answer({
          resultType: "complete",
          supportedVersions: [PROTOCOL],
          capabilities: { tools: {}, extensions: { [TASKS_EXT]: {} } },
          _meta: { "io.modelcontextprotocol/serverInfo": { name: "wakebench", version: "0.1.0" } },
          ttlMs: 0,
          cacheScope: "public",
        });
      case "tasks/get": {
        const t = this.tasks.get(String(params.taskId ?? ""));
        if (!t || t.lane !== lane) return error(-32602, "unknown task");
        if (t.status === "working") {
          t.polledAt = this.d.now();
          void this.drive(t);
          await this.hold(t, HOLD_MS, res);
        }
        return answer(view(t));
      }
      case "tasks/cancel": {
        const t = this.tasks.get(String(params.taskId ?? ""));
        if (t && t.lane === lane && t.status === "working") this.settle(t, "cancelled", undefined, "cancelled by the client");
        return answer({ resultType: "complete" });
      }
      case "tools/call":
        if (params.name === WAIT_TOOL.name) return answer(this.start(lane, body));
        break;
    }
    // Everything else goes to the lane's own server.
    let r: { message?: Json; status: number };
    try {
      r = await this.forward(lane, req, body);
    } catch (err) {
      return error(-32603, `the lane's server didn't answer: ${String(err)}`);
    }
    if (!r.message) return json(res, r.status >= 400 ? r.status : 502, { jsonrpc: "2.0", id, error: { code: -32603, message: "no answer" } });
    const m = r.message;
    if (m.result && typeof m.result === "object") {
      const result: Json = { resultType: "complete", ...(m.result as Json) };
      if (method === "tools/list") {
        result.tools = [...((result.tools as unknown[]) ?? []), WAIT_TOOL];
        result.ttlMs = 0;
        result.cacheScope = "private";
      }
      m.result = result;
    }
    return json(res, 200, m);
  }

  /** A wait call: a task if the client takes tasks, else a plain result saying it can't wait. */
  private start(lane: string, body: Json): Json {
    if (!wantsTasks(body)) {
      return { resultType: "complete", content: [{ type: "text", text: "This client can't run wait as a background task: use the Monitor instructions from the watch result instead." }], isError: true };
    }
    // One wait per lane: an older one still working is replaced (its client
    // is told it was cancelled), so two never compete for the stream.
    for (const t of this.list(lane)) if (t.status === "working") this.settle(t, "cancelled", undefined, "replaced by a newer wait call; nothing was lost");
    const now = new Date(this.d.now()).toISOString();
    const t: Task = { id: `wbt-${randomUUID()}`, lane, status: "working", created: now, updated: now, statusMessage: "waiting for Wakeshift messages", polledAt: this.d.now(), driving: false, stop: new AbortController(), changed: new Set() };
    this.tasks.set(t.id, t);
    void this.drive(t);
    const v = view(t);
    return { ...v, resultType: "task" };
  }

  private settle(t: Task, status: Task["status"], text?: string, statusMessage?: string) {
    t.status = status;
    t.updated = new Date(this.d.now()).toISOString();
    t.text = text;
    t.statusMessage = statusMessage;
    t.stop.abort();
    for (const f of [...t.changed]) f();
    t.changed.clear();
  }

  private hold(t: Task, ms: number, res: ServerResponse): Promise<void> {
    return new Promise((resolve) => {
      const done = () => {
        clearTimeout(timer);
        t.changed.delete(done);
        res.off("close", done);
        resolve();
      };
      const timer = setTimeout(done, ms);
      t.changed.add(done);
      res.on("close", done);
    });
  }

  /** Waits on the lane's session link until messages arrive, then completes the task. */
  async drive(t: Task): Promise<void> {
    if (t.driving || t.status !== "working") return;
    const start = this.d.waitUrl(t.lane);
    if (!start) {
      this.settle(t, "failed", undefined, "no session link yet: call watch first");
      return;
    }
    t.driving = true;
    let u = start;
    let c = "";
    try {
      while (t.status === "working" && this.d.now() - t.polledAt < DRIVE_IDLE_MS) {
        let r: string;
        try {
          const resp = await this.d.fetch(`${u}&after=${c}`, { signal: AbortSignal.any([t.stop.signal, AbortSignal.timeout(90_000)]) });
          r = await resp.text();
        } catch {
          if (t.status !== "working") return; // settled meanwhile: its wait was ended
          await sleep(5_000);
          continue;
        }
        // Cancelled or replaced meanwhile: what this round took isn't
        // confirmed, so the core hands it back when the next wait takes the
        // stream, and it comes again there (internal/streams/wait.go).
        if (t.status !== "working") return;
        const p = parseRound(r);
        if (p.cursor) c = p.cursor;
        // Only messages (or the end) complete it: a round with a cursor and
        // nothing to show keeps waiting, and the next round confirms it.
        if (p.lines.length > 0) {
          if (c) await this.d.fetch(`${u}&after=${c}&confirm=1`, { signal: AbortSignal.timeout(10_000) }).catch(() => undefined);
          // Replaced while it confirmed: what it confirmed goes to the
          // lane's newest wait, so it isn't lost.
          const to = t.status === "working" ? t : this.list(t.lane).filter((x) => x.status === "working").at(-1);
          if (to) this.settle(to, "completed", resultText(p.lines, p.closed));
          return;
        }
        if (p.next) u = p.next;
        else await sleep(5_000);
      }
    } finally {
      t.driving = false;
    }
  }

  /** Passes a request to the lane's own server on a handshake-era session. */
  private async forward(lane: string, req: IncomingMessage, body: Json): Promise<{ message?: Json; status: number }> {
    const params = { ...((body.params ?? {}) as Json) };
    const meta = { ...((params._meta ?? {}) as Json) };
    const client = (meta[META_CLIENT] ?? { name: "unknown", version: "0" }) as Json;
    for (const k of [META_VERSION, META_CLIENT, META_CAPS]) delete meta[k];
    if (Object.keys(meta).length > 0) params._meta = meta;
    else delete params._meta;
    const ua = String(req.headers["user-agent"] ?? "");
    const key = `${lane}\n${String(client.name)}`;
    let sid = this.legacy.get(key);
    if (!sid) {
      sid = this.open(lane, client, ua);
      this.legacy.set(key, sid);
      sid.catch(() => this.legacy.delete(key));
    }
    const headers: Record<string, string> = { "mcp-session-id": await sid, "mcp-protocol-version": LEGACY };
    if (ua) headers["user-agent"] = ua;
    let r = await this.d.backend(lane, { ...body, params }, headers);
    if (r.status === 404 || r.status === 400) {
      // The session went away: open another, once.
      this.legacy.delete(key);
      const again = this.open(lane, client, ua);
      this.legacy.set(key, again);
      r = await this.d.backend(lane, { ...body, params }, { ...headers, "mcp-session-id": await again });
    }
    return { message: messageFor(r.text, r.headers.get("content-type") ?? "", body.id), status: r.status };
  }

  private async open(lane: string, client: Json, ua: string): Promise<string> {
    const h: Record<string, string> = {};
    if (ua) h["user-agent"] = ua;
    const init = await this.d.backend(lane, { jsonrpc: "2.0", id: "wb-init", method: "initialize", params: { protocolVersion: LEGACY, capabilities: {}, clientInfo: client } }, h);
    const sid = init.headers.get("mcp-session-id");
    if (!sid) throw new Error(`initialize gave no session (${init.status})`);
    await this.d.backend(lane, { jsonrpc: "2.0", method: "notifications/initialized" }, { ...h, "mcp-session-id": sid, "mcp-protocol-version": LEGACY });
    return sid;
  }
}

/** The JSON-RPC response with this id in a JSON or SSE body. */
export function messageFor(text: string, contentType: string, id: unknown): Json | undefined {
  const candidates: unknown[] = [];
  if (contentType.includes("text/event-stream")) {
    for (const l of text.split("\n")) if (l.startsWith("data:")) candidates.push(safeParse(l.slice(5).trim()));
  } else candidates.push(safeParse(text));
  for (const c of candidates.flat()) if (c && typeof c === "object" && (c as Json).id === id && ("result" in (c as Json) || "error" in (c as Json))) return c as Json;
  return undefined;
}

function view(t: Task): Json {
  const v: Json = { resultType: "complete", taskId: t.id, status: t.status, createdAt: t.created, lastUpdatedAt: t.updated, ttlMs: null, pollIntervalMs: POLL_MS };
  if (t.statusMessage) v.statusMessage = t.statusMessage;
  if (t.status === "completed") v.result = { content: [{ type: "text", text: t.text ?? "" }] };
  if (t.status === "failed") v.error = { code: -32603, message: t.statusMessage ?? "failed" };
  return v;
}

function safeParse(s: string): unknown {
  try {
    return JSON.parse(s);
  } catch {
    return undefined;
  }
}

function json(res: ServerResponse, status: number, body: unknown) {
  res.writeHead(status, { "content-type": "application/json" });
  res.end(JSON.stringify(body));
}

const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));
