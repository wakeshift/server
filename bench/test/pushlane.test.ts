// Tests for the realistic tier's push lane (R7-R9): opencode subscribing
// itself with MCP Events push, no plugin. How an ask's outcome is read, the
// CI app's record of events/stream, and the opencode driver's settings.

import { strict as assert } from "node:assert";
import { test } from "node:test";
import { legacySessions, streamsIn, type CiCall, type CiStream } from "../app/ci.js";
import { BlockedError, loginPrompt, QuotaError, quotaPrompt } from "../drivers/base.js";
import { eventsWithoutFlag, pushFlag, pushLane, pushUnavailable, serversEntry, standalone } from "../drivers/opencode/index.js";
import { keyOf } from "../src/grade.js";
import { parseScenarios, SCENARIOS, type ScenarioResult } from "../src/model.js";
import { NATIVE_LINE, PUSH_LANE, pushOutcome } from "../src/pushlane.js";

const call = (added: string): CiCall => ({ at: "2026-11-12T00:00:01Z", tool: "watch_build", lane: "opencode-rp", client: "opencode", args: { number: 413 }, text: added, added, isError: false, events_in_result: [] });
const stream = (n: number, at = "2026-11-12T00:00:02Z"): CiStream => ({ at, lane: "opencode-rp", event: "build.finished", arguments: { pipeline: "acme/web", number: n }, protocol: "2025-11-25", resumed: false });
const LINE = 'To be woken by build.finished with {"number":413,"pipeline":"acme/web"}, subscribe to it (MCP Events).';
const OFFER =
  "Wakeshift can't wake this conversation yet. Ask the user now whether to install Wakeshift's plugin for opencode … If they say no, install nothing, and tell them you won't be notified here when something arrives.";

test("push lane: the native line, no offer, and the agent's own stream for the build work", () => {
  const o = pushOutcome({ build: 413, sub: call(LINE + "\n\nPass session_key=\"wsc_x\" to this app's tools from now on."), added: LINE, streams: [stream(413)], how: "none", relayed: false, cmd: null });
  assert.equal(o.works, true, o.reason ?? "");
  assert.equal(o.metrics.native_line, true);
  assert.equal(o.metrics.offer_in_result, false);
  assert.equal(o.metrics.streamed, true);
  assert.equal(o.metrics.stream_protocol, "2025-11-25");
  assert.match('To be woken by build.finished with {"number":413}, subscribe to it with wakeshift_voucher="wsv_x" (MCP Events).', NATIVE_LINE);
});

test("push lane: the install offer beside the native line fails (the v1.4.50 gate's bug 1)", () => {
  const added = `${LINE}\n\n${OFFER}`;
  const o = pushOutcome({ build: 413, sub: call(added), added, streams: [stream(413)], how: "none", relayed: false, cmd: "curl -fsSL http://127.0.0.1:1/install/opencode.sh | sh" });
  assert.equal(o.works, false);
  assert.match(o.reason ?? "", /offered the install/);
});

