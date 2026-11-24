// Processes and tmux sessions the bench started: recorded (also to
// <run>/pids.json, so a crash leaves a record), and the only ones it ever
// kills. Nothing here kills by name, pattern or port.

import { execFileSync, spawn, type ChildProcess, type SpawnOptions } from "node:child_process";
import { writeFileSync } from "node:fs";

export interface Owned {
  pid: number;
  what: string;
  at: string;
}

export class Procs {
  readonly owned = new Map<number, Owned>();
  readonly sessions = new Set<string>();
  constructor(
    private file: string | null,
    readonly tmuxSocket: string,
  ) {}

  private save() {
    if (!this.file) return;
    writeFileSync(this.file, JSON.stringify({ tmuxSocket: this.tmuxSocket, sessions: [...this.sessions], containers: [...this.containers], pids: [...this.owned.values()] }, null, 1) + "\n");
  }

  record(pid: number | undefined, what: string): void {
    if (!pid) return;
    this.owned.set(pid, { pid, what, at: new Date().toISOString() });
    this.save();
  }

  /** Docker containers the bench started (wb-<run>-<lane>): removed by name at the end. */
  readonly containers = new Set<string>();

  recordContainer(name: string): void {
    if (!/^wb-\d{8}-\d{6}-[0-9a-f]{4}-[a-z0-9-]+$/.test(name)) throw new Error(`not a bench container name: ${name}`);
    this.containers.add(name);
    this.save();
  }

  recordSession(name: string): void {
    this.sessions.add(name);
    this.save();
  }

  /** Spawns a child and records it. */
  spawn(cmd: string, args: string[], opts: SpawnOptions, what: string): ChildProcess {
    const c = spawn(cmd, args, opts);
    this.record(c.pid, what);
    c.on("exit", () => {
      this.owned.delete(c.pid!);
      this.save();
    });
    return c;
  }

  /** Kills one process the bench can attribute to itself (recorded, or attributed by the caller). */
  kill(pid: number, why: string, signal: NodeJS.Signals = "SIGTERM"): boolean {
    try {
      process.kill(pid, signal);
      this.record(pid, `killed (${why})`);
      return true;
    } catch {
      return false;
    }
  }

  async killAll(): Promise<void> {
    for (const name of [...this.sessions]) {
      try {
        execFileSync("tmux", ["-L", this.tmuxSocket, "kill-session", "-t", `=${name}`], { stdio: "ignore" });
      } catch {
        // Already gone.
      }
      this.sessions.delete(name);
    }
    for (const name of [...this.containers]) {
      try {
        execFileSync("docker", ["rm", "-f", name], { stdio: "ignore", timeout: 60_000 });
      } catch {
        // Never started, or gone.
      }
      this.containers.delete(name);
    }
    const pids = [...this.owned.keys()];
    for (const pid of pids) {
      try {
        process.kill(pid, "SIGTERM");
      } catch {
        this.owned.delete(pid);
      }
    }
    await new Promise((r) => setTimeout(r, 1500));
    for (const pid of [...this.owned.keys()]) {
      try {
        process.kill(pid, 0);
        process.kill(pid, "SIGKILL");
      } catch {
        // Gone.
      }
      this.owned.delete(pid);
    }
    this.save();
  }
}

export interface PsRow {
  pid: number;
  ppid: number;
  command: string;
}

export function ps(): PsRow[] {
  const out = execFileSync("ps", ["-Ao", "pid=,ppid=,command="], { encoding: "utf8", maxBuffer: 32 << 20 });
  const rows: PsRow[] = [];
  for (const line of out.split("\n")) {
    const m = /^\s*(\d+)\s+(\d+)\s+(.*)$/.exec(line);
    if (m) rows.push({ pid: Number(m[1]), ppid: Number(m[2]), command: m[3] });
  }
  return rows;
}

/** Every descendant of pid (not pid itself). */
export function descendants(pid: number, rows: PsRow[] = ps()): PsRow[] {
  const kids = new Map<number, PsRow[]>();
  for (const r of rows) {
    if (!kids.has(r.ppid)) kids.set(r.ppid, []);
    kids.get(r.ppid)!.push(r);
  }
  const out: PsRow[] = [];
  const stack = [pid];
  while (stack.length) {
    const p = stack.pop()!;
    for (const k of kids.get(p) ?? []) {
      out.push(k);
      stack.push(k.pid);
    }
  }
  return out;
}

/**
 * Processes the bench can attribute to a lane: descendants of its pane, or
 * processes whose command line names the lane's temp HOME (a detached
 * connector started by a hook, a daemon). Only these are ever killed.
 */
export function attributable(panePid: number | null, home: string, rows: PsRow[] = ps()): PsRow[] {
  const out = new Map<number, PsRow>();
  if (panePid) for (const r of descendants(panePid, rows)) out.set(r.pid, r);
  if (home.length > 8) for (const r of rows) if (r.command.includes(home + "/") || r.command.endsWith(home)) out.set(r.pid, r);
  out.delete(process.pid);
  for (const [pid, r] of out) if (/^(\S*\/)?tmux\b/.test(r.command)) out.delete(pid);
  return [...out.values()];
}
