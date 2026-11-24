// Hermes Agent in its own container (see ../container.ts), driven through
// `hermes chat`. Install: Wakeshift's Python plugin
// ($HERMES_HOME/plugins/wakeshift). Enabling it reloads the gateway's
// plugins, but a running `hermes chat` keeps the plugins it started with,
// so after the install the chat restarts (`--continue`), as a user would.
//
// The image is pinned to a release (HERMES_IMAGE; WB_HERMES_IMAGE to run
// another), the same one bench/state.sh makes the state with, so every
// machine runs the same Hermes: `latest` moved from 0.22.2 to 0.22.5 under
// one machine and not another, and 0.22.3 changed how CLI chats are woken.

import { execFile } from "node:child_process";
import { createHash } from "node:crypto";
import { existsSync, readdirSync, readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { promisify } from "node:util";
import { parse, stringify } from "yaml";
import type { Lane } from "../../src/lane.js";
import { pickerFile } from "../../src/picker.js";
import { numberedOptions, selectInMenu, tail, usePicker, type DialogOption } from "../base.js";
import { ContainerDriver, inContainer, type ContainerSpec } from "../container.js";
import type { Received } from "../../src/arrivals.js";
import type { AnswerPolicy, Driver } from "../types.js";

const exec = promisify(execFile);

/** Hermes's images; a release's tag is its version with a v ("v0.22.5"). */
export const HERMES_REPO = "nousresearch/hermes-agent";

/** The Hermes release the bench runs (keep bench/state.sh's hermes_image the same). */
export const HERMES_IMAGE = `${HERMES_REPO}:v0.22.5`;

/**
 * The TUI's status bar ("─ ready │ gpt 6 luna │ … │ 4m 7s │ ✓ 3m 33s │ …"):
 * its first field is the session's state, "ready" when idle, and its timers
 * tick every second.
 */
export const HERMES_TUI_STATUS = /^\s*─ (\S[^│]*?)\s*│.*│/;

/** The TUI's scrollbar, drawn at the end of every line. */
export const HERMES_TUI_GUTTER = /\s*[┃│]\s*$/;

/** Whether the TUI's status bar (the last one on screen) says a turn is running: any state but "ready". */
export function hermesTuiBusy(screen: string): boolean {
  const bars = tail(screen, 15).split("\n").map((l) => HERMES_TUI_STATUS.exec(l)?.[1]).filter((s): s is string => !!s);
  const last = bars.pop();
  return last !== undefined && !/^ready\b/i.test(last);
}

/** What Hermes shows under its input box while a turn runs (with or without "esc to interrupt"). */
export const HERMES_BUSY = /type to redirect this turn|Tab queues it/i;

/**
 * Hermes's question picker about Wakeshift's install, on the bottom of the
 * screen: its numbered options (Other aside) with a yes (to install) and a
 * no. Null if that isn't what's showing.
 */
export function hermesInstallQuestion(t: string): { options: DialogOption[]; yes: DialogOption; no: DialogOption } | null {
  const classic = /needs your input/i.test(t) && /Enter to lock|to select/i.test(t);
  // The TUI's: "ask 1 question", "▸ 1. Yes (Recommended)" (▸ the cursor),
  // "0/1 answered · ↑/↓ select · Enter confirm and continue".
  const tui = /\d+\/\d+ answered/i.test(t) && /Enter confirm/i.test(t);
  if (!classic && !tui) return null;
  if (!/install|hooks?|plugin|wakeshift/i.test(t)) return null;
  const options = numberedOptions(tui ? t.replace(/^(\s*)▸(?=\s*\d[.)]\s)/gm, "$1❯") : t);
  const no = options.find((o) => /^(no|don't|do not|skip|not now)\b/i.test(o.label));
  const yes = options.find((o) => o !== no && !/^other\b/i.test(o.label) && (/^yes\b/i.test(o.label) || /\binstall/i.test(o.label)));
  return yes && no ? { options, yes, no } : null;
}

/**
 * WB_HERMES_TUI=1 drives Hermes's Ink TUI (`hermes chat --tui`) instead of
 * the classic CLI: a local session of the same gateway, whose hooks say
 * platform "tui", and which shows a plugin's turn as it runs.
 */
export function hermesTui(): boolean {
  return process.env.WB_HERMES_TUI === "1" && !hermesDesktop();
}

/**
 * WB_HERMES_SURFACE=desktop: the chat says it's Hermes Desktop's
 * (`hermes chat --source gui`, the label Hermes Desktop creates its chats
 * with: `--source desktop` is invalid_params), so its hooks say platform
 * "desktop", as a chat in Nous's Desktop app does. The classic CLI drives
 * it, so a woken turn isn't painted (unpaintedWakes) and its arrival is
 * graded from the agent's report_seen and Hermes's own record (state.db).
 * Reporting-only.
 */
export function hermesDesktop(): boolean {
  return process.env.WB_HERMES_SURFACE === "desktop";
}

/**
 * The surface Hermes recorded for the bench's chats (state.db's sessions:
 * created_source, else source), newest first, from the rows desktopCheck
 * reads; what the Desktop variant checks it ran as.
 */
export function hermesSurfaces(rows: { source: string | null; created_source: string | null }[]): string[] {
  return rows.map((r) => (r.created_source || r.source || "").toLowerCase()).filter((s) => s);
}

/** Hermes's container: its gateway (`gateway run`) on the run's copy of the bench's state, chats with `hermes chat`. */
export function hermesSpec(): ContainerSpec {
  return {
    image: process.env.WB_HERMES_IMAGE || HERMES_IMAGE,
    state: { name: "hermes", to: "/opt/data" },
    volume: "10000:10000", // hermes:hermes in the image
    env: { HERMES_HOME: "/opt/data", PYTHONUNBUFFERED: "1" },
    secrets: ["OPENAI_API_KEY"],
    cmd: ["gateway", "run"],
    port: 8642,
    chat: ["/opt/hermes/bin/hermes", "chat"],
    resume: ["--continue"],
    version: ["/opt/hermes/bin/hermes", "--version"],
  };
}

export class Hermes extends ContainerDriver implements Driver {
  id = "hermes";
  name = "Hermes Agent";
  harness = "hermes";
  secondMcp = true;
  trustStep = /resum|--continue|new chat|reply|next\s+(turn|message)|send\s+any/i;
  spec: ContainerSpec = hermesSpec();

  /** The TUI repaints only what changed: its log is fragments (Driver.paneLog). */
  get paneLog(): boolean | undefined {
    return hermesTui() ? false : undefined;
  }

  /** The TUI's scrollbar at the end of every line (Driver.gutter). */
  get gutter(): RegExp | undefined {
    return hermesTui() ? HERMES_TUI_GUTTER : undefined;
  }

  get volatile(): RegExp | undefined {
    return hermesTui() ? HERMES_TUI_STATUS : undefined;
  }

  /** The classic CLI, with WB_HERMES_TUI=1 the Ink TUI (`hermes chat --tui`), with WB_HERMES_SURFACE=desktop a Desktop chat. */
  override chat(_l: Lane): string[] {
    if (hermesDesktop()) return [...this.spec.chat, "--source", "gui"];
    return hermesTui() ? [...this.spec.chat, "--tui"] : this.spec.chat;
  }

  override async versionIn(l: Lane): Promise<string | null> {
    const v = await super.versionIn(l);
    return v && hermesDesktop() ? `${v} · Desktop` : v && hermesTui() ? `${v} · TUI` : v;
  }

  /**
   * Hermes's own question ("Hermes Agent needs your input", numbered
   * options with "Other (type your answer)", ↑/↓ and Enter): the agent
   * asking about the install with a picker, answered as the user would
   * (policy.pick, default no), as Claude Code's AskUserQuestion is. Typing
   * the reply into the chat instead left the question open and the bench
   * didn't count it as asked with a picker.
   */
  override async answer(l: Lane, screen: string, policy: AnswerPolicy): Promise<string | null> {
    const q = hermesInstallQuestion(tail(screen, 30));
    if (q) {
      const yes = policy.pick === "yes";
      await selectInMenu(l, q.options, yes ? q.yes : q.no);
      l.state.picked = yes ? "yes" : "no";
      l.state.pickedAt = Date.now();
      l.prompted({ kind: "question", asks: "Hermes's question about the install", options: q.options.map((o) => o.label), chosen: (yes ? q.yes : q.no).label, scope: "n/a", sessionOffered: false });
      return `picked "${yes ? "yes, install" : "no"}" in Hermes's question`;
    }
    return super.answer(l, screen, policy);
  }

  /**
   * Busy: also while a turn's hint shows under the input box ("type to
   * redirect this turn", "Tab queues it"), which Hermes 0.22.5 shows in
   * place of "esc to interrupt" during a turn; without it the bench took a
   * running turn for idle and typed over it.
   */
  override busy(screen: string): boolean {
    if (hermesTui() && hermesTuiBusy(screen)) return true;
    return super.busy(screen) || HERMES_BUSY.test(tail(screen, 15));
  }

  /**
   * Hermes 0.22.3 and later run a plugin's turn in a CLI chat in the gateway,
   * and the classic CLI attached to it paints only turns it submitted or a
   * core producer's (with a display card, gateway.session_automation
   * admission_display): a woken turn runs, and is in the history, but its
   * GOT line never shows in the pane. Only once the install's plugin runs
   * there (phase b), not on 0.22.2. The TUI (WB_HERMES_TUI=1) paints them.
   */
  unpaintedWakes(l: Lane): boolean {
    if (hermesTui()) return false;
    return this.installed(l).length > 0 && this.existsIn(l, "/opt/hermes/hermes_cli/plugin_session.py");
  }

  async afterInstall(l: Lane): Promise<string[]> {
    await l.waitIdle({ allowInstall: true }, 60_000);
    const notes = await this.overlayPlugin(l);
    await this.stop(l);
    await this.start(l, { resume: true });
    return [...notes, "restarted hermes chat (--continue) to load the plugin", ...(await this.desktopCheck(l))];
  }

  /**
   * With WB_HERMES_SURFACE=desktop: the surface Hermes recorded for the
   * lane's chats (state.db), so a run that didn't run as Desktop says so.
   */
  async desktopCheck(l: Lane): Promise<string[]> {
    if (!hermesDesktop()) return [];
    const rows = await this.queryState(l, "select source,created_source from sessions order by started_at desc limit 5", ["source", "created_source"]);
    const surfaces = rows ? hermesSurfaces(rows as { source: string | null; created_source: string | null }[]) : null;
    const desktop = !!surfaces?.length && (surfaces[0] === "desktop" || surfaces[0] === "gui");
    l.state.hermesSurface = surfaces?.[0] ?? null;
    return [surfaces === null ? "Desktop variant: Hermes's sessions couldn't be read" : desktop ? `Desktop variant: Hermes recorded the chat as ${surfaces[0]}` : `Desktop variant: Hermes recorded the chat as ${surfaces[0] ?? "nothing"}, not desktop`];
  }

  /**
   * WB_HERMES_PLUGIN=<dir> (plugins/hermes/wakeshift in a checkout): try that
   * plugin's code against a target that serves another (a hosted core, before a
   * deploy). After the install, its Python files replace the installed ones
   * (the installed _version.py and plugin.yaml stay, so it names the
   * target's version), and, as the installer from a checkout does, Hermes's
   * allow_gateway_injection is set for it, unless WB_HERMES_ALLOW=0.
   */
  async overlayPlugin(l: Lane): Promise<string[]> {
    const src = process.env.WB_HERMES_PLUGIN;
    if (!src || this.installed(l).length === 0) return [];
    const dir = "/opt/data/plugins/wakeshift";
    const files = readdirSync(src).filter((f) => f.endsWith(".py") && f !== "_version.py");
    for (const f of files) await exec("docker", ["cp", join(src, f), `${this.container(l)}:${dir}/${f}`], { timeout: 60_000 });
    await exec("docker", ["exec", "-u", "0", this.container(l), "chown", "-R", "10000:10000", dir], { timeout: 60_000 });
    const notes = [`replaced the installed plugin's code with ${src} (${files.join(", ")})`];
    if (process.env.WB_HERMES_ALLOW !== "0") {
      await exec("docker", ["exec", "-u", "hermes", this.container(l), "/opt/hermes/bin/hermes", "config", "set", "plugins.entries.wakeshift.allow_gateway_injection", "true"], { timeout: 60_000 });
      notes.push("set plugins.entries.wakeshift.allow_gateway_injection: true, as the installer from this checkout does");
    }
    return notes;
  }

  override async configure(l: Lane): Promise<void> {
    // mcp_servers in the run's config.yaml (what `hermes mcp add` writes):
    // only the bench's. The bench's state has none (bench/state.sh); any
    // other is replaced, so a test agent can't sign in anywhere else.
    const dir = this.stateDir(l);
    if (!dir) return;
    const cfg = join(dir, "config.yaml");
    const doc = (existsSync(cfg) ? (parse(readFileSync(cfg, "utf8")) as Record<string, unknown>) : {}) ?? {};
    const dropped = Object.keys((doc.mcp_servers ?? {}) as Record<string, unknown>);
    // The picker's config.yaml text (its file step), parsed and put in: the
    // picker's command (`hermes mcp add`) can stop to ask about
    // authentication, which the bench can't answer before the container
    // starts. A text that isn't YAML, or has no entry for the server, fails the lane.
    const fromPicker = (name: string, url: string): Record<string, unknown> => {
      const f = pickerFile("hermes", { name, url, noAuth: true });
      let entry: unknown;
      try {
        entry = ((parse(f.text) as { mcp_servers?: Record<string, unknown> } | null)?.mcp_servers ?? {})[name];
      } catch (err) {
        throw new Error(`the picker's Hermes ${f.path} isn't YAML: ${err instanceof Error ? err.message : String(err)}`);
      }
      if (!entry || typeof entry !== "object") throw new Error(`the picker's Hermes ${f.path} has no mcp_servers.${name}`);
      return entry as Record<string, unknown>;
    };
    const entry = (picker: boolean, name: string, url: string) => (picker ? fromPicker(name, url) : { url });
    doc.mcp_servers = {
      [l.mcpName]: entry(usePicker(l), l.mcpName, inContainer(l.mcpUrl)),
      // S15: the second app's server, next to the first.
      ...(l.mcp2 ? { [l.mcp2.name]: entry(usePicker(l, l.mcp2), l.mcp2.name, inContainer(l.mcp2.url)) } : {}),
    };
    if (usePicker(l)) l.note("the picker's config.yaml entry for the MCP server (its file step)");
    if (dropped.length) l.note("replaced MCP servers in the bench's Hermes state", { dropped });
    writeFileSync(cfg, stringify(doc), { mode: 0o600 });
  }

  async addMcp(l: Lane, url: string): Promise<void> {
    l.note(`MCP server in config.yaml (mcp_servers.${l.mcpName})`, { url });
    if (l.mcp2) l.note(`second MCP server in config.yaml (mcp_servers.${l.mcp2.name})`, { url: inContainer(l.mcp2.url) });
  }

  /**
   * What reached the model, from the container's session store
   * ($HERMES_HOME/state.db's messages, read-only, only rows with a bench
   * code: the toy app's WB-…, or the codes given, digits only); null if it
   * can't be read (the container is gone, no Python).
   */
  async received(l: Lane, codes?: string[]): Promise<Received[] | null> {
    if (!l.state.container) return null;
    const likes = codes?.length ? codes.filter((c) => /^\d+$/.test(c)) : null;
    if (likes && !likes.length) return [];
    const where = likes ? `(${likes.map((c) => `content like '%%${c}%%'`).join(" or ")})` : "content like '%%WB-%%'";
    const py = `
import json,sqlite3
c=sqlite3.connect("file:/opt/data/state.db?mode=ro",uri=True)
cols=[r[1] for r in c.execute("pragma table_info(messages)")]
skip="and coalesce(_compressed_summary,0)=0" if "_compressed_summary" in cols else ""
rows=c.execute("select id,role,content,tool_name,timestamp from messages where ${where} %s order by id" % skip).fetchall()
print(json.dumps([dict(zip(["id","role","content","tool_name","timestamp"],r)) for r in rows]))
`;
    try {
      const { stdout } = await exec(
        "docker",
        ["exec", this.container(l), "sh", "-c", `for p in /opt/hermes/.venv/bin/python /opt/hermes/bin/python python3; do [ -x "$p" ] || command -v "$p" >/dev/null 2>&1 && exec "$p" -c "$0"; done; exit 127`, py],
        { timeout: 60_000, maxBuffer: 32 << 20 },
      );
      return hermesReceived(JSON.parse(stdout.trim().split("\n").pop() ?? "[]") as HermesRow[]);
    } catch {
      return null;
    }
  }

  /** Rows of a read-only query on the container's state.db (cols: the names to give them); null if it can't be read. */
  async queryState(l: Lane, sql: string, cols: string[]): Promise<Record<string, unknown>[] | null> {
    if (!l.state.container) return null;
    const py = `
import json,sqlite3
c=sqlite3.connect("file:/opt/data/state.db?mode=ro",uri=True)
print(json.dumps([dict(zip(${JSON.stringify(cols)},r)) for r in c.execute(${JSON.stringify(sql)}).fetchall()]))
`;
    try {
      const { stdout } = await exec(
        "docker",
        ["exec", this.container(l), "sh", "-c", `for p in /opt/hermes/.venv/bin/python /opt/hermes/bin/python python3; do [ -x "$p" ] || command -v "$p" >/dev/null 2>&1 && exec "$p" -c "$0"; done; exit 127`, py],
        { timeout: 60_000, maxBuffer: 1 << 20 },
      );
      return JSON.parse(stdout.trim().split("\n").pop() ?? "[]") as Record<string, unknown>[];
    } catch {
      return null;
    }
  }

  installed(l: Lane): string[] {
    // In the container's state volume, not the host's copy.
    return this.existsIn(l, "/opt/data/plugins/wakeshift") ? ["$HERMES_HOME/plugins/wakeshift"] : [];
  }
}

/** A row of Hermes's messages table (state.db). */
export interface HermesRow {
  id: number;
  role: string;
  content: string | null;
  tool_name: string | null;
  /** Seconds since the epoch. */
  timestamp: number | null;
}

/**
 * What reached the model, from Hermes's messages: user messages (the
 * plugin's wakes are user turns) and MCP tool results (watch: an inbox
 * result), not the model's own messages or other tools' output. Keyed by
 * time and content, so a row copied into a continued session counts once.
 */
export function hermesReceived(rows: HermesRow[]): Received[] {
  const out: Received[] = [];
  for (const r of rows) {
    const text = r.content ?? "";
    const at = typeof r.timestamp === "number" ? Math.round(r.timestamp * 1000) : null;
    const key = `${r.timestamp ?? r.id}:${createHash("sha256").update(text).digest("hex").slice(0, 16)}`;
    if (r.role === "user") out.push({ key, at, via: "user", text });
    else if (r.role === "tool" && (r.tool_name ?? "").startsWith("mcp__")) out.push({ key, at, via: `tool_result:${r.tool_name}`, text });
  }
  return out;
}
