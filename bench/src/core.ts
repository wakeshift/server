// The core a run talks to, and the toy app in front of it.
//
// local:   builds this checkout's server and connector, makes a database of
//          its own on the dev Postgres (make db, port 5439), runs the server
//          on a free port with dev sign-in, registers an app (wakebench) as
//          a developer would in the console, and keeps its key in a 0600 file.
// Other targets (a hosted core) are extensions (src/extensions.ts).
// The toy app (bench/app) runs as a child on 127.0.0.1. S15 runs it twice,
// the second on its own app (wakebench2 on a local core), checked to be
// another app before either key is used.

import { createHash, randomBytes } from "node:crypto";
import { chmodSync, createWriteStream, existsSync, mkdirSync, readFileSync, statSync, writeFileSync } from "node:fs";
import { connect, createServer, type Server, type Socket } from "node:net";
import { join } from "node:path";
import { execFile } from "node:child_process";
import { promisify } from "node:util";
import pg from "pg";
import { app2FromEnv, appFromEnv, DEFAULT_APP, isPort, preparePort, variant, type AppId, type PortCmd } from "../app/variants.js";
import type { Build, CiCall, CiClient, CiEvent, CiForm, CiStream } from "../app/ci.js";
import { CONNECTOR_CLIENT, startConnectorRoute } from "./connectorroute.js";
import { ext } from "./extensions.js";
import { describeExternal, startExternal, type ExternalDescription } from "./external.js";
import type { Procs } from "./procs.js";
import { redactPlain } from "./redact.js";
import { sleep } from "./tmux.js";

const exec = promisify(execFile);

export const DEV_ADMIN_DB = "postgres://postgres:postgres@localhost:5439/postgres?sslmode=disable";
const DEV_SECRET = "dev-secret-key-at-least-32-bytes-long";

export interface Bench {
  target: string;
  core: string;
  /** The toy app's base URL; a lane's MCP server is <app>/mcp/<lane>. */
  app: string;
  mcpUrl(lane: string): string;
  publish(lane: string, count?: number, tag?: string): Promise<Published[]>;
  /**
   * Local cores only: one event's delivery stages so far, from the core's
   * database and the bench's state log (TRANSITIONS_SQL), on the core's
   * clock: created, may go out (pacing), first held by a waiter
   * (in_flight), confirmed. One read, no waiting. Null where there's no
   * database to read (a hosted core).
   */
  trace?(eventId: string): Promise<DeliveryTrace | null>;
  lane(lane: string): Promise<LaneState>;
  status(lane: string): Promise<StatusState>;
  /**
   * S16, S17: a lane's options on the toy app (its job tools, and whether it
   * looks unrecognized), set before its harness connects. Only the ts-v2 toy
   * app has them; it throws elsewhere (a port, the Mastra route), and the
   * fake target has none.
   */
  options?(lane: string, o: { unrecognized?: boolean; tools?: ("job_status" | "start_job")[] }): Promise<void>;
  stop(): Promise<void>;
  /** The first app's ID, as the core said (GET /app). */
  appId?: string;
  /** S15's second toy app, on its own key (a local core registers it). */
  app2?: ToyApp;
  /** Which toy app each is (WB_APP, WB_APP_2: app/variants.ts). */
  variant?: AppId;
  variant2?: AppId;
  /**
   * The plugin marketplace the core's offers name for Codex and Copilot
   * CLI: a local core's own (agentPluginsFor, unless WB_CURL_INSTALL=1), or
   * a hosted core's (its target's marketplace); null where the core offers
   * Wakeshift's installer instead.
   */
  agentPlugins: string | null;
  /**
   * A local core's plugin upgrade (S19, S19h): renders the marketplace into
   * a directory of a lane's own, first as published (`from`), then as this
   * run's core renders it, pinned to a newer connector (`to`). Undefined on
   * a hosted core.
   */
  pluginUpgrade?: PluginUpgrade;
  /**
   * Antigravity CLI's plugin for a local core: a checkout of
   * wakeshift/antigravity-plugin rendered for it (antigravityPluginFor,
   * unless WB_CURL_INSTALL=1), which the core's offers name; null otherwise.
   */
  antigravityPlugin: string | null;
  /** The realistic tier's app (app/ci.ts), in a run of realistic scenarios; the run then has no toy app. */
  ci?: CiApi;
  /** The MCP server's name in each harness (default wakebench): an external app's own (WB_EXTERNAL_APP). */
  mcpName?: string;
  /** An external app (WB_EXTERNAL_APP, src/external.ts): a vendor's server in place of the toy app, and the user's words for it. */
  external?: ExternalDescription;
  /** An external app: the texts of its events that name these codes, as the core has them (not the agent's words). */
  eventTexts?(codes: string[]): Promise<string[]>;
}

/** The realistic tier's CI app, as the bench drives it (its control API, which no agent sees). */
export interface CiApi {
  /** The app's ID, as the core said (GET /app). */
  appId: string;
  base: string;
  mcpUrl(lane: string): string;
  /** A running build finishes (the newest running one, if no number): build.finished is emitted. */
  finish(lane: string, status: "passed" | "failed" | "canceled", number?: number): Promise<CiEvent>;
  lane(lane: string): Promise<CiLaneState>;
  /** build.finished's status for one build: subscriptions, connected, via. */
  status(lane: string, number: number): Promise<StatusState>;
}

/** The CI app's record of a lane: its tool calls, the events emitted and its builds. */
export interface CiLaneState {
  calls: CiCall[];
  events: CiEvent[];
  builds: Build[];
  subscriptions: string[];
  /** The lane's events/stream requests (MCP Events push), oldest first. */
  streams?: CiStream[];
  /** The forms the app's SDK sent during calls, and the answers (WB_CI_STATEFUL=1), oldest first. */
  forms?: CiForm[];
  /** The clients that started sessions, and what they declared (WB_CI_STATEFUL=1), oldest first. */
  clients?: CiClient[];
  /** Whether the app keeps MCP sessions (WB_CI_STATEFUL=1): only then can a form go out. */
  stateful?: boolean;
}

