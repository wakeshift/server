// Antigravity CLI (agy), in a temp HOME. Login: the login keychain
// (symlinked). Zero install: the agent runs the one-shot wait in the
// background and re-runs it after each wake. Install: hooks under the
// wakeshift key of ~/.gemini/config/hooks.json (WB_CURL_INSTALL=1), or
// Wakeshift's plugin in ~/.gemini/config/plugins/wakeshift, from `agy plugin
// install` (Bench.antigravityPlugin, a checkout rendered for a local core;
// the hosted core's own otherwise); either loads when agy starts. Every run
// has AGY_CLI_DISABLE_AUTO_UPDATE=1: agy's updater replaces the installed
// binary, outside the temp HOME (1.5.5 to 1.5.8 from a version probe).

import { execFile } from "node:child_process";
import { promisify } from "node:util";
import { existsSync, readFileSync } from "node:fs";
import { join } from "node:path";
import { borrowKeychain, REAL_HOME, writeJson } from "../../src/home.js";
import { antigravitySource } from "../../src/core.js";
import type { Lane } from "../../src/lane.js";
import type { PsRow } from "../../src/procs.js";
import { sleep } from "../../src/tmux.js";
import { addWithPicker, BlockedError, chooseOption, dialogAsks, INSTALLER, loginPrompt, menuOptions, NotLoggedInError, scopeOf, selectInMenu, tail, TmuxDriver, usePicker, withShellParents } from "../base.js";
import type { AnswerPolicy, Driver } from "../types.js";

const exec = promisify(execFile);

/** Where agy records that the first-run screens (terms, data use) were accepted. */
export const ONBOARDING = ".gemini/antigravity-cli/cache/onboarding.json";

/** Only the onboarding booleans from agy's onboarding.json, whatever else it holds. */
export function onboardingOnly(v: unknown): Record<string, boolean> {
  if (!v || typeof v !== "object" || Array.isArray(v)) return {};
  return Object.fromEntries(
    Object.entries(v).filter((e): e is [string, boolean] => /^(consumer|enterprise)?[Oo]nboardingComplete$/.test(e[0]) && typeof e[1] === "boolean"),
  );
}

/** agy's self-update, off for every run the bench makes. */
export const AGY_NO_UPDATE = { AGY_CLI_DISABLE_AUTO_UPDATE: "1" };

export class Antigravity extends TmuxDriver implements Driver {
  id = "antigravity";
  name = "Antigravity CLI";
  bin = "agy";
  zeroInstallWake = true;
  installPath = "hooks" as const;
  harness = "antigravity";
  conversation = "auto" as const;
  trustStep = /restart|\/hooks/i;
  override versionArgs = ["changelog"];

  /** The plugin command the core offers, for S6's fallback. */
  pluginInstall = { refSep: "/tree/", command: (source: string) => `agy plugin install ${source}`, source: antigravitySource };

  async version() {
    try {
      const { stdout } = await exec(this.binPath()!, ["changelog"], { timeout: 20_000, env: { ...process.env, ...AGY_NO_UPDATE } });
      return /\b(\d+\.\d+\.\d+)\b/.exec(stdout)?.[1];
    } catch {
      return undefined;
    }
  }

  /** The lane's environment, with agy's self-update off. */
  override env(l: Lane, extra: Record<string, string> = {}): Record<string, string> {
    return super.env(l, { ...AGY_NO_UPDATE, ...extra });
  }

  async prepare(l: Lane): Promise<void> {
    borrowKeychain(l.home);
    // The user's own acceptance of agy's first-run screens (terms and data
    // use, onboarding): agy keeps it in cache/onboarding.json, three
    // booleans. Copied (the real file is only read; the run's safety check
    // hashes it) so the bench never accepts anything itself. Nothing else
    // from the profile comes along: no credentials, no conversations.
    const src = join(REAL_HOME, ONBOARDING);
    if (existsSync(src)) {
      const only = onboardingOnly(JSON.parse(readFileSync(src, "utf8")));
      writeJson(join(l.home.dir, ONBOARDING), only);
      l.note("copied the user's agy onboarding acceptance (cache/onboarding.json, booleans only)", only);
    }
    if (usePicker(l)) {
      await addWithPicker(l, { harness: "antigravity", program: "agy", run: (args) => l.run(this.env(l), this.binPath()!, args) });
      return;
    }
    // The bench's own command (WB_MCP_ADD=bench).
    const r = await l.run(this.env(l), this.binPath()!, ["mcp", "add", "--type", "http", l.mcpName, l.mcpUrl]);
    l.note(`agy mcp add: ${r.ok ? "ok" : "failed"}`, { out: r.out.slice(-600) });
    if (!r.ok) throw new Error("agy mcp add failed");
  }

