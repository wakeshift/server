// pi (earendil-works/pi, the terminal coding agent), in a temp HOME with its
// agent directory there too (PI_CODING_AGENT_DIR, configEnv), on an OpenAI
// model with the key from the bench's env file (OPENAI_API_KEY), written to
// the temp agent directory's auth.json (0600), never on a command line. Set
// WB_PI_MODEL to change the model, WB_PI_BIN to run another pi than the one
// on PATH (bench/releases/install-harness.sh pi <version> puts a release in
// the bench's state). Nothing wakes pi without the install: messages wait
// for the next call. Install: Wakeshift's extension in
// ~/.pi/agent/extensions/wakeshift.ts, or, where the core offers it
// (WB_PI_PLUGIN), the npm package @wakeshift/pi in settings.json's packages
// (pi install npm:…), from WB_NPM_REGISTRY (a local registry holding
// packages packed for this core); pi loads either at start and on /reload.

import { execFile } from "node:child_process";
import { existsSync, readFileSync, rmSync } from "node:fs";
import { join } from "node:path";
import { promisify } from "node:util";
import { readSecrets } from "../../src/benchstate.js";
import { writeJson } from "../../src/home.js";
import type { Lane } from "../../src/lane.js";
import type { PsRow } from "../../src/procs.js";
import { sleep } from "../../src/tmux.js";
import { addWithPicker, BlockedError, mcpAddArgs, tail, TmuxDriver, usePicker } from "../base.js";
import type { Driver } from "../types.js";

const exec = promisify(execFile);
export const PI_MODEL = "openai/gpt-6-luna";

/** pi's editor border while a run is going ("── ⠼ Working ──"). */
export const PI_BUSY = /─\s*\S?\s*Working\b/;
/** pi's screen once it takes input: its key help, or its footer (context use and model). */
export const PI_READY = /escape interrupt|\/ commands · ! bash|\d+(?:\.\d+)?%\/\d+k/;

export class Pi extends TmuxDriver implements Driver {
  id = "pi";
  name = "pi";
  bin = "pi";
  zeroInstallWake = false;
  installPath = "plugin" as const;
  harness = "pi";
  conversation = "none" as const;
  trustStep = /\/reload|pi --continue|restart/i;

  binPath(): string | null {
    const b = process.env.WB_PI_BIN;
    if (b) return existsSync(b) ? b : null;
    return super.binPath();
  }

  async version() {
    try {
      // Its own temp-free check: --version reads nothing of the user's.
      const { stdout } = await exec(this.binPath()!, ["--version"], { timeout: 20_000, env: { PATH: process.env.PATH ?? "", HOME: "/nonexistent", PI_SKIP_VERSION_CHECK: "1", PI_TELEMETRY: "0" } });
      return stdout.trim();
    } catch {
      return undefined;
    }
  }

  env(l: Lane, extra: Record<string, string> = {}): Record<string, string> {
    // No update check or telemetry from the bench's runs.
    return super.env(l, { PI_SKIP_VERSION_CHECK: "1", PI_TELEMETRY: "0", ...extra });
  }

  private async preparePi(l: Lane): Promise<void> {
    const agent = join(l.home.dir, ".pi/agent");
    const key = readSecrets(["OPENAI_API_KEY"]);
    if (!key.ok) throw new BlockedError(`pi needs an OpenAI key: ${key.reason}`);
    writeJson(join(agent, "auth.json"), { openai: { type: "api_key", key: key.env.OPENAI_API_KEY } });
    l.note("wrote the temp agent directory's auth.json (OpenAI key from the bench's env file)");
    // A server with a token (S11): the entry written directly, so the token
    // is never on a command line.
    if (l.mcpToken) {
      writeJson(join(agent, "mcp.json"), { mcpServers: { [l.mcpName]: { url: l.mcpUrl, headers: { Authorization: `Bearer ${l.mcpToken}` } } } });
      l.note(`added MCP server ${l.mcpName} (with its token) to mcp.json`);
      return;
    }
    if (usePicker(l)) {
      await addWithPicker(l, { harness: "pi", program: "pi", run: (args) => l.run(this.env(l), this.binPath()!, args) });
      return;
    }
    // The bench's own command (WB_MCP_ADD=bench), with mcp.json written where it fails.
    const r = await l.run(this.env(l), this.binPath()!, mcpAddArgs(l, "opencode"));
    l.note(`pi mcp add: ${r.ok ? "ok" : "failed"}`, { out: r.out.slice(-600) });
    if (!r.ok) writeJson(join(agent, "mcp.json"), { mcpServers: { [l.mcpName]: { url: l.mcpUrl } } });
  }

