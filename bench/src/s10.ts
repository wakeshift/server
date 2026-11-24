// S10: a real MCP Events (SEP-3415) client against the toy app on a local
// core (bench/README.md, "S10: a spec client"). No agent and no model: the
// client is Mastra's MCPClient (@mastra/mcp, the bench's pinned version),
// written to the spec by its own authors rather than shaped to our server.
//
//   node dist/src/s10.js [--app ts-v2] [--results out.json] [--summary out.md]
//
// (in bench/, after npx tsc -p tsconfig.json, with WAKESHIFT_TEST_DATABASE_URL
// set as for the Go tests)
//
// It starts a core with Go's TestSDKHarness, as conformance.ts does (or uses
// WAKESHIFT_SDK_HARNESS's running one), and the toy app with its control API,
// then on one lane:
//
//   list           events/list offers bench.event, with webhook and poll
//   subscribe      a webhook subscription (events/subscribe), the client's
//                  receiver on loopback (the harness core allows it)
//   delivery       three published events reach the client in order through
//                  native webhook delivery, signatures verified by the client
//   refresh        subscribing again with the same key is an upsert: the
//                  next event is POSTed once, not twice
//   unsubscribe    events/unsubscribe ends it: the next event isn't POSTed
//                  (counted at the receiver, before the client sees it)
//   poll           a poll subscription from now gets the next event
//   replay (poll)  resumed from the cursor saved after that event, a new poll
//                  subscription gets the events published while none was open,
//                  in order, without truncated
//   replay (webhook, after a lapse)  a new webhook subscription given the
//                  cursor from before it lapsed gets what was published since,
//                  or says truncated
//
// Reporting-only where it runs on a schedule: it never fails a job. The exit
// code is 1 when a check that isn't marked known fails.

import { createServer, type Server } from "node:http";
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { createRequire } from "node:module";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { DEFAULT_APP, VARIANTS, type AppId } from "../app/variants.js";
import { controlApi, killGroup, startCore, startToy, type Toy } from "./conformance.js";
import { freePort } from "./core.js";
import { MCPClient, type MCPEventSubscription } from "@mastra/mcp";

const here = dirname(fileURLToPath(import.meta.url));
const benchDir = resolve(here, "../..");
const LANE = "s10";
const EVENT = "bench.event";
const WAIT_MS = 30_000;

export interface S10Check {
  name: string;
  ok: boolean;
  detail?: string;
  /** A failure the spec review already lists, with its item: reported, never counted against the run. */
  known?: string;
}

export interface S10Result {
  id: "S10";
  client: string;
  app: string;
  outcome: "pass" | "fail";
  checks: S10Check[];
}

interface Occurrence {
  eventId?: unknown;
  name?: unknown;
  data?: unknown;
}

type Subscription = MCPEventSubscription;

/** Reads occurrences off one subscription, with a deadline per read. */
export class Reader {
  private it: AsyncIterator<Occurrence>;
  /** A read that outlived the last take's deadline: the next take waits on it, so nothing is skipped or reordered. */
  private pending?: Promise<IteratorResult<Occurrence>>;
  constructor(sub: AsyncIterable<Occurrence>) {
    this.it = sub[Symbol.asyncIterator]();
  }
  /** The next n occurrences, or as many as arrived before the deadline. */
  async take(n: number, ms = WAIT_MS): Promise<Occurrence[]> {
    const out: Occurrence[] = [];
    const deadline = Date.now() + ms;
    while (out.length < n) {
      const left = deadline - Date.now();
      if (left <= 0) break;
      let timer: NodeJS.Timeout | undefined;
      this.pending ??= this.it.next();
      let r: IteratorResult<Occurrence> | "timeout";
      try {
        r = await Promise.race([this.pending, new Promise<"timeout">((res) => (timer = setTimeout(() => res("timeout"), left)))]);
      } finally {
        clearTimeout(timer);
      }
      if (r === "timeout") break;
      this.pending = undefined;
      if (r.done) break;
      out.push(r.value);
    }
    return out;
  }
}

/** The codes of occurrences, in the order they arrived. */
export const codesOf = (occ: Occurrence[]) => occ.map((o) => String((o.data as { code?: unknown } | undefined)?.code ?? "?"));

/** Whether got is want, in order. */
export const sameOrder = (got: string[], want: string[]) => got.length === want.length && got.every((c, i) => c === want[i]);

async function publish(t: Toy, count: number): Promise<string[]> {
  const r = await controlApi<{ events: { code: string }[] }>(t, "POST", "/control/publish", { lane: LANE, count, tag: "s10" });
  return r.events.map((e) => e.code);
}

