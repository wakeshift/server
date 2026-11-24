// The toy app's conformance check (app/CONTRACT.md, "Conformance"): every
// variant (WB_APP) runs as the bench runs it, the toy app with its control
// API (a port behind front.ts), against a real core, and a scripted client
// checks it serves the contract and that the bench's recording works on it:
//
//   node dist/src/conformance.js [--only go-sdk,py-mcpserver] [--results file.json]
//
// It starts a core with Go's TestSDKHarness (Postgres from make db, or
// WAKESHIFT_TEST_DATABASE_URL), unless WAKESHIFT_SDK_HARNESS names a running one's
// file. The checks, in order (a variant stops at its first failure):
//
//   server/discover   2026-07-28: resultType, supportedVersions, tools and
//                     the events capability
//   tools/list        exactly watch, unwatch and report_seen; the compact
//                     list: watch (subscribes: [bench.event]) has the
//                     contract's description plus the subscribes sentence
//                     and session_key, harness and conversation_id;
//                     unwatch and report_seen (subscribes: []) the
//                     contract's descriptions and only session_key
//   watch             the contract's text first, then Wakeshift's block with
//                     Claude Code's Monitor command; recorded with the client,
//                     what Wakeshift recognized, the subscription and the
//                     session's hash
//   status            one live subscription (statusOf)
//   publish           matched 1, and the event reaches the session's wait
//   report_seen       its text; recorded with the session_key's hash equal
//                     to watch's session, and the codes seen
//   unwatch           its text, and no subscription left
//   tasks (no wait)   with the SDK's default (no wait tool), a client that
//                     takes MCP tasks sees no wakeshift_wait, and is told
//                     about the Monitor, not the wait tool
//   events/list       bench.event, with the same definition as ts-v2's (server.ts)
//   handshake         initialize (a Codex client): watch recorded as codex,
//                     with a beacon, through the session the server keeps
//                     (or none, stateless), SSE or JSON
//   tasks (wait)      the variant again with the wait tool on (WB_WAIT=1):
//                     a client that takes MCP tasks lists wakeshift_wait and
//                     is told to call it

import { spawn, type ChildProcess } from "node:child_process";
import { randomBytes } from "node:crypto";
import { existsSync, mkdtempSync, openSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { loadContract, type Contract } from "../app/front.js";
import { DEFAULT_APP, preparePort, VARIANTS, type AppId, type Variant } from "../app/variants.js";
import { freePort, type LaneState, type StatusState } from "./core.js";

const here = dirname(fileURLToPath(import.meta.url));
const benchDir = resolve(here, "../..");
const repo = resolve(benchDir, "..");
const PROTOCOL = "2026-07-28";
const CLAUDE = { name: "claude-code", version: "2.1.316" };
const CODEX = { name: "codex-mcp-client", version: "0.166.0" };
/** The Monitor's command (it reads the conversation's link from a file) and the link step that has the agent write that file. */
const MONITOR = /with this command: f=\$HOME\/\.wakeshift\/links\/[0-9a-f]{16}; [^\n]*?u="\$b(&until=hooks&timeout=[^"]*)"; c=; p=\$PPID;/;
const LINK_STEP = /its whole content this one line: (https?:\/\/\S+\/v1\/stream\/wait\?link=\S+) \./;
const TASKS_EXT = "io.modelcontextprotocol/tasks";

export interface Check {
  name: string;
  ok: boolean;
  detail?: string;
  error?: string;
}

export interface VariantResult {
  id: AppId;
  framework: string;
  outcome: "pass" | "fail" | "pending";
  checks: Check[];
  reason?: string;
  log?: string;
  /** bench.event as events/list lists it (compared across variants). */
  definition?: unknown;
}

/** Canonical JSON (keys sorted), for comparing definitions across SDKs. */
export function canonical(v: unknown): string {
  if (Array.isArray(v)) return `[${v.map(canonical).join(",")}]`;
  if (v && typeof v === "object")
    return `{${Object.keys(v as object)
      .sort()
      .map((k) => `${JSON.stringify(k)}:${canonical((v as Record<string, unknown>)[k])}`)
      .join(",")}}`;
  return JSON.stringify(v);
}

