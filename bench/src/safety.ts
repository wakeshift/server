// Checksums of the real harness configs a run could touch, before and after
// (and between harnesses). Any change aborts the run and is reported. Only
// hashes are kept, never contents.

import { createHash } from "node:crypto";
import { existsSync, readdirSync, readFileSync, statSync } from "node:fs";
import { join, relative } from "node:path";

export interface Snapshot {
  [what: string]: string;
}

const FILES = [
  ".claude/settings.json",
  ".claude/settings.local.json",
  ".codex/hooks.json",
  ".codex/config.toml",
  ".codex/auth.json",
  ".cursor/hooks.json",
  ".cursor/mcp.json",
  // Where approvals persist (an "always" answered in a harness that wrote to the real HOME).
  ".cursor/permissions.json",
  ".copilot/permissions-config.json",
  ".gemini/settings.json",
  ".gemini/config/hooks.json",
  ".gemini/config/mcp_config.json",
  ".gemini/antigravity-cli/settings.json",
  ".gemini/antigravity-cli/cache/onboarding.json",
  ".copilot/config.json",
  ".copilot/mcp-config.json",
  // pi's agent directory: its settings and MCP servers.
  ".pi/agent/settings.json",
  ".pi/agent/mcp.json",
];

// Trees hashed as a whole (names and contents), skipping caches.
const TREES = [".config/opencode", ".copilot/hooks", ".codex/rules", ".gemini/config/plugins", ".pi/agent/extensions"];
const SKIP = /(^|\/)(node_modules|\.git|cache|logs?|tmp)(\/|$)/;

// ~/.claude.json: only the keys a run could change (it's rewritten often).
// Not oauthAccount: the user's own Claude Code sessions rewrite it (it
// stopped the October 27 S12 run), and the bench's Claude Code runs on its
// own token in a temp HOME, so a run can't change it.
const CLAUDE_JSON_KEYS = ["mcpServers", "hooks"];

// ~/.cursor/cli-config.json: the keys where Cursor keeps approvals (its allowlist, approval mode, sandbox).
const CURSOR_CLI_KEYS = ["permissions", "approvalMode", "sandbox"];

const sha = (b: Buffer | string) => createHash("sha256").update(b).digest("hex").slice(0, 16);

function treeHash(root: string): string {
  const parts: string[] = [];
  const walk = (dir: string) => {
    let entries: string[] = [];
    try {
      entries = readdirSync(dir).sort();
    } catch {
      return;
    }
    for (const e of entries) {
      const p = join(dir, e);
      const rel = relative(root, p);
      if (SKIP.test(rel)) continue;
      let st;
      try {
        st = statSync(p);
      } catch {
        continue;
      }
      if (st.isDirectory()) walk(p);
      else if (st.isFile()) parts.push(`${rel}:${sha(readFileSync(p))}`);
    }
  };
  walk(root);
  return sha(parts.join("\n"));
}

/** Hashes of the real configs under home. */
export function snapshot(home: string): Snapshot {
  const out: Snapshot = {};
  for (const f of FILES) {
    const p = join(home, f);
    out[`~/${f}`] = existsSync(p) ? sha(readFileSync(p)) : "absent";
  }
  for (const t of TREES) {
    const p = join(home, t);
    out[`~/${t}/`] = existsSync(p) ? treeHash(p) : "absent";
  }
  // cursor-agent rewrites its cli-config.json for caches (privacy, server
  // config) whenever any cursor-agent of the user's runs: only the keys an
  // approval could change are hashed.
  const cc = join(home, ".cursor/cli-config.json");
  if (existsSync(cc)) {
    try {
      const d = JSON.parse(readFileSync(cc, "utf8")) as Record<string, unknown>;
      for (const k of CURSOR_CLI_KEYS) out[`~/.cursor/cli-config.json ${k}`] = sha(JSON.stringify(d[k] ?? null));
    } catch {
      out["~/.cursor/cli-config.json"] = "unreadable";
    }
  } else out["~/.cursor/cli-config.json"] = "absent";
  const cj = join(home, ".claude.json");
  if (existsSync(cj)) {
    try {
      const d = JSON.parse(readFileSync(cj, "utf8")) as Record<string, unknown>;
      for (const k of CLAUDE_JSON_KEYS) out[`~/.claude.json ${k}`] = sha(JSON.stringify(d[k] ?? null));
      const projects = (d.projects ?? {}) as Record<string, { mcpServers?: unknown }>;
      out[`~/.claude.json projects[~].mcpServers`] = sha(JSON.stringify(projects[home]?.mcpServers ?? null));
      // Any project entry a bench HOME or work dir could have added.
      out[`~/.claude.json bench projects`] = sha(JSON.stringify(Object.keys(projects).filter((p) => p.includes("wakebench"))));
    } catch {
      out["~/.claude.json"] = "unreadable";
    }
  } else out["~/.claude.json"] = "absent";
  return out;
}

/** What differs between two snapshots. */
export function changed(before: Snapshot, after: Snapshot): string[] {
  const keys = new Set([...Object.keys(before), ...Object.keys(after)]);
  return [...keys].filter((k) => before[k] !== after[k]).sort();
}
