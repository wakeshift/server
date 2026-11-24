// Runs the bench's toy app (server.ts).
//
//   BENCH_KEY_FILE        a file holding the app key (wst_…, or an older wst_test_…), mode 0600: never printed
//   WAKESHIFT_CORE        the core the key came from
//   BENCH_RUN             the run ID (topics are bench/<run>/<lane>)
//   BENCH_CONTROL_FILE    a file holding the control API's bearer token, mode 0600
//   BENCH_CORE_DB         local cores only: Postgres URL of the core's database, read
//                         for delivery state (never set for a hosted core)
//   PORT                  default 0 (any); the chosen port is printed as {"listening": port}
//   BENCH_CHECK_APP=1     only say which app the key is ({"app": id}, from GET /app,
//                         which can't change the manifest) and exit: nothing else is sent
//   BENCH_APP_SLOT        2 for S15's second app: event codes with a nonce of their own
//   BENCH_MCP_NAME        the MCP server's name (default wakebench)
//   BENCH_REQUIRE_APP     refuse to serve unless the key is this app
//   BENCH_FORBID_APP      refuse to serve if the key is this app (the run's first app)
//   BENCH_PROTECTED_APP   an app whose manifest the bench must leave as it is: only
//                         server.ts (ts-v2) runs on it, never the CI app or a port
//                         (a target sets it: src/extensions.ts, toyEnv)
//   BENCH_APP             the variant (WB_APP: ts-v2, the default, ts-v1, go-sdk, …; variants.ts)
//   BENCH_PORT_CMD        a port's command, as JSON {argv, cwd} (the bench builds it)
import type { ChildProcess } from "node:child_process";
import { readFileSync, statSync } from "node:fs";
import type { Server } from "node:http";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import pg from "pg";
import { Wakeshift } from "@wakeshift/sdk";
import { freePort } from "../src/core.js";
import { front, loadContract, startPort } from "./front.js";
import { benchWait } from "./tasks.js";
import { benchApp, type DeliveryRow } from "./server.js";
import { ciApp } from "./ci.js";
import { DEFAULT_APP, isPort, type AppId, type PortCmd } from "./variants.js";

function secretFile(name: string): string {
  const path = process.env[name];
  if (!path) throw new Error(`set ${name}`);
  const st = statSync(path);
  if ((st.mode & 0o077) !== 0) throw new Error(`${name} must be readable only by you (chmod 600)`);
  return readFileSync(path, "utf8").trim();
}

