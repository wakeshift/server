// Picker installs (src/picker.ts): the bench reads the picker's own data
// and fills it as the picker does, so its commands are the ones users see.
import { strict as assert } from "node:assert";
import { execFileSync } from "node:child_process";
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { test } from "node:test";
import { fileURLToPath } from "node:url";
import { defaultPickerData, fill, mcpAddMode, pickerCommand, pickerFile, readPicker, shellWords, unknownPlaceholder } from "../src/picker.js";

const golden = join(dirname(defaultPickerData()), "testdata/noauth.html");
const unescape = (s: string) => s.replace(/&#39;/g, "'").replace(/&#34;/g, '"').replace(/&lt;/g, "<").replace(/&gt;/g, ">").replace(/&#43;/g, "+").replace(/&amp;/g, "&");

test("picker: the bench reads this checkout's apps/agentpicker/agents.json", () => {
  const agents = readPicker();
  for (const id of ["claude-code", "codex", "cursor", "opencode", "hermes", "pi", "antigravity", "openclaw", "copilot"])
    assert.ok(agents.some((a) => a.id === id), `the picker has a ${id} tab`);
});

test("picker: the bench fills every step as the Go renderer does (its golden page)", () => {
  // apps/agentpicker/golden_test.go renders noauth.html with these options.
  const o = { name: "webhooks", url: "https://try.example/t/abc/mcp", each: "each post", noAuth: true };
  const html = readFileSync(golden, "utf8");
  const agents = readPicker();
  let checked = 0;
  for (const a of agents) {
    const panel = new RegExp(`<section class="ap-panel" id="ap-${a.id}"[\\s\\S]*?</section>`).exec(html)?.[0];
    assert.ok(panel, `the golden page has ${a.id}'s panel`);
    const codes = [...panel.matchAll(/<code>([\s\S]*?)<\/code>/g)].map((m) => unescape(m[1]));
    const ours = a.steps.filter((s) => s.kind === "run" || s.kind === "file" || s.kind === "url").map((s) => fill(s.text ?? "", o));
    assert.deepEqual(ours, codes, `${a.id}'s commands`);
    checked += ours.length;
  }
  assert.ok(checked > 10);
});

test("picker: no step holds a placeholder the bench doesn't fill", () => {
  for (const a of readPicker()) for (const s of a.steps) assert.equal(unknownPlaceholder(s.text ?? ""), null, `${a.id}: ${s.text}`);
  assert.equal(unknownPlaceholder("x {name} {newthing}"), "{newthing}");
});

test("picker: shellWords splits as sh does, and refuses what needs a shell", () => {
  assert.deepEqual(shellWords("claude mcp add --transport http wakebench http://127.0.0.1:1/mcp/x"), ["claude", "mcp", "add", "--transport", "http", "wakebench", "http://127.0.0.1:1/mcp/x"]);
  assert.deepEqual(shellWords(`openclaw config set mcp.servers.w '{"url":"u","transport":"streamable-http"}' --strict-json`), ["openclaw", "config", "set", "mcp.servers.w", '{"url":"u","transport":"streamable-http"}', "--strict-json"]);
  assert.deepEqual(shellWords(`a "b \\"c\\"" d\\ e ''`), ["a", 'b "c"', "d e", ""]);
  // A single quote inside single quotes, the shell's way ('\''), stays one word, and quoted metacharacters are literal.
  assert.deepEqual(shellWords(`echo 'it'\\''s $(x); y'`), ["echo", "it's $(x); y"]);
  // The same words /bin/sh makes of them.
  for (const cmd of [`echo 'it'\\''s $(x); y'`, `a "b \\"c\\"" d\\ e ''`, `openclaw config set mcp.servers.w '{"url":"u","transport":"streamable-http"}' --strict-json`]) {
    const sh = execFileSync("/bin/sh", ["-c", `printf '%s\\0' ${cmd}`], { encoding: "utf8" }).split("\0").slice(0, -1);
    assert.deepEqual(shellWords(cmd), sh, cmd);
  }
  for (const bad of ["a; b", "a | b", "a $(b)", 'a "$HOME"', "a > f", "a 'b", 'a "b']) assert.throws(() => shellWords(bad), bad);
});

test("picker: each automated harness's command, with the lane's name and URL", () => {
  const o = { name: "wakebench", url: "http://127.0.0.1:4100/mcp/codex-b", noAuth: true };
  assert.deepEqual(pickerCommand("claude-code", o), { text: "claude mcp add --transport http wakebench http://127.0.0.1:4100/mcp/codex-b", program: "claude", args: ["mcp", "add", "--transport", "http", "wakebench", o.url] });
  assert.equal(pickerCommand("codex", o).program, "codex");
  assert.equal(pickerCommand("antigravity", o).program, "agy");
  assert.deepEqual(JSON.parse(pickerCommand("openclaw", o).args[3]), { url: o.url, transport: "streamable-http" });
  // Cursor and Hermes: the file the picker shows.
  const cursor = pickerFile("cursor", o);
  assert.equal(cursor.path, "~/.cursor/mcp.json");
  assert.deepEqual(JSON.parse(cursor.text), { mcpServers: { wakebench: { url: o.url } } });
  assert.equal(pickerFile("hermes", o).path, "$HERMES_HOME/config.yaml");
  assert.doesNotMatch(pickerFile("hermes", o).text, /auth/, "an app without sign-in has no auth line");
  assert.throws(() => pickerCommand("cursor", o), /no run step/);
  assert.throws(() => pickerCommand("nobody", o), /no nobody tab/);
  // A file step whose label (where the path is) grew a placeholder the bench doesn't fill.
  const labeled = [{ id: "x", name: "X", group: "more", steps: [{ kind: "file" as const, label: "Add to ~/.x/{profile}/mcp.json", text: "{}" }], wakes: [] }];
  assert.throws(() => pickerFile("x", o, labeled), /label has a placeholder the bench doesn't fill: \{profile\}/);
});

test("picker: WB_PICKER_DATA reads another agents.json (an older picker, to check the bench fails on it)", () => {
  const dir = mkdtempSync(join(tmpdir(), "wb-picker-"));
  try {
    const path = join(dir, "agents.json");
    // OpenClaw's command before its fix: refused while the gateway runs.
    writeFileSync(path, JSON.stringify([{ id: "openclaw", name: "OpenClaw", group: "more", steps: [{ kind: "run", label: "Run", text: "openclaw mcp add {name} --url {url} --transport streamable-http" }], wakes: [] }]));
    const old = readPicker(path);
    assert.deepEqual(pickerCommand("openclaw", { name: "w", url: "u" }, old).args, ["mcp", "add", "w", "--url", "u", "--transport", "streamable-http"]);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test("picker: WB_MCP_ADD picks the way, the picker's by default", () => {
  assert.equal(mcpAddMode({}), "picker");
  assert.equal(mcpAddMode({ WB_MCP_ADD: "bench" }), "bench");
  assert.throws(() => mcpAddMode({ WB_MCP_ADD: "nope" }), /picker or bench/);
});

// The bench is run from the checkout: the default path must resolve there.
test("picker: the default data path is the checkout's", () => {
  assert.equal(defaultPickerData(), join(dirname(fileURLToPath(import.meta.url)), "../../../apps/agentpicker/agents.json"));
});
