// S6n: the install with no nudge (the user says only "done").
import { strict as assert } from "node:assert";
import { dirname, join } from "node:path";
import { test } from "node:test";
import { fileURLToPath } from "node:url";
import { grade, loadTiers, tierOf } from "../src/grade.js";
import { noNudge } from "../src/metrics.js";
import { DEFAULT_SCENARIOS, parseScenarios, type ScenarioResult } from "../src/model.js";
import { DONE, DONE_READ, s6nBack } from "../src/scenarios.js";

const tiers = loadTiers(join(dirname(fileURLToPath(import.meta.url)), "../../tiers.yaml"));

function s6n(m: Parameters<typeof noNudge>[0], back_messages = 1): ScenarioResult {
  const o = noNudge(m);
  return { id: "S6n", harness: "opencode", status: "ran", works: o.works, reason: o.reason, metrics: { ...o.metrics, back: "done", back_messages }, steps: [], notes: [] };
}

test("S6n: runs only by name, so a range over S6 stays S6", () => {
  assert.deepEqual(parseScenarios("S6n"), ["S6n"]);
  assert.deepEqual(parseScenarios("S6,s6n"), ["S6", "S6n"]);
  assert.ok(!parseScenarios("S1-S9").includes("S6n"));
  assert.ok(!DEFAULT_SCENARIOS.includes("S6n"));
});

test("S6n: the user's words name no tool", () => {
  for (const said of [DONE, DONE_READ]) assert.doesNotMatch(said, /watch|report_seen|job_status|tool|wakeshift/i);
  assert.equal(s6nBack({}), "done");
  assert.equal(s6nBack({ WB_S6N_BACK: "read" }), "read");
  assert.throws(() => s6nBack({ WB_S6N_BACK: "nudge" }), /done or read/);
});

test("S6n: connected and woken after \"done\" passes", () => {
  const r = grade(s6n({ installed: true, connected_ms: 4200, wake_ms: 3100, watch_again: 0 }), tierOf(tiers, "opencode"));
  assert.equal(r.grade, "pass");
  assert.equal(r.key, `conn 4.2s, woke 3.1s · "done"`);
});

test("S6n: never connected after \"done\" fails (S6's nudge hid it)", () => {
  const r = grade(s6n({ installed: true, connected_ms: null, wake_ms: null, watch_again: 0 }), tierOf(tiers, "hermes"));
  assert.equal(r.grade, "fail");
  assert.match(r.why!.join(" "), /never connected after "done"/);
  assert.equal(r.key, `not connected · "done"`);
  const again = grade(s6n({ installed: true, connected_ms: null, wake_ms: null, watch_again: 1 }), tierOf(tiers, "hermes"));
  assert.match(again.why!.join(" "), /though the agent called watch again/);
  assert.equal(again.key, `not connected, watch ×1 again · "done"`);
});

test("S6n: woken without the install's own connection is below tier 1's bar", () => {
  const r = grade(s6n({ installed: true, connected_ms: null, wake_ms: 5000, watch_again: 1 }), tierOf(tiers, "codex"));
  assert.equal(r.grade, "below-bar");
  assert.match(r.why!.join(" "), /connected is false/);
});

test("S6n: nothing installed fails", () => {
  const r = grade(s6n({ installed: false, connected_ms: null, wake_ms: null, watch_again: 0 }, 0), tierOf(tiers, "claude-code"));
  assert.equal(r.grade, "fail");
  assert.deepEqual(r.why, ["nothing was installed"]);
});
