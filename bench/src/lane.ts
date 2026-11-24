// A lane: one harness, one phase of a run (a = nothing installed, b = the
// install), with its own temp HOME, tmux session, MCP URL and topic.
// Everything the scenarios do to a harness goes through here: snapshots
// (redacted ANSI + HTML per step), waiting for text, answering dialogs.

import { execFile } from "node:child_process";
import { closeSync, existsSync, mkdirSync, openSync, readSync, statSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { promisify } from "node:util";
import { QuotaError, quotaPrompt } from "../drivers/base.js";
import type { AnswerPolicy, Driver, PromptRecord } from "../drivers/types.js";
import { ansiToHtml, logText } from "./ansi.js";
import { SECOND_MCP_NAME, type Bench } from "./core.js";
import { makeHome, type Home } from "./home.js";
import type { Step } from "./model.js";
import type { Procs } from "./procs.js";
import { credentialKinds, redact, redactDeep, redactPlain } from "./redact.js";
import { savedRuleCredentials } from "./visible.js";
import { sleep, type Tmux } from "./tmux.js";

const exec = promisify(execFile);

export interface RunCtx {
  run: string;
  dir: string;
  repo: string;
  tmux: Tmux;
  procs: Procs;
  bench: Bench;
  log: (msg: string) => void;
  /** Multiplies every wait (slow models, a loaded machine). */
  slow: number;
  /**
   * Answer each dialog (approvals, pickers, trust) this many ms after it
   * appears, not at once (--approve-delay, WB_APPROVE_DELAY): a user who
   * isn't watching the terminal. 0 or unset: at once.
   */
  approveDelayMs?: number;
}

/** A lane pointed somewhere other than the bench's own app (S11: a real app, signed in as a player). */
export interface LaneOpts {
  /** The lane's ID (default: the driver's, plus -i for phase b). */
  id?: string;
  mcpUrl?: string;
  /** The MCP server's name in the harness (default the bench's: wakebench, or an external app's own). */
  mcpName?: string;
  /** A bearer token the harness sends to the MCP server (never logged). */
  mcpToken?: string;
  /** S15: add the run's second toy app as a second MCP server (wakebench2), next to the first. */
  second?: boolean;
}

/** A second MCP server in the harness (S15's second app): its name and URL. */
export interface SecondMcp {
  name: string;
  url: string;
}

export class Lane {
  readonly id: string;
  readonly session: string;
  readonly home: Home;
  readonly mcpUrl: string;
  readonly mcpName: string;
  readonly mcpToken: string | undefined;
  /** S15's second app's MCP server, which the driver adds next to the first; null without one. */
  readonly mcp2: SecondMcp | null;
  readonly logFile: string;
  steps: Step[] = [];
  private t0 = Date.now();
  private shot = 0;
  /** Dialogs answered in this lane, in order. */
  answered: { at: string; what: string; dialog?: string }[] = [];
  /** The dialogs answered, as the driver read them (what it asked, the options, the choice): S12 counts them. */
  prompts: PromptRecord[] = [];
  /** Facts a driver keeps between calls (its own state). */
  state: Record<string, unknown> = {};
  /**
   * Wakeshift credentials the user was shown or a harness kept: in a dialog
   * the lane answered, or in a rule file in the temp HOME (by kind, never
   * the value). Each scenario fails on those in its time (Scenarios.visibility).
   */
  exposures: { at: string; where: string; kinds: string[] }[] = [];

  constructor(
    readonly ctx: RunCtx,
    readonly driver: Driver,
    readonly phase: "a" | "b",
    opts: LaneOpts = {},
  ) {
    this.id = opts.id ?? (phase === "a" ? driver.id : `${driver.id}-i`);
    this.session = `wb-${ctx.run}-${this.id}`;
    this.home = makeHome(ctx.dir, this.id);
    this.mcpUrl = opts.mcpUrl ?? ctx.bench.mcpUrl(this.id);
    this.mcpName = opts.mcpName ?? ctx.bench.mcpName ?? "wakebench";
    this.mcpToken = opts.mcpToken;
    const app2 = ctx.bench.app2;
    this.mcp2 = opts.second && app2 ? { name: SECOND_MCP_NAME, url: app2.mcpUrl(this.id) } : null;
    mkdirSync(join(ctx.dir, "logs"), { recursive: true });
    mkdirSync(join(ctx.dir, "snapshots", this.id), { recursive: true });
    this.logFile = join(ctx.dir, "logs", `${this.id}.log`);
  }

  get tmux() {
    return this.ctx.tmux;
  }

  log(msg: string) {
    this.ctx.log(`[${this.id}] ${msg}`);
  }

  /** Starts a new timeline (one per scenario). */
  begin(): Step[] {
    this.steps = [];
    this.t0 = Date.now();
    return this.steps;
  }

  /** A step without a snapshot. */
  note(label: string, data?: unknown): Step {
    const masked = data === undefined ? undefined : (JSON.parse(this.mask(JSON.stringify(data) ?? "null")) as unknown);
    const s: Step = { at: new Date().toISOString(), t: Date.now() - this.t0, label: redactPlain(this.mask(label)), ...(masked === undefined ? {} : { data: redactDeep(masked) }) };
    this.steps.push(s);
    return s;
  }

  /** Records a dialog the driver answered (redacted: never a link, key or token). */
  prompted(r: Omit<PromptRecord, "at">): PromptRecord {
    const clean = (s: string) => redactPlain(this.mask(s)).slice(0, 500);
    const phase = typeof this.state.phase === "string" ? this.state.phase : undefined;
    const p: PromptRecord = { ...r, at: new Date().toISOString(), asks: clean(r.asks), options: r.options.map(clean), chosen: clean(r.chosen), ...(phase ? { phase } : {}) };
    this.prompts.push(p);
    return p;
  }

  private rulesSeen = new Set<string>();

  /** Rule files in the temp HOME that now hold a Wakeshift credential, each recorded once as an exposure. */
  checkSavedRules(): void {
    for (const f of savedRuleCredentials(this.home.dir, (s) => this.mask(s))) {
      const key = `${f.file}:${f.kinds.join(",")}`;
      if (this.rulesSeen.has(key)) continue;
      this.rulesSeen.add(key);
      this.exposures.push({ at: new Date().toISOString(), where: `a saved rule (~/${f.file})`, kinds: f.kinds });
      this.note(`~/${f.file} holds a Wakeshift credential: ${f.kinds.join(", ")}`);
    }
  }

  /** The lane's MCP token taken out of text (it isn't shaped like any secret redaction knows). */
  mask(s: string): string {
    return this.mcpToken ? s.split(this.mcpToken).join("‹token›") : s;
  }

  /** A step with a snapshot of the pane (redacted), as .ansi and .html. */
  async snap(label: string, data?: unknown): Promise<Step> {
    const s = this.note(label, data);
    const raw = await this.capture();
    return raw === null ? s : this.keepShot(s, label, raw);
  }

  /** The pane's last 400 lines with their colors (not redacted), or null if it's gone. */
  private async capture(): Promise<string | null> {
    if (!(await this.tmux.exists(this.session))) return null;
    try {
      return await this.tmux.capture(this.session, 400);
    } catch {
      return null;
    }
  }

  /** Writes raw (a capture) as s's snapshot, redacted. */
  private keepShot(s: Step, label: string, raw: string): Step {
    const clean = redact(this.mask(raw.replace(/\n+$/, "\n")));
    const n = String(++this.shot).padStart(3, "0");
    const slug = label.toLowerCase().replace(/[^a-z0-9]+/g, "-").replace(/^-|-$/g, "").slice(0, 40);
    const rel = join("snapshots", this.id, `${n}-${slug}`);
    writeFileSync(join(this.ctx.dir, rel + ".ansi"), clean, { mode: 0o600 });
    writeFileSync(join(this.ctx.dir, rel + ".html"), `<pre class="term">${ansiToHtml(clean)}</pre>\n`, { mode: 0o600 });
    s.snapshot = rel;
    return s;
  }

  /** The pane as plain text (not redacted: for matching only). */
  async screen(history = 2000): Promise<string> {
    try {
      const text = await this.tmux.text(this.session, history);
      const g = this.driver?.gutter;
      return g ? text.split("\n").map((l) => l.replace(g, "")).join("\n") : text;
    } catch {
      return "";
    }
  }

  /**
   * text without the lines the driver calls volatile (Hermes's TUI status
   * bar, which also shows the chat's title, "Monitor build 412 for
   * failure"): what the agent and the user wrote.
   */
  prose(text: string): string {
    const v = this.driver?.volatile;
    return v ? text.split("\n").filter((l) => !v.test(l)).join("\n") : text;
  }

  /** What the pane printed since byte offset (the pipe-pane log), plain. */
  logSince(offset: number): string {
    // A renderer that repaints only what changed (Hermes's TUI) leaves a log
    // of fragments, not text: the pane itself is read instead.
    if (this.driver?.paneLog === false) return "";
    if (!existsSync(this.logFile)) return "";
    const size = statSync(this.logFile).size;
    if (size <= offset) return "";
    const fd = openSync(this.logFile, "r");
    try {
      const len = Math.min(size - offset, 8 << 20);
      const buf = Buffer.alloc(len);
      readSync(fd, buf, 0, len, size - len);
      return logText(buf.toString("utf8"));
    } finally {
      closeSync(fd);
    }
  }

  /** What the pane printed between two byte offsets of its log (at most 8 MB, from the end of the range), plain. */
  logRange(from: number, to: number): string {
    if (this.driver?.paneLog === false || !existsSync(this.logFile)) return "";
    const end = Math.min(to, statSync(this.logFile).size);
    if (end <= from) return "";
    const fd = openSync(this.logFile, "r");
    try {
      const len = Math.min(end - from, 8 << 20);
      const buf = Buffer.alloc(len);
      readSync(fd, buf, 0, len, end - len);
      return logText(buf.toString("utf8"));
    } finally {
      closeSync(fd);
    }
  }

  logSize(): number {
    return existsSync(this.logFile) ? statSync(this.logFile).size : 0;
  }

  private lastTrack = 0;
  /** The dialog the delayed-approval variant is waiting out, and since when. */
  private pendingDialog: { key: string; since: number } | null = null;

  /**
   * Whether the driver would answer a dialog on this screen, without
   * answering it: its answer run against a copy of the lane whose tmux
   * types nothing and whose state, prompts and steps are its own.
   */
  private async dryAnswer(screen: string, policy: AnswerPolicy): Promise<boolean> {
    const real = this.tmux;
    const tmux = new Proxy(real, {
      get: (t, k) => {
        if (k === "keys" || k === "type") return async () => {};
        const v = Reflect.get(t, k, t);
        return typeof v === "function" ? v.bind(t) : v;
      },
    });
    const dry = Object.create(this) as Lane;
    Object.defineProperty(dry, "tmux", { get: () => tmux });
    dry.state = { ...this.state };
    dry.prompts = [];
    dry.answered = [];
    dry.steps = [];
    dry.snap = async (label: string) => ({ at: new Date().toISOString(), t: 0, label });
    // Nothing else that acts: answers type keys and record what they did,
    // and a driver that did more would fail here (and be answered at once).
    const refuse = async () => {
      throw new Error("not in a dry run");
    };
    dry.run = refuse;
    dry.say = refuse;
    dry.waitFor = refuse;
    dry.waitIdle = refuse;
    try {
      return (await this.driver.answer(dry, screen, policy)) !== null;
    } catch {
      return false;
    }
  }

  private async answerOnce(policy: AnswerPolicy, screen: string): Promise<boolean> {
    if (this.driver.track && Date.now() - this.lastTrack > 4000) {
      this.lastTrack = Date.now();
      try {
        this.driver.track(this);
      } catch {
        // ps failed: next time.
      }
    }
    // A dialog the bench keeps answering isn't going away: stop, so a
    // misread screen can't turn into a stream of keystrokes.
    // The same answer to the same dialog (its last lines), that is: a busy
    // agent's many tool approvals each count as their own.
    const dialog = this.driver.dialogOf?.(screen) ?? screen.trimEnd().split("\n").slice(-16).join("\n");
    const recent = this.answered.filter((a) => Date.now() - Date.parse(a.at) < 30_000);
    if (recent.length >= 4 && recent.every((a) => a.what === recent[0].what && a.dialog === recent[0].dialog)) {
      if (!this.state.loopNoted) {
        this.state.loopNoted = true;
        await this.snap(`stopped answering: "${recent[0].what}" four times in 30s`);
      }
      return false;
    }
    // The delayed-approval variant: a dialog is answered only once it has
    // been on screen for approveDelayMs (seen by a dry run of the driver's
    // answer, which types nothing).
    const delay = this.ctx.approveDelayMs ?? 0;
    if (delay > 0) {
      const key = dialog.replace(/\d+/g, "#");
      if (this.pendingDialog?.key !== key) {
        this.pendingDialog = (await this.dryAnswer(screen, policy)) ? { key, since: Date.now() } : null;
        return false;
      }
      if (Date.now() - this.pendingDialog.since < delay) return false;
      this.pendingDialog = null;
    }
    const before = this.prompts.length;
    // The dialog as it stood, kept only if the driver answers it: the
    // snapshot after the answer no longer shows what was asked.
    const asked = await this.capture();
    const what = await this.driver.answer(this, screen, policy);
    if (!what) return false;
    if (asked !== null) this.keepShot(this.note(`dialog, before answering: ${what}`), `dialog-${what}`, asked);
    this.answered.push({ at: new Date().toISOString(), what, dialog });
    // A Wakeshift credential in the dialog the user was asked to answer (the inline wait loop once showed its link in every approval).
    const kinds = credentialKinds(this.mask(dialog));
    if (kinds.length) {
      this.exposures.push({ at: new Date().toISOString(), where: `a dialog (${what})`, kinds });
      this.note(`the dialog showed a Wakeshift credential: ${kinds.join(", ")}`);
    }
    // A dialog the driver answered without describing it: counted all the same.
    if (this.prompts.length === before)
      this.prompted({
        kind: /trust/i.test(what) ? "trust" : /picked|submitted|question/i.test(what) ? "question" : /approved|denied|allowed/i.test(what) ? "approval" : "other",
        asks: redactPlain(dialog).split("\n").slice(-10).map((x) => x.replace(/\s+/g, " ").trim()).filter(Boolean).join(" ⏎ ").slice(-400),
        options: [],
        chosen: what,
        scope: "n/a",
        sessionOffered: false,
      });
    await this.snap(`answered: ${what}`);
    await sleep(800);
    return true;
  }

  /**
   * Waits until re matches the pane (or what it printed since `since`, for
   * full-screen harnesses), answering dialogs meanwhile. Returns the time
   * it took, or null on timeout.
   */
  /** The harness's model quota ran out (drivers/base.ts quotaPrompt): the lane is blocked, not failed. */
  private checkQuota(screen: string): void {
    if (quotaPrompt(this.driver.id, screen)) throw new QuotaError(this.driver.name);
  }

  async waitFor(
    re: RegExp | null,
    timeoutMs: number,
    policy: AnswerPolicy,
    opts: { since?: number; tick?: () => Promise<void>; watch?: (screen: string, printed: string) => boolean } = {},
  ): Promise<number | null> {
    const start = Date.now();
    const end = start + timeoutMs * this.ctx.slow;
    const since = opts.since ?? this.logSize();
    while (Date.now() < end) {
      const screen = await this.screen();
      this.checkQuota(screen);
      const printed = this.logSince(since);
      // watch: the caller's own check of each look at the pane (true: done),
      // without the harness's status bar (Hermes's TUI names the chat there).
      if (opts.watch ? opts.watch(this.prose(screen), printed) : re && (re.test(screen) || re.test(printed))) return Date.now() - start;
      if (await this.answerOnce(policy, screen)) continue;
      if (opts.tick) await opts.tick();
      await sleep(250);
    }
    return null;
  }

  /**
   * Waits until the agent is idle: not busy and the screen unchanged for
   * quietMs, answering dialogs meanwhile. Returns false on timeout.
   */
  async waitIdle(policy: AnswerPolicy, maxMs = 240_000, quietMs = 6000): Promise<boolean> {
    const end = Date.now() + maxMs * this.ctx.slow;
    let last = "";
    let since = Date.now();
    await sleep(1500);
    while (Date.now() < end) {
      const screen = await this.screen(200);
      this.checkQuota(screen);
      // A dialog the delayed-approval variant hasn't answered yet: the turn
      // isn't over.
      if ((await this.answerOnce(policy, screen)) || this.pendingDialog) {
        since = Date.now();
        if (this.pendingDialog) await sleep(500);
        continue;
      }
      // Spinners and timers change while nothing else does.
      const norm = screen
        .split("\n")
        .filter((l) => !/esc to interrupt|[⠁-⣿]|\(\d+s\b|\d+s ·|tokens|Working|Thinking|ctrl\+c to stop/i.test(l) && !this.driver.volatile?.test(l))
        .join("\n");
      if (norm !== last) {
        last = norm;
        since = Date.now();
      } else if (!this.driver.busy(screen) && Date.now() - since >= quietMs && !this.driver.turnOpen?.(this)) return true;
      await sleep(500);
    }
    return false;
  }

  /** Sends a message and waits until the turn is over. */
  async say(text: string, policy: AnswerPolicy, maxMs = 240_000): Promise<boolean> {
    if (this.driver.beforeSay) {
      const did = await this.driver.beforeSay(this, await this.screen(200));
      if (did) await this.snap(did);
    }
    // A harness just started (cursor-agent after a restart) draws its
    // history and input box for a few seconds: text typed meanwhile can sit
    // in the box unsent. Wait for the screen to settle, then check it went.
    await this.settle();
    await this.driver.send(this, text);
    this.note(`said: ${text.length > 160 ? text.slice(0, 157) + "…" : text}`);
    await this.confirmSent(text);
    return this.waitIdle(policy, maxMs);
  }

  /**
   * Sends a message and runs during() once its turn is under way (the
   * agent shows busy, or after at most startMs), then waits until the turn
   * is over. Returns whether the agent showed busy before during() ran (S15:
   * a post made mid-turn).
   */
  async sayDuring(text: string, policy: AnswerPolicy, during: () => Promise<void>, maxMs = 240_000, startMs = 8000): Promise<boolean> {
    if (this.driver.beforeSay) {
      const did = await this.driver.beforeSay(this, await this.screen(200));
      if (did) await this.snap(did);
    }
    await this.settle();
    await this.driver.send(this, text);
    this.note(`said: ${text.length > 160 ? text.slice(0, 157) + "…" : text}`);
    let busy = false;
    const end = Date.now() + startMs * this.ctx.slow;
    while (Date.now() < end) {
      if (this.driver.busy(await this.screen(200))) {
        busy = true;
        break;
      }
      await sleep(200);
    }
    await during();
    if (!busy) await this.confirmSent(text);
    await this.waitIdle(policy, maxMs);
    return busy;
  }

  /** Waits (up to maxMs) until the screen hasn't changed for quietMs and the agent isn't busy. */
  async settle(maxMs = 20_000, quietMs = 1500): Promise<void> {
    const end = Date.now() + maxMs * this.ctx.slow;
    let last = "";
    let since = Date.now();
    while (Date.now() < end) {
      const s = await this.screen(60);
      const v = this.driver.volatile;
      const norm = v ? s.split("\n").filter((l) => !v.test(l)).join("\n") : s;
      if (norm !== last) {
        last = norm;
        since = Date.now();
      } else if (Date.now() - since >= quietMs && !this.driver.busy(s) && !this.driver.turnOpen?.(this)) return;
      await sleep(250);
    }
  }

  /**
   * Checks the message left the input box: still there at the bottom, with
   * the agent not busy and no dialog up, Enter goes again (twice at most),
   * and the step says so. A dialog is never answered with that Enter.
   */
  async confirmSent(text: string): Promise<void> {
    // Only where the driver can read its input box: a submitted message
    // shows near the bottom too, so the screen alone can't tell.
    if (!this.driver.unsent) return;
    for (let tries = 0; tries < 2; tries++) {
      await sleep(2500);
      const s = await this.screen(60);
      if (!this.driver.unsent(s, text) || this.driver.busy(s)) return;
      if (await this.dryAnswer(s, { allowInstall: false })) return;
      await this.tmux.keys(this.session, "Enter");
      await this.snap("the message sat unsent in the input box: pressed Enter again");
    }
  }

  /** Runs a command in the lane's environment (outside tmux): mcp add, uninstall. */
  async run(env: Record<string, string>, cmd: string, args: string[], cwd = this.home.work): Promise<{ ok: boolean; out: string }> {
    try {
      const { stdout, stderr } = await exec(cmd, args, { env, cwd, timeout: 120_000, maxBuffer: 16 << 20 });
      return { ok: true, out: redactPlain(stdout + stderr) };
    } catch (err) {
      const e = err as { stdout?: string; stderr?: string; message?: string };
      return { ok: false, out: redactPlain(`${e.stdout ?? ""}${e.stderr ?? ""}${e.message ?? ""}`) };
    }
  }
}
