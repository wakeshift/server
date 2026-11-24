// The toy app's variants (WB_APP; bench/app/CONTRACT.md): the same app on
// each MCP server framework the SDKs support. ts-v2 is server.ts itself (the
// default) and ts-mastra its Mastra route (server-mastra.ts); the others are
// ports, run behind front.ts.

import { execFile } from "node:child_process";
import { createHash } from "node:crypto";
import { existsSync, mkdirSync, readFileSync, rmSync, statSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { promisify } from "node:util";

const exec = promisify(execFile);

export type AppId = "ts-v2" | "ts-v1" | "ts-mastra" | "go-sdk" | "go-mcpgo" | "py-mcpserver" | "py-fastmcp" | "py-lowlevel";

export interface Variant {
  id: AppId;
  lang: "ts" | "go" | "python";
  /** The MCP server framework, as developers know it. */
  framework: string;
  /** Which Wakeshift SDK attaches to it. */
  sdk: string;
  /** A port's entry: the directory under bench/app (Node), the package under bench/app/go, the script in bench/app/python. None: server.ts. */
  entry?: string;
  /** server.ts variants: the environment that selects their route (server-mastra.ts). */
  env?: Record<string, string>;
  /** Not runnable yet, and why (the SDK can't attach to it in this checkout). */
  pending?: string;
}

export const VARIANTS: Variant[] = [
  { id: "ts-v2", lang: "ts", framework: "@modelcontextprotocol/server 2.x (sessions; createMcpHandler for 2026-07-28)", sdk: "@wakeshift/sdk" },
  { id: "ts-v1", lang: "ts", framework: "@modelcontextprotocol/sdk 1.x (McpServer, a session per client)", sdk: "@wakeshift/sdk", entry: "node-v1" },
  { id: "ts-mastra", lang: "ts", framework: "Mastra MCPServer (@mastra/mcp, startHTTP)", sdk: "@wakeshift/sdk/mastra", env: { WB_MCP_SDK: "mastra" } },
  { id: "go-sdk", lang: "go", framework: "modelcontextprotocol/go-sdk (stateless Streamable HTTP)", sdk: "pkg/wakeshift", entry: "gosdk" },
  { id: "go-mcpgo", lang: "go", framework: "mark3labs/mcp-go (Streamable HTTP)", sdk: "pkg/wakeshift/mcpgo", entry: "mcpgo" },
  { id: "py-mcpserver", lang: "python", framework: "mcp 2.x MCPServer (streamable_http_app)", sdk: "wakeshift (Python)", entry: "mcpserver_app.py" },
  { id: "py-fastmcp", lang: "python", framework: "FastMCP 4 (fastmcp, http_app)", sdk: "wakeshift[fastmcp] (Python)", entry: "fastmcp_app.py" },
  { id: "py-lowlevel", lang: "python", framework: "mcp 2.x low-level Server (streamable_http_app)", sdk: "wakeshift (Python)", entry: "lowlevel_app.py" },
];

export const APP_IDS = VARIANTS.map((v) => v.id);

/** The toy app the bench has always run, server.ts: the reference the others are compared with. */
export const DEFAULT_APP: AppId = "ts-v2";

export function variant(id: string): Variant {
  const v = VARIANTS.find((x) => x.id === id);
  if (!v) throw new Error(`unknown app ${JSON.stringify(id)}: one of ${APP_IDS.join(", ")}`);
  return v;
}

/**
 * Which app the run's first toy app is: WB_APP, else ts-v2 (server.ts).
 * WB_MCP_SDK, the setting WB_APP generalizes, names the MCP SDK's major
 * line or framework: v1 is ts-v1, mastra ts-mastra, v2 (server.ts is on 2.x) the default.
 */
export function appFromEnv(env: NodeJS.ProcessEnv = process.env): AppId {
  if (env.WB_APP) return variant(env.WB_APP.trim()).id;
  return env.WB_MCP_SDK === "v1" ? "ts-v1" : env.WB_MCP_SDK === "mastra" ? "ts-mastra" : DEFAULT_APP;
}

/** S15's second toy app: WB_APP_2, else the first's. */
export function app2FromEnv(env: NodeJS.ProcessEnv = process.env): AppId {
  return env.WB_APP_2 ? variant(env.WB_APP_2.trim()).id : appFromEnv(env);
}

/** A variant with an entry of its own runs as a port behind front.ts (server.ts's routes don't). */
export function isPort(id: AppId): boolean {
  return variant(id).entry !== undefined;
}

/** How the front starts a port: argv and working directory. */
export interface PortCmd {
  argv: string[];
  cwd?: string;
}

/**
 * Builds a port (Node: npm ci, which copies this checkout's SDK in; Go: go
 * build into dir; Python: uv sync, each from its lockfile) and returns how to
 * start it. server.ts needs nothing.
 */
export async function preparePort(id: AppId, repo: string, dir: string): Promise<PortCmd | null> {
  const v = variant(id);
  if (v.pending) throw new Error(`${id} isn't runnable yet: ${v.pending}`);
  if (!v.entry) return null;
  if (v.lang === "ts") {
    const cwd = join(repo, "bench/app", v.entry);
    await installOnce(cwd, join(repo, "sdk/typescript/dist/index.js"));
    return { argv: [process.execPath, "server.mjs"], cwd };
  }
  if (v.lang === "go") {
    // dir is the bench run's own (<run>/core/toy), and a run prepares its
    // two apps one after the other: no other build writes this path.
    const bin = join(dir, `toy-${id}`);
    if (!existsSync(bin)) await exec("go", ["build", "-o", bin, `./bench/app/go/${v.entry}`], { cwd: repo, maxBuffer: 16 << 20 });
    return { argv: [bin] };
  }
  const py = join(repo, "bench/app/python");
  await exec("uv", ["sync", "--frozen", "--quiet"], { cwd: py, maxBuffer: 16 << 20 });
  return { argv: ["uv", "run", "--frozen", "--quiet", "python", v.entry!], cwd: py };
}

/**
 * npm ci in a Node port's directory, once for its lockfile and this
 * checkout's SDK build (a stamp in node_modules), under a lock: bench runs
 * at once share the directory, and npm ci empties node_modules first.
 */
async function installOnce(cwd: string, sdk: string): Promise<void> {
  const st = statSync(sdk);
  const stamp = createHash("sha256").update(readFileSync(join(cwd, "package-lock.json"))).update(`${st.size}:${st.mtimeMs}`).digest("hex");
  const stampFile = join(cwd, "node_modules", ".wakebench-stamp");
  const lock = join(cwd, ".install-lock");
  const deadline = Date.now() + 300_000;
  for (;;) {
    try {
      mkdirSync(lock);
      break;
    } catch {
      // Another run is installing (a lock older than 5 minutes was left by one that died).
      try {
        if (Date.now() - statSync(lock).mtimeMs > 300_000) rmSync(lock, { recursive: true, force: true });
      } catch {
        // gone already
      }
      if (Date.now() > deadline) throw new Error(`${cwd}: another bench run's npm ci didn't finish`);
      await new Promise((r) => setTimeout(r, 500));
    }
  }
  try {
    if (existsSync(stampFile) && readFileSync(stampFile, "utf8") === stamp) return;
    await exec("npm", ["ci", "--no-fund", "--no-audit", "--silent"], { cwd, maxBuffer: 16 << 20 });
    writeFileSync(stampFile, stamp);
  } finally {
    rmSync(lock, { recursive: true, force: true });
  }
}
