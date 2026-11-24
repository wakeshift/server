// try.wakeshift.ai: a webhook URL that wakes your agent. See README.md.
//
//   WAKESHIFT_KEY       the app's key (wst_test_… from the console, or wst_live_try_… for an env-registered app)
//   WAKESHIFT_CORE      Wakeshift's public URL (default https://app.wakeshift.ai): app assertions are addressed to it
//   TRY_CORE_ADDR       where requests to Wakeshift go, if not WAKESHIFT_CORE (docker compose: http://server:8370)
//   TRY_SECRET          signs links (at least 32 random bytes; changing it ends every link)
//   TRY_DATABASE_URL    Postgres for what's kept per webhook (every instance shares it)
//   TRY_MIGRATE_DATABASE_URL
//                       the database's owner, which migrates it at startup, if TRY_DATABASE_URL's role only reads and writes
//   TRY_DB_POOL         connections per instance (default 3; one more LISTENs)
//   PUBLIC_URL          this server's public URL (default http://localhost:$PORT)
//   PORT                default 8380
//   TRY_HOST            the address to listen on (default: every interface; 127.0.0.1 for this machine only)
//   TRY_PROXY_HOPS      how many proxies in front append to X-Forwarded-For (default 0: the socket's address)
//   TRY_PER_HOUR, TRY_PER_DAY, TRY_GLOBAL_PER_HOUR, TRY_ADDRESS_PER_HOUR, TRY_LINKS_PER_HOUR
//                       the limits (defaults 60, 500, 20000, 1000, 30)
//   WAKESHIFT_POSTHOG_KEY, WAKESHIFT_POSTHOG_HOST
//                       PostHog's project key and host: visits to the landing page; unset key: off
//   TRY_HOSTED          1 only for Wakeshift's own deployment (try.wakeshift.ai): serves and links its terms and privacy policy
import { Wakeshift } from "@wakeshift/sdk";
import { analyticsFromEnv } from "./analytics.js";
import { fetchVia } from "./coreaddr.js";
import { Db } from "./db.js";
import { logLine, tryServer } from "./server.js";

function env(name: string, fallback?: string): string {
  const v = process.env[name]?.trim() || fallback;
  if (!v) {
    console.error(`try: set ${name}`);
    process.exit(2);
  }
  return v;
}

const log = (level: string, msg: string, attrs: Record<string, unknown> = {}) => process.stdout.write(logLine(level, msg, attrs) + "\n");

function limitsFromEnv() {
  const out: Record<string, number> = {};
  for (const [name, key] of [
    ["TRY_PER_HOUR", "perHour"],
    ["TRY_PER_DAY", "perDay"],
    ["TRY_GLOBAL_PER_HOUR", "globalPerHour"],
    ["TRY_ADDRESS_PER_HOUR", "addressPerHour"],
    ["TRY_LINKS_PER_HOUR", "linksPerHour"],
  ]) {
    const v = Number(process.env[name]);
    if (v > 0) out[key] = v;
  }
  return out;
}

const port = Number(env("PORT", "8380"));
// Unset: every interface (as in a container). TRY_HOST=127.0.0.1 keeps a
// run on your own machine to it.
const host = process.env.TRY_HOST?.trim() || undefined;
// TRY_SECRET signs every link: at least 32 bytes, and never a published
// example (the README's before, the published .env.example's), which
// anyone can read.
const EXAMPLE_SECRETS = new Set(["dev-try-secret-at-least-32-bytes-long", "replace-with-32-or-more-random-bytes"]);
const secret = env("TRY_SECRET");
if (Buffer.byteLength(secret) < 32 || EXAMPLE_SECRETS.has(secret)) {
  console.error("try: TRY_SECRET must be at least 32 random bytes of your own (openssl rand -base64 48)");
  process.exit(2);
}
const coreUrl = process.env.WAKESHIFT_CORE?.trim() || undefined;
const coreAddr = process.env.TRY_CORE_ADDR?.trim();
if (coreAddr && !coreUrl) {
  console.error("try: TRY_CORE_ADDR needs WAKESHIFT_CORE (the public URL it stands in for)");
  process.exit(2);
}
const wake = new Wakeshift({ key: env("WAKESHIFT_KEY"), core: coreUrl, fetch: coreAddr ? fetchVia(coreUrl!, coreAddr) : undefined });
const dbUrl = env("TRY_DATABASE_URL");
const migrateUrl = process.env.TRY_MIGRATE_DATABASE_URL?.trim();
if (migrateUrl) {
  // The owner migrates, before the app's own pool exists; the app then
  // runs as a role that only reads and writes (the owner's default
  // privileges give it the tables).
  const owner = new Db(migrateUrl, { max: 1, log });
  try {
    await owner.migrate();
  } finally {
    await owner.close();
  }
}
const db = new Db(dbUrl, { max: Number(process.env.TRY_DB_POOL ?? 3) || 3, log });
if (!migrateUrl) await db.migrate();
const server = tryServer({
  wake,
  db,
  secret,
  publicUrl: env("PUBLIC_URL", `http://localhost:${port}`),
  proxyHops: Number(process.env.TRY_PROXY_HOPS ?? 0) || 0,
  analytics: analyticsFromEnv(),
  limits: limitsFromEnv(),
  log,
});
server.listen(port, host, () => {
  const core = wake.toJSON().core;
  wake.app().then(
    (app) => log("info", "try listening", { host: host ?? "*", port, app: app.id, core }),
    (err) => log("error", "try listening, but Wakeshift didn't say which app the key is", { host: host ?? "*", port, core, err: String(err) }),
  );
});
for (const sig of ["SIGTERM", "SIGINT"] as const) {
  process.on(sig, () => {
    server
      .stop()
      .then(() => db.close())
      .finally(() => process.exit(0));
  });
}