/** S19's two renders of the plugin marketplace (Bench.pluginUpgrade); each returns the connector version it pins. */
export interface PluginUpgrade {
  /** The git ref `from` renders (WB_S19_FROM, default PUBLISHED_PLUGIN_REF). */
  fromRef: string;
  /** The git ref `to` renders: the core's own (WB_AGENT_PLUGINS_REF), or null for this checkout. */
  toRef: string | null;
  from(dir: string): Promise<string>;
  to(dir: string): Promise<string>;
}

/**
 * The commit production's plugins in wakeshift/agent-plugins were rendered
 * from (release v1.4.35, Claude Code's 2026.11.07.0303+5b418e55da6a, with
 * its issuer option): rendered with the same pin, it's the published
 * plugin byte for byte. S19 starts from it, and WB_AGENT_PLUGINS_REF set to
 * it is the baseline that measures the plugin users have today.
 */
export const PUBLISHED_PLUGIN_REF = "f1923a8b1f4d";

/** A git ref from the environment, checked to be one (no option, no spaces). */
export function gitRef(name: string, env: NodeJS.ProcessEnv = process.env): string | null {
  const r = (env[name] ?? "").trim();
  if (!r) return null;
  if (r.startsWith("-") || !/^[0-9A-Za-z._/-]+$/.test(r)) throw new Error(`${name}: not a git ref: ${r}`);
  return r;
}

/** The marketplace a harness's plugin command takes for a run's core (Bench.agentPlugins); refSep joins a repository and a branch. */
export function marketplaceSource(b: Pick<Bench, "target" | "agentPlugins">, refSep: string): string | null {
  const t = ext.targets.get(b.target);
  if (t?.marketplace) return t.marketplace(refSep);
  return b.agentPlugins;
}

/** Where `agy plugin install` takes Antigravity CLI's plugin from for a run's core. */
export function antigravitySource(b: Pick<Bench, "target" | "antigravityPlugin">): string | null {
  return ext.targets.get(b.target)?.antigravityPlugin ?? b.antigravityPlugin;
}

/** A toy app of the run: what S15 drives its second app through. */
export interface ToyApp {
  /** The app's ID, as the core said (GET /app). */
  appId: string;
  /** Its base URL; a lane's MCP server is <app>/mcp/<lane>. */
  app: string;
  mcpUrl(lane: string): string;
  publish(lane: string, count?: number, tag?: string): Promise<Published[]>;
  lane(lane: string): Promise<LaneState>;
  status(lane: string): Promise<StatusState>;
  options?: Bench["options"];
}

/** The second app's MCP server name in each harness (next to wakebench). */
export const SECOND_MCP_NAME = "wakebench2";
/**
 * Why the second app's key can't be used (null: it can). Its app must
 * differ from the first's, and the run's target may have rules of its own.
 * The IDs come from GET /app, which can't change a manifest, before anything
 * else is sent.
 */
export function secondAppProblem(o: { target: string; first: string; second: string; options?: Record<string, string> }): string | null {
  if (o.second === o.first) return `the second key is for ${o.second}, the same app as the first: S15 needs two different apps`;
  return ext.targets.get(o.target)?.secondAppProblem?.({ first: o.first, second: o.second, options: o.options ?? {} }) ?? null;
}

/** Why a toy app variant can't run on a key's app (null: it can): the run's target's rule, if it has one. */
export function variantProblem(o: { target: string; variant: AppId; app: string }): string | null {
  return ext.targets.get(o.target)?.variantProblem?.({ variant: o.variant, app: o.app }) ?? null;
}

/** Why the CI app can't run on a key's app (null: it can): the run's target's rule, if it has one. */
export function ciAppProblem(o: { target: string; app: string; options?: Record<string, string> }): string | null {
  return ext.targets.get(o.target)?.ciAppProblem?.({ app: o.app, options: o.options ?? {} }) ?? null;
}

export interface Published {
  /** The app's event ID (<run>:<lane>:<seq>). */
  id: string;
  code: string;
  /** When the app called publish, and when Wakeshift answered. */
  at: string;
  accepted_at?: string;
  matched: number;
}

export interface DeliveryTrace {
  created_at: string | null;
  available_at: string | null;
  /** When it went in_flight (recorded by the state log, not inferred from the confirmation). */
  handed_at: string | null;
  finished_at: string | null;
  state: string | null;
  attempts: number | null;
}

/**
 * The bench's record of delivery state changes, in the run's own throwaway
 * database (local cores only): a trigger writes each change of a delivery's
 * state with the time it happened (clock_timestamp()), so the hand-off time
 * is when the delivery went in_flight, recorded by the core's own
 * transaction, however quickly it was confirmed afterwards. The core's own
 * tables are only read; the trigger runs as the table's owner (SECURITY
 * DEFINER), so the core's role needs no new grants.
 */
export const TRANSITIONS_SQL = `
CREATE TABLE IF NOT EXISTS wakebench_delivery_states (
    delivery_id uuid NOT NULL,
    event_id    uuid NOT NULL,
    state       text NOT NULL,
    at          timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE OR REPLACE FUNCTION wakebench_record_state() RETURNS trigger LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
BEGIN
    IF TG_OP = 'INSERT' OR NEW.state IS DISTINCT FROM OLD.state THEN
        INSERT INTO wakebench_delivery_states (delivery_id, event_id, state) VALUES (NEW.id, NEW.event_id, NEW.state);
    END IF;
    RETURN NULL;
END $$;
DROP TRIGGER IF EXISTS wakebench_record_state ON deliveries;
CREATE TRIGGER wakebench_record_state AFTER INSERT OR UPDATE OF state ON deliveries FOR EACH ROW EXECUTE FUNCTION wakebench_record_state();
`;

/** One delivery's stages from its state changes (first in_flight = handed; first acked/delivered = confirmed). */
export function traceOf(row: { created_at: Date | string | null; available_at: Date | string | null; state: string | null; attempts: number | null } | undefined, changes: { state: string; at: Date | string }[]): DeliveryTrace | null {
  if (!row) return null;
  const iso = (d: Date | string | null | undefined) => (d ? new Date(d).toISOString() : null);
  const first = (states: string[]) => changes.filter((c) => states.includes(c.state)).map((c) => new Date(c.at).getTime()).sort((a, b) => a - b)[0];
  const handed = first(["in_flight"]);
  // Delivered to a consumer that can't acknowledge skips in_flight: that write is the hand-off.
  const finished = first(["acked", "delivered"]);
  return {
    created_at: iso(row.created_at),
    available_at: iso(row.available_at),
    handed_at: handed !== undefined ? iso(new Date(handed)) : finished !== undefined && !changes.some((c) => c.state === "acked") ? iso(new Date(finished)) : null,
    finished_at: finished !== undefined ? iso(new Date(finished)) : null,
    state: row.state,
    attempts: row.attempts,
  };
}

