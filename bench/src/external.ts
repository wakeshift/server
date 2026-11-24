// An external MCP server in place of the toy app (WB_EXTERNAL_APP): a real
// vendor's server, built on the SDK, with its vendor's API mocked. The
// bench runs it through an adapter, an executable that knows the vendor:
//
//   <adapter> describe   prints one JSON line (ExternalDescription): the
//                        server's name, the app's name, which tools start a
//                        job, check one or subscribe, and the user's words
//                        for each scenario
//   <adapter> serve      starts the vendor's mock and its server with the
//                        app's key (from WB_KEY_FILE, mode 0600) and the
//                        core (WAKESHIFT_CORE), and answers on 127.0.0.1:
//                        WB_ADAPTER_PORT:
//                          GET  /health
//                          GET  /info   {mcp, headers?, pids?}
//                          POST /fire   {lane} → {code}: the newest job the
//                                       server started and that hasn't
//                                       finished finishes, through the
//                                       mock's own trigger; code is the ID
//                                       the agent reads in the event
//
// The bench stands a recording proxy in front of the server's MCP URL
// (/mcp/<lane>, adding the adapter's headers, e.g. the vendor's own API key
// for the mock), so each tool call is recorded from the result the client
// receives, as the toy app records its own: what Wakeshift added (its text
// block, the beacon), with session keys only as hashes. A post (publish) is
// a fire; the agent shows it saw the event with its GOT <code> line, since
// there's no report_seen tool. Presence and delivery stages are read from
// the local core's database (local cores only).

import { createHash } from "node:crypto";
import { execFile } from "node:child_process";
import { appendFileSync, createWriteStream } from "node:fs";
import { createServer, type IncomingMessage, type Server, type ServerResponse } from "node:http";
import { join } from "node:path";
import { promisify } from "node:util";
import pg from "pg";
import type { Bench, LaneState, Published, StatusState, StartOptions } from "./core.js";
import { freePort, TRANSITIONS_SQL, traceOf, type DeliveryTrace } from "./core.js";
import { redactPlain } from "./redact.js";
import { sleep } from "./tmux.js";

const exec = promisify(execFile);

/** What `<adapter> describe` prints. */
export interface ExternalDescription {
  /** The MCP server's name in the harness, and the app's ID on the local core (lowercase, a-z 0-9 -). */
  name: string;
  /** The app's name, as a developer names it in the console (Wakeshift's text uses it). */
  title: string;
  /** Tools that start a job (a side effect: S17 counts them). */
  start_tools: string[];
  /** Tools that check on a job (polling, when the agent does it). */
  status_tools: string[];
  /** Tools that subscribe the conversation (Wakeshift's arguments on them). */
  subscribing: string[];
  /** What the user says, in the vendor's own terms. */
  prompts: {
    /** S6's first message: the mechanics tier's, for this server (start a job with a named tool, quote, follow, GOT <ID>). */
    intro: string;
    /** S2i, before each post: start another job ({n} is the post's number), GOT <ID> as before. */
    next: string;
    /** S18's first message, in a new conversation: start a job, nothing about installing. */
    s18: string;
    /** S17 and S17d: a natural ask ("Start a sync of example.com and let me know when it finishes."). */
    natural: string;
    /** S6's one nudge when the install doesn't connect (counted); none if absent. */
    nudge?: string;
  };
}

/** What `GET /info` answers once the adapter serves. */
export interface ExternalServe {
  mcp: string;
  headers?: Record<string, string>;
  /** The processes the adapter started (the vendor's server, its mock), so the bench can stop them too. */
  pids?: number[];
}

export function parseDescription(text: string): ExternalDescription {
  const line = text.trim().split("\n").reverse().find((x) => x.trim().startsWith("{"));
  if (!line) throw new Error("the adapter's describe printed no JSON");
  const d = JSON.parse(line) as ExternalDescription;
  if (!/^[a-z][a-z0-9-]{1,40}$/.test(d.name ?? "")) throw new Error(`the adapter's name must be lowercase a-z, 0-9 and -: ${JSON.stringify(d.name)}`);
  for (const k of ["intro", "next", "s18", "natural"] as const) if (typeof d.prompts?.[k] !== "string" || !d.prompts[k]) throw new Error(`the adapter's prompts need ${k}`);
  if (typeof d.title !== "string" || !d.title.trim()) throw new Error("the adapter's description needs a title");
  for (const k of ["start_tools", "status_tools", "subscribing"] as const) if (!Array.isArray(d[k]) || !d[k].every((t) => typeof t === "string" && t)) throw new Error(`the adapter's description needs ${k} (a list of tool names)`);
  return d;
}

