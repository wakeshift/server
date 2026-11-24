// A scripted stand-in for an agent, run in a tmux pane by the fake driver
// (dry runs and CI). It reads the bench's prompts on stdin and behaves like
// a well-behaved harness: calls watch, quotes the result, asks before
// installing, runs a waiter child that long-polls (and starts it again if it
// dies), reports what arrives and prints GOT <code>. Usage:
//   node agent.js <lane url> [--resume] [--quiet-installed]   the agent
//   node agent.js --waiter <wait url>         its waiter (one line per batch)

import { spawn, type ChildProcess } from "node:child_process";
import { existsSync, mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { createInterface } from "node:readline";
import { fileURLToPath } from "node:url";

const self = fileURLToPath(import.meta.url);

async function waiter(url: string) {
  for (;;) {
    try {
      const r = await fetch(url);
      const j = (await r.json()) as { events: { code: string; text: string }[]; next: string };
      if (j.events.length) process.stdout.write(JSON.stringify(j.events) + "\n");
      url = j.next;
    } catch {
      await new Promise((r) => setTimeout(r, 1000));
    }
  }
}

async function agent(base: string, resume: boolean, quietInstalled: boolean) {
  const home = process.env.HOME ?? "/tmp";
  const stateFile = join(home, ".fake-agent.json");
  const state = (resume && existsSync(stateFile) ? JSON.parse(readFileSync(stateFile, "utf8")) : {}) as { session?: string; installed?: boolean };
  const save = () => writeFileSync(stateFile, JSON.stringify(state));
  let child = null as ChildProcess | null;
  let stopping = false;
  let pendingApproval: (() => void) | null = null;
  const out = (s: string) => process.stdout.write(s + "\n");
  const prompt = () => process.stdout.write("\n> ");

  const post = async (path: string, body: unknown) => {
    const r = await fetch(base + path, { method: "POST", headers: { "content-type": "application/json" }, body: JSON.stringify(body) });
    return r.json() as Promise<{ text?: string; session?: string; token?: string }>;
  };
  const report = async (codes: string[]) => {
    if (!codes.length) return;
    await post("/report", { ids: codes, session_key: state.session });
    // --quiet-installed: once installed, a woken turn reports and prints
    // nothing in the pane, as OpenClaw's woken turns may (their reply goes
    // to the session, not necessarily to the TUI the bench reads).
    if (quietInstalled && state.installed) return;
    for (const c of codes) out(`GOT ${c}`);
    prompt();
  };
  const startWaiter = (token: string) => {
    child?.kill();
    const c = spawn(process.execPath, [self, "--waiter", `${base}/wait?link=${token}&ack=`], { stdio: ["ignore", "pipe", "ignore"] });
    child = c;
    createInterface({ input: c.stdout! }).on("line", (line) => {
      try {
        void report((JSON.parse(line) as { code: string }[]).map((e) => e.code));
      } catch {
        // Not ours.
      }
    });
    c.on("exit", () => {
      if (stopping || child !== c) return;
      out("(the waiter stopped; starting it again)");
      setTimeout(() => void watch(false), 3000);
    });
  };
  const watch = async (quote: boolean) => {
    const r = await post("/watch", { harness: "fake", session_key: state.session, via: state.installed ? "hooks" : "stream" });
    state.session = r.session;
    save();
    if (quote) {
      out("QUOTE:");
      out(r.text ?? "");
      out("");
    }
    const held = [...(r.text ?? "").matchAll(/WB-[a-z0-9]{4}-\d+/g)].map((m) => m[0]);
    await report(held);
    startWaiter(r.token!);
  };

  out("fake agent ready");
  if (resume && state.session) await watch(false);
  prompt();
  const rl = createInterface({ input: process.stdin });
  for await (const line of rl) {
    const t = line.trim();
    if (pendingApproval) {
      const go = pendingApproval;
      pendingApproval = null;
      if (/^y/i.test(t)) go();
      else out("Denied.");
      prompt();
      continue;
    }
    if (t === "/exit") {
      stopping = true;
      child?.kill();
      process.exit(0);
    } else if (/in a new conversation/.test(t)) {
      // S18: a new conversation in a HOME where the install is in already:
      // watch, and ask nothing about installing if it's there.
      state.installed = existsSync(join(home, ".wakeshift/fake/installed"));
      out("Allow tool wakebench.watch? [y/n]");
      pendingApproval = () =>
        void watch(false).then(() => {
          out(state.installed ? "Watching. I'll report bench events as they arrive." : "Wakeshift can wake me through a one-time install. Do you want me to install it?");
          prompt();
        });
      continue;
    } else if (/watch tool now/.test(t)) {
      out("Allow tool wakebench.watch? [y/n]");
      pendingApproval = () =>
        void watch(true).then(() => {
          out("Wakeshift can wake me through a one-time install. Do you want me to install it?");
          prompt();
        });
      continue;
    } else if (/^No, don't install/.test(t)) out("OK, I won't install anything. The waiter keeps running.");
    else if (/^Yes, please install|one-time install/.test(t)) {
      mkdirSync(join(home, ".wakeshift/fake"), { recursive: true });
      writeFileSync(join(home, ".wakeshift/fake/installed"), "wakeshift fake install\n");
      state.installed = true;
      out("Installed. If it asks, choose: Yes, I trust this folder.");
      await watch(false);
      out("Wakeshift: connected");
    } else if (/unwatch tool, then call watch again/.test(t)) {
      await post("/unwatch", { session_key: state.session });
      await watch(false);
      out("Watching again.");
    } else if (/watch tool again/.test(t)) await watch(false);
    // The user, back after a restart or the trust step: the agent calls the tool again, as its install note says.
    else if (/^ok, I'm back|^Done, I ran it|^done\b/.test(t)) await watch(false);
    else if (/Did any bench events/.test(t)) out("Nothing new.");
    prompt();
  }
}

const argv = process.argv.slice(2);
if (argv[0] === "--waiter") void waiter(argv[1]);
else void agent(argv[0], argv.includes("--resume"), argv.includes("--quiet-installed"));