export async function runS10(t: Toy, app: string): Promise<S10Result> {
  const require = createRequire(join(benchDir, "package.json"));
  const version = (require("@mastra/mcp/package.json") as { version: string }).version;
  const res: S10Result = { id: "S10", client: `@mastra/mcp ${version}`, app, outcome: "fail", checks: [] };
  const check = (name: string, ok: boolean, detail?: string, known?: string) => (res.checks.push({ name, ok, ...(detail ? { detail } : {}), ...(known && !ok ? { known } : {}) }), ok);
  const args = { run: t.run, lane: LANE };
  const port = await freePort();
  const callback = `http://127.0.0.1:${port}/mcp-events`;
  const mcp = new MCPClient({
    servers: { wake: { url: new URL(`${t.base}/mcp/${LANE}`) } },
    webhook: { url: callback },
    registerProcessHooks: false,
  });
  const errors: string[] = [];
  // Every signed POST the core makes to the receiver (verification, deliveries, envelopes), counted before the client sees it.
  let posts = 0;
  const receiver: Server = createServer(async (req, rsp) => {
    const chunks: Buffer[] = [];
    for await (const c of req) chunks.push(c as Buffer);
    if (req.method === "POST" && req.headers["webhook-id"]) posts++;
    const headers = new Headers();
    for (const [k, v] of Object.entries(req.headers)) if (typeof v === "string") headers.set(k, v);
    const r = await mcp.events.handleWebhook(new Request(new URL(req.url ?? "/", callback), { method: req.method, headers, body: req.method === "POST" ? Buffer.concat(chunks) : undefined }));
    rsp.writeHead(r.status, Object.fromEntries(r.headers));
    rsp.end(Buffer.from(await r.arrayBuffer()));
  });
  await new Promise<void>((r) => receiver.listen(port, "127.0.0.1", () => r()));
  const subs: Subscription[] = [];
  const track = (s: Subscription) => (subs.push(s), s.onError((e) => errors.push(`${e.id ?? "error"}: ${e.message ?? ""}`)), s);
  try {
    const list = await mcp.events.list("wake");
    const def = list.find((e) => e.name === EVENT);
    if (!check("list", !!def && def.delivery.includes("webhook") && def.delivery.includes("poll"), def ? `delivery: ${def.delivery.join(", ")}` : `no ${EVENT} in ${list.map((e) => e.name).join(", ")}`)) return res;

    // Webhook: subscribe, three deliveries in order, refresh, unsubscribe.
    const hook = track(await mcp.events.subscribe("wake", EVENT, args, { delivery: "webhook" }));
    if (!check("subscribe", hook.mode === "webhook", `mode ${hook.mode}`)) return res;
    const hookReader = new Reader(hook);
    const sent = await publish(t, 3);
    const got = codesOf(await hookReader.take(3));
    check("delivery", sameOrder(got, sent), `sent ${sent.join(", ")}; got ${got.join(", ") || "nothing"}`);
    const lapseCursor = hook.cursor;
    check("cursor", typeof lapseCursor === "string" && lapseCursor.length > 0, lapseCursor ? "a watermark after the last delivery" : "none");
    // The same key again is an upsert: one subscription, so one delivery per event, not two.
    const again = track(await mcp.events.subscribe("wake", EVENT, args, { delivery: "webhook" }));
    const before = posts;
    const once = await publish(t, 1);
    const onceGot = codesOf(await hookReader.take(1));
    await new Promise((r) => setTimeout(r, 2000));
    check("refresh", again.mode === "webhook" && sameOrder(onceGot, once) && posts - before === 1, `subscribed again with the same key; ${once[0]} POSTed ${posts - before} time(s)${again.deliveryStatus ? `, deliveryStatus.active ${again.deliveryStatus.active}` : ""}`);
    await again.cancel();
    await hook.cancel();
    const ended = posts;
    const afterEnd = await publish(t, 1);
    await new Promise((r) => setTimeout(r, 5000));
    check("unsubscribe", posts === ended, posts === ended ? `${afterEnd[0]} not POSTed` : `${afterEnd[0]} still POSTed (${posts - ended})`);

    // Poll from now, then replay from a saved cursor.
    const poll = track(await mcp.events.subscribe("wake", EVENT, args, { delivery: "poll", minPollIntervalMs: 500 }));
    const pollReader = new Reader(poll);
    const first = await publish(t, 1);
    const p1 = codesOf(await pollReader.take(1));
    check("poll", sameOrder(p1, first), `sent ${first[0]}; got ${p1.join(", ") || "nothing"}`);
    const saved = poll.cursor;
    await poll.cancel();
    const missed = await publish(t, 2);
    const resumed = track(await mcp.events.subscribe("wake", EVENT, args, { delivery: "poll", cursor: saved, minPollIntervalMs: 500 }));
    const p2 = codesOf(await new Reader(resumed).take(2));
    check("replay (poll)", !!saved && sameOrder(p2, missed) && !resumed.truncated, `missed ${missed.join(", ")}; resumed from the saved cursor and got ${p2.join(", ") || "nothing"}${resumed.truncated ? " (truncated)" : ""}`);
    await resumed.cancel();

    // Webhook after a lapse: every event since lapseCursor is owed, or truncated.
    const owed = [...once, ...afterEnd, ...first, ...missed];
    const back = track(await mcp.events.subscribe("wake", EVENT, args, { delivery: "webhook", cursor: lapseCursor }));
    const replayed = codesOf(await new Reader(back).take(owed.length, 10_000));
    check(
      "replay (webhook, after a lapse)",
      sameOrder(replayed, owed) || back.truncated,
      `owed ${owed.join(", ")}; got ${replayed.join(", ") || "nothing"}, truncated ${back.truncated}`,
    );
    await back.cancel();
  } catch (err) {
    check("run", false, err instanceof Error ? err.message : String(err));
  } finally {
    for (const s of subs) await s.cancel().catch(() => {});
    await mcp.disconnect().catch(() => {});
    await new Promise<void>((r) => receiver.close(() => r()));
    if (errors.length) res.checks.push({ name: "client errors", ok: true, detail: [...new Set(errors)].slice(0, 5).join("; ") });
  }
  res.outcome = res.checks.every((c) => c.ok || c.known) ? "pass" : "fail";
  return res;
}

