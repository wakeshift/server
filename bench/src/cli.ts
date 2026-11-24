// wakebench: bench/run --harness claude-code,codex [--target local|fake]
//   [--scenarios S1-S9] [--assisted chatgpt,claude-cowork]
//   [--parallel 2] [--slow 1.5] [--out-root /tmp/wakebench] [--keep-homes] [--approve-delay 20]
// bench/run --only-report <run-id>   re-renders a run's report.html from its results.json
// Extensions (src/extensions.ts) can add targets, options, scenarios the
// harnesses play together and run modes of their own.

import { chmodSync, existsSync, readdirSync, readFileSync, realpathSync, rmSync, statSync, writeFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { randomBytes } from "node:crypto";
import { BlockedError } from "../drivers/base.js";
import { assistedDriver, ASSISTED, driverFor, HARNESSES } from "../drivers/index.js";
import { runAssisted } from "./assisted.js";
import { startBench, type Bench } from "./core.js";
import { ext, loadExtensions } from "./extensions.js";
import { startFakeBench } from "./fake.js";
import { gradeRun, loadTiers, tierOf, type Tiers } from "./grade.js";
import { makeRunDir, REAL_HOME, RUN_ID, RUN_MARKER } from "./home.js";
import { DEFAULT_SCENARIOS, isRealistic, parseScenarios, TOGETHER, type HarnessResult, type RunResult, type ScenarioId, type ScenarioResult } from "./model.js";
import { mcpAddMode } from "./picker.js";
import { Realistic } from "./realistic.js";
import { attributable, Procs } from "./procs.js";
import { holdsSecretValue, redact, redactDeep } from "./redact.js";
import { writeReport } from "./report.js";
import { changed, snapshot } from "./safety.js";
import { EXTERNAL_SCENARIOS, Scenarios, s13Path, s15Path, s18Applies, s18NotApplies, s6nBack } from "./scenarios.js";
import { Tmux } from "./tmux.js";
import type { RunCtx } from "./lane.js";

const here = dirname(fileURLToPath(import.meta.url));
const benchDir = resolve(here, "../..");
const repo = resolve(benchDir, "..");

interface Args {
  harness: string[];
  assisted: string[];
  /** local, fake, or a target an extension added. */
  target: string;
  scenarios: ScenarioId[];
  /** The options extensions added (ext.options), by name ("--name"): a value, or "1" for a switch. */
  options: Record<string, string>;
  onlyReport?: string;
  outRoot: string;
  parallel: number;
  slow: number;
  keepHomes: boolean;
  /** Seconds before each dialog is answered (--approve-delay, WB_APPROVE_DELAY): the delayed-approval variant. */
  approveDelay: number;
  /** Exit 1 unless every cell passed (the dry run). */
  expectPass?: boolean;
  /** The realistic tier's seed (--seed, WB_SEED): which wording each step uses. Default: random, and recorded. */
  seed?: string;
}

/** A number option, or a clear error (no silent fallback for a typo, NaN, Infinity or a fraction). */
export function numberArg(name: string, raw: string, o: { min: number; max: number; integer?: boolean }): number {
  const n = Number(raw);
  if (raw.trim() === "" || !Number.isFinite(n) || (o.integer && !Number.isInteger(n)) || n < o.min || n > o.max)
    throw new Error(`${name} must be ${o.integer ? "a whole number" : "a number"} from ${o.min} to ${o.max}, not ${JSON.stringify(raw)}`);
  return n;
}

/** A run's directory for --only-report: a run ID, resolved inside the output root, made by the bench. */
export function reportDir(outRoot: string, run: string): string {
  if (!RUN_ID.test(run)) throw new Error(`not a run ID: ${run}`);
  const root = realpathSync(outRoot);
  const dir = realpathSync(join(outRoot, run));
  if (dirname(dir) !== root) throw new Error(`${run} isn't inside ${outRoot}`);
  if (!existsSync(join(dir, RUN_MARKER)) && !existsSync(join(dir, "results.json"))) throw new Error(`${dir} isn't a wakebench run`);
  return dir;
}

export function parseArgs(argv: string[]): Args {
  const a: Args = { harness: [], assisted: [], target: "local", scenarios: [...DEFAULT_SCENARIOS], options: {}, outRoot: "/tmp/wakebench", parallel: 1, slow: 1, keepHomes: false, approveDelay: process.env.WB_APPROVE_DELAY ? numberArg("WB_APPROVE_DELAY", process.env.WB_APPROVE_DELAY, { min: 0, max: 600 }) : 0 };
  const targets = ["local", "fake", ...ext.targets.keys()];
  for (let i = 0; i < argv.length; i++) {
    const k = argv[i];
    const v = () => {
      const x = argv[++i];
      if (x === undefined) throw new Error(`${k} needs a value`);
      return x;
    };
    const list = (s: string) => s.split(",").map((x) => x.trim()).filter(Boolean);
    if (k === "--harness") a.harness = list(v());
    else if (k === "--assisted") a.assisted = list(v());
    else if (k === "--target") {
      const t = v();
      if (!targets.includes(t)) throw new Error(`--target is ${targets.slice(0, -1).join(", ")}${targets.length > 2 ? "," : ""} or ${targets.at(-1)}`);
      a.target = t;
    } else if (k === "--scenarios") a.scenarios = parseScenarios(v());
    else if (ext.options.has(k)) a.options[k] = ext.options.get(k)!.value ? v().replace(/^~(?=\/)/, REAL_HOME) : "1";
    else if (k === "--only-report") {
      a.onlyReport = v();
      if (!RUN_ID.test(a.onlyReport)) throw new Error(`--only-report takes a run ID like 20261020-011712-34a8, not ${JSON.stringify(a.onlyReport)}`);
    }
    else if (k === "--out-root") a.outRoot = v();
    else if (k === "--parallel") a.parallel = numberArg(k, v(), { min: 1, max: 16, integer: true });
    else if (k === "--slow") a.slow = numberArg(k, v(), { min: 0.2, max: 10 });
    else if (k === "--keep-homes") a.keepHomes = true;
    else if (k === "--approve-delay") a.approveDelay = numberArg(k, v(), { min: 0, max: 600 });
    else if (k === "--expect-pass") a.expectPass = true;
    else if (k === "--seed") a.seed = v();
    else if (k === "-h" || k === "--help") {
      console.log(readFileSync(join(benchDir, "README.md"), "utf8").split("\n## ")[1] ?? "see bench/README.md");
      process.exit(0);
    } else throw new Error(`unknown argument ${k} (see bench/README.md)`);
  }
  for (const [name, o] of ext.options) {
    const env = o.env ? process.env[o.env] : undefined;
    if (env && a.options[name] === undefined) a.options[name] = o.value ? env.replace(/^~(?=\/)/, REAL_HOME) : "1";
  }
  // The scenarios the harnesses play together come from extensions.
  const apart = a.scenarios.filter((s) => TOGETHER.includes(s) && !ext.together.some((t) => t.id === s));
  if (apart.length) throw new Error(`${apart.join(", ")} ${apart.length === 1 ? "isn't" : "aren't"} in this bench`);
  for (const h of a.harness) if (!HARNESSES.includes(h)) throw new Error(`unknown harness ${h}: one of ${HARNESSES.join(", ")}`);
  for (const h of a.assisted) if (!ASSISTED.includes(h)) throw new Error(`unknown assisted harness ${h}: one of ${ASSISTED.join(", ")}`);
  if (!a.onlyReport && !a.harness.length && !a.assisted.length) throw new Error("say which harnesses: --harness claude-code,codex (or --assisted chatgpt)");
  a.seed ??= process.env.WB_SEED || undefined;
  // The two tiers run apart: a realistic run has the CI app and no toy app, and its report is its own.
  const real = a.scenarios.filter(isRealistic);
  if (real.length && real.length !== a.scenarios.length) throw new Error("the realistic tier (R1-R6) runs on its own: run it apart from the S scenarios");
  if (real.length && a.assisted.length) throw new Error("the realistic tier has no assisted mode yet");
  if (real.length && a.target === "fake") throw new Error("the realistic tier needs a real core: --target local");
  if (process.env.WB_EXTERNAL_APP) {
    // An external app (src/external.ts) has no watch or report_seen tool: only the scenarios written for it run.
    const other = a.scenarios.filter((s) => !EXTERNAL_SCENARIOS.includes(s));
    if (other.length) throw new Error(`WB_EXTERNAL_APP runs ${EXTERNAL_SCENARIOS.join(", ")}: not ${other.join(", ")}`);
    if (a.target !== "local") throw new Error("WB_EXTERNAL_APP runs on a local core: --target local");
  }
  // Bad values fail here, before anything starts.
  mcpAddMode();
  if (a.scenarios.includes("S6n")) s6nBack();
  return a;
}

/** Claude Code's wake path in a run, for the report: its MCP task, or the Monitor (WB_CLAUDE_TRANSPORT). */
export function claudePath(env: NodeJS.ProcessEnv = process.env): string {
  for (const c of ext.claude) {
    const p = c.path?.(env);
    if (p) return p;
  }
  const t = env.WB_CLAUDE_TRANSPORT;
  if (t === "task") return "MCP task";
  if (t && t !== "wait") return t;
  // Unset, the temp HOME has the account's own cached flags, which decide.
  return "the account's default";
}

function runId(): string {
  const d = new Date();
  const p = (n: number) => String(n).padStart(2, "0");
  return `${d.getFullYear()}${p(d.getMonth() + 1)}${p(d.getDate())}-${p(d.getHours())}${p(d.getMinutes())}${p(d.getSeconds())}-${randomBytes(2).toString("hex")}`;
}

/** A plain-text grid for the terminal. */
export function textGrid(run: RunResult, tiers: Tiers): string {
  const rows: string[] = [];
  const w = 30;
  rows.push("".padEnd(24) + run.scenarios.map((s) => s.padEnd(w)).join(""));
  for (const h of run.harnesses) {
    const cells = run.scenarios.map((s) => {
      const r = h.scenarios.find((x) => x.id === s);
      return (r ? `${r.grade === "below-bar" ? "below" : r.grade}:${r.key ?? ""}` : "-").slice(0, w - 1).padEnd(w);
    });
    rows.push(`${(tiers.names[h.id] ?? h.id).slice(0, 15).padEnd(16)}${h.tier.padEnd(8)}${cells.join("")}`);
  }
  return rows.join("\n");
}

async function main() {
  const args = parseArgs(process.argv.slice(2));
  const tiers = loadTiers(join(benchDir, "tiers.yaml"));
  if (args.onlyReport) {
    const dir = reportDir(args.outRoot, args.onlyReport);
    const fixed = scrub(dir);
    if (fixed.length) console.log(`redacted again: ${fixed.join(", ")}`);
    const run = JSON.parse(readFileSync(join(dir, "results.json"), "utf8")) as RunResult;
    gradeRun(run, tiers);
    writeFileSync(join(dir, "results.json"), JSON.stringify(redactDeep(run), null, 1) + "\n", { mode: 0o600 });
    console.log(writeReport(run, tiers, dir));
    console.log(textGrid(run, tiers));
    return;
  }

  const id = runId();
  const dir = makeRunDir(args.outRoot, id);
  const log = (m: string) => console.log(`${new Date().toISOString().slice(11, 19)} ${m}`);
  log(`run ${id} → ${dir}`);
  const realBefore = snapshot(REAL_HOME);
  writeFileSync(join(dir, "safety-before.json"), JSON.stringify(realBefore, null, 1) + "\n");
  const tmuxConf = join(dir, "tmux.conf");
  writeFileSync(tmuxConf, 'set -g history-limit 50000\nset -g default-terminal "xterm-256color"\nset -g remain-on-exit off\nset -g status off\n');
  const procs = new Procs(join(dir, "pids.json"), `wb-${id}`);
  const tmux = new Tmux(`wb-${id}`, tmuxConf, procs);
  const run: RunResult = {
    run: id,
    target: args.target,
    core: "",
    startedAt: new Date().toISOString(),
    scenarios: args.scenarios,
    harnesses: [],
    safety: { ok: true, changed: [], checked: Object.keys(realBefore) },
    notes: [],
  };

  const realistic = args.scenarios.some(isRealistic);
  if (realistic) {
    run.tier = "realistic";
    run.seed = args.seed ?? randomBytes(4).toString("hex");
    log(`realistic tier, seed ${run.seed} (--seed ${run.seed} repeats its wordings)`);
    if (process.env.WB_CI_WATCH_DESCRIPTION) run.notes.push(`watch_build's description: ${process.env.WB_CI_WATCH_DESCRIPTION} (WB_CI_WATCH_DESCRIPTION)`);
    if (process.env.WB_CI_PLUGIN_ASK) run.notes.push(`the plugin question's every-session step: ${process.env.WB_CI_PLUGIN_ASK} (WB_CI_PLUGIN_ASK)`);
    if (process.env.WB_CI_STATEFUL === "1") run.notes.push("the CI app keeps MCP sessions (WB_CI_STATEFUL=1), so a form can go out during a call: every Codex lane can get Wakeshift's form (answered \"Not now\" outside R10)");
  }
  let bench: Bench | undefined;
  let automatedAfter: ReturnType<typeof snapshot> | undefined;
  let cleaning: Promise<void> | null = null;
  const cleanup = () => (cleaning ??= doCleanup());
  const doCleanup = async () => {
    // Each step on its own, so one failing doesn't skip the rest.
    const step = async (what: string, f: () => unknown) => {
      try {
        await f();
      } catch (err) {
        log(`cleanup: ${what}: ${err instanceof Error ? err.message : String(err)}`);
      }
    };
    await step("processes", () => procs.killAll());
    // Anything still running from one of this run's temp HOMEs (a daemon
    // a harness left, a lane cut short by an interrupt).
    await step("leftovers", () => {
      for (const home of [join(dir, "homes"), join(realpathSync(dir), "homes")])
        for (const r of attributable(null, home)) procs.kill(r.pid, "left in a temp HOME");
    });
    // The run's own tmux socket file, by its exact path, once its server is
    // gone (tmux leaves the file behind).
    await step("tmux socket", () => {
      const sock = join(process.env.TMUX_TMPDIR ?? "/tmp", `tmux-${process.getuid?.() ?? 0}`, `wb-${id}`);
      try {
        tmux.tmuxSync("list-sessions");
      } catch {
        if (existsSync(sock)) rmSync(sock);
      }
    });
    await step("core", () => bench?.stop());
    await step("processes", () => procs.killAll());
    await step("logs", () => redactLogs(join(dir, "logs")));
    await step("scrub", () => {
      const fixed = scrub(dir);
      if (fixed.length) run.notes.push(`redacted again after the run (a secret got past the first pass): ${fixed.join(", ")}`);
    });
    if (!args.keepHomes) await step("homes", () => rmSync(join(dir, "homes"), { recursive: true, force: true }));
    await step("keys", () => {
      rmSync(join(dir, "core", "app.key"), { force: true });
      rmSync(join(dir, "core", "app2.key"), { force: true });
      rmSync(join(dir, "core", "control.token"), { force: true });
    });
  };
  const save = () => {
    gradeRun(run, tiers);
    writeFileSync(join(dir, "results.json"), JSON.stringify(redactDeep(run), null, 1) + "\n", { mode: 0o600 });
    return writeReport(run, tiers, dir);
  };
  for (const sig of ["SIGINT", "SIGTERM"] as const)
    process.once(sig, () => {
      log(`${sig}: cleaning up`);
      run.notes.push(`interrupted (${sig})`);
      cleanup()
        .then(() => save())
        .finally(() => process.exit(130));
    });

  try {
    bench =
      args.target === "fake"
        ? await startFakeBench({ run: id, procs, log })
        : await startBench({
            target: args.target,
            run: id,
            dir: join(dir, "core"),
            repo,
            procs,
            options: args.options,
            // S15's second app: a local core registers it.
            second: args.scenarios.includes("S15"),
            openclaw: args.harness.some((h) => h.startsWith("openclaw")),
            ci: realistic,
            ...(process.env.WB_EXTERNAL_APP ? { external: process.env.WB_EXTERNAL_APP } : {}),
            log,
          });
    run.core = bench.core;
    if (bench.variant) run.app = bench.variant;
    if (bench.variant2) run.app2 = bench.variant2;
    if (args.harness.includes("claude-code")) run.claudePath = claudePath();
    const ctx: RunCtx = { run: id, dir, repo, tmux, procs, bench, log, slow: args.slow, approveDelayMs: args.approveDelay * 1000 };
    if (args.approveDelay) run.notes.push(`delayed approvals: every dialog answered ${args.approveDelay}s after it appeared`);
    run.mcpAdd = mcpAddMode();
    run.notes.push(
      run.mcpAdd === "picker"
        ? `MCP servers added with the picker's commands (${process.env.WB_PICKER_DATA ? `WB_PICKER_DATA=${process.env.WB_PICKER_DATA}` : "apps/agentpicker/agents.json"})`
        : "MCP servers added with the bench's own setup code (WB_MCP_ADD=bench), not the picker's commands",
    );
    const want = new Set(args.scenarios);

    const one = async (hid: string): Promise<HarnessResult> => {
      const d = driverFor(hid);
      const hr: HarnessResult = { id: hid, name: tiers.names[hid] ?? d.name, tier: tierOf(tiers, hid)?.id ?? "untiered", mode: d.mode, status: "ran", scenarios: [], logs: [] };
      const av = await d.check();
      hr.version = av.version;
      if (!av.ok) {
        hr.status = "blocked";
        hr.reason = av.reason;
        hr.scenarios = args.scenarios.map((s) => ({ id: s, harness: hid, status: "blocked", reason: av.reason, metrics: {}, steps: [], notes: [] }));
        log(`${hid}: blocked: ${av.reason}`);
        return hr;
      }
      log(`${hid}: ${av.version ?? ""}`);
      if (realistic) {
        const rz = new Realistic(ctx, d, want, run.seed!);
        let blockedWhy: string | null = null;
        try {
          await rz.run();
        } catch (err) {
          if (err instanceof BlockedError) blockedWhy = err.message;
          hr.reason = err instanceof Error ? err.message : String(err);
          log(`${hid}: ${hr.reason}`);
        }
        for (const s of args.scenarios) {
          const r = rz.results.find((x) => x.id === s);
          if (r) hr.scenarios.push(blockedWhy && r.status === "error" ? { ...r, status: "blocked", reason: blockedWhy } : r);
          else hr.scenarios.push(blockedWhy ? { id: s, harness: hid, status: "blocked", reason: blockedWhy, metrics: {}, steps: [], notes: [] } : hr.reason ? { id: s, harness: hid, status: "error", reason: hr.reason, metrics: {}, steps: [], notes: [] } : Scenarios.na(s, hid, "not run"));
        }
        if (blockedWhy && !rz.results.some((r) => r.status === "ran")) hr.status = "blocked";
        hr.logs = rz.lanes.map((l) => `logs/${l.id}.log`);
        hr.versions = Object.fromEntries(rz.lanes.map((l) => [l.id, (l.state.version as string | null | undefined) ?? null]));
        hr.version ??= Object.values(hr.versions).find((v) => v) ?? undefined;
        setModels(hr, rz.lanes);
        return hr;
      }
      const sc = new Scenarios(ctx, d, want);
      const got: ScenarioResult[] = [];
      const lanes = [];
      const needA = ["S1", "S5", "S9"].some((s) => want.has(s as ScenarioId)) || (d.zeroInstallWake && ["S2", "S3", "S4", "S7"].some((s) => want.has(s as ScenarioId))) || (d.zeroInstallWake && want.has("S13") && s13Path() === "zero-install") || sc.s15In("a");
      const needB = d.installPath !== null && (want.has("S6") || want.has("S8") || want.has("S2i") || want.has("S20") || want.has("S21") || (!d.zeroInstallWake && ["S3", "S4", "S7"].some((s) => want.has(s as ScenarioId))) || (want.has("S13") && s13Path() === "installed") || sc.s15In("b") || (want.has("S18") && s18Applies(d)));
      let blocked: string | null = null;
      const fail = (phase: string, err: unknown) => {
        if (err instanceof BlockedError) {
          blocked = err.message;
          for (const r of sc.results) if (r.status === "error") Object.assign(r, { status: "blocked", reason: err.message });
        }
        const msg = `${phase}: ${err instanceof Error ? err.message : String(err)}`;
        log(`${hid}: ${msg}`);
        hr.reason = msg;
      };
      if (needA) {
        try {
          await sc.phaseA();
        } catch (err) {
          fail("phase a", err);
        }
      }
      if (needB && !blocked) {
        try {
          await sc.phaseB();
        } catch (err) {
          fail("phase b", err);
        }
      }
      // S6n, S16, S17, S17d, S19 and S19h: each in a lane of its own, after both phases.
      const apart: [ScenarioId, () => Promise<void>][] = [
        ["S6n", () => sc.phaseS6n()],
        ["S16", () => sc.phaseS16()],
        ["S17", () => sc.phaseS17("b")],
        ["S17d", () => sc.phaseS17("a")],
        // S19 and S19h: the plugin updated under a connected conversation.
        ["S19", () => sc.phaseS19("plugin")],
        ["S19h", () => sc.phaseS19("hooks")],
      ];
      for (const [s, go] of apart) {
        if (!want.has(s) || blocked) continue;
        try {
          await go();
        } catch (err) {
          fail(s, err);
        }
      }
      got.push(...sc.results);
      lanes.push(...sc.lanes);
      if (sc.tokens) hr.tokens = sc.tokens;
      if (want.has("S9") && lanes.length && !blocked) got.push(await sc.s9(lanes));
      for (const s of args.scenarios) {
        // The scenarios the harnesses play together run once, after this.
        if (TOGETHER.includes(s)) continue;
        const r = got.find((x) => x.id === s);
        if (r) hr.scenarios.push(r);
        else if (blocked) hr.scenarios.push({ id: s, harness: hid, status: "blocked", reason: blocked, metrics: {}, steps: [], notes: [] });
        else hr.scenarios.push(missing(s, hid, d, hr.reason));
      }
      if (blocked && !got.some((r) => r.status === "ran")) hr.status = "blocked";
      hr.logs = lanes.map((l) => `logs/${l.id}.log`);
      hr.versions = Object.fromEntries(lanes.map((l) => [l.id, (l.state.version as string | null | undefined) ?? null]));
      hr.version ??= Object.values(hr.versions).find((v) => v) ?? undefined;
      setModels(hr, lanes);
      return hr;
    };

    const queue = [...args.harness];
    const workers = Array.from({ length: Math.min(args.parallel, queue.length) }, async () => {
      while (queue.length) {
        const hid = queue.shift()!;
        const hr = await one(hid);
        run.harnesses.push(hr);
        save();
        const now = changed(realBefore, snapshot(REAL_HOME));
        if (now.length) {
          run.safety = { ...run.safety, ok: false, changed: now };
          run.notes.push(`aborted after ${hid}: real configs changed: ${now.join(", ")}`);
          log(`ABORT: real configs changed: ${now.join(", ")}`);
          queue.length = 0;
        }
      }
    });
    await Promise.all(workers);

    // The scenarios the harnesses that ran play together (extensions), each
    // once, with the safety check after each (and during, if it asks).
    for (const t of ext.together) {
      if (!want.has(t.id) || !run.safety.ok) continue;
      const playing = run.harnesses.filter((h) => h.status !== "blocked");
      const results = await t.run(
        ctx,
        playing.map((h) => driverFor(h.id)),
        { target: args.target, changed: () => changed(realBefore, snapshot(REAL_HOME)) },
      );
      for (const hr of run.harnesses) {
        const r = results.find((x) => x.harness === hr.id);
        if (r) {
          hr.scenarios.push(r);
          hr.logs.push(`logs/${hr.id}${t.lane}.log`);
        } else hr.scenarios.push({ id: t.id, harness: hr.id, status: "blocked", reason: hr.reason ?? "blocked", metrics: {}, steps: [], notes: [] });
      }
      save();
      const now = changed(realBefore, snapshot(REAL_HOME));
      if (now.length) {
        run.safety = { ...run.safety, ok: false, changed: now };
        run.notes.push(`after ${t.id}: real configs changed: ${now.join(", ")}`);
      }
    }

    // The automated part's safety check ends here: in assisted mode the
    // user drives their own apps, which may change their own configs.
    automatedAfter = snapshot(REAL_HOME);
    for (const hid of args.assisted) {
      if (!run.safety.ok) break;
      const d = assistedDriver(hid);
      const before = snapshot(REAL_HOME);
      const hr = await runAssisted(ctx, d, want, tiers.names[hid] ?? d.name, tierOf(tiers, hid)?.id ?? "untiered");
      const diff = changed(before, snapshot(REAL_HOME));
      if (diff.length) run.notes.push(`during assisted ${hid} (the user's own app): ${diff.join(", ")} changed`);
      run.harnesses.push(hr);
      save();
    }
  } catch (err) {
    run.notes.push(`run failed: ${err instanceof Error ? err.message : String(err)}`);
    log(`run failed: ${err instanceof Error ? err.stack : String(err)}`);
  } finally {
    await cleanup();
    const after = changed(realBefore, automatedAfter ?? snapshot(REAL_HOME));
    if (after.length) run.safety = { ...run.safety, ok: false, changed: after };
    run.finishedAt = new Date().toISOString();
    let path = save();
    console.log(textGrid(run, tiers));
    // Work after the run (extensions): it may render the report again.
    for (const after of ext.after) path = (await after({ dir, options: args.options, render: () => writeReport(run, tiers, dir), log })) ?? path;
    console.log(`\nreport: ${path}\nresults: ${join(dir, "results.json")}`);
    if (!run.safety.ok) {
      console.error(`real configs changed: ${run.safety.changed.join(", ")}`);
      process.exitCode = 3;
    }
    if (args.expectPass) {
      const bad = run.harnesses.flatMap((h) => h.scenarios.filter((s) => s.grade !== "pass").map((s) => `${h.id} ${s.id}: ${s.grade} (${(s.why ?? []).join("; ")})`));
      if (bad.length || !run.harnesses.length) {
        console.error(`expected every cell to pass:\n${bad.join("\n")}`);
        process.exitCode ||= 1;
      }
    }
  }
}

/** Rewrites a file in place, keeping its permissions (a 0600 file stays 0600). */
export function rewrite(path: string, content: string): void {
  const mode = statSync(path).mode & 0o777;
  writeFileSync(path, content, { mode });
  chmodSync(path, mode);
}

/** What a secret that got past redaction looks like. */
const LEAK = /[?&](?:link|ticket|token)=(?!‹)[A-Za-z0-9_-]{16,}|\bwsc_(?!‹)[A-Za-z0-9_-]{8,}|\bwst_(?!‹)[A-Za-z0-9_-]{8,}|\bsk-ant-(?!‹)[A-Za-z0-9_-]{8,}|eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}/;

