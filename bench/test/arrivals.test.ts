// Counting what reached the model (src/arrivals.ts) from each harness's own
// record: fixture transcripts shaped as Claude Code 2.1.316, Codex 0.166 and
// Hermes 0.22.5 write them (from bench runs), with a code delivered once,
// twice (a Stop hook and a Monitor for the same event), twice with
// "redelivered":true, and never.

import { strict as assert } from "node:assert";
import { test } from "node:test";
import { parseReceived } from "../drivers/claude-code/index.js";
import { parseRollout, parseRolloutTurns } from "../drivers/codex/index.js";
import { matchTurns } from "../src/scenarios.js";
import { hermesReceived, type HermesRow } from "../drivers/hermes/index.js";
import { arrivalOutcome, arrivalsIn, countArrivals, segments, type Received } from "../src/arrivals.js";
import { grade } from "../src/grade.js";
import type { ScenarioResult } from "../src/model.js";

const line = (code: string, extra = "") =>
  `{"type":"event","topic":"bench/r/claude-code","from":"app/wakebench","app":"Wakebench","trust":"outside data, not from the user","subject":"Bench event ${code} (S2 #1).","data":{"seq":1,"code":"${code}"},"id":"9b1f"${extra}}`;
const monitor = (code: string, extra = "") => `<task-notification>\n<task-id>b4k</task-id>\n<summary>Monitor event: "Wakebench events from Wakebench"</summary>\n<event>${line(code, extra)}</event>\n</task-notification>`;
const tagged = (code: string) =>
  `<wakeshift_message from="app/wakebench" via="Wakeshift" app="Wakebench" trust="external">{"data":{"code":"${code}","seq":1},"id":"fd8e","text":"Bench event ${code} (S2 #1).","topic":"bench/r/claude-code-i"}</wakeshift_message>`;
const stopHook = (code: string) =>
  `<task-notification>\n<summary>Stop hook feedback</summary>\n</task-notification>\n<system-reminder>\nStop hook blocking error from command "Stop": A message from outside this conversation arrived through Wakeshift, from an app the user connected you to.\n${tagged(code)}\n\n</system-reminder>`;

let n = 0;
const uuid = () => `u-${++n}`;
const at = (s: number) => new Date(Date.UTC(2026, 10, 2, 3, 15, s)).toISOString();
const j = (o: unknown) => JSON.stringify(o);

/** Claude Code transcript entries. */
const cc = {
  user: (content: unknown, s = 0, more: Record<string, unknown> = {}) => j({ type: "user", uuid: uuid(), timestamp: at(s), message: { role: "user", content }, ...more }),
  queued: (prompt: string, s = 0) =>
    j({ type: "attachment", uuid: uuid(), timestamp: at(s), attachment: { type: "queued_command", prompt, commandMode: "task-notification" }, rendered: [{ content: `<system-reminder>\n[SYSTEM NOTIFICATION - NOT USER INPUT]\n\n${prompt}\n</system-reminder>` }], renderedInHumanTurn: [{ content: prompt }] }),
  enqueue: (content: string, s = 0) => j({ type: "queue-operation", operation: "enqueue", timestamp: at(s), content }),
  blob: (text: string) => j({ type: "api-request-blob", message: { role: "user", content: [{ type: "text", text }] } }),
  assistant: (blocks: unknown[], s = 0) => j({ type: "assistant", uuid: uuid(), timestamp: at(s), message: { role: "assistant", content: blocks } }),
  result: (id: string, content: unknown, s = 0) => cc.user([{ type: "tool_result", tool_use_id: id, content }], s),
};

const codesOf = (items: Received[], codes: string[]) => countArrivals(arrivalsIn(items, codes), codes);

