/**
 * Picker installs: each lane adds the toy app's MCP server exactly as the
 * public agent picker tells users to, so a broken picker command fails the
 * bench (OpenClaw's `mcp add`, refused while its gateway ran, once passed
 * because the lanes used the bench's own setup code).
 *
 * The data is the picker's own: apps/agentpicker/agents.json, which `make
 * agent-picker` generates from internal/harnesses.Picker and every app's
 * picker (the Webhooks app's, the Go apps', the site's) renders. The bench
 * reads it at run time, never a copy, and fills its placeholders as the
 * picker does (fill: the same as apps/agentpicker's Fill and picker.ts's
 * fill; test/picker.test.ts checks it against the Go renderer's golden
 * page). WB_PICKER_DATA=<file> reads another agents.json instead, e.g. one
 * from before a fix (`git show <commit>:apps/agentpicker/agents.json`), to
 * check that the bench fails on it.
 *
 * WB_MCP_ADD picks the way: `picker` (the default) or `bench` (the bench's
 * own setup code, as before November 10, 2026). A lane whose server needs
 * a token (S11, S12) always uses the bench's way: the picker's commands
 * have no token, and the bench never puts one on a command line.
 */
import { existsSync, readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

export interface PickerStep {
  kind: "run" | "file" | "url" | "note" | "cursor";
  label?: string;
  text?: string;
}

export interface PickerAgent {
  id: string;
  name: string;
  group: string;
  steps: PickerStep[];
  signIn?: string;
  wakes: string[];
}

/** What the picker fills in: the server's name, its URL, and whether the app has sign-in. */
export interface PickerOptions {
  name: string;
  url: string;
  each?: string;
  /** The app has no sign-in (the toy app): no OAuth flag for Hermes, "No authentication" for a connector. */
  noAuth?: boolean;
}

const here = dirname(fileURLToPath(import.meta.url));

/** apps/agentpicker/agents.json in this checkout (from dist/src or src). */
export function defaultPickerData(): string {
  for (const up of ["../../..", "../.."]) {
    const p = join(here, up, "apps/agentpicker/agents.json");
    if (existsSync(p)) return p;
  }
  return join(here, "../../../apps/agentpicker/agents.json");
}

/** Where the picker's data is read from (WB_PICKER_DATA, else this checkout's). */
export function pickerDataPath(env: NodeJS.ProcessEnv = process.env): string {
  return env.WB_PICKER_DATA || defaultPickerData();
}

export function readPicker(path = pickerDataPath()): PickerAgent[] {
  const doc = JSON.parse(readFileSync(path, "utf8")) as unknown;
  if (!Array.isArray(doc)) throw new Error(`${path}: not the picker's agent list`);
  return doc as PickerAgent[];
}

/** The way lanes add the MCP server: the picker's (default) or the bench's own (WB_MCP_ADD=bench). */
export function mcpAddMode(env: NodeJS.ProcessEnv = process.env): "picker" | "bench" {
  const v = (env.WB_MCP_ADD ?? "picker").trim();
  if (v === "picker" || v === "") return "picker";
  if (v === "bench") return "bench";
  throw new Error(`WB_MCP_ADD=${v}: use picker or bench`);
}

/** Placeholders the picker knows; any other {word} in a step is new to the bench, and fails it. */
const PLACEHOLDERS = ["{name}", "{url}", "{each}", "{Each}", "{auth}", "{hermesauthyaml}", "{hermesauth}"];

/** The picker's fill (apps/agentpicker Fill, picker.ts fill). */
export function fill(s: string, o: PickerOptions): string {
  const each = o.each || "each message";
  const vals: Record<string, string> = {
    "{name}": o.name,
    "{url}": o.url,
    "{each}": each,
    "{Each}": each[0].toUpperCase() + each.slice(1),
    "{auth}": o.noAuth ? "No authentication" : "OAuth",
    "{hermesauth}": o.noAuth ? "" : " --auth oauth",
    "{hermesauthyaml}": o.noAuth ? "" : "\n    auth: oauth",
  };
  return s.replace(/\{name\}|\{url\}|\{each\}|\{Each\}|\{auth\}|\{hermesauthyaml\}|\{hermesauth\}/g, (k) => vals[k]);
}

/** A placeholder in the text that fill doesn't know (the picker grew one the bench must learn). */
export function unknownPlaceholder(s: string): string | null {
  for (const m of s.matchAll(/\{[a-zA-Z]+\}/g)) if (!PLACEHOLDERS.includes(m[0])) return m[0];
  return null;
}

/** A harness's picker entry, or an error naming what's missing. */
export function pickerAgent(id: string, agents = readPicker()): PickerAgent {
  const a = agents.find((x) => x.id === id);
  if (!a) throw new Error(`the picker has no ${id} tab (${pickerDataPath()})`);
  return a;
}

/** A step of a kind, filled in; throws if the picker has none or it holds a placeholder the bench doesn't know. */
export function pickerStep(id: string, kind: PickerStep["kind"], o: PickerOptions, agents = readPicker()): { step: PickerStep; text: string } {
  const step = pickerAgent(id, agents).steps.find((s) => s.kind === kind);
  if (!step) throw new Error(`the picker's ${id} tab has no ${kind} step`);
  const raw = step.text ?? "";
  const unknown = unknownPlaceholder(raw);
  if (unknown) throw new Error(`the picker's ${id} ${kind} step has a placeholder the bench doesn't fill: ${unknown}`);
  return { step, text: fill(raw, o) };
}

/**
 * A command as a POSIX shell splits it: words, single quotes (literal),
 * double quotes (backslash escapes \ " $ `), and backslashes outside
 * quotes. Anything a shell would expand ($, `, globs, ;, |, &, redirects)
 * outside single quotes is refused: the picker's commands are plain, and
 * the bench runs them without a shell.
 */
export function shellWords(cmd: string): string[] {
  const out: string[] = [];
  let cur = "";
  let inWord = false;
  for (let i = 0; i < cmd.length; i++) {
    const c = cmd[i];
    if (c === "'") {
      const end = cmd.indexOf("'", i + 1);
      if (end < 0) throw new Error(`unterminated ' in: ${cmd}`);
      cur += cmd.slice(i + 1, end);
      i = end;
      inWord = true;
    } else if (c === '"') {
      i++;
      for (; i < cmd.length && cmd[i] !== '"'; i++) {
        if (cmd[i] === "\\" && i + 1 < cmd.length && '\\"$`'.includes(cmd[i + 1])) cur += cmd[++i];
        else if (cmd[i] === "$" || cmd[i] === "`") throw new Error(`shell expansion in: ${cmd}`);
        else cur += cmd[i];
      }
      if (i >= cmd.length) throw new Error(`unterminated " in: ${cmd}`);
      inWord = true;
    } else if (c === "\\") {
      if (i + 1 >= cmd.length) throw new Error(`trailing \\ in: ${cmd}`);
      cur += cmd[++i];
      inWord = true;
    } else if (/\s/.test(c)) {
      if (inWord) out.push(cur);
      cur = "";
      inWord = false;
    } else if ("$`;|&<>*?()".includes(c)) {
      throw new Error(`shell syntax (${c}) in: ${cmd}`);
    } else {
      cur += c;
      inWord = true;
    }
  }
  if (inWord) out.push(cur);
  return out;
}

/**
 * The picker's config file for a harness (its "file" step): the path its
 * label names (under ~, e.g. "~/.cursor/mcp.json"; for Hermes,
 * "$HERMES_HOME/config.yaml") and the text to put there, filled in.
 */
export function pickerFile(id: string, o: PickerOptions, agents = readPicker()): { path: string; text: string } {
  const { step, text } = pickerStep(id, "file", o, agents);
  const unknown = unknownPlaceholder(step.label ?? "");
  if (unknown) throw new Error(`the picker's ${id} file step's label has a placeholder the bench doesn't fill: ${unknown}`);
  const path =/(?:~|\$[A-Z_]+)\/[\w./-]+\.(?:json|ya?ml|toml)/.exec(step.label ?? "")?.[0];
  if (!path) throw new Error(`the picker's ${id} file step doesn't say which file: ${step.label ?? "(no label)"}`);
  return { path, text };
}

/**
 * The picker's command for a harness, split into words, the program first
 * ("claude", "codex", "openclaw", ...). bin is what the lane runs for it.
 */
export function pickerCommand(id: string, o: PickerOptions, agents = readPicker()): { text: string; program: string; args: string[] } {
  const { text } = pickerStep(id, "run", o, agents);
  const words = shellWords(text);
  if (!words.length) throw new Error(`the picker's ${id} command is empty`);
  return { text, program: words[0], args: words.slice(1) };
}
