// What a run records (results.json) and the report renders.

export const SCENARIOS = ["S1", "S2", "S2i", "S3", "S4", "S5", "S6", "S6n", "S7", "S8", "S9", "S11", "S12", "S13", "S14", "S15", "S16", "S17", "S17d", "S18", "S19", "S19h", "S20", "S21", "R1", "R2", "R3", "R4", "R5", "R6", "R7", "R8", "R9", "R10"] as const;
export type ScenarioId = (typeof SCENARIOS)[number];
/**
 * The realistic tier (src/realistic.ts): a real-looking app (app/ci.ts) and
 * a user who asks for something real, never naming Wakeshift or a tool. The
 * S scenarios are the mechanics tier: exact plumbing, scripted prompts.
 */
export const REALISTIC: ScenarioId[] = ["R1", "R2", "R3", "R4", "R5", "R6", "R7", "R8", "R9", "R10"];
/** The realistic tier without the push lane (R7-R9, run by name for a harness that pushes) and R10 (Codex's own dialog, on the CI app with sessions): the lanes a repeated run of the tier covers. */
export const REALISTIC_LANES: ScenarioId[] = ["R1", "R2", "R3", "R4", "R5", "R6"];
export const isRealistic = (s: ScenarioId) => s.startsWith("R");
/** The scenarios every harness of a run plays together, after their own, where an extension adds them (src/extensions.ts). */
export const TOGETHER: ScenarioId[] = ["S11", "S12", "S14"];
/** What a run does without --scenarios: S11, S12 and S14 (real agents together in real apps, from extensions), S13 (35+ minutes idle), S15 (a second app), S16 and S17 (the job tools, their own lanes), S18 (a new conversation after the install) and S19 and S19h (the plugin updated under a connected conversation) are asked for by name. */
export const DEFAULT_SCENARIOS: ScenarioId[] = ["S1", "S2", "S2i", "S3", "S4", "S5", "S6", "S7", "S8", "S9"];

export const SCENARIO_TITLES: Record<ScenarioId, string> = {
  S1: "Watch: instructions, install offer",
  S2: "Idle wake, nothing installed",
  S2i: "Idle wake, installed",
  S3: "Burst of 5",
  S4: "Waiter killed",
  S5: "Install: no",
  S6: "Install: yes",
  S6n: "Install: yes, no nudge (the user says only \"done\")",
  S7: "Re-arm (unwatch + watch)",
  S8: "Harness restart",
  S9: "Arguments passed",
  S11: "Multiplayer app (Agent Hold'em)",
  S12: "Agent Chat soak: approvals",
  S13: "Long idle (35+ min)",
  S14: "Nightfall table (consecutive games)",
  S15: "Two apps, one conversation",
  S16: "Unrecognized harness: the app's status tool",
  S17: "Side-effecting subscribe, installed: called once",
  S17d: "Side-effecting subscribe, declined: called once",
  S18: "Already installed: a new conversation asks nothing",
  S19: "Plugin update: the published plugin to this one, same conversation",
  S19h: "Installer hooks to the plugin, same conversation",
  S20: "Off switch, as the docs give it",
  S21: "Wakes in a row, and across a restart",
  R1: "Subscribes from intent",
  R2: "Idle wake from a real event",
  R3: "Install offer in context: yes",
  R4: "Install declined, still told",
  R5: "Wake while on another task",
  R6: "Idle wake after the install",
  R7: "Push: subscribes itself from intent, no install offer",
  R8: "Push: woken in the open session",
  R9: "Push: a second watch after the wake",
  R10: "R-elicit: the install from Codex's own dialog",
};

export type Grade = "pass" | "below-bar" | "fail" | "blocked" | "n/a";
export type Status = "ran" | "blocked" | "n/a" | "error";

export type Metric = number | boolean | string | null | number[];

/** A point on a scenario's timeline. */
export interface Step {
  /** ISO time. */
  at: string;
  /** Milliseconds since the scenario started. */
  t: number;
  label: string;
  /** Path of the snapshot (relative to the run dir): .ansi and .html. */
  snapshot?: string;
  /** Anything worth showing next to it (redacted): latencies, delivery rows, quotes. */
  data?: unknown;
}

/**
 * Where one event's latency went, in ms after the app called publish
 * (null: not seen, or not measurable on this target). Server stages come
 * from the core's database (local cores only); pane stages from the
 * bench watching the terminal every 250ms.
 */
export interface Breakdown {
  code: string;
  /** Wakeshift answered the publish. */
  accepted: number | null;
  /** The delivery row was written (the pipeline fanned the event out). */
  created: number | null;
  /** When the delivery may go out (pacing holds some back). */
  available: number | null;
  /** A waiter (wait, poll, stream, hook) first held it. */
  handed: number | null;
  /** It was confirmed (the waiter's next round, or the hook's ack). */
  confirmed: number | null;
  /** The harness put the event into the conversation and a turn started (from its transcript; Claude Code only). */
  turn?: number | null;
  /** The model's first output in that turn (its first transcript block: thinking, text or a tool call). */
  first?: number | null;
  /** The event's code first showed in the pane (a Monitor line, a tool result), not counting the agent's GOT line. */
  pane: number | null;
  /** The agent called report_seen with it. */
  reported: number | null;
  /** The agent's GOT line was visible. */
  got: number | null;
}

export const STAGES = ["accepted", "created", "available", "handed", "confirmed", "turn", "first", "pane", "reported", "got"] as const;

