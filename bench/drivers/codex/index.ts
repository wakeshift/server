// Codex (codex), in a temp HOME. Login: a copy of ~/.codex/auth.json, never
// a symlink (its hash is in the run's safety check). Nothing wakes Codex
// without the install: messages wait for the next call. Install: Wakeshift's
// plugin from the marketplace the core offers (Codex's own `codex plugin
// marketplace add` and `codex plugin add`; a local core's own marketplace,
// Bench.agentPlugins, or Wakeshift's installer with WB_CURL_INSTALL=1),
// whose hooks run the connector (a hook that starts `wakeshift run
// --thread`: after MCP tool calls, and again at SessionStart and
// UserPromptSubmit if it stopped), then Codex's own trust step: quit,
// `codex resume --last`, "Trust all and continue".

import { execFile } from "node:child_process";
import { closeSync, copyFileSync, chmodSync, constants as fsConstants, existsSync, lstatSync, mkdirSync, openSync, readdirSync, readFileSync, readSync, statSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { promisify } from "node:util";
import { stateRoot } from "../../src/benchstate.js";
import { marketplaceSource } from "../../src/core.js";
import { readJsonIf, REAL_HOME } from "../../src/home.js";
import type { Lane } from "../../src/lane.js";
import type { PsRow } from "../../src/procs.js";
import { redactPlain } from "../../src/redact.js";
import { sleep } from "../../src/tmux.js";
import { addWithPicker, chooseOption, dialogAsks, notLoggedInReason, INSTALLER, mcpAddArgs, numberedOptions, scopeOf, tail, TmuxDriver, usePicker, type DialogOption } from "../base.js";
import type { Received } from "../../src/arrivals.js";
import type { AnswerPolicy, Availability, Driver, Turn } from "../types.js";

const exec = promisify(execFile);

/** How often turnOpen looks for a newer rollout file (a resumed or new thread). */
const ROLLOUT_RESCAN_MS = 5000;

/** Every rollout file under dir (Codex's sessions/YYYY/MM/DD), newest first. */
function rollouts(dir: string): string[] {
  const found: { f: string; t: number }[] = [];
  const walk = (d: string, depth: number) => {
    if (depth > 4) return;
    let es: import("node:fs").Dirent[];
    try {
      es = readdirSync(d, { withFileTypes: true });
    } catch {
      return;
    }
    for (const e of es) {
      const p = join(d, e.name);
      if (e.isDirectory()) walk(p, depth + 1);
      else if (e.isFile() && /^rollout-.*\.jsonl$/.test(e.name)) {
        try {
          found.push({ f: p, t: statSync(p).mtimeMs });
        } catch {
          // Gone.
        }
      }
    }
  };
  walk(dir, 0);
  found.sort((x, y) => y.t - x.t);
  return found.map((x) => x.f);
}

/** The most recently written rollout file under dir, or null. */
function latestRollout(dir: string): string | null {
  return rollouts(dir)[0] ?? null;
}

/**
 * The [features] every lane's config.toml sets. The ChatGPT account's
 * connected apps (Gmail, Google Drive, Wakeshift's own Webhooks app…) reach
 * Codex as one MCP server, codex_apps, even in an empty temp HOME, and the
 * agent calls them: the account's mail and files in a lane's context, and a
 * lane that isn't the scenario. So apps are off, with remote plugins and
 * browser and computer use. plugins stays on: in 0.167 plugins = false also
 * turns off Wakeshift's own plugin. Older Codex ignores a key it doesn't know.
 */
export const CODEX_FEATURES = ["apps", "remote_plugin", "browser_use", "browser_use_external", "computer_use", "in_app_browser"];

/** config.toml's [features] table: CODEX_FEATURES off. */
export const codexFeaturesToml = () => `[features]\n${CODEX_FEATURES.map((f) => `${f} = false`).join("\n")}\n`;

/** MCP servers Codex adds itself, allowed in every lane. */
export const CODEX_BUILTIN_SERVERS = ["codex_tui"];

/** The server an MCP namespace or tool name belongs to ("mcp__codex_apps__gmail" → codex_apps, "mcp__wakebench__report_seen" → wakebench). */
function serverOf(name: string): string | null {
  const m = /^mcp__(.+)$/.exec(name);
  if (!m) return null;
  if (m[1].startsWith("codex_apps")) return "codex_apps";
  return m[1].split("__")[0] || null;
}

/**
 * The MCP servers a Codex thread had in reach, from its rollout lines: the
 * namespaces of its tool list (world_state's tools, "mcp__<server>", one
 * "mcp__codex_apps__<app>" per app of the account), and any server a tool
 * call named (a direct call, or tools.mcp__… in a code-mode exec). listed:
 * whether the rollout had a world_state at all (Codex before 0.166 may not).
 */
export function rolloutToolServers(lines: string[]): { servers: string[]; listed: boolean } {
  const servers = new Set<string>();
  let listed = false;
  const add = (name: string) => {
    const s = serverOf(name);
    if (s) servers.add(s);
  };
  for (const raw of lines) {
    if (!raw.trim()) continue;
    let o: { type?: string; payload?: { type?: string; name?: string; input?: unknown; arguments?: unknown; state?: { tools?: unknown } } };
    try {
      o = JSON.parse(raw);
    } catch {
      continue;
    }
    const p = o.payload;
    if (!p) continue;
    // A world_state with no tools: no MCP server at all (Codex leaves the key out).
    if (o.type === "world_state" && p.state && typeof p.state === "object") {
      listed = true;
      if (p.state.tools && typeof p.state.tools === "object") for (const k of Object.keys(p.state.tools)) add(k);
    }
    if (o.type === "response_item" && (p.type === "function_call" || p.type === "custom_tool_call")) {
      add(p.name ?? "");
      const code = typeof p.input === "string" ? p.input : typeof p.arguments === "string" ? p.arguments : "";
      for (const m of code.matchAll(/\btools\s*(?:\.\s*|\[\s*["'`])(mcp__[A-Za-z0-9_-]+)/g)) add(m[1]);
    }
  }
  return { servers: [...servers].sort(), listed };
}

/** rolloutToolServers over every thread in a temp HOME, and the servers beyond expected (and Codex's own). */
export function codexToolServers(home: string, expected: string[]): { servers: string[]; unexpected: string[]; listed: boolean } {
  const servers = new Set<string>();
  let listed = false;
  for (const f of rollouts(join(home, ".codex/sessions"))) {
    let text: string;
    try {
      text = readFileSync(f, "utf8");
    } catch {
      continue;
    }
    const r = rolloutToolServers(text.split("\n"));
    listed ||= r.listed;
    for (const s of r.servers) servers.add(s);
  }
  const all = [...servers].sort();
  return { servers: all, unexpected: all.filter((s) => !expected.includes(s) && !CODEX_BUILTIN_SERVERS.includes(s)), listed };
}

const STARTED = '"type":"event_msg","payload":{"type":"task_started"';
const ENDED = ['"type":"event_msg","payload":{"type":"task_complete"', '"type":"event_msg","payload":{"type":"turn_aborted"'];

/**
 * Whether a Codex rollout (its JSONL text, or the part since `open` was
 * known) ends inside a turn: its last task_started event comes after its
 * last task_complete or turn_aborted. With neither in text, `open` stands.
 */
export function rolloutTurnOpen(text: string, open = false): boolean {
  const started = text.lastIndexOf(STARTED);
  const ended = Math.max(...ENDED.map((e) => text.lastIndexOf(e)));
  if (started < 0 && ended < 0) return open;
  return started > ended;
}

/**
 * A rollout read as it grows: the lines appended since the last read
 * (complete ones only), so a poll reads only what's new.
 */
export class RolloutTurns {
  private file: string | null = null;
  private offset = 0;
  private rest = "";
  private open = false;
  private scanned = 0;

  constructor(private readonly dir: string) {}

  /** Whether the newest rollout under dir has a turn under way. */
  turnOpen(): boolean {
    if (!this.file || Date.now() - this.scanned >= ROLLOUT_RESCAN_MS) {
      this.scanned = Date.now();
      const f = latestRollout(this.dir);
      if (f !== this.file) {
        this.file = f;
        this.offset = 0;
        this.rest = "";
        this.open = false;
      }
    }
    if (!this.file) return false;
    let fd: number | null = null;
    try {
      const size = statSync(this.file).size;
      if (size < this.offset) {
        this.offset = 0;
        this.rest = "";
        this.open = false;
      }
      if (size > this.offset) {
        fd = openSync(this.file, "r");
        const buf = Buffer.alloc(size - this.offset);
        const n = readSync(fd, buf, 0, buf.length, this.offset);
        this.offset += n;
        const text = this.rest + buf.subarray(0, n).toString("utf8");
        const cut = text.lastIndexOf("\n");
        this.rest = cut < 0 ? text : text.slice(cut + 1);
        if (cut >= 0) this.open = rolloutTurnOpen(text.slice(0, cut), this.open);
      }
      return this.open;
    } catch {
      return false;
    } finally {
      if (fd !== null) closeSync(fd);
    }
  }
}

/** sun_path's limit, less its terminating NUL (macOS 104; Linux 108). */
export const SOCKET_PATH_MAX = (process.platform === "darwin" ? 104 : 108) - 1;

/** Whether Codex's /hooks lists no hook at all (0.166's table: "PostToolUse  0  0  …"). */
export function listsNoHooks(screen: string): boolean {
  return /^\s*›?\s*PostToolUse\s+0\s+0\b/m.test(screen) && /^\s*›?\s*UserPromptSubmit\s+0\s+0\b/m.test(screen);
}

/**
 * Trusts the hooks at the review prompt Codex shows at start: 0.166's picker
 * ("2. Trust all and continue", then Enter), else t.
 */
async function trustAtReview(l: Lane, screen: string): Promise<string> {
  const m = /(\d+)\.\s*Trust all and continue/i.exec(screen);
  if (m) {
    await l.tmux.keys(l.session, m[1]);
    await sleep(500);
    await l.tmux.keys(l.session, "Enter");
    await sleep(1500);
    return "trusted the hooks (review prompt: Trust all and continue)";
  }
  await l.tmux.keys(l.session, "t");
  return "trusted the hooks (review prompt)";
}

/**
 * An approval at the bottom of Codex's pane (its last 14 lines): "Would you
 * like to run the following command?", an MCP tool call, an escalation.
 * Wakeshift's form isn't one (elicitForm is checked first).
 */
export function isApproval(t: string): boolean {
  return /Would you like to|Allow .{0,80}(to run|tool)|Approve|needs your approval|Do you want to (allow|approve|run)/i.test(t) && /›\s*1\.|1\.\s*(Yes|Allow)|\(y\)/i.test(t);
}

/** The field Wakeshift's form asks about, and its two answers (internal/harnesses: ElicitSelfRun). */
export const FORM_FIELD = "Notifications in this conversation";
export const FORM_SETUP = /I'?ll set it up/i;
export const FORM_LATER = /Not now/i;

/** Wakeshift's form as Codex's bottom pane shows it: the app it's for, and its options. */
export interface ElicitForm {
  /** The app named in the form's first line ("Wakeshift, for <app>:"). */
  app: string;
  options: DialogOption[];
}

/**
 * Wakeshift's form (an MCP elicitation) at the bottom of Codex's pane, or
 * null: Codex's form ("Field 1/1 …", then the message as plain text, then
 * the field's title and a numbered list, then "enter to submit | esc to
 * cancel"), whose message starts with the line the SDK writes ("Wakeshift,
 * for <app>:") and whose field is "Notifications in this conversation",
 * with "Not now" and "I'll set it up". Only the dialog counts: the
 * "Wakeshift, for" line is looked for between the last line that isn't
 * part of a dialog and the submit line, so the transcript above quoting it
 * isn't a form.
 */
export function elicitForm(screen: string): ElicitForm | null {
  const lines = screen.trimEnd().split("\n");
  let end = -1;
  for (let i = lines.length - 1; i >= Math.max(0, lines.length - 8); i--)
    if (/enter to submit/i.test(lines[i])) {
      end = i;
      break;
    }
  if (end < 0) return null;
  // The message can run to 20 lines (the SDK's bound), wrapped: look back far enough.
  const from = Math.max(0, end - 80);
  let start = -1;
  for (let i = end; i >= from; i--) {
    if (/^\s*Wakeshift, for .{1,80}:\s*$/.test(lines[i])) {
      start = i;
      break;
    }
    // Codex's own lines above a dialog: the transcript ends there.
    if (/^\s*(›\s*Ask Codex|[•■]\s|─{8,})/.test(lines[i]) && !/^\s*›\s*\d\./.test(lines[i])) break;
  }
  if (start < 0) return null;
  const block = lines.slice(start, end + 1);
  const at = block.findIndex((x) => x.trim() === FORM_FIELD);
  if (at < 0) return null;
  const options = numberedOptions(block.slice(at).join("\n"));
  if (!options.some((o) => FORM_SETUP.test(o.label)) || !options.some((o) => FORM_LATER.test(o.label))) return null;
  const app = /Wakeshift, for (.{1,80}):/.exec(block[0])![1].trim();
  return { app, options };
}

/** Where Codex's background server listens, for a HOME. */
export function codexSocket(home: string): string {
  return join(home, ".codex/app-server-control/app-server-control.sock");
}

export class Codex extends TmuxDriver implements Driver {
  id = "codex";
  name = "Codex";
  bin = "codex";
  zeroInstallWake = false;
  installPath = "connector" as const;
  secondMcp = true;
  harness = "codex";
  conversation = "auto" as const;
  // The install note's trust step, or the /hooks one before it.
  trustStep = /resume --last|Trust all and continue|\/hooks/i;
  pluginInstall = { refSep: "@", command: (source: string) => `codex plugin marketplace add ${source} && codex plugin add wakeshift@wakeshift` };

  /**
   * WB_CODEX_PIN=floor (the oldest Codex the core's offers support, from
   * harness-pins.json, installed with bench/releases/install-harness.sh),
   * else WB_CODEX_BIN (e.g. a release unpacked into the bench's state),
   * else codex on PATH.
   */
  binPath(): string | null {
    const pinned = pinnedCodexBin();
    if (pinned) return existsSync(pinned) ? pinned : null;
    const b = process.env.WB_CODEX_BIN;
    if (b) return existsSync(b) ? b : null;
    return super.binPath();
  }

  async version() {
    try {
      const { stdout } = await exec(this.binPath()!, ["--version"], { timeout: 20_000 });
      return stdout.trim().replace(/^codex(-cli)?\s*/i, "");
    } catch {
      return undefined;
    }
  }

  async check(): Promise<Availability> {
    const base = await super.check();
    if (!base.ok) return base;
    if (!existsSync(join(REAL_HOME, ".codex/auth.json"))) return { ok: false, reason: notLoggedInReason(this.name, "no ~/.codex/auth.json") };
    return base;
  }

  async prepare(l: Lane): Promise<void> {
    // Codex's background server listens on a unix socket in CODEX_HOME;
    // past the OS's path limit it can't, Codex runs in-process, and the
    // connector says "not connected" (the bench would grade that as
    // Wakeshift's failure). Refuse a run dir that deep.
    // The limit is in bytes (a non-ASCII path is longer than its length).
    const bytes = Buffer.byteLength(codexSocket(l.home.dir));
    if (bytes > SOCKET_PATH_MAX) throw new Error(`the temp HOME is too deep for Codex's socket (${bytes} > ${SOCKET_PATH_MAX} bytes): use a shorter --out-root`);
    const codex = join(l.home.dir, ".codex");
    mkdirSync(codex, { recursive: true, mode: 0o700 });
    // The login, copied (0600), never symlinked: Codex rewrites auth.json
    // when it refreshes its token, and through a symlink that would write the
    // user's own file (the run's real-config check then fails it).
    copyLogin(join(REAL_HOME, ".codex/auth.json"), join(codex, "auth.json"));
    const model = process.env.WB_CODEX_MODEL;
    writeFileSync(
      join(codex, "config.toml"),
      (model ? `model = "${model}"\n` : "") +
        `check_for_update_on_startup = false\n\n` +
        codexFeaturesToml() +
        "\n" +
        // The work dir as given and as its real path (/tmp is /private/tmp), once each: TOML refuses a key twice.
        [...new Set([l.home.work, `${l.home.real}/work`])].map((p) => `[projects."${p}"]\ntrust_level = "trusted"\n`).join("\n"),
      { mode: 0o600 },
    );
    const run = (args: string[]) => l.run(this.env(l), this.binPath()!, args);
    if (usePicker(l)) await addWithPicker(l, { harness: "codex", program: "codex", run });
    else {
      // The bench's own command (WB_MCP_ADD=bench, or a server with a token).
      const r = await run(mcpAddArgs(l, "codex"));
      l.note(`codex mcp add: ${r.ok ? "ok" : "failed"}`, { out: r.out.slice(-1000) });
      if (!r.ok) throw new Error("codex mcp add failed");
    }
    // S15: the second app's server, added the same way, next to the first.
    if (l.mcp2 && usePicker(l, l.mcp2)) await addWithPicker(l, { harness: "codex", program: "codex", server: l.mcp2, run });
    else if (l.mcp2) {
      const r2 = await l.run(this.env(l), this.binPath()!, mcpAddArgs(l, "codex", l.mcp2));
      l.note(`codex mcp add ${l.mcp2.name}: ${r2.ok ? "ok" : "failed"}`, { out: r2.out.slice(-1000) });
      if (!r2.ok) throw new Error(`codex mcp add ${l.mcp2.name} failed`);
    }
    // WB_CODEX_PLUGIN_PREINSTALLED=1: a user who installed Wakeshift's
    // plugin earlier (from another thread's offer), so the lane's first
    // thread starts with it. The core's own offer command, run before Codex
    // starts; Codex asks to trust its hooks at the first start (start()).
    if (process.env.WB_CODEX_PLUGIN_PREINSTALLED === "1") {
      const src = marketplaceSource(l.ctx.bench, this.pluginInstall.refSep);
      if (!src) throw new Error("WB_CODEX_PLUGIN_PREINSTALLED: this core offers no plugin marketplace (WB_CURL_INSTALL?)");
      const add = await l.run(this.env(l), this.binPath()!, ["plugin", "marketplace", "add", src]);
      const r3 = add.ok ? await l.run(this.env(l), this.binPath()!, ["plugin", "add", "wakeshift@wakeshift"]) : add;
      l.note(`WB_CODEX_PLUGIN_PREINSTALLED: codex plugin add: ${r3.ok ? "ok" : "failed"}`, { out: r3.out.slice(-1000) });
      if (!r3.ok || !existsSync(join(l.home.dir, ".codex/plugins/cache/wakeshift/wakeshift"))) throw new Error("WB_CODEX_PLUGIN_PREINSTALLED: codex plugin add failed");
      l.state.trustPreinstalled = true;
    }
    // WB_WAKESHIFT_OFF=1: Wakeshift turned off on the machine before Codex
    // starts (`wakeshift disable`'s switch file, ~/.wakeshift/disabled), so
    // its hooks, run in Codex's daemon, must do nothing and nothing wakes.
    if (process.env.WB_WAKESHIFT_OFF === "1") {
      mkdirSync(join(l.home.dir, ".wakeshift"), { recursive: true, mode: 0o700 });
      writeFileSync(join(l.home.dir, ".wakeshift", "disabled"), "", { mode: 0o600 });
      l.note("WB_WAKESHIFT_OFF: Wakeshift's switch file is on");
    }
  }

  async start(l: Lane, opts: { resume?: boolean } = {}): Promise<void> {
    await this.openPane(l);
    l.state.panePid = await l.tmux.panePid(l.session);
    await this.shell(l, [this.binPath()!, ...(opts.resume ? ["resume", "--last"] : [])]);
    const ok = await l.waitFor(/Ask Codex|›\s|To get started|context left|for shortcuts/i, 60_000, { allowInstall: false });
    // Its sign-in screen ("› 1. Sign in with ChatGPT") matches the ready text too.
    await this.checkLogin(l, "the borrowed ~/.codex/auth.json didn't sign it in");
    if (ok === null) await this.didntStart(l, "Codex didn't reach its start screen", opts);
    await sleep(3000);
    // The preinstalled plugin's hooks (WB_CODEX_PLUGIN_PREINSTALLED): the
    // user trusts them at the review prompt of the first start, as after
    // an install. The prompt can take a while to show: wait for it (up to
    // 30s), so the user's first message never lands in it.
    if (l.state.trustPreinstalled) {
      const did = await this.trustWhenAsked(l, 30_000);
      l.note(`WB_CODEX_PLUGIN_PREINSTALLED: ${did ?? "no hooks review prompt within 30s"}`);
      l.state.trustPreinstalled = false;
    }
  }

  /**
   * Waits up to ms for Codex's hooks review prompt and trusts the hooks
   * there; null if it never showed. The prompt can follow the composer by
   * a few seconds, so a message typed meanwhile would land in it and leave
   * the hooks untrusted. The picker sometimes takes the Enter as "1. Review hooks" and
   * opens the hooks table ("t trust all · … · esc close"): trusted there,
   * and closed.
   */
  async trustWhenAsked(l: Lane, ms: number): Promise<string | null> {
    const end = Date.now() + ms;
    while (Date.now() < end) {
      const t = tail(await l.screen(60), 14);
      if (/Hooks? need review|hooks? (are|is) new or changed/i.test(t)) {
        let did = await trustAtReview(l, t);
        await sleep(1500);
        if (/t trust all/i.test(tail(await l.screen(60), 14))) {
          await l.tmux.keys(l.session, "t");
          await sleep(1500);
          await l.tmux.keys(l.session, "Escape");
          await sleep(800);
          did = "trusted the hooks in the hooks table (t)";
        }
        return did;
      }
      await sleep(500);
    }
    return null;
  }

  async stop(l: Lane): Promise<void> {
    await l.tmux.keys(l.session, "Escape");
    await sleep(300);
    await l.tmux.type(l.session, "/quit");
    if (!(await this.waitShell(l, 15_000))) {
      await l.tmux.keys(l.session, "C-c");
      await sleep(400);
      await l.tmux.keys(l.session, "C-c");
      await this.waitShell(l, 10_000);
    }
  }

  async answer(l: Lane, screen: string, policy: AnswerPolicy): Promise<string | null> {
    // Wakeshift's form in Codex's own dialog (an MCP elicitation), before
    // the approvals: its message quotes the install commands, which the
    // approval branch below would take for the installer. The user's answer
    // is the policy's (R10: "I'll set it up"), else "Not now".
    const form = elicitForm(screen);
    if (form) {
      const want = policy.form === "setup" ? FORM_SETUP : FORM_LATER;
      const target = form.options.find((o) => want.test(o.label))!;
      const cursor = Math.max(0, form.options.findIndex((o) => o.cursor));
      const to = form.options.indexOf(target);
      for (let i = cursor; i < to; i++) await l.tmux.keys(l.session, "Down");
      for (let i = cursor; i > to; i--) await l.tmux.keys(l.session, "Up");
      await sleep(200);
      await l.tmux.keys(l.session, "Enter");
      l.prompted({ kind: "form", asks: `Wakeshift, for ${form.app}: ${FORM_FIELD}`, options: form.options.map((o) => o.label), chosen: target.label, scope: "n/a", sessionOffered: false });
      return `answered Wakeshift's form: ${target.label}`;
    }
    // Dialogs sit at the bottom, above "enter to submit | esc to cancel" (or
    // the like): look only there; the transcript above can quote anything.
    const t = tail(screen, 14);
    // Approvals: "Would you like to run the following command?", MCP tool calls, escalation.
    if (isApproval(t)) {
      if (INSTALLER.test(t) && !policy.allowInstall) {
        await l.tmux.keys(l.session, "Escape");
        return "denied the installer (the user said no)";
      }
      // By default "Always allow" / "don't ask again" where offered; under
      // scope "session", the session's option, else yes once.
      const opts = numberedOptions(t);
      const c = chooseOption(opts, policy) ?? { opt: { n: 1, label: "Yes", cursor: true }, scope: "once" as const, forcedAlways: false };
      await l.tmux.keys(l.session, String(c.opt.n ?? 1));
      await sleep(200);
      if (/›\s*\d\./.test(tail(await l.screen(30), 30)) && /Would you like to|Allow/i.test(tail(await l.screen(30), 30))) await l.tmux.keys(l.session, "Enter");
      l.prompted({ kind: "approval", asks: dialogAsks(t, opts[0]?.label), options: opts.map((o) => o.label), chosen: c.opt.label, scope: c.scope, sessionOffered: opts.some((o) => scopeOf(o.label) === "session"), forcedAlways: c.forcedAlways });
      return `approved ${INSTALLER.test(t) ? "the installer" : /wakebench|agentchat|MCP|tool/i.test(t) ? "an MCP tool" : "a command"}${c.scope === "once" ? "" : c.scope === "session" ? " (this session)" : " (don't ask again)"}`;
    }
    // The hook review prompt at start ("Hooks need review"): trust them only after an install the user agreed to.
    if (/Hooks? need review|hooks? (are|is) new or changed/i.test(t) && policy.allowInstall && /Trust/i.test(t) && /›/.test(t)) {
      return await trustAtReview(l, t);
    }
    return null;
  }

  override busy(screen: string): boolean {
    return /esc to interrupt|Working \(|Esc to interrupt/i.test(tail(screen, 12));
  }

  /**
   * Whether the thread's rollout has a turn that started and hasn't ended
   * (RolloutTurns). Codex (0.166 and 0.167 alike) shows no "Working" line
   * while an answer streams, and draws a line only once it ends: an answer
   * quoting the watch result's 2,000-character paragraph word for word left
   * the screen still for over six seconds mid-turn, so the bench sent its
   * next message into the running turn, before the model got to ask
   * (S6 then failed every run). A turn that
   * never ends holds Lane.waitIdle only until its maxMs; stop() presses
   * Escape (turn_aborted) before /quit.
   */
  turnOpen(l: Lane): boolean {
    let t = l.state.rolloutTurns as RolloutTurns | undefined;
    if (!t) l.state.rolloutTurns = t = new RolloutTurns(join(l.home.dir, ".codex/sessions"));
    return t.turnOpen();
  }

  /**
   * Codex's trust step, as the install note says: quit, continue the thread
   * (`codex resume --last`), and trust the hooks at the review prompt Codex
   * shows at start. WB_CODEX_TRUST=hooks instead types /hooks and presses t
   * (the earlier step, which works up to Codex 0.165); 0.166 and later
   * list a running thread's own hooks there, loaded when it started, so a
   * hook installed since isn't listed, and the driver restarts after all.
   */
  async afterInstall(l: Lane): Promise<string[]> {
    const done: string[] = [];
    await l.waitIdle({ allowInstall: true }, 60_000);
    let s = "";
    if (process.env.WB_CODEX_TRUST === "hooks") {
      await l.tmux.type(l.session, "/hooks");
      await sleep(2500);
      await l.snap("/hooks");
      s = await l.screen(60);
    }
    if (process.env.WB_CODEX_TRUST !== "hooks" || listsNoHooks(tail(s, 40))) {
      if (s) {
        await l.tmux.keys(l.session, "Escape");
        await sleep(800);
      }
      await this.stop(l);
      await this.start(l, { resume: true });
      l.state.panePid = await l.tmux.panePid(l.session);
      done.push(s ? "/hooks listed no hooks (Codex 0.166 loads a thread's hooks when it starts): restarted Codex (resume --last)" : "restarted Codex (resume --last), as the install note says");
      // The review prompt as asked, before it's answered (this isn't a dialog Lane answers, so it keeps no snapshot of its own).
      // It can follow the composer by a few seconds: waited for (trustWhenAsked).
      for (let i = 0; i < 20 && !/Hooks? need review|hooks? (are|is) new or changed/i.test(tail(await l.screen(60), 14)); i++) await sleep(500);
      await l.snap("after the restart: the hooks review prompt");
      done.push((await this.trustWhenAsked(l, 20_000)) ?? "no review prompt after the restart");
      await sleep(1500);
      await l.snap("after the restart's review");
      l.state.trustAgainOnRestart = false;
      return done;
    }
    if (/review|untrusted|needs? (review|trust)|press t|\bt\b.*trust/i.test(tail(s, 40))) {
      await l.tmux.keys(l.session, "t");
      await sleep(1500);
      done.push("trusted the hooks in /hooks (t)");
    } else {
      await l.tmux.keys(l.session, "t");
      await sleep(1500);
      done.push("pressed t in /hooks (no review prompt recognized)");
    }
    await l.snap("after t");
    await l.tmux.keys(l.session, "Escape");
    await sleep(800);
    l.state.trustAgainOnRestart = false;
    return done;
  }

  waiters(l: Lane, rows: PsRow[]): PsRow[] {
    // The per-thread connector (wakeshift run --thread), started detached by the hook.
    return this.mine(l, rows).filter((r) => /wakeshift(-codex)? run\b|\/v1\/stream/.test(r.command));
  }

  /** What reached the model, from the rollout files in the temp HOME (parseRollout); null if there are none. */
  async received(l: Lane): Promise<Received[] | null> {
    const files = rolloutFiles(join(l.home.dir, ".codex/sessions"));
    const out: Received[] = [];
    let read = 0;
    for (const f of files) {
      try {
        out.push(...parseRollout(readFileSync(f, "utf8").split("\n"), f));
        read++;
      } catch {
        // Gone.
      }
    }
    return read ? out : null;
  }

  /** Woken turns since a time (ms), from the rollout files in the temp HOME (parseRolloutTurns). */
  turns(l: Lane, sinceMs: number): Turn[] {
    const out: Turn[] = [];
    for (const f of rolloutFiles(join(l.home.dir, ".codex/sessions"))) {
      try {
        out.push(...parseRolloutTurns(readFileSync(f, "utf8").split("\n")));
      } catch {
        // Gone.
      }
    }
    return out.filter((t) => t.queued >= sinceMs).sort((a, b) => a.queued - b.queued);
  }

  /** The MCP servers the lane's threads had in reach, and any beyond the lane's own (codexToolServers). */
  toolServers(l: Lane): { servers: string[]; unexpected: string[]; listed: boolean } {
    return codexToolServers(l.home.dir, [l.mcpName, ...(l.mcp2 ? [l.mcp2.name] : [])]);
  }

  installed(l: Lane): string[] {
    return this.mentions(l, [".codex/hooks.json", ".codex/plugins/cache/wakeshift/wakeshift", ".wakeshift/codex", ".wakeshift/bin/wakeshift"]);
  }

  /** Keeps the connectors' logs (what each delivered, its reconnects, why it stopped; no links or content) with the run's logs. */
  async cleanup(l: Lane): Promise<void> {
    const dir = join(l.home.dir, ".wakeshift/codex/logs");
    if (!existsSync(dir)) return;
    const files = readdirSync(dir).filter((f) => f.endsWith(".log")).sort();
    if (files.length === 0) return;
    const text = files.map((f) => `== ${f}\n${readFileSync(join(dir, f), "utf8")}`).join("\n");
    writeFileSync(join(l.ctx.dir, "logs", `${l.id}.connector.log`), redactPlain(text), { mode: 0o600 });
    l.note("kept the connector's log", { file: `logs/${l.id}.connector.log` });
  }

  async uninstall(l: Lane): Promise<void> {
    // The uninstaller removes ~/.wakeshift/codex, logs and all: keep them first.
    await this.cleanup(l).catch(() => {});
    if (existsSync(join(l.home.dir, ".codex/plugins/cache/wakeshift/wakeshift"))) {
      const r = await l.run(this.env(l), this.binPath()!, ["plugin", "remove", "wakeshift@wakeshift"]);
      l.note(`codex plugin remove wakeshift@wakeshift: ${r.ok ? "ok" : "failed"}`, { out: r.out.slice(-1000) });
      const m = await l.run(this.env(l), this.binPath()!, ["plugin", "marketplace", "remove", "wakeshift"]);
      l.note(`codex plugin marketplace remove wakeshift: ${m.ok ? "ok" : "failed"}`, { out: m.out.slice(-1000) });
    }
    await this.uninstallHarness(l, "codex");
  }
}

/** Every rollout file under Codex's sessions directory (sessions/YYYY/MM/DD). */
function rolloutFiles(root: string): string[] {
  const files: string[] = [];
  const walk = (d: string, depth: number) => {
    if (depth > 4) return;
    let es: import("node:fs").Dirent[];
    try {
      es = readdirSync(d, { withFileTypes: true });
    } catch {
      return;
    }
    for (const e of es) {
      if (e.isDirectory()) walk(join(d, e.name), depth + 1);
      else if (e.isFile() && /^rollout-.*\.jsonl$/.test(e.name)) files.push(join(d, e.name));
    }
  };
  walk(root, 0);
  return files;
}

/**
 * Woken turns in a Codex rollout (its JSONL lines): each connector delivery
 * (a wakeshift_event output, the woken turn's first item) is when the event
 * reached Codex (queued and started: the connector starts the turn with it),
 * the model's next item (reasoning, a message or a tool call) its first
 * output, and the turn's task_complete its end. S2i's bar reads Wakeshift's
 * part as ending at the delivery; the rest is Codex and the model.
 */
export function parseRolloutTurns(lines: string[]): Turn[] {
  type Line = { type?: string; timestamp?: string; payload?: { type?: string; name?: string; role?: string; output?: unknown } };
  const text = (c: unknown): string =>
    typeof c === "string" ? c : Array.isArray(c) ? c.map((x: { text?: unknown } | string) => (typeof x === "string" ? x : typeof x?.text === "string" ? x.text : "")).join("\n") : "";
  const es: { at: number; o: Line }[] = [];
  for (const s of lines) {
    if (!s.trim()) continue;
    try {
      const o = JSON.parse(s) as Line;
      const at = o.timestamp ? Date.parse(o.timestamp) : NaN;
      if (Number.isFinite(at)) es.push({ at, o });
    } catch {
      // A line being written.
    }
  }
  const delivery = (o: Line) => o.type === "response_item" && (o.payload?.type === "function_call_output" || o.payload?.type === "custom_tool_call_output") && o.payload?.name === "wakeshift_event";
  const out: Turn[] = [];
  es.forEach(({ at, o }, i) => {
    if (!delivery(o)) return;
    let first: { at: number; kind: string } | null = null;
    let ended: number | null = null;
    for (const { at: t, o: x } of es.slice(i + 1)) {
      // The next delivery starts another turn.
      if (delivery(x)) break;
      const q = x.payload;
      if (!first && x.type === "response_item" && q && (q.type === "reasoning" || (q.type === "message" && q.role === "assistant") || q.type === "function_call" || q.type === "custom_tool_call")) first = { at: t, kind: q.type ?? "" };
      if (x.type === "event_msg" && q?.type === "task_complete") {
        ended = t;
        break;
      }
    }
    out.push({ source: "channel", text: text(o.payload?.output), queued: at, started: at, first: first?.at ?? null, firstKind: first?.kind ?? null, ended, outputTokens: 0, inputTokens: 0, model: null });
  });
  return out;
}

/**
 * What reached the model in a Codex rollout file (its JSONL lines), from its
 * response items, the conversation as the model gets it (the event_msg lines
 * repeat them for the UI): user and developer messages, the connector's
 * wakeshift_event outputs (a woken turn's first item), and the outputs of
 * MCP tool calls (watch, made directly or from code mode's exec: an inbox
 * result). Not: the model's own output or other tools' outputs (a shell
 * command, or an exec that reads back a stored result, isn't a delivery).
 * Keyed by the item's ID, so a resumed rollout's copy counts once.
 */
export function parseRollout(lines: string[], file = ""): Received[] {
  type Payload = { type?: string; id?: string; role?: string; name?: string; call_id?: string; input?: unknown; arguments?: unknown; output?: unknown; content?: unknown };
  const text = (c: unknown): string =>
    typeof c === "string" ? c : Array.isArray(c) ? c.map((x: { text?: unknown } | string) => (typeof x === "string" ? x : typeof x?.text === "string" ? x.text : "")).join("\n") : "";
  const items: { at: number | null; p: Payload; i: number }[] = [];
  lines.forEach((s, i) => {
    if (!s.trim()) return;
    try {
      const o = JSON.parse(s) as { type?: string; timestamp?: string; payload?: Payload };
      if (o.type === "response_item" && o.payload) items.push({ at: o.timestamp ? Date.parse(o.timestamp) : null, p: o.payload, i });
    } catch {
      // A line being written.
    }
  });
  // The model's tool calls by call ID: whether each called an MCP tool.
  const mcp = new Map<string, boolean>();
  for (const { p } of items)
    if ((p.type === "function_call" || p.type === "custom_tool_call") && p.call_id) {
      const what = `${p.name ?? ""} ${typeof p.input === "string" ? p.input : ""} ${typeof p.arguments === "string" ? p.arguments : ""}`;
      mcp.set(p.call_id, /^mcp__/.test(p.name ?? "") || /\btools\.mcp__/.test(what));
    }
  const out: Received[] = [];
  for (const { at, p, i } of items) {
    const key = p.id ?? `${file}:${i}`;
    if (p.type === "message" && (p.role === "user" || p.role === "developer")) out.push({ key, at, via: p.role, text: text(p.content) });
    else if (p.type === "function_call_output" || p.type === "custom_tool_call_output") {
      if (p.name === "wakeshift_event") out.push({ key, at, via: "wakeshift_event", text: text(p.output) });
      else if (p.call_id && mcp.get(p.call_id)) out.push({ key, at, via: "tool_result:mcp", text: text(p.output) });
    }
  }
  return out;
}

/**
 * WB_CODEX_PIN=floor: the oldest Codex the core's offers support
 * (bench/harness-pins.json's "codex-floor"), as
 * bench/releases/install-harness.sh puts it in the bench's state. Null
 * without the pin; the path (which may not exist yet) with it.
 */
export function pinnedCodexBin(env: NodeJS.ProcessEnv = process.env, pinsFile = fileURLToPath(new URL("../../../harness-pins.json", import.meta.url))): string | null {
  if (env.WB_CODEX_PIN !== "floor") return null;
  const v = readJsonIf<Record<string, string>>(pinsFile)?.["codex-floor"];
  if (!v || !/^[0-9][0-9A-Za-z.+-]*$/.test(v)) throw new Error(`WB_CODEX_PIN=floor: no "codex-floor" version in ${pinsFile}`);
  return join(stateRoot(env), "codex", v, "bin", "codex");
}

/**
 * Copies a login file into a temp HOME, 0600: a plain copy, never a link, so
 * nothing the harness writes there reaches the user's own file. A file
 * already at dst (a link included) is an error, never followed.
 */
export function copyLogin(src: string, dst: string): void {
  if (!statSync(src).isFile()) throw new Error(`${src} isn't a file`);
  copyFileSync(src, dst, fsConstants.COPYFILE_EXCL);
  chmodSync(dst, 0o600);
  if (lstatSync(dst).isSymbolicLink()) throw new Error(`${dst} is a link`);
}