/** Reads one delivery's stages from the core's database (a superuser connection to the run's own database). */
export function deliveryTracer(adminUrl: string, appId: string): { install: () => Promise<void>; trace: (eventId: string) => Promise<DeliveryTrace | null>; close: () => Promise<void> } {
  const pool = new pg.Pool({ connectionString: adminUrl, max: 4 });
  const install = async () => {
    await pool.query(TRANSITIONS_SQL);
  };
  // One read, no waiting: the state log already holds every change so far.
  const trace = async (eventId: string): Promise<DeliveryTrace | null> => {
    try {
      const r = await pool.query(
        `SELECT d.id, d.created_at, d.available_at, d.state, d.attempts
           FROM topic_events te JOIN deliveries d ON d.event_id = te.event_id
          WHERE te.app_id = $1 AND te.source_id = $2 ORDER BY d.created_at LIMIT 1`,
        [appId, eventId],
      );
      const row = r.rows[0];
      if (!row) return null;
      const ch = await pool.query(`SELECT state, at FROM wakebench_delivery_states WHERE delivery_id = $1 ORDER BY at`, [row.id]);
      return traceOf(row, ch.rows);
    } catch {
      return null;
    }
  };
  return { install, trace, close: () => pool.end() };
}

export interface LaneState {
  calls: {
    at: string;
    tool: string;
    client: string;
    args: { harness: string | null; conversation_id: string | null; session_key: string | null; ids?: string[] };
    agent?: { harness: string | null; via: string | null; claimed: string | null };
    subscription?: string;
    session?: string;
    added?: string;
    beacon?: boolean;
    events_in_result: string[];
    /** An external app's calls (src/external.ts): whether the tool starts a job, checks one, or neither; whether it subscribes; where Wakeshift's part was in the result. */
    role?: "start" | "status" | "other";
    subscribes?: boolean;
    shape?: string;
    mismatch?: boolean;
    chars?: number;
    is_error?: boolean;
  }[];
  events: { code: string; at: string; matched: number; tag?: string }[];
  seen: { at: string; ids: string[] }[];
  subscriptions: string[];
  deliveries: { event: string; state: string; attempts: number; created_at: string; finished_at: string | null; subscription: string }[] | null;
  /** S16, S17: the lane's options, and how many jobs start_job started (the side effect, counted by the app). */
  options?: { unrecognized?: boolean; tools?: string[] };
  jobs?: number;
}

/** The toy app's /control/status: bench.event's status for the lane, and statusOf its subscriptions. */
export interface StatusState {
  event: { subscriptions: number; connected: number; via: Record<string, number> };
  subscriptions: { id: string; user?: string; live?: boolean; connected?: boolean; via?: string; error?: string }[];
}

/** WB_CORE_LATENCY_MS: milliseconds each way between the bench's clients and a local core (0: none; at most 2000). */
export function coreLatencyMs(env: NodeJS.ProcessEnv = process.env): number {
  const n = Number(env.WB_CORE_LATENCY_MS ?? 0);
  return Number.isFinite(n) && n > 0 ? Math.min(Math.round(n), 2000) : 0;
}

/**
 * A TCP proxy on 127.0.0.1:port to 127.0.0.1:target that holds every chunk
 * ms before passing it on, each way: one queue per direction, drained in
 * order (the end and the close too). It doesn't keep the process alive.
 */
export function delayProxy(port: number, target: number, ms: number): Server {
  const relay = (from: Socket, to: Socket) => {
    const queue: { due: number; act: () => void }[] = [];
    let timer: NodeJS.Timeout | null = null;
    const pump = () => {
      timer = null;
      while (queue.length && queue[0].due <= Date.now()) queue.shift()!.act();
      if (queue.length) timer = setTimeout(pump, queue[0].due - Date.now());
    };
    const later = (act: () => void) => {
      queue.push({ due: Date.now() + ms, act });
      if (!timer) timer = setTimeout(pump, ms);
    };
    from.on("data", (b) => later(() => to.writable && to.write(b)));
    from.on("end", () => later(() => to.end()));
    from.on("error", () => to.destroy());
    from.on("close", () => later(() => to.destroy()));
  };
  const srv = createServer((c) => {
    const u = connect(target, "127.0.0.1");
    relay(c, u);
    relay(u, c);
  });
  srv.listen(port, "127.0.0.1");
  srv.unref();
  return srv;
}

export async function freePort(): Promise<number> {
  return new Promise((resolve, reject) => {
    const s = createServer();
    s.listen(0, "127.0.0.1", () => {
      const a = s.address();
      const port = typeof a === "object" && a ? a.port : 0;
      s.close(() => resolve(port));
    });
    s.on("error", reject);
  });
}

async function waitHttp(url: string, ms: number, alive: () => boolean): Promise<void> {
  const end = Date.now() + ms;
  while (Date.now() < end) {
    if (!alive()) throw new Error(`exited before ${url} answered`);
    try {
      const r = await fetch(url);
      if (r.ok) return;
    } catch {
      // Not yet.
    }
    await sleep(250);
  }
  throw new Error(`${url} didn't answer within ${ms / 1000}s`);
}

/**
 * The run's database: wakebench_<run>, within Postgres's 63-byte identifier
 * limit (it would truncate silently, and the cleanup would miss it). A run
 * ID too long for that is cut and given a hash, so names stay distinct.
 */
export function dbName(run: string): string {
  const clean = run.replace(/[^a-z0-9]/gi, "_").toLowerCase();
  const name = "wakebench_" + clean;
  if (name.length <= 63) return name;
  const hash = createHash("sha256").update(run).digest("hex").slice(0, 10);
  return `wakebench_${clean.slice(0, 63 - 10 - 11)}_${hash}`;
}

