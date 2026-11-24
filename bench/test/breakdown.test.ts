import { strict as assert } from "node:assert";
import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { test } from "node:test";
import { fileURLToPath } from "node:url";
import { onboardingOnly } from "../drivers/antigravity/index.js";
import { codexSocket, listsNoHooks, SOCKET_PATH_MAX } from "../drivers/codex/index.js";
import { Hermes } from "../drivers/hermes/index.js";
import { OpenClaw } from "../drivers/openclaw/index.js";
import { gradeRun, parseTiers } from "../src/grade.js";
import { traceOf } from "../src/core.js";
import type { Lane } from "../src/lane.js";
import { breakdownOf, install, stageMedians } from "../src/metrics.js";
import type { RunResult } from "../src/model.js";
import { renderReport } from "../src/report.js";
import { blockReason, shownRaw } from "../src/scenarios.js";

const tiers = parseTiers(readFileSync(join(dirname(fileURLToPath(import.meta.url)), "../../tiers.yaml"), "utf8"));

test("a breakdown is ms after publish at each stage", () => {
  const t0 = "2026-10-20T10:00:00.000Z";
  const b = breakdownOf("WB-ab12-1", t0, {
    accepted_at: "2026-10-20T10:00:00.020Z",
    trace: { created_at: "2026-10-20T10:00:00.050Z", available_at: "2026-10-20T10:00:00.050Z", handed_at: "2026-10-20T10:00:00.300Z", finished_at: "2026-10-20T10:00:01.000Z", state: "acked", attempts: 1 },
    pane: Date.parse(t0) + 2500,
    reported_at: "2026-10-20T10:00:03.000Z",
    got: Date.parse(t0) + 3600,
  });
  assert.deepEqual(b, { code: "WB-ab12-1", accepted: 20, created: 50, available: 50, handed: 300, confirmed: 1000, pane: 2500, reported: 3000, got: 3600 });
  const none = breakdownOf("WB-ab12-2", t0, { accepted_at: "2026-10-20T10:00:00.010Z" });
  assert.equal(none.handed, null);
  assert.equal(none.got, null);
});

test("agy: only the onboarding booleans are carried into a temp HOME", () => {
  assert.deepEqual(onboardingOnly({ onboardingComplete: true, consumerOnboardingComplete: true, enterpriseOnboardingComplete: false, token: "secret", onboardingCompleteX: true, nested: { a: 1 } }), {
    onboardingComplete: true,
    consumerOnboardingComplete: true,
    enterpriseOnboardingComplete: false,
  });
  assert.deepEqual(onboardingOnly({ onboardingComplete: "yes" }), {});
  assert.deepEqual(onboardingOnly(null), {});
  assert.deepEqual(onboardingOnly([true]), {});
});

test("the hand-off is when the delivery went in_flight, not when it was confirmed", () => {
  const row = { created_at: "2026-10-20T10:00:00.010Z", available_at: "2026-10-20T10:00:00.010Z", state: "acked", attempts: 1 };
  // Confirmed 5ms after it was handed over (between any two polls).
  const t = traceOf(row, [
    { state: "pending", at: "2026-10-20T10:00:00.010Z" },
    { state: "in_flight", at: "2026-10-20T10:00:00.040Z" },
    { state: "acked", at: "2026-10-20T10:00:00.045Z" },
  ])!;
  assert.equal(t.handed_at, "2026-10-20T10:00:00.040Z");
  assert.equal(t.finished_at, "2026-10-20T10:00:00.045Z");
  // Redelivered: the first hand-off counts, the last confirmation too.
  const again = traceOf(row, [
    { state: "in_flight", at: "2026-10-20T10:00:01.000Z" },
    { state: "pending", at: "2026-10-20T10:00:31.000Z" },
    { state: "in_flight", at: "2026-10-20T10:00:32.000Z" },
    { state: "acked", at: "2026-10-20T10:00:33.000Z" },
  ])!;
  assert.equal(again.handed_at, "2026-10-20T10:00:01.000Z");
  // Acked with no in_flight recorded: unknown, never the ack time.
  assert.equal(traceOf(row, [{ state: "acked", at: "2026-10-20T10:00:00.045Z" }])!.handed_at, null);
  // Delivered to a consumer that can't acknowledge: that write is the hand-off.
  assert.equal(traceOf({ ...row, state: "delivered" }, [{ state: "delivered", at: "2026-10-20T10:00:00.050Z" }])!.handed_at, "2026-10-20T10:00:00.050Z");
  // Still pending when the wait ended.
  const pending = traceOf({ ...row, state: "pending" }, [{ state: "pending", at: "2026-10-20T10:00:00.010Z" }])!;
  assert.equal(pending.handed_at, null);
  assert.equal(pending.finished_at, null);
  assert.equal(traceOf(undefined, []), null);
});