  async start(l: Lane, opts: { resume?: boolean } = {}): Promise<void> {
    await this.openPane(l);
    l.state.panePid = await l.tmux.panePid(l.session);
    await this.shell(l, [this.binPath()!, ...(opts.resume ? ["--continue"] : [])]);
    // A fresh HOME gets agy's first-run screens (color scheme, …): take the defaults.
    const end = Date.now() + 120_000;
    for (;;) {
      if (Date.now() > end) await this.didntStart(l, "agy didn't reach its start screen", opts);
      const s = tail(await l.screen(80), 30);
      // Consent is the user's to give, not the bench's.
      if (/Terms of Service/i.test(s) && /agree/i.test(s))
        throw new BlockedError("agy's first-run screen in a fresh HOME asks to accept its Terms of Service and data use; the bench doesn't accept terms for the user");
      if (loginPrompt(this.id, s)) throw new NotLoggedInError(this.name, "the keychain borrow doesn't cover agy's login");
      if (/Choose your|\[Next\]|enter Confirm|Welcome to Antigravity|Press Enter to continue/i.test(s)) {
        await l.tmux.keys(l.session, "Enter");
        l.note("agy first-run screen: took the default");
        await sleep(1500);
        continue;
      }
      if (/Type your message|Ask anything|\? for shortcuts|shift\+tab|^\s*>\s*$/im.test(s)) break;
      await sleep(500);
    }
    await sleep(3000);
  }

  async stop(l: Lane): Promise<void> {
    await l.tmux.keys(l.session, "Escape");
    await sleep(300);
    await l.tmux.type(l.session, "/quit");
    if (!(await this.waitShell(l, 10_000))) {
      await l.tmux.keys(l.session, "C-c");
      await sleep(500);
      await l.tmux.keys(l.session, "C-c");
      await this.waitShell(l, 10_000);
    }
  }

  async answer(l: Lane, screen: string, policy: AnswerPolicy): Promise<string | null> {
    const t = tail(screen, 14);
    if (/(Allow|Approve|Run) (this )?(command|tool|once)|Always allow|Deny/i.test(t) && /Allow|Approve|Yes/i.test(t)) {
      if (INSTALLER.test(t) && !policy.allowInstall) {
        await l.tmux.keys(l.session, "Escape");
        return "denied the installer (the user said no)";
      }
      const opts = menuOptions(t);
      const what = INSTALLER.test(t) ? "the installer" : /curl|stream\/wait|wakeshift wait/.test(t) ? "the background wait" : "a tool";
      // Under scope "session", its "in this conversation" option where offered;
      // otherwise the default (the first: once), as before.
      const c = policy.scope === "session" && opts.length >= 2 ? chooseOption(opts, policy) : null;
      if (c) await selectInMenu(l, opts, c.opt);
      else await l.tmux.keys(l.session, "Enter");
      const chosen = c?.opt.label ?? opts.find((o) => o.cursor)?.label ?? opts[0]?.label ?? "Enter (default)";
      l.prompted({ kind: "approval", asks: dialogAsks(t, opts[0]?.label), options: opts.map((o) => o.label), chosen, scope: c?.scope ?? scopeOf(chosen), sessionOffered: opts.some((o) => scopeOf(o.label) === "session"), forcedAlways: c?.forcedAlways ?? false });
      return `approved ${what}${c && c.scope !== "once" ? ` (${c.scope})` : ""}`;
    }
    if (/Allow access to|read (this|the) file/i.test(t) && /Allow/i.test(t)) {
      await l.tmux.keys(l.session, "Enter");
      return "allowed file access";
    }
    return null;
  }

  override busy(screen: string): boolean {
    return /esc to (cancel|interrupt)|Working\.\.\.|Thinking/i.test(tail(screen, 8));
  }

  async afterInstall(l: Lane): Promise<string[]> {
    await l.waitIdle({ allowInstall: true }, 60_000);
    await this.stop(l);
    await this.start(l, { resume: true });
    return ["restarted agy (--continue) to load the hooks"];
  }

  waiters(l: Lane, rows: PsRow[]): PsRow[] {
    const mine = this.mine(l, rows);
    return withShellParents(l, mine, mine.filter((r) => /\/v1\/stream|hook --harness antigravity|\.wakeshift\/bin\/wakeshift wait/.test(r.command)));
  }

  installed(l: Lane): string[] {
    return this.mentions(l, [".gemini/config/hooks.json", ".gemini/config/plugins/wakeshift/plugin.json", ".wakeshift/antigravity", ".wakeshift/bin/wakeshift"]);
  }

  async uninstall(l: Lane): Promise<void> {
    // The plugin first (agy's own uninstall), as the uninstall note says.
    if (existsSync(join(l.home.dir, ".gemini/config/plugins/wakeshift"))) {
      const r = await l.run(this.env(l), this.binPath()!, ["plugin", "uninstall", "wakeshift"]);
      l.note(`agy plugin uninstall wakeshift: ${r.ok ? "ok" : "failed"}`, { out: r.out.slice(-600) });
    }
    await this.uninstallHarness(l, "antigravity");
  }
}
