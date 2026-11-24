// The one interface every harness driver implements. A driver knows how to
// set a harness up in a temp HOME, start it in tmux, talk to it, answer its
// dialogs, find the process that holds its Wakeshift stream, restart it and
// clean up. The scenarios (src/scenarios.ts) are the same for every driver.

import type { Bench } from "../src/core.js";
import type { PsRow } from "../src/procs.js";
import type { Lane } from "../src/lane.js";
import type { Received } from "../src/arrivals.js";

export interface Availability {
  ok: boolean;
  /** Why not (reported as blocked). */
  reason?: string;
  version?: string;
}

/** A turn started by something queued while the conversation was idle (a Monitor event, a channel message). */
export interface Turn {
  /** What queued it. */
  source: "monitor" | "channel" | "other";
  /** The queued text (to find the event it carried). */
  text: string;
  queued: number;
  started: number | null;
  first: number | null;
  firstKind: string | null;
  ended: number | null;
  outputTokens: number;
  /** Input tokens the turn's model calls read: new, cached and cache writes. */
  inputTokens: number;
  model: string | null;
}

/** What the driver may agree to in a dialog. */
export interface AnswerPolicy {
  /** Approve running Wakeshift's installer (only after the user said yes). */
  allowInstall: boolean;
  /**
   * The user's answer when the agent asks about the install with a picker
   * (Claude Code's AskUserQuestion); default "no". "yes" also approves the
   * installer the agent runs after it.
   */
  pick?: "yes" | "no";
  /**
   * Which approval to give where a dialog offers a choice of scope:
   * "session" (S12) takes the broadest option that ends with the session or
   * conversation, else a one-time approval, and persists ("always", "don't
   * ask again", an allowlist entry) only when that's the only way to say
   * yes. Default: the driver's own choice (the persistent one where offered,
   * so S1–S9 aren't slowed by repeats).
   */
  scope?: "session";
  /**
   * The user's answer to Wakeshift's form in the harness's own dialog (an
   * MCP elicitation: "Wakeshift, for <app>:", "Notifications in this
   * conversation"): "setup" ("I'll set it up", R10) or "later" ("Not now",
   * the default, as a user who wasn't asked to set anything up).
   */
  form?: "setup" | "later";
}

/** How long an approval lasts, as the dialog's option says. */
export type PromptScope = "once" | "session" | "always" | "deny";

/** One dialog the bench answered (S12 counts them). */
export interface PromptRecord {
  at: string;
  /** approval: run a command or tool; trust: a folder, hooks; question: the agent's own picker; form: Wakeshift's form in the harness's dialog. */
  kind: "approval" | "trust" | "question" | "form" | "other";
  /** What it asked to run or allow, as the dialog says it (redacted, shortened). */
  asks: string;
  /** The options offered, as labeled. */
  options: string[];
  /** The option chosen. */
  chosen: string;
  scope: PromptScope | "n/a";
  /** Whether an option scoped to the session or conversation was offered. */
  sessionOffered: boolean;
  /** Persisting was the only way to say yes (noted, never chosen otherwise). */
  forcedAlways?: boolean;
  /** What the run was doing (S12: setup, join, conversation, wrap), from the lane's state. */
  phase?: string;
}

/** A tool call the harness refused without asking the user (Claude Code's auto mode classifier), from its transcript. */
export interface Refusal {
  /** ISO time of the refusal. */
  at: string;
  /** The tool, and for a shell command its start (redacted, shortened). */
  tool: string;
  /** The reason the harness gave (redacted, shortened). */
  reason: string;
}