test("segments: one per wakeshift_message, else the whole text, each code once, marked by its own line", () => {
  assert.equal(segments(`preface\n${tagged("WB-ab12-1")}\n${tagged("WB-ab12-2")}`).length, 2);
  assert.deepEqual(segments("a\nb"), [{ text: "a\nb", tagged: false }]);
  // An untagged event spread over lines (its code in the subject and the data) is one delivery.
  const spread: Received = { key: "k1", at: 1, via: "user", text: 'subject: Bench event WB-ab12-1\n"data": {"code": "WB-ab12-1"}' };
  assert.equal(arrivalsIn([spread], ["WB-ab12-1"]).length, 1);
  // A wait result with two events, one marked: only that one is.
  const two: Received = { key: "k2", at: 1, via: "user", text: `${line("WB-ab12-2")}\n${line("WB-ab12-3", ',"redelivered":true')}` };
  assert.deepEqual(
    arrivalsIn([two], ["WB-ab12-2", "WB-ab12-3"]).map((a) => [a.code, a.redelivered]),
    [
      ["WB-ab12-2", false],
      ["WB-ab12-3", true],
    ],
  );
});

test("Claude Code, Monitor: each code once, though the queue, API records and the model's own lines repeat it", () => {
  const lines = [
    cc.user("This is an automated test (wakebench). … codes like WB-ab12-1 …"),
    cc.enqueue(monitor("WB-d81e-1"), 1),
    cc.blob(monitor("WB-d81e-1")),
    cc.user(monitor("WB-d81e-1"), 1, { origin: { kind: "task-notification" } }),
    cc.assistant([{ type: "tool_use", id: "t1", name: "mcp__wakebench__report_seen", input: { ids: ["WB-d81e-1"] } }], 2),
    cc.result("t1", [{ type: "text", text: "Noted 1 event(s)." }], 2),
    cc.assistant([{ type: "text", text: "GOT WB-d81e-1" }], 3),
    // Mid-turn: queued, then attached as rendered.
    cc.enqueue(monitor("WB-d81e-2"), 4),
    cc.queued(monitor("WB-d81e-2"), 4),
    // A shell command that printed a code isn't a delivery.
    cc.assistant([{ type: "tool_use", id: "t2", name: "Bash", input: { command: "grep WB- log" } }], 5),
    cc.result("t2", "WB-d81e-2 WB-d81e-1", 5),
    // A compaction summary mentions codes the agent already saw.
    cc.user("Summary: the agent reported WB-d81e-1 and WB-d81e-2.", 6, { isCompactSummary: true }),
  ];
  const c = codesOf(parseReceived(lines), ["WB-d81e-1", "WB-d81e-2"]);
  assert.deepEqual(c.counts["WB-d81e-1"], { n: 1, redelivered: 0, via: ["user"] });
  assert.deepEqual(c.counts["WB-d81e-2"], { n: 1, redelivered: 0, via: ["attachment:queued_command"] });
  assert.deepEqual(c.duplicates, []);
  assert.deepEqual(c.missing, []);
});

