// A scripted agent Wakeshift doesn't recognize, run in a tmux pane by the
// generic driver: a real MCP client of the bench app (client name
// "wakebench-generic", no harness argument), so the core gives it the
// neutral note for an agent it can't wake (internal/harnesses CantWake;
// GenericNote's command is gone since November 5, 2026). It reads the
// bench's prompts on stdin like the fake agent, and tells the user what the
// note says to.
//
//   node agent.js <mcp url> [--resume]           would follow a background
//       command if a result gave one: starts it in the background, and when
//       it exits, starts it again first, then reports what it printed (GOT <code>)
//   node agent.js <mcp url> --plain [--resume]   can't run commands

import { spawn, type ChildProcess } from "node:child_process";
import { existsSync, mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { createInterface } from "node:readline";
import { Client, StreamableHTTPClientTransport } from "@modelcontextprotocol/client";

/** The command a generic result gives (the session's wait in a loop that ends when messages arrive). */
export const COMMAND = /(f=\$HOME\/\.wakeshift\/links\/[0-9a-f]{16}; [^\n]*?c=; p=\$PPID; .*?; done; fi) \./s;
/** The step that has the agent write the conversation's link to the file the command reads. */
const LINK_STEP = /If you haven't written this conversation's link to the file ~\/\.wakeshift\/links\/([0-9a-f]{16}) yet, first write it .*? its whole content this one line: (\S+) \./gs;
const SESSION = /session_key="([^"]+)"/;
const CODE = /WB-[a-z0-9]{4}-\d+/g;

async function main(url: string, plain: boolean, resume: boolean) {
  const home = process.env.HOME ?? "/tmp";
  const stateFile = join(home, ".generic-agent.json");
  const state = (resume && existsSync(stateFile) ? JSON.parse(readFileSync(stateFile, "utf8")) : {}) as { session?: string };
  const save = () => writeFileSync(stateFile, JSON.stringify(state), { mode: 0o600 });
  const out = (s: string) => process.stdout.write(s + "\n");
  const prompt = () => process.stdout.write("\n> ");

  const client = new Client({ name: "wakebench-generic", version: "0.1.0" });
  await client.connect(new StreamableHTTPClientTransport(new URL(url)));
  const call = async (name: string, args: Record<string, unknown> = {}) => {
    if (state.session) args.session_key = state.session;
    const r = await client.callTool({ name, arguments: args });
    const text = ((r.content ?? []) as { type: string; text?: string }[]).map((c) => (c.type === "text" ? (c.text ?? "") : "")).join("\n");
    const s = SESSION.exec(text);
    if (s) {
      state.session = s[1];
      save();
    }
    return text;
  };

  let child: ChildProcess | null = null;
  let command = "";
  let stopping = false;
  const report = async (codes: string[]) => {
    const fresh = [...new Set(codes)];
    if (!fresh.length) return;
    await call("report_seen", { ids: fresh });
    for (const c of fresh) out(`GOT ${c}`);
    prompt();
  };
  // Starts the command in the background; when it exits (this process is
  // told, as a harness tells its agent), starts it again first, then acts.
  const start = () => {
    if (plain || !command || child) return;
    const c = spawn("sh", ["-c", command], { stdio: ["ignore", "pipe", "ignore"] });
    child = c;
    let buf = "";
    c.stdout!.on("data", (d) => (buf += d));
    c.on("exit", () => {
      child = null;
      if (stopping) return;
      if (/^\{"type":"hub\.closed"/m.test(buf)) {
        out("(the command says to stop: " + (/"note":"([^"]*)"/.exec(buf)?.[1] ?? "") + ")");
        prompt();
        return;
      }
      start();
      void report(buf.match(CODE) ?? []);
    });
  };
  const watch = async (quote: boolean) => {
    const text = await call("watch");
    if (quote) {
      out("QUOTE:");
      out(text);
      out("");
    }
    const m = COMMAND.exec(text);
    if (m) command = m[1];
    // As an agent's file tool would: the link file, 0644 in folders it makes.
    for (const [, key, link] of text.matchAll(LINK_STEP)) {
      mkdirSync(join(home, ".wakeshift", "links"), { recursive: true });
      writeFileSync(join(home, ".wakeshift", "links", key), link + "\n");
    }
    await report(text.match(CODE) ?? []);
    start();
  };

  out("generic agent ready" + (plain ? " (no background commands)" : ""));
  if (resume && state.session) await watch(false);
  prompt();
  for await (const line of createInterface({ input: process.stdin })) {
    const t = line.trim();
    try {
      if (t === "/exit") {
        stopping = true;
        (child as ChildProcess | null)?.kill();
        process.exit(0);
      } else if (/watch tool now/.test(t)) {
        await watch(true);
        out(command && !plain ? "Started the command in the background." : "Nothing will notify me here when something arrives; ask me to check with the app's tools.");
      } else if (/^No, don't install|one-time install|^Yes, please install/.test(t)) out("There's nothing to install for this agent.");
      else if (/unwatch tool, then call watch again/.test(t)) {
        await call("unwatch");
        await watch(false);
        out("Watching again.");
      } else if (/watch tool again/.test(t)) await watch(false);
      // The user, back after a restart: call the tool again, as a result's note says.
      else if (/^ok, I'm back|^Done, I ran it/.test(t)) await watch(false);
      else if (/Did any bench events/.test(t)) {
        // What a result shows, if anything (no result hands held messages over since November 5).
        const text = await call("report_seen", { ids: [] });
        const held = text.match(CODE) ?? [];
        if (held.length) await report(held);
        else out("Nothing new.");
        start();
      }
    } catch (e) {
      out("error: " + (e as Error).message);
    }
    prompt();
  }
}

const argv = process.argv.slice(2);
void main(argv[0], argv.includes("--plain"), argv.includes("--resume"));
