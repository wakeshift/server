// GitHub Copilot CLI (copilot), in a temp HOME. Login: the gh login in the
// login keychain (symlinked). The account may not be allowed to use Copilot
// CLI ("Access denied by policy settings"): then every scenario is blocked.
// Zero install: the agent runs the session's wait with its bash tool in
// async mode (shellId wakeshift-wait); Copilot wakes an idle session when
// that command exits. Install (optional): Wakeshift's plugin from the
// marketplace the core offers (`copilot plugin marketplace add` and `copilot
// plugin install`; a local core's own marketplace, Bench.agentPlugins), or
// with WB_CURL_INSTALL=1 the installer's ~/.copilot/hooks/wakeshift.json;
// either way hooks for agentStop, postToolUse and preMcpToolCall, loaded
// when copilot starts.
//
// No self-updates: Copilot's own updater downloads newer CLI versions into
// its package cache, and an install can replace the binary it runs from,
// outside the temp HOME. Every run gets COPILOT_AUTO_UPDATE=false (no CLI
// download, no plugin updates at session start), the interactive session
// also --no-auto-update, and COPILOT_CACHE_HOME in the temp HOME. Copilot
// then runs the version built into the installed binary, which is what
// version() reports (--binary-version, which writes nothing).

import { execFile } from "node:child_process";
import { readFileSync, realpathSync } from "node:fs";
import { join } from "node:path";
import { promisify } from "node:util";
import { marketplaceSource } from "../../src/core.js";
import { borrowKeychain } from "../../src/home.js";
import type { Lane } from "../../src/lane.js";
import type { PsRow } from "../../src/procs.js";
import { sleep } from "../../src/tmux.js";
import { addWithPicker, BlockedError, chooseOption, type DialogOption, dialogAsks, INSTALLER, numberedOptions, scopeOf, selectInMenu, tail, TmuxDriver, usePicker, withShellParents } from "../base.js";
import type { AnswerPolicy, Driver } from "../types.js";

const exec = promisify(execFile);
const POLICY = /Access denied by policy|not authorized to use this Copilot feature/i;
// The prompt's footer once Copilot is ready for input: up to 1.0.x
// "Type @ to mention…" / "shift+tab"; 1.0.101 "/ commands · ? help · tab next tab".
export const READY = /Type @ to mention|Enter @|shift\+tab|\/help|\/ commands|tab next tab/i;
// Copilot's status line offers "esc esc interrupt" while a turn runs
// ("◉ Working · 41 B esc esc interrupt" in 1.0.10x; older versions said
// "esc to interrupt"), and also once the turn is over while the async wait
// runs ("● Waiting for background shells · … esc esc interrupt"): that's
// idle. Since 1.0.105 the model's thinking isn't streamed into the
// transcript, so the status line is all that shows a turn running.
export const BUSY = /\besc (esc|to) interrupt\b/i;
const WAITING = /Waiting for background shells/i;

/** Copilot's updater and package cache, kept off and in the temp HOME (see the top). */
export function copilotNoUpdateEnv(home: string): Record<string, string> {
  return { COPILOT_AUTO_UPDATE: "false", COPILOT_CACHE_HOME: join(home, "Library/Caches/copilot") };
}

/** The interactive session's arguments: no self-update, --continue to resume, then any deny rule. */
export function copilotStartArgs(opts: { resume?: boolean }, deny: string[] = []): string[] {
  return ["--no-auto-update", ...(opts.resume ? ["--continue"] : []), ...deny];
}

/**
 * Copilot CLI's ask_user picker about the install (1.0.105):
 *
 *   Copilot needs information.
 *   Install Wakeshift's plugin for GitHub Copilot CLI? It runs `…`.
 *   Install plugin?:
 *   ❯ Yes
 *     No
 *     Other (type your answer)
 *   ↑/↓ select · enter accept · ctrl+d decline · esc cancel
 *
 * The options are the lines between the field's label (ending ":") and the
 * key hint; null unless it's about the install and has a yes and a no.
 */
