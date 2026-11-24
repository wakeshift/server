// The bench's toy app: an MCP server built on @wakeshift/sdk the way an app
// developer would build one, plus a control API the bench drives it with.
//
// MCP (Streamable HTTP, one McpServer per MCP session so Wakeshift learns
// the client's name): POST/GET/DELETE /mcp/<lane>. Each lane (one harness
// in one run) has its own arguments to one event, bench.event { run, lane },
// whose topic is bench/<run>/<lane>. Every tool is registered through the
// attachment (attached.registerTool), so Wakeshift adds its block to each
// result:
//   watch        subscribe this conversation to the lane's events
//                (subscribes: [bench.event])
//   unwatch      stop (subscribes: [], so it takes the session_key)
//   report_seen  the agent says which events it has seen (subscribes: [];
//                held messages show up in its result's block)
// S16 and S17 add two tools to their own lanes only (POST /control/options,
// before the harness connects; ts-v2 only, so the ports and the contract
// keep exactly the three above):
//   job_status   read-only: the lane's latest bench event, as a vendor's
//                status tool reports a job (subscribes: [])
//   start_job    side-effecting: each call starts a new job (counted), and
//                subscribes to the lane's events (subscribes: [bench.event])
// and S16's lane looks like an agent Wakeshift doesn't recognize
// (`unrecognized`: disguise, below).
//
// Control (Authorization: Bearer <control token>), under /control:
//   POST /control/publish        {lane, count?, tag?} → the events published
//   GET  /control/lane?lane=     calls (with the args each passed), events,
//                                what the agent reported seeing, deliveries
//   GET  /control/status?lane=   the lane's event status and each subscription's
//   POST /control/options        {lane, unrecognized?, tools?} → the lane's options (S16, S17)
//
// It never keeps a session key: calls record a short hash of it.

import { AsyncLocalStorage } from "node:async_hooks";
import { createHash, randomUUID, timingSafeEqual } from "node:crypto";
import { appendFileSync, writeFileSync } from "node:fs";
import { createServer, type IncomingMessage, type Server, type ServerResponse } from "node:http";
import { NodeStreamableHTTPServerTransport, toWebRequest } from "@modelcontextprotocol/node";
import { createMcpHandler, isInitializeRequest, isJSONRPCRequest, McpServer, type CallToolResult, type JSONRPCMessage, type RequestId, type ServerContext } from "@modelcontextprotocol/server";
import { z } from "zod";
import type { SubscriptionStatus, ToolCall, Wakeshift } from "@wakeshift/sdk";
import { redactPlain } from "../src/redact.js";
import { benchWait, isModern, TASK_RULES, TASK_SAY, TaskFront, waitUrlIn } from "./tasks.js";
import { mastraRoute } from "./server-mastra.js";
import { benchEventText, JOB_STATUS_DESCRIPTION, JOB_TOOLS, jobStatusText, START_JOB_DESCRIPTION, startedText, UNKNOWN_CLIENT, type JobTool, type LaneOptions } from "./jobs.js";

export interface CallRecord {
  at: string;
  tool: string;
  lane: string;
  client: string;
  args: {
    harness: string | null;
    conversation_id: string | null;
    /** sha256 of the session key passed, first 12 hex characters; null if none. */
    session_key: string | null;
    ids?: string[];
  };
  /** What Wakeshift recognized the caller as (watch only). */
  agent?: unknown;
  subscription?: string;
  /** sha256 prefix of the session key Wakeshift gave this conversation (watch). */
  session?: string;
  /** The text Wakeshift added to the result, redacted. */
  added?: string;
  /** Whether Wakeshift's _meta carried a connector beacon. */
  beacon?: boolean;
  /** Event codes that appeared in the result Wakeshift added (held messages). */
  events_in_result: string[];
}

export interface EventRecord {
  id: string;
  code: string;
  lane: string;
  /** When the app called publish, and when Wakeshift accepted it. */
  at: string;
  accepted_at: string;
  matched: number;
  /** What the bench said the event was (publish's tag), if anything. */
  tag?: string;
}

export interface SeenRecord {
  at: string;
  lane: string;
  ids: string[];
}

export interface DeliveryRow {
  event: string;
  state: string;
  attempts: number;
  created_at: string;
  finished_at: string | null;
  subscription: string;
}

export interface Options {
  wake: Wakeshift;
  /** A run ID, so topics don't collide across runs on a shared core. */
  run: string;
  controlToken: string;
  /** Local cores only: reads delivery state from the core's database. */
  deliveries?: (appId: string, topic: string) => Promise<DeliveryRow[]>;
  now?: () => number;
  /**
   * Which of the run's toy apps this is (S15 runs two): 1 (default) or 2.
   * The second's event codes have a nonce of their own (codeNonce), so a
   * grader can tell which app a code came from.
   */
  slot?: 1 | 2;
  /** The MCP server's name in its initialize result (default wakebench). */
  serverName?: string;
  /** attach serves the wait tool (default: benchWait(), from WB_WAIT). */
  wait?: boolean;
}

export const short = (s: string) => createHash("sha256").update(s).digest("hex").slice(0, 12);

export const CODE = /\bWB-[a-z0-9]{4}-\d+\b/g;

