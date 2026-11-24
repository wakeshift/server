// S16 (an agent Wakeshift doesn't recognize falls back to the app's status
// tool) and S17 (a tool with a side effect that subscribes is called once):
// the toy app's disguise, the scoring, the agent's words, and the cells.

import { strict as assert } from "node:assert";
import { test } from "node:test";
import { benchEventText, jobStatusText } from "../app/jobs.js";
import { disguise } from "../app/server.js";
import { keyOf } from "../src/grade.js";
import { sideEffectOnce, statusFallback } from "../src/metrics.js";
import type { ScenarioResult } from "../src/model.js";
import { JOB_RESULT, jobWords, PROMISE, s16WaitMs, s17IdleMs, toldResult } from "../src/scenarios.js";

const CODE = "WB-ab12-1";
const PUB = "2026-11-05T10:00:00.000Z";
const call = (tool: string, at: string, codes: string[] = []) => ({ tool, at, events_in_result: codes });

test("disguise: an unknown client name, only the protocol's _meta keys, no harness claim; the original untouched", () => {
  const init = { jsonrpc: "2.0", id: 0, method: "initialize", params: { protocolVersion: "2025-06-18", capabilities: {}, clientInfo: { name: "codex-mcp-client", title: "Codex", version: "0.166.0" } } };
  const callMsg = {
    jsonrpc: "2.0",
    id: 1,
    method: "tools/call",
    params: {
      name: "watch",
      arguments: { harness: "codex", session_key: "wsc_x" },
      _meta: { "x-codex-turn-metadata": { thread_id: "t" }, "claudecode/toolUseId": "toolu_1", "agent-session-id": "a", progressToken: 3, "io.modelcontextprotocol/clientInfo": { name: "claude-code", version: "2" }, "io.modelcontextprotocol/protocolVersion": "2026-07-28" },
    },
  };
  const req = { headers: { "user-agent": "codex-mcp-client/0.166.0" } as Record<string, string | string[] | undefined>, rawHeaders: ["Host", "x", "User-Agent", "codex-mcp-client/0.166.0"] };
  const [i, c] = disguise([init, callMsg], req as never) as [typeof init, typeof callMsg];
  assert.equal(i.params.clientInfo.name, "acme-agent");
  assert.equal(i.params.clientInfo.version, "0.166.0");
  assert.deepEqual(c.params.arguments, { session_key: "wsc_x" });
  assert.deepEqual(c.params._meta, { progressToken: 3, "io.modelcontextprotocol/clientInfo": { name: "acme-agent", title: "Acme Agent", version: "2" }, "io.modelcontextprotocol/protocolVersion": "2026-07-28" });
  assert.equal(req.headers["user-agent"], "acme-agent/1.0");
  assert.deepEqual(req.rawHeaders, ["Host", "x", "User-Agent", "acme-agent/1.0"]);
  // The record keeps what the agent sent.
  assert.equal(callMsg.params.arguments.harness, "codex");
  assert.equal(init.params.clientInfo.name, "codex-mcp-client");
  // A request without a User-Agent gets one; a body-less GET is fine.
  const bare = { headers: {} as Record<string, string | string[] | undefined>, rawHeaders: [] as string[] };
  assert.equal(disguise(undefined, bare as never), undefined);
  assert.deepEqual(bare.rawHeaders, ["User-Agent", "acme-agent/1.0"]);
});

test("statusFallback: works when told, by a tool that doesn't subscribe, with watch called once", () => {
  const base = { code: CODE, publishedAt: PUB, toldUnasked: false, toldWhenAsked: true, promised: true };
  const ok = statusFallback({ ...base, calls: [call("watch", "2026-11-05T09:59:00Z"), call("job_status", "2026-11-05T10:01:30Z", [CODE])] });
  assert.equal(ok.works, true);
  assert.equal(ok.metrics.watch_calls, 1);
  assert.equal(ok.metrics.watch_repeats, 0);
  assert.equal(ok.metrics.status_calls, 1);
  assert.equal(ok.metrics.found_by_tool, "job_status");
  assert.equal(ok.metrics.said_it_would_notify, true);
  // watch again (the old text's "call this tool again"): fails, even though the user heard.
  const again = statusFallback({ ...base, calls: [call("watch", "2026-11-05T09:59:00Z"), call("watch", "2026-11-05T10:01:30Z", [CODE])] });
  assert.equal(again.works, false);
  assert.equal(again.metrics.watch_repeats, 1);
  assert.match(again.reason!, /watch 2 times/);
  // Told without any tool showing it the result: not a fallback.
  const guessed = statusFallback({ ...base, calls: [call("watch", "2026-11-05T09:59:00Z")] });
  assert.equal(guessed.works, false);
  assert.equal(guessed.metrics.found_by_tool, null);
  // Never told.
  const never = statusFallback({ ...base, toldWhenAsked: false, calls: [call("watch", "2026-11-05T09:59:00Z"), call("job_status", "2026-11-05T10:01:30Z", [CODE])] });
  assert.equal(never.works, false);
  assert.match(never.reason!, /never heard/);
  // Woken anyway (no status call needed), and a status call before the publish doesn't count as finding out.
  assert.equal(statusFallback({ ...base, toldUnasked: true, toldWhenAsked: null, calls: [call("watch", "2026-11-05T09:59:00Z")] }).works, true);
  assert.equal(statusFallback({ ...base, calls: [call("job_status", "2026-11-05T09:59:30Z", [])] }).metrics.found_by_tool, null);
});

