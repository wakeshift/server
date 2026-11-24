// What each scenario measures, and whether it worked: one model, used by
// the automated scenarios (scenarios.ts) and assisted mode (assisted.ts),
// so both report the same metrics with the same meaning and grade alike.

import type { DeliveryTrace, LaneState } from "./core.js";
import { median, STAGES, type Breakdown, type Metric } from "./model.js";

export interface Outcome {
  metrics: Record<string, Metric>;
  works: boolean;
  reason?: string;
  notes?: string[];
}

/** S2: idle wakes, latency from publish to the agent showing it. */
export function idleWake(latencies: number[], of: number, wakes: number): Outcome {
  const works = latencies.length > 0;
  return {
    metrics: { latencies_ms: latencies, p50_ms: median(latencies), max_ms: latencies.length ? Math.max(...latencies) : null, woke: latencies.length, of, wakes },
    works,
    reason: works ? undefined : "no wake",
    notes: latencies.length < of ? [`${of - latencies.length} of ${of} posts didn't wake it`] : [],
  };
}

/** S3: a burst, all seen, in how many wakes. */
export function burst(published: number, seen: number, wakes: number, duplicates: number, extra: Record<string, Metric> = {}): Outcome {
  const works = seen === published;
  return { metrics: { published, seen, wakes, duplicates, ...extra }, works, reason: works ? undefined : `${seen} of ${published} arrived` };
}

/** S7: after unwatch + watch, a post arrives; automatically, or after nudges. */
export function rearm(arrivedMs: number | null, nudges: number): Outcome {
  const works = arrivedMs !== null;
  return {
    metrics: { wake_ms: nudges === 0 ? arrivedMs : null, arrived_ms: arrivedMs, nudges, rearm_automatic: works && nudges === 0 },
    works,
    reason: works ? undefined : "no delivery after unwatch + watch",
  };
}

/**
 * S13: a long idle (past Claude Code's 30-minute Monitor limit), with posts
 * after the 30-minute mark. Works if every post arrived; the bars add no
 * nudges and that the agent never asked the user whether to keep watching.
 */
export function longIdle(o: {
  idleMs: number;
  /** ms from publish to the GOT line, per post (null: never, even after a nudge). */
  arrivals: (number | null)[];
  /** Posts that arrived without a nudge. */
  unprompted: number;
  nudges: number;
  askedUser: boolean;
  toldStopped: boolean;
  restarts: number | null;
  expired: boolean | null;
  maxGapMs: number | null;
  connectedShare: number | null;
}): Outcome {
  const got = o.arrivals.filter((x): x is number => x !== null);
  const works = got.length === o.arrivals.length;
  const notes: string[] = [];
  if (o.askedUser) notes.push("the agent asked the user whether to keep watching");
  if (o.toldStopped) notes.push("the agent told the user it had stopped watching");
  if (o.expired) notes.push("a Monitor expired (Claude Code's limit) rather than ending by itself");
  return {
    metrics: {
      idle_ms: o.idleMs,
      posts: o.arrivals.length,
      arrived: got.length,
      lost: o.arrivals.length - got.length,
      unprompted: o.unprompted,
      latencies_ms: got,
      max_ms: got.length ? Math.max(...got) : null,
      nudges: o.nudges,
      asked_user: o.askedUser,
      told_stopped: o.toldStopped,
      restarts: o.restarts,
      expired: o.expired,
      max_gap_ms: o.maxGapMs,
      connected_share: o.connectedShare,
    },
    works,
    reason: works ? undefined : `${o.arrivals.length - got.length} of ${o.arrivals.length} posts after the 30-minute mark never arrived`,
    notes,
  };
}

/**
 * S4's watch after the kill: the post (postAfterMs after the kill), when
 * presence cleared, and when the post arrived, seen while it still watches
 * presence. An arrival counts from when it showed, not from when the watch
 * ended: a waiter re-armed before the post keeps presence up, and the watch
 * used to run its full giveUpMs before it looked (a 70s "redelivery" that
 * had taken a few seconds).
 */
