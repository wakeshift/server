// Tests for what keeps a lane in its temp HOME (every harness's config
// variable), the snapshot of a dialog before it's answered, and Hermes's
// busy footer.

import { strict as assert } from "node:assert";
import { existsSync, mkdtempSync, readdirSync, readFileSync, realpathSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { test } from "node:test";
import { AGY_NO_UPDATE, Antigravity } from "../drivers/antigravity/index.js";
import { Copilot, copilotNoUpdateEnv, copilotStartArgs } from "../drivers/copilot/index.js";
import { ClaudeCode, claudeJsonPaths } from "../drivers/claude-code/index.js";
import { Hermes, HERMES_BUSY, HERMES_TUI_GUTTER, HERMES_TUI_STATUS, hermesTuiBusy } from "../drivers/hermes/index.js";
import { Opencode, OPENCODE_ISOLATION, OPENCODE_SERVICE, opencodeMajor, opencodePin } from "../drivers/opencode/index.js";
import { Pi, PI_BUSY, PI_READY } from "../drivers/pi/index.js";
import { loginPrompt } from "../drivers/base.js";
import type { AnswerPolicy, Driver } from "../drivers/types.js";
import { configEnv, isoEnv, makeHome, makeRunDir } from "../src/home.js";
import { Lane, type RunCtx } from "../src/lane.js";

const scratch = () => realpathSync(mkdtempSync(join(tmpdir(), "wb-homes-")));

test("a lane's environment points every harness's config variable into its temp HOME", () => {
  const h = makeHome(makeRunDir(scratch(), "20261102-120000-ab12"), "codex");
  const env = isoEnv(h, []);
  for (const k of ["CODEX_HOME", "CLAUDE_CONFIG_DIR", "XDG_CONFIG_HOME", "HERMES_HOME", "OPENCODE_CONFIG", "COPILOT_HOME", "PI_CODING_AGENT_DIR"]) {
    assert.ok(env[k], `${k} is set`);
    assert.ok(env[k].startsWith(h.dir + "/"), `${k} is inside the temp HOME: ${env[k]}`);
  }
  assert.equal(env.HOME, h.dir);
  assert.deepEqual(Object.keys(configEnv(h)).sort(), ["CLAUDE_CONFIG_DIR", "CODEX_HOME", "COPILOT_HOME", "HERMES_HOME", "OPENCODE_CONFIG", "PI_CODING_AGENT_DIR", "XDG_CONFIG_HOME"]);
  // The user's own shell never leaks in, whatever it exports.
  const before = process.env.CODEX_HOME;
  process.env.CODEX_HOME = "/Users/someone/.codex";
  try {
    assert.equal(isoEnv(h, []).CODEX_HOME, join(h.dir, ".codex"));
  } finally {
    if (before === undefined) delete process.env.CODEX_HOME;
    else process.env.CODEX_HOME = before;
  }
});

test("every driver's environment keeps the config variables in the temp HOME (Claude Code's keychain entry keeps its name)", () => {
  const h = makeHome(makeRunDir(scratch(), "20261102-120001-ab12"), "x");
  const l = { home: h } as unknown as Lane;
  for (const d of [new ClaudeCode(), new Antigravity(), new Hermes(), new Pi()]) {
    const env = d.env(l);
    assert.equal(env.CLAUDE_CONFIG_DIR, join(h.dir, ".claude"), d.id);
    assert.equal(env.CODEX_HOME, join(h.dir, ".codex"), d.id);
    assert.equal(env.PI_CODING_AGENT_DIR, join(h.dir, ".pi/agent"), d.id);
  }
  // pi's own runs: no update check, no telemetry.
  assert.equal(new Pi().env(l).PI_SKIP_VERSION_CHECK, "1");
  assert.equal(new Pi().env(l).PI_TELEMETRY, "0");
  // CLAUDE_CONFIG_DIR alone would rename the borrowed keychain's entry.
  assert.equal(new ClaudeCode().env(l).CLAUDE_SECURESTORAGE_CONFIG_DIR, "");
  assert.deepEqual(claudeJsonPaths("/h"), ["/h/.claude.json", "/h/.claude/.claude.json"]);
});

test("Copilot and agy never update themselves from a lane; Copilot's package cache stays in the temp HOME", () => {
  const h = makeHome(makeRunDir(scratch(), "20261116-120000-ab12"), "copilot");
  const l = { home: h } as unknown as Lane;
  const env = new Copilot().env(l);
  // Copilot's updater writes outside the temp HOME (2026-11-16: the real ~/Library/Caches/copilot/pkg).
  assert.equal(env.COPILOT_AUTO_UPDATE, "false");
  assert.equal(env.COPILOT_CACHE_HOME, join(h.dir, "Library/Caches/copilot"));
  assert.ok(env.COPILOT_CACHE_HOME.startsWith(h.dir + "/"));
  assert.equal(env.COPILOT_HOME, join(h.dir, ".copilot"));
  assert.deepEqual(copilotNoUpdateEnv("/h"), { COPILOT_AUTO_UPDATE: "false", COPILOT_CACHE_HOME: "/h/Library/Caches/copilot" });
  // A caller's extra variables still win, and the switch survives them.
  assert.equal(new Copilot().env(l, { X: "1" }).X, "1");
  assert.equal(new Copilot().env(l, { X: "1" }).COPILOT_AUTO_UPDATE, "false");
  // The interactive session also gets the flag, fresh or resumed, with any deny rule after it.
  assert.deepEqual(copilotStartArgs({}), ["--no-auto-update"]);
  assert.deepEqual(copilotStartArgs({ resume: true }, ["--deny-tool", "shell(x)"]), ["--no-auto-update", "--continue", "--deny-tool", "shell(x)"]);
  // agy replaces its own binary when it updates.
  assert.equal(new Antigravity().env(l).AGY_CLI_DISABLE_AUTO_UPDATE, "1");
  assert.deepEqual(AGY_NO_UPDATE, { AGY_CLI_DISABLE_AUTO_UPDATE: "1" });
  // Other harnesses don't get them.
  assert.equal(new Pi().env(l).COPILOT_AUTO_UPDATE, undefined);
  assert.equal(new Pi().env(l).AGY_CLI_DISABLE_AUTO_UPDATE, undefined);
});

test("opencode's lane turns off its discovery above the work directory (the user's ~/.claude and ~/.agents)", () => {
  const h = makeHome(makeRunDir(scratch(), "20261102-120003-ab12"), "opencode");
  const env = new Opencode().env({ home: h } as unknown as Lane);
  for (const [k, v] of Object.entries(OPENCODE_ISOLATION)) assert.equal(env[k], v, k);
  assert.deepEqual(Object.keys(OPENCODE_ISOLATION).sort(), ["OPENCODE_DISABLE_CLAUDE_CODE", "OPENCODE_DISABLE_EXTERNAL_SKILLS", "OPENCODE_DISABLE_PROJECT_CONFIG"]);
  // The temp HOME still holds its config.
  assert.equal(env.HOME, h.dir);
  assert.equal(env.OPENCODE_CONFIG, join(h.dir, ".config/opencode/opencode.json"));
  // Other harnesses don't get them.
  assert.equal(new Pi().env({ home: h } as unknown as Lane).OPENCODE_DISABLE_PROJECT_CONFIG, undefined);
});

test("Claude Code's account record goes where CLAUDE_CONFIG_DIR makes it read, and where it isn't set", async () => {
  const h = makeHome(makeRunDir(scratch(), "20261102-120002-ab12"), "claude-code");
  const steps: string[] = [];
  const l = {
    home: h,
    state: {},
    mcpName: "wakebench",
    mcpUrl: "http://127.0.0.1:1/mcp/claude-code",
    mcp2: null,
    mcpToken: "tok",
    note: (s: string) => void steps.push(s),
    run: async () => ({ ok: true, out: "" }),
  } as unknown as Lane;
  await new ClaudeCode().prepare(l);
  for (const p of claudeJsonPaths(h.dir)) {
    assert.ok(existsSync(p), p);
    const j = JSON.parse(readFileSync(p, "utf8")) as { hasCompletedOnboarding?: boolean; projects?: Record<string, unknown> };
    assert.equal(j.hasCompletedOnboarding, true);
    assert.ok(j.projects?.[h.work]);
  }
});

test("a dialog is snapshotted as it was asked, before the driver answers it", async () => {
  const dir = scratch();
  let screen = "history…\nAllow the tool? 1. Yes 2. No";
  const driver = {
    id: "stub",
    answer: async (l: Lane, s: string, _p: AnswerPolicy) => {
      if (!/Allow the tool\?/.test(s)) return null;
      screen = "history…\n(approved)";
      await l.tmux.keys(l.session, "1");
      return "approved a tool";
    },
    busy: () => false,
  } as unknown as Driver;
  const tmux = { keys: async () => {}, type: async () => {}, exists: async () => true, capture: async () => screen, text: async () => screen };
  const ctx = { run: "r", dir, tmux, log: () => {}, slow: 1, bench: { mcpUrl: () => "http://x" } } as unknown as RunCtx;
  const l = Object.create(Lane.prototype) as Lane;
  Object.assign(l, { ctx, driver, id: "lane", session: "s", state: {}, prompts: [], answered: [], steps: [], pendingDialog: null, lastTrack: 0, shot: 0, t0: Date.now() });
  const { mkdirSync } = await import("node:fs");
  mkdirSync(join(dir, "snapshots", "lane"), { recursive: true });
  const once = (s: string) => (l as unknown as { answerOnce: (p: AnswerPolicy, s: string) => Promise<boolean> }).answerOnce({ allowInstall: false }, s);
  assert.equal(await once(screen), true);
  const shots = readdirSync(join(dir, "snapshots", "lane")).filter((f) => f.endsWith(".ansi")).sort();
  assert.equal(shots.length, 2);
  assert.match(readFileSync(join(dir, "snapshots", "lane", shots[0]), "utf8"), /Allow the tool\?/);
  assert.match(readFileSync(join(dir, "snapshots", "lane", shots[1]), "utf8"), /\(approved\)/);
  assert.deepEqual(
    l.steps.map((s) => s.label),
    ["dialog, before answering: approved a tool", "answered: approved a tool"],
  );
  // No dialog: nothing kept.
  assert.equal(await once("history…\nidle"), false);
  assert.equal(readdirSync(join(dir, "snapshots", "lane")).filter((f) => f.endsWith(".ansi")).length, 2);
});

test("Hermes's TUI (WB_HERMES_TUI=1) is busy unless its status bar says ready, and its ticking bar is volatile", () => {
  const bar = (state: string, t: string) => `history…\n ${"\n".repeat(3)} ─ ${state} │ gpt 6 luna │ 14.6k/1.1M │ [░░░░░░░░░░] 1% │ ${t} │ ✓ 3m 33s │ voice off │ 1 session   ─  Monitor\n ❯`;
  assert.equal(hermesTuiBusy(bar("ready", "4m 7s")), false);
  assert.equal(hermesTuiBusy(bar("thinking", "4m 7s")), true);
  assert.equal(hermesTuiBusy(bar("running…", "4m 7s")), true);
  assert.equal(hermesTuiBusy("no status bar\n❯"), false);
  assert.ok(HERMES_TUI_STATUS.test(bar("ready", "1s").split("\n")[4]));
  assert.ok(!HERMES_TUI_STATUS.test(" ─ a line of the transcript"));
  const was = process.env.WB_HERMES_TUI;
  process.env.WB_HERMES_TUI = "1";
  try {
    const d = new Hermes();
    assert.equal(d.busy(bar("thinking", "1s")), true);
    assert.equal(d.busy(bar("ready", "1s")), false);
    assert.equal(d.volatile, HERMES_TUI_STATUS);
    assert.equal(d.paneLog, false);
    assert.equal(d.gutter, HERMES_TUI_GUTTER);
    // The status bar names the chat ("Monitor build 412 for failure"): never the agent's words.
    const status = " ─ ready │ gpt 6 luna │ 16k/1.1M │ 1m 13s │ voice off │ 1 session   ─  Monitor build 412 for failure";
    assert.equal(Lane.prototype.prose.call({ driver: d } as never, `hello\n${status}`), "hello");
    // A sentence the TUI wrapped, its scrollbar off: the trust step reads as one.
    const wrapped = ["    Wakeshift needs one more turn to connect—send   ┃", "    any message (for example, “done”).                ┃"].map((x) => x.replace(HERMES_TUI_GUTTER, "")).join("\n");
    assert.ok(d.trustStep.test(wrapped));
    assert.deepEqual(d.chat({} as never), ["/opt/hermes/bin/hermes", "chat", "--tui"]);
  } finally {
    if (was === undefined) delete process.env.WB_HERMES_TUI;
    else process.env.WB_HERMES_TUI = was;
  }
  assert.equal(new Hermes().volatile, undefined);
  assert.equal(new Hermes().paneLog, undefined);
  assert.equal(new Hermes().gutter, undefined);
  assert.deepEqual(new Hermes().chat({} as never), ["/opt/hermes/bin/hermes", "chat"]);
});

test("Hermes is busy while its turn hint shows, and idle without it", () => {
  const d = new Hermes();
  const footer = (s: string) => `history…\n${"\n".repeat(5)}❯ \n ${s}`;
  assert.equal(d.busy(footer("⚕ type to redirect this turn · Tab queues it")), true);
  assert.equal(d.busy(footer("esc to interrupt")), true);
  assert.equal(d.busy(footer("? for help")), false);
  assert.ok(HERMES_BUSY.test("Tab queues it"));
  // Only the bottom of the screen: the hint quoted far up in the history doesn't count.
  assert.equal(d.busy("type to redirect this turn\n" + "line\n".repeat(30) + "❯ "), false);
});

test("pi's screen: busy while its editor border says Working, ready at its key help or footer, a missing key is a login", () => {
  const pi = new Pi();
  const border = "─".repeat(40);
  assert.equal(pi.busy(`hello\n── ⠇ Working ${border}\n\n${border}\n/tmp/w\n0.5%/272k (auto)   gpt-6-luna • medium`), true);
  assert.equal(pi.busy(`Hi there, friend!\n${border}\n\n${border}\n/tmp/w\n↑3 ↓9 0.4%/272k (auto)   gpt-6-luna • medium`), false);
  assert.equal(pi.busy("Working on it, as you asked.\n" + border), false, "the agent's own words aren't the border");
  assert.ok(PI_READY.test(" █▀ █ escape interrupt · ctrl+c/ctrl+d clear/exit · / commands · ! bash · ctrl+o more"));
  assert.ok(PI_READY.test("0.0%/272k (auto)                     gpt-6-luna • medium"));
  assert.ok(PI_BUSY.test("── ⠼ Working ──"));
  assert.ok(loginPrompt("pi", "No providers logged in. Use /login first."));
  assert.ok(!loginPrompt("pi", "Hi there, friend!"));
});

test("opencode 2.x lanes run a private server (--standalone): its major version", () => {
  assert.equal(opencodeMajor("opencode v2.3.0"), 2);
  assert.equal(opencodeMajor("2.0.1"), 2);
  assert.equal(opencodeMajor("1.19.5"), 1);
  assert.equal(opencodeMajor("opencode 1.18.7\n"), 1);
  assert.equal(opencodeMajor(undefined), 0);
  assert.equal(opencodeMajor("dev"), 0);
});

test("the opencode lane's pin is a 1.x release (Wakeshift's plugin doesn't load on 2.x yet)", () => {
  const pin = opencodePin();
  assert.ok(pin, "harness-pins.json has an opencode pin");
  assert.equal(opencodeMajor(pin!), 1);
});

test("opencode 2.x's shared background service is recognized by its command line", () => {
  assert.match("/opt/homebrew/lib/node_modules/@opencode/cli/bin/opencode.exe serve --service", OPENCODE_SERVICE);
  assert.doesNotMatch("/opt/homebrew/bin/opencode --standalone --continue", OPENCODE_SERVICE);
  assert.doesNotMatch("/opt/homebrew/lib/node_modules/@opencode/cli/bin/opencode.exe serve --stdio --port 0", OPENCODE_SERVICE);
});