/** Checks a file holding a secret is readable only by its owner. */
export function checkKeyFile(path: string): void {
  const st = statSync(path);
  if ((st.mode & 0o077) !== 0) throw new Error(`${path} must be readable only by you (chmod 600)`);
}

export interface StartOptions {
  /** local, or a target an extension added (src/extensions.ts). */
  target: string;
  run: string;
  dir: string;
  repo: string;
  procs: Procs;
  /** The command-line options extensions added (--name: value), for their target. */
  options?: Record<string, string>;
  /** S15's second toy app: a local core registers a second app (wakebench2) for it. */
  second?: boolean;
  /** A local core serves this checkout's plugin for OpenClaw (built here). */
  openclaw?: boolean;
  /**
   * A run of the realistic tier: the CI app (app/ci.ts) instead of the toy
   * app, on an app a local core registers (shipyard), or the target's.
   */
  ci?: boolean;
  /** An external app's adapter (WB_EXTERNAL_APP, src/external.ts) in place of the toy app: local core only. */
  external?: string;
  log: (msg: string) => void;
}

/**
 * Which app a key is, from the toy app run with BENCH_CHECK_APP=1: it calls
 * GET /app (which can't change the manifest) and exits, before anything
 * else is sent. The key goes by its file's path in the environment, never
 * on a command line, and is never printed.
 */
export async function keyApp(repo: string, core: string, keyFile: string): Promise<string> {
  try {
    const { stdout } = await exec(process.execPath, [join(repo, "bench/dist/app/main.js")], {
      env: { PATH: process.env.PATH ?? "", HOME: process.env.HOME ?? "", BENCH_KEY_FILE: keyFile, WAKESHIFT_CORE: core, BENCH_CHECK_APP: "1" },
      timeout: 60_000,
      maxBuffer: 1 << 20,
    });
    const line = stdout.trim().split("\n").reverse().find((x) => x.startsWith("{"));
    const app = line ? (JSON.parse(line) as { app?: unknown }).app : undefined;
    if (typeof app !== "string" || !app) throw new Error("no app in its answer");
    return app;
  } catch (err) {
    const e = err as { stderr?: string; message?: string };
    throw new Error(`couldn't tell which app ${keyFile} is for: ${redactPlain(String(e.stderr || e.message || err)).slice(0, 400)}`);
  }
}

/** Starts the core (local) and the toy app (and S15's second one). */
export async function startBench(o: StartOptions): Promise<Bench> {
  mkdirSync(o.dir, { recursive: true, mode: 0o700 });
  const keyFile = join(o.dir, "app.key");
  const keyFile2 = join(o.dir, "app2.key");
  let core: string;
  let coreDb: string | undefined;
  let dropDb: (() => Promise<void>) | undefined;
  let agentPlugins: string | null = null;
  let antigravityPlugin: string | null = null;
  let pluginUpgrade: PluginUpgrade | undefined;
  if (o.external) {
    // A vendor's own server in place of the toy app, on an app the local core registers under the vendor's name.
    if (o.target !== "local") throw new Error("WB_EXTERNAL_APP runs on a local core (--target local): the app is registered under the vendor's name, and presence is read from the core's database");
    const d = await describeExternal(o.external);
    const local = await startLocalCore(o, keyFile, null, { id: d.name, name: d.title });
    return startExternal(o, { cmd: o.external, d, core: local.core, keyFile, adminUrl: local.adminUrl, agentPlugins: local.agentPlugins, antigravityPlugin: local.antigravityPlugin, dropDb: local.drop });
  }
  if (o.target === "local") {
    const local = await startLocalCore(o, keyFile, o.second ? keyFile2 : null);
    core = local.core;
    agentPlugins = local.agentPlugins;
    antigravityPlugin = local.antigravityPlugin;
    pluginUpgrade = local.pluginUpgrade;
    coreDb = local.adminUrl;
    dropDb = local.drop;
  } else {
    const t = ext.targets.get(o.target);
    if (!t) throw new Error(`unknown target ${o.target}`);
    core = await t.start(o, keyFile, keyFile2);
  }
  const started: { pid?: number; alive: () => boolean }[] = [];
  // WB_CONNECTOR_ROUTE=1's proxy, stopped with everything else.
  let route: Awaited<ReturnType<typeof startConnectorRoute>> | null = null;
  let tracer: ReturnType<typeof deliveryTracer> | null = null;
  const stop = async () => {
    if (route) await route.stop().catch(() => {});
    if (tracer) await tracer.close().catch(() => {});
    for (const a of started) if (a.alive() && a.pid) o.procs.kill(a.pid, "toy app");
    if (dropDb) await dropDb().catch(() => {});
  };
  try {
    if (o.ci) {
      // The realistic tier: the CI app alone, on an app the target allows.
      const appId = await keyApp(o.repo, core, keyFile);
      const why = ciAppProblem({ target: o.target, app: appId, options: o.options });
      if (why) throw new Error(`the CI app: ${why}`);
      const controlFile = join(o.dir, "control.token");
      const control = randomBytes(24).toString("base64url");
      writeFileSync(controlFile, control + "\n", { mode: 0o600 });
      const one = await spawnToyApp(o, { core, keyFile, controlFile, control, log: "app.log", name: "CI app", variant: DEFAULT_APP, portCmd: null, env: { BENCH_APP: "ci", BENCH_REQUIRE_APP: appId } });
      started.push(one);
      o.log(`CI app (${appId}) on ${one.base} (core ${core})`);
      const ci: CiApi = {
        appId,
        base: one.base,
        mcpUrl: (lane) => `${one.base}/mcp/${lane}`,
        finish: async (lane, status, number) => (await one.call<{ event: CiEvent }>("POST", "/control/finish", { lane, status, ...(number !== undefined ? { number } : {}) })).event,
        lane: (lane) => one.call<CiLaneState>("GET", `/control/lane?lane=${encodeURIComponent(lane)}`),
        status: (lane, number) => one.call<StatusState>("GET", `/control/status?lane=${encodeURIComponent(lane)}&number=${number}`),
      };
      const none = () => Promise.reject(new Error("a realistic run has no toy app"));
      return { target: o.target, core, agentPlugins, antigravityPlugin, appId, app: one.base, mcpUrl: ci.mcpUrl, publish: none, lane: none, status: none, ci, stop };
    }
    const variant = appFromEnv();
    const variant2 = app2FromEnv();
    // Which app each key is, before either is used: S15's second app must be
    // another app, and the target may restrict which apps a port runs on.
    let appId: string | undefined;
    let appId2: string | undefined;
    if (o.second || variant !== DEFAULT_APP) appId = await keyApp(o.repo, core, keyFile);
    if (o.second) {
      appId2 = await keyApp(o.repo, core, keyFile2);
      const why = secondAppProblem({ target: o.target, first: appId!, second: appId2, options: o.options });
      if (why) throw new Error(`S15's second app: ${why}`);
      o.log(`second app: ${appId2} (first: ${appId})`);
    }
    for (const [v, id, which] of [[variant, appId, "the first"], [variant2, appId2, "S15's second"]] as const) {
      const why = id ? variantProblem({ target: o.target, variant: v, app: id }) : null;
      if (why) throw new Error(`${which} toy app: ${why}`);
    }
    const binDir = join(o.dir, "toy");
    mkdirSync(binDir, { recursive: true, mode: 0o700 });
    const portCmd = isPort(variant) ? await preparePort(variant, o.repo, binDir) : null;
    const portCmd2 = o.second && isPort(variant2) ? await preparePort(variant2, o.repo, binDir) : null;
    if (variant !== DEFAULT_APP || (o.second && variant2 !== DEFAULT_APP)) o.log(`toy app: ${variant}${o.second ? `, second: ${variant2}` : ""}`);
    const controlFile = join(o.dir, "control.token");
    const control = randomBytes(24).toString("base64url");
    writeFileSync(controlFile, control + "\n", { mode: 0o600 });
    chmodSync(controlFile, 0o600);

    tracer = coreDb ? deliveryTracer(coreDb, "wakebench") : null;
    if (tracer) await tracer.install();
    const one = await spawnToyApp(o, { core, coreDb, keyFile, controlFile, control, log: "app.log", name: "toy app", variant, portCmd });
    started.push(one);
    o.log(`toy app on ${one.base} (core ${core})`);
    let app2: ToyApp | undefined;
    if (o.second && appId && appId2) {
      const two = await spawnToyApp(o, {
        core,
        coreDb,
        keyFile: keyFile2,
        controlFile,
        control,
        log: "app2.log",
        name: "second toy app",
        variant: variant2,
        portCmd: portCmd2,
        env: { BENCH_APP_SLOT: "2", BENCH_MCP_NAME: SECOND_MCP_NAME, BENCH_REQUIRE_APP: appId2, BENCH_FORBID_APP: appId },
      });
      started.push(two);
      o.log(`second toy app (${appId2}) on ${two.base}`);
      app2 = { appId: appId2, ...two.api };
    }
    // WB_CONNECTOR_ROUTE=1: the app as a claude.ai connector reaches the
    // harness (connectorroute.ts), in front of the first app.
    if (process.env.WB_CONNECTOR_ROUTE === "1") {
      route = await startConnectorRoute(one.api.mcpUrl);
      o.log(`WB_CONNECTOR_ROUTE: the toy app through a claude.ai-connector-style route (client "${CONNECTOR_CLIENT}", no call ID, result _meta trimmed)`);
    }
    return {
      target: o.target,
      core,
      agentPlugins,
      antigravityPlugin,
      ...(pluginUpgrade ? { pluginUpgrade } : {}),
      ...one.api,
      ...(route ? { mcpUrl: route.mcpUrl } : {}),
      ...(tracer ? { trace: tracer.trace } : {}),
      ...(appId ? { appId } : {}),
      ...(app2 ? { app2 } : {}),
      variant,
      ...(app2 ? { variant2 } : {}),
      stop,
    };
  } catch (err) {
    await stop();
    throw err;
  }
}

