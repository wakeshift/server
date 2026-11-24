// Assisted mode: for harnesses without a terminal the bench can drive. Each
// step is printed for the user (prompts and URLs to copy), the bench waits
// for Enter, and records what happened from the toy app's data: the calls
// the agent made (and their arguments), what it reported seeing and when,
// delivery state; plus the user's own y/n judgments.

import { createInterface, type Interface } from "node:readline/promises";
import type { AssistedDriver } from "../drivers/assisted/index.js";
import type { RunCtx } from "./lane.js";
import { argsCheck, burst, idleWake, install, rearm, restart } from "./metrics.js";
import type { HarnessResult, ScenarioId, ScenarioResult, Step } from "./model.js";
import { apply, ASK_INSTALL, installConnected, INTRO, REARM, SAY_NO, SAY_YES, WATCH_AGAIN } from "./scenarios.js";
import { redactPlain } from "./redact.js";
import { sleep } from "./tmux.js";

export interface Io {
  ask(q: string): Promise<string>;
  say(s: string): void;
  close(): void;
}

export function terminalIo(): Io {
  const rl: Interface = createInterface({ input: process.stdin, output: process.stdout });
  return { ask: (q) => rl.question(q), say: (s) => console.log(s), close: () => rl.close() };
}

const yes = (s: string) => /^\s*y/i.test(s);

