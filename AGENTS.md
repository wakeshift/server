# Instructions for coding agents

This repository is the Wakeshift server: it holds MCP Events subscriptions for apps' MCP servers and delivers each occurrence by webhook, poll or push, or wakes the agent's conversation through a bridge on the user's machine. [README.md](README.md) says what it does; [docs/codebase.md](docs/codebase.md) says where each part is. Read its section before working in a part.

## Layout

- `cmd/wakeshift/` is the server binary; `cmd/appkey/` makes an app's key.
- `internal/` is the server's packages: `config` (the environment), `server` (HTTP), `db` (Postgres and migrations), `appapi` (the API the SDKs call), `mcpevents` (MCP Events webhook delivery), `streams` (stream links), `harnesses` (what the server knows about each agent), and the rest listed in [docs/codebase.md](docs/codebase.md).
- `internal/connector`, `internal/codexconnector`, `internal/connectcli` and `cmd/wakeshift-connector` are the connector that runs on users' machines, also published in [wakeshift/connectors](https://github.com/wakeshift/connectors).
- `pkg/wakeshift/` is the Go SDK.
- `web/` is the console (React, TypeScript, Vite, Tailwind), embedded from `web/dist`.
- `bench/` is the harness runner that reproduces the harness matrix with real agents.
- `site/docs/` is the source of the developer documentation at https://wakeshift.ai/docs (read it there: its links are site paths and some pages hold template placeholders); `docs/` covers MCP Events, the harnesses and self-hosting.

## Running the tests

- Go 1.27, Node 22 or later (26 or later for the bench), and Docker for Postgres.
- `make db` starts Postgres 17 from `docker-compose.yml` on `127.0.0.1:5439`. `make test` runs the Go tests with `-race` against it and type-checks the console. `make lint` runs gofmt, vet and the TypeScript check.
- Tests that need a database call `dbtest.New(t)`, which gives each test its own database; they need `WAKESHIFT_TEST_DATABASE_URL`, which `make test` sets.
- `internal/coretest` runs a whole server in-process and acts as its users, agents and apps. A change to delivery, consent or a bridge's server side gets a test there.
- Run the tests for the packages you changed while you work (`go test ./internal/streams/...`); run `make test` before you open a pull request.
- `make run` builds the console and runs the server on `127.0.0.1:8370` with development sign-in (it prints a one-time sign-in token at startup). `make gate` runs the release gate: the server in compose, a new app, and an event delivered on push.
- `make bench-test` and `make bench-dry-run` test the harness runner in `bench/` without any agent or account ([bench/README.md](bench/README.md)).

## Tenant isolation

Every tenant's data shares tables, separated by `tenant_id` and enforced by Postgres row-level security:

- Every table with tenant data has `tenant_id`, `ENABLE` and `FORCE ROW LEVEL SECURITY`, and the `tenant_isolation` policy (the template is in `001_tenants.sql`). `TestEveryTenantTableIsIsolated` fails if a table misses any of them. Only `tenants`, `schema_migrations`, the account tables and platform-level tables read before a workspace is chosen are exempt.
- Read and write tenant data only inside `db.InTenant`. Outside it, scoped tables read as empty and reject writes.
- The server refuses to run as a superuser or a `BYPASSRLS` role, or a member of one, since either would silently turn isolation off.
- An app acting for an agent reads only its own deliveries (`deliveries.OfApp`) and resolves only sessions it joined (`sessions.Session.Joined`). Keep new app-facing reads scoped the same way.

## Security-sensitive areas

Treat changes to these as security-sensitive, and test them:

- **Tenant isolation**, above.
- **Consent and authorization:** who may subscribe to an event (`personal`, `authorize`, `public`, or only the app's own tools), re-authorization of open subscriptions, and revocation. A new way to consent, sign in or revoke records an `audit.Record` entry in the same transaction.
- **Sign-in, OAuth and tokens:** `internal/auth`, `internal/oauth`, app keys (`internal/appkey`), and development sign-in, which must stay limited to localhost.
- **Beacons and stream links:** a hook acts only on a beacon signed by the issuer pinned at install, for its own conversation or call, from the tool the beacon names. A stream link is a bearer credential. Anything that lets one agent wake, read or act as another is in this category.
- **MCP Events delivery:** callback verification, the SSRF checks on every connection, signatures, and the per-client dialects.
- **Telemetry:** attributes leave only on the allowlists in `internal/telemetry`, and names and messages are scrubbed of credentials. Don't add a key without a reason it can't carry a secret.
- **The connector and installers:** what they write on users' machines, the issuer they trust, and checksum checks.

## Conventions

- Match the surrounding code: `net/http` routing, `pgx` with hand-written SQL, `log/slog`. API errors are `{"error": {"code", "message"}}`.
- Never log tokens, secrets, query strings, or request bodies that can carry them.
- Migrations: add a new numbered file; never edit or rename one that's been released; no `BEGIN` or `COMMIT` inside. A migration runs while the previous release still serves, so add first and remove in a later release. Row-level security applies to migrations too: a data migration that reads tenant tables must loop over tenants and set `app.tenant_id` for each (the pattern is in `005_deliveries.sql`), or it silently does nothing.
- Messages delivered to an agent are framed as information from outside the conversation (`internal/framing`), never as the user, and never interrupt a running turn.
- Public names use American spelling.
- Commit messages and pull request titles follow Conventional Commits (`feat(scope): summary`, `fix: …`, `docs: …`), and every commit is signed off ([CONTRIBUTING](CONTRIBUTING.md)).
