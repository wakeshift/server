// What a bench can add to the runner: targets beyond the local core (a hosted
// core), scenarios the run's harnesses play together after their own, run
// modes of their own, command-line options, work after the run and sections
// in its report. The runner itself has none: a checkout that has more
// registers it in src/extend.ts, which cli.ts loads when it's there.

import { existsSync } from "node:fs";
import { fileURLToPath } from "node:url";
import type { Driver } from "../drivers/types.js";
import type { AppId } from "../app/variants.js";
import type { StartOptions } from "./core.js";
import type { RunCtx } from "./lane.js";
import type { RunResult, ScenarioId, ScenarioResult } from "./model.js";

/** A core other than the local one, chosen with --target <name>. */
export interface Target {
  /**
   * Starts (or reaches) the core: writes the first app's key to keyFile
   * (and, with o.second, the second app's to keyFile2), mode 0600, and
   * returns the core's URL.
   */
  start(o: StartOptions, keyFile: string, keyFile2: string): Promise<string>;
  /** Why the second app (S15) can't run on this target (null: it can), beyond being a different app than the first. */
  secondAppProblem?(o: { first: string; second: string; options: Record<string, string> }): string | null;
  /** Why a toy app variant can't run on the key's app (null: it can). */
  variantProblem?(o: { variant: AppId; app: string }): string | null;
  /** Why the realistic tier's CI app can't run on the key's app (null: it can). */
  ciAppProblem?(o: { app: string; options: Record<string, string> }): string | null;
  /** The environment every toy app gets on this target (its own guards). */
  toyEnv?: Record<string, string>;
  /** The plugin marketplace a harness's plugin command takes for this core; refSep joins a repository and a branch. */
  marketplace?(refSep: string): string;
  /** Where Antigravity CLI's plugin comes from for this core. */
  antigravityPlugin?: string;
}

/** A scenario every harness of the run plays together, after their own (in the order registered). */
export interface Together {
  id: ScenarioId;
  /** The suffix of each harness's lane in it (its log is logs/<harness><suffix>.log). */
  lane: string;
  run(ctx: RunCtx, drivers: Driver[], o: { target: string; changed: () => string[] }): Promise<ScenarioResult[]>;
}

/** Work after the run, once its report is written: returns the report's path if it wrote it again. */
export type After = (o: { dir: string; options: Record<string, string>; render: () => string; log: (m: string) => void }) => Promise<string | null>;

/**
 * A section of report.html of its own: its data (null: none in this run),
 * and the CSS and script that render it. The script and CSS are fixed code,
 * never built from a run's data: data goes through data(), which the page
 * embeds as JSON, and the script reads it from there.
 */
export interface ReportSection {
  /** The section element's id: lowercase letters, digits and dashes. */
  id: string;
  data(dir: string, run: RunResult): unknown;
  css?: string;
  /**
   * A script run in the page after the grid: D (the page's data, this
   * section's at D.sections[id]), esc and HOOKS are in scope. It may set
   * HOOKS.cell.push((hid, sid) => html) for a line in each cell,
   * HOOKS.detail.push((hid, sid) => html) for a block in a cell's detail,
   * and render into the element with its id.
   */
  js: string;
}

/** A change to Claude Code's global config in each temp HOME (~/.claude.json). */
export interface ClaudeConfig {
  /** Returns what to write, given what the bench would write and the real HOME's config. */
  amend(json: Record<string, unknown>, real: Record<string, unknown>): Record<string, unknown>;
  /** Claude Code's wake path for the report, when this change decides it (null: it doesn't). */
  path?(env: NodeJS.ProcessEnv): string | null;
}

export interface Extensions {
  targets: Map<string, Target>;
  together: Together[];
  /** Run modes of their own, chosen by a flag anywhere in argv: the mode gets argv and returns the exit code. */
  modes: Map<string, (argv: string[]) => Promise<number>>;
  /** More command-line options: whether each takes a value (else it's a switch, "1" when given). An environment variable can stand in. */
  options: Map<string, { value: boolean; env?: string }>;
  after: After[];
  report: ReportSection[];
  /** Changes to Claude Code's config in each temp HOME, in order. */
  claude: ClaudeConfig[];
}

export const ext: Extensions = { targets: new Map(), together: [], modes: new Map(), options: new Map(), after: [], report: [], claude: [] };

/**
 * Adds report sections, refusing an id that's already taken (two sections
 * with one id would share one element and one slot of the page's data).
 */
export function addReportSections(...sections: ReportSection[]): void {
  for (const s of sections) {
    if (ext.report.some((x) => x.id === s.id)) throw new Error(`report section ${JSON.stringify(s.id)} is already registered`);
    ext.report.push(s);
  }
}

/** Loads src/extend.ts if this checkout has it (the runner doesn't need it). */
export async function loadExtensions(): Promise<void> {
  const url = new URL("./extend.js", import.meta.url);
  if (existsSync(fileURLToPath(url))) await import(url.href);
}
