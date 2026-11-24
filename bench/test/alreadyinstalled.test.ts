// S18, already installed: which harnesses it runs on, what counts as a
// prompt for Wakeshift's own command, its grading, and its scenario ID.

import { strict as assert } from "node:assert";
import { test } from "node:test";
import { grade, parseTiers } from "../src/grade.js";
import { parseScenarios, type ScenarioResult } from "../src/model.js";
import { asksInstall, S18_ASK, s18Applies, s18NotApplies, wakeshiftPrompt, withoutMarkLines } from "../src/scenarios.js";
import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const tiers = parseTiers(readFileSync(join(dirname(fileURLToPath(import.meta.url)), "../../tiers.yaml"), "utf8"));

test("S18 is asked for by name, and parses", () => {
  assert.deepEqual(parseScenarios("S18"), ["S18"]);
  assert.deepEqual(parseScenarios("S6,S18"), ["S6", "S18"]);
  assert.ok(parseScenarios("S1-S9").every((s) => s !== "S18"));
});

test("S18 runs on Claude Code, Codex, pi, Copilot CLI and the fake driver; others say why not", () => {
  for (const id of ["claude-code", "codex", "pi", "copilot", "fake"]) assert.equal(s18Applies({ id, installPath: "hooks" }), true, id);
  assert.equal(s18Applies({ id: "cursor", installPath: "hooks" }), false);
  assert.match(s18NotApplies({ id: "cursor", installPath: "hooks" }), /not checked/);
  assert.equal(s18Applies({ id: "claude-code", installPath: null }), false);
  assert.equal(s18NotApplies({ id: "chatgpt", installPath: null }), "nothing to install");
});

test("S18's message says nothing about installing or asking first", () => {
  assert.doesNotMatch(S18_ASK, /install|plugin|hooks?|connector|ask (me|the user)|notif/i);
});

test("a prompt for Wakeshift's own command, not the app's MCP tool", () => {
  assert.equal(wakeshiftPrompt({ kind: "approval", chosen: "approved Monitor", asks: "Monitor: curl -s …/v1/stream/wait" }), true);
  assert.equal(wakeshiftPrompt({ kind: "approval", chosen: "approved installer", asks: "claude plugin install wakeshift" }), true);
  assert.equal(wakeshiftPrompt({ kind: "approval", chosen: "approved a Bash command", asks: "~/.wakeshift/bin/wakeshift wait" }), true);
  assert.equal(wakeshiftPrompt({ kind: "approval", chosen: "approved an MCP tool", asks: "wakebench — Watch (MCP) Shipyard delivers through Wakeshift" }), false);
  assert.equal(wakeshiftPrompt({ kind: "approval", chosen: "approved wakebench.watch", asks: "Allow tool wakebench.watch?" }), false);
  assert.equal(wakeshiftPrompt({ kind: "question", chosen: "picked \"no\"", asks: "install Wakeshift?" }), false);
});

test("the plugin's mark in a dialog isn't a question; an install question is", () => {
  assert.equal(asksInstall(withoutMarkLines("Watching.\n  │ claude-code; wakeshift-plugin=1.0; trusts=0123456789abcdef\n")), false);
  assert.equal(asksInstall("Do you want me to install Wakeshift's plugin?"), true);
});

test("S18 grades: tier 1 passes only with no question, no Wakeshift prompt and no offer in Wakeshift's text", () => {
  const r = (metrics: ScenarioResult["metrics"], works = true): ScenarioResult => ({ id: "S18", harness: "claude-code", status: "ran", works, metrics, steps: [], notes: [] });
  const t1 = tiers.tiers.find((t) => t.id === "T1");
  assert.equal(grade(r({ watch_called: true, questions: 0, wakeshift_prompts: 0, install_offer_in_result: false, woke: true, wake_ms: 2000 }), t1).grade, "pass");
  assert.equal(grade(r({ watch_called: true, questions: 1, wakeshift_prompts: 0, install_offer_in_result: false, woke: true, wake_ms: 2000 }), t1).grade, "below-bar");
  assert.equal(grade(r({ watch_called: true, questions: 0, wakeshift_prompts: 1, install_offer_in_result: false, woke: true, wake_ms: 2000 }), t1).grade, "below-bar");
  assert.equal(grade(r({ watch_called: true, questions: 0, wakeshift_prompts: 0, woke: false }, false), t1).grade, "fail");
});