/**
 * The safety net: every file the run wrote is scanned for anything that
 * still looks like a secret, and redacted again if so. Returns the files it
 * had to fix.
 */
export function scrub(dir: string): string[] {
  const fixed: string[] = [];
  const walk = (d: string) => {
    for (const e of readdirSync(d, { withFileTypes: true })) {
      const p = join(d, e.name);
      if (e.isDirectory()) {
        if (e.name !== "homes" && e.name !== "bin" && e.name !== "containers") walk(p);
        continue;
      }
      if (!/\.(ansi|html|log|json|txt)$/.test(e.name)) continue;
      const s = readFileSync(p, "utf8");
      if (!LEAK.test(s) && !holdsSecretValue(s)) continue;
      rewrite(p, redactText(s));
      fixed.push(p.slice(dir.length + 1));
    }
  };
  if (existsSync(dir)) walk(dir);
  return fixed;
}

/**
 * A file's text redacted as a whole, so a secret wrapped over lines is
 * caught where its parts join (redact's cross-line pass); a very large one
 * line by line, which redact can't hold all at once.
 */
export function redactText(s: string): string {
  return s.length < 4 << 20 ? redact(s) : s.split("\n").map(redact).join("\n");
}

/** Pipe-pane logs are written raw while a pane runs: redact them in place. */
export function redactLogs(dir: string): void {
  if (!existsSync(dir)) return;
  for (const f of readdirSync(dir)) {
    const p = join(dir, f);
    rewrite(p, redactText(readFileSync(p, "utf8")));
  }
}