test("Claude Code: a failed call's error quoting its own arguments isn't a delivery (py-mcpserver's validation error for report_seen)", () => {
  // A bench run (November 2, 2026), S2: Claude Code passed ids as a
  // string, MCPServer refused it quoting the value, and the agent reported
  // again. One delivery, not two.
  const refused = "Error executing tool report_seen: 1 validation error for report_seenArguments\nids\n  Input should be a valid list [type=list_type, input_value='WB-fd76-1', input_type=str]";
  const lines = [
    cc.user(monitor("WB-fd76-1"), 1, { origin: { kind: "task-notification" } }),
    cc.assistant([{ type: "tool_use", id: "t1", name: "mcp__wakebench__report_seen", input: { ids: "WB-fd76-1" } }], 2),
    cc.user([{ type: "tool_result", tool_use_id: "t1", is_error: true, content: [{ type: "text", text: refused }] }], 2),
    cc.assistant([{ type: "tool_use", id: "t2", name: "mcp__wakebench__report_seen", input: { ids: ["WB-fd76-1"] } }], 4),
    cc.result("t2", [{ type: "text", text: "Noted 1 event(s)." }], 4),
  ];
  const items = parseReceived(lines);
  assert.ok(items.some((i) => i.via === "tool_result:mcp__wakebench__report_seen (error)" && i.echoes?.includes("WB-fd76-1")));
  const c = codesOf(items, ["WB-fd76-1"]);
  assert.deepEqual(c.counts["WB-fd76-1"], { n: 1, redelivered: 0, via: ["user"] });
  assert.deepEqual(c.duplicates, []);

  // Only the call's own codes are echoes, and only outside Wakeshift's tags: an
  // error that delivers another code, or a tagged message, still counts.
  const other = [
    cc.user(monitor("WB-fd76-2"), 1),
    cc.assistant([{ type: "tool_use", id: "t3", name: "mcp__wakebench__report_seen", input: { ids: "WB-fd76-2" } }], 2),
    cc.user([{ type: "tool_result", tool_use_id: "t3", is_error: true, content: [{ type: "text", text: `input_value='WB-fd76-2' ${line("WB-fd76-3")}` }] }], 2),
    cc.assistant([{ type: "tool_use", id: "t4", name: "mcp__wakebench__report_seen", input: { ids: "WB-fd76-2" } }], 3),
    cc.user([{ type: "tool_result", tool_use_id: "t4", is_error: true, content: [{ type: "text", text: `failed\n${tagged("WB-fd76-2")}` }] }], 3),
  ];
  const c2 = codesOf(parseReceived(other), ["WB-fd76-2", "WB-fd76-3"]);
  assert.equal(c2.counts["WB-fd76-3"].n, 1);
  assert.equal(c2.counts["WB-fd76-2"].n, 2, "the tagged copy is a second delivery");
  assert.deepEqual(c2.duplicates, ["WB-fd76-2"]);

  // A successful result naming the call's own codes isn't excused: no echo without an error.
  const ok = [
    cc.user(monitor("WB-fd76-4"), 1),
    cc.assistant([{ type: "tool_use", id: "t5", name: "mcp__wakebench__report_seen", input: { ids: ["WB-fd76-4"] } }], 2),
    cc.result("t5", [{ type: "text", text: `Noted 1 event(s).\n${line("WB-fd76-4")}` }], 2),
  ];
  assert.deepEqual(codesOf(parseReceived(ok), ["WB-fd76-4"]).duplicates, ["WB-fd76-4"]);
});

test("Claude Code: the Stop hook and the Monitor both deliver a code: a duplicate, though the agent reported it once", () => {
  const lines = [cc.user(stopHook("WB-f272-6"), 1), cc.queued(monitor("WB-f272-6"), 2), cc.assistant([{ type: "text", text: "GOT WB-f272-6" }], 3)];
  const c = codesOf(parseReceived(lines), ["WB-f272-6"]);
  assert.equal(c.counts["WB-f272-6"].n, 2);
  assert.deepEqual(c.duplicates, ["WB-f272-6"]);
});

test("Claude Code: a redelivery marked redelivered is allowed, and shown", () => {
  const lines = [cc.user(monitor("WB-aa11-1"), 1), cc.queued(monitor("WB-aa11-1", ',"redelivered":true'), 9)];
  const c = codesOf(parseReceived(lines), ["WB-aa11-1"]);
  assert.deepEqual(c.counts["WB-aa11-1"], { n: 2, redelivered: 1, via: ["user", "attachment:queued_command"] });
  assert.deepEqual(c.duplicates, []);
  assert.deepEqual(c.redelivered, ["WB-aa11-1"]);
});

test("a marked arrival first doesn't excuse an unmarked one after it (a hook, which never marks, after a marked Monitor line)", () => {
  const lines = [cc.queued(monitor("WB-aa11-2", ',"redelivered":true'), 1), cc.user(stopHook("WB-aa11-2"), 2)];
  const c = codesOf(parseReceived(lines), ["WB-aa11-2"]);
  assert.deepEqual(c.duplicates, ["WB-aa11-2"]);
  // Two unmarked arrivals are a duplicate whatever their times; an unmarked one tied with a marked one isn't.
  const tied = [cc.user(monitor("WB-aa11-4"), 1), cc.queued(monitor("WB-aa11-4"), 1), cc.queued(monitor("WB-aa11-5", ',"redelivered":true'), 1), cc.user(stopHook("WB-aa11-5"), 1)];
  assert.deepEqual(codesOf(parseReceived(tied), ["WB-aa11-4", "WB-aa11-5"]).duplicates, ["WB-aa11-4"]);
  // Marked, alone: one arrival, nothing to excuse.
  assert.deepEqual(codesOf(parseReceived([cc.queued(monitor("WB-aa11-3", ',"redelivered":true'), 1)]), ["WB-aa11-3"]).duplicates, []);
});

