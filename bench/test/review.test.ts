// Tests for the review fixes: paths the bench may create or read, option
// and input validation, the toy app's session binding, the shared scenario
// model, container and file hygiene.

import { strict as assert } from "node:assert";
import { createHash } from "node:crypto";
import { existsSync, mkdirSync, mkdtempSync, readFileSync, realpathSync, rmSync, statSync, symlinkSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { test } from "node:test";
import { Client, StreamableHTTPClientTransport } from "@modelcontextprotocol/client";
import { Wakeshift } from "@wakeshift/sdk";
import { benchApp } from "../app/server.js";
import { numberArg, parseArgs, reportDir, rewrite, scrub } from "../src/cli.js";
import { dbName, goModule } from "../src/core.js";
import { addReportSections, ext } from "../src/extensions.js";
import { makeHome, makeRunDir, RUN_MARKER } from "../src/home.js";
import { argsCheck, KillWatch, rearm, restart } from "../src/metrics.js";
import { Procs } from "../src/procs.js";
import { renderReport } from "../src/report.js";
import { installConnected } from "../src/scenarios.js";

const scratch = () => realpathSync(mkdtempSync(join(tmpdir(), "wb-review-")));

// ── makeHome: only fresh dirs, only inside a run dir the bench made ─────

test("makeHome creates a fresh HOME inside a run directory the bench made", () => {
  const root = scratch();
  const run = makeRunDir(root, "20261020-011712-34a8");
  const h = makeHome(run, "codex");
  assert.ok(existsSync(join(h.dir, "work")) && existsSync(join(h.dir, "Library/Keychains")));
  assert.equal(statSync(h.dir).mode & 0o777, 0o700);
  rmSync(root, { recursive: true });
});

test("makeHome never deletes: an existing HOME, an unmarked dir and a bad name are refused", () => {
  const root = scratch();
  const run = makeRunDir(root, "20261020-011712-34a8");
  const h = makeHome(run, "codex");
  writeFileSync(join(h.dir, "keep"), "x");
  assert.throws(() => makeHome(run, "codex"), /exists already/);
  assert.equal(readFileSync(join(h.dir, "keep"), "utf8"), "x");
  // A directory that isn't a run dir (no marker), e.g. a real home.
  const other = join(root, "someone");
  mkdirSync(other);
  writeFileSync(join(other, "precious"), "x");
  assert.throws(() => makeHome(other, "codex"), /isn't a run directory/);
  assert.ok(existsSync(join(other, "precious")));
  // A marker that names another directory doesn't count.
  writeFileSync(join(other, RUN_MARKER), "20261020-011712-34a8\n");
  assert.throws(() => makeHome(other, "codex"), /isn't a run directory/);
  for (const bad of ["../x", "a/b", "", "UPPER", ".hidden"]) assert.throws(() => makeHome(run, bad), /not a lane name/);
  rmSync(root, { recursive: true });
});

test("a run directory is always new", () => {
  const root = scratch();
  makeRunDir(root, "20261020-011712-34a8");
  assert.throws(() => makeRunDir(root, "20261020-011712-34a8"), /EEXIST/);
  assert.throws(() => makeRunDir(root, "../escape"), /not a run ID/);
  rmSync(root, { recursive: true });
});

// ── --only-report, --parallel, --slow ─────────────────────────────────────

test("--only-report takes only a run ID, resolved inside the output root", () => {
  for (const bad of ["../x", "/etc", "20261020-011712-34a8/../..", "x"]) assert.throws(() => parseArgs(["--only-report", bad]), /run ID/);
  assert.equal(parseArgs(["--only-report", "20261020-011712-34a8"]).onlyReport, "20261020-011712-34a8");
  const root = scratch();
  const run = makeRunDir(root, "20261020-011712-34a8");
  assert.equal(reportDir(root, "20261020-011712-34a8"), run);
  assert.throws(() => reportDir(root, "20261020-011712-ffff"), /ENOENT/);
  // A symlink inside the root that points elsewhere is refused.
  const elsewhere = scratch();
  writeFileSync(join(elsewhere, "results.json"), "{}");
  symlinkSync(elsewhere, join(root, "20261020-011712-eeee"));
  assert.throws(() => reportDir(root, "20261020-011712-eeee"), /isn't inside/);
  rmSync(root, { recursive: true });
  rmSync(elsewhere, { recursive: true });
});

test("--parallel and --slow reject typos, NaN, Infinity and fractions with a clear error", () => {
  for (const bad of ["abc", "", "NaN", "Infinity", "1.5", "0", "-2", "17"]) assert.throws(() => parseArgs(["--harness", "fake", "--parallel", bad]), /--parallel must be a whole number from 1 to 16/, bad);
  for (const bad of ["abc", "", "Infinity", "0.1", "11"]) assert.throws(() => parseArgs(["--harness", "fake", "--slow", bad]), /--slow must be a number from 0.2 to 10/, bad);
  assert.equal(parseArgs(["--harness", "fake", "--parallel", "3"]).parallel, 3);
  assert.equal(parseArgs(["--harness", "fake", "--slow", "1.5"]).slow, 1.5);
  assert.equal(numberArg("--x", " 2 ", { min: 1, max: 3, integer: true }), 2);
});

// ── The runner without extensions (src/extensions.ts) ────────────────────

test("without extensions the targets are local and fake, and scenarios played together aren't there", () => {
  assert.equal(parseArgs(["--harness", "codex"]).target, "local");
  assert.equal(parseArgs(["--harness", "fake", "--target", "fake"]).target, "fake");
  assert.throws(() => parseArgs(["--harness", "codex", "--target", "elsewhere"]), /--target is local or fake/);
  assert.throws(() => parseArgs(["--harness", "codex", "--key-file", "/k"]), /unknown argument --key-file/);
  assert.throws(() => parseArgs(["--harness", "codex", "--scenarios", "S1,S12"]), /S12 isn't in this bench/);
  assert.throws(() => parseArgs(["--harness", "codex", "--scenarios", "S11-S14"]), /S11, S12, S14 aren't in this bench/);
  assert.deepEqual(parseArgs(["--harness", "codex"]).options, {});
});

test("a report section from an extension renders in the page; one whose script could end its element is refused", () => {
  const run = { run: "r", target: "local", core: "", startedAt: "", scenarios: [], harnesses: [], safety: { ok: true, changed: [], checked: [] }, notes: [] };
  const tiers = { tiers: [], names: {} } as never;
  const dir = scratch();
  ext.report.push({ id: "extra", data: () => ({ n: 1 }), css: "#extra{color:red}", js: "HOOKS.cell.push(() => \"\");" });
  try {
    const html = renderReport(run as never, tiers, dir);
    assert.match(html, /<section id="extra"><\/section>/);
    assert.match(html, /#extra\{color:red\}/);
    assert.equal(JSON.parse(/<script id="data" type="application\/json">([\s\S]*?)<\/script>/.exec(html)![1]).sections.extra.n, 1);
  } finally {
    ext.report.pop();
  }
  for (const bad of [{ id: "x", js: "</script><script>alert(1)" }, { id: "x", js: "", css: "</style><script>" }, { id: "x\"><b", js: "" }]) {
    ext.report.push({ data: () => null, ...bad });
    try {
      assert.throws(() => renderReport(run as never, tiers, dir), /report section/);
    } finally {
      ext.report.pop();
    }
  }
  // Two sections with one id: refused when registered, and at render if one was pushed directly.
  const one = { id: "dup", data: () => null, js: "" };
  addReportSections(one);
  try {
    assert.throws(() => addReportSections({ ...one }), /report section "dup" is already registered/);
    assert.equal(ext.report.filter((s) => s.id === "dup").length, 1);
    ext.report.push({ ...one });
    assert.throws(() => renderReport(run as never, tiers, dir), /report section "dup" is registered twice/);
  } finally {
    ext.report.length = 0;
  }
  // Without extensions, the page has no sections of theirs.
  assert.deepEqual(JSON.parse(/<script id="data" type="application\/json">([\s\S]*?)<\/script>/.exec(renderReport(run as never, tiers, dir))![1]).sections, {});
  rmSync(dir, { recursive: true });
});

test("the connector's version flag names the checkout's own Go module", () => {
  const dir = scratch();
  writeFileSync(join(dir, "go.mod"), "module example.com/server\n\ngo 1.27.0\n");
  assert.equal(goModule(dir), "example.com/server");
  writeFileSync(join(dir, "go.mod"), "go 1.27.0\n");
  assert.throws(() => goModule(dir), /no module line/);
  rmSync(dir, { recursive: true });
});

// ── The database name ────────────────────────────────────────────────────

test("database names fit Postgres's 63-byte limit and stay distinct", () => {
  assert.equal(dbName("20261020-011712-34a8"), "wakebench_20261020_011712_34a8");
  const a = dbName("x".repeat(80) + "a");
  const b = dbName("x".repeat(80) + "b");
  assert.ok(a.length <= 63 && b.length <= 63);
  assert.notEqual(a, b);
  assert.match(a, /^wakebench_[a-z0-9_]+$/);
});

// ── The toy app ──────────────────────────────────────────────────────────

// A Wakeshift client on a fake core that answers what the toy app asks
// (the manifest, emit, subscribe, the call's block, status) and records it.
const BLOCK =
  "Wakeshift: If you haven't written this conversation's link to the file ~/.wakeshift/links/0123456789abcdef yet, first write it with your file-writing tool (by its full path in your home directory, not in a folder named ~), its whole content this one line: http://core.test/v1/stream/wait?link=wsl_abc . Never write it with a shell command or put the link in one: it's this conversation's private link, and a command would show it in approvals and logs. To be woken, start the Monitor tool now (persistent) with this command: f=$HOME/.wakeshift/links/0123456789abcdef; u=\"$b&until=hooks&timeout=50s\"; c=; p=$PPID; while :; do :; done; fi .";
const TRAILER = "(For Wakeshift's hook on this computer, not for you: wakeshift-beacon: aaaaaaaaaa.bbbbbbbbbb.cccccccccc)";

interface FakeCore {
  calls: { path: string; body: any }[];
}

function fakeWake(): { wake: Wakeshift; core: FakeCore } {
  const core: FakeCore = { calls: [] };
  const fetch = (async (url: string | URL | Request, init?: RequestInit) => {
    const path = String(url).replace("https://core.test/api/apps/v1", "");
    const body = init?.body ? JSON.parse(String(init.body)) : undefined;
    core.calls.push({ path, body });
    const reply = (json: unknown) => new Response(JSON.stringify(json), { status: 200, headers: { "Content-Type": "application/json" } });
    switch (path) {
      case "/manifest":
      case "/manifest/merge":
        return reply(body ?? { description: "" }); // GET: nothing published yet
      case "/events/emit":
        return reply({ matched: 1, duplicate: false });
      case "/event-subscriptions":
        return reply({
          event_subscription: { id: "11111111-1111-4111-8111-111111111111", user: body.call.user, created: true },
          session: "wsc_secret",
          agent: { harness: "claude-code", via: "client_name" },
          pending: "pa_1",
        });
      case "/calls/addition":
        return reply(body.pending ? { text: BLOCK, trailer: TRAILER } : { text: "Wakeshift: connected." });
      case "/events/status":
        return body.subscriptions
          ? reply({ subscriptions: body.subscriptions.map((s: { id: string; user: string }) => ({ id: s.id, user: s.user, live: true, connected: true, via: "stream" })) })
          : reply({ subscriptions: 1, connected: 1, via: { stream: 1 } });
    }
    return new Response(JSON.stringify({ error: { code: "not_found", message: path } }), { status: 404 });
  }) as typeof globalThis.fetch;
  const wake = new Wakeshift({ key: `wst_test_wakebench_${Buffer.alloc(32, 9).toString("base64url")}`, core: "https://core.test", fetch, logLevel: "off" });
  return { wake, core };
}

async function withApp<T>(f: (base: string, core: FakeCore) => Promise<T>): Promise<T> {
  const { wake, core } = fakeWake();
  const server = benchApp({ wake, run: "r", controlToken: "tok" });
  await new Promise<void>((r) => server.listen(0, "127.0.0.1", () => r()));
  const a = server.address();
  const base = `http://127.0.0.1:${typeof a === "object" && a ? a.port : 0}`;
  try {
    return await f(base, core);
  } finally {
    server.closeAllConnections();
    await new Promise<void>((r) => server.close(() => r()));
  }
}

const H = { "content-type": "application/json", accept: "application/json, text/event-stream" };

test("an MCP session works only on the lane that created it", async () => {
  await withApp(async (base) => {
    const init = await fetch(`${base}/mcp/lane-a`, {
      method: "POST",
      headers: H,
      body: JSON.stringify({ jsonrpc: "2.0", id: 1, method: "initialize", params: { protocolVersion: "2025-06-18", capabilities: {}, clientInfo: { name: "t", version: "0" } } }),
    });
    const sid = init.headers.get("mcp-session-id")!;
    await init.text();
    assert.ok(sid);
    const list = (lane: string) =>
      fetch(`${base}/mcp/${lane}`, { method: "POST", headers: { ...H, "mcp-session-id": sid, "mcp-protocol-version": "2025-06-18" }, body: JSON.stringify({ jsonrpc: "2.0", id: 2, method: "tools/list" }) });
    const other = await list("lane-b");
    assert.equal(other.status, 404);
    await other.text();
    const own = await list("lane-a");
    assert.equal(own.status, 200);
    const tools = await own.text();
    assert.match(tools, /report_seen/);
    assert.doesNotMatch(tools, /subscribe_events|list_subscriptions/, "a lane lists only the tools it uses");
  });
});

// A watch call through the MCP SDK's own client, and what the bench recorded
// of it: by session (initialize first), or on protocol 2026-07-28 (no
// session, a server per request; served only with WB_MCP_2026).
for (const modern of [false, true]) {
  test(`a watch call is recorded with Wakeshift's block, ${modern ? "on protocol 2026-07-28" : "in a session"}`, async () => {
    const was = process.env.WB_MCP_2026;
    if (modern) process.env.WB_MCP_2026 = "1";
    try {
      await withApp(async (base) => {
        const c = new Client({ name: "claude-code", version: "0" }, modern ? { versionNegotiation: { mode: { pin: "2026-07-28" } } } : {});
        await c.connect(new StreamableHTTPClientTransport(new URL(`${base}/mcp/lane-a`)));
        try {
          assert.equal(c.getProtocolEra(), modern ? "modern" : "legacy");
          const r = await c.callTool({ name: "watch", arguments: { session_key: "wsc_given" } });
          assert.ok(!r.isError);
          const lane = (await (await fetch(`${base}/control/lane?lane=lane-a`, { headers: { authorization: "Bearer tok" } })).json()) as { calls: any[] };
          assert.equal(lane.calls.length, 1, "the call is recorded once");
          const [call] = lane.calls;
          assert.equal(call.tool, "watch");
          assert.equal(call.client, "claude-code");
          assert.equal(call.args.session_key, createHash("sha256").update("wsc_given").digest("hex").slice(0, 12));
          assert.equal(call.subscription, "11111111-1111-4111-8111-111111111111");
          assert.match(call.added, /Monitor/, "what Wakeshift added to the result");
          assert.equal(call.beacon, true);
          if (modern) {
            // subscriptions/listen is a stream: its ack arrives while it stays open.
            const sub = await c.listen({ toolsListChanged: true }, { timeout: 5000 });
            await sub.close();
          }
        } finally {
          await c.close();
        }
      });
    } finally {
      if (was === undefined) delete process.env.WB_MCP_2026;
      else process.env.WB_MCP_2026 = was;
    }
  });
}

test("publish takes a whole count from 1 to 20, else 400 and nothing published", async () => {
  await withApp(async (base, core) => {
    const emits = () => core.calls.filter((c) => c.path === "/events/emit");
    const pub = (count: unknown) =>
      fetch(`${base}/control/publish`, { method: "POST", headers: { authorization: "Bearer tok", "content-type": "application/json" }, body: JSON.stringify({ lane: "a", count }) });
    for (const bad of ["x", 1.5, 0, 21, null, "3"]) {
      const r = await pub(bad);
      assert.equal(r.status, 400, String(bad));
      await r.text();
    }
    assert.equal(emits().length, 0);
    // Only a process that serves tools sends the manifest (SDK 0.5.0): a
    // lane's session attaches.
    const init = await fetch(`${base}/mcp/lane-a`, {
      method: "POST",
      headers: H,
      body: JSON.stringify({ jsonrpc: "2.0", id: 1, method: "initialize", params: { protocolVersion: "2025-06-18", capabilities: {}, clientInfo: { name: "t", version: "0" } } }),
    });
    await init.text();
    const ok = await pub(2);
    assert.equal(ok.status, 200);
    assert.equal(((await ok.json()) as { events: unknown[] }).events.length, 2);
    // bench.event { run, lane }, whose topic is bench/<run>/<lane> (what topic(lane) was).
    const e = emits()[0].body;
    assert.deepEqual([e.name, e.arguments, e.id, e.payload.seq], ["bench.event", { run: "r", lane: "a" }, "r:a:1", 1]);
    assert.match(e.text, /^Bench event WB-[a-z0-9]{4}-1\.$/);
    const manifest = core.calls.find((c) => c.path === "/manifest/merge")!.body;
    assert.equal(manifest.events.find((x: { name: string }) => x.name === "bench.event").topic, "bench/{run}/{lane}");
    const unauth = await fetch(`${base}/control/publish`, { method: "POST", body: "{}" });
    assert.equal(unauth.status, 401);
  });
});

// rpc makes one JSON-RPC request in an MCP session, reading the reply from
// JSON or the SSE stream it comes on.
async function rpc(base: string, lane: string, sid: string | null, id: number, method: string, params: unknown): Promise<{ sid: string; body: any }> {
  const r = await fetch(`${base}/mcp/${lane}`, {
    method: "POST",
    headers: { ...H, ...(sid ? { "mcp-session-id": sid, "mcp-protocol-version": "2025-06-18" } : {}) },
    body: JSON.stringify({ jsonrpc: "2.0", id, method, params }),
  });
  const text = await r.text();
  const data = text.trimStart().startsWith("{") ? text : text.split("\n").find((l) => l.startsWith("data: "))!.slice("data: ".length);
  return { sid: sid ?? r.headers.get("mcp-session-id")!, body: JSON.parse(data) };
}

test("watch: the record and the Monitor rewrite work on the final result, Wakeshift's block added", async () => {
  const before = process.env.WB_CLAUDE_TRANSPORT;
  process.env.WB_CLAUDE_TRANSPORT = "lines";
  try {
    await withApp(async (base, core) => {
      const { sid } = await rpc(base, "lane-a", null, 1, "initialize", { protocolVersion: "2025-06-18", capabilities: {}, clientInfo: { name: "claude-code", version: "2" } });
      const call = (id: number, name: string, args: Record<string, unknown>) => rpc(base, "lane-a", sid, id, "tools/call", { name, arguments: args });
      const watch = (await call(2, "watch", { harness: "claude-code", session_key: "wsc_given", conversation_id: "conv-1" })).body.result;
      const text = watch.content.map((c: { text: string }) => c.text).join("\n");
      // The app's text, then Wakeshift's block (rewritten to the lines arm), then the beacon.
      assert.match(watch.content[0].text, /^Watching bench events\./);
      assert.match(text, /format=lines/);
      assert.doesNotMatch(text, /stream\/wait/);
      assert.equal(watch.content.at(-1).text, TRAILER);
      const sub = core.calls.find((c) => c.path === "/event-subscriptions")!.body;
      assert.deepEqual([sub.name, sub.arguments, sub.call.user, sub.call.tool], ["bench.event", { run: "r", lane: "lane-a" }, "lane-a", "watch"]);
      await call(3, "report_seen", { ids: ["WB-ab12-1"], session_key: "wsc_secret" });

      const ctl = { authorization: "Bearer tok" };
      const lane = (await (await fetch(`${base}/control/lane?lane=lane-a`, { headers: ctl })).json()) as { calls: any[]; subscriptions: string[] };
      const [w, seen] = lane.calls;
      assert.equal(w.tool, "watch");
      assert.equal(w.client, "claude-code");
      assert.deepEqual(w.args, { harness: "claude-code", conversation_id: "conv-1", session_key: createHash("sha256").update("wsc_given").digest("hex").slice(0, 12) });
      assert.equal(w.agent.harness, "claude-code");
      assert.equal(w.subscription, "11111111-1111-4111-8111-111111111111");
      assert.equal(w.beacon, true);
      // added is what Wakeshift added (before the bench's rewrite), redacted; not the app's own text.
      assert.match(w.added, /stream\/wait\?link=‹redacted›/);
      assert.match(w.added, /wakeshift-beacon: ‹redacted›/);
      assert.doesNotMatch(w.added, /Watching bench events|wsl_abc/);
      assert.equal(seen.tool, "report_seen");
      assert.deepEqual(seen.args.ids, ["WB-ab12-1"]);
      assert.equal(seen.added, "Wakeshift: connected.");
      assert.deepEqual(lane.subscriptions, [w.subscription]);

      const status = (await (await fetch(`${base}/control/status?lane=lane-a`, { headers: ctl })).json()) as { event: any; subscriptions: any[] };
      assert.equal(status.event.subscriptions, 1);
      assert.deepEqual(status.subscriptions, [{ id: w.subscription, user: "lane-a", live: true, connected: true, via: "stream" }]);
      const read = core.calls.filter((c) => c.path === "/events/status").map((c) => c.body);
      assert.deepEqual(read, [
        { name: "bench.event", arguments: { run: "r", lane: "lane-a" } },
        { name: "bench.event", subscriptions: [{ user: "lane-a", id: w.subscription }] },
      ]);
    });
  } finally {
    if (before === undefined) delete process.env.WB_CLAUDE_TRANSPORT;
    else process.env.WB_CLAUDE_TRANSPORT = before;
  }
});

// ── One scenario model ───────────────────────────────────────────────────

test("S6: connected means the install holds the stream", () => {
  // A harness with its own zero-install command: only the install's transport counts.
  assert.equal(installConnected({ zeroInstallWake: true }, { connected: true, via: "poll" }), false);
  assert.equal(installConnected({ zeroInstallWake: true }, { connected: true, via: "hooks" }), true);
  assert.equal(installConnected({ zeroInstallWake: true, installVia: ["hooks", "stream"] }, { connected: true, via: "stream" }), true);
  // Nothing else could hold it: any transport is the install's (a plugin's SSE stream, a wait).
  for (const via of ["hooks", "stream", "poll"]) assert.equal(installConnected({ zeroInstallWake: false }, { connected: true, via }), true);
  assert.equal(installConnected({ zeroInstallWake: false }, { connected: false, via: null }), false);
});

test("re-arm and restart outcomes", () => {
  assert.deepEqual(rearm(5000, 0).metrics, { wake_ms: 5000, arrived_ms: 5000, nudges: 0, rearm_automatic: true });
  const late = rearm(90000, 1);
  assert.equal(late.works, true);
  assert.equal(late.metrics.wake_ms, null);
  assert.equal(rearm(null, 2).works, false);
  assert.equal(restart(3, 2, 0).reason, "1 of 3 lost");
});

test("S4 times the post's arrival when it shows, while presence is still up", () => {
  // A hosted core's run: the waiter was re-armed before the post, so presence
  // never cleared; the post showed 5s after it, but the watch looked only
  // at 100s and called it 70s.
  const w = new KillWatch(0);
  assert.equal(w.due(29_000), false);
  assert.equal(w.due(30_000), true);
  w.post(42_000);
  w.presence(43_000, true);
  assert.equal(w.done(44_000), false);
  w.arrived(47_000);
  assert.equal(w.arrivedMs, 5000);
  assert.equal(w.done(47_000), true);
  assert.equal(w.clearedMs, null);
  // Presence clearing (the post not arrived yet) also ends the watch, and
  // so does giving up; an arrival before the post doesn't count.
  const c = new KillWatch(0);
  c.arrived(10_000);
  assert.equal(c.arrivedMs, null);
  c.presence(17_000, false);
  assert.equal(c.done(20_000), false);
  c.post(30_000);
  assert.equal(c.clearedMs, 17_000);
  assert.equal(c.done(30_000), true);
  const g = new KillWatch(0);
  g.post(30_000);
  assert.equal(g.done(100_000), false);
  assert.equal(g.done(100_001), true);
});

test("S9's argument check", () => {
  const call = (o: Partial<{ tool: string; at: string; harness: string | null; key: string | null; conv: string | null; session: string; rec: string }>) => ({
    at: o.at ?? "2026-10-20T00:00:01Z",
    tool: o.tool ?? "watch",
    client: "c",
    args: { harness: o.harness ?? null, conversation_id: o.conv ?? null, session_key: o.key ?? null },
    agent: { harness: o.rec ?? "cursor", via: "client", claimed: null },
    session: o.session,
    events_in_result: [],
  });
  const good = argsCheck([[call({ harness: "cursor", conv: "c1", session: "k1" }), call({ at: "2026-10-20T00:00:02Z", harness: "cursor-agent", key: "k1", conv: "c1", session: "k1" })]], { harness: "cursor", conversation: "agent" });
  assert.equal(good.metrics.harness_ok, true);
  assert.equal(good.metrics.session_key_ok, true);
  assert.equal(good.metrics.conversation_ok, true);
  const bad = argsCheck([[call({ harness: "claude-code", session: "k1" }), call({ at: "2026-10-20T00:00:02Z", key: "other", session: "k1" })]], { harness: "cursor", conversation: "agent" });
  assert.equal(bad.metrics.harness_ok, false);
  assert.equal(bad.metrics.session_key_ok, false);
  assert.equal(bad.metrics.conversation_ok, false);
  assert.equal(argsCheck([[]], { harness: "x", conversation: "none" }).works, false);
});

// ── Files and containers ─────────────────────────────────────────────────

test("rewriting and scrubbing keep a file's permissions", () => {
  const root = scratch();
  const p = join(root, "results.json");
  writeFileSync(p, `{"u":"http://h/v1/stream?link=Xk3vQ9mZ2pLr7sT4wY8bN1cD5fG6hJ0aE"}`, { mode: 0o600 });
  assert.deepEqual(scrub(root), ["results.json"]);
  assert.equal(statSync(p).mode & 0o777, 0o600);
  assert.ok(!readFileSync(p, "utf8").includes("Xk3vQ9mZ"));
  const q = join(root, "x.log");
  writeFileSync(q, "a", { mode: 0o640 });
  rewrite(q, "b");
  assert.equal(statSync(q).mode & 0o777, 0o640);
  rmSync(root, { recursive: true });
});

test("only bench-named containers are recorded for removal", () => {
  const p = new Procs(null, "wb-test");
  p.recordContainer("wb-20261020-011712-34a8-hermes");
  assert.ok(p.containers.has("wb-20261020-011712-34a8-hermes"));
  for (const bad of ["ws-hermes", "ws-openclaw", "wb-hermes", "wb-20261020-011712-34a8-x; rm"]) assert.throws(() => p.recordContainer(bad), /not a bench container/);
});

// S16 and S17's lane options: the job tools on the lanes that ask for them,
// and S16's disguise (an agent Wakeshift doesn't recognize).
const control = (base: string, path: string, body?: unknown) =>
  fetch(`${base}${path}`, { method: body === undefined ? "GET" : "POST", headers: { authorization: "Bearer tok", "content-type": "application/json" }, body: body === undefined ? undefined : JSON.stringify(body) });

async function toolNames(base: string, lane: string): Promise<string[]> {
  const c = new Client({ name: "t", version: "0" });
  await c.connect(new StreamableHTTPClientTransport(new URL(`${base}/mcp/${lane}`)));
  try {
    return (await c.listTools()).tools.map((t) => t.name).sort();
  } finally {
    await c.close();
  }
}

test("lane options: the job tools only on the lanes that ask for them; bad options are refused", async () => {
  await withApp(async (base) => {
    for (const bad of [{ lane: "x", tools: ["rm_rf"] }, { lane: "x", tools: "start_job" }, { lane: "X!", tools: [] }, { lane: "x", unrecognized: "yes" }]) {
      const r = await control(base, "/control/options", bad);
      assert.equal(r.status, 400, JSON.stringify(bad));
      await r.text();
    }
    assert.equal((await control(base, "/control/options", { lane: "jobs", tools: ["start_job", "job_status"] })).status, 200);
    assert.equal((await control(base, "/control/options", { lane: "status", tools: ["job_status"] })).status, 200);
    assert.deepEqual(await toolNames(base, "plain"), ["report_seen", "unwatch", "watch"], "a lane without options keeps exactly the contract's three");
    assert.deepEqual(await toolNames(base, "status"), ["job_status", "report_seen", "unwatch", "watch"]);
    assert.deepEqual(await toolNames(base, "jobs"), ["job_status", "report_seen", "start_job", "unwatch", "watch"]);
  });
});

test("start_job: each call starts a job (counted) and subscribes; job_status reads the latest event", async () => {
  await withApp(async (base, core) => {
    await control(base, "/control/options", { lane: "jobs", tools: ["start_job", "job_status"] });
    const c = new Client({ name: "codex-mcp-client", version: "0" });
    await c.connect(new StreamableHTTPClientTransport(new URL(`${base}/mcp/jobs`)));
    try {
      const text = (r: Awaited<ReturnType<typeof c.callTool>>) => (r.content as { type: string; text?: string }[]).map((x) => x.text ?? "").join("\n");
      assert.match(text(await c.callTool({ name: "job_status", arguments: {} })), /^Status: running/);
      assert.match(text(await c.callTool({ name: "start_job", arguments: {} })), /^Started bench job #1\./);
      assert.match(text(await c.callTool({ name: "start_job", arguments: {} })), /^Started bench job #2\./);
      assert.equal(core.calls.filter((x) => x.path === "/event-subscriptions").length, 2, "start_job subscribes, as watch does");
      const [e] = ((await (await control(base, "/control/publish", { lane: "jobs", tag: "the bench job passed" })).json()) as { events: { code: string; at: string }[] }).events;
      assert.equal(text(await c.callTool({ name: "job_status", arguments: {} })).split("\n")[0], `Status: finished. Result: the bench job passed (${e.code}, at ${e.at}).`);
      const lane = (await (await control(base, "/control/lane?lane=jobs")).json()) as { jobs: number; calls: { tool: string; subscription?: string }[]; options: { tools: string[] } };
      assert.equal(lane.jobs, 2);
      assert.deepEqual(lane.options.tools, ["start_job", "job_status"]);
      assert.deepEqual(lane.calls.map((x) => x.tool), ["job_status", "start_job", "start_job", "job_status"]);
      assert.ok(lane.calls[1].subscription);
    } finally {
      await c.close();
    }
  });
});

test("S16's disguise: Wakeshift sees an unknown client, no harness _meta, no claim; the record keeps the claim", async () => {
  await withApp(async (base, core) => {
    await control(base, "/control/options", { lane: "unk", unrecognized: true, tools: ["job_status"] });
    const c = new Client({ name: "claude-code", version: "2.1.316" });
    await c.connect(new StreamableHTTPClientTransport(new URL(`${base}/mcp/unk`), { requestInit: { headers: { "user-agent": "claude-code/2.1.316 (cli)" } } }));
    try {
      const r = await c.callTool({ name: "watch", arguments: { harness: "claude-code" }, _meta: { "claudecode/toolUseId": "toolu_1", "x-codex-turn-metadata": { thread_id: "t1" }, progressToken: 7 } });
      assert.ok(!r.isError);
    } finally {
      await c.close();
    }
    const sub = core.calls.find((x) => x.path === "/event-subscriptions");
    assert.ok(sub, "watch subscribed");
    const sent = JSON.stringify(sub.body);
    assert.doesNotMatch(sent, /claude-code|claudecode|x-codex|toolu_1/, `what reached Wakeshift: ${sent}`);
    assert.match(sent, /acme-agent/);
    const lane = (await (await control(base, "/control/lane?lane=unk")).json()) as { calls: { tool: string; client: string; args: { harness: string | null } }[] };
    assert.equal(lane.calls[0].client, "acme-agent");
    assert.equal(lane.calls[0].args.harness, "claude-code", "the agent's claim is in the record, not sent on");
  });
});