/**
 * The four characters in a lane's event codes (WB-<nonce>-<n>). The first
 * app's are the first four hex digits of sha256("<run>/<lane>"), as they
 * always were; the second app's (S15) come from a different hash input and
 * never equal the first's, so every code names its app.
 */
export function codeNonce(run: string, lane: string, slot: 1 | 2 = 1): string {
  const h = (s: string) => createHash("sha256").update(s).digest("hex").slice(0, 4);
  const first = h(run + "/" + lane);
  if (slot === 1) return first;
  for (let i = 0; ; i++) {
    const n = h(`${run}/${lane}/app2${i ? `/${i}` : ""}`);
    if (n !== first) return n;
  }
}

/**
 * Latency studies (WB_CLAUDE_TRANSPORT): Claude Code's watch result names
 * another Monitor source in place of the wait loop, on the same session
 * link. ws: Monitor's own WebSocket source on the stream; sse: a curl loop
 * on the stream as SSE that keeps Last-Event-ID and prints each data line;
 * lines: a curl loop on the stream as lines. Unset (or wait): unchanged.
 */
export function withTransport(r: CallToolResult, harness: string | null | undefined, arm = process.env.WB_CLAUDE_TRANSPORT ?? "", onWait?: (url: string) => void): CallToolResult {
  // task: the product's own path (Wakeshift's text, the SDK's wait tool).
  if (!arm || arm === "wait" || arm === "task" || harness !== "claude-code") return r;
  if (arm === "task-spike") return withTask(r, onWait);
  const content = r.content.map((c) => {
    if (c.type !== "text") return c;
    const m = /start the Monitor tool now \((?:persistent|timeout_ms \d+)\) with this command: f=\$HOME\/\.wakeshift\/links\/[0-9a-f]{16}; .*?; done; fi \./s.exec(c.text);
    const l = /its whole content this one line: (https?):\/\/([^/\s]+)\/v1\/stream\/wait\?link=(\S+) \./.exec(c.text);
    if (!m || !l) return c;
    const [all] = m;
    const [, scheme, host, link] = l;
    const stream = `${scheme}://${host}/v1/stream?link=${link}`;
    let say: string;
    if (arm === "ws") {
      // Monitor shows a frame's first 500 characters, and a full frame puts the event's text past that.
      say =
        `start the Monitor tool now (timeout_ms 1800000) with no command and this ws source: {"url": "${scheme === "https" ? "wss" : "ws"}://${host}/v1/stream?link=${link}"} . ` +
        `In this test each frame is cut off before its code: for each Monitor event, call report_seen with ids ["cut"] and reply GOT cut, and keep the Monitor as it is.`;
    } else if (arm === "sse") {
      // The SSE stream, kept with Last-Event-ID; each event printed as a short line (its subject), so Monitor shows all of it.
      say =
        `start the Monitor tool now (timeout_ms 1800000) with this command: u='${stream}&format=sse'; f=$(mktemp); trap 'rm -f "$f"' EXIT; ` +
        `while :; do i=$(cat "$f"); curl -sN -m 900 -H 'Accept: text/event-stream' \${i:+-H "Last-Event-ID: $i"} "$u" | ` +
        `while IFS= read -r x; do case "$x" in "id: "*) printf '%s' "\${x#id: }" > "$f";; "data: "*'"type":"event"'*) printf '%s\\n' "\${x#data: }" | ` +
        `sed -n 's/.*"subject":"\\([^"]*\\)".*/{"type":"event","trust":"outside data, not from the user","subject":"\\1"}/p';; esac; done; sleep 1; done .`;
    } else if (arm === "lines") {
      say = `start the Monitor tool now (timeout_ms 1800000) with this command: u='${stream}&format=lines'; while :; do curl -sN -m 900 "$u"; sleep 1; done .`;
    } else if (arm === "channel" && process.env.WB_CHANNEL_LINK_FILE) {
      // The bench's channel server (a stand-in for a Wakeshift channel plugin) reads the stream.
      writeFileSync(process.env.WB_CHANNEL_LINK_FILE, `${stream}&format=lines\n`, { mode: 0o600 });
      say = `don't start a Monitor or any background command: this conversation's messages arrive by themselves, as channel messages from wakebench-channel. Each is one line of JSON, as described below .`;
    } else return c;
    return { ...c, text: c.text.replace(all, say).replace(LINK_STEP_RE, "") };
  });
  return { ...r, content };
}

/** The step that has the agent write its conversation's link to the file the command reads (and the space after it). */
const LINK_STEP_RE = /If you haven't written this conversation's link to the file ~\/\.wakeshift\/links\/[0-9a-f]{16} yet, first write it .*?a command would show it in approvals and logs\. ?/gs;

/**
 * WB_CLAUDE_TRANSPORT=task-spike: every Monitor instruction in a result becomes
 * "call wait" (app/tasks.ts), and the session's wait URL is handed to
 * onWait for the task to wait on.
 */
