// Grading: a scenario's result against its harness's tier's bars
// (tiers.yaml).

import { readFileSync } from "node:fs";
import { parse } from "yaml";
import type { Metric, RunResult, ScenarioId, ScenarioResult } from "./model.js";

export type Bars = Partial<Record<ScenarioId, Record<string, Metric>>>;

export interface Tier {
  id: string;
  label: string;
  harnesses: string[];
  bars: Bars;
  /** One harness's own bars for a scenario, each over the tier's bar of the same name (tiers.yaml harness_bars). */
  harnessBars: Record<string, Bars>;
}

export interface Tiers {
  tiers: Tier[];
  names: Record<string, string>;
}

export function parseTiers(text: string): Tiers {
  const doc = parse(text) as { tiers?: Record<string, { label?: string; harnesses?: string[]; bars?: Bars; harness_bars?: Record<string, Bars> }>; names?: Record<string, string> };
  if (!doc || typeof doc !== "object" || !doc.tiers) throw new Error("tiers.yaml: no tiers");
  const tiers: Tier[] = Object.entries(doc.tiers).map(([id, t]) => ({ id, label: t.label ?? id, harnesses: t.harnesses ?? [], bars: t.bars ?? {}, harnessBars: t.harness_bars ?? {} }));
  for (const t of tiers)
    for (const h of Object.keys(t.harnessBars)) if (!t.harnesses.includes(h)) throw new Error(`tiers.yaml: ${t.id}'s harness_bars names ${h}, which isn't in ${t.id}`);
  const seen = new Map<string, string>();
  for (const t of tiers)
    for (const h of t.harnesses) {
      if (seen.has(h)) throw new Error(`tiers.yaml: ${h} is in ${seen.get(h)} and ${t.id}`);
      seen.set(h, t.id);
    }
  return { tiers, names: doc.names ?? {} };
}

export function loadTiers(path: string): Tiers {
  return parseTiers(readFileSync(path, "utf8"));
}

export function tierOf(t: Tiers, harness: string): Tier | undefined {
  return t.tiers.find((x) => x.harnesses.includes(harness));
}

/** Checks one bar; returns why it's missed, or null. */
export function checkBar(name: string, want: Metric, metrics: Record<string, Metric>): string | null {
  let key = name;
  let op: "eq" | "max" | "min" = "eq";
  if (name.endsWith("_max")) {
    key = name.slice(0, -4);
    op = "max";
  } else if (name.endsWith("_min")) {
    key = name.slice(0, -4);
    op = "min";
  }
  const got = metrics[key];
  if (got === undefined || got === null) return `${key} not measured (bar: ${describe(op, want)})`;
  if (op === "eq") return got === want ? null : `${key} is ${fmt(got)} (bar: ${fmt(want)})`;
  if (typeof got !== "number" || typeof want !== "number") return `${key} is ${fmt(got)} (bar: ${describe(op, want)})`;
  if (op === "max" && got > want) return `${key} is ${fmt(got)} (bar: at most ${fmt(want)})`;
  if (op === "min" && got < want) return `${key} is ${fmt(got)} (bar: at least ${fmt(want)})`;
  return null;
}

function describe(op: "eq" | "max" | "min", want: Metric): string {
  return op === "eq" ? fmt(want) : `${op === "max" ? "at most" : "at least"} ${fmt(want)}`;
}

function fmt(v: Metric): string {
  if (Array.isArray(v)) return `[${v.join(", ")}]`;
  return String(v);
}

