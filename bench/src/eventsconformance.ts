// The official MCP Events (SEP-3415) server conformance suite against the
// toy app on a local core (bench/README.md, "MCP Events conformance"):
//
//   node dist/src/eventsconformance.js [--suite <checkout>] [--app ts-v2]
//        [--scenarios events-poll,events-webhook] [--results out.json] [--summary out.md]
//   make bench-events-conformance
//
// The suite is modelcontextprotocol/conformance#521, still open, run from its
// branch at the commit mcp-events/conformance.json pins (fetched and built
// into MCP_CONFORMANCE_DIR, or the OS temp dir, unless --suite names a built
// checkout). The core is Go's TestSDKHarness, as conformance.ts starts it
// (or WAKESHIFT_SDK_HARNESS's running one). The toy app serves bench.event
// {run, lane} on /mcp/<lane>, and a ticker publishes to the conformance lane
// every second so the suite sees occurrences. The harness core accepts http
// callbacks to loopback, which is how the suite's receiver gets deliveries;
// it is also why the SSRF rows that expect a loopback refusal fail here.
//
// Every check the suite emits is compared with mcp-events/conformance-expected.yaml,
// where each expected failure names the item of Wakeshift's spec-compliance plan
// that fixes it (or says why the bench can't grade it). The exit code is 1 on
// a failure not on the list, on an entry that now passes (so the list is
// shortened as items land), or on an entry the suite no longer emits. CI runs it as a non-blocking job.

