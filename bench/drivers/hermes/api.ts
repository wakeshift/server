// Hermes Agent through its OpenAI-compatible API server, as an app that uses
// Hermes's API talks to it: the same container and gateway as the
// hermes lane (./index.ts), with the API server on (API_SERVER_KEY, random
// per run), and in the lane's pane a small client (api-chat.py, run inside
// the container) that POSTs each message to /v1/chat/completions with
// X-Hermes-Session-Id, so every message continues one Hermes session. The
// plugin wakes an API server session with a run on /v1/runs, which this
// client never shows (an app sees it only in the session's history), so
// wakes are counted from report_seen and from Hermes's state.db, as for the
// classic CLI.
//
// The install needs no restart: the gateway loads the plugin, and an API
// server chat runs in the gateway. WB_HERMES_API_RESTART=1 restarts the
// gateway after it anyway, as a user might. S8 restarts the
// gateway (the container) and continues the session: the plugin listens
// again for API server sessions when the gateway starts.

import { execFile } from "node:child_process";
import { existsSync, readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { promisify } from "node:util";
import type { Lane } from "../../src/lane.js";
import { ContainerDriver, type ContainerSpec } from "../container.js";
import type { Driver } from "../types.js";
import { Hermes, hermesSpec } from "./index.js";

const exec = promisify(execFile);

/** Where the client goes in the container. */
export const API_CHAT_PATH = "/tmp/wakebench-api-chat.py";

/** What the client shows while a request is in flight (api-chat.py's BUSY). */
export const API_CHAT_BUSY = /waiting for Hermes's API server/;

/** The Hermes session the lane's client continues: one per lane and run. */
export function apiSession(l: Lane): string {
  return `wakebench-${l.ctx.run}-${l.id}`;
}

/** Whether the client is waiting for a reply: its status line is the last line drawn. */
export function apiChatBusy(screen: string): boolean {
  const last = screen.replace(/\s+$/, "").split("\n").pop() ?? "";
  return API_CHAT_BUSY.test(last);
}

/** A .env's lines without the ones that set API_SERVER_KEY. */
export function withoutApiKey(lines: string[]): string[] {
  return lines.filter((x) => !/^\s*(export\s+)?API_SERVER_KEY\s*=/.test(x));
}

export class HermesApi extends Hermes implements Driver {
  override id = "hermes-api";
  override name = "Hermes Agent (API server)";
  override spec: ContainerSpec = {
    ...hermesSpec(),
    // The API server starts only with a usable key (gateway/config_env.py);
    // the plugin posts woken runs to it with the same key.
    generated: ["API_SERVER_KEY"],
    chat: ["/opt/hermes/.venv/bin/python", API_CHAT_PATH],
    // Continuing is the same command: the session ID is the conversation.
    resume: [],
  };

  override get paneLog(): boolean | undefined {
    return undefined;
  }

  override get gutter(): RegExp | undefined {
    return undefined;
  }

  override get volatile(): RegExp | undefined {
    return API_CHAT_BUSY;
  }

  override chat(l: Lane): string[] {
    return [...this.spec.chat, apiSession(l)];
  }

  override async versionIn(l: Lane): Promise<string | null> {
    const v = await ContainerDriver.prototype.versionIn.call(this, l);
    return v ? `${v} · API server` : v;
  }

  /**
   * The run's key, not the state's: `hermes setup` writes an API_SERVER_KEY
   * of its own into $HERMES_HOME/.env, which wins over the container's
   * environment, so the run's copy drops that line (its value is never read).
   */
  override async configure(l: Lane): Promise<void> {
    await super.configure(l);
    const dir = this.stateDir(l);
    const env = dir ? join(dir, ".env") : "";
    if (!env || !existsSync(env)) return;
    const lines = readFileSync(env, "utf8").split("\n");
    const kept = withoutApiKey(lines);
    if (kept.length !== lines.length) {
      writeFileSync(env, kept.join("\n"), { mode: 0o600 });
      l.note("dropped the state's API_SERVER_KEY from the run's $HERMES_HOME/.env: the run's own key is in the container's environment");
    }
  }

  override async prepare(l: Lane): Promise<void> {
    await super.prepare(l);
    // The client, into the container's own filesystem (kept across restarts).
    await exec("docker", ["cp", join(l.ctx.repo, "bench", "drivers", "hermes", "api-chat.py"), `${this.container(l)}:${API_CHAT_PATH}`], { timeout: 60_000 });
    l.note(`API server client in ${this.container(l)}: X-Hermes-Session-Id ${apiSession(l)}`);
  }

  override busy(screen: string): boolean {
    return apiChatBusy(screen);
  }

  /** The client never shows a run Hermes starts in the session: every wake after the install is unpainted. */
  override unpaintedWakes(l: Lane): boolean {
    return this.installed(l).length > 0;
  }

  /**
   * No restart: the API server runs in the gateway, which loads the plugin
   * at the install. WB_HERMES_API_RESTART=1 restarts the gateway (the
   * container) and continues the session, as a user might.
   */
  override async afterInstall(l: Lane): Promise<string[]> {
    await l.waitIdle({ allowInstall: true }, 60_000);
    const notes = await this.overlayPlugin(l);
    if (process.env.WB_HERMES_API_RESTART !== "1") return [...notes, "no restart: the API server's chats run in the gateway, which loaded the plugin"];
    await this.stop(l);
    await this.start(l, { resume: true });
    return [...notes, "restarted the gateway and continued the session (WB_HERMES_API_RESTART)"];
  }
}