test("Codex's socket path: the default run root fits, a deep one doesn't", () => {
  const bytes = (p: string) => Buffer.byteLength(codexSocket(p));
  assert.ok(bytes("/tmp/wakebench/20261020-112919-23ce/homes/codex-i") <= SOCKET_PATH_MAX);
  assert.ok(bytes("/private/tmp/wb-sec90.QSQp/runs/20261020-113905-1950/homes/codex-i") > SOCKET_PATH_MAX);
  // In bytes: a non-ASCII path is longer than its length in characters.
  const wide = "/tmp/ü/20261020-112919-23ce/homes/codex-i";
  assert.ok(bytes(wide) > codexSocket(wide).length);
});

test("Codex 0.166's /hooks for a thread started before the install lists no hooks", () => {
  const none = [
    "  Event                 Installed   Active      Description",
    "› PreToolUse            0           0           Before a tool executes",
    "  PostToolUse           0           0           After a tool executes",
    "  SessionStart          0           0           When a new session starts",
    "  UserPromptSubmit      0           0           When the user submits a prompt",
  ].join("\n");
  assert.ok(listsNoHooks(none));
  assert.ok(!listsNoHooks(none.replace("PostToolUse           0           0", "PostToolUse           1           0")));
  assert.ok(!listsNoHooks("1 hook needs review before it can run. Press t to trust."));
});

test("why the agent's installer run was blocked, quoted from the pane", () => {
  const screen = [
    "  ▎ Ask them first; only if they agree, run curl -fsSL https://x/install/claude-code.sh | sh.",
    "⏺ I tried to run the installer, but auto mode blocked it: it pipes downloaded code into a shell.",
    "  You can run it yourself:",
    "  ! curl -fsSL https://x/install/claude-code.sh | sh",
    "⏺ GOT WB-ab12-1",
  ].join("\n");
  assert.match(blockReason(screen, "Ask them first; only if they agree, run curl -fsSL https://x/install/claude-code.sh | sh.")!, /auto mode blocked it: it pipes downloaded code into a shell/);
  assert.equal(blockReason("⏺ Installed. Hooks connect in running sessions."), null);
  // A skill's name isn't a refusal; Hermes's approval timing out is.
  assert.equal(blockReason("│   web: blocked-page-recovery"), null);
  assert.match(blockReason("⚠ Approval: curl -fsSL https://x/install/hermes.sh | sh → timed out (no response)")!, /timed out/);
});

test("S6: woken but never reported is told apart from no wake", () => {
  const base = { agent_ran_installer: true, trust_relayed: true, installed: true, connected_ms: 1000, wake_ms: null };
  assert.equal(install({ ...base, seen_unreported: true }).reason, "woken, but the agent never reported the event");
  assert.equal(install({ ...base, seen_unreported: false }).reason, "no wake after the install");
  assert.equal(install({ ...base, wake_ms: 5000 }).works, true);
});

test("container harnesses: a menu approval is answered, the installer refused when the user said no", async () => {
  const keys: string[] = [];
  const lane = { session: "s", prompted: () => ({}), tmux: { keys: async (_s: string, k: string) => void keys.push(k), type: async () => {} } } as unknown as Lane;
  const menu = ["⚠️  Dangerous Command", "curl -fsSL https://x/install/hermes.sh | sh", "❯ 1. Allow once", "  2. Allow for this session", "  3. Add to permanent allowlist", "  4. Deny"].join("\n");
  const h = new Hermes();
  assert.equal(await h.answer(lane, menu, { allowInstall: true }), "approved the installer");
  assert.deepEqual(keys, ["Enter"]);
  keys.length = 0;
  assert.equal(await h.answer(lane, menu, { allowInstall: false }), "denied the installer (the user said no)");
  assert.deepEqual(keys, ["Down", "Down", "Down", "Enter"]);
  keys.length = 0;
  // S12: "Allow for this session", never the permanent allowlist.
  assert.equal(await h.answer(lane, menu, { allowInstall: true, scope: "session" }), "approved the installer (session)");
  assert.deepEqual(keys, ["Down", "Enter"]);
  keys.length = 0;
  assert.equal(await h.answer(lane, "Welcome to Hermes Agent! Type your message", { allowInstall: true }), null);
  assert.deepEqual(keys, []);
});

test("OpenClaw is busy while its status line says so", () => {
  const o = new OpenClaw();
  assert.equal(o.busy("…\n ⠼ streaming • 1m 49s | connected\n agent main | session x"), true);
  assert.equal(o.busy("…\n gateway connected | idle\n agent main | session x"), false);
  // While the model thinks, a word of its own; while a message goes out, "sending".
  assert.equal(o.busy("…\n ⠴ hobnobbing… • 6s | connected\n agent main | session x"), true);
  assert.equal(o.busy("…\n ⠸ sending • 0s | connected\n agent main | session x"), true);
});