test("sideEffectOnce: works only when the job started exactly once", () => {
  const calls = [call("start_job", "2026-11-05T09:59:00Z")];
  assert.equal(sideEffectOnce({ sideEffects: 1, calls, toldUnasked: true, toldWhenAsked: null }).works, true);
  const twice = sideEffectOnce({ sideEffects: 2, calls: [...calls, call("start_job", "2026-11-05T09:59:05Z")], toldUnasked: true, toldWhenAsked: null });
  assert.equal(twice.works, false);
  assert.equal(twice.metrics.side_effect_calls, 2);
  assert.equal(twice.metrics.start_job_calls, 2);
  assert.match(twice.reason!, /started 2 times/);
  assert.match(sideEffectOnce({ sideEffects: 0, calls: [], toldUnasked: false, toldWhenAsked: false }).reason!, /never started/);
});

test("toldResult: the code or the outcome as news, not a promise or the tools' own output", () => {
  assert.ok(toldResult(`The bench job finished (${CODE}).`, CODE));
  assert.ok(toldResult("Good news: the job passed.", CODE));
  assert.ok(toldResult("It finished successfully ✅", CODE));
  assert.ok(!toldResult("I'll let you know whether it passed.", CODE));
  assert.ok(!toldResult("No news yet: it's still running. I'll tell you once it passes or fails.", CODE));
  assert.ok(!toldResult("The job hasn't passed or failed yet.", CODE));
  assert.ok(!toldResult(`  ⎿  Status: finished. Result: ${JOB_RESULT} (${CODE}, at ${PUB}).`, CODE));
  assert.ok(!toldResult(`  └ ${benchEventText(CODE, JOB_RESULT)}`, CODE));
  // A tool call naming the code (Codex's approval dialog for report_seen) isn't telling the user.
  assert.ok(!toldResult(["• Calling wakebench.report_seen", '  Allow the wakebench MCP server to run tool "report_seen"?', `  ids: ["${CODE}"]`].join("\n"), CODE));
  assert.ok(!toldResult(`⏺ wakebench - report_seen (MCP)(ids: ["${CODE}"])`, CODE));
});

test("jobWords: the user's words, Wakeshift's texts and the job tools' output are left out", () => {
  const status = jobStatusText({ code: CODE, tag: JOB_RESULT, at: PUB }, 0);
  const pane = [`› Any news?`, `• Called wakebench.job_status({})`, `  └ ${status}`, `• Yes: the bench job passed.`].join("\n");
  const own = jobWords(pane, ["Any news?", status]);
  assert.doesNotMatch(own, /Status: finished/);
  assert.match(own, /the bench job passed\./);
  assert.ok(toldResult(own, CODE));
  assert.ok(!toldResult(jobWords(`  └ ${status}`, [status]), CODE));
});

test("PROMISE: the agent says it will tell the user by itself", () => {
  assert.ok(PROMISE.test("Watching it now. I'll let you know when it finishes."));
  assert.ok(PROMISE.test("You'll be notified here when it's done."));
  assert.ok(!PROMISE.test("Ask me later and I can check its status."));
});

test("S16 and S17 waits: defaults, and bounded overrides", () => {
  assert.equal(s16WaitMs({}), 75_000);
  assert.equal(s16WaitMs({ WB_S16_WAIT_SECONDS: "90" }), 90_000);
  assert.equal(s16WaitMs({ WB_S16_WAIT_SECONDS: "x" }), 75_000);
  assert.equal(s17IdleMs({}), 30_000);
  assert.equal(s17IdleMs({ WB_S17_IDLE_SECONDS: "1" }), 30_000);
});

test("S16 and S17 cells", () => {
  const r = (id: "S16" | "S17" | "S17d", metrics: ScenarioResult["metrics"]): ScenarioResult => ({ id, harness: "codex", status: "ran", works: true, metrics, steps: [], notes: [] });
  assert.equal(keyOf(r("S16", { watch_calls: 1, status_calls: 2, woke: false, told_when_asked: true })), "told when asked, watch ×1, status ×2");
  assert.equal(keyOf(r("S17", { side_effect_calls: 1, woke: true })), "1 job started, woke");
  assert.equal(keyOf(r("S17d", { side_effect_calls: 3, woke: false, told_when_asked: false })), "3 jobs started, never told");
});