export function s10Summary(r: S10Result): string {
  const lines = [`## S10: a spec client (${r.client}) against the toy app \`${r.app}\` on a local core: ${r.outcome}`, "", "| Check | Result | Detail |", "| --- | --- | --- |"];
  for (const c of r.checks) lines.push(`| ${c.name} | ${c.ok ? "pass" : c.known ? `known (${c.known.split(":")[0]})` : "fail"} | ${(c.detail ?? "").replace(/\|/g, "\\|")} |`);
  return lines.join("\n") + "\n";
}

async function main() {
  const argv = process.argv.slice(2);
  const flag = (name: string) => {
    const i = argv.indexOf(`--${name}`);
    return i >= 0 ? argv[i + 1] : undefined;
  };
  const app = (flag("app") ?? DEFAULT_APP) as AppId;
  const variant = VARIANTS.find((v) => v.id === app);
  if (!variant) throw new Error(`unknown app ${app}`);
  const work = mkdtempSync(join(tmpdir(), "wakebench-s10-"));
  const cleanups: (() => Promise<void>)[] = [];
  let toy: Toy | undefined;
  let r: S10Result;
  try {
    const h = await startCore(work, cleanups);
    toy = await startToy(variant, h, work);
    r = await runS10(toy, app);
  } catch (err) {
    // A setup failure is a result too, so the summary and --results still say what happened.
    r = { id: "S10", client: "@mastra/mcp", app, outcome: "fail", checks: [{ name: toy ? "run" : "start", ok: false, detail: err instanceof Error ? err.message.slice(0, 2000) : String(err) }] };
  } finally {
    if (toy) {
      const p = toy.proc;
      killGroup(p);
      await new Promise<void>((done) => (p.exitCode !== null ? done() : (p.once("exit", () => done()), setTimeout(done, 5000))));
    }
    for (const c of cleanups.reverse()) await c().catch(() => {});
  }
  const md = s10Summary(r);
  console.log(md);
  const out = flag("results");
  if (out) writeFileSync(out, JSON.stringify(r, null, 1) + "\n");
  const sum = flag("summary") ?? process.env.GITHUB_STEP_SUMMARY;
  if (sum) writeFileSync(sum, md, { flag: "a" });
  if (r.outcome === "fail") {
    console.log(`the toy app's log: ${toy?.log ?? "none"}\n${toy ? readFileSync(toy.log, "utf8").split("\n").slice(-20).join("\n") : ""}`);
    process.exitCode = 1;
  } else rmSync(work, { recursive: true, force: true });
}

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  main().catch((err) => {
    console.error(err instanceof Error ? err.message : String(err));
    process.exit(2);
  });
}