export class KillWatch {
  posted: number | null = null;
  clearedMs: number | null = null;
  arrivedMs: number | null = null;
  constructor(
    readonly tKill: number,
    readonly postAfterMs = 30_000,
    readonly giveUpMs = 100_000,
  ) {}
  /** Time to post (once). */
  due(now: number): boolean {
    return this.posted === null && now - this.tKill >= this.postAfterMs;
  }
  post(now: number): void {
    this.posted = now;
  }
  /** What presence said at now. */
  presence(now: number, connected: boolean): void {
    if (this.clearedMs === null && !connected) this.clearedMs = now - this.tKill;
  }
  /** The post showed in the pane at now. */
  arrived(now: number): void {
    if (this.posted !== null && this.arrivedMs === null) this.arrivedMs = now - this.posted;
  }
  /** Nothing more to watch: posted, and it arrived, presence cleared, or the watch gave up. */
  done(now: number): boolean {
    return this.posted !== null && (this.arrivedMs !== null || this.clearedMs !== null || now - this.tKill > this.giveUpMs);
  }
}

/** S8: posts around a restart, none lost or duplicated. */
export function restart(expected: number, seen: number, duplicates: number, extra: Record<string, Metric> = {}): Outcome {
  const works = seen === expected;
  return { metrics: { lost: expected - seen, duplicates, ...extra }, works, reason: works ? undefined : `${expected - seen} of ${expected} lost` };
}

/** S6: the install path connected and woke the conversation. */
export function install(m: { agent_ran_installer: boolean; trust_relayed: boolean | null; installed: boolean; connected_ms: number | null; wake_ms: number | null } & Record<string, Metric>): Outcome {
  const woke = m.wake_ms !== null;
  // offer_fits_version false: the core offered a command this harness
  // version can't run (Claude Code's --marketplace before 2.1.292).
  const fits = m.offer_fits_version !== false;
  const works = m.installed && woke && fits;
  return {
    metrics: { ...m, connected: m.connected_ms !== null, woke },
    works,
    // seen_unreported: the code reached the pane (the agent was woken and
    // acted on it) but no GOT line came in time: a stuck report, not a missed wake.
    reason: works
      ? undefined
      : !fits
        ? `the offered install doesn't run on ${m.harness_version ?? "this version"}`
        : !m.installed
          ? "nothing was installed"
          : m.seen_unreported === true
            ? "woken, but the agent never reported the event"
            : "no wake after the install",
  };
}

/**
 * S6n: the install with no nudge. After the trust step the user said only
 * "done" (or asked something the app's read tool answers), never to call
 * watch again. Works when the install is in and a post woke the
 * conversation. connected (the install's hooks or plugin held the
 * conversation's stream) and watch_again (the agent called watch again by
 * itself, which S6's nudge used to do for it) are recorded for the bars.
 */
export function noNudge(m: { installed: boolean; connected_ms: number | null; wake_ms: number | null; watch_again: number } & Record<string, Metric>): Outcome {
  const connected = m.connected_ms !== null;
  const woke = m.wake_ms !== null;
  const works = m.installed && woke;
  return {
    metrics: { ...m, connected, woke },
    works,
    reason: works
      ? undefined
      : !m.installed
        ? "nothing was installed"
        : connected
          ? "connected, but no wake after the install"
          : m.watch_again > 0
            ? `never connected after "done", though the agent called watch ${m.watch_again === 1 ? "again" : `${m.watch_again} more times`}`
            : `never connected after "done" (no nudge to call watch again)`,
  };
}

/** A tool call as S16 and S17 count it (the app's call record). */
export interface JobCall {
  at: string;
  tool: string;
  events_in_result: string[];
}

/** The tools that subscribe the conversation (calling one again is what S16 and S17 must never do). */
export const SUBSCRIBING_TOOLS = ["watch", "start_job"];

/**
 * S16: an agent Wakeshift doesn't recognize can't be woken, so it finds out
 * the way it would without Wakeshift: the app's own status tool (or any
 * tool that doesn't subscribe), when the user asks. It works when the user
 * heard the event's outcome (woken anyway, or when they asked), it never
 * called the subscribing tool (watch) again after its first call, and,
 * unless it was woken, a tool that doesn't subscribe showed it the event
 * after it was published (found_by_tool).
 */
