// A chat client of Claude Code through the Claude Agent SDK, as an SDK host
// (T3 Code, Zed) drives it: wakebench's claude-code-sdk lane
// (drivers/claude-code/sdk.ts).
//
// Streaming input (the default, WB_CLAUDE_SDK_MODE=streaming): one query()
// whose prompt is an async iterable that stays open, so the session lives
// between turns and a waiter can wake it. User settings, the claude_code
// tool and system prompt presets, and CLAUDE_CODE_ENTRYPOINT set to the
// host's name (t3-code; "default" leaves the SDK's own, sdk-ts). Single
// (WB_CLAUDE_SDK_MODE=single): each line is a one-shot query (a string
// prompt) resuming the last session, whose process exits after its turn.
//
// The pane is a terminal: each line typed is a user message. Permission
// requests and AskUserQuestion show as numbered choices, in the shapes the
// bench's Claude Code driver reads (ClaudeCode.answer), and a digit answers
// them; Esc declines. "!<command>" runs a command as the user. "/exit", or
// the end of input, closes the session's input; the CLI then has 20 seconds
// to end (a running Monitor keeps it up for about 10 minutes otherwise)
// before it's stopped.
//
// Each turn that starts with no line typed (a wake) is marked "◆ woken".
// No credentials are read or printed: the CLI gets this process's
// environment (the pane's: the bench's token, by name).
//
// Usage: node sdk-chat.mjs --sdk <sdk.mjs> --claude <its claude> --cwd <dir>
//   [--mode streaming|single] [--entrypoint t3-code|default|<name>]
//   [--model <id>] [--permission-mode <mode>] [--resume <session id>]

import { execFileSync, spawnSync } from "node:child_process";
import { pathToFileURL } from "node:url";

const args = {};
for (let i = 2; i < process.argv.length; i++) {
  const a = process.argv[i];
  if (a.startsWith("--")) args[a.slice(2)] = process.argv[++i];
}
for (const need of ["sdk", "claude", "cwd"]) if (!args[need]) fail(`--${need} is required`);
const mode = args.mode ?? "streaming";
if (mode !== "streaming" && mode !== "single") fail(`--mode streaming or single, not ${mode}`);
const entry = args.entrypoint ?? "t3-code";
const { query } = await import(pathToFileURL(args.sdk).href);

function fail(why) {
  process.stderr.write(`sdk-chat: ${why}\n`);
  process.exit(2);
}

const write = (s = "") => process.stdout.write(s + "\n");
// While a dialog is open, the transcript's lines wait below it until it's
// answered: a line drawn under an open dialog (the SDK runs two tool calls
// at once, and the second's call shows while the first asks) would push the
// dialog up from the bottom, where the bench reads it, and the erase after
// its answer would take the wrong rows.
const held = [];
let choice = null; // the open dialog: { options: string[], resolve(n|null) }
const out = (s = "") => (choice ? held.push(s) : write(s));
const PROMPT = "❯ ";
const BUSY = "✻ Working… (esc to interrupt)";

// The header the driver reads the model from (the line under "Claude Code v…").
let version = "?";
try {
  version = execFileSync(args.claude, ["--version"], { encoding: "utf8", timeout: 20_000 }).trim().split(/\s/)[0];
} catch {
  // The header says "?": the driver's version check reports it.
}
out(`Claude Code v${version} (Claude Agent SDK client)`);
out(`${args.model ?? "the account's default model"} · Agent SDK, ${mode} input, entrypoint ${entry}`);
out();

const env = { ...process.env, DISABLE_AUTOUPDATER: "1" };
if (entry === "default") delete env.CLAUDE_CODE_ENTRYPOINT;
else env.CLAUDE_CODE_ENTRYPOINT = entry;

// State: idle (the prompt is the last line), busy, or asking (a choice).
let state = "idle";
let typed = ""; // the line being typed
let session = args.resume ?? null;
let closed = false;
let ended = false;
let current = null; // the running query (single mode: this line's)
const abort = new AbortController(); // stops the CLI

function showPrompt() {
  state = "idle";
  process.stdout.write("\n" + PROMPT);
}

function short(s, n = 300) {
  s = String(s ?? "").replace(/\s+/g, " ").trim();
  return s.length > n ? s.slice(0, n) + "…" : s;
}