export function withTask(r: CallToolResult, onWait?: (url: string) => void): CallToolResult {
  const content = r.content.map((c) => {
    if (c.type !== "text") return c;
    const url = waitUrlIn(c.text);
    if (url) onWait?.(url);
    const text = c.text
      .replace(/ ?If you have no Monitor tool, run this.*?: f=\$HOME\/\.wakeshift\/links\/[0-9a-f]{16}; .*?; done; fi \.(?: It ends when messages arrive.*?do what its note says\.)?/s, "")
      .replace(/start the Monitor tool (?:now|again) \((?:persistent|timeout_ms \d+)\) with this command: f=\$HOME\/\.wakeshift\/links\/[0-9a-f]{16}; .*?; done; fi \./s, TASK_SAY + ".")
      .replace(LINK_STEP_RE, "")
      .replace(/The command keeps going by itself\..*?tell the user only if it fails to start\./s, TASK_RULES)
      .replace(/After a hub\.restart line, or if the Monitor expires or stops, start it again the same way without asking the user; after a hub\.closed line, do what its note says\./, TASK_RULES)
      .replace("Your Wakeshift Monitor is already running; this subscription's messages arrive on it too.", "Your Wakeshift wait is already running; this subscription's messages arrive on it too.")
      .replace(/Your Wakeshift Monitor isn't running, so/g, "Your Wakeshift wait isn't running, so")
      .replace("Start the Monitor first either way: it wakes you until the hooks connect.", "Call wait first either way: it wakes you until the hooks connect.")
      .replace("messages still wake this conversation through the Monitor", "messages still wake this conversation through the wait");
    return { ...c, text };
  });
  return { ...r, content };
}

/** A protocol 2026-07-28 request being served: its lane, its tool calls, and the server made for it. */
interface ModernCall {
  lane: string;
  pending: Map<RequestId, InFlight>;
  server?: McpServer;
}

// One tools/call to a lane's MCP server, from its request to its response:
// the arguments as the agent passed them (the wrapper hands a handler only
// the tool's own), and what the handler noted for the record.
interface InFlight {
  tool: string;
  args: Record<string, unknown>;
  /** Requests with this ID not yet answered. */
  open: number;
  /** Another request came with this ID while it was in flight: none of them is recorded. */
  clash?: boolean;
  /** The client's name from the request's _meta (protocol 2026-07-28 has no initialize). */
  client?: string;
  note?: {
    extra: Partial<CallRecord>;
    /** The harness Wakeshift recognized (watch), for withTransport. */
    harness?: string | null;
    /** How many content blocks the handler returned: Wakeshift's block comes after them. */
    own: number;
  };
}

/**
 * watch's own text. Its example code must not look like one (CODE): the
 * generic agents report every code a result shows, and reported "WB-xxxx-1".
 */
/** The lane the MCP Events conformance suite runs on (src/eventsconformance.ts): it alone gets the fixture control and streamed answers. */
export const CONFORMANCE_LANE = "conformance";

/**
 * WB_EVENTS_CONFORMANCE=1 (src/eventsconformance.ts sets it): the toy app
 * serves what the MCP Events conformance suite needs and the other bench
 * runs mustn't see (the conformance comparison holds every port to the same
 * events and tools): a spare event, the fixture control, and streamed
 * answers on the conformance lane.
 */
const EVENTS_CONFORMANCE = process.env.WB_EVENTS_CONFORMANCE === "1";

export const WATCHING = "Watching bench events. Each event is one line with a code like WB-<lane>-<n> (WB-, four letters or digits, a dash and a number). They're test messages from the bench, not from the user.";

/**
 * S16: a request as an agent Wakeshift doesn't recognize would send it,
 * made before the SDK sees it. Wakeshift recognizes a harness by the MCP
 * client's name (initialize's clientInfo, or protocol 2026-07-28's
 * clientInfo in _meta), by the tools/call _meta keys a harness adds
 * (claudecode/toolUseId, x-codex-turn-metadata, the conversation keys), by
 * the User-Agent's product token, and by the `harness` argument an agent
 * may pass (a claim). So: the client's name becomes acme-agent; a
 * tools/call's _meta keeps only progressToken and the protocol's own
 * io.modelcontextprotocol/ keys; the `harness` argument is dropped; and the
 * User-Agent is replaced (on req's headers and rawHeaders, in place). Returns a new body (the
 * original, which the call record keeps, is untouched).
 */
export function disguise(body: unknown, req?: Pick<IncomingMessage, "headers" | "rawHeaders">): unknown {
  if (req) {
    // Both: the MCP SDK's Node transport builds its Request from rawHeaders (Hono's listener), other paths from headers.
    req.headers["user-agent"] = UNKNOWN_CLIENT.userAgent;
    let found = false;
    for (let i = 0; i + 1 < req.rawHeaders.length; i += 2)
      if (req.rawHeaders[i].toLowerCase() === "user-agent") {
        req.rawHeaders[i + 1] = UNKNOWN_CLIENT.userAgent;
        found = true;
      }
    if (!found) req.rawHeaders.push("User-Agent", UNKNOWN_CLIENT.userAgent);
  }
  const one = (m: unknown): unknown => {
    if (!m || typeof m !== "object" || Array.isArray(m)) return m;
    const msg = structuredClone(m) as { method?: unknown; params?: Record<string, unknown> };
    const p = msg.params;
    if (!p || typeof p !== "object") return msg;
    if (msg.method === "initialize" && p.clientInfo && typeof p.clientInfo === "object") {
      p.clientInfo = { ...(p.clientInfo as Record<string, unknown>), name: UNKNOWN_CLIENT.name, title: UNKNOWN_CLIENT.title };
    }
    const meta = p._meta;
    if (meta && typeof meta === "object" && !Array.isArray(meta)) {
      const kept: Record<string, unknown> = {};
      for (const [k, v] of Object.entries(meta as Record<string, unknown>)) {
        if (k === "progressToken") kept[k] = v;
        else if (k === "io.modelcontextprotocol/clientInfo" && v && typeof v === "object") kept[k] = { ...(v as Record<string, unknown>), name: UNKNOWN_CLIENT.name, title: UNKNOWN_CLIENT.title };
        else if (k.startsWith("io.modelcontextprotocol/")) kept[k] = v;
      }
      p._meta = kept;
    }
    if (msg.method === "tools/call" && p.arguments && typeof p.arguments === "object" && !Array.isArray(p.arguments)) {
      const { harness: _claim, ...rest } = p.arguments as Record<string, unknown>;
      p.arguments = rest;
    }
    return msg;
  };
  return Array.isArray(body) ? body.map(one) : one(body);
}

export function benchApp(o: Options): Server {
  const wake = o.wake;
  const wait = o.wait ?? benchWait();
  const now = o.now ?? Date.now;
  const iso = () => new Date(now()).toISOString();
  const calls: CallRecord[] = [];
  const events: EventRecord[] = [];
  const seen: SeenRecord[] = [];
  const subs = new Map<string, Set<string>>(); // lane → subscription IDs
  const nonce = new Map<string, string>(); // lane → the 4 characters in its event codes
  const counter = new Map<string, number>();
  // S16, S17: each lane's options, and how many jobs start_job started (its side effect).
  const laneOpts = new Map<string, LaneOptions>();
  const jobs = new Map<string, number>();
  const transports = new Map<string, { lane: string; transport: NodeStreamableHTTPServerTransport; inflight: Map<RequestId, InFlight> }>();
  // WB_CLAUDE_TRANSPORT=task-spike: each lane's session wait URL and harness
  // (from its watch result), and the 2026-07-28 front that serves the wait
  // task (app/tasks.ts, the October 29 spike).
  const laneWait = new Map<string, string>();
  const laneHarness = new Map<string, string | null | undefined>();
  const taskArm = process.env.WB_CLAUDE_TRANSPORT === "task-spike";
  // WB_CLAUDE_TRANSPORT=task (or WB_MCP_2026=1): protocol 2026-07-28
  // requests are served the way an app on a stateless transport serves them
  // (a server per request, the SDK's attach), so a client that takes MCP
  // tasks gets the SDK's wait tool and Wakeshift's instructions for it:
  // the product's path. Handshake-era clients keep their sessions.
  const serve2026 = process.env.WB_CLAUDE_TRANSPORT === "task" || process.env.WB_MCP_2026 === "1";
  const front = new TaskFront({
    waitUrl: (lane) => laneWait.get(lane),
    backend: async (lane, body, headers) => {
      const a = http.address();
      const port = typeof a === "object" && a ? a.port : 0;
      const r = await fetch(`http://127.0.0.1:${port}/mcp/${lane}`, {
        method: "POST",
        headers: { "content-type": "application/json", accept: "application/json, text/event-stream", ...headers },
        body: JSON.stringify(body),
      });
      return { status: r.status, headers: r.headers, text: await r.text() };
    },
  });
  // Protocol 2026-07-28 requests (stateless, below): a server per request,
  // for the lane the request is to.
  const modernCall = new AsyncLocalStorage<ModernCall>();
  const makeServer = () => {
    const c = modernCall.getStore();
    if (!c) throw new Error("no lane for this MCP request");
    c.server = mcpServer(c.lane, c.pending);
    return c.server;
  };
  const modern = createMcpHandler(makeServer, { legacy: "reject", responseMode: "json" });
  // The conformance lane answers on a stream when a request writes
  // notifications (responseMode "auto"), as push (events/stream) needs: in
  // JSON mode the SDK can't stream, so it doesn't list push.
  const modernStreaming = createMcpHandler(makeServer, { legacy: "reject", responseMode: "auto" });
  // One event for every lane: { run, lane } picks bench/<run>/<lane>, the
  // topic the delivery SQL (main.ts, src/core.ts) reads. Both are topic
  // segments: run IDs are <date>-<time>-<hex>, lanes [a-z0-9-].
  const benchEvent = wake.defineEvent("bench.event", {
    description: "A bench test event for one lane of one run",
    args: z.object({ run: z.string(), lane: z.string() }),
    payload: z.object({ code: z.string(), seq: z.number().int(), sent_at: z.string(), tag: z.string().optional() }),
    text: (p) => benchEventText(p.code, p.tag),
    topic: "bench/{run}/{lane}",
    public: true,
  });
  // A second event, for the conformance suite only: terminating an event
  // type (events_conformance_terminate) is one-shot for the fixture, so the
  // suite terminates a type nothing else in the run depends on. Never
  // emitted. Its topic is outside bench/: under it, bench.event's
  // bench/{run}/{lane} could render the same topic (a lane named "spare"),
  // and the core refuses a manifest whose topics can overlap.
  const spareEvent = !EVENTS_CONFORMANCE ? undefined : wake.defineEvent("bench.spare", {
    description: "A spare bench event the conformance suite may end (never emitted)",
    args: z.object({}),
    payload: z.object({ code: z.string() }),
    text: (p) => p.code,
    topic: `bench-spare/${o.run}`,
    public: true,
  });
  const argsOf = (lane: string) => ({ run: o.run, lane });
  const topic = (lane: string) => `bench/${o.run}/${lane}`;

  const laneNonce = (lane: string) => {
    let n = nonce.get(lane);
    if (!n) {
      n = codeNonce(o.run, lane, o.slot ?? 1);
      nonce.set(lane, n);
    }
    return n;
  };

  function record(lane: string, tool: string, server: McpServer | null, args: Record<string, unknown>, extra: Partial<CallRecord>, result?: CallToolResult, client = "") {
    const text = textOf(result?.content ?? []);
    const meta = (result?._meta ?? {}) as Record<string, unknown>;
    calls.push({
      at: iso(),
      tool,
      lane,
      client: server?.server.getClientVersion()?.name ?? client,
      args: {
        harness: typeof args.harness === "string" ? args.harness : null,
        conversation_id: typeof args.conversation_id === "string" ? args.conversation_id : null,
        session_key: typeof args.session_key === "string" && args.session_key ? short(args.session_key) : null,
        ...(Array.isArray(args.ids) ? { ids: args.ids.map(String) } : {}),
      },
      events_in_result: [...new Set(text.match(CODE) ?? [])],
      beacon: Object.keys(meta).some((k) => k.startsWith("wakeshift")) || /wakeshift-beacon:/.test(text),
      ...extra,
    });
  }

  function mcpServer(lane: string, inflight: Map<RequestId, InFlight>): McpServer {
    const server = new McpServer({ name: o.serverName ?? "wakebench", version: "0.1.0" });
    // Every caller of a lane's server is that lane's user: statusOf reads
    // each subscription with it.
    // Its lanes use only their own tools, so attach adds none of its own.
    const attached = wake.attach(server, { identify: () => ({ user: lane }), builtinTools: { subscribe: false, list: false, unsubscribe: false, wait } });
    // note leaves what a handler knows for its call's record, which finish
    // (in mcp) writes once the wrapper has added Wakeshift's block.
    const note = (call: ToolCall<ServerContext, never>, n: NonNullable<InFlight["note"]>) => {
      const f = inflight.get(call.ctx.mcpReq.id);
      if (f) f.note = n;
    };

    attached.registerTool(
      "watch",
      { description: "Wake this conversation when a bench event is published.", inputSchema: z.object({}), subscribes: [benchEvent] },
      async (_args, _ctx, call) => {
        const sub = await call.subscribe(benchEvent, argsOf(lane), { ttl: "1d" });
        const created = sub.state === "created" || sub.state === "renewed" ? sub : undefined;
        if (!subs.has(lane)) subs.set(lane, new Set());
        if (created) subs.get(lane)!.add(created.id);
        const recognized = sub.state === "unavailable" || sub.state === "disabled" ? undefined : sub.recognized;
        note(call, { extra: { agent: recognized, subscription: created?.id, session: created ? short(created.session) : undefined }, harness: recognized?.harness, own: 1 });
        return { content: [{ type: "text", text: WATCHING }] };
      },
    );
    attached.registerTool(
      "unwatch",
      { description: "Stop waking this conversation for bench events.", inputSchema: z.object({}), subscribes: [] },
      async (_args, _ctx, call) => {
        const ended = await call.unsubscribe(benchEvent, argsOf(lane));
        note(call, { extra: {}, own: 1 });
        return { content: [{ type: "text", text: ended ? "Stopped watching bench events." : "This conversation wasn't watching bench events." }] };
      },
    );
    attached.registerTool(
      "report_seen",
      {
        description: "Tell the bench which event codes (WB-…) you've received since you last reported.",
        inputSchema: z.object({ ids: z.array(z.string()).describe("The event codes, e.g. WB-ab12-3") }),
        subscribes: [],
      },
      async ({ ids }, _ctx, call) => {
        seen.push({ at: iso(), lane, ids });
        note(call, { extra: {}, own: 1 });
        return { content: [{ type: "text", text: `Noted ${ids.length} event(s).` }] };
      },
    );
    // The conformance suite's fixture control (its events helpers:
    // events_conformance_terminate {name}), on the conformance lane only:
    // ends every live subscription the lane's user has to the event, as the
    // app's own end does, so a push stream gets terminated and then its
    // final result. Gap and upstream-error controls aren't offered: the core
    // makes a gap only for content it can't send (too large, deleted), which
    // an app can't cause, and it never sends notifications/events/error.
    if (EVENTS_CONFORMANCE && lane === CONFORMANCE_LANE)
      attached.registerTool(
        "events_conformance_terminate",
        { description: "Conformance fixture control: end every subscription to an event type.", inputSchema: z.object({ name: z.string() }), subscribes: [] },
        async ({ name }) => {
          const ev = [benchEvent, spareEvent].find((e) => e?.name === name);
          if (!ev) return { content: [{ type: "text", text: `no event ${name}` }], isError: true };
          // The user's subscriptions, by the app API the SDK's own listing uses
          // (requestApp is the SDK's internal call: the bench's fixture only).
          const app = wake as unknown as { requestApp<T>(method: string, path: string, body?: unknown): Promise<T> };
          const listed = await app.requestApp<{ subscriptions?: { id: string; event: string }[] }>("POST", "/event-subscriptions/list", { user: lane });
          let ended = 0;
          for (const s of listed.subscriptions ?? []) if (s.event === name && (await ev.end(lane, s.id))) ended++;
          return { content: [{ type: "text", text: `ended ${ended}` }] };
        },
      );
    // S16, S17: a vendor's status tool and a tool with a side effect that subscribes, on the lanes that asked for them.
    const extra = laneOpts.get(lane)?.tools ?? [];
    if (extra.includes("job_status"))
      attached.registerTool("job_status", { description: JOB_STATUS_DESCRIPTION, inputSchema: z.object({}), subscribes: [] }, async (_args, _ctx, call) => {
        const latest = events.filter((e) => e.lane === lane).at(-1);
        note(call, { extra: {}, own: 1 });
        return { content: [{ type: "text", text: jobStatusText(latest && { code: latest.code, tag: latest.tag, at: latest.at }, jobs.get(lane) ?? 0) }] };
      });
    if (extra.includes("start_job"))
      attached.registerTool("start_job", { description: START_JOB_DESCRIPTION, inputSchema: z.object({}), subscribes: [benchEvent] }, async (_args, _ctx, call) => {
        // The side effect first, as a real job would start whatever Wakeshift then does.
        const n = (jobs.get(lane) ?? 0) + 1;
        jobs.set(lane, n);
        const sub = await call.subscribe(benchEvent, argsOf(lane), { ttl: "1d" });
        const created = sub.state === "created" || sub.state === "renewed" ? sub : undefined;
        if (!subs.has(lane)) subs.set(lane, new Set());
        if (created) subs.get(lane)!.add(created.id);
        const recognized = sub.state === "unavailable" || sub.state === "disabled" ? undefined : sub.recognized;
        note(call, { extra: { agent: recognized, subscription: created?.id, session: created ? short(created.session) : undefined }, harness: recognized?.harness, own: 1 });
        return { content: [{ type: "text", text: startedText(n) }] };
      });
    return server;
  }

  async function mcp(lane: string, req: IncomingMessage, res: ServerResponse) {
    const sid = req.headers["mcp-session-id"];
    const body = req.method === "POST" ? await readJson(req) : undefined;
    if (process.env.WB_MCP_LOG) {
      // Methods and protocol versions only: never arguments or results.
      const b = (Array.isArray(body) ? body[0] : body) as { method?: string; params?: { _meta?: Record<string, unknown> } } | undefined;
      const caps = JSON.stringify(b?.params?._meta?.["io.modelcontextprotocol/clientCapabilities"] ?? null);
      appendFileSync(process.env.WB_MCP_LOG, `${iso()} ${lane} ${req.method} ${b?.method ?? "-"} header=${req.headers["mcp-protocol-version"] ?? "-"} meta=${b?.params?._meta?.["io.modelcontextprotocol/protocolVersion"] ?? "-"} caps=${caps} accept=${req.headers.accept ?? "-"} method-header=${req.headers["mcp-method"] ?? "-"}\n`);
      res.on("finish", () => appendFileSync(process.env.WB_MCP_LOG!, `${iso()} ${lane} -> ${res.statusCode} ${b?.method ?? "-"} ${String(res.getHeader("content-type") ?? "")}\n`));
    }
    // S16: the lane's requests look like an agent Wakeshift doesn't recognize
    // (the call record keeps what the agent sent: `harness` is its claim).
    const unknown = laneOpts.get(lane)?.unrecognized === true;
    const sent = unknown ? disguise(body, req) : body;
    if (taskArm && isModern(body)) return front.handle(lane, req, res, sent as Record<string, unknown>);
    if (serve2026 && req.method === "POST" && isModern(body)) return stateless(lane, req, res, body, sent);
    const known = typeof sid === "string" ? transports.get(sid) : undefined;
    // A session belongs to the lane that created it: another lane's URL
    // with its ID is refused, as an unknown session would be.
    if (known && known.lane !== lane) return json(res, 404, { jsonrpc: "2.0", error: { code: -32001, message: "no such MCP session for this lane" }, id: null });
    let t = known?.transport;
    let inflight = known?.inflight;
    if (!t || !inflight) {
      if (req.method !== "POST" || !isInitializeRequest(body)) {
        return json(res, 400, { jsonrpc: "2.0", error: { code: -32000, message: "no MCP session: initialize first" }, id: null });
      }
      const pending = new Map<RequestId, InFlight>();
      const transport: NodeStreamableHTTPServerTransport = new NodeStreamableHTTPServerTransport({
        sessionIdGenerator: () => randomUUID(),
        onsessioninitialized: (id) => {
          transports.set(id, { lane, transport, inflight: pending });
        },
      });
      transport.onclose = () => {
        if (transport.sessionId) transports.delete(transport.sessionId);
      };
      const server = mcpServer(lane, pending);
      // The record and withTransport need the final result, the one the
      // client receives: the wrapper adds Wakeshift's block after a handler
      // returns, and refuses a registered callback that isn't its own, so
      // neither can wrap the callback. Every tool result leaves through the
      // transport, and is finished there on its way out.
      const send = transport.send.bind(transport);
      transport.send = (message, options) => send(finish(lane, server, pending, message), options);
      await server.connect(transport);
      t = transport;
      inflight = pending;
    }
    // Results are matched to calls by request ID. Requests sharing an ID
    // while one is in flight (a client reusing one, or an earlier call that
    // never got a response) can't be told apart, so none of them is
    // recorded: a result is never put down to the wrong call.
    for (const m of Array.isArray(body) ? body : [body]) {
      if (!isJSONRPCRequest(m)) continue;
      const f = inflight.get(m.id);
      if (f) {
        f.open++;
        f.clash = true;
        continue;
      }
      const p = (m.params ?? {}) as { name?: unknown; arguments?: unknown };
      if (m.method !== "tools/call" || typeof p.name !== "string") continue;
      const args = p.arguments && typeof p.arguments === "object" && !Array.isArray(p.arguments) ? (p.arguments as Record<string, unknown>) : {};
      inflight.set(m.id, { tool: p.name, args, open: 1 });
    }
    await t.handleRequest(req, res, sent);
  }

  // stateless serves one protocol 2026-07-28 request on a server of its own
  // (no session), as an app serves them: through the MCP SDK's handler for
  // that revision, which makes the lane's server (modernCall says which
  // lane, and keeps the server for finish). Its JSON reply is finished here,
  // as a session's messages are in its transport.
  async function stateless(lane: string, req: IncomingMessage, res: ServerResponse, body: unknown, sent: unknown = body) {
    const pending = new Map<RequestId, InFlight>();
    for (const m of Array.isArray(body) ? body : [body]) {
      if (!isJSONRPCRequest(m) || m.method !== "tools/call") continue;
      const p = (m.params ?? {}) as { name?: unknown; arguments?: unknown; _meta?: Record<string, unknown> };
      if (typeof p.name !== "string") continue;
      const args = p.arguments && typeof p.arguments === "object" && !Array.isArray(p.arguments) ? (p.arguments as Record<string, unknown>) : {};
      const info = p._meta?.["io.modelcontextprotocol/clientInfo"] as { name?: unknown } | undefined;
      pending.set(m.id, { tool: p.name, args, open: 1, client: typeof info?.name === "string" ? info.name : "" });
    }
    const call: ModernCall = { lane, pending };
    const gone = new AbortController();
    res.on("close", () => gone.abort());
    const handler = EVENTS_CONFORMANCE && lane === CONFORMANCE_LANE ? modernStreaming : modern;
    const r = await modernCall.run(call, async () => handler.fetch(await toWebRequest(req, sent, { signal: gone.signal }), { parsedBody: sent }));
    const headers = Object.fromEntries(r.headers);
    if ((r.headers.get("content-type") ?? "").startsWith("text/event-stream")) {
      // A stream (subscriptions/listen): passed on as it comes. Tool calls
      // are answered with JSON (responseMode), never on a stream.
      res.writeHead(r.status, headers);
      if (r.body) for await (const chunk of r.body) res.write(chunk);
      res.end();
      return;
    }
    let text = await r.text();
    if (call.server && text && (r.headers.get("content-type") ?? "").startsWith("application/json")) {
      const m = JSON.parse(text) as JSONRPCMessage | JSONRPCMessage[];
      text = JSON.stringify(Array.isArray(m) ? m.map((x) => finish(lane, call.server!, pending, x)) : finish(lane, call.server, pending, m));
    }
    delete headers["content-length"];
    res.writeHead(r.status, headers);
    res.end(text);
  }

  // finish records a tool call from its final result, Wakeshift's block
  // added, and for watch rewrites the Monitor line (withTransport). A
  // handler that threw left no note, so its call isn't recorded.
  function finish(lane: string, server: McpServer, inflight: Map<RequestId, InFlight>, m: JSONRPCMessage): JSONRPCMessage {
    if ("method" in m || !("id" in m) || m.id === undefined || !inflight.has(m.id)) return m;
    const f = inflight.get(m.id)!;
    if (--f.open <= 0) inflight.delete(m.id);
    if (!("result" in m) || !f.note || f.clash) return m;
    let result = m.result as CallToolResult;
    // What Wakeshift added: the blocks after the handler's own.
    const added = result.isError ? "" : textOf((result.content ?? []).slice(f.note.own));
    const subscribing = f.tool === "watch" || f.tool === "start_job";
    if (subscribing) laneHarness.set(lane, f.note.harness);
    if (subscribing || taskArm) result = withTransport(result, laneHarness.get(lane), undefined, (u) => laneWait.set(lane, u));
    record(lane, f.tool, server, f.args, { ...f.note.extra, added: redactPlain(added) }, result, f.client);
    return { ...m, result };
  }

  // WB_MCP_SDK=mastra: the MCP route on a Mastra MCPServer (server-mastra.ts),
  // with the same tools, event and records.
  const mastra =
    process.env.WB_MCP_SDK === "mastra"
      ? mastraRoute({
          wake,
          benchEvent,
          argsOf,
          subs,
          seenPush: (lane, ids) => void seen.push({ at: iso(), lane, ids }),
          record: (lane, tool, args, extra, result, client) => record(lane, tool, null, args, extra as Partial<CallRecord>, result, client),
          finishWatch: (lane, harness, result) => {
            laneHarness.set(lane, harness);
            return withTransport(result, harness, undefined, (u) => laneWait.set(lane, u));
          },
          readJson,
          log: (line) => console.error(line),
          serverName: o.serverName,
          wait,
        })
      : null;

  async function control(url: URL, req: IncomingMessage, res: ServerResponse) {
    if (!authorized(req, o.controlToken)) return json(res, 401, { error: { code: "unauthenticated", message: "control token" } });
    const lane = url.searchParams.get("lane") ?? "";
    if (req.method === "POST" && url.pathname === "/control/publish") {
      const b = (await readJson(req)) as { lane?: string; count?: number; tag?: string };
      if (!b?.lane || !/^[a-z0-9-]+$/.test(b.lane)) return json(res, 400, { error: { code: "bad_request", message: "lane" } });
      const n = b.count === undefined ? 1 : b.count;
      if (typeof n !== "number" || !Number.isInteger(n) || n < 1 || n > 20) return json(res, 400, { error: { code: "bad_request", message: "count is a whole number from 1 to 20" } });
      const out: EventRecord[] = [];
      for (let i = 0; i < n; i++) {
        const seq = (counter.get(b.lane) ?? 0) + 1;
        counter.set(b.lane, seq);
        const code = `WB-${laneNonce(b.lane)}-${seq}`;
        const at = iso();
        const id = `${o.run}:${b.lane}:${seq}`;
        const p = await benchEvent.emit(argsOf(b.lane), { code, seq, sent_at: at, ...(b.tag ? { tag: b.tag } : {}) }, { id });
        const e: EventRecord = { id, code, lane: b.lane, at, accepted_at: iso(), matched: p.matched, ...(b.tag ? { tag: b.tag } : {}) };
        events.push(e);
        out.push(e);
      }
      return json(res, 200, { events: out });
    }
    if (req.method === "POST" && url.pathname === "/control/options") {
      // S16, S17 (ts-v2 only): the Mastra route doesn't take them, and says so.
      if (mastra) return json(res, 501, { error: { code: "not_implemented", message: "lane options need the ts-v2 toy app (not WB_MCP_SDK=mastra)" } });
      const b = (await readJson(req)) as { lane?: string; unrecognized?: unknown; tools?: unknown };
      if (!b?.lane || !/^[a-z0-9-]+$/.test(b.lane)) return json(res, 400, { error: { code: "bad_request", message: "lane" } });
      const tools = b.tools === undefined ? [] : b.tools;
      if (!Array.isArray(tools) || tools.some((t) => !JOB_TOOLS.includes(t as JobTool))) return json(res, 400, { error: { code: "bad_request", message: `tools are some of ${JOB_TOOLS.join(", ")}` } });
      if (b.unrecognized !== undefined && typeof b.unrecognized !== "boolean") return json(res, 400, { error: { code: "bad_request", message: "unrecognized is true or false" } });
      const opts: LaneOptions = { unrecognized: b.unrecognized === true, tools: tools as JobTool[] };
      laneOpts.set(b.lane, opts);
      return json(res, 200, { lane: b.lane, options: opts });
    }
    if (req.method === "GET" && url.pathname === "/control/lane") {
      let deliveries: DeliveryRow[] | null = null;
      if (o.deliveries) {
        try {
          deliveries = await o.deliveries(wake.appId ?? "", topic(lane));
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
        // S16, S17: the lane's options, and the jobs start_job started.
        ...(laneOpts.has(lane) ? { options: laneOpts.get(lane), jobs: jobs.get(lane) ?? 0 } : {}),
      });
    }
    if (req.method === "GET" && url.pathname === "/control/status") {
      const t = await benchEvent.status(argsOf(lane));
      const ids = [...(subs.get(lane) ?? [])];
      let each: (SubscriptionStatus | { id: string; error: string })[];
      try {
        each = await benchEvent.statusOf(ids.map((id) => ({ user: lane, subscription: id })));
      } catch (err) {
        each = ids.map((id) => ({ id, error: redactPlain(String(err)) }));
      }
      return json(res, 200, { event: t, subscriptions: each });
    }
    return json(res, 404, { error: { code: "not_found", message: "no such control call" } });
  }

  const http = createServer((req, res) => {
    const url = new URL(req.url ?? "/", "http://bench");
    const m = /^\/mcp\/([a-z0-9-]{1,40})$/.exec(url.pathname);
    const work = m ? (mastra ? mastra(m[1], req, res) : mcp(m[1], req, res)) : url.pathname.startsWith("/control/") ? control(url, req, res) : Promise.resolve(json(res, url.pathname === "/health" ? 200 : 404, { ok: url.pathname === "/health" }));
    work.catch((err) => {
      if (!res.headersSent) json(res, 500, { error: { code: "internal", message: redactPlain(String(err)) } });
      else res.end();
    });
  });
  return http;
}

function textOf(content: CallToolResult["content"]): string {
  return content.map((c) => (c.type === "text" ? c.text : "")).filter((t) => t !== "").join("\n");
}

function authorized(req: IncomingMessage, token: string): boolean {
  const got = Buffer.from(String(req.headers.authorization ?? ""));
  const want = Buffer.from(`Bearer ${token}`);
  return got.length === want.length && timingSafeEqual(got, want);
}

function json(res: ServerResponse, status: number, body: unknown) {
  res.writeHead(status, { "content-type": "application/json" });
  res.end(JSON.stringify(body));
}

export async function readJson(req: IncomingMessage): Promise<unknown> {
  const chunks: Buffer[] = [];
  let size = 0;
  for await (const c of req) {
    size += (c as Buffer).length;
    if (size > 1 << 20) throw new Error("body too large");
    chunks.push(c as Buffer);
  }
  const s = Buffer.concat(chunks).toString("utf8");
  return s ? JSON.parse(s) : undefined;
}
