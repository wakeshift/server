// Temp HOMEs and borrowed logins. A harness runs with HOME set to its own
// directory under the run; logins are borrowed read-only: the login keychain
// is symlinked at $HOME/Library/Keychains/login.keychain-db (the real
// keychain search list is never edited), and only non-secret metadata is
// copied (Claude Code's account record, never a token).

import { accessSync, constants, existsSync, mkdirSync, readFileSync, realpathSync, symlinkSync, writeFileSync } from "node:fs";
import { homedir } from "node:os";
import { basename, delimiter, dirname, join } from "node:path";

export const REAL_HOME = homedir();

export interface Home {
  dir: string;
  /** realpath of dir (/tmp is /private/tmp on macOS): what command lines show. */
  real: string;
  work: string;
}

/** The file that marks a directory as one the bench created for a run. */
export const RUN_MARKER = ".wakebench-run";

/** Creates a run's directory, fresh (it must not exist), and marks it as the bench's. */
export function makeRunDir(outRoot: string, run: string): string {
  if (!RUN_ID.test(run)) throw new Error(`not a run ID: ${run}`);
  mkdirSync(outRoot, { recursive: true, mode: 0o700 });
  const dir = join(outRoot, run);
  mkdirSync(dir, { mode: 0o700 }); // throws if it exists: a run never reuses a directory
  writeFileSync(join(dir, RUN_MARKER), run + "\n", { mode: 0o600 });
  return dir;
}

/** Run IDs: 20261020-011712-34a8. */
export const RUN_ID = /^\d{8}-\d{6}-[0-9a-f]{4}$/;

/**
 * A fresh temp HOME for a lane: <run dir>/homes/<name>. The run dir must be
 * one the bench created (its marker names it), the name a plain lane name,
 * and the HOME must not exist yet: nothing is ever deleted to make room.
 */
export function makeHome(runDir: string, name: string): Home {
  if (!/^[a-z0-9][a-z0-9-]{0,40}$/.test(name)) throw new Error(`not a lane name: ${name}`);
  let marker = "";
  try {
    marker = readFileSync(join(runDir, RUN_MARKER), "utf8").trim();
  } catch {
    // Not ours.
  }
  if (!marker || marker !== basename(realpathSync(runDir))) throw new Error(`${runDir} isn't a run directory the bench created`);
  const homes = join(runDir, "homes");
  mkdirSync(homes, { recursive: true, mode: 0o700 });
  const dir = join(homes, name);
  if (existsSync(dir)) throw new Error(`${dir} exists already: a lane's HOME is always new`);
  for (const d of ["", "work", "tmp", "Library/Keychains"]) mkdirSync(join(dir, d), { recursive: true, mode: 0o700 });
  // Wakeshift's npm packages (@wakeshift/opencode, @wakeshift/connect) from
  // a local registry holding packages packed for this core, never npm's own.
  if (process.env.WB_NPM_REGISTRY) writeFileSync(join(dir, ".npmrc"), `@wakeshift:registry=${process.env.WB_NPM_REGISTRY}\n`, { mode: 0o600 });
  return { dir, real: realpathSync(dir), work: join(dir, "work") };
}

/** Borrows the login keychain read-only (a symlink; the search list is untouched). */
export function borrowKeychain(h: Home): boolean {
  const src = join(REAL_HOME, "Library/Keychains/login.keychain-db");
  if (!existsSync(src)) return false;
  const dst = join(h.dir, "Library/Keychains/login.keychain-db");
  if (!existsSync(dst)) symlinkSync(src, dst);
  return true;
}

export function writeJson(path: string, v: unknown, mode = 0o600) {
  mkdirSync(dirname(path), { recursive: true, mode: 0o700 });
  writeFileSync(path, JSON.stringify(v, null, 2) + "\n", { mode });
}

export function readJsonIf<T>(path: string): T | null {
  try {
    return JSON.parse(readFileSync(path, "utf8")) as T;
  } catch {
    return null;
  }
}

/** The real binary for name on PATH, skipping the agent wrapper directories a terminal workspace app can put first on PATH. */
export function realBinary(name: string, path = process.env.PATH ?? ""): string | null {
  for (const dir of path.split(delimiter)) {
    if (!dir || /\/\.superset[^/]*\/bin\/?$/.test(dir)) continue;
    const p = join(dir, name);
    try {
      accessSync(p, constants.X_OK);
      return p;
    } catch {
      // Next.
    }
  }
  return null;
}

/**
 * Every harness's config directory, in h. The pane starts with env -i, so
 * nothing from the user's shell is inherited, but a harness, plugin or
 * installer the lane runs can still reach the user's own config through a
 * variable it sets or a default it derives; naming them all here keeps
 * every one in the temp HOME (a run that kept the shell's CODEX_HOME once
 * rewrote the real ~/.codex/hooks.json).
 */
export function configEnv(h: Home): Record<string, string> {
  return {
    CODEX_HOME: join(h.dir, ".codex"),
    CLAUDE_CONFIG_DIR: join(h.dir, ".claude"),
    XDG_CONFIG_HOME: join(h.dir, ".config"),
    HERMES_HOME: join(h.dir, ".hermes"),
    OPENCODE_CONFIG: join(h.dir, ".config/opencode/opencode.json"),
    COPILOT_HOME: join(h.dir, ".copilot"),
    PI_CODING_AGENT_DIR: join(h.dir, ".pi/agent"),
  };
}

/** A clean environment for a harness in h (nothing from the bench's own env leaks in). */
export function isoEnv(h: Home, binDirs: string[], extra: Record<string, string> = {}): Record<string, string> {
  const base = ["/opt/homebrew/bin", "/usr/local/bin", "/usr/bin", "/bin", "/usr/sbin", "/sbin"];
  const path = [join(h.dir, ".wakeshift/bin"), ...binDirs, ...base].filter((d, i, a) => a.indexOf(d) === i).join(":");
  return {
    HOME: h.dir,
    USER: process.env.USER ?? "user",
    LOGNAME: process.env.LOGNAME ?? process.env.USER ?? "user",
    SHELL: "/bin/bash",
    TERM: "xterm-256color",
    COLORTERM: "truecolor",
    LANG: "en_US.UTF-8",
    PATH: path,
    TMPDIR: join(h.dir, "tmp"),
    ...configEnv(h),
    ...extra,
  };
}

/** argv for a pane: an interactive bash with exactly env (env -i). */
export function shellArgv(env: Record<string, string>): string[] {
  return ["env", "-i", ...Object.entries(env).map(([k, v]) => `${k}=${v}`), "/bin/bash", "--noprofile", "--norc", "-i"];
}

/** Shell-quotes one word. */
export function q(s: string): string {
  return /^[A-Za-z0-9_\-./:=@%+,]+$/.test(s) ? s : `'${s.replace(/'/g, "'\\''")}'`;
}
