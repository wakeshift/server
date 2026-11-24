// The Hermes API server lane and the OpenClaw lane on Codex's runtime.
import { strict as assert } from "node:assert";
import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { test } from "node:test";
import { fileURLToPath } from "node:url";
import { driverFor, HARNESSES } from "../drivers/index.js";
import { apiChatBusy, apiSession, HermesApi, withoutApiKey } from "../drivers/hermes/api.js";
import { sdkChatBusy, sdkDialog, sdkOptions, sdkPaths, sdkPin, sdkVersion } from "../drivers/claude-code/sdk.js";
import { requireCodexRuntime, type OpenClawConfig } from "../drivers/openclaw/index.js";
import { grade, parseTiers, tierOf } from "../src/grade.js";
import type { Lane } from "../src/lane.js";
import type { ScenarioResult } from "../src/model.js";

const here = dirname(fileURLToPath(import.meta.url));
const tiers = parseTiers(readFileSync(join(here, "../../tiers.yaml"), "utf8"));

test("the new lanes are registered, tiered and named", () => {
  for (const h of ["hermes-api", "openclaw-codex"]) assert.ok(HARNESSES.includes(h), h);
  assert.equal(tierOf(tiers, "hermes-api")?.id, "T2");
  assert.equal(tierOf(tiers, "openclaw-codex")?.id, "T3");
  assert.equal(tiers.names["hermes-api"], "Hermes Agent (API server)");
  assert.equal(tiers.names["openclaw-codex"], "OpenClaw (Codex runtime)");
  // Both are the harness they run (S9), in containers.
  assert.equal(driverFor("hermes-api").harness, "hermes");
  assert.equal(driverFor("openclaw-codex").harness, "openclaw");
  assert.equal(driverFor("openclaw-codex").mode, "container");
});

test("Hermes API server lane: the API server's key, the client, its session", () => {
  const d = new HermesApi();
  assert.deepEqual(d.spec.generated, ["API_SERVER_KEY"]);
  assert.deepEqual(d.spec.secrets, ["OPENAI_API_KEY"]);
  assert.deepEqual(d.spec.cmd, ["gateway", "run"]);
  const l = { id: "hermes-api-i", ctx: { run: "20261110-120000-ab12" } } as unknown as Lane;
  assert.equal(apiSession(l), "wakebench-20261110-120000-ab12-hermes-api-i");
  assert.deepEqual(d.chat(l).slice(-1), [apiSession(l)]);
  // Never the TUI's quirks, whatever WB_HERMES_TUI says.
  assert.equal(d.paneLog, undefined);
  assert.equal(d.gutter, undefined);
});

test("Hermes API server lane: the state's own API_SERVER_KEY is dropped from the run's .env", () => {
  assert.deepEqual(withoutApiKey(["A=1", "API_SERVER_KEY=x", "export API_SERVER_KEY=y", "API_SERVER_PORT=8642", ""]), ["A=1", "API_SERVER_PORT=8642", ""]);
});

test("Hermes API server lane: busy while the client's status line is last", () => {
  assert.equal(apiChatBusy("> watch the bench\n⠙ waiting for Hermes's API server · 12s\n\n"), true);
  assert.equal(apiChatBusy("> watch the bench\nGOT WB-ab12-1\n\n> "), false);
  // The status line from an earlier turn, quoted above, doesn't count.
  assert.equal(apiChatBusy("waiting for Hermes's API server\nDone.\n> "), false);
});

test("OpenClaw on Codex: the state's model requires Codex's runtime", () => {
  const doc: OpenClawConfig = {
    plugins: { entries: { codex: { config: { sessionCatalog: { enabled: false } } }, openai: { enabled: true } } },
    agents: { defaults: { model: { primary: "openai/gpt-6-luna" }, models: { "openai/gpt-6-luna": {} }, workspace: "/w" } },
  };
  assert.equal(requireCodexRuntime(doc), "openai/gpt-6-luna");
  assert.deepEqual(doc.agents?.defaults?.models?.["openai/gpt-6-luna"], { agentRuntime: { id: "codex" } });
  assert.deepEqual(doc.plugins?.entries?.codex, { config: { sessionCatalog: { enabled: false } }, enabled: true });
  assert.deepEqual(doc.plugins?.entries?.openai, { enabled: true });
  assert.equal(doc.agents?.defaults?.workspace, "/w");
  // A model given as a string; and a state with none.
  const s: OpenClawConfig = { agents: { defaults: { model: "openai/gpt-6-luna" } } };
  assert.equal(requireCodexRuntime(s), "openai/gpt-6-luna");
  assert.deepEqual(s.agents?.defaults?.models, { "openai/gpt-6-luna": { agentRuntime: { id: "codex" } } });
  assert.equal(requireCodexRuntime({}), null);
});