/** Runs one toy app (bench/app/main.js) and waits for it; returns its control API. */
async function spawnToyApp(
  o: StartOptions,
  a: { core: string; coreDb?: string; keyFile: string; controlFile: string; control: string; log: string; name: string; env?: Record<string, string>; variant: AppId; portCmd: PortCmd | null },
): Promise<{ base: string; pid?: number; alive: () => boolean; api: Omit<ToyApp, "appId">; call: <T>(method: string, path: string, body?: unknown) => Promise<T> }> {
  const port = await freePort();
  const log = createWriteStream(join(o.dir, a.log));
  const app = o.procs.spawn(
    process.execPath,
    [join(o.repo, "bench/dist/app/main.js")],
    {
      env: {
        PATH: process.env.PATH ?? "",
        HOME: process.env.HOME ?? "",
        BENCH_KEY_FILE: a.keyFile,
        BENCH_CONTROL_FILE: a.controlFile,
        WAKESHIFT_CORE: a.core,
        BENCH_RUN: o.run,
        PORT: String(port),
        ...(a.coreDb ? { BENCH_CORE_DB: a.coreDb } : {}),
        ...(process.env.WB_CLAUDE_TRANSPORT ? { WB_CLAUDE_TRANSPORT: process.env.WB_CLAUDE_TRANSPORT } : {}),
        ...(process.env.WB_CHANNEL_LINK_FILE ? { WB_CHANNEL_LINK_FILE: process.env.WB_CHANNEL_LINK_FILE } : {}),
        ...(process.env.WB_MCP_2026 ? { WB_MCP_2026: process.env.WB_MCP_2026 } : {}),
        ...(process.env.WB_WAIT ? { WB_WAIT: process.env.WB_WAIT } : {}),
        ...(process.env.WB_MCP_LOG ? { WB_MCP_LOG: process.env.WB_MCP_LOG } : {}),
        ...(process.env.WB_CI_WATCH_DESCRIPTION ? { WB_CI_WATCH_DESCRIPTION: process.env.WB_CI_WATCH_DESCRIPTION } : {}),
        ...(process.env.WB_CI_PLUGIN_ASK ? { WB_CI_PLUGIN_ASK: process.env.WB_CI_PLUGIN_ASK } : {}),
        ...(process.env.WB_CI_INJECT ? { WB_CI_INJECT: process.env.WB_CI_INJECT } : {}),
        ...(process.env.WB_CI_LEGACY ? { WB_CI_LEGACY: process.env.WB_CI_LEGACY } : {}),
        ...(process.env.WB_CI_STATEFUL ? { WB_CI_STATEFUL: process.env.WB_CI_STATEFUL } : {}),
        // The target's own guards (src/extensions.ts), if it has any.
        ...(ext.targets.get(o.target)?.toyEnv ?? {}),
        // ts-v2 and ts-mastra are server.ts (ts-mastra by its env); the rest are ports behind front.ts.
        ...(variant(a.variant).env ?? {}),
        BENCH_APP: a.variant,
        ...(a.portCmd ? { BENCH_PORT_CMD: JSON.stringify(a.portCmd) } : {}),
        ...a.env,
      },
      stdio: ["ignore", "pipe", "pipe"],
    },
    a.name,
  );
  app.stdout!.pipe(log);
  app.stderr!.pipe(log);
  let alive = true;
  app.on("exit", () => (alive = false));
  const base = `http://127.0.0.1:${port}`;
  try {
    // A port starts its own process first (uv's first run can take a while).
    await waitHttp(`${base}/health`, a.portCmd ? 150_000 : 20_000, () => alive);
  } catch (err) {
    if (alive && app.pid) o.procs.kill(app.pid, a.name);
    throw new Error(`${a.name}: ${err instanceof Error ? err.message : String(err)} (see ${a.log})`);
  }

  const api = async <T>(method: string, path: string, body?: unknown): Promise<T> => {
    const r = await fetch(base + path, {
      method,
      headers: { authorization: `Bearer ${a.control}`, "content-type": "application/json" },
      body: body === undefined ? undefined : JSON.stringify(body),
    });
    const j = (await r.json()) as T & { error?: { message: string } };
    if (!r.ok) throw new Error(`${a.name} ${path}: ${r.status} ${j.error?.message ?? ""}`);
    return j;
  };
  return {
    base,
    pid: app.pid,
    alive: () => alive,
    call: api,
    api: {
      app: base,
      mcpUrl: (lane) => `${base}/mcp/${lane}`,
      publish: async (lane, count = 1, tag) => (await api<{ events: Published[] }>("POST", "/control/publish", { lane, count, tag })).events,
      lane: (lane) => api<LaneState>("GET", `/control/lane?lane=${encodeURIComponent(lane)}`),
      status: (lane) => api<StatusState>("GET", `/control/status?lane=${encodeURIComponent(lane)}`),
      options: async (lane, opts) => void (await api("POST", "/control/options", { lane, ...opts })),
    },
  };
}