  async start(l: Lane, opts: { resume?: boolean } = {}): Promise<void> {
    await this.openPane(l);
    l.state.panePid = await l.tmux.panePid(l.session);
    await this.shell(l, [this.binPath()!, "--model", process.env.WB_PI_MODEL ?? PI_MODEL, ...(opts.resume ? ["--continue"] : [])]);
    // The first start in a fresh HOME downloads ripgrep and fd into the agent directory.
    const ok = await l.waitFor(PI_READY, 90_000, { allowInstall: false });
    if (ok === null) await this.checkLogin(l, "its model needs an API key");
    if (ok === null) await this.didntStart(l, "pi didn't reach its start screen", opts);
    await sleep(2000);
  }

  async stop(l: Lane): Promise<void> {
    await l.tmux.keys(l.session, "Escape");
    await sleep(300);
    await l.tmux.type(l.session, "/quit");
    if (!(await this.waitShell(l, 10_000))) {
      await l.tmux.keys(l.session, "C-d");
      await sleep(500);
      await l.tmux.keys(l.session, "C-c");
      await sleep(300);
      await l.tmux.keys(l.session, "C-c");
      await this.waitShell(l, 10_000);
    }
  }

  override busy(screen: string): boolean {
    return PI_BUSY.test(tail(screen, 10));
  }

  /** pi loads extensions when a session starts and on /reload: the user types it, as the install note says. */
  async afterInstall(l: Lane): Promise<string[]> {
    await l.waitIdle({ allowInstall: true }, 60_000);
    await l.tmux.type(l.session, "/reload");
    await sleep(4000);
    return ["typed /reload in pi to load the extension"];
  }

  waiters(l: Lane, rows: PsRow[]): PsRow[] {
    return this.mine(l, rows).filter((r) => /bridge --harness pi\b/.test(r.command));
  }

  installed(l: Lane): string[] {
    return this.mentions(l, [".pi/agent/extensions/wakeshift.ts", ".pi/agent/settings.json", ".pi/agent/npm/node_modules/@wakeshift/pi", ".wakeshift/pi", ".wakeshift/bin/wakeshift"]);
  }

  async prepare(l: Lane): Promise<void> {
    if (process.env.WB_NPM_REGISTRY) l.note(`@wakeshift packages from ${process.env.WB_NPM_REGISTRY}`); // the HOME's .npmrc (makeHome)
    await this.preparePi(l);
  }

  async uninstall(l: Lane): Promise<void> {
    await this.uninstallHarness(l, "pi");
    // The npm package: pi's own command removes it, as the uninstall note says.
    let settings = "";
    try {
      settings = readFileSync(join(l.home.dir, ".pi/agent/settings.json"), "utf8");
    } catch {
      return;
    }
    if (!settings.includes("@wakeshift/pi")) return;
    const r = await l.run(this.env(l), this.binPath()!, ["remove", "npm:@wakeshift/pi"]);
    l.note(`pi remove npm:@wakeshift/pi: ${r.ok ? "ok" : "failed"}`, { out: r.out.slice(-1000) });
    rmSync(join(l.home.dir, ".wakeshift/pi"), { recursive: true, force: true });
  }
}