export interface Driver {
  id: string;
  name: string;
  mode: "automated" | "container" | "assisted" | "fake";
  /** Wakes an idle conversation with nothing installed (Claude Code's Monitor, Cursor's and agy's background command). */
  zeroInstallWake: boolean;
  /**
   * Wakeshift can't wake it (an agent it doesn't recognize): subscribing
   * gets only the neutral note (harnesses.CantWake, "Wakeshift can't wake
   * this agent…"), with no command and no install offer, and S1 checks that.
   */
  cantWake?: boolean;
  /** The harness subscribes itself with MCP Events push here (opencode with mcp_events): the realistic tier's push lane (R7-R9) runs. */
  readonly pushes?: boolean;
  /** How an install delivers: hooks, connector, plugin; null if there's no install. */
  installPath: "hooks" | "connector" | "plugin" | null;
  /** What S9 expects the harness argument (or recognition) to be. */
  harness: string;
  /** Whether the agent itself must pass conversation_id ("agent": Cursor), the harness sends it ("auto"), or there is none. */
  conversation: "agent" | "auto" | "none";
  /** The transports (statusOf's via) an install's connection shows as; default hooks. */
  installVia?: string[];
  /** The installer's name (/install/<name>.sh); default: id. */
  installName?: string;
  /**
   * The command that installs Wakeshift's plugin with the harness's own
   * plugin command from marketplace source (Bench.agentPlugins), for a
   * harness the core offers that for; the bench runs it as the user when
   * the agent doesn't. refSep joins a repository and a branch;
   * source, if set, is where it installs from instead (a plugin with a
   * repository of its own: Antigravity CLI's, antigravitySource).
   */
  pluginInstall?: { refSep: string; command(source: string): string; source?(b: Bench): string | null };
  /** The install command the user runs when the agent didn't (core: the bench's core); default: Wakeshift's installer, /install/<installName>.sh. */
  installCommand?(l: Lane, core: string): string;
  /** The lane's environment, for running a command as the user would (the installer, when the agent can't). */
  envFor?(l: Lane): Record<string, string>;
  /** The harness's binary this run uses (null: not installed). */
  binPath?(): string | null;
  /** false: no process on this machine holds the stream (a container's plugin), so S4 is n/a. */
  killable?: boolean;
  /**
   * The harness runs a turn the install started without showing it in the
   * chat's pane (Hermes 0.22.3 and later: the classic CLI doesn't paint a
   * plugin's turn), so a wake's GOT line can't show: S2, S3 and S7 take the
   * agent's report_seen for the code as its arrival, and say so.
   */
  unpaintedWakes?(l: Lane): boolean;
  /**
   * Lines that change while nothing happens (Hermes's TUI status bar, whose
   * timers tick every second, and which names the chat), left out when the
   * bench waits for the screen to go quiet and from what it reads as the
   * agent's words (Lane.prose).
   */
  volatile?: RegExp;
  /**
   * false: the pane's log (pipe-pane) isn't readable text, because the
   * harness repaints only the cells that changed (Hermes's TUI, an Ink app:
   * "notif cation", a word with a letter skipped), so the bench reads only
   * the pane and its scrollback.
   */
  paneLog?: boolean;
  /**
   * A column the harness draws at the end of every line (Hermes's TUI
   * scrollbar, ┃ or │), taken off each line the bench reads from the pane,
   * so a sentence wrapped over lines reads as one.
   */
  gutter?: RegExp;
  /** false: uninstall isn't run or checked (a container's state is removed with it), so S6 says so. */
  uninstallTested?: boolean;
  /** What the agent should relay as the install's trust step (S6), as a regex on the pane. */
  trustStep?: RegExp;
  /** trustStep for the install the watch result offered (added: Wakeshift's text), where that depends on it (Claude Code: its plugin, or Wakeshift's installer). */
  trustStepFor?(added: string): RegExp;
  /** The install command the watch result offered (added: Wakeshift's text), for the user to run when the agent's run is refused; default Wakeshift's installer. */
  offeredInstall?(added: string): string | null;
  /** prepare adds a lane's second MCP server (Lane.mcp2, S15's second app) next to the first; without it S15 is n/a. */
  secondMcp?: boolean;

  /**
   * The conversation's wake turns since a time (ms), from the harness's own
   * transcript (Claude Code): when each was queued, when the turn started,
   * the model's first output and the turn's end. For latency breakdowns.
   */
  turns?(l: Lane, sinceMs: number): Turn[];

  /**
   * What the harness put in front of the model, from its own record of the
   * conversation (Claude Code's session transcripts, Codex's rollout files,
   * Hermes's state DB): the wake scenarios count each code's arrivals from
   * it (src/arrivals.ts), so a duplicate the model noticed and reported once
   * still shows. Null if the record can't be read; without the method,
   * arrivals aren't counted. codes: the codes being counted, for a record
   * read by search (Hermes's); others return everything.
   */
  received?(l: Lane, codes?: string[]): Promise<Received[] | null>;

