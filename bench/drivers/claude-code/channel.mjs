// A minimal Claude Code channel (spike): a stdio MCP server that declares
// claude/channel and forwards each line of a Wakeshift session's stream
// (format=lines) as notifications/claude/channel. The stream link comes
// from a file the bench's toy app writes once the agent has called watch
// (WB_CHANNEL_LINK_FILE). Logs (no links) to WB_CHANNEL_LOG.
import { appendFileSync, existsSync, readFileSync } from "node:fs";
import { createInterface } from "node:readline";

const LOG = process.env.WB_CHANNEL_LOG;
const log = (m) => LOG && appendFileSync(LOG, `${new Date().toISOString()} ${m}\n`);
const send = (o) => process.stdout.write(JSON.stringify(o) + "\n");
const OLD = ["2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"];
let started = false;

createInterface({ input: process.stdin }).on("line", (s) => {
  let m;
  try {
    m = JSON.parse(s);
  } catch {
    return;
  }
  if (m.method === "initialize") {
    const asked = m.params?.protocolVersion;
    const v = OLD.includes(asked) ? asked : OLD[0];
    log(`initialize: client asked ${asked}, answering ${v}`);
    send({ jsonrpc: "2.0", id: m.id, result: { protocolVersion: v, capabilities: { experimental: { "claude/channel": {} }, tools: {} }, serverInfo: { name: "wakebench-channel", version: "0.1.0" }, instructions: "Bench events arrive in this conversation as channel messages from wakebench-channel." } });
  } else if (m.method === "notifications/initialized") {
    if (!started) {
      started = true;
      stream().catch((e) => log(`stream: ${e}`));
    }
  } else if (m.method === "tools/list") send({ jsonrpc: "2.0", id: m.id, result: { tools: [] } });
  else if (m.method === "ping") send({ jsonrpc: "2.0", id: m.id, result: {} });
  else if (m.id !== undefined) send({ jsonrpc: "2.0", id: m.id, error: { code: -32601, message: "not supported" } });
});

async function stream() {
  const file = process.env.WB_CHANNEL_LINK_FILE;
  for (;;) {
    while (!file || !existsSync(file)) await new Promise((r) => setTimeout(r, 200));
    const url = readFileSync(file, "utf8").trim();
    log("connecting");
    try {
      const r = await fetch(url);
      let buf = "";
      const dec = new TextDecoder();
      for await (const chunk of r.body) {
        buf += dec.decode(chunk, { stream: true });
        let i;
        while ((i = buf.indexOf("\n")) >= 0) {
          const line = buf.slice(0, i).trim();
          buf = buf.slice(i + 1);
          if (!line) continue;
          log(`line ${line.length} bytes`);
          send({ jsonrpc: "2.0", method: "notifications/claude/channel", params: { content: line, meta: { source: "wakebench" } } });
        }
      }
      log("stream ended");
    } catch (e) {
      log(`stream error: ${e}`);
    }
    await new Promise((r) => setTimeout(r, 1000));
  }
}
