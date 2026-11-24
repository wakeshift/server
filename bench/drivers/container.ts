// Harnesses that run in Docker (Hermes Agent, OpenClaw): the bench runs its
// own container from the image (wb-<run>-<harness>), on a throwaway copy of
// the bench's own state (bench/state.sh: made by the harness's onboarding,
// with no conversations, channels or MCP servers), and drives the harness's
// chat CLI with `docker exec -it` in a tmux pane. The user's own containers
// and state are never read.
//
// A container reaches the host's toy app at host.docker.internal. A local
// core is pinned under its own URL (http://localhost:<port>), which inside
// a container is the container itself, so the bench forwards that port
// there to the host (a small Node TCP forwarder in the container, started
// again after a restart); an image without Node needs a hosted core.

import { execFile, execFileSync } from "node:child_process";
import { randomBytes } from "node:crypto";
import { cpSync, mkdirSync, rmSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { promisify } from "node:util";
import { harnessState, readSecrets, stateRoot } from "../src/benchstate.js";
import type { Lane } from "../src/lane.js";
import type { PsRow } from "../src/procs.js";
import { redactPlain } from "../src/redact.js";
import { sleep } from "../src/tmux.js";
import { BlockedError, chooseOption, dialogAsks, INSTALLER, menuOptions, scopeOf, selectInMenu, tail, TmuxDriver } from "./base.js";
import type { AnswerPolicy, Availability, Driver } from "./types.js";

const exec = promisify(execFile);

/** How long the bench waits for `docker pull` of a missing image (Hermes's is a few GB). */
export const PULL_TIMEOUT_MS = 15 * 60_000;

/** Runs docker with args, rejecting on a non-zero exit or the timeout. */
export type DockerRun = (args: string[], timeoutMs: number) => Promise<unknown>;

const docker: DockerRun = (args, timeoutMs) => exec("docker", args, { timeout: timeoutMs });

/**
 * Makes sure the image is here: one that isn't is pulled (a pinned public
 * release, so every machine runs the same one; a tag like `latest` of the
 * same image doesn't count), with a timeout, before the harness is blocked
 * for it. Blocked only if the pull fails.
 */
export async function ensureImage(image: string, run: DockerRun = docker, pullTimeoutMs = PULL_TIMEOUT_MS): Promise<{ ok: true; pulled: boolean } | { ok: false; reason: string }> {
  try {
    await run(["image", "inspect", image], 20_000);
    return { ok: true, pulled: false };
  } catch {
    // Not here: pull it.
  }
  try {
    await run(["pull", "--quiet", image], pullTimeoutMs);
    await run(["image", "inspect", image], 20_000);
    return { ok: true, pulled: true };
  } catch (e) {
    const err = e as { killed?: boolean; signal?: string; stderr?: unknown; message?: string };
    const timedOut = err.killed === true || err.signal === "SIGTERM";
    const detail = String(err.stderr || err.message || "").trim().split("\n").pop() ?? "";
    const why = timedOut ? `didn't finish in ${Math.round(pullTimeoutMs / 60_000)} min` : `failed${detail ? `: ${redactPlain(detail).slice(0, 200)}` : ""}`;
    return { ok: false, reason: `the image ${image} isn't here, and docker pull ${why}` };
  }
}

/** A toy app's URL as a container reaches it (the host is host.docker.internal there). */
export function inContainer(url: string): string {
  return url.replace("127.0.0.1", "host.docker.internal");
}

export interface ContainerSpec {
  image: string;
  /** The bench's state for it (bench/state.sh: <state root>/<name>), copied per run and mounted at `to`. */
  state: { name: string; to: string };
  /**
   * Copy the state into the container's own filesystem (owned by this
   * numeric uid:gid) instead of mounting it: OpenClaw's gateway dies with SIGBUS
   * on a hooks run over a Docker Desktop bind mount (a memory-mapped file
   * there), and runs fine on the container's filesystem.
   */
  copyIn?: string;
  /**
   * Copy the state into a fresh Docker volume owned by this numeric uid:gid
   * (by a throwaway container of the same image), mounted at `to`, instead of
   * mounting the copy: Hermes 0.22.3 and later refuse their gateway's control
   * socket unless it's 0600, which a Docker Desktop bind mount can't make it
   * ("Gateway inaccessible: unsafe_control_permissions"). The run's copy on
   * the host is what's copied in (after configure), not read back.
   */
  volume?: string;
  /** Env set in the container. */
  env: Record<string, string>;
  /** Env read from the bench's env file (<state root>/env; values never printed). */
  secrets: string[];
  /** Env made fresh for each run (random values), e.g. a gateway token the state refers to. */
  generated?: string[];
  cmd: string[];
  port: number;
  /** The chat CLI inside the container. */
  chat: string[];
  /** Added to chat to continue the last conversation (a restart after the install). */
  resume?: string[];
  /** The container log line that says it's ready (else a fixed wait); its CLI fails before then. */
  ready?: RegExp;
  /** A command in the container that prints the harness's version. */
  version?: string[];
}

export abstract class ContainerDriver extends TmuxDriver implements Driver {
  abstract spec: ContainerSpec;
  bin = "docker";
  override mode: Driver["mode"] = "container";
  zeroInstallWake = false;
  installPath = "plugin" as const;
  conversation = "none" as const;
  abstract harness: string;
  abstract name: string;
  /** No process on the host holds the stream: S4 can't kill it. */
  killable = false;
  uninstallTested = false;

  container(l: Lane): string {
    return `wb-${l.ctx.run}-${l.id}`;
  }

  /** The run's state volume (spec.volume). */
  volumeName(l: Lane): string {
    return `${this.container(l)}-state`;
  }

  /** Whether a path exists in the running container (state in a volume or copied in isn't on the host). */
  existsIn(l: Lane, path: string): boolean {
    if (!l.state.container) return false;
    try {
      execFileSync("docker", ["exec", this.container(l), "test", "-e", path], { timeout: 20_000, stdio: "ignore" });
      return true;
    } catch {
      return false;
    }
  }

  /** The harness's own version, asked inside the running container (with the image it came from). */
  override async versionIn(l: Lane): Promise<string | null> {
    if (!this.spec.version) return this.spec.image;
    const r = await this.dockerExec(l, this.spec.version);
    const v = /\b(\d+\.\d+\.\d+[\w.+-]*)/.exec(r.out)?.[1];
    return v ? `${v} (${this.spec.image})` : this.spec.image;
  }

  async check(): Promise<Availability> {
    const img = await ensureImage(this.spec.image);
    if (!img.ok) return img;
    if (!harnessState(this.spec.state.name)) return { ok: false, reason: `no bench state for ${this.name} in ${stateRoot()}: run bench/state.sh` };
    const s = readSecrets(this.spec.secrets);
    if (!s.ok) return { ok: false, reason: s.reason };
    // No version yet: the harness's own comes from the running container (versionIn).
    return { ok: true };
  }

  async prepare(l: Lane): Promise<void> {
    mkdirSync(join(l.ctx.dir, "containers"), { recursive: true, mode: 0o700 });
    const dir = join(l.ctx.dir, "containers", l.id);
    mkdirSync(dir, { mode: 0o700 }); // fresh: throws if it exists
    l.state.containerDir = dir; // removed by cleanup, whatever fails below
    const src = harnessState(this.spec.state.name);
    if (!src) throw new BlockedError(`no bench state for ${this.name} in ${stateRoot()}: run bench/state.sh`);
    const secrets = readSecrets(this.spec.secrets);
    if (!secrets.ok) throw new BlockedError(secrets.reason);
    // A copy per run, so runs don't accumulate history. Timestamps kept: a
    // copy dated now makes OpenClaw's gateway-ownership heartbeat look
    // fresh, so every command waits ~95 s for an owner that's gone.
    const dst = join(dir, "state");
    cpSync(src, dst, { recursive: true, preserveTimestamps: true });
    const mounts = ["-v", `${dst}:${this.spec.state.to}`];
    // Through a 0600 file, never on a command line.
    const generated = Object.fromEntries((this.spec.generated ?? []).map((n) => [n, randomBytes(24).toString("hex")]));
    const env = { ...this.spec.env, ...secrets.env, ...generated };
    const envFile = join(dir, "env");
    writeFileSync(envFile, Object.entries(env).map(([k, v]) => `${k}=${v}`).join("\n") + "\n", { mode: 0o600 });
    await this.configure(l, dir);
    const name = this.container(l);
    // Recorded before it starts, so a failure from here on (a docker run that
    // started it and then errored, the MCP setup below) still removes it:
    // the phase's cleanup and the run's both do.
    l.state.container = name;
    l.state.containerDir = dir;
    l.ctx.procs.recordContainer(name);
    const since = new Date(Date.now() - 2000);
    const args = ["--name", name, "--add-host", "host.docker.internal:host-gateway", "--env-file", envFile];
    if (this.spec.copyIn) {
      // Owned by the container's user before it starts: a tar made with
      // that owner, copied in keeping it (docker cp -a).
      const [uid, gid] = this.spec.copyIn.split(":");
      await exec("docker", ["create", ...args, this.spec.image, ...this.spec.cmd], { timeout: 120_000 });
      // No extended attributes (macOS adds com.apple.* ones, which the
      // container's filesystem refuses).
      await exec("sh", ["-c", 'COPYFILE_DISABLE=1 tar -C "$1" --no-xattrs --no-acls --uid "$3" --gid "$4" -cf - . | docker cp -a - "$2"', "sh", dst, `${name}:${this.spec.state.to}`, uid, gid], { timeout: 120_000 });
      await exec("docker", ["start", name], { timeout: 120_000 });
    } else if (this.spec.volume) {
      const vol = this.volumeName(l);
      l.state.volume = vol; // removed by cleanup
      await exec("docker", ["volume", "create", vol], { timeout: 60_000 });
      await exec("docker", ["run", "--rm", "-v", `${vol}:/into`, "-v", `${dst}:/from:ro`, "--entrypoint", "sh", this.spec.image, "-c",
        'cp -a /from/. /into/ && chown -R "$0" /into', this.spec.volume], { timeout: 120_000 });
      await exec("docker", ["run", "-d", ...args, "-v", `${vol}:${this.spec.state.to}`, this.spec.image, ...this.spec.cmd], { timeout: 120_000 });
    } else {
      await exec("docker", ["run", "-d", ...args, ...mounts, this.spec.image, ...this.spec.cmd], { timeout: 120_000 });
    }
    l.note(`started container ${name}`);
    await this.forwardCore(l);
    await this.waitReady(l, since);
    await this.addMcp(l, inContainer(l.mcpUrl));
  }

  /**
   * A local core's port, forwarded inside the container to the host, so
   * its pinned URL (http://localhost:<port>) works there.
   */
  async forwardCore(l: Lane): Promise<void> {
    if (l.ctx.bench.target !== "local" && l.ctx.bench.target !== "fake") return; // a hosted core needs no forwarding
    const port = Number(new URL(l.ctx.bench.core).port);
    const js = `const net=require("net");for(const h of ["127.0.0.1","::1"])net.createServer(c=>{const u=net.connect(${port},"host.docker.internal");c.pipe(u).pipe(c);u.on("error",()=>c.destroy());c.on("error",()=>u.destroy())}).on("error",()=>{}).listen(${port},h)`;
    try {
      await exec("docker", ["exec", "-d", this.container(l), "node", "-e", js], { timeout: 30_000 });
    } catch {
      throw new BlockedError(`${this.name}'s image has no Node to forward the local core's port with (an image without Node needs a hosted core)`);
    }
    l.note(`forwarding localhost:${port} in ${this.container(l)} to the local core`);
  }

  /** Waits for the container's ready line (logged since `since`), or 8 s without one. */
  async waitReady(l: Lane, since: Date): Promise<void> {
    if (!this.spec.ready) return sleep(8000);
    const end = Date.now() + 150_000;
    while (Date.now() < end) {
      const r = await exec("docker", ["logs", "--since", since.toISOString(), this.container(l)], { timeout: 20_000, maxBuffer: 16 << 20 }).catch(() => ({ stdout: "", stderr: "" }));
      if (this.spec.ready.test(r.stdout + r.stderr)) return;
      await sleep(2000);
    }
    l.note(`${this.name}'s container didn't log that it was ready within 150 s`);
  }

  /** The container's state and the end of its log (redacted), for a run that failed. */
  async noteLogs(l: Lane): Promise<void> {
    const name = this.container(l);
    const st = await exec("docker", ["inspect", name, "--format", "{{.State.Status}} exit={{.State.ExitCode}} oom={{.State.OOMKilled}}"], { timeout: 20_000 }).catch(() => ({ stdout: "?" }));
    const r = await exec("docker", ["logs", "--tail", "30", name], { timeout: 20_000, maxBuffer: 16 << 20 }).catch(() => ({ stdout: "", stderr: "" }));
    l.note(`${this.name}'s container: ${String(st.stdout).trim()}`, { log: redactPlain(String(r.stdout) + String(r.stderr)).slice(-3000) });
  }

  /** The chat command for this lane (default: spec.chat). */
  chat(_l: Lane): string[] {
    return this.spec.chat;
  }

  /** The run's copy of the state, on the host. */
  stateDir(l: Lane): string | undefined {
    const dir = l.state.containerDir as string | undefined;
    return dir ? join(dir, "state") : undefined;
  }

  /** Edits the run's copy of the state before the container starts (MCP config). */
  async configure(_l: Lane, _dir: string): Promise<void> {}

  abstract addMcp(l: Lane, url: string): Promise<void>;

  abstract installed(l: Lane): string[];

  async dockerExec(l: Lane, args: string[]): Promise<{ ok: boolean; out: string }> {
    return l.run(process.env as Record<string, string>, "docker", ["exec", this.container(l), ...args], l.ctx.dir);
  }

  async start(l: Lane, opts: { resume?: boolean } = {}): Promise<void> {
    await this.openPane(l);
    l.state.panePid = await l.tmux.panePid(l.session);
    const chat = [...this.chat(l), ...(opts.resume && this.spec.resume ? this.spec.resume : [])];
    await this.shell(l, ["docker", "exec", "-it", this.container(l), ...chat]);
    // "gateway connected": OpenClaw's TUI on a new session shows no prompt.
    if ((await l.waitFor(/>\s*$|❯|Type|message|gateway connected/im, 90_000, { allowInstall: false })) === null) {
      await this.noteLogs(l);
      await this.didntStart(l, `${this.name}'s chat didn't reach its start screen`, opts);
    }
    await sleep(2000);
  }

  async stop(l: Lane): Promise<void> {
    await l.tmux.keys(l.session, "C-c");
    await sleep(500);
    await l.tmux.keys(l.session, "C-d");
    await this.waitShell(l, 10_000);
    // A restart of the harness is a restart of its container.
    const since = new Date();
    await exec("docker", ["restart", this.container(l)], { timeout: 120_000 }).catch(() => {});
    await this.forwardCore(l);
    await this.waitReady(l, since);
  }

  /** The installer the agent didn't run, as the user would: inside the container. */
  async installAsUser(l: Lane, cmd: string): Promise<{ ok: boolean; out: string }> {
    return this.dockerExec(l, ["sh", "-c", cmd]);
  }

  async answer(l: Lane, screen: string, policy: AnswerPolicy): Promise<string | null> {
    const t = tail(screen, 20);
    // A menu (Hermes: "❯ 1. Allow once … 4. Deny", Enter to confirm): the
    // first choice allows once; the last denies.
    if (/\b1\. Allow once\b/i.test(t) && /\bDeny\b/.test(t)) {
      if (INSTALLER.test(t) && !policy.allowInstall && l.state?.picked !== "yes") {
        const n = /(\d)\.\s*Deny\b/.exec(t)?.[1];
        for (let i = 1; i < Number(n ?? 4); i++) await l.tmux.keys(l.session, "Down");
        await l.tmux.keys(l.session, "Enter");
        return "denied the installer (the user said no)";
      }
      const opts = menuOptions(t);
      // Under scope "session", "Allow for this session"; otherwise the first (once), as before.
      const c = policy.scope === "session" ? chooseOption(opts, policy) : null;
      if (c) await selectInMenu(l, opts, c.opt);
      else await l.tmux.keys(l.session, "Enter");
      const chosen = c?.opt.label ?? opts.find((o) => o.cursor)?.label ?? "Allow once";
      l.prompted({ kind: "approval", asks: dialogAsks(t, opts[0]?.label), options: opts.map((o) => o.label), chosen, scope: c?.scope ?? scopeOf(chosen), sessionOffered: opts.some((o) => scopeOf(o.label) === "session"), forcedAlways: c?.forcedAlways ?? false });
      return (INSTALLER.test(t) ? "approved the installer" : "approved") + (c && c.scope !== "once" ? ` (${c.scope})` : "");
    }
    // A y/n prompt, at the very bottom: not the transcript above, where an
    // agent's question ("…this conversation?") and the user's "Yes" would
    // otherwise read as one, and a "y" would go into the chat.
    if (/\[y\/n\]|\(y\/n\)/i.test(tail(screen, 4))) {
      if (INSTALLER.test(t) && !policy.allowInstall && l.state?.picked !== "yes") {
        await l.tmux.type(l.session, "n");
        return "denied the installer (the user said no)";
      }
      await l.tmux.type(l.session, "y");
      l.prompted({ kind: "approval", asks: dialogAsks(tail(screen, 8)), options: ["y", "n"], chosen: "y", scope: "once", sessionOffered: false });
      return "approved";
    }
    return null;
  }

  waiters(_l: Lane, _rows: PsRow[]): PsRow[] {
    return [];
  }

  override leftovers(): PsRow[] {
    return [];
  }

  async cleanup(l: Lane): Promise<void> {
    const name = l.state.container as string | undefined;
    if (name) {
      await exec("docker", ["rm", "-f", name], { timeout: 60_000 }).catch(() => {});
      l.ctx.procs.containers.delete(name);
    }
    const vol = l.state.volume as string | undefined;
    if (vol) await exec("docker", ["volume", "rm", "-f", vol], { timeout: 60_000 }).catch(() => {});
    const dir = l.state.containerDir as string | undefined;
    if (dir) rmSync(dir, { recursive: true, force: true });
  }

  async uninstall(l: Lane): Promise<void> {
    l.note("uninstall: the container is removed with its copied state");
  }
}

/** Container harnesses: the model's tokens so far, from their own session records (best effort). */
export async function tokensUsed(l: Lane): Promise<{ input: number; output: number; cached: number } | null> {
  const name = l.state.container as string | undefined;
  if (!name) return null;
  // Every "usage"-like object in the harness's session files and databases, summed.
  const py = `
import json,os,re,sqlite3,glob
tot={"input":0,"output":0,"cached":0}
def add(u):
  if not isinstance(u,dict): return
  i=u.get("input_tokens",u.get("input",u.get("prompt_tokens",0))) or 0
  o=u.get("output_tokens",u.get("output",u.get("completion_tokens",0))) or 0
  c=u.get("cache_read_input_tokens",u.get("cacheRead",u.get("cached_tokens",0))) or 0
  d=u.get("input_tokens_details") or u.get("prompt_tokens_details") or {}
  if isinstance(d,dict): c=c or d.get("cached_tokens",0) or 0
  if isinstance(i,(int,float)): tot["input"]+=i
  if isinstance(o,(int,float)): tot["output"]+=o
  if isinstance(c,(int,float)): tot["cached"]+=c
def walk(v):
  if isinstance(v,dict):
    if "usage" in v and isinstance(v["usage"],dict): add(v["usage"])
    for x in v.values():
      if isinstance(x,(dict,list)): walk(x)
  elif isinstance(v,list):
    for x in v: walk(x)
for root in ["/home/node/.openclaw/agents","/opt/data/sessions"]:
  for p in glob.glob(root+"/**/*.jsonl",recursive=True):
    for line in open(p,errors="ignore"):
      try: walk(json.loads(line))
      except Exception: pass
db="/opt/data/state.db"
if os.path.exists(db):
  try:
    con=sqlite3.connect(db); cur=con.cursor()
    cols=[r[1] for r in cur.execute("pragma table_info(sessions)")]
    if "input_tokens" in cols:
      for row in cur.execute("select coalesce(sum(input_tokens),0),coalesce(sum(output_tokens),0),coalesce(sum(%s),0) from sessions" % ("cache_read_tokens" if "cache_read_tokens" in cols else "0")):
        tot["input"]+=row[0]; tot["output"]+=row[1]; tot["cached"]+=row[2]
  except Exception: pass
print(json.dumps(tot))
`;
  const node = `
const fs=require("fs"),path=require("path");const tot={input:0,output:0,cached:0};
function add(u){if(!u||typeof u!=="object")return;const n=(x)=>typeof x==="number"?x:0;tot.input+=n(u.input_tokens??u.input??u.prompt_tokens);tot.output+=n(u.output_tokens??u.output??u.completion_tokens);tot.cached+=n(u.cache_read_input_tokens??u.cacheRead??u.cached_tokens??(u.input_tokens_details||{}).cached_tokens)}
function walk(v){if(Array.isArray(v)){v.forEach(walk);return}if(v&&typeof v==="object"){if(v.usage&&typeof v.usage==="object")add(v.usage);for(const x of Object.values(v))if(x&&typeof x==="object")walk(x)}}
function files(d){let out=[];try{for(const e of fs.readdirSync(d,{withFileTypes:true})){const p=path.join(d,e.name);if(e.isDirectory())out=out.concat(files(p));else if(p.endsWith(".jsonl"))out.push(p)}}catch{}return out}
for(const f of files("/home/node/.openclaw/agents"))for(const line of fs.readFileSync(f,"utf8").split("\\n")){try{walk(JSON.parse(line))}catch{}}
console.log(JSON.stringify(tot))`;
  const cmd = l.driver.id.startsWith("openclaw") ? ["node", "-e", node] : ["sh", "-c", `for p in /opt/hermes/.venv/bin/python /opt/hermes/bin/python python3; do [ -x "$p" ] || command -v "$p" >/dev/null 2>&1 && exec "$p" -c "$0"; done`, py];
  try {
    const { stdout } = await exec("docker", ["exec", name, ...cmd], { timeout: 60_000, maxBuffer: 4 << 20 });
    const v = JSON.parse(stdout.trim().split("\n").pop() ?? "{}") as { input?: number; output?: number; cached?: number };
    return { input: v.input ?? 0, output: v.output ?? 0, cached: v.cached ?? 0 };
  } catch {
    return null;
  }
}