  /** Tool calls the harness refused without asking the user since a time (ms): Claude Code's auto mode classifier. */
  refusals?(l: Lane, sinceMs: number): Refusal[];

  /**
   * Since a time (ms): how many times the agent started its wake command
   * (Claude Code: Monitor calls), and whether one expired at the harness's
   * own limit rather than ending by itself (S13), from its transcript.
   */
  monitorLog?(l: Lane, sinceMs: number): { starts: number; expired: boolean };

  check(): Promise<Availability>;
  /** The harness's version, run in the lane's own environment (e.g. claude --version with the temp HOME). */
  versionIn?(l: Lane): Promise<string | null>;
  /** Temp HOME, borrowed login, the MCP server added the way the try page says. */
  prepare(l: Lane): Promise<void>;
  /** Starts the harness in the lane's tmux pane (resume: continue the last conversation) and waits until it takes input. */
  start(l: Lane, opts?: { resume?: boolean }): Promise<void>;
  /** Quits the harness, leaving the pane's shell. */
  stop(l: Lane): Promise<void>;
  /** Sends a user message. */
  send(l: Lane, text: string): Promise<void>;
  /**
   * Answers a dialog on screen (tool approval, trust, a yes/no): returns
   * what it answered, or null if there was none.
   */
  answer(l: Lane, screen: string, policy: AnswerPolicy): Promise<string | null>;
  /**
   * The dialog on screen, from its first line, where the harness draws a
   * dialog below the transcript instead of in place of its input box (the
   * Agent SDK client): what a dialog's repeat check and its credential
   * check read. Default: the screen's last 16 lines.
   */
  dialogOf?(screen: string): string;
  /**
   * If the agent asked the user to run cmd (the installer) themselves in
   * the harness (Claude Code: `! <cmd>`), runs it that way and says so;
   * null if it didn't ask.
   */
  runAsUser?(l: Lane, screen: string, cmd: string): Promise<string | null>;
  /** Whether the agent asked the user to run the installer themselves (what runAsUser acts on). */
  asksUserToRun?(screen: string): boolean;
  /**
   * Runs cmd (the installer) as the user, where the harness runs, when the
   * agent didn't (a container's harness: inside the container). Default: a
   * shell on this machine with envFor's environment.
   */
  installAsUser?(l: Lane, cmd: string): Promise<{ ok: boolean; out: string }>;
  /** Before the bench types a message: clear anything that would swallow it (opencode's question picker). Returns what it did. */
  beforeSay?(l: Lane, screen: string): Promise<string | null>;
  /** Whether the agent is mid-turn on this screen. */
  busy(screen: string): boolean;
  /**
   * Whether the harness's own record says a turn is under way, for a
   * harness whose screen can look idle mid-turn (Codex shows no "Working"
   * line while an answer streams, and draws a long line only once it ends). Lane.waitIdle and settle wait while it's true.
   */
  turnOpen?(l: Lane): boolean;
  /** Whether text still sits in the harness's input box, unsent (Lane.confirmSent presses Enter again). */
  unsent?(screen: string, text: string): boolean;
  /** The harness's own trust step after an install (Codex: /hooks then t; restart for others). */
  afterInstall?(l: Lane): Promise<string[]>;
  /** Processes holding the conversation's stream now (attributable to the lane only). */
  waiters(l: Lane, rows: PsRow[]): PsRow[];
  /** Files in the temp HOME showing Wakeshift is installed (S5: none). */
  installed(l: Lane): string[];
  uninstall(l: Lane): Promise<void>;
  /** Records the lane's processes (polled), so a daemon that outlives the pane is still known as the lane's. */
  track?(l: Lane): void;
  /** The lane's processes still running (killed when the lane ends). */
  leftovers?(l: Lane): PsRow[];
  /** Anything beyond the tmux session, the HOME and recorded PIDs. */
  cleanup?(l: Lane): Promise<void>;
  /**
   * The MCP servers the lane's conversation had in reach (from the harness's
   * own record), and those beyond the lane's own (unexpected): a server the
   * user's account or config added makes the lane's result not the
   * scenario's. listed: whether the record had a tool list to check.
   */
  toolServers?(l: Lane): { servers: string[]; unexpected: string[]; listed: boolean };
}
