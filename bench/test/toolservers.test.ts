import { strict as assert } from "node:assert";
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { test } from "node:test";
import { codexFeaturesToml, codexToolServers, rolloutToolServers } from "../drivers/codex/index.js";
import type { Driver } from "../drivers/types.js";
import { grade, keyOf } from "../src/grade.js";
import type { Lane } from "../src/lane.js";
import type { ScenarioResult } from "../src/model.js";
import { checkToolServers } from "../src/toolservers.js";

const line = (o: Record<string, unknown>) => JSON.stringify(o);

// The tool list Codex 0.167 records (world_state), with the account's apps in it.
const withApps = [
  line({ type: "session_meta", payload: { cli_version: "0.167.1" } }),
  line({ type: "world_state", payload: { full: true, state: { tools: { mcp__wakebench: "Bench tools", mcp__codex_apps__gmail: "Gmail tools", mcp__codex_apps__google_drive: "Drive", mcp__codex_tui: "" } } } }),
  line({ type: "response_item", payload: { type: "custom_tool_call", name: "exec", input: "text(await tools.mcp__wakebench__report_seen({code:'x'}));" } }),
  "{ half a line",
];

test("the tool list's servers, the account's apps as one", () => {
  assert.deepEqual(rolloutToolServers(withApps), { servers: ["codex_apps", "codex_tui", "wakebench"], listed: true });
});

test("a server only a call names counts too (a direct call, or tools.mcp__… in an exec)", () => {
  const calls = [
    line({ type: "response_item", payload: { type: "function_call", name: "mcp__other__thing", arguments: "{}" } }),
    line({ type: "response_item", payload: { type: "custom_tool_call", name: "exec", input: 'await tools["mcp__codex_apps__webhook_watch"]({})' } }),
  ];
  assert.deepEqual(rolloutToolServers(calls), { servers: ["codex_apps", "other"], listed: false });
});

test("a world_state diff adds to the list; no tool list says so", () => {
  const diff = [line({ type: "world_state", payload: { full: false, state: { tools: { mcp__wakebench: "" } } } })];
  assert.deepEqual(rolloutToolServers(diff), { servers: ["wakebench"], listed: true });
  assert.deepEqual(rolloutToolServers([line({ type: "event_msg", payload: { type: "task_started" } })]), { servers: [], listed: false });
  // No MCP server at all: world_state without its tools key (codex exec, 0.167.1).
  assert.deepEqual(rolloutToolServers([line({ type: "world_state", payload: { full: true, state: { top_level_tools: {} } } })]), { servers: [], listed: true });
});

test("codexToolServers reads every thread of the HOME; codex_tui is Codex's own", () => {
  const home = mkdtempSync(join(tmpdir(), "wb-toolservers-"));
  try {
    const day = join(home, ".codex/sessions/2026/11/13");
    mkdirSync(day, { recursive: true });
    writeFileSync(join(day, "rollout-a.jsonl"), withApps.join("\n"));
    writeFileSync(join(day, "rollout-b.jsonl"), line({ type: "world_state", payload: { state: { tools: { mcp__wakebench: "", mcp__second: "" } } } }));
    assert.deepEqual(codexToolServers(home, ["wakebench", "second"]), { servers: ["codex_apps", "codex_tui", "second", "wakebench"], unexpected: ["codex_apps"], listed: true });
    assert.deepEqual(codexToolServers(join(home, "none"), ["wakebench"]), { servers: [], unexpected: [], listed: false });
  } finally {
    rmSync(home, { recursive: true, force: true });
  }
});

test("config.toml turns the account's apps, remote plugins, browser and computer use off, and leaves plugins on", () => {
  const t = codexFeaturesToml();
  assert.match(t, /^\[features\]\n/);
  for (const f of ["apps", "remote_plugin", "browser_use", "browser_use_external", "computer_use", "in_app_browser"]) assert.match(t, new RegExp(`^${f} = false$`, "m"));
  assert.doesNotMatch(t, /^plugins\s*=/m);
});

const result = (metrics: ScenarioResult["metrics"] = {}): ScenarioResult => ({ id: "S2", harness: "codex", status: "ran", works: true, metrics, steps: [], notes: [] });

test("a lane with servers beyond its own fails, whatever it did", () => {
  const r = grade(result({ tool_servers_unexpected: "codex_apps" }), undefined);
  assert.equal(r.grade, "fail");
  assert.match(r.why!.join(" "), /codex_apps/);
  assert.match(keyOf(r), /unexpected MCP servers: codex_apps/);
  assert.equal(grade(result({ tool_servers: "wakebench", tool_servers_checked: true }), undefined).grade, "pass");
  assert.match(keyOf(grade(result({ tool_servers_checked: false }), undefined)), /tool list not checked/);
});

test("checkToolServers records the list on every result of the lane", () => {
  const notes: string[] = [];
  const l = { note: (s: string) => notes.push(s) } as unknown as Lane;
  const d = { toolServers: () => ({ servers: ["codex_apps", "wakebench"], unexpected: ["codex_apps"], listed: true }) } as unknown as Driver;
  const rs = [result(), result()];
  checkToolServers(d, l, rs);
  for (const r of rs) {
    assert.equal(r.metrics.tool_servers_unexpected, "codex_apps");
    assert.equal(r.metrics.tool_servers_checked, true);
    assert.ok(r.notes.some((n) => /beyond the lane's/.test(n)));
  }
  assert.match(notes[0], /codex_apps, wakebench/);
  const failing = { toolServers: () => { throw new Error("unreadable"); } } as unknown as Driver;
  const r = result();
  checkToolServers(failing, l, [r]);
  assert.equal(r.metrics.tool_servers_checked, false);
});