import { spawn, spawnSync } from "node:child_process";
import { existsSync, mkdirSync, mkdtempSync, openSync, readdirSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { parse as parseYaml } from "yaml";
import { DEFAULT_APP, VARIANTS, type AppId } from "../app/variants.js";
import { controlApi, killGroup, startCore, startToy, type Toy } from "./conformance.js";

const here = dirname(fileURLToPath(import.meta.url));
const benchDir = resolve(here, "../..");
const pinFile = join(benchDir, "mcp-events/conformance.json");
const expectedFile = join(benchDir, "mcp-events/conformance-expected.yaml");

/** The suite's five server scenarios for Events. */
export const EVENTS_SCENARIOS = ["events-discovery", "events-poll", "events-push", "events-webhook", "events-webhook-delivery"] as const;
const LANE = "conformance";

export interface Pin {
  repo: string;
  pr: string;
  sha: string;
  sep: string;
}

export interface Check {
  id: string;
  scenario: string;
  status: string;
  message?: string;
}

export interface Expected {
  /** `<scenario>:<check id>`, as the suite's own baseline files write it (a check ID can repeat across scenarios). */
  check: string;
  /** The review item that fixes it ("P0 #2"), or "bench" or "suite" for a limit of the setup or the suite. */
  item: string;
  why: string;
  /** The toy app variants it applies to (WB_APP); every variant when absent. */
  apps?: string[];
}

export interface Outcome {
  check: Check;
  expected?: Expected;
  verdict: "pass" | "expected-fail" | "unexpected-fail" | "stale" | "warn" | "skip";
}

export function loadExpected(raw: string): Expected[] {
  const doc = parseYaml(raw) as { expected?: unknown };
  const list = Array.isArray(doc?.expected) ? doc.expected : [];
  const out: Expected[] = [];
  const seen = new Set<string>();
  for (const e of list) {
    const r = e as Partial<Expected>;
    if (!r || typeof r.check !== "string" || typeof r.item !== "string" || typeof r.why !== "string" || !r.item.trim() || !r.why.trim())
      throw new Error(`conformance-expected.yaml: each entry needs check, item and why: ${JSON.stringify(e)}`);
    if (r.apps !== undefined && (!Array.isArray(r.apps) || !r.apps.length || r.apps.some((a) => typeof a !== "string")))
      throw new Error(`conformance-expected.yaml: ${r.check}: apps is a non-empty list of variants`);
    const [scenario, id] = r.check.split(":");
    if (!id || !(EVENTS_SCENARIOS as readonly string[]).includes(scenario)) throw new Error(`conformance-expected.yaml: ${r.check} isn't <scenario>:<check id>`);
    // One entry per check and app: an entry for every app can't sit beside one for some.
    for (const a of r.apps ?? ["*"]) {
      const clash = a === "*" ? [...seen].some((k) => k.startsWith(`${r.check}@`)) : seen.has(`${r.check}@${a}`) || seen.has(`${r.check}@*`);
      if (clash) throw new Error(`conformance-expected.yaml: ${r.check} is listed twice${a === "*" ? "" : ` for ${a}`}`);
      seen.add(`${r.check}@${a}`);
    }
    out.push({ check: r.check, item: r.item, why: r.why, ...(r.apps ? { apps: r.apps } : {}) });
  }
  return out;
}

const keyOf = (c: Check) => `${c.scenario}:${c.id}`;

/** One verdict per check: SUCCESS passes; FAILURE fails unless expected for this app; an expected check that passes is stale. */
export function grade(checks: Check[], expected: Expected[], app: string): { outcomes: Outcome[]; missing: Expected[] } {
  const mine = expected.filter((e) => !e.apps || e.apps.includes(app));
  const byId = new Map(mine.map((e) => [e.check, e]));
  const outcomes = checks.map((check): Outcome => {
    const exp = byId.get(keyOf(check));
    if (check.status === "FAILURE") return { check, expected: exp, verdict: exp ? "expected-fail" : "unexpected-fail" };
    if (check.status === "SUCCESS") return { check, expected: exp, verdict: exp ? "stale" : "pass" };
    if (check.status === "WARNING") return { check, expected: exp, verdict: "warn" };
    return { check, expected: exp, verdict: "skip" };
  });
  const emitted = new Set(checks.map(keyOf));
  // Missing only from the scenarios that ran (--scenarios runs some).
  const ran = new Set(checks.map((c) => c.scenario));
  return { outcomes, missing: mine.filter((e) => ran.has(e.check.split(":")[0]) && !emitted.has(e.check)) };
}

/** The most severe status per check ID, as the suite collapses duplicates. */
export function collapse(checks: Check[]): Check[] {
  const rank = (s: string) => (s === "FAILURE" ? 3 : s === "WARNING" ? 2 : s === "SUCCESS" ? 1 : 0);
  const best = new Map<string, Check>();
  for (const c of checks) {
    if (c.status === "INFO") continue;
    const cur = best.get(keyOf(c));
    if (!cur || rank(c.status) >= rank(cur.status)) best.set(keyOf(c), c);
  }
  return [...best.values()];
}

export function summary(pin: Pin, app: string, outcomes: Outcome[], missing: Expected[]): string {
  const count = (v: Outcome["verdict"]) => outcomes.filter((o) => o.verdict === v).length;
  const lines = [
    `## MCP Events conformance (${pin.pr} at \`${pin.sha.slice(0, 7)}\`, ${pin.sep})`,
    "",
    `Against the toy app \`${app}\` on a local core. ${outcomes.length} checks: ${count("pass")} pass, ${count("expected-fail")} expected failures, ${count("unexpected-fail")} unexpected failures, ${count("stale")} expected but now passing, ${count("warn")} warnings, ${count("skip")} skipped.`,
    "",
  ];
  const bad = outcomes.filter((o) => o.verdict === "unexpected-fail" || o.verdict === "stale");
  if (bad.length) {
    lines.push("### Needs attention", "", "| Check | Scenario | Verdict | Message |", "| --- | --- | --- | --- |");
    for (const o of bad) lines.push(`| \`${o.check.id}\` | ${o.check.scenario} | ${o.verdict} | ${cell(o.check.message ?? o.expected?.why ?? "")} |`);
    lines.push("");
  }
  lines.push("### Every check", "", "| Check | Scenario | Status | Item | Note |", "| --- | --- | --- | --- | --- |");
  for (const o of [...outcomes].sort((a, b) => a.check.scenario.localeCompare(b.check.scenario) || a.check.id.localeCompare(b.check.id)))
    lines.push(`| \`${o.check.id}\` | ${o.check.scenario} | ${o.check.status}${o.verdict === "stale" ? " (stale)" : ""} | ${o.expected?.item ?? ""} | ${cell(o.expected?.why ?? (o.check.status === "SUCCESS" ? "" : (o.check.message ?? "")))} |`);
  if (missing.length) lines.push("", `Expected failures the suite didn't emit: ${missing.map((m) => `\`${m.check}\``).join(", ")}.`);
  return lines.join("\n") + "\n";
}

const cell = (s: string) => s.replace(/\|/g, "\\|").replace(/\s+/g, " ").slice(0, 300);

/**
 * A built checkout of the pinned suite (refused at any other commit): --suite (built already), or fetched and
 * built into MCP_CONFORMANCE_DIR (CI caches it) or the temp dir when it isn't there yet.
 */
function ensureSuite(pin: Pin, given?: string): string {
  const dir = given ?? process.env.MCP_CONFORMANCE_DIR ?? join(tmpdir(), `mcp-conformance-${pin.sha.slice(0, 12)}`);
  if (existsSync(join(dir, "dist/index.js"))) {
    const head = spawnSync("git", ["rev-parse", "HEAD"], { cwd: dir, encoding: "utf8" }).stdout?.trim();
    if (head !== pin.sha) throw new Error(`${dir} is at ${head || "no commit"}, not the pinned ${pin.sha}: ${given ? "check out the pin" : "remove it to fetch the pin"}`);
    return dir;
  }
  if (given) throw new Error(`${given} has no dist/index.js: build it (npm ci && npm run build)`);
  const run = (cmd: string, args: string[], cwd: string) => {
    const r = spawnSync(cmd, args, { cwd, stdio: ["ignore", "inherit", "inherit"] });
    if (r.status !== 0) throw new Error(`${cmd} ${args.join(" ")} failed (${r.status})`);
  };
  mkdirSync(dir, { recursive: true });
  run("git", ["init", "-q"], dir);
  run("git", ["fetch", "-q", "--depth", "1", `https://github.com/${pin.repo}.git`, pin.sha], dir);
  run("git", ["checkout", "-q", "FETCH_HEAD"], dir);
  const head = spawnSync("git", ["rev-parse", "HEAD"], { cwd: dir, encoding: "utf8" }).stdout.trim();
  if (head !== pin.sha) throw new Error(`fetched ${head}, not the pinned ${pin.sha}`);
  run("npm", ["ci", "--ignore-scripts", "--no-audit", "--no-fund"], dir);
  run("npm", ["run", "build"], dir);
  return dir;
}

