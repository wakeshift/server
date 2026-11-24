// tmux, on the bench's own socket (tmux -L <socket>): the user's tmux server
// is never touched. Sessions are named wb-<run>-<harness> and recorded.

import { execFile, execFileSync } from "node:child_process";
import { randomBytes } from "node:crypto";
import { promisify } from "node:util";
import { stripAnsi } from "./ansi.js";
import type { Procs } from "./procs.js";

const run = promisify(execFile);

export class Tmux {
  constructor(
    readonly socket: string,
    private conf: string,
    private procs: Procs,
  ) {}

  private args(a: string[]): string[] {
    return ["-L", this.socket, "-f", this.conf, ...a];
  }

  async tmux(...a: string[]): Promise<string> {
    const { stdout } = await run("tmux", this.args(a), { maxBuffer: 64 << 20 });
    return stdout;
  }

  tmuxSync(...a: string[]): string {
    return execFileSync("tmux", this.args(a), { encoding: "utf8", maxBuffer: 64 << 20 });
  }

  /** Starts a detached session running argv (an interactive shell, usually). */
  async newSession(name: string, cwd: string, argv: string[], size = { x: 200, y: 50 }): Promise<void> {
    this.procs.recordSession(name);
    await this.tmux("new-session", "-d", "-s", name, "-x", String(size.x), "-y", String(size.y), "-c", cwd, ...argv);
  }

  async exists(name: string): Promise<boolean> {
    try {
      await this.tmux("has-session", "-t", `=${name}`);
      return true;
    } catch {
      return false;
    }
  }

  async kill(name: string): Promise<void> {
    try {
      await this.tmux("kill-session", "-t", `=${name}`);
    } catch {
      // Gone already.
    }
    this.procs.sessions.delete(name);
  }

  /** Appends everything the pane prints to file. */
  async pipe(name: string, file: string): Promise<void> {
    await this.tmux("pipe-pane", "-t", `=${name}:`, "-o", `cat >> '${file.replace(/'/g, "'\\''")}'`);
  }

  async panePid(name: string): Promise<number | null> {
    try {
      const out = await this.tmux("display-message", "-p", "-t", `=${name}:`, "#{pane_pid}");
      const n = Number(out.trim());
      return n > 0 ? n : null;
    } catch {
      return null;
    }
  }

  /** The pane with ANSI escapes, wrapped lines joined, and `history` lines of scrollback. */
  async capture(name: string, history = 2000): Promise<string> {
    return this.tmux("capture-pane", "-p", "-e", "-J", "-S", `-${history}`, "-t", `=${name}:`);
  }

  /** The pane as plain text (for matching). */
  async text(name: string, history = 2000): Promise<string> {
    return stripAnsi(await this.capture(name, history));
  }

  /** Types text literally, then presses Enter (separately: TUIs treat a fast Enter as a newline). */
  async type(name: string, text: string, enter = true): Promise<void> {
    await this.tmux("send-keys", "-t", `=${name}:`, "-l", text);
    if (enter) {
      await sleep(400);
      await this.tmux("send-keys", "-t", `=${name}:`, "Enter");
    }
  }

  /**
   * Pastes a secret into a pane: loaded into a tmux buffer from stdin, never
   * an argument (other processes can read arguments) nor a file, pasted, and
   * the buffer deleted. Whatever reads it in the pane must have echo off.
   */
  async pasteSecret(name: string, text: string): Promise<void> {
    const buf = `wb-secret-${randomBytes(6).toString("hex")}`;
    try {
      execFileSync("tmux", this.args(["load-buffer", "-b", buf, "-"]), { input: text, stdio: ["pipe", "ignore", "ignore"] });
      await this.tmux("paste-buffer", "-d", "-b", buf, "-t", `=${name}:`);
    } finally {
      try {
        await this.tmux("delete-buffer", "-b", buf);
      } catch {
        // Pasted and deleted (-d).
      }
    }
  }

  async keys(name: string, ...keys: string[]): Promise<void> {
    await this.tmux("send-keys", "-t", `=${name}:`, ...keys);
  }
}

export const sleep = (ms: number) => new Promise<void>((r) => setTimeout(r, ms));