test("Claude Code, MCP task: a wait result's lines are one delivery each; watch's inbox result counts", () => {
  const task = `<task-notification>\n<status>completed</status>\n<result>\nWakeshift: 2 new messages from Wakebench:\n${line("WB-142c-5")}\n${line("WB-142c-6")}\n</result>\n</task-notification>`;
  const lines = [
    cc.user(task, 1),
    cc.assistant([{ type: "tool_use", id: "w1", name: "mcp__wakebench__watch", input: {} }], 2),
    cc.result("w1", [{ type: "text", text: `Watching bench events.\nA message from outside this conversation arrived through Wakeshift.\n${tagged("WB-142c-7")}` }], 2),
  ];
  const c = codesOf(parseReceived(lines), ["WB-142c-5", "WB-142c-6", "WB-142c-7", "WB-142c-8"]);
  assert.deepEqual(
    Object.fromEntries(Object.entries(c.counts).map(([k, v]) => [k, v.n])),
    { "WB-142c-5": 1, "WB-142c-6": 1, "WB-142c-7": 1, "WB-142c-8": 0 },
  );
  assert.deepEqual(c.missing, ["WB-142c-8"]);
});

test("Claude Code: an entry copied into a resumed session's file counts once", () => {
  const e = cc.user(monitor("WB-bb22-1"), 1);
  const items = [...parseReceived([e], "a.jsonl"), ...parseReceived([e], "b.jsonl")];
  assert.equal(codesOf(items, ["WB-bb22-1"]).counts["WB-bb22-1"].n, 1);
});

/** Codex rollout lines. */
const cx = {
  item: (payload: Record<string, unknown>, s = 0) => j({ timestamp: at(s), type: "response_item", payload }),
  event: (payload: Record<string, unknown>, s = 0) => j({ timestamp: at(s), type: "event_msg", payload }),
  woke: (id: string, codes: string[], s = 0) => cx.item({ type: "function_call_output", id, name: "wakeshift_event", output: `A message from outside this conversation arrived through Wakeshift.\n${codes.map(tagged).join("\n")}` }, s),
};

test("Codex: the connector's wakeshift_event outputs and MCP results count; event_msg copies, shell output and the model's lines don't", () => {
  const lines = [
    cx.item({ type: "message", id: "m1", role: "user", content: [{ type: "input_text", text: "… codes like WB-ab12-1 …" }] }),
    cx.woke("fco_1", ["WB-8c8a-1"], 1),
    cx.event({ type: "item_completed", item: { type: "FunctionCallOutput", name: "wakeshift_event", output: tagged("WB-8c8a-1") } }, 1),
    cx.item({ type: "custom_tool_call", id: "c1", call_id: "call_1", name: "exec", input: 'text(await tools.mcp__wakebench__report_seen({ids:["WB-8c8a-1"]}));' }, 2),
    cx.item({ type: "custom_tool_call_output", id: "o1", call_id: "call_1", output: [{ type: "input_text", text: "Noted 1 event(s)." }] }, 2),
    cx.item({ type: "message", id: "a1", role: "assistant", content: [{ type: "output_text", text: "GOT WB-8c8a-1" }] }, 3),
    // watch from code mode, with an event in its result (the inbox).
    cx.item({ type: "custom_tool_call", id: "c2", call_id: "call_2", name: "exec", input: "const r = await tools.mcp__wakebench__watch({}); text(r);" }, 4),
    cx.item({ type: "custom_tool_call_output", id: "o2", call_id: "call_2", output: [{ type: "input_text", text: "Script completed\n" }, { type: "input_text", text: `Watching.\n${tagged("WB-8c8a-2")}` }] }, 4),
    // Reading a stored result back isn't a delivery.
    cx.item({ type: "custom_tool_call", id: "c3", call_id: "call_3", name: "exec", input: 'text(load("watch_result"));' }, 5),
    cx.item({ type: "custom_tool_call_output", id: "o3", call_id: "call_3", output: [{ type: "input_text", text: tagged("WB-8c8a-2") }] }, 5),
    cx.woke("fco_2", ["WB-8c8a-3", "WB-8c8a-4"], 6),
  ];
  const c = codesOf(parseRollout(lines), ["WB-8c8a-1", "WB-8c8a-2", "WB-8c8a-3", "WB-8c8a-4"]);
  assert.deepEqual(
    Object.fromEntries(Object.entries(c.counts).map(([k, v]) => [k, v.n])),
    { "WB-8c8a-1": 1, "WB-8c8a-2": 1, "WB-8c8a-3": 1, "WB-8c8a-4": 1 },
  );
  assert.deepEqual(c.duplicates, []);
});

