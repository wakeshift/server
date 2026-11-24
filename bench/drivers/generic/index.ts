// The generic drivers: an agent Wakeshift doesn't recognize
// (drivers/generic/agent.ts, a real MCP client with an unknown client name
// and no harness argument), in a tmux pane. Since November 5, 2026 such an
// agent gets only the neutral note (harnesses.CantWake: "Wakeshift can't
// wake this agent…"), with no background command and no install offer,
// and no result hands it held messages (no result asks for another call):
// S1 checks the note, and S5 that a message stays held.
// The two stay apart on purpose: "generic" would still follow a background
// command if a result gave one, as a harness that tells its agent when one
// exits (S1 fails a result that gives one), and "generic-plain" can't run
// commands, so a regression that brought the command back shows as the two
// lanes diverging. Both run against a real core (a local one), not the
// fake target.

import { existsSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import type { Lane } from "../../src/lane.js";
import type { PsRow } from "../../src/procs.js";
import { sleep } from "../../src/tmux.js";
import { TmuxDriver } from "../base.js";
import type { AnswerPolicy, Availability, Driver } from "../types.js";

const agentJs = join(dirname(fileURLToPath(import.meta.url)), "agent.js");

export class Generic extends TmuxDriver implements Driver {
  id: string;
  name: string;
  bin = "node";
  // Nothing wakes it: no command, no install (CantWake).
  zeroInstallWake = false;
  cantWake = true;
  installPath = null;
  // Nothing recognizes it: S9 expects no harness.
  harness = "";
  conversation = "none" as const;

  constructor(readonly plain: boolean) {
    super();
    this.id = plain ? "generic-plain" : "generic";
    this.name = plain ? "Unrecognized agent (no commands)" : "Unrecognized agent (background command)";
  }

  override binPath(): string {
    return process.execPath;
  }

  async check(): Promise<Availability> {
    return existsSync(agentJs) ? { ok: true, version: "generic-1" } : { ok: false, reason: "build the bench first" };
  }

  async prepare(l: Lane): Promise<void> {
    l.note("generic agent: nothing to set up", { mcp: l.mcpUrl });
  }

  async start(l: Lane, opts: { resume?: boolean } = {}): Promise<void> {
    await this.openPane(l);
    l.state.panePid = await l.tmux.panePid(l.session);
    await this.shell(l, [process.execPath, agentJs, l.mcpUrl, ...(this.plain ? ["--plain"] : []), ...(opts.resume ? ["--resume"] : [])]);
    if ((await l.waitFor(/generic agent ready/, 20_000, { allowInstall: false })) === null) throw new Error("the generic agent didn't start");
    await sleep(300);
  }

  async stop(l: Lane): Promise<void> {
    await l.tmux.type(l.session, "/exit");
    await this.waitShell(l, 10_000);
  }

  async answer(_l: Lane, _screen: string, _policy: AnswerPolicy): Promise<string | null> {
    return null;
  }

  override busy(): boolean {
    return false;
  }

  // The background command (the session's wait) holds the stream.
  waiters(l: Lane, rows: PsRow[]): PsRow[] {
    return this.mine(l, rows).filter((r) => r.command.includes("/v1/stream/wait"));
  }

  installed(): string[] {
    return [];
  }

  async uninstall(l: Lane): Promise<void> {
    l.note("generic agent: nothing to uninstall");
  }
}
