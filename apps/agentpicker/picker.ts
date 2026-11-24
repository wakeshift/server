// The agent picker for the TypeScript apps: the same markup as
// apps/agentpicker/agentpicker.go renders. AGENTS, PICKER_JS, PICKER_CSS and
// PICKER_LOGOS (data.ts) are generated from internal/harnesses and
// apps/agentpicker/static, and checked in: don't edit data.ts by hand.
import { createHash } from "node:crypto";
import { AGENTS, PICKER_CSS, PICKER_JS, PICKER_LOGOS } from "./data.js";

export interface PickerStep {
  kind: "run" | "file" | "url" | "note" | "cursor";
  label?: string;
  text?: string;
}

export interface PickerAgent {
  id: string;
  name: string;
  group: "main" | "more";
  steps: PickerStep[];
  signIn?: string;
  wakes: string[];
}

export interface PickerOptions {
  /** The server's name in the agent's config ("webhooks"). */
  name: string;
  /** The app's MCP URL. */
  url: string;
  /** What to say to the agent. */
  ask: string;
  /** What wakes the agent, mid-sentence ("each post"). */
  each?: string;
  /** A sentence about signing in, shown for every agent. */
  signIn?: string;
  /** Signing in needs the user, so each agent's way to start it shows too. */
  interactive?: boolean;
  /** The app has no sign-in ("No authentication" for a connector). */
  noAuth?: boolean;
}

export const SCRIPT_PATH = "/agent-picker/agent-picker.js";
export const STYLE_PATH = "/agent-picker/agent-picker.css";
export const LOGOS_PATH = "/agent-picker/agent-logos.svg";

/** The script, the stylesheet and the pills' logos, for a server to send at SCRIPT_PATH, STYLE_PATH and LOGOS_PATH. */
export const pickerAssets: Record<string, { type: string; body: string }> = {
  [SCRIPT_PATH]: { type: "text/javascript; charset=utf-8", body: PICKER_JS },
  [STYLE_PATH]: { type: "text/css; charset=utf-8", body: PICKER_CSS },
  [LOGOS_PATH]: { type: "image/svg+xml", body: PICKER_LOGOS },
};

/** The agent IDs with a logo in the sprite at LOGOS_PATH. */
const hasLogo = new Set([...PICKER_LOGOS.matchAll(/<symbol id="([^"]+)"/g)].map((m) => m[1]));

/** The tags a page puts in its <head>. */
/** The tags a page puts in its <head>, each URL with a hash of its file (?v=), so a page always gets the copy it was made with (agentpicker.go's Head). */
export function pickerHead(): string {
  return `<link rel="stylesheet" href="${STYLE_PATH}?v=${version(PICKER_CSS)}"><script src="${SCRIPT_PATH}?v=${version(PICKER_JS)}" defer></script>`;
}

const version = (s: string) => createHash("sha256").update(s).digest("hex").slice(0, 12);

// As Go's html/template escapes (agentpicker.go), so both renderers agree.
const esc = (s: string) =>
  s.replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;").replace(/"/g, "&#34;").replace(/'/g, "&#39;").replace(/\+/g, "&#43;");

export function fill(s: string, o: PickerOptions): string {
  const each = o.each || "each message";
  const vals: Record<string, string> = {
    "{name}": o.name,
    "{url}": o.url,
    "{each}": each,
    "{Each}": each[0].toUpperCase() + each.slice(1),
    "{auth}": o.noAuth ? "No authentication" : "OAuth",
    "{hermesauth}": o.noAuth ? "" : " --auth oauth",
    "{hermesauthyaml}": o.noAuth ? "" : "\n    auth: oauth",
  };
  return s.replace(/\{name\}|\{url\}|\{each\}|\{Each\}|\{auth\}|\{hermesauthyaml\}|\{hermesauth\}/g, (k) => vals[k]);
}

/** Cursor's link that adds the server: its name and its mcp.json entry as base64 JSON. */
export function cursorLink(name: string, mcpUrl: string): string {
  const cfg = Buffer.from(JSON.stringify({ url: mcpUrl })).toString("base64");
  return `cursor://anysphere.cursor-deeplink/mcp/install?name=${encodeURIComponent(name)}&config=${encodeURIComponent(cfg)}`;
}

export function pickerHtml(o: PickerOptions, agents: PickerAgent[] = AGENTS): string {
  const pill = (a: PickerAgent) =>
    `<button type="button" class="ap-pill" data-agent="${esc(a.id)}" aria-pressed="false" aria-controls="ap-${esc(a.id)}">${hasLogo.has(a.id) ? `<svg class="ap-logo" aria-hidden="true" focusable="false"><use href="${LOGOS_PATH}#${esc(a.id)}"></use></svg>` : ""}${esc(a.name)}</button>`;
  const main = agents.filter((a) => a.group === "main");
  const more = agents.filter((a) => a.group !== "main");
  const step = (s: PickerStep) => {
    const text = fill(s.text ?? "", o);
    if (s.kind === "note") return `<p class="ap-note">${esc(text)}</p>\n`;
    if (s.kind === "cursor") return `<p><a class="ap-btn" href="${esc(cursorLink(o.name, o.url))}">${esc(text)}</a></p>\n`;
    const label = s.label ? `<p class="ap-label">${esc(fill(s.label, o))}</p>\n` : "";
    return `${label}<div class="ap-codewrap"><pre class="ap-code"><code>${esc(text)}</code></pre></div>\n`;
  };
  const panel = (a: PickerAgent) => {
    const notes: string[] = [];
    if (o.interactive && a.signIn) notes.push(fill(a.signIn, o));
    if (o.signIn) notes.push(o.signIn);
    return `<section class="ap-panel" id="ap-${esc(a.id)}" data-agent="${esc(a.id)}" aria-label="${esc(a.name)}">
<h3 class="ap-title">${esc(a.name)}</h3>
${a.steps.map(step).join("")}${notes.map((n) => `<p class="ap-note">${esc(n)}</p>\n`).join("")}<p class="ap-say">Then ask your agent: <q>${esc(o.ask)}</q></p>
<ul class="ap-facts">${a.wakes.map((w) => `<li>${esc(fill(w, o))}</li>`).join("")}</ul>
</section>
`;
  };
  return `<div class="ap" data-ap data-default="claude-code">
<div class="ap-pills" role="group" aria-label="Your agent">${main.map(pill).join("")}<button type="button" class="ap-pill ap-more" aria-expanded="false" aria-controls="ap-more">More agents</button></div>
<div class="ap-pills ap-more-list" id="ap-more" role="group" aria-label="More agents">${more.map(pill).join("")}</div>
${agents.map(panel).join("")}</div>`;
}
