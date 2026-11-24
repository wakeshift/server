import { strict as assert } from "node:assert";
import { mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { scrub } from "../src/cli.js";
import { test } from "node:test";
import { fileURLToPath } from "node:url";
import { namesHome } from "../drivers/base.js";
import { ansiToHtml, color256, stripAnsi } from "../src/ansi.js";
import { checkBar, grade, gradeRun, keyOf, parseTiers, tierOf } from "../src/grade.js";
import { normalizeHarness, ourPart } from "../src/metrics.js";
import { assisted } from "../drivers/assisted/index.js";
import { median, parseScenarios, type Breakdown, type RunResult, type ScenarioResult } from "../src/model.js";
import { redact, redactDeep, redactPlain, REDACTED } from "../src/redact.js";
import { renderReport, scriptJson } from "../src/report.js";
import { changed } from "../src/safety.js";
import { CANT_WAKE, extractQuote, gotRe, ownWords, s1Offer } from "../src/scenarios.js";
import { driverFor, HARNESSES } from "../drivers/index.js";

const here = dirname(fileURLToPath(import.meta.url));
const tiersText = readFileSync(join(here, "../../tiers.yaml"), "utf8");

// ── Redaction ────────────────────────────────────────────────────────────

const TOKEN = "Xk3vQ9mZ2pLr7sT4wY8bN1cD5fG6hJ0aE";

test("redacts stream and wait links, keeping the rest of the URL", () => {
  const s = `curl -s "http://localhost:8370/v1/stream/wait?link=${TOKEN}&ack=&timeout=50s"`;
  const r = redactPlain(s);
  assert.ok(!r.includes(TOKEN));
  assert.ok(r.includes(`/v1/stream/wait?link=${REDACTED}&ack=&timeout=50s`));
});

test("redacts session keys, app keys, beacons, bearer tokens and publish URLs", () => {
  const cases = [
    `Pass session_key="wsc_${TOKEN}" when you call`,
    `key wst_test_wakebench_${TOKEN}`,
    `(For Wakeshift's hook on this computer, not for you: wakeshift-beacon: eyJhbGciOiJFZERTQSJ9.eyJ0aWNrZXQiOiJhYmMifQ.c2lnbmF0dXJlLWJ5dGVz)`,
    `"_meta":{"wakeshift/connector":{"beacon":"eyJhbGciOiJFZERTQSJ9.eyJ0aWNrZXQiOiJhYmMifQ.c2lnbmF0dXJlLWJ5dGVz"}}`,
    `Authorization: Bearer ${TOKEN}`,
    `POST https://app.wakeshift.ai/v1/publish/${TOKEN}`,
    `{"session_key": "${TOKEN}"}`,
  ];
  for (const c of cases) {
    const r = redactPlain(c);
    assert.ok(!r.includes(TOKEN) && !r.includes("eyJ0aWNrZXQi"), `leaked: ${r}`);
    assert.ok(r.includes(REDACTED), r);
  }
  // A current-format app key: wst_ and 50 base58 characters (the API key format's test vector).
  const key = "wst_2h52eWJ6Nz65aiGDNwcugUMid1EBPpxtzZszqoePwD2ybDJTaf";
  const r = redactPlain(`WAKESHIFT_KEY=${key}`);
  assert.ok(!r.includes(key.slice(4)), `leaked: ${r}`);
  assert.equal(r, `WAKESHIFT_KEY=wst_${REDACTED}`);
  // A subscribe voucher: wsv_ and 32 lowercase base32 characters, as a
  // native client's tool result and events/subscribe carry it.
  const voucher = "wsv_abcdefghijklmnopqrstuvwxyz234567";
  for (const c of [`wakeshift_voucher set to ${voucher} in its arguments`, `{"wakeshift_voucher":"${voucher}"}`]) {
    const v = redactPlain(c);
    assert.ok(!v.includes(voucher.slice(4)), `leaked: ${v}`);
    assert.ok(v.includes(`wsv_${REDACTED}`), v);
  }
});

test("redacts the wrapped rest of a token on the next line", () => {
  const s = `u='http://localhost/v1/stream/wait?link=Xk3vQ9mZ\n│ 2pLr7sT4wY8bN1cD5fG6hJ0aE&ack='`;
  const r = redactPlain(s);
  assert.ok(!r.includes("2pLr7sT4wY8bN1cD5fG6hJ0aE"), r);
});

test("redacts a signed link token Claude Code wrapped across lines", () => {
  const s =
    "  ▎ To be woken… u='http://localhost:53619/v1/stream/wait?link=eyJ2IjoxLCJzIjoiZTA1Mz\n" +
    "  ▎ 4NTJkYTRhIiwibCI6IjkxM2VmMmI5LWQ5NGEtNGE1MC1hZWY0LWRiYjg0NTFlODU0OCIsImUiOjE3OTI0ODEyOTF9.yMGmY8PGoh2j7HAdchP4gDAjtvuwlMeWeXbaGeazc8s&ack=&timeout=50s'";
  const r = redactPlain(s);
  for (const part of ["eyJ2IjoxLCJzIjoiZTA1Mz", "4NTJkYTRh", "yMGmY8PGoh2j7HAdchP4"]) assert.ok(!r.includes(part), `${part} in ${r}`);
  assert.match(r, /&ack=&timeout=50s'$/);
  // Wrapped right before the signature.
  const t = redactPlain("link=eyJ2IjoxLCJzIjoiZTA1MzNTJkYTRhIiwibCI6IjkxM2Vm\n│ .yMGmY8PGoh2j7HAdchP4gDAjtvuwlMeWeXbaGeazc8s&ack=");
  assert.ok(!t.includes("yMGmY8PGoh2j7HAdchP4"), t);
});

test("redacts a JWS split into short pieces over lines, as a quote stripped of its borders joins them", () => {
  // Each line's part is too short and too plain for the line rules alone.
  const s = "QUOTE: beacon eyJhbGciOi\n│ JFZERTQSJ9.eyJhIjox\n│ fQ.c2lnbmF0dXJl done";
  const r = redactPlain(s);
  for (const part of ["eyJhbGciOi", "JFZERTQSJ9", "eyJhIjox", "c2lnbmF0dXJl"]) assert.ok(!r.includes(part), `${part} in ${r}`);
  assert.match(r, /^QUOTE: beacon /);
  assert.match(r, / done$/);
  // The same with ANSI escapes kept.
  const a = redact("\x1b[2mQUOTE: beacon eyJhbGciOi\x1b[0m\n│ JFZERTQSJ9.eyJhIjox\n│ fQ.c2lnbmF0dXJl done");
  for (const part of ["eyJhbGciOi", "JFZERTQSJ9", "c2lnbmF0dXJl"]) assert.ok(!a.includes(part), `${part} in ${a}`);
  assert.ok(a.includes("\x1b[2m") && a.includes("\x1b[0m"));
  // A wrapped link token, kept parameter and all.
  const l = redactPlain("wait?link=abcd1234\n  efgh5678&timeout=50s");
  assert.ok(!l.includes("abcd1234") && !l.includes("efgh5678"), l);
  assert.match(l, /link=/);
  // Lines that only happen to touch aren't joined into a secret.
  assert.equal(redactPlain("see ?link=‹redacted›\nnext line"), "see ?link=‹redacted›\nnext line");
});

test("leaves ordinary text alone", () => {
  for (const s of ["GOT WB-ab12-3", "Watching bench events.", "/private/tmp/wakebench/20261020-000251-1378/homes/claude-code/work", "commit 4e00127", "a1b2c3d4-e5f6-7890-abcd-ef1234567890"]) {
    assert.equal(redactPlain(s), s);
  }
});

test("redacts through ANSI escapes and keeps them", () => {
  const s = `\x1b[32mlink=\x1b[1m${TOKEN.slice(0, 10)}\x1b[22m${TOKEN.slice(10)}\x1b[0m&ack= \x1b[31mwsc_${TOKEN}\x1b[0m`;
  const r = redact(s);
  assert.ok(!stripAnsi(r).includes(TOKEN.slice(10)), stripAnsi(r));
  assert.ok(!stripAnsi(r).includes(TOKEN.slice(0, 10)), stripAnsi(r));
  assert.equal(stripAnsi(r), `link=${REDACTED}&ack= wsc_${REDACTED}`);
  assert.ok(r.includes("\x1b[32m") && r.includes("\x1b[31m") && r.endsWith("\x1b[0m"));
});

test("redacts a long log line with escapes (no stack overflow)", () => {
  const line = ("\x1b[1Gword \x1b[32mmore\x1b[0m ").repeat(60_000) + `link=${TOKEN}`;
  const r = redact(line);
  assert.ok(!r.includes(TOKEN));
});

test("redacts inside OSC 8 hyperlinks (Cursor links the stream URL)", () => {
  const s = `\x1b[4m\x1b]8;;http://localhost:60132/v1/stream?format=lines&link=${TOKEN}\x07curl -sN 'http://localhost:60132/v1/stream?format=lines&link=${TOKEN}'\x1b]8;;\x07\x1b[0m`;
  const r = redact(s);
  assert.ok(!r.includes(TOKEN), r);
  assert.ok(r.includes("\x1b]8;;http://localhost:60132/v1/stream?format=lines&link=‹redacted›\x07"));
});

test("redaction is idempotent", () => {
  const s = `?link=${TOKEN} wsc_${TOKEN}`;
  assert.equal(redactPlain(redactPlain(s)), redactPlain(s));
  assert.equal(redact("\x1b[1m" + redact("\x1b[1m" + s)), redact("\x1b[1m\x1b[1m" + s));
});

test("redactDeep redacts strings and secret-named fields", () => {
  const v = redactDeep({ a: [`link=${TOKEN}`], session_key: "anything", n: 3, nested: { token: "t", ok: "fine" } });
  assert.deepEqual(v, { a: [`link=${REDACTED}`], session_key: REDACTED, n: 3, nested: { token: REDACTED, ok: "fine" } });
});

// ── ANSI rendering ───────────────────────────────────────────────────────

test("renders SGR colors and styles as spans", () => {
  assert.equal(ansiToHtml("\x1b[1;31mred\x1b[0m plain"), '<span style="color:#cd3131;font-weight:bold">red</span> plain');
  assert.equal(ansiToHtml("\x1b[38;5;196mx\x1b[39m"), `<span style="color:${color256(196)}">x</span>`);
  assert.equal(ansiToHtml("\x1b[38;2;10;20;30mx"), '<span style="color:#0a141e">x</span>');
  assert.equal(ansiToHtml("\x1b[48;5;236m\x1b[97mx\x1b[m"), `<span style="color:#ffffff;background:${color256(236)}">x</span>`);
  assert.equal(ansiToHtml("\x1b[7mx"), '<span style="color:var(--term-bg,#111);background:var(--term-fg,#ddd)">x</span>');
});

test("escapes HTML and drops other escapes and control characters", () => {
  assert.equal(ansiToHtml("<b>&\x1b[2K\x1b]0;title\x07\x1b[?25l</b>\x07"), "&lt;b&gt;&amp;&lt;/b&gt;");
  assert.equal(stripAnsi("\x1b[1ma\x1b[0mb\x1b]8;;http://x\x1b\\c"), "abc");
});

test("256-color palette", () => {
  assert.equal(color256(16), "#000000");
  assert.equal(color256(231), "#ffffff");
  assert.equal(color256(232), "#080808");
  assert.equal(color256(9), "#f14c4c");
});

// ── Grading ──────────────────────────────────────────────────────────────

const tiers = parseTiers(tiersText);
const r = (id: ScenarioResult["id"], harness: string, metrics: ScenarioResult["metrics"], works = true, status: ScenarioResult["status"] = "ran"): ScenarioResult => ({ id, harness, status, works, metrics, steps: [], notes: [] });

test("tiers.yaml: each harness in its tier", () => {
  assert.deepEqual(tierOf(tiers, "claude-code")?.id, "T1");
  assert.deepEqual(tierOf(tiers, "codex")?.id, "T1");
  for (const h of ["claude-cowork", "cursor", "cursor-ide", "opencode", "hermes"]) assert.equal(tierOf(tiers, h)?.id, "T2", h);
  for (const h of ["antigravity", "grok-bot", "chatgpt", "openclaw", "copilot"]) assert.equal(tierOf(tiers, h)?.id, "T3", h);
  assert.equal(tierOf(tiers, "fake")?.id, "dry-run");
  assert.deepEqual(tierOf(tiers, "fake")?.bars, tierOf(tiers, "claude-code")?.bars);
});

test("a harness in two tiers is refused", () => {
  assert.throws(() => parseTiers("tiers:\n  A: {harnesses: [x]}\n  B: {harnesses: [x]}\n"), /x is in A and B/);
});

test("bars: equal, at most, at least, not measured", () => {
  assert.equal(checkBar("p50_ms_max", 3000, { p50_ms: 2500 }), null);
  assert.match(checkBar("p50_ms_max", 3000, { p50_ms: 3100 })!, /at most 3000/);
  assert.equal(checkBar("seen_min", 5, { seen: 5 }), null);
  assert.match(checkBar("seen_min", 5, { seen: 4 })!, /at least 5/);
  assert.equal(checkBar("lost", false, { lost: false }), null);
  assert.match(checkBar("lost", false, { lost: true })!, /lost is true/);
  assert.match(checkBar("presence_clear_ms_max", 90000, { presence_clear_ms: null })!, /not measured/);
});

test("S2: tier 1 at 5s p50 / 10s max, tier 2 at 10s", () => {
  const t1 = tierOf(tiers, "claude-code");
  const t2 = tierOf(tiers, "cursor");
  assert.equal(grade(r("S2", "claude-code", { p50_ms: 4800, max_ms: 7000 }), t1).grade, "pass");
  // The turn start is a diagnostic: a slow one alone doesn't miss a bar.
  assert.equal(grade(r("S2", "claude-code", { p50_ms: 4800, max_ms: 7000, p50_turn_ms: 2500 }), t1).grade, "pass");
  const slow = grade(r("S2", "codex", { p50_ms: 5500, max_ms: 11000 }), tierOf(tiers, "codex"));
  assert.equal(slow.grade, "below-bar");
  assert.equal(slow.why!.length, 2);
  assert.equal(grade(r("S2", "cursor", { p50_ms: 5500, max_ms: 7000 }), t2).grade, "pass");
  assert.equal(grade(r("S2", "cursor", { p50_ms: 12000 }), t2).grade, "below-bar");
});

test("S2: Claude Code's own p50 bar is 6.5s (harness_bars), its max the tier's 10s; Codex keeps 5s", () => {
  const cc = tierOf(tiers, "claude-code");
  assert.equal(grade(r("S2", "claude-code", { p50_ms: 5800, max_ms: 8400 }), cc).grade, "pass");
  const slow = grade(r("S2", "claude-code", { p50_ms: 6800, max_ms: 11000 }), cc);
  assert.deepEqual(slow.why, ["p50_ms is 6800 (bar: at most 6500)", "max_ms is 11000 (bar: at most 10000)"]);
  assert.equal(grade(r("S2", "codex", { p50_ms: 5800, max_ms: 8400 }), tierOf(tiers, "codex")).grade, "below-bar");
  // The dry run (fake) is held to the tier's own bars, not Claude Code's.
  assert.equal(grade(r("S2", "fake", { p50_ms: 5800, max_ms: 8400 }), tierOf(tiers, "fake")).grade, "below-bar");
  // harness_bars only for the tier's own harnesses.
  assert.throws(() => parseTiers("tiers:\n  A: {harnesses: [x], harness_bars: {y: {S2: {p50_ms_max: 1}}}}\n"), /harness_bars names y, which isn't in A/);
});

test("S7: tier 1 needs automatic re-arm, tier 2 allows one nudge", () => {
  assert.equal(grade(r("S7", "codex", { nudges: 1, wake_ms: null }), tierOf(tiers, "codex")).grade, "below-bar");
  assert.equal(grade(r("S7", "opencode", { nudges: 1, wake_ms: null }), tierOf(tiers, "opencode")).grade, "pass");
});

test("tier 3: no bars beyond works", () => {
  assert.equal(grade(r("S2", "antigravity", { p50_ms: 60000 }), tierOf(tiers, "antigravity")).grade, "pass");
  assert.equal(grade(r("S2", "antigravity", {}, false), tierOf(tiers, "antigravity")).grade, "fail");
});

test("blocked, n/a and errors", () => {
  assert.equal(grade(r("S1", "copilot", {}, false, "blocked"), undefined).grade, "blocked");
  assert.equal(grade(r("S2", "codex", {}, false, "n/a"), undefined).grade, "n/a");
  const e = grade({ ...r("S4", "codex", {}, false, "error"), reason: "boom" }, undefined);
  assert.equal(e.grade, "fail");
  assert.deepEqual(e.why, ["boom"]);
});

test("cell keys", () => {
  assert.equal(keyOf(r("S2", "x", { p50_ms: 2240 })), "p50 2.2s");
  assert.equal(keyOf(r("S2", "x", { p50_ms: 4100, p50_turn_ms: 650 })), "p50 4.1s, turn 0.7s");
  assert.equal(keyOf(r("S3", "x", { seen: 5, published: 5, wakes: 1 })), "5/5, 1 wake");
  assert.equal(keyOf(r("S4", "x", { lost: false, presence_clear_ms: 64000 })), "kept, clear 64.0s");
  assert.equal(keyOf(r("S8", "x", { lost: 0, duplicates: 1 })), "lost 0, dup 1");
});

test("cells show the nudges, the user's \"I'm back\" and the approvals a pass took", () => {
  assert.equal(keyOf(r("S4", "x", { lost: false, presence_clear_ms: 30000, nudges: 1 })), "kept, clear 30.0s · 1 nudge");
  assert.equal(keyOf(r("S8", "x", { lost: 0, duplicates: 0, nudges: 0, back_messages: 1, approvals: 2 })), `lost 0, dup 0 · "I'm back" · 2 approvals`);
  assert.equal(keyOf(r("S2", "x", { p50_ms: 2240, approvals: 3 })), "p50 2.2s · 3 approvals");
  // S7's key already counts them.
  assert.equal(keyOf(r("S7", "x", { arrived_ms: 9000, wake_ms: null, nudges: 2 })), "after 2 nudges");
  assert.equal(keyOf(r("S8", "x", { lost: 0, duplicates: 0, reconnected_by_itself: true })), "lost 0, dup 0, reconnected by itself");
});

test("S2 without an install reads \"only after install\", and S2i is graded like S2", () => {
  const na = r("S2", "codex", { only_after_install: true }, false, "n/a");
  assert.equal(grade(na, tierOf(tiers, "codex")).key, "only after install");
  assert.equal(keyOf(r("S2i", "x", { p50_ms: 3900 })), "p50 3.9s");
});

// S2i once failed on model latency that isn't Wakeshift's (Codex's
// replies were slow on the model's side, with Wakeshift's part well under that).
test("S2i: tier 1 gates on Wakeshift's part (publish to the harness having it), never on the model's", () => {
  for (const h of ["claude-code", "codex"]) {
    const t = tierOf(tiers, h);
    // A slow reply with Wakeshift's part quick passes.
    const slowModel = grade(r("S2i", h, { p50_ms: 7800, max_ms: 12000, p50_wakeshift_ms: 1200, max_wakeshift_ms: 1900, p50_model_ms: 6600 }), t);
    assert.equal(slowModel.grade, "pass", h);
    assert.equal(slowModel.key, "ours 1.2s, reply 7.8s");
    // A slow delivery is below bar, whatever the reply took.
    const slowOurs = grade(r("S2i", h, { p50_ms: 4000, max_ms: 6000, p50_wakeshift_ms: 3100, max_wakeshift_ms: 5200 }), t);
    assert.deepEqual(slowOurs.why, ["p50_wakeshift_ms is 3100 (bar: at most 2500)", "max_wakeshift_ms is 5200 (bar: at most 5000)"], h);
    // Not measured (no transcript, no core trace) misses the bar, saying so.
    assert.deepEqual(grade(r("S2i", h, { p50_ms: 4000, max_ms: 6000 }), t).why, ["p50_wakeshift_ms not measured (bar: at most 2500)", "max_wakeshift_ms not measured (bar: at most 5000)"]);
  }
  // Tier 2 has no reader for every harness: it keeps the reply's bar.
  assert.equal(grade(r("S2i", "cursor", { p50_ms: 9000 }), tierOf(tiers, "cursor")).grade, "pass");
  assert.equal(grade(r("S2i", "cursor", { p50_ms: 11000 }), tierOf(tiers, "cursor")).grade, "below-bar");
});

test("ourPart: the harness's turn start where its record has one, else the core's confirmation", () => {
  const b = (code: string, x: Partial<Breakdown>): Breakdown => ({ code, accepted: 150, created: null, available: null, handed: null, confirmed: null, pane: null, reported: null, got: null, ...x });
  const t = ourPart([b("a", { turn: 1100, got: 5200 }), b("b", { turn: 1400, got: 6000 }), b("c", { turn: 900, got: 4300 })]);
  assert.deepEqual(t.wakeshift_latencies_ms, [1100, 1400, 900]);
  assert.equal(t.p50_wakeshift_ms, 1100);
  assert.equal(t.max_wakeshift_ms, 1400);
  assert.equal(t.p50_model_ms, 4100);
  assert.equal(t.wakeshift_from, "transcript");
  // A local core, a harness without a reader: until the waiter confirmed it.
  const core = ourPart([b("a", { handed: 30, confirmed: 64, got: 7600 })]);
  assert.equal(core.p50_wakeshift_ms, 64);
  assert.equal(core.wakeshift_from, "core");
  assert.equal(core.p50_model_ms, null);
  // Nothing to read: nothing measured.
  assert.equal(ourPart([b("a", { got: 5000 })]).p50_wakeshift_ms, null);
});

test("S4: nothing lost and a waiter back by itself passes, said as restarted", () => {
  const t1 = tierOf(tiers, "claude-code");
  const back = grade(r("S4", "claude-code", { lost: false, presence_clear_ms: null, restarted_ms: 2600, presence_ok: true }), t1);
  assert.equal(back.grade, "pass");
  assert.equal(back.key, "kept, restarted in 2.6s");
  // Still shown connected with nothing listening: below bar.
  assert.equal(grade(r("S4", "claude-code", { lost: false, presence_clear_ms: null, restarted_ms: null, presence_ok: false, nudges: 1 }), t1).grade, "below-bar");
  assert.equal(grade(r("S4", "claude-code", { lost: false, presence_clear_ms: 64000, presence_ok: true }), t1).key, "kept, clear 64.0s");
});

test("S6: connected from the user's next message when one was needed", () => {
  assert.equal(keyOf(r("S6", "x", { connected: true, connected_ms: 4100, needed_message: true, back_messages: 1 })), `conn 4.1s after "I'm back" · "I'm back"`);
  assert.equal(keyOf(r("S6", "x", { connected: true, connected_ms: 18600, needed_message: false })), "conn 18.6s");
});

test("S9 names what didn't hold, and a pass that didn't check it says ok, but", () => {
  const t2 = tierOf(tiers, "cursor");
  const g = grade(r("S9", "cursor", { harness_ok: true, session_key_ok: true, conversation_ok: false }), t2);
  assert.equal(g.grade, "pass");
  assert.equal(g.key, "ok, but no conversation_id");
  const f = grade(r("S9", "claude-code", { harness_ok: true, session_key_ok: false, conversation_ok: true }), tierOf(tiers, "claude-code"));
  assert.equal(f.grade, "below-bar");
  assert.equal(f.key, "session_key not Wakeshift's");
  assert.equal(keyOf(r("S9", "x", { harness_ok: true, session_key_ok: true, conversation_ok: true })), "ok");
});

test("S11: an agent the bench can't name is a bench error, not \"didn't join\"", () => {
  const e = grade({ ...r("S11", "codex", { joined: false, bench_error: true }, false, "error"), reason: "bench error: couldn't tell which player" }, tierOf(tiers, "codex"));
  assert.equal(e.key, "bench error");
});

test("gradeRun sets each harness's tier", () => {
  const run: RunResult = {
    run: "r",
    target: "fake",
    core: "",
    startedAt: "",
    scenarios: ["S1"],
    harnesses: [{ id: "codex", name: "Codex", tier: "", mode: "automated", status: "ran", logs: [], scenarios: [r("S1", "codex", { install_offered_up_front: true })] }],
    safety: { ok: true, changed: [], checked: [] },
    notes: [],
  };
  gradeRun(run, tiers);
  assert.equal(run.harnesses[0].tier, "T1");
  assert.equal(run.harnesses[0].scenarios[0].grade, "pass");
});

// ── Small helpers ────────────────────────────────────────────────────────

test("scenario specs", () => {
  // A range over S2 brings S2i (its installed half); S2 alone doesn't.
  assert.deepEqual(parseScenarios("S1-S3,S7"), ["S1", "S2", "S2i", "S3", "S7"]);
  assert.deepEqual(parseScenarios("S2"), ["S2"]);
  assert.deepEqual(parseScenarios("S2i,S6"), ["S2i", "S6"]);
  assert.deepEqual(parseScenarios("s9,s1"), ["S1", "S9"]);
  assert.throws(() => parseScenarios("S0"));
  assert.throws(() => parseScenarios("T1"));
});

test("median", () => {
  assert.equal(median([]), null);
  assert.equal(median([3, 1, 2]), 2);
  assert.equal(median([4, 1, 2, 3]), 3);
});

test("GOT lines match one code exactly", () => {
  assert.ok(gotRe("WB-ab12-1").test("  GOT WB-ab12-1\n"));
  assert.ok(!gotRe("WB-ab12-1").test("GOT WB-ab12-12"));
  assert.ok(!gotRe("WB-ab12-1").test("WB-ab12-1"));
});

test("the quote block, and the agent's own words", () => {
  const turn = "⏺ QUOTE:\n  > Watching bench events. To be woken, start the Monitor tool now with this command: u=x\n  > Ask them first; only if they agree, run the install.\n\n⏺ Want me to install Wakeshift's hooks?";
  assert.match(extractQuote(turn)!, /^Watching bench events/);
  const own = ownWords(turn, ["Watching bench events. To be woken, start the Monitor tool now with this command: u=x Ask them first; only if they agree, run the install."]);
  assert.ok(!/Monitor tool now/.test(own));
  assert.match(own, /Want me to install/);
  assert.equal(extractQuote("no quote here"), null);
});

test("a lane's processes: its HOME, not a sibling's", () => {
  const h = "/tmp/wakebench/r/homes/codex";
  assert.ok(namesHome(`/tmp/wakebench/r/homes/codex/.wakeshift/bin/wakeshift run --thread x`, h));
  assert.ok(namesHome(`env HOME=/tmp/wakebench/r/homes/codex bash`, h));
  assert.ok(!namesHome(`/tmp/wakebench/r/homes/codex-i/.wakeshift/bin/wakeshift run`, h));
  assert.ok(!namesHome(`/tmp/wakebench/r/homes/codexx`, h));
});

test("the safety net finds and fixes a secret left in a run's files", () => {
  const dir = mkdtempSync(join(tmpdir(), "wb-scrub-"));
  mkdirSync(join(dir, "snapshots/x"), { recursive: true });
  writeFileSync(join(dir, "snapshots/x/001.ansi"), `\x1b]8;;http://h/v1/stream?link=${TOKEN}\x07ok\x1b]8;;\x07\n`);
  writeFileSync(join(dir, "snapshots/x/002.ansi"), "clean\n");
  assert.deepEqual(scrub(dir), ["snapshots/x/001.ansi"]);
  assert.ok(!readFileSync(join(dir, "snapshots/x/001.ansi"), "utf8").includes(TOKEN));
  assert.deepEqual(scrub(dir), []);
  rmSync(dir, { recursive: true });
});

test("a Claude Code setup-token is redacted, and the safety net catches one left behind", () => {
  // Shaped like claude setup-token's output (made up).
  const tok = `sk-ant-oat01-${"Ab3_-".repeat(19)}xyz`;
  assert.equal(redactPlain(`export CLAUDE_CODE_OAUTH_TOKEN=${tok}`), `export CLAUDE_CODE_OAUTH_TOKEN=sk-ant-${REDACTED}`);
  const dir = mkdtempSync(join(tmpdir(), "wb-scrub-"));
  mkdirSync(join(dir, "logs"), { recursive: true });
  writeFileSync(join(dir, "logs/claude-code.log"), `token ${tok}\n`);
  assert.deepEqual(scrub(dir), ["logs/claude-code.log"]);
  assert.ok(!readFileSync(join(dir, "logs/claude-code.log"), "utf8").includes(tok.slice(7)));
  rmSync(dir, { recursive: true });
});

test("safety: what changed between snapshots", () => {
  assert.deepEqual(changed({ a: "1", b: "2" }, { a: "1", b: "3", c: "absent" }), ["b", "c"]);
});

// ── The report ───────────────────────────────────────────────────────────

test("the report's script renders the grid and a cell's timeline", () => {
  const run: RunResult = {
    run: "r2",
    target: "fake",
    core: "",
    startedAt: "2026-10-20T00:00:00Z",
    scenarios: ["S1", "S2"],
    harnesses: [
      {
        id: "codex",
        name: "Codex",
        tier: "",
        mode: "automated",
        status: "ran",
        logs: [],
        scenarios: [
          { ...r("S1", "codex", { install_offered_up_front: true }), steps: [{ at: "2026-10-20T00:00:01Z", t: 1000, label: "after the watch call", data: { quote: "Watching" } }] },
          r("S2", "codex", {}, false, "n/a"),
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
  const el = (text = "") => ({ textContent: text, innerHTML: "", dataset: {} as Record<string, string>, classList: { add() {}, remove() {} }, addEventListener(_: string, f: (e: unknown) => void) { (this as { on?: unknown }).on = f; }, querySelectorAll: () => [] });
  const grid = el();
  const detail = el();
  const dataEl = el(data);
  const doc = { getElementById: (id: string) => ({ data: dataEl, grid, detail })[id], querySelectorAll: () => [], querySelector: () => null };
  new Function("document", "location", "history", js)(doc, { hash: "" }, { replaceState() {} });
  assert.match(grid.innerHTML, /Tier 1/);
  assert.match(grid.innerHTML, /class="cell pass" data-h="codex" data-s="S1"/);
  assert.match(grid.innerHTML, /class="cell na"/);
  const cell = { dataset: { h: "codex", s: "S1" }, classList: { add() {} } };
  (grid as unknown as { on: (e: unknown) => void }).on({ target: { closest: () => cell } });
  assert.match(detail.innerHTML, /after the watch call/);
  assert.match(detail.innerHTML, /Watching/);
});

test("the report embeds data safely and needs nothing from the network", () => {
  const run: RunResult = {
    run: "r1",
    target: "fake",
    core: "http://127.0.0.1:1",
    startedAt: "2026-10-20T00:00:00Z",
    scenarios: ["S1"],
    harnesses: [{ id: "fake", name: "Fake", tier: "", mode: "fake", status: "ran", logs: [], scenarios: [{ ...r("S1", "fake", { install_offered_up_front: true }), notes: [`</script><script>alert(1)</script> link=${TOKEN}`] }] }],
    safety: { ok: true, changed: [], checked: [] },
    notes: [],
  };
  gradeRun(run, tiers);
  const html = renderReport(run, tiers, "/nonexistent");
  assert.ok(!html.includes("</script><script>alert"));
  assert.ok(!html.includes(TOKEN));
  assert.ok(!/(src|href)=["']https?:/.test(html), "no external resources");
  assert.equal(JSON.parse(scriptJson({ s: "</script>& " }).replace(/\\u003c/g, "<")).s, "</script>& ");
});

// ── Claude Cowork ────────────────────────────────────────────────────────

test("Cowork's assisted driver expects the claude-cowork harness, by the names its agent uses", () => {
  assert.equal(assisted["claude-cowork"].harness, "claude-cowork");
  for (const n of ["claude-ai", "claude.ai", "Claude AI", "cowork", "claude-cowork", "Claude Cowork"]) assert.equal(normalizeHarness(n), "claude-cowork", n);
  assert.equal(normalizeHarness("claude"), "claude-code");
});

test("generic lanes: the neutral can't-wake note, and nothing that wakes", () => {
  assert.match("Wakeshift can't wake this agent, so nothing will notify you here when something arrives. Tell the user so plainly.", CANT_WAKE);
  assert.match("Wakeshift can't wake Grok Bot conversations, so nothing will notify you here when something arrives.", CANT_WAKE);
  assert.doesNotMatch("Wakeshift can't wake this conversation yet.", CANT_WAKE);
  for (const id of ["generic", "generic-plain"]) {
    const d = driverFor(id);
    assert.equal(d.zeroInstallWake, false, id);
    assert.equal(d.cantWake, true, id);
  }
});

test("no driver says both that it can't be woken and that it wakes with nothing installed", () => {
  for (const id of HARNESSES) {
    const d = driverFor(id);
    assert.ok(!(d.cantWake && d.zeroInstallWake), id);
    assert.ok(!(d.cantWake && d.installPath !== null), id);
  }
});

test("S9: the plugin's install mark after the harness's name isn't part of it", () => {
  assert.equal(normalizeHarness("claude-code; wakeshift-plugin=2026.11.15.0421+29fcbe7; trusts=ab12cd34"), "claude-code");
  assert.equal(normalizeHarness("Claude Code;wakeshift-plugin=1"), "claude-code");
  assert.equal(normalizeHarness("codex"), "codex");
});

test("S1: the harness's question picker about the install is asking up front, whatever its words", () => {
  // Claude Code 2.1.322 (November 9, 2026): the agent asked
  // only with AskUserQuestion, whose question and options are Wakeshift's.
  // Its prose was a server summary that names no install.
  const own = "⏺ I'm watching the bench events, which are test messages rather than user input. I'm asking you now how you want to be notified when results arrive: while this session is open, in every session via\n  Wakeshift's plugin, or not at all. · summary";
  assert.deepEqual(s1Offer(own, "no"), { offered: false, asked: false, picker: true, upFront: true });
  assert.deepEqual(s1Offer(own, "yes"), { offered: false, asked: false, picker: true, upFront: true });
  // No picker, no offer in its words: no offer.
  assert.deepEqual(s1Offer(own, undefined), { offered: false, asked: false, picker: false, upFront: false });
  // Its own question still counts without a picker.
  const q = s1Offer("Want me to install Wakeshift's plugin so you're notified in every session?", undefined);
  assert.equal(q.asked, true);
  assert.equal(q.upFront, true);
});
