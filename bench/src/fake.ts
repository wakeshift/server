// --target fake: an in-process stand-in for the core and the toy app, for
// the bench's own dry run (CI) with the fake driver (drivers/fake). It
// speaks a tiny HTTP API to the fake agent: watch (gives a fake link and
// session key, so redaction is exercised), a long-poll wait whose open
// request counts as connected, report, unwatch. No secrets are real.

import { randomBytes } from "node:crypto";
import { createServer, type IncomingMessage, type ServerResponse } from "node:http";
import type { Bench, DeliveryTrace, LaneState, StatusState } from "./core.js";
import type { Procs } from "./procs.js";

interface FakeLane {
  seq: number;
  nonce: string;
  events: { id: string; code: string; at: string; matched: number }[];
  pending: { code: string; text: string }[];
  inflight: Map<string, { code: string; text: string }[]>;
  calls: LaneState["calls"];
  seen: LaneState["seen"];
  subscribed: boolean;
  session: string;
  waiting: number;
  lastWait: number;
  via: "stream" | "hooks";
  parked: ((v: unknown) => void)[];
}

export async function startFakeBench(o: { run: string; procs: Procs; log: (m: string) => void }): Promise<Bench> {
  const lanes = new Map<string, FakeLane>();
  // Each event's delivery stages, by code (trace: as a local core's state log has them).
  const stages = new Map<string, { created: string; handed: string | null; finished: string | null }>();
  const lane = (id: string): FakeLane => {
    let l = lanes.get(id);
    if (!l) {
      l = { seq: 0, nonce: randomBytes(2).toString("hex"), events: [], pending: [], inflight: new Map(), calls: [], seen: [], subscribed: false, session: "", waiting: 0, lastWait: 0, via: "stream", parked: [] };
      lanes.set(id, l);
    }
    return l;
  };
  const iso = () => new Date().toISOString();
  const read = async (req: IncomingMessage) => {
    let s = "";
    for await (const c of req) s += c;
    return s ? JSON.parse(s) : {};
  };
  const json = (res: ServerResponse, code: number, v: unknown) => {
    res.writeHead(code, { "content-type": "application/json" });
    res.end(JSON.stringify(v));
  };
  const wakeParked = (l: FakeLane) => {
    for (const p of l.parked.splice(0)) p(null);
  };
  let base = "";

  const server = createServer(async (req, res) => {
    try {
      const url = new URL(req.url ?? "/", "http://fake");
      const m = /^\/fake\/([a-z0-9-]+)\/(watch|wait|report|unwatch)$/.exec(url.pathname);
      if (!m) return json(res, url.pathname === "/health" ? 200 : 404, {});
      const l = lane(m[1]);
      if (m[2] === "watch") {
        const b = await read(req);
        if (!l.session) l.session = "wsc_" + randomBytes(12).toString("base64url");
        if (b.via === "hooks") l.via = "hooks";
        l.subscribed = true;
        // Unconfirmed (and held) messages come with the result while nothing waits.
        const held = l.waiting === 0 ? [...l.pending, ...[...l.inflight.values()].flat()] : [];
        if (held.length) {
          l.pending = [];
          l.inflight.clear();
        }
        const token = randomBytes(24).toString("base64url");
        const text =
          `Watching bench events. To be woken, run the waiter with ${base}/fake/${m[1]}/wait?link=${token}&ack= ; ` +
          `pass session_key="${l.session}" when you call this tool again. Wake-ups without the waiter need a one-time install: ask the user first.` +
          (held.length ? `\nWaiting messages: ${held.map((h) => h.text).join(" ")}` : "");
        l.calls.push({
          at: iso(),
          tool: "watch",
          client: "fake",
          args: { harness: b.harness ?? null, conversation_id: null, session_key: b.session_key ? "k" + String(b.session_key).length : null },
          agent: { harness: "fake", via: "client", claimed: b.harness ?? null },
          subscription: "sub_" + m[1],
          session: "k" + l.session.length,
          added: text.replace(/link=[^&\s]+/, "link=‹redacted›").replace(/wsc_[A-Za-z0-9_-]+/, "wsc_‹redacted›"),
          beacon: false,
          events_in_result: held.map((h) => h.code),
        });
        return json(res, 200, { text, session: l.session, token });
      }
      if (m[2] === "unwatch") {
        l.subscribed = false;
        l.calls.push({ at: iso(), tool: "unwatch", client: "fake", args: { harness: null, conversation_id: null, session_key: "k" + l.session.length }, events_in_result: [] });
        return json(res, 200, { ok: true });
      }
      if (m[2] === "report") {
        const b = await read(req);
        l.seen.push({ at: iso(), ids: b.ids ?? [] });
        return json(res, 200, { ok: true });
      }
      // wait: confirm the last batch, then hold until something comes (or 20s).
      const ack = url.searchParams.get("ack");
      if (ack) {
        for (const e of l.inflight.get(ack) ?? []) {
          const st = stages.get(e.code);
          if (st) st.finished ??= iso();
        }
        l.inflight.delete(ack);
      }
      // What a dead waiter took but never confirmed comes back.
      for (const [k, v] of l.inflight) {
        l.pending.unshift(...v);
        l.inflight.delete(k);
      }
      l.waiting++;
      l.lastWait = Date.now();
      let closed = false;
      res.on("close", () => {
        if (!closed) {
          closed = true;
          l.waiting--;
          l.lastWait = Date.now();
        }
      });
      const end = Date.now() + 20_000;
      while (!l.pending.length && Date.now() < end && !res.destroyed) {
        await new Promise((r) => {
          l.parked.push(r);
          setTimeout(r, 1000);
        });
      }
      const batch = l.pending.splice(0);
      const id = randomBytes(4).toString("hex");
      if (batch.length) l.inflight.set(id, batch);
      for (const e of batch) {
        const st = stages.get(e.code);
        if (st) st.handed ??= iso();
      }
      if (!res.destroyed) json(res, 200, { events: batch, next: `${base}/fake/${m[1]}/wait?link=${randomBytes(24).toString("base64url")}&ack=${batch.length ? id : ""}` });
    } catch (err) {
      if (!res.headersSent) json(res, 500, { error: String(err) });
    }
  });
  await new Promise<void>((r) => server.listen(0, "127.0.0.1", () => r()));
  const a = server.address();
  base = `http://127.0.0.1:${typeof a === "object" && a ? a.port : 0}`;
  o.log(`fake core and app on ${base}`);

  return {
    target: "fake",
    core: base,
    agentPlugins: null,
    antigravityPlugin: null,
    app: base,
    mcpUrl: (id) => `${base}/fake/${id}`,
    publish: async (id, count = 1, tag) => {
      const l = lane(id);
      const out = [];
      for (let i = 0; i < count; i++) {
        const code = `WB-${l.nonce}-${++l.seq}`;
        const now = iso();
        const e = { id: `fake:${id}:${l.seq}`, code, at: now, accepted_at: now, matched: l.subscribed ? 1 : 0 };
        l.events.push(e);
        if (l.subscribed) {
          l.pending.push({ code, text: `Bench event ${code}${tag ? ` (${tag})` : ""}.` });
          stages.set(code, { created: now, handed: null, finished: null });
        }
        out.push(e);
      }
      wakeParked(l);
      return out;
    },
    trace: async (eventId): Promise<DeliveryTrace | null> => {
      const e = [...lanes.values()].flatMap((l) => l.events).find((x) => x.id === eventId);
      const st = e ? stages.get(e.code) : undefined;
      return st ? { created_at: st.created, available_at: st.created, handed_at: st.handed, finished_at: st.finished, state: st.finished ? "delivered" : st.handed ? "in_flight" : "pending", attempts: st.handed ? 1 : 0 } : null;
    },
    lane: async (id) => {
      const l = lane(id);
      return { calls: l.calls, events: l.events, seen: l.seen, subscriptions: l.subscribed ? ["sub_" + id] : [], deliveries: null };
    },
    status: async (id): Promise<StatusState> => {
      const l = lane(id);
      const connected = l.subscribed && (l.waiting > 0 || Date.now() - l.lastWait < 2000);
      return {
        event: { subscriptions: l.subscribed ? 1 : 0, connected: connected ? 1 : 0, via: { stream: connected && l.via === "stream" ? 1 : 0, poll: 0, hooks: connected && l.via === "hooks" ? 1 : 0 } },
        subscriptions: l.subscribed ? [{ id: "sub_" + id, user: id, live: true, connected, via: connected ? l.via : undefined }] : [],
      };
    },
    stop: async () => {
      server.closeAllConnections();
      await new Promise<void>((r) => server.close(() => r()));
    },
  };
}
