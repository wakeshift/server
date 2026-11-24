// The bench's own state for container harnesses (bench/state.sh): made by
// each harness's onboarding in $WB_STATE_DIR (default ~/.wakeshift-bench),
// never from the user's own state, with the model key in its env file.

import { existsSync, readFileSync, statSync } from "node:fs";
import { homedir } from "node:os";
import { join } from "node:path";

export function stateRoot(env: NodeJS.ProcessEnv = process.env): string {
  return env.WB_STATE_DIR || join(homedir(), ".wakeshift-bench");
}

/** The harness's state dir, if bench/state.sh made it (its .wakebench marker). */
export function harnessState(name: string, root = stateRoot()): string | null {
  const dir = join(root, name);
  return existsSync(join(dir, ".wakebench")) ? dir : null;
}

/** KEY=VALUE lines of the env file (blank lines and # comments skipped). */
export function parseEnv(text: string): Record<string, string> {
  const out: Record<string, string> = {};
  for (const line of text.split("\n")) {
    const t = line.trim();
    if (!t || t.startsWith("#")) continue;
    const i = t.indexOf("=");
    if (i > 0) out[t.slice(0, i).trim()] = t.slice(i + 1).trim();
  }
  return out;
}

/** The named secrets from the env file, or why they can't be read. */
export function readSecrets(names: string[], root = stateRoot()): { ok: true; env: Record<string, string> } | { ok: false; reason: string } {
  const file = join(root, "env");
  if (!existsSync(file)) return { ok: false, reason: `${file} is missing: put ${names.join(", ")} in it (mode 0600)` };
  if ((statSync(file).mode & 0o077) !== 0) return { ok: false, reason: `${file} is readable by others: chmod 600 it` };
  const all = parseEnv(readFileSync(file, "utf8"));
  const missing = names.filter((n) => !all[n]);
  if (missing.length) return { ok: false, reason: `${file} has no ${missing.join(", ")}` };
  return { ok: true, env: Object.fromEntries(names.map((n) => [n, all[n]])) };
}