export async function runAssisted(ctx: RunCtx, d: AssistedDriver, want: Set<ScenarioId>, name: string, tier: string, io: Io = terminalIo()): Promise<HarnessResult> {
  const hr: HarnessResult = { id: d.id, name, tier, mode: "assisted", status: "ran", scenarios: [], logs: [] };
  const lane = d.id;
  const t0 = { v: Date.now() };
  let steps: Step[] = [];
  const step = (label: string, data?: unknown) => steps.push({ at: new Date().toISOString(), t: Date.now() - t0.v, label: redactPlain(label), ...(data === undefined ? {} : { data }) });
  const begin = (id: ScenarioId): ScenarioResult => {
    steps = [];
    t0.v = Date.now();
    return { id, harness: d.id, status: "ran", metrics: {}, steps, notes: [], startedAt: new Date().toISOString() };
  };
  const finish = (r: ScenarioResult) => {
    r.durationMs = Date.now() - Date.parse(r.startedAt!);
    hr.scenarios.push(r);
  };
  const block = (title: string, lines: string[]) => {
    io.say(`\n── ${d.name}: ${title} ${"─".repeat(Math.max(0, 50 - title.length))}`);
    for (const l of lines) io.say(l);
  };
  const enter = (what = "Press Enter when done.") => io.ask(`${what} `);
  const prompt = async (text: string, after = "Press Enter once the agent has finished its reply.") => {
    io.say("\nPaste this into the conversation:\n");
    io.say(`    ${text}\n`);
    await enter(after);
  };
  // Waits for report_seen to name codes (the agent calls it when woken).
  const waitSeen = async (codes: string[], ms: number, since: string) => {
    const end = Date.now() + ms;
    const at = new Map<string, number>();
    while (Date.now() < end && at.size < codes.length) {
      const st = await ctx.bench.lane(lane);
      for (const s of st.seen) if (s.at >= since) for (const id of s.ids) if (codes.includes(id) && !at.has(id)) at.set(id, Date.parse(s.at));
      await sleep(1000);
    }
    return at;
  };

  let mcp = ctx.bench.mcpUrl(lane);
  if (d.needsPublicUrl && /127\.0\.0\.1|localhost/.test(mcp)) {
    block("a public URL", [
      `${d.name} can't reach this machine's localhost. Expose the toy app (${ctx.bench.app}) with a tunnel, e.g.:`,
      `    cloudflared tunnel --url ${ctx.bench.app}`,
      "then paste the https URL it prints (or press Enter to skip this harness).",
    ]);
    const u = (await io.ask("Public URL: ")).trim().replace(/\/+$/, "");
    if (!/^https:\/\//.test(u)) {
      hr.status = "blocked";
      hr.reason = "no public URL for the toy app";
      for (const s of [...want]) hr.scenarios.push({ id: s, harness: d.id, status: "blocked", reason: hr.reason, metrics: {}, steps: [], notes: [] });
      io.close();
      return hr;
    }
    mcp = `${u}/mcp/${lane}`;
  }

  block("add the MCP server", d.add(mcp));
  await enter();

  // S1
  const s1 = begin("S1");
  await prompt(INTRO);
  let st = await ctx.bench.lane(lane);
  const watch = st.calls.find((c) => c.tool === "watch");
  step(watch ? "watch called" : "no watch call", watch ? { args: watch.args, recognized: watch.agent } : undefined);
  const quoted = yes(await io.ask("Did it quote the instructions (a QUOTE: block)? y/n "));
  const asked = yes(await io.ask("Did it ask you before installing anything, or offer an install? y/n "));
  const quote = (await io.ask("Optional: paste the first line of its quote (Enter to skip): ")).trim();
  if (quote) step("quote", { quote: redactPlain(quote) });
  s1.metrics = { watch_called: !!watch, quoted, install_offered_up_front: asked, recognized_as: watch?.agent?.harness ?? null };
  s1.works = !!watch;
  if (!watch) s1.reason = "the agent didn't call watch";
  if (want.has("S1")) finish(s1);

  // S5: the user says no.
  const s5 = begin("S5");
  if (asked) await prompt(SAY_NO);
  s5.metrics.installed_nothing = !yes(await io.ask("Did it install anything anyway? y/n "));

  const latency = async (id: ScenarioId, count: number, label: string) => {
    const r = begin(id);
    const lat: number[] = [];
    let seenAll = 0;
    let wakes = 0;
    const rounds = id === "S2" ? 3 : 1;
    for (let i = 0; i < rounds; i++) {
      block(label, ["Leave the conversation idle (don't type anything)."]);
      await enter("Press Enter to publish.");
      const since = new Date().toISOString();
      const evs = await ctx.bench.publish(lane, count, `${id} assisted`);
      const tp = Date.now();
      step(`published ${evs.map((e) => e.code).join(", ")}`);
      io.say(`Published ${evs.map((e) => e.code).join(", ")}; waiting up to 2 minutes for the agent to report them…`);
      const at = await waitSeen(evs.map((e) => e.code), 120_000, since);
      seenAll += at.size;
      for (const v of at.values()) lat.push(v - tp);
      const s = await ctx.bench.lane(lane);
      wakes += s.seen.filter((x) => x.at >= since).length;
      step(`${at.size}/${count} reported`, { latency_ms: [...at.values()].map((v) => v - tp) });
      if (at.size < count && yes(await io.ask("The agent didn't report them all. Did it reply GOT for them anyway? y/n "))) step("the user saw GOT lines the agent didn't report");
    }
    return { r, lat, seenAll, wakes };
  };

  if (d.wakes) {
    if (want.has("S2")) {
      const { r, lat, wakes } = await latency("S2", 1, "idle wake");
      finish(apply(r, idleWake(lat, 3, wakes)));
    }
    if (want.has("S3")) {
      const { r, seenAll, wakes } = await latency("S3", 5, "burst of 5");
      finish(apply(r, burst(5, seenAll, wakes, 0)));
    }
  } else {
    for (const s of ["S2", "S3"] as ScenarioId[]) if (want.has(s)) hr.scenarios.push({ id: s, harness: d.id, status: "n/a", reason: `${d.name} can't be woken while idle`, metrics: {}, steps: [], notes: d.notes ? [d.notes] : [] });
  }
  if (want.has("S4")) hr.scenarios.push({ id: "S4", harness: d.id, status: "n/a", reason: "no waiter process the bench can kill (assisted)", metrics: {}, steps: [], notes: [] });
  if (want.has("S2i")) hr.scenarios.push({ id: "S2i", harness: d.id, status: "n/a", reason: "nothing to install (assisted)", metrics: {}, steps: [], notes: [] });

  // S5: a message arrives at the next watch call.
  steps = s5.steps;
  const [e] = await ctx.bench.publish(lane, 1, "S5 assisted");
  const since = new Date().toISOString();
  step(`published ${e.code}`);
  await prompt(WATCH_AGAIN);
  st = await ctx.bench.lane(lane);
  const arrived = st.calls.some((c) => c.at >= since && c.events_in_result.includes(e.code)) || st.seen.some((s) => s.ids.includes(e.code));
  s5.metrics.arrived = arrived;
  s5.works = s5.metrics.installed_nothing === true && arrived;
  if (!s5.works) s5.reason = s5.metrics.installed_nothing ? "the message didn't arrive" : "it installed something after the user said no";
  if (want.has("S5")) finish(s5);

  if (want.has("S6")) {
    if (!d.install) hr.scenarios.push({ id: "S6", harness: d.id, status: "n/a", reason: "nothing to install", metrics: {}, steps: [], notes: [] });
    else {
      const r = begin("S6");
      await prompt(asked ? SAY_YES : ASK_INSTALL, "Press Enter once it has installed and told you what to do next.");
      const ran = yes(await io.ask("Did the agent run the installer itself (rather than asking you to)? y/n "));
      const relayed = yes(await io.ask("Did it tell you a trust/restart step? y/n "));
      block("trust step", ["Do the step it told you (restart, trust)."]);
      await enter();
      const tYes = Date.now();
      let connectedMs: number | null = null;
      while (Date.now() - tYes < 180_000) {
        const s = await ctx.bench.status(lane);
        if (s.subscriptions.some((x) => installConnected({ zeroInstallWake: d.wakes }, { connected: !!x.connected, via: x.via ?? null }))) {
          connectedMs = Date.now() - tYes;
          break;
        }
        await sleep(2000);
      }
      const evSince = new Date().toISOString();
      const [w] = await ctx.bench.publish(lane, 1, "S6 assisted");
      const tp = Date.now();
      const at = await waitSeen([w.code], 120_000, evSince);
      finish(apply(r, install({ agent_ran_installer: ran, trust_relayed: relayed, installed: true, connected_ms: connectedMs, wake_ms: at.size ? [...at.values()][0] - tp : null })));
    }
  }

  if (want.has("S7")) {
    if (!d.wakes) hr.scenarios.push({ id: "S7", harness: d.id, status: "n/a", reason: `${d.name} can't be woken while idle`, metrics: {}, steps: [], notes: [] });
    else {
      const r = begin("S7");
      await prompt(REARM);
      const evSince = new Date().toISOString();
      const [w] = await ctx.bench.publish(lane, 1, "S7 assisted");
      const tp = Date.now();
      step(`published ${w.code}`);
      const at = await waitSeen([w.code], 90_000, evSince);
      finish(apply(r, rearm(at.size ? [...at.values()][0] - tp : null, 0)));
    }
  }

  if (want.has("S8")) {
    if (!d.restart) hr.scenarios.push({ id: "S8", harness: d.id, status: "n/a", reason: "no restart to test", metrics: {}, steps: [], notes: [] });
    else {
      const r = begin("S8");
      block("restart", [d.restart, "First close it; press Enter once it's closed."]);
      await enter();
      const evSince = new Date().toISOString();
      const [w] = await ctx.bench.publish(lane, 1, "S8 while closed");
      step(`published ${w.code} while closed`);
      await enter("Now reopen it (the same conversation) and press Enter.");
      if (!d.wakes) await prompt(WATCH_AGAIN);
      const at = await waitSeen([w.code], 150_000, evSince);
      const s = await ctx.bench.lane(lane);
      const dup = s.seen.filter((x) => x.ids.includes(w.code)).length > 1 ? 1 : 0;
      finish(apply(r, restart(1, at.size, dup)));
    }
  }

  if (want.has("S9")) {
    const r = begin("S9");
    st = await ctx.bench.lane(lane);
    step(`${st.calls.length} call(s)`, { calls: st.calls.map((c) => ({ at: c.at, tool: c.tool, client: c.client, args: c.args, recognized: c.agent ?? null })) });
    finish(apply(r, argsCheck([st.calls], { harness: d.harness, conversation: d.conversation })));
  }
  io.close();
  return hr;
}
