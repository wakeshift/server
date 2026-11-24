// Claude Code through the Claude Agent SDK, as an SDK host (T3 Code, Zed)
// drives it: the claude-code lane's temp HOME, login, MCP server
// and plugin (./index.ts, prepare), and in the lane's pane a small client
// (sdk-chat.mjs) instead of claude's terminal UI. The client runs the
// pinned SDK's own Claude Code (the SDK bundles one), with streaming input,
// user settings, the claude_code presets and CLAUDE_CODE_ENTRYPOINT set, so
// hooks see what they see under T3 Code: CLAUDE_CODE_SESSION_ATTENDED=0
// and --input-format stream-json on Claude's command line.
//
// Install it once: bench/releases/install-harness.sh claude-agent-sdk <pin>
// (the pin is harness-pins.json's "claude-agent-sdk"), or point
// WB_CLAUDE_SDK_DIR at a directory with the SDK in node_modules.
//
// Options:
//   WB_CLAUDE_SDK_MODE        streaming (default: a session that stays open) or
//                             single (each line a one-shot query resuming the last)
//   WB_CLAUDE_SDK_ENTRYPOINT  t3-code (default), or default (the SDK's own, sdk-ts)
//   WB_CLAUDE_SDK_PERMISSION_MODE  default (the default: every tool asks, as
//                             T3 Code's approvals), or another SDK permission mode
//
// A plugin installed mid-session loads when the session restarts: an SDK
// host has no /reload-plugins, so after the install the lane resumes the
// session (a new CLI process, as a host reopening the thread). Wakes are
// counted from the session's transcripts, as for the terminal lane, and the
// client marks each turn nobody typed ("◆ woken").

import { existsSync } from "node:fs";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { stateRoot } from "../../src/benchstate.js";
import { readJsonIf } from "../../src/home.js";
import type { Lane } from "../../src/lane.js";
import { sleep } from "../../src/tmux.js";
import type { AnswerPolicy, Availability, Driver } from "../types.js";
import { ClaudeCode, claudeModel, newestSession, pluginInstalled, tokenExport } from "./index.js";

/** The client's prompt: the last line while it's idle. */
export const SDK_PROMPT = /^❯\s*$/;

/**
 * The client's open dialog (sdk-chat.mjs): from its first line ("Permission
 * requested", or an AskUserQuestion's "☐ <header>") to its footer, which
 * must be the screen's last line; "" if none is open. The client erases a
 * dialog once it's answered, and the transcript above it (the tool call,
 * the agent's quote of a result) isn't the dialog: an installer named
 * there isn't what the dialog asks to run.
 */
export function sdkDialog(screen: string): string {
  const lines = screen.replace(/\s+$/, "").split("\n");
  if (!/^(Esc to cancel|Enter to select\b.*)$/.test(lines[lines.length - 1]?.trim() ?? "")) return "";
  for (let i = lines.length - 1; i >= 0; i--) if (/^(Permission requested|☐ .*)$/.test(lines[i].trim())) return lines.slice(i).join("\n");
  return "";
}

/** Whether the client is mid-turn: anything but its prompt is the last line it drew. */
export function sdkChatBusy(screen: string): boolean {
  const lines = screen.replace(/\s+$/, "").split("\n");
  const last = lines[lines.length - 1] ?? "";
  return !SDK_PROMPT.test(last) && !/^❯ \S/.test(last);
}

/** The lane's options (WB_CLAUDE_SDK_*), checked. */
export function sdkOptions(env: NodeJS.ProcessEnv = process.env): { mode: "streaming" | "single"; entrypoint: string; permissionMode: string } {
  const mode = (env.WB_CLAUDE_SDK_MODE ?? "streaming").trim() || "streaming";
  if (mode !== "streaming" && mode !== "single") throw new Error(`WB_CLAUDE_SDK_MODE is streaming or single, not ${mode}`);
  const entrypoint = (env.WB_CLAUDE_SDK_ENTRYPOINT ?? "t3-code").trim() || "t3-code";
  if (!/^[a-z0-9-]{1,40}$/.test(entrypoint)) throw new Error(`WB_CLAUDE_SDK_ENTRYPOINT: not an entrypoint name: ${entrypoint}`);
  const permissionMode = (env.WB_CLAUDE_SDK_PERMISSION_MODE ?? "default").trim() || "default";
  if (!/^[A-Za-z]{1,30}$/.test(permissionMode)) throw new Error(`WB_CLAUDE_SDK_PERMISSION_MODE: not a mode: ${permissionMode}`);
  return { mode, entrypoint, permissionMode };
}

/** The pinned Agent SDK version (harness-pins.json's "claude-agent-sdk"). */
export function sdkPin(pinsFile = fileURLToPath(new URL("../../../harness-pins.json", import.meta.url))): string {
  const v = readJsonIf<Record<string, string>>(pinsFile)?.["claude-agent-sdk"];
  if (!v || !/^[0-9][0-9A-Za-z.+-]*$/.test(v)) throw new Error(`no "claude-agent-sdk" version in ${pinsFile}`);
  return v;
}

/** Where the SDK is installed: WB_CLAUDE_SDK_DIR, else the bench's state for the pin. */
export function sdkDir(env: NodeJS.ProcessEnv = process.env): string {
  return env.WB_CLAUDE_SDK_DIR || join(stateRoot(env), "claude-agent-sdk", sdkPin());
}

