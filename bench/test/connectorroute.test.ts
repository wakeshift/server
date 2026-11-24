// WB_CONNECTOR_ROUTE: what the claude.ai-connector-style route does to a
// call and its result, as the app and the core see them.

import { strict as assert } from "node:assert";
import { createServer } from "node:http";
import type { AddressInfo } from "node:net";
import { test } from "node:test";
import { CONNECTOR_CLIENT, rewriteRequest, rewriteResponse, startConnectorRoute } from "../src/connectorroute.js";

test("a call states Anthropic's client and keeps only the protocol's _meta", () => {
  const call = {
    jsonrpc: "2.0",
    id: 3,
    method: "tools/call",
    params: {
      name: "watch",
      arguments: { harness: "claude-code+mark" },
      _meta: { "claudecode/toolUseId": "toolu_1", progressToken: 7, "io.modelcontextprotocol/clientInfo": { name: "claude-code", version: "2.1.321" } },
    },
  };
  const out = rewriteRequest(call) as typeof call;
  assert.deepEqual(out.params._meta, { progressToken: 7, "io.modelcontextprotocol/clientInfo": { name: CONNECTOR_CLIENT, version: "1.0.0" } });
  assert.deepEqual(out.params.arguments, call.params.arguments);
  const init = rewriteRequest({ jsonrpc: "2.0", id: 1, method: "initialize", params: { clientInfo: { name: "claude-code", version: "2.1.321" } } }) as { params: { clientInfo: { name: string } } };
  assert.equal(init.params.clientInfo.name, CONNECTOR_CLIENT);
});

test("a result keeps only the protocol's _meta (serverInfo), not a beacon", () => {
  const res = { jsonrpc: "2.0", id: 3, result: { content: [{ type: "text", text: "Subscribed." }], _meta: { "wakeshift/connector": "eyJ.x.y", "io.modelcontextprotocol/serverInfo": { name: "wakebench", version: "1" } } } };
  const out = rewriteResponse(res) as typeof res;
  assert.deepEqual(out.result._meta, { "io.modelcontextprotocol/serverInfo": { name: "wakebench", version: "1" } });
  assert.deepEqual(out.result.content, res.result.content);
});

test("the route forwards to the lane's MCP URL, rewritten both ways", async () => {
  let seen: { path?: string; ua?: string; body?: unknown } = {};
  const app = createServer((req, res) => {
    let b = "";
    req.on("data", (c) => (b += c));
    req.on("end", () => {
      seen = { path: req.url, ua: req.headers["user-agent"], body: JSON.parse(b) };
      res.writeHead(200, { "content-type": "text/event-stream" });
      res.end(`event: message\ndata: ${JSON.stringify({ jsonrpc: "2.0", id: 1, result: { content: [], _meta: { "wakeshift/connector": "t" } } })}\n\n`);
    });
  });
  await new Promise<void>((r) => app.listen(0, "127.0.0.1", r));
  const base = `http://127.0.0.1:${(app.address() as AddressInfo).port}`;
  const route = await startConnectorRoute((lane) => `${base}/mcp/${lane}`);
  try {
    const r = await fetch(route.mcpUrl("cc-1"), {
      method: "POST",
      headers: { "content-type": "application/json", "user-agent": "claude-code/2.1.321" },
      body: JSON.stringify({ jsonrpc: "2.0", id: 1, method: "tools/call", params: { name: "watch", _meta: { "claudecode/toolUseId": "toolu_9" } } }),
    });
    const text = await r.text();
    assert.equal(seen.path, "/mcp/cc-1");
    assert.notEqual(seen.ua, "claude-code/2.1.321");
    assert.deepEqual((seen.body as { params: { _meta: unknown } }).params._meta, {});
    assert.ok(!text.includes("wakeshift/connector"), text);
  } finally {
    await route.stop();
    await new Promise<void>((r) => app.close(() => r()));
  }
});
