# The toy app's contract

What every toy app variant serves, so the bench's scenarios run the same against an app built on any SDK and framework we support (`WB_APP`). The words are in `contract.json`; this file says what they're for and what each part must do. `src/conformance.ts` checks a variant against both (`node dist/src/conformance.js` in `bench/`, after `npm run build`).

## Variants

| `WB_APP` | Framework | SDK | Where |
| --- | --- | --- | --- |
| `ts-v2` (default) | `@modelcontextprotocol/server` 2.x: a session per client that initializes, `createMcpHandler` for 2026-07-28 (with `WB_MCP_2026=1` or the task path) | `@wakeshift/sdk` | `server.ts` |
| `ts-v1` | `@modelcontextprotocol/sdk` 1.x: the same two ways | `@wakeshift/sdk` | `node-v1/server.mjs` (also `WB_MCP_SDK=v1`, the setting `WB_APP` generalizes) |
| `ts-mastra` | Mastra `MCPServer` (`@mastra/mcp`), `startHTTP` | `@wakeshift/sdk/mastra` | `server-mastra.ts`, `server.ts`'s Mastra route (also `WB_MCP_SDK=mastra`) |
| `go-sdk` | `modelcontextprotocol/go-sdk`, stateless Streamable HTTP | `pkg/wakeshift` | `go/gosdk` |
| `go-mcpgo` | `mark3labs/mcp-go`, Streamable HTTP | `pkg/wakeshift/mcpgo` | `go/mcpgo` |
| `py-mcpserver` | `mcp` 2.x `MCPServer` | `wakeshift` | `python/mcpserver_app.py` |
| `py-fastmcp` | FastMCP 4, `http_app` | `wakeshift[fastmcp]` | `python/fastmcp_app.py` |
| `py-lowlevel` | `mcp` 2.x low-level `Server`, `on_list_tools` and `on_call_tool` | `wakeshift` (`wrapped_tool`, `attached.call`) | `python/lowlevel_app.py` |

`WB_APP_2` picks S15's second app (default: the same as the first). Each port is served the way its framework's README serves one. The list is `variants.ts`.

## The MCP server