async function startLocalCore(o: StartOptions, keyFile: string, keyFile2: string | null, app?: { id: string; name: string }) {
  const bin = join(o.dir, "bin");
  mkdirSync(join(bin, "connector"), { recursive: true });
  o.log("building the core and the connector (go build)");
  const goarch = process.arch === "arm64" ? "arm64" : "amd64";
  const goos = process.platform === "darwin" ? "darwin" : "linux";
  await exec("go", ["build", "-o", join(bin, "wakeshift"), "./cmd/wakeshift"], { cwd: o.repo, maxBuffer: 16 << 20 });
  // A release version (the plugins' launcher pins one, and installs over an
  // older connector): this minute's, as the Makefile's VERSION.
  const version = connectorVersion(new Date());
  for (const [os, arch] of [[goos, goarch], ["linux", goarch]]) {
    const out = join(bin, "connector", `wakeshift-${os}-${arch}`);
    if (existsSync(out) && os !== goos) continue;
    await exec("go", ["build", "-trimpath", "-ldflags", `-X ${goModule(o.repo)}/internal/version.Version=${version}`, "-o", out, "./cmd/wakeshift-connector"], {
      cwd: o.repo,
      env: { ...process.env, CGO_ENABLED: "0", GOOS: os, GOARCH: arch },
      maxBuffer: 16 << 20,
    });
  }
  if (o.openclaw) {
    // /install/openclaw.sh serves the plugin from the connector's directory.
    o.log("building the plugin for OpenClaw (npm run pack)");
    const dir = join(o.repo, "plugins", "openclaw");
    if (!existsSync(join(dir, "node_modules"))) await exec("npm", ["ci", "--no-fund", "--no-audit"], { cwd: dir, maxBuffer: 16 << 20 });
    await exec("npm", ["run", "pack"], { cwd: dir, env: { ...process.env, PACK_DIR: join(bin, "connector"), WAKESHIFT_VERSION: "dev" }, maxBuffer: 16 << 20 });
  }

  // A database of its own, owned by the dev role (row-level security applies).
  const name = dbName(o.run);
  const admin = new pg.Client({ connectionString: DEV_ADMIN_DB });
  try {
    await admin.connect();
  } catch (err) {
    throw new Error(`can't reach the dev Postgres on localhost:5439 (run make db): ${String(err)}`);
  }
  await admin.query(`CREATE DATABASE ${name} OWNER wakeshift`);
  await admin.end();
  const dbUrl = `postgres://wakeshift:wakeshift@localhost:5439/${name}?sslmode=disable`;
  const adminUrl = `postgres://postgres:postgres@localhost:5439/${name}?sslmode=disable`;

  // WB_CORE_PORT: a fixed port, for a package packed to trust this core
  // (opencode's npm plugin, packed with --issuer by plugins/pack-npm.mjs in
  // github.com/wakeshift/connectors).
  const port = Number(process.env.WB_CORE_PORT) || (await freePort());
  // WB_CORE_PUBLIC_PORT: the core's public URL (its issuer: what hooks and
  // harnesses reach) on another port than it listens on, where something
  // in between adds network latency to hooks' requests (a TCP proxy
  // of your own).
  const publicPort = Number(process.env.WB_CORE_PUBLIC_PORT) || port;
  const core = `http://localhost:${publicPort}`;
  const internal = `http://localhost:${port}`;
  // WB_CORE_LATENCY_MS: the core behind a proxy that holds every byte that
  // long each way, as the network to a hosted core does (a hook's redeem
  // takes hundreds of milliseconds there, a few on a local core).
  const latency = coreLatencyMs();
  const listen = latency ? await freePort() : port;
  // The plugin marketplace for this core (Codex's and Copilot's offers name it).
  // WB_AGENT_PLUGINS_REF: rendered from that commit's plugins instead
  // (PUBLISHED_PLUGIN_REF: the plugins production users have).
  const pluginsRef = gitRef("WB_AGENT_PLUGINS_REF");
  const agentPlugins = process.env.WB_CURL_INSTALL === "1" ? null : await agentPluginsFor(o, core, join(bin, "connector"), version, pluginsRef);
  // Antigravity CLI's plugin for this core (its offers name it).
  const antigravityPlugin = process.env.WB_CURL_INSTALL === "1" ? null : await antigravityPluginFor(o, core, join(bin, "connector"), version);
  const log = createWriteStream(join(o.dir, "core.log"));
  // The token dev sign-in must present (WAKESHIFT_DEV_SIGN_IN_TOKEN).
  const devToken = randomBytes(24).toString("hex");
  const srv = o.procs.spawn(
    join(bin, "wakeshift"),
    [],
    {
      cwd: o.dir,
      env: {
        PATH: process.env.PATH ?? "",
        HOME: process.env.HOME ?? "",
        DATABASE_URL: dbUrl,
        WAKESHIFT_SECRET_KEY: DEV_SECRET,
        WAKESHIFT_DEV_SIGN_IN: "1",
        WAKESHIFT_DEV_SIGN_IN_TOKEN: devToken,
        WAKESHIFT_CONNECTOR_DIR: join(bin, "connector"),
        WAKESHIFT_ADDR: `127.0.0.1:${listen}`,
        WAKESHIFT_PUBLIC_URL: core,
        WAKESHIFT_LOG_FORMAT: "text",
        // Redeem by call: the connector's redeem made by a tool call.
        ...(process.env.WAKESHIFT_REDEEM_BY_TOOL_CALL ? { WAKESHIFT_REDEEM_BY_TOOL_CALL: process.env.WAKESHIFT_REDEEM_BY_TOOL_CALL } : {}),
        ...(agentPlugins ? { WAKESHIFT_AGENT_PLUGINS: agentPlugins } : {}),
        // The version of opencode's npm plugin the offers give (else the installer).
        ...(process.env.WB_OPENCODE_PLUGIN ? { WAKESHIFT_OPENCODE_PLUGIN: process.env.WB_OPENCODE_PLUGIN } : {}),
        ...(process.env.WB_PI_PLUGIN ? { WAKESHIFT_PI_PLUGIN: process.env.WB_PI_PLUGIN } : {}),
        // And of OpenClaw's (@wakeshift/openclaw), with the issuer set first.
        ...(process.env.WB_OPENCLAW_PLUGIN ? { WAKESHIFT_OPENCLAW_PLUGIN: process.env.WB_OPENCLAW_PLUGIN } : {}),
        ...(antigravityPlugin ? { WAKESHIFT_ANTIGRAVITY_PLUGIN: antigravityPlugin } : {}),
        // The version of @wakeshift/connect Cursor's offers run with npx (else the installer).
        ...(process.env.WB_CONNECT_PACKAGE ? { WAKESHIFT_CONNECT_PACKAGE: process.env.WB_CONNECT_PACKAGE } : {}),
      },
      stdio: ["ignore", "pipe", "pipe"],
    },
    "core",
  );
  srv.stdout!.pipe(log);
  srv.stderr!.pipe(log);
  if (latency) {
    delayProxy(port, listen, latency);
    o.log(`the core is ${latency}ms away each way (WB_CORE_LATENCY_MS)`);
  }
  let alive = true;
  srv.on("exit", () => (alive = false));
  const drop = async () => {
    if (alive && srv.pid) o.procs.kill(srv.pid, "core");
    await sleep(1000);
    const c = new pg.Client({ connectionString: DEV_ADMIN_DB });
    await c.connect();
    await c.query(`DROP DATABASE IF EXISTS ${name} WITH (FORCE)`);
    await c.end();
  };
  // Anything failing from here on stops the core and drops its database.
  try {
    await waitHttp(`${internal}/health`, 60_000, () => alive);
    o.log(`core on ${core} (database ${name})`);

    // Register the app as a developer would: sign in, then Build → new app.
    const signin = await fetch(`${core}/api/v1/session/dev`, {
      method: "POST",
      headers: { origin: core, "content-type": "application/json", "x-wakeshift-dev-token": devToken },
      body: JSON.stringify({ email: "bench@example.com" }),
    });
    if (!signin.ok) throw new Error(`dev sign-in: ${signin.status}`);
    const cookie = (signin.headers.getSetCookie?.() ?? []).map((c) => c.split(";")[0]).join("; ");
    const register = async (id: string, appName: string, file: string) => {
      const made = await fetch(`${core}/api/v1/developer/apps`, {
        method: "POST",
        headers: { origin: core, "content-type": "application/json", cookie },
        body: JSON.stringify({ id, name: appName }),
      });
      const j = (await made.json()) as { key?: string };
      if (!made.ok || !j.key) throw new Error(`registering the app ${id}: ${made.status}`);
      writeFileSync(file, j.key + "\n", { mode: 0o600 });
    };
    // The realistic tier's run registers its CI app instead, named as its maker would.
    if (app) await register(app.id, app.name, keyFile);
    else if (o.ci) await register("shipyard", "Shipyard", keyFile);
    else await register("wakebench", "Wakebench", keyFile);
    // S15's second app, registered the same way.
    if (keyFile2) await register("wakebench2", "Wakebench 2", keyFile2);
  } catch (err) {
    await drop().catch(() => {});
    throw err;
  }
  // S19: the plugin as published, then as this core renders it, pinned
  // to a connector built a minute later (built when first needed).
  let next: Promise<string> | null = null;
  const nextConnector = () =>
    (next ??= (async () => {
      const v = connectorVersion(new Date(Date.now() + 60_000));
      const dir = join(bin, "connector-next");
      mkdirSync(dir, { recursive: true });
      await exec("go", ["build", "-trimpath", "-ldflags", `-X ${goModule(o.repo)}/internal/version.Version=${v}`, "-o", join(dir, `wakeshift-${goos}-${goarch}`), "./cmd/wakeshift-connector"], {
        cwd: o.repo,
        env: { ...process.env, CGO_ENABLED: "0", GOOS: goos, GOARCH: goarch },
        maxBuffer: 16 << 20,
      });
      o.log(`a newer connector for S19: ${v}`);
      return v;
    })());
  const fromRef = gitRef("WB_S19_FROM") ?? PUBLISHED_PLUGIN_REF;
  const pluginUpgrade: PluginUpgrade = {
    fromRef,
    toRef: pluginsRef,
    from: async (dir) => {
      await renderMarketplace(o, await pluginSource(o, fromRef), dir, core, join(bin, "connector"), version, "from");
      return version;
    },
    to: async (dir) => {
      const v = await nextConnector();
      await renderMarketplace(o, await pluginSource(o, pluginsRef), dir, core, join(bin, "connector-next"), v, "to");
      return v;
    },
  };
  return { core, internal, adminUrl, drop, agentPlugins, antigravityPlugin, pluginUpgrade };
}

