// opencode, in a temp HOME, on a free model (no login needed; set
// WB_OPENCODE_MODEL to change it). Nothing wakes opencode without the
// install: messages wait for the next call. Install: a plugin in
// ~/.config/opencode/plugins/wakeshift.ts, or, where the core offers it
// (WB_OPENCODE_PLUGIN), the npm plugin @wakeshift/opencode in opencode.json's
// plugin list, from WB_NPM_REGISTRY (a local registry holding packages packed
// for this core); either loads when opencode starts.
//
// MCP Events push (the realistic tier's push lane, R7-R9): WB_OPENCODE_BIN
// runs another opencode than the one on PATH (a build with MCP Events, such
// as a wrapper that runs a checkout from source), WB_OPENCODE_FLAGS sets
// its OPENCODE_FLAGS (mcp_events; opencode 2.4 and later have Events with
// no flag: WB_OPENCODE_PUSH=1 marks the lane instead), and WB_OPENCODE_PROTOCOL (legacy, auto
// or 2026-07-28) writes the server's entry in opencode 2.x's shape
// (mcp.servers.<name>, with protocol; 1.x's shape drops it, so it always
// ran legacy). A model on openai/ takes the key from the bench's env file
// (OPENAI_API_KEY), written to the temp HOME's auth.json (0600), never on a
// command line.

