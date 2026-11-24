// WB_CLAUDE_TRANSPORT=task-spike (app/tasks.ts): the 2026-07-28 front, the wait
// task and the watch result's rewrite.

import { strict as assert } from "node:assert";
import { test } from "node:test";
import type { CallToolResult } from "@modelcontextprotocol/server";
import { withTask } from "../app/server.js";
import { isModern, parseRound, resultText, TaskFront, TASK_SAY, waitUrlIn, wantsTasks } from "../app/tasks.js";

const WAIT = "https://core.test/v1/stream/wait?link=wsl_abc&until=hooks&timeout=50s";
const STEP = `If you haven't written this conversation's link to the file ~/.wakeshift/links/0123456789abcdef yet, first write it with your file-writing tool (by its full path in your home directory, not in a folder named ~), its whole content this one line: ${WAIT.split("&")[0]} . Never write it with a shell command or put the link in one: it's this conversation's private link, and a command would show it in approvals and logs.`;
const CMD = `f=$HOME/.wakeshift/links/0123456789abcdef; u="$b&until=hooks&timeout=50s"; c=; p=$PPID; while :; do r=$(curl -s "$u&after=$c"); done; fi`;
const MONITOR = `${STEP} To be woken when Test App sends something, start the Monitor tool now (timeout_ms 1800000) with this command: ${CMD} .`;
const RULES = `The command keeps going by itself. Every 25 minutes it ends with a {"type":"hub.restart"} line, since Claude Code stops a Monitor after 30 minutes: then start the Monitor again the same way, without asking the user (it's routine, and nothing is missed). When a {"type":"hub.closed"} line arrives, it has ended for good: do what its note says. If the Monitor expires or stops any other way, start it again the same way, also without asking; tell the user only if it fails to start.`;
const NO_MONITOR = `If you have no Monitor tool, run this instead with your Bash tool in the background (run_in_background: true), after the link file is written: ${CMD} . It ends when messages arrive, printing them, and you're woken when it ends: act on them, then run the same command again the same way. If its output has a hub.closed line, don't run it again: do what its note says.`;

const meta = (tasks: boolean) => ({
  "io.modelcontextprotocol/protocolVersion": "2026-07-28",
  "io.modelcontextprotocol/clientInfo": { name: "claude-code", version: "2.1.314" },
  "io.modelcontextprotocol/clientCapabilities": tasks ? { extensions: { "io.modelcontextprotocol/tasks": {} } } : {},
});

test("withTask: the Monitor instructions become the wait tool's, and the wait URL is kept", () => {
  let got = "";
  const r: CallToolResult = { content: [{ type: "text", text: "Watching." }, { type: "text", text: [MONITOR, RULES, NO_MONITOR, "Pass session_key=\"wsc_x\"."].join(" ") }] };
  const out = withTask(r, (u) => (got = u));
  const text = (out.content[1] as { text: string }).text;
  assert.equal(got, WAIT);
  assert.ok(text.includes(TASK_SAY), text);
  assert.doesNotMatch(text, /Monitor tool|stream\/wait|run_in_background|hub\.restart/);
  assert.match(text, /Pass session_key="wsc_x"\./);
  assert.equal((out.content[0] as { text: string }).text, "Watching.");
});

test("isModern and wantsTasks read the request's _meta", () => {
  assert.equal(isModern({ jsonrpc: "2.0", id: 1, method: "server/discover", params: {} }), true);
  assert.equal(isModern({ jsonrpc: "2.0", id: 1, method: "tools/list", params: { _meta: meta(false) } }), true);
  assert.equal(isModern({ jsonrpc: "2.0", id: 1, method: "initialize", params: { protocolVersion: "2025-11-25" } }), false);
  assert.equal(isModern([{ method: "tools/list" }]), false);
  assert.equal(wantsTasks({ params: { _meta: meta(true) } }), true);
  assert.equal(wantsTasks({ params: { _meta: meta(false) } }), false);
  assert.equal(waitUrlIn("no command here"), undefined);
});

test("parseRound reads events, the cursor, hub.next and hub.closed", () => {
  const r = ['{"type":"event","subject":"a"}', '{"type":"cursor","after":"0a1b"}', '{"type":"hub.next","url":"https://core.test/v1/stream/wait?link=n"}', "junk"].join("\n");
  assert.deepEqual(parseRound(r), { lines: ['{"type":"event","subject":"a"}'], cursor: "0a1b", closed: false, next: "https://core.test/v1/stream/wait?link=n" });
  const closed = parseRound('{"type":"hub.closed","reason":"hooks","note":"n"}');
  assert.equal(closed.closed, true);
  assert.match(resultText(closed.lines, true), /ended for good/);
  assert.match(resultText(['{"type":"event"}'], false), /^Wakeshift: 1 new message .*\n\{"type":"event"\}\nAct on them, then call wait again now/s);
});