/** The checkout's Go module (go.mod's module line), for the connector's -X version flag: an exported tree's differs. */
export function goModule(repo: string): string {
  const m = /^module\s+(\S+)\s*$/m.exec(readFileSync(join(repo, "go.mod"), "utf8"));
  if (!m) throw new Error(`no module line in ${join(repo, "go.mod")}`);
  return m[1];
}

/** A connector version in Wakeshift's scheme (YYYY.MM.DD.HHMM+bench), for a time. */
export function connectorVersion(d: Date): string {
  const p = (n: number) => String(n).padStart(2, "0");
  return `${d.getUTCFullYear()}.${p(d.getUTCMonth() + 1)}.${p(d.getUTCDate())}.${p(d.getUTCHours())}${p(d.getUTCMinutes())}+bench`;
}

/**
 * Renders the plugin marketplace (plugins/agent-plugins) for a local core:
 * its plugins trust the core and pin the connector just built, fetched from
 * its directory (file://), with its SHA-256; a platform not built gets a
 * checksum nothing matches. Returns the marketplace's directory.
 */
async function agentPluginsFor(o: StartOptions, core: string, connectorDir: string, version: string, ref: string | null): Promise<string> {
  const dir = join(o.dir, "agent-plugins");
  await renderMarketplace(o, await pluginSource(o, ref), dir, core, connectorDir, version, "core");
  o.log(`plugin marketplace for the core: ${dir}${ref ? ` (the plugins of ${ref}: WB_AGENT_PLUGINS_REF)` : ""}`);
  return dir;
}

