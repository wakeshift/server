// Checks on what a user sees and keeps, beyond whether a wake worked:
// no Wakeshift credential in a rule a harness saved, no
// Wakeshift-internal ID in what the user or the model is shown, and what an
// uninstall leaves behind against what the public docs say stays
// (site/docs/agents.md, "Uninstall"). Approval prompts are checked as the
// lane answers them (Lane.answerOnce, credentialKinds in redact.ts).

import { existsSync, lstatSync, readdirSync, readFileSync } from "node:fs";
import { join, relative } from "node:path";
import type { ScenarioResult } from "./model.js";
import { credentialKinds } from "./redact.js";

/**
 * Where harnesses save permission rules, relative to a temp HOME (the
 * files the safety check hashes in the real HOME, and the work dir's own
 * project settings): a rule saved from a prompt that showed a link keeps
 * the link (agy's "always allow" once saved the whole command).
 */
export const RULE_FILES = [
  ".claude/settings.json",
  ".claude/settings.local.json",
  "work/.claude/settings.json",
  "work/.claude/settings.local.json",
  ".cursor/permissions.json",
  ".cursor/cli-config.json",
  "work/.cursor/cli.json",
  ".codex/config.toml",
  ".copilot/permissions-config.json",
  ".copilot/settings.json",
  ".copilot/config.json",
  ".gemini/settings.json",
  ".gemini/antigravity-cli/settings.json",
  ".pi/agent/settings.json",
];

/** Folders whose files are all rules or settings (Codex's rules, opencode's config). */
export const RULE_TREES = [".codex/rules", ".config/opencode"];

/** Files under dir (at most depth levels down), symlinks not followed. */
function walk(dir: string, depth = 4, skip: (rel: string) => boolean = () => false, root = dir): string[] {
  let es: import("node:fs").Dirent[];
  try {
    es = readdirSync(dir, { withFileTypes: true });
  } catch {
    return [];
  }
  const out: string[] = [];
  for (const e of es) {
    const p = join(dir, e.name);
    const rel = relative(root, p);
    if (skip(rel)) continue;
    if (e.isDirectory()) {
      out.push(rel + "/");
      if (depth > 0) out.push(...walk(p, depth - 1, skip, root));
    } else out.push(rel);
  }
  return out;
}

function readSmall(p: string, max = 1 << 20): string | null {
  try {
    const st = lstatSync(p);
    if (!st.isFile() || st.size > max) return null;
    return readFileSync(p, "utf8");
  } catch {
    return null;
  }
}

/**
 * Saved rules in a temp HOME that hold a Wakeshift credential: each file
 * and the kinds it holds (never the value). mask takes out what the bench
 * itself put there (a lane's own MCP token).
 */
export function savedRuleCredentials(home: string, mask: (s: string) => string = (s) => s): { file: string; kinds: string[] }[] {
  const files = [...RULE_FILES, ...RULE_TREES.flatMap((t) => walk(join(home, t), 3).filter((f) => !f.endsWith("/")).map((f) => join(t, f)))];
  const out: { file: string; kinds: string[] }[] = [];
  for (const f of files) {
    const text = readSmall(join(home, f));
    if (text === null) continue;
    const kinds = credentialKinds(mask(text));
    if (kinds.length) out.push({ file: f, kinds });
  }
  return out;
}

/**
 * Fails r if a Wakeshift credential was shown in a dialog or saved in a
 * rule during its time (within): like a lost message, this decides whether
 * the scenario works, so it's a rule in code, not a bar. The kinds and
 * where go in the notes, never a value.
 */
export function applyExposures(r: ScenarioResult, exposures: { at: string; where: string; kinds: string[] }[], within: (at: string) => boolean): void {
  const mine = exposures.filter((e) => within(e.at));
  r.metrics.credentials_shown = mine.length;
  if (!mine.length) return;
  for (const e of mine) r.notes.push(`a Wakeshift credential in ${e.where}: ${e.kinds.join(", ")}`);
  const what = `a Wakeshift credential in ${mine[0].where} (${mine[0].kinds.join(", ")})${mine.length > 1 ? ` and ${mine.length - 1} more` : ""}`;
  if (r.works !== false) {
    r.works = false;
    r.reason = what;
  } else r.reason = r.reason ? `${r.reason}; ${what}` : what;
}

/**
 * Wakeshift's own IDs that never belong in text a user or a model reads
 * (internal/: mcpevents' subscription IDs, sub_ and 24 hex digits; an
 * owner's conversation key, conv_ and 16 base32 letters; a connector's
 * server name, wsn_ and 32 hex digits). The framing shows an event's
 * topic, text and data, never its routing fields (internal/framing).
 */
const ID_SHAPES: { kind: string; re: RegExp }[] = [
  { kind: "subscription ID (sub_)", re: /\bsub_[0-9a-f]{24}\b/ },
  { kind: "conversation ID (conv_)", re: /\bconv_[a-z2-7]{16}\b/ },
  { kind: "server name (wsn_)", re: /\bwsn_[0-9a-f]{32}\b/ },
];

/**
 * Wakeshift-internal IDs in text: the shapes above, and any of the IDs the
 * bench knows for the lane (its subscriptions' IDs, the deliveries a
 * connector logged), by kind. Each kind once.
 */
export function internalIds(text: string, known: { id: string; kind: string }[] = []): string[] {
  const found = new Set<string>();
  for (const s of ID_SHAPES) if (s.re.test(text)) found.add(s.kind);
  const lower = text.toLowerCase();
  for (const k of known) if (k.id.length >= 12 && lower.includes(k.id.toLowerCase())) found.add(k.kind);
  return [...found];
}