export function statusFallback(o: { calls: JobCall[]; code: string; publishedAt: string; toldUnasked: boolean; toldWhenAsked: boolean | null; promised: boolean }): Outcome {
  const watch = o.calls.filter((c) => c.tool === "watch");
  const status = o.calls.filter((c) => c.tool === "job_status");
  const other = o.calls.filter((c) => c.tool !== "watch" && c.tool !== "job_status");
  const after = o.calls.filter((c) => c.at >= o.publishedAt);
  const foundBy = after.find((c) => !SUBSCRIBING_TOOLS.includes(c.tool) && c.events_in_result.includes(o.code));
  const repeats = Math.max(0, watch.length - 1);
  const told = o.toldUnasked || o.toldWhenAsked === true;
  const works = told && repeats === 0 && (o.toldUnasked || !!foundBy);
  const reason = works
    ? undefined
    : repeats > 0
      ? `the agent called watch ${watch.length} times (again after its first call)`
      : !told
        ? "the user never heard the job's result, even when they asked"
        : "the agent named the result without a tool showing it to it";
  return {
    metrics: {
      watch_calls: watch.length,
      watch_repeats: repeats,
      status_calls: status.length,
      other_calls: other.length,
      status_calls_after_publish: after.filter((c) => c.tool === "job_status").length,
      found_by_tool: foundBy?.tool ?? null,
      woke: o.toldUnasked,
      told_when_asked: o.toldWhenAsked,
      told,
      said_it_would_notify: o.promised,
    },
    works,
    reason,
    notes: watch.length === 0 ? ["the agent never called watch"] : [],
  };
}

/**
 * S17: a tool that both does something (each call starts a new job) and
 * subscribes. It works when the job was started exactly once in the whole
 * conversation (sideEffects: the app's own count); hearing the result
 * (woken, or when asked) is reported beside it.
 */
export function sideEffectOnce(o: { sideEffects: number; calls: JobCall[]; toldUnasked: boolean; toldWhenAsked: boolean | null }): Outcome {
  const n = o.sideEffects;
  const works = n === 1;
  return {
    metrics: {
      side_effect_calls: n,
      start_job_calls: o.calls.filter((c) => c.tool === "start_job").length,
      status_calls: o.calls.filter((c) => c.tool === "job_status").length,
      woke: o.toldUnasked,
      told_when_asked: o.toldWhenAsked,
      told: o.toldUnasked || o.toldWhenAsked === true,
    },
    works,
    reason: works ? undefined : n === 0 ? "the agent never started the job" : `the job was started ${n} times: the agent called start_job again`,
  };
}

/** One event's breakdown from its timestamps (ISO, or ms since the epoch for what the bench saw). */
export function breakdownOf(
  code: string,
  publishedAt: string,
  t: { accepted_at?: string | null; trace?: DeliveryTrace | null; turn?: number | null; first?: number | null; pane?: number | null; reported_at?: string | null; got?: number | null },
): Breakdown {
  const t0 = Date.parse(publishedAt);
  const iso = (s: string | null | undefined) => (s ? Math.max(0, Date.parse(s) - t0) : null);
  const ms = (n: number | null | undefined) => (typeof n === "number" ? Math.max(0, n - t0) : null);
  return {
    code,
    accepted: iso(t.accepted_at),
    created: iso(t.trace?.created_at),
    available: iso(t.trace?.available_at),
    handed: iso(t.trace?.handed_at),
    confirmed: iso(t.trace?.finished_at),
    ...(t.turn !== undefined ? { turn: ms(t.turn) } : {}),
    ...(t.first !== undefined ? { first: ms(t.first) } : {}),
    pane: ms(t.pane),
    reported: iso(t.reported_at),
    got: ms(t.got),
  };
}

/**
 * Wakeshift's own part of each wake, apart from the harness's and the
 * model's: from the publish until the harness had the event, read from its
 * own record (the turn its transcript or rollout says the event started:
 * publish, delivery, the hook or listener handing it over), else where only
 * the core can say (a local core, a harness without a reader) until the
 * waiter confirmed it. S2i's tier 1 bar is on this; the rest, the turn to
 * the agent's reply (`p50_model_ms`: the harness and the model), is
 * reported, never graded.
 */
