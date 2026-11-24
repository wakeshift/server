// Claude Code (claude), in a temp HOME. Login: a token from `claude
// setup-token` in the bench's state ($WB_STATE_DIR/claude-token, or
// $WB_CLAUDE_TOKEN_FILE), read by the pane's shell at start into
// CLAUDE_CODE_OAUTH_TOKEN (only the file's path is typed; the token is never
// copied or printed); without that file, the login keychain (symlinked). Either
// way, a copy of ~/.claude.json's account record and cached feature flags
// (no tokens; the flags decide whether Monitor exists). Zero install: Monitor on the session's wait loop.
// Install: Wakeshift's plugin for Claude Code (its marketplace, rendered by a
// local core for itself), loaded with /reload-plugins; or, where the core
// offers it, Wakeshift's installer's hooks in ~/.claude/settings.json.

import { execFile } from "node:child_process";
import { existsSync, readdirSync, readFileSync, realpathSync, statSync, writeFileSync } from "node:fs";
import { stateRoot } from "../../src/benchstate.js";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { redactPlain } from "../../src/redact.js";
import { promisify } from "node:util";
import { borrowKeychain, q, readJsonIf, REAL_HOME, writeJson } from "../../src/home.js";
import type { Lane } from "../../src/lane.js";
import { marketplaceSource } from "../../src/core.js";
import { ext } from "../../src/extensions.js";
import type { PsRow } from "../../src/procs.js";
import { sleep } from "../../src/tmux.js";
import { addWithPicker, chooseOption, dialogAsks, INSTALLER, mcpAddArgs, mcpServerEntry, notLoggedInReason, numberedOptions, scopeOf, tail, TmuxDriver, usePicker } from "../base.js";
import type { Received } from "../../src/arrivals.js";
import type { AnswerPolicy, Availability, Driver, Refusal, Turn } from "../types.js";
import { PLUGIN_OFFER } from "./offer.js";

const exec = promisify(execFile);

/**
 * The permission mode Claude Code lanes start in (`--permission-mode`):
 * WB_CLAUDE_PERMISSION_MODE, else "auto", Claude Code's own default for
 * interactive sessions since 2.1.283 (2.1.280 started in manual mode on the
 * bench's token: the default came from a feature flag then). "default" or
 * "manual" is the ask-before-acting mode; "harness" passes no flag, so the
 * binary's own default applies.
 */
export function claudePermissionMode(env: NodeJS.ProcessEnv = process.env): string | null {
  const m = (env.WB_CLAUDE_PERMISSION_MODE ?? "auto").trim();
  return m === "" || m === "harness" ? null : m;
}

/** A version with the lane's permission mode after it ("2.1.312 · auto mode"), so the report says which mode ran. */
function withMode(v: string): string {
  const m = claudePermissionMode();
  return `${v} · ${m === null ? "own default mode" : m === "default" ? "manual mode" : `${m} mode`}`;
}

/**
 * The models Claude Code lanes run, by full ID. An alias moves under the
 * bench: the account's default switched lanes from Opus 5.5 to Sonnet 5.5
 * between two Claude Code releases, and on 2.1.280 `--model sonnet` is Sonnet 5.
 * 2.1.280 doesn't list claude-sonnet-5-5 (it warns "unrecognized_model" on
 * stderr) but sends it as given, and the API runs Sonnet 5.5.
 */
export const CLAUDE_SONNET = "claude-sonnet-5-5";
export const CLAUDE_OPUS = "claude-opus-5-5";

/**
 * The model Claude Code lanes start with (`--model`): WB_CLAUDE_MODEL, else
 * Sonnet 5.5 (CLAUDE_SONNET), the bench's default. "harness" passes no
 * flag, so the account's own default applies.
 */
export function claudeModel(env: NodeJS.ProcessEnv = process.env): string | null {
  const m = (env.WB_CLAUDE_MODEL ?? "").trim();
  return m === "" ? CLAUDE_SONNET : m === "harness" ? null : m;
}

/**
 * The model Claude Code's start header names ("Sonnet 5.5 · Claude API",
 * the line under "Claude Code v2.1.319"), from the last header on the
 * screen; null if there's none.
 */
export function shownModel(screen: string): string | null {
  const lines = screen.split("\n");
  for (let i = lines.length - 2; i >= 0; i--) {
    if (!/Claude Code v\d/.test(lines[i])) continue;
    const m = /^(.+?)\s+·\s/.exec(lines[i + 1].replace(/^[^A-Za-z]*/, ""));
    return m ? m[1].trim() : null;
  }
  return null;
}

/**
 * Whether the header's model is the one asked for: its display name
 * ("claude-sonnet-5-5" is "Sonnet 5.5", not "Sonnet 5"), or the ID itself
 * where Claude Code doesn't know it. An alias ("opus") matches its family.
 */
export function modelMatches(want: string, shown: string): boolean {
  const s = shown.toLowerCase();
  const w = want.toLowerCase().replace(/\[1m\]$/, "");
  if (s.includes(w)) return true;
  const id = /^claude-([a-z]+)-(\d+)(?:-(\d{1,2}))?(?:-\d{8})?$/.exec(w);
  if (!id) return s.startsWith(w);
  const name = `${id[1]} ${id[2]}${id[3] ? `.${id[3]}` : ""}`;
  return s.startsWith(name) && !/^[\d.]/.test(s.slice(name.length));
}

/** The environment variable Claude Code reads a `claude setup-token` token from. */
export const CLAUDE_TOKEN_ENV = "CLAUDE_CODE_OAUTH_TOKEN";

/**
 * The file holding the bench's Claude Code token (`claude setup-token`):
 * $WB_CLAUDE_TOKEN_FILE, else claude-token in the bench's state. Null if
 * it's missing or empty: the keychain is borrowed instead. Throws if other
 * users could read it (neither it nor its directory is closed to them).
 * Never read here.
 */
export function claudeTokenFile(env: NodeJS.ProcessEnv = process.env): string | null {
  let f: string;
  let st;
  try {
    // The file itself, through any symlink, so its own directory is the one checked.
    f = realpathSync(env.WB_CLAUDE_TOKEN_FILE || join(stateRoot(env), "claude-token"));
    st = statSync(f);
  } catch {
    return null;
  }
  if (!st.isFile() || st.size === 0) return null;
  if ((st.mode & 0o077) !== 0 && (statSync(dirname(f)).mode & 0o077) !== 0) throw new Error(`${f} can be read by other users: chmod 600 it`);
  return f;
}