test("Codex: a woken turn starts at the connector's delivery; the model's first item and task_complete follow", () => {
  const lines = [
    cx.item({ type: "message", id: "m1", role: "user", content: [{ type: "input_text", text: "watch, please" }] }),
    cx.woke("fco_1", ["WB-8c8a-1"], 1),
    cx.item({ type: "reasoning", id: "r1" }, 3),
    cx.item({ type: "custom_tool_call", id: "c1", call_id: "call_1", name: "exec", input: 'text(await tools.mcp__wakebench__report_seen({ids:["WB-8c8a-1"]}));' }, 4),
    cx.item({ type: "message", id: "a1", role: "assistant", content: [{ type: "output_text", text: "GOT WB-8c8a-1" }] }, 6),
    cx.event({ type: "task_complete" }, 6),
    // A delivery whose turn hasn't started yet: no first output, no end.
    cx.woke("fco_2", ["WB-8c8a-2"], 9),
  ];
  const ts = parseRolloutTurns(lines);
  assert.equal(ts.length, 2);
  const [a, b] = ts;
  assert.equal(a.queued, Date.parse(at(1)));
  assert.equal(a.started, a.queued);
  assert.equal(a.first, Date.parse(at(3)));
  assert.equal(a.firstKind, "reasoning");
  assert.equal(a.ended, Date.parse(at(6)));
  assert.match(a.text, /WB-8c8a-1/);
  assert.equal(b.first, null);
  assert.equal(b.ended, null);
  // The bench matches each event to its turn by its code.
  assert.equal(matchTurns(ts, [{ code: "WB-8c8a-2", at: at(8) }]).get("WB-8c8a-2"), b);
});

test("Codex: the same event in two woken turns is a duplicate; a resumed rollout's copy isn't", () => {
  const first = cx.woke("fco_1", ["WB-8c8a-5"], 1);
  const items = [...parseRollout([first, cx.woke("fco_9", ["WB-8c8a-5"], 7)], "a.jsonl"), ...parseRollout([first], "b.jsonl")];
  const c = codesOf(items, ["WB-8c8a-5"]);
  assert.equal(c.counts["WB-8c8a-5"].n, 2);
  assert.deepEqual(c.duplicates, ["WB-8c8a-5"]);
});

