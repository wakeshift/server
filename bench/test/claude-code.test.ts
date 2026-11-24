// Claude Code's driver: the install picker (AskUserQuestion) and the "!"
// fallback when auto mode refuses the installer.

import { strict as assert } from "node:assert";
import { test } from "node:test";
import { chmodSync, mkdtempSync, realpathSync, rmSync, symlinkSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { askedToType, CLAUDE_OPUS, CLAUDE_SONNET, CLAUDE_TOKEN_ENV, claudeModel, claudePermissionMode, claudeTokenFile, ClaudeCode, inputEmpty, installPicker, modelMatches, notLoggedIn, parseRefusals, shownModel, tokenExport } from "../drivers/claude-code/index.js";
import { q } from "../src/home.js";
import type { Lane } from "../src/lane.js";

// As Claude Code 2.1.280 draws it (from a bench run).
const PICKER = `⏺ Monitor(Wakebench bench events)
────────────────────────────────────────
 ☐ Hooks

│ Do you want to install Wakeshift's hooks for Claude Code so bench events are delivered without the Monitor?

❯ 1. Yes, install Wakeshift's hooks for Claude Code
     runs \`curl -fsSL http://localhost:61339/install/claude-code.sh | sh\`, adds three hooks to ~/.claude/settings.json, keeping a backup
  2. No, don't install them
     messages still wake this conversation through the Monitor
  3. Type something.
────────────────────────────────────────
  4. Chat about this

Enter to select · ↑/↓ to navigate · Esc to cancel`;

test("the install picker's yes and no", () => {
  assert.deepEqual(installPicker(PICKER), { yes: 1, no: 2 });
  const swapped = PICKER.replace("❯ 1. Yes, install Wakeshift's hooks for Claude Code", "❯ 1. No, don't install them").replace("  2. No, don't install them", "  2. Yes, install the hooks");
  assert.deepEqual(installPicker(swapped), { yes: 2, no: 1 });
  assert.deepEqual(installPicker("Review your answers\n ● Hooks → Yes\nReady to submit your answers?\n❯ 1. Submit answers\n  2. Cancel"), { submit: 1 });
});

test("not a picker: the installer's permission dialog, or options quoted in the transcript", () => {
  const perm = [
    "Bash command",
    "  curl -fsSL http://localhost:61339/install/claude-code.sh | sh",
    "Do you want to proceed?",
    "❯ 1. Yes",
    "  2. Yes, and don't ask again for curl -fsSL http://localhost:61339/install/claude-code.sh commands",
    "  3. No, and tell Claude what to do differently (esc)",
    "Esc to cancel",
  ].join("\n");
  assert.equal(installPicker(perm), null);
  assert.equal(installPicker(PICKER.replace(/Enter to select.*$/, "? for shortcuts")), null);
  assert.equal(installPicker(PICKER.replace(/  2\. No, don't install them\n/, "")), null);
});

test("a three-way notification picker: install, this session only, or no", () => {
  const p = installPicker(
    "Shipyard can notify you here when build #413 finishes. How should I set that up?\n❯ 1. Notify me while this session is open\n     starts a background listener for this session only; nothing is installed\n  2. Notify me in every session\n     installs Wakeshift's plugin\n  3. Don't notify me\n  4. Type something.\nEnter to select · ↑/↓ to navigate · Esc to cancel",
  );
  assert.deepEqual(p, { yes: 2, no: 3, session: 1 });
});

test("the agent asking the user to type the installer with !", () => {
  const cmd = "curl -fsSL http://localhost:61339/install/claude-code.sh | sh";
  assert.ok(askedToType(`⏺ Auto mode refused it. Please run it yourself by typing:\n  ! ${cmd}\n  then I'll call watch again.`));
  assert.ok(askedToType("⏺ Type `! curl -fsSL http://localhost:61339/install/\nclaude-code.sh | sh` in the prompt."));
  assert.ok(!askedToType(`⏺ Running \`${cmd}\` now.`));
  assert.ok(!askedToType(`⏺ Bash(${cmd})\n  ⎿ installed`));
  // Wakeshift's plugin, with Claude Code's own installer (auto mode refused the agent's run).
  const plugin = "claude plugin install wakeshift --marketplace wakeshift/agent-plugins#main";
  assert.ok(askedToType(`⏺ The plugin install was blocked by auto mode.\n  If you still want it, type this in the prompt so it runs as you:\n  ! ${plugin}`));
  assert.ok(!askedToType(`⏺ Bash(${plugin})\n  ⎿ Blocked by auto mode`));
  // Asked in a sentence, the ! after a word ("type ! claude …").
  assert.ok(askedToType(`  - Install it yourself: type ! ${plugin.slice(0, 60)}\n    ${plugin.slice(60)}. Then run /reload-plugins`));
});

test("a login that didn't carry over is recognized from the status line", () => {
  assert.ok(notLoggedIn("❯ \n──────\n ⏵⏵ auto mode on (shift+tab to cycle)   Not logged in · Run /login"));
  assert.ok(!notLoggedIn("❯ \n──────\n ⏵⏵ auto mode on (shift+tab to cycle)"));
});

test("the first-start fullscreen renderer offer is declined (Claude Code 2.1.280)", async () => {
  const keys: string[] = [];
  const lane = { session: "s", state: {}, tmux: { keys: async (_s: string, k: string) => void keys.push(k), type: async () => {} } } as unknown as Lane;
  const screen = ["Try the new fullscreen renderer?", " · Flicker-free output", "❯ 1. Yes, try it", "  2. Not now", "Enter to confirm · Esc to cancel"].join("\n");
  assert.equal(await new ClaudeCode().answer(lane, screen, { allowInstall: false }), "declined the fullscreen renderer");
  assert.deepEqual(keys, ["2"]);
});

test("the bench's Claude Code token: its file if present, else the keychain (null)", () => {
  const dir = realpathSync(mkdtempSync(join(tmpdir(), "wb-token-")));
  try {
    // No file in the state dir: fall back.
    assert.equal(claudeTokenFile({ WB_STATE_DIR: dir }), null);
    // An empty file doesn't count.
    writeFileSync(join(dir, "claude-token"), "");
    assert.equal(claudeTokenFile({ WB_STATE_DIR: dir }), null);
    writeFileSync(join(dir, "claude-token"), "sk-ant-oat01-made-up\n", { mode: 0o600 });
    assert.equal(claudeTokenFile({ WB_STATE_DIR: dir }), join(dir, "claude-token"));
    // Readable by others, in a directory open to them: refused.
    const open = mkdtempSync(join(tmpdir(), "wb-token-open-"));
    chmodSync(open, 0o755);
    writeFileSync(join(open, "t"), "sk-ant-oat01-made-up\n", { mode: 0o644 });
    assert.throws(() => claudeTokenFile({ WB_CLAUDE_TOKEN_FILE: join(open, "t") }), /chmod 600/);
    // A link in a private directory to that file: the file's own directory counts.
    symlinkSync(join(open, "t"), join(dir, "link"));
    assert.throws(() => claudeTokenFile({ WB_CLAUDE_TOKEN_FILE: join(dir, "link") }), /chmod 600/);
    // The same file mode inside a 0700 directory is fine.
    chmodSync(dir, 0o700);
    writeFileSync(join(dir, "loose"), "sk-ant-oat01-made-up\n", { mode: 0o644 });
    assert.equal(claudeTokenFile({ WB_CLAUDE_TOKEN_FILE: join(dir, "loose") }), join(dir, "loose"));
    rmSync(open, { recursive: true });
    // WB_CLAUDE_TOKEN_FILE wins; a missing one falls back too.
    assert.equal(claudeTokenFile({ WB_STATE_DIR: dir, WB_CLAUDE_TOKEN_FILE: join(dir, "nope") }), null);
    // The pane reads the file itself: only its path is typed, never the token.
    const line = tokenExport(join(dir, "claude-token"));
    assert.equal(line, `export ${CLAUDE_TOKEN_ENV}="$(cat ${q(join(dir, "claude-token"))})"`);
    assert.equal(tokenExport("/a b/claude-token"), `export ${CLAUDE_TOKEN_ENV}="$(cat '/a b/claude-token')"`);
    assert.ok(!line.includes("made-up"));
  } finally {
    rmSync(dir, { recursive: true });
  }
});

test("the input box counts as empty with only Claude Code's dim suggestion in it", () => {
  const box = (line: string) => ["⏺ Please run it yourself by typing:", "  ! curl -fsSL https://x/install/claude-code.sh | sh", "────────", line, "────────", "  ⏵⏵ auto mode on · 1 monitor · ↓ to manage"].join("\n");
  assert.ok(inputEmpty(box("❯ ")));
  // 2.1.312 as captured: ESC[39m❯ ESC[2mI ran the install myselfESC[0m
  assert.ok(inputEmpty(box("\x1b[39m❯ \x1b[2mI ran the install myself\x1b[0m")));
  // 2.1.316 as captured: the dim run isn't closed on its line.
  assert.ok(inputEmpty(box("\x1b[39m❯ \x1b[2mcheck build 413")));
  assert.ok(!inputEmpty(box("\x1b[39m❯ ok, I'm back")));
  assert.ok(!inputEmpty(box("❯ 1. Yes")));
  assert.ok(!inputEmpty("no box here"));
  // The tools variant of the installer counts as asked too.
  assert.ok(askedToType("⏺ Type this yourself:\n  ! curl -fsSL https://x/install/claude-code.sh | WAKESHIFT_ALLOW_TOOLS=agentchat:post,history sh"));
});

test("Claude Code lanes run in auto mode unless WB_CLAUDE_PERMISSION_MODE says otherwise", () => {
  assert.equal(claudePermissionMode({}), "auto");
  assert.equal(claudePermissionMode({ WB_CLAUDE_PERMISSION_MODE: "default" }), "default");
  assert.equal(claudePermissionMode({ WB_CLAUDE_PERMISSION_MODE: " manual " }), "manual");
  // The binary's own default: no flag.
  assert.equal(claudePermissionMode({ WB_CLAUDE_PERMISSION_MODE: "harness" }), null);
  assert.equal(claudePermissionMode({ WB_CLAUDE_PERMISSION_MODE: "" }), null);
});

test("Claude Code lanes run Sonnet 5.5 by its full ID unless WB_CLAUDE_MODEL says otherwise", () => {
  assert.equal(CLAUDE_SONNET, "claude-sonnet-5-5");
  assert.equal(CLAUDE_OPUS, "claude-opus-5-5");
  assert.equal(claudeModel({}), CLAUDE_SONNET);
  assert.equal(claudeModel({ WB_CLAUDE_MODEL: "" }), CLAUDE_SONNET);
  assert.equal(claudeModel({ WB_CLAUDE_MODEL: " claude-opus-5-5 " }), CLAUDE_OPUS);
  // The account's own default: no --model.
  assert.equal(claudeModel({ WB_CLAUDE_MODEL: "harness" }), null);
});

test("the start header's model, checked against the one asked for", () => {
  const header = (line: string) => ["bash-3.2$ claude --model x", " ▐▛███▜▌   Claude Code v2.1.319", `▝▜█████▛▘  ${line}`, "  ▘▘ ▝▝    /tmp/work", "", "❯ "].join("\n");
  assert.equal(shownModel(header("Sonnet 5.5 · Claude API")), "Sonnet 5.5");
  assert.equal(shownModel(header("Opus 5.5 (1M context) · Claude Max")), "Opus 5.5 (1M context)");
  assert.equal(shownModel(header("claude-sonnet-5-5 · Claude API")), "claude-sonnet-5-5");
  // The last header: a restart (claude --continue) draws another.
  assert.equal(shownModel(header("Opus 5.5 · Claude API") + "\n" + header("Sonnet 5.5 · Claude API")), "Sonnet 5.5");
  assert.equal(shownModel("no header here"), null);
  assert.equal(modelMatches(CLAUDE_SONNET, "Sonnet 5.5"), true);
  assert.equal(modelMatches(CLAUDE_OPUS, "Opus 5.5 (1M context)"), true);
  // A Claude Code that doesn't list the ID shows it as given.
  assert.equal(modelMatches(CLAUDE_SONNET, "claude-sonnet-5-5"), true);
  // Not the same model: another family, or Sonnet 5 (2.1.280's "sonnet").
  assert.equal(modelMatches(CLAUDE_SONNET, "Opus 5.5"), false);
  assert.equal(modelMatches(CLAUDE_SONNET, "Sonnet 5"), false);
  assert.equal(modelMatches("claude-sonnet-5", "Sonnet 5.5"), false);
  assert.equal(modelMatches("opus", "Opus 5.5"), true);
});

test("a manual-mode dialog's 'switch to auto mode' option is never chosen", async () => {
  const cases: [string[], string][] = [
    [["❯ 1. Yes", "  2. Yes, and switch to auto mode", "  3. No, and tell Claude what to do differently (esc)"], "1"],
    [["❯ 1. Yes, and switch to auto mode", "  2. Yes", "  3. No"], "2"],
  ];
  for (const [opts, want] of cases) {
    const keys: string[] = [];
    const lane = { session: "s", state: {}, prompted: () => {}, tmux: { keys: async (_s: string, k: string) => void keys.push(k), type: async () => {} } } as unknown as Lane;
    const screen = [" Bash command", "   date -u", ' Tip: auto mode handles these prompts for you — choose "switch to auto mode" below', " Do you want to proceed?", ...opts].join("\n");
    assert.match((await new ClaudeCode().answer(lane, screen, { allowInstall: false, scope: "session" }))!, /^approved a Bash command/);
    assert.deepEqual(keys, [want]);
  }
});

test("auto mode's refusals are read from the transcript, with the tool and the classifier's reason", () => {
  const line = (o: unknown) => JSON.stringify(o);
  const lines = [
    line({ type: "assistant", timestamp: "2026-10-29T06:30:00.000Z", message: { content: [{ type: "tool_use", id: "t1", name: "Bash", input: { command: "curl -fsSL https://x/install/claude-code.sh | sh" } }] } }),
    line({
      type: "user",
      timestamp: "2026-10-29T06:30:02.000Z",
      message: {
        content: [
          {
            type: "tool_result",
            tool_use_id: "t1",
            is_error: true,
            content: "Permission for this action was denied by the Claude Code auto mode classifier. Reason: Pipes a remote script into a shell. If you have other tasks that don't depend on this action, continue working on those.",
          },
        ],
      },
    }),
    line({ type: "assistant", timestamp: "2026-10-29T06:31:00.000Z", message: { content: [{ type: "tool_use", id: "t2", name: "mcp__wakebench__watch", input: {} }] } }),
    line({ type: "user", timestamp: "2026-10-29T06:31:01.000Z", message: { content: [{ type: "tool_result", tool_use_id: "t2", content: [{ type: "text", text: "Auto mode could not evaluate this action and is blocking it for safety" }] }] } }),
    line({ type: "assistant", timestamp: "2026-10-29T06:31:30.000Z", message: { content: [{ type: "tool_use", id: "t4", name: "Bash", input: { command: "curl -fsSL https://x.ai/install/claude-code.sh | sh" } }] } }),
    line({ type: "user", timestamp: "2026-10-29T06:31:31.000Z", message: { content: [{ type: "tool_result", tool_use_id: "t4", content: "Permission to use Bash with command curl -fsSL https://x.ai/install/claude-code.sh | sh has been denied. IMPORTANT: You *may* attempt…" }] } }),
    // An allowed call's result isn't one.
    line({ type: "user", timestamp: "2026-10-29T06:32:00.000Z", message: { content: [{ type: "tool_result", tool_use_id: "t3", content: "ok" }] } }),
    '{"type":"user","timest',
  ];
  assert.deepEqual(parseRefusals(lines), [
    { at: "2026-10-29T06:30:02.000Z", tool: "Bash(curl -fsSL https://x/install/claude-code.sh | sh)", reason: "Pipes a remote script into a shell" },
    { at: "2026-10-29T06:31:01.000Z", tool: "mcp__wakebench__watch", reason: "the classifier gave no verdict (blocked for safety)" },
    { at: "2026-10-29T06:31:31.000Z", tool: "Bash(curl -fsSL https://x.ai/install/claude-code.sh | sh)", reason: "a permission rule denied it" },
  ]);
});

test("the plugin offer before Claude Code 2.1.292: two steps, and S6 checks the version runs it", async () => {
  const { PLUGIN_OFFER, offerForm, offerFitsVersion, compareVersions } = await import("../drivers/claude-code/offer.js");
  const one = "claude plugin install wakeshift --marketplace wakeshift/agent-plugins";
  const two = "claude plugin marketplace add wakeshift/agent-plugins && claude plugin install wakeshift@wakeshift";
  assert.ok(PLUGIN_OFFER.test(one) && PLUGIN_OFFER.test(two));
  assert.equal(offerForm(one), "one-command");
  assert.equal(offerForm(two), "two-step");
  assert.equal(offerForm("curl -fsSL http://localhost:1/install/claude-code.sh | sh"), "");
  assert.equal(offerForm(null), "");
  // The offer the driver reads from the result, in either form.
  const d = new ClaudeCode();
  assert.equal(d.offeredInstall(`"Notify me in every session" (description: runs \`${two}\` (Wakeshift's plugin for Claude Code), which …`), two);
  assert.equal(d.offeredInstall(`runs \`${one}\` (Wakeshift's plugin`), one);
  // 2.1.280 (the floor) can't run --marketplace; 2.1.292 and later run both.
  assert.equal(offerFitsVersion(one, "2.1.280 (Claude Code)"), false);
  assert.equal(offerFitsVersion(one, "2.1.291"), false);
  assert.equal(offerFitsVersion(two, "2.1.280 (Claude Code)"), true);
  assert.equal(offerFitsVersion(one, "2.1.292 (Claude Code)"), true);
  assert.equal(offerFitsVersion(one, "2.2.0"), true);
  assert.equal(offerFitsVersion(one, null), null);
  assert.equal(offerFitsVersion(null, "2.1.280"), null);
  assert.ok(compareVersions("2.1.1000", "2.1.292") > 0 && compareVersions("2.1.292", "2.1.292") === 0 && compareVersions("2.1.29", "2.1.292") < 0);
  // The user typing the two steps with ! (auto mode refused the agent's run).
  assert.ok(askedToType(`⏺ Auto mode blocked it. Type this in the prompt:\n  ! ${two}`));
  assert.ok(!askedToType(`⏺ Bash(${two})\n  ⎿ Blocked by auto mode`));
});

test("S6 fails an offer this Claude Code can't run", async () => {
  const { install } = await import("../src/metrics.js");
  const base = { agent_ran_installer: true, trust_relayed: true, installed: true, connected_ms: 1000, wake_ms: 2000, harness_version: "2.1.280 (Claude Code)" };
  assert.equal(install({ ...base, offer_fits_version: true }).works, true);
  const bad = install({ ...base, offer_fits_version: false });
  assert.equal(bad.works, false);
  assert.match(bad.reason ?? "", /doesn't run on 2\.1\.280/);
  assert.equal(install({ ...base, offer_fits_version: null }).works, true);
});

test("WB_CLAUDE_PIN=floor runs the pinned oldest Claude Code from the bench's state", async () => {
  const { pinnedClaudeBin } = await import("../drivers/claude-code/index.js");
  const dir = mkdtempSync(join(tmpdir(), "wb-pins-"));
  try {
    const pins = join(dir, "pins.json");
    writeFileSync(pins, JSON.stringify({ "claude-code": "2.1.318", "claude-code-floor": "2.1.280" }));
    assert.equal(pinnedClaudeBin({}, pins), null);
    assert.equal(pinnedClaudeBin({ WB_CLAUDE_PIN: "floor", WB_STATE_DIR: dir }, pins), join(dir, "claude-code", "2.1.280", "claude"));
    writeFileSync(pins, JSON.stringify({ "claude-code": "2.1.318" }));
    assert.throws(() => pinnedClaudeBin({ WB_CLAUDE_PIN: "floor", WB_STATE_DIR: dir }, pins));
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
  // The repository's pins name a floor.
  assert.match(pinnedClaudeBin({ WB_CLAUDE_PIN: "floor", WB_STATE_DIR: "/s" }) ?? "", /^\/s\/claude-code\/\d+\.\d+\.\d+\/claude$/);
});

// tmux prints every row of the 50-row pane, so early in a conversation the
// capture ends in blank rows below Claude Code's status line. busy() read
// only those and said idle while `claude plugin install` was still
// "Waiting…" (R3: "not installed", then the agent said it had installed
// it).
test("busy while the status line says so, however many blank rows follow it", () => {
  const d = new ClaudeCode();
  const drawn = [
    "❯ Kick off a build of main and let me know when it's done.",
    "⏺ Bash(claude plugin install wakeshift --marketplace /tmp/x/core/agent-plugins)",
    "  ⎿  Waiting…",
    "✽ Nucleating… (17s · ↓ 885 tokens)",
    "❯ ",
    "  ⏵⏵ auto mode on (shift+tab to cycle) · esc to interrupt",
  ].join("\n");
  assert.equal(d.busy(drawn + "\n".repeat(20)), true);
  assert.equal(d.busy(drawn + "\n" + "   \n".repeat(20)), true);
  assert.equal(d.busy(drawn.replace(" · esc to interrupt", "") + "\n".repeat(20)), false);
});
