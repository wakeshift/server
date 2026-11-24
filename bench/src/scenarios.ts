// The scenarios, the same for every harness. A harness runs in two phases,
// each a fresh temp HOME and conversation:
//   a (nothing installed): S1, S5, and on a zero-install wake path S2, S3, S4, S7
//   b (install: yes):      S6, S2i (idle wakes through the install), then
//                          S3, S4, S7 on the installed path if a didn't run
//                          them, then S8
// After a restart or the install's trust step the user says only "ok, I'm
// back" (BACK), as a real user would, never what to do; anything more is a
// nudge, counted and shown in the cell.
// S9 is computed from every call either phase made. S13 (asked for by name,
// 35+ minutes idle) runs in phase a on the zero-install path, or with
// WB_S13_PATH=installed in phase b, after S6. S15 (asked for by name, a
// second app) runs in phase b after S6 (WB_S15_PATH=zero-install: phase a,
// on a zero-install wake path), before S8. S16, S17 and S17d (asked for
// by name) each run after both phases in a lane of their own (a fresh HOME
// and conversation), with the toy app's job tools on that lane only.
// S19 and S19h (asked for by name, Claude Code on a local core) each run
// in a lane of their own too: Wakeshift's plugin updated under a
// conversation it keeps connected (phaseS19).

import { benchEventText, jobStatusText } from "../app/jobs.js";
import { INSTALLER } from "../drivers/base.js";
import { installedPluginVersion, pluginInstalled, scriptHooks } from "../drivers/claude-code/index.js";
import { compareVersions, offerFitsVersion, offerForm, PLUGIN_ONE_COMMAND } from "../drivers/claude-code/offer.js";
import type { AnswerPolicy, Driver, Turn } from "../drivers/types.js";
import { arrivalOutcome, type ArrivalOutcome } from "./arrivals.js";
import { tokensUsed } from "../drivers/container.js";
import { marketplaceSource, SECOND_MCP_NAME, type LaneState, type PluginUpgrade, type Published, type ToyApp } from "./core.js";
import { checkToolServers } from "./toolservers.js";
import { Lane, type RunCtx } from "./lane.js";
import { appSide, argsCheck, breakdownOf, burst, idleWake, install, KillWatch, longIdle, noNudge, ourPart, rearm, restart, sideEffectOnce, stageMedians, statusFallback, twoApps, type Outcome } from "./metrics.js";
import { median, type Breakdown, type ScenarioId, type ScenarioResult } from "./model.js";
import { join } from "node:path";
import { existsSync, readdirSync, readFileSync, writeFileSync } from "node:fs";
import { ps } from "./procs.js";
import { redactPlain } from "./redact.js";
import { sleep } from "./tmux.js";
import { applyExposures, internalIds, loggedDeliveries, uninstallLeftovers } from "./visible.js";

/**
 * WB_ASK_WITH_PICKER=1 adds this line to INTRO, so a harness that has a
 * question tool (Hermes's, Cursor's, opencode's) is steered to it and its
 * picker handling is exercised live. Off by default: results stay
 * comparable with runs without it, and the report's notes say when it's on.
 */
export const ASK_WITH_PICKER = "If you need to ask me anything, ask with your question tool (the one that shows me options to pick from), not in a chat reply.";
export const INTRO =
  "This is an automated test (wakebench). Use the wakebench MCP server's watch tool now so you're woken when bench events arrive, and do what its result says, " +
  "including anything it says to ask me first. Before you act on it, quote the result's instructions to me word for word, in a block whose first line is QUOTE: . " +
  "From then on, whenever bench events reach you (codes like WB-ab12-1), call the wakebench report_seen tool with their codes, then reply with one line per event: GOT <code>. Do nothing else with them." +
  (process.env.WB_ASK_WITH_PICKER === "1" ? " " + ASK_WITH_PICKER : "");
export const SAY_NO = "No, don't install anything. Keep using what works without an install.";
export const SAY_YES = "Yes, please install it.";
export const SAY_YES_AGAIN = "Yes, that's fine too. Go ahead.";
export const ASK_INSTALL = "Please do the one-time install the watch result mentions, so you can be woken.";
export const NUDGE_EVENTS =
  "Did any bench events reach you that you haven't reported yet? If so, report just those, as before. Then make sure you'll be woken for the next ones, as the watch result said.";
export const REARM = "Call the wakebench unwatch tool, then call watch again and do what its result says, so you keep getting bench events.";
/** A nudge: the bench tells the agent what to do, which no user would (counted as a nudge). */
/** How every background command Wakeshift gives starts (internal/harnesses LinkStart). */
const LINK_START = "f=$HOME/.wakeshift/links/";

/** The neutral note an agent Wakeshift can't wake gets (internal/harnesses CantWake). */
export const CANT_WAKE = /Wakeshift can't wake (this agent|[^.]* conversations), so nothing will notify you here/;

export const WATCH_AGAIN = "Call the wakebench watch tool again (pass your session_key if you have one), and report any bench events in its result as before.";
/**
 * What a user says on coming back after a restart or the install's trust
 * step: nothing about what to do. Install notes ask for "any message" where
 * the agent needs a turn; it's counted, but it isn't a nudge.
 */
export const BACK = "ok, I'm back";
/**
 * WB_S6_BACK=report_seen: after the trust step the user's message asks for a
 * tool that subscribes to nothing (report_seen) and not watch, as an agent
 * that calls a read tool after the restart does (the Webhooks app's
 * recent_posts, without its session_key). The install then connects
 * only if that result does, and there's no nudge to call watch again.
 */
export const BACK_REPORT = "ok, I'm back. Report any bench events you've received with report_seen (an empty list if none). Don't call watch.";
export const backText = () => (process.env.WB_S6_BACK === "report_seen" ? BACK_REPORT : BACK);
/**
 * WB_S8_REWATCH=1: nothing is published while S8's harness is down, and
 * after the restart the user asks the agent to call watch again at once, as a resumed conversation does (a redeem-by-tool-call
 * prototype's waiter once exited "the link was revoked" there, and nothing
 * woke it). WB_S8_REWATCH=fresh asks for a watch without the session_key,
 * so the re-watch starts a new Wakeshift session, which supersedes the
 * conversation's earlier one and revokes its link, as a model that drops
 * the key does. WB_S8_IDLE=<seconds> leaves the session idle that long
 * before the last publish, which must then wake it by itself.
 */
export const BACK_REWATCH_KEEP = "ok, I'm back. Call watch again so you keep getting the bench events, then just say ok.";
export const BACK_REWATCH_FRESH = "ok, I'm back. Call watch again so you keep getting the bench events, as a fresh watch: don't pass a session_key. Then just say ok.";
export const s8Rewatch = () => process.env.WB_S8_REWATCH === "1" || process.env.WB_S8_REWATCH === "fresh";
const rewatchText = () => (process.env.WB_S8_REWATCH === "fresh" ? BACK_REWATCH_FRESH : BACK_REWATCH_KEEP);
export const s8IdleMs = () => Math.max(0, Math.min(Number(process.env.WB_S8_IDLE ?? 0) || 0, 3600)) * 1000;
/** The user ran the installer themselves (the agent couldn't), and says so. */
export const RAN_IT = "Done, I ran it.";
/**
 * S6n (no nudge): what the user says after the install's trust step, as
 * real users do: "done", and nothing about what to call. With
 * WB_S6N_BACK=read the user asks something unrelated instead, which the
 * app's read tool answers (job_status, the toy app's on that lane), as an
 * agent that calls a read tool after the restart does.
 */
export const DONE = "done";
export const DONE_READ = "done. By the way, what's the status of the latest bench job?";
export function s6nBack(env: NodeJS.ProcessEnv = process.env): "done" | "read" {
  const v = env.WB_S6N_BACK ?? "done";
  if (v !== "done" && v !== "read") throw new Error(`WB_S6N_BACK=${v}: use done or read`);
  return v;
}
/** How long the user takes, after the trust step or a restart, before saying BACK (where nothing has happened by then). */
export const BACK_AFTER_MS = 20_000;

const NO: AnswerPolicy = { allowInstall: false };
const YES: AnswerPolicy = { allowInstall: true };
/** Phase b's first turn: no to the installer unless the user picks yes where the agent asks with a picker. */
const PICK_YES: AnswerPolicy = { allowInstall: false, pick: "yes" };