/** The delivery IDs a Codex connector's log names ("codex: delivered" delivery=<uuid>). */
export function loggedDeliveries(log: string): string[] {
  return [...new Set([...log.matchAll(/\bdelivery=([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})\b/g)].map((m) => m[1]))];
}

/**
 * Where a harness keeps its conversations and logs: the agent's own record
 * of what it was shown (which quotes Wakeshift's text) isn't Wakeshift's
 * file, so the inventory doesn't read it.
 */
const NOT_OURS = /^(work|tmp|Library)(\/|$)|^\.claude\/(projects|todos|shell-snapshots|statsig|session-env|file-history|debug|ide)(\/|$)|^\.codex\/(sessions|log|archived_sessions|history\.jsonl|tmp|shell_snapshots|cache\/codex_)|^\.cursor\/(projects|chats|logs)(\/|$)|^\.copilot\/(session-state|logs|history-session-state|command-history-state\.json)|^\.gemini\/antigravity-cli\/(brain|conversations|logs|cache)(\/|$)|^\.local\/share\/opencode(\/|$)|^\.pi\/agent\/sessions(\/|$)|^\.npm(\/|$)|^\.cache\/(?!opencode\/packages\/@wakeshift)/;

/**
 * What of Wakeshift's is in a temp HOME: everything under ~/.wakeshift,
 * every path whose name says wakeshift, and every small settings or hooks
 * file (outside the harness's conversations and logs) that mentions it.
 * Relative paths; a folder once, with a trailing slash, and nothing under it.
 */
export function wakeshiftInventory(home: string): string[] {
  if (!existsSync(home)) return [];
  // 12 levels: the deepest of Wakeshift's paths (a plugin cache: marketplace, plugin, version, then its own folders) is about 8.
  const all = walk(home, 12, (rel) => NOT_OURS.test(rel));
  const named = all.filter((p) => /wakeshift/i.test(p));
  const mentioned = all.filter((p) => !p.endsWith("/") && !/wakeshift/i.test(p) && /\.(json|jsonc|toml|ya?ml|ts|js|mjs|sh|md|conf)$|(^|\/)(hooks|config|settings)[^/]*$/.test(p) && /wakeshift/i.test(readSmall(join(home, p), 256 << 10) ?? ""));
  // A folder stands for what's under it.
  const dirs = named.filter((p) => p.endsWith("/"));
  const top = named.filter((p) => !dirs.some((d) => d !== p && p.startsWith(d)));
  return [...top, ...mentioned].sort();
}

/**
 * What the public uninstall docs say stays after a harness's uninstall
 * (site/docs/agents.md, "Uninstall": its table's "What stays", and the
 * backups), as path tests: a leftover none of these allows is a finding.
 * For a file that mentions Wakeshift, content narrows it to the entry the
 * docs name.
 */
const DOCUMENTED: Record<string, { why: string; path: RegExp; content?: RegExp }[]> = {
  "*": [{ why: "backups (`.before-wakeshift-…`), kept on purpose", path: /\.before-wakeshift-\d{8}-\d{6}$/ }],
  "claude-code": [{ why: "Claude Code's cached copy of the plugin, which it deletes itself later", path: /^\.claude\/plugins\/cache\/wakeshift\// }],
  codex: [
    { why: "Codex's hooks.state entries for the plugin in ~/.codex/config.toml", path: /^\.codex\/config\.toml$/, content: /hooks\.state\."?wakeshift@wakeshift/ },
    { why: "Codex's record of trusting the plugin's hooks (~/.codex/.hook-trust)", path: /^\.codex\/\.hook-trust(\/|$)/ },
  ],
  copilot: [{ why: "Copilot's record that the plugin is off (\"wakeshift@wakeshift\": false in ~/.copilot/settings.json)", path: /^\.copilot\/settings\.json$/, content: /"wakeshift@wakeshift"\s*:\s*false/ }],
  opencode: [
    { why: "opencode's own copy of the package (~/.cache/opencode/packages/@wakeshift), the user deletes it", path: /^\.cache\/opencode\/packages\/@wakeshift(\/|$)/ },
    { why: "the folder ~/.wakeshift/opencode, the user deletes it", path: /^\.wakeshift\/opencode(\/|$)/ },
    { why: "the @wakeshift/opencode entry in opencode.json's plugin list, the user deletes it", path: /^\.config\/opencode\/opencode\.json$/ },
  ],
  pi: [{ why: "the folder ~/.wakeshift/pi, the user deletes it", path: /^\.wakeshift\/pi(\/|$)/ }],
};

/** Why the docs say p stays after the harness's uninstall, or null where they don't. */
export function documentedLeftover(harness: string, p: string, content: string | null = null): string | null {
  for (const d of [...(DOCUMENTED["*"] ?? []), ...(DOCUMENTED[harness] ?? [])]) {
    if (!d.path.test(p)) continue;
    if (d.content && !(content !== null && d.content.test(content))) continue;
    return d.why;
  }
  return null;
}

/**
 * After an uninstall: what's left of Wakeshift's in the temp HOME, split
 * into what the docs say stays and what they don't. A file that mentions
 * Wakeshift for more than the entry the docs name (a hooks entry left in
 * a settings file the docs only allow a plugin flag in) isn't documented.
 */
export function uninstallLeftovers(home: string, harness: string): { documented: { path: string; why: string }[]; undocumented: string[] } {
  const documented: { path: string; why: string }[] = [];
  const undocumented: string[] = [];
  for (const p of wakeshiftInventory(home)) {
    const content = p.endsWith("/") ? null : readSmall(join(home, p), 256 << 10);
    const why = documentedLeftover(harness, p, content);
    if (why) documented.push({ path: p, why });
    else undocumented.push(p);
  }
  return { documented, undocumented };
}