export function ourPart(b: Breakdown[]): Record<string, Metric> {
  const ours = b.map((x) => (typeof x.turn === "number" ? x.turn : typeof x.confirmed === "number" ? x.confirmed : null)).filter((v): v is number => v !== null);
  const model = b.map((x) => (typeof x.turn === "number" && typeof x.got === "number" ? Math.max(0, x.got - x.turn) : null)).filter((v): v is number => v !== null);
  return {
    wakeshift_latencies_ms: ours,
    p50_wakeshift_ms: median(ours),
    max_wakeshift_ms: ours.length ? Math.max(...ours) : null,
    wakeshift_from: b.some((x) => typeof x.turn === "number") ? "transcript" : ours.length ? "core" : null,
    p50_model_ms: median(model),
  };
}

/** The median of each stage over events, as metrics (p50_<stage>_ms). */
export function stageMedians(b: Breakdown[]): Record<string, Metric> {
  const out: Record<string, Metric> = {};
  for (const s of STAGES) {
    const xs = b.map((x) => x[s]).filter((v): v is number => typeof v === "number");
    out[`p50_${s}_ms`] = median(xs);
  }
  return out;
}

/**
 * The harness a call's harness argument names. Only the part before the
 * first ";" names it: Wakeshift's plugin for Claude Code has the model pass
 * its install mark after it ("claude-code; wakeshift-plugin=…; trusts=…",
 * connector.ClaudeMarkNote), which the core takes out as it reads the call.
 */
export function normalizeHarness(h: string): string {
  const s = h.split(";")[0].trim().toLowerCase().replace(/[\s_]+/g, "-");
  const alias: Record<string, string> = {
    claude: "claude-code",
    "claude-code-cli": "claude-code",
    "cursor-agent": "cursor",
    "cursor-cli": "cursor",
    agy: "antigravity",
    "antigravity-cli": "antigravity",
    "copilot-cli": "copilot",
    "github-copilot": "copilot",
    grok: "grok-bot",
    "claude-ai": "claude-cowork",
    "claude.ai": "claude-cowork",
    "claude-app": "claude-cowork",
    cowork: "claude-cowork",
  };
  return alias[s] ?? s;
}

/**
 * S9: the arguments a conversation's calls passed. harness: passed and
 * right, or (on watch) recognized as the expected harness; session_key:
 * after the first watch, the key Wakeshift gave (required on watch where
 * the harness sends no conversation); conversation_id: on every watch where
 * the agent must pass it.
 */
export function argsCheck(
  lanes: LaneState["calls"][],
  expect: { harness: string; conversation: "agent" | "auto" | "none" },
  /** Calls the bench asked to make without a session_key (S15's new session): not held against it. */
  unkeyed: (c: LaneState["calls"][number]) => boolean = () => false,
): Outcome {
  const notes: string[] = [];
  let harnessOk = true;
  let keyOk = true;
  let convOk = true;
  let calls = 0;
  let withHarness = 0;
  let withKey = 0;
  let withConv = 0;
  for (const st of lanes) {
    const first = st.find((c) => c.tool === "watch");
    if (!first) continue;
    const issued = new Set(st.filter((c) => c.tool === "watch" && c.session).map((c) => c.session!));
    for (const c of st) {
      calls++;
      if (c.args.harness) withHarness++;
      if (c.args.conversation_id) withConv++;
      if (c.args.session_key) withKey++;
      const rec = c.agent?.harness ?? null;
      if (c.args.harness && normalizeHarness(c.args.harness) !== expect.harness) {
        harnessOk = false;
        notes.push(`${c.tool} at ${c.at}: harness "${c.args.harness}" (expected ${expect.harness})`);
      }
      // An unrecognized agent (drivers/generic) expects "": recognized as nothing.
      if (c.tool === "watch" && !c.args.harness && (rec ?? "") !== expect.harness) {
        harnessOk = false;
        notes.push(`${c.tool} at ${c.at}: no harness argument, recognized as ${rec ?? "nothing"}`);
      }
      if (c !== first && c.at > first.at) {
        if (!c.args.session_key) {
          if (expect.conversation === "none" && c.tool === "watch" && !unkeyed(c)) {
            keyOk = false;
            notes.push(`${c.tool} at ${c.at}: no session_key`);
          }
        } else if (!issued.has(c.args.session_key)) {
          keyOk = false;
          notes.push(`${c.tool} at ${c.at}: a session_key Wakeshift didn't give this conversation`);
        }
      }
      if (expect.conversation === "agent" && c.tool === "watch" && !c.args.conversation_id) {
        convOk = false;
        notes.push(`${c.tool} at ${c.at}: no conversation_id`);
      }
    }
  }
  return {
    metrics: {
      calls,
      with_harness: withHarness,
      with_session_key: withKey,
      with_conversation_id: withConv,
      harness_ok: calls ? harnessOk : null,
      session_key_ok: calls ? keyOk : null,
      conversation_ok: calls ? convOk : null,
    },
    works: calls > 0,
    reason: calls ? undefined : "no calls to check",
    notes,
  };
}

