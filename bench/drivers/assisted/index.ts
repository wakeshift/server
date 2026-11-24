// Assisted harnesses: no terminal the bench can drive (web apps, desktop
// apps). The bench prints each step (with prompts and URLs to copy), waits
// for Enter, and records timings and outcomes from the toy app's own data
// (which calls the agent made, with which arguments; what it reported
// seeing; delivery state), plus the user's y/n answers. See src/assisted.ts.

export interface AssistedDriver {
  id: string;
  name: string;
  mode: "assisted";
  /** What S9 expects the harness to be recognized or named as. */
  harness: string;
  conversation: "agent" | "auto" | "none";
  /** Where the MCP server has to be reachable from (a web app can't reach this machine's localhost). */
  needsPublicUrl: boolean;
  /** How to add the server, per step. */
  add(mcpUrl: string): string[];
  /** Whether an idle conversation can be woken (S2–S4). */
  wakes: boolean;
  /** Whether there's a one-time install (S6). */
  install: boolean;
  /** How to restart it (S8), or null. */
  restart: string | null;
  notes?: string;
}

export const assisted: Record<string, AssistedDriver> = {
  "claude-cowork": {
    id: "claude-cowork",
    name: "Claude Cowork",
    mode: "assisted",
    harness: "claude-cowork",
    conversation: "none",
    needsPublicUrl: true,
    add: (u) => [
      `In the Claude app: Settings → Connectors → Add custom connector. Name: wakebench, URL: ${u}`,
      "Cowork's sandbox must reach the core (its Monitor reads the core's stream link): run against a core it can reach (a hosted one), not a local one, and if your workspace limits network access, allow the core's host under Settings → Capabilities (Organization settings → Capabilities on Team and Enterprise). To test the refusal path, leave it blocked for S1: the agent should say it can't reach Wakeshift and tell you where to allow it.",
      "Open a new Cowork task with the wakebench connector enabled.",
    ],
    wakes: true,
    install: false,
    restart: "Quit the Claude app (Cmd-Q), and reopen the same Cowork task.",
    notes: "Woken by a Monitor the agent starts (docs/harnesses.md); a quit app ends it, and the agent starts it again on its next turn.",
  },
  chatgpt: {
    id: "chatgpt",
    name: "ChatGPT",
    mode: "assisted",
    harness: "chatgpt",
    conversation: "none",
    needsPublicUrl: true,
    add: (u) => [`In ChatGPT: Settings → Apps & Connectors → Advanced → Developer mode on; then Create a connector. Name: wakebench, URL: ${u}, no authentication.`, "Start a new chat and enable the wakebench connector (+ → Developer mode → wakebench)."],
    wakes: false,
    install: false,
    restart: "Reload the ChatGPT tab and open the same chat.",
  },
  "grok-bot": {
    id: "grok-bot",
    name: "Grok Bot",
    mode: "assisted",
    harness: "grok-bot",
    conversation: "agent",
    needsPublicUrl: true,
    add: (u) => [`Add the MCP server to Grok Bot's Cursor configuration (its mcp.json): {"mcpServers": {"wakebench": {"url": "${u}"}}}`, "Start a new Grok Bot conversation."],
    wakes: false,
    install: false,
    restart: null,
    notes: "Grok Bot can only wait in the foreground (docs/harnesses.md): an idle wake isn't expected.",
  },
  "cursor-ide": {
    id: "cursor-ide",
    name: "Cursor IDE",
    mode: "assisted",
    harness: "cursor",
    conversation: "agent",
    needsPublicUrl: false,
    add: (u) => [`In Cursor: Settings → MCP → Add a custom MCP server, or add to ~/.cursor/mcp.json: {"mcpServers": {"wakebench": {"url": "${u}"}}}. (Use a profile you're fine changing.)`, "Open a new Agent chat."],
    wakes: true,
    install: true,
    restart: "Quit Cursor (Cmd-Q), reopen it and the same chat.",
  },
};
