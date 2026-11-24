import { strict as assert } from "node:assert";
import { test } from "node:test";
import { withTransport } from "../app/server.js";
import { parseTurns } from "../drivers/claude-code/index.js";
import { matchTurns } from "../src/scenarios.js";

const line = (o: Record<string, unknown>) => JSON.stringify(o);

// A Monitor wake as Claude Code's transcript records it.
const transcript = [
  line({ type: "queue-operation", operation: "enqueue", timestamp: "2026-10-21T02:23:26.536Z", content: '<task-notification>\n<summary>Monitor event: "x"</summary>\n<event>{"subject":"Bench event WB-ab12-1."}</event>' }),
  line({ type: "queue-operation", operation: "dequeue", timestamp: "2026-10-21T02:23:26.538Z" }),
  line({ type: "user", timestamp: "2026-10-21T02:23:26.543Z", message: { role: "user", content: "<task-notification>…" } }),
  line({ type: "assistant", timestamp: "2026-10-21T02:23:28.934Z", message: { id: "m1", model: "claude-opus-5-5", content: [{ type: "tool_use", name: "mcp__wakebench__report_seen" }], usage: { input_tokens: 4, cache_read_input_tokens: 29723, cache_creation_input_tokens: 395, output_tokens: 139 } } }),
  line({ type: "user", timestamp: "2026-10-21T02:23:29.290Z", message: { role: "user", content: [{ type: "tool_result" }] } }),
  line({ type: "assistant", timestamp: "2026-10-21T02:23:30.741Z", message: { id: "m2", content: [{ type: "text", text: "GOT WB-ab12-1" }], usage: { input_tokens: 2, cache_read_input_tokens: 30118, cache_creation_input_tokens: 180, output_tokens: 12 } } }),
  line({ type: "system", subtype: "turn_duration", timestamp: "2026-10-21T02:23:30.747Z" }),
  "",
  line({ type: "queue-operation", operation: "enqueue", timestamp: "2026-10-21T02:24:00.000Z", content: '<channel source="wakebench-channel">{"type":"event"}' }),
  "{ half a line",
];

test("parseTurns: a wake turn's stages and tokens from the transcript", () => {
  const [t, c] = parseTurns(transcript);
  assert.equal(t.source, "monitor");
  assert.equal(t.queued, Date.parse("2026-10-21T02:23:26.536Z"));
  assert.equal(t.started, Date.parse("2026-10-21T02:23:26.538Z"));
  assert.equal(t.first, Date.parse("2026-10-21T02:23:28.934Z"));
  assert.equal(t.firstKind, "tool_use:mcp__wakebench__report_seen");
  assert.equal(t.ended, Date.parse("2026-10-21T02:23:30.747Z"));
  assert.equal(t.outputTokens, 151);
  assert.equal(t.inputTokens, 4 + 29723 + 395 + 2 + 30118 + 180);
  assert.equal(t.model, "claude-opus-5-5");
  // A channel message still waiting for its turn.
  assert.equal(c.source, "channel");
  assert.equal(c.started, null);
});

test("parseTurns: events queued while busy each get their own dequeue and turn", () => {
  const busy = [
    line({ type: "queue-operation", operation: "enqueue", timestamp: "2026-10-21T03:00:00.000Z", content: "<task-notification>Monitor event: a</task-notification>" }),
    line({ type: "queue-operation", operation: "enqueue", timestamp: "2026-10-21T03:00:00.100Z", content: "<task-notification>Monitor event: b</task-notification>" }),
    line({ type: "queue-operation", operation: "dequeue", timestamp: "2026-10-21T03:00:01.000Z" }),
    line({ type: "assistant", timestamp: "2026-10-21T03:00:02.000Z", message: { id: "a1", content: [{ type: "text" }], usage: { output_tokens: 10 } } }),
    line({ type: "system", subtype: "turn_duration", timestamp: "2026-10-21T03:00:02.500Z" }),
    line({ type: "queue-operation", operation: "dequeue", timestamp: "2026-10-21T03:00:03.000Z" }),
    line({ type: "assistant", timestamp: "2026-10-21T03:00:04.000Z", message: { id: "b1", content: [{ type: "text" }], usage: { output_tokens: 20 } } }),
    line({ type: "system", subtype: "turn_duration", timestamp: "2026-10-21T03:00:04.500Z" }),
  ];
  const [a, b] = parseTurns(busy);
  assert.equal(a.started, Date.parse("2026-10-21T03:00:01.000Z"));
  assert.equal(a.outputTokens, 10);
  assert.equal(b.started, Date.parse("2026-10-21T03:00:03.000Z"));
  assert.equal(b.first, Date.parse("2026-10-21T03:00:04.000Z"));
  assert.equal(b.outputTokens, 20);
  // A dequeue that records its content goes to the matching item.
  const named = [
    busy[0],
    busy[1],
    line({ type: "queue-operation", operation: "dequeue", timestamp: "2026-10-21T03:00:01.000Z", content: "<task-notification>Monitor event: b</task-notification>" }),
    line({ type: "queue-operation", operation: "dequeue", timestamp: "2026-10-21T03:00:05.000Z", content: "<task-notification>Monitor event: a</task-notification>" }),
  ];
  const [na, nb] = parseTurns(named);
  assert.equal(nb.started, Date.parse("2026-10-21T03:00:01.000Z"));
  assert.equal(na.started, Date.parse("2026-10-21T03:00:05.000Z"));
});

test("matchTurns: by code, else the first unclaimed turn after the publish", () => {
  const turns = parseTurns(transcript);
  const m = matchTurns(turns, [
    { code: "WB-ab12-1", at: "2026-10-21T02:23:26.300Z" },
    { code: "WB-ab12-2", at: "2026-10-21T02:23:59.800Z" },
  ]);
  assert.equal(m.get("WB-ab12-1")?.source, "monitor");
  assert.equal(m.get("WB-ab12-2")?.source, "channel");
});

test("withTransport: only Claude Code's Monitor command changes, on the same link", () => {
  const text = "Watching. If you haven't written this conversation's link to the file ~/.wakeshift/links/0123456789abcdef yet, first write it with your file-writing tool (by its full path in your home directory, not in a folder named ~), its whole content this one line: https://core.example/v1/stream/wait?link=L1 . Never write it with a shell command or put the link in one: it's this conversation's private link, and a command would show it in approvals and logs. To be woken, start the Monitor tool now (persistent) with this command: f=$HOME/.wakeshift/links/0123456789abcdef; u=\"$b&until=hooks&timeout=50s\"; c=; p=$PPID; while :; do :; done; fi . Each line is JSON.";
  const r = { content: [{ type: "text" as const, text }] };
  assert.equal(withTransport(r, "claude-code", ""), r);
  assert.equal(withTransport(r, "codex", "ws"), r);
  const ws = (withTransport(r, "claude-code", "ws").content[0] as { text: string }).text;
  assert.match(ws, /ws source: \{"url": "wss:\/\/core\.example\/v1\/stream\?link=L1"\}/);
  assert.doesNotMatch(ws, /stream\/wait/);
  const sse = (withTransport(r, "claude-code", "sse").content[0] as { text: string }).text;
  assert.match(sse, /u='https:\/\/core\.example\/v1\/stream\?link=L1&format=sse'/);
  assert.match(sse, /Last-Event-ID/);
  const lines = (withTransport(r, "claude-code", "lines").content[0] as { text: string }).text;
  assert.match(lines, /format=lines/);
  assert.match(lines, /Each line is JSON\.$/);
});
