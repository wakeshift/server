// The guard on what a lane's harness had in reach: a lane whose conversation
// could reach MCP servers beyond the lane's own (the ChatGPT account's apps
// as Codex's codex_apps, a server from the user's config) isn't the scenario,
// and its agent may have read the user's mail or files. Every cell of such a
// lane fails (grade.ts), and the notes say which servers.

import type { Driver } from "../drivers/types.js";
import type { Lane } from "./lane.js";
import type { ScenarioResult } from "./model.js";

/** Checks the lane's tool list once its harness stopped, and records it on each of the lane's results. */
export function checkToolServers(d: Driver, l: Lane, results: ScenarioResult[]): void {
  if (!d.toolServers || results.length === 0) return;
  let c: ReturnType<NonNullable<Driver["toolServers"]>>;
  try {
    c = d.toolServers(l);
  } catch (err) {
    const why = `tool list not checked: ${err instanceof Error ? err.message : String(err)}`;
    for (const r of results) {
      r.metrics.tool_servers_checked = false;
      if (!r.notes.includes(why)) r.notes.push(why);
    }
    return;
  }
  l.note(`MCP servers in reach: ${c.servers.join(", ") || "none"}${c.listed ? "" : " (no tool list recorded)"}`);
  for (const r of results) {
    r.metrics.tool_servers = c.servers.join(", ");
    r.metrics.tool_servers_checked = c.listed;
    if (c.unexpected.length) {
      r.metrics.tool_servers_unexpected = c.unexpected.join(", ");
      const why = `MCP servers beyond the lane's in reach: ${c.unexpected.join(", ")}`;
      if (!r.notes.includes(why)) r.notes.push(why);
    }
  }
}
