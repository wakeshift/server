// What users see and keep: credentials in dialogs and saved
// rules, Wakeshift-internal IDs, and what an uninstall leaves.

import { strict as assert } from "node:assert";
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { test } from "node:test";
import { grade, keyOf } from "../src/grade.js";
import type { ScenarioResult } from "../src/model.js";
import { credentialKinds } from "../src/redact.js";
import { applyExposures, documentedLeftover, internalIds, loggedDeliveries, savedRuleCredentials, uninstallLeftovers, wakeshiftInventory } from "../src/visible.js";

// Made-up values in the shapes Wakeshift uses: none is a real credential.
const TOKEN = "Zk3mQ9rT2vXw8yB1nC4dE6fG7hJ0kL5pS";
const LINK = `https://app.wakeshift.ai/v1/stream/wait?link=${TOKEN}`;
const SESSION = `wsc_${TOKEN}`;
const JWS = "eyJhbGciOiJFZERTQSJ9.eyJzdWIiOiJ4In0.c2lnbmF0dXJlLXZhbHVl";

function home(files: Record<string, string>): string {
  const dir = mkdtempSync(join(tmpdir(), "wb-visible-"));
  for (const [rel, text] of Object.entries(files)) {
    mkdirSync(dirname(join(dir, rel)), { recursive: true });
    writeFileSync(join(dir, rel), text);
  }
  return dir;
}

test("credentialKinds: Wakeshift's credentials by kind, a wrapped one too; redacted text and placeholders don't count", () => {
  // Cursor's approval for the inline loop, as the core once served it.
  const cursor = [" Run this command?", ` $ u='${LINK}&until=wait&timeout=50s'; c=; while :; do r=$(curl -s -m 60 "$u")`, " → Run (once) (y)", "   Run everything (shift+tab)", "   Skip (esc or n)"].join("\n");
  assert.deepEqual(credentialKinds(cursor), ["stream or wait link"]);
  assert.deepEqual(credentialKinds(`call watch with session_key="${SESSION}"`), ["session key (wsc_)"]);
  assert.deepEqual(credentialKinds(`wakeshift-beacon: ${JWS}`), ["beacon", "JWS"]);
  assert.deepEqual(credentialKinds(`POST https://app.wakeshift.ai/v1/publish/${TOKEN}`), ["publish URL"]);
  assert.deepEqual(credentialKinds(`key wst_${TOKEN} and voucher wsv_${TOKEN}`), ["subscribe voucher (wsv_)", "app key (wst_)"]);
  // A link a TUI wrapped inside its border.
  assert.deepEqual(credentialKinds(`│ u='https://app.wakeshift.ai/v1/stream/wait?link=Zk3m │\n│ Q9rT2vXw8yB1nC4dE6fG7hJ0kL5pS&until=wait' │`), ["stream or wait link"]);
  // Not credentials: what redaction already took out, the link file's path, placeholders, a bench code.
  assert.deepEqual(credentialKinds("u='https://app.wakeshift.ai/v1/stream/wait?link=‹redacted›' wsc_‹redacted›"), []);
  assert.deepEqual(credentialKinds("f=$HOME/.wakeshift/links/0123456789abcdef; u=$(head -n1 \"$f\")"), []);
  assert.deepEqual(credentialKinds("pass session_key (wsc_…) when you call watch again; GOT WB-8c8a-1"), []);
  // Cursor's command now: a one-time link claim, spent once the command has run.
  assert.deepEqual(credentialKinds(`m=$(curl -s -m 30 -d 'token=wso_0f1e2d3c-0000-4000-8000-000000000000_${TOKEN}' --data-urlencode "conversation=$CURSOR_CONVERSATION_ID" 'https://app.wakeshift.ai/v1/stream/claim')`), []);
  // A Monitor that reads its link from its file names a shell variable, not a link (v1.4.57 follow-up: the Agent SDK lane's Monitor).
  assert.deepEqual(credentialKinds(`t=$(cat "$HOME/.wakeshift/links/2973a3678111d3d6"); curl -sgN "https://app.wakeshift.ai/v1/stream/wait?loop=$i&link=\${t#*link=}"`), []);
  // ANSI escapes inside don't hide one.
  assert.deepEqual(credentialKinds(`\x1b[1m?link=\x1b[0m${TOKEN}`), ["stream or wait link"]);
});

