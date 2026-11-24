// Codex's login in a lane's temp HOME: a copy of ~/.codex/auth.json, never a
// symlink, so a token refresh in the lane can't write the user's own file.
import { strict as assert } from "node:assert";
import { lstatSync, mkdtempSync, readFileSync, rmSync, statSync, symlinkSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { test } from "node:test";
import { copyLogin } from "../drivers/codex/index.js";

test("copyLogin: a 0600 copy, never a link; writing it leaves the original as it was", () => {
  const dir = mkdtempSync(join(tmpdir(), "wb-codexlogin-"));
  try {
    const real = join(dir, "real-auth.json");
    writeFileSync(real, '{"tokens":"original"}\n', { mode: 0o644 });
    const lane = join(dir, "auth.json");
    copyLogin(real, lane);
    assert.equal(lstatSync(lane).isSymbolicLink(), false);
    assert.equal(statSync(lane).mode & 0o777, 0o600);
    assert.equal(readFileSync(lane, "utf8"), '{"tokens":"original"}\n');
    // Codex refreshing its token in the lane.
    writeFileSync(lane, '{"tokens":"refreshed"}\n');
    assert.equal(readFileSync(real, "utf8"), '{"tokens":"original"}\n');
    // Something already there, a link above all, is never followed or replaced.
    const linked = join(dir, "linked.json");
    symlinkSync(real, linked);
    assert.throws(() => copyLogin(real, linked));
    assert.equal(readFileSync(real, "utf8"), '{"tokens":"original"}\n');
    assert.throws(() => copyLogin(join(dir, "missing.json"), join(dir, "x.json")));
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});