/** S15: one app's side of two apps in one conversation. */
export interface AppSide {
  /** The codes published to this app, with when each was published (ms since the epoch). */
  published: { code: string; at: number }[];
  /** When each code was first seen: its report_seen call on either app's server, or its GOT line (ms since the epoch). */
  firstSeen: Record<string, number>;
  /** Codes first seen only after the nudge. */
  afterNudge: string[];
  /** How many report_seen calls (either server) named each code; re-reports in a nudge turn left out. */
  reportCounts: Record<string, number>;
  /** Codes Wakeshift showed in a tool result again after the agent had reported them. */
  shownAgain: string[];
  /** Codes the agent reported on the other app's server (seen all the same). */
  misreported: string[];
}

/**
 * S15: two apps watched in one conversation. Per app: every code published
 * and seen, lost (never seen, after one nudge at most), duplicates (a code
 * reported in two report_seen calls, or shown again after it was reported)
 * and the wake latency (publish to first seen). It works only with nothing
 * lost and no duplicates, for both apps.
 */
export function twoApps(
  a1: AppSide,
  a2: AppSide,
  o: { nudges: number; newSession: boolean | null; midTurn: { c: boolean | null; d: boolean | null }; watchedSecond: boolean; deliveredOnce?: Set<string> },
): Outcome {
  const metrics: Record<string, Metric> = {};
  const notes: string[] = [];
  const once = o.deliveredOnce ?? new Set<string>();
  let lost = 0;
  let dups = 0;
  let misreported = 0;
  const all: number[] = [];
  for (const [n, s] of [
    [1, a1],
    [2, a2],
  ] as const) {
    const p = `app${n}_`;
    const codes = s.published.map((x) => x.code);
    const seen = codes.filter((c) => s.firstSeen[c] !== undefined);
    const lostCodes = codes.filter((c) => s.firstSeen[c] === undefined);
    // Reported twice but counted reaching the model once: the agent reported it again (reportedAgain), not a duplicate.
    const twice = codes.filter((c) => (s.reportCounts[c] ?? 0) > 1);
    const again = twice.filter((c) => once.has(c));
    const dupCodes = [...new Set([...twice.filter((c) => !once.has(c)), ...s.shownAgain.filter((c) => codes.includes(c))])];
    const lat = s.published.filter((x) => s.firstSeen[x.code] !== undefined).map((x) => Math.max(0, s.firstSeen[x.code] - x.at));
    all.push(...lat);
    lost += lostCodes.length;
    dups += dupCodes.length;
    misreported += s.misreported.length;
    metrics[`${p}published`] = codes.length;
    metrics[`${p}seen`] = seen.length;
    metrics[`${p}lost`] = lostCodes.length;
    metrics[`${p}duplicates`] = dupCodes.length;
    metrics[`${p}reported_again`] = again.length;
    metrics[`${p}after_nudge`] = s.afterNudge.filter((c) => codes.includes(c)).length;
    metrics[`${p}latencies_ms`] = lat;
    metrics[`${p}p50_ms`] = median(lat);
    metrics[`${p}max_ms`] = lat.length ? Math.max(...lat) : null;
    metrics[`${p}codes`] = codes.join(",");
    metrics[`${p}seen_codes`] = seen.join(",");
    if (lostCodes.length) notes.push(`app ${n}: lost ${lostCodes.join(", ")}`);
    if (dupCodes.length) notes.push(`app ${n}: duplicated ${dupCodes.join(", ")}`);
    if (again.length) notes.push(`app ${n}: reported again, though it reached the model once (not a duplicate): ${again.join(", ")}`);
    if (s.misreported.length) notes.push(`app ${n}: reported on the other app's server: ${s.misreported.join(", ")}`);
  }
  metrics.lost = lost;
  metrics.duplicates = dups;
  metrics.misreported = misreported;
  metrics.p50_ms = median(all);
  metrics.max_ms = all.length ? Math.max(...all) : null;
  metrics.nudges = o.nudges;
  metrics.watched_second = o.watchedSecond;
  metrics.new_session = o.newSession;
  metrics.mid_turn_c = o.midTurn.c;
  metrics.mid_turn_d = o.midTurn.d;
  const works = lost === 0 && dups === 0;
  const why = [lost ? `${lost} lost (app 1: ${metrics.app1_lost}, app 2: ${metrics.app2_lost})` : "", dups ? `${dups} duplicated (app 1: ${metrics.app1_duplicates}, app 2: ${metrics.app2_duplicates})` : ""].filter(Boolean);
  return { metrics, works, reason: works ? undefined : why.join("; "), notes };
}