/**
 * A definition as the comparison across ports reads it: "push" left out of its
 * delivery. Whether push is listed depends on the host's transport, by
 * design (a host answering in JSON-response
 * mode, the ts-v2 lane, doesn't list it; one that streams, Go or ts-mastra,
 * does). The rest of the definition must match.
 */
export function withoutHostDelivery(def: unknown): unknown {
  if (!def || typeof def !== "object" || Array.isArray(def)) return def;
  const d = def as Record<string, unknown>;
  if (!Array.isArray(d.delivery)) return def;
  return { ...d, delivery: d.delivery.filter((m) => m !== "push") };
}

/** Where two definitions differ, as JSON paths (first few). */
export function diffPaths(a: unknown, b: unknown, path = "$", out: string[] = []): string[] {
  if (out.length >= 8) return out;
  if (canonical(a) === canonical(b)) return out;
  if (a && b && typeof a === "object" && typeof b === "object" && !Array.isArray(a) && !Array.isArray(b)) {
    const keys = new Set([...Object.keys(a as object), ...Object.keys(b as object)]);
    for (const k of [...keys].sort()) diffPaths((a as Record<string, unknown>)[k], (b as Record<string, unknown>)[k], `${path}.${k}`, out);
    return out;
  }
  out.push(`${path}: ${clip(JSON.stringify(a), 120)} vs ${clip(JSON.stringify(b), 120)}`);
  return out;
}

