// Cursor CLI (cursor-agent), in a temp HOME. Login: with CURSOR_API_KEY in
// the bench's environment (the repository secret on the bench machine), that
// key, handed to the pane's shell through a tmux buffer with echo off (never
// an argument, a file, the screen or a log; cursorKeyExport), which
// cursor-agent reads headlessly, keeping the login it gets in memory
// (CURSOR_STORE_ENV); without it, the login keychain (symlinked),
// the machine's own login. Zero install: the agent starts a background Shell with
// notify_on_output on the session's wait loop (the same loop as Claude Code's
// Monitor; it keeps running with the hooks installed). Install: hooks in
// ~/.cursor/hooks.json, picked up when cursor-agent starts again.

import { execFile } from "node:child_process";
import { mkdirSync, writeFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { promisify } from "node:util";
import { borrowKeychain, writeJson } from "../../src/home.js";
import type { Lane } from "../../src/lane.js";
import { pickerFile } from "../../src/picker.js";
import type { PsRow } from "../../src/procs.js";
import { sleep } from "../../src/tmux.js";
import { chooseOption, dialogAsks, INSTALLER, scopeOf, tail, TmuxDriver, usePicker, withShellParents } from "../base.js";
import type { AnswerPolicy, Driver } from "../types.js";

const exec = promisify(execFile);

/** The environment variable cursor-agent reads its API key from (as --api-key, which would put it on a command line). */
export const CURSOR_KEY_ENV = "CURSOR_API_KEY";

/** The bench's Cursor API key, if it has one (the CURSOR_API_KEY repository secret). Never logged. */
export function cursorApiKey(env: NodeJS.ProcessEnv = process.env): string | null {
  const k = env[CURSOR_KEY_ENV]?.trim();
  return k ? k : null;
}

/**
 * The line the pane's shell runs to take the key: echo off, a marker (its
 * output, "(42)", differs from the typed line, "($((6*7)))"), the key read
 * from the terminal, echo back on. The key is then pasted (Tmux.pasteSecret).
 */
export const cursorKeyExport = `stty -echo; printf '%s\\n' "wakebench: reading ${CURSOR_KEY_ENV} ($((6*7)))"; IFS= read -r ${CURSOR_KEY_ENV}; stty echo; export ${CURSOR_KEY_ENV}`;
const KEY_READY = new RegExp(`wakebench: reading ${CURSOR_KEY_ENV} \\(42\\)`);

/**
 * Where cursor-agent keeps the login it gets for an API key
 * (AGENT_CLI_CREDENTIAL_STORE: "memory", "file" or its default, the macOS
 * keychain). With the key, the bench has it keep that login in memory: the
 * temp HOME has no keychain it can write, and cursor-agent can exit at start
 * when it can't save its login there. Each start logs in with the key again,
 * and nothing is written anywhere.
 */
export const CURSOR_STORE_ENV = "AGENT_CLI_CREDENTIAL_STORE";

export class Cursor extends TmuxDriver implements Driver {
  id = "cursor";
  name = "Cursor CLI";
  bin = "cursor-agent";
  zeroInstallWake = true;
  installPath = "hooks" as const;
  harness = "cursor";
  conversation = "agent" as const;
  trustStep = /restart|new (chat|session)|start (cursor-agent|it) again|quit and/i;

  async version() {
    try {
      const { stdout } = await exec(this.binPath()!, ["--version"], { timeout: 20_000 });
      return stdout.trim();
    } catch {
      return undefined;
    }
  }

  /** The lane's environment; with the bench's key, cursor-agent keeps its login in memory (CURSOR_STORE_ENV). */
  override env(l: Lane, extra: Record<string, string> = {}): Record<string, string> {
    return super.env(l, cursorApiKey() ? { [CURSOR_STORE_ENV]: "memory", ...extra } : extra);
  }

  async prepare(l: Lane): Promise<void> {
    // The bench's API key over the machine's login, where it has one.
    if (cursorApiKey()) l.note(`login: the bench's ${CURSOR_KEY_ENV}, not the keychain`);
    else borrowKeychain(l.home);
    if (usePicker(l)) {
      // The picker's way for cursor-agent: its mcp.json text, in the file its label names.
      const f = pickerFile("cursor", { name: l.mcpName, url: l.mcpUrl, noAuth: true });
      if (!f.path.startsWith("~/")) throw new Error(`the picker's Cursor file isn't under ~: ${f.path}`);
      try {
        JSON.parse(f.text);
      } catch (err) {
        throw new Error(`the picker's Cursor ${f.path} isn't JSON: ${err instanceof Error ? err.message : String(err)}`);
      }
      const path = join(l.home.dir, f.path.slice(2));
      mkdirSync(dirname(path), { recursive: true, mode: 0o700 });
      writeFileSync(path, f.text + "\n", { mode: 0o600 });
      l.note(`the picker's ${f.path}, as it shows it`);
    } else {
      // The bench's own way (WB_MCP_ADD=bench): the same file, written as JSON.
      writeJson(join(l.home.dir, ".cursor/mcp.json"), { mcpServers: { [l.mcpName]: { url: l.mcpUrl } } });
    }
    // Local config: no login needed, so not the key either.
    const r = await l.run(this.env(l), this.binPath()!, ["mcp", "enable", l.mcpName]);
    l.note(`cursor-agent mcp enable: ${r.ok ? "ok" : "failed"}`, { out: r.out.slice(-600) });
  }

  async start(l: Lane, opts: { resume?: boolean } = {}): Promise<void> {
    const fresh = !(await l.tmux.exists(l.session));
    await this.openPane(l);
    l.state.panePid = await l.tmux.panePid(l.session);
    // A new pane's shell takes the key once; a restart reuses the pane (and the key).
    const key = cursorApiKey();
    if (key && fresh) await this.exportKey(l, key);
    const model = process.env.WB_CURSOR_MODEL;
    // --approve-mcps skips the one-time approval of the MCP server;
    // WB_CURSOR_APPROVE_MCPS=0 leaves it to the dialog (S12 counts it).
    const approveMcps = process.env.WB_CURSOR_APPROVE_MCPS === "0" ? [] : ["--approve-mcps"];
    await this.shell(l, [this.binPath()!, "--trust", ...approveMcps, ...(opts.resume ? ["--continue"] : []), ...(model ? ["--model", model] : [])]);
    const ok = await l.waitFor(/Plan, search, build|Add a follow-up|→|ctrl\+c|\/ commands|Auto|Press any key to log in|provided API key is invalid/i, 60_000, { allowInstall: false });
    // "Press any key to log in...": no login on this machine (seen with 2026.11.05).
    // "The provided API key is invalid.": cursor-agent refused the key and exited.
    await this.checkLogin(l, key ? `cursor-agent refused the bench's ${CURSOR_KEY_ENV}` : `no ${CURSOR_KEY_ENV}, and the keychain's login didn't carry over`);
    if (ok === null) await this.didntStart(l, "cursor-agent didn't reach its start screen", opts);
    await sleep(3000);
  }

  /** Hands the pane's shell the key: echo off, read from the terminal, pasted from a tmux buffer. */
  private async exportKey(l: Lane, key: string): Promise<void> {
    await l.tmux.type(l.session, cursorKeyExport);
    const end = Date.now() + 10_000;
    while (!KEY_READY.test(await l.screen(50))) {
      if (Date.now() > end) throw new Error(`the pane's shell didn't take ${CURSOR_KEY_ENV}`);
      await sleep(200);
    }
    await sleep(200);
    await l.tmux.pasteSecret(l.session, key + "\n");
    await sleep(500);
    l.note(`${CURSOR_KEY_ENV} handed to the pane's shell (not shown)`);
  }

  async stop(l: Lane): Promise<void> {
    await l.tmux.keys(l.session, "Escape");
    await sleep(300);
    for (let i = 0; i < 3; i++) {
      await l.tmux.keys(l.session, "C-c");
      await sleep(700);
      if (await this.waitShell(l, 2000)) return;
    }
    await l.tmux.keys(l.session, "C-d");
    await this.waitShell(l, 8000);
  }

  async answer(l: Lane, screen: string, policy: AnswerPolicy): Promise<string | null> {
    // The agent's own question picker about the install (AskQuestion,
    // "Clarifying Questions"): answered as the user would (policy.pick,
    // default no), as Claude Code's AskUserQuestion is. Unanswered, the
    // user's typed yes didn't reach it and the agent didn't install.
    const q = cursorInstallQuestion(tail(screen, 24));
    if (q) {
      const yes = policy.pick === "yes";
      const target = yes ? q.yes : q.no;
      const step = target - q.cursor;
      for (let i = 0; i < Math.abs(step); i++) {
        await l.tmux.keys(l.session, step > 0 ? "Down" : "Up");
        await sleep(120);
      }
      await l.tmux.keys(l.session, "Space");
      await sleep(200);
      await l.tmux.keys(l.session, "Enter");
      l.state.picked = yes ? "yes" : "no";
      l.state.pickedAt = Date.now();
      l.prompted({ kind: "question", asks: "the agent's question about the install", options: q.labels, chosen: q.labels[target], scope: "n/a", sessionOffered: false });
      return `picked "${yes ? "yes, install" : "no"}" in Cursor's question`;
    }
    const t = tail(screen, 14);
    // Approvals: "Run this command?" with "Run (once) (y)" / "Skip (esc or n)", MCP tools similarly.
    // The dialog's own option, keyed: not the status line's tip ("Use /run-everything to skip all
    // approvals"), which matched the words alone and had the bench type "y" into the chat.
    if (/\((y|esc or n|n)\)\s*$/m.test(t) && /(Skip|Reject|Deny|\(n\)|esc)/i.test(t) && /Run \(once\)|Run this|Allow|Approve/i.test(t)) {
      if (INSTALLER.test(t) && !policy.allowInstall && l.state?.picked !== "yes") {
        await l.tmux.keys(l.session, "n");
        return "denied the installer (the user said no)";
      }
      // Its options carry their keys: "Run (once) (y)", "Skip (esc or n)", an allowlist offer.
      const opts = keyedOptions(t);
      const c = chooseOption(opts.map((o) => ({ label: o.label, cursor: false })), policy);
      const key = (c && opts.find((o) => o.label === c.opt.label)?.key) || "y";
      await l.tmux.keys(l.session, key);
      l.prompted({
        kind: "approval",
        asks: dialogAsks(t, opts[0]?.label),
        options: opts.map((o) => `${o.label} (${o.key})`),
        chosen: c ? `${c.opt.label} (${key})` : "y",
        scope: c?.scope ?? "once",
        sessionOffered: opts.some((o) => scopeOf(o.label) === "session"),
        forcedAlways: c?.forcedAlways ?? false,
      });
      return `approved ${INSTALLER.test(t) ? "the installer" : /curl|stream|wakeshift wait/.test(t) ? "the background command" : /MCP/i.test(t) ? "an MCP tool" : "a command"}`;
    }
    if (/Trust this workspace|Do you trust/i.test(t)) {
      await l.tmux.keys(l.session, "Enter");
      return "trusted the workspace";
    }
    return null;
  }

  /**
   * The message still in the input box: its placeholder ("Add a follow-up",
   * "Plan, search, build") is gone from the bottom and the message's start is
   * there. Typed right after a restart, before cursor-agent was ready, it
   * sat there unsent (the restart after the install).
   */
  unsent(screen: string, text: string): boolean {
    const bottom = tail(screen, 8);
    const norm = (s: string) => s.replace(/[│┃╭╮╰╯─━→>]/g, " ").replace(/\s+/g, " ").trim();
    return !/Add a follow-up|Plan, search, build/i.test(bottom) && norm(bottom).includes(norm(text).slice(0, 30));
  }

  override busy(screen: string): boolean {
    return /ctrl\+c to stop|Generating|Running|esc to (interrupt|stop)/i.test(tail(screen, 10));
  }

  /** cursor-agent loads hooks when it starts: restart it, continuing the chat. */
  async afterInstall(l: Lane): Promise<string[]> {
    await l.waitIdle({ allowInstall: true }, 60_000);
    await this.stop(l);
    await this.start(l, { resume: true });
    l.state.trustAgainOnRestart = false;
    return ["restarted cursor-agent (--continue) to load the hooks"];
  }

  waiters(l: Lane, rows: PsRow[]): PsRow[] {
    // The loop's shell too, not just its curl (withShellParents).
    const mine = this.mine(l, rows);
    return withShellParents(l, mine, mine.filter((r) => /\/v1\/stream\b|hook --harness cursor|\.wakeshift\/bin\/wakeshift wait/.test(r.command)));
  }

  installed(l: Lane): string[] {
    return this.mentions(l, [".cursor/hooks.json", ".wakeshift/cursor", ".wakeshift/bin/wakeshift"]);
  }

  async uninstall(l: Lane): Promise<void> {
    await this.uninstallHarness(l, "cursor");
  }
}

/**
 * cursor-agent's question picker ("Clarifying Questions" or "Question 1 of 1", "Space select ·
 * Enter next/submit") when it asks about Wakeshift's install: its options
 * (the "Other" one aside), which is yes and which is no, and where the
 * cursor (›) is. Null if that isn't what's showing.
 */
export function cursorInstallQuestion(t: string): { labels: string[]; yes: number; no: number; cursor: number } | null {
  // Its header is "Clarifying Questions", or (cursor-agent 2026.10.22) the
  // title the agent gave the question, over "Question 1 of 1".
  if (!/Clarifying Questions|Question \d+ of \d+/.test(t) || !/Space select/.test(t)) return null;
  // The question itself is about the install, not just words on the screen around it.
  const asked = /^[\s│┃]*\d+\.\s+(.+)$/m.exec(t)?.[1] ?? "";
  if (!/install|hooks?|wakeshift/i.test(asked)) return null;
  const labels: string[] = [];
  let cursor = 0;
  for (const line of t.split("\n")) {
    const m = /^[\s│┃]*(›)?\s*\[[ x]\]\s+(.+?)[\s│┃]*$/.exec(line);
    if (!m) continue;
    if (m[1]) cursor = labels.length;
    labels.push(m[2].trim());
  }
  const no = labels.findIndex((x) => /^(no|don't|do not|skip)\b/i.test(x));
  const yes = labels.findIndex((x, i) => i !== no && !/^other\b/i.test(x) && (/^yes\b/i.test(x) || /\binstall/i.test(x)));
  return yes >= 0 && no >= 0 ? { labels, yes, no, cursor } : null;
}

/** cursor-agent's dialog options with the key each takes ("Run (once) (y)", "Skip (esc or n)"): the key's first word. */
export function keyedOptions(t: string): { label: string; key: string }[] {
  const out: { label: string; key: string }[] = [];
  for (const line of t.split("\n")) {
    const s = line.replace(/[│┃╭╮╰╯─━]+/g, " ").replace(/^[\s→>❯›]+/, "").trim();
    const m = /^(.{2,120}?)\s*\(((?:esc|tab|shift\+tab|enter|[a-z])(?: or [a-z])?)\)\s*$/i.exec(s);
    if (!m) continue;
    const keys = m[2].toLowerCase().split(" or ");
    // "esc or n": the letter is the one to type.
    const key = keys.find((k) => /^[a-z]$/.test(k)) ?? keys[0];
    out.push({ label: m[1].trim(), key: key === "esc" ? "Escape" : key === "tab" ? "Tab" : key === "enter" ? "Enter" : key });
  }
  return out;
}