function findChecks(dir: string): string[] {
  const out: string[] = [];
  for (const e of readdirSync(dir, { withFileTypes: true })) {
    const p = join(dir, e.name);
    if (e.isDirectory()) out.push(...findChecks(p));
    else if (e.name === "checks.json") out.push(p);
  }
  return out;
}

async function runScenario(suite: string, url: string, scenario: string, env: Record<string, string>, work: string): Promise<Check[]> {
  const out = join(work, scenario);
  mkdirSync(out, { recursive: true });
  const log = join(work, `${scenario}.log`);
  const fd = openSync(log, "a");
  const p = spawn(process.execPath, [join(suite, "dist/index.js"), "server", "--url", url, "--scenario", scenario, "--output-dir", out, "--timeout", "600000"], {
    env: { ...process.env, ...env },
    stdio: ["ignore", fd, fd],
  });
  // The suite exits 1 when a check fails; anything else (a crash, a signal) means its checks may be partial.
  const exit = await new Promise<{ code: number | null; signal: string | null }>((r) => p.once("exit", (code, signal) => r({ code, signal })));
  const files = findChecks(out);
  if (!files.length) return [{ id: `${scenario}:no-result`, scenario, status: "FAILURE", message: `the suite wrote no checks (log: ${readFileSync(log, "utf8").split("\n").slice(-8).join(" ")})` }];
  const raw = files.flatMap((f) => JSON.parse(readFileSync(f, "utf8")) as { id: string; status: string; errorMessage?: string }[]);
  // An empty result would make the scenario look ungraded rather than failed (grade() only counts scenarios that ran).
  if (!raw.some((c) => c.status !== "INFO")) return [{ id: `${scenario}:no-result`, scenario, status: "FAILURE", message: `the suite wrote no checks (log: ${readFileSync(log, "utf8").split("\n").slice(-8).join(" ")})` }];
  const checks = collapse(raw.map((c) => ({ id: c.id, scenario, status: c.status, message: c.errorMessage })));
  if (exit.signal || (exit.code !== 0 && exit.code !== 1)) checks.push({ id: `${scenario}:suite-exit`, scenario, status: "FAILURE", message: `the suite ended with ${exit.signal ?? `exit code ${exit.code}`}, so its checks may be partial` });
  return checks;
}