/** The adapter's description, from `<adapter> describe` (no key, no network). */
export async function describeExternal(cmd: string): Promise<ExternalDescription> {
  const { stdout } = await exec(cmd, ["describe"], { timeout: 60_000, maxBuffer: 1 << 20, env: { PATH: process.env.PATH ?? "", HOME: process.env.HOME ?? "" } });
  return parseDescription(stdout);
}

const short = (s: string) => createHash("sha256").update(s).digest("hex").slice(0, 12);

/** The text of a result's text blocks. */
function textBlocks(content: unknown): string[] {
  if (!Array.isArray(content)) return [];
  return content.filter((c) => c && typeof c === "object" && (c as { type?: unknown }).type === "text").map((c) => String((c as { text?: unknown }).text ?? ""));
}

/** Wakeshift's beacon line, as the SDK ends a result with it for a hook. */
export const BEACON_LINE = /\(For Wakeshift's hook on this computer, not for you: wakeshift-beacon: /;
/** What marks Wakeshift's text in a result (the SDK's block names Wakeshift or the session key). */
const WAKESHIFT_MARK = /Wakeshift|wakeshift|session_key/;

/**
 * What Wakeshift added to a vendor's result, and what the result kept of
 * it. The SDK puts its text in a block of its own after the app's content
 * (or, for a result returned as a string, joins it after a blank line), a
 * structured result also carries it as `structuredContent.wakeshift`, and
 * the beacon goes last (a trailer line) or in `_meta`. The vendor's own
 * text isn't known here, so the block is found by those marks: the
 * structured field if there is one, else the text blocks after the first
 * that carry a mark, else the paragraphs of a single block from the first
 * that carries one.
 */
export function wakeshiftPart(result: { content?: unknown; structuredContent?: unknown; _meta?: Record<string, unknown> }): {
  added: string;
  beacon: boolean;
  /** Where the block was found: a block of its own, joined to the vendor's text, the structured field only, or nowhere. */
  shape: "own-block" | "joined" | "structured-only" | "none";
  /** The structured field and the text block differ (the vendor reshaped one of them). */
  mismatch: boolean;
  chars: number;
} {
  const texts = textBlocks(result.content);
  const sc = result.structuredContent && typeof result.structuredContent === "object" ? (result.structuredContent as Record<string, unknown>) : null;
  const field = sc && typeof sc.wakeshift === "string" ? sc.wakeshift : null;
  const meta = result._meta ?? {};
  const beacon = Object.keys(meta).some((k) => k.startsWith("wakeshift")) || texts.some((t) => BEACON_LINE.test(t));
  const chars = texts.reduce((n, t) => n + t.length, 0);
  const strip = (t: string) =>
    t
      .split("\n")
      .filter((x) => !BEACON_LINE.test(x))
      .join("\n")
      .trim();
  let added = "";
  let shape: "own-block" | "joined" | "structured-only" | "none" = "none";
  const own = texts.slice(1).map(strip).filter((t) => t && WAKESHIFT_MARK.test(t));
  if (own.length) {
    added = own.join("\n\n");
    shape = "own-block";
  } else if (texts.length) {
    const paras = strip(texts[0]).split(/\n\s*\n/);
    const i = paras.findIndex((p) => WAKESHIFT_MARK.test(p));
    if (i >= 0) {
      added = paras.slice(i).join("\n\n").trim();
      shape = "joined";
    }
  }
  if (!added && field) {
    added = field;
    shape = "structured-only";
  }
  const mismatch = field !== null && added !== "" && shape !== "structured-only" && !added.includes(field.trim());
  return { added, beacon, shape, mismatch, chars };
}

/** One recorded call, with what the result kept of Wakeshift's part. */
interface ExtCall {
  at: string;
  lane: string;
  tool: string;
  client: string;
  args: { harness: string | null; conversation_id: string | null; session_key: string | null };
  role: "start" | "status" | "other";
  subscribes: boolean;
  added: string;
  beacon: boolean;
  shape: string;
  mismatch: boolean;
  chars: number;
  is_error: boolean;
  /** The result's text, redacted, for finding fired codes in it. */
  text: string;
}

const DROP_REQUEST = new Set(["host", "connection", "content-length", "transfer-encoding", "keep-alive", "upgrade"]);
const DROP_RESPONSE = new Set(["content-length", "transfer-encoding", "connection", "keep-alive"]);

/** The JSON-RPC messages in a response body: JSON (one or a batch) or SSE data lines. */
export function messagesOf(body: string, contentType: string): unknown[] {
  if (/text\/event-stream/.test(contentType)) {
    const out: unknown[] = [];
    for (const ev of body.split(/\r?\n\r?\n/)) {
      const data = ev
        .split(/\r?\n/)
        .filter((l) => l.startsWith("data:"))
        .map((l) => l.slice(5).replace(/^ /, ""))
        .join("\n");
      if (!data) continue;
      try {
        out.push(JSON.parse(data));
      } catch {
        // Not JSON.
      }
    }
    return out;
  }
  try {
    const j = JSON.parse(body) as unknown;
    return Array.isArray(j) ? j : [j];
  } catch {
    return [];
  }
}

/** The recording proxy in front of the vendor's MCP URL. */
function proxy(o: { target: string; headers: Record<string, string>; d: ExternalDescription; calls: ExtCall[]; firstSeen: Map<string, number>; resultsFile: string; listed: Set<string> }): Server {
  const sessionClient = new Map<string, string>();
  const laneClient = new Map<string, string>();
  const target = new URL(o.target);
  return createServer(async (req: IncomingMessage, res: ServerResponse) => {
    const url = new URL(req.url ?? "/", "http://127.0.0.1");
    if (url.pathname === "/health") {
      res.writeHead(200).end("ok");
      return;
    }
    const m = /^\/mcp\/([a-z0-9-]+)$/.exec(url.pathname);
    if (!m) {
      res.writeHead(404).end();
      return;
    }
    const lane = m[1];
    if (!o.firstSeen.has(lane)) o.firstSeen.set(lane, Date.now());
    const chunks: Buffer[] = [];
    for await (const c of req) chunks.push(c as Buffer);
    const raw = Buffer.concat(chunks);
    let body: unknown;
    try {
      body = raw.length ? JSON.parse(raw.toString("utf8")) : undefined;
    } catch {
      body = undefined;
    }
    const sid = typeof req.headers["mcp-session-id"] === "string" ? req.headers["mcp-session-id"] : "";
    const pending = new Map<string | number, { tool: string; args: Record<string, unknown>; client: string }>();
    let listing = false;
    for (const x of Array.isArray(body) ? body : [body]) {
      const r = x as { method?: string; id?: string | number; params?: { name?: unknown; arguments?: unknown; clientInfo?: { name?: unknown }; _meta?: Record<string, unknown> } } | undefined;
      if (!r || typeof r !== "object") continue;
      if (r.method === "initialize" && typeof r.params?.clientInfo?.name === "string") laneClient.set(lane, r.params.clientInfo.name);
      if (r.method === "tools/list") listing = true;
      if (r.method !== "tools/call" || r.id === undefined || typeof r.params?.name !== "string") continue;
      const args = r.params.arguments && typeof r.params.arguments === "object" && !Array.isArray(r.params.arguments) ? (r.params.arguments as Record<string, unknown>) : {};
      const info = r.params._meta?.["io.modelcontextprotocol/clientInfo"] as { name?: unknown } | undefined;
      const client = (typeof info?.name === "string" && info.name) || (sid && sessionClient.get(sid)) || laneClient.get(lane) || "";
      pending.set(r.id, { tool: r.params.name, args, client });
    }
    const headers: Record<string, string> = {};
    for (const [k, v] of Object.entries(req.headers)) if (!DROP_REQUEST.has(k) && typeof v === "string") headers[k] = v;
    Object.assign(headers, o.headers);
    const up = new URL(target.toString());
    up.search = url.search;
    let r: Response;
    try {
      r = await fetch(up, { method: req.method, headers, body: raw.length ? raw : undefined, redirect: "manual" });
    } catch (err) {
      res.writeHead(502, { "content-type": "text/plain" }).end(`upstream: ${err instanceof Error ? err.message : String(err)}`);
      return;
    }
    const out: Record<string, string> = {};
    r.headers.forEach((v, k) => {
      if (!DROP_RESPONSE.has(k) && k !== "content-encoding") out[k] = v;
    });
    const newSid = r.headers.get("mcp-session-id");
    if (newSid && laneClient.has(lane)) sessionClient.set(newSid, laneClient.get(lane)!);
    res.writeHead(r.status, out);
    const ct = r.headers.get("content-type") ?? "";
    let text = "";
    if (r.body) {
      const reader = r.body.getReader();
      const dec = new TextDecoder();
      for (;;) {
        const { value, done } = await reader.read();
        if (done) break;
        res.write(value);
        if (pending.size || listing) text += dec.decode(value, { stream: true });
      }
    }
    res.end();
    if (!pending.size && !listing) return;
    // Each tools/list once per lane: every description's length, against Claude Code's 2,048-character cap.
    if (listing && !o.listed.has(lane)) {
      for (const msg of messagesOf(text, ct)) {
        const tools = (msg as { result?: { tools?: { name?: string; description?: string }[] } })?.result?.tools;
        if (!Array.isArray(tools)) continue;
        o.listed.add(lane);
        const lengths = Object.fromEntries(tools.map((t) => [String(t.name), (t.description ?? "").length]));
        appendFileSync(o.resultsFile, JSON.stringify({ at: new Date().toISOString(), lane, tools_list: lengths, over_2048: Object.entries(lengths).filter(([, n]) => n > 2048).map(([k]) => k) }) + "\n", { mode: 0o600 });
      }
    }
    if (!pending.size) return;
    for (const msg of messagesOf(text, ct)) {
      const mm = msg as { id?: string | number; result?: { content?: unknown; structuredContent?: unknown; isError?: boolean; _meta?: Record<string, unknown> } };
      if (mm?.id === undefined || !pending.has(mm.id) || !mm.result) continue;
      const p = pending.get(mm.id)!;
      const part = wakeshiftPart(mm.result);
      const role = o.d.start_tools.includes(p.tool) ? "start" : o.d.status_tools.includes(p.tool) ? "status" : "other";
      const all = textBlocks(mm.result.content).join("\n");
      const call: ExtCall = {
        at: new Date().toISOString(),
        lane,
        tool: p.tool,
        client: p.client,
        args: {
          harness: typeof p.args.harness === "string" ? p.args.harness : null,
          conversation_id: typeof p.args.conversation_id === "string" ? p.args.conversation_id : null,
          session_key: typeof p.args.session_key === "string" && p.args.session_key ? short(p.args.session_key) : null,
        },
        role,
        subscribes: o.d.subscribing.includes(p.tool),
        added: redactPlain(part.added),
        beacon: part.beacon,
        shape: part.shape,
        mismatch: part.mismatch,
        chars: part.chars,
        is_error: mm.result.isError === true,
        text: redactPlain(all).slice(0, 20_000),
      };
      o.calls.push(call);
      // Each result as the client got it (redacted), for reading what the vendor's server did to Wakeshift's part.
      appendFileSync(o.resultsFile, JSON.stringify({ at: call.at, lane, tool: p.tool, client: p.client, role, subscribes: call.subscribes, shape: part.shape, beacon: part.beacon, mismatch: part.mismatch, chars: part.chars, result: JSON.parse(redactPlain(JSON.stringify(mm.result))) }) + "\n", { mode: 0o600 });
    }
  });
}

async function waitHttp(url: string, ms: number, alive: () => boolean): Promise<void> {
  const end = Date.now() + ms;
  while (Date.now() < end) {
    if (!alive()) throw new Error(`exited before ${url} answered`);
    try {
      if ((await fetch(url)).ok) return;
    } catch {
      // Not yet.
    }
    await sleep(250);
  }
  throw new Error(`${url} didn't answer within ${ms / 1000}s`);
}

/** Where an external app's conversation is connected, from the local core's database (as the core's event status counts it). */
const PRESENCE_SQL = `
SELECT ts.subscription_id::text AS id, s.runtime, s.connector_seen_at IS NOT NULL AS connector_seen,
  coalesce((SELECT CASE WHEN l.renewed_at > now() - interval '1 minute' THEN CASE WHEN l.holder LIKE 'poll\\_%' THEN 'poll' ELSE 'stream' END END
      FROM stream_leases l WHERE l.tenant_id = s.tenant_id AND l.agent_id = s.agent_id
      AND l.target = CASE s.runtime WHEN 'connector' THEN '' ELSE s.id::text END LIMIT 1),
    (SELECT 'webhook' FROM mcp_event_webhooks w WHERE w.tenant_id = s.tenant_id AND w.session_id = s.id AND w.expires_at > now() AND w.ending_reason IS NULL LIMIT 1),
    (SELECT 'webhook' FROM agent_webhooks h WHERE h.tenant_id = s.tenant_id AND h.session_id = s.id AND h.state = 'live' LIMIT 1), '') AS connected_via
  FROM topic_subscriptions ts
  JOIN subscriptions sub ON sub.tenant_id = ts.tenant_id AND sub.id = ts.subscription_id
  JOIN sessions s ON s.tenant_id = ts.tenant_id AND s.id = ts.session_id
 WHERE ts.app_id = $1 AND ts.created_at >= $2 AND ts.ended_at IS NULL AND ts.expires_at > now() AND sub.status = 'active'
 ORDER BY ts.created_at`;

/** The core's name for how a session is connected (internal/appapi connectedVia). */
export function viaOf(row: { runtime: string; connector_seen: boolean; connected_via: string }): string | null {
  if (!row.connected_via) return null;
  if (row.runtime === "stream" && row.connector_seen) return "hooks";
  if (row.connected_via === "webhook") return "webhook";
  if (row.connected_via === "poll") return "poll";
  return "stream";
}

/**
 * Starts the adapter and the proxy in front of its server, on the local
 * core's app (keyFile), and returns the run's Bench.
 */
export async function startExternal(
  o: StartOptions,
  a: { cmd: string; d: ExternalDescription; core: string; keyFile: string; adminUrl: string; agentPlugins: string | null; antigravityPlugin: string | null; dropDb: () => Promise<void> },
): Promise<Bench> {
  const port = await freePort();
  const log = createWriteStream(join(o.dir, "external.log"));
  const child = o.procs.spawn(
    a.cmd,
    ["serve"],
    {
      env: { PATH: process.env.PATH ?? "", HOME: process.env.HOME ?? "", WAKESHIFT_CORE: a.core, WB_KEY_FILE: a.keyFile, WB_ADAPTER_PORT: String(port), WB_RUN: o.run, WB_RUN_DIR: o.dir },
      stdio: ["ignore", "pipe", "pipe"],
    },
    `external app (${a.d.name})`,
  );
  child.stdout!.pipe(log);
  child.stderr!.pipe(log);
  let alive = true;
  child.on("exit", () => (alive = false));
  const control = `http://127.0.0.1:${port}`;
  const pool = new pg.Pool({ connectionString: a.adminUrl, max: 3 });
  let srv: Server | null = null;
  const stop = async () => {
    srv?.close();
    srv?.closeAllConnections();
    await pool.end().catch(() => {});
    if (alive && child.pid) o.procs.kill(child.pid, "external app");
    await sleep(1500);
    await a.dropDb().catch(() => {});
  };
  try {
    await waitHttp(`${control}/health`, 180_000, () => alive);
    const info = (await (await fetch(`${control}/info`)).json()) as ExternalServe;
    for (const pid of info.pids ?? []) o.procs.record(pid, `external app's own (${a.d.name})`);
    await pool.query(TRANSITIONS_SQL);
    const calls: ExtCall[] = [];
    const firstSeen = new Map<string, number>();
    const fired: { lane: string; code: string; at: string }[] = [];
    const pport = await freePort();
    srv = proxy({ target: info.mcp, headers: info.headers ?? {}, d: a.d, calls, firstSeen, resultsFile: join(o.dir, "external-results.jsonl"), listed: new Set() });
    await new Promise<void>((r) => srv!.listen(pport, "127.0.0.1", () => r()));
    const base = `http://127.0.0.1:${pport}`;
    o.log(`external app ${a.d.title} (${a.d.name}) behind ${base}, its server at ${info.mcp}`);

    const publish = async (lane: string, count = 1): Promise<Published[]> => {
      const out: Published[] = [];
      for (let i = 0; i < count; i++) {
        // The id keeps the time before the fire, to find the first event the app published after it;
        // the job counts as ended once the fire answers, so status calls before then are before its end.
        const before = new Date().toISOString();
        const r = await fetch(`${control}/fire`, { method: "POST", headers: { "content-type": "application/json" }, body: JSON.stringify({ lane }) });
        const j = (await r.json().catch(() => ({}))) as { code?: string; error?: string };
        if (!r.ok || !j.code) throw new Error(`the external app couldn't fire a job's end: ${r.status} ${j.error ?? ""}`);
        const at = new Date().toISOString();
        fired.push({ lane, code: j.code, at });
        out.push({ id: `ext:${before}`, code: j.code, at, accepted_at: at, matched: 0 });
      }
      return out;
    };
    const lane = async (id: string): Promise<LaneState> => {
      const mine = calls.filter((c) => c.lane === id);
      const codes = fired.filter((f) => f.lane === id).map((f) => f.code);
      return {
        calls: mine.map((c) => ({
          at: c.at,
          tool: c.tool,
          client: c.client,
          args: c.args,
          added: c.added,
          beacon: c.beacon,
          role: c.role,
          subscribes: c.subscribes,
          shape: c.shape,
          mismatch: c.mismatch,
          chars: c.chars,
          is_error: c.is_error,
          events_in_result: codes.filter((code) => c.text.includes(code)),
        })),
        events: fired.filter((f) => f.lane === id).map((f) => ({ code: f.code, at: f.at, matched: 0 })),
        seen: [],
        subscriptions: [],
        deliveries: null,
        jobs: mine.filter((c) => c.role === "start" && !c.is_error).length,
      };
    };
    const status = async (id: string): Promise<StatusState> => {
      const since = new Date(firstSeen.get(id) ?? Date.now());
      const rows = (await pool.query(PRESENCE_SQL, [a.d.name, since])).rows as { id: string; runtime: string; connector_seen: boolean; connected_via: string }[];
      const subs = rows.map((r) => {
        const via = viaOf(r);
        return { id: r.id, live: true, connected: via !== null, ...(via ? { via } : {}) };
      });
      const via: Record<string, number> = {};
      for (const s of subs) if (s.via) via[s.via] = (via[s.via] ?? 0) + 1;
      return { event: { subscriptions: subs.length, connected: subs.filter((s) => s.connected).length, via }, subscriptions: subs };
    };
    // A fire's delivery stages: the first event the app published after it (the vendor's emit), and its delivery.
    const trace = async (eventId: string): Promise<DeliveryTrace | null> => {
      const at = eventId.startsWith("ext:") ? eventId.slice(4) : null;
      if (!at) return null;
      try {
        const ev = await pool.query(`SELECT event_id FROM topic_events WHERE app_id = $1 AND published_at >= $2 ORDER BY published_at LIMIT 1`, [a.d.name, at]);
        const evId = ev.rows[0]?.event_id as string | undefined;
        if (!evId) return null;
        const r = await pool.query(`SELECT id, created_at, available_at, state, attempts FROM deliveries WHERE event_id = $1 ORDER BY created_at LIMIT 1`, [evId]);
        const row = r.rows[0];
        if (!row) return null;
        const ch = await pool.query(`SELECT state, at FROM wakebench_delivery_states WHERE delivery_id = $1 ORDER BY at`, [row.id]);
        return traceOf(row, ch.rows);
      } catch {
        return null;
      }
    };
    return {
      target: o.target,
      core: a.core,
      agentPlugins: a.agentPlugins,
      antigravityPlugin: a.antigravityPlugin,
      appId: a.d.name,
      app: base,
      mcpUrl: (l) => `${base}/mcp/${l}`,
      mcpName: a.d.name,
      external: a.d,
      publish,
      lane,
      status,
      trace,
      eventTexts: async (codes) => {
        const out: string[] = [];
        for (const code of codes) {
          const r = await pool.query(`SELECT text FROM topic_events WHERE app_id = $1 AND strpos(text, $2) > 0 ORDER BY published_at DESC LIMIT 5`, [a.d.name, code]);
          for (const row of r.rows as { text: string }[]) out.push(row.text);
        }
        return out;
      },
      // S17's lane needs no options here: the vendor's tools are what they are.
      options: async () => {},
      stop,
    };
  } catch (err) {
    await stop();
    throw new Error(`external app (${a.d.name}): ${err instanceof Error ? err.message : String(err)} (see core/external.log)`);
  }
}
