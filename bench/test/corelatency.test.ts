// WB_CORE_LATENCY_MS: a local core behind a delaying proxy, and the S8 re-watch options.
import { strict as assert } from "node:assert";
import { mkdirSync, mkdtempSync, rmSync, utimesSync, writeFileSync } from "node:fs";
import { connect, createServer, type AddressInfo } from "node:net";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { test } from "node:test";
import { newestSession } from "../drivers/claude-code/index.js";
import { coreLatencyMs, delayProxy, freePort } from "../src/core.js";
import { s8IdleMs, s8Rewatch } from "../src/scenarios.js";

test("coreLatencyMs: none by default, a positive number of milliseconds, capped", () => {
  assert.equal(coreLatencyMs({}), 0);
  assert.equal(coreLatencyMs({ WB_CORE_LATENCY_MS: "abc" }), 0);
  assert.equal(coreLatencyMs({ WB_CORE_LATENCY_MS: "-5" }), 0);
  assert.equal(coreLatencyMs({ WB_CORE_LATENCY_MS: "150" }), 150);
  assert.equal(coreLatencyMs({ WB_CORE_LATENCY_MS: "99999" }), 2000);
});

test("delayProxy: every chunk arrives in order, each way, no sooner than the delay", async () => {
  // An echo server behind the proxy.
  const echo = createServer((s) => s.pipe(s));
  await new Promise<void>((r) => echo.listen(0, "127.0.0.1", () => r()));
  const port = await freePort();
  const proxy = delayProxy(port, (echo.address() as AddressInfo).port, 60);
  await new Promise<void>((r) => (proxy.listening ? r() : proxy.once("listening", () => r())));
  try {
    const c = connect(port, "127.0.0.1");
    await new Promise<void>((r) => c.once("connect", () => r()));
    const t0 = Date.now();
    let got = "";
    const done = new Promise<void>((r) => c.on("data", (b) => (got += b.toString()) === "abcdefghij" && r()));
    for (const ch of "abcdefghij") {
      c.write(ch);
      await new Promise((r) => setTimeout(r, 3));
    }
    await done;
    assert.equal(got, "abcdefghij");
    assert.ok(Date.now() - t0 >= 120, `the round trip took ${Date.now() - t0}ms, under twice the delay`);
    c.destroy();
  } finally {
    proxy.close();
    echo.close();
  }
});

test("S8 re-watch options: off by default; 1 or fresh; the idle time capped", () => {
  const keep = { ...process.env };
  try {
    delete process.env.WB_S8_REWATCH;
    delete process.env.WB_S8_IDLE;
    assert.equal(s8Rewatch(), false);
    assert.equal(s8IdleMs(), 0);
    process.env.WB_S8_REWATCH = "fresh";
    assert.equal(s8Rewatch(), true);
    process.env.WB_S8_REWATCH = "1";
    assert.equal(s8Rewatch(), true);
    process.env.WB_S8_IDLE = "240";
    assert.equal(s8IdleMs(), 240_000);
    process.env.WB_S8_IDLE = "100000";
    assert.equal(s8IdleMs(), 3_600_000);
  } finally {
    process.env = keep;
  }
});

test("newestSession: the newest main-session transcript's ID, never an odd file name", () => {
  const home = mkdtempSync(join(tmpdir(), "wb-newest-"));
  try {
    assert.equal(newestSession(home), null);
    const dir = join(home, ".claude/projects/-work");
    mkdirSync(dir, { recursive: true });
    const main = "22222222-bbbb-4ccc-8ddd-eeeeeeeeeeee";
    const old = join(dir, "11111111-aaaa-4aaa-8aaa-aaaaaaaaaaaa.jsonl");
    writeFileSync(old, "{}\n");
    utimesSync(old, new Date(Date.now() - 60_000), new Date(Date.now() - 60_000));
    writeFileSync(join(dir, `${main}.jsonl`), "{}\n");
    writeFileSync(join(dir, "bad name;rm.jsonl"), "{}\n");
    // A subagent's transcript, newer, beside them (older releases): never resumed.
    writeFileSync(join(dir, "agent-3f2a9c.jsonl"), "{}\n");
    assert.equal(newestSession(home), main);
  } finally {
    rmSync(home, { recursive: true, force: true });
  }
});
