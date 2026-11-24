// Tests for the bench-realism review: the questions it reads across wrapped
// lines, the placeholder code generic agents took for an event, the shell a
// background loop runs in, Hermes's own question picker, the
// delayed-approval variant and the option that sets it.

import { strict as assert } from "node:assert";
import { test } from "node:test";
import { CODE, WATCHING } from "../app/server.js";
import { withShellParents } from "../drivers/base.js";
import { Cursor, cursorInstallQuestion } from "../drivers/cursor/index.js";
import { openclawInstallQuestion } from "../drivers/openclaw/index.js";
import { hermesInstallQuestion } from "../drivers/hermes/index.js";
import type { AnswerPolicy, Driver } from "../drivers/types.js";
import { parseArgs } from "../src/cli.js";
import { Lane, type RunCtx } from "../src/lane.js";
import type { PsRow } from "../src/procs.js";
import { ASK_WITH_PICKER, asksInstall, INTRO, unwrap } from "../src/scenarios.js";

test("an install question wrapped over two lines is still a question", () => {
  const wrapped = "⏺ Wakeshift can wake this conversation if you install its hooks. Do you want me to run the\n  installer now?";
  assert.ok(asksInstall(wrapped));
  // Claude Code's and opencode's borders and markers come off before joining.
  assert.ok(asksInstall("│ Should I go ahead and install the\n│ Wakeshift plugin for opencode?"));
  assert.match(unwrap("Want me to install the\n  hooks?"), /Want me to install the hooks\?/);
  // Separate sentences and paragraphs stay apart.
  assert.equal(unwrap("I installed it.\nAnything else?"), "I installed it.\nAnything else?");
  assert.ok(!asksInstall("I won't install anything.\n\nWatching for events now."));
});

test("watch's own text shows no example that reads as an event code", () => {
  assert.equal(WATCHING.match(new RegExp(CODE.source, "g")), null, WATCHING);
  assert.ok("GOT WB-ab12-3".match(new RegExp(CODE.source, "g")));
});

test("a waiter's shells up to the harness are the lane's waiters too, never the pane's shell", () => {
  const rows: PsRow[] = [
    { pid: 10, ppid: 1, command: "bash --norc --noprofile" },
    { pid: 11, ppid: 10, command: "node /opt/cursor-agent/index.js --trust" },
    { pid: 12, ppid: 11, command: "/bin/zsh -c source /tmp/snap; eval \"$CMD\"" },
    { pid: 13, ppid: 12, command: "sh -c u=…; while :; do …; done" },
    { pid: 14, ppid: 13, command: "curl -s https://x/v1/stream/wait?link=…" },
    { pid: 20, ppid: 10, command: "sh -c true" },
  ];
  const l = { state: { panePid: 10 } } as unknown as Lane;
  const got = withShellParents(l, rows, rows.filter((r) => r.command.includes("/v1/stream")))
    .map((r) => r.pid)
    .sort();
  assert.deepEqual(got, [12, 13, 14]);
});