test("container harnesses: a question in the chat and the user's yes aren't a y/n prompt", async () => {
  const typed: string[] = [];
  const lane = { session: "s", prompted: () => ({}), tmux: { keys: async () => {}, type: async (_s: string, t: string) => void typed.push(t) } } as unknown as Lane;
  const chat = ["…it turns on OpenClaw's hooks endpoint, allowing it to start turns only in the conversations…", "Would you like me to install Wakeshift's OpenClaw hooks?", "Yes, please install it.", " ⠸ sending • 0s | connected", " agent main | session x"].join("\n");
  assert.equal(await new OpenClaw().answer(lane, chat, { allowInstall: true }), null);
  assert.deepEqual(typed, []);
  assert.equal(await new OpenClaw().answer(lane, "…\nRun curl -fsSL https://x/install/openclaw.sh | sh? [y/N]", { allowInstall: false }), "denied the installer (the user said no)");
  assert.deepEqual(typed, ["n"]);
});

test("stage medians", () => {
  const mk = (handed: number | null, got: number) => ({ code: "x", accepted: 10, created: 20, available: 20, handed, confirmed: null, pane: null, reported: null, got });
  const m = stageMedians([mk(100, 3000), mk(300, 4000), mk(null, 5000)]);
  assert.equal(m.p50_handed_ms, 200);
  assert.equal(m.p50_got_ms, 4000);
  assert.equal(m.p50_confirmed_ms, null);
});

test("the code in the pane, not counting GOT lines or the report_seen call", () => {
  assert.equal(shownRaw("⏺ GOT WB-ab12-1\n", "WB-ab12-1"), false);
  assert.equal(shownRaw("  Called wakebench.report_seen ids: [WB-ab12-1]\n", "WB-ab12-1"), false);
  assert.equal(shownRaw('{"type":"event","subject":"Bench event WB-ab12-1."}\n', "WB-ab12-1"), true);
});

test("the report shows the breakdown in the cell and the detail", () => {
  const run: RunResult = {
    run: "r",
    target: "local",
    core: "",
    startedAt: "2026-10-20T00:00:00Z",
    scenarios: ["S2"],
    harnesses: [
      {
        id: "claude-code",
        name: "Claude Code",
        tier: "",
        mode: "automated",
        status: "ran",
        logs: [],
        scenarios: [
          {
            id: "S2",
            harness: "claude-code",
            status: "ran",
            works: true,
            metrics: { p50_ms: 3700, max_ms: 3800, p50_handed_ms: 120, p50_got_ms: 3700 },
            breakdown: [{ code: "WB-ab12-1", accepted: 20, created: 40, available: 40, handed: 120, confirmed: 900, pane: 2200, reported: 3000, got: 3700 }],
            steps: [],
            notes: [],
          },
        ],
      },
    ],
    safety: { ok: true, changed: [], checked: [] },
    notes: [],
  };
  gradeRun(run, tiers);
  const html = renderReport(run, tiers, "/nonexistent");
  const data = /<script id="data" type="application\/json">(.*?)<\/script>/s.exec(html)![1];
  const js = [...html.matchAll(/<script>(.*?)<\/script>/gs)].pop()![1];
  const el = (text = "") => ({ textContent: text, innerHTML: "", on: null as null | ((e: unknown) => void), classList: { add() {}, remove() {} }, addEventListener(_: string, f: (e: unknown) => void) { this.on = f; }, querySelectorAll: () => [] });
  const grid = el();
  const detail = el();
  const doc = { getElementById: (id: string) => ({ data: el(data), grid, detail })[id], querySelectorAll: () => [], querySelector: () => null };
  new Function("document", "location", "history", js)(doc, { hash: "" }, { replaceState() {} });
  assert.match(grid.innerHTML, /waiter 0\.1s · then 3\.6s/);
  grid.on!({ target: { closest: () => ({ dataset: { h: "claude-code", s: "S2" }, classList: { add() {} } }) } });
  assert.match(detail.innerHTML, /where the time went/);
  assert.match(detail.innerHTML, /WB-ab12-1/);
  assert.match(detail.innerHTML, /handed to a waiter: \+0\.08s/);
});

test("WB_CODEX_PIN=floor runs the pinned oldest Codex from the bench's state", async () => {
  const { mkdtempSync, rmSync, writeFileSync } = await import("node:fs");
  const { tmpdir } = await import("node:os");
  const { pinnedCodexBin } = await import("../drivers/codex/index.js");
  const dir = mkdtempSync(join(tmpdir(), "wb-pins-"));
  try {
    const pins = join(dir, "pins.json");
    writeFileSync(pins, JSON.stringify({ codex: "0.167.0", "codex-floor": "0.163.0" }));
    assert.equal(pinnedCodexBin({}, pins), null);
    assert.equal(pinnedCodexBin({ WB_CODEX_PIN: "floor", WB_STATE_DIR: dir }, pins), join(dir, "codex", "0.163.0", "bin", "codex"));
    writeFileSync(pins, JSON.stringify({ codex: "0.167.0" }));
    assert.throws(() => pinnedCodexBin({ WB_CODEX_PIN: "floor", WB_STATE_DIR: dir }, pins));
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
  // The repository's pins name a floor.
  assert.match(pinnedCodexBin({ WB_CODEX_PIN: "floor", WB_STATE_DIR: "/s" }) ?? "", /^\/s\/codex\/\d+\.\d+\.\d+\/bin\/codex$/);
});
