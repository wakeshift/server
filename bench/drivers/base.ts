// What the terminal drivers share: a pane running a clean bash with the
// lane's HOME, the harness typed into it, and the processes under it.

import { existsSync, readFileSync, writeFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { stripAnsi } from "../src/ansi.js";
import { isoEnv, q, realBinary, shellArgv } from "../src/home.js";
import type { Lane, SecondMcp } from "../src/lane.js";
import { mcpAddMode, pickerCommand } from "../src/picker.js";
import { ps, type PsRow } from "../src/procs.js";
import { redactPlain } from "../src/redact.js";
import { sleep } from "../src/tmux.js";
import type { AnswerPolicy, Availability, Driver, PromptScope } from "./types.js";

/** The shell prompt of the pane's bash (--norc): "bash-5.2$ ". */
export const SHELL_PROMPT = /bash-[\d.]+\$\s*$/;

export abstract class TmuxDriver implements Partial<Driver> {
  abstract id: string;
  abstract name: string;
  /** The harness's binary on PATH. */
  abstract bin: string;
  mode: Driver["mode"] = "automated";

  binPath(): string | null {
    return realBinary(this.bin);
  }

  async version(): Promise<string | undefined> {
    return undefined;
  }

  async check(): Promise<Availability> {
    const p = this.binPath();
    if (!p) return { ok: false, reason: `${this.bin} isn't installed (not on PATH)` };
    return { ok: true, version: await this.version() };
  }

  /** The lane's environment: the HOME, and the harness's own directory on PATH. */
  env(l: Lane, extra: Record<string, string> = {}): Record<string, string> {
    const p = this.binPath();
    return isoEnv(l.home, p ? [dirname(p)] : [], extra);
  }

  /** Arguments that print the version (default --version). */
  versionArgs = ["--version"];

  async versionIn(l: Lane): Promise<string | null> {
    const p = this.binPath();
    if (!p) return null;
    const r = await l.run(this.env(l), p, this.versionArgs);
    const v = /(\d+\.\d+\.\d+(?:[-.][0-9A-Za-z]+)*)/.exec(r.out);
    return r.ok && v ? v[1] : null;
  }

  envFor(l: Lane): Record<string, string> {
    return this.env(l);
  }

  /** Opens the pane (a clean bash in the work dir) and starts logging it. */
  async openPane(l: Lane, extra: Record<string, string> = {}): Promise<void> {
    if (await l.tmux.exists(l.session)) return;
    await l.tmux.newSession(l.session, l.home.work, shellArgv(this.env(l, extra)));
    await l.tmux.pipe(l.session, l.logFile);
    const pid = await l.tmux.panePid(l.session);
    l.ctx.procs.record(pid ?? undefined, `pane ${l.session}`);
    await sleep(500);
    // A lane signed in to a real app (S11): the token the harness reads by
    // name (Codex's bearer_token_env_var). Never an argument (tmux keeps a
    // pane's start command): the shell sources it from a 0600 file, and only
    // the file's path is typed.
    if (l.mcpToken) {
      const f = tokenFile(l);
      writeFileSync(f, `export ${MCP_TOKEN_ENV}=${q(l.mcpToken)}\n`, { mode: 0o600 });
      await l.tmux.type(l.session, `. ${q(f)}`);
      await sleep(300);
    }
  }

  /**
   * The harness's login prompt on the pane (loginPrompt): the bench machine
   * isn't logged in to it, so the run is blocked (inconclusive), not failed.
   */
  async checkLogin(l: Lane, detail?: string): Promise<void> {
    if (loginPrompt(this.id, await l.screen(200))) throw new NotLoggedInError(this.name, detail);
  }

  /**
   * The harness never reached its start screen. On a lane's first start that
   * says nothing about the release (a key, login or machine problem the bench
   * can't see): CouldntStartError, blocked, with the screen's last lines. On
   * a restart (resume) the harness had started before, so it's an error.
   */
  async didntStart(l: Lane, detail: string, opts: { resume?: boolean } = {}): Promise<never> {
    if (opts.resume) throw new Error(detail);
    const screen = await l.screen(200).catch(() => "");
    throw new CouldntStartError(this.name, detail, screen);
  }

  /** Types a command at the pane's shell prompt. */
  async shell(l: Lane, argv: string[]): Promise<void> {
    await l.tmux.type(l.session, argv.map(q).join(" "));
  }

  /** Waits for the pane's shell prompt (the harness has exited). */
  async waitShell(l: Lane, ms = 20_000): Promise<boolean> {
    const end = Date.now() + ms;
    while (Date.now() < end) {
      const lines = (await l.screen(50)).trimEnd().split("\n");
      if (SHELL_PROMPT.test(lines[lines.length - 1] ?? "")) return true;
      await sleep(300);
    }
    return false;
  }

  async send(l: Lane, text: string): Promise<void> {
    await l.tmux.type(l.session, text);
  }

  /** The pane's processes, and any naming the lane's HOME (a detached connector). */
  mine(l: Lane, rows: PsRow[]): PsRow[] {
    const pane = l.state.panePid as number | undefined;
    const kids = new Map<number, PsRow[]>();
    for (const r of rows) {
      if (!kids.has(r.ppid)) kids.set(r.ppid, []);
      kids.get(r.ppid)!.push(r);
    }
    const out = new Map<number, PsRow>();
    const stack = pane ? [pane] : [];
    while (stack.length) {
      const p = stack.pop()!;
      for (const k of kids.get(p) ?? []) {
        out.set(k.pid, k);
        stack.push(k.pid);
      }
    }
    for (const r of rows) if (namesHome(r.command, l.home.dir) || namesHome(r.command, l.home.real)) out.set(r.pid, r);
    // Never the tmux server (its command line holds the first session's
    // arguments, HOME and all), nor the bench itself.
    for (const [pid, r] of out) if (/^(\S*\/)?tmux\b/.test(r.command) || pid === process.pid) out.delete(pid);
    // Remember them: a daemon the harness starts outlives the pane (Codex's
    // app-server), and is then no longer its descendant.
    const tracked = (l.state.tracked ??= {}) as Record<number, string>;
    for (const r of out.values()) tracked[r.pid] = r.command;
    return [...out.values()];
  }

  /** Records the lane's processes now (called while the lane is polled). */
  track(l: Lane): void {
    this.mine(l, ps());
  }

  /** The lane's processes still running: tracked earlier with the same command line. */
  leftovers(l: Lane): PsRow[] {
    const tracked = (l.state.tracked ?? {}) as Record<number, string>;
    const rows = ps();
    const now = this.mine(l, rows);
    const seen = new Set(now.map((r) => r.pid));
    for (const r of rows) if (!seen.has(r.pid) && tracked[r.pid] === r.command) now.push(r);
    return now;
  }

  /** Files under the HOME that mention Wakeshift (relative), from candidates. */
  mentions(l: Lane, candidates: string[]): string[] {
    const out: string[] = [];
    for (const rel of candidates) {
      const p = join(l.home.dir, rel);
      if (!existsSync(p)) continue;
      try {
        if (/wakeshift/i.test(readFileSync(p, "utf8"))) out.push(rel);
      } catch {
        out.push(rel); // A directory.
      }
    }
    return out;
  }

  /** The installer's uninstall, run in the lane's environment. */
  async uninstallHarness(l: Lane, harness: string): Promise<void> {
    const bin = join(l.home.dir, ".wakeshift/bin/wakeshift");
    if (!existsSync(bin)) return;
    const r = await l.run(this.env(l), bin, ["uninstall", "--harness", harness]);
    l.note(`uninstall --harness ${harness}: ${r.ok ? "ok" : "failed"}`, { out: r.out.slice(-2000) });
  }

  /**
   * Whether the harness says it's mid-turn, in the last lines it drew. A
   * capture of a pane the harness hasn't filled ends in blank rows (tmux
   * prints all 50), so those don't count: with them, Claude Code's status
   * line ("· esc to interrupt") fell outside the last 15 rows early in a
   * conversation, a turn waiting on `claude plugin install` read as over,
   * and R3 scored "not installed" while the install was still running.
   */
  busy(screen: string): boolean {
    return /esc to interrupt|Esc to interrupt/i.test(screen.replace(/\s+$/, "").split("\n").slice(-15).join("\n"));
  }

  /** Default: no dialog recognized. */
  async answer(_l: Lane, _screen: string, _policy: AnswerPolicy): Promise<string | null> {
    return null;
  }
}

const SHELL = /^(\S*\/)?-?(ba|z|da|k)?sh(\s|$)/;

/**
 * matched, plus the shells above each of them up to the first process that
 * isn't a shell (the harness), looked up in the lane's own processes and
 * never the pane's shell. A background command's loop runs in a shell whose
 * command line needn't name the stream (Cursor hands it the script another
 * way): killing only its curl left the loop running, which started the next
 * curl, so S4 killed nothing that mattered.
 */
export function withShellParents(l: Lane, mine: PsRow[], matched: PsRow[]): PsRow[] {
  const byPid = new Map(mine.map((r) => [r.pid, r]));
  const pane = l.state.panePid as number | undefined;
  const out = new Map(matched.map((r) => [r.pid, r]));
  for (const r of matched) {
    for (let p = byPid.get(r.ppid); p && p.pid !== pane && SHELL.test(p.command); p = byPid.get(p.ppid)) out.set(p.pid, p);
  }
  return [...out.values()];
}

/** Whether a command line names a path inside home (home/…, or home itself), not a sibling (homes/codex vs homes/codex-i). */
export function namesHome(command: string, home: string): boolean {
  let i = command.indexOf(home);
  while (i >= 0) {
    const next = command[i + home.length];
    if (next === undefined || next === "/" || /[\s'"=:]/.test(next)) return true;
    i = command.indexOf(home, i + 1);
  }
  return false;
}

/** The harness can't run here (no login, account policy, a container target it can't reach): its scenarios are blocked. */
export class BlockedError extends Error {}

/** Why a run was blocked when the bench machine isn't logged in to a harness (notLoggedInHarness reads it back). */
export function notLoggedInReason(name: string, detail?: string): string {
  return `the bench machine isn't logged in to ${name}${detail ? ` (${detail})` : ""}`;
}

/** The harness named in a notLoggedInReason, if text holds one. */
export function notLoggedInHarness(text: string): string | null {
  return /the bench machine isn't logged in to (.+?)(?: \(|[:;\n]|$)/.exec(text)?.[1] ?? null;
}

/** The bench machine isn't logged in to the harness: blocked, not failed (the run says nothing about the harness). */
export class NotLoggedInError extends BlockedError {
  constructor(
    readonly harness: string,
    detail?: string,
  ) {
    super(notLoggedInReason(harness, detail));
  }
}

/** How many of the screen's last lines a CouldntStartError keeps. */
export const START_SCREEN_LINES = 8;

/** The screen's last non-empty lines, without colors, spaces collapsed, clipped and redacted. */
export function lastScreenLines(screen: string, n = START_SCREEN_LINES): string[] {
  const lines = stripAnsi(screen)
    .split("\n")
    .map((x) => x.replace(/\s+/g, " ").trim())
    .filter(Boolean)
    .slice(-n);
  return lines.map((x) => redactPlain(x.length > 160 ? `${x.slice(0, 159)}…` : x));
}

/** Joins a reason's screen lines (a reason stays on one line). */
export const SCREEN_LINE_SEP = " ⏎ ";

/** Why a run was blocked when the bench couldn't start a harness (couldntStartIn reads it back). */
export function couldntStartReason(name: string, detail: string, screen: string): string {
  const lines = lastScreenLines(screen);
  return `the bench couldn't start ${name} (${detail}); ${lines.length ? `its screen's last lines: ${lines.join(SCREEN_LINE_SEP)}` : "its screen was empty"}`;
}

/** The harness named in a couldntStartReason, and the screen lines it quotes, if text holds one. */
export function couldntStartIn(text: string): { harness: string; screen: string[] } | null {
  const m = /the bench couldn't start (.+?) \(/.exec(text);
  if (!m) return null;
  const lines = /its screen's last lines: (.*)$/s.exec(text.slice(m.index))?.[1];
  return { harness: m[1], screen: lines ? lines.split(SCREEN_LINE_SEP) : [] };
}

/**
 * The bench couldn't start the harness: it never reached its start screen,
 * and no login screen was on it either. Blocked, not failed:
 * the run says nothing about the harness.
 */
export class CouldntStartError extends BlockedError {
  constructor(
    readonly harness: string,
    detail: string,
    screen: string,
  ) {
    super(couldntStartReason(harness, detail, screen));
  }
}

/**
 * What each harness shows when it isn't logged in: its login screen, or a
 * status line asking for /login. Matched on the bottom of the screen (where
 * a harness that just started draws it), so a transcript quoting the words
 * further up doesn't count.
 */
export const LOGIN_PROMPTS: Record<string, RegExp> = {
  // cursor-agent's start screen with no login: "Cursor Agent / v… / Press any key to log in...".
  // With CURSOR_API_KEY set to a key Cursor refuses, it exits at once: "⚠ Warning: The provided
  // API key is invalid. / The API key was loaded from the CURSOR_API_KEY environment variable."
  cursor: /Press any key to log in|Not logged in|log ?in to continue|cursor-agent login\b|provided API key is invalid/i,
  // Claude Code's login method picker, or its status line ("Not logged in · Run /login",
  // "Invalid API key · Please run /login", an expired or revoked OAuth token).
  "claude-code": /Select login method|Claude account with subscription|Anthropic Console account|(?:Not logged in|Invalid API key|OAuth token (?:has expired|revoked)|token (?:has )?expired)\s*·\s*(?:Please )?run \/login/i,
  // Codex's sign-in screen: "Sign in with ChatGPT" / "Provide your own API key".
  codex: /Sign in with ChatGPT|Provide your own API key|Please (?:log|sign) in to Codex/i,
  // Copilot CLI: "Please use /login to sign in", "You are not logged in".
  copilot: /not (?:logged|signed) in|(?:use|run|type) \/login\b|Please (?:log|sign) in\b/i,
  // opencode's model needs no login (a free one); a provider that does: "run `opencode auth login`".
  opencode: /opencode auth login|ProviderAuthError/i,
  // pi: no provider key (the bench writes the OpenAI key into the temp agent directory's auth.json).
  pi: /No providers logged in|Use \/login to log into a provider|No API key (?:found|providers)/i,
  // agy: its Google sign-in.
  antigravity: /sign in with Google|Sign in to continue|Log in to|authenticate/i,
};

/** Whether the screen shows the harness's login prompt (LOGIN_PROMPTS) in its last 30 lines. */
export function loginPrompt(harness: string, screen: string): boolean {
  const re = LOGIN_PROMPTS[harness];
  return !!re && re.test(tail(screen, 30).replace(/[ \t]+/g, " "));
}

/**
 * What a harness shows when its model's free quota is used up: the model
 * stops answering, so the run says nothing about Wakeshift. opencode's free
 * models answer HTTP 429 and its TUI shows "Free usage limit reached.
 * Connect a provider or subscribe to OpenCode Go to keep going, or switch
 * models." (2.4.0, 2026-11-23).
 */
export const QUOTA_PROMPTS: Record<string, RegExp> = {
  opencode: /Free usage limit reached/i,
};

/** Whether the screen shows the harness's used-up quota (QUOTA_PROMPTS) in its last 30 lines. */
export function quotaPrompt(harness: string, screen: string): boolean {
  const re = QUOTA_PROMPTS[harness];
  return !!re && re.test(tail(screen, 30).replace(/[ \t]+/g, " "));
}

/** Why a run was blocked when the harness's model quota ran out. */
export function quotaReason(name: string): string {
  return `${name}'s model quota is used up (its free model's usage limit): use another model, or wait for the limit to reset`;
}

/** The harness's model quota ran out mid-run (quotaPrompt): blocked, not failed. */
export class QuotaError extends BlockedError {
  constructor(readonly harness: string) {
    super(quotaReason(harness));
  }
}

/** The last n lines of a screen. */
export function tail(screen: string, n = 30): string {
  return screen.trimEnd().split("\n").slice(-n).join("\n");
}

/** Whether a command on screen is Wakeshift's installer. */
export const INSTALLER = /\/install\/[a-z-]+\.sh|wakeshift(?:-codex)? install\b|hermes plugins install wakeshift\/hermes-plugin\b|claude plugin install wakeshift\b|\b(?:codex|copilot) plugin (?:marketplace add|add|install|update)\b|opencode plugin @wakeshift\/opencode\b|pi install npm:@wakeshift\/pi\b|@wakeshift\/connect\b|openclaw plugins install npm:@wakeshift\/openclaw\b|\bagy plugin install \S*antigravity-plugin\b/;

/** The environment variable holding a lane's MCP bearer token (S11). */
export const MCP_TOKEN_ENV = "WB_MCP_TOKEN";

/** Where a lane's pane sources its MCP token from (0600, in the temp HOME, removed with it). */
export function tokenFile(l: Lane): string {
  return join(l.home.dir, ".wb-mcp-token");
}

/**
 * Whether the lane adds its MCP server (or S15's second, server) with the
 * picker's command (src/picker.ts): WB_MCP_ADD=picker, the default, and a
 * server without a token (the picker's commands have none).
 */
export function usePicker(l: Lane, server?: SecondMcp): boolean {
  return mcpAddMode() === "picker" && (server !== undefined || !l.mcpToken);
}

/**
 * Adds the lane's MCP server (or S15's second) with exactly the command the
 * picker shows for this harness, its program replaced by the lane's own
 * (run gets the arguments). url overrides the server's (a container's view
 * of it). Throws when the picker has no command for the harness, names
 * another program, or the command fails: a broken picker command fails the
 * lane, never falls back to the bench's own way.
 */
export async function addWithPicker(
  l: Lane,
  o: { harness: string; program: string; run: (args: string[]) => Promise<{ ok: boolean; out: string }>; server?: SecondMcp; url?: string },
): Promise<void> {
  const name = o.server?.name ?? l.mcpName;
  const url = o.url ?? o.server?.url ?? l.mcpUrl;
  const c = pickerCommand(o.harness, { name, url, noAuth: true });
  if (c.program !== o.program) throw new Error(`the picker's ${o.harness} command runs ${c.program}, not ${o.program}: ${c.text}`);
  const r = await o.run(c.args);
  l.note(`the picker's command: ${r.ok ? "ok" : "failed"}`, { command: redactPlain(c.text), out: r.out.slice(-1000) });
  if (!r.ok) throw new Error(`the picker's command for ${o.harness} failed: ${redactPlain(c.text)}: ${r.out.trim().split("\n").slice(-3).join(" ").slice(0, 400)}`);
}

/**
 * The arguments that add a lane's MCP server. A token never goes on a
 * command line (other processes can read arguments): Codex gets the name
 * of the environment variable that holds it; Claude Code and opencode get
 * their config entry written directly instead (mcpServerEntry).
 */
export function mcpAddArgs(l: Lane, style: "claude" | "codex" | "opencode", server?: SecondMcp): string[] {
  // S15's second server (server) has no token.
  const name = server?.name ?? l.mcpName;
  const url = server?.url ?? l.mcpUrl;
  const token = server ? undefined : l.mcpToken;
  if (style === "claude") return ["mcp", "add", "--transport", "http", name, url];
  if (style === "codex") return ["mcp", "add", name, "--url", url, ...(token ? ["--bearer-token-env-var", MCP_TOKEN_ENV] : [])];
  return ["mcp", "add", name, "--url", url];
}

// ── Approval dialogs: their options, and which to choose ────────────────

/** A dialog's option: its label, its number if it has one, and whether the cursor is on it. */
export interface DialogOption {
  label: string;
  n?: number;
  cursor: boolean;
}

/** Numbered options at the bottom of a screen ("❯ 1. Yes", "  2. Yes, and don't ask again …"): the last list on it. */
export function numberedOptions(t: string): DialogOption[] {
  const out: DialogOption[] = [];
  for (const line of t.split("\n")) {
    const m = /^[\s│┃|]*([❯›>→▶●])?\s*(\d)[.)]\s+(.+?)[\s│┃|]*$/.exec(line);
    if (!m) continue;
    const n = Number(m[2]);
    // A new list starts at 1: keep only the last one on screen.
    if (n === 1) out.length = 0;
    if (out.length && out[out.length - 1].n !== n - 1) continue;
    out.push({ n, label: m[3].trim(), cursor: Boolean(m[1]) });
  }
  return out;
}

/**
 * An arrow-key menu's options (agy, Hermes): the dialog's lines that name an
 * approval or a refusal, numbered or not, with the cursor's glyph on one.
 */
export function menuOptions(t: string): DialogOption[] {
  const numbered = numberedOptions(t);
  if (numbered.length >= 2) return numbered;
  const out: DialogOption[] = [];
  for (const line of t.split("\n")) {
    const m = /^[\s│┃]*([❯›>→▶●◉])?\s*((?:Allow|Always|Approve|Run|Yes|No|Deny|Reject|Skip|Cancel|Don't)\b[^│┃\n]{0,100}?)[\s│┃]*$/.exec(line);
    // The dialog's question ("Allow calling this tool?") isn't an option.
    if (!m || /\?\s*$/.test(m[2])) continue;
    out.push({ label: m[2].trim(), cursor: Boolean(m[1]) });
  }
  return out.length >= 2 ? out : [];
}

/** How long an option's approval lasts, from its label. */
export function scopeOf(label: string): PromptScope {
  const l = label.toLowerCase().replace(/[’‘]/g, "'");
  if (/^\s*(no\b|deny|reject|skip|cancel|decline|don't allow|do not allow)/.test(l)) return "deny";
  if (/\b(this|current|running|rest of the( running)?|whole) (session|conversation|chat|thread|task)\b|\bsession only\b|\bfor (the )?session\b/.test(l)) return "session";
  if (/always|don't ask again|do not ask again|never ask|permanent|persist|allowlist|allow ?list|add .{0,60} to|remember/.test(l)) return "always";
  return "once";
}

/**
 * The option to choose: under scope "session", the broadest that ends with
 * the session, else one-time, else (the only yes) a persistent one; by
 * default, the persistent one where offered, else for the session, else
 * one-time (the bench's choice before S12). Null if there's no yes.
 */
export function chooseOption(opts: DialogOption[], policy: Pick<AnswerPolicy, "scope">): { opt: DialogOption; scope: PromptScope; forcedAlways: boolean } | null {
  const yes = opts.filter((o) => scopeOf(o.label) !== "deny");
  // WB_APPROVE_SCOPE=session: every dialog answered as S12 answers them (a
  // user who says "Yes" each time rather than "don't ask again").
  if (policy.scope === undefined && process.env.WB_APPROVE_SCOPE === "session") policy = { scope: "session" };
  const order: PromptScope[] = policy.scope === "session" ? ["session", "once", "always"] : ["always", "session", "once"];
  for (const s of order) {
    const o = yes.find((x) => scopeOf(x.label) === s);
    if (o) return { opt: o, scope: s, forcedAlways: policy.scope === "session" && s === "always" };
  }
  return null;
}

/** What a dialog asks to run: the lines above its options (box drawing and blanks dropped), shortened. */
export function dialogAsks(t: string, firstOption?: string): string {
  // Redacted while it still has its lines: a token wrapped onto the next
  // line is only recognized there.
  const lines = redactPlain(t).split("\n");
  let end = lines.length;
  if (firstOption) {
    const i = lines.findIndex((x) => x.includes(firstOption));
    if (i > 0) end = i;
  }
  const body = lines
    .slice(Math.max(0, end - 10), end)
    .map((x) => x.replace(/[│┃╭╮╰╯─━┌┐└┘]+/g, " ").replace(/\s+/g, " ").trim())
    .filter(Boolean)
    .join(" ⏎ ");
  return body.length > 400 ? "…" + body.slice(-400) : body;
}

/**
 * Moves an arrow-key menu's cursor to an option and confirms it (Hermes,
 * agy): from the option the cursor is on (else the first), Down or Up as
 * many times as it takes, then Enter.
 */
export async function selectInMenu(l: Lane, opts: DialogOption[], target: DialogOption): Promise<void> {
  const from = Math.max(0, opts.findIndex((o) => o.cursor));
  const to = opts.indexOf(target);
  const key = to >= from ? "Down" : "Up";
  for (let i = 0; i < Math.abs(to - from); i++) {
    await l.tmux.keys(l.session, key);
    await sleep(120);
  }
  await l.tmux.keys(l.session, "Enter");
}

/** A remote MCP server's config entry with its bearer token, as Claude Code (type http) and opencode (type remote) store it. */
export function mcpServerEntry(l: Lane, kind: "claude" | "opencode" = "claude"): Record<string, unknown> {
  const headers = l.mcpToken ? { headers: { Authorization: `Bearer ${l.mcpToken}` } } : {};
  return kind === "claude" ? { type: "http", url: l.mcpUrl, ...headers } : { type: "remote", url: l.mcpUrl, enabled: true, ...headers };
}