/**
 * S15: one app's side from what the bench saw. reports: every report_seen
 * call on either app's server (server: which), in time order; inNudge: whether
 * a time falls in a nudge or "I'm back" turn (a code re-reported there isn't
 * a duplicate, but one first reported there is seen); calls: each server's
 * tool calls with the codes Wakeshift showed in their results.
 */
export function appSide(
  app: 1 | 2,
  published: { code: string; at: number }[],
  reports: { at: string; ids: string[]; server: 1 | 2 }[],
  got: Record<string, number>,
  calls: { at: string; events_in_result: string[] }[],
  inNudge: (at: string) => boolean,
  nudgedAt: number | null,
): AppSide {
  const codes = new Set(published.map((p) => p.code));
  const firstSeen: Record<string, number> = {};
  const reportCounts: Record<string, number> = {};
  const misreported = new Set<string>();
  for (const r of [...reports].sort((a, b) => a.at.localeCompare(b.at))) {
    for (const id of new Set(r.ids)) {
      if (!codes.has(id)) continue;
      if (r.server !== app) misreported.add(id);
      const t = Date.parse(r.at);
      // A record without a usable time can't time anything (nor make a wake look instant).
      if (!Number.isFinite(t)) continue;
      if (firstSeen[id] === undefined || t < firstSeen[id]) firstSeen[id] = t;
      // A re-report in a nudge turn answers the bench: not a second report.
      if (inNudge(r.at) && (reportCounts[id] ?? 0) > 0) continue;
      reportCounts[id] = (reportCounts[id] ?? 0) + 1;
    }
  }
  for (const [code, t] of Object.entries(got)) if (codes.has(code) && (firstSeen[code] === undefined || t < firstSeen[code])) firstSeen[code] = t;
  // Shown again: in a result after the agent had already shown it (its report, or its GOT line).
  const shownAgain = new Set<string>();
  for (const c of calls)
    for (const id of c.events_in_result) {
      const at = Date.parse(c.at);
      if (codes.has(id) && firstSeen[id] !== undefined && Number.isFinite(at) && at > firstSeen[id]) shownAgain.add(id);
    }
  const afterNudge = nudgedAt === null ? [] : [...codes].filter((c) => firstSeen[c] !== undefined && firstSeen[c] >= nudgedAt);
  return { published, firstSeen, afterNudge, reportCounts, shownAgain: [...shownAgain], misreported: [...misreported] };
}
