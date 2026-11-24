// The delivery state log (TRANSITIONS_SQL) against a real Postgres: the dev
// one from `make db` (skipped when it isn't running, as in the bench's CI
// job). It runs in a database of its own, dropped at the end.

import { strict as assert } from "node:assert";
import { randomBytes, randomUUID } from "node:crypto";
import { test } from "node:test";
import pg from "pg";
import { DEV_ADMIN_DB, TRANSITIONS_SQL, traceOf } from "../src/core.js";

async function reachable(): Promise<boolean> {
  const c = new pg.Client({ connectionString: DEV_ADMIN_DB, connectionTimeoutMillis: 1500 });
  try {
    await c.connect();
    await c.end();
    return true;
  } catch {
    return false;
  }
}

test("the trigger records each state change, even an ack right after the hand-off", async (t) => {
  if (!(await reachable())) return t.skip("no dev Postgres on localhost:5439 (make db)");
  const name = "wakebench_trigger_" + randomBytes(4).toString("hex");
  const admin = new pg.Client({ connectionString: DEV_ADMIN_DB });
  await admin.connect();
  await admin.query(`CREATE DATABASE ${name}`);
  const db = new pg.Client({ connectionString: DEV_ADMIN_DB.replace("/postgres?", `/${name}?`) });
  try {
    await db.connect();
    await db.query(`CREATE TABLE deliveries (id uuid PRIMARY KEY, event_id uuid NOT NULL, state text NOT NULL)`);
    await db.query(TRANSITIONS_SQL);
    await db.query(TRANSITIONS_SQL); // installing twice is fine
    const id = randomUUID();
    await db.query(`INSERT INTO deliveries VALUES ($1, $2, 'pending')`, [id, randomUUID()]);
    await db.query(`UPDATE deliveries SET state = 'in_flight' WHERE id = $1`, [id]);
    await db.query(`UPDATE deliveries SET state = 'in_flight' WHERE id = $1`, [id]); // no change: not recorded
    await db.query(`UPDATE deliveries SET state = 'acked' WHERE id = $1`, [id]);
    const r = await db.query(`SELECT state, at FROM wakebench_delivery_states WHERE delivery_id = $1 ORDER BY at`, [id]);
    assert.deepEqual(
      r.rows.map((x) => x.state),
      ["pending", "in_flight", "acked"],
    );
    const tr = traceOf({ created_at: new Date(), available_at: new Date(), state: "acked", attempts: 1 }, r.rows)!;
    assert.ok(tr.handed_at && tr.finished_at && tr.handed_at <= tr.finished_at);
  } finally {
    await db.end().catch(() => {});
    await admin.query(`DROP DATABASE IF EXISTS ${name} WITH (FORCE)`);
    await admin.end();
  }
});
