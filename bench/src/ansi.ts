// ANSI (SGR) to HTML, for terminal snapshots (tmux capture-pane -e). Colors
// (16, 256 and 24-bit), bold, dim, italic, underline, inverse and strike;
// every other escape is dropped. The output is spans with inline styles, so
// the report needs nothing at view time.

const BASE16 = [
  "#1e1e1e", "#cd3131", "#0dbc79", "#e5e510", "#2472c8", "#bc3fbc", "#11a8cd", "#e5e5e5",
  "#666666", "#f14c4c", "#23d18b", "#f5f543", "#3b8eea", "#d670d6", "#29b8db", "#ffffff",
];

/** The xterm 256-color palette entry n, as #rrggbb. */
export function color256(n: number): string {
  if (n < 16) return BASE16[n];
  if (n < 232) {
    const i = n - 16;
    const steps = [0, 95, 135, 175, 215, 255];
    const r = steps[Math.floor(i / 36)];
    const g = steps[Math.floor(i / 6) % 6];
    const b = steps[i % 6];
    return hex(r, g, b);
  }
  const v = 8 + (n - 232) * 10;
  return hex(v, v, v);
}

function hex(r: number, g: number, b: number): string {
  return "#" + [r, g, b].map((x) => Math.max(0, Math.min(255, x)).toString(16).padStart(2, "0")).join("");
}

export function escapeHtml(s: string): string {
  return s.replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;").replace(/"/g, "&quot;");
}

interface Style {
  fg?: string;
  bg?: string;
  bold?: boolean;
  dim?: boolean;
  italic?: boolean;
  underline?: boolean;
  inverse?: boolean;
  strike?: boolean;
}

function css(s: Style): string {
  let fg = s.fg;
  let bg = s.bg;
  if (s.inverse) {
    [fg, bg] = [bg ?? "var(--term-bg,#111)", fg ?? "var(--term-fg,#ddd)"];
  }
  const out: string[] = [];
  if (fg) out.push(`color:${fg}`);
  if (bg) out.push(`background:${bg}`);
  if (s.bold) out.push("font-weight:bold");
  if (s.dim) out.push("opacity:.6");
  if (s.italic) out.push("font-style:italic");
  const deco = [s.underline && "underline", s.strike && "line-through"].filter(Boolean);
  if (deco.length) out.push(`text-decoration:${deco.join(" ")}`);
  return out.join(";");
}

function applySgr(params: number[], s: Style): Style {
  const st: Style = { ...s };
  if (params.length === 0) params = [0];
  for (let i = 0; i < params.length; i++) {
    const p = params[i];
    if (p === 0) {
      for (const k of Object.keys(st)) delete (st as Record<string, unknown>)[k];
    } else if (p === 1) st.bold = true;
    else if (p === 2) st.dim = true;
    else if (p === 3) st.italic = true;
    else if (p === 4) st.underline = true;
    else if (p === 7) st.inverse = true;
    else if (p === 9) st.strike = true;
    else if (p === 22) {
      st.bold = false;
      st.dim = false;
    } else if (p === 23) st.italic = false;
    else if (p === 24) st.underline = false;
    else if (p === 27) st.inverse = false;
    else if (p === 29) st.strike = false;
    else if (p >= 30 && p <= 37) st.fg = BASE16[p - 30];
    else if (p >= 90 && p <= 97) st.fg = BASE16[p - 90 + 8];
    else if (p >= 40 && p <= 47) st.bg = BASE16[p - 40];
    else if (p >= 100 && p <= 107) st.bg = BASE16[p - 100 + 8];
    else if (p === 39) delete st.fg;
    else if (p === 49) delete st.bg;
    else if (p === 38 || p === 48) {
      const key = p === 38 ? "fg" : "bg";
      if (params[i + 1] === 5 && params[i + 2] !== undefined) {
        st[key] = color256(params[i + 2]);
        i += 2;
      } else if (params[i + 1] === 2 && params[i + 4] !== undefined) {
        st[key] = hex(params[i + 2], params[i + 3], params[i + 4]);
        i += 4;
      }
    }
  }
  for (const k of Object.keys(st) as (keyof Style)[]) if (st[k] === false) delete st[k];
  return st;
}

// CSI sequences (SGR among them), OSC, and other two-byte escapes.
const ESC = /\x1b(?:\[([0-?]*)([ -/]*)([@-~])|\][^\x07\x1b]*(?:\x07|\x1b\\)|[@-Z\\-_])/g;

/** Converts text with ANSI escapes to HTML (no wrapping element). */
export function ansiToHtml(input: string): string {
  let style: Style = {};
  let out = "";
  // The style of the span that's open ("" for none); spans open lazily, at text.
  let open = "";
  let last = 0;
  const text = (t: string) => {
    if (!t) return;
    // Drop other control characters except newline and tab.
    t = t.replace(/[\x00-\x08\x0b-\x1f\x7f]/g, "");
    if (!t) return;
    const c = css(style);
    if (c !== open) {
      if (open) out += "</span>";
      if (c) out += `<span style="${c}">`;
      open = c;
    }
    out += escapeHtml(t);
  };
  const restyle = (next: Style) => {
    style = next;
  };
  ESC.lastIndex = 0;
  let m: RegExpExecArray | null;
  while ((m = ESC.exec(input))) {
    text(input.slice(last, m.index));
    last = m.index + m[0].length;
    if (m[3] === "m" && m[2] === "") {
      // ; and : both separate SGR parameters (38:5:n).
      const params = m[1] === "" ? [] : m[1].split(/[;:]/).map((x) => (x === "" ? 0 : Number(x)));
      if (params.every((n) => Number.isFinite(n))) restyle(applySgr(params, style));
    }
  }
  text(input.slice(last));
  if (open) out += "</span>";
  return out;
}

/** Strips ANSI escapes (for matching and quotes). */
export function stripAnsi(s: string): string {
  return s.replace(ESC, "");
}

/**
 * A pane log (pipe-pane's raw output) as text. TUIs move the cursor instead
 * of printing spaces and newlines: a move right (CSI n C) is n spaces, a
 * move to a column (CSI n G, how Claude Code 2.1.319 places each word on a
 * line) is a space, a move down or to a row is a newline. Without the
 * column moves, Claude Code's words ran together ("Youdidn'taskfor"), and
 * a dismissal read as a mention of the offer.
 */
export function logText(raw: string): string {
  return stripAnsi(
    raw
      .replace(/\x1b\[(\d*)C/g, (_, n) => " ".repeat(Math.min(200, Number(n || 1))))
      .replace(/\x1b\[\d*G/g, " ")
      .replace(/\x1b\[\d*(;\d*)?[HfE]|\x1b\[\d*B/g, "\n")
      .replace(/\r\n?/g, "\n"),
  );
}