export interface ScenarioResult {
  /** S2/S3: where each event's latency went. */
  breakdown?: Breakdown[];
  id: ScenarioId;
  harness: string;
  status: Status;
  /** Why it's blocked, n/a or errored. */
  reason?: string;
  /** The wake path the scenario exercised: monitor, background, hooks, connector, plugin, held, … */
  path?: string;
  /** Whether the basic thing worked at all (a ran scenario without it fails). */
  works?: boolean;
  metrics: Record<string, Metric>;
  steps: Step[];
  notes: string[];
  startedAt?: string;
  durationMs?: number;
  // Filled in by grading.
  grade?: Grade;
  /** The number shown in the grid cell ("2.4s", "5/5"). */
  key?: string;
  /** Bars missed, or why it failed. */
  why?: string[];
}

export interface HarnessResult {
  id: string;
  name: string;
  tier: string;
  mode: "automated" | "container" | "assisted" | "fake";
  version?: string;
  /** The version each lane's own environment reported (lane → version). */
  versions?: Record<string, string | null>;
  /** The model the harness ran (Claude Code: the one its start header named, e.g. "claude-sonnet-5-5 (Sonnet 5.5)"). */
  model?: string;
  /** The model each lane ran (lane → model), where the driver knows it. */
  models?: Record<string, string | null>;
  status: "ran" | "blocked" | "skipped";
  reason?: string;
  scenarios: ScenarioResult[];
  /** Pipe-pane logs (relative paths). */
  logs: string[];
  /** Container harnesses: the model's tokens over the run's lanes (best effort). */
  tokens?: { input: number; output: number; cached: number };
}

export interface RunResult {
  run: string;
  /** Which tier ran: the S scenarios (mechanics, the default) or R1-R6 (realistic). */
  tier?: "mechanics" | "realistic";
  /** The realistic tier's seed: which wording each step used (--seed repeats them). */
  seed?: string;
  target: string;
  core: string;
  /** Which toy app served (WB_APP: app/variants.ts), and S15's second. */
  app?: string;
  app2?: string;
  /** How lanes added the MCP server: the picker's commands (WB_MCP_ADD=picker, the default) or the bench's own setup code. */
  mcpAdd?: "picker" | "bench";
  /** Claude Code's wake path in this run (WB_CLAUDE_TRANSPORT: task, or the Monitor). */
  claudePath?: string;
  startedAt: string;
  finishedAt?: string;
  scenarios: ScenarioId[];
  harnesses: HarnessResult[];
  safety: { ok: boolean; changed: string[]; checked: string[] };
  notes: string[];
}

/**
 * Expands "S1-S4,S7" to scenario IDs, in order. A range over S2 includes
 * S2i (S2's installed half); "S2i" names it alone. Likewise a range over
 * S17 includes S17d (S17 declined); "S17d" names it alone, and "S17" is
 * only the installed half. A range over S19 includes S19h (from the
 * installer's hooks); "S19h" names it alone.
 */
export function parseScenarios(spec: string): ScenarioId[] {
  const out = new Set<ScenarioId>();
  for (const part of spec.split(",").map((s) => s.trim().toUpperCase()).filter(Boolean)) {
    if (part === "S2I") {
      out.add("S2i");
      continue;
    }
    // S6n (no nudge) runs only by name: a range over S6 keeps S6 alone, so runs stay comparable.
    if (part === "S6N") {
      out.add("S6n");
      continue;
    }
    if (part === "S17D") {
      out.add("S17d");
      continue;
    }
    if (part === "S19H") {
      out.add("S19h");
      continue;
    }
    // S10 has no harness: a spec client against a local core (src/s10.ts).
    if (part === "S10") throw new Error("S10 runs on its own, not through a harness: node dist/src/s10.js, in bench/");
    const r = /^R(\d{1,2})(?:-R?(\d{1,2}))?$/.exec(part);
    if (r) {
      const a = Number(r[1]);
      const b = r[2] ? Number(r[2]) : a;
      if (a < 1 || b > REALISTIC.length || a > b) throw new Error(`bad scenario range ${part}: the realistic tier is R1-R${REALISTIC.length}`);
      for (let i = a; i <= b; i++) out.add(`R${i}` as ScenarioId);
      continue;
    }
    const m = /^S(\d{1,2})(?:-S?(\d{1,2}))?$/.exec(part);
    if (!m) throw new Error(`bad scenario ${part}: use S1-S9, S2i, S6n, S11-S21, S17d, S19h, R1-R10 or a list like S1,S2,S5`);
    const a = Number(m[1]);
    const b = m[2] ? Number(m[2]) : a;
    const known = new Set<string>(SCENARIOS);
    if (a < 1 || a > b || !known.has(`S${a}`) || !known.has(`S${b}`)) throw new Error(`bad scenario range ${part}`);
    for (let i = a; i <= b; i++) {
      if (known.has(`S${i}`)) out.add(`S${i}` as ScenarioId);
      if (i === 2 && b > a) out.add("S2i");
      if (i === 17 && b > a) out.add("S17d");
      if (i === 19 && b > a) out.add("S19h");
    }
  }
  return SCENARIOS.filter((s) => out.has(s));
}

export function median(xs: number[]): number | null {
  if (!xs.length) return null;
  const s = [...xs].sort((a, b) => a - b);
  const mid = Math.floor(s.length / 2);
  return s.length % 2 ? s[mid] : Math.round((s[mid - 1] + s[mid]) / 2);
}