/** Grades one scenario result in place and returns it. */
export function grade(r: ScenarioResult, tier: Tier | undefined): ScenarioResult {
  r.why = [];
  if (r.status === "blocked") r.grade = "blocked";
  else if (r.status === "n/a") r.grade = "n/a";
  else if (r.status === "error") {
    r.grade = "fail";
    r.why.push(r.reason ?? "the scenario errored");
  } else if (typeof r.metrics.tool_servers_unexpected === "string" && r.metrics.tool_servers_unexpected) {
    // The harness could reach servers beyond the lane's (src/toolservers.ts): not the scenario, whatever it did.
    r.grade = "fail";
    r.why.push(`MCP servers beyond the lane's were in reach (${r.metrics.tool_servers_unexpected}): the account's apps or the user's config, not the scenario`);
  } else if (!r.works) {
    r.grade = "fail";
    r.why.push(r.reason ?? "it didn't work");
  } else {
    const bars = { ...(tier?.bars[r.id] ?? {}), ...(tier?.harnessBars[r.harness]?.[r.id] ?? {}) };
    for (const [name, want] of Object.entries(bars)) {
      // With the plugin installed before the conversation (the realistic
      // tier's WB_CLAUDE_PLUGIN_PREINSTALLED or WB_CODEX_PLUGIN_PREINSTALLED), relaying an offer is the miss,
      // which the scenario's works already says.
      if (r.metrics.plugin_preinstalled === true && name === "offer_relayed") continue;
      const miss = checkBar(name, want, r.metrics);
      if (miss) r.why.push(miss);
    }
    // Every scenario, every tier: no Wakeshift-internal ID in what the user or the model was shown (scenarios.ts idsShown).
    if (r.metrics.internal_ids_checked === false) r.why.push("internal IDs not checked (the scan couldn't run: see the notes)");
    if (typeof r.metrics.internal_ids_shown === "number" && r.metrics.internal_ids_shown > 0) r.why.push(`showed ${r.metrics.internal_ids_shown} Wakeshift-internal ID(s) (bar: none; the notes say which and where)`);
    r.grade = r.why.length ? "below-bar" : "pass";
  }
  r.key = keyOf(r);
  if (r.status !== "ran" && r.reason && !r.why.includes(r.reason)) r.why.push(r.reason);
  return r;
}

const sec = (ms: Metric) => (typeof ms === "number" ? `${(ms / 1000).toFixed(1)}s` : "–");

const plural = (n: number, one: string) => `${n} ${one}${n === 1 ? "" : "s"}`;

/**
 * The headline for a cell, then what it took that a user would notice:
 * the bench's nudges, the user's "I'm back" messages and the dialogs
 * answered ("· 1 nudge", "· 2 approvals"), and tool calls the harness
 * refused unasked (auto mode: "· 1 refused"), so a pass that needed them says so.
 */
