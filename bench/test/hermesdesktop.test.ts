// Hermes's Desktop-surface variant (WB_HERMES_SURFACE=desktop): a classic
// CLI chat that says it's Hermes Desktop's, so its hooks say "desktop".
import { strict as assert } from "node:assert";
import { test } from "node:test";
import { Hermes, hermesDesktop, hermesSurfaces, hermesTui } from "../drivers/hermes/index.js";

function withEnv(env: Record<string, string | undefined>, fn: () => void) {
  const was = Object.fromEntries(Object.keys(env).map((k) => [k, process.env[k]]));
  for (const [k, v] of Object.entries(env)) {
    if (v === undefined) delete process.env[k];
    else process.env[k] = v;
  }
  try {
    fn();
  } finally {
    for (const [k, v] of Object.entries(was)) {
      if (v === undefined) delete process.env[k];
      else process.env[k] = v;
    }
  }
}

test("WB_HERMES_SURFACE=desktop starts a Desktop chat in the classic CLI, whose wakes aren't painted", () => {
  withEnv({ WB_HERMES_SURFACE: "desktop", WB_HERMES_TUI: undefined }, () => {
    const d = new Hermes();
    assert.equal(hermesDesktop(), true);
    assert.deepEqual(d.chat({} as never), ["/opt/hermes/bin/hermes", "chat", "--source", "gui"]);
    assert.equal(d.paneLog, undefined);
    assert.equal(d.volatile, undefined);
  });
  // It wins over WB_HERMES_TUI: the Desktop variant is one surface.
  withEnv({ WB_HERMES_SURFACE: "desktop", WB_HERMES_TUI: "1" }, () => {
    assert.equal(hermesTui(), false);
    assert.deepEqual(new Hermes().chat({} as never), ["/opt/hermes/bin/hermes", "chat", "--source", "gui"]);
  });
  withEnv({ WB_HERMES_SURFACE: undefined, WB_HERMES_TUI: undefined }, () => {
    assert.equal(hermesDesktop(), false);
    assert.deepEqual(new Hermes().chat({} as never), ["/opt/hermes/bin/hermes", "chat"]);
  });
});

test("the surface Hermes recorded for a chat: created_source, else source", () => {
  assert.deepEqual(
    hermesSurfaces([
      { source: "local", created_source: "desktop" },
      { source: "gui", created_source: null },
      { source: "cli", created_source: "" },
      { source: null, created_source: null },
    ]),
    ["desktop", "gui", "cli"],
  );
});
