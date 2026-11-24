// Redaction for everything the bench writes: terminal snapshots, pipe-pane
// logs, tool results the toy app records, results.json. Secrets that show
// up in a harness's terminal: stream and wait links (?link=…), session keys
// (wsc_…), subscribe vouchers (wsv_…), beacons (a JWS, or
// "wakeshift-beacon: …"), app keys (wst_…),
// bearer tokens and publish-URL capabilities. Text may carry ANSI escapes
// (capture-pane -e) between the characters of a secret, so matching runs
// on the plain text and replacements keep every escape in place. Secrets the
// bench holds by value (SECRET_ENV: an API key a harness is given) are
// redacted by value too, whatever their shape.

export const REDACTED = "‹redacted›";

interface Rule {
  re: RegExp;
  /** The replacement for the whole match (groups as in String.replace). */
  to: string;
}

const RULES: Rule[] = [
  // Query parameters that carry a credential (stream/wait links, tickets).
  { re: /([?&](?:link|token|ticket|key|session_key|access_token|code)=)[^&\s'"\\<>)\]\x07\x1b]+/g, to: `$1${REDACTED}` },
  // A beacon in text, and any JWS (beacons in _meta, assertions).
  { re: /(wakeshift-beacon:\s*)[^\s)'"\x07\x1b]+/g, to: `$1${REDACTED}` },
  { re: /eyJ[A-Za-z0-9_-]{6,}\.[A-Za-z0-9_-]{6,}\.[A-Za-z0-9_-]{6,}/g, to: REDACTED },
  // Session keys, app keys.
  { re: /\bwsc_[A-Za-z0-9_-]+/g, to: `wsc_${REDACTED}` },
  // Subscribe vouchers (wsv_ and 32 base32 characters): a native client's
  // bearer for one subscription.
  { re: /\bwsv_[A-Za-z0-9_-]+/g, to: `wsv_${REDACTED}` },
  // App keys: wst_<50 base58> and the older wst_test_… / wst_live_….
  { re: /\bwst_[A-Za-z0-9_-]+/g, to: `wst_${REDACTED}` },
  // A command's one-time link claim (wso_…): redacted, though it's
  // spent once the command has run and CREDENTIALS doesn't count it.
  { re: /\bwso_[A-Za-z0-9_-]+/g, to: `wso_${REDACTED}` },
  // Anthropic tokens (claude setup-token's sk-ant-oat…, API keys).
  { re: /\bsk-ant-[A-Za-z0-9_-]+/g, to: `sk-ant-${REDACTED}` },
  // "session_key": "…" in JSON a harness prints (a key without the prefix).
  { re: /("?session_key"?\s*[:=]\s*"?)[^"\s,}\x07\x1b]{6,}/g, to: `$1${REDACTED}` },
  { re: /(\bBearer\s+)[A-Za-z0-9._~+/=-]{8,}/g, to: `$1${REDACTED}` },
  // Capability URLs: publish URLs (/v1/publish/<token>).
  { re: /(\/v1\/publish\/)[A-Za-z0-9_-]{8,}/g, to: `$1${REDACTED}` },
  // What's left of a secret a terminal wrapped onto the next line: a long
  // run that looks random (letters, digits and both cases).
  { re: /(?<![A-Za-z0-9_\-/.])(?=[A-Za-z0-9_-]{20,})(?=[A-Za-z0-9_-]*[0-9])(?=[A-Za-z0-9_-]*[a-z])(?=[A-Za-z0-9_-]*[A-Z])[A-Za-z0-9_-]{20,}(?:\.[A-Za-z0-9_-]{6,})*/g, to: REDACTED },
  // A token's tail wrapped onto the next line: the end of its payload, a dot
  // and its signature (a long random-looking run), "MzQxfQ.e9Y9Zi…".
  { re: /(?<![A-Za-z0-9_\-/.])[A-Za-z0-9_-]{1,40}\.(?=[A-Za-z0-9_-]{30,})(?=[A-Za-z0-9_-]*[0-9])(?=[A-Za-z0-9_-]*[a-z])(?=[A-Za-z0-9_-]*[A-Z])[A-Za-z0-9_-]{30,}/g, to: REDACTED },
  // The signature of a wrapped token: ".<43 base64url characters>" at the start of a line.
  { re: /(^|[\s│▎>|])\.[A-Za-z0-9_-]{30,}/gm, to: `$1${REDACTED}` },
];

/**
 * Wakeshift's credentials by kind, in the shapes RULES redacts (and
 * CROSS, for one a terminal wrapped): what must never show in a prompt the
 * user is asked to approve, or in a rule a harness saves. Not the generic
 * random-run rules, which also catch harmless IDs; nothing already
 * redacted (‹redacted›) matches.
 */
// A command's one-time link claim (wso_…) isn't one: the command that
// carries it spends it seconds after its approval, and it hands nothing
// over while that command listens.
const CREDENTIALS: { kind: string; re: RegExp }[] = [
  // Not a shell expansion: a command that reads its link from its file (`link=${t#*link=}`) holds none.
  { kind: "stream or wait link", re: /[?&](?:link|ticket|session_key|access_token)=(?!‹|\$)[^&\s'"\\<>)\]\x07\x1b]{8,}/ },
  { kind: "session key (wsc_)", re: /\bwsc_(?!‹)[A-Za-z0-9_-]{8,}/ },
  { kind: "subscribe voucher (wsv_)", re: /\bwsv_(?!‹)[A-Za-z0-9_-]{8,}/ },
  { kind: "app key (wst_)", re: /\bwst_(?!‹)[A-Za-z0-9_-]{8,}/ },
  { kind: "beacon", re: /wakeshift-beacon:\s*(?!‹)[^\s)'"\x07\x1b]{8,}/ },
  { kind: "JWS", re: /eyJ[A-Za-z0-9_-]{6,}\.[A-Za-z0-9_-]{6,}\.[A-Za-z0-9_-]{6,}/ },
  { kind: "publish URL", re: /\/v1\/publish\/(?!‹)[A-Za-z0-9_-]{8,}/ },
];

/**
 * The kinds of Wakeshift credential in text, before redaction (empty if
 * none). A credential a terminal wrapped over lines counts too: the lines
 * are also read joined, their TUI borders taken off.
 */
export function credentialKinds(text: string): string[] {
  const plain = text.replace(/\x1b(?:\[[0-?]*[ -/]*[@-~]|\][^\x07\x1b]*(?:\x07|\x1b\\)|[@-Z\\-_])/g, "");
  const joined = plain
    .split("\n")
    .map((l) => l.replace(/^[\s│┃▎>|]+|[\s│┃▎|]+$/g, ""))
    .join("");
  return CREDENTIALS.filter((c) => c.re.test(plain) || c.re.test(joined)).map((c) => c.kind);
}

/**
 * Environment variables whose values the bench hands a harness (Cursor CLI's
 * API key, from the CURSOR_API_KEY repository secret): their values are
 * redacted wherever they show up, a value wrapped over lines included.
 */
export const SECRET_ENV = ["CURSOR_API_KEY"];

/** The values of SECRET_ENV that are set (8 characters or more: anything shorter isn't a key). */
export function secretValues(env: NodeJS.ProcessEnv = process.env): string[] {
  return SECRET_ENV.map((k) => env[k]?.trim() ?? "").filter((v) => v.length >= 8);
}

const escapeRe = (s: string) => s.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");

/** A rule per secret value: the whole value, literally. */
function valueRules(): Rule[] {
  return secretValues().map((v) => ({ re: new RegExp(escapeRe(v), "g"), to: REDACTED }));
}

/** Whether text still holds a secret value (scrub's check for what got past redaction). */
export function holdsSecretValue(s: string): boolean {
  return secretValues().some((v) => s.includes(v));
}

// ESC [ … final, ESC ] … BEL/ST, and other two-byte escapes.
const ESCAPE = /\x1b(?:\[[0-?]*[ -/]*[@-~]|\][^\x07\x1b]*(?:\x07|\x1b\\)|[@-Z\\-_])/y;

interface Piece {
  esc?: string;
  ch?: string;
}

function pieces(s: string): Piece[] {
  const out: Piece[] = [];
  let i = 0;
  while (i < s.length) {
    if (s.charCodeAt(i) === 0x1b) {
      ESCAPE.lastIndex = i;
      const m = ESCAPE.exec(s);
      if (m) {
        // OSC sequences carry text of their own (OSC 8 hyperlinks: the URL,
        // a stream link included): redact inside them too.
        out.push({ esc: m[0].startsWith("\x1b]") ? redactPlain(m[0]) : m[0] });
        i += m[0].length;
        continue;
      }
    }
    const cp = s.codePointAt(i)!;
    const ch = String.fromCodePoint(cp);
    out.push({ ch });
    i += ch.length;
  }
  return out;
}

/**
 * Secrets that can start on one line and go on over the next ones, where a
 * terminal wrapped them (or the bench stripped a TUI's borders and joined
 * the lines back, as a quote is): a JWS whose parts land on two lines, a
 * link whose token does. Each line on its own can be too short or too
 * plain for the rules above.
 */
const CROSS: RegExp[] = [
  /eyJ[A-Za-z0-9_-]{6,}\.[A-Za-z0-9_-]{6,}\.[A-Za-z0-9_-]{6,}/g,
  /[?&](?:link|token|ticket|key|session_key|access_token|code)=[^&\s'"\\<>)\]\x07\x1b]{8,}/g,
  /\b(?:wsc|wst)_[A-Za-z0-9_-]{8,}/g,
  /\bsk-ant-[A-Za-z0-9_-]{8,}/g,
];

/** What a cross-line match keeps of its first line: the parameter or prefix that names it. */
const KEEP = /^(?:[?&][a-z_]+=|wsc_|wst_|sk-ant-)/;

/**
 * Edits (plain-text offsets) that redact a CROSS secret spanning lines:
 * each line's part of it, with the TUI's borders (│ ┃ ▎ > |) and the
 * spaces around them left out of the join. A part on one line alone is
 * the line rules' to catch.
 */
export function crossLineEdits(text: string, patterns: RegExp[] = CROSS): { start: number; end: number; to: string }[] {
  if (!text.includes("\n") || !patterns.length) return [];
  if (patterns === CROSS && !/eyJ|[?&][a-z_]+=|wsc_|wst_|sk-ant-/.test(text)) return [];
  // Each line's content: its offsets in text, without borders and edge spaces.
  const parts: { start: number; end: number }[] = [];
  let at = 0;
  for (const line of text.split("\n")) {
    const lead = /^[\s│┃▎|>]*/.exec(line)![0].length;
    const trail = /[\s│┃▎|]*$/.exec(line.slice(lead))![0].length;
    parts.push({ start: at + lead, end: at + line.length - trail });
    at += line.length + 1;
  }
  let joined = "";
  const map: number[] = []; // joined index → text offset
  const lineOf: number[] = []; // joined index → line
  parts.forEach((p, i) => {
    for (let k = p.start; k < p.end; k++) {
      map.push(k);
      lineOf.push(i);
    }
    joined += text.slice(p.start, p.end);
  });
  const edits: { start: number; end: number; to: string }[] = [];
  for (const rule of patterns) {
    const re = new RegExp(rule.source, rule.flags);
    for (let m = re.exec(joined); m; m = re.exec(joined)) {
      let a = m.index;
      const b = a + m[0].length;
      if (lineOf[a] === lineOf[b - 1]) continue;
      let keep = KEEP.exec(m[0])?.[0].length ?? 0;
      if (m[0].includes(REDACTED)) {
        // A line rule redacted the first line's part already: what follows
        // on the next line is the token's rest if the redaction ended that
        // line and the rest looks random (a digit, or both cases), not the
        // next line's first word.
        const r = a + m[0].indexOf(REDACTED) + REDACTED.length;
        const rest = joined.slice(r, b);
        if (r >= b || lineOf[r] === lineOf[r - 1] || !/^[A-Za-z0-9_-]{4,}/.test(rest) || !(/\d/.test(rest) || (/[a-z]/.test(rest) && /[A-Z]/.test(rest)))) continue;
        a = r;
        keep = 0;
      }
      for (let i = a; i < b; ) {
        let j = i;
        while (j < b && lineOf[j] === lineOf[i]) j++;
        const k = Math.min(keep, j - i);
        keep -= k;
        if (j - i - k > 0) edits.push({ start: map[i + k], end: map[j - 1] + 1, to: REDACTED });
        i = j;
      }
    }
  }
  return edits.sort((x, y) => x.start - y.start).filter((e, i, all) => i === 0 || e.start >= all[i - 1].end);
}

/** A secret value wrapped over lines: each line's part of it (crossLineEdits with the values as its patterns). */
function valueCrossEdits(text: string): { start: number; end: number; to: string }[] {
  const values = secretValues();
  // Its first characters are on some line, or there's nothing to join.
  const present = values.filter((v) => text.includes(v.slice(0, 6)));
  return present.length ? crossLineEdits(text, present.map((v) => new RegExp(escapeRe(v), "g"))) : [];
}

/** Redacts secrets in plain text, a secret wrapped over lines included. */
export function redactPlain(s: string): string {
  // Secret values first, whole, before a shape rule takes part of one.
  for (const r of valueRules()) s = s.replace(r.re, r.to);
  for (const e of valueCrossEdits(s).reverse()) s = s.slice(0, e.start) + e.to + s.slice(e.end);
  for (const r of RULES) s = s.replace(r.re, r.to);
  for (const e of crossLineEdits(s).reverse()) s = s.slice(0, e.start) + e.to + s.slice(e.end);
  return s;
}

/**
 * Redacts secrets in text that may carry ANSI escapes, keeping the escapes.
 * Each rule matches within a line; then a secret wrapped over lines is
 * redacted where its parts join up (crossLineEdits).
 */
export function redact(s: string): string {
  if (!s.includes("\x1b")) return redactPlain(s);
  const ps = pieces(s);
  const chars: { ch: string; at: number }[] = [];
  ps.forEach((p, at) => {
    if (p.ch !== undefined) chars.push({ ch: p.ch, at });
  });
  // Plain-string offset → index in chars (characters can be two UTF-16 units).
  const offsetToChar: number[] = [];
  chars.forEach((c, i) => {
    for (let k = 0; k < c.ch.length; k++) offsetToChar.push(i);
  });
  offsetToChar.push(chars.length);
  // Each rule's edits, then the cross-line ones, on the plain text as it is by then.
  const passes: ((text: string) => { start: number; end: number; to: string }[])[] = [
    ...valueRules().map((r) => ruleEdits(r)),
    valueCrossEdits,
    ...RULES.map((r) => ruleEdits(r)),
    (text: string) => crossLineEdits(text),
  ];
  function ruleEdits(r: Rule) {
    return (text: string) => {
      const re = new RegExp(r.re.source, r.re.flags);
      const edits: { start: number; end: number; to: string }[] = [];
      let m: RegExpExecArray | null;
      while ((m = re.exec(text))) {
        if (m[0].length === 0) {
          re.lastIndex++;
          continue;
        }
        const to = m[0].replace(new RegExp(r.re.source, r.re.flags.replace("g", "")), r.to);
        edits.push({ start: m.index, end: m.index + m[0].length, to });
      }
      return edits;
    };
  }
  for (const pass of passes) {
    const edits = pass(chars.map((c) => c.ch).join(""));
    // Apply from the end so earlier offsets stay valid.
    for (const e of edits.reverse()) {
      const a = offsetToChar[e.start];
      const b = offsetToChar[e.end];
      // Keep the part of the match the replacement keeps as a prefix (a
      // group like "?link="), character by character, so escapes stay put.
      const matched = chars.slice(a, b).map((c) => c.ch).join("");
      let keep = 0;
      while (keep < matched.length && keep < e.to.length && matched[keep] === e.to[keep]) keep++;
      let consumed = 0;
      let placed = false;
      for (let i = a; i < b; i++) {
        const len = chars[i].ch.length;
        if (consumed + len <= keep) {
          consumed += len;
          continue;
        }
        ps[chars[i].at].ch = placed ? "" : e.to.slice(keep);
        placed = true;
        consumed += len;
      }
      for (let i = a; i < b; i++) chars[i].ch = ps[chars[i].at].ch!;
    }
    // Rebuild the character list for the next rule.
    const rebuilt: { ch: string; at: number }[] = [];
    ps.forEach((p, at) => {
      if (p.ch) rebuilt.push({ ch: p.ch, at });
    });
    // (Not push(...rebuilt): a long log line overflows the call stack.)
    chars.length = 0;
    for (const c of rebuilt) chars.push(c);
    offsetToChar.length = 0;
    chars.forEach((c, i) => {
      for (let k = 0; k < c.ch.length; k++) offsetToChar.push(i);
    });
    offsetToChar.push(chars.length);
  }
  return ps.map((p) => p.esc ?? p.ch ?? "").join("");
}

/** Redacts every string in a JSON-able value. */
export function redactDeep<T>(v: T): T {
  if (typeof v === "string") return redactPlain(v) as T;
  if (Array.isArray(v)) return v.map(redactDeep) as T;
  if (v && typeof v === "object") {
    const out: Record<string, unknown> = {};
    for (const [k, x] of Object.entries(v)) out[k] = /^(session_key|token|link|app_key)$/.test(k) && typeof x === "string" ? REDACTED : redactDeep(x);
    return out as T;
  }
  return v;
}
