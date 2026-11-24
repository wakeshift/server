// What actually reached the model: each bench code's arrivals, counted from
// the harness's own record of the conversation (Claude Code's session
// transcripts, Codex's rollout files, Hermes's state DB), not from what the
// model chose to report with report_seen. A model that notices a duplicate
// usually reports the code once, so report_seen alone can't see one (a bug
// that delivered every Monitor event twice was graded a pass 9 times out of 10).
//
// A driver turns its harness's record into Received items: each thing put
// in front of the model with its text (a user message, a queued Monitor
// notification, a Wakeshift tool result). arrivalsIn splits each into the
// deliveries it carries (a <wakeshift_message> element, an event line) and
// counts every code once per delivery; countArrivals says, per code, how many
// times it arrived and whether the extra arrivals said "redelivered":true
// (Wakeshift's mark for an event the model may have seen already).

import type { Metric } from "./model.js";

/** One thing the harness put in front of the model, with its text. */
export interface Received {
  /** Identifies the record (a transcript entry's uuid, a row ID), so one copied into two files counts once. */
  key: string;
  /** ms, or null if the record has no time. */
  at: number | null;
  /** What kind of record (user, queued, tool_result:<tool>, hook, …), for the notes. */
  via: string;
  text: string;
  /**
   * Codes the record only echoes back rather than delivers: a failed tool
   * call's error quoting the call's own arguments (MCPServer's validation
   * error for report_seen's ids says `input_value='WB-…'`). Not counted
   * outside a <wakeshift_message>, which is a delivery wherever it is.
   */
  echoes?: string[];
}

/** One code reaching the model once. */
export interface Arrival {
  code: string;
  at: number | null;
  via: string;
  /** The delivery said "redelivered":true: Wakeshift sent it again, saying the model may have seen it. */
  redelivered: boolean;
}

/** A bench code (bench/app/server.ts CODE). */
const CODE = /\bWB-[a-z0-9]{4}-\d+\b/g;

/** Wakeshift's mark on a delivery it sent again (a Monitor line, a wait result's event; quotes possibly escaped). */
const REDELIVERED = /\\*"redelivered\\*"\s*:\s*true|\bredelivered="true"/;

/**
 * The deliveries in one text: each <wakeshift_message> element (hooks,
 * connectors, plugins, inbox results); without them, the whole text (a
 * Monitor notification's <event> line, a wait result's JSON lines), in
 * which each code counts once, so an event whose code shows on two lines
 * (its subject and its data) isn't two deliveries. tagged says which.
 */
export function segments(text: string): { text: string; tagged: boolean }[] {
  const tagged = text.match(/<wakeshift_message\b[\s\S]*?(<\/wakeshift_message>|$)/g);
  if (tagged) return tagged.map((t) => ({ text: t, tagged: true }));
  return [{ text, tagged: false }];
}

/**
 * Every bench code in codes that each received item carries, once per
 * delivery. Items with the same key count once. pattern: what a code looks
 * like (default the toy app's WB-…; the realistic tier's CI app counts the
 * run IDs in its events).
 */
export function arrivalsIn(items: Received[], codes: string[], pattern: RegExp = CODE): Arrival[] {
  const want = new Set(codes);
  const done = new Set<string>();
  const out: Arrival[] = [];
  for (const it of items) {
    if (done.has(it.key)) continue;
    done.add(it.key);
    if (pattern === CODE && !it.text.includes("WB-")) continue;
    for (const s of segments(it.text)) {
      // An echo is outside Wakeshift's tags: a <wakeshift_message> is a delivery wherever it is.
      const found = new Set((s.text.match(pattern) ?? []).filter((c) => want.has(c) && (s.tagged || !it.echoes?.includes(c))));
      // A tagged message is marked as a whole; untagged lines (several events in one wait result) each carry their own mark.
      const lines = s.tagged ? [] : s.text.split("\n");
      for (const code of found) {
        const named = new RegExp(`\\b${code}\\b`);
        const redelivered = s.tagged ? REDELIVERED.test(s.text) : lines.some((l) => named.test(l) && REDELIVERED.test(l));
        out.push({ code, at: it.at, via: it.via, redelivered });
      }
    }
  }
  return out.sort((a, b) => (a.at ?? 0) - (b.at ?? 0));
}

/** Per code: how many times it reached the model, and how many of those were marked redelivered. */
export interface ArrivalCount {
  /** code → arrivals, and how many said "redelivered":true. */
  counts: Record<string, { n: number; redelivered: number; via: string[] }>;
  /** Codes that arrived again, after their first arrival, without the mark: Wakeshift (or the harness) delivered them again as new. */
  duplicates: string[];
  /** Codes that arrived again marked redelivered (allowed: the model was told). */
  redelivered: string[];
  /** Codes that never arrived. */
  missing: string[];
}