// A numbered choice the bench answers with a digit (Esc: none). Once
// answered, the dialog is erased and one line says what was chosen, as
// Claude Code's dialogs leave the screen: a dialog still drawn after its
// answer would read as a new one, and the bench would answer it again (it
// once answered stale dialogs many times over, their digits typed into the
// next message). Asks run one at a time: the SDK can ask for two tool
// calls at once, and a second dialog drawn over the first would leave the
// first unanswerable.
let asking = Promise.resolve();
function ask(lines, options, footer) {
  const turn = asking.then(() => drawChoice(lines, options, footer));
  asking = turn.catch(() => {});
  return turn;
}

// The terminal rows a line takes (a long tool input wraps): the erase after
// an answer goes up exactly this many. The bench's panes don't change size
// (200×50), and a line exactly as wide as the pane takes one row (the
// newline that follows moves past its pending wrap).
function rows(line) {
  const cols = process.stdout.columns || 80;
  return Math.max(1, Math.ceil([...line].length / cols));
}

function drawChoice(lines, options, footer) {
  // Input closed: nobody answers any more, so what's still asked is declined.
  if (closed) return Promise.resolve(null);
  return new Promise((resolve) => {
    const was = state;
    state = "asking";
    const drawn = ["", ...lines, ...options.map((o, i) => `${i === 0 ? "❯" : " "} ${i + 1}. ${o}`), footer];
    for (const l of drawn) write(l);
    const height = drawn.reduce((n, l) => n + rows(l), 0);
    choice = {
      options,
      resolve: (n) => {
        choice = null;
        state = was === "idle" ? "busy" : was;
        // Up to the dialog's first row, and clear from there down.
        process.stdout.write(`\x1b[${height}A\r\x1b[J`);
        write(`  ⎿ ${n === null ? "(declined)" : `chose ${n}`}`);
        for (const l of held.splice(0)) write(l);
        resolve(n);
      },
    };
  });
}

async function canUseTool(name, input) {
  if (name === "AskUserQuestion") {
    const answers = {};
    for (const q of input.questions ?? []) {
      const labels = (q.options ?? []).map((o) => o.label);
      const n = await ask([`☐ ${q.header ?? "Question"}`, q.question], labels, "Enter to select · ↑/↓ to navigate · Esc to cancel");
      if (n === null) return { behavior: "deny", message: "The user declined to answer." };
      answers[q.question] = labels[n - 1];
      out(`  ⎿ ${labels[n - 1]}`);
    }
    return { behavior: "allow", updatedInput: { ...input, answers } };
  }
  const what = name === "Bash" ? `Bash command: ${input.command ?? ""}` : `${name}(${short(JSON.stringify(input), 400)})`;
  const n = await ask(["Permission requested", what, "Do you want to proceed?"], ["Yes", "No"], "Esc to cancel");
  if (n === 1) return { behavior: "allow", updatedInput: input };
  return { behavior: "deny", message: "The user declined." };
}

function options(extra = {}) {
  return {
    cwd: args.cwd,
    env,
    pathToClaudeCodeExecutable: args.claude,
    ...(args.model ? { model: args.model } : {}),
    settingSources: ["user", "project", "local"],
    tools: { type: "preset", preset: "claude_code" },
    systemPrompt: { type: "preset", preset: "claude_code" },
    permissionMode: args["permission-mode"] ?? "default",
    canUseTool,
    abortController: abort,
    ...extra,
  };
}

// The transcript, one message at a time.
function show(m) {
  if (m.type === "system" && m.subtype === "init") {
    session = m.session_id ?? session;
    return;
  }
  if (m.type === "assistant") {
    for (const c of m.message?.content ?? []) {
      if (c.type === "text" && c.text.trim()) out(`⏺ ${c.text.trim()}`);
      if (c.type === "tool_use") out(`⏺ ${c.name}(${short(JSON.stringify(c.input), 200)})`);
    }
    return;
  }
  if (m.type === "user" && Array.isArray(m.message?.content)) {
    for (const c of m.message.content) {
      if (c.type !== "tool_result") continue;
      const text = typeof c.content === "string" ? c.content : (c.content ?? []).map((x) => x.text ?? "").join(" ");
      out(`  ⎿ ${short(text)}`);
    }
    return;
  }
  if (m.type === "result") out(`✻ Done (${m.subtype}${typeof m.total_cost_usd === "number" ? `, $${m.total_cost_usd.toFixed(4)}` : ""})`);
}