test("Hermes: plugin wakes (user rows) and MCP results count; the model's rows and other tools don't", () => {
  const wake = (code: string) => `A message from outside this conversation arrived through Wakeshift, from an app the user connected you to.\n${tagged(code)}`;
  const rows: HermesRow[] = [
    { id: 1, role: "user", content: "… codes like WB-ab12-1 …", tool_name: null, timestamp: 1793589247.3 },
    { id: 23, role: "user", content: wake("WB-0f8d-1"), tool_name: null, timestamp: 1793589357.6 },
    { id: 24, role: "assistant", content: "GOT WB-0f8d-1", tool_name: null, timestamp: 1793589374.4 },
    { id: 25, role: "tool", content: `{"output": "WB-0f8d-1"}`, tool_name: "terminal", timestamp: 1793589375.0 },
    { id: 29, role: "user", content: wake("WB-0f8d-2"), tool_name: null, timestamp: 1793589378.2 },
    { id: 31, role: "user", content: wake("WB-0f8d-2"), tool_name: null, timestamp: 1793589390.0 },
    // The same row in a continued session (same time and content): once.
    { id: 40, role: "user", content: wake("WB-0f8d-1"), tool_name: null, timestamp: 1793589357.6 },
  ];
  const c = codesOf(hermesReceived(rows), ["WB-0f8d-1", "WB-0f8d-2", "WB-0f8d-3"]);
  assert.deepEqual(
    Object.fromEntries(Object.entries(c.counts).map(([k, v]) => [k, v.n])),
    { "WB-0f8d-1": 1, "WB-0f8d-2": 2, "WB-0f8d-3": 0 },
  );
  assert.deepEqual(c.duplicates, ["WB-0f8d-2"]);
  assert.deepEqual(c.missing, ["WB-0f8d-3"]);
});

const ran = (id: ScenarioResult["id"], metrics: ScenarioResult["metrics"]): ScenarioResult => ({ id, harness: "claude-code", status: "ran", works: true, metrics, steps: [], notes: [] });

/** Applies an outcome as Scenarios.countArrivals does, then grades with no bars. */
function graded(r: ScenarioResult, items: Received[] | null, codes: string[], seen: string[]) {
  const o = arrivalOutcome(items, codes, new Set(seen), "Claude Code");
  r.metrics = { ...r.metrics, ...o.metrics };
  r.notes.push(...o.notes);
  if (o.failure) {
    r.works = false;
    r.reason = o.failure;
  }
  return grade(r, undefined);
}

test("grading: single passes; an unmarked duplicate fails as a duplicate delivery; a marked one passes, shown", () => {
  const once = graded(ran("S2", { p50_ms: 4000 }), parseReceived([cc.user(monitor("WB-cc33-1"), 1)]), ["WB-cc33-1"], ["WB-cc33-1"]);
  assert.equal(once.grade, "pass");
  assert.equal(once.metrics.duplicate_deliveries, 0);
  assert.equal(once.metrics.arrivals, j({ "WB-cc33-1": 1 }));

  const dup = graded(ran("S15", { app1_published: 1, app2_published: 0, app1_seen: 1, app2_seen: 0, max_ms: 9000 }), parseReceived([cc.user(stopHook("WB-cc33-2"), 1), cc.queued(monitor("WB-cc33-2"), 2)]), ["WB-cc33-2"], ["WB-cc33-2"]);
  assert.equal(dup.grade, "fail");
  assert.match(dup.key!, /^duplicate delivery, 1\/1 \+ 0\/0/);
  assert.match(dup.why!.join(" "), /duplicate delivery: WB-cc33-2 ×2/);

  const marked = graded(ran("S3", { seen: 5, published: 5 }), parseReceived([cc.user(monitor("WB-cc33-3"), 1), cc.queued(monitor("WB-cc33-3", ',"redelivered":true'), 2)]), ["WB-cc33-3"], ["WB-cc33-3"]);
  assert.equal(marked.grade, "pass");
  assert.match(marked.key!, /1 redelivered/);
  assert.ok(marked.notes.some((x) => /redelivered.*WB-cc33-3/.test(x)));
});

test("grading: a code that never arrived is shown; one the agent reported but the count missed isn't called lost", () => {
  const r = graded(ran("S3", { seen: 1, published: 2 }), parseReceived([cc.user(monitor("WB-dd44-1"), 1)]), ["WB-dd44-1", "WB-dd44-2", "WB-dd44-3"], ["WB-dd44-1", "WB-dd44-3"]);
  assert.equal(r.metrics.never_arrived, 1);
  assert.ok(r.notes.some((x) => /never reached the model: WB-dd44-2$/.test(x)));
  assert.ok(r.notes.some((x) => /not found in .*: WB-dd44-3$/.test(x)));
});