/** The SDK's module and the Claude Code it bundles (macOS arm64, the bench's machines). */
export function sdkPaths(dir = sdkDir()): { module: string; claude: string } {
  const nm = join(dir, "node_modules", "@anthropic-ai");
  return { module: join(nm, "claude-agent-sdk", "sdk.mjs"), claude: join(nm, "claude-agent-sdk-darwin-arm64", "claude") };
}

/**
 * The lane's version as reported: Claude Code's, with the client's own
 * permission mode (WB_CLAUDE_SDK_PERMISSION_MODE, not the terminal lane's
 * WB_CLAUDE_PERMISSION_MODE that ClaudeCode's version names), then the SDK's.
 */
export function sdkVersion(claude: string, env: NodeJS.ProcessEnv = process.env): string {
  const o = sdkOptions(env);
  const mode = o.permissionMode === "default" ? "manual mode" : `${o.permissionMode} mode`;
  return `${claude.replace(/ · [^·]*mode$/, "")} · ${mode} · Agent SDK ${sdkPin()}, ${o.mode} input, ${o.entrypoint}`;
}

export class ClaudeCodeSdk extends ClaudeCode implements Driver {
  override id = "claude-code-sdk";
  override name = "Claude Code (Agent SDK)";

  /** The SDK's own Claude Code: prepare's `claude mcp add` and plugin install run it too. */
  override binPath(): string | null {
    const { claude, module } = sdkPaths();
    return existsSync(claude) && existsSync(module) ? claude : null;
  }

  override async check(): Promise<Availability> {
    if (!this.binPath()) return { ok: false, reason: `the Claude Agent SDK isn't installed: bench/releases/install-harness.sh claude-agent-sdk ${sdkPin()} (or WB_CLAUDE_SDK_DIR)` };
    return super.check();
  }

  override async version() {
    const v = await super.version();
    return v ? sdkVersion(v) : v;
  }

  override async versionIn(l: Lane): Promise<string | null> {
    const v = await super.versionIn(l);
    return v ? sdkVersion(v) : v;
  }

  dialogOf(screen: string): string {
    return sdkDialog(screen);
  }

  /** Only an open dialog, read on its own (sdkDialog): Claude Code's answers, given just its lines. */
  override async answer(l: Lane, screen: string, policy: AnswerPolicy): Promise<string | null> {
    const dialog = sdkDialog(screen);
    return dialog ? super.answer(l, dialog, policy) : null;
  }

  override busy(screen: string): boolean {
    return sdkChatBusy(screen);
  }

  override async start(l: Lane, opts: { resume?: boolean } = {}): Promise<void> {
    await this.openPane(l, { DISABLE_AUTOUPDATER: "1" });
    l.state.panePid = await l.tmux.panePid(l.session);
    const token = (l.state.claudeToken as string | null | undefined) ?? null;
    if (token) {
      await l.tmux.type(l.session, tokenExport(token));
      await sleep(300);
    }
    const o = sdkOptions();
    const model = claudeModel();
    const { module, claude } = sdkPaths();
    const id = opts.resume ? newestSession(l.home.dir) : null;
    if (!opts.resume) l.note(`Agent SDK ${sdkPin()}: ${o.mode} input, CLAUDE_CODE_ENTRYPOINT ${o.entrypoint === "default" ? "unset (sdk-ts)" : o.entrypoint}, permission mode ${o.permissionMode}; model: ${model ?? "the account's default"}`);
    else l.note(id ? "resumed the session (a new CLI process, resume: its ID)" : "no session to resume: started a new one");
    const client = join(l.ctx.repo, "bench", "drivers", "claude-code", "sdk-chat.mjs");
    await this.shell(l, [process.execPath, client, "--sdk", module, "--claude", claude, "--cwd", l.home.work, "--mode", o.mode, "--entrypoint", o.entrypoint,
      "--permission-mode", o.permissionMode, ...(model ? ["--model", model] : []), ...(id ? ["--resume", id] : [])]);
    const ok = await l.waitFor(/^❯\s*$/m, 60_000, { allowInstall: false });
    if (ok === null) await this.didntStart(l, "the Agent SDK client didn't reach its prompt", opts);
    await this.checkModel(l, model);
    await sleep(2000);
  }

  /** Closes the session's input ("/exit"): the client gives the CLI 20 seconds to end, then stops it. */
  override async stop(l: Lane): Promise<void> {
    await l.tmux.type(l.session, "/exit");
    if (!(await this.waitShell(l, 40_000))) {
      await l.tmux.keys(l.session, "C-c");
      await this.waitShell(l, 10_000);
    }
  }

  /** A plugin installed from the shell loads when the session starts again: the lane resumes it. */
  override async afterInstall(l: Lane): Promise<string[]> {
    if (!pluginInstalled(l)) return [];
    if (!(await l.waitIdle({ allowInstall: false }, 120_000))) return ["the turn didn't end within 2 minutes; the plugin loads at the session's next start"];
    await this.stop(l);
    await this.start(l, { resume: true });
    return ["resumed the session so it loads the plugin (an SDK host has no /reload-plugins)"];
  }
}
