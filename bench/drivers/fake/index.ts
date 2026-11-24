// The fake driver: drives drivers/fake/agent.ts in tmux against the fake
// target. It exercises the whole bench (tmux, dialogs, waiters, kills,
// restarts, snapshots, redaction, grading, the report) without a harness,
// a login or a core: the CI dry run.

import { existsSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import type { Lane } from "../../src/lane.js";
import type { PsRow } from "../../src/procs.js";
import { sleep } from "../../src/tmux.js";
import { tail, TmuxDriver } from "../base.js";
import type { AnswerPolicy, Availability, Driver } from "../types.js";

const agentJs = join(dirname(fileURLToPath(import.meta.url)), "agent.js");

export class Fake extends TmuxDriver implements Driver {
  id = "fake";
  name = "Fake (dry run)";
  bin = "node";
  override mode: Driver["mode"] = "fake";
  zeroInstallWake = true;
  installPath = "hooks" as const;
  harness = "fake";
  conversation = "none" as const;
  trustStep = /trust this folder/;

  override binPath(): string {
    return process.execPath;
  }

  async check(): Promise<Availability> {
    return existsSync(agentJs) ? { ok: true, version: "fake-1" } : { ok: false, reason: "build the bench first" };
  }

  async prepare(l: Lane): Promise<void> {
    l.note("fake agent: nothing to set up", { mcp: l.mcpUrl });
  }

  async start(l: Lane, opts: { resume?: boolean } = {}): Promise<void> {
    await this.openPane(l);
    l.state.panePid = await l.tmux.panePid(l.session);
    // WB_FAKE_QUIET_INSTALLED=1: woken turns after the install report but print nothing (OpenClaw's).
    const quiet = process.env.WB_FAKE_QUIET_INSTALLED === "1" ? ["--quiet-installed"] : [];
    await this.shell(l, [process.execPath, agentJs, l.mcpUrl, ...(opts.resume ? ["--resume"] : []), ...quiet]);
    if ((await l.waitFor(/fake agent ready/, 20_000, { allowInstall: false })) === null) throw new Error("the fake agent didn't start");
    await sleep(300);
  }

  async stop(l: Lane): Promise<void> {
    await l.tmux.type(l.session, "/exit");
    await this.waitShell(l, 10_000);
  }

  async answer(l: Lane, screen: string, _policy: AnswerPolicy): Promise<string | null> {
    const last = tail(screen, 3);
    if (/Allow tool wakebench\.watch\? \[y\/n\]\s*$/.test(last)) {
      await l.tmux.type(l.session, "y");
      return "approved wakebench.watch";
    }
    return null;
  }

  override busy(): boolean {
    return false;
  }

  waiters(l: Lane, rows: PsRow[]): PsRow[] {
    return this.mine(l, rows).filter((r) => r.command.includes("agent.js --waiter"));
  }

  installed(l: Lane): string[] {
    return this.mentions(l, [".wakeshift/fake/installed"]);
  }

  async uninstall(l: Lane): Promise<void> {
    l.note("fake uninstall");
  }
}
