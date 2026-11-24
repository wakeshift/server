// TRY_CORE_ADDR: where requests to Wakeshift go when that isn't
// WAKESHIFT_CORE itself. App assertions are addressed to WAKESHIFT_CORE, the
// server's public URL, so it stays that; inside docker compose the server
// is reached at another address (http://server:8370), and requests to
// WAKESHIFT_CORE's origin are sent there instead. Nothing else changes: the
// path, query, method, headers and body are the same.

/** A fetch that sends requests for core's origin to addr's, and any other request as it is. */
export function fetchVia(core: string, addr: string, base: typeof fetch = (...a) => globalThis.fetch(...a)): typeof fetch {
  const from = new URL(core).origin;
  const to = new URL(addr);
  if (to.protocol !== "http:" && to.protocol !== "https:") throw new Error("TRY_CORE_ADDR is an http:// or https:// URL");
  const moved = (url: string): string | undefined => {
    const u = new URL(url);
    if (u.origin !== from) return undefined;
    u.protocol = to.protocol;
    u.host = to.host;
    return u.href;
  };
  return ((input: string | URL | Request, init?: RequestInit) => {
    if (input instanceof Request) {
      const url = moved(input.url);
      return base(url ? new Request(url, input) : input, init);
    }
    const url = moved(String(input));
    return base(url ?? input, init);
  }) as typeof fetch;
}
