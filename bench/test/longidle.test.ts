// S13 (long idle, past Claude Code's 30-minute Monitor limit): what the
// transcript says about Monitor starts and expiries, the agent's words, the
// outcome and its cell.

import { strict as assert } from "node:assert";
import { test } from "node:test";
import { chooseOption } from "../drivers/base.js";
import { parseMonitorLog } from "../drivers/claude-code/index.js";
import { keyOf } from "../src/grade.js";
import { longIdle } from "../src/metrics.js";
import type { ScenarioResult } from "../src/model.js";
import { ASKS_REARM, rearmWords, s13Path, STOPPED } from "../src/scenarios.js";

const line = (o: unknown) => JSON.stringify(o);
const t = (min: number) => new Date(Date.UTC(2026, 9, 29, 12, min)).toISOString();

test("parseMonitorLog: Monitor calls since a time, and an expiry at Claude Code's limit", () => {
  const monitor = (id: string, min: number) => line({ type: "assistant", timestamp: t(min), message: { content: [{ type: "tool_use", id, name: "Monitor", input: { command: "u='x'" } }] } });
  const lines = [
    monitor("a", 0),
    // The same call written twice (streamed): one start.
    monitor("b", 26),
    monitor("b", 26),
    line({ type: "assistant", timestamp: t(27), message: { content: [{ type: "tool_use", id: "c", name: "Bash", input: { command: "ls" } }] } }),
    "{ half a line",
  ];
  assert.deepEqual(parseMonitorLog(lines, Date.parse(t(1))), { starts: 1, expired: false });
  assert.deepEqual(parseMonitorLog(lines, Date.parse(t(0))), { starts: 2, expired: false });
  const expiry = line({ type: "queue-operation", operation: "enqueue", timestamp: t(30), content: "<task-notification>[Monitor expired after 30m with no events delivered. Re-arm it if you still need the watch.]</task-notification>" });
  assert.equal(parseMonitorLog([...lines, expiry], Date.parse(t(0))).expired, true);
  assert.equal(parseMonitorLog([...lines, expiry], Date.parse(t(31))).expired, false);
});

test("S13: the agent asking whether to keep watching, or saying it stopped", () => {
  for (const s of [
    "The monitor has expired. Want me to re-arm it?",
    "Should I restart the watch?",
    "Would you like me to start the monitor again?",
    "Let me know if you want me to keep watching.",
  ])
    assert.ok(ASKS_REARM.test(s), s);
  for (const s of ["Restarted the monitor.", "GOT WB-ab12-3", "Re-armed the Wakeshift Monitor (routine restart)."]) assert.ok(!ASKS_REARM.test(s), s);
  assert.ok(STOPPED.test("I'm no longer watching for bench events."));
  assert.ok(!STOPPED.test("Still watching for bench events."));
  // The install question isn't about keeping watch (wakebench, October 29: a false "asked").
  const install = "⎿ · Should I install Wakeshift's hooks for Claude Code so bench events arrive without a Monitor that has to be restarted every 25 minutes?";
  assert.deepEqual(rearmWords(install + "\n⏺ The Monitor hit its routine 25-minute restart and I started it again."), { asked: false, stopped: false });
  assert.deepEqual(rearmWords("⏺ The Monitor ended, so I'm no longer watching.\n  Want me to re-arm it?"), { asked: true, stopped: true });
  // A line about the install can still ask to keep watching.
  assert.deepEqual(rearmWords("⏺ Should I restart the Monitor, or install the hooks instead?"), { asked: true, stopped: false });
  assert.ok(!ASKS_REARM.test("Should I install the hooks, so you're woken without a Monitor?"));
});

test("S13's path: the Monitor unless WB_S13_PATH=installed", () => {
  assert.equal(s13Path({}), "zero-install");
  assert.equal(s13Path({ WB_S13_PATH: "installed" }), "installed");
});

test("longIdle: works when every post arrives; the cell says how", () => {
  const ok = longIdle({ idleMs: 36 * 60_000, arrivals: [4200, 5100], unprompted: 2, nudges: 0, askedUser: false, toldStopped: false, restarts: 2, expired: false, maxGapMs: 15_000, connectedShare: 0.98 });
  assert.equal(ok.works, true);
  assert.equal(ok.metrics.lost, 0);
  const r: ScenarioResult = { id: "S13", harness: "claude-code", status: "ran", metrics: ok.metrics, steps: [], notes: [] };
  assert.equal(keyOf(r), "2/2, max 5.1s, 2 restarts");
  const lost = longIdle({ idleMs: 36 * 60_000, arrivals: [null, 9000], unprompted: 0, nudges: 1, askedUser: true, toldStopped: true, restarts: 1, expired: true, maxGapMs: 400_000, connectedShare: 0.8 });
  assert.equal(lost.works, false);
  assert.match(lost.reason ?? "", /1 of 2 posts/);
  assert.equal(keyOf({ ...r, metrics: { ...lost.metrics } }), "lost 1/2, 1 restart, asked the user, expired · 1 nudge");
});

test("WB_APPROVE_SCOPE=session: a user who says Yes each time, not don't ask again", () => {
  const opts = [
    { n: 1, label: "Yes", cursor: true },
    { n: 2, label: "Yes, and don't ask again for Bash(:)", cursor: false },
    { n: 3, label: "No", cursor: false },
  ];
  const was = process.env.WB_APPROVE_SCOPE;
  try {
    delete process.env.WB_APPROVE_SCOPE;
    assert.equal(chooseOption(opts, {})?.opt.n, 2);
    process.env.WB_APPROVE_SCOPE = "session";
    assert.equal(chooseOption(opts, {})?.opt.n, 1);
  } finally {
    if (was === undefined) delete process.env.WB_APPROVE_SCOPE;
    else process.env.WB_APPROVE_SCOPE = was;
  }
});