/**
 * Counts arrivals per code. A code's first arrival is its delivery, marked
 * or not (an earlier attempt may not have reached the model); a later one
 * marked "redelivered":true is allowed (Wakeshift said the model may have
 * seen it); a later one without the mark is a duplicate delivery (two paths
 * delivering the same event).
 */
export function countArrivals(arrivals: Arrival[], codes: string[]): ArrivalCount {
  const counts: ArrivalCount["counts"] = {};
  for (const c of codes) counts[c] = { n: 0, redelivered: 0, via: [] };
  const unmarked = new Map<string, (number | null)[]>();
  const markedAt = new Map<string, (number | null)[]>();
  for (const a of arrivals) {
    const c = counts[a.code];
    if (!c) continue;
    c.n++;
    if (a.redelivered) c.redelivered++;
    c.via.push(a.via);
    const m = a.redelivered ? markedAt : unmarked;
    m.set(a.code, [...(m.get(a.code) ?? []), a.at]);
  }
  // Not hanging on arrival order where times tie or are missing: two unmarked
  // arrivals are a duplicate in any order; one unmarked arrival is the
  // delivery unless a marked one came strictly before it.
  const duplicates = codes.filter((c) => {
    const u = unmarked.get(c) ?? [];
    if (u.length > 1) return true;
    const t = u[0];
    return u.length === 1 && t != null && (markedAt.get(c) ?? []).some((m) => m != null && m < t);
  });
  const redelivered = codes.filter((c) => counts[c].redelivered > 0 && counts[c].n > 1);
  const missing = codes.filter((c) => counts[c].n === 0);
  return { counts, duplicates, redelivered, missing };
}

/** "WB-ab12-1 ×1, WB-ab12-2 ×2 (1 redelivered)": a count for the cell's notes. */
export function describeCounts(c: ArrivalCount): string {
  return Object.entries(c.counts)
    .map(([code, x]) => `${code} ×${x.n}${x.redelivered ? ` (${x.redelivered} redelivered)` : ""}`)
    .join(", ");
}

/** The outcome of counting a scenario's codes, as metrics, notes and (for a duplicate) why it fails. */
export interface ArrivalOutcome {
  metrics: Record<string, Metric>;
  notes: string[];
  /** Set when a code reached the model twice without the redelivered mark: the cell fails. */
  failure?: string;
  /**
   * Codes counted as reaching the model exactly once (none where the record
   * couldn't be read). One the agent named in two report_seen calls was
   * reported again (e.g. after a call it thought went wrong: a mistyped
   * session_key), not delivered again, so S3 and S15 don't count it as a
   * duplicate.
   */
  once: Set<string>;
}

/**
 * A scenario's arrivals as metrics: null items means the harness's record
 * couldn't be read (graded on report_seen and the pane alone, as before,
 * and the cell says so). seen: codes the bench saw some other way (a GOT
 * line, report_seen), to tell a code the count missed from one that was lost.
 */
export function arrivalOutcome(items: Received[] | null, codes: string[], seen: Set<string>, harness: string, why = "its transcript isn't readable here", pattern: RegExp = CODE): ArrivalOutcome {
  if (!items) {
    return {
      metrics: { arrivals_counted: false },
      notes: [`arrivals not counted: ${why}, so duplicates and losses are judged from report_seen and the pane only`],
      once: new Set(),
    };
  }
  const c = countArrivals(arrivalsIn(items, codes, pattern), codes);
  const once = new Set(codes.filter((x) => c.counts[x].n === 1));
  // A code the agent reported that the count never found arrived by a path the
  // count can't see: the count is incomplete for it, not evidence of a loss.
  const uncounted = c.missing.filter((x) => seen.has(x));
  const lost = c.missing.filter((x) => !seen.has(x));
  const metrics: Record<string, Metric> = {
    arrivals_counted: true,
    arrivals: JSON.stringify(Object.fromEntries(Object.entries(c.counts).map(([k, v]) => [k, v.redelivered ? { n: v.n, redelivered: v.redelivered } : v.n]))),
    duplicate_deliveries: c.duplicates.length,
    redeliveries: c.redelivered.length,
    never_arrived: lost.length,
  };
  const notes = [`reached the model (${harness}'s own record): ${describeCounts(c) || "no codes"}`];
  if (c.redelivered.length) notes.push(`redelivered, marked "redelivered":true (allowed): ${c.redelivered.join(", ")}`);
  if (uncounted.length) notes.push(`reported, but not found in ${harness}'s record (a path the count doesn't read): ${uncounted.join(", ")}`);
  if (lost.length) notes.push(`never reached the model: ${lost.join(", ")}`);
  if (!c.duplicates.length) return { metrics, notes, once };
  const what = c.duplicates.map((x) => `${x} ×${c.counts[x].n}${c.counts[x].redelivered ? ` (${c.counts[x].redelivered} marked)` : ""}`).join(", ");
  return { metrics, notes, once, failure: `duplicate delivery: ${what} reached the model again without "redelivered":true` };
}
