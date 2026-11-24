// What Wakeshift's offer of its plugin for Claude Code looks like, and
// whether a Claude Code version runs it. No imports: the scenarios read it
// too.

/**
 * Claude Code's plugin install command, as Wakeshift's offer gives it: the
 * one command from 2.1.292 (`--marketplace`), and before it (or when the
 * core can't tell the version) the marketplace first, then the install.
 */
export const PLUGIN_OFFER = /claude plugin (?:install wakeshift --marketplace |marketplace add \S+ && claude plugin install wakeshift@wakeshift(?![\w@.-]))/;

/** The first Claude Code whose `plugin install` takes `--marketplace`. */
export const PLUGIN_ONE_COMMAND = "2.1.292";

/** Compares dotted versions by their leading numbers: <0, 0 or >0. */
export function compareVersions(a: string, b: string): number {
  const nums = (v: string) => v.split(/[-+]/)[0].split(".").map((x) => Number.parseInt(x, 10) || 0);
  const x = nums(a);
  const y = nums(b);
  for (let i = 0; i < Math.max(x.length, y.length); i++) {
    const d = (x[i] ?? 0) - (y[i] ?? 0);
    if (d !== 0) return d;
  }
  return 0;
}

/** Which form of the plugin install an offered command is ("" if it's neither: the installer, or nothing). */
export function offerForm(cmd: string | null): "one-command" | "two-step" | "" {
  if (!cmd) return "";
  if (/^claude plugin install wakeshift --marketplace /.test(cmd)) return "one-command";
  if (/^claude plugin marketplace add \S+ && claude plugin install wakeshift@wakeshift(?![\w@.-])/.test(cmd)) return "two-step";
  return "";
}

/**
 * Whether the offered plugin install runs on this Claude Code: before
 * 2.1.292 `--marketplace` is "unknown option", so only the two steps do.
 * Null when there's no plugin offer or no version to tell by.
 */
export function offerFitsVersion(cmd: string | null, version: string | null): boolean | null {
  const form = offerForm(cmd);
  const v = /\d+\.\d+\.\d+/.exec(version ?? "")?.[0];
  if (!form || !v) return null;
  return form === "two-step" || compareVersions(v, PLUGIN_ONE_COMMAND) >= 0;
}