/**
 * Renders the marketplace with the renderer in src (this checkout, or
 * another commit's: pluginSource) into dir, over what's there (as the
 * release renders over the published tree), every plugin pinned to the
 * connector in connectorDir.
 */
async function renderMarketplace(o: StartOptions, src: string, dir: string, core: string, connectorDir: string, version: string, name: string): Promise<void> {
  const sumsFile = connectorSums(o, connectorDir, `agent-plugins-${name}.SHA256SUMS`);
  await exec("go", ["run", "./cmd/wakeshift-agent-plugins", "-dir", dir, "-issuer", core, "-stamp", "all", "-version", version, "-release-url", `file://${connectorDir}`, "-sums", sumsFile], {
    cwd: src,
    maxBuffer: 16 << 20,
  });
}

/**
 * Where to run the plugin renderer from: this checkout, or (for a ref) that
 * commit's renderer and plugins, extracted once into the run's directory
 * (git archive: the module files, the renderer and what it imports).
 */
const sources = new Map<string, Promise<string>>();
function pluginSource(o: StartOptions, ref: string | null): Promise<string> {
  if (!ref) return Promise.resolve(o.repo);
  // One extraction per run directory and ref.
  const key = `${o.dir}\0${ref}`;
  let p = sources.get(key);
  if (!p) {
    p = (async () => {
      const dir = join(o.dir, `plugins-src-${ref.replace(/[^0-9A-Za-z._-]/g, "_")}`);
      mkdirSync(dir, { recursive: true });
      const tar = join(o.dir, `plugins-src-${ref.replace(/[^0-9A-Za-z._-]/g, "_")}.tar`);
      await exec("git", ["archive", "--format=tar", "-o", tar, ref, "--", "go.mod", "go.sum", "cmd/wakeshift-agent-plugins", "plugins/agent-plugins", "internal"], { cwd: o.repo, maxBuffer: 16 << 20 });
      await exec("tar", ["-xf", tar, "-C", dir], { maxBuffer: 16 << 20 });
      o.log(`the plugin renderer and plugins of ${ref}: ${dir}`);
      return dir;
    })();
    sources.set(key, p);
  }
  return p;
}

/**
 * Renders Antigravity CLI's plugin (plugins/agent-plugins/standalone/antigravity,
 * published as wakeshift/antigravity-plugin) for a local core, pinned as the
 * marketplace's plugins are. `agy plugin install` takes the folder.
 */
async function antigravityPluginFor(o: StartOptions, core: string, connectorDir: string, version: string): Promise<string> {
  const dir = join(o.dir, "antigravity-plugin");
  const sumsFile = connectorSums(o, connectorDir);
  await exec(
    "go",
    ["run", "./cmd/wakeshift-agent-plugins", "-plugin", "antigravity", "-dir", dir, "-issuer", core, "-stamp", "antigravity", "-version", version, "-release-url", `file://${connectorDir}`, "-sums", sumsFile],
    { cwd: o.repo, maxBuffer: 16 << 20 },
  );
  o.log(`Antigravity CLI's plugin for the core: ${dir}`);
  return dir;
}

/** The connector just built's SHA256SUMS, for the plugins' pins; a platform not built gets a checksum nothing matches. */
function connectorSums(o: StartOptions, connectorDir: string, name = "agent-plugins.SHA256SUMS"): string {
  const sums = ["darwin-amd64", "darwin-arm64", "linux-amd64", "linux-arm64"].map((p) => {
    const f = join(connectorDir, `wakeshift-${p}`);
    const sum = existsSync(f) ? createHash("sha256").update(readFileSync(f)).digest("hex") : "0".repeat(64);
    return `${sum}  wakeshift-${p}\n`;
  });
  const sumsFile = join(o.dir, name);
  writeFileSync(sumsFile, sums.join(""));
  return sumsFile;
}
