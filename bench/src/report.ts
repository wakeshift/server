// report.html: one static page per run, nothing fetched at view time. The
// grid (rows by tier, columns S1–S9, cells colored by grade with their key
// number); a cell opens that scenario's timeline: steps with times, data
// (latencies, deliveries, quotes) and the terminal snapshot at each step.

import { existsSync, readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { escapeHtml } from "./ansi.js";
import { ext } from "./extensions.js";
import type { Tiers } from "./grade.js";
import { SCENARIOS, SCENARIO_TITLES, STAGES, type RunResult } from "./model.js";
import { redact, redactDeep } from "./redact.js";

/** JSON safe to put inside <script>. */
export function scriptJson(v: unknown): string {
  return JSON.stringify(v).replace(/</g, "\\u003c").replace(/>/g, "\\u003e").replace(/&/g, "\\u0026").replace(/\u2028/g, "\\u2028").replace(/\u2029/g, "\\u2029");
}

export function renderReport(run: RunResult, tiers: Tiers, dir: string): string {
  const snaps: Record<string, string> = {};
  for (const h of run.harnesses)
    for (const s of h.scenarios)
      for (const st of s.steps)
        if (st.snapshot && !snaps[st.snapshot]) {
          const p = join(dir, st.snapshot + ".html");
          // Snapshots were redacted when written; redact again in case one wasn't.
          if (existsSync(p)) snaps[st.snapshot] = redact(readFileSync(p, "utf8"));
        }
  const order = tiers.tiers.map((t) => ({ id: t.id, label: t.label }));
  // The run's own scenarios as columns (S11-S14 only when asked for), in the usual order.
  const scenarios = run.scenarios?.length ? SCENARIOS.filter((s) => run.scenarios.includes(s)) : SCENARIOS;
  // Sections extensions add (src/extensions.ts), each with its own data, redacted like the rest.
  // Their script and CSS are the extension's own code, never data: refuse
  // anything that could end the element it goes in, or an id that isn't plain.
  for (const [i, s] of ext.report.entries()) {
    if (!/^[a-z][a-z0-9-]*$/.test(s.id) || /<\/(script|style)/i.test(s.js + (s.css ?? ""))) throw new Error(`report section ${JSON.stringify(s.id)}: its id must be plain and its script and CSS can't end their element`);
    // Registered with addReportSections, a duplicate is refused there; this catches one pushed directly.
    if (ext.report.findIndex((x) => x.id === s.id) !== i) throw new Error(`report section ${JSON.stringify(s.id)} is registered twice`);
  }
  const sections = Object.fromEntries(ext.report.map((s) => [s.id, redactDeep(s.data(dir, run) ?? null)]));
  const data = { run: redactDeep(run), order, names: tiers.names, scenarios, titles: SCENARIO_TITLES, snaps, sections };
  return `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><title>wakebench ${escapeHtml(run.run)}</title>
<meta name="viewport" content="width=device-width, initial-scale=1">
<style>${CSS}${ext.report.map((s) => s.css ?? "").join("")}</style></head>
<body>
<header><h1>wakebench <span class="run">${escapeHtml(run.run)}</span>${run.tier === "realistic" ? ` <span class="run">realistic tier</span>` : ""}</h1>
<p class="meta">${run.tier === "realistic" ? `the CI app (shipyard), a user who asks for real things (seed <b>${escapeHtml(run.seed ?? "")}</b>) · ` : ""}target <b>${escapeHtml(run.target)}</b> · core ${escapeHtml(run.core)}${run.app ? ` · toy app <b>${escapeHtml(run.app)}</b>${run.app2 && run.app2 !== run.app ? ` (second: <b>${escapeHtml(run.app2)}</b>)` : ""}` : ""}${run.claudePath ? ` · Claude Code: ${escapeHtml(run.claudePath)}` : ""} · ${escapeHtml(run.startedAt)}${run.finishedAt ? ` – ${escapeHtml(run.finishedAt)}` : ""}
 · safety: <b class="${run.safety.ok ? "ok" : "bad"}">${run.safety.ok ? "real configs unchanged" : "CHANGED: " + escapeHtml(run.safety.changed.join(", "))}</b></p>
${run.notes.length ? `<ul class="meta">${run.notes.map((n) => `<li>${escapeHtml(redactDeep(n))}</li>`).join("")}</ul>` : ""}
<p class="legend"><span class="g pass">pass</span><span class="g below-bar">below bar</span><span class="g fail">fail</span><span class="g blocked">blocked</span><span class="g na">n/a</span> Click a cell for its timeline and snapshots.</p>
</header>
<main><table id="grid"></table>${ext.report.map((s) => `<section id="${escapeHtml(s.id)}"></section>`).join("")}<section id="detail"><p class="hint">Click a cell.</p></section></main>
<script id="data" type="application/json">${scriptJson(data)}</script>
<script>${JS}${ext.report.map((s) => `{\n${s.js}\n}\n`).join("")}${START}</script>
</body></html>
`;
}

export function writeReport(run: RunResult, tiers: Tiers, dir: string): string {
  const path = join(dir, "report.html");
  writeFileSync(path, renderReport(run, tiers, dir), { mode: 0o600 });
  return path;
}

const CSS = `
:root{--bg:#fafaf7;--ink:#1d1d1b;--mute:#6b6b66;--line:#e2e0d8;--pass:#2f9e5b;--below:#d99a1e;--fail:#cf3d3d;--blocked:#7b6fb0;--na:#c9c7bf}
*{box-sizing:border-box}body{margin:0;background:var(--bg);color:var(--ink);font:14px/1.45 -apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif}
header{padding:18px 24px 6px}h1{margin:0;font-size:20px}.run{font-family:ui-monospace,Menlo,monospace;font-weight:500;color:var(--mute)}
.meta,.legend{color:var(--mute);margin:6px 0}.ok{color:var(--pass)}.bad{color:var(--fail)}
.g{display:inline-block;padding:1px 8px;border-radius:4px;margin-right:6px;color:#fff;font-size:12px}
.g.pass{background:var(--pass)}.g.below-bar{background:var(--below)}.g.fail{background:var(--fail)}.g.blocked{background:var(--blocked)}.g.na{background:var(--na);color:#333}
main{padding:0 24px 40px}
table{border-collapse:separate;border-spacing:3px;margin:8px 0 18px}
th{font-weight:600;text-align:left;padding:4px 8px;font-size:12px;color:var(--mute)}
th.sc{text-align:center;max-width:110px}th.sc small{display:block;font-weight:400}
tr.tier td{padding:12px 4px 2px;font-weight:600;color:var(--mute);font-size:12px;text-transform:uppercase;letter-spacing:.04em}
td.name{padding:4px 10px 4px 4px;white-space:nowrap}td.name small{color:var(--mute);display:block}
td.cell{width:112px;height:44px;text-align:center;border-radius:6px;cursor:pointer;color:#fff;font-size:12px;padding:2px 4px}
td.cell b{display:block;font-size:11px;opacity:.85;font-weight:500;text-transform:uppercase}
td.cell.pass{background:var(--pass)}td.cell.below-bar{background:var(--below)}td.cell.fail{background:var(--fail)}td.cell.blocked{background:var(--blocked)}td.cell.na{background:var(--na);color:#333}td.cell.none{background:#eee;color:#999;cursor:default}
td.cell.sel{outline:3px solid var(--ink)}
#detail{border-top:1px solid var(--line);padding-top:14px}.hint{color:var(--mute)}
#detail h2{margin:0 0 4px;font-size:17px}#detail .why{color:var(--fail)}
.metrics{font-family:ui-monospace,Menlo,monospace;font-size:12px;background:#fff;border:1px solid var(--line);padding:8px 10px;border-radius:6px;white-space:pre-wrap}
.notes li{margin:2px 0}
table.bd{font:12px ui-monospace,Menlo,monospace;border-spacing:0;margin:6px 0 4px;width:100%}table.bd th,table.bd td{padding:3px 6px;border-bottom:1px solid var(--line);text-align:right;white-space:nowrap}
table.bd th:first-child,table.bd td:first-child{text-align:left}table.bd i{display:inline-block;width:8px;height:8px;border-radius:2px;margin-right:4px}
table.bd td.bar{width:40%}table.bd td.bar div{display:flex;height:12px;background:#eee;border-radius:3px;overflow:hidden}table.bd td.bar span{display:block;height:100%}
td.cell small{display:block;font-size:10px;opacity:.85}
td.cell i.flag{display:block;font-style:normal;font-size:10px;background:#fff;color:var(--ink);border-radius:3px;margin-top:2px}
.step{display:grid;grid-template-columns:90px minmax(0,1fr);gap:10px;border-top:1px dashed var(--line);padding:10px 0}
.step .t{font-family:ui-monospace,Menlo,monospace;color:var(--mute);font-size:12px}
.step .label{font-weight:600}.step pre.data{font-size:11px;background:#fff;border:1px solid var(--line);padding:6px 8px;border-radius:4px;max-height:220px;overflow:auto;white-space:pre-wrap}
details summary{cursor:pointer;color:var(--mute);font-size:12px;margin:4px 0}
pre.term{background:#111;color:#ddd;font:12px/1.25 ui-monospace,Menlo,monospace;padding:10px;border-radius:6px;max-height:560px;overflow:auto;white-space:pre;margin:4px 0}
`;

const JS = `
const D = JSON.parse(document.getElementById("data").textContent);
const grid = document.getElementById("grid"), detail = document.getElementById("detail");
const esc = (s) => String(s).replace(/[&<>"]/g, (c) => ({"&":"&amp;","<":"&lt;",">":"&gt;",'"':"&quot;"}[c]));
const cls = (g) => g === "n/a" ? "na" : (g || "none");
// What a report section (src/extensions.ts) adds: a line in each cell, a block in a cell's detail.
const HOOKS = { cell: [], detail: [] };
function render() {
  let h = "<tr><th></th>" + D.scenarios.map((s) => '<th class="sc">' + s + "<small>" + esc(D.titles[s]) + "</small></th>").join("") + "</tr>";
  const groups = D.order.map((t) => ({ ...t, rows: D.run.harnesses.filter((x) => x.tier === t.id) }));
  const rest = D.run.harnesses.filter((x) => !D.order.some((t) => t.id === x.tier));
  if (rest.length) groups.push({ id: "untiered", label: "Untiered", rows: rest });
  for (const g of groups) {
    if (!g.rows.length) continue;
    h += '<tr class="tier"><td colspan="' + (D.scenarios.length + 1) + '">' + esc(g.label) + "</td></tr>";
    for (const hr of g.rows) {
      h += '<tr><td class="name">' + esc(hr.name) + "<small>" + esc(hr.mode + (hr.version ? " · " + hr.version : "") + (hr.model ? " · " + hr.model : "") + (hr.status === "blocked" ? " · blocked" : "")) + "</small></td>";
      for (const s of D.scenarios) {
        const r = hr.scenarios.find((x) => x.id === s);
        if (!r) { h += '<td class="cell none">–</td>'; continue; }
        h += '<td class="cell ' + cls(r.grade) + '" data-h="' + esc(hr.id) + '" data-s="' + s + '" title="' + esc((r.why || []).join("; ")) + '"><b>' + esc(r.grade || "") + "</b>" + esc(r.key || "") + cellBreakdown(r) + HOOKS.cell.map((f) => f(hr.id, s)).join("") + "</td>";
      }
      h += "</tr>";
    }
  }
  grid.innerHTML = h;
}
function show(hid, sid, cell) {
  document.querySelectorAll("td.sel").forEach((x) => x.classList.remove("sel"));
  if (cell) cell.classList.add("sel");
  const hr = D.run.harnesses.find((x) => x.id === hid), r = hr.scenarios.find((x) => x.id === sid);
  let h = "<h2>" + esc(hr.name) + " · " + sid + " " + esc(D.titles[sid]) + "</h2>";
  h += "<p>grade <span class=\\"g " + cls(r.grade) + "\\">" + esc(r.grade) + "</span> " + esc(r.key || "") + (r.path ? " · path <b>" + esc(r.path) + "</b>" : "") + (r.durationMs ? " · took " + (r.durationMs / 1000).toFixed(0) + "s" : "") + "</p>";
  if (r.why && r.why.length) h += '<ul class="why">' + r.why.map((w) => "<li>" + esc(w) + "</li>").join("") + "</ul>";
  for (const f of HOOKS.detail) h += f(hid, sid);
  if (r.breakdown && r.breakdown.length) h += breakdownTable(r.breakdown);
  h += '<div class="metrics">' + esc(JSON.stringify(r.metrics, null, 1)) + "</div>";
  if (r.notes && r.notes.length) h += '<ul class="notes">' + r.notes.map((n) => "<li>" + esc(n) + "</li>").join("") + "</ul>";
  for (const st of r.steps || []) {
    h += '<div class="step"><div class="t">+' + (st.t / 1000).toFixed(1) + "s<br>" + esc(st.at.slice(11, 19)) + '</div><div><div class="label">' + esc(st.label) + "</div>";
    if (st.data !== undefined) h += '<pre class="data">' + esc(JSON.stringify(st.data, null, 1)) + "</pre>";
    if (st.snapshot && D.snaps[st.snapshot]) h += "<details" + (isLastSnap(r, st) ? " open" : "") + "><summary>terminal snapshot</summary>" + D.snaps[st.snapshot] + "</details>";
    h += "</div></div>";
  }
  if (!r.steps || !r.steps.length) h += '<p class="hint">' + esc(r.reason || "No steps recorded.") + "</p>";
  detail.innerHTML = h;
  detail.querySelectorAll("details").forEach((d) => d.addEventListener("toggle", () => { const p = d.querySelector("pre.term"); if (p) p.scrollTop = p.scrollHeight; }));
  detail.querySelectorAll("details[open] pre.term").forEach((p) => (p.scrollTop = p.scrollHeight));
  history.replaceState(null, "", "#" + hid + "/" + sid);
}
// Where each event's time went: ms after publish at each stage, and a bar
// with a segment per stage (from the previous stage seen to this one).
const STAGES = ${JSON.stringify(STAGES)};
const STAGE_COLORS = { accepted: "#9aa5b1", created: "#7b8ea3", available: "#b4a06b", handed: "#4c78a8", confirmed: "#72b7b2", turn: "#5c6bc0", first: "#ab47bc", pane: "#e09f3e", reported: "#b279a2", got: "#2f9e5b" };
const STAGE_NAMES = { accepted: "publish accepted", created: "delivery created", available: "may go out", handed: "handed to a waiter", confirmed: "confirmed", turn: "turn started (transcript)", first: "model's first output (transcript)", pane: "code in the pane", reported: "report_seen", got: "GOT visible" };
// In the cell: the median time to the waiter, and from the waiter to GOT.
function cellBreakdown(r) {
  const m = r.metrics || {};
  if (!r.breakdown || m.p50_got_ms == null) return "";
  const s = (x) => (x == null ? "–" : (x / 1000).toFixed(1) + "s");
  const handed = m.p50_handed_ms;
  return "<small>waiter " + s(handed) + " · then " + (handed == null ? "–" : s(m.p50_got_ms - handed)) + "</small>";
}
function breakdownTable(b) {
  const max = Math.max(1, ...b.map((x) => Math.max(...STAGES.map((s) => x[s] || 0))));
  let h = '<table class="bd"><tr><th>event</th>' + STAGES.map((s) => '<th title="' + esc(STAGE_NAMES[s]) + '"><i style="background:' + STAGE_COLORS[s] + '"></i>' + esc(s) + "</th>").join("") + "<th>where the time went</th></tr>";
  for (const x of b) {
    h += "<tr><td>" + esc(x.code) + "</td>" + STAGES.map((s) => "<td>" + (x[s] == null ? "–" : (x[s] / 1000).toFixed(2) + "s") + "</td>").join("");
    let prev = 0, bar = "";
    for (const s of STAGES) {
      if (x[s] == null || x[s] < prev) continue;
      bar += '<span title="' + esc(STAGE_NAMES[s] + ": +" + ((x[s] - prev) / 1000).toFixed(2) + "s") + '" style="width:' + (((x[s] - prev) / max) * 100).toFixed(2) + "%;background:" + STAGE_COLORS[s] + '"></span>';
      prev = x[s];
    }
    h += '<td class="bar"><div>' + bar + "</div></td></tr>";
  }
  return h + '</table><p class="hint">Server stages from the core\\'s database (local core only: each state change as the core wrote it); pane stages from the terminal, checked every 250ms. Hover a segment for the time it took.</p>';
}
function isLastSnap(r, st) { const s = (r.steps || []).filter((x) => x.snapshot); return s.length && s[s.length - 1] === st; }
`;

/** After the sections' scripts: the grid, and a cell from the URL. */
const START = `
render();
grid.addEventListener("click", (e) => { const c = e.target.closest("td.cell[data-h]"); if (c) show(c.dataset.h, c.dataset.s, c); });
if (location.hash.length > 1) { const [h, s] = location.hash.slice(1).split("/"); const c = document.querySelector('td.cell[data-h="' + h + '"][data-s="' + s + '"]'); if (c) show(h, s, c); }
`;
