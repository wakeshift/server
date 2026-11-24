// The driver registry: --harness and --assisted names.

import { assisted, type AssistedDriver } from "./assisted/index.js";
import { Antigravity } from "./antigravity/index.js";
import { ClaudeCode } from "./claude-code/index.js";
import { ClaudeCodeSdk } from "./claude-code/sdk.js";
import { Codex } from "./codex/index.js";
import { Copilot } from "./copilot/index.js";
import { Cursor } from "./cursor/index.js";
import { Fake } from "./fake/index.js";
import { Generic } from "./generic/index.js";
import { HermesApi } from "./hermes/api.js";
import { Hermes } from "./hermes/index.js";
import { OpenClaw, OpenClawCodex } from "./openclaw/index.js";
import { Opencode } from "./opencode/index.js";
import { Pi } from "./pi/index.js";
import type { Driver } from "./types.js";

const drivers: Record<string, () => Driver> = {
  "claude-code": () => new ClaudeCode(),
  "claude-code-sdk": () => new ClaudeCodeSdk(),
  codex: () => new Codex(),
  cursor: () => new Cursor(),
  opencode: () => new Opencode(),
  pi: () => new Pi(),
  antigravity: () => new Antigravity(),
  copilot: () => new Copilot(),
  hermes: () => new Hermes(),
  "hermes-api": () => new HermesApi(),
  openclaw: () => new OpenClaw(),
  "openclaw-codex": () => new OpenClawCodex(),
  fake: () => new Fake(),
  generic: () => new Generic(false),
  "generic-plain": () => new Generic(true),
};

export const HARNESSES = Object.keys(drivers);
export const ASSISTED = Object.keys(assisted);

export function driverFor(id: string): Driver {
  const d = drivers[id];
  if (!d) throw new Error(`no driver ${id}`);
  return d();
}

export function assistedDriver(id: string): AssistedDriver {
  const d = assisted[id];
  if (!d) throw new Error(`no assisted harness ${id}`);
  return d;
}