test("push lane: no native line, the agent asking to install, or no stream of its own each fail", () => {
  const youre = 'You\'re subscribed to build.finished with {"number":413}: its events reach you.';
  assert.match(pushOutcome({ build: 413, sub: call(youre), added: youre, streams: [stream(413)], how: "none", relayed: false, cmd: null }).reason ?? "", /no native line/);
  assert.match(pushOutcome({ build: 413, sub: call(LINE), added: LINE, streams: [stream(413)], how: "question", relayed: true, cmd: null }).reason ?? "", /put an install/);
  // A stream for another build (R7's, in R9) isn't this one's.
  assert.match(pushOutcome({ build: 414, sub: call(LINE), added: LINE, streams: [stream(413)], how: "none", relayed: false, cmd: null }).reason ?? "", /didn't subscribe itself/);
  assert.match(pushOutcome({ build: null, sub: undefined, added: "", streams: [], how: "none", relayed: false, cmd: null }).reason ?? "", /didn't start/);
  assert.match(pushOutcome({ build: 413, sub: undefined, added: "", streams: [], how: "none", relayed: false, cmd: null }).reason ?? "", /subscribing tool/);
});

test("the CI app records events/stream requests, never a cursor", () => {
  const got = streamsIn([
    { jsonrpc: "2.0", id: 1, method: "events/stream", params: { name: "build.finished", arguments: { pipeline: "acme/web", number: 413 } } },
    { jsonrpc: "2.0", id: 2, method: "events/stream", params: { name: "build.finished", arguments: { number: 413 }, cursor: "c2.secret" } },
    { jsonrpc: "2.0", id: 3, method: "tools/call", params: { name: "watch_build", arguments: {} } },
  ]);
  assert.deepEqual(got, [
    { event: "build.finished", arguments: { pipeline: "acme/web", number: 413 }, resumed: false },
    { event: "build.finished", arguments: { number: 413 }, resumed: true },
  ]);
  assert.doesNotMatch(JSON.stringify(got), /c2\.secret/);
});

test("the push lane's scenarios: R7-R9, by name, with lines for the report", () => {
  assert.deepEqual(PUSH_LANE, ["R7", "R8", "R9"]);
  for (const s of PUSH_LANE) assert.ok(SCENARIOS.includes(s));
  assert.deepEqual(parseScenarios("R7-R9"), ["R7", "R8", "R9"]);
  const r = (id: "R7" | "R8" | "R9", metrics: ScenarioResult["metrics"]): ScenarioResult => ({ id, harness: "opencode", status: "ran", metrics, steps: [], notes: [] });
  assert.equal(keyOf(r("R7", { started_build: true, native_line: true, offer_in_result: false, streamed: true })), "native line, subscribed itself");
  assert.equal(keyOf(r("R7", { started_build: true, native_line: true, offer_in_result: true, streamed: false })), "native line, install offered, didn't subscribe itself");
  assert.equal(keyOf(r("R8", { woke: true, told_ms: 4200 })), "told in 4.2s");
});

test("opencode's push settings: mcp_events in OPENCODE_FLAGS, and 2.x's mcp.servers shape for a protocol", () => {
  assert.equal(pushFlag("mcp_events"), true);
  assert.equal(pushFlag("foo, mcp_events"), true);
  assert.equal(pushFlag("mcp_events_x"), false);
  assert.equal(pushFlag(undefined), false);
  const entry = { type: "remote", url: "http://127.0.0.1:1/mcp/x", enabled: true };
  assert.deepEqual(serversEntry("shipyard", entry), { shipyard: entry });
  assert.deepEqual(serversEntry("shipyard", entry, "legacy"), { servers: { shipyard: { ...entry, protocol: "legacy" } } });
});

// opencode 2.4.0 has no mcp_events flag (Events always on): WB_OPENCODE_PUSH=1
// marks its push lane, which runs --standalone like any 2.x lane, so it never
// uses or leaves behind the user's shared service on port 49374.
test("opencode 2.4's push lane: WB_OPENCODE_PUSH=1, and --standalone", () => {
  assert.equal(pushLane({ WB_OPENCODE_PUSH: "1" }), true);
  assert.equal(pushLane({ WB_OPENCODE_FLAGS: "mcp_events" }), true);
  assert.equal(pushLane({ WB_OPENCODE_PUSH: "0" }), false);
  assert.equal(pushLane({}), false);
  assert.equal(standalone({ WB_OPENCODE_PUSH: "1" }, 2), true);
  assert.equal(standalone({}, 2), true);
  assert.equal(standalone({}, 1), false);
  assert.equal(standalone({ WB_OPENCODE_FLAGS: "mcp_events" }, 2), false);
  // WB_OPENCODE_PUSH=1 only where Events need no flag (2.4 and later); else the lane is blocked, not run.
  assert.equal(eventsWithoutFlag("opencode v2.4.0"), true);
  assert.equal(eventsWithoutFlag("2.10.1"), true);
  assert.equal(eventsWithoutFlag("opencode v3.0.0"), true);
  assert.equal(eventsWithoutFlag("opencode v2.3.0"), false);
  assert.equal(eventsWithoutFlag("1.19.5"), false);
  assert.equal(eventsWithoutFlag(undefined), false);
  assert.equal(pushUnavailable({ WB_OPENCODE_PUSH: "1" }, "opencode v2.4.0"), null);
  assert.match(pushUnavailable({ WB_OPENCODE_PUSH: "1" }, "opencode v2.3.0") ?? "", /needs opencode 2\.4 or later/);
  assert.match(pushUnavailable({ WB_OPENCODE_PUSH: "1" }, "1.19.5") ?? "", /needs opencode 2\.4 or later/);
  assert.equal(pushUnavailable({ WB_OPENCODE_PUSH: "1", WB_OPENCODE_FLAGS: "mcp_events" }, "opencode v2.3.0"), null);
  assert.equal(pushUnavailable({}, "1.19.5"), null);
});

// opencode 2.4.0's free model, out of quota mid-run (a push lane run on
// 2026-11-23): the lane is blocked, not failed. Only the bottom of the
// screen counts, as for login prompts.
test("quota: opencode's used-up free model blocks the lane", () => {
  const screen = "  ┃  Kick off a build of main\n     + 1 thought, 1 tool\n  ┃  Free usage limit reached. Connect a provider or subscribe to OpenCode Go to keep going, or switch models.\n  ┃  Build · DeepSeek V4.1 Flash Free";
  assert.equal(quotaPrompt("opencode", screen), true);
  assert.equal(quotaPrompt("opencode", "  opencode\n  > Ask anything...\n  enter send   ctrl+p commands   tab agents"), false);
  assert.equal(quotaPrompt("cursor", screen), false);
  assert.equal(quotaPrompt("opencode", [screen, ...Array(40).fill("…"), "Ask anything"].join("\n")), false);
  const e = new QuotaError("opencode");
  assert.ok(e instanceof BlockedError);
  assert.match(e.message, /opencode's model quota is used up/);
  assert.equal(loginPrompt("opencode", screen), false);
});

test("Shipyard keeps legacy sessions only with WB_CI_LEGACY=sessions", () => {
  assert.equal(legacySessions({ WB_CI_LEGACY: "sessions" }), true);
  assert.equal(legacySessions({}), false);
  assert.equal(legacySessions({ WB_CI_LEGACY: "stateless" }), false);
});