export class ClaudeCode extends TmuxDriver implements Driver {
  id = "claude-code";
  name = "Claude Code";
  bin = "claude";
  zeroInstallWake = true;
  installPath = "hooks" as const;
  secondMcp = true;
  harness = "claude-code";
  conversation = "none" as const;
  trustStep = /trust this folder|nothing to approve|picks? (the hooks )?up|hooks are (now )?(installed|in place)/i;

  /** The plugin's step is loading it: /reload-plugins, or a restart with claude --continue. */
  trustStepFor(added: string): RegExp {
    return PLUGIN_OFFER.test(added) ? /reload-plugins|claude --continue/i : this.trustStep;
  }

  offeredInstall(added: string): string | null {
    return new RegExp("`(" + PLUGIN_OFFER.source + "[^`]*)`").exec(added)?.[1] ?? null;
  }

  /**
   * The plugin's step: the user types /reload-plugins once the agent's turn
   * is over (Claude Code loads a plugin installed from the shell only then,
   * or when a session starts). Wakeshift's installer's hooks need nothing.
   */
  async afterInstall(l: Lane): Promise<string[]> {
    if (!pluginInstalled(l)) return [];
    const notes: string[] = [];
    // WB_CLAUDE_SCRIPT_HOOKS=1: a user moving over from Wakeshift's
    // installer. Its hooks go into ~/.claude/settings.json (which running
    // sessions read live) before the plugin loads, so both are there until
    // the plugin's first hook removes the installer's.
    if (process.env.WB_CLAUDE_SCRIPT_HOOKS === "1") {
      const r = await l.run(this.env(l), "/bin/sh", ["-c", `curl -fsSL ${l.ctx.bench.core}/install/claude-code.sh | sh`]);
      notes.push(`WB_CLAUDE_SCRIPT_HOOKS: ran Wakeshift's installer too (${r.ok ? "ok" : "failed"}); ~/.claude/settings.json has its hooks: ${scriptHooks(l)}`);
    }
    if (!(await l.waitIdle({ allowInstall: false }, 120_000))) return [...notes, "the turn didn't end within 2 minutes; typed /reload-plugins anyway"];
    await l.tmux.type(l.session, "/reload-plugins");
    await sleep(4000);
    const shown = /Reloaded|reloaded|plugins? (re)?loaded/i.test(tail(await l.screen(), 20));
    return [...notes, `typed /reload-plugins${shown ? "" : " (no confirmation seen on screen)"}`];
  }

  /**
   * CLAUDE_CONFIG_DIR (home.ts configEnv) also renames the keychain entry
   * Claude Code reads its login from ("Claude Code-credentials-<hash of the
   * directory>"), so a borrowed keychain's login wasn't found. An empty
   * CLAUDE_SECURESTORAGE_CONFIG_DIR keeps the default entry's name, and its
   * other use (a directory, empty: ~/.claude) is the temp HOME's.
   */
  override env(l: Lane, extra: Record<string, string> = {}): Record<string, string> {
    return super.env(l, { CLAUDE_SECURESTORAGE_CONFIG_DIR: "", ...extra });
  }

  /**
   * WB_CLAUDE_PIN=floor (the oldest Claude Code the offers support, from
   * harness-pins.json, installed with bench/releases/install-harness.sh),
   * else WB_CLAUDE_BIN (e.g. a newer release than the one on PATH), else
   * claude on PATH.
   */
  binPath(): string | null {
    const pinned = pinnedClaudeBin();
    if (pinned) return existsSync(pinned) ? pinned : null;
    const b = process.env.WB_CLAUDE_BIN;
    if (b) return existsSync(b) ? b : null;
    return super.binPath();
  }

  async version() {
    try {
      const { stdout } = await exec(this.binPath()!, ["--version"], { timeout: 20_000 });
      return withMode(stdout.trim().split(/\s/)[0]);
    } catch {
      return undefined;
    }
  }

  async versionIn(l: Lane): Promise<string | null> {
    const v = await super.versionIn(l);
    return v ? withMode(v) : null;
  }

  async check(): Promise<Availability> {
    const base = await super.check();
    if (!base.ok) return base;
    let token: string | null;
    try {
      token = claudeTokenFile();
    } catch (err) {
      return { ok: false, reason: err instanceof Error ? err.message : String(err) };
    }
    const cj = readJsonIf<{ oauthAccount?: unknown }>(join(REAL_HOME, ".claude.json"));
    if (!cj?.oauthAccount && !token) return { ok: false, reason: notLoggedInReason(this.name, "no account in ~/.claude.json, and no claude-token in the bench's state") };
    return base;
  }

