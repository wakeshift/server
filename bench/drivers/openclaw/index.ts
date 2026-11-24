// OpenClaw in its own container (see ../container.ts), driven through
// `openclaw tui`. Install: Wakeshift's plugin from npm (openclaw plugins
// install npm:@wakeshift/openclaw), working from the next turn.
// The core offers it at WB_OPENCLAW_PLUGIN's version (else Wakeshift's
// installer, openclaw.sh). WB_OPENCLAW_NPM_REGISTRY (else WB_NPM_REGISTRY,
// as the container reaches it) points the container's npm at another
// registry: a local one holding plugins/pack-npm.mjs's package, to bench
// it before it's published.

import { execFile, execFileSync } from "node:child_process";
import { existsSync, readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { promisify } from "node:util";
import type { Lane } from "../../src/lane.js";
import { addWithPicker, BlockedError, numberedOptions, selectInMenu, tail, usePicker, type DialogOption } from "../base.js";
import { ContainerDriver, inContainer, type ContainerSpec } from "../container.js";
import type { AnswerPolicy, Driver } from "../types.js";
import { sleep } from "../../src/tmux.js";

const exec = promisify(execFile);

/** Where Codex's app-server keeps an OpenClaw agent's threads (find -path). */
export const CODEX_ROLLOUTS = "*/agent/codex-home/sessions/*";

/** The registry the container's npm installs @wakeshift/openclaw from ("" for npm's own). */
function npmRegistry(): string {
  if (process.env.WB_OPENCLAW_NPM_REGISTRY) return process.env.WB_OPENCLAW_NPM_REGISTRY;
  return process.env.WB_NPM_REGISTRY ? inContainer(process.env.WB_NPM_REGISTRY).replace("localhost", "host.docker.internal") : "";
}

/**
 * OpenClaw's own question in its TUI ("Question 1/1 · <header>", numbered
 * options with "Other…", "↑/↓ or numbers select · Enter confirm") when the
 * agent asks about Wakeshift's install: its options, and which is yes and
 * which no. Null if that isn't what's showing.
 */
export function openclawInstallQuestion(t: string): { options: DialogOption[]; yes: DialogOption; no: DialogOption } | null {
  if (!/Question \d+\/\d+/.test(t) || !/numbers select/i.test(t)) return null;
  const lines = t.split("\n");
  // The live picker: its footer is the screen's last line, under its header.
  const foot = lines.findLastIndex((x) => /numbers select/i.test(x));
  const at = lines.findLastIndex((x, i) => i < foot && /Question \d+\/\d+/.test(x));
  if (at < 0 || lines.slice(foot + 1).some((x) => /[^\s─━]/.test(x))) return null;
  // The question itself, not its header (the agent's title for it).
  const asked = lines.slice(at + 1, foot).join("\n");
  if (!/install|hooks?|plugin|wakeshift/i.test(asked)) return null;
  const options = numberedOptions(asked);
  const no = options.find((o) => /^(no|don't|do not|skip|not now)\b/i.test(o.label));
  const yes = options.find((o) => o !== no && !/^other\b/i.test(o.label) && (/^yes\b/i.test(o.label) || /\binstall/i.test(o.label)));
  return yes && no ? { options, yes, no } : null;
}

export class OpenClaw extends ContainerDriver implements Driver {
  id = "openclaw";
  name = "OpenClaw";
  harness = "openclaw";
  secondMcp = true;
  trustStep = /reply|next (turn|message)|restart/i;
  spec: ContainerSpec = {
    image: "ghcr.io/openclaw/openclaw:latest",
    state: { name: "openclaw", to: "/home/node/.openclaw" },
    copyIn: "1000:1000", // node:node in the image
    env: {
      HOME: "/home/node",
      OPENCLAW_HOME: "/home/node",
      OPENCLAW_STATE_DIR: "/home/node/.openclaw",
      OPENCLAW_CONFIG_PATH: "/home/node/.openclaw/openclaw.json",
      OPENCLAW_WORKSPACE_DIR: "/home/node/.openclaw/workspace",
      OPENCLAW_DISABLE_BONJOUR: "1",
      TZ: "UTC",
      NODE_ENV: "production",
      ...(npmRegistry() ? { NPM_CONFIG_REGISTRY: npmRegistry() } : {}),
    },
    // The state refers to both (bench/state.sh, --secret-input-mode ref).
    secrets: ["OPENAI_API_KEY"],
    // The gateway's token (gateway.auth.token from env WS_TOKEN): without it every command fails.
    generated: ["WS_TOKEN"],
    // lan, as OpenClaw's Docker setup does: Wakeshift's installer then asks
    // about the hooks endpoint on that listener. WB_OPENCLAW_BIND=loopback
    // runs a gateway only this container reaches.
    cmd: ["node", "dist/index.js", "gateway", "--bind", process.env.WB_OPENCLAW_BIND || "lan", "--port", "18789"],
    port: 18789,
    chat: ["node", "dist/index.js", "tui"],
    // Its CLI (config set, the TUI) fails until the gateway has started.
    ready: /\[gateway\] ready\b/,
    version: ["node", "dist/index.js", "--version"],
  };

  /**
   * The bench's state has no MCP servers or channels (bench/state.sh); a
   * state that does (edited by hand, or someone else's) isn't used: a test
   * agent mustn't sign in as anyone or answer anyone's chats. The bench's
   * server is added below.
   */
  override async configure(l: Lane): Promise<void> {
    const dir = this.stateDir(l);
    const cfg = dir ? join(dir, "openclaw.json") : "";
    if (!dir || !existsSync(cfg)) throw new BlockedError("the bench's OpenClaw state has no openclaw.json: run bench/state.sh --refresh");
    const doc = JSON.parse(readFileSync(cfg, "utf8")) as Record<string, Record<string, unknown> | undefined>;
    const extra = [...Object.keys((doc.mcp?.servers ?? {}) as object).map((s) => `MCP server ${s}`), ...Object.keys(doc.channels ?? {}).map((c) => `channel ${c}`)];
    if (extra.length) throw new BlockedError(`the bench's OpenClaw state has ${extra.join(", ")}: run bench/state.sh --refresh`);
  }

  /**
   * The agent's question about the install in OpenClaw's own picker,
   * answered as the user would (policy.pick, default no). Unanswered, it
   * held the turn open until it timed out (wakebench R1/R4, November 3,
   * 2026), and the bench's typed reply went nowhere.
   */
  override async answer(l: Lane, screen: string, policy: AnswerPolicy): Promise<string | null> {
    const q = openclawInstallQuestion(tail(screen, 30));
    if (q) {
      const yes = policy.pick === "yes";
      await selectInMenu(l, q.options, yes ? q.yes : q.no);
      // A "Confirm answer" step can follow the choice.
      await sleep(600);
      const after = tail(await l.screen(40), 30);
      if (/Question \d+\/\d+/.test(after) && /Confirm answer/.test(after)) await l.tmux.keys(l.session, "Enter");
      l.state.picked = yes ? "yes" : "no";
      l.state.pickedAt = Date.now();
      l.prompted({ kind: "question", asks: "OpenClaw's question about the install", options: q.options.map((o) => o.label), chosen: (yes ? q.yes : q.no).label, scope: "n/a", sessionOffered: false });
      return `picked "${yes ? "yes, install" : "no"}" in OpenClaw's question`;
    }
    return super.answer(l, screen, policy);
  }

  /**
   * Its status line while a turn runs: a spinner, a word and a timer
   * ("⠼ streaming • 1m 49s | connected"; also sending, running, and a
   * whimsical word while the model thinks: "⠴ hobnobbing… • 6s"). Idle is
   * "connected | idle".
   */
  override busy(screen: string): boolean {
    return /^\s*[⠁-⣿]\s+\S[^\n•]*•\s*\d/m.test(tail(screen, 6));
  }

  /** A session of the lane's own, not the default (main). Restarts keep it. */
  override chat(l: Lane): string[] {
    return [...this.spec.chat, "--session", `wakebench-${l.ctx.run}-${l.id}`];
  }

  async addMcp(l: Lane, url: string): Promise<void> {
    // The picker's command, inside the container, with the gateway running
    // (as users run it), `openclaw` being the image's `node dist/index.js`.
    // A picker command that a running gateway refuses fails the lane.
    if (usePicker(l)) {
      const run = (args: string[]) => this.dockerExec(l, ["node", "dist/index.js", ...args]);
      await addWithPicker(l, { harness: "openclaw", program: "openclaw", url, run }).catch(async (err) => {
        await this.noteLogs(l);
        throw err;
      });
      if (l.mcp2) await addWithPicker(l, { harness: "openclaw", program: "openclaw", server: l.mcp2, url: inContainer(l.mcp2.url), run });
      return;
    }
    // The bench's own command (WB_MCP_ADD=bench), inside the container:
    // `config set`, which a running gateway applies (`mcp add` refuses
    // while it runs, since 2026.11).
    const add = (name: string, u: string) =>
      this.dockerExec(l, ["node", "dist/index.js", "config", "set", `mcp.servers.${name}`, JSON.stringify({ url: u, transport: "streamable-http" }), "--strict-json"]);
    const r = await add(l.mcpName, url);
    l.note(`openclaw config set mcp.servers: ${r.ok ? "ok" : "failed"}`, { out: r.out.slice(-600) });
    if (!r.ok) await this.noteLogs(l);
    // S15: the second app's server, added the same way, next to the first.
    if (l.mcp2) {
      const r2 = await add(l.mcp2.name, inContainer(l.mcp2.url));
      l.note(`openclaw config set mcp.servers.${l.mcp2.name}: ${r2.ok ? "ok" : "failed"}`, { out: r2.out.slice(-600) });
      if (!r2.ok) await this.noteLogs(l);
    }
  }

  /**
   * The install the agent didn't run, as the user would: OpenClaw's own
   * plugin installer, as the core offers it (harnesses.OfferCommand), with
   * the issuer (the bench's core isn't the plugin's default). The bench's
   * gateway listens on the container's network (--bind lan), so the plugin
   * turns hooks on only with the user's yes, which the user (the bench)
   * gives: allowNetworkHooks.
   */
  installCommand(_l: Lane, core: string): string {
    // Before a version is offered, Wakeshift's installer, as the core's offer.
    if (!process.env.WB_OPENCLAW_PLUGIN) return `curl -fsSL ${core}/install/openclaw.sh | WAKESHIFT_OPENCLAW_NETWORK_HOOKS=1 sh`;
    return (
      `openclaw config set plugins.entries.wakeshift.config.allowNetworkHooks true --strict-json && ` +
      `openclaw config set plugins.entries.wakeshift.config.issuer '"${core.replace(/\/$/, "")}"' --strict-json && ` +
      `openclaw plugins install npm:@wakeshift/openclaw@${process.env.WB_OPENCLAW_PLUGIN} --force --accept-capabilities && openclaw plugins enable wakeshift --accept-capabilities`
    );
  }

  /**
   * After the install: which runtime ran the conversation's turns so far,
   * for the lane's notes. Nothing is restarted.
   */
  async afterInstall(l: Lane): Promise<string[]> {
    const n = await this.codexThreads(l);
    return [
      n === null
        ? "couldn't read the agent's Codex home for the runtime"
        : n > 0
          ? `turns ran in Codex's app-server (${n} Codex thread${n === 1 ? "" : "s"} in the agent's Codex home)`
          : "no Codex thread in the agent's Codex home: turns ran in OpenClaw's own runtime",
    ];
  }

  /**
   * How many Codex threads the agent's turns ran in: the rollout files in
   * each agent's Codex home (`agents/<id>/agent/codex-home/sessions`), which
   * Codex's app-server writes for a turn and OpenClaw's own runtime never
   * does. The gateway's log names the app-server at start whatever runs the
   * turns. Null if it can't be read.
   */
  async codexThreads(l: Lane): Promise<number | null> {
    if (!l.state.container) return null;
    try {
      const r = await exec("docker", ["exec", this.container(l), "find", "/home/node/.openclaw/agents", "-path", CODEX_ROLLOUTS, "-name", "rollout-*.jsonl"], { timeout: 30_000 });
      return r.stdout.split("\n").filter(Boolean).length;
    } catch {
      return null;
    }
  }

  /** The plugin, inside the container (the state is copied in, not mounted): npm's install, or the earlier installer's state directory. */
  installed(l: Lane): string[] {
    if (!l.state.container) return [];
    try {
      const out = execFileSync("docker", ["exec", this.container(l), "sh", "-c", "ls -d /home/node/.openclaw/npm/projects/wakeshift-openclaw-* /home/node/.openclaw/wakeshift 2>/dev/null || true"], { timeout: 20_000 }).toString();
      return out.split("\n").filter(Boolean).map((p) => p.replace("/home/node", "~"));
    } catch {
      return [];
    }
  }
}

/**
 * OpenClaw with its turns run by Codex: the bundled `codex` plugin's
 * app-server as the agent's runtime, required by a model-scoped
 * `agentRuntime.id: "codex"` on the state's model (OpenClaw's own setting,
 * from its documentation on agent runtimes), so a turn fails rather
 * than fall back to OpenClaw's runtime. Everything else is the openclaw
 * lane's. The plain lane leaves the runtime to OpenClaw (auto).
 */
export class OpenClawCodex extends OpenClaw {
  override id = "openclaw-codex";
  override name = "OpenClaw (Codex runtime)";

  override async configure(l: Lane): Promise<void> {
    await super.configure(l);
    const cfg = join(this.stateDir(l)!, "openclaw.json");
    const doc = JSON.parse(readFileSync(cfg, "utf8")) as OpenClawConfig;
    const model = requireCodexRuntime(doc);
    if (!model) throw new BlockedError("the bench's OpenClaw state names no model (agents.defaults.model.primary): run bench/state.sh --refresh");
    writeFileSync(cfg, JSON.stringify(doc, null, 2) + "\n", { mode: 0o600 });
    l.note(`required Codex's runtime for ${model} (agents.defaults.models["${model}"].agentRuntime.id: "codex")`);
  }

  override async versionIn(l: Lane): Promise<string | null> {
    const v = await super.versionIn(l);
    return v ? `${v} · Codex runtime` : v;
  }
}

/** The parts of openclaw.json the Codex lane changes. */
export interface OpenClawConfig {
  plugins?: { entries?: Record<string, Record<string, unknown>> } & Record<string, unknown>;
  agents?: { defaults?: { model?: { primary?: string } | string; models?: Record<string, Record<string, unknown>> } & Record<string, unknown> } & Record<string, unknown>;
  [k: string]: unknown;
}

/**
 * Requires Codex's runtime for the state's default model, in place: the
 * model's `agentRuntime.id: "codex"`, and the bundled `codex` plugin
 * enabled. Returns the model, or null if the state names none.
 */
export function requireCodexRuntime(doc: OpenClawConfig): string | null {
  const agents = (doc.agents ??= {});
  const defaults = (agents.defaults ??= {});
  const model = typeof defaults.model === "string" ? defaults.model : defaults.model?.primary;
  if (!model) return null;
  const models = (defaults.models ??= {});
  models[model] = { ...(models[model] ?? {}), agentRuntime: { id: "codex" } };
  const entries = ((doc.plugins ??= {}).entries ??= {});
  entries.codex = { ...(entries.codex ?? {}), enabled: true };
  return model;
}