function missing(s: ScenarioId, hid: string, d: ReturnType<typeof driverFor>, err?: string): ScenarioResult {
  if (err) return { id: s, harness: hid, status: "error", reason: err, metrics: {}, steps: [], notes: [] };
  if (s === "S2" && !d.zeroInstallWake) {
    // Not a gap: it's woken once its install is in (S2i). Messages wait for the next call until then (S5).
    const r = Scenarios.na(s, hid, "only after install: with nothing installed, messages wait for the next call (S5); S2i is its idle wake once installed");
    r.metrics.only_after_install = true;
    return r;
  }
  if (["S2i", "S6", "S8"].includes(s) && d.installPath === null) return Scenarios.na(s, hid, "nothing to install");
  if (s === "S18" && !s18Applies(d)) return Scenarios.na(s, hid, s18NotApplies(d));
  if (["S3", "S4", "S7"].includes(s) && !d.zeroInstallWake && d.installPath === null) return Scenarios.na(s, hid, "no wake path");
  if (s === "S15") {
    if (!d.secondMcp) return Scenarios.na(s, hid, "the driver doesn't add a second MCP server (yet)");
    if (s15Path() === "zero-install" && !d.zeroInstallWake) return Scenarios.na(s, hid, "WB_S15_PATH=zero-install, and it doesn't wake with nothing installed");
    if (s15Path() === "installed" && d.installPath === null) return Scenarios.na(s, hid, "nothing to install (WB_S15_PATH=zero-install runs it without one)");
    return Scenarios.na(s, hid, "no second app in this run (a local core registers one)");
  }
  return Scenarios.na(s, hid, "not run");
}

// Run when started as a program (compare real paths: /tmp is /private/tmp on macOS, and a run from a /tmp checkout did nothing).
if (process.argv[1] && realpathSync(resolve(process.argv[1])) === realpathSync(fileURLToPath(import.meta.url))) {
  // Extensions first: they add targets, options and run modes of their own.
  const start = loadExtensions().then(() => {
    const argv = process.argv.slice(2);
    const mode = [...ext.modes].find(([flag]) => argv.includes(flag));
    return mode ? mode[1](argv).then((code) => void (process.exitCode = code)) : main();
  });
  start.catch((err) => {
    console.error(err instanceof Error ? err.message : String(err));
    process.exit(2);
  });
}

/** The model each lane ran, where its driver recorded one (l.state.model), and the run's in the report. */
export function setModels(hr: HarnessResult, lanes: { id: string; state: Record<string, unknown> }[]): void {
  const models = Object.fromEntries(lanes.filter((l) => "model" in l.state).map((l) => [l.id, (l.state.model as string | null | undefined) ?? null]));
  if (!Object.keys(models).length) return;
  hr.models = models;
  const seen = [...new Set(Object.values(models).filter((m): m is string => Boolean(m)))];
  if (seen.length) hr.model = seen.join(", ");
}