  async prepare(l: Lane): Promise<void> {
    // Chosen once for the lane: start uses the same choice.
    const token = claudeTokenFile();
    l.state.claudeToken = token;
    if (token) l.note(`login: the bench's setup-token (${token}), not the keychain`);
    else borrowKeychain(l.home);
    const real = readJsonIf<Record<string, unknown>>(join(REAL_HOME, ".claude.json")) ?? {};
    // With CLAUDE_CONFIG_DIR set (home.ts configEnv), Claude Code reads its
    // global config from $CLAUDE_CONFIG_DIR/.claude.json, not ~/.claude.json:
    // the same record in both, so either reading finds it.
    for (const path of claudeJsonPaths(l.home.dir))
      writeJson(path, ext.claude.reduce((json, c) => c.amend(json, real), {
        oauthAccount: real.oauthAccount,
        hasCompletedOnboarding: true,
        theme: real.theme ?? "dark",
        autoUpdates: false,
        // The account's cached feature flags, as they are (Monitor is behind one); no credentials.
        ...cachedFlags(real),
        projects: { [l.home.work]: { hasTrustDialogAccepted: true, hasCompletedProjectOnboarding: true }, [l.home.real + "/work"]: { hasTrustDialogAccepted: true, hasCompletedProjectOnboarding: true } },
        // A server with a token (S11): the entry mcp add --header would write
        // (user scope), written here so the token is never on a command line.
        ...(l.mcpToken ? { mcpServers: { [l.mcpName]: mcpServerEntry(l) } } : {}),
      } as Record<string, unknown>));
    // The refusal path on demand (auto mode's classifier refuses `curl | sh`
    // only sometimes): a deny rule for the installer's download, so the
    // agent's run is refused and the note's fallback (the user types `! <cmd>`) is what's tested.
    if (process.env.WB_CLAUDE_DENY_INSTALLER === "1") {
      // Merged into whatever the temp HOME's settings already hold (a fresh HOME: nothing).
      const path = join(l.home.dir, ".claude/settings.json");
      const s = readJsonIf<{ permissions?: { deny?: string[] } & Record<string, unknown> } & Record<string, unknown>>(path) ?? {};
      const deny = [...new Set([...(s.permissions?.deny ?? []), DENY_INSTALLER])];
      writeJson(path, { ...s, permissions: { ...s.permissions, deny } });
      l.note(`refusing the agent's installer run: ${DENY_INSTALLER} in ~/.claude/settings.json (WB_CLAUDE_DENY_INSTALLER)`);
    }
    if (l.mcpToken) l.note(`added MCP server ${l.mcpName} (with its token) to ~/.claude.json`);
    else if (usePicker(l)) await addWithPicker(l, { harness: "claude-code", program: "claude", run: (args) => l.run(this.env(l), this.binPath()!, args) });
    else {
      // The bench's own command (WB_MCP_ADD=bench).
      const r = await l.run(this.env(l), this.binPath()!, mcpAddArgs(l, "claude"));
      l.note(`claude mcp add: ${r.ok ? "ok" : "failed"}`, { out: r.out.slice(-1000) });
      if (!r.ok) throw new Error("claude mcp add failed");
    }
    // WB_CLAUDE_PLUGIN_PREINSTALLED=1: a user who installed Wakeshift's
    // plugin earlier (from another conversation's offer), so the lane's
    // first conversation starts with it loaded. The core's own offer
    // command, run before Claude Code starts.
    if (process.env.WB_CLAUDE_PLUGIN_PREINSTALLED === "1") {
      const src = marketplaceSource(l.ctx.bench, "#");
      if (!src) throw new Error("WB_CLAUDE_PLUGIN_PREINSTALLED: this core offers no plugin marketplace (WB_CURL_INSTALL?)");
      // The two steps, which every Claude Code with plugins runs (the one
      // command needs 2.1.292).
      const add = await l.run(this.env(l), this.binPath()!, ["plugin", "marketplace", "add", src]);
      const r = add.ok ? await l.run(this.env(l), this.binPath()!, ["plugin", "install", "wakeshift@wakeshift"]) : add;
      l.note(`WB_CLAUDE_PLUGIN_PREINSTALLED: claude plugin install: ${r.ok ? "ok" : "failed"}`, { out: r.out.slice(-1000) });
      if (!r.ok || !pluginInstalled(l)) throw new Error("WB_CLAUDE_PLUGIN_PREINSTALLED: claude plugin install failed");
    }
    // S15: the second app's server, added the same way, next to the first.
    if (l.mcp2 && usePicker(l, l.mcp2)) await addWithPicker(l, { harness: "claude-code", program: "claude", server: l.mcp2, run: (args) => l.run(this.env(l), this.binPath()!, args) });
    else if (l.mcp2) {
      const r = await l.run(this.env(l), this.binPath()!, mcpAddArgs(l, "claude", l.mcp2));
      l.note(`claude mcp add ${l.mcp2.name}: ${r.ok ? "ok" : "failed"}`, { out: r.out.slice(-1000) });
      if (!r.ok) throw new Error(`claude mcp add ${l.mcp2.name} failed`);
    }
    // Latency studies: a channel server (WB_CHANNEL_SERVER, a stdio MCP
    // server declaring claude/channel) that forwards the session's stream.
    const ch = channelServer();
    if (ch) {
      const c = await l.run(this.env(l), this.binPath()!, ["mcp", "add", "wakebench-channel", "-e", `WB_CHANNEL_LINK_FILE=${ch.linkFile}`, "-e", `WB_CHANNEL_LOG=${ch.log}`, "--", process.execPath, ch.server]);
      l.note(`claude mcp add wakebench-channel: ${c.ok ? "ok" : "failed"}`, { out: c.out.slice(-1000) });
      if (!c.ok) throw new Error("claude mcp add wakebench-channel failed");
    }
  }