test("the outcome names the codes that reached the model exactly once (none where the record can't be read)", () => {
  const items = parseReceived([cc.user(monitor("WB-ff66-1"), 1), cc.user(stopHook("WB-ff66-2"), 2), cc.queued(monitor("WB-ff66-2"), 3)]);
  const o = arrivalOutcome(items, ["WB-ff66-1", "WB-ff66-2", "WB-ff66-3"], new Set(["WB-ff66-1", "WB-ff66-2"]), "Claude Code");
  assert.deepEqual([...o.once], ["WB-ff66-1"]);
  assert.deepEqual([...arrivalOutcome(null, ["WB-ff66-1"], new Set(["WB-ff66-1"]), "Claude Code").once], []);
});

test("grading: an unreadable record falls back to today's grading, and says so", () => {
  const r = ran("S7", { wake_ms: 5000 });
  r.metrics.arrivals_unreadable = true;
  const g = graded(r, null, ["WB-ee55-1"], ["WB-ee55-1"]);
  assert.equal(g.grade, "pass");
  assert.equal(g.metrics.arrivals_counted, false);
  assert.match(g.key!, /arrivals not counted/);
  assert.ok(g.notes.some((x) => /arrivals not counted/.test(x)));
});

// Codex's turn state from its rollout (drivers/codex turnOpen): Codex shows no
// "Working" line while an answer streams, so the bench reads it there.
test("a Codex rollout's turn is open from task_started until task_complete or turn_aborted", async () => {
  const { rolloutTurnOpen } = await import("../drivers/codex/index.js");
  const ev = (t: string) => `{"timestamp":"2026-11-08T19:27:40.090Z","type":"event_msg","payload":{"type":"${t}","turn_id":"t1"}}`;
  const item = `{"timestamp":"2026-11-08T19:27:48.064Z","type":"response_item","payload":{"type":"reasoning"}}`;
  assert.equal(rolloutTurnOpen(""), false);
  assert.equal(rolloutTurnOpen([ev("task_started"), item].join("\n")), true);
  assert.equal(rolloutTurnOpen([ev("task_started"), item, ev("task_complete")].join("\n")), false);
  assert.equal(rolloutTurnOpen([ev("task_started"), ev("turn_aborted")].join("\n")), false);
  assert.equal(rolloutTurnOpen([ev("task_started"), ev("task_complete"), ev("task_started"), item].join("\n")), true);
  // Text with neither keeps what was known.
  assert.equal(rolloutTurnOpen(item, true), true);
  assert.equal(rolloutTurnOpen(item, false), false);
});

test("RolloutTurns reads a growing rollout a part at a time, including a line cut mid-write", async () => {
  const { RolloutTurns } = await import("../drivers/codex/index.js");
  const { appendFileSync, mkdirSync, mkdtempSync, rmSync } = await import("node:fs");
  const { tmpdir } = await import("node:os");
  const { join } = await import("node:path");
  const sessions = mkdtempSync(join(tmpdir(), "wb-rollout-"));
  const day = join(sessions, "2026", "11", "08");
  mkdirSync(day, { recursive: true });
  const f = join(day, "rollout-2026-11-08T11-27-38-x.jsonl");
  const ev = (t: string) => `{"type":"event_msg","payload":{"type":"${t}"}}\n`;
  const t = new RolloutTurns(sessions);
  assert.equal(t.turnOpen(), false);
  appendFileSync(f, ev("task_started"));
  // The file appeared after the first look: found at the next rescan, so start fresh.
  const t2 = new RolloutTurns(sessions);
  assert.equal(t2.turnOpen(), true);
  appendFileSync(f, `{"type":"response_item","payload":{"type":"reasoning"}}\n`);
  assert.equal(t2.turnOpen(), true);
  const done = ev("task_complete");
  appendFileSync(f, done.slice(0, 20));
  assert.equal(t2.turnOpen(), true);
  appendFileSync(f, done.slice(20));
  assert.equal(t2.turnOpen(), false);
  appendFileSync(f, ev("task_started"));
  assert.equal(t2.turnOpen(), true);
  appendFileSync(f, ev("turn_aborted"));
  assert.equal(t2.turnOpen(), false);
  rmSync(sessions, { recursive: true, force: true });
});