// Reads one query's messages; a message while idle is a turn nobody typed.
async function drain(q) {
  for await (const m of q) {
    if (state === "idle" && m.type !== "result") {
      state = "busy";
      out();
      out("◆ woken: a turn started with no line typed");
      out(BUSY);
    }
    show(m);
    if (m.type === "result") showPrompt();
  }
}

// Streaming input: a queue the session reads, open until close().
const queue = [];
let wakeInput = null;
async function* input() {
  for (;;) {
    while (queue.length) yield queue.shift();
    if (closed) return;
    await new Promise((r) => (wakeInput = r));
    wakeInput = null;
  }
}

let streaming = null;
if (mode === "streaming") {
  current = query({ prompt: input(), options: options(session ? { resume: session } : {}) });
  streaming = drain(current)
    .catch((e) => out(`error: ${short(e?.message ?? e)}`))
    .finally(() => {
      ended = true;
      out("(the session ended)");
      process.exit(0);
    });
}

function submit(line) {
  out();
  if (line === "/exit") return closeInput();
  if (line.startsWith("!")) {
    // As the user, in the pane (Claude Code's "!" bash mode).
    const r = spawnSync("/bin/sh", ["-c", line.slice(1)], { cwd: args.cwd, env: process.env, encoding: "utf8", timeout: 300_000 });
    out(short(`${r.stdout ?? ""}${r.stderr ?? ""}`, 2000) || `(exit ${r.status})`);
    return showPrompt();
  }
  if (!line.trim()) return showPrompt();
  state = "busy";
  out(BUSY);
  if (mode === "streaming") {
    queue.push({ type: "user", message: { role: "user", content: line }, parent_tool_use_id: null, session_id: "" });
    if (wakeInput) wakeInput();
    return;
  }
  current = query({ prompt: line, options: options(session ? { resume: session } : {}) });
  drain(current).catch((e) => {
    out(`error: ${short(e?.message ?? e)}`);
    showPrompt();
  });
}

async function closeInput() {
  if (closed) return;
  closed = true;
  // A dialog still open is declined (and the asks queued behind it, by drawChoice).
  choice?.resolve(null);
  if (wakeInput) wakeInput();
  out("(input closed)");
  if (mode === "single" || !streaming) process.exit(0);
  await new Promise((r) => setTimeout(r, 20_000));
  if (!ended) {
    out("(the CLI was still running 20 s after its input closed: stopping it)");
    abort.abort();
    setTimeout(() => process.exit(0), 3000);
  }
}

// Keys: a raw terminal, so a digit answers a choice at once (the bench
// sends a choice's digit with no Enter, as Claude Code's dialogs take it).
function key(s) {
  // A lone digit while a turn runs and no dialog is up answers a dialog
  // that's gone (the bench sends a choice's digit as a keystroke of its
  // own): it isn't the start of the next message. A message that starts
  // with a digit comes with the rest of its text.
  if (!choice && state !== "idle" && !typed && /^[1-9]$/.test(s)) return;
  for (const ch of s) {
    if (ch === "\x03") return closeInput(); // Ctrl-C
    if (choice) {
      if (ch === "\x1b") choice.resolve(null);
      else if (/[1-9]/.test(ch) && Number(ch) <= choice.options.length) choice.resolve(Number(ch));
      else if (ch === "\r" || ch === "\n") choice.resolve(1);
      continue;
    }
    if (ch === "\x1b") {
      if (state === "busy") current?.interrupt?.().catch(() => {});
      continue;
    }
    if (ch === "\r" || ch === "\n") {
      const line = typed;
      typed = "";
      submit(line);
    } else if (ch === "\x7f" || ch === "\b") {
      if (typed) {
        typed = typed.slice(0, -1);
        process.stdout.write("\b \b");
      }
    } else if (ch >= " ") {
      typed += ch;
      process.stdout.write(ch);
    }
  }
}

process.stdin.setEncoding("utf8");
if (process.stdin.isTTY) process.stdin.setRawMode(true);
process.stdin.on("data", key);
process.stdin.on("end", () => closeInput());
showPrompt();