  async start(l: Lane, opts: { resume?: boolean } = {}): Promise<void> {
    await this.openPane(l, { DISABLE_AUTOUPDATER: "1" });
    l.state.panePid = await l.tmux.panePid(l.session);
    // The bench's token: the pane's shell reads it from its file, so only the
    // path is typed (tmux keeps a pane's start command; arguments are public).
    const token = (l.state.claudeToken as string | null | undefined) ?? null;
    if (token) {
      await l.tmux.type(l.session, tokenExport(token));
      await sleep(300);
    }
    const model = claudeModel();
    const mode = claudePermissionMode();
    if (!opts.resume) l.note(`permission mode: ${mode ?? "Claude Code's own default (no --permission-mode)"}; model: ${model ?? "the account's default (no --model)"}`);
    const channel = channelServer() ? ["--dangerously-load-development-channels", "server:wakebench-channel"] : [];
    // WB_CLAUDE_RESUME=id resumes by the session's ID (--resume <id>), the newest transcript's, instead of --continue.
    const id = opts.resume && process.env.WB_CLAUDE_RESUME === "id" ? newestSession(l.home.dir) : null;
    if (opts.resume) l.note(id ? "resumed with --resume <the session's ID> (WB_CLAUDE_RESUME=id)" : "resumed with --continue");
    await this.shell(l, [this.binPath()!, ...(opts.resume ? (id ? ["--resume", id] : ["--continue"]) : []),...(model ? ["--model", model] : []), ...(mode ? ["--permission-mode", mode] : []), ...channel]);
    // Ready: the input box. Then a few seconds more, so the session's tool
    // list is built after the feature flags load.
    const ok = await l.waitFor(/\? for shortcuts|shift\+tab to cycle|╭─|Try "|^❯\s*$|>\s*$|Select login method/m, 60_000, { allowInstall: false });
    // The borrowed login didn't take: a run can't do anything, so say why.
    const why = token ? `it refused the bench's token, ${token}: make a new one with claude setup-token` : "the keychain's login didn't carry over: put a claude setup-token token in the bench's state, claude-token; bench/README.md";
    await this.checkLogin(l, why);
    if (ok === null) await this.didntStart(l, "Claude Code didn't reach its start screen", opts);
    await this.checkModel(l, model);
    await sleep(10_000);
    await this.checkLogin(l, why);
  }

  /**
   * The model the start header names, kept for the report (l.state.model);
   * a lane that started on another model than the one asked for fails.
   */
  protected async checkModel(l: Lane, want: string | null): Promise<void> {
    let shown: string | null = null;
    for (let i = 0; i < 10 && shown === null; i++) {
      shown = shownModel(await l.screen());
      if (shown === null) await sleep(500);
    }
    const ok = want !== null && shown !== null && modelMatches(want, shown);
    l.state.model = shown === null ? (want ? `${want} (unconfirmed)` : null) : ok && want !== shown ? `${want} (${shown})` : shown;
    if (shown === null) {
      l.note(`model: the start header didn't name one; asked for ${want ?? "the account's default"}`);
      return;
    }
    l.note(`model: ${shown} (asked for ${want ?? "the account's default"})`);
    if (want && !ok) throw new Error(`Claude Code started on ${shown}, not ${want} (WB_CLAUDE_MODEL)`);
  }

  async stop(l: Lane): Promise<void> {
    await l.tmux.keys(l.session, "Escape");
    await sleep(300);
    await l.tmux.type(l.session, "/exit");
    if (!(await this.waitShell(l, 15_000))) {
      await l.tmux.keys(l.session, "C-c");
      await sleep(500);
      await l.tmux.keys(l.session, "C-c");
      await this.waitShell(l, 10_000);
    }
  }

  async answer(l: Lane, screen: string, policy: AnswerPolicy): Promise<string | null> {
    // Dialogs replace the input box at the bottom: look only there, and only
    // at a numbered choice with the cursor on it (the transcript above can
    // quote the same words).
    // AskUserQuestion about the install (taller than a permission dialog,
    // with each option's description): pick the user's answer.
    const picker = installPicker(tail(screen, 30));
    if (picker) {
      if (picker.submit !== undefined) {
        await l.tmux.keys(l.session, String(picker.submit));
        return "submitted the answer";
      }
      const yes = policy.pick === "yes";
      // A user who won't install, offered notifications for this session without one, takes those.
      const pickNo = picker.session ?? picker.no;
      await l.tmux.keys(l.session, String(yes ? picker.yes : pickNo));
      l.state.picked = yes ? "yes" : "no";
      l.state.pickedAt = Date.now();
      return `picked "${yes ? "yes, install" : picker.session !== undefined ? "this session only, nothing installed" : "no"}" in AskUserQuestion`;
    }
    const t = tail(screen, 16);
    const choice = /^\s*❯\s*\d\.\s/m.test(t);
    if (!choice) return null;
    // The bench's own channel server, loaded with the development flag it asked for.
    if (channelServer() && /Loading development channels/.test(t) && /❯\s*1\.\s*I am using this for local development/.test(t)) {
      await l.tmux.keys(l.session, "Enter");
      return "confirmed the bench's development channel";
    }
    // A first-start offer (Claude Code 2.1.28x: "Try the new fullscreen renderer?"): not now, so the
    // screen stays the one the patterns here read.
    if (/Try the new fullscreen renderer\?/i.test(t)) {
      const n = /(\d)\.\s*Not now/i.exec(t)?.[1];
      if (n) {
        await l.tmux.keys(l.session, n);
        return "declined the fullscreen renderer";
      }
    }
    // A permission dialog: "Do you want to proceed?" / "Do you want to make this edit" with numbered options.
    if (PERMISSION.test(t) && /❯\s*1\.\s*Yes/.test(t)) {
      // The installer only after the user said yes (in chat, or in the picker).
      if (INSTALLER.test(t) && !policy.allowInstall && l.state.picked !== "yes") {
        await l.tmux.keys(l.session, "Escape");
        return "denied the installer (the user said no)";
      }
      const opts = numberedOptions(t);
      // Never an option that changes the session's permission mode (manual
      // mode's "Yes, and switch to auto mode", under its "Tip: auto mode
      // handles these prompts for you"): the lane keeps the mode it ran in.
      const c = chooseOption(opts.filter((o) => !SWITCHES_MODE.test(o.label)), policy) ?? { opt: { n: 1, label: "Yes", cursor: true }, scope: "once" as const, forcedAlways: false };
      await l.tmux.keys(l.session, String(c.opt.n ?? 1));
      if (!/\d\.\s.*\(.*\)\s*$/.test(t)) await sleep(100);
      const what = /Monitor/i.test(t) ? "Monitor" : INSTALLER.test(t) ? "installer" : /mcp__|\(MCP\)/i.test(t) ? "an MCP tool" : /Bash command|Bash\(/i.test(t) ? "a Bash command" : "a tool";
      l.prompted({ kind: "approval", asks: dialogAsks(t, opts[0]?.label), options: opts.map((o) => o.label), chosen: c.opt.label, scope: c.scope, sessionOffered: opts.some((o) => scopeOf(o.label) === "session"), forcedAlways: c.forcedAlways });
      return `approved ${what}${c.scope === "always" ? " (don't ask again)" : c.scope === "session" ? " (this session)" : ""}`;
    }
    if (/Do you trust the files in this folder\?|Is this a project you (created|trust)/i.test(t) && /❯\s*1\.\s*Yes/.test(t)) {
      await l.tmux.keys(l.session, "Enter");
      return "trusted the folder";
    }
    return null;
  }

  asksUserToRun(screen: string): boolean {
    return askedToType(screen);
  }

  async runAsUser(l: Lane, screen: string, cmd: string): Promise<string | null> {
    if (!askedToType(screen)) return null;
    // Only at the input box, once the turn is over (a turn that's still
    // going would take the keys).
    if (!(await l.waitIdle({ allowInstall: false }, 60_000)) || !inputEmpty(await l.tmux.capture(l.session, 40))) return null;
    // "!" puts the input box in bash mode: what follows runs as the user.
    await l.tmux.type(l.session, "!", false);
    await sleep(500);
    await l.tmux.type(l.session, cmd);
    await l.waitIdle({ allowInstall: false }, 120_000);
    return `typed "! ${cmd}"`;
  }

  waiters(l: Lane, rows: PsRow[]): PsRow[] {
    // Monitor's wait loop (a shell running the loop, and its curl), or the
    // hooks' rewake waiter.
    return this.mine(l, rows).filter((r) => /\/v1\/stream\b|\/v1\/stream\/wait|hook --harness claude-code --rewake/.test(r.command));
  }

  installed(l: Lane): string[] {
    return this.mentions(l, [".claude/settings.json", ".claude/plugins/installed_plugins.json", ".claude/plugins/known_marketplaces.json", ".wakeshift/claude-code", ".wakeshift/bin/wakeshift"]);
  }

  /** Keeps the hooks' waiter log (why each waiter stopped; no links or content) with the run's logs. */
  async cleanup(l: Lane): Promise<void> {
    const src = join(l.home.dir, ".wakeshift/claude-code/logs/waiters.log");
    if (!existsSync(src)) return;
    writeFileSync(join(l.ctx.dir, "logs", `${l.id}.waiters.log`), redactPlain(readFileSync(src, "utf8")), { mode: 0o600 });
    l.note("kept the hooks' waiter log", { file: `logs/${l.id}.waiters.log` });
  }

  async uninstall(l: Lane): Promise<void> {
    if (process.env.WB_CLAUDE_SCRIPT_HOOKS === "1") l.note(`before uninstall, ~/.claude/settings.json has the installer's hooks: ${scriptHooks(l)}`);
    // The uninstaller removes ~/.wakeshift/claude-code, its waiter log too: keep it first.
    await this.cleanup(l);
    // The plugin, as the uninstall note says (claude plugin uninstall), and
    // its marketplace, then what the connector set up.
    if (pluginInstalled(l)) {
      for (const args of [["plugin", "uninstall", "wakeshift@wakeshift"], ["plugin", "marketplace", "remove", "wakeshift"]]) {
        const r = await l.run(this.env(l), this.binPath()!, args);
        l.note(`claude ${args.join(" ")}: ${r.ok ? "ok" : "failed"}`, { out: r.out.slice(-1000) });
      }
    }
    await this.uninstallHarness(l, "claude-code");
  }

  /** Tool calls auto mode's classifier refused since a time, from the session transcripts in the temp HOME. */
  refusals(l: Lane, sinceMs: number): Refusal[] {
    const dir = join(l.home.dir, ".claude/projects");
    if (!existsSync(dir)) return [];
    const out: Refusal[] = [];
    // A transcript Claude Code is writing or moving can come and go: skip what can't be read.
    const list = (d: string) => {
      try {
        return readdirSync(d);
      } catch {
        return [];
      }
    };
    for (const p of list(dir)) {
      for (const f of list(join(dir, p))) {
        if (!f.endsWith(".jsonl")) continue;
        try {
          out.push(...parseRefusals(readFileSync(join(dir, p, f), "utf8").split("\n")));
        } catch {
          // Gone, or not a file.
        }
      }
    }
    return out.filter((x) => Date.parse(x.at) >= sinceMs).sort((a, b) => a.at.localeCompare(b.at));
  }

  /** Monitor calls since a time, and whether a Monitor expired at Claude Code's limit (S13), from the transcripts in the temp HOME. */
  monitorLog(l: Lane, sinceMs: number): { starts: number; expired: boolean } {
    const dir = join(l.home.dir, ".claude/projects");
    const out = { starts: 0, expired: false };
    // A transcript directory Claude Code is writing or moving can come and go: skip what can't be read.
    const list = (d: string) => {
      try {
        return readdirSync(d);
      } catch {
        return [];
      }
    };
    for (const p of list(dir)) {
      for (const f of list(join(dir, p))) {
        if (!f.endsWith(".jsonl")) continue;
        try {
          const r = parseMonitorLog(readFileSync(join(dir, p, f), "utf8").split("\n"), sinceMs);
          out.starts += r.starts;
          out.expired ||= r.expired;
        } catch {
          // Gone, or not a file.
        }
      }
    }
    return out;
  }

  /** What reached the model, from the session transcripts in the temp HOME (parseReceived); null if there are none. */
  async received(l: Lane): Promise<Received[] | null> {
    const dir = join(l.home.dir, ".claude/projects");
    const list = (d: string) => {
      try {
        return readdirSync(d);
      } catch {
        return [];
      }
    };
    let files = 0;
    const out: Received[] = [];
    for (const p of list(dir)) {
      for (const f of list(join(dir, p))) {
        if (!f.endsWith(".jsonl")) continue;
        try {
          out.push(...parseReceived(readFileSync(join(dir, p, f), "utf8").split("\n"), f));
          files++;
        } catch {
          // Gone, or not a file.
        }
      }
    }
    return files ? out : null;
  }

  // The transcripts as last read, by path: polled every 250ms while a
  // scenario waits, so a file is read and parsed again only once it grows.
  private transcripts = new Map<string, { size: number; turns: Turn[] }>();

  turns(l: Lane, sinceMs: number): Turn[] {
    const dir = join(l.home.dir, ".claude/projects");
    if (!existsSync(dir)) return [];
    const out: Turn[] = [];
    for (const p of readdirSync(dir)) {
      for (const f of readdirSync(join(dir, p))) {
        if (!f.endsWith(".jsonl")) continue;
        const path = join(dir, p, f);
        const size = statSync(path).size;
        let c = this.transcripts.get(path);
        if (!c || c.size !== size) {
          c = { size, turns: parseTurns(readFileSync(path, "utf8").split("\n")) };
          this.transcripts.set(path, c);
        }
        out.push(...c.turns);
      }
    }
    return out.filter((t) => t.queued >= sinceMs);
  }
}

/**
 * Wake turns from a Claude Code transcript (its JSONL lines): each
 * queue-operation enqueue (a Monitor event, a channel message: what Claude
 * Code queues while it's idle or busy), the dequeue that started its turn,
 * the turn's first assistant block, its end (system turn_duration) and the
 * tokens its model calls used (each API message counted once).
 */
export function parseTurns(lines: string[]): Turn[] {
  type Entry = { type?: string; operation?: string; subtype?: string; timestamp?: string; content?: unknown; message?: { id?: string; model?: string; content?: unknown; usage?: Record<string, number> } };
  const es: Entry[] = [];
  for (const s of lines) {
    if (!s.trim()) continue;
    try {
      es.push(JSON.parse(s) as Entry);
    } catch {
      // A line being written.
    }
  }
  const at = (e: Entry) => (e.timestamp ? Date.parse(e.timestamp) : NaN);
  const textOf = (e: Entry) => (typeof e.content === "string" ? e.content : JSON.stringify(e.content ?? ""));
  // Each dequeue starts one queued item's turn: events queued while
  // Claude Code was busy are matched to dequeues first in, first out
  // (by content where the dequeue records it), so two never claim one.
  const claimed = new Set<number>();
  const dequeueFor = (i: number, text: string): number | null => {
    for (let j = i + 1; j < es.length; j++) {
      const x = es[j];
      if (x.type !== "queue-operation" || x.operation !== "dequeue" || claimed.has(j)) continue;
      if (x.content === undefined || textOf(x) === text) return j;
    }
    return null;
  };
  const out: Turn[] = [];
  for (let i = 0; i < es.length; i++) {
    const e = es[i];
    if (e.type !== "queue-operation" || e.operation !== "enqueue") continue;
    const text = textOf(e);
    const t: Turn = {
      source: /<task-notification>[\s\S]*Monitor event/.test(text) ? "monitor" : /<channel\b/.test(text) ? "channel" : "other",
      text,
      queued: at(e),
      started: null,
      first: null,
      firstKind: null,
      ended: null,
      outputTokens: 0,
      inputTokens: 0,
      model: null,
    };
    const seen = new Set<string>();
    const d = dequeueFor(i, text);
    if (d === null) {
      out.push(t);
      continue;
    }
    claimed.add(d);
    t.started = at(es[d]);
    for (let j = d + 1; j < es.length; j++) {
      const x = es[j];
      if (x.type === "assistant" && x.message) {
        if (t.first === null) {
          t.first = at(x);
          const c = Array.isArray(x.message.content) ? (x.message.content[0] as { type?: string; name?: string } | undefined) : undefined;
          t.firstKind = c ? (c.name ? `${c.type}:${c.name}` : (c.type ?? null)) : null;
          t.model = x.message.model ?? null;
        }
        const id = x.message.id ?? String(j);
        if (!seen.has(id) && x.message.usage) {
          seen.add(id);
          const u = x.message.usage;
          t.outputTokens += u.output_tokens ?? 0;
          t.inputTokens += (u.input_tokens ?? 0) + (u.cache_read_input_tokens ?? 0) + (u.cache_creation_input_tokens ?? 0);
        }
      }
      if (x.type === "system" && x.subtype === "turn_duration") {
        t.ended = at(x);
        break;
      }
    }
    out.push(t);
  }
  return out;
}

/**
 * What reached the model in a Claude Code transcript (its JSONL lines): user
 * entries (a typed message, a Monitor or MCP task notification that started
 * a turn, a Stop hook's message), the text of tool results from MCP tools
 * (watch, the wait tool), and attachments as they were rendered for the
 * model (a Monitor event queued mid-turn, a PostToolUse hook's context).
 * A failed MCP call's error doesn't deliver the codes its own arguments
 * named (Received.echoes): it quotes them back.
 * Not:the model's own output, other tools' results (a shell command that
 * printed a code isn't a delivery), compaction summaries, or the queue and
 * API request records (copies of what's above). Keyed by the entry's uuid,
 * so an entry copied into a resumed session's file counts once.
 */
export function parseReceived(lines: string[], file = ""): Received[] {
  type Block = { type?: string; id?: string; name?: string; text?: unknown; tool_use_id?: string; content?: unknown; input?: unknown; is_error?: boolean };
  type Entry = {
    type?: string;
    uuid?: string;
    timestamp?: string;
    isSidechain?: boolean;
    isCompactSummary?: boolean;
    isVisibleInTranscriptOnly?: boolean;
    message?: { content?: unknown };
    attachment?: { type?: string; prompt?: unknown };
    rendered?: { content?: unknown }[];
  };
  const text = (c: unknown): string =>
    typeof c === "string"
      ? c
      : Array.isArray(c)
        ? c.map((x: { text?: unknown; content?: unknown } | string) => (typeof x === "string" ? x : typeof x?.text === "string" ? x.text : text(x?.content))).join("\n")
        : "";
  const es: Entry[] = [];
  for (const s of lines) {
    if (!s.trim()) continue;
    try {
      es.push(JSON.parse(s) as Entry);
    } catch {
      // A line being written.
    }
  }
  // Tool names and inputs by call ID, from the model's calls.
  const tools = new Map<string, string>();
  const inputs = new Map<string, string>();
  for (const e of es)
    if (e.type === "assistant" && Array.isArray(e.message?.content))
      for (const b of e.message!.content as Block[])
        if (b.type === "tool_use" && b.id) {
          tools.set(b.id, b.name ?? "");
          inputs.set(b.id, JSON.stringify(b.input ?? null));
        }
  const out: Received[] = [];
  es.forEach((e, i) => {
    if (e.isSidechain || e.isCompactSummary || e.isVisibleInTranscriptOnly) return;
    const key = e.uuid ?? `${file}:${i}`;
    const at = e.timestamp ? Date.parse(e.timestamp) : null;
    if (e.type === "user") {
      const c = e.message?.content;
      if (typeof c === "string") {
        out.push({ key, at, via: "user", text: c });
        return;
      }
      if (!Array.isArray(c)) return;
      (c as Block[]).forEach((b, j) => {
        if (b.type === "text" && typeof b.text === "string") out.push({ key: `${key}:${j}`, at, via: "user", text: b.text });
        else if (b.type === "tool_result") {
          const tool = tools.get(b.tool_use_id ?? "") ?? "";
          if (!tool.startsWith("mcp__")) return;
          // A failed call's error can quote its own arguments (report_seen's ids that didn't validate): those codes are the model's own, echoed.
          const echoes = b.is_error ? (inputs.get(b.tool_use_id ?? "")?.match(/\bWB-[a-z0-9]{4}-\d+\b/g) ?? []) : [];
          out.push({ key: `${key}:${j}`, at, via: `tool_result:${tool}${b.is_error ? " (error)" : ""}`, text: text(b.content), ...(echoes.length ? { echoes } : {}) });
        }
      });
    } else if (e.type === "attachment" && e.attachment) {
      // As rendered for the model; a queued command without its rendering, as queued.
      const r = Array.isArray(e.rendered) ? e.rendered.map((x) => text(x?.content)).join("\n") : "";
      const t = r || (e.attachment.type === "queued_command" ? text(e.attachment.prompt) : "");
      if (t) out.push({ key, at, via: `attachment:${e.attachment.type ?? "?"}`, text: t });
    }
  });
  return out;
}

/**
 * From a Claude Code transcript (its JSONL lines), since a time: how many
 * Monitor calls the model made, and whether Claude Code told it a Monitor
 * expired ("[Monitor expired after 30m …]", 2.1.312; "[Monitor timed out"
 * before), rather than its command ending by itself.
 */
export function parseMonitorLog(lines: string[], sinceMs: number): { starts: number; expired: boolean } {
  const ids = new Set<string>();
  let expired = false;
  for (const s of lines) {
    if (!s.trim()) continue;
    let e: { type?: string; timestamp?: string; message?: { content?: unknown } };
    try {
      e = JSON.parse(s);
    } catch {
      continue; // A line being written.
    }
    if (!e.timestamp || Date.parse(e.timestamp) < sinceMs) continue;
    if (e.type === "assistant" && Array.isArray(e.message?.content))
      for (const b of e.message!.content as { type?: string; id?: string; name?: string }[]) if (b.type === "tool_use" && b.name === "Monitor" && b.id) ids.add(b.id);
    if (/\[Monitor (expired after|timed out)/.test(s)) expired = true;
  }
  return { starts: ids.size, expired };
}

/** How Claude Code words a tool result its auto mode classifier refused (2.1.312), up to the reason. */
const AUTO_DENIED = "Permission for this action was denied by the Claude Code auto mode classifier. Reason: ";
/** ...and one it blocked because the classifier gave no answer. */
const AUTO_UNAVAILABLE = "Auto mode could not evaluate this action";
/** ...and one a deny rule refused ("Permission to use Bash with command … has been denied."). */
const RULE_DENIED = "Permission to use ";

/**
 * Tool calls refused without a dialog in a Claude Code transcript (its JSONL
 * lines): by auto mode's classifier (AUTO_DENIED, AUTO_UNAVAILABLE) or a
 * deny rule (RULE_DENIED), each with the tool it answered (a Bash command's
 * start) and the reason, redacted.
 */
export function parseRefusals(lines: string[]): Refusal[] {
  type Block = { type?: string; id?: string; name?: string; input?: { command?: unknown }; tool_use_id?: string; content?: unknown };
  const calls = new Map<string, string>();
  const out: Refusal[] = [];
  const text = (c: unknown): string => (typeof c === "string" ? c : Array.isArray(c) ? c.map((x: { text?: unknown }) => (typeof x?.text === "string" ? x.text : "")).join("") : "");
  for (const s of lines) {
    if (!s.trim()) continue;
    let e: { type?: string; timestamp?: string; message?: { content?: unknown } };
    try {
      e = JSON.parse(s);
    } catch {
      continue; // A line being written.
    }
    const blocks = Array.isArray(e.message?.content) ? (e.message!.content as Block[]) : [];
    for (const b of blocks) {
      if (b.type === "tool_use" && b.id) {
        const cmd = typeof b.input?.command === "string" ? `(${b.input.command.replace(/\s+/g, " ").slice(0, 160)})` : "";
        calls.set(b.id, `${b.name ?? "tool"}${cmd}`);
      } else if (b.type === "tool_result" && e.timestamp) {
        const t = text(b.content);
        let reason: string;
        if (t.startsWith(AUTO_DENIED)) {
          const rest = t.slice(AUTO_DENIED.length);
          const end = rest.indexOf(". If you have other tasks");
          reason = (end > 0 ? rest.slice(0, end) : rest).slice(0, 400);
        } else if (t.startsWith(AUTO_UNAVAILABLE)) reason = "the classifier gave no verdict (blocked for safety)";
        else if (t.startsWith(RULE_DENIED) && t.slice(0, 4000).includes(" has been denied.")) reason = "a permission rule denied it";
        else continue;
        out.push({ at: e.timestamp, tool: redactPlain(calls.get(b.tool_use_id ?? "") ?? "a tool"), reason: redactPlain(reason) });
      }
    }
  }
  return out;
}

/** The channel server for WB_CLAUDE_TRANSPORT=channel: its script, the link file the toy app writes, its log. */
function channelServer(): { server: string; linkFile: string; log: string } | null {
  // dist/drivers/claude-code/index.js → bench/drivers/claude-code/channel.mjs (not compiled).
  const server = process.env.WB_CHANNEL_SERVER ?? fileURLToPath(new URL("../../../drivers/claude-code/channel.mjs", import.meta.url));
  const linkFile = process.env.WB_CHANNEL_LINK_FILE;
  if (process.env.WB_CLAUDE_TRANSPORT !== "channel" || !linkFile) return null;
  return { server, linkFile, log: process.env.WB_CHANNEL_LOG ?? linkFile + ".log" };
}

/** The real config's cached feature flags (its cached…Features keys), copied as they are. */
export function cachedFlags(real: Record<string, unknown>): Record<string, unknown> {
  return Object.fromEntries(Object.entries(real).filter(([k]) => /^cached\w*Features$/.test(k)));
}

/**
 * WB_CLAUDE_PIN=floor: the oldest Claude Code the core's offers support
 * (bench/harness-pins.json's "claude-code-floor"), as
 * bench/releases/install-harness.sh puts it in the bench's state. Null
 * without the pin; the path (which may not exist yet) with it.
 */
export function pinnedClaudeBin(env: NodeJS.ProcessEnv = process.env, pinsFile = fileURLToPath(new URL("../../../harness-pins.json", import.meta.url))): string | null {
  if (env.WB_CLAUDE_PIN !== "floor") return null;
  const v = readJsonIf<Record<string, string>>(pinsFile)?.["claude-code-floor"];
  if (!v || !/^[0-9][0-9A-Za-z.+-]*$/.test(v)) throw new Error(`WB_CLAUDE_PIN=floor: no "claude-code-floor" version in ${pinsFile}`);
  return join(stateRoot(env), "claude-code", v, "claude");
}

/** Where a temp HOME's Claude Code reads its global config: ~/.claude.json, or $CLAUDE_CONFIG_DIR/.claude.json once that's set. */
export function claudeJsonPaths(home: string): string[] {
  return [join(home, ".claude.json"), join(home, ".claude/.claude.json")];
}

/** The session ID of a temp HOME's newest main-session transcript (~/.claude/projects/<project>/<id>.jsonl), or null. */
export function newestSession(home: string): string | null {
  const dir = join(home, ".claude/projects");
  let best: { id: string; at: number } | null = null;
  const list = (d: string) => {
    try {
      return readdirSync(d);
    } catch {
      return [];
    }
  };
  for (const p of list(dir))
    for (const f of list(join(dir, p))) {
      const id = f.endsWith(".jsonl") ? f.slice(0, -".jsonl".length) : "";
      // A session ID is a UUID: never a subagent's agent-<id>.jsonl (written beside them by older releases).
      if (!/^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i.test(id)) continue;
      try {
        const at = statSync(join(dir, p, f)).mtimeMs;
        if (!best || at > best.at) best = { id, at };
      } catch {
        // Gone meanwhile.
      }
    }
  return best?.id ?? null;
}

/** Whether ~/.claude/settings.json has hooks Wakeshift's installer wrote. */
export function scriptHooks(l: Pick<Lane, "home">): boolean {
  try {
    return /hook --harness claude-code/.test(readFileSync(join(l.home.dir, ".claude/settings.json"), "utf8"));
  } catch {
    return false;
  }
}

/** The version of Wakeshift's plugin installed in the lane's Claude Code (installed_plugins.json), or null. */
export function installedPluginVersion(l: Pick<Lane, "home">): string | null {
  const j = readJsonIf<{ plugins?: Record<string, unknown> }>(join(l.home.dir, ".claude/plugins/installed_plugins.json"));
  const e = j?.plugins?.["wakeshift@wakeshift"];
  const one = (Array.isArray(e) ? e[e.length - 1] : e) as { version?: unknown } | undefined;
  return typeof one?.version === "string" ? one.version : null;
}

/** Whether Wakeshift's plugin is installed in the lane's Claude Code. */
export function pluginInstalled(l: Pick<Lane, "home">): boolean {
  const j = readJsonIf<{ plugins?: Record<string, unknown> }>(join(l.home.dir, ".claude/plugins/installed_plugins.json"));
  return Boolean(j?.plugins && "wakeshift@wakeshift" in j.plugins);
}

/** WB_CLAUDE_DENY_INSTALLER's rule: the installer's download (`curl -fsSL <core>/install/…`); the Monitor's `curl -s` doesn't match. */
export const DENY_INSTALLER = "Bash(curl -fsSL *)";

/** A dialog option that changes the permission mode ("Yes, and switch to auto mode", "Yes, and use auto mode"). */
const SWITCHES_MODE = /\b(switch to|use) [a-z ]*mode\b/i;

/** A permission dialog's question. */
const PERMISSION = /Do you want to (proceed|run|allow|make this edit|create)|Allow (this|the) (tool|command)/i;

/**
 * Claude Code's AskUserQuestion dialog about the install, from the bottom
 * of the screen: the option numbers for yes and no (the note's "Yes,
 * install Wakeshift's hooks…" and "No, don't install them", or the agent's
 * own yes/no wording), or on its review step the number of "Submit". Null
 * if there's no such dialog.
 */
export function installPicker(t: string): { yes?: number; no?: number; session?: number; submit?: number } | null {
  const submit = /^\s*(?:❯\s*)?(\d)\.\s*Submit( answers?)?\s*$/im.exec(t);
  if (submit && /Review your answers|Ready to submit/i.test(t)) return { submit: Number(submit[1]) };
  if (!/Enter to select|to navigate|Esc to cancel/i.test(t) || PERMISSION.test(t)) return null;
  let yes: number | undefined;
  let no: number | undefined;
  let session: number | undefined;
  for (const m of t.matchAll(/^\s*(?:❯\s*)?(\d)\.\s+(.+)$/gm)) {
    const label = m[2].trim();
    if (yes === undefined && ((/^yes\b/i.test(label) && /install|hooks?|plugin/i.test(label)) || /\bevery session\b|^install\b/i.test(label))) yes = Number(m[1]);
    else if (no === undefined && /^(no\b|don'?t (install|notify)|skip|not now)/i.test(label)) no = Number(m[1]);
    // A choice of how to be notified: this session only, with nothing installed.
    else if (session === undefined && /\b(this session|session is open|while (this|the) session|nothing (is )?installed|without (an )?install)/i.test(label)) session = Number(m[1]);
  }
  return yes !== undefined && no !== undefined ? { yes, no, ...(session !== undefined ? { session } : {}) } : null;
}

/** Whether Claude Code's status line says it isn't logged in ("Not logged in · Run /login"). */
export function notLoggedIn(screen: string): boolean {
  return /Not logged in\s*·\s*(Please )?run \/login/i.test(tail(screen, 12).replace(/[ \t]+/g, " "));
}

/** Whether the agent asked the user to run the installer themselves with "!" (the note's fallback when running it is refused). */
export function askedToType(screen: string): boolean {
  // Without whitespace, so a command wrapped anywhere still matches.
  const flat = tail(screen, 40).replace(/\s+/g, "");
  // The plain command, or with the app's tools (`| WAKESHIFT_ALLOW_TOOLS=… sh`).
  if (/(?:^|[^\w!])!curl-fsSL\S+?\/install\/claude-code\.sh\|(?:WAKESHIFT_ALLOW_TOOLS=\S*?)?sh/.test(flat)) return true;
  // Wakeshift's plugin, with Claude Code's own plugin installer, with its
  // spaces kept: "type ! claude plugin install …" flattens to "type!claude…".
  const spaced = tail(screen, 40).replace(/\s+/g, " ");
  return /(?:^|[^\w!])! ?claude plugin (?:install wakeshift --marketplace|marketplace add \S+ && claude plugin install wakeshift@wakeshift)/.test(spaced);
}

/**
 * Whether Claude Code's input box is empty, from the pane with its colors
 * (tmux capture -e): nothing after the last "❯" but a dim suggestion of what
 * to say next (2.1.31x: "I ran the install myself"), which typing replaces.
 */
export function inputEmpty(raw: string): boolean {
  const lines = raw.trimEnd().split("\n").slice(-8);
  for (let i = lines.length - 1; i >= 0; i--) {
    const at = lines[i].indexOf("❯");
    if (at < 0) continue;
    // What follows the marker, without color resets and spaces; a dim run (ESC[2m … ESC[0m or ESC[22m, or to the end of the line: 2.1.316) counts as empty.
    const rest = lines[i]
      .slice(at + 1)
      .replace(/\x1b\[2m[^\x1b]*(?:\x1b\[(?:0|22)?m|$)/g, "")
      .replace(/\x1b\[[0-9;]*m/g, "")
      .replace(/[\s ]+/g, "");
    return rest === "";
  }
  return false;
}

/** The line the pane's shell runs to read the token from its file into Claude Code's environment. */
export function tokenExport(file: string): string {
  return `export ${CLAUDE_TOKEN_ENV}="$(cat ${q(file)})"`;
}