import { execFile, execFileSync } from "node:child_process";
import { existsSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { promisify } from "node:util";
import { readSecrets } from "../../src/benchstate.js";
import { readJsonIf, writeJson } from "../../src/home.js";
import type { Lane } from "../../src/lane.js";
import { ps, type PsRow } from "../../src/procs.js";
import { sleep } from "../../src/tmux.js";
import { addWithPicker, BlockedError, dialogAsks, INSTALLER, mcpAddArgs, mcpServerEntry, tail, TmuxDriver, usePicker } from "../base.js";
import type { AnswerPolicy, Availability, Driver } from "../types.js";

const exec = promisify(execFile);
export const OPENCODE_MODEL = "opencode/deepseek-v4.1-flash-free";
/** How long opencode's "Allow always" lasts (to confirm against opencode's docs: S12 reports it). */
export const OPENCODE_ALWAYS_SCOPE: "session" | "always" = "session";
/** The key help at the bottom of an open question picker. */
export const PICKER = /enter submit\s+esc dismiss|esc dismiss/i;

/**
 * opencode looks for `.claude`, `.agents` and `AGENTS.md` in every directory
 * from its working directory up (to the git root, or to `/` outside one), not
 * only in $HOME. A lane's work directory sits in the run directory, so with an
 * --out-root under the user's home it found the user's own ~/.claude and
 * ~/.agents skills, and loaded them into the lane's sessions, whatever HOME
 * said. These turn that discovery off: project config and instruction files
 * (1.x and 2.x), `.claude` (both), `.claude` and `.agents` skills (1.x). The
 * bench writes opencode's config to the temp HOME, so it needs none of them.
 */
export const OPENCODE_ISOLATION: Readonly<Record<string, string>> = {
  OPENCODE_DISABLE_PROJECT_CONFIG: "1",
  OPENCODE_DISABLE_CLAUDE_CODE: "1",
  OPENCODE_DISABLE_EXTERNAL_SKILLS: "1",
};

/** opencode 2.x's background service (`opencode serve --service`), shared by every opencode of the user. */
export const OPENCODE_SERVICE = /opencode\S*\s+serve\b.*--service\b/;

/** A process's HOME, from its environment (ps eww: the same user's processes only); null if unreadable. */
function envHome(pid: number): string | null {
  try {
    const out = execFileSync("ps", ["eww", "-o", "command=", "-p", String(pid)], { encoding: "utf8", timeout: 5000 });
    return / HOME=(\S+)/.exec(out)?.[1] ?? null;
  } catch {
    return null;
  }
}

/** The opencode release the bench pins (harness-pins.json's "opencode"), or null. */
export function opencodePin(pinsFile = fileURLToPath(new URL("../../../harness-pins.json", import.meta.url))): string | null {
  const v = readJsonIf<Record<string, string>>(pinsFile)?.opencode;
  return v && /^[0-9][0-9A-Za-z.+-]*$/.test(v) ? v : null;
}

/** The major version in `opencode --version`'s output ("opencode v2.3.0", "1.19.5"); 0 if there's none. */
export function opencodeMajor(version: string | undefined): number {
  const m = /(?:^|\s|v)(\d+)\.\d+\.\d+/.exec(version ?? "");
  return m ? Number(m[1]) : 0;
}

/** Whether `opencode --version`'s output is 2.4 or later: MCP Events always on, with no flag. */
export function eventsWithoutFlag(version: string | undefined): boolean {
  const m = /(?:^|\s|v)(\d+)\.(\d+)\.\d+/.exec(version ?? "");
  if (!m) return false;
  const [major, minor] = [Number(m[1]), Number(m[2])];
  return major > 2 || (major === 2 && minor >= 4);
}

/**
 * Why a lane can't push, or null: WB_OPENCODE_PUSH=1 (no flag) needs an
 * opencode whose Events are on without one; before 2.4, set
 * WB_OPENCODE_FLAGS=mcp_events on a build that has the flag.
 */
export function pushUnavailable(env: Record<string, string | undefined>, version: string | undefined): string | null {
  if (env.WB_OPENCODE_PUSH !== "1" || pushFlag(env.WB_OPENCODE_FLAGS) || eventsWithoutFlag(version)) return null;
  return `${version ?? "this opencode"}: WB_OPENCODE_PUSH=1 needs opencode 2.4 or later, whose MCP Events need no flag (for a build with the mcp_events flag, set WB_OPENCODE_FLAGS=mcp_events)`;
}

/** Whether an OPENCODE_FLAGS value turns on MCP Events (mcp_events). */
export function pushFlag(flags: string | undefined): boolean {
  return (flags ?? "").split(",").some((f) => f.trim() === "mcp_events");
}

/**
 * Whether a lane's opencode subscribes itself with MCP Events push: the
 * flag build (mcp_events in WB_OPENCODE_FLAGS), or opencode 2.4 and later,
 * whose Events are always on with no flag (WB_OPENCODE_PUSH=1).
 */
export function pushLane(env: Record<string, string | undefined>): boolean {
  return pushFlag(env.WB_OPENCODE_FLAGS) || env.WB_OPENCODE_PUSH === "1";
}

/**
 * Whether a lane runs opencode --standalone: every 2.x lane, the 2.4+ push
 * lane included, but not the mcp_events flag build (verified without it).
 */
export function standalone(env: Record<string, string | undefined>, major: number): boolean {
  return !pushFlag(env.WB_OPENCODE_FLAGS) && major >= 2;
}

/**
 * opencode.json's mcp section for one server: 1.x's shape (mcp.<name>)
 * without a protocol, 2.x's (mcp.servers.<name>, which keeps it) with one.
 */
export function serversEntry(name: string, entry: Record<string, unknown>, protocol?: string): Record<string, unknown> {
  return protocol ? { servers: { [name]: { ...entry, protocol } } } : { [name]: entry };
}

export class Opencode extends TmuxDriver implements Driver {
  id = "opencode";
  name = "opencode";
  bin = "opencode";
  zeroInstallWake = false;
  installPath = "plugin" as const;
  harness = "opencode";
  conversation = "none" as const;
  trustStep = /restart/i;

  /** The lane's environment, with opencode's discovery outside the temp HOME off (OPENCODE_ISOLATION). */
  override env(l: Lane, extra: Record<string, string> = {}): Record<string, string> {
    const flags: Record<string, string> = process.env.WB_OPENCODE_FLAGS ? { OPENCODE_FLAGS: process.env.WB_OPENCODE_FLAGS } : {};
    return super.env(l, { ...OPENCODE_ISOLATION, ...flags, ...extra });
  }

  /** WB_OPENCODE_BIN, else opencode on PATH. */
  override binPath(): string | null {
    const b = process.env.WB_OPENCODE_BIN;
    if (b) return existsSync(b) ? b : null;
    return super.binPath();
  }

  /**
   * opencode 2.x only with WB_OPENCODE_2=1. Wakeshift's plugin is a 1.x
   * plugin (a default-exported function), and 2.x loads only a default
   * export with an id and a setup or effect function ("failed to load
   * plugin … Plugin must export a default definition with an id and an
   * effect or setup function", 2.3.0): its install never connects, so a 2.x
   * lane fails S6 onward for a reason the bench already knows. The bench
   * runs the pinned 1.x (harness-pins.json's "opencode"; npm's opencode-ai,
   * what opencode's default install still gives): install-harness.sh opencode
   * <pin>, then WB_OPENCODE_BIN or its directory first on PATH.
   */
  override async check(): Promise<Availability> {
    const a = await super.check();
    const noPush = a.ok ? pushUnavailable(process.env, a.version) : null;
    if (noPush) return { ok: false, version: a.version, reason: noPush };
    // The push lane (R7-R9) runs an opencode with MCP Events and no plugin of ours: 2.4 and later, or the flag build.
    if (!a.ok || this.pushes || opencodeMajor(a.version) < 2 || process.env.WB_OPENCODE_2 === "1") return a;
    return { ok: false, reason: `${a.version}: Wakeshift's plugin doesn't load on opencode 2.x yet; run the pinned 1.x (bench/releases/install-harness.sh opencode ${opencodePin() ?? "<pin>"}, then WB_OPENCODE_BIN), or set WB_OPENCODE_2=1 to run 2.x anyway` };
  }

  /**
   * The lane's leftovers, and an opencode 2.x background service started
   * with the lane's HOME. The lane's own opencode runs --standalone, but any
   * other opencode command run in the lane (the agent's `opencode api …`, in
   * v1.4.57's follow-up run, as it tried to rewrite the plugin for 2.x)
   * starts the shared service, detached: its command line names no HOME, so
   * it's found by its environment, and it would otherwise outlive the lane
   * holding the shared port with the lane's config.
   */
  override leftovers(l: Lane): PsRow[] {
    const out = super.leftovers(l);
    for (const r of ps()) {
      if (out.some((x) => x.pid === r.pid) || !OPENCODE_SERVICE.test(r.command)) continue;
      const home = envHome(r.pid);
      if (home && (home === l.home.dir || home === l.home.real)) out.push(r);
    }
    return out;
  }

  /** The lane's opencode subscribes itself with MCP Events push (pushLane). */
  get pushes(): boolean {
    return pushLane(process.env);
  }

  async version() {
    try {
      const { stdout } = await exec(this.binPath()!, ["--version"], { timeout: 20_000 });
      return stdout.trim();
    } catch {
      return undefined;
    }
  }

  async prepare(l: Lane): Promise<void> {
    if (process.env.WB_NPM_REGISTRY) l.note(`@wakeshift packages from ${process.env.WB_NPM_REGISTRY}`); // the HOME's .npmrc (makeHome)
    const model = process.env.WB_OPENCODE_MODEL ?? OPENCODE_MODEL;
    if (model.startsWith("openai/")) {
      const key = readSecrets(["OPENAI_API_KEY"]);
      if (!key.ok) throw new BlockedError(`${model} needs an OpenAI key: ${key.reason}`);
      writeJson(join(l.home.dir, ".local/share/opencode/auth.json"), { openai: { type: "api", key: key.env.OPENAI_API_KEY } });
      l.note("wrote the temp HOME's opencode auth.json (OpenAI key from the bench's env file)");
    }
    if (process.env.WB_OPENCODE_FLAGS) l.note(`OPENCODE_FLAGS=${process.env.WB_OPENCODE_FLAGS}`);
    const protocol = process.env.WB_OPENCODE_PROTOCOL;
    const config = (mcp?: boolean) =>
      writeJson(join(l.home.dir, ".config/opencode/opencode.json"), {
        $schema: "https://opencode.ai/config.json",
        model,
        autoupdate: false,
        ...(mcp ? { mcp: serversEntry(l.mcpName, mcpServerEntry(l, "opencode"), protocol) } : {}),
      });
    // opencode 2.x's protocol setting (WB_OPENCODE_PROTOCOL): the entry written directly, in 2.x's shape.
    if (protocol) {
      config(true);
      l.note(`added MCP server ${l.mcpName} to opencode.json (mcp.servers, protocol ${protocol})`);
      return;
    }
    // A server with a token (S11): the entry written directly, so the token
    // is never on a command line.
    if (l.mcpToken) {
      config(true);
      l.note(`added MCP server ${l.mcpName} (with its token) to opencode.json`);
      return;
    }
    config();
    if (usePicker(l)) {
      await addWithPicker(l, { harness: "opencode", program: "opencode", run: (args) => l.run(this.env(l), this.binPath()!, args) });
      return;
    }
    // The bench's own command (WB_MCP_ADD=bench).
    const r = await l.run(this.env(l), this.binPath()!, mcpAddArgs(l, "opencode"));
    l.note(`opencode mcp add: ${r.ok ? "ok" : "failed"}`, { out: r.out.slice(-600) });
    // Older versions prompt; write the entry the command would have.
    if (!r.ok) config(true);
  }

  async start(l: Lane, opts: { resume?: boolean } = {}): Promise<void> {
    await this.openPane(l);
    l.state.panePid = await l.tmux.panePid(l.session);
    // opencode 2.x runs its sessions on a background service shared by every
    // opencode of the user (a fixed port, 49374), started with whichever
    // HOME and config the first one had: a lane must run its own
    // (--standalone), or it would use, or leave behind, a service with
    // another HOME's config.
    // (Not the mcp_events flag build, which ran as verified without it; a
    // 2.4+ push lane, WB_OPENCODE_PUSH=1, is standalone like any 2.x lane.)
    const own = standalone(process.env, opencodeMajor(await this.version()));
    if (own && !opts.resume) l.note("opencode 2.x: --standalone (a private server for the lane, not the shared background service)");
    await this.shell(l, [this.binPath()!, ...(own ? ["--standalone"] : []), ...(opts.resume ? ["--continue"] : [])]);
    const ok = await l.waitFor(/ctrl\+p|tab\s+agents?|Ask anything|enter send|\/help/i, 60_000, { allowInstall: false });
    if (ok === null) await this.checkLogin(l, "its model's provider needs a login");
    if (ok === null) await this.didntStart(l, "opencode didn't reach its start screen", opts);
    await sleep(3000);
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

  async answer(l: Lane, screen: string, policy: AnswerPolicy): Promise<string | null> {
    const t = tail(screen, 14);
    // Permission prompt: "Allow once / Allow always / Reject".
    if (/Allow once|Allow always|Permission required/i.test(t)) {
      if (INSTALLER.test(t) && !policy.allowInstall) {
        await l.tmux.keys(l.session, "Escape");
        return "denied the installer (the user said no)";
      }
      const options = ["Allow once", "Allow always", "Reject"].filter((o) => t.includes(o));
      // opencode's "Allow always" lasts until opencode restarts (it's kept in
      // memory, not written to opencode.json: OPENCODE_ALWAYS_SCOPE), so it's
      // the session's option. Arrow to it if it's there, else Enter on the default (once).
      const always = /Allow always/i.test(t);
      if (always) {
        await l.tmux.keys(l.session, "Right");
        await sleep(150);
      }
      await l.tmux.keys(l.session, "Enter");
      l.prompted({
        kind: "approval",
        asks: dialogAsks(t, options[0]),
        options: options.map((o) => (o === "Allow always" ? `${o} (until opencode restarts)` : o)),
        chosen: always ? "Allow always" : "Allow once",
        scope: always ? OPENCODE_ALWAYS_SCOPE : "once",
        sessionOffered: always && OPENCODE_ALWAYS_SCOPE === "session",
      });
      return `approved ${INSTALLER.test(t) ? "the installer" : "a tool"}${always ? " (always)" : ""}`;
    }
    return null;
  }

  /**
   * The agent's question tool shows a picker that takes the next Enter as
   * its first option ("Yes, install"): dismiss it, so the user's typed
   * answer reaches the agent as a message.
   */
  async beforeSay(l: Lane, screen: string): Promise<string | null> {
    // An open picker ends with its key help ("↑↓ select  enter submit  esc
    // dismiss"); an answered one stays in the transcript as "# Questions",
    // so that's not a sign. It can show a moment after the turn looks done.
    const open = (s: string) => PICKER.test(tail(s, 8));
    for (let i = 0; i < 4; i++) {
      if (open(screen)) {
        await l.tmux.keys(l.session, "Escape");
        await sleep(1200);
        if (!open(await l.screen(200))) return "dismissed opencode's question picker, to answer in a message";
      }
      await sleep(1000);
      screen = await l.screen(200);
    }
    return open(screen) ? "opencode's question picker didn't close" : null;
  }

  override busy(screen: string): boolean {
    return /esc (to )?interrupt|working…|Generating/i.test(tail(screen, 8));
  }

  /** opencode loads plugins when it starts: restart it, continuing the session. */
  async afterInstall(l: Lane): Promise<string[]> {
    await l.waitIdle({ allowInstall: true }, 60_000);
    await this.stop(l);
    await this.start(l, { resume: true });
    return ["restarted opencode (--continue) to load the plugin"];
  }

  waiters(l: Lane, rows: PsRow[]): PsRow[] {
    return this.mine(l, rows).filter((r) => /wakeshift bridge --harness opencode|bridge --harness opencode|\/v1\/stream/.test(r.command));
  }

  installed(l: Lane): string[] {
    return this.mentions(l, [".config/opencode/plugins/wakeshift.ts", ".config/opencode/opencode.json", ".wakeshift/opencode", ".wakeshift/bin/wakeshift"]);
  }

  async uninstall(l: Lane): Promise<void> {
    await this.uninstallHarness(l, "opencode");
    // The npm plugin: opencode has no command to remove it, so the user
    // deletes its entry from the plugin list, as the uninstall note says.
    const file = join(l.home.dir, ".config/opencode/opencode.json");
    if (!existsSync(file)) return;
    const cfg = JSON.parse(readFileSync(file, "utf8"));
    if (!Array.isArray(cfg.plugin)) return;
    const ours = (p: unknown) => String(Array.isArray(p) ? p[0] : p).startsWith("@wakeshift/opencode");
    if (!cfg.plugin.some(ours)) return;
    cfg.plugin = cfg.plugin.filter((p: unknown) => !ours(p));
    if (!cfg.plugin.length) delete cfg.plugin;
    writeFileSync(file, JSON.stringify(cfg, null, 2) + "\n");
    rmSync(join(l.home.dir, ".wakeshift/opencode"), { recursive: true, force: true });
    l.note("removed @wakeshift/opencode from opencode.json's plugin list, and ~/.wakeshift/opencode");
  }
}
