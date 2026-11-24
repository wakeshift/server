// Who a request is from, for the per-address limits (server.ts: new links,
// posts an hour, requests in flight, open streams): the client's IP as the
// proxies in front saw it, and the key it's counted under.

import type { IncomingMessage } from "node:http";
import { isIP } from "node:net";

/**
 * The client's IP: the entry the hops proxies in front added to
 * X-Forwarded-For, else (fewer entries, or one that isn't an IP address,
 * so the header isn't theirs) the socket's; "" if there's neither. As the
 * analytics proxy finds it (clientAddress in analytics.ts, a copy of
 * apps/analytics that this can't share).
 */
export function clientIp(req: Pick<IncomingMessage, "headers" | "socket">, hops: number): string {
  if (hops > 0) {
    const h = req.headers["x-forwarded-for"];
    const list = (Array.isArray(h) ? h[0] : (h ?? "")).split(",").map((s) => s.trim());
    const ip = list.length >= hops ? list[list.length - hops] : "";
    if (isIP(ip.split("%")[0])) return ip;
  }
  return req.socket.remoteAddress ?? "";
}

/**
 * What a client is counted as: an IPv4 address as it is, an IPv6 one by
 * its /64 (one customer's usual allocation, so cycling through it doesn't
 * make a new address). An IPv4 address carried in IPv6 (IPv4-mapped, or
 * NAT64's 64:ff9b::/96) is that IPv4 address, so a translator's clients
 * aren't all one. "" for "".
 */
export function addressKey(ip: string): string {
  const v4 = ip.match(/^(?:::ffff:)?(\d{1,3}(?:\.\d{1,3}){3})$/i);
  if (v4) return v4[1];
  if (!ip.includes(":")) return ip;
  const g = ipv6Groups(ip.split("%")[0].toLowerCase());
  if (!g) return ip;
  const embedded = `${g[6] >> 8}.${g[6] & 255}.${g[7] >> 8}.${g[7] & 255}`;
  if (g[0] === 0x64 && g[1] === 0xff9b && g.slice(2, 6).every((x) => x === 0)) return embedded;
  if (g.slice(0, 5).every((x) => x === 0) && g[5] === 0xffff) return embedded;
  return g.slice(0, 4).map((x) => x.toString(16)).join(":") + "::/64";
}

/** An IPv6 address's eight groups (an IPv4 tail as the last two), or undefined if it isn't one. */
function ipv6Groups(addr: string): number[] | undefined {
  let s = addr;
  const tail: number[] = [];
  const dotted = s.match(/^(.*:)(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})$/);
  if (dotted) {
    const o = dotted.slice(2, 6).map(Number);
    if (o.some((x) => x > 255)) return undefined;
    tail.push((o[0] << 8) | o[1], (o[2] << 8) | o[3]);
    s = dotted[1].endsWith("::") ? dotted[1] : dotted[1].slice(0, -1);
  }
  const halves = s.split("::");
  if (halves.length > 2) return undefined;
  const groups = (h: string) => (h ? h.split(":") : []).map((x) => (/^[0-9a-f]{1,4}$/.test(x) ? parseInt(x, 16) : NaN));
  const left = groups(halves[0]);
  const right = halves.length === 2 ? groups(halves[1]) : [];
  if ([...left, ...right].some(Number.isNaN)) return undefined;
  const have = left.length + right.length + tail.length;
  if (halves.length === 1 ? have !== 8 : have > 7) return undefined;
  return [...left, ...Array(8 - have).fill(0), ...right, ...tail];
}