async function main() {
  const argv = process.argv.slice(2);
  const flag = (name: string) => {
    const i = argv.indexOf(`--${name}`);
    return i >= 0 ? argv[i + 1] : undefined;
  };
  const pin = JSON.parse(readFileSync(pinFile, "utf8")) as Pin;
  const expected = loadExpected(readFileSync(expectedFile, "utf8"));
  const app = (flag("app") ?? DEFAULT_APP) as AppId;
  const variant = VARIANTS.find((v) => v.id === app);
  if (!variant) throw new Error(`unknown app ${app}`);
  const scenarios = (flag("scenarios") ?? EVENTS_SCENARIOS.join(",")).split(",").map((s) => s.trim()).filter(Boolean);
  for (const s of scenarios) if (!(EVENTS_SCENARIOS as readonly string[]).includes(s)) throw new Error(`unknown scenario ${s}`);
  const suite = ensureSuite(pin, flag("suite"));

  const work = mkdtempSync(join(tmpdir(), "wakebench-events-conf-"));
  const cleanups: (() => Promise<void>)[] = [];
  let toy: Toy | undefined;
  let ticker: NodeJS.Timeout | undefined;
  const checks: Check[] = [];
  try {
    const h = await startCore(work, cleanups);
    // The suite's fixture: a spare event, its terminate control, and streamed
    // answers on the conformance lane (app/server.ts EVENTS_CONFORMANCE).
    toy = await startToy(variant, h, work, false, { WB_EVENTS_CONFORMANCE: "1" });
    const t = toy;
    // An occurrence every second, so poll, push and webhook each see one within the suite's waits.
    ticker = setInterval(() => void controlApi(t, "POST", "/control/publish", { lane: LANE, tag: "conformance" }).catch(() => {}), 1000);
    const env = {
      EVENTS_ARGUMENTS: JSON.stringify({ "bench.event": { run: t.run, lane: LANE } }),
      // Retries back off from 20ms on the harness core, so a short settle sees them.
      EVENTS_DELIVERY_SETTLE_MS: "8000",
      EVENTS_GC_WAIT_MS: "5000",
    };
    for (const s of scenarios) {
      console.log(`running ${s}…`);
      checks.push(...(await runScenario(suite, `${t.base}/mcp/${LANE}`, s, env, work)));
    }
  } finally {
    if (ticker) clearInterval(ticker);
    if (toy) {
      killGroup(toy.proc);
      const p = toy.proc;
      await new Promise<void>((r) => (p.exitCode !== null ? r() : (p.once("exit", () => r()), setTimeout(r, 5000))));
    }
    for (const c of cleanups.reverse()) await c().catch(() => {});
  }
  const { outcomes, missing } = grade(checks, expected, app);
  const md = summary(pin, app, outcomes, missing);
  console.log(md);
  const res = flag("results");
  if (res) writeFileSync(res, JSON.stringify({ pin, app, outcomes, missing }, null, 1) + "\n");
  const sum = flag("summary") ?? process.env.GITHUB_STEP_SUMMARY;
  if (sum) writeFileSync(sum, md, { flag: "a" });
  if (missing.length || outcomes.some((o) => o.verdict === "unexpected-fail" || o.verdict === "stale")) {
    process.exitCode = 1;
    console.log(`the suite's logs and checks: ${work}`);
  } else rmSync(work, { recursive: true, force: true });
}

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  main().catch((err) => {
    console.error(err instanceof Error ? err.message : String(err));
    process.exit(2);
  });
}