// The SDK logs to stderr: stdout carries this process's JSON lines.
const toStderr = (m: string) => console.error(m);
const wake = new Wakeshift({ key: secretFile("BENCH_KEY_FILE"), core: process.env.WAKESHIFT_CORE, logger: { error: toStderr, warn: toStderr, info: toStderr } });
const core = wake.toJSON().core;
// A current-format key doesn't name its app: wake.appId needs this first.
// GET /app only reads; nothing below it has run yet (no manifest, no event).
await wake.app();
if (process.env.BENCH_CHECK_APP === "1") {
  console.log(JSON.stringify({ app: wake.appId }));
  process.exit(0);
}
// The second app's guards, again in the process that serves it (the bench
// checked the key's app before starting it).
const mustBe = process.env.BENCH_REQUIRE_APP;
if (mustBe && wake.appId !== mustBe) {
  console.error(JSON.stringify({ refused: `the key is for ${wake.appId}, not ${mustBe}` }));
  process.exit(3);
}
const forbid = process.env.BENCH_FORBID_APP;
if (forbid && wake.appId === forbid) {
  console.error(JSON.stringify({ refused: `the key is for ${forbid}, the run's first app` }));
  process.exit(3);
}
const run = process.env.BENCH_RUN ?? "manual";
let deliveries: ((app: string, topic: string) => Promise<DeliveryRow[]>) | undefined;
if (process.env.BENCH_CORE_DB) {
  const pool = new pg.Pool({ connectionString: process.env.BENCH_CORE_DB, max: 2 });
  deliveries = async (app, topic) => {
    const r = await pool.query(
      `SELECT te.source_id AS event, d.state, d.attempts, d.created_at, d.finished_at, d.subscription_id::text AS subscription
         FROM topic_events te JOIN deliveries d ON d.event_id = te.event_id
        WHERE te.app_id = $1 AND $2 = ANY (te.topics)
        ORDER BY d.created_at`,
      [app, topic],
    );
    return r.rows.map((x) => ({ ...x, created_at: new Date(x.created_at).toISOString(), finished_at: x.finished_at ? new Date(x.finished_at).toISOString() : null }));
  };
}
const slot = process.env.BENCH_APP_SLOT === "2" ? 2 : 1;
const controlToken = secretFile("BENCH_CONTROL_FILE");
// The realistic tier's app (BENCH_APP=ci, app/ci.ts): a CI service, never
// on the protected app (its manifest stays the toy app's), and only on the
// app the bench checked (BENCH_REQUIRE_APP, above).
const ci = process.env.BENCH_APP === "ci";
const protectedApp = process.env.BENCH_PROTECTED_APP;
if (ci && protectedApp && wake.appId === protectedApp) {
  console.error(JSON.stringify({ refused: `the CI app doesn't run on ${protectedApp}, whose manifest the bench mustn't change` }));
  process.exit(3);
}
// A port (WB_APP=go-sdk, py-mcpserver, …): the Go or Python toy app serves
// MCP, and this process is its front (front.ts). Its command comes from
// the bench, which built it (variants.ts, preparePort).
const app = (ci ? DEFAULT_APP : process.env.BENCH_APP || DEFAULT_APP) as AppId;
let port: ChildProcess | null = null;
let server: Server;
// Each variant's SDK or adapter sends its own manifest, which could differ
// in detail from server.ts's: only server.ts runs on the app whose manifest
// the bench must leave as it is.
if (app !== DEFAULT_APP && protectedApp && wake.appId === protectedApp) {
  console.error(JSON.stringify({ refused: `${app} doesn't run on ${protectedApp}, whose manifest the bench mustn't change` }));
  process.exit(3);
}
if (ci) {
  server = ciApp({ wake, run, controlToken });
} else if (isPort(app)) {
  const cmd = JSON.parse(process.env.BENCH_PORT_CMD ?? "null") as PortCmd | null;
  if (!cmd?.argv?.length) throw new Error(`${app} needs BENCH_PORT_CMD (the bench builds the port)`);
  const here = dirname(fileURLToPath(import.meta.url));
  const contractPath = resolve(here, "../../app/contract.json");
  const portPort = await freePort();
  port = await startPort({
    cmd,
    port: portPort,
    timeoutMs: 120_000,
    log: (line) => console.error(line),
    env: {
      PATH: process.env.PATH ?? "",
      HOME: process.env.HOME ?? "",
      BENCH_KEY_FILE: process.env.BENCH_KEY_FILE!,
      BENCH_CONTROL_FILE: process.env.BENCH_CONTROL_FILE!,
      WAKESHIFT_CORE: core,
      BENCH_CONTRACT: contractPath,
      BENCH_RUN: run,
      BENCH_REQUIRE_APP: wake.appId ?? "",
      ...(process.env.BENCH_MCP_NAME ? { BENCH_MCP_NAME: process.env.BENCH_MCP_NAME } : {}),
      ...(benchWait() ? { BENCH_WAIT: "1" } : {}),
    },
  });
  port.on("exit", (code) => {
    console.error(JSON.stringify({ port_exited: code }));
    process.exit(1);
  });
  server = front({ port: `http://127.0.0.1:${portPort}`, run, appId: wake.appId ?? "", contract: loadContract(contractPath), controlToken, slot, deliveries, log: (l) => console.error(l) });
} else {
  server = benchApp({ wake, run, controlToken, deliveries, slot, serverName: process.env.BENCH_MCP_NAME || undefined });
}
server.listen(Number(process.env.PORT ?? 0), "127.0.0.1", () => {
  const a = server.address();
  console.log(JSON.stringify({ listening: typeof a === "object" && a ? a.port : null, app: wake.appId, core, variant: ci ? "ci" : app }));
});
for (const sig of ["SIGTERM", "SIGINT"] as const)
  process.on(sig, () => {
    port?.removeAllListeners("exit");
    port?.kill("SIGTERM");
    server.close(() => process.exit(0));
  });