test("a wait task waits on the session link, confirms what it took and completes; a held tasks/get answers at once", async () => {
  const fetched: string[] = [];
  const rounds = [
    '{"type":"hub.next","url":"https://core.test/v1/stream/wait?link=wsl_abc&until=hooks&timeout=50s"}',
    // A cursor with nothing to show doesn't complete it: the next round confirms it.
    ['{"type":"cursor","after":"00aa"}', '{"type":"hub.next","url":"https://core.test/v1/stream/wait?link=wsl_abc&until=hooks&timeout=50s"}'].join("\n"),
    ['{"type":"event","subject":"WB-ab12-1"}', '{"type":"cursor","after":"00ff"}', '{"type":"hub.next","url":"x"}'].join("\n"),
  ];
  const fakeFetch = (async (u: string | URL) => {
    fetched.push(String(u));
    const body = String(u).includes("confirm=1") ? "" : (rounds.shift() ?? "");
    return new Response(body);
  }) as typeof fetch;
  const front = new TaskFront({ waitUrl: () => WAIT, backend: async () => ({ status: 500, headers: new Headers(), text: "" }), fetch: fakeFetch });
  const { res, done } = fakeRes();
  await front.handle("lane-a", fakeReq(), res, { jsonrpc: "2.0", id: 7, method: "tools/call", params: { name: "wait", arguments: {}, _meta: meta(true) } });
  const created = (await done).result;
  assert.equal(created.resultType, "task");
  assert.equal(created.status, "working");
  // Let the drive finish, then tasks/get answers completed with the event.
  const t = front.list("lane-a")[0];
  for (let i = 0; i < 50 && t.status === "working"; i++) await new Promise((r) => setTimeout(r, 10));
  assert.equal(t.status, "completed");
  assert.deepEqual(fetched, [`${WAIT}&after=`, `${WAIT}&after=`, `${WAIT}&after=00aa`, `${WAIT}&after=00ff&confirm=1`]);
  const g = fakeRes();
  await front.handle("lane-a", fakeReq(), g.res, { jsonrpc: "2.0", id: 8, method: "tasks/get", params: { taskId: created.taskId, _meta: meta(true) } });
  const got = (await g.done).result;
  assert.equal(got.status, "completed");
  assert.match(got.result.content[0].text, /WB-ab12-1/);
});

test("wait without the tasks extension is refused; a second wait replaces the first; cancel ends it", async () => {
  const never = (async (_u: string | URL, init?: RequestInit) =>
    new Promise<Response>((_, reject) => init?.signal?.addEventListener("abort", () => reject(new Error("aborted"))))) as typeof fetch;
  const front = new TaskFront({ waitUrl: () => WAIT, backend: async () => ({ status: 500, headers: new Headers(), text: "" }), fetch: never });
  const call = async (id: number, method: string, params: Record<string, unknown>) => {
    const { res, done } = fakeRes();
    await front.handle("lane-a", fakeReq(), res, { jsonrpc: "2.0", id, method, params });
    return (await done).result;
  };
  const plain = await call(1, "tools/call", { name: "wait", _meta: meta(false) });
  assert.equal(plain.isError, true);
  const a = await call(2, "tools/call", { name: "wait", _meta: meta(true) });
  const b = await call(3, "tools/call", { name: "wait", _meta: meta(true) });
  const [ta, tb] = front.list("lane-a");
  assert.equal(ta.id, a.taskId);
  assert.equal(ta.status, "cancelled");
  assert.match(ta.statusMessage ?? "", /replaced by a newer wait/);
  // The replaced task's wait is ended, so only one holds the stream.
  assert.equal(ta.stop.signal.aborted, true);
  assert.equal(tb.status, "working");
  assert.equal(tb.stop.signal.aborted, false);
  await call(4, "tasks/cancel", { taskId: b.taskId });
  assert.equal(tb.status, "cancelled");
  assert.equal(tb.stop.signal.aborted, true);
  await new Promise((r) => setTimeout(r, 10));
  assert.equal(tb.driving, false);
  // Another lane can't read it.
  const { res, done } = fakeRes();
  await front.handle("lane-b", fakeReq(), res, { jsonrpc: "2.0", id: 5, method: "tasks/get", params: { taskId: b.taskId } });
  assert.equal((await done).error.code, -32602);
});

test("a wait replaced while it confirms hands what it confirmed to the newer wait", async () => {
  let release!: () => void;
  const confirmed = new Promise<void>((r) => (release = r));
  let confirming = false;
  let rounds = 0;
  const fakeFetch = (async (u: string | URL, init?: RequestInit) => {
    if (String(u).includes("confirm=1")) {
      confirming = true;
      await confirmed;
      return new Response("");
    }
    // The first wait's round has a message; later ones (the newer wait's) wait until aborted.
    if (rounds++ === 0) return new Response(['{"type":"event","subject":"WB-ab12-9"}', '{"type":"cursor","after":"0b"}'].join("\n"));
    return new Promise<Response>((_, reject) => init?.signal?.addEventListener("abort", () => reject(new Error("aborted"))));
  }) as typeof fetch;
  const front = new TaskFront({ waitUrl: () => WAIT, backend: async () => ({ status: 500, headers: new Headers(), text: "" }), fetch: fakeFetch });
  const call = async (id: number) => {
    const { res, done } = fakeRes();
    await front.handle("lane-a", fakeReq(), res, { jsonrpc: "2.0", id, method: "tools/call", params: { name: "wait", _meta: meta(true) } });
    return (await done).result;
  };
  await call(1);
  for (let i = 0; i < 50 && !confirming; i++) await new Promise((r) => setTimeout(r, 5));
  await call(2);
  release();
  await new Promise((r) => setTimeout(r, 10));
  const [a, b] = front.list("lane-a");
  assert.equal(a.status, "cancelled");
  assert.equal(b.status, "completed");
  assert.match(b.text ?? "", /WB-ab12-9/);
  b.stop.abort();
});

function fakeReq() {
  return { headers: { "user-agent": "claude-code/2.1.314 (cli)" } } as never;
}

function fakeRes(): { res: never; done: Promise<any> } {
  let resolve!: (v: any) => void;
  const done = new Promise<any>((r) => (resolve = r));
  const res = {
    writeHead() {},
    end(s: string) {
      resolve(JSON.parse(s));
    },
    on() {},
    off() {},
  };
  return { res: res as never, done };
}
