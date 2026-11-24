# Self-hosting

The server is one Go binary with the console built in, and a Postgres database. It reads its configuration from the environment (`internal/config/config.go` lists every setting) and applies its own database migrations at startup.

Run the container image, `ghcr.io/wakeshift/server:<version>`, built and signed for each release tag (`v*`). Until you pick a release, `docker compose up --build` or `make image` builds it from this repository. The image listens on `:8370` and also carries `/appkey`. A plain `go build ./cmd/wakeshift` gives a server with an empty console: `make build` builds the console first and embeds it.

[docker-compose.yml](../docker-compose.yml) is a working example for one machine, with everything bound to loopback; the [README](../README.md#run-it) walks through it. `make gate` runs the release gate: a fresh server in compose, a new app, and an event delivered on push to an MCP Events client. Its profile `webhooks` adds Webhooks, a webhook URL that wakes your agent ([below](#webhooks)); `make gate-webhooks` runs the gate with it too.

## Required settings

| Setting | What it is |
| --- | --- |
| `DATABASE_URL` | Postgres connection string. The role must not be a superuser or have `BYPASSRLS`, or be a member of a role that is or has: the server refuses to start, because either would turn off tenant isolation. |
| `WAKESHIFT_PUBLIC_URL` | The server's externally reachable base URL, such as `https://wakeshift.example.com`: `https`, no path. Links, cookies, sign-in and the issuer that plugins trust all derive from it. `http` is accepted only on `localhost`. Changing it later invalidates every connector install that trusts the old one. |
| `WAKESHIFT_SECRET_KEY` | At least 32 random bytes, for example `openssl rand -base64 48`. Keys for each purpose (console sessions, stream links, sealed secrets) are derived from it. Changing it signs everyone out and invalidates stream links. The development key in the Makefile is refused anywhere but `localhost`. |
| `WAKESHIFT_APPS` | The apps the server registers at startup, as JSON: `[{"id": "my-app", "name": "My app", "public_key": "<base64 Ed25519>", "mcp_url": "https://…"}]` (`mcp_url` optional). Not needed if your developers create apps in the console. |

`go run ./cmd/appkey -app my-app` (or, with Docker, `docker compose run --build --rm --no-deps --entrypoint /appkey server -app my-app`) makes an app's key and prints two lines: `WAKESHIFT_KEY=wst_…` for the app's SDK, and `WAKESHIFT_APPS=[…]` for the server. The key is a password: it signs every request the app makes. The server stores only the public half.

## Postgres

Use Postgres 17, with two ordinary roles: one that owns the database and migrates it, and one for the server that only reads and writes. The owner needs `CREATEROLE` (a migration creates a read-only role for an optional admin service, and refuses to run without it):

```sql
CREATE ROLE wakeshift LOGIN PASSWORD '…' NOSUPERUSER NOBYPASSRLS CREATEROLE;
CREATE ROLE wakeshift_server LOGIN PASSWORD '…' NOSUPERUSER NOBYPASSRLS NOCREATEROLE NOCREATEDB;
CREATE DATABASE wakeshift OWNER wakeshift;
```

Set `WAKESHIFT_MIGRATE_DATABASE_URL` to the owner's URL and `DATABASE_URL` to the server's role, as [docker-compose.yml](../docker-compose.yml) and [deploy/postgres-init.sql](../deploy/postgres-init.sql) do. At each start the owner migrates, then grants the server's role what it needs: reads and writes on the tables, but not altering them, and neither updating nor deleting the audit log. The server refuses to start if its own role owns a table or can become the owner. Without `WAKESHIFT_MIGRATE_DATABASE_URL`, the server runs as the owner, which works but can alter the tables, including the audit log's protections.

To use a Postgres 17 you already run with `make run`, instead of the one in Docker, create the roles above and skip `make db`: `make -o db run DEV_DATABASE_URL='postgres://wakeshift_server:…@localhost:5432/wakeshift' DEV_MIGRATE_DATABASE_URL='postgres://wakeshift:…@localhost:5432/wakeshift'`.

Finished deliveries and events are deleted after 30 days. Back up the database as you would any other; it holds sealed secrets that only `WAKESHIFT_SECRET_KEY` opens, so back up the key separately.

## Signing in to the console

People sign in to the console to create apps, see subscriptions and manage their organizations. Configure one or both providers:

| Settings | Provider | Callback URL to register |
| --- | --- | --- |
| `WAKESHIFT_GOOGLE_CLIENT_ID`, `WAKESHIFT_GOOGLE_CLIENT_SECRET` | A Google OAuth client ("Web application") | `$WAKESHIFT_PUBLIC_URL/auth/google/callback` |
| `WAKESHIFT_GITHUB_CLIENT_ID`, `WAKESHIFT_GITHUB_CLIENT_SECRET` | A GitHub OAuth app | `$WAKESHIFT_PUBLIC_URL/auth/github/callback` |

Set both halves of a pair, or neither. The compose file passes these through from your shell.

### Development sign-in

To use the console on your own computer without an OAuth app, run `make run`: Postgres in compose, and the server on the host at `127.0.0.1:8370` with `WAKESHIFT_DEV_SIGN_IN=1`. At startup it prints a one-time development sign-in token and a link; the console asks for the token, then signs you in with just an email, creating the account on first use.

`make run` needs Go 1.27 and Node 22 or later, besides Docker. Postgres is published on `127.0.0.1:5439`; if port 5439 is taken, run `WAKESHIFT_DB_PORT=5440 make run`. If you ran the compose setup from the README first, stop it and remove its database with `docker compose down -v` before `make run`: `make run` needs port 8370, and it uses a different secret key, so with the old database it warns "oauth: skipping a signing key … was WAKESHIFT_SECRET_KEY changed?".

Development sign-in works only when `WAKESHIFT_PUBLIC_URL` is a `localhost` URL and the request's connection comes from loopback. That's why it isn't in compose: a browser's connection to a container arrives from Docker's network, not loopback. Never set it on a server other people can reach, and never behind a reverse proxy, which connects from the same machine on everyone's behalf. `make run` also uses the published development secret key, which the server accepts only on `localhost`.

## Who can sign up

Sign-up is closed by default: only the people on the allow list get accounts.

- `WAKESHIFT_ALLOWED_SIGNUPS`: comma-separated emails and `@domains`, such as `@example.com,ada@example.org`. An `@domain` matches Google accounts that the domain manages (Google Workspace). Put yourself on it before you first sign in: with the list empty, nobody new can make an account, and the server says so at startup.
- Anyone else who signs in joins a waitlist, with no account. Invite links and organization invites don't admit them either; add them to the list instead. To let one in, mark their entry invited as the database's owner, and their next sign-in makes the account: `UPDATE waitlist SET invited_at = now() WHERE email = 'cy@example.org';`.
- Existing users can always sign in.

To open sign-up as the hosted service has it, set `WAKESHIFT_OPEN_SIGNUPS=1`. Then a fresh install admits anyone for its first 50 accounts. From the 51st, a new sign-in joins the waitlist instead. People invited to an organization or with a user's invite link, and people on the allow list, get accounts whatever the cap. To change the cap, update its one row as the database's owner: `UPDATE signup_cap SET cap = 500, updated_at = now();`. `WAKESHIFT_INVITES_PER_USER` (default 5) is how many people each user may invite.

Development sign-in (`make run`, above) makes any account it's asked for, whatever these settings.

## Point the SDKs, CLI and plugins at your server

Everything published defaults to the hosted service, `https://app.wakeshift.ai`. Point each piece at yours:

| What | How |
| --- | --- |
| The SDKs | `WAKESHIFT_CORE=https://wakeshift.example.com` in the MCP server's environment, or the `core` option. |
| `@wakeshift/cli` | `npx @wakeshift/cli init --origin https://wakeshift.example.com`, or `WAKESHIFT_CORE`. |
| The connector | Install offers from your server run `curl -fsSL https://wakeshift.example.com/install/<harness>.sh \| sh`, which trusts only your server, once it serves the connector (below). By hand: `wakeshift install --harness <harness> --issuer https://wakeshift.example.com`, with the binary you built for your platform (`bin/connector/wakeshift-<os>-<arch>`, below). The connector binary is the same for every server; the plugin's `wakeshift.env` sets which server it trusts. |
| Claude Code, Codex, Copilot CLI plugins | Each plugin trusts the server its marketplace was rendered for, never a setting. Your server's offers use the installer above instead, unless `WAKESHIFT_AGENT_PLUGINS` names a marketplace rendered for your server (`owner/repo`, or an absolute path; below). |
| Antigravity CLI plugin | The installer, unless `WAKESHIFT_ANTIGRAVITY_PLUGIN` names a plugin rendered for your server (a GitHub URL or an absolute path). |
| opencode, pi | Their npm packages take an `issuers` option in their own settings (for opencode, `["@wakeshift/opencode@<version>", { "issuers": ["https://wakeshift.example.com"] }]` in `opencode.json`). Your server's offers use the installer unless `WAKESHIFT_OPENCODE_PLUGIN` or `WAKESHIFT_PI_PLUGIN` names a version or dist-tag of a package that trusts it. |
| OpenClaw | Your server's offer sets the plugin's issuer before installing it (`plugins.entries.wakeshift.config.issuer`). `WAKESHIFT_OPENCLAW_PLUGIN` picks the npm version, or `off` for the installer. |
| Hermes Agent | The plugin's `issuers` setting in Hermes's `config.yaml` (it replaces the default, so list every server it should trust). |
| Cursor | `WAKESHIFT_CONNECT_PACKAGE` names a version or dist-tag of `@wakeshift/connect` for your server, or the installer is offered. |

### The connector and installs

The image carries no connector binaries, so out of the box `/download/` serves nothing, `/install/<harness>.sh` serves a script that only says the connector isn't available, and only the agents that need no install are woken: Claude Code through its Monitor, Cursor, Copilot CLI and Antigravity CLI through their background commands, and MCP Events clients. To serve installs, either:

- **Build the connector** with Go 1.27 for each platform into a directory, such as `bin/connector/` (`bin/` is gitignored), and point `WAKESHIFT_CONNECTOR_DIR` at it:

  ```sh
  for p in darwin/arm64 darwin/amd64 linux/arm64 linux/amd64; do
    CGO_ENABLED=0 GOOS=${p%/*} GOARCH=${p#*/} go build -trimpath -o bin/connector/wakeshift-${p%/*}-${p#*/} ./cmd/wakeshift-connector
  done
  WAKESHIFT_CONNECTOR_DIR=$PWD/bin/connector make run
  ```

  With compose, uncomment the `./bin/connector` volume in [docker-compose.yml](../docker-compose.yml) and start with `WAKESHIFT_CONNECTOR_DIR=/connector docker compose up --build`.

  The server then serves those binaries, and install scripts that pin your `WAKESHIFT_PUBLIC_URL` as the only issuer. OpenClaw's script also needs its plugin packed into the same directory; its npm package (`WAKESHIFT_OPENCLAW_PLUGIN`) doesn't.
- **Or set `WAKESHIFT_INSTALLER_URL`** to a release of the installers built for your server (https, no query). The installer releases published at github.com/wakeshift/connectors are built for the hosted service and trust only it, so don't point a self-hosted server at them.

**Your own plugin marketplace.** To have Claude Code, Codex and Copilot CLI install the plugin with their own commands, render the marketplace for your server into a clone of [wakeshift/agent-plugins](https://github.com/wakeshift/agent-plugins), which keeps each plugin's pinned, checksummed connector from the signed releases:

```sh
go run ./cmd/wakeshift-agent-plugins -dir <your clone of wakeshift/agent-plugins> -issuer https://wakeshift.example.com
```

On one machine, add the rendered clone directly with `claude plugin marketplace add /absolute/path/to/agent-plugins`. Use `http://localhost:8370` as the issuer for the README’s Compose setup. To distribute the marketplace, push the result to a repository of your own and set `WAKESHIFT_AGENT_PLUGINS=<owner>/<repo>`. With `-plugin antigravity`, it renders Antigravity CLI's plugin the same way, for `WAKESHIFT_ANTIGRAVITY_PLUGIN`.

## Console links

- `WAKESHIFT_SITE_URL`: your public site. The console links its Docs to `$WAKESHIFT_SITE_URL/docs` (with no site, to wakeshift.ai's), and the server redirects `/llms.txt`, `/llms-full.txt` and `/AGENTS.md` there. Unset, it's `WAKESHIFT_PUBLIC_URL` without a leading `app.`, or none.
- `WAKESHIFT_TERMS_URL` and `WAKESHIFT_TERMS_VERSION` (both or neither), and `WAKESHIFT_PRIVACY_URL` (only with the Terms): your own Terms of Service and privacy policy. The version is any short label, such as the Terms' date. Only with them does sign-in say "By signing in, you agree to…" and record which version each new account accepted.
- `WAKESHIFT_SUPPORT_EMAIL`: where app owners are told to write about a suspension or a limit, in API errors and the console, and the contact in the server's `/.well-known/security.txt`, beside this repository's security advisories (default `support@wakeshift.ai`: set your own).

## Behind a proxy or load balancer

Terminate TLS in front of the server and pass requests through unbuffered: stream links, long polls and `events/stream` hold a request open for up to an hour, with a write at least every 45 seconds. Keep the proxy's idle timeout above 60 seconds and its request timeout above an hour, or the client reconnects more often (nothing is lost; it costs a reconnect).

- `WAKESHIFT_PROXY_HOPS`: how many trusted proxies append to `X-Forwarded-For` in front of the server, for limits per client address (default 0: the connection's address).
- `WAKESHIFT_MAX_STREAMS`: the most streams and long polls one instance holds at once (default 900; `0`: no cap). Past it, new ones get 503 with `Retry-After`, and one app holding a quarter of it gets another only while more than a quarter is free. Set it below your platform's per-instance request limit and the process's open-file limit.

### Keep credentials out of access logs

Some URLs carry credentials. The server's own request log already replaces them with placeholders, and never logs query strings, but a proxy's or load balancer's access log records the full URL. Exclude these from it:

| URL contains | What's in it |
| --- | --- |
| `/v1/stream` | Stream links (`link=` in the query) and the waits, polls, setup and claims made with them |
| `ticket=` | One-time connect tickets |
| `/connect/link` | Connect links |
| `/callback?` | OAuth authorization codes |
| `invite` | Organization and waitlist invite codes |
| `/v1/publish/` | Publish URLs: the path is the credential |

Check the `Referer` field too: a page opened from such a URL sends it there. The hosted service drops requests matching these from its platform's request logs, and keeps no load-balancer log entry whose URL or `Referer` has a query string or one of these paths.

## Push

MCP Events push (`events/stream`) is on by default only when `WAKESHIFT_PUBLIC_URL` is on `localhost`. On any other host, set `WAKESHIFT_MCP_EVENTS_PUSH=1`. The SDKs read the setting from the server and offer push in `events/list` only while it's on; `0` turns it off, and `events/stream` then answers `Unsupported`. Each open push stream is one request held through your proxy (above).

## Other settings

| Setting | Default | What it does |
| --- | --- | --- |
| `WAKESHIFT_ADDR` | `:$PORT`, else `:8080` (the image sets `:8370`) | Listen address |
| `WAKESHIFT_LOG_FORMAT` | `json` | `json` or `text` |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | unset | Export traces and logs with OpenTelemetry. Every span and log record is scrubbed of credentials, IDs, emails and client text first. `OTEL_LOGS_EXPORTER=none` exports no logs; `WAKESHIFT_OTLP_LOG_LEVEL` sets the lowest level exported |
| `WAKESHIFT_SUBSCRIBE_VOUCHERS` | on only on `localhost` | `1` hands native MCP Events clients one-time vouchers for events only an app's tools may subscribe to ([mcp-events.md](mcp-events.md)); `0` refuses them |
| `WAKESHIFT_RESEND_KEY`, `WAKESHIFT_MAIL_FROM` | unset | Send organization invites by email through Resend. Unset, the console shows invite links to copy |
| `WAKESHIFT_CONNECTOR_MIN_VERSIONS` | none | The oldest connector versions served; older ones are asked to update |
| `WAKESHIFT_REDEEM_BY_TOOL_CALL` | off | `1` lets Claude Code's hooks connect a conversation by the tool call's ID |
| `WAKESHIFT_CLAUDE_SESSION_STEPS` | `inline` | `deferred` gives Claude Code's install offer the session-only steps through a one-time URL |
| `WAKESHIFT_CODEX_ELICIT` | `off` | `all`, or app IDs: Codex's install offer as a form in Codex's own dialog |
| `WAKESHIFT_GROK_WEBHOOKS` | on only on `localhost` | `1` wakes Grok Bot through its own Automations webhook (`WAKESHIFT_GROK_*` tune it) |

The admin API settings (`WAKESHIFT_ADMIN_*`) are for the hosted service's internal admin service, behind Google's Identity-Aware Proxy. Leave them unset: the API is then off. `WAKESHIFT_PROFILER` sends profiles to Google Cloud Profiler and needs a build with the `profiler` tag.

## Webhooks

Webhooks (`apps/try`, a Node app on the TypeScript SDK) gives each visitor a link: an MCP URL for their agent and a webhook URL, and each POST to the webhook URL wakes the conversations watching it. The [README](../README.md#try-it-with-your-agent) runs it with `docker compose --profile webhooks up --build`, after `sh deploy/webhooks-env.sh`, which writes `.env` (readable by you alone; never commit it) with a fresh `WAKESHIFT_SECRET_KEY`, a key for the app `webhooks` and its `WAKESHIFT_APPS` entry, and a secret for its links. docker compose reads `.env` by itself, but a variable set in your shell wins: if you register your own apps in `WAKESHIFT_APPS`, add the `webhooks` entry from `.env` to your list. A database volume made before the profile existed lacks Webhooks' database: create it as [deploy/postgres-init.sql](../deploy/postgres-init.sql) does, or start over with `docker compose down -v`.

Its settings, in its environment (docker-compose.yml sets them for one machine):

| Setting | What it is |
| --- | --- |
| `WAKESHIFT_KEY` | The app's key (`WEBHOOKS_KEY` in `.env`), registered with the server in `WAKESHIFT_APPS`. |
| `WAKESHIFT_CORE` | The server's public URL (its `WAKESHIFT_PUBLIC_URL`), which the app's signed requests are addressed to. **Unset, it is `https://app.wakeshift.ai`, Wakeshift's hosted service:** the app's key and every webhook body would go there. Always set it to your server. |
| `TRY_CORE_ADDR` | Where requests to the server go when that isn't `WAKESHIFT_CORE` (in compose, `http://server:8370`). |
| `TRY_SECRET` | Signs every link: at least 32 random bytes (`WEBHOOKS_SECRET` in `.env`). The app refuses a shorter one. Changing it ends every link. |
| `TRY_DATABASE_URL` | Its own Postgres database, shared by every instance. |
| `TRY_MIGRATE_DATABASE_URL` | Optional: the database's owner, which migrates it at startup, so that `TRY_DATABASE_URL`'s role only reads and writes (compose's two roles are in deploy/postgres-init.sql). Unset, `TRY_DATABASE_URL`'s role migrates. |
| `PUBLIC_URL` | Its public URL (default `http://localhost:$PORT`): the links, webhook URLs and MCP URLs it hands out. |
| `PORT`, `TRY_HOST` | Where it listens: port 8380, on every interface unless `TRY_HOST` names one (`127.0.0.1` for this machine only). Compose listens on every interface inside the container and publishes the port on `127.0.0.1` only. |
| `TRY_PROXY_HOPS` | How many proxies in front append to `X-Forwarded-For` (default 0: the connection's address). Posts and new links are limited per client address, so set it exactly. Left at 0 behind a proxy, every visitor shares the proxy's address and its limits. Set above 0 while the port is also reachable directly, anyone can choose their address with a forged `X-Forwarded-For`. |
| `TRY_PER_HOUR`, `TRY_PER_DAY`, `TRY_GLOBAL_PER_HOUR`, `TRY_ADDRESS_PER_HOUR`, `TRY_LINKS_PER_HOUR` | Limits: posts an hour and a day per webhook URL (60, 500), posts an hour across all webhooks (20,000), posts an hour from one address (1,000), new links an hour per address (30). |
| `WAKESHIFT_POSTHOG_KEY`, `WAKESHIFT_POSTHOG_HOST` | Optional page analytics with PostHog. Unset (the default), it loads none. |

It serves no terms or privacy policy of its own: if you offer it to others, link yours from your own site. Anyone with a link can read that webhook's posts, and anyone with a webhook URL can post to it.

Link codes and webhook tokens are in URL paths: `/l/…` (a link's page), `/t/…` (its MCP URL) and `/in/…` (its webhook URLs). A reverse proxy in front of Webhooks should leave those paths out of its access logs, like the server's [credential URLs](#keep-credentials-out-of-access-logs); the app's own log never records a path.

## Moving an app between servers

An app's subscriptions, consents and connected conversations live on the server that made them. To move an app to your own server, register it there (a new key), point its `WAKESHIFT_CORE` at it, and have its users' agents subscribe again: their bridges trust one server each, so each one installs again from yours.
