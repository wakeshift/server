// S15 (two apps, one conversation): the second app's codes, its setup's
// refusals, the grading, and the second MCP server in each harness.
import { strict as assert } from "node:assert";
import { createHash } from "node:crypto";
import { mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { test } from "node:test";
import { parse } from "yaml";
import { codeNonce } from "../app/server.js";
import { mcpAddArgs } from "../drivers/base.js";
import { Hermes } from "../drivers/hermes/index.js";
import { grade, parseTiers, tierOf } from "../src/grade.js";
import { SECOND_MCP_NAME, secondAppProblem } from "../src/core.js";
import type { Lane } from "../src/lane.js";
import { appSide, argsCheck, twoApps } from "../src/metrics.js";
import type { ScenarioResult } from "../src/model.js";
import { apply, s15Path } from "../src/scenarios.js";
import { dirname } from "node:path";
import { fileURLToPath } from "node:url";

const tiers = parseTiers(readFileSync(join(dirname(fileURLToPath(import.meta.url)), "../../tiers.yaml"), "utf8"));
const h4 = (s: string) => createHash("sha256").update(s).digest("hex").slice(0, 4);

test("the first app's codes keep their nonce; the second app's never equal it", () => {
  // As the toy app always made them: sha256("<run>/<lane>"), four hex digits.
  assert.equal(codeNonce("20261031-120000-ab12", "claude-code-i"), h4("20261031-120000-ab12/claude-code-i"));
  assert.equal(codeNonce("20261031-120000-ab12", "claude-code-i", 1), codeNonce("20261031-120000-ab12", "claude-code-i"));
  for (let i = 0; i < 500; i++) {
    const run = `run-${i}`;
    const two = codeNonce(run, "hermes-i", 2);
    assert.match(two, /^[0-9a-f]{4}$/);
    assert.notEqual(two, codeNonce(run, "hermes-i", 1));
    assert.equal(two, codeNonce(run, "hermes-i", 2), "the same every time");
  }
  // A run whose second-app hash happens to equal the first's: the next input is used.
  let run = "";
  for (let i = 0; i < 2_000_000 && !run; i++) if (h4(`c${i}/l/app2`) === h4(`c${i}/l`)) run = `c${i}`;
  assert.ok(run, "found a colliding input");
  assert.equal(codeNonce(run, "l", 2), h4(`${run}/l/app2/1`));
  assert.notEqual(codeNonce(run, "l", 2), codeNonce(run, "l", 1));
});

test("the second app's key: another app than the first", () => {
  // Two keys for the same app: refused.
  assert.match(secondAppProblem({ target: "local", first: "wakebench", second: "wakebench" }) ?? "", /same app/);
  // A local core's own second app.
  assert.equal(secondAppProblem({ target: "local", first: "wakebench", second: "wakebench2" }), null);
});

const pub = (code: string, at: number) => ({ code, at });

test("S15 works with every code seen once, for both apps; per-app latency", () => {
  const t = 1_000_000;
  const reports = [
    { at: new Date(t + 2000).toISOString(), ids: ["WB-aaaa-1"], server: 1 as const },
    { at: new Date(t + 3000).toISOString(), ids: ["WB-bbbb-1"], server: 2 as const },
    // Reported on the other app's server: seen all the same, and counted.
    { at: new Date(t + 4000).toISOString(), ids: ["WB-aaaa-2"], server: 2 as const },
  ];
  const s1 = appSide(1, [pub("WB-aaaa-1", t), pub("WB-aaaa-2", t + 1000)], reports, {}, [], () => false, null);
  const s2 = appSide(2, [pub("WB-bbbb-1", t)], reports, { "WB-bbbb-1": t + 2500 }, [], () => false, null);
  const o = twoApps(s1, s2, { nudges: 0, newSession: true, midTurn: { c: true, d: true }, watchedSecond: true });
  assert.equal(o.works, true);
  assert.equal(o.metrics.app1_lost, 0);
  assert.equal(o.metrics.app2_lost, 0);
  assert.deepEqual(o.metrics.app1_latencies_ms, [2000, 3000]);
  // The GOT line came before the report: the earlier sign counts.
  assert.deepEqual(o.metrics.app2_latencies_ms, [2500]);
  assert.equal(o.metrics.misreported, 1);
  assert.equal(o.metrics.app1_codes, "WB-aaaa-1,WB-aaaa-2");
  const r: ScenarioResult = { id: "S15", harness: "claude-code", status: "ran", metrics: {}, steps: [], notes: [] };
  apply(r, o);
  grade(r, tierOf(tiers, "claude-code"));
  assert.equal(r.grade, "pass");
  assert.equal(r.key, "2/2 + 1/1, max 3.0s");
});

test("S15 fails with a code lost for either app, or a code reported twice or shown again", () => {
  const t = 1_000_000;
  const at = (ms: number) => new Date(t + ms).toISOString();
  // App 2's code never arrives.
  const none = appSide(2, [pub("WB-bbbb-1", t)], [], {}, [], () => false, null);
  const ok1 = appSide(1, [pub("WB-aaaa-1", t)], [{ at: at(1000), ids: ["WB-aaaa-1"], server: 1 }], {}, [], () => false, null);
  const lost = twoApps(ok1, none, { nudges: 1, newSession: true, midTurn: { c: true, d: true }, watchedSecond: true });
  assert.equal(lost.works, false);
  assert.equal(lost.metrics.app2_lost, 1);
  assert.match(lost.reason ?? "", /1 lost \(app 1: 0, app 2: 1\)/);
  const r: ScenarioResult = { id: "S15", harness: "openclaw", status: "ran", metrics: {}, steps: [], notes: [] };
  apply(r, lost);
  grade(r, tierOf(tiers, "openclaw"));
  assert.equal(r.grade, "fail", "tier 3 too: nothing lost is the scenario itself");
  assert.equal(r.key, "1/1 + 0/1, lost 0+1 · 1 nudge");

  // Reported in two report_seen calls: a duplicate (fails for any tier).
  const twice = appSide(1, [pub("WB-aaaa-1", t)], [
    { at: at(1000), ids: ["WB-aaaa-1"], server: 1 },
    { at: at(9000), ids: ["WB-aaaa-1"], server: 1 },
  ], {}, [], () => false, null);
  const dup = twoApps(twice, appSide(2, [], [], {}, [], () => false, null), { nudges: 0, newSession: null, midTurn: { c: null, d: null }, watchedSecond: true });
  assert.equal(dup.works, false);
  assert.equal(dup.metrics.app1_duplicates, 1);

  // Re-reported in the nudge turn: answering the bench, not a duplicate.
  const nudged = appSide(1, [pub("WB-aaaa-1", t)], [
    { at: at(1000), ids: ["WB-aaaa-1"], server: 1 },
    { at: at(9000), ids: ["WB-aaaa-1"], server: 1 },
  ], {}, [], (x) => x === at(9000), t + 8000);
  assert.deepEqual(nudged.reportCounts, { "WB-aaaa-1": 1 });
  assert.deepEqual(nudged.afterNudge, []);
  // First seen only in the nudge turn: seen, late.
  const late = appSide(1, [pub("WB-aaaa-1", t)], [{ at: at(9000), ids: ["WB-aaaa-1"], server: 1 }], {}, [], (x) => x === at(9000), t + 8000);
  assert.deepEqual(late.afterNudge, ["WB-aaaa-1"]);
  assert.equal(twoApps(late, appSide(2, [], [], {}, [], () => false, null), { nudges: 1, newSession: null, midTurn: { c: null, d: null }, watchedSecond: true }).works, true);

  // Shown again by Wakeshift in a later result, after it was reported.
  const shown = appSide(1, [pub("WB-aaaa-1", t)], [{ at: at(1000), ids: ["WB-aaaa-1"], server: 1 }], {}, [
    { at: at(500), events_in_result: ["WB-aaaa-1"] },
    { at: at(5000), events_in_result: ["WB-aaaa-1"] },
  ], () => false, null);
  assert.deepEqual(shown.shownAgain, ["WB-aaaa-1"]);
  // Shown again after only its GOT line (never reported): a duplicate too.
  const afterGot = appSide(1, [pub("WB-aaaa-1", t)], [], { "WB-aaaa-1": t + 1000 }, [{ at: at(5000), events_in_result: ["WB-aaaa-1"] }], () => false, null);
  assert.deepEqual(afterGot.shownAgain, ["WB-aaaa-1"]);
  // A report without a usable time doesn't count as an instant arrival.
  const bad = appSide(1, [pub("WB-aaaa-1", t)], [{ at: "not a time", ids: ["WB-aaaa-1"], server: 1 }], {}, [], () => false, null);
  assert.deepEqual(bad.firstSeen, {});
  const o = twoApps(shown, appSide(2, [], [], {}, [], () => false, null), { nudges: 0, newSession: null, midTurn: { c: null, d: null }, watchedSecond: true });
  assert.equal(o.works, false);
  assert.match(o.reason ?? "", /duplicated/);
});

test("S15: a code reported twice that reached the model once was reported again, not duplicated (a bench run, November 2, 2026)", () => {
  // Claude Code on its hooks, step (d): one Stop-hook delivery of WB-cac6-5;
  // the agent reported it, said "I typed that session key wrong. I'll
  // re-report with the correct one." and reported it again 5s later. The
  // transcript count read every code once.
  const ts = (s: string) => Date.parse(s);
  const published = [
    pub("WB-cac6-2", ts("2026-11-02T09:47:36.394Z")),
    pub("WB-cac6-3", ts("2026-11-02T09:48:26.190Z")),
    pub("WB-cac6-4", ts("2026-11-02T09:48:48.170Z")),
    pub("WB-cac6-5", ts("2026-11-02T09:49:15.452Z")),
    pub("WB-cac6-6", ts("2026-11-02T09:49:56.815Z")),
  ];
  const reports = [
    { at: "2026-11-02T09:47:39.421Z", server: 1 as const, ids: ["WB-cac6-2"] },
    { at: "2026-11-02T09:48:30.249Z", server: 1 as const, ids: ["WB-cac6-3"] },
    { at: "2026-11-02T09:48:54.896Z", server: 1 as const, ids: ["WB-cac6-4"] },
    { at: "2026-11-02T09:49:23.466Z", server: 1 as const, ids: ["WB-cac6-5"] },
    { at: "2026-11-02T09:49:28.451Z", server: 1 as const, ids: ["WB-cac6-5"] },
    { at: "2026-11-02T09:50:00.299Z", server: 1 as const, ids: ["WB-cac6-6"] },
    { at: "2026-11-02T09:48:36.196Z", server: 2 as const, ids: ["WB-e644-1"] },
    { at: "2026-11-02T09:50:06.271Z", server: 2 as const, ids: ["WB-e644-2"] },
  ];
  const s1 = appSide(1, published, reports, {}, [], () => false, null);
  const s2 = appSide(2, [pub("WB-e644-1", ts("2026-11-02T09:48:26.356Z")), pub("WB-e644-2", ts("2026-11-02T09:49:56.997Z"))], reports, {}, [], () => false, null);
  const opts = { nudges: 0, newSession: true, midTurn: { c: true, d: true }, watchedSecond: true };
  // Without the count (a harness whose record can't be read): a duplicate, as before.
  const before = twoApps(s1, s2, opts);
  assert.equal(before.works, false);
  assert.equal(before.reason, "1 duplicated (app 1: 1, app 2: 0)");

  const once = new Set(["WB-cac6-2", "WB-cac6-3", "WB-cac6-4", "WB-cac6-5", "WB-cac6-6", "WB-e644-1", "WB-e644-2"]);
  const after = twoApps(s1, s2, { ...opts, deliveredOnce: once });
  assert.equal(after.works, true, after.reason ?? "");
  assert.equal(after.metrics.duplicates, 0);
  assert.equal(after.metrics.app1_reported_again, 1);
  assert.ok((after.notes ?? []).some((n) => n === "app 1: reported again, though it reached the model once (not a duplicate): WB-cac6-5"));
  const r: ScenarioResult = { id: "S15", harness: "claude-code", status: "ran", metrics: {}, steps: [], notes: [] };
  apply(r, after);
  grade(r, tierOf(tiers, "claude-code"));
  assert.equal(r.grade, "pass");

  // Counted twice (or not counted at all): still a duplicate.
  const twice = new Set([...once].filter((c) => c !== "WB-cac6-5"));
  assert.equal(twoApps(s1, s2, { ...opts, deliveredOnce: twice }).metrics.app1_duplicates, 1);
  // Shown again in a result after it was reported is a duplicate whatever the count.
  const shown = appSide(1, published, reports, {}, [{ at: "2026-11-02T09:49:40.000Z", events_in_result: ["WB-cac6-3"] }], () => false, null);
  const o = twoApps(shown, s2, { ...opts, deliveredOnce: once });
  assert.equal(o.metrics.app1_duplicates, 1);
  assert.ok((o.notes ?? []).includes("app 1: duplicated WB-cac6-3"));
});

test("S15's tier bars: tier 1 needs no nudge, tier 3 only that it works", () => {
  const t = 1_000_000;
  const late = appSide(1, [pub("WB-aaaa-1", t)], [{ at: new Date(t + 9000).toISOString(), ids: ["WB-aaaa-1"], server: 1 }], {}, [], () => true, t + 8000);
  const o = twoApps(late, appSide(2, [], [], {}, [], () => false, null), { nudges: 1, newSession: true, midTurn: { c: true, d: true }, watchedSecond: true });
  for (const [h, want] of [
    ["codex", "below-bar"],
    ["openclaw", "pass"],
  ] as const) {
    const r: ScenarioResult = { id: "S15", harness: h, status: "ran", metrics: {}, steps: [], notes: [] };
    apply(r, o);
    grade(r, tierOf(tiers, h));
    assert.equal(r.grade, want, h);
  }
});

test("S15 runs in phase b unless WB_S15_PATH=zero-install", () => {
  assert.equal(s15Path({}), "installed");
  assert.equal(s15Path({ WB_S15_PATH: "zero-install" }), "zero-install");
  assert.equal(s15Path({ WB_S15_PATH: "other" }), "installed");
});

test("the second app's MCP server is added next to the first, with no token", () => {
  const l = { mcpName: "wakebench", mcpUrl: "http://127.0.0.1:1/mcp/codex-i", mcpToken: undefined } as unknown as Lane;
  const two = { name: SECOND_MCP_NAME, url: "http://127.0.0.1:2/mcp/codex-i" };
  assert.deepEqual(mcpAddArgs(l, "claude", two), ["mcp", "add", "--transport", "http", "wakebench2", "http://127.0.0.1:2/mcp/codex-i"]);
  assert.deepEqual(mcpAddArgs(l, "codex", two), ["mcp", "add", "wakebench2", "--url", "http://127.0.0.1:2/mcp/codex-i"]);
  assert.deepEqual(mcpAddArgs(l, "codex"), ["mcp", "add", "wakebench", "--url", "http://127.0.0.1:1/mcp/codex-i"]);
  const t = { mcpName: "wakebench", mcpUrl: "u", mcpToken: "S3CR3T" } as unknown as Lane;
  assert.ok(!mcpAddArgs(t, "codex", two).includes("--bearer-token-env-var"));
});

test("Hermes's config.yaml gets both servers, reached from the container", async () => {
  const dir = mkdtempSync(join(tmpdir(), "wb-s15-hermes-"));
  try {
    mkdirSync(join(dir, "state"));
    writeFileSync(join(dir, "state", "config.yaml"), "model: x\n");
    const notes: string[] = [];
    const l = {
      state: { containerDir: dir },
      mcpName: "wakebench",
      mcpUrl: "http://127.0.0.1:1/mcp/hermes-i",
      mcp2: { name: SECOND_MCP_NAME, url: "http://127.0.0.1:2/mcp/hermes-i" },
      note: (s: string) => void notes.push(s),
    } as unknown as Lane;
    await new Hermes().configure(l);
    const doc = parse(readFileSync(join(dir, "state", "config.yaml"), "utf8")) as { mcp_servers: Record<string, { url: string }> };
    assert.deepEqual(doc.mcp_servers, {
      wakebench: { url: "http://host.docker.internal:1/mcp/hermes-i" },
      wakebench2: { url: "http://host.docker.internal:2/mcp/hermes-i" },
    });
    // Without a second app, only the first.
    writeFileSync(join(dir, "state", "config.yaml"), "model: x\n");
    await new Hermes().configure({ ...l, mcp2: null } as unknown as Lane);
    const one = parse(readFileSync(join(dir, "state", "config.yaml"), "utf8")) as { mcp_servers: Record<string, unknown> };
    assert.deepEqual(Object.keys(one.mcp_servers), ["wakebench"]);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test("S9 doesn't hold S15's watch without a session_key against the agent", () => {
  const call = (at: string, session_key: string | null, session?: string) => ({ at, tool: "watch", client: "c", args: { harness: null, conversation_id: null, session_key }, agent: { harness: "claude-code", via: null, claimed: null }, ...(session ? { session } : {}), events_in_result: [] });
  const calls = [call("t1", null, "s1"), call("t2", "s1"), call("t3", null, "s2"), call("t4", "s2")];
  assert.equal(argsCheck([calls], { harness: "claude-code", conversation: "none" }).metrics.session_key_ok, false);
  assert.equal(argsCheck([calls], { harness: "claude-code", conversation: "none" }, (c) => c.at === "t3").metrics.session_key_ok, true);
});
