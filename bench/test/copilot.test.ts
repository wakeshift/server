import { strict as assert } from "node:assert";
import { test } from "node:test";
import { Copilot, READY } from "../drivers/copilot/index.js";

// Copilot CLI 1.0.101's first screen in a fresh HOME: the trust dialog. Its
// text mentions "shell commands", but it isn't ready for a prompt.
const trust = `
│ Copilot can read files in this folder and, with your permission, edit them or run code and shell commands. It will remember your permissions for the rest of this session.
│ Do you trust the files in this folder?
│ ❯ 1. Yes
│   2. Yes, and remember this folder for future sessions
│   3. No (Esc)
│ ↑/↓ to navigate · enter to select · esc to cancel
`;

// The same version once the prompt is up.
const prompt = `
 /private/tmp/wakebench/run/homes/copilot/work                                     Session: 0 AIC used
────────────────────────────────────────────────────────────────────────────
❯
────────────────────────────────────────────────────────────────────────────
 ← open sidebar · Interactive · Manual Approval · / commands · ? help · tab next tab        Claude Sonnet 5.5
`;

test("copilot: the trust dialog isn't ready for a prompt; 1.0.101's prompt footer is", () => {
  assert.equal(READY.test(trust), false);
  assert.equal(READY.test(prompt), true);
  // Older versions' footer still counts.
  assert.equal(READY.test("Type @ to mention files, # for issues/PRs, / for commands"), true);
});

// Copilot CLI 1.0.105's bottom lines while a turn runs (its thinking isn't
// in the transcript any more, so nothing else changes on screen), and once
// the turn is over with the async wait still running.
const footer = (status: string) => `
 ● watch (MCP: wakebench) · harness: "copilot-cli" Watching bench events. Each event is one line with a code like WB-<lane>-<n> ...
 /private/tmp/wakebench/run/homes/copilot/work                                     Session: 2.42 AIC used
────────────────────────────────────────────────────────────────────────────
❯
────────────────────────────────────────────────────────────────────────────
 ${status}                                                                    Claude Sonnet 5.5
`;

test("copilot: busy while Working, idle while only the background wait runs", () => {
  const d = new Copilot();
  assert.equal(d.busy(footer("◉ Working · 41 B esc esc interrupt")), true);
  assert.equal(d.busy(footer("● Thinking · 1.2 KiB esc esc interrupt")), true);
  assert.equal(d.busy(footer("◎ Compacting · 4 B esc esc interrupt")), true);
  assert.equal(d.busy(footer("● Waiting for background shells · 1.2 KiB esc esc interrupt")), false);
  assert.equal(d.busy(prompt), false);
});

// Copilot CLI 1.0.105's ask_user picker, as the agent asked about the
// install (S5, 2026-11-03): the first option is selected, so text typed
// into it and Enter accepted "Yes".
const picker = `
   Copilot will ask you to approve this command each time it starts (once per wake).
 ○ Asking user Waiting for response                                                       3m 47s ┃
────────────────────────────────────────────────────────────────────────────
 Copilot needs information.
 Install Wakeshift's plugin for GitHub Copilot CLI? It runs \`copilot plugin marketplace add /tmp/x/agent-plugins &&
 copilot plugin install wakeshift@wakeshift\`.
 Install plugin?:
 ❯ Yes
   No
   Other (type your answer)
 ↑/↓ select · enter accept · ctrl+d decline · esc cancel
────────────────────────────────────────────────────────────────────────────
`;

test("copilot: the install picker's options, yes and no", async () => {
  const { copilotInstallQuestion } = await import("../drivers/copilot/index.js");
  const q = copilotInstallQuestion(picker);
  assert.ok(q);
  assert.deepEqual(q.options.map((o) => o.label), ["Yes", "No", "Other (type your answer)"]);
  assert.equal(q.options.findIndex((o) => o.cursor), 0);
  assert.equal(q.yes.label, "Yes");
  assert.equal(q.no.label, "No");
  // Not a picker about the install, and not a picker at all.
  assert.equal(copilotInstallQuestion(picker.replace(/Install[^\n]*\n[^\n]*\n Install plugin\?:/, " Which color?:")), null);
  assert.equal(copilotInstallQuestion(picker.replace(/Wakeshift's plugin/, "the prettier plugin").replace(/wakeshift@wakeshift/, "prettier")), null);
  // A picker that's no longer at the bottom (something came after it).
  assert.equal(copilotInstallQuestion(picker + prompt), null);
  assert.equal(copilotInstallQuestion(trust), null);
  assert.equal(copilotInstallQuestion(prompt), null);
});