test("savedRuleCredentials: a rule a harness saved with a link in it; other files and clean rules don't count", () => {
  const dir = home({
    // agy's "always allow" once saved the whole command.
    ".gemini/antigravity-cli/settings.json": JSON.stringify({ permissions: { allow: [`run_command(u='${LINK}'; while :; do curl -s "$u"; done)`] } }),
    ".claude/settings.json": JSON.stringify({ permissions: { allow: ["Bash(~/.wakeshift/bin/wakeshift wait:*)"] } }),
    ".codex/rules/default.rules": `prefix_rule(pattern=["curl", "${LINK}"], decision="allow")`,
    // The link file is where the link is kept on purpose: not a rule.
    ".wakeshift/links/0123456789abcdef": LINK,
    "work/notes.txt": LINK,
  });
  try {
    assert.deepEqual(savedRuleCredentials(dir), [
      { file: ".gemini/antigravity-cli/settings.json", kinds: ["stream or wait link"] },
      { file: ".codex/rules/default.rules", kinds: ["stream or wait link"] },
    ]);
    // The lane's own MCP token, masked, isn't Wakeshift's.
    assert.deepEqual(savedRuleCredentials(dir, (s) => s.split(LINK).join("‹token›")), []);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test("applyExposures: a credential in the scenario's time fails it, saying where and what kind", () => {
  const r: ScenarioResult = { id: "S2", harness: "cursor", status: "ran", works: true, metrics: { p50_ms: 7800 }, steps: [], notes: [] };
  const ex = [
    { at: "2026-11-10T10:00:05.000Z", where: "a dialog (approved the background command once)", kinds: ["stream or wait link"] },
    { at: "2026-11-10T11:00:00.000Z", where: "a saved rule (~/.cursor/cli-config.json)", kinds: ["stream or wait link"] },
  ];
  applyExposures(r, ex, (at) => at < "2026-11-10T10:30:00.000Z");
  assert.equal(r.works, false);
  assert.equal(r.metrics.credentials_shown, 1);
  assert.equal(r.reason, "a Wakeshift credential in a dialog (approved the background command once) (stream or wait link)");
  const g = grade(r, undefined);
  assert.equal(g.grade, "fail");
  assert.match(keyOf(g), /a credential shown/);
  // None in its time: unchanged.
  const ok: ScenarioResult = { id: "S2", harness: "cursor", status: "ran", works: true, metrics: {}, steps: [], notes: [] };
  applyExposures(ok, ex, () => false);
  assert.equal(ok.works, true);
  assert.equal(ok.metrics.credentials_shown, 0);
});

test("internalIds: Wakeshift's prefixed IDs, and the IDs the bench knows for the lane; codes and other UUIDs don't count", () => {
  const sub = "0b1c2d3e-4f50-4a6b-8c7d-9e0f1a2b3c4d";
  const known = [{ id: sub, kind: "subscription ID" }, ...loggedDeliveries('time=… msg="codex: delivered" delivery=04b6ae8f-93e8-8b3d-8591-1d1616ffc88e batch=1').map((id) => ({ id, kind: "delivery ID" }))];
  assert.deepEqual(known.map((k) => k.id), [sub, "04b6ae8f-93e8-8b3d-8591-1d1616ffc88e"]);
  assert.deepEqual(internalIds(`Subscribed (subscription ${sub.toUpperCase()}).`, known), ["subscription ID"]);
  assert.deepEqual(internalIds("Woken by delivery 04b6ae8f-93e8-8b3d-8591-1d1616ffc88e.", known), ["delivery ID"]);
  assert.deepEqual(internalIds("subscription sub_0123456789abcdef01234567 is live", known), ["subscription ID (sub_)"]);
  assert.deepEqual(internalIds("conv_abcdefgh234567ab and wsn_0123456789abcdef0123456789abcdef", known), ["conversation ID (conv_)", "server name (wsn_)"]);
  // A harness's own session UUID, a bench code, the session key the agent is meant to pass.
  assert.deepEqual(internalIds("session: 01a1c6ae-b01a-78a2-815d-12fc224eda5a · GOT WB-8c8a-1 · session_key wsc_abc", known), []);
});

test("internal IDs shown: below bar in any tier, with the count in the cell", () => {
  const r: ScenarioResult = { id: "S7", harness: "antigravity", status: "ran", works: true, metrics: { nudges: 0, wake_ms: 9000, internal_ids_shown: 2 }, steps: [], notes: [] };
  const g = grade(r, undefined);
  assert.equal(g.grade, "below-bar");
  assert.deepEqual(g.why, ["showed 2 Wakeshift-internal ID(s) (bar: none; the notes say which and where)"]);
  assert.match(g.key!, /2 internal IDs/);
});

test("uninstall inventory: what's left of Wakeshift's, against what the docs say stays", () => {
  const dir = home({
    ".wakeshift/links/0123456789abcdef": "x",
    ".wakeshift/bin/wakeshift": "#!/bin/sh",
    ".claude/plugins/cache/wakeshift/wakeshift/1.0/plugin.json": "{}",
    ".claude/plugins/known_marketplaces.json": "{}",
    ".claude/settings.json": JSON.stringify({ hooks: { Stop: [{ hooks: [{ command: "~/.wakeshift/bin/wakeshift hook stop" }] }] } }),
    ".claude/settings.json.before-wakeshift-20261109-170548": "{}",
    ".claude/projects/-work/abc.jsonl": "Wakeshift's text, as the model saw it",
    ".codex/config.toml": '[hooks.state."wakeshift@wakeshift:hooks/hooks.json"]\ntrusted = true\n',
    ".copilot/settings.json": JSON.stringify({ enabledPlugins: { "wakeshift@wakeshift": false } }),
    "work/README.md": "wakeshift",
  });
  try {
    assert.deepEqual(wakeshiftInventory(dir), [
      ".claude/plugins/cache/wakeshift/",
      ".claude/settings.json",
      ".claude/settings.json.before-wakeshift-20261109-170548",
      ".codex/config.toml",
      ".copilot/settings.json",
      ".wakeshift/",
    ]);
    const cc = uninstallLeftovers(dir, "claude-code");
    assert.deepEqual(cc.documented.map((d) => d.path), [".claude/plugins/cache/wakeshift/", ".claude/settings.json.before-wakeshift-20261109-170548"]);
    // A hooks entry left in settings.json, and ~/.wakeshift itself: not what the docs say stays.
    assert.deepEqual(cc.undocumented, [".claude/settings.json", ".codex/config.toml", ".copilot/settings.json", ".wakeshift/"]);
    // Codex's hooks.state entry and Copilot's off flag are documented for those harnesses only.
    assert.ok(documentedLeftover("codex", ".codex/config.toml", '[hooks.state."wakeshift@wakeshift:x"]'));
    assert.equal(documentedLeftover("codex", ".codex/config.toml", '[marketplaces.wakeshift]\nsource = "x"'), null);
    assert.ok(documentedLeftover("copilot", ".copilot/settings.json", '{"wakeshift@wakeshift": false}'));
    assert.equal(documentedLeftover("copilot", ".copilot/settings.json", '{"wakeshift@wakeshift": true}'), null);
    assert.ok(documentedLeftover("pi", ".wakeshift/pi/"));
    assert.equal(documentedLeftover("cursor", ".wakeshift/"), null);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});
