import { strict as assert } from "node:assert";
import { chmodSync, mkdirSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { test } from "node:test";
import { ensureImage } from "../drivers/container.js";
import { Hermes } from "../drivers/hermes/index.js";
import { OpenClaw } from "../drivers/openclaw/index.js";
import { harnessState, parseEnv, readSecrets, stateRoot } from "../src/benchstate.js";

test("the state root is WB_STATE_DIR, else ~/.wakeshift-bench", () => {
  assert.equal(stateRoot({ WB_STATE_DIR: "/x/y" }), "/x/y");
  assert.match(stateRoot({}), /\/\.wakeshift-bench$/);
});

test("env files: KEY=VALUE, comments and blanks skipped, = in values kept", () => {
  assert.deepEqual(parseEnv("# a key\nOPENAI_API_KEY=sk-a=b\n\n  OTHER = x \nnot a line\n"), { OPENAI_API_KEY: "sk-a=b", OTHER: "x" });
});

test("a harness's state counts only with state.sh's marker; secrets need a private env file", () => {
  const root = mkdtempSync(join(tmpdir(), "wb-state-"));
  try {
    mkdirSync(join(root, "openclaw"));
    assert.equal(harnessState("openclaw", root), null, "a dir state.sh didn't make");
    writeFileSync(join(root, "openclaw", ".wakebench"), "openclaw 2026.10.4\n");
    assert.equal(harnessState("openclaw", root), join(root, "openclaw"));

    const r0 = readSecrets(["OPENAI_API_KEY"], root);
    assert.ok(!r0.ok && /missing/.test(r0.reason));
    const env = join(root, "env");
    writeFileSync(env, "OPENAI_API_KEY=sk-test\n", { mode: 0o644 });
    chmodSync(env, 0o644);
    const r1 = readSecrets(["OPENAI_API_KEY"], root);
    assert.ok(!r1.ok && /readable by others/.test(r1.reason));
    chmodSync(env, 0o600);
    assert.deepEqual(readSecrets(["OPENAI_API_KEY"], root), { ok: true, env: { OPENAI_API_KEY: "sk-test" } });
    const r2 = readSecrets(["OPENAI_API_KEY", "OTHER"], root);
    assert.ok(!r2.ok && /no OTHER/.test(r2.reason));
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("container drivers use the bench's state, never the user's", () => {
  for (const d of [new OpenClaw(), new Hermes()]) {
    const spec = d.spec as unknown as Record<string, unknown>;
    assert.equal(spec.source, undefined, `${d.id}: no source container`);
    assert.equal(spec.copy, undefined, `${d.id}: no copied host dirs`);
    assert.deepEqual(d.spec.secrets, ["OPENAI_API_KEY"]);
    assert.ok(!JSON.stringify(d.spec).includes("ws-claw"), `${d.id}: no /tmp/ws-claw`);
  }
  assert.deepEqual(new OpenClaw().spec.generated, ["WS_TOKEN"]);
});

test("a missing container image is pulled (with a timeout) before the harness is blocked for it", async () => {
  const img = "nousresearch/hermes-agent:v0.22.5";
  const fake = (have: boolean, pull: "ok" | "fail" | "timeout") => {
    const calls: { args: string[]; timeoutMs: number }[] = [];
    let here = have;
    const run = async (args: string[], timeoutMs: number) => {
      calls.push({ args, timeoutMs });
      if (args[0] === "image") {
        if (!here) throw Object.assign(new Error("Command failed"), { stderr: `Error: No such image: ${img}` });
        return;
      }
      if (pull === "timeout") throw Object.assign(new Error("Command failed"), { killed: true, signal: "SIGTERM" });
      if (pull === "fail") throw Object.assign(new Error("Command failed"), { stderr: "Error response from daemon: manifest unknown\n" });
      here = true;
    };
    return { calls, run };
  };

  const present = fake(true, "fail");
  assert.deepEqual(await ensureImage(img, present.run), { ok: true, pulled: false });
  assert.deepEqual(
    present.calls.map((c) => c.args[0]),
    ["image"],
    "an image that's here isn't pulled",
  );

  const missing = fake(false, "ok");
  assert.deepEqual(await ensureImage(img, missing.run, 1234), { ok: true, pulled: true });
  assert.deepEqual(
    missing.calls.map((c) => c.args),
    [
      ["image", "inspect", img],
      ["pull", "--quiet", img],
      ["image", "inspect", img],
    ],
  );
  assert.equal(missing.calls[1].timeoutMs, 1234, "the pull has its own timeout");

  const fails = fake(false, "fail");
  const r1 = await ensureImage(img, fails.run);
  assert.ok(!r1.ok && /isn't here, and docker pull failed: Error response from daemon: manifest unknown$/.test(r1.reason), JSON.stringify(r1));

  const slow = fake(false, "timeout");
  const r2 = await ensureImage(img, slow.run, 120_000);
  assert.ok(!r2.ok && /docker pull didn't finish in 2 min$/.test(r2.reason), JSON.stringify(r2));
});