test("Hermes's question about the install is answered in its picker", () => {
  const screen = [
    "╭─ Hermes Agent needs your input ────────────────────────────────╮",
    "│ 1 question                                                     │",
    "│ ▸ Do you agree to install Wakeshift's hooks for Hermes Agent?  │",
    "│   ❯ 1. Yes, install Wakeshift hooks (Recommended)              │",
    "│     2. No, do not install                                      │",
    "│     3. Other (type your answer)                                │",
    "╰────────────────────────────────────────────────────────────────╯",
    "  ↑/↓ to select, Enter to lock, Tab next question  (3318s)",
  ].join("\n");
  const q = hermesInstallQuestion(screen);
  assert.ok(q);
  assert.equal(q.yes.label, "Yes, install Wakeshift hooks (Recommended)");
  assert.equal(q.no.label, "No, do not install");
  // Another question in the same picker isn't the install's.
  assert.equal(hermesInstallQuestion(screen.replace(/install Wakeshift hooks|install Wakeshift's hooks for Hermes Agent|do not install/g, "proceed")), null);
});

test("Hermes's TUI question about the install is answered in its picker", () => {
  const screen = [
    "   └─ ● ⡁ Clarify(\"Install Wakeshift's plugin for Hermes Agent? It runs `hermes co…\") (504s)",
    " ask 1 question",
    " ▸ Install Wakeshift's plugin for Hermes Agent? It runs `hermes plugins install wakeshift/hermes-plugin --enable`.",
    "   ▸ 1. Yes (Recommended)",
    "     2. No",
    "     3. Other (type your answer)",
    " 0/1 answered · ↑/↓ select · Enter confirm and continue · Tab/Shift+Tab switch question · Esc/Ctrl+C cancel",
    " ─ (⌐■_■) contemplating… · 8m 36s │ gpt 6 luna │ 13.2k/1.1M │ 8m 39s │ voice off │ 1 session",
  ].join("\n");
  const q = hermesInstallQuestion(screen);
  assert.ok(q);
  assert.equal(q.yes.label, "Yes (Recommended)");
  assert.equal(q.no.label, "No");
  assert.equal(q.options.find((o) => o.cursor)?.label, "Yes (Recommended)");
  assert.equal(hermesInstallQuestion(screen.replace(/install|plugin|Wakeshift/gi, "proceed")), null);
});

test("Cursor's question about the install is answered in its picker", () => {
  const screen = [
    " │ Clarifying Questions",
    " │ Question 1 of 1",
    " │ 1. Do you want me to install Wakeshift's optional Cursor hooks? This runs a remote install script",
    " │   › [ ] No, just keep the background listener",
    " │     [ ] Yes, install the hooks (I agree to the terms above)",
    " │     [ ] Other: (type to answer)",
    " │ ↑/↓ option · ←/→ question · Space select · Enter next/submit · Esc to skip",
  ].join("\n");
  const q = cursorInstallQuestion(screen);
  assert.ok(q);
  assert.deepEqual([q.cursor, q.no, q.yes], [0, 0, 1]);
  assert.equal(cursorInstallQuestion(screen.replace("Clarifying Questions", "Plan").replace(/ │ Question 1 of 1\n/, "")), null);
  // cursor-agent 2026.10.22 titles the picker with the agent's own header.
  const titled = screen.replace("Clarifying Questions", "Wakeshift hooks").replace("No, just keep the background listener", "Yes").replace("Yes, install the hooks (I agree to the terms above)", "No");
  const t2 = cursorInstallQuestion(titled);
  assert.ok(t2);
  assert.deepEqual([t2.cursor, t2.yes, t2.no], [0, 0, 1]);
  // Another question, with the install only in the conversation around it, isn't the install's.
  const other = titled.replace(/1\. .+\n/, "1. Should I also run the e2e tests?\n");
  assert.equal(cursorInstallQuestion(`Install Wakeshift's hooks? Done earlier.\n${other}`), null);
});

test("OpenClaw's question about the install is answered in its picker", () => {
  // As OpenClaw's TUI showed it (wakebench R4, November 3, 2026).
  const screen = [
    " Question pending · 3530s · /question to open",
    "Question 1/1 · Wakeshift",
    "Build #412 is running: install Wakeshift's plugin for OpenClaw so notifications can wake this conversation while idle? It runs `curl -fsSL https://app.wakeshift.ai/install/openclaw.sh |",
    "WAKESHIFT_OPENCLAW_NETWORK_HOOKS=1 sh`.",
    "Expires in 3529s",
    "› 1. Yes",
    "  2. No",
    "  3. Other…",
    "  Confirm answer",
    "  Skip",
    "↑/↓ or numbers select · Enter confirm · Esc collapse",
  ].join("\n");
  const q = openclawInstallQuestion(screen);
  assert.ok(q);
  assert.deepEqual([q.yes.label, q.no.label, q.options.findIndex((o) => o.cursor)], ["Yes", "No", 0]);
  // An old picker in the scrollback, with the chat under it, isn't live.
  assert.equal(openclawInstallQuestion(`${screen}\nUnderstood, I won't install anything.`), null);
  // Another question isn't the install's.
  assert.equal(openclawInstallQuestion(screen.replace(/install Wakeshift's plugin for OpenClaw.*\n.*\n/, "should I also run e2e?\n")), null);
});

test("--approve-delay and WB_APPROVE_DELAY set the delayed-approval variant", () => {
  assert.equal(parseArgs(["--harness", "codex", "--approve-delay", "20"]).approveDelay, 20);
  assert.equal(parseArgs(["--harness", "codex"]).approveDelay, 0);
  assert.throws(() => parseArgs(["--harness", "codex", "--approve-delay", "-1"]), /--approve-delay/);
});

test("with a delay, a dialog is answered only once it has been up that long", async () => {
  let answered = 0;
  const keys: string[] = [];
  const driver = {
    id: "stub",
    answer: async (l: Lane, screen: string, _p: AnswerPolicy) => {
      if (!/Allow\?/.test(screen)) return null;
      await l.tmux.keys(l.session, "y");
      answered++;
      return "approved";
    },
    busy: () => false,
  } as unknown as Driver;
  const tmux = { keys: async (_s: string, k: string) => void keys.push(k), type: async () => {}, exists: async () => false, capture: async () => "" };
  const ctx = { run: "r", dir: "/tmp", tmux, log: () => {}, slow: 1, approveDelayMs: 300, bench: { mcpUrl: () => "http://x" } } as unknown as RunCtx;
  const l = Object.create(Lane.prototype) as Lane;
  Object.assign(l, { ctx, driver, session: "s", state: {}, prompts: [], answered: [], steps: [], pendingDialog: null, lastTrack: 0 });
  const once = (screen: string) => (l as unknown as { answerOnce: (p: AnswerPolicy, s: string) => Promise<boolean> }).answerOnce({ allowInstall: false }, screen);
  assert.equal(await once("Allow?"), false); // seen (by a dry run that typed nothing)
  assert.equal(await once("Allow?"), false); // not up long enough
  assert.deepEqual(keys, []);
  assert.equal(answered, 1); // the dry run's
  await new Promise((r) => setTimeout(r, 350));
  assert.equal(await once("Allow?"), true);
  assert.deepEqual(keys, ["y"]);
  assert.equal(answered, 2);
});

test("a message left sitting in the input box is sent again; a dialog never gets that Enter", async () => {
  const run = async (screen: string, dialog = false) => {
    const keys: string[] = [];
    const driver = { answer: async () => (dialog ? "approved" : null), busy: () => false, unsent: (s: string, t: string) => new Cursor().unsent(s, t) } as unknown as Driver;
    const tmux = { keys: async (_s: string, k: string) => void keys.push(k), type: async () => {}, text: async () => screen, exists: async () => false };
    const ctx = { tmux, slow: 1 } as unknown as RunCtx;
    const l = Object.create(Lane.prototype) as Lane;
    Object.assign(l, { ctx, driver, session: "s", state: {}, prompts: [], answered: [], steps: [], snap: async () => ({}) });
    await l.confirmSent("ok, I'm back");
    return keys;
  };
  assert.deepEqual(await run("history…\n╭──────╮\n│ > ok, I'm back │\n╰──────╯"), ["Enter", "Enter"]);
  assert.deepEqual(await run("history…\n> ok, I'm back\nGOT WB-ab12-1\n╭──────╮\n│ → Add a follow-up │\n╰──────╯"), []);
  assert.deepEqual(await run("│ > ok, I'm back │\n Allow? 1. Yes", true), []);
});

test("WB_ASK_WITH_PICKER's line is off by default and isn't read as the agent asking", () => {
  if (process.env.WB_ASK_WITH_PICKER !== "1") assert.ok(!INTRO.includes(ASK_WITH_PICKER));
  assert.ok(!asksInstall(ASK_WITH_PICKER));
});
