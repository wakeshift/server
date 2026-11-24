// S19 and S19h, Wakeshift's plugin updated under a connected conversation:
// their scenario IDs, the refs the bench renders plugins from, how the
// update offer is chosen, and their grading.

import { strict as assert } from "node:assert";
import { test } from "node:test";
import { mkdirSync, mkdtempSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { gitRef, PUBLISHED_PLUGIN_REF } from "../src/core.js";
import { grade, parseTiers } from "../src/grade.js";
import { DEFAULT_SCENARIOS, parseScenarios, type ScenarioResult } from "../src/model.js";
import { pluginTakesIssuer } from "../src/scenarios.js";

const tiers = parseTiers(readFileSync(join(dirname(fileURLToPath(import.meta.url)), "../../tiers.yaml"), "utf8"));

test("S19 and S19h are asked for by name; a range over S19 includes S19h", () => {
  assert.ok(!DEFAULT_SCENARIOS.includes("S19") && !DEFAULT_SCENARIOS.includes("S19h"));
  assert.deepEqual(parseScenarios("S19"), ["S19"]);
  assert.deepEqual(parseScenarios("s19h"), ["S19h"]);
  assert.deepEqual(parseScenarios("S18-S19"), ["S18", "S19", "S19h"]);
  assert.deepEqual(parseScenarios("S6,S2i,S18,S19"), ["S2i", "S6", "S18", "S19"]);
});

test("refs from the environment are git refs, never options", () => {
  assert.equal(gitRef("X", {}), null);
  assert.equal(gitRef("X", { X: " f1923a8b " }), "f1923a8b");
  assert.equal(gitRef("X", { X: "origin/main" }), "origin/main");
  for (const bad of ["--output=/tmp/x", "-o", "a b", "a;rm", "$(id)", "a\nb"]) assert.throws(() => gitRef("X", { X: bad }), /not a git ref/);
  assert.match(PUBLISHED_PLUGIN_REF, /^[0-9a-f]{7,40}$/);
});

test("the update offer keeps --config issuer= only for a plugin that still has the option", () => {
  const dir = mkdtempSync(join(tmpdir(), "wb-s19-"));
  const manifest = join(dir, "plugins/claude-code/.claude-plugin/plugin.json");
  mkdirSync(dirname(manifest), { recursive: true });
  writeFileSync(manifest, JSON.stringify({ name: "wakeshift", version: "2026.11.07.0303+5b418e55da6a", userConfig: { issuer: { type: "string" } } }));
  assert.equal(pluginTakesIssuer(dir), true);
  writeFileSync(manifest, JSON.stringify({ name: "wakeshift", version: "1.0.0" }));
  assert.equal(pluginTakesIssuer(dir), false);
  assert.equal(pluginTakesIssuer(join(dir, "missing")), false);
});

test("S19 grades: works is every post woken on the new pin; tier 1 also asks nothing and says nothing", () => {
  const t1 = tiers.tiers.find((t) => t.id === "T1");
  for (const id of ["S19", "S19h"] as const) {
    const r = (metrics: ScenarioResult["metrics"], works = true): ScenarioResult => ({ id, harness: "claude-code", status: "ran", works, metrics, steps: [], notes: [] });
    const ok = { woke: 3, of: 3, updated: true, pin_moved: true, questions: 0, wakeshift_prompts: 0, nudges: 0, back_messages: 0 };
    assert.equal(grade(r(ok), t1).grade, "pass");
    assert.equal(grade(r({ ...ok, questions: 1 }), t1).grade, "below-bar");
    assert.equal(grade(r({ ...ok, back_messages: 1 }), t1).grade, "below-bar");
    assert.equal(grade(r({ ...ok, woke: 2, pin_moved: false }, false), t1).grade, "fail");
  }
});
