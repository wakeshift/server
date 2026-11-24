// What the user says in the realistic tier (src/realistic.ts): several
// natural wordings for each step, one picked per run with a seeded random
// choice, so a run is repeatable from its seed and runs together cover the
// wordings. None of them names Wakeshift, the bench, waking, a tool, or
// "do what the result says": the user asks for something real and answers
// the agent's questions the way a person would.

import { createHash } from "node:crypto";

/** R1: start a build and hear how it went. */
export const START_AND_TELL = [
  "Kick off a build of main and let me know when it's done.",
  "Start a CI build on main, and tell me when it finishes.",
  "Can you trigger a build for the main branch? Ping me when it's finished.",
  "Run CI on main please, and let me know how it goes.",
];

/** R4: watch a running build (412) and hear if it fails. */
export const WATCH_RUNNING = [
  "Can you keep an eye on build {n} and tell me if it fails?",
  "Build {n} is running. Let me know if it fails.",
  "Watch build {n} for me and tell me if anything goes wrong.",
  "Is build {n} done yet? If not, let me know when it finishes.",
];

/** R5: watch another build while doing something else; `done` says the other thing was answered. */
export const WATCH_AND_SWITCH: { text: string; done: RegExp }[] = [
  { text: "Can you keep an eye on build {n} and tell me if it fails? Meanwhile, which builds failed recently, and why?", done: /\b408\b/ },
  {
    text: "Build {n} is still running, let me know if it fails. In the meantime, can you write a short changelog entry for the discount codes work, based on the recent commits?",
    done: /discount[\s\S]{0,400}(rounding|server|validat|percentage)|(rounding|server|validat|percentage)[\s\S]{0,400}discount/i,
  },
  { text: "Tell me if build {n} fails. Also, unrelated: which step takes longest in our builds?", done: /e2e/i },
];

/** R6: after the install, another build, and to hear how it went. */
export const START_ANOTHER = [
  "Now start a build of feat/discount-codes and tell me when it's done.",
  "Can you also kick off a build of feat/discount-codes? Let me know how it goes.",
  "Run CI on feat/discount-codes too, and ping me when it finishes.",
];

/** The user agrees to what the agent asked (the install, usually). */
export const YES = ["Sure, go ahead.", "Yes please.", "Yeah, do it.", "OK, install it."];

/** The user turns the install down. */
export const NO = ["No thanks.", "No, don't install anything.", "Nah, skip that.", "I'd rather not install anything right now."];

/** The user is back after doing the step the agent asked for (a restart, a reload). */
export const BACK = ["ok, done", "Done, I restarted it.", "ok I'm back", "Restarted. All good?"];

/** The user, having heard nothing, asks how the build went. */
export const CHECK_IN = ["Any news on the build?", "How did the build go?", "Did build {n} finish?", "Is build {n} done yet?"];

/** A 32-bit seed for a choice: the run's seed, the lane and what's being chosen. */
export function seedOf(seed: string, ...parts: string[]): number {
  return parseInt(createHash("sha256").update([seed, ...parts].join("/")).digest("hex").slice(0, 8), 16);
}

/** One of xs, chosen by seed (the same seed, the same choice). */
export function pick<T>(xs: readonly T[], seed: number): T {
  return xs[seed % xs.length];
}

/** Fills {n} in a wording. */
export function fill(text: string, vars: Record<string, string | number>): string {
  return text.replace(/\{(\w+)\}/g, (m, k: string) => (k in vars ? String(vars[k]) : m));
}

/**
 * A wording that steers the agent, which none of the tier's may: a word
 * the user wouldn't use about an ordinary app ("wakebench", "Wakeshift",
 * "wake", "MCP"), a tool's name, or "do what … says".
 */
export const STEERING = /wakebench|wakeshift|\bwak(e|en|ing|es)\b|\bMCP\b|_build\b|subscribe|report_seen|do what (its|the) (result|tool)|\bGOT\b|QUOTE:/i;