One MCP server per lane at `/mcp/<lane>`, named `wakebench` (S15's second app: `wakebench2`), version `0.1.0`. Every caller of a lane's server is that lane's user (`identify`). Wakeshift is attached with none of its own tools (`builtinTools` all off), so `tools/list` is exactly:

| Tool | Input (besides Wakeshift's arguments) | Does | Replies |
| --- | --- | --- | --- |
| `watch` | none | subscribes the calling conversation to `bench.event {run, lane}`, TTL 1 day, from inside the SDK's wrapper | `tools.watch.text` |
| `unwatch` | none | ends that subscription | `tools.unwatch.ended`, or `notWatching` if there was none |
| `report_seen` | `ids`: array of strings, required | nothing but the record | `Noted <n> event(s).` |

Each is wrapped by the SDK (in TypeScript, `attached.registerTool`; each SDK's equivalent in the ports), so Wakeshift adds its block to every reply, after the tool's own text. `watch` declares `subscribes: [bench.event]`; `unwatch` and `report_seen` declare `subscribes: []`. So, as every SDK's compact `tools/list` has it, `watch` lists all three of Wakeshift's arguments (`session_key`, `harness`, `conversation_id`) and its description is the contract's followed by the SDK's subscribes sentence ("This tool is how <App> notifies you in this conversation of this event: A bench test event for one lane of one run. …"); `unwatch` and `report_seen` list only `session_key`, and their descriptions are the contract's, word for word.

## The event

`bench.event`, description `A bench test event for one lane of one run`, arguments `{run, lane}` (both strings), topic `bench/{run}/{lane}`, payload `{code, seq, sent_at, tag?}` (`seq` an integer, `tag` optional), and the text `Bench event <code>.` (`Bench event <code> (<tag>).` with a tag). It's `public: true`, open to `subscribe_events` and native subscriptions. A port's SDK sends its own manifest, so the definition must be the same as `server.ts`'s, field for field, or two variants on one app would keep replacing each other's (conformance: "same event as ts-v2"). For the same reason, a target can keep the other variants off an app whose manifest must stay as `server.ts` sends it (`src/core.ts`, `variantProblem`; the toy app's `BENCH_PROTECTED_APP`, `main.ts`).

Codes are `WB-<nonce>-<seq>`: the nonce is four hex digits of `sha256("<run>/<lane>")`, the second app's from another input (`codeNonce`), and `seq` counts from 1 per lane. The event's ID is `<run>:<lane>:<seq>`.

## The control API

The bench drives every variant through the same API, under `/control`, with `Authorization: Bearer <token>` (the token from `BENCH_CONTROL_FILE`, mode 0600):

- `POST /control/publish {lane, count?, tag?}`: emits `count` events (1–20), returns `{events: [{id, code, lane, at, accepted_at, matched}]}`. `at` is when the app called emit, `accepted_at` when it returned.
- `GET /control/lane?lane=`: `{lane, topic, calls, events, seen, subscriptions, deliveries}`. `deliveries` is read from the core's database on a local core, else null.
- `GET /control/status?lane=`: `{event: {subscriptions, connected, via}, subscriptions: [statusOf each]}`, from the SDK's `status` and `statusOf`.

Not part of the contract: `POST /control/options {lane, unrecognized?, tools?}`, which only `server.ts` (ts-v2) serves, for S16 and S17. It gives one lane `job_status` and `start_job` (`jobs.ts`) and, with `unrecognized`, disguises that lane as an agent Wakeshift doesn't recognize. A lane without options lists exactly the three tools, so ports needn't implement it: the front answers 404, the Mastra route 501, and the bench marks S16 and S17 n/a there.
- `GET /health`.

## The call record

Each call of the three tools is recorded from the result the client receives, after Wakeshift's block is added (`CallRecord` in `server.ts`): `at`, `tool`, `lane`, `client` (initialize's client name, else the request's `_meta`), `args` (`harness`, `conversation_id`, `session_key` as the first 12 hex digits of its SHA-256, never the key, and `ids`), `agent` (what Wakeshift recognized: `harness`, `via`, `claimed`, null where none), `subscription` and `session` (the session Wakeshift gave, hashed the same way; `watch` only), `added` (the text Wakeshift added, redacted), `beacon` (Wakeshift's `_meta` or a beacon line), and `events_in_result` (codes in the result). A call whose handler threw isn't recorded. `report_seen` also adds `{at, lane, ids}` to `seen`.

The scenarios read these: S1 the quote against `added`, S9 `args` against `session` and `agent`, S15 `session` across calls, S2 to S15 `seen` and `events_in_result` for what arrived.

## Ports

Every variant but `server.ts` and its Mastra route is a port (Node, Go or Python) and runs behind `front.ts`, which serves the control API and records calls, so a port is only its framework's server and the SDK's wrapper. The front passes `/mcp/<lane>` through to the port's `/mcp` (SSE rewritten event by event as it streams, never held), adding two headers: `Authorization: Bearer <lane>` (the port's `identify` reads the lane from it) and `X-Wakebench-Call`, an ID for the exchange. What the wire doesn't show, each handler files under that ID and the front reads before the result goes on: `GET /bench/note?call=` returns `{tool, lane, subscription?, session?, agent?}` (the session hashed by the port, never sent). The port also serves `POST /bench/emit {lane, id, payload}` → `{matched}` and `POST /bench/status {lane, subscriptions}` → `{event, subscriptions}`, both under the control token, and `GET /health`. It reads `PORT`, `BENCH_KEY_FILE`, `WAKESHIFT_CORE`, `BENCH_CONTROL_FILE`, `BENCH_CONTRACT`, `BENCH_MCP_NAME`, `BENCH_RUN`, `BENCH_REQUIRE_APP` (refuse a key for any other app) and `BENCH_WAIT` (`1`: serve the SDK's wait tool; otherwise not, the SDKs' default), and exits when its standard input closes. A port adds about 15ms to a tool call (the front logs each call's time in `app.log`).

Shared code: `go/toy` (Go), `python/toy.py` (Python); the 1.x port is one file. Adding a port: write the server the way the framework's docs do, with the three tools calling the shared bodies; add it to `variants.ts`; run `node dist/src/conformance.js --only <id>`.

## Conformance

`src/conformance.ts` runs each variant as the bench does (the toy app and its control API, a port behind the front) against a core from Go's `TestSDKHarness` (`internal/coretest/sdk_harness_test.go`), and checks, in order: `server/discover` on 2026-07-28 (with the events capability), `tools/list` (exactly the three tools; `watch` with the contract's description plus the subscribes sentence and all three of Wakeshift's arguments, `unwatch` and `report_seen` with the contract's descriptions and only `session_key`; `ids`), `watch` (the contract's text first, then Wakeshift's block with Claude Code's Monitor command; recorded with the client, the recognized harness, the subscription and the session's hash), status (one live subscription), publish (matched 1, and the event reaches the session's wait), `report_seen` (recorded with the session key's hash equal to watch's session), `unwatch` (no subscription left), MCP tasks without the wait tool, the SDKs' default (a client that takes them sees no `wakeshift_wait` and no tasks extension, and is told about the Monitor), `events/list` (`bench.event`, the same definition as `server.ts`'s), and a handshake-era client (initialize, as Codex): recognized as Codex, with a beacon. Then it starts the variant again with the wait tool on (`WB_WAIT=1`, `BENCH_WAIT=1` for a port) and checks that a client that takes MCP tasks gets the tasks extension, lists `wakeshift_wait` and is told to call it.