export function copilotInstallQuestion(t: string): { options: DialogOption[]; yes: DialogOption; no: DialogOption } | null {
  if (!/needs information/i.test(t)) return null;
  const lines = t.split("\n");
  const hint = lines.findLastIndex((s) => /↑\/↓ select.*enter accept/i.test(s));
  const asked = lines.findLastIndex((s) => /needs information/i.test(s));
  if (hint < 0 || asked < 0 || asked > hint) return null;
  // The live picker sits at the bottom: nothing but its border after the hint.
  if (lines.slice(hint + 1).some((s) => /[^\s─━]/.test(s))) return null;
  const asks = lines.slice(asked, hint).join("\n");
  if (!/wakeshift/i.test(asks) || !/install/i.test(asks)) return null; // Wakeshift's install, nothing else
  const options: DialogOption[] = [];
  for (let i = hint - 1; i > asked; i--) {
    const s = lines[i].replace(/[│┃]/g, "").trimEnd();
    if (/:\s*$/.test(s)) break; // the field's label
    const m = /^\s*(❯)?\s*(\S.*)$/.exec(s);
    if (!m) continue;
    options.unshift({ label: m[2].trim(), cursor: m[1] === "❯" });
  }
  const no = options.find((o) => /^(no|don't|do not|skip|not now)\b/i.test(o.label));
  const yes = options.find((o) => o !== no && !/^other\b/i.test(o.label) && (/^(yes|true)\b/i.test(o.label) || /\binstall/i.test(o.label)));
  return yes && no ? { options, yes, no } : null;
}

export class Copilot extends TmuxDriver implements Driver {
  id = "copilot";
  name = "Copilot CLI";
  bin = "copilot";
  zeroInstallWake = true;
  installPath = "hooks" as const;
  harness = "copilot";
  conversation = "none" as const;
  trustStep = /restart/i;
  pluginInstall = { refSep: "#", command: (source: string) => `copilot plugin marketplace add ${source} && copilot plugin install wakeshift@wakeshift` };

  /**
   * The version built into the binary (--binary-version), which the lanes
   * run with updates off. Not --version: that extracts the bundled package
   * into the package cache, and with the bench's own environment that was
   * the user's real ~/Library/Caches/copilot. No HOME, no updates.
   */
  async version() {
    try {
      const env = { PATH: process.env.PATH ?? "", HOME: "/nonexistent", ...copilotNoUpdateEnv("/nonexistent") };
      const { stdout } = await exec(this.binPath()!, ["--binary-version"], { timeout: 20_000, env });
      return /(\d+\.\d+\.\d+)/.exec(stdout)?.[1] ?? stdout.trim().split("\n")[0];
    } catch {
      return undefined;
    }
  }

  /** The lane's environment, with Copilot's self-update off and its package cache in the temp HOME. */
  override env(l: Lane, extra: Record<string, string> = {}): Record<string, string> {
    return super.env(l, { ...copilotNoUpdateEnv(l.home.dir), ...extra });
  }

  async prepare(l: Lane): Promise<void> {
    borrowKeychain(l.home);
    if (usePicker(l)) await addWithPicker(l, { harness: "copilot", program: "copilot", run: (args) => l.run(this.env(l), this.binPath()!, args) });
    else {
      // The bench's own command (WB_MCP_ADD=bench).
      const r = await l.run(this.env(l), this.binPath()!, ["mcp", "add", "--transport", "http", l.mcpName, l.mcpUrl]);
      l.note(`copilot mcp add: ${r.ok ? "ok" : "failed"}`, { out: r.out.slice(-600) });
    }
    // WB_COPILOT_PLUGIN_PREINSTALLED=1: a user who installed Wakeshift's
    // plugin earlier (from another session's offer), so the lane's first
    // session starts with it: the core's own offer commands, run before
    // copilot starts (plugin hooks need no trust).
    if (process.env.WB_COPILOT_PLUGIN_PREINSTALLED === "1") {
      const src = marketplaceSource(l.ctx.bench, this.pluginInstall.refSep);
      if (!src) throw new Error("WB_COPILOT_PLUGIN_PREINSTALLED: this core offers no plugin marketplace (WB_CURL_INSTALL?)");
      const add = await l.run(this.env(l), this.binPath()!, ["plugin", "marketplace", "add", src]);
      const r2 = add.ok ? await l.run(this.env(l), this.binPath()!, ["plugin", "install", "wakeshift@wakeshift"]) : add;
      l.note(`WB_COPILOT_PLUGIN_PREINSTALLED: copilot plugin install: ${r2.ok ? "ok" : "failed"}`, { out: r2.out.slice(-1000) });
      if (!r2.ok) throw new Error("WB_COPILOT_PLUGIN_PREINSTALLED: copilot plugin install failed");
    }
  }

  async start(l: Lane, opts: { resume?: boolean } = {}): Promise<void> {
    await this.openPane(l);
    l.state.panePid = await l.tmux.panePid(l.session);
    await this.shell(l, [this.binPath()!, ...copilotStartArgs(opts, this.denyArgs(l))]);
    // Ready is the prompt's own footer, not the trust dialog: waitFor answers
    // that dialog (answer()), and a prompt typed while it's up is lost.
    const ok = await l.waitFor(READY, 60_000, { allowInstall: false });
    // The account check (the model catalog) answers a few seconds after the UI is up.
    await sleep(8000);
    const s = await l.screen();
    if (POLICY.test(s)) throw new BlockedError("Copilot CLI: access denied by the account's policy (needs Copilot CLI enabled for this account)");
    // Whether or not the ready text matched: a login screen can carry it too.
    await this.checkLogin(l, "the keychain's gh login didn't carry over");
    if (ok === null) await this.didntStart(l, "copilot didn't reach its start screen", opts);
    await sleep(2000);
  }

  /**
   * WB_COPILOT_DENY_WAIT=1: copilot starts with a --deny-tool rule for
   * Wakeshift's wait command, so the plugin's permissionRequest hook must
   * step aside (Copilot gives a hook's allow precedence over the rule).
   */
  private denyArgs(l: Lane): string[] {
    if (process.env.WB_COPILOT_DENY_WAIT !== "1") return [];
    const rule = `shell(${join(realpathSync(l.home.dir), ".wakeshift/bin/wakeshift")} wait)`;
    l.note(`copilot runs with --deny-tool for the wait command (WB_COPILOT_DENY_WAIT)`);
    return ["--deny-tool", rule];
  }

  async stop(l: Lane): Promise<void> {
    await l.tmux.keys(l.session, "Escape");
    await sleep(300);
    await l.tmux.type(l.session, "/exit");
    if (!(await this.waitShell(l, 10_000))) {
      await l.tmux.keys(l.session, "C-c");
      await sleep(500);
      await l.tmux.keys(l.session, "C-c");
      await this.waitShell(l, 10_000);
    }
  }

  override busy(screen: string): boolean {
    return tail(screen, 6)
      .split("\n")
      .some((line) => BUSY.test(line) && !WAITING.test(line));
  }

  async answer(l: Lane, screen: string, policy: AnswerPolicy): Promise<string | null> {
    if (POLICY.test(tail(screen, 40))) throw new BlockedError("Copilot CLI: access denied by the account's policy");
    // The agent's own question about the install (ask_user: "Copilot needs
    // information", options under the cursor, the first one selected),
    // answered as the user would (policy.pick, default no), as Claude
    // Code's AskUserQuestion is. Unanswered, the bench's typed "No" went
    // into the picker and Enter accepted its selected "Yes".
    const q = copilotInstallQuestion(tail(screen, 24));
    if (q) {
      const yes = policy.pick === "yes";
      await selectInMenu(l, q.options, yes ? q.yes : q.no);
      l.state.picked = yes ? "yes" : "no";
      l.state.pickedAt = Date.now();
      l.prompted({ kind: "question", asks: "the agent's question about the install", options: q.options.map((o) => o.label), chosen: (yes ? q.yes : q.no).label, scope: "n/a", sessionOffered: false });
      return `picked "${yes ? "yes, install" : "no"}" in Copilot's question`;
    }
    const t = tail(screen, 14);
    if (/Do you trust the files in this folder/i.test(t)) {
      await l.tmux.keys(l.session, "1");
      return "trusted the folder";
    }
    // "Do you want to create …?": the conversation's link file, which the
    // agent writes with its create tool (harnesses.LinkStep).
    if (/Allow|Do you want to (run|use|create|edit)/i.test(t) && /1\.\s*Yes/i.test(t)) {
      if (INSTALLER.test(t) && !policy.allowInstall && l.state.picked !== "yes") {
        await l.tmux.keys(l.session, "Escape");
        return "denied the installer (the user said no)";
      }
      // Copilot's "Yes, and approve <tool> for the rest of the running session" is session-scoped.
      const opts = numberedOptions(t);
      const c = chooseOption(opts, policy) ?? { opt: { n: 1, label: "Yes", cursor: true }, scope: "once" as const, forcedAlways: false };
      await l.tmux.keys(l.session, String(c.opt.n ?? 1));
      l.prompted({ kind: "approval", asks: dialogAsks(t, opts[0]?.label), options: opts.map((o) => o.label), chosen: c.opt.label, scope: c.scope, sessionOffered: opts.some((o) => scopeOf(o.label) === "session"), forcedAlways: c.forcedAlways });
      const what = INSTALLER.test(t) ? "the installer" : /Do you want to (create|edit)/i.test(t) ? "a file" : /wakeshift-wait|wakeshift wait|stream\/wait|curl/.test(t) ? "the background wait" : "a tool";
      return `approved ${what}${c.scope === "once" ? "" : ` (${c.scope})`}`;
    }
    return null;
  }

  async afterInstall(l: Lane): Promise<string[]> {
    await l.waitIdle({ allowInstall: true }, 60_000);
    await this.stop(l);
    await this.start(l, { resume: true });
    return ["restarted copilot (--continue) to load the hooks"];
  }

  waiters(l: Lane, rows: PsRow[]): PsRow[] {
    const mine = this.mine(l, rows);
    return withShellParents(l, mine, mine.filter((r) => /hook --harness copilot|\/v1\/stream|\.wakeshift\/bin\/wakeshift wait/.test(r.command)));
  }

  installed(l: Lane): string[] {
    const found = this.mentions(l, [join(".copilot/hooks/wakeshift.json"), ".wakeshift/copilot", ".wakeshift/bin/wakeshift"]);
    // The plugin, while Copilot's settings have it enabled (uninstalling it
    // leaves it there, disabled).
    if (/"wakeshift@wakeshift"\s*:\s*true/.test(this.copilotSettings(l))) found.push(".copilot/settings.json");
    return found;
  }

  private copilotSettings(l: Lane): string {
    try {
      return readFileSync(join(l.home.dir, ".copilot/settings.json"), "utf8");
    } catch {
      return "";
    }
  }

  async uninstall(l: Lane): Promise<void> {
    // The plugin, if the agent installed it (Copilot's settings name it).
    if (/wakeshift@wakeshift/.test(this.copilotSettings(l))) {
      for (const args of [["plugin", "uninstall", "wakeshift"], ["plugin", "marketplace", "remove", "wakeshift"]]) {
        const r = await l.run(this.env(l), this.binPath()!, args);
        l.note(`copilot ${args.join(" ")}: ${r.ok ? "ok" : "failed"}`, { out: r.out.slice(-600) });
      }
    }
    await this.uninstallHarness(l, "copilot");
  }
}