export const OFFER = /\binstall/i;
/** A question the installer made the agent ask (OpenClaw's network hooks). */
export const FOLLOWUP = /(network|listen|gateway|hooks?|firewall|reachable|all right|okay|agree|proceed|go ahead|continue)[^\n]{0,300}\?/i;
export const ASKS = /\binstall[^\n]{0,400}\?|(want me to|should I|shall I|would you like( me)? to|do you want( me)? to|let me know if|say if|tell me if|if you('d| would)? like|if you want)[^\n]{0,200}(install|hooks?|plugin|connector|set (it|this) up)|(install|hooks?|plugin|connector)[^\n]{0,200}(okay|ok)\?/i;

/**
 * Text with the terminal's wrapping undone: a line that doesn't end a
 * sentence and is followed by one that carries on (a lowercase letter, a
 * digit, a bracket or a backquote) is joined to it, after taking off the
 * TUI's borders and markers. ASKS and FOLLOWUP look within a line, and a
 * question wrapped as "Want me to install the\n  hooks?" was missed.
 */
export function unwrap(text: string): string {
  const lines = text.split("\n").map((l) => l.replace(/^[\s│┃▎|>⎿●•]+/, "").replace(/[\s│┃]+$/, ""));
  const out: string[] = [];
  for (const l of lines) {
    const prev = out.length ? out[out.length - 1] : null;
    if (prev !== null && prev !== "" && l !== "" && !/[.!?:]["')\]]?$/.test(prev) && /^[a-z0-9(`'"[]/.test(l)) out[out.length - 1] = `${prev} ${l}`;
    else out.push(l);
  }
  return out.join("\n");
}

/**
 * S1's reading of the install offer. offered and asked are the agent's own
 * words (a mention of the install, a question about it); picker is the
 * harness's question picker about it, which the driver answered (Claude
 * Code's AskUserQuestion, OpenClaw's, Copilot's; picked is set only by
 * each driver's install-question reader, installPicker and the like, never
 * for a picker about anything else): its question and options
 * are the ones Wakeshift's text gives word for word, so they're never the
 * agent's own words, yet it's the ask Wakeshift's text asks for. Any of the
 * three before anything ran is asking up front. (Until November 9, 2026 the
 * picker counted only by luck: Claude Code shows its options capitalized and
 * reworded, so ownWords kept them, and OFFER found "nothing is installed" in
 * them.)
 */
export function s1Offer(own: string, picked: unknown): { offered: boolean; asked: boolean; picker: boolean; upFront: boolean } {
  const offered = OFFER.test(own);
  const asked = asksInstall(own);
  const picker = picked === "yes" || picked === "no";
  return { offered, asked, picker, upFront: asked || offered || picker };
}

/** Whether the agent's words ask about the install (ASKS), on the text as shown or with its wrapping undone. */
export function asksInstall(text: string): boolean {
  return ASKS.test(text) || ASKS.test(unwrap(text));
}

/** Whether the code shows in text other than as the agent's GOT line or its report_seen call. */
export function shownRaw(text: string, code: string): boolean {
  for (const line of text.split("\n")) {
    if (!line.includes(code)) continue;
    if (/report_seen|GOT\s/.test(line)) continue;
    return true;
  }
  return false;
}

/**
 * S13's path (WB_S13_PATH): "zero-install" (default; phase a, Claude Code's
 * Monitor) or "installed" (phase b, after S6: through the install).
 */
export function s13Path(env: NodeJS.ProcessEnv = process.env): "zero-install" | "installed" {
  return env.WB_S13_PATH === "installed" ? "installed" : "zero-install";
}

/**
 * S15's path (WB_S15_PATH): "installed" (default; phase b, after S6, where
 * every harness with an install is woken through it, and where a second
 * app's link sits next to the first's) or "zero-install" (phase a, Claude
 * Code's Monitor, for a harness that wakes with nothing installed).
 */
export function s15Path(env: NodeJS.ProcessEnv = process.env): "zero-install" | "installed" {
  return env.WB_S15_PATH === "zero-install" ? "zero-install" : "installed";
}

/**
 * S18's first message, in a new conversation with the install already in:
 * watch and report, with nothing about installing or asking first.
 */
export const S18_ASK =
  "This is an automated test (wakebench), in a new conversation. Use the wakebench MCP server's watch tool now so you're woken when bench events arrive, and do what its result says. " +
  "Whenever bench events reach you (codes like WB-ab12-1), call the wakebench report_seen tool with their codes, then reply with one line per event: GOT <code>.";

/**
 * Wakeshift's text asking the agent to offer the install: its offer
 * ("Ask the user now whether to install Wakeshift's plugin for …") or
 * Claude Code's picker for it. In a new conversation with the plugin
 * installed it's a bug (Codex and Copilot CLI each had it once).
 */
export const INSTALL_OFFER = /AskUserQuestion|whether to install/i;

/** The harnesses S18 runs on (their fresh-conversation start and install checked). */
export const S18_HARNESSES = ["claude-code", "codex", "pi", "copilot", "fake"];

/** Whether S18 runs on a harness: one with an install, among S18_HARNESSES. */
export function s18Applies(d: Pick<Driver, "id" | "installPath">): boolean {
  return d.installPath !== null && S18_HARNESSES.includes(d.id);
}

/** Why S18 doesn't run on a harness. */
export function s18NotApplies(d: Pick<Driver, "id" | "installPath">): string {
  return d.installPath === null ? "nothing to install" : "not checked on this harness yet: its install persists, but its start in a new conversation after S6 hasn't been verified (S18 runs on Claude Code, Codex, pi and Copilot CLI)";
}

/**
 * A dialog the user answered for Wakeshift's own command (the Monitor, a
 * background wait, the installer, a shell command), not for the app's MCP
 * tool, which the harness asks about itself in manual mode.
 */
export function wakeshiftPrompt(p: { kind: string; asks: string; chosen: string }): boolean {
  if (p.kind !== "approval") return false;
  const t = `${p.chosen} ${p.asks}`;
  if (/\bMCP\b|mcp__|wakebench[.\s—-]+(watch|report_seen|unwatch)/i.test(t)) return false;
  return /Monitor|installer|Bash|shell|command|wakeshift|curl/i.test(t);
}

/** Text without the plugin's mark (internal/pluginmark), as manual mode's dialog shows it: not the agent's words. */
export function withoutMarkLines(text: string): string {
  return text.replace(/^.*wakeshift-plugin=.*$/gm, "");
}

/** S15's messages: what a user says to watch the first app again, add the second, and so on (script, not nudges). */
export const S15_WATCH_1 = "Call the wakebench watch tool again (pass your session_key), so you keep getting its bench events, and report any in its result as before.";
export const S15_WATCH_2 =
  `There's a second bench app now: the ${SECOND_MCP_NAME} MCP server. Use its watch tool too, and do what its result says (it gives a session_key of its own: pass that one on later ${SECOND_MCP_NAME} calls). Keep watching wakebench as well. ` +
  `Its events have codes like WB-ab12-1 too: report each code with the report_seen tool of the server it came from, then reply GOT <code> as before.`;
export const S15_REWATCH_2 = `Call the ${SECOND_MCP_NAME} watch tool again (pass its session_key), and report any bench events in its result as before.`;
export const S15_NEW_SESSION_1 =
  "Call the wakebench watch tool again, but this time leave the session_key argument out, so it starts a new session. Do what its result says, and report any bench events as before.";
/** S15's one nudge (counted), for codes still missing at the end. */
export const NUDGE_EVENTS_2 =
  `Did any bench events from wakebench or ${SECOND_MCP_NAME} reach you that you haven't reported yet? If so, report just those, each with the report_seen tool of the server it came from, as before. Then make sure you'll be woken for the next ones from both, as the watch results said.`;

/**
 * S16 and S17: what the user says. Realistic, not the mechanics tier's
 * script: never Wakeshift, a tool by name, or waking.
 */
export const S16_ASK = "A bench job is running on wakebench. Watch it and let me know when it finishes.";
export const S17_ASK = "Start a bench job on wakebench and let me know when it's done.";
/** S17, where the agent didn't bring the install up: the user asks for it (as S6's ASK_INSTALL, without naming a tool). */
export const S17_SETUP = "Please do the one-time setup its result mentions, so you can tell me when it's done.";
/** The scenarios an external app runs (WB_EXTERNAL_APP, src/external.ts): it has no watch or report_seen tool. */
export const EXTERNAL_SCENARIOS: ScenarioId[] = ["S6", "S2i", "S18", "S17", "S17d"];
/** S6 on an external app, where the agent didn't bring the install up (ASK_INSTALL, without naming the toy app's tool). */
export const ASK_INSTALL_EXT = "Please do the one-time install the tool's result mentions, so you can be woken.";
/** The call that subscribed the conversation: the toy app's watch, or one of an external app's subscribing tools. */
export const isWatch = (c: { tool: string; subscribes?: boolean }) => c.tool === "watch" || c.subscribes === true;
/** The call that started a job: the toy app's start_job, or one of an external app's job tools. */
export const isStart = (c: { tool: string; role?: string }) => c.tool === "start_job" || c.role === "start";
/** An external app's job news, said as news (its job's end is "completed", "finished" or "done", not "passed"). */
export const DONE_WORDS = /\b(complete[ds]?|finish(ed|es)|done|ready|succeeded)\b/i;

/** S16, S17: the user checks in, without a word about how. */
export const ANY_NEWS = "Any news?";
/** The bench job's result: the tag of the event S16 and S17 publish (its text: "Bench event WB-…-1 (the bench job passed).") */
export const JOB_RESULT = "the bench job passed";

/** How long S16 waits after the job finishes before the user asks (WB_S16_WAIT_SECONDS, default 75). */
export function s16WaitMs(env: NodeJS.ProcessEnv = process.env): number {
  const n = Number(env.WB_S16_WAIT_SECONDS ?? 75);
  return (Number.isFinite(n) && n >= 10 ? Math.min(n, 900) : 75) * 1000;
}

/** How long S17 sits idle, once the install is done, before the job finishes (WB_S17_IDLE_SECONDS, default 30). */
export function s17IdleMs(env: NodeJS.ProcessEnv = process.env): number {
  const n = Number(env.WB_S17_IDLE_SECONDS ?? 30);
  return (Number.isFinite(n) && n >= 4 ? Math.min(n, 900) : 30) * 1000;
}

/**
 * The tool output a pane shows for the job tools (job_status, start_job,
 * the event's own text), and tool calls and their approval dialogs (Codex's
 * "Calling wakebench.report_seen … ids: ["WB-…"]", Claude Code's
 * "wakebench - job_status (MCP)"): not the agent's words.
 */
const JOB_TOOL_LINE = /Bench event WB-|^[\s⎿│└>]*(Status: (running|finished)|Started bench job #|\d+ bench jobs started)|report_seen|\bids:\s*\[|\bCall(ed|ing)\s+\w+[.(]|\b\w+\.\w+\(\{|Allow the \w+ MCP server|\(MCP\)/;

/** Whether text (the agent's own words) tells the user the job's result: its code, or that it passed, said as news (not "I'll tell you whether it passed"). */
export function toldResult(own: string, code: string | null, extra?: RegExp, before?: string): boolean {
  return toldSentence(own, code, extra, before) !== null;
}

/** The line or sentence toldResult found (null: none), for the notes. */
export function toldSentence(own: string, code: string | null, extra?: RegExp, before?: string): string | null {
  // before: what the pane showed before the job ended; a sentence found there (whitespace aside: a TUI re-wraps) isn't news.
  const flat = (x: string) => x.replace(/[\s│┃▎|>⎿●•⏺]+/g, "");
  const old = before ? flat(before) : "";
  // Lines already on screen before (by their text without whitespace, so a re-wrapped line still matches) go
  // first, so what's left to join into sentences is only new text.
  if (old) own = own.split("\n").filter((l) => flat(l).length < 8 || !old.includes(flat(l))).join("\n");
  for (const line of unwrap(own).split("\n")) {
    if (JOB_TOOL_LINE.test(line)) continue;
    // Claude Code's rotating tips and its spinner line ("✽ Whisking…"), whole lines of the harness's.
    if (extra && /\bTip:|…\s*(\(.*)?$/.test(line)) continue;
    // An external app's code is its job's ID, which the agent knows from the start (code null): only news counts there.
    if (code !== null && line.includes(code)) return line;
    for (const sentence of line.split(/(?<=[.!?])\s+/)) {
      if (!/\b(pass(ed|es)?|succeed(ed|s)?|success(ful(ly)?)?|green)\b|✅/i.test(sentence) && !extra?.test(sentence)) continue;
      if (/\b(once|when|whether|if|until|as soon as)\b|\bnot (yet )?(passed|finished|done)|hasn'?t|still running|no (news|result|update)/i.test(sentence)) continue;
      if (old && flat(sentence).length >= 8 && old.includes(flat(sentence))) continue;
      // A fragment ("\"done\".") isn't news: an external app's needs three words.
      if (extra && sentence.trim().split(/\s+/).length < 3) continue;
      // The harness's own lines, not the agent's: Claude Code's turn line ("Brewed for 3s · done 11:38 PM"), its banners, the user's prompt.
      if (extra && /· done \d|\bfor \d+(m \d+)?s\b|finished work sooner|^\s*[❯›>]/i.test(sentence)) continue;
      return sentence;
    }
  }
  return null;
}

/** Whether the agent promised to tell the user by itself ("I'll let you know when it finishes"). */
export const PROMISE = /\b(I'?ll|I will|I'?m going to|will)\s+(let you know|notify you|tell you|ping you|report back|update you|get back to you|flag it|alert you)|you'?ll (be notified|hear from me|get a notification|be told)/i;

/** Agent words in a turn: ownWords without the user's messages, Wakeshift's texts and the job tools' output. */
export function jobWords(text: string, theirs: string[]): string {
  // Codex marks a tool's output with └ and Claude Code its own lines with ⏺, which ownWords doesn't take off.
  const plain = text
    .split("\n")
    .map((x) => x.replace(/^(\s*)[└⏺]\s?/, "$1"))
    .join("\n");
  return ownWords(plain, theirs)
    .split("\n")
    .filter((x) => !JOB_TOOL_LINE.test(x))
    .join("\n");
}

/** S13: the agent asking the user whether to keep watching (re-arm, restart, start again), in its own words. */
export const ASKS_REARM =
  /\b(re-?arm|restart|start (it|the monitor|watching|listening) again|keep (watching|monitoring|listening))\b[^\n]{0,200}\?|(want me to|should I|shall I|would you like( me)? to|do you want( me)? to|let me know if)[^\n]{0,120}\b(re-?arm|restart|start (it|the monitor|watching|listening) again|keep (watching|monitoring|listening))\b/i;
/** S13: the agent telling the user it stopped watching. */
export const STOPPED = /\b(no longer|not|stopped|isn't|is not|am not|aren't) (watching|monitoring|listening)\b/i;

/**
 * S13's checks on the agent's own words (ASKS_REARM, STOPPED), leaving out
 * Wakeshift's restart line and Claude Code's notices about the Monitor.
 * ASKS_REARM takes whole words and names the restart itself, so the install
 * question ("…without a Monitor that has to be restarted every 25
 * minutes?", which a TUI redraws in the log) isn't counted as asking.
 */
export function rearmWords(own: string): { asked: boolean; stopped: boolean } {
  const kept = unwrap(own)
    .split("\n")
    .filter((x) => !/hub\.restart|Routine restart|Monitor expired|Monitor event/i.test(x))
    .join("\n");
  return { asked: ASKS_REARM.test(kept), stopped: STOPPED.test(kept) };
}

export const gotRe = (code: string) => new RegExp(`GOT\\s+${code.replace(/[.*+?^${}()|[\]\\-]/g, "\\$&")}\\b`);
/**
 * An external app's GOT line: only at the start of a line (past the TUI's
 * markers). Its code is the job's ID, which the agent knows from the start,
 * and says ahead ("I'll reply \"GOT 897128\" when it finishes"). An ID that
 * ends in the code counts too: an MCP task's ID wraps the job's, and Claude
 * Code reports "GOT job-…-<job>".
 */
export const gotLineRe = (code: string) => new RegExp(`^[\\s⏺•●│▎>*-]*GOT\\s+[\\w.-]*${code.replace(/[.*+?^${}()|[\]\\-]/g, "\\$&")}\\b`, "m");

/**
 * Each event's wake turn: the turn whose queued text names its code, else
 * (a transport that cut the text off before the code) the first turn
 * queued after it was published that no other event claimed.
 */
export function matchTurns(turns: Turn[], evs: { code: string; at: string }[]): Map<string, Turn> {
  const out = new Map<string, Turn>();
  const used = new Set<Turn>();
  for (const e of evs) {
    const t = turns.find((x) => x.text.includes(e.code));
    if (t) {
      out.set(e.code, t);
      used.add(t);
    }
  }
  for (const e of evs) {
    if (out.has(e.code)) continue;
    const t = turns.find((x) => !used.has(x) && x.source !== "other" && x.queued >= Date.parse(e.at));
    if (t) {
      out.set(e.code, t);
      used.add(t);
    }
  }
  return out;
}

export interface Phase {
  lane: Lane;
  results: ScenarioResult[];
}

export class Scenarios {
  /** Results so far (kept when a phase fails part way). */
  readonly results: ScenarioResult[] = [];
  readonly lanes: Lane[] = [];
  /** Container harnesses: the model's tokens over every lane, read as each lane ends (best effort). */
  tokens: { input: number; output: number; cached: number } | null = null;
  /** The scenario in progress, marked error if the phase fails. */
  private current: ScenarioResult | null = null;

  constructor(
    readonly ctx: RunCtx,
    readonly driver: Driver,
    readonly want: Set<ScenarioId>,
  ) {}

  /** An external app (WB_EXTERNAL_APP): the user's words come from its adapter, in the vendor's own terms. */
  private get ext() {
    return this.ctx.bench.external ?? null;
  }
  /** The agent's GOT line for a code (gotLineRe on an external app). */
  private got(code: string) {
    return this.ext ? gotLineRe(code) : gotRe(code);
  }
  private get intro() {
    return this.ext?.prompts.intro ?? INTRO;
  }

  private result(id: ScenarioId, l: Lane): ScenarioResult {
    const r: ScenarioResult = { id, harness: this.driver.id, status: "ran", metrics: {}, steps: l.begin(), notes: [], startedAt: new Date().toISOString() };
    this.current = r;
    this.open(r, l);
    return r;
  }

  /**
   * The spans of time each scenario ran in (S5 is split around S2–S7), so
   * the nudges, "I'm back" messages and dialogs answered in them are its
   * own: shown in its cell, so a pass that needed one says so.
   */
  private windows = new Map<ScenarioResult, { lane: Lane; spans: { from: number; to: number | null }[] }>();

  /** Starts (or resumes) r's span; any other scenario's open span ends. */
  private open(r: ScenarioResult, l: Lane) {
    const now = Date.now();
    for (const w of this.windows.values()) for (const s of w.spans) if (s.to === null) s.to = now;
    const w = this.windows.get(r) ?? { lane: l, spans: [] };
    w.spans.push({ from: now, to: null });
    this.windows.set(r, w);
    for (const s of this.segments) if (s.to === null) s.to = s.lane.logSize();
    this.segments.push({ r, lane: l, from: l.logSize(), to: null });
  }

  /** The stretch of each lane's pane log each scenario ran in (byte offsets): what it showed the user (idsShown). */
  private segments: { r: ScenarioResult; lane: Lane; from: number; to: number | null }[] = [];

  /** r's nudges, "I'm back" messages and approvals (dialogs answered), as metrics. */
  private visibility(r: ScenarioResult) {
    const w = this.windows.get(r);
    if (!w) return;
    // Saved rules first, while r's span is still open: what it finds is stamped now, inside the span.
    w.lane.checkSavedRules();
    const now = Date.now();
    for (const s of w.spans) if (s.to === null) s.to = now;
    const within = (at: string) => {
      const t = Date.parse(at);
      return w.spans.some((s) => t >= s.from && t <= (s.to ?? now));
    };
    // A Wakeshift credential in a dialog the user answered, or in a rule a harness saved.
    applyExposures(r, w.lane.exposures, within);
    r.metrics.nudges = this.nudges.filter((n) => within(n.from)).length;
    r.metrics.back_messages = this.backs.filter((n) => within(n.from)).length;
    r.metrics.approvals = w.lane.prompts.filter((p) => (p.kind === "approval" || p.kind === "trust") && within(p.at)).length;
    // Tool calls the harness refused without asking (Claude Code's auto mode), and why.
    if (this.driver.refusals) {
      const refused = this.driver.refusals(w.lane, Math.min(...w.spans.map((s) => s.from))).filter((x) => within(x.at));
      r.metrics.refusals = refused.length;
      for (const x of refused) r.notes.push(`refused without asking: ${x.tool}: ${x.reason}`);
    }
  }

  /** Says text as a nudge (counted: the bench told the agent what to do). */
  private async nudge(l: Lane, text: string, policy: AnswerPolicy, maxMs?: number) {
    const from = new Date().toISOString();
    await l.say(text, policy, maxMs);
    this.nudges.push({ from, to: new Date().toISOString() });
  }

  /** Says BACK (or RAN_IT): what a user says on coming back; counted, but not a nudge. Returns when it was said. */
  private async back(l: Lane, policy: AnswerPolicy, text = BACK, maxMs?: number): Promise<number> {
    const at = Date.now();
    const from = new Date(at).toISOString();
    await l.say(text, policy, maxMs);
    this.backs.push({ from, to: new Date().toISOString() });
    return at;
  }

  /** The harness's version as the lane's own environment (temp HOME, PATH) reports it. */
  private async recordVersion(l: Lane) {
    if (!this.driver.versionIn) return;
    try {
      l.state.version = await this.driver.versionIn(l);
      l.note(`version in this lane: ${l.state.version ?? "unknown"}`);
    } catch {
      l.state.version = null;
    }
  }

  /** Runs a phase body; on failure, the scenario in progress is an error (with its steps so far). */
  private async phase(l: Lane, body: (l: Lane, results: ScenarioResult[]) => Promise<void>): Promise<Phase> {
    this.lanes.push(l);
    try {
      await body(l, this.results);
    } catch (err) {
      // Failed while setting up (no scenario yet): the phase's first scenario
      // carries the error and the screen at that point.
      if (!this.current) {
        const id = l.phase === "a" ? "S1" : "S6";
        this.current = { id, harness: this.driver.id, status: "ran", metrics: {}, steps: l.steps, notes: [], startedAt: new Date().toISOString() };
      }
      const c = this.current;
      if (c && !this.results.includes(c)) {
        c.status = "error";
        c.reason = err instanceof Error ? err.message : String(err);
        await l.snap(`error: ${c.reason}`).catch(() => {});
        this.results.push(this.done(c));
      }
      throw err;
    } finally {
      this.current = null;
      await this.finish(l);
    }
    return { lane: l, results: this.results };
  }

  private done(r: ScenarioResult) {
    r.durationMs = r.startedAt ? Date.now() - Date.parse(r.startedAt) : undefined;
    this.visibility(r);
    return r;
  }

  static na(id: ScenarioId, harness: string, reason: string): ScenarioResult {
    return { id, harness, status: "n/a", reason, metrics: {}, steps: [], notes: [] };
  }

  /**
   * Waits for GOT <code> lines, nudging (up to nudges times) if they don't
   * come. Returns ms from t0 per code (null: never). firstMs: how long the
   * first round waits, before any nudge (default timeoutMs).
   */
  private async waitGot(l: Lane, codes: string[], t0: number, timeoutMs: number, policy: AnswerPolicy, nudges = 0, firstMs = timeoutMs): Promise<{ at: Map<string, number | null>; nudged: number }> {
    const at = new Map<string, number | null>(codes.map((c) => [c, null]));
    let nudged = 0;
    const since = l.logSize();
    for (let round = 0; round <= nudges; round++) {
      const end = Date.now() + (round === 0 ? firstMs : timeoutMs) * this.ctx.slow;
      for (const c of codes) {
        if (at.get(c) !== null) continue;
        const left = end - Date.now();
        if (left <= 0) break;
        const ms = await l.waitFor(this.got(c), left / this.ctx.slow, policy, { since });
        if (ms !== null) at.set(c, Date.now() - t0);
      }
      if ([...at.values()].every((v) => v !== null) || round === nudges) break;
      await l.snap("no GOT yet: nudging");
      nudged++;
      await this.nudge(l, NUDGE_EVENTS, policy, 180_000);
    }
    return { at, nudged };
  }

  /**
   * Waits for a woken turn's sign for code: its GOT line in the pane, or its
   * report_seen call naming it (made after `after`, not in a nudge turn).
   * Some woken turns never reach the pane: OpenClaw runs a hook's wake as a
   * heartbeat, whose reply its TUI doesn't show, so the report is all the
   * bench sees. Returns ms until either, or null.
   */
  private async waitWake(l: Lane, code: string, after: string, timeoutMs: number, policy: AnswerPolicy): Promise<number | null> {
    let reported = false;
    let lastPoll = 0;
    return l.waitFor(null, timeoutMs, policy, {
      watch: (screen, printed) => reported || this.got(code).test(screen) || this.got(code).test(printed),
      tick: async () => {
        if (Date.now() - lastPoll < 2000) return;
        lastPoll = Date.now();
        const st = await this.ctx.bench.lane(l.id).catch(() => null);
        if (st && this.reports(st, after, [code]).counts.has(code)) reported = true;
      },
    });
  }

  /** The bench's nudge turns: what an agent reports in one answers the bench, it isn't a wake. */
  private nudges: { from: string; to: string }[] = [];
  /** The user's "I'm back" turns: what an agent reports in one isn't a wake either. */
  private backs: { from: string; to: string }[] = [];

  /** report_seen calls after a time (not in answer to a nudge or an "I'm back"), and what they reported. */
  private reports(state: LaneState, after: string, codes: string[]) {
    const inNudge = (at: string) => [...this.nudges, ...this.backs].some((n) => at >= n.from && at <= n.to);
    const calls = state.seen.filter((s) => s.at >= after && !inNudge(s.at));
    // A code first reported in a nudge turn still counts as seen (once).
    for (const s of state.seen.filter((x) => x.at >= after && inNudge(x.at)))
      for (const id of s.ids) if (codes.includes(id) && !calls.some((c) => c.ids.includes(id))) calls.push({ at: s.at, ids: [id] });
    const relevant = calls.filter((s) => s.ids.some((id) => codes.includes(id)));
    const counts = new Map<string, number>();
    for (const s of relevant) for (const id of new Set(s.ids)) if (codes.includes(id)) counts.set(id, (counts.get(id) ?? 0) + 1);
    return { wakes: relevant.length, counts };
  }

  /**
   * Counts how many times each code reached the model, from the harness's
   * own record of the conversation (src/arrivals.ts), and puts the counts on
   * r: a code that arrived twice without "redelivered":true fails it as a
   * duplicate delivery, even if the agent reported it once. seen: the codes
   * the bench saw otherwise (a GOT line, report_seen). Where the record
   * can't be read, r is graded as before and its notes say so.
   */
  private async countArrivals(r: ScenarioResult, l: Lane, codes: string[], seen: Set<string>) {
    // An external app's codes are its jobs' IDs, which its own tool results show too: an arrival count would count those.
    if (this.ext) {
      r.notes.push("arrivals not counted: an external app's job IDs show in its own tool results too");
      return;
    }
    this.applyArrivals(r, await this.arrivals(l, codes, seen));
  }

  /** The arrival count (countArrivals) before it's put on a result: S3 and S15 read `once` first. */
  private async arrivals(l: Lane, codes: string[], seen: Set<string>): Promise<ArrivalOutcome & { unreadable: boolean }> {
    const d = this.driver;
    const items = d.received ? await d.received(l).catch(() => null) : null;
    const why = d.received ? `${d.name}'s record of the conversation couldn't be read` : `the bench doesn't read ${d.name}'s record of the conversation`;
    return { ...arrivalOutcome(items, codes, seen, d.name, why), unreadable: !!d.received && !items };
  }

  private applyArrivals(r: ScenarioResult, o: ArrivalOutcome & { unreadable: boolean }) {
    r.metrics = { ...r.metrics, ...o.metrics };
    if (o.unreadable) r.metrics.arrivals_unreadable = true;
    r.notes.push(...o.notes);
    if (o.failure) {
      r.reason = r.works === false && r.reason ? `${o.failure}; ${r.reason}` : o.failure;
      r.works = false;
    }
  }

  /** Deliveries Wakeshift has as acknowledged whose event the agent never showed: handed over, then lost. Null without delivery data (a hosted core). */
  private ackedUnseen(state: LaneState, codes: string[], seen: Set<string>): number | null {
    if (!state.deliveries) return null;
    const bySeq = new Map(codes.map((c) => [c.split("-").pop(), c]));
    return this.deliveryRows(state, codes).filter((d) => d.state === "acked" && !seen.has(bySeq.get(d.event.split(":").pop()) ?? "")).length;
  }

  private deliveryRows(state: LaneState, codes: string[]) {
    // The event IDs are <run>:<lane>:<seq>; codes end in -<seq>.
    const seqs = new Set(codes.map((c) => c.split("-").pop()));
    return (state.deliveries ?? []).filter((d) => seqs.has(d.event.split(":").pop()));
  }

  private async presence(l: Lane): Promise<{ connected: boolean; via: string | null }> {
    try {
      const s = await this.ctx.bench.status(l.id);
      const live = s.subscriptions.filter((x) => x.live);
      const c = live.find((x) => x.connected);
      return { connected: !!c, via: c?.via ?? null };
    } catch {
      return { connected: false, via: null };
    }
  }

  // ── Phase a ────────────────────────────────────────────────────────────

  async phaseA(): Promise<Phase> {
    return this.phase(new Lane(this.ctx, this.driver, "a", { second: this.s15In("a") }), (l, results) => this.bodyA(l, results));
  }

  async phaseB(): Promise<Phase> {
    return this.phase(new Lane(this.ctx, this.driver, "b", { second: this.s15In("b") }), (l, results) => this.bodyB(l, results));
  }

  /** Whether S15 runs in this phase: its lane then has the second app's MCP server from the start. */
  s15In(phase: "a" | "b"): boolean {
    if (!this.want.has("S15") || !this.driver.secondMcp || !this.ctx.bench.app2) return false;
    return phase === "a" ? this.driver.zeroInstallWake && s15Path() === "zero-install" : this.driver.installPath !== null && s15Path() === "installed";
  }

  private async bodyA(l: Lane, results: ScenarioResult[]): Promise<void> {
    const d = this.driver;
    const wakeHere = d.zeroInstallWake;
    await d.prepare(l);
    await this.recordVersion(l);
    await d.start(l);
    l.state.panePid = await l.tmux.panePid(l.session);

    // S1: watch, instructions, the install offer.
    const s1 = this.result("S1", l);
    await l.snap("started");
    const before = l.logSize();
    if (process.env.WB_ASK_WITH_PICKER === "1") l.note("WB_ASK_WITH_PICKER: the first message asks the agent to ask with its question tool");
    await l.say(INTRO, NO);
    const screen = await l.screen(2000);
    const turn = l.logSince(before) + "\n" + screen;
    const s1shot = await l.snap("after the watch call");
    const st = await this.ctx.bench.lane(l.id);
    const watch = st.calls.find((c) => c.tool === "watch");
    const quote = extractQuote(screen) ?? extractQuote(turn);
    const own = ownWords(turn, [withoutSuggestedAsk(watch?.added ?? "", turn), INTRO]);
    const { offered, asked, picker, upFront } = s1Offer(own, l.state.picked);
    const installedEarly = d.installed(l);
    s1.metrics = {
      watch_called: !!watch,
      quoted: !!quote,
      install_offered: offered,
      install_offered_up_front: upFront,
      asked_as_question: asked,
      installed_without_asking: installedEarly.length > 0,
      recognized_as: watch?.agent?.harness ?? null,
      asked_with_picker: picker,
    };
    s1.works = !!watch;
    s1.path = wakeHere ? "zero-install" : "held";
    if (!watch) s1.reason = "the agent didn't call watch";
    // An agent Wakeshift can't wake gets the neutral note, and no offer.
    if (d.cantWake && watch) {
      s1.metrics.cant_wake_note = CANT_WAKE.test(watch.added ?? "");
      if (!s1.metrics.cant_wake_note) {
        s1.works = false;
        s1.reason = "the result didn't say Wakeshift can't wake this agent";
      } else if (offered) {
        s1.works = false;
        s1.reason = "an install offered to an agent Wakeshift can't wake";
      } else if ((watch.added ?? "").includes(LINK_START)) {
        // The old GenericNote's background command: "generic" would run it, and nothing should give it.
        s1.works = false;
        s1.reason = "a background command given to an agent Wakeshift can't wake";
      }
    }
    if (quote) s1shot.data = { quote: redactPlain(quote) };
    if (watch?.added) s1.notes.push(`Wakeshift's text: ${watch.added.slice(0, 1500)}`);
    if (installedEarly.length) s1.notes.push(`installed without asking: ${installedEarly.join(", ")}`);
    results.push(this.done(s1));

    // The user says no (S5).
    const s5 = this.result("S5", l);
    if (l.state.picked === "no") l.note("answered no in the agent's picker");
    else if (offered || asked) {
      await l.say(SAY_NO, NO);
      await l.snap("after saying no");
    } else l.note("no install offer to answer");
    s5.metrics.installed_nothing = d.installed(l).length === 0;
    s5.metrics.answered_no = l.state.picked === "no" || offered || asked;

    if (wakeHere) {
      if (this.want.has("S2")) results.push(await this.s2(l));
      if (this.want.has("S3")) results.push(await this.s3(l, NO));
      if (this.want.has("S4")) results.push(await this.s4(l, NO));
      if (this.want.has("S7")) results.push(await this.s7(l, NO));
      if (this.want.has("S13") && s13Path() === "zero-install") results.push(await this.s13(l, NO));
      if (l.mcp2) results.push(await this.s15(l, NO));
    }

    // S5: still nothing installed, and a message arrives, or, for a
    // harness that can't be woken without the install, is held for the
    // conversation. Since November 5 no result hands held messages over (no
    // result asks for another call): the next watch
    // call must leave it held, for a listener the user may start later.
    l.steps = s5.steps;
    this.current = s5;
    this.open(s5, l);
    // Its duration: the answer above and this part, not the scenarios between.
    const between = Date.now() - Date.parse(s5.startedAt!) - (s5.steps.at(-1)?.t ?? 0);
    if (between > 0) s5.startedAt = new Date(Date.parse(s5.startedAt!) + between).toISOString();
    const [e] = await this.ctx.bench.publish(l.id, 1, "S5");
    l.note(`published ${e.code}`);
    const t0 = Date.now();
    let arrived: number | null = null;
    // On the held path, why the message isn't held (null: it is). One reason, so S5's failure says which check missed.
    let notHeld: string | null = null;
    if (wakeHere) {
      arrived = await l.waitFor(gotRe(e.code), 90_000, NO);
    } else {
      await sleep(5000);
      const at = new Date().toISOString();
      await l.say(WATCH_AGAIN, NO);
      const st2 = await this.ctx.bench.lane(l.id);
      const inResult = st2.calls.some((c) => c.at >= at && c.events_in_result.includes(e.code));
      const got = gotRe(e.code).test(await l.screen()) || gotRe(e.code).test(l.logSince(0));
      s5.metrics.in_next_result = inResult;
      s5.metrics.matched = e.matched;
      if (inResult || got) arrived = Date.now() - t0;
      // Held: the publish matched the conversation's subscription, no result
      // handed it over, and its delivery is still pending (a local core,
      // where the bench reads deliveries) or, on a hosted core, the
      // subscription that holds it is still live.
      const rows = this.deliveryRows(st2, [e.code]);
      const states = st2.deliveries ? rows.map((x) => x.state) : null;
      if (states) s5.metrics.delivery_states = states.join(",") || "none";
      // A result that handed it over fails S5 whatever the agent then showed.
      if (inResult) notHeld = "a result handed the held message over (none should since November 5)";
      // Shown with nothing installed to wake it and no result handing it over: a route this path
      // shouldn't have, so it's flagged for a look rather than counted as held.
      else if (got) notHeld = "the message reached the agent with nothing installed to wake it and no result handing it over";
      else if (!(e.matched > 0)) notHeld = "the publish matched no subscription of the conversation, so nothing holds it";
      else if (states && !states.includes("pending")) notHeld = `its delivery isn't pending (${states.join(", ") || "no delivery"})`;
      else if (!states) {
        // A hosted core: no delivery rows to read. The conversation's subscription
        // must still be live, or what it held is gone with it.
        const live = await this.ctx.bench
          .status(l.id)
          .then((x) => x.subscriptions.some((y) => y.live === true))
          .catch(() => null);
        s5.metrics.subscription_live = live;
        if (live === false) notHeld = "the conversation's subscription isn't live, so nothing holds the message";
        else if (live === null) notHeld = "couldn't read the subscription's status, so the hold isn't confirmed";
      }
      s5.metrics.held = notHeld === null;
    }
    await l.snap(wakeHere ? (arrived !== null ? `${e.code} arrived` : `${e.code} didn't arrive`) : notHeld === null ? `${e.code} held` : `${e.code} not held`);
    s5.metrics.arrived = arrived !== null;
    s5.metrics.installed_nothing = s5.metrics.installed_nothing === true && d.installed(l).length === 0;
    s5.works = s5.metrics.installed_nothing === true && (wakeHere ? arrived !== null : notHeld === null);
    if (!s5.works) s5.reason = !s5.metrics.installed_nothing ? `installed: ${d.installed(l).join(", ")}` : wakeHere ? "the message didn't arrive" : notHeld!;
    s5.path = wakeHere ? "zero-install" : "held";
    results.push(this.done(s5));
  }

  // ── Phase b ────────────────────────────────────────────────────────────

  private async bodyB(l: Lane, results: ScenarioResult[]): Promise<void> {
    const d = this.driver;
    await d.prepare(l);
    await this.recordVersion(l);
    await d.start(l);
    l.state.panePid = await l.tmux.panePid(l.session);

    const s6 = this.result("S6", l);
    s6.path = d.installPath ?? undefined;
    await l.snap("started");
    let mark = l.logSize();
    // The user picks yes if the agent asks with a picker (AskUserQuestion):
    // then the agent installs in this turn.
    let tYes = Date.now();
    if (process.env.WB_ASK_WITH_PICKER === "1") l.note("WB_ASK_WITH_PICKER: the first message asks the agent to ask with its question tool");
    await l.say(this.intro, PICK_YES, 300_000);
    const picked = l.state.picked === "yes";
    const added = (await this.ctx.bench.lane(l.id)).calls.find(isWatch)?.added ?? "";
    const shown6 = l.logSince(mark) + "\n" + (await l.screen(400));
    const turn = ownWords(shown6, [withoutSuggestedAsk(added, shown6), this.intro]);
    const offered = picked || OFFER.test(turn) || asksInstall(turn);
    s6.metrics.asked_with_picker = picked;
    await l.snap(picked ? "picked yes in the agent's picker" : offered ? "install offered" : "no install offer");
    if (picked) tYes = (l.state.pickedAt as number) ?? tYes;
    else {
      tYes = Date.now();
      mark = l.logSize();
      await l.say(offered ? SAY_YES : this.ext ? ASK_INSTALL_EXT : ASK_INSTALL, YES, 300_000);
    }
    s6.metrics.offered = offered;
    const { connectedMs } = await this.driveInstall(l, s6, { mark, added, tYes, said: [this.intro], nudge: this.ext ? (this.ext.prompts.nudge ?? null) : WATCH_AGAIN });
    await l.snap(connectedMs !== null ? "connected" : "not connected", { presence: await this.presence(l) });
    // A wake through the install.
    await l.waitIdle(YES, 60_000);
    const after = new Date().toISOString();
    const [e] = await this.ctx.bench.publish(l.id, 1, "S6");
    const t0 = Date.now();
    const sincePublish = l.logSize();
    l.note(`published ${e.code}`);
    const ms = await this.waitWake(l, e.code, after, 120_000, YES);
    // No GOT line: did the code reach the pane anyway (woken, the report stuck)?
    // The note can reach the model without showing in the pane: in a tool
    // result after the install (OpenClaw running its turns in Codex: the
    // core's, on the next call).
    s6.metrics.connected_note ||= (await this.ctx.bench.lane(l.id)).calls.some((c) => c.at >= new Date(tYes).toISOString() && /Wakeshift: connected\./.test(c.added ?? ""));
    const seenUnreported = ms === null && shownRaw(ownWords(l.logSince(sincePublish) + "\n" + (await l.screen(400)), [added]), e.code);
    s6.metrics.seen_unreported = seenUnreported;
    await l.snap(ms !== null ? `woke for ${e.code}` : seenUnreported ? `woken for ${e.code}, never reported` : `no wake for ${e.code}`, { latency_ms: ms === null ? null : Date.now() - t0 });
    apply(
      s6,
      install({
        ...s6.metrics,
        agent_ran_installer: s6.metrics.agent_ran_installer === true,
        trust_relayed: typeof s6.metrics.trust_relayed === "boolean" ? s6.metrics.trust_relayed : null,
        installed: s6.metrics.installed === true,
        connected_ms: connectedMs,
        wake_ms: ms === null ? null : Date.now() - t0,
      }),
    );
    results.push(this.done(s6));

    // Idle wakes through the install (S2's installed half).
    if (this.want.has("S2i")) results.push(await this.s2(l, "S2i", YES));
    if (!d.zeroInstallWake) {
      if (this.want.has("S3")) results.push(await this.s3(l, YES));
      if (this.want.has("S4")) results.push(await this.s4(l, YES));
      if (this.want.has("S7")) results.push(await this.s7(l, YES));
    }
    if (this.want.has("S13") && s13Path() === "installed") results.push(await this.s13(l, YES));
    if (l.mcp2) results.push(await this.s15(l, YES));
    if (this.want.has("S21")) results.push(await this.s21(l, YES));
    if (this.want.has("S20")) results.push(await this.s20(l));
    if (this.want.has("S8")) results.push(await this.s8(l));
    if (this.want.has("S18")) results.push(await this.s18(l, s6.metrics.connected_ms != null));

    // Uninstall, and check it left nothing.
    l.begin();
    await d.uninstall(l);
    if (d.uninstallTested === false) {
      // A container's state goes with it: nothing runs or checks the uninstaller.
      s6.metrics.uninstall = "untested";
      s6.notes.push("uninstall untested: the container is removed with its copied state, so nothing checks what the uninstaller leaves");
    } else {
      const left = d.installed(l).filter((f) => !f.startsWith(".wakeshift/bin"));
      s6.metrics.uninstall = left.length ? "left files" : "clean";
      if (left.length) s6.notes.push(`after uninstall, still mentions Wakeshift: ${left.join(", ")}`);
      this.uninstallInventory(l, s6);
    }
    s6.steps.push(...l.steps);
  }

  /**
   * The install after the user's yes, as S6 drives it (S17 too): a second
   * yes if the agent asks again, the user running the installer where the
   * agent couldn't, the trust step, "ok, I'm back" (or "Done, I ran it.")
   * where nothing connected within BACK_AFTER_MS, and one nudge (o.nudge;
   * none if null) 75s after that. Waits up to 180s for the install to
   * connect; puts its metrics on r.
   */
  private async driveInstall(l: Lane, r: ScenarioResult, o: { mark: number; added: string; tYes: number; said: string[]; nudge: string | null; back?: string }) {
    const d = this.driver;
    // What the user says on coming back: S6n's own words (o.back), else BACK (or WB_S6_BACK's).
    const backMsg = o.back ?? backText();
    // The installer can stop to ask something more (OpenClaw's, on a
    // gateway that listens beyond this computer: may its hooks endpoint be
    // reachable there?), and the agent asks the user. The user agrees once
    // more, and the agent runs it again.
    r.metrics.asked_again = false;
    // Not when the agent asked the user to run it themselves (the note's
    // fallback when its own run is refused): that's below, with runAsUser.
    if (d.installed(l).length === 0 && !d.asksUserToRun?.(await l.screen(400))) {
      const again = lastWords(ownWords(l.logSince(o.mark) + "\n" + (await l.screen(400)), [o.added, ...o.said, SAY_YES]), 30);
      if (asksInstall(again) || FOLLOWUP.test(again) || FOLLOWUP.test(unwrap(again))) {
        r.metrics.asked_again = true;
        await l.snap("the agent asked again");
        await l.say(SAY_YES_AGAIN, YES, 300_000);
      }
    }
    // The agent's closing words (the installer prints the trust step too, so
    // look only at the end of the turn, where the agent relays it).
    const closing = lastWords(await l.screen(400));
    await l.snap("after the install turn", { closing: redactPlain(closing) });
    r.metrics.agent_ran_installer = d.installed(l).length > 0;
    const trustStep = d.trustStepFor?.(o.added) ?? d.trustStep;
    r.metrics.trust_relayed = trustStep ? trustStep.test(closing) : null;
    r.metrics.install_fallback = "none";
    r.metrics.install_block_reason = null;
    r.metrics.harness_version = (l.state.version as string | null) ?? null;
    if (d.id === "claude-code") {
      // Which plugin install the core offered, and whether this Claude Code
      // runs it (before 2.1.292 only the two steps do: WB_CLAUDE_PIN=floor).
      const cmd = d.offeredInstall?.(o.added) ?? null;
      r.metrics.offer_form = offerForm(cmd) || null;
      r.metrics.offer_fits_version = offerFitsVersion(cmd, r.metrics.harness_version as string | null);
    }
    if (!r.metrics.agent_ran_installer) {
      // The harness's own reason where its transcript has one (auto mode's classifier), else the pane's.
      const refused = d.refusals?.(l, o.tYes).find((x) => INSTALLER.test(x.tool));
      const why = refused ? `${refused.tool}: ${refused.reason}` : blockReason(await l.screen(600), o.added);
      r.metrics.install_block_reason = why;
      if (why) l.note("why the agent's installer run didn't happen", { quote: redactPlain(why) });
      // The agent couldn't (Claude Code's auto mode refuses `curl | sh`):
      // the user runs it, the way the agent asked (Claude Code: `! <cmd>`),
      // or else the bench does, as the user would.
      const cmd = d.offeredInstall?.(o.added) ?? userInstallCommand(d, this.ctx, l);
      const typed = d.runAsUser ? await d.runAsUser(l, await l.screen(400), cmd) : null;
      if (typed && d.installed(l).length > 0) {
        l.note(`the agent asked the user to run the installer: ${typed}`);
        await l.snap("the user ran the installer in the harness");
        r.metrics.install_fallback = "user typed it in the harness";
        r.notes.push("the agent's run of the installer was refused; it asked the user to run it, and the bench typed it");
      } else {
        const ran = await installAsTheUser(d, l, cmd);
        l.note(`the agent didn't install: the user ran the installer (${ran.ok ? "ok" : "failed"})`, { out: ran.out.slice(-1500) });
        r.metrics.install_fallback = "the bench ran it";
        r.notes.push(typed ? "the installer typed in the harness didn't install; the bench ran it as the user" : "the agent didn't run the installer; the bench ran it as the user");
      }
    }
    r.metrics.installed = d.installed(l).length > 0;
    if (d.afterInstall) {
      for (const what of await d.afterInstall(l)) l.note(what);
      l.state.panePid = await l.tmux.panePid(l.session);
      await l.snap("after the trust step");
    }
    // The user, back from running the installer or the trust step, says so
    // in their own words (no instructions), as the install note asks where
    // the agent needs a turn; where nothing needs one (Claude Code's agent
    // connects in the same turn), nothing is said unless nothing has
    // happened within BACK_AFTER_MS. The time to connect counts from that
    // message when one was needed, not from the yes: the trust step and the
    // user's reading time aren't Wakeshift's.
    let tMsg: number | null = null;
    if (r.metrics.install_fallback !== "none") tMsg = await this.back(l, YES, RAN_IT);
    // S6n (o.back): the user always says it once back, as real users do,
    // whether or not anything shows as connected yet (a link from before a
    // restart can show as connected while the resumed chat isn't).
    else if (o.back !== undefined) tMsg = await this.back(l, YES, backMsg);
    // Connected: what the install added holds the stream (installConnected).
    let connectedMs: number | null = null;
    let connectedVia: string | null = null;
    let nudged = false;
    const tReady = Date.now();
    const end = tReady + 180_000 * this.ctx.slow;
    let noteSeen = false;
    while (Date.now() < end) {
      const p = await this.presence(l);
      // The note itself, not the watch result's instructions (which name it) quoted back.
      noteSeen ||= /Wakeshift: connected/.test(ownWords(await l.screen(), [o.added]));
      if (installConnected(d, p)) {
        connectedMs = Date.now() - (tMsg ?? o.tYes);
        connectedVia = p.via;
        break;
      }
      if (tMsg === null && Date.now() - tReady > BACK_AFTER_MS * this.ctx.slow) {
        tMsg = await this.back(l, YES, backMsg);
        continue;
      }
      // Still nothing 75s after the user's message: the bench tells the agent (a nudge).
      if (o.nudge && backMsg === BACK && !nudged && tMsg !== null && Date.now() - tMsg > 75_000 * this.ctx.slow) {
        nudged = true;
        await this.nudge(l, o.nudge, YES);
        continue;
      }
      await sleep(1000);
    }
    r.metrics.connected_ms = connectedMs;
    r.metrics.connected_from = tMsg !== null ? "the user's next message" : "the yes";
    r.metrics.connected_via = connectedVia;
    r.metrics.connected_note = noteSeen || /Wakeshift: connected/.test(ownWords(l.logSince(o.mark), [o.added]));
    r.metrics.needed_message = tMsg !== null;
    r.metrics.needed_watch_again = nudged;
    if (o.back === undefined && backText() !== BACK) r.notes.push(`after the trust step the user asked for report_seen only (WB_S6_BACK): "${BACK_REPORT}"`);
    return { tMsg, connectedMs, connectedVia, noteSeen, nudged };
  }

  /**
   * Wakeshift-internal IDs (visible.ts internalIds) each scenario of the
   * lane showed: in the pane (the user), in Wakeshift's text on the app's
   * tool results, and in the wakes as the harness gave them to the model.
   * The IDs the bench knows: the lane's subscriptions, and the deliveries a
   * Codex connector logged. Counted per scenario (internal_ids_shown, a
   * rule in grade.ts), the kinds and where in the notes.
   */
  private async idsShown(l: Lane) {
    const st = await this.ctx.bench.lane(l.id).catch(() => null);
    const known: { id: string; kind: string }[] = [];
    for (const c of st?.calls ?? []) if (c.subscription) known.push({ id: c.subscription, kind: "subscription ID" });
    for (const s of st?.subscriptions ?? []) known.push({ id: s, kind: "subscription ID" });
    const logs = [join(l.ctx.dir, "logs", `${l.id}.connector.log`)];
    const connDir = join(l.home.dir, ".wakeshift/codex/logs");
    if (existsSync(connDir)) for (const f of readdirSync(connDir)) logs.push(join(connDir, f));
    for (const f of logs) {
      try {
        for (const id of loggedDeliveries(readFileSync(f, "utf8"))) known.push({ id, kind: "delivery ID" });
      } catch {
        // None.
      }
    }
    const received = this.driver.received ? await this.driver.received(l).catch(() => null) : null;
    for (const seg of this.segments.filter((s) => s.lane === l)) {
      const w = this.windows.get(seg.r);
      if (!w) continue;
      const within = (at: string | number | null) => at !== null && w.spans.some((s) => { const t = typeof at === "number" ? at : Date.parse(at); return t >= s.from && t <= (s.to ?? Date.now()); });
      const where: string[] = [];
      const add = (place: string, kinds: string[]) => {
        for (const k of kinds) where.push(`${k} in ${place}`);
      };
      add("the pane", internalIds(l.logRange(seg.from, seg.to ?? l.logSize()), known));
      add("Wakeshift's text on a tool result", internalIds((st?.calls ?? []).filter((c) => within(c.at)).map((c) => c.added ?? "").join("\n"), known));
      add("what reached the model", internalIds((received ?? []).filter((x) => within(x.at)).map((x) => x.text).join("\n"), known));
      const prev = typeof seg.r.metrics.internal_ids_shown === "number" ? seg.r.metrics.internal_ids_shown : 0;
      seg.r.metrics.internal_ids_shown = prev + where.length;
      for (const x of where) seg.r.notes.push(`a Wakeshift-internal ID: ${x}`);
    }
  }

  private async finish(l: Lane) {
    // A scan that couldn't run says so on every cell of the lane, never reads as clean.
    await this.idsShown(l).catch((err) => {
      const why = `internal IDs not checked: ${err instanceof Error ? err.message : String(err)}`;
      for (const s of this.segments.filter((x) => x.lane === l)) {
        s.r.metrics.internal_ids_checked = false;
        if (!s.r.notes.includes(why)) s.r.notes.push(why);
      }
    });
    // The container's own session records go with it: count its model's tokens first.
    if (l.state.container) {
      const t = await tokensUsed(l).catch(() => null);
      if (t) this.tokens = { input: (this.tokens?.input ?? 0) + t.input, output: (this.tokens?.output ?? 0) + t.output, cached: (this.tokens?.cached ?? 0) + t.cached };
    }
    try {
      await this.driver.stop(l);
    } catch {
      // Killed below.
    }
    if (this.driver.cleanup) await this.driver.cleanup(l).catch(() => {});
    checkToolServers(this.driver, l, this.segments.filter((s) => s.lane === l).map((s) => s.r));
    await l.tmux.kill(l.session);
    // Anything still running from this lane (a detached connector, a daemon).
    const left = this.driver.leftovers ? this.driver.leftovers(l) : this.driver.waiters(l, ps());
    for (const r of left) this.ctx.procs.kill(r.pid, `left over in ${l.id}`);
    if (left.length) l.note(`killed ${left.length} process(es) left over`, { commands: left.map((r) => redactPlain(r.command).slice(0, 160)) });
  }

  // ── S2–S4, S7, S8 ──────────────────────────────────────────────────────

  /**
   * Waits for each event's GOT line (no nudges), and says where the time
   * went: the core's delivery stages (local), the code first in the pane,
   * report_seen, the GOT line (metrics.ts breakdownOf). Returns ms from publish per
   * code (null: never) and the breakdowns.
   */
  private async follow(l: Lane, evs: Published[], policy: AnswerPolicy, timeoutMs: number) {
    const pane = new Map<string, number>();
    const got = new Map<string, number>();
    const since = l.logSize();
    const t0 = Math.min(...evs.map((e) => Date.parse(e.at)));
    // With a transcript (Claude Code): the turns each event started. A
    // turn that ended counts as done for an event whose GOT line can't come
    // (a transport that cut the event's text: WB_CLAUDE_TRANSPORT).
    const turnsFor = () => (this.driver.turns ? matchTurns(this.driver.turns(l, t0 - 1000), evs) : null);
    let endedAt: number | null = null;
    // A harness that doesn't paint the install's turns (unpaintedWakes): the
    // agent's report_seen for a code (not in a nudge) is its arrival.
    const unpainted = this.driver.unpaintedWakes?.(l) ?? false;
    const after = new Date(t0 - 1000).toISOString();
    let lastPoll = 0;
    const reportedAt = new Map<string, number>();
    await l.waitFor(null, timeoutMs, policy, {
      since,
      tick: unpainted
        ? async () => {
            if (Date.now() - lastPoll < 2000) return;
            lastPoll = Date.now();
            const st = await this.ctx.bench.lane(l.id).catch(() => null);
            if (!st) return;
            const counted = this.reports(st, after, evs.map((e) => e.code)).counts;
            for (const e of evs) {
              const rep = st.seen.find((s) => s.at >= after && s.ids.includes(e.code));
              if (counted.has(e.code) && rep && !got.has(e.code)) {
                got.set(e.code, Date.parse(rep.at));
                reportedAt.set(e.code, Date.parse(rep.at));
              }
            }
          }
        : undefined,
      watch: (screen, printed) => {
        const now = Date.now();
        for (const e of evs) {
          if (!pane.has(e.code) && (shownRaw(screen, e.code) || shownRaw(printed, e.code))) pane.set(e.code, now);
          if (!got.has(e.code) && (this.got(e.code).test(screen) || this.got(e.code).test(printed))) {
            got.set(e.code, now);
            if (!pane.has(e.code)) pane.set(e.code, now);
          }
        }
        if (got.size === evs.length) return true;
        if (process.env.WB_CLAUDE_TRANSPORT && this.driver.turns) {
          const m = turnsFor();
          if (m && evs.every((e) => m.get(e.code)?.ended != null)) {
            endedAt ??= now;
            return now - endedAt > 3000;
          }
        }
        return false;
      },
    });
    if (reportedAt.size)
      l.note(`arrived by report_seen, not shown in the pane (${this.driver.name} doesn't paint the install's turns): ${[...reportedAt.keys()].join(", ")}`);
    const turns = turnsFor();
    // One read of each delivery's stages once the wait is over: nothing
    // extends the scenario (a confirmation still to come shows as null).
    const done = await Promise.all(evs.map((e) => (this.ctx.bench.trace ? this.ctx.bench.trace(e.id) : Promise.resolve(null))));
    const st = await this.ctx.bench.lane(l.id);
    const breakdown = evs.map((e, i) => {
      const rep = st.seen.find((s) => s.ids.includes(e.code));
      const t = turns?.get(e.code);
      return breakdownOf(e.code, e.at, {
        accepted_at: e.accepted_at,
        trace: done[i],
        ...(turns ? { turn: t?.started ?? null, first: t?.first ?? null } : {}),
        pane: pane.get(e.code),
        reported_at: rep?.at,
        got: got.get(e.code),
      });
    });
    const at = new Map<string, number | null>(evs.map((e) => [e.code, got.has(e.code) ? got.get(e.code)! - Date.parse(e.at) : null]));
    return { at, breakdown, turns };
  }

  /** Idle wakes: S2 with nothing installed (phase a), S2i through the install (phase b, after S6). */
  private async s2(l: Lane, id: "S2" | "S2i" = "S2", policy: AnswerPolicy = NO): Promise<ScenarioResult> {
    const r = this.result(id, l);
    r.path = id === "S2" ? "zero-install" : (this.driver.installPath ?? undefined);
    const lat: number[] = [];
    const breakdown: Breakdown[] = [];
    let wakes = 0;
    const codes: string[] = [];
    const seen = new Set<string>();
    const turns: Record<string, string | number | null>[] = [];
    // WB_S2_POSTS: more idle wakes, for latency studies (default 3).
    const posts = Math.max(3, Number(process.env.WB_S2_POSTS ?? 3) || 3);
    // WB_S2_IDLE_SECONDS: how long the conversation sits idle before each post
    // (default 4), e.g. past Codex 0.166's one-minute unload of idle threads.
    const idle = Math.max(4, Number(process.env.WB_S2_IDLE_SECONDS ?? 4) || 4);
    if (idle > 4) l.note(`idle ${idle}s before each post (WB_S2_IDLE_SECONDS)`);
    for (let i = 0; i < posts; i++) {
      await l.waitIdle(policy, 120_000);
      if (this.ext) {
        // An external app's jobs end once: each post is a new job's end, so the user asks for one first.
        const before = (await this.ctx.bench.lane(l.id)).jobs ?? 0;
        await l.say(this.ext.prompts.next.replaceAll("{n}", String(i + 2)), policy, 300_000);
        await l.waitIdle(policy, 120_000);
        const now = (await this.ctx.bench.lane(l.id)).jobs ?? 0;
        l.note(`asked for job ${i + 2}: ${now - before} job(s) started`);
      }
      await sleep(idle * 1000);
      const after = new Date().toISOString();
      const [e] = await this.ctx.bench.publish(l.id, 1, `S2 #${i + 1}`);
      l.note(`published ${e.code}`);
      const f = await this.follow(l, [e], policy, 90_000);
      const ms = f.at.get(e.code) ?? null;
      if (ms !== null) lat.push(ms);
      breakdown.push(...f.breakdown);
      const t = f.turns?.get(e.code);
      if (t) turns.push({ code: e.code, queued_ms: t.queued - Date.parse(e.at), started_ms: t.started === null ? null : t.started - Date.parse(e.at), first_ms: t.first === null ? null : t.first - Date.parse(e.at), ended_ms: t.ended === null ? null : t.ended - Date.parse(e.at), first_kind: t.firstKind, output_tokens: t.outputTokens, input_tokens: t.inputTokens, model: t.model, source: t.source });
      await l.snap(ms !== null ? `woke for ${e.code}` : `no wake for ${e.code}`, { latency_ms: ms, breakdown: f.breakdown[0], ...(t ? { turn: turns[turns.length - 1] } : {}) });
      await l.waitIdle(policy, 90_000);
      const rep = this.reports(await this.ctx.bench.lane(l.id), after, [e.code]);
      // An external app has no report_seen: its wakes are its GOT lines.
      wakes += this.ext ? (ms !== null ? 1 : 0) : rep.wakes;
      codes.push(e.code);
      if (ms !== null || rep.counts.has(e.code)) seen.add(e.code);
    }
    r.breakdown = breakdown;
    apply(r, idleWake(lat, posts, wakes));
    r.metrics = { ...r.metrics, ...stageMedians(breakdown), ...ourPart(breakdown) };
    await this.countArrivals(r, l, codes, seen);
    if (turns.length) {
      // From the transcript: wake turns (a GOT line or not), their tokens.
      r.metrics.turns = turns.length;
      r.metrics.p50_queued_ms = median(turns.map((t) => t.queued_ms as number));
      r.metrics.p50_output_tokens = median(turns.map((t) => t.output_tokens as number));
      r.metrics.p50_input_tokens = median(turns.map((t) => t.input_tokens as number));
      r.metrics.turn_detail = JSON.stringify(turns);
    }
    return this.done(r);
  }

  private async s3(l: Lane, policy: AnswerPolicy): Promise<ScenarioResult> {
    const r = this.result("S3", l);
    r.path = l.phase === "a" ? "zero-install" : (this.driver.installPath ?? undefined);
    await l.waitIdle(policy, 120_000);
    const after = new Date().toISOString();
    const evs = await this.ctx.bench.publish(l.id, 5, "S3 burst");
    l.note(`published ${evs.map((e) => e.code).join(", ")}`);
    const codes = evs.map((e) => e.code);
    const nudged = 0;
    const { at, breakdown } = await this.follow(l, evs, policy, 150_000);
    r.breakdown = breakdown;
    await l.waitIdle(policy, 90_000);
    const st = await this.ctx.bench.lane(l.id);
    const rep = this.reports(st, after, codes);
    const seenPane = [...at.values()].filter((v) => v !== null).length;
    const seen = new Set([...[...at.entries()].filter(([, v]) => v !== null).map(([c]) => c), ...rep.counts.keys()]);
    // A code reported twice that the count saw reach the model once was reported again, not duplicated.
    const arrived = await this.arrivals(l, codes, seen);
    const twice = [...rep.counts.entries()].filter(([, n]) => n > 1).map(([c]) => c);
    const again = twice.filter((c) => arrived.once.has(c));
    const dup = twice.length - again.length;
    await l.snap(`burst: ${seen.size}/5`, { reported: Object.fromEntries(rep.counts), deliveries: this.deliveryRows(st, codes) });
    const last = Math.max(...[...at.values()].map((v) => v ?? 0));
    apply(r, burst(5, seen.size, rep.wakes, dup, { seen_in_pane: seenPane, acked_unseen: this.ackedUnseen(st, codes, seen), last_ms: last || null, nudged, reported_again: again.length, ...stageMedians(breakdown) }));
    if (again.length) r.notes.push(`reported again, though it reached the model once (not a duplicate): ${again.join(", ")}`);
    this.applyArrivals(r, arrived);
    return this.done(r);
  }

  private async s4(l: Lane, policy: AnswerPolicy): Promise<ScenarioResult> {
    if (this.driver.killable === false) return Scenarios.na("S4", this.driver.id, "no process on this machine holds the stream (it's inside the container)");
    const r = this.result("S4", l);
    r.path = l.phase === "a" ? "zero-install" : (this.driver.installPath ?? undefined);
    await l.waitIdle(policy, 120_000);
    await sleep(3000);
    const before = await this.presence(l);
    const ws = this.driver.waiters(l, ps());
    if (!ws.length) {
      await l.snap("no waiter process found");
      r.works = false;
      r.reason = "no process holding the stream could be found to kill";
      r.metrics = { connected_before: before.connected, lost: null };
      return this.done(r);
    }
    for (const w of ws) this.ctx.procs.kill(w.pid, `S4 waiter in ${l.id}`, "SIGKILL");
    const tKill = Date.now();
    await l.snap(`killed ${ws.length} waiter process(es)`, { killed: ws.map((w) => ({ pid: w.pid, command: redactPlain(w.command).slice(0, 200) })), presence_before: before });
    let posted: { code: string } | null = null;
    let tPost = 0;
    let since = 0;
    const w = new KillWatch(tKill);
    const end = tKill + 150_000 * this.ctx.slow;
    // A waiter that came back by itself (opencode's plugin restarts its
    // bridge; a hook starts a new waiter): presence rightly stays up.
    const killed = new Set(ws.map((x) => x.pid));
    let restartedMs: number | null = null;
    // Presence, the post 30s after the kill, and its arrival meanwhile.
    while (Date.now() < end) {
      if (restartedMs === null && this.driver.waiters(l, ps()).some((x) => !killed.has(x.pid))) {
        restartedMs = Date.now() - tKill;
        l.note(`a new waiter was running ${(restartedMs / 1000).toFixed(1)}s after the kill`);
      }
      if (w.due(Date.now())) {
        since = l.logSize();
        [posted] = await this.ctx.bench.publish(l.id, 1, "S4 after the kill");
        tPost = Date.now();
        w.post(tPost);
        l.note(`published ${posted.code} (30s after the kill)`);
      }
      const p = await this.presence(l);
      const wasCleared = w.clearedMs !== null;
      w.presence(Date.now(), p.connected);
      if (!wasCleared && w.clearedMs !== null) l.note(`presence cleared after ${(w.clearedMs / 1000).toFixed(1)}s`);
      const screen = await l.screen();
      if (posted && (gotRe(posted.code).test(screen) || gotRe(posted.code).test(l.logSince(since)))) w.arrived(Date.now());
      if (w.done(Date.now())) break;
      // Answer dialogs (a re-armed Monitor needs approval) meanwhile.
      const a = await this.driver.answer(l, screen.split("\n").slice(-200).join("\n"), policy);
      if (a) {
        l.answered.push({ at: new Date().toISOString(), what: a });
        await l.snap(`answered: ${a}`);
      }
      await sleep(1000);
    }
    if (!posted) {
      since = l.logSize();
      [posted] = await this.ctx.bench.publish(l.id, 1, "S4 after the kill");
      tPost = Date.now();
      w.post(tPost);
    }
    const cleared = w.clearedMs;
    let ms = w.arrivedMs;
    let nudged = 0;
    if (ms === null) {
      const got = await this.waitGot(l, [posted.code], tPost, 120_000, policy, 1);
      ms = got.at.get(posted.code) ?? null;
      nudged = got.nudged;
    }
    const st = await this.ctx.bench.lane(l.id);
    await l.snap(ms !== null ? `${posted.code} arrived` : `${posted.code} lost`, { deliveries: this.deliveryRows(st, [posted.code]), presence: await this.presence(l) });
    // Presence is right if it cleared in time, or if something listened again
    // by itself: a new waiter, or the post arriving without a nudge (what
    // delivered it was alive, so "connected" was true). Stale presence (no
    // waiter, still shown connected) misses the bar.
    const restarted = restartedMs !== null || (ms !== null && nudged === 0);
    r.metrics = {
      connected_before: before.connected,
      killed: ws.length,
      presence_clear_ms: cleared,
      restarted_ms: restartedMs ?? (ms !== null && nudged === 0 ? tPost - tKill + ms : null),
      presence_ok: (cleared !== null && cleared <= 90_000) || (cleared === null && restarted),
      lost: ms === null,
      redelivered_ms: ms,
      nudges: nudged,
    };
    r.works = ms !== null;
    if (!r.works) r.reason = "the post made while the waiter was dead never arrived";
    if (!before.connected) r.notes.push("presence said not connected before the kill");
    return this.done(r);
  }

  private async s7(l: Lane, policy: AnswerPolicy): Promise<ScenarioResult> {
    const r = this.result("S7", l);
    r.path = l.phase === "a" ? "zero-install" : (this.driver.installPath ?? undefined);
    await l.waitIdle(policy, 120_000);
    await l.say(REARM, policy);
    await l.snap("after unwatch + watch");
    await sleep(3000);
    const [e] = await this.ctx.bench.publish(l.id, 1, "S7 after re-arm");
    const t0 = Date.now();
    l.note(`published ${e.code}`);
    // Its GOT line, or a report_seen naming it (as S6 and S15 count it): a
    // woken turn that reports the code but prints no GOT line was woken.
    const since = l.logSize();
    let ms = await this.waitWake(l, e.code, new Date(t0 - 1000).toISOString(), 75_000, policy);
    let nudged = 0;
    if (ms !== null) {
      // The report comes a moment before its GOT line: time S7 to the line, as before, when it shows.
      const got = await l.waitFor(gotRe(e.code), 10_000, policy, { since });
      if (got !== null) ms = Date.now() - t0;
      else r.notes.push(`${e.code} was reported (report_seen) without a GOT line`);
    } else {
      // Waited out already: straight to the nudges, 75s after each.
      const g = await this.waitGot(l, [e.code], t0, 75_000, policy, 2, 1);
      ms = g.at.get(e.code) ?? null;
      nudged = g.nudged;
    }
    await l.snap(ms !== null ? `${e.code} arrived` : `${e.code} didn't arrive`);
    apply(r, rearm(ms, nudged));
    // The re-armed watch's own turn ends before the count: a second copy has had time to come.
    await l.waitIdle(policy, 60_000);
    await this.countArrivals(r, l, [e.code], new Set(ms !== null ? [e.code] : []));
    return this.done(r);
  }

  /**
   * S13: the conversation sits idle for WB_S13_MINUTES (default 36), past
   * Claude Code's 30-minute Monitor limit, the bench answering only dialogs
   * (as a user who approves what's asked, or with WB_APPROVE_SCOPE=session,
   * "Yes" each time). One post just after the 30-minute mark and one at the
   * end must arrive without a nudge; the agent mustn't ask the user whether
   * to keep watching. Presence is sampled meanwhile (the longest stretch
   * nothing listened), and from the transcript, how often the agent started
   * its Monitor and whether one expired at Claude Code's limit.
   */
  private async s13(l: Lane, policy: AnswerPolicy): Promise<ScenarioResult> {
    const r = this.result("S13", l);
    r.path = l.phase === "a" ? "zero-install" : (this.driver.installPath ?? undefined);
    await l.waitIdle(policy, 120_000);
    const minutes = Math.max(32, Number(process.env.WB_S13_MINUTES ?? 36) || 36);
    const t0 = Date.now();
    const since = l.logSize();
    const end = t0 + minutes * 60_000 * this.ctx.slow;
    const firstAt = t0 + 31 * 60_000 * this.ctx.slow;
    l.note(`idle for ${minutes} minutes: posts at 31 minutes and at the end`);
    const posts: { e: Published; t: number; at: number | null }[] = [];
    let connectedSamples = 0;
    let samples = 0;
    let gapFrom: number | null = null;
    let maxGap = 0;
    let lastSnap = t0;
    while (Date.now() < end) {
      // Dialogs are answered meanwhile (a re-armed Monitor in manual mode),
      // and a post's GOT line is timed as it shows (on screen, or in the
      // pane's log, which can lag behind it).
      await l.waitFor(null, 15_000, policy, {
        since,
        watch: (screen, printed) => {
          for (const x of posts) if (x.at === null && (gotRe(x.e.code).test(screen) || gotRe(x.e.code).test(printed))) x.at = Date.now() - x.t;
          return false;
        },
      });
      const now = Date.now();
      const p = await this.presence(l);
      samples++;
      if (p.connected) {
        connectedSamples++;
        if (gapFrom !== null) maxGap = Math.max(maxGap, now - gapFrom);
        gapFrom = null;
      } else gapFrom ??= now;
      if (!posts.length && now >= firstAt) {
        const [e] = await this.ctx.bench.publish(l.id, 1, "S13 after 30 minutes");
        posts.push({ e, t: Date.now(), at: null });
        l.note(`published ${e.code} (${Math.round((Date.now() - t0) / 60_000)} minutes idle)`);
      }
      if (now - lastSnap >= 5 * 60_000) {
        lastSnap = now;
        await l.snap(`${Math.round((now - t0) / 60_000)} minutes idle${p.connected ? "" : ": not connected"}`);
      }
    }
    if (gapFrom !== null) maxGap = Math.max(maxGap, Date.now() - gapFrom);
    // The loop's last round can end past the window before the first post went out: it still goes (after 31 minutes).
    if (!posts.length) {
      const [e] = await this.ctx.bench.publish(l.id, 1, "S13 after 30 minutes");
      posts.push({ e, t: Date.now(), at: null });
      l.note(`published ${e.code} (${Math.round((Date.now() - t0) / 60_000)} minutes idle)`);
      await sleep(5000);
    }
    const [e2] = await this.ctx.bench.publish(l.id, 1, "S13 at the end");
    posts.push({ e: e2, t: Date.now(), at: null });
    l.note(`published ${e2.code} (${Math.round((Date.now() - t0) / 60_000)} minutes idle)`);
    // ms from each post's publish to its GOT line (waitGot counts from when it's called).
    const collect = async (timeoutMs: number) => {
      const from = Date.now();
      const left = posts.filter((x) => x.at === null);
      const g = await this.waitGot(l, left.map((x) => x.e.code), from, timeoutMs, policy, 0);
      for (const x of left) {
        const ms = g.at.get(x.e.code);
        if (ms != null) x.at = from - x.t + ms;
      }
    };
    await collect(120_000);
    const byItself = posts.filter((x) => x.at !== null).length;
    // Still missing: one nudge (counted, so the cell misses its bar), to tell lost from late.
    let nudged = 0;
    if (byItself < posts.length) {
      await l.snap("no GOT yet: nudging");
      await this.nudge(l, NUDGE_EVENTS, policy, 180_000);
      nudged = 1;
      await collect(60_000);
    }
    await l.waitIdle(policy, 90_000);
    const words = rearmWords(ownWords(l.logSince(since), [INTRO, NUDGE_EVENTS]));
    const mon = this.driver.monitorLog ? this.driver.monitorLog(l, t0) : null;
    await l.snap(`after ${minutes} minutes: ${posts.filter((x) => x.at !== null).length}/${posts.length}`, { codes: posts.map((x) => ({ code: x.e.code, ms: x.at })), monitor: mon });
    return this.done(
      apply(
        r,
        longIdle({
          idleMs: Date.now() - t0,
          arrivals: posts.map((x) => x.at),
          unprompted: byItself,
          nudges: nudged,
          askedUser: words.asked,
          toldStopped: words.stopped,
          restarts: mon ? mon.starts : null,
          expired: mon ? mon.expired : null,
          maxGapMs: samples ? maxGap : null,
          connectedShare: samples ? Math.round((connectedSamples / samples) * 100) / 100 : null,
        }),
      ),
    );
  }

  /** S8's first half: woken once, then the harness stopped. */
  private async s8Stop(l: Lane) {
    const r = this.result("S8", l);
    r.path = this.driver.installPath ?? undefined;
    await l.waitIdle(YES, 120_000);
    const after = new Date().toISOString();
    const [a] = await this.ctx.bench.publish(l.id, 1, "S8 before the restart");
    l.note(`published ${a.code}`);
    const got = await l.waitFor(gotRe(a.code), 90_000, YES);
    await l.waitIdle(YES, 90_000);
    await l.snap(got !== null ? "before restart: delivered" : "before restart: not delivered");
    await this.driver.stop(l);
    await l.snap("harness stopped");
    return { r, after, a, got };
  }

  private async s8(l: Lane): Promise<ScenarioResult> {
    if (s8Rewatch()) return this.s8WithRewatch(l);
    const { r, after, a, got } = await this.s8Stop(l);
    const [b] = await this.ctx.bench.publish(l.id, 1, "S8 while stopped");
    l.note(`published ${b.code} while the harness was down`);
    await sleep(10_000);
    // Reported before the harness started again: it wasn't down yet when it
    // took that one (Codex's app-server runs the thread a few seconds past
    // the TUI's exit), so it says nothing about reconnecting.
    const tStart = new Date().toISOString();
    const bEarly = this.reportedBetween(await this.ctx.bench.lane(l.id), after, tStart, b.code);
    if (bEarly) l.note(`${b.code} was delivered before the harness was down (it didn't test the restart)`);
    await this.driver.start(l, { resume: true });
    l.state.panePid = await l.tmux.panePid(l.session);
    if (this.driver.afterInstall && l.state.trustAgainOnRestart) for (const w of await this.driver.afterInstall(l)) l.note(w);
    await l.snap("harness restarted");
    // The user says nothing at first: a harness whose hooks or plugin
    // reconnect at session start delivers what came while it was down, and
    // what comes after, by itself. Then they say they're back (no
    // instructions), as the install note asks where it needs a message;
    // only then does the bench nudge. A woken turn counts by its report
    // after the restart began (a resumed TUI shows old GOT lines again).
    const t1 = Date.now();
    const at = new Map<string, number | null>([[b.code, bEarly ? 0 : null]]);
    let nudged = 0;
    let backSaid = false;
    const unprompted = async (code: string, ms: number) => {
      const end = Date.now() + ms * this.ctx.slow;
      while (Date.now() < end) {
        if (this.reportedBetween(await this.ctx.bench.lane(l.id), tStart, null, code)) return true;
        await l.waitFor(null, 2000, YES, { watch: () => false });
      }
      return false;
    };
    const bByItself = bEarly ? null : await unprompted(b.code, 45_000);
    if (bByItself) at.set(b.code, Date.now() - t1);
    else if (!bEarly) {
      await l.snap(`no ${b.code} without a message: saying "${BACK}"`);
      await this.back(l, YES);
      backSaid = true;
      const g = await this.waitGot(l, [b.code], t1, 90_000, YES, 1);
      at.set(b.code, g.at.get(b.code) ?? null);
      nudged = g.nudged;
    }
    await l.waitIdle(YES, 90_000);
    const [c] = await this.ctx.bench.publish(l.id, 1, "S8 after the restart");
    l.note(`published ${c.code}`);
    const tc = Date.now();
    let cAt: number | null = null;
    let cByItself = false;
    if (!backSaid && (cByItself = await unprompted(c.code, 60_000))) cAt = Date.now() - tc;
    else {
      if (!backSaid) {
        await l.snap(`no ${c.code} without a message: saying "${BACK}"`);
        await this.back(l, YES);
      }
      const cw = await this.waitGot(l, [c.code], tc, 120_000, YES, 1);
      cAt = cw.at.get(c.code) ?? null;
      nudged += cw.nudged;
    }
    await l.waitIdle(YES, 90_000);
    const st = await this.ctx.bench.lane(l.id);
    const codes = [a.code, b.code, c.code];
    const rep = this.reports(st, after, codes);
    const seen = new Set<string>([...rep.counts.keys()]);
    if (got !== null) seen.add(a.code);
    if (at.get(b.code) !== null) seen.add(b.code);
    if (cAt !== null) seen.add(c.code);
    const dups = [...rep.counts.values()].filter((n) => n > 1).length;
    const rows = this.deliveryRows(st, codes);
    await l.snap(`after restart: ${seen.size}/3`, { reported: Object.fromEntries(rep.counts), deliveries: rows });
    return this.done(
      apply(
        r,
        restart(3, seen.size, dups, {
          acked_unseen: this.ackedUnseen(st, codes, seen),
          while_down_ms: at.get(b.code) ?? null,
          // Woken after the restart without the user saying anything (session-start reconnect): what came
          // while it was down (unless it was taken before the harness was down) and what came after.
          reconnected_by_itself: (bEarly || bByItself === true) && cByItself,
          delivered_before_down: bEarly,
          after_restart_ms: cAt,
          nudges: nudged,
          redelivery_attempts_max: rows.length ? Math.max(...rows.map((x) => x.attempts)) : null,
        }),
      ),
    );
  }

  /**
   * S8 with a re-watch (WB_S8_REWATCH): woken before the restart; nothing
   * published while it's down, so the re-watch's turn has nothing to
   * report and ends right after the call, as a resumed conversation's
   * often does (a report keeps the turn going until the re-watch's redeem
   * is done, which once hid a miss); after the restart the user
   * asks for watch again at once; then idle (WB_S8_IDLE), and the last
   * event must wake the session by itself.
   */
  private async s8WithRewatch(l: Lane): Promise<ScenarioResult> {
    const { r, after, a, got } = await this.s8Stop(l);
    await sleep(10_000);
    const tStart = new Date().toISOString();
    await this.driver.start(l, { resume: true });
    l.state.panePid = await l.tmux.panePid(l.session);
    if (this.driver.afterInstall && l.state.trustAgainOnRestart) for (const w of await this.driver.afterInstall(l)) l.note(w);
    await l.snap("harness restarted");
    await l.snap(`saying "${rewatchText()}"`);
    await this.back(l, YES, rewatchText());
    await l.waitIdle(YES, 90_000);
    const idle = s8IdleMs();
    if (idle) {
      l.note(`idle ${idle / 1000}s before the last publish (WB_S8_IDLE)`);
      await l.waitFor(null, idle * this.ctx.slow, YES, { watch: () => false });
    }
    const [c] = await this.ctx.bench.publish(l.id, 1, "S8 after the restart");
    l.note(`published ${c.code}`);
    const tc = Date.now();
    // Woken by itself: nothing more is said for a minute.
    let woke = false;
    for (const end = Date.now() + 60_000 * this.ctx.slow; !woke && Date.now() < end; ) {
      woke = this.reportedBetween(await this.ctx.bench.lane(l.id), tStart, null, c.code);
      if (!woke) await l.waitFor(null, 2000, YES, { watch: () => false });
    }
    let cAt: number | null = woke ? Date.now() - tc : null;
    let nudged = 0;
    if (!woke) {
      const cw = await this.waitGot(l, [c.code], tc, 120_000, YES, 1);
      cAt = cw.at.get(c.code) ?? null;
      nudged = cw.nudged;
    }
    await l.waitIdle(YES, 90_000);
    const st = await this.ctx.bench.lane(l.id);
    const codes = [a.code, c.code];
    const rep = this.reports(st, after, codes);
    const seen = new Set<string>([...rep.counts.keys()]);
    if (got !== null) seen.add(a.code);
    if (cAt !== null) seen.add(c.code);
    const dups = [...rep.counts.values()].filter((n) => n > 1).length;
    const rows = this.deliveryRows(st, codes);
    await l.snap(`after restart: ${seen.size}/2`, { reported: Object.fromEntries(rep.counts), deliveries: rows });
    const out = restart(2, seen.size, dups, {
      acked_unseen: this.ackedUnseen(st, codes, seen),
      after_restart_ms: cAt,
      nudges: nudged,
      redelivery_attempts_max: rows.length ? Math.max(...rows.map((x) => x.attempts)) : null,
      woken_after_rewatch: woke,
      idle_before_last_s: idle / 1000,
    });
    r.notes.push(`after the restart the user asked for watch again (WB_S8_REWATCH: "${rewatchText()}"); the last event had to wake it by itself`);
    if (!woke && out.works) {
      out.works = false;
      out.reason = "not woken by itself after the re-watch";
    }
    return this.done(apply(r, out));
  }

  /**
   * After an uninstall: everything of Wakeshift's left in the temp HOME
   * (~/.wakeshift, paths named for it, settings and hooks files that
   * mention it), against what the public uninstall docs say stays
   * (visible.ts uninstallLeftovers). An undocumented leftover is a
   * finding: uninstall_undocumented, and the notes list each.
   */
  private uninstallInventory(l: Lane, r: ScenarioResult) {
    const { documented, undocumented } = uninstallLeftovers(l.home.dir, this.driver.id);
    r.metrics.uninstall_undocumented = undocumented.length;
    r.metrics.uninstall_documented = documented.length;
    if (undocumented.length) r.notes.push(`after uninstall, left and not in the uninstall docs: ${undocumented.map((p) => `~/${p}`).join(", ")}`);
    if (documented.length) r.notes.push(`after uninstall, left as the docs say: ${documented.map((d) => `~/${d.path} (${d.why})`).join("; ")}`);
    l.note("what's left of Wakeshift's after the uninstall", { undocumented, documented: documented.map((d) => d.path) });
  }

  /**
   * S21: several wakes in a row on one long-lived conversation, then across
   * a restart. WB_S21_POSTS posts (default 5), each after the conversation
   * sat idle WB_S21_IDLE_SECONDS (default 20), must each wake it with
   * nothing said. Then the harness quits and starts again continuing the
   * conversation, and 3 more posts must each wake it; if the first doesn't
   * arrive by itself within 45s the user says "ok, I'm back" once (counted:
   * back_after_restart), as a harness that reconnects on the next message
   * needs. A wake is the code's GOT line or a report_seen naming it. Works:
   * every post woke it, so "wakes once, then never" fails.
   */
  private async s21(l: Lane, policy: AnswerPolicy): Promise<ScenarioResult> {
    const r = this.result("S21", l);
    r.path = l.phase === "a" ? "zero-install" : (this.driver.installPath ?? undefined);
    const posts = Math.max(3, Number(process.env.WB_S21_POSTS ?? 5) || 5);
    const idle = Math.max(4, Number(process.env.WB_S21_IDLE_SECONDS ?? 20) || 20);
    const codes: string[] = [];
    const seen = new Set<string>();
    const one = async (tag: string, waitMs: number) => {
      await l.waitIdle(policy, 120_000);
      await sleep(idle * 1000);
      const after = new Date().toISOString();
      const [e] = await this.ctx.bench.publish(l.id, 1, tag);
      codes.push(e.code);
      l.note(`published ${e.code} (${tag})`);
      const ms = await this.waitWake(l, e.code, after, waitMs, policy);
      if (ms !== null) seen.add(e.code);
      await l.snap(ms !== null ? `woke for ${e.code}` : `no wake for ${e.code}`, { latency_ms: ms });
      return { code: e.code, after, ms };
    };
    const before: (number | null)[] = [];
    for (let i = 0; i < posts; i++) before.push((await one(`S21 #${i + 1}`, 90_000)).ms);
    await l.waitIdle(policy, 90_000);
    await this.driver.stop(l);
    await l.snap("harness stopped");
    await this.driver.start(l, { resume: true });
    l.state.panePid = await l.tmux.panePid(l.session);
    if (this.driver.afterInstall && l.state.trustAgainOnRestart) for (const w of await this.driver.afterInstall(l)) l.note(w);
    await l.snap("harness restarted");
    const after: (number | null)[] = [];
    let backSaid = false;
    for (let i = 0; i < 3; i++) {
      const x = await one(`S21 after the restart #${i + 1}`, i === 0 ? 45_000 : 90_000);
      let ms = x.ms;
      if (ms === null && !backSaid) {
        await l.snap(`no ${x.code} after the restart without a message: saying "${BACK}"`);
        await this.back(l, policy);
        backSaid = true;
        ms = await this.waitWake(l, x.code, x.after, 90_000, policy);
        if (ms !== null) seen.add(x.code);
      }
      after.push(ms);
    }
    const woke = (xs: (number | null)[]) => xs.filter((x) => x !== null).length;
    const firstMiss = [...before, ...after].findIndex((x) => x === null);
    const lat = [...before, ...after].filter((x): x is number => x !== null);
    r.metrics = {
      ...r.metrics,
      posts: posts + 3,
      woke_in_a_row: woke(before),
      of_in_a_row: posts,
      woke_after_restart: woke(after),
      of_after_restart: 3,
      first_missed: firstMiss < 0 ? null : firstMiss < posts ? `#${firstMiss + 1}` : `after the restart #${firstMiss - posts + 1}`,
      back_after_restart: backSaid,
      latencies_ms: lat,
      p50_ms: median(lat),
      max_ms: lat.length ? Math.max(...lat) : null,
    };
    r.works = firstMiss < 0;
    if (!r.works) r.reason = woke(before) < posts ? `woke for ${woke(before)} of ${posts} posts in a row (first missed: ${r.metrics.first_missed})` : `woke for ${woke(after)} of 3 posts after the restart`;
    await this.countArrivals(r, l, codes, seen);
    return this.done(r);
  }

  /** The harnesses whose off switch the public docs give (site/docs/agents.md, "Uninstall"), and which of them the variable reaches. */
  static readonly OFF_SWITCH: Record<string, { variable: boolean }> = {
    "claude-code": { variable: true },
    // Codex's threads, and their hooks, run in its background app-server: the docs say the variable doesn't reach them.
    codex: { variable: false },
    cursor: { variable: true },
    copilot: { variable: true },
    antigravity: { variable: true },
  };

  /**
   * S20: the off switch, set the way the docs tell users (site/docs/agents.md,
   * "Uninstall"), on a connected conversation. (1) `~/.wakeshift/bin/wakeshift
   * disable` in another terminal while everything runs (Codex's background
   * daemon included): a post must not wake the conversation within
   * 45s; then `wakeshift enable`, and the user's next message gets what
   * waited. (2) Where the docs promise the variable (not Codex), `export
   * WAKESHIFT_DISABLED=1` in the shell profile (~/.bashrc), and the harness
   * started again from a shell that read it, continuing the conversation:
   * a post must not wake it within 45s; then the line goes, the harness
   * starts again from a shell without it, and the user's next message gets
   * what waited. Works: nothing woke while off, and every post arrived once
   * it was back on.
   */
  private async s20(l: Lane): Promise<ScenarioResult> {
    const sw = Scenarios.OFF_SWITCH[this.driver.id];
    if (!sw) return Scenarios.na("S20", this.driver.id, `the docs give no off switch for ${this.driver.name}`);
    const r = this.result("S20", l);
    r.path = this.driver.installPath ?? undefined;
    const d = this.driver;
    const env = d.envFor ? d.envFor(l) : (process.env as Record<string, string>);
    const bin = join(l.home.dir, ".wakeshift/bin/wakeshift");
    const OFF_MS = 45_000;
    const why: string[] = [];
    const codes: string[] = [];
    const seen = new Set<string>();
    // A post while off: it must not wake; once on again, the user's next message gets it.
    const whileOff = async (how: string, on: () => Promise<void>) => {
      await l.waitIdle(YES, 120_000);
      await sleep(5000);
      const after = new Date().toISOString();
      const [e] = await this.ctx.bench.publish(l.id, 1, `S20 while off (${how})`);
      codes.push(e.code);
      l.note(`published ${e.code} with the switch on (${how})`);
      const woke = await this.waitWake(l, e.code, after, OFF_MS, YES);
      await l.snap(woke !== null ? `woke for ${e.code} while off (${how})` : `no wake for ${e.code} while off (${how})`);
      await on();
      await l.waitIdle(YES, 60_000);
      await this.back(l, YES);
      const ms = woke !== null ? woke : await this.waitWake(l, e.code, after, 90_000, YES);
      if (ms !== null) seen.add(e.code);
      await l.snap(ms !== null ? `${e.code} arrived once back on` : `${e.code} never arrived once back on`);
      return { woke: woke !== null, arrived: ms !== null };
    };
    if (!existsSync(bin)) {
      r.works = false;
      r.reason = "~/.wakeshift/bin/wakeshift isn't there, so the documented `wakeshift disable` can't run";
      return this.done(r);
    }
    // If the scenario throws midway, the lane must not stay off for the scenarios after it.
    const rc = join(l.home.dir, ".bashrc");
    let disabled = false;
    let profileSet = false;
    try {
      // (1) The switch file, by the documented command, while everything runs.
      disabled = true;
      const off = await l.run(env, bin, ["disable"]);
      l.note(`wakeshift disable: ${off.ok ? "ok" : "failed"}`, { out: off.out.slice(-500) });
      r.metrics.disable_ran = off.ok;
      const file = await whileOff("wakeshift disable", async () => {
        const on = await l.run(env, bin, ["enable"]);
        l.note(`wakeshift enable: ${on.ok ? "ok" : "failed"}`, { out: on.out.slice(-500) });
        r.metrics.enable_ran = on.ok;
        disabled = !on.ok;
      });
      r.metrics.woke_while_disabled = file.woke;
      r.metrics.arrived_after_enable = file.arrived;
      if (!off.ok) why.push("`wakeshift disable` failed");
      if (r.metrics.enable_ran === false) why.push("`wakeshift enable` failed");
      if (file.woke) why.push("a post woke the conversation after `wakeshift disable`");
      if (!file.arrived) why.push("the post made while disabled never arrived after `wakeshift enable`");
      // (2) The variable, from the shell profile, where the docs promise it.
      if (sw.variable) {
        await l.waitIdle(YES, 90_000);
        await d.stop(l);
        profileSet = true;
        writeFileSync(rc, "export WAKESHIFT_DISABLED=1\n", { mode: 0o600 });
        // A new terminal reads the profile (the pane's bash is --norc, so it reads it here).
        await l.tmux.type(l.session, ". ~/.bashrc");
        await sleep(500);
        await d.start(l, { resume: true });
        l.state.panePid = await l.tmux.panePid(l.session);
        await l.snap("restarted with WAKESHIFT_DISABLED=1 from ~/.bashrc");
        const variable = await whileOff("WAKESHIFT_DISABLED=1 in ~/.bashrc", async () => {
          await l.waitIdle(YES, 60_000);
          await d.stop(l);
          writeFileSync(rc, "", { mode: 0o600 });
          profileSet = false;
          await l.tmux.type(l.session, "unset WAKESHIFT_DISABLED");
          await sleep(500);
          await d.start(l, { resume: true });
          l.state.panePid = await l.tmux.panePid(l.session);
          await l.snap("restarted without WAKESHIFT_DISABLED");
        });
        r.metrics.woke_with_variable = variable.woke;
        r.metrics.arrived_after_variable = variable.arrived;
        if (variable.woke) why.push("a post woke the conversation with WAKESHIFT_DISABLED=1 set in the shell profile");
        if (!variable.arrived) why.push("the post made with WAKESHIFT_DISABLED=1 never arrived once it was unset");
      } else {
        r.metrics.woke_with_variable = null;
        r.notes.push(`the variable isn't tested for ${d.name}: the docs say it doesn't reach its hooks, and to use \`wakeshift disable\``);
      }
    } finally {
      if (profileSet) writeFileSync(rc, "", { mode: 0o600 });
      if (disabled) await l.run(env, bin, ["enable"]).catch(() => undefined);
    }
    r.works = why.length === 0;
    if (why.length) r.reason = why.join("; ");
    await this.countArrivals(r, l, codes, seen);
    return this.done(r);
  }

  /** Spans in which the bench asked the agent to call watch without its session_key (S15's new session): S9 doesn't hold those calls against it. */
  private unkeyed: { from: string; to: string }[] = [];

  /**
   * S15: two apps, one conversation. The lane has the second app's MCP
   * server (wakebench2) next to the first. In one conversation: (a) watch
   * app 1 again, a post to app 1; (b) watch app 2 too, a post to each; (c)
   * watch app 2 again, with a post to app 1 made while that turn runs; (d)
   * watch app 1 again leaving session_key out (a new session), with a post
   * to app 1 made during that turn; (e) a post to each. Each code must be
   * seen (its GOT line, or a report_seen call on either server) within 90s
   * of its step; whatever is still missing at the end gets one nudge
   * (counted), then counts as lost. Per app: codes published and seen,
   * lost, duplicates (twoApps, appSide), wake latency.
   */
  private async s15(l: Lane, policy: AnswerPolicy): Promise<ScenarioResult> {
    const app2 = this.ctx.bench.app2;
    if (!app2 || !l.mcp2) return Scenarios.na("S15", this.driver.id, "no second app in this run");
    const r = this.result("S15", l);
    r.path = l.phase === "a" ? "zero-install" : (this.driver.installPath ?? undefined);
    const apps: Record<1 | 2, Pick<ToyApp, "publish" | "lane">> = { 1: this.ctx.bench, 2: app2 };
    const from = new Date().toISOString();
    const since = l.logSize();
    const pubs: { app: 1 | 2; code: string; at: number; step: string }[] = [];
    const got: Record<string, number> = {};
    const reported = new Set<string>();
    let lastPoll = 0;
    const publish = async (app: 1 | 2, step: string): Promise<string> => {
      const [e] = await apps[app].publish(l.id, 1, `S15 ${step}`);
      pubs.push({ app, code: e.code, at: Date.parse(e.at), step });
      l.note(`published ${e.code} to app ${app} (${step})`);
      return e.code;
    };
    const pubAt = (code: string) => pubs.find((p) => p.code === code)?.at ?? 0;
    // report_seen calls on either app's server since S15 began.
    const poll = async (force = false) => {
      if (!force && Date.now() - lastPoll < 2000) return;
      lastPoll = Date.now();
      for (const a of [1, 2] as const) {
        const st = await apps[a].lane(l.id).catch(() => null);
        for (const s of st?.seen ?? []) if (s.at >= from) for (const id of s.ids) reported.add(id);
      }
    };
    // Waits until each code is seen (its GOT line, or reported on either server), answering dialogs; returns those still missing.
    const collect = async (codes: string[], timeoutMs: number): Promise<string[]> => {
      const missing = () => codes.filter((c) => got[c] === undefined && !reported.has(c));
      await l.waitFor(null, timeoutMs, policy, {
        since,
        watch: (screen, printed) => {
          for (const c of codes) if (got[c] === undefined && (gotRe(c).test(screen) || gotRe(c).test(printed))) got[c] = Date.now();
          return missing().length === 0;
        },
        tick: () => poll(),
      });
      await poll(true);
      return missing();
    };
    const step = async (label: string, codes: string[]) => {
      const missing = await collect(codes, 90_000);
      await l.waitIdle(policy, 90_000);
      await l.snap(`${label}: ${codes.length - missing.length}/${codes.length} seen`, { codes, missing });
    };
    const watchSince = async (app: 1 | 2, at: string) => (await apps[app].lane(l.id)).calls.filter((c) => c.tool === "watch" && c.at >= at);

    await l.waitIdle(policy, 120_000);
    // (a) Watch app 1 (again, in phase b: S6 watched it), a post to app 1.
    await l.say(S15_WATCH_1, policy);
    await sleep(3000);
    await step("a", [await publish(1, "a: app 1")]);

    // (b) Watch app 2 too, a post to each.
    const tB = new Date().toISOString();
    await l.say(S15_WATCH_2, policy, 300_000);
    const watchedSecond = (await watchSince(2, tB)).length > 0;
    l.note(watchedSecond ? `the agent watched ${l.mcp2.name}` : `no watch call on ${l.mcp2.name}`);
    await l.waitIdle(policy, 60_000);
    await sleep(3000);
    await step("b", [await publish(1, "b: app 1"), await publish(2, "b: app 2")]);

    // (c) Watch app 2 again; a post to app 1 while that turn runs.
    const tC = new Date().toISOString();
    let c1 = "";
    const busyC = await l.sayDuring(S15_REWATCH_2, policy, async () => {
      c1 = await publish(1, "c: app 1, mid-turn");
    });
    const wC = (await watchSince(2, tC))[0];
    const midC = busyC || (wC ? Date.parse(wC.at) >= pubAt(c1) : false);
    if (!wC) l.note(`no ${l.mcp2.name} watch call in (c)`);
    await step("c", [c1]);

    // (d) Watch app 1 again without session_key (a new session); a post to app 1 during that turn.
    const tD = new Date().toISOString();
    const priorSession = (await this.ctx.bench.lane(l.id)).calls.filter((c) => c.tool === "watch" && c.at < tD).pop()?.session ?? null;
    let d1 = "";
    const busyD = await l.sayDuring(S15_NEW_SESSION_1, policy, async () => {
      d1 = await publish(1, "d: app 1, during the new session's turn");
    });
    this.unkeyed.push({ from: tD, to: new Date().toISOString() });
    const wD = (await watchSince(1, tD))[0];
    const midD = busyD || (wD ? Date.parse(wD.at) >= pubAt(d1) : false);
    const newSession = wD ? wD.args.session_key === null && !!wD.session && wD.session !== priorSession : null;
    l.note(wD ? `watch without session_key: ${wD.args.session_key === null ? "left out" : "passed anyway"}; ${newSession ? "a new session" : "the same session"}` : "no wakebench watch call in (d)");
    await step("d", [d1]);

    // (e) A post to each.
    await l.waitIdle(policy, 60_000);
    await sleep(3000);
    await step("e", [await publish(1, "e: app 1"), await publish(2, "e: app 2")]);

    // Anything still missing: one nudge (counted), to tell lost from late.
    let missing = pubs.map((p) => p.code).filter((c) => got[c] === undefined && !reported.has(c));
    let nudgedAt: number | null = null;
    if (missing.length) {
      await l.snap(`missing ${missing.join(", ")}: nudging`);
      nudgedAt = Date.now();
      await this.nudge(l, NUDGE_EVENTS_2, policy, 180_000);
      missing = await collect(missing, 60_000);
    }
    await l.waitIdle(policy, 90_000);

    const st1 = await this.ctx.bench.lane(l.id);
    const st2 = await app2.lane(l.id);
    const reports = [...st1.seen.filter((s) => s.at >= from).map((s) => ({ ...s, server: 1 as const })), ...st2.seen.filter((s) => s.at >= from).map((s) => ({ ...s, server: 2 as const }))];
    const calls = [...st1.calls, ...st2.calls].filter((c) => c.at >= from);
    const inNudge = (at: string) => [...this.nudges, ...this.backs].some((n) => at >= n.from && at <= n.to);
    const side = (app: 1 | 2) =>
      appSide(
        app,
        pubs.filter((p) => p.app === app).map((p) => ({ code: p.code, at: p.at })),
        reports,
        got,
        calls,
        inNudge,
        nudgedAt,
      );
    const s1 = side(1);
    const s2 = side(2);
    const codes1 = s1.published.map((p) => p.code);
    const codes2 = s2.published.map((p) => p.code);
    await l.snap(`two apps: ${Object.keys(s1.firstSeen).length}/${codes1.length} + ${Object.keys(s2.firstSeen).length}/${codes2.length}`, {
      published: pubs.map((p) => ({ app: p.app, code: p.code, step: p.step })),
      reported: reports.map((x) => ({ at: x.at, server: x.server, ids: x.ids })),
      deliveries_app1: this.deliveryRows(st1, codes1),
      deliveries_app2: this.deliveryRows(st2, codes2),
    });
    // Counted first: a code reported twice that reached the model once was reported again, not duplicated.
    const arrived = await this.arrivals(l, [...codes1, ...codes2], new Set([...Object.keys(s1.firstSeen), ...Object.keys(s2.firstSeen)]));
    apply(r, twoApps(s1, s2, { nudges: nudgedAt === null ? 0 : 1, newSession, midTurn: { c: midC, d: midD }, watchedSecond, deliveredOnce: arrived.once }));
    r.metrics.app1_acked_unseen = this.ackedUnseen(st1, codes1, new Set(Object.keys(s1.firstSeen)));
    r.metrics.app2_acked_unseen = this.ackedUnseen(st2, codes2, new Set(Object.keys(s2.firstSeen)));
    this.applyArrivals(r, arrived);
    if (!watchedSecond) r.notes.push(`the agent never called ${l.mcp2.name}'s watch tool in (b)`);
    return this.done(r);
  }

  // ── S6n: the install with no nudge, its own lane ──────────────────────

  /**
   * S6n: S6 as a real user does it. The same first message and yes, the
   * same trust step, but after it the user only says "done" (DONE; or,
   * with WB_S6N_BACK=read, asks something unrelated that the app's read
   * tool answers: DONE_READ) once back from it, connected or not,
   * and never tells the agent to call watch again. S6's nudge once hid misses
   * (opencode and pi; Hermes's CLI and TUI), though toy-app agents
   * that pass session_key or call watch again by themselves still pass
   * on the code before those fixes. Works when the
   * install is in and a post then wakes the conversation; connected,
   * watch_again (the agent called watch again by itself) and read_calls
   * are recorded. A lane of its own (a fresh HOME and conversation), so
   * S6 stays comparable with earlier runs.
   */
  async phaseS6n(): Promise<void> {
    const d = this.driver;
    if (d.installPath === null) {
      this.results.push(Scenarios.na("S6n", d.id, "nothing to install"));
      return;
    }
    const back = s6nBack();
    let l: Lane | null;
    if (back === "read") l = await this.ownLane("S6n", "b", "q", { tools: ["job_status"] });
    else {
      l = new Lane(this.ctx, d, "b", { id: `${d.id}-q` });
      l.state.apart = true;
    }
    if (!l) return;
    await this.phase(l, async (l, results) => {
      const r = this.result("S6n", l);
      r.path = d.installPath ?? undefined;
      r.metrics.back = back;
      await d.prepare(l);
      await this.recordVersion(l);
      await d.start(l);
      l.state.panePid = await l.tmux.panePid(l.session);
      await l.snap("started");
      let mark = l.logSize();
      let tYes = Date.now();
      await l.say(this.intro, PICK_YES, 300_000);
      const picked = l.state.picked === "yes";
      const added = (await this.ctx.bench.lane(l.id)).calls.find(isWatch)?.added ?? "";
      const turn = ownWords(l.logSince(mark) + "\n" + (await l.screen(400)), [added, this.intro]);
      const offered = picked || OFFER.test(turn) || asksInstall(turn);
      r.metrics.offered = offered;
      r.metrics.asked_with_picker = picked;
      await l.snap(picked ? "picked yes in the agent's picker" : offered ? "install offered" : "no install offer");
      if (picked) tYes = (l.state.pickedAt as number) ?? tYes;
      else {
        tYes = Date.now();
        mark = l.logSize();
        await l.say(offered ? SAY_YES : ASK_INSTALL, YES, 300_000);
      }
      const said = back === "read" ? DONE_READ : DONE;
      const { connectedMs, tMsg } = await this.driveInstall(l, r, { mark, added, tYes, said: [this.intro], nudge: null, back: said });
      await l.snap(connectedMs !== null ? "connected" : "not connected", { presence: await this.presence(l) });
      // A wake through the install, with nothing more said.
      await l.waitIdle(YES, 60_000);
      const after = new Date().toISOString();
      const [e] = await this.ctx.bench.publish(l.id, 1, "S6n");
      const t0 = Date.now();
      l.note(`published ${e.code}`);
      const ms = await this.waitWake(l, e.code, after, 120_000, YES);
      await l.snap(ms !== null ? `woke for ${e.code}` : `no wake for ${e.code}`, { latency_ms: ms === null ? null : Date.now() - t0 });
      const st = await this.ctx.bench.lane(l.id);
      const yesAt = new Date(tYes).toISOString();
      apply(
        r,
        noNudge({
          ...r.metrics,
          installed: r.metrics.installed === true,
          connected_ms: connectedMs,
          wake_ms: ms === null ? null : Date.now() - t0,
          said_done: tMsg !== null,
          watch_again: st.calls.filter((c) => isWatch(c) && c.at >= yesAt).length,
          // Of those, after the user's "done": the agent reconnecting by itself
          // (the shape of a miss where only another watch connected the chat).
          watch_after_done: tMsg === null ? 0 : st.calls.filter((c) => isWatch(c) && c.at >= new Date(tMsg).toISOString() && c.at < after).length,
          // Tools that subscribe to nothing, called between the yes and the post (report_seen, job_status).
          read_calls: st.calls.filter((c) => !isWatch(c) && c.at >= yesAt && c.at < after).length,
        }),
      );
      if (tMsg !== null) r.notes.push(`after the trust step the user said only "${said}" (S6n: no nudge)`);
      results.push(this.done(r));
      // Tidy: the install goes with the HOME anyway; S6 checks the uninstall.
      await d.uninstall(l).catch(() => {});
    });
  }

  // ── S16, S17: their own lanes ─────────────────────────────────────────

  /**
   * A lane of its own for S16 or S17 (a fresh HOME and conversation), with
   * the toy app's options set before the harness connects. Null (and an n/a
   * result) where the toy app has no options (the fake target, a port, the
   * Mastra route: ts-v2 only).
   */
  private async ownLane(id: "S6n" | "S16" | "S17" | "S17d", phase: "a" | "b", suffix: string, opts: { unrecognized?: boolean; tools: ("job_status" | "start_job")[] }): Promise<Lane | null> {
    const b = this.ctx.bench;
    const na = (why: string) => {
      this.results.push(Scenarios.na(id, this.driver.id, why));
      return null;
    };
    if (!b.options) return na("the toy app here has no job tools (the ts-v2 toy app on a real core has them)");
    const laneId = `${this.driver.id}-${suffix}`;
    try {
      await b.options(laneId, opts);
    } catch (err) {
      return na(`the toy app didn't take the lane's options (only the ts-v2 toy app has the job tools): ${err instanceof Error ? err.message : String(err)}`);
    }
    const l = new Lane(this.ctx, this.driver, phase, { id: laneId });
    l.state.apart = true;
    return l;
  }

  /** The lane's calls since its start, as S16 and S17 count them. */
  private async jobState(l: Lane) {
    const st = await this.ctx.bench.lane(l.id);
    // An external app's tools count by what they do: starting a job (start_job) or checking one (job_status).
    const tool = (c: LaneState["calls"][number]) => (c.role === "start" ? "start_job" : c.role === "status" ? "job_status" : c.tool);
    return { st, calls: st.calls.map((c) => ({ at: c.at, tool: tool(c), events_in_result: c.events_in_result })) };
  }

  /** What Wakeshift added to each result of the lane, in the notes (S16, S17). */
  private addedNotes(r: ScenarioResult, st: LaneState) {
    const n = new Map<string, number>();
    for (const c of st.calls) {
      const i = (n.get(c.tool) ?? 0) + 1;
      n.set(c.tool, i);
      r.notes.push(`Wakeshift's text on ${c.tool} #${i}${c.agent ? ` (recognized as ${c.agent.harness ?? "nothing"})` : ""}: ${c.added ? c.added.slice(0, 1200) : "(none)"}`);
    }
  }

  /**
   * After the job finished (published at t0, log offset mark): told without
   * asking within waitMs (the pane), else the user asks ANY_NEWS once.
   * Returns whether, and the agent's words in each.
   */
  private async hearJob(l: Lane, r: ScenarioResult, o: { code: string; mark: number; before: string; theirs: string[]; waitMs: number; policy: AnswerPolicy }) {
    const seenBefore = new Set(o.before.split("\n").map((x) => x.replace(/\s+/g, " ").trim()).filter((x) => x.length >= 12));
    // Wakeshift's texts on calls made meanwhile (a held message in a status result) are theirs too.
    const theirs = [...o.theirs];
    // An external app's event text, however the harness shows it (a line of JSON, a quote): not the agent's words.
    const snips: string[] = [];
    let lastPoll = 0;
    const refresh = async () => {
      if (Date.now() - lastPoll < 2000) return;
      lastPoll = Date.now();
      const st = await this.ctx.bench.lane(l.id).catch(() => null);
      for (const c of st?.calls ?? []) if (c.added && !theirs.includes(c.added)) theirs.push(c.added);
      for (const t of (await this.ctx.bench.eventTexts?.([o.code]).catch(() => [])) ?? []) {
        const snip = t.replace(/\s+/g, " ").trim().slice(0, 40);
        if (snip && !snips.includes(snip)) snips.push(snip);
      }
    };
    const notEvent = (x: string) => !snips.some((p) => x.replace(/\s+/g, " ").includes(p));
    // An external app's check is by sentence against what was on screen before
    // (toldResult's before): dropping old lines first would join what's left
    // into sentences that look new.
    const fresh = (text: string) =>
      jobWords(text, theirs)
        .split("\n")
        .filter((x) => (this.ext || !seenBefore.has(x.replace(/\s+/g, " ").trim())) && notEvent(x))
        .join("\n");
    const t0 = Date.now();
    let said: string | null = null;
    const unasked = await l.waitFor(null, o.waitMs, o.policy, {
      since: o.mark,
      tick: refresh,
      // An external app reads the rendered screen only: the raw log keeps half-redrawn fragments ("Send ay messag"), which no earlier text matches.
      watch: (screen, printed) => (said = toldSentence(fresh(this.ext ? screen : printed + "\n" + screen), this.ext ? null : o.code, this.ext ? DONE_WORDS : undefined, this.ext ? o.before : undefined)) !== null,
    });
    if (said && this.ext) l.note("told: the agent's words", { said: redactPlain(said).slice(0, 400) });
    r.metrics.told_ms = unasked === null ? null : Date.now() - t0;
    let toldWhenAsked: boolean | null = null;
    if (unasked === null) {
      await l.snap(`not told within ${Math.round(o.waitMs / 1000)}s: the user asks`);
      await l.waitIdle(o.policy, 60_000);
      const m2 = l.logSize();
      const before2 = l.logSince(Math.max(0, m2 - 400_000)) + "\n" + (await l.screen(2000));
      await l.say(ANY_NEWS, o.policy, 240_000);
      const seen2 = new Set(before2.split("\n").map((x) => x.replace(/\s+/g, " ").trim()).filter((x) => x.length >= 12));
      lastPoll = 0;
      await refresh();
      const reply = jobWords((this.ext ? "" : l.logSince(m2) + "\n") + lastWords(await l.screen(400), 30), [...theirs, ANY_NEWS])
        .split("\n")
        .filter((x) => (this.ext || !seen2.has(x.replace(/\s+/g, " ").trim())) && notEvent(x))
        .join("\n");
      toldWhenAsked = toldResult(reply, this.ext ? null : o.code, this.ext ? DONE_WORDS : undefined, this.ext ? before2 : undefined);
      await l.snap(toldWhenAsked ? "told when asked" : "not told, even when asked", { reply: redactPlain(lastWords(reply, 12)) });
    } else {
      await l.snap("told without asking");
      await l.waitIdle(o.policy, 90_000);
    }
    return { toldUnasked: unasked !== null, toldWhenAsked };
  }

  /**
   * S16: an agent Wakeshift doesn't recognize (the lane's requests carry an
   * unknown client name and User-Agent, no harness _meta keys and no
   * `harness` claim: app/server.ts disguise) is asked to watch a running
   * bench job and say when it finishes. The job finishes while the
   * conversation is idle; after WB_S16_WAIT_SECONDS (default 75) without
   * the news, the user asks "Any news?". It works when the user hears the
   * result, from the app's status tool (job_status) or any tool that
   * doesn't subscribe, and watch is never called again after its first call.
   */
  async phaseS16(): Promise<void> {
    const l = await this.ownLane("S16", "a", "u", { unrecognized: true, tools: ["job_status"] });
    if (!l) return;
    await this.phase(l, async (l, results) => {
      const d = this.driver;
      const r = this.result("S16", l);
      r.path = "unrecognized";
      await d.prepare(l);
      await this.recordVersion(l);
      await d.start(l);
      l.state.panePid = await l.tmux.panePid(l.session);
      await l.snap("started");
      const mark = l.logSize();
      await l.say(S16_ASK, NO, 300_000);
      await l.waitIdle(NO, 60_000);
      const first = await this.jobState(l);
      const added = first.st.calls.map((c) => c.added ?? "").filter(Boolean);
      const own = jobWords(l.logSince(mark) + "\n" + (await l.screen(400)), [...added, S16_ASK]);
      // An install offer (there shouldn't be one for an agent Wakeshift doesn't know): the user says no.
      if (l.state.picked === "no") l.note("answered no in the agent's picker");
      else if (asksInstall(own)) {
        await l.say(SAY_NO, NO);
        r.metrics.offered_install = true;
      }
      const promised = PROMISE.test(unwrap(own));
      const watch = first.st.calls.find((c) => c.tool === "watch");
      r.metrics.recognized_as = watch?.agent?.harness ?? null;
      r.metrics.client = first.st.calls[0]?.client ?? null;
      r.metrics.disguised = first.st.calls.length > 0 && first.st.calls.every((c) => c.client === "acme-agent");
      await l.snap(watch ? `watched (recognized as ${watch.agent?.harness ?? "nothing"})` : "no watch call");
      // The job finishes while the conversation is idle.
      await l.waitIdle(NO, 60_000);
      await sleep(5000);
      r.metrics.polled_instead = d.busy(await l.screen(200));
      const before = l.logSince(Math.max(0, l.logSize() - 400_000)) + "\n" + (await l.screen(2000));
      const pubMark = l.logSize();
      const [e] = await this.ctx.bench.publish(l.id, 1, JOB_RESULT);
      l.note(`the job finished: published ${e.code}`);
      const heard = await this.hearJob(l, r, { code: e.code, mark: pubMark, before, theirs: [...added, S16_ASK, benchEventText(e.code, JOB_RESULT), jobStatusText({ code: e.code, tag: JOB_RESULT, at: e.at }, 0)], waitMs: s16WaitMs(), policy: NO });
      const end = await this.jobState(l);
      const claimed = end.st.calls.map((c) => c.args.harness).filter((x): x is string => !!x);
      apply(r, statusFallback({ calls: end.calls, code: e.code, publishedAt: e.at, toldUnasked: heard.toldUnasked, toldWhenAsked: heard.toldWhenAsked, promised }));
      r.metrics.recognized_as = end.st.calls.find((c) => c.tool === "watch")?.agent?.harness ?? r.metrics.recognized_as;
      r.metrics.claimed_harness = claimed.length ? [...new Set(claimed)].join(", ") : null;
      r.metrics.added_watch = end.st.calls.find((c) => c.tool === "watch")?.added?.slice(0, 1000) ?? null;
      if (r.metrics.recognized_as) r.notes.push(`Wakeshift recognized the disguised lane as ${r.metrics.recognized_as}: the scenario didn't test an unrecognized harness`);
      this.addedNotes(r, end.st);
      results.push(this.done(r));
    });
  }

  /**
   * S17 (phase b: install yes) and S17d (phase a: install no): start_job
   * both starts a job (its side effect, counted by the app) and subscribes
   * the conversation. The user asks once to start a job and hear when it's
   * done; S17 then installs as S6 does (picker or "yes", the trust step,
   * "ok, I'm back"; never a nudge to call the tool again), S17d says no.
   * After WB_S17_IDLE_SECONDS idle the job finishes; woken, or else the
   * user asks "Any news?". It works when the job was started exactly once.
   */
  async phaseS17(phase: "a" | "b"): Promise<void> {
    const id = phase === "b" ? "S17" : "S17d";
    if (phase === "b" && this.driver.installPath === null) {
      this.results.push(Scenarios.na(id, this.driver.id, "nothing to install (S17d runs it declined)"));
      return;
    }
    const l = await this.ownLane(id, phase, phase === "b" ? "j" : "n", { tools: ["start_job", "job_status"] });
    if (!l) return;
    await this.phase(l, async (l, results) => {
      const d = this.driver;
      const r = this.result(id, l);
      r.path = phase === "b" ? (d.installPath ?? undefined) : d.zeroInstallWake ? "zero-install" : "held";
      await d.prepare(l);
      await this.recordVersion(l);
      await d.start(l);
      l.state.panePid = await l.tmux.panePid(l.session);
      await l.snap("started");
      let mark = l.logSize();
      let tYes = Date.now();
      const ask = this.ext?.prompts.natural ?? S17_ASK;
      await l.say(ask, phase === "b" ? PICK_YES : NO, 300_000);
      const added = (await this.ctx.bench.lane(l.id)).calls.find((c) => isStart(c) && !!c.added)?.added ?? (await this.ctx.bench.lane(l.id)).calls.find(isWatch)?.added ?? "";
      const shown17 = l.logSince(mark) + "\n" + (await l.screen(400));
      const own = ownWords(shown17, [withoutSuggestedAsk(added, shown17), ask]);
      const picked = l.state.picked === "yes" || l.state.picked === "no";
      const offered = picked || OFFER.test(own) || asksInstall(own);
      r.metrics.offered = offered;
      r.metrics.asked_with_picker = picked;
      r.metrics.said_it_would_notify = PROMISE.test(unwrap(jobWords(own, [added, ask])));
      await l.snap(picked ? `picked ${l.state.picked} in the agent's picker` : offered ? "install offered" : "no install offer");
      let policy: AnswerPolicy = NO;
      if (phase === "b") {
        policy = YES;
        r.metrics.asked_for_install = false;
        if (l.state.picked === "yes") tYes = (l.state.pickedAt as number) ?? tYes;
        else {
          tYes = Date.now();
          mark = l.logSize();
          r.metrics.asked_for_install = !offered;
          await l.say(offered ? SAY_YES : S17_SETUP, YES, 300_000);
        }
        const { connectedMs } = await this.driveInstall(l, r, { mark, added, tYes, said: [ask, S17_SETUP], nudge: null });
        r.metrics.connected = connectedMs !== null;
        await l.snap(connectedMs !== null ? "connected" : "not connected", { presence: await this.presence(l) });
      } else {
        if (l.state.picked === "no") l.note("answered no in the agent's picker");
        else if (offered) {
          await l.say(SAY_NO, NO);
          await l.snap("after saying no");
        }
        r.metrics.installed_nothing = d.installed(l).length === 0;
      }
      // The job finishes while the conversation is idle.
      await l.waitIdle(policy, 90_000);
      await sleep(s17IdleMs());
      r.metrics.polled_instead = d.busy(await l.screen(200));
      const before = l.logSince(Math.max(0, l.logSize() - 400_000)) + "\n" + (await l.screen(2000));
      const pubMark = l.logSize();
      const [e] = await this.ctx.bench.publish(l.id, 1, JOB_RESULT);
      l.note(`the job finished: published ${e.code}`);
      const all = (await this.ctx.bench.lane(l.id)).calls.map((c) => c.added ?? "").filter(Boolean);
      const heard = await this.hearJob(l, r, { code: e.code, mark: pubMark, before, theirs: [...all, ask, S17_SETUP, SAY_YES, SAY_NO, BACK, RAN_IT, benchEventText(e.code, JOB_RESULT), jobStatusText({ code: e.code, tag: JOB_RESULT, at: e.at }, 0)], waitMs: 120_000, policy });
      const end = await this.jobState(l);
      const jobs = end.st.jobs ?? end.calls.filter((c) => c.tool === "start_job").length;
      apply(r, sideEffectOnce({ sideEffects: jobs, calls: end.calls, toldUnasked: heard.toldUnasked, toldWhenAsked: heard.toldWhenAsked }));
      r.metrics.recognized_as = end.st.calls.find(isStart)?.agent?.harness ?? null;
      if (this.ext) this.externalNotes(r, end.st, e.at);
      if (phase === "a") r.metrics.installed_nothing = r.metrics.installed_nothing === true && d.installed(l).length === 0;
      this.addedNotes(r, end.st);
      results.push(this.done(r));
      if (phase === "b") {
        // Tidy: the install goes with the HOME anyway; nothing is checked here (S6 checks the uninstall).
        await d.uninstall(l).catch(() => {});
      }
    });
  }

  /**
   * S18, already installed: after S6 (and S8) the install is in the temp
   * HOME and connected from that conversation. The harness quits, a new
   * conversation starts in the same HOME, and the user asks it to watch
   * (S18_ASK: nothing about installing or asking). It passes when the agent
   * asks the user nothing about installing or enabling Wakeshift (no
   * picker, no offer relayed, no question: \`questions\`), no permission
   * prompt for Wakeshift's own commands appears beyond what the harness
   * itself requires (the app's MCP tool: \`wakeshift_prompts\`), and a later
   * post wakes it (works). What the agent asked, if anything, is in the notes.
   */
  private async s18(l: Lane, connected: boolean): Promise<ScenarioResult> {
    const d = this.driver;
    if (!s18Applies(d)) return Scenarios.na("S18", d.id, s18NotApplies(d));
    if (!connected) return Scenarios.na("S18", d.id, "the install didn't connect in S6, so there's nothing installed and connected to start from");
    const r = this.result("S18", l);
    r.path = d.installPath ?? undefined;
    await l.waitIdle(YES, 120_000);
    await d.stop(l);
    await l.snap("harness stopped");
    // A new conversation in the same HOME (not a resume).
    await d.start(l);
    l.state.panePid = await l.tmux.panePid(l.session);
    if (d.afterInstall && l.state.trustAgainOnRestart) for (const w of await d.afterInstall(l)) l.note(w);
    await l.snap("new conversation");
    delete l.state.picked;
    const prompts = l.prompts.length;
    const mark = l.logSize();
    // What's on screen already (an earlier conversation's lines, where the pane keeps them) isn't this turn's.
    const norm = (x: string) => x.replace(/\s+/g, " ").trim();
    const shown = new Set((await l.screen(400)).split("\n").map(norm).filter((x) => x.length >= 12));
    const t0 = new Date().toISOString();
    // A user who won't install anything here: any question is answered no.
    const ask = this.ext?.prompts.s18 ?? S18_ASK;
    await l.say(ask, NO, 300_000);
    await l.waitIdle(NO, 90_000);
    // A new conversation: its first watch has no session_key to pass, so
    // S9 doesn't hold that against it.
    this.unkeyed.push({ from: t0, to: new Date().toISOString() });
    const st = await this.ctx.bench.lane(l.id);
    const calls = st.calls.filter((c) => c.at >= t0);
    const watch = calls.find(isWatch);
    const added = watch?.added ?? "";
    const turn = (l.logSince(mark) + "\n" + (await l.screen(400)))
      .split("\n")
      .filter((x) => !shown.has(norm(x)))
      .join("\n");
    const own = withoutMarkLines(ownWords(turn, [...calls.map((c) => c.added ?? ""), ask]));
    const picker = l.state.picked !== undefined;
    const asked = asksInstall(own);
    const asks = unwrap(own)
      .split("\n")
      .filter((x) => ASKS.test(x) || (/\?\s*$/.test(x) && OFFER.test(x)))
      .map((x) => x.trim())
      .slice(0, 3);
    const mine = l.prompts.slice(prompts);
    const wakeshiftPrompts = mine.filter(wakeshiftPrompt);
    r.metrics = {
      ...r.metrics,
      watch_called: !!watch,
      recognized_as: watch?.agent?.harness ?? null,
      asked_with_picker: picker,
      asked_in_chat: asked,
      questions: (picker ? 1 : 0) + (asked && !picker ? 1 : 0),
      offer_in_result: /AskUserQuestion|\binstall\b/i.test(added),
      // Wakeshift's text itself asks for the install (offer_in_result also
      // matches "don't install anything"): the bar.
      install_offer_in_result: INSTALL_OFFER.test(added),
      wakeshift_prompts: wakeshiftPrompts.length,
      prompts: mine.filter((p) => p.kind === "approval").length,
    };
    if (picker) r.notes.push(`the agent asked with its picker (answered ${String(l.state.picked)})`);
    for (const a of asks) r.notes.push(`the agent asked: "${redactPlain(a).slice(0, 300)}"`);
    for (const p of wakeshiftPrompts) r.notes.push(`a prompt for Wakeshift's own command: ${p.chosen} (${p.asks.slice(-200)})`);
    if (added) r.notes.push(`Wakeshift's text: ${added.slice(0, 1500)}`);
    await l.snap(r.metrics.questions ? "the agent asked about notifications" : "no question", { questions: r.metrics.questions });
    // A later post: the install wakes the new conversation.
    await l.waitIdle(NO, 60_000);
    const after = new Date().toISOString();
    const [e] = await this.ctx.bench.publish(l.id, 1, "S18");
    l.note(`published ${e.code}`);
    const tPub = Date.now();
    const ms = await this.waitWake(l, e.code, after, 120_000, NO);
    r.metrics.woke = ms !== null;
    r.metrics.wake_ms = ms === null ? null : Date.now() - tPub;
    await l.snap(ms !== null ? `woke for ${e.code}` : `no wake for ${e.code}`);
    r.works = !!watch && ms !== null;
    if (!r.works) r.reason = !watch ? "the agent didn't call watch in the new conversation" : `the post (${e.code}) didn't wake the new conversation`;
    return this.done(r);
  }

  /**
   * S19 and S19h: Wakeshift's plugin updated under a conversation it keeps
   * connected, in a lane of its own (Claude Code, a local core). S19 starts
   * on the plugin as published (Bench.pluginUpgrade.from: the commit
   * production's was rendered from, with its issuer option, installed the
   * way the core's offer did, `--config issuer=`); S19h on the hooks
   * Wakeshift's installer wrote to ~/.claude/settings.json. The
   * conversation watches (S18_ASK) and is woken once through what's
   * installed. Then the marketplace is rendered as this run's core renders
   * it (`to`: this checkout's plugin, or WB_AGENT_PLUGINS_REF's), pinned to
   * a connector a minute newer, and the user updates in another terminal:
   * S19 `claude plugin marketplace update wakeshift` and `claude plugin
   * update wakeshift@wakeshift`; S19h the core's update offer (the plugin in
   * place of the installer's hooks). Then /reload-plugins in the
   * conversation, and nothing else said. Two posts must wake the same
   * conversation with no nudge and no message from the user: the first
   * right after the reload, the second once the hooks have run the new
   * plugin. It works when all three posts woke it, the update and install
   * succeeded, the connector moved to the new pin, nothing asked the user
   * about installing (no picker, no question, no prompt for Wakeshift's
   * own commands), nothing reached the model twice, and (S19h) the
   * installer's hooks are gone from settings.json.
   */
  async phaseS19(from: "plugin" | "hooks"): Promise<void> {
    const id: ScenarioId = from === "plugin" ? "S19" : "S19h";
    const d = this.driver;
    const up = this.ctx.bench.pluginUpgrade;
    if (d.id !== "claude-code") {
      this.results.push(Scenarios.na(id, d.id, "Claude Code's plugin only"));
      return;
    }
    if (!up) {
      this.results.push(Scenarios.na(id, d.id, "needs a local core: it renders the marketplace as published, then as this checkout has it"));
      return;
    }
    const l = new Lane(this.ctx, d, "b", { id: `${d.id}-${from === "plugin" ? "up" : "uh"}` });
    l.state.apart = true;
    await this.phase(l, (l, results) => this.bodyS19(l, results, id, from, up));
  }

  private async bodyS19(l: Lane, results: ScenarioResult[], id: ScenarioId, from: "plugin" | "hooks", up: PluginUpgrade): Promise<void> {
    const d = this.driver;
    const core = this.ctx.bench.core;
    const claude = d.binPath?.();
    if (!claude) throw new Error(`${d.name}'s binary isn't there`);
    const env = d.envFor!(l);
    const r = this.result(id, l);
    r.path = "hooks";
    await d.prepare(l);
    await this.recordVersion(l);
    const harnessVersion = String(l.state.version ?? "").split(" ")[0];
    const mk = join(this.ctx.dir, `marketplace-${l.id}`);
    const run = async (label: string, cmd: string, args: string[]) => {
      const x = await l.run(env, cmd, args);
      l.note(`${label}: ${x.ok ? "ok" : "failed"}`, { out: redactPlain(x.out.slice(-1500)) });
      return x;
    };
    let installedFrom = false;
    if (from === "plugin") {
      // The published plugin, installed as its offer did (with the issuer option it has).
      const v1 = await up.from(mk);
      l.note(`rendered ${up.fromRef}'s plugins into ${mk}, pinned to ${v1}`);
      const add = await run("claude plugin marketplace add", claude, ["plugin", "marketplace", "add", mk]);
      const ins = add.ok ? await run("claude plugin install (as published)", claude, ["plugin", "install", "wakeshift@wakeshift", "--config", `issuer=${core}`]) : add;
      installedFrom = ins.ok && pluginInstalled(l);
    } else {
      const ins = await run("Wakeshift's installer", "/bin/sh", ["-c", `curl -fsSL ${core}/install/claude-code.sh | sh`]);
      installedFrom = ins.ok && scriptHooks(l);
    }
    r.metrics.from = from === "plugin" ? up.fromRef : "installer hooks";
    r.metrics.from_version = from === "plugin" ? installedPluginVersion(l) : null;
    r.metrics.installed_from = installedFrom;
    if (!installedFrom) {
      r.works = false;
      r.reason = from === "plugin" ? "the published plugin didn't install" : "Wakeshift's installer didn't write its hooks";
      results.push(this.done(r));
      return;
    }
    await d.start(l);
    l.state.panePid = await l.tmux.panePid(l.session);
    await l.snap("started, with what's installed");
    // Watch, as a user would in a conversation that's already set up.
    const t0 = new Date().toISOString();
    await l.say(S18_ASK, NO, 300_000);
    await l.waitIdle(NO, 90_000);
    this.unkeyed.push({ from: t0, to: new Date().toISOString() });
    let connected = false;
    const until = Date.now() + 60_000 * this.ctx.slow;
    while (Date.now() < until && !(connected = installConnected(d, await this.presence(l)))) await sleep(1000);
    if (!connected) {
      await this.back(l, NO);
      const again = Date.now() + 60_000 * this.ctx.slow;
      while (Date.now() < again && !(connected = installConnected(d, await this.presence(l)))) await sleep(1000);
    }
    r.metrics.connected_before = connected;
    await l.snap(connected ? "connected through what's installed" : "not connected");
    const codes: string[] = [];
    const seen = new Set<string>();
    const post = async (tag: string): Promise<number | null> => {
      await l.waitIdle(NO, 90_000);
      await sleep(4000);
      const after = new Date().toISOString();
      const [e] = await this.ctx.bench.publish(l.id, 1, tag);
      codes.push(e.code);
      l.note(`published ${e.code} (${tag})`);
      const t = Date.now();
      const ms = await this.waitWake(l, e.code, after, 120_000, NO);
      if (ms !== null) seen.add(e.code);
      await l.snap(ms !== null ? `woke for ${e.code}` : `no wake for ${e.code}`);
      return ms === null ? null : Date.now() - t;
    };
    const before = await post(`${id} before the update`);
    r.metrics.woke_before_ms = before;
    const connector = async () => {
      const x = await l.run(env, join(l.home.dir, ".wakeshift/bin/wakeshift"), ["version"]);
      return x.ok ? x.out.trim().split("\n").pop()!.trim() : null;
    };
    r.metrics.connector_before = await connector();

    // The update, in another terminal: the conversation is told nothing.
    await l.waitIdle(NO, 90_000);
    const v2 = await up.to(mk);
    l.note(`rendered ${up.toRef ?? "this checkout"}'s plugins into ${mk}, pinned to ${v2}`);
    r.metrics.pin = v2;
    let updated: boolean;
    if (from === "plugin") {
      const m = await run("claude plugin marketplace update wakeshift", claude, ["plugin", "marketplace", "update", "wakeshift"]);
      const u = m.ok ? await run("claude plugin update wakeshift@wakeshift", claude, ["plugin", "update", "wakeshift@wakeshift"]) : m;
      updated = u.ok;
    } else {
      // The core's update offer for this version (internal/harnesses: Update, BeforeUpdate), with the
      // issuer option only where the plugin it installs still has one (WB_AGENT_PLUGINS_REF=PUBLISHED_PLUGIN_REF,
      // as production's core offered it with that plugin).
      const opt = pluginTakesIssuer(mk) ? ` --config issuer=${core}` : "";
      const q = (x: string) => `'${x.replace(/'/g, "'\\''")}'`;
      const tail = " && claude plugin marketplace update wakeshift && claude plugin update wakeshift@wakeshift";
      const cmd =
        harnessVersion && compareVersions(harnessVersion, PLUGIN_ONE_COMMAND) >= 0
          ? `claude plugin install wakeshift --marketplace ${q(mk)}${opt}${tail}`
          : `claude plugin marketplace add ${q(mk)} && claude plugin install wakeshift@wakeshift${opt}${tail}`;
      l.note(`the core's update offer: ${cmd}`);
      // The lane's environment has this lane's claude first on PATH.
      const u = await run("the update offer", "/bin/sh", ["-c", cmd]);
      updated = u.ok;
    }
    r.metrics.updated = updated && pluginInstalled(l);
    r.metrics.to_version = installedPluginVersion(l);
    const prompts = l.prompts.length;
    const mark = l.logSize();
    const norm = (x: string) => x.replace(/\s+/g, " ").trim();
    const shown = new Set((await l.screen(400)).split("\n").map(norm).filter((x) => x.length >= 12));
    for (const w of (await d.afterInstall?.(l)) ?? []) l.note(w);
    l.state.panePid = await l.tmux.panePid(l.session);
    await l.snap("after /reload-plugins");
    delete l.state.picked;
    // Woken with nothing said: right after the reload, then once the new hooks have run.
    r.metrics.woke_after_update_ms = await post(`${id} after the update`);
    r.metrics.woke_after_pin_ms = await post(`${id} on the new pin`);
    r.metrics.connector_after = await connector();
    r.metrics.pin_moved = r.metrics.connector_after === v2;
    if (from === "hooks") r.metrics.installer_hooks_left = scriptHooks(l);
    // Asked nothing: no picker, no question, no prompt for Wakeshift's own commands.
    const turn = (l.logSince(mark) + "\n" + (await l.screen(400)))
      .split("\n")
      .filter((x) => !shown.has(norm(x)))
      .join("\n");
    const st = await this.ctx.bench.lane(l.id);
    const own = withoutMarkLines(ownWords(turn, [...st.calls.map((c) => c.added ?? ""), S18_ASK]));
    const picker = l.state.picked !== undefined;
    const asked = asksInstall(own);
    const mine = l.prompts.slice(prompts);
    r.metrics.questions = (picker ? 1 : 0) + (asked && !picker ? 1 : 0);
    r.metrics.wakeshift_prompts = mine.filter(wakeshiftPrompt).length;
    for (const x of unwrap(own).split("\n").filter((x) => ASKS.test(x)).slice(0, 3)) r.notes.push(`the agent asked: "${redactPlain(x.trim()).slice(0, 300)}"`);
    // Reported twice is the agent's choice (it may report a code again in a later turn); what
    // reached the model twice is a delivery's (countArrivals, below), and fails it.
    const rep = this.reports(st, t0, codes);
    for (const c of rep.counts.keys()) seen.add(c);
    r.metrics.duplicates = [...rep.counts.values()].filter((n) => n > 1).length;
    r.metrics.acked_unseen = this.ackedUnseen(st, codes, seen);
    const woke = [before, r.metrics.woke_after_update_ms, r.metrics.woke_after_pin_ms].filter((x) => x !== null).length;
    r.metrics.woke = woke;
    r.metrics.of = 3;
    const why = [
      before === null ? "the first post (before the update) didn't wake it" : "",
      r.metrics.updated !== true ? "the update failed" : "",
      r.metrics.woke_after_update_ms === null ? "the post right after the update didn't wake it" : "",
      r.metrics.woke_after_pin_ms === null ? "the post on the new pin didn't wake it" : "",
      r.metrics.pin_moved !== true ? `the connector is ${String(r.metrics.connector_after)}, not the new pin ${v2}` : "",
      r.metrics.questions ? "the agent asked about installing" : "",
      r.metrics.wakeshift_prompts ? "a prompt for Wakeshift's own command" : "",

      r.metrics.installer_hooks_left === true ? "the installer's hooks are still in settings.json" : "",
    ].filter(Boolean);
    r.works = why.length === 0;
    if (why.length) r.reason = why.join("; ");
    await this.countArrivals(r, l, codes, seen);
    results.push(this.done(r));

    // Uninstall, as S6 checks it.
    l.begin();
    await d.uninstall(l);
    const left = d.installed(l).filter((f) => !f.startsWith(".wakeshift/bin"));
    r.metrics.uninstall = left.length ? "left files" : "clean";
    if (left.length) r.notes.push(`after uninstall, still mentions Wakeshift: ${left.join(", ")}`);
    this.uninstallInventory(l, r);
  }

  /** Whether the agent reported code in a report_seen call at or after from (and before to, if given). */
  private reportedBetween(state: LaneState, from: string, to: string | null, code: string): boolean {
    return state.seen.some((s) => s.at >= from && (to === null || s.at < to) && s.ids.includes(code));
  }

  /**
   * An external app's calls, for S17's notes and metrics: how often the
   * agent checked the job itself (its status tools, before and after the
   * job ended), and what each result kept of Wakeshift's part (src/external.ts
   * wakeshiftPart: where its text was, the beacon, a structured field that
   * differs from the text).
   */
  private externalNotes(r: ScenarioResult, st: LaneState, endedAt: string) {
    const status = st.calls.filter((c) => c.role === "status");
    r.metrics.status_calls_before_end = status.filter((c) => c.at < endedAt).length;
    r.metrics.status_calls_after_end = status.filter((c) => c.at >= endedAt).length;
    const subs = st.calls.filter((c) => c.subscribes);
    r.metrics.subscribing_calls = subs.length;
    r.metrics.results_without_wakeshift = subs.filter((c) => !c.is_error && !c.added).length;
    r.metrics.results_without_beacon = subs.filter((c) => !c.is_error && !c.beacon).length;
    r.metrics.results_reshaped = subs.filter((c) => c.mismatch).length;
    for (const c of st.calls) r.notes.push(`${c.tool} (${c.role ?? "other"}${c.subscribes ? ", subscribes" : ""}): Wakeshift's part ${c.shape ?? "?"}, beacon ${c.beacon ? "yes" : "no"}, ${c.chars ?? 0} chars${c.is_error ? ", error" : ""}`);
  }

  /** S9 from both phases' calls. */
  async s9(lanes: Lane[]): Promise<ScenarioResult> {
    const d = this.driver;
    const r: ScenarioResult = { id: "S9", harness: d.id, status: "ran", metrics: {}, steps: [], notes: [], startedAt: new Date().toISOString() };
    const all: LaneState["calls"][] = [];
    // S16 and S17's lanes are apart: S16's calls are disguised, and neither is the mechanics prompt's.
    for (const l of lanes.filter((x) => x.state.apart !== true)) {
      const st = await this.ctx.bench.lane(l.id);
      all.push(st.calls);
      l.steps = r.steps;
      l.note(`${l.id}: ${st.calls.length} call(s)`, { calls: st.calls.map((c) => ({ at: c.at, tool: c.tool, client: c.client, args: c.args, recognized: c.agent ?? null, beacon: c.beacon })) });
    }
    const unkeyed = (c: LaneState["calls"][number]) => this.unkeyed.some((w) => c.at >= w.from && c.at <= w.to);
    return this.done(apply(r, argsCheck(all, { harness: d.harness, conversation: d.conversation }, unkeyed)));
  }
}

/**
 * Whether the install holds the conversation's stream now. A harness that
 * wakes with nothing installed already has its own command connected
 * (Claude Code's Monitor, Cursor's and agy's wait loops), so only the
 * transports an install uses count there (installVia, default hooks: the
 * hooks, connector and plugins report that). Where nothing else could hold
 * the stream, any connected transport is the install's (a container's
 * plugin holds an SSE stream).
 */
/**
 * The install the user runs when the agent didn't (S6's fallback, R10): the
 * core's offer for the lane, the harness's own plugin command from the
 * run's marketplace (marketplaceSource, or the plugin's own source), else
 * the driver's install command, else Wakeshift's installer.
 */
export function userInstallCommand(d: Driver, ctx: Pick<RunCtx, "bench">, l: Lane): string {
  const src = d.pluginInstall ? (d.pluginInstall.source ? d.pluginInstall.source(ctx.bench) : marketplaceSource(ctx.bench, d.pluginInstall.refSep)) : null;
  return src && d.pluginInstall ? d.pluginInstall.command(src) : (d.installCommand?.(l, ctx.bench.core) ?? `curl -fsSL ${ctx.bench.core}/install/${d.installName ?? d.id}.sh | sh`);
}

/** Runs the install as the user, where the harness runs: the driver's way (a container), else a shell here with the lane's environment (its temp HOME). */
export async function installAsTheUser(d: Driver, l: Lane, cmd: string): Promise<{ ok: boolean; out: string }> {
  return d.installAsUser ? d.installAsUser(l, cmd) : l.run(d.envFor ? d.envFor(l) : (process.env as Record<string, string>), "/bin/sh", ["-c", cmd]);
}

export function installConnected(d: Pick<Driver, "zeroInstallWake" | "installVia">, p: { connected: boolean; via: string | null }): boolean {
  if (!p.connected) return false;
  if (!d.zeroInstallWake) return true;
  return (d.installVia ?? ["hooks"]).includes(p.via ?? "");
}

/** Whether the rendered Claude Code plugin in a marketplace still has the issuer option (the plugin as published before it lost that option). */
export function pluginTakesIssuer(marketplace: string): boolean {
  try {
    const m = JSON.parse(readFileSync(join(marketplace, "plugins/claude-code/.claude-plugin/plugin.json"), "utf8")) as { userConfig?: Record<string, unknown> };
    return Boolean(m.userConfig && "issuer" in m.userConfig);
  } catch {
    return false;
  }
}

/** Puts an outcome (metrics.ts) on a result. */
export function apply(r: ScenarioResult, o: Outcome): ScenarioResult {
  r.metrics = { ...r.metrics, ...o.metrics };
  r.works = o.works;
  if (o.reason) r.reason = o.reason;
  if (o.notes) r.notes.push(...o.notes);
  return r;
}

/** The QUOTE: block from what the agent printed (redact before showing). */
export function extractQuote(text: string): string | null {
  // The agent's block, not the prompt's own "QUOTE: ." (the last one that isn't).
  let i = -1;
  for (const m of text.matchAll(/QUOTE:/g)) if (!/^\s*\.\s/.test(text.slice(m.index + 6, m.index + 12))) i = m.index;
  if (i < 0) return null;
  const rest = text.slice(i + 6).replace(/^[\s>"“]+/, "");
  const lines = rest.split("\n");
  const out: string[] = [];
  for (const line of lines) {
    if (out.length && /^\s*$/.test(line) && out.join("\n").length > 80) break;
    out.push(line.replace(/^\s*[│>|]\s?/, "").trimEnd());
    if (out.join("\n").length > 2000) break;
  }
  const q = out.join("\n").trim();
  return q || null;
}

/**
 * Why a harness refused the agent's command, quoted from the pane: the
 * lines saying it was blocked, denied or refused (Claude Code's auto mode
 * classifier, a permission dialog), not Wakeshift's own text. Null if
 * there's none.
 */
export function blockReason(screen: string, theirs = ""): string | null {
  const own = ownWords(screen, [theirs]);
  const lines = own.split("\n").map((x) => x.replace(/^[\s⏺⎿●•│▎>]+/, "").trimEnd());
  const hits: string[] = [];
  lines.forEach((line, i) => {
    // Whole words: "blocked-page-recovery" (a Hermes skill's name) isn't a refusal.
    // "Not run · auto mode's check had no usable answer": Claude Code's classifier didn't answer.
    if (!/(?<![\w-])(blocked|denied|denying|refused|not allowed|rejected|disallowed|declined|timed out)(?![\w-])|no usable answer/i.test(line)) return;
    if (/GOT\s|report_seen/.test(line)) return;
    const next = lines[i + 1] && !/^\s*$/.test(lines[i + 1]) && lines[i + 1].length > 20 ? " " + lines[i + 1].trim() : "";
    hits.push((line.trim() + next).slice(0, 400));
  });
  return hits.length ? [...new Set(hits)].slice(-2).join(" / ") : null;
}

/** The end of the last turn on screen: the lines above the input box and status lines. */
export function lastWords(screen: string, n = 14): string {
  const lines = screen.trimEnd().split("\n");
  // The input box and status line: from the last separator rule or prompt marker down.
  let end = lines.length;
  for (let i = lines.length - 1; i >= Math.max(0, lines.length - 12); i--) {
    if (/^\s*[─━-]{20,}|^\s*[❯›>]\s*$|^\s*›\s|Ask Codex|for shortcuts|auto mode|ctrl\+p/.test(lines[i])) end = i;
  }
  return lines
    .slice(0, end)
    .filter((x) => x.trim())
    .slice(-n)
    .join("\n");
}

/**
 * The agent's own words in a turn: lines that aren't part of Wakeshift's
 * text (shown in the tool result, or quoted back) or the prompt.
 */
/**
 * Wakeshift's text without its example question ('for example: "Install
 * Wakeshift's plugin for Hermes Agent? It runs `…`."') where the turn shows
 * that question as a line of its own: an agent that asks it word for word,
 * as the text suggests, is asking, but with the example left in, ownWords
 * dropped its line as Wakeshift's and S6 scored offered: false (the Hermes
 * API server lane). Inside the agent's QUOTE block the example
 * sits mid-line, after its opening quote mark, so that stays Wakeshift's.
 */
export function withoutSuggestedAsk(added: string, shown: string): string {
  // The question can quote a command with quotes in it, escaped (`'[\"https://…\"]'`) or not
  // (`'["https://…"]'`, as v1.4.56 gives Hermes's): it ends at its first "?" or "." followed by
  // the closing quote mark and a space (or the text's end).
  const m = /for example: "([^\n]{12,800}?[?.])"(?=\s|$)/.exec(added);
  if (!m) return added;
  const head = m[1].replace(/\s+/g, " ").slice(0, 40);
  const ownLine = shown.split("\n").some((l) => l.replace(/^[\s│┃▎>|⎿●•*-]+/, "").replace(/\s+/g, " ").startsWith(head));
  return ownLine ? added.slice(0, m.index) + added.slice(m.index + m[0].length) : added;
}

/**
 * The turn without the lines that are someone else's text (Wakeshift's, the
 * user's): a line whose start (60 characters) is in one of theirs. A line
 * the harness cut short with "…" (Codex's collapsed tool result, in a
 * narrow pane) is judged without the "…". Lines too short to judge alone
 * (under 12 characters: a pane log can hold a line drawn a word or a few
 * at a time, each piece a line of its own) are judged together: a run of
 * consecutive short lines that, joined, is in one of theirs (at least
 * FRAGMENT_MIN characters) goes too. Without that, Codex's collapsed
 * result drawn word by word read as the agent's words, and R1 scored a
 * "mention" of the install the agent never made (Codex 0.169.1).
 */
export function ownWords(turn: string, theirs: string[]): string {
  const norm = (x: string) => x.replace(/\s+/g, " ").trim();
  const known = theirs.map(norm).filter(Boolean);
  // Codex's tool result starts under "└".
  const bare = (line: string) => norm(line.replace(/^[\s│>|⎿●•└*-]+/, "")).replace(/[\s│]+$/, "");
  // Cut short by the harness: "…" (or "...") at the end isn't in their text.
  const uncut = (t: string) => t.replace(/\s*(…|\.\.\.)$/, "");
  const theirsHas = (t: string) => known.some((k) => k.includes(t));
  const lines = turn.split("\n");
  const drop = new Set<number>();
  lines.forEach((line, i) => {
    const t = bare(line);
    if (t.length < 12) return;
    const probe = uncut(t.slice(0, 60));
    if (probe.length >= 12 && theirsHas(probe)) drop.add(i);
  });
  // Runs of short pieces: from each, the longest stretch that's in their text, joined.
  const short = (i: number) => {
    const t = bare(lines[i]);
    return t.length > 0 && t.length < FRAGMENT_LINE;
  };
  for (let i = 0; i < lines.length; ) {
    if (!short(i)) {
      i++;
      continue;
    }
    let j = i;
    let joined = bare(lines[i]);
    let best = -1;
    while (j + 1 < lines.length && short(j + 1)) {
      const next = `${joined} ${bare(lines[j + 1])}`;
      if (!theirsHas(uncut(next))) break;
      joined = next;
      j++;
      best = j;
    }
    if (best > i && uncut(joined).length >= FRAGMENT_MIN) {
      for (let x = i; x <= best; x++) drop.add(x);
      i = best + 1;
    } else i++;
  }
  return lines.filter((_, i) => !drop.has(i)).join("\n");
}

/** A line shorter than this can be a piece of a line drawn a few words at a time (ownWords). */
const FRAGMENT_LINE = 24;
/** How much of their text a run of pieces must make, joined, to be theirs (ownWords). */
const FRAGMENT_MIN = 24;
