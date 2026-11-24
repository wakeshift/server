import { strict as assert } from "node:assert";
import { test } from "node:test";
import { messagesOf, parseDescription, viaOf, wakeshiftPart } from "../src/external.js";
import { DONE_WORDS, gotLineRe, gotRe, isStart, isWatch, toldResult } from "../src/scenarios.js";

const BEACON = "(For Wakeshift's hook on this computer, not for you: wakeshift-beacon: abc.def.ghi)";

const desc = {
  name: "acme",
  title: "Acme",
  start_tools: ["start"],
  status_tools: ["status"],
  subscribing: ["start", "status"],
  prompts: { intro: "i", next: "n {n}", s18: "s", natural: "a" },
};

test("parseDescription takes the last JSON line and checks it", () => {
  assert.equal(parseDescription(`npm notice\n${JSON.stringify(desc)}\n`).name, "acme");
  assert.throws(() => parseDescription(JSON.stringify({ ...desc, name: "Acme App" })), /lowercase/);
  assert.throws(() => parseDescription(JSON.stringify({ ...desc, prompts: { ...desc.prompts, natural: "" } })), /natural/);
  assert.throws(() => parseDescription(JSON.stringify({ ...desc, subscribing: undefined })), /subscribing/);
  assert.throws(() => parseDescription(JSON.stringify({ ...desc, start_tools: [1] })), /start_tools/);
  assert.throws(() => parseDescription(JSON.stringify({ ...desc, title: "" })), /title/);
  assert.throws(() => parseDescription("nothing"), /no JSON/);
});

test("wakeshiftPart: Wakeshift's block of its own after the vendor's content, the beacon trailer", () => {
  const p = wakeshiftPart({ content: [{ type: "text", text: '{"id":"Sync_1"}' }, { type: "text", text: "Wakeshift: start the Monitor… Pass session_key=…" }, { type: "text", text: BEACON }] });
  assert.equal(p.shape, "own-block");
  assert.equal(p.added, "Wakeshift: start the Monitor… Pass session_key=…");
  assert.equal(p.beacon, true);
  assert.equal(p.mismatch, false);
});

test("wakeshiftPart: a string result, joined after a blank line", () => {
  const p = wakeshiftPart({ content: [{ type: "text", text: `Sync started: Sync_1\nstatus running\n\nTo be woken, Pass session_key="wsc_x" to this app's tools.\n\n${BEACON}` }] });
  assert.equal(p.shape, "joined");
  assert.match(p.added, /^To be woken/);
  assert.doesNotMatch(p.added, /wakeshift-beacon/);
  assert.equal(p.beacon, true);
});

test("wakeshiftPart: the beacon in _meta, a structured field, and a vendor that reshaped the text", () => {
  const meta = { "wakeshift/connector": "x" };
  const same = wakeshiftPart({ content: [{ type: "text", text: "{}" }, { type: "text", text: "Wakeshift: connected." }], structuredContent: { wakeshift: "Wakeshift: connected." }, _meta: meta });
  assert.equal(same.beacon, true);
  assert.equal(same.mismatch, false);
  const cut = wakeshiftPart({ content: [{ type: "text", text: "{}" }, { type: "text", text: "Wakeshift: conn…" }], structuredContent: { wakeshift: "Wakeshift: connected." } });
  assert.equal(cut.mismatch, true);
  const only = wakeshiftPart({ content: [{ type: "text", text: "{}" }], structuredContent: { wakeshift: "Wakeshift: connected." } });
  assert.equal(only.shape, "structured-only");
  const none = wakeshiftPart({ content: [{ type: "text", text: "plain" }] });
  assert.deepEqual([none.shape, none.added, none.beacon], ["none", "", false]);
});

test("messagesOf reads JSON, batches and SSE", () => {
  assert.deepEqual(messagesOf('{"id":1}', "application/json"), [{ id: 1 }]);
  assert.deepEqual(messagesOf('[{"id":1},{"id":2}]', "application/json"), [{ id: 1 }, { id: 2 }]);
  assert.deepEqual(messagesOf('event: message\ndata: {"id":3}\n\n: ping\n\ndata: {"id":4}\n\n', "text/event-stream"), [{ id: 3 }, { id: 4 }]);
  assert.deepEqual(messagesOf("not json", "application/json"), []);
});

test("viaOf names connections as the core's event status does", () => {
  assert.equal(viaOf({ runtime: "stream", connector_seen: true, connected_via: "stream" }), "hooks");
  assert.equal(viaOf({ runtime: "connector", connector_seen: false, connected_via: "stream" }), "stream");
  assert.equal(viaOf({ runtime: "stream", connector_seen: false, connected_via: "poll" }), "poll");
  assert.equal(viaOf({ runtime: "stream", connector_seen: false, connected_via: "webhook" }), "webhook");
  assert.equal(viaOf({ runtime: "stream", connector_seen: true, connected_via: "" }), null);
});

test("an external app's calls, codes and news", () => {
  assert.ok(isWatch({ tool: "start_task", subscribes: true }));
  assert.ok(isWatch({ tool: "watch" }));
  assert.ok(!isWatch({ tool: "get_build" }));
  assert.ok(isStart({ tool: "start_job", role: "start" }));
  assert.ok(gotRe("Sync_4d71a2a1a490").test("GOT Sync_4d71a2a1a490"));
  assert.ok(gotRe("s_mock1_l9.x").test("GOT s_mock1_l9.x"));
  assert.ok(!gotRe("s_mock1_l9.x").test("GOT s_mock1_l9Xx"));
  assert.ok(gotRe("1234").test("GOT 1234") && !gotRe("1234").test("GOT 12345"));
  assert.ok(gotLineRe("897128").test("some\n⏺ GOT 897128\n") && gotLineRe("897128").test("  GOT 897128"));
  assert.ok(gotLineRe("7f4f-aebb").test("⏺ GOT job-mw4obkst-7f4f-aebb"));
  assert.ok(!gotLineRe("897128").test('⏺ Build 897128 is scheduled. I\'ll reply "GOT 897128" when it finishes.'));
  assert.ok(toldResult("Your sync of example.com is complete: 3 pages.", null, DONE_WORDS));
  assert.ok(toldResult("The sync finished: 3 pages.", null, DONE_WORDS));
  assert.ok(!toldResult("I'll tell you when the sync finished.", null, DONE_WORDS));
  assert.ok(!toldResult("The sync (Sync_1) is still in_progress. It hasn't finished.", null, DONE_WORDS), "the job's ID alone isn't news");
  assert.ok(!toldResult("The sync finished: 3 pages.", "x"), "the toy app's words stay as they were");
  const before = '  2. Send any message afterwards ("done" is\n     enough). The plugin connects.';
  assert.ok(!toldResult('2. Send any message afterwards ("done" is enough).', null, DONE_WORDS, before), "a re-wrapped old sentence isn't news");
  assert.ok(toldResult("The report is done: 12 rows.", null, DONE_WORDS, before));
  assert.ok(!toldResult('Send any message.\n"done".', null, DONE_WORDS, "x"), "a fragment isn't news");
  assert.ok(!toldResult("✻ Brewed for 3s · done 11:38 PM\nGet to finished work sooner with Opus 5.5.", null, DONE_WORDS));
  assert.ok(!toldResult("⏺ Woken by Wakeshift\n✽ Whisking…\n  ⎿  Tip: Press Esc twice to rewind once you're done. Ready to go.", null, DONE_WORDS));
});