test("OpenClaw on Codex: S6 must connect without the bench's nudge", () => {
  const s6 = (metrics: ScenarioResult["metrics"]): ScenarioResult =>
    grade({ id: "S6", harness: "openclaw-codex", status: "ran", works: true, metrics, steps: [], notes: [] }, tierOf(tiers, "openclaw-codex"));
  assert.equal(s6({ connected: true, needed_watch_again: false, woke: true }).grade, "pass");
  assert.equal(s6({ connected: true, needed_watch_again: true, woke: true }).grade, "below-bar");
  // The plain lane has no such bar.
  const plain = grade({ id: "S6", harness: "openclaw", status: "ran", works: true, metrics: { connected: true, needed_watch_again: true, woke: true }, steps: [], notes: [] }, tierOf(tiers, "openclaw"));
  assert.equal(plain.grade, "pass");
});

test("Claude Code through the Agent SDK: registered, Claude Code's tier and harness", () => {
  assert.ok(HARNESSES.includes("claude-code-sdk"));
  assert.equal(tierOf(tiers, "claude-code-sdk")?.id, "T1");
  assert.equal(tiers.names["claude-code-sdk"], "Claude Code (Agent SDK)");
  const d = driverFor("claude-code-sdk");
  assert.equal(d.harness, "claude-code");
  assert.equal(d.id, "claude-code-sdk");
});

test("Claude Code through the Agent SDK: the pin, the options, the client's prompt", () => {
  assert.match(sdkPin(), /^\d+\.\d+\.\d+$/);
  assert.deepEqual(sdkOptions({}), { mode: "streaming", entrypoint: "t3-code", permissionMode: "default" });
  assert.deepEqual(sdkOptions({ WB_CLAUDE_SDK_MODE: "single", WB_CLAUDE_SDK_ENTRYPOINT: "default" }), { mode: "single", entrypoint: "default", permissionMode: "default" });
  assert.throws(() => sdkOptions({ WB_CLAUDE_SDK_MODE: "print" }));
  assert.throws(() => sdkOptions({ WB_CLAUDE_SDK_ENTRYPOINT: "t3 code; rm" }));
  assert.ok(sdkPaths("/s").claude.endsWith("/s/node_modules/@anthropic-ai/claude-agent-sdk-darwin-arm64/claude"));
  // Idle once its prompt is the last line; busy while a turn's lines come.
  assert.equal(sdkChatBusy("⏺ Done.\n✻ Done (success)\n\n❯ \n\n"), false);
  assert.equal(sdkChatBusy("❯ watch the bench\n✻ Working… (esc to interrupt)\n⏺ watch(…)"), true);
  assert.equal(sdkChatBusy("◆ woken: a turn started with no line typed\n✻ Working… (esc to interrupt)"), true);
  // A line being typed at the prompt is idle too.
  assert.equal(sdkChatBusy("✻ Done (success)\n\n❯ hello"), false);
});

test("Agent SDK lane: its version names the client's permission mode, not the terminal lane's", () => {
  assert.equal(sdkVersion("2.1.323 · auto mode", {}), `2.1.323 · manual mode · Agent SDK ${sdkPin()}, streaming input, t3-code`);
  assert.equal(sdkVersion("2.1.323 · auto mode", { WB_CLAUDE_SDK_PERMISSION_MODE: "acceptEdits" }), `2.1.323 · acceptEdits mode · Agent SDK ${sdkPin()}, streaming input, t3-code`);
});

test("Agent SDK client: only an open dialog is read, never the transcript above it", () => {
  const above = "⏺ QUOTE: … run `claude plugin install wakeshift --marketplace wakeshift/agent-plugins#main` …\n⏺ Bash({\"command\":\"curl -sS 'https://x/v1/stream/setup?token=t'\"})";
  const perm = "Permission requested\nBash command: curl -sS 'https://x/v1/stream/setup?token=t'\nDo you want to proceed?\n❯ 1. Yes\n  2. No\nEsc to cancel";
  // The dialog alone: the installer quoted above isn't what it asks to run.
  assert.equal(sdkDialog(`${above}\n\n${perm}\n`), perm);
  // Answered: the client erased it and said what was chosen, so nothing is open.
  assert.equal(sdkDialog(`${above}\n  ⎿ chose 1\n  ⎿ ok`), "");
  assert.equal(sdkDialog(`${above}\n\n❯ `), "");
  const q = "☐ Notifications\nHow should I notify you?\n❯ 1. Notify me while this session is open\n  2. Notify me in every session\n  3. Don't notify me\nEnter to select · ↑/↓ to navigate · Esc to cancel";
  assert.equal(sdkDialog(`${above}\n\n${q}`), q);
});