const clip = (s: string | undefined, n = 600) => {
  const r = String(s ?? "")
    .replace(/([?&](?:link|token|key|ticket)=)[^&'"\s]+/g, "$1[redacted]")
    .replace(/wsc_[A-Za-z0-9_-]+/g, "wsc_[redacted]")
    .replace(/eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+/g, "[jws]");
  return r.length > n ? r.slice(0, n) + "…" : r;
};

function need(cond: unknown, what: string): asserts cond {
  if (!cond) throw new Error(what);
}

const textOf = (r: { content?: { type?: string; text?: string }[] }) => (r.content ?? []).map((c) => (c.type === "text" ? (c.text ?? "") : "")).filter(Boolean).join("\n");

/** The JSON-RPC message with id from a response body (JSON or SSE). */
export function messageFrom(type: string, raw: string, id: number): { result?: any; error?: { code: number; message: string } } | undefined {
  if (type.includes("text/event-stream")) {
    for (const ev of raw.split(/\r?\n\r?\n/)) {
      const data = ev
        .split(/\r?\n/)
        .filter((l) => l.startsWith("data:"))
        .map((l) => l.slice(5).replace(/^ /, ""))
        .join("\n");
      if (!data) continue;
      try {
        const m = JSON.parse(data);
        if (m.id === id) return m;
      } catch {
        // not a message
      }
    }
    return undefined;
  }
  try {
    const m = JSON.parse(raw);
    return Array.isArray(m) ? m.find((x) => x.id === id) : m;
  } catch {
    return undefined;
  }
}

let nextId = 0;

/** A client on protocol 2026-07-28: each request carries its own _meta. */
async function rpc2026(url: string, method: string, params: Record<string, unknown> = {}, o: { client?: typeof CLAUDE; caps?: Record<string, unknown> } = {}): Promise<any> {
  const id = ++nextId;
  const meta = {
    "io.modelcontextprotocol/protocolVersion": PROTOCOL,
    "io.modelcontextprotocol/clientInfo": o.client ?? CLAUDE,
    "io.modelcontextprotocol/clientCapabilities": o.caps ?? {},
    ...((params._meta as Record<string, unknown>) ?? {}),
  };
  const headers: Record<string, string> = {
    "content-type": "application/json",
    accept: "application/json, text/event-stream",
    "user-agent": `claude-cli/${CLAUDE.version} (external, cli)`,
    "mcp-protocol-version": PROTOCOL,
    "mcp-method": method,
  };
  if (method === "tools/call") headers["mcp-name"] = String(params.name);
  const res = await fetch(url, { method: "POST", headers, body: JSON.stringify({ jsonrpc: "2.0", id, method, params: { ...params, _meta: meta } }), signal: AbortSignal.timeout(40_000) });
  const raw = await res.text();
  const m = messageFrom(res.headers.get("content-type") ?? "", raw, id);
  need(m, `${method}: HTTP ${res.status}, no JSON-RPC response: ${clip(raw)}`);
  need(!m.error, `${method}: error ${m.error?.code}: ${m.error?.message}`);
  need(m.result && typeof m.result === "object", `${method}: no result`);
  return m.result;
}

/** A handshake-era client (initialize, then its session if the server keeps one), as Codex is. */
class Handshake {
  private sid = "";
  private version = "2025-11-25";
  constructor(private url: string) {}
  async request(method: string, params: Record<string, unknown> = {}): Promise<{ result: any; type: string }> {
    const id = ++nextId;
    const headers: Record<string, string> = { "content-type": "application/json", accept: "application/json, text/event-stream", "mcp-protocol-version": this.version };
    if (this.sid) headers["mcp-session-id"] = this.sid;
    const res = await fetch(this.url, { method: "POST", headers, body: JSON.stringify({ jsonrpc: "2.0", id, method, params }), signal: AbortSignal.timeout(40_000) });
    const sid = res.headers.get("mcp-session-id");
    if (sid) this.sid = sid;
    const type = res.headers.get("content-type") ?? "";
    const raw = await res.text();
    const m = messageFrom(type, raw, id);
    need(m, `${method}: HTTP ${res.status}, no JSON-RPC response: ${clip(raw)}`);
    need(!m.error, `${method}: error ${m.error?.code}: ${m.error?.message}`);
    return { result: m.result, type };
  }
  async open(): Promise<string> {
    const { result } = await this.request("initialize", { protocolVersion: this.version, capabilities: {}, clientInfo: CODEX });
    if (typeof result?.protocolVersion === "string") this.version = result.protocolVersion;
    const headers: Record<string, string> = { "content-type": "application/json", accept: "application/json, text/event-stream", "mcp-protocol-version": this.version };
    if (this.sid) headers["mcp-session-id"] = this.sid;
    await fetch(this.url, { method: "POST", headers, body: JSON.stringify({ jsonrpc: "2.0", method: "notifications/initialized" }) });
    return this.sid ? "a session" : "stateless";
  }
  async close() {
    if (this.sid) await fetch(this.url, { method: "DELETE", headers: { "mcp-session-id": this.sid, "mcp-protocol-version": this.version } }).catch(() => {});
  }
}

export interface Toy {
  base: string;
  /** BENCH_RUN: topics are bench/<run>/<lane>. */
  run: string;
  control: string;
  proc: ChildProcess;
  log: string;
}

export async function controlApi<T>(t: Toy, method: string, path: string, body?: unknown): Promise<T> {
  const r = await fetch(t.base + path, { method, headers: { authorization: `Bearer ${t.control}`, "content-type": "application/json" }, body: body === undefined ? undefined : JSON.stringify(body) });
  const j = (await r.json()) as T & { error?: { message?: string } };
  if (!r.ok) throw new Error(`${path}: ${r.status} ${j.error?.message ?? ""}`);
  return j;
}

/** Waits for a code on a session's wait URL (the Monitor's command reads it the same way). */
async function waitFor(waitURL: string, code: string, ms = 20_000): Promise<void> {
  const deadline = Date.now() + ms;
  let after = "";
  for (;;) {
    const u = waitURL.replace(/([?&])timeout=[^&]*/, "$1timeout=10s") + `&after=${encodeURIComponent(after)}`;
    const res = await fetch(u, { signal: AbortSignal.timeout(30_000) });
    const lines = (await res.text()).split("\n").filter(Boolean);
    need(res.status === 200, `the wait: HTTP ${res.status}: ${clip(lines.join("\n"))}`);
    for (const l of lines) {
      let j: { type?: string; subject?: string; text?: string; cursor?: string };
      try {
        j = JSON.parse(l);
      } catch {
        continue;
      }
      if (j.type === "event" && JSON.stringify(j).includes(code)) return;
      if (typeof j.cursor === "string") after = j.cursor;
    }
    if (Date.now() > deadline) throw new Error(`${code} didn't reach the session's wait within ${ms / 1000}s`);
  }
}

/** The checks for one running variant (the lane is the conformance run's own). */
export async function checkVariant(t: Toy, c: Contract, rec: VariantResult): Promise<void> {
  const url = `${t.base}/mcp/conf`;
  const check = async (name: string, fn: () => Promise<string | void>) => {
    try {
      const detail = await fn();
      rec.checks.push({ name, ok: true, ...(detail ? { detail } : {}) });
    } catch (err) {
      rec.checks.push({ name, ok: false, error: clip(err instanceof Error ? err.message : String(err), 1500) });
      throw err;
    }
  };
  const lane = () => controlApi<LaneState>(t, "GET", "/control/lane?lane=conf");
  const status = () => controlApi<StatusState>(t, "GET", "/control/status?lane=conf");

  await check("server/discover", async () => {
    const d = await rpc2026(url, "server/discover");
    need(d.resultType === "complete", `resultType is ${JSON.stringify(d.resultType)}`);
    need(Array.isArray(d.supportedVersions) && d.supportedVersions.includes(PROTOCOL), `supportedVersions ${JSON.stringify(d.supportedVersions)} lacks ${PROTOCOL}`);
    need(d.capabilities?.tools, "no tools capability");
    const ev = d.capabilities?.extensions?.["io.modelcontextprotocol/events"] ?? d.capabilities?.events;
    need(ev, "no events capability (extensions[io.modelcontextprotocol/events] or events)");
  });

  await check("tools/list", async () => {
    const l = await rpc2026(url, "tools/list");
    const names = (l.tools ?? []).map((x: { name: string }) => x.name).sort();
    need(canonical(names) === canonical(["report_seen", "unwatch", "watch"]), `tools are ${names.join(", ")}, not exactly watch, unwatch and report_seen`);
    // The compact tools/list every SDK serves: watch subscribes to bench.event,
    // so it gets all three wake arguments and the SDK's subscribes sentence
    // after the contract's description (joined with a space; the app's name
    // is the console's); unwatch and report_seen subscribe to nothing
    // (subscribes: []), so only session_key and the contract's description.
    const sentence = ` This tool is how `;
    const ofEvent = ` notifies you in this conversation of this event: ${c.event.description}.`;
    for (const tool of l.tools as { name: "watch" | "unwatch" | "report_seen"; description?: string; inputSchema?: { properties?: Record<string, any>; required?: string[] } }[]) {
      const want = c.tools[tool.name].description;
      const props = tool.inputSchema?.properties ?? {};
      if (tool.name === "watch") {
        const d = tool.description ?? "";
        need(d.startsWith(want + sentence) && d.includes(ofEvent), `watch's description is ${JSON.stringify(d)}, not the contract's followed by the subscribes sentence for ${c.event.name}`);
        for (const k of ["session_key", "harness", "conversation_id"]) need(props[k], `watch lacks Wakeshift's ${k}`);
      } else {
        need(tool.description === want, `${tool.name}'s description is ${JSON.stringify(tool.description)}, not the contract's`);
        need(props.session_key, `${tool.name} lacks Wakeshift's session_key`);
        for (const k of ["harness", "conversation_id"]) need(!props[k], `${tool.name} lists Wakeshift's ${k}, which only tools that subscribe to an event list`);
      }
    }
    const seen = (l.tools as any[]).find((x) => x.name === "report_seen");
    const ids = seen.inputSchema?.properties?.ids;
    // go-sdk's schemas let a Go slice be null too (["null","array"]): still an array of strings.
    need([ids?.type].flat().includes("array") && ids?.items?.type === "string", `report_seen's ids isn't an array of strings: ${clip(JSON.stringify(ids))}`);
    need(ids?.description === c.tools.report_seen.idsDescription, `report_seen's ids description is ${JSON.stringify(ids?.description)}`);
    need((seen.inputSchema?.required ?? []).includes("ids"), "report_seen doesn't require ids");
  });

  let waitURL = "";
  let session = "";
  let sessionKey = "";
  await check("watch", async () => {
    const r = await rpc2026(url, "tools/call", { name: "watch", arguments: { harness: "claude-code" }, _meta: { "claudecode/toolUseId": `toolu_conf_${randomBytes(6).toString("hex")}` } });
    const said = textOf(r);
    need(!r.isError, `isError: ${clip(said)}`);
    need(said.startsWith(c.tools.watch.text), `the contract's text isn't first: ${clip(said)}`);
    const m = said.match(MONITOR);
    const l = said.match(LINK_STEP);
    need(m && l, `Wakeshift's block has no Monitor command and link step for Claude Code: ${clip(said)}`);
    waitURL = l[1] + m[1];
    const k = /session_key[="':\s]+(wsc_[A-Za-z0-9_-]+)/.exec(said);
    if (k) sessionKey = k[1];
    const st = await lane();
    const w = st.calls.filter((x) => x.tool === "watch").pop();
    need(w, "the watch call wasn't recorded");
    need(w.client === CLAUDE.name, `recorded client ${JSON.stringify(w.client)}, not ${CLAUDE.name}`);
    need(w.agent?.harness === "claude-code", `recorded harness ${JSON.stringify(w.agent)}`);
    need(w.subscription, "no subscription recorded");
    need(w.session && /^[0-9a-f]{12}$/.test(w.session), `no session hash recorded: ${JSON.stringify(w.session)}`);
    need(w.added && /Monitor/.test(w.added) && !w.added.startsWith(c.tools.watch.text), `what Wakeshift added wasn't recorded: ${clip(w.added)}`);
    need(st.subscriptions.includes(w.subscription), "the lane doesn't list the subscription");
    session = w.session;
    return `session key in the block: ${sessionKey ? "yes" : "no"}`;
  });

  await check("status", async () => {
    const s = await status();
    need(s.event.subscriptions === 1, `event status counts ${s.event.subscriptions} subscriptions`);
    need(s.subscriptions.length === 1 && s.subscriptions[0].live === true, `statusOf: ${clip(JSON.stringify(s.subscriptions))}`);
  });

  let code = "";
  await check("publish", async () => {
    const { events } = await controlApi<{ events: { code: string; matched: number }[] }>(t, "POST", "/control/publish", { lane: "conf", count: 1 });
    need(events.length === 1 && events[0].matched === 1, `publish: ${JSON.stringify(events)}`);
    code = events[0].code;
    need(/^WB-[a-z0-9]{4}-1$/.test(code), `code ${code}`);
    await waitFor(waitURL, code);
  });

  await check("report_seen", async () => {
    const args: Record<string, unknown> = { ids: [code], harness: "claude-code" };
    if (sessionKey) args.session_key = sessionKey;
    const r = await rpc2026(url, "tools/call", { name: "report_seen", arguments: args });
    const said = textOf(r);
    need(said.startsWith(c.tools.report_seen.text.replace("{n}", "1")), `report_seen said ${clip(said)}`);
    const st = await lane();
    const s = st.calls.filter((x) => x.tool === "report_seen").pop();
    need(s, "the report_seen call wasn't recorded");
    need(canonical(s.args.ids) === canonical([code]), `recorded ids ${JSON.stringify(s.args.ids)}`);
    if (sessionKey) need(s.args.session_key === session, "the recorded session_key isn't the session watch was given");
    need(st.seen.some((x) => x.ids.includes(code)), "seen doesn't have the code");
  });

  await check("unwatch", async () => {
    const args: Record<string, unknown> = { harness: "claude-code" };
    if (sessionKey) args.session_key = sessionKey;
    const r = await rpc2026(url, "tools/call", { name: "unwatch", arguments: args });
    const said = textOf(r);
    need(said.startsWith(c.tools.unwatch.ended), `unwatch said ${clip(said)}`);
    const s = await status();
    need(s.event.subscriptions === 0, `${s.event.subscriptions} subscriptions left`);
    need((await lane()).calls.some((x) => x.tool === "unwatch"), "the unwatch call wasn't recorded");
  });

  await check("tasks (no wait)", async () => {
    // The SDKs serve no wait tool unless the app turns it on, so a client
    // that takes tasks is woken the way any other Claude Code is.
    const caps = { extensions: { [TASKS_EXT]: {} } };
    const l = await rpc2026(url, "tools/list", {}, { caps });
    const names = (l.tools ?? []).map((x: { name: string }) => x.name);
    need(!names.includes("wakeshift_wait"), `without wait, a client that takes tasks sees wakeshift_wait: ${names.join(", ")}`);
    const d = await rpc2026(url, "server/discover", {}, { caps });
    need(!d.capabilities?.extensions?.[TASKS_EXT], "without wait, server/discover offers the tasks extension");
    const r = await rpc2026(url, "tools/call", { name: "watch", arguments: { harness: "claude-code" } }, { caps });
    const said = textOf(r);
    need(!/wakeshift_wait/.test(said) && /Monitor/.test(said), `without wait, the block for a client that takes tasks names wakeshift_wait, or not the Monitor: ${clip(said)}`);
    await rpc2026(url, "tools/call", { name: "unwatch", arguments: { harness: "claude-code" } }, { caps }).catch(() => {});
  });

  await check("events/list", async () => {
    const r = await rpc2026(url, "events/list");
    const ev = (r.events ?? []).find((e: { name: string }) => e.name === c.event.name);
    need(ev, `events/list lacks ${c.event.name}: ${clip(JSON.stringify(r))}`);
    need(ev.description === c.event.description, `its description is ${JSON.stringify(ev.description)}`);
    rec.definition = ev;
  });

  await check("handshake (initialize, as Codex)", async () => {
    const h = new Handshake(`${t.base}/mcp/conf-codex`);
    const how = await h.open();
    try {
      const { result: l } = await h.request("tools/list");
      need((l.tools ?? []).some((x: { name: string }) => x.name === "watch"), "no watch tool");
      // Codex names its thread in each call's _meta: what its beacon binds to.
      const thread = { "x-codex-turn-metadata": { thread_id: `0199${randomBytes(14).toString("hex")}` } };
      const { result: r, type } = await h.request("tools/call", { name: "watch", arguments: { harness: "codex" }, _meta: thread });
      need(!r.isError && textOf(r).startsWith(c.tools.watch.text), `watch said ${clip(textOf(r))}`);
      const st = await controlApi<LaneState>(t, "GET", "/control/lane?lane=conf-codex");
      const w = st.calls.filter((x) => x.tool === "watch").pop();
      need(w, "the watch call wasn't recorded");
      // A stateless handler (go-sdk's, and 2.x's createMcpHandler) sees the client's name
      // only in _meta, which a handshake-era client doesn't send: noted, not
      // failed (Wakeshift still recognizes Codex by its harness argument).
      need(w.client === CODEX.name || w.client === "", `recorded client ${JSON.stringify(w.client)}`);
      need(w.agent?.harness === "codex", `recorded harness ${JSON.stringify(w.agent)}`);
      need(w.beacon, "no beacon recorded for Codex's watch");
      await h.request("tools/call", { name: "unwatch", arguments: { harness: "codex" }, _meta: thread });
      return `${how}, ${type.includes("event-stream") ? "SSE" : "JSON"} responses${w.client ? "" : "; no client name recorded"}, recognized via ${w.agent?.via}`;
    } finally {
      await h.close();
    }
  });
}

/** The check for the variant run with the wait tool on (WB_WAIT=1). */
export async function checkWait(t: Toy, rec: VariantResult): Promise<void> {
  const url = `${t.base}/mcp/conf-wait`;
  try {
    const caps = { extensions: { [TASKS_EXT]: {} } };
    const d = await rpc2026(url, "server/discover", {}, { caps });
    need(d.capabilities?.extensions?.[TASKS_EXT], "with wait, server/discover doesn't offer the tasks extension");
    const l = await rpc2026(url, "tools/list", {}, { caps });
    const names = (l.tools ?? []).map((x: { name: string }) => x.name);
    need(names.includes("wakeshift_wait"), `with wait, a client that takes tasks doesn't see wakeshift_wait: ${names.join(", ")}`);
    const r = await rpc2026(url, "tools/call", { name: "watch", arguments: { harness: "claude-code" } }, { caps });
    const said = textOf(r);
    need(/wakeshift_wait/.test(said), `with wait, the block doesn't tell a client that takes tasks to call wakeshift_wait: ${clip(said)}`);
    await rpc2026(url, "tools/call", { name: "unwatch", arguments: { harness: "claude-code" } }, { caps }).catch(() => {});
    rec.checks.push({ name: "tasks (wait)", ok: true });
  } catch (err) {
    rec.checks.push({ name: "tasks (wait)", ok: false, error: clip(err instanceof Error ? err.message : String(err), 1500) });
    throw err;
  }
}

// ── Running it ─────────────────────────────────────────────────────────────

const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));

export async function startCore(work: string, cleanups: (() => Promise<void>)[]): Promise<{ core: string; new_key: string }> {
  if (process.env.WAKESHIFT_SDK_HARNESS) return JSON.parse(readFileSync(process.env.WAKESHIFT_SDK_HARNESS, "utf8"));
  const out = join(work, "harness.json");
  const logFile = join(work, "core.log");
  const fd = openSync(logFile, "a");
  const p = spawn("go", ["test", "-count=1", "-run", "^TestSDKHarness$", "-timeout", "65m", "-v", "./internal/coretest/"], {
    cwd: repo,
    env: {
      ...process.env,
      WAKESHIFT_TEST_DATABASE_URL: process.env.WAKESHIFT_TEST_DATABASE_URL || "postgres://postgres:postgres@localhost:5439/postgres?sslmode=disable",
      WAKESHIFT_SDK_HARNESS: out,
      WAKESHIFT_SDK_HARNESS_MINUTES: "60",
    },
    stdio: ["ignore", fd, fd],
    detached: true,
  });
  let exited = false;
  p.on("exit", () => (exited = true));
  cleanups.push(async () => {
    rmSync(out, { force: true }); // the harness stops once its file is gone
    if (!exited) await new Promise<void>((r) => (p.once("exit", () => r()), setTimeout(() => (killGroup(p), r()), 10_000)));
  });
  const deadline = Date.now() + 240_000;
  while (!existsSync(out)) {
    if (exited || Date.now() > deadline) throw new Error(`the core didn't start:\n${readFileSync(logFile, "utf8").split("\n").slice(-40).join("\n")}`);
    await sleep(100);
  }
  return JSON.parse(readFileSync(out, "utf8"));
}

export function killGroup(p: ChildProcess) {
  try {
    if (p.pid) process.kill(-p.pid, "SIGTERM");
  } catch {
    // gone
  }
}

export async function startToy(v: Variant, h: { core: string; new_key: string }, work: string, wait = false, extraEnv: Record<string, string> = {}): Promise<Toy> {
  const run = `conf-${randomBytes(3).toString("hex")}`;
  const keyFile = join(work, "app.key");
  const controlFile = join(work, "control.token");
  const control = randomBytes(18).toString("base64url");
  writeFileSync(keyFile, h.new_key + "\n", { mode: 0o600 });
  writeFileSync(controlFile, control + "\n", { mode: 0o600 });
  const portCmd = await preparePort(v.id, repo, work);
  const port = await freePort();
  const log = join(work, `${v.id}${wait ? "-wait" : ""}.log`);
  const fd = openSync(log, "a");
  const proc = spawn(process.execPath, [join(benchDir, "dist/app/main.js")], {
    env: {
      PATH: process.env.PATH ?? "",
      HOME: process.env.HOME ?? "",
      BENCH_KEY_FILE: keyFile,
      BENCH_CONTROL_FILE: controlFile,
      WAKESHIFT_CORE: h.core,
      BENCH_RUN: run,
      PORT: String(port),
      BENCH_APP: v.id,
      ...(v.env ?? {}),
      // server.ts serves 2026-07-28 (createMcpHandler) only when asked, as an app on a stateless transport does.
      ...(v.id === DEFAULT_APP ? { WB_MCP_2026: "1" } : {}),
      ...(portCmd ? { BENCH_PORT_CMD: JSON.stringify(portCmd) } : {}),
      // The SDKs' default (no wait tool), or the wait tool on (app/tasks.ts's benchWait).
      WB_WAIT: wait ? "1" : "0",
      ...extraEnv,
    },
    stdio: ["ignore", fd, fd],
    detached: true,
  });
  let exited: number | string | null = null;
  proc.on("exit", (c, s) => (exited = c ?? s ?? "exited"));
  const base = `http://127.0.0.1:${port}`;
  const deadline = Date.now() + 150_000;
  for (;;) {
    if (exited !== null) throw new Error(`the toy app exited (${exited}):\n${readFileSync(log, "utf8").split("\n").slice(-25).join("\n")}`);
    try {
      if ((await fetch(`${base}/health`, { signal: AbortSignal.timeout(2000) })).ok) break;
    } catch {
      // not up yet
    }
    if (Date.now() > deadline) throw new Error("the toy app didn't come up");
    await sleep(200);
  }
  return { base, run, control, proc, log };
}

async function main() {
  const argv = process.argv.slice(2);
  const flag = (name: string) => {
    const i = argv.indexOf(`--${name}`);
    return i >= 0 ? argv[i + 1] : undefined;
  };
  const only = (flag("only") ?? process.env.APPS ?? "").split(",").map((s) => s.trim()).filter(Boolean);
  for (const id of only) if (!VARIANTS.some((v) => v.id === id)) throw new Error(`unknown app ${id}`);
  const chosen = VARIANTS.filter((v) => !only.length || only.includes(v.id));
  // server.ts's definition is the reference: run it first when comparing.
  const reference = chosen.some((v) => v.id !== DEFAULT_APP) ? VARIANTS.find((v) => v.id === DEFAULT_APP)! : null;
  const list = reference && !chosen.includes(reference) ? [reference, ...chosen] : chosen;
  const contract = loadContract(join(benchDir, "app/contract.json"));
  const work = mkdtempSync(join(tmpdir(), "wakebench-conf-"));
  const cleanups: (() => Promise<void>)[] = [];
  const results: VariantResult[] = [];
  try {
    const h = await startCore(work, cleanups);
    for (const v of list) {
      const rec: VariantResult = { id: v.id, framework: v.framework, outcome: "fail", checks: [] };
      if (v.pending) {
        results.push({ ...rec, outcome: "pending", reason: v.pending });
        continue;
      }
      let toy: Toy | undefined;
      const stop = async (t: Toy | undefined) => {
        if (!t) return;
        killGroup(t.proc);
        await new Promise<void>((r) => (t.proc.exitCode !== null ? r() : (t.proc.once("exit", () => r()), setTimeout(r, 5000))));
      };
      try {
        toy = await startToy(v, h, work);
        rec.log = toy.log;
        rec.checks.push({ name: "start", ok: true });
        await checkVariant(toy, contract, rec);
        await stop(toy);
        // Again with the wait tool on.
        toy = await startToy(v, h, work, true);
        rec.log = toy.log;
        await checkWait(toy, rec);
        rec.outcome = "pass";
      } catch (err) {
        if (!rec.checks.length || rec.checks[rec.checks.length - 1].ok) rec.checks.push({ name: toy ? "check" : "start", ok: false, error: clip(err instanceof Error ? err.message : String(err), 3000) });
      } finally {
        await stop(toy);
      }
      results.push(rec);
    }
    // The same event everywhere: a port's SDK sends its own manifest, so a
    // definition that differs from server.ts's flips the app's between runs.
    const ref = results.find((r) => r.id === DEFAULT_APP)?.definition;
    for (const r of results) {
      if (r.id === DEFAULT_APP || r.outcome !== "pass" || !ref) continue;
      const d = diffPaths(withoutHostDelivery(ref), withoutHostDelivery(r.definition));
      r.checks.push(d.length ? { name: "same event as ts-v2", ok: false, error: d.join("; ") } : { name: "same event as ts-v2", ok: true });
      if (d.length) r.outcome = "fail";
    }
  } finally {
    for (const c of cleanups.reverse()) await c().catch(() => {});
  }
  const shown = results.filter((r) => chosen.some((v) => v.id === r.id));
  for (const r of shown) {
    const bad = r.checks.find((c) => !c.ok);
    console.log(`${r.outcome.toUpperCase().padEnd(8)} ${r.id.padEnd(14)} ${r.framework}${bad ? `\n         at ${bad.name}: ${bad.error}` : r.reason ? `\n         ${r.reason}` : ""}`);
    for (const c of r.checks) if (c.ok && c.detail) console.log(`         ${c.name}: ${c.detail}`);
  }
  const out = flag("results");
  if (out) writeFileSync(out, JSON.stringify(shown, null, 1) + "\n");
  if (shown.some((r) => r.outcome === "fail")) {
    for (const r of shown.filter((x) => x.outcome === "fail" && x.log && existsSync(x.log))) console.log(`\n${r.id}'s log (last lines):\n${clip(readFileSync(r.log!, "utf8").split("\n").slice(-30).join("\n"), 4000)}`);
    process.exitCode = 1;
  }
  rmSync(work, { recursive: true, force: true });
}

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  main().catch((err) => {
    console.error(err instanceof Error ? err.message : String(err));
    process.exit(2);
  });
}
