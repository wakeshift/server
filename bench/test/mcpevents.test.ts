// MCP Events: the official conformance suite's runner (src/eventsconformance.ts,
// its pin and expected failures) and S10's spec-client helpers (src/s10.ts).
import { strict as assert } from "node:assert";
import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { test } from "node:test";
import { fileURLToPath } from "node:url";
import { collapse, EVENTS_SCENARIOS, grade, loadExpected, summary, type Check, type Pin } from "../src/eventsconformance.js";
import { parseScenarios } from "../src/model.js";
import { codesOf, Reader, s10Summary, sameOrder } from "../src/s10.js";

const bench = join(dirname(fileURLToPath(import.meta.url)), "../..");
const read = (f: string) => readFileSync(join(bench, f), "utf8");
const pin = JSON.parse(read("mcp-events/conformance.json")) as Pin;

test("the suite is pinned to a full commit of conformance#521's branch", () => {
  assert.match(pin.sha, /^[0-9a-f]{40}$/);
  assert.equal(pin.pr, "modelcontextprotocol/conformance#521");
  assert.match(pin.repo, /^[\w.-]+\/[\w.-]+$/);
});

test("every expected failure names a scenario, a review item and why", () => {
  const list = loadExpected(read("mcp-events/conformance-expected.yaml"));
  assert.ok(list.length > 0);
  for (const e of list) {
    const [scenario, id] = e.check.split(":");
    assert.ok((EVENTS_SCENARIOS as readonly string[]).includes(scenario), e.check);
    assert.match(id, /^(sep-3415-|wire-)/, e.check);
    assert.match(e.item, /^(P[012] #\d+|bench|suite)$/, `${e.check}: item ${e.item}`);
    assert.ok(e.why.length > 20, e.check);
  }
  // The review's deliberate deviations are listed by their items.
  const item = (c: string, app = "ts-v2") => list.find((e) => e.check === c && (!e.apps || e.apps.includes(app)))?.item;
  // P0 #2 landed: the suite's client isn't in the client table, so it gets a webhook-id per message.
  assert.equal(item("events-webhook-delivery:sep-3415-delivery-webhook-id-per-message"), undefined);
  assert.equal(item("events-poll:sep-3415-mcp-name-missing-rejected"), "P0 #3");
  // Push is served everywhere the conformance job runs.
  for (const app of ["ts-v2", "go-sdk", "py-mcpserver"]) assert.equal(item("events-push:sep-3415-stream-implemented", app), undefined, app);
  assert.equal(item("events-push:sep-3415-stream-gap-resends-active"), "bench");
});

test("the expected list rejects malformed and duplicate entries", () => {
  assert.throws(() => loadExpected("expected:\n  - check: events-poll:x\n    item: P0 #1\n"), /needs check, item and why/);
  assert.throws(() => loadExpected("expected:\n  - check: sep-3415-x\n    item: bench\n    why: y\n"), /<scenario>:<check id>/);
  assert.throws(() => loadExpected("expected:\n  - check: events-poll:a\n    item: bench\n    why: y\n  - check: events-poll:a\n    item: bench\n    why: z\n"), /listed twice/);
  assert.throws(() => loadExpected("expected:\n  - check: events-poll:a\n    item: bench\n    why: y\n    apps: [ts-v2]\n  - check: events-poll:a\n    item: bench\n    why: z\n"), /listed twice/);
  assert.throws(() => loadExpected("expected:\n  - check: events-poll:a\n    item: bench\n    why: y\n    apps: []\n"), /apps/);
  // The same check for disjoint apps is fine.
  assert.equal(loadExpected("expected:\n  - check: events-poll:a\n    item: P0 #4\n    why: y\n    apps: [ts-v2]\n  - check: events-poll:a\n    item: suite\n    why: z\n    apps: [go-sdk]\n").length, 2);
});

test("grading: an unlisted failure fails, a listed one that passes is stale, apps scope entries", () => {
  const expected = loadExpected(
    ["expected:", "  - check: events-poll:a", "    item: P0 #3", "    why: listed", "  - check: events-poll:b", "    item: P0 #4", "    why: only ts", "    apps: [ts-v2]", "  - check: events-push:gone", "    item: P1 #1", "    why: not emitted"].join("\n"),
  );
  const checks: Check[] = [
    { id: "a", scenario: "events-poll", status: "FAILURE" },
    { id: "b", scenario: "events-poll", status: "FAILURE" },
    { id: "a", scenario: "events-webhook", status: "FAILURE" },
    { id: "c", scenario: "events-poll", status: "SUCCESS" },
    { id: "d", scenario: "events-poll", status: "WARNING" },
    { id: "e", scenario: "events-push", status: "SKIPPED" },
  ];
  const ts = grade(checks, expected, "ts-v2");
  assert.deepEqual(
    ts.outcomes.map((o) => o.verdict),
    ["expected-fail", "expected-fail", "unexpected-fail", "pass", "warn", "skip"],
  );
  // An entry for a scenario that didn't run isn't missing.
  assert.deepEqual(grade(checks.slice(0, 2), expected, "ts-v2").missing, []);
  assert.deepEqual(
    ts.missing.map((m) => m.check),
    ["events-push:gone"],
  );
  const go = grade(checks, expected, "go-sdk");
  assert.equal(go.outcomes[1].verdict, "unexpected-fail");
  const fixed = grade([{ id: "a", scenario: "events-poll", status: "SUCCESS" }], expected, "ts-v2");
  assert.equal(fixed.outcomes[0].verdict, "stale");
  const md = summary(pin, "ts-v2", ts.outcomes, ts.missing);
  assert.match(md, /1 unexpected failures/);
  assert.match(md, /Needs attention/);
  assert.match(md, /`events-push:gone`/);
});

test("collapse keeps the most severe status per check, and drops INFO", () => {
  const out = collapse([
    { id: "x", scenario: "events-poll", status: "SUCCESS" },
    { id: "x", scenario: "events-poll", status: "FAILURE", message: "bad" },
    { id: "x", scenario: "events-poll", status: "SUCCESS" },
    { id: "y", scenario: "events-poll", status: "INFO" },
  ]);
  assert.deepEqual(out, [{ id: "x", scenario: "events-poll", status: "FAILURE", message: "bad" }]);
});

test("S10 runs on its own, not through bench/run", () => {
  assert.throws(() => parseScenarios("S10"), /make bench-s10/);
  assert.ok(!parseScenarios("S1-S21").includes("S10" as never));
});

test("S10's reader takes occurrences in order, and stops at its deadline", async () => {
  async function* gen() {
    yield { data: { code: "WB-1" } };
    yield { data: { code: "WB-2" } };
    await new Promise((r) => setTimeout(r, 1000));
    yield { data: { code: "WB-3" } };
  }
  const r = new Reader(gen());
  assert.deepEqual(codesOf(await r.take(2, 500)), ["WB-1", "WB-2"]);
  assert.deepEqual(codesOf(await r.take(1, 50)), []);
  // The read that timed out is the next take's, so WB-3 isn't lost.
  assert.deepEqual(codesOf(await r.take(1, 2000)), ["WB-3"]);
  assert.ok(sameOrder(["a", "b"], ["a", "b"]));
  assert.ok(!sameOrder(["b", "a"], ["a", "b"]));
  assert.ok(!sameOrder(["a"], ["a", "b"]));
});

test("S10's summary marks a known failure with its review item", () => {
  const md = s10Summary({
    id: "S10",
    client: "@mastra/mcp 2.7.0",
    app: "ts-v2",
    outcome: "pass",
    checks: [
      { name: "delivery", ok: true, detail: "sent a; got a" },
      { name: "replay (webhook, after a lapse)", ok: false, known: "P0 #5: no replay", detail: "got nothing" },
    ],
  });
  assert.match(md, /\| delivery \| pass \|/);
  assert.match(md, /known \(P0 #5\)/);
});
