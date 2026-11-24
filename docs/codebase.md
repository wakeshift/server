# The codebase: where things are

What each part of this repository is, and how the parts connect. The rules for changing them are in [AGENTS.md](../AGENTS.md).

The server wakes agents (Claude Code, Codex, Cursor and others) when events happen in the apps they use. Apps are MCP servers with one of the SDKs attached; agents subscribe through them; the server holds the subscriptions and delivers each occurrence by MCP Events or through a bridge on the agent's machine.

## The server

- `cmd/wakeshift/` is the server binary. `internal/core` assembles it: the database, sign-in, token signing, the app API, the stream server and the background passes (expiry, retention, delivery).
- `internal/config/` reads the environment ([self-hosting.md](self-hosting.md) explains each setting). `internal/server/` is the HTTP server: the console's JSON API under `/api/v1`, the app API under `/api/apps/v1`, the stream endpoints under `/v1`, health checks, and the embedded console for every other path.
- `internal/db/` is Postgres access and migrations (`internal/db/migrations/NNN_name.sql`, applied at startup). `internal/dbtest/` gives each test its own database.
- `internal/appapi/` is the app API the SDKs call: event subscriptions, emits, publish URLs, the MCP Events routes (`eventsubs.go`, `eventspoll.go`, `eventspush.go`), and what a tool call's result tells the agent (`/calls/session`, `/calls/addition`), including the beacons and install offers for each harness.
- `internal/mcpevents/` is MCP Events webhook delivery: signing, verification, retries, the SSRF-safe client, and the per-client dialects (`event/`, with the client table `mcp-events-clients.json`). [mcp-events.md](mcp-events.md) explains it.
- `internal/streams/` serves stream links: WebSocket, Server-Sent Events, JSON lines, long polls and one-shot waits, all sharing claiming, leases, takeover, rotation and revocation.
- `internal/events/`, `internal/pipeline/`, `internal/deliveries/` and `internal/subscriptions/` are the event log, matching events to subscriptions, and the per-subscription outbox with its states, retries and 30-day retention.
- `internal/sessions/` and `internal/agents/` are agents (durable roles such as `claude-code`) and their sessions (one per conversation). `internal/grants/` records consents; `internal/audit/` is each workspace's append-only record of them.
- `internal/apps/` is the app registry: `WAKESHIFT_APPS`, apps created in the console, their keys and manifests. `internal/appkey/` reads and writes app keys, shared with the Go SDK.
- `internal/accounts/`, `internal/auth/` and `internal/oauth/` are console accounts and sign-in (Google, GitHub, development sign-in), the sign-up cap and waitlist, and the keys that sign tokens and beacons. `internal/orgs/` is organizations, their roles and invites; `apps.Registry.Access` is the one check on every developer route. `internal/console/` is the API behind the console.
- `internal/harnesses/` is what the server knows about each harness: client names, how each is woken, install offers and their wording. [harnesses.md](harnesses.md) explains it.
- `internal/agenthooks/` is delivery to webhooks an agent registers for itself (Capy). `internal/botkeys/` is how a Grok Bot proves its key.
- `internal/waittask/` seals the task IDs of Wakeshift's wait tool, scoped to the conversation.
- `internal/telemetry/` exports traces and logs with OpenTelemetry, only when `OTEL_EXPORTER_OTLP_ENDPOINT` is set, every span and record scrubbed first: attributes leave only on allowlists, and URLs, quoted values, emails, IP addresses and tokens are removed from names and messages.
- `internal/adminapi/` and `internal/iap/` are the hosted service's internal admin API, off unless its settings are set.
- `internal/mail/` sends invite email through Resend, or not at all.
- `web/` is the console: React, TypeScript, Vite and Tailwind, embedded into the binary from `web/dist` (`web/embed.go`).
- `site/docs/` is the source of the developer documentation at [wakeshift.ai/docs](https://wakeshift.ai/docs). Read it there: its links are the site's paths, and some pages hold template placeholders the site fills in, so it doesn't render properly on GitHub.

There's no MCP endpoint on the server itself. Agents talk only to apps' MCP servers, which wrap the app API; the server's agent-facing surface is its stream endpoints, which apps hand out links to.

Apps identify their own users, in workspaces they own. An app can't act in a person's own Wakeshift workspace.

## On users' machines

These are published, with their own tests and releases, in [wakeshift/connectors](https://github.com/wakeshift/connectors). The packages are here too, because the server builds and serves the connector:

- `cmd/wakeshift-connector/` is the connector, installed as `wakeshift`: `internal/connectcli` is its command line, `internal/connector` the hooks, installers and long-poll client for each harness, and `internal/codexconnector` Codex's per-thread process.
- `internal/beacon/`, `internal/jws/` and `internal/hookproof/` are the signed beacons hooks act on, and what a hook's own key signs.
- `internal/framing/` is how a delivered message is put in front of a model: as information from outside the conversation, never as the user.
- `internal/downloads/` serves the connector binaries and the `/install/<harness>.sh` scripts.

## The SDKs

The SDKs, plugins and apps in this tree (`pkg/wakeshift/`, `sdk/`, `plugins/`, `apps/`) are here because the server builds and tests against them: its tests run the SDKs and an app against a real server, and it serves the plugins' files. Each is published from here to a repository of its own, with its own releases: install them from there (the repositories below), not from this tree.

`pkg/wakeshift/` is the Go SDK, published as [wakeshift-go](https://github.com/wakeshift/wakeshift-go); the TypeScript and Python SDKs are [wakeshift-node](https://github.com/wakeshift/wakeshift-node) and [wakeshift-python](https://github.com/wakeshift/wakeshift-python). The three share golden files for the wire format and the text they add to tool results, so they answer the same bytes. `apps/agentkit/` and `apps/email/` are what the hosted service's own apps share with the server (an agent's sessions and waits; the email layout). `apps/try/` is Webhooks, which docker-compose.yml runs behind the profile `webhooks` so you can wake an agent with one POST ([self-hosting.md](self-hosting.md#webhooks)).

## Tests

- `make test` runs the Go tests with `-race` against Postgres and type-checks the console.
- `internal/coretest/` runs a real server in-process against a real database: the end-to-end tests for delivery, MCP Events, the bridges' server side and tenant isolation.
- `TestEveryTenantTableIsIsolated` fails if a table with tenant data lacks row-level security.
