// The realistic tier's push lane (R7-R9; the lane's flow is Realistic.pushLane
// in realistic.ts, beside the tier's other lanes): which scenarios it is, and
// how one ask's outcome is read from the CI app's records (the subscribing
// result, the agent's own events/stream) and the agent's words about the
// install.

import type { CiCall, CiStream } from "../app/ci.js";
import type { ScenarioId } from "./model.js";

/** The push lane's scenarios (R7-R9), run only where a harness subscribes itself with push (Driver.pushes). */
export const PUSH_LANE: ScenarioId[] = ["R7", "R8", "R9"];

/** The native line Wakeshift gives a push client: "To be woken by … subscribe to it (MCP Events)", with a voucher for a tools-only event. */
export const NATIVE_LINE = /To be woken by [\w.]+ with [^\n]*?subscribe to it[^\n]*\(MCP Events\)/;

/**
 * What one push-lane ask did (R7, R9), and whether it works: the agent
 * started the build and called the subscribing tool, whose result gave
 * the native line and no install offer; it then opened a stream for that
 * build itself (events/stream, opencode's watch_mcp_event); and it put no
 * install to the user (how: realistic.ts offerHow; relayed: it reached the user).
 */
export function pushOutcome(o: { build: number | null; sub: CiCall | undefined; added: string; streams: CiStream[]; how: string; relayed: boolean; cmd: string | null }): {
  works: boolean;
  reason?: string;
  metrics: Record<string, string | number | boolean | null>;
} {
  const nativeLine = NATIVE_LINE.test(o.added);
  const offer = !!o.cmd || /install|plugin|can't wake this conversation|won't be notified/i.test(o.added);
  const mine = o.streams.filter((s) => s.event === "build.finished" && (o.build === null || Number(s.arguments.number) === o.build));
  const metrics = {
    started_build: o.build !== null,
    build: o.build,
    subscribed_via: o.sub?.tool ?? null,
    native_line: nativeLine,
    offer_in_result: offer,
    offer_how: o.how,
    offer_relayed: o.relayed,
    streamed: mine.length > 0,
    streams: o.streams.length,
    stream_protocol: mine[0]?.protocol ?? null,
  };
  const reason =
    o.build === null
      ? "the agent didn't start the build"
      : !o.sub
        ? "the agent didn't call the subscribing tool"
        : !nativeLine
          ? "the subscribing result had no native line"
          : offer
            ? "the subscribing result offered the install beside the native line"
            : o.relayed
              ? "the agent put an install to the user"
              : mine.length === 0
                ? "the agent didn't subscribe itself (no events/stream for the build)"
                : undefined;
  return { works: reason === undefined, ...(reason ? { reason } : {}), metrics };
}