export function keyOf(r: ScenarioResult): string {
  const m = r.metrics;
  if (r.status === "blocked") return "blocked";
  if (r.status === "n/a") return m.only_after_install === true ? "only after install" : "n/a";
  if (r.status === "error" && m.bench_error === true) return "bench error";
  // The lane never started: the picker's own setup failed (drivers/base.ts addWithPicker, src/picker.ts).
  if (r.status === "error" && /^the picker's\b/.test(r.reason ?? "")) return "picker command failed";
  // A code that reached the model twice unmarked (src/arrivals.ts) is the headline, whatever the agent reported.
  const dups = typeof m.duplicate_deliveries === "number" ? m.duplicate_deliveries : 0;
  const head = dups > 0 ? `duplicate delivery${dups > 1 ? ` ×${dups}` : ""}, ${headOf(r)}` : headOf(r);
  const extra: string[] = [];
  if (typeof m.redeliveries === "number" && m.redeliveries > 0) extra.push(`${m.redeliveries} redelivered`);
  if (m.arrivals_unreadable === true) extra.push("arrivals not counted");
  if (typeof m.credentials_shown === "number" && m.credentials_shown > 0) extra.push(m.credentials_shown === 1 ? "a credential shown" : `${m.credentials_shown} credentials shown`);
  if (typeof m.internal_ids_shown === "number" && m.internal_ids_shown > 0) extra.push(plural(m.internal_ids_shown, "internal ID"));
  if (m.internal_ids_checked === false) extra.push("internal IDs not checked");
  if (typeof m.tool_servers_unexpected === "string" && m.tool_servers_unexpected) extra.push(`unexpected MCP servers: ${m.tool_servers_unexpected}`);
  else if (m.tool_servers_checked === false) extra.push("tool list not checked");
  if (typeof m.uninstall_undocumented === "number" && m.uninstall_undocumented > 0) extra.push(`${m.uninstall_undocumented} left after uninstall`);
  // S7's headline counts them already when it arrived only after nudges ("after 2 nudges").
  if (typeof m.nudges === "number" && m.nudges > 0 && !(r.id === "S7" && typeof m.wake_ms !== "number")) extra.push(plural(m.nudges, "nudge"));
  // S6n's user says "done" (or asks about the job), not "I'm back".
  const backWord = r.id === "S6n" ? (m.back === "read" ? `"done" + a question` : `"done"`) : `"I'm back"`;
  if (typeof m.back_messages === "number" && m.back_messages > 0) extra.push(m.back_messages === 1 ? backWord : `${m.back_messages} ${backWord}s`);
  if (r.id !== "S12" && typeof m.approvals === "number" && m.approvals > 0) extra.push(plural(m.approvals, "approval"));
  if (typeof m.refusals === "number" && m.refusals > 0) extra.push(`${m.refusals} refused`);
  return [head, ...extra].join(" · ");
}

function headOf(r: ScenarioResult): string {
  const m = r.metrics;
  switch (r.id) {
    case "S1":
      if (m.cant_wake_note === true) return m.install_offered === true ? "can't-wake note, but offered" : "can't-wake note";
      if (m.cant_wake_note === false) return "no can't-wake note";
      return m.install_offered_up_front === true ? "asked" : m.install_offered === true ? "offered late" : "no offer";
    case "S2":
    case "S2i":
      // The turn start (Claude Code's transcript) is a diagnostic, not a bar.
      if (typeof m.p50_ms !== "number") return "no wake";
      // S2i's tier 1 bar is on Wakeshift's part ("ours"); the reply's p50 is shown beside it.
      if (r.id === "S2i" && typeof m.p50_wakeshift_ms === "number") return `ours ${sec(m.p50_wakeshift_ms)}, reply ${sec(m.p50_ms)}`;
      return typeof m.p50_turn_ms === "number" ? `p50 ${sec(m.p50_ms)}, turn ${sec(m.p50_turn_ms)}` : `p50 ${sec(m.p50_ms)}`;
    case "S3":
      return `${m.seen ?? 0}/${m.published ?? 5}${typeof m.wakes === "number" ? `, ${plural(m.wakes, "wake")}` : ""}`;
    case "S4":
      if (m.lost === true) return "lost";
      if (m.lost !== false) return "–";
      // Presence that stayed up because a waiter came back by itself is right, not slow to clear.
      if (m.presence_clear_ms == null && typeof m.restarted_ms === "number") return `kept, restarted in ${sec(m.restarted_ms)}`;
      return `kept, clear ${sec(m.presence_clear_ms)}`;
    case "S5":
      return m.installed_nothing === true ? (m.arrived === true ? "clean, arrived" : m.held === true ? "clean, held" : "clean") : m.installed_nothing === false ? "installed" : "–";
    case "S6":
      if (m.connected !== true) return "not connected";
      return m.needed_message === true ? `conn ${sec(m.connected_ms)} after "I'm back"` : `conn ${sec(m.connected_ms)}`;
    case "S6n": {
      // What the user said is in the extras ("done"); the head is what came of it.
      const again = typeof m.watch_again === "number" && m.watch_again > 0 ? `, watch ×${m.watch_again} again` : "";
      if (m.connected !== true) return m.woke === true ? `woke, not connected${again}` : `not connected${again}`;
      return `conn ${sec(m.connected_ms)}${m.woke === true ? `, woke ${sec(m.wake_ms)}` : ", no wake"}${again}`;
    }
    case "S7":
      if (typeof m.wake_ms === "number") return sec(m.wake_ms);
      return typeof m.arrived_ms === "number" ? `after ${plural(Number(m.nudges ?? 0), "nudge")}` : "no wake";
    case "S8":
      return typeof m.lost === "number" ? `lost ${m.lost}, dup ${m.duplicates ?? 0}${m.reconnected_by_itself === true ? ", reconnected by itself" : ""}` : "–";
    case "S9": {
      // What didn't hold, by name: a pass on a tier that doesn't check it says "ok, but …", not "mismatch".
      const off = [m.harness_ok === false ? "wrong harness" : "", m.session_key_ok === false ? "session_key not Wakeshift's" : "", m.conversation_ok === false ? "no conversation_id" : ""].filter(Boolean);
      if (!off.length) return "ok";
      return r.grade === "pass" ? `ok, but ${off.join(", ")}` : off.join(", ");
    }
    case "S11":
      if (m.joined !== true) return "didn't join";
      return `${m.hands_completed ?? 0} hands, p50 ${sec(m.p50_act_ms)}${typeof m.timeouts === "number" && m.timeouts ? `, ${plural(m.timeouts, "timeout")}` : ""}`;
    case "S12":
      if (m.joined !== true) return "didn't join";
      return `${m.approvals ?? 0} approvals, ${m.posts ?? 0} posts`;
    case "S13": {
      if (typeof m.posts !== "number") return "–";
      const head = m.lost === 0 ? `${m.arrived}/${m.posts}, max ${sec(m.max_ms)}` : `lost ${m.lost}/${m.posts}`;
      const why = [typeof m.restarts === "number" ? plural(m.restarts, "restart") : "", m.asked_user === true ? "asked the user" : "", m.expired === true ? "expired" : ""].filter(Boolean);
      return why.length ? `${head}, ${why.join(", ")}` : head;
    }
    case "S14": {
      if (m.joined !== true) return typeof m.join_calls === "number" && m.join_calls === 0 ? "didn't join" : "–";
      const games = Number(m.games_played ?? 0);
      const joins = Number(m.join_calls ?? 0);
      const why = [joins !== 1 ? plural(joins, "join") : "", m.acted_each_game === false ? "missed a game" : "", m.left_early === true ? "left early" : ""].filter(Boolean);
      return [plural(games, "game"), ...why].join(", ");
    }
    case "S15": {
      // Per app: seen of published (app 1 + app 2), then what went wrong.
      if (typeof m.app1_published !== "number" || typeof m.app2_published !== "number") return "–";
      const head = `${m.app1_seen ?? 0}/${m.app1_published} + ${m.app2_seen ?? 0}/${m.app2_published}`;
      const why = [typeof m.lost === "number" && m.lost ? `lost ${m.app1_lost ?? 0}+${m.app2_lost ?? 0}` : "", typeof m.duplicates === "number" && m.duplicates ? `dup ${m.app1_duplicates ?? 0}+${m.app2_duplicates ?? 0}` : ""].filter(Boolean);
      return why.length ? `${head}, ${why.join(", ")}` : `${head}, max ${sec(m.max_ms)}`;
    }
    case "S16": {
      if (typeof m.watch_calls !== "number") return "–";
      const told = m.woke === true ? "told unasked" : m.told_when_asked === true ? "told when asked" : "never told";
      return `${told}, watch ×${m.watch_calls}, status ×${m.status_calls ?? 0}`;
    }
    case "S17":
    case "S17d": {
      if (typeof m.side_effect_calls !== "number") return "–";
      const told = m.woke === true ? "woke" : m.told_when_asked === true ? "told when asked" : "never told";
      return `${plural(m.side_effect_calls, "job")} started, ${told}`;
    }
    case "S18": {
      if (m.watch_called !== true) return "didn't watch";
      const q = typeof m.questions === "number" && m.questions > 0 ? plural(m.questions, "question") : "no question";
      const p = typeof m.wakeshift_prompts === "number" && m.wakeshift_prompts > 0 ? `, ${plural(m.wakeshift_prompts, "Wakeshift prompt")}` : "";
      return `${q}${p}, ${m.woke === true ? `woke in ${sec(m.wake_ms)}` : "no wake"}`;
    }
    case "S19":
    case "S19h": {
      if (typeof m.woke !== "number") return m.installed_from === false ? "didn't install" : "–";
      const why = [m.updated !== true ? "update failed" : "", m.pin_moved === false ? "pin not moved" : "", typeof m.questions === "number" && m.questions > 0 ? plural(m.questions, "question") : "", m.installer_hooks_left === true ? "installer hooks left" : ""].filter(Boolean);
      return [`woke ${m.woke}/${m.of ?? 3}`, ...why].join(", ");
    }
    case "S20": {
      if (typeof m.woke_while_disabled !== "boolean") return "–";
      const off = m.woke_while_disabled || m.woke_with_variable === true ? `woke while off (${[m.woke_while_disabled ? "disable" : "", m.woke_with_variable === true ? "variable" : ""].filter(Boolean).join(", ")})` : "stayed off";
      const lost = m.arrived_after_enable === false || m.arrived_after_variable === false ? ", lost after back on" : "";
      return off + lost;
    }
    case "S21":
      if (typeof m.woke_in_a_row !== "number") return "–";
      return `${m.woke_in_a_row}/${m.of_in_a_row} in a row, ${m.woke_after_restart ?? 0}/${m.of_after_restart ?? 3} after restart${m.back_after_restart === true ? ` (after "I'm back")` : ""}`;
    // The realistic tier (src/realistic.ts).
    case "R1": {
      const sub = m.subscribed === true ? `subscribed (${m.subscribed_via})` : typeof m.polled === "number" && m.polled > 0 ? `polled ${m.polled}×` : "didn't subscribe";
      return `${sub}, offer ${m.offer_how ?? "–"}${m.question_has_command === false ? " (no command)" : ""}`;
    }
    case "R2":
    case "R6":
    case "R4":
    case "R5": {
      const told = m.woke === true ? `told in ${sec(m.told_ms)}` : m.told_after_asking === true ? "told when asked" : "never told";
      if (r.id === "R4") return `${told}, ${m.installed_nothing === true ? "nothing installed" : "installed"}`;
      if (r.id === "R5") return `${told}, ${m.task_done === true ? "task done" : "task not done"}`;
      return m.presented === false && m.woke === true ? `${told}, unclear` : told;
    }
    case "R7":
    case "R9": {
      if (m.started_build !== true) return "didn't start a build";
      const line = m.native_line === true ? "native line" : "no native line";
      const offer = m.offer_in_result === true ? ", install offered" : m.offer_relayed === true ? ", asked to install" : "";
      const sub = m.streamed === true ? "subscribed itself" : "didn't subscribe itself";
      const told = r.id === "R9" && m.ask_works !== undefined ? (m.woke === true ? `, told in ${sec(m.told_ms)}` : m.told_after_asking === true ? ", told when asked" : m.woke === false ? ", never told" : "") : "";
      return `${line}${offer}, ${sub}${told}`;
    }
    case "R8": {
      const told = m.woke === true ? `told in ${sec(m.told_ms)}` : m.told_after_asking === true ? "told when asked" : "never told";
      return m.presented === false && m.woke === true ? `${told}, unclear` : told;
    }
    case "R10": {
      if (m.form_shown !== true) return "no form";
      const conn = m.connected === true ? `conn ${sec(m.connected_ms)}` : m.installed === true ? "not connected" : "not installed";
      const wake = m.woke === true ? `, woke in ${sec(m.wake_ms)}` : m.connected === true ? ", no wake" : "";
      return `form: ${m.form_answer ?? "unanswered"}, ${conn}${wake}${m.agent_ran_install === true ? ", the agent ran the install" : ""}`;
    }
    case "R3":
      if (m.offer_relayed !== true && m.asked !== true) return "never offered";
      if (m.connected === true) return `installed, conn ${sec(m.connected_ms)}`;
      return m.installed === true ? "installed, not connected" : "not installed";
  }
}

/** Grades every scenario of a run in place. */
export function gradeRun(run: RunResult, tiers: Tiers): RunResult {
  for (const h of run.harnesses) {
    const tier = tierOf(tiers, h.id);
    h.tier = tier?.id ?? "untiered";
    for (const s of h.scenarios) grade(s, tier);
  }
  return run;
}
