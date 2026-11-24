// The agent picker (apps/agentpicker): shows one agent's steps at a time and
// remembers the choice. Without it, every agent's steps show.
(function () {
  "use strict";
  var KEY = "wakeshift-agent";
  function load() {
    try { return window.localStorage.getItem(KEY); } catch (e) { return null; }
  }
  function save(id) {
    try { window.localStorage.setItem(KEY, id); } catch (e) { /* private mode */ }
  }
  function setup(root) {
    var pills = Array.prototype.slice.call(root.querySelectorAll(".ap-pill[data-agent]"));
    var panels = Array.prototype.slice.call(root.querySelectorAll(".ap-panel[data-agent]"));
    var more = root.querySelector(".ap-pill.ap-more");
    var moreList = root.querySelector(".ap-more-list");
    if (!pills.length || !panels.length) return;
    function openMore(open) {
      if (!more || !moreList) return;
      more.setAttribute("aria-expanded", open ? "true" : "false");
      moreList.classList.toggle("ap-open", open);
    }
    function select(id, remember) {
      var found = false;
      pills.forEach(function (p) {
        var on = p.getAttribute("data-agent") === id;
        p.setAttribute("aria-pressed", on ? "true" : "false");
        if (on) {
          found = true;
          if (moreList && moreList.contains(p)) openMore(true);
        }
      });
      if (!found) return false;
      panels.forEach(function (p) { p.classList.toggle("ap-on", p.getAttribute("data-agent") === id); });
      if (remember) save(id);
      return true;
    }
    root.classList.add("ap-js");
    pills.forEach(function (p) {
      p.addEventListener("click", function () { select(p.getAttribute("data-agent"), true); });
    });
    if (more) {
      more.addEventListener("click", function () { openMore(more.getAttribute("aria-expanded") !== "true"); });
    }
    if (!select(load() || "", false)) select(root.getAttribute("data-default") || pills[0].getAttribute("data-agent"), false);
    // Copy buttons on commands and config, and on what to ask the agent.
    if (navigator.clipboard) {
      // An icon (two sheets; a tick once copied), drawn here: the page's
      // markup carries no SVG of its own.
      var NS = "http://www.w3.org/2000/svg";
      var icon = function (copied) {
        var svg = document.createElementNS(NS, "svg");
        svg.setAttribute("viewBox", "0 0 16 16");
        svg.setAttribute("aria-hidden", "true");
        svg.setAttribute("class", "ap-copy-icon");
        var shapes = copied ? [["path", { d: "M3.5 8.5l3 3 6-7" }]]
          : [["rect", { x: "5.5", y: "5.5", width: "8", height: "8", rx: "1.6" }], ["path", { d: "M10.5 3.2V3a1.5 1.5 0 0 0-1.5-1.5H4A1.5 1.5 0 0 0 2.5 3v5A1.5 1.5 0 0 0 4 9.5h.3" }]];
        shapes.forEach(function (sh) {
          var e = document.createElementNS(NS, sh[0]);
          for (var k in sh[1]) e.setAttribute(k, sh[1][k]);
          svg.appendChild(e);
        });
        return svg;
      };
      var copyButton = function (source, cls, label, asIcon) {
        var b = document.createElement("button");
        b.type = "button";
        b.className = cls;
        var show = function (copied) {
          if (!asIcon) { b.textContent = copied ? "Copied" : "Copy"; return; }
          b.replaceChildren(icon(copied));
          b.setAttribute("aria-label", copied ? "Copied" : label);
          b.title = copied ? "Copied" : "Copy";
        };
        show(false);
        if (label && !asIcon) b.setAttribute("aria-label", label);
        b.addEventListener("click", function () {
          navigator.clipboard.writeText(source.textContent || "").then(function () {
            show(true);
            setTimeout(function () { show(false); }, 1500);
          }, function () { /* clipboard refused: leave the text selectable */ });
        });
        source.parentNode.insertBefore(b, source.nextSibling);
      };
      root.querySelectorAll(".ap-code").forEach(function (pre) {
        copyButton(pre, "ap-copy");
        pre.classList.add("ap-has-copy");
      });
      root.querySelectorAll(".ap-say q").forEach(function (q) {
        copyButton(q, "ap-copy ap-copy-inline", "Copy what to ask your agent", true);
      });
    }
  }
  function init() {
    Array.prototype.forEach.call(document.querySelectorAll("[data-ap]"), setup);
  }
  if (document.readyState === "loading") document.addEventListener("DOMContentLoaded", init);
  else init();
})();
