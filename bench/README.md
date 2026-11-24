# wakebench

wakebench checks how Wakeshift wakes real agent harnesses, and lets you reproduce the results yourself. It builds the server and connector from this checkout and runs them on your machine as a local core with a database of its own. It registers a small toy app, then runs each harness you name in a throwaway HOME: Claude Code, Codex, Cursor CLI, opencode, Copilot CLI, Antigravity CLI and pi, plus Hermes Agent and OpenClaw in Docker. A tmux pane plays the user, every harness gets the same scripted scenarios, and each harness × scenario cell is graded. The grid, with a terminal snapshot at every step, goes to `/tmp/wakebench/<run-id>/report.html`, and the data to `results.json` next to it.

The harnesses are the real programs, logged in with your own accounts. A run uses your subscriptions or API keys, as any session would.

## Run it

```sh
bench/run --harness claude-code,codex                         # S1-S9 and S2i, the default
bench/run --harness codex --scenarios S6,S2i                  # one harness, some scenarios
bench/run --harness claude-code --scenarios S2                # one harness, one scenario
bench/run --harness claude-code,codex,cursor,opencode --parallel 2
bench/run --only-report 20261123-101500-ab12                  # render a run's report again from its results.json
```

`bench/run` builds what it needs first: the TypeScript SDK (`sdk/typescript`), the bench, and, for each run, the server and connector (`go build`). Each run makes its own database, `wakebench_<run>`, and drops it at the end. The core runs on a free port with dev sign-in on localhost.

The bench's own tests, and a dry run that needs no harness, no login and no core (a scripted fake agent against a stand-in core):

```sh
(cd sdk/typescript && npm ci --ignore-scripts && npm run build)
(cd bench && npm ci --ignore-scripts && npm test)
bench/run --harness fake --target fake --scenarios S1-S9,S6n,S18 --out-root "${TMPDIR:-/tmp}/wakebench-dry" --expect-pass
```

Options:

- `--scenarios`: a list or range, e.g. `S1,S5`, `S1-S9` (a range over S2 includes S2i), or `R1-R6` for the realistic tier.
- `--parallel N`: runs N harnesses at once, each with its own HOME, tmux session and topic.
- `--slow 1.5`: stretches every wait.
- `--approve-delay N`: answers each dialog N seconds after it appears, as a user who isn't watching would.
- `--keep-homes`: keeps the temp HOMEs for debugging. They hold borrowed logins (see Safety), so delete them afterwards.
- `--out-root <dir>`: writes the run somewhere other than `/tmp/wakebench`.

Environment:

- `WB_CLAUDE_BIN`, `WB_CODEX_BIN`, `WB_OPENCODE_BIN`, `WB_PI_BIN`: run a harness binary other than the one on your PATH.
- `bench/releases/install-harness.sh <harness> <version>`: installs the versions pinned in `harness-pins.json` into `~/.wakeshift-bench` (macOS arm64) and prints each binary's path. It never replaces your own install.
- `WB_CLAUDE_MODEL`: the model for Claude Code. The default is `claude-sonnet-5-5`; set `harness` to use the account's own default.
- `WB_CODEX_MODEL`, `WB_CURSOR_MODEL`, `WB_OPENCODE_MODEL`, `WB_PI_MODEL`: the same for the other harnesses.
- `WB_APP`: runs the toy app on another SDK and framework (see The toy app).

`bench/run --help` prints this section.

## What it needs

- **macOS**, where the bench is developed and run. Harnesses that keep their login in the macOS keychain need it, and `install-harness.sh` is macOS arm64 only.
- **Postgres** on `localhost:5439`, with the superuser `postgres` (password `postgres`) and the role `wakeshift`: the repository's development database (`make db`, in Docker).
- **Go** (the version in `go.mod`), **Node 26** and **tmux**. The toy app's Python ports also need **uv**.
- **Each harness you name, installed and logged in** with your own account. A harness that isn't installed, stops at its login prompt, never reaches its start screen or runs out of its model's quota mid-run (opencode's free models: "Free usage limit reached") is reported as blocked, never failed: the run says nothing about it.
  - **Claude Code.** For a login that carries into a temp HOME, run `claude setup-token` and save the token, mode 0600: `umask 077; pbpaste > ~/.wakeshift-bench/claude-token`. When Claude Code starts, the pane's shell reads the file into `CLAUDE_CODE_OAUTH_TOKEN`. The bench types only the file's path, never the token. Without the file, the bench borrows the keychain login (below).
  - **Codex**, logged in with ChatGPT or an API key.
  - **Cursor CLI** (`cursor-agent`), logged in, or with `CURSOR_API_KEY` in the bench's environment. The pane reads the key with echo off, so it never appears on a command line, in a file or on the screen.
  - **opencode 1.x.** Wakeshift's opencode plugin doesn't load on 2.x yet. Install the pin with `bench/releases/install-harness.sh opencode 1.19.5` and point `WB_OPENCODE_BIN` at it.
  - **opencode 2.4 and later** subscribe to MCP Events themselves, with no plugin: the realistic tier's push lane, R7–R9, checks that the agent subscribes and that the event wakes its open session. `install-harness.sh` installs 2.x too, and every 2.x lane runs its own opencode server (`--standalone`), never the shared background one: `WB_OPENCODE_BIN=$(bench/releases/install-harness.sh opencode 2.4.0) WB_OPENCODE_PUSH=1 WB_OPENCODE_PROTOCOL=auto bench/run --harness opencode --scenarios R7-R9`. 2.4 offers events only on protocol 2026-07-28, which `auto` tries first. Set `WB_OPENCODE_PROTOCOL` on 2.x: it writes the MCP server into the temp HOME's config, while `opencode mcp add` on 2.x writes the project's `opencode.json`, which the lane's isolation hides.
  - **Copilot CLI** (`copilot`) and **Antigravity CLI** (`agy`), logged in. For agy, the bench copies only the booleans of its first-run screens into the temp HOME (`~/.gemini/antigravity-cli/cache/onboarding.json`).
  - **pi** and the **Docker harnesses** (Hermes Agent, OpenClaw) need an OpenAI API key, in a file only you can read: `umask 077; printf 'OPENAI_API_KEY=%s\n' 'sk-…' > ~/.wakeshift-bench/env`. Hermes and OpenClaw also need their own state, which `bench/state.sh` makes with each harness's non-interactive onboarding: no conversations, channels, MCP servers or secrets. Every run works on a throwaway copy of that state.

## Safety

The harnesses are real agents and run as your user. The bench keeps them away from your own configuration. It isn't a sandbox for what the agents can reach, so run it where you'd run the harnesses yourself.

**What it borrows, read-only, and only for the run:**

- Your logins. The macOS login keychain is symlinked into the temp HOME; the keychain search list is never touched. Codex's `auth.json` is copied, never linked, because Codex rewrites it when it refreshes its token. Claude Code gets a copy of its account record and cached feature flags from `~/.claude.json`, which hold no tokens. The temp HOMEs, and the borrowed logins in them, are deleted when the run ends, unless you pass `--keep-homes`.
- Your Docker, for the bench's own containers (`wb-<run>-<lane>`). It never reads your own Hermes or OpenClaw state or containers.

**What it never touches:**

- **Your harnesses' configuration.**
  - Each harness starts with `env -i` and `HOME` set to `<run>/homes/<lane>`. Every config variable the harnesses read points into that HOME too: `CODEX_HOME`, `CLAUDE_CONFIG_DIR`, `XDG_CONFIG_HOME`, `HERMES_HOME`, `OPENCODE_CONFIG`, `COPILOT_HOME` and `PI_CODING_AGENT_DIR` (`configEnv` in `src/home.ts`). So a harness, a plugin or an installer can't reach your own config through a variable it sets or a default it derives.
  - opencode also looks for `.claude`, `.agents` and `AGENTS.md` in every directory above its working directory, whatever HOME says. Its lanes turn that off: `OPENCODE_DISABLE_PROJECT_CONFIG`, `OPENCODE_DISABLE_CLAUDE_CODE` and `OPENCODE_DISABLE_EXTERNAL_SKILLS` (`OPENCODE_ISOLATION` in `drivers/opencode`).
- **Your accounts' connected apps.** In each lane, Codex's `config.toml` turns off `apps`, `remote_plugin` and the browser and computer-use features. With a ChatGPT login, the apps connected to your account (mail, drive and others) would otherwise reach every lane as an MCP server. The bench also fails any lane whose harness could reach an MCP server other than the lane's own.
- **Your real config files.** They're hashed before the run, between harnesses and after (`src/safety.ts`), and only the hashes are kept:
  - Claude Code's settings and the keys of `~/.claude.json` a run could change;
  - Codex's hooks, config, auth and rules;
  - Cursor's hooks, MCP config and approvals;
  - Copilot CLI's config, MCP config, hooks and permissions;
  - opencode's config;
  - Antigravity's settings, hooks, plugins and MCP config;
  - pi's settings, MCP config and extensions.

  Any change aborts the run, is reported at the top of the report, and exits with code 3.
- **Your processes.** The bench runs tmux on its own socket (`tmux -L wb-<run>`). It kills only the processes it started or can tie to a lane: the PIDs it recorded (`<run>/pids.json`), a pane's descendants, and processes whose command line names the lane's HOME. Nothing is killed by name, pattern or port.
- **Your secrets, in what it writes.** Snapshots, logs, tool results and `results.json` are redacted before they're written (`src/redact.ts`). That covers stream and wait links, session keys, app keys, beacons and other JWSs, bearer tokens, a Claude Code token and the Cursor key, even where a terminal wrapped one over lines. Pane logs are redacted in place when the run ends. As a last check, every file the run wrote is scanned for anything still shaped like a secret and redacted again; the report says when that happened. The app keys and the toy app's control token live in 0600 files in the run directory and are deleted at the end.

## Scenarios

Each harness runs in two phases, each with a fresh HOME and conversation: **a**, with nothing installed, and **b**, where the user says yes to Wakeshift's install. The first prompt is the same everywhere. It tells the agent to call the toy app's `watch` tool, quote the instructions in the result, and do what they say (asking the user first where it says to). On each bench event, the agent calls `report_seen` and prints `GOT <code>`. Latencies run from the app's publish to the `GOT` line showing in the pane.

| | What it checks | Phase |
|---|---|---|
| S1 | `watch` called, its instructions quoted, and whether the agent offered the install up front | a |
| S2 | An idle conversation woken with nothing installed: 3 posts, p50 and max latency. "Only after install" for harnesses that need an install to wake (S2i covers them) | a |
| S2i | The same idle wakes through the install, right after S6 | b |
| S3 | A burst of 5: all seen, how many wakes, no duplicates | a or b |
| S4 | The waiter killed (found by PID), then a post 30s later: nothing lost, and presence clears or a waiter comes back by itself | a or b |
| S5 | The user says no to the install: nothing is installed, and a post still arrives where the harness wakes without one, or else is held for the conversation | a |
| S6 | The user says yes: the agent runs the install, relays the trust step, the conversation connects and a post wakes it; the uninstall leaves nothing behind | b |
| S7 | `unwatch` then `watch`, then a post: re-armed without a nudge | a or b |
| S8 | The harness quits, a post arrives while it's down, and it starts again on the same conversation: nothing lost or duplicated | b |
| S9 | The arguments Wakeshift reads: `harness`, `session_key`, and `conversation_id` where the agent must pass it | a and b |

More scenarios run when you ask for them by name. Their titles are in `src/model.ts`, and what each does is at its phase in `src/scenarios.ts`:

- S6n: the install with no nudge;
- S13: a long idle;
- S15: two apps in one conversation;
- S16 and S17: an unrecognized agent, and a tool with a side effect;
- S18: a new conversation after the install;
- S19: a plugin update under a connected conversation;
- S20: the off switch;
- S21: wakes in a row and across a restart.

The **realistic tier** (`R1`-`R10`, `src/realistic.ts`) runs apart from these. A real-looking CI app takes the toy app's place, and the user asks for real things without naming Wakeshift or a tool.

After each scenario the bench also counts how often each code reached the model, from the harness's own transcript (`src/arrivals.ts`), so a duplicate the agent reported once still shows. It also checks that no Wakeshift credential appeared in a dialog or a saved rule (`src/visible.ts`).

## Reading the report

`bench/run` prints a text grid and the paths of `report.html` and `results.json`. `report.html` is one static file that fetches nothing.

- **Rows are harnesses, grouped by tier; columns are scenarios.** Tiers and their bars live in `tiers.yaml`, the one place thresholds are set. Tier 1 (Claude Code, Codex) has the strictest bars; tier 2 has looser times, and tier 3 is graded on whether it works, plus a few bars for particular harnesses.
- **Each cell is a grade with its key number.**
  - **pass**: it worked and every bar was met.
  - **below bar**: it worked, but a bar was missed. Hover over the cell for which one.
  - **fail**: it didn't work, or it errored.
  - **blocked**: the harness couldn't run (no install, no login, account policy).
  - **n/a**: the scenario doesn't apply to the harness.

  S2 and S3 cells also split the median latency into the time to the waiter and the time from there to the `GOT` line.
- **Click a cell for its timeline.**
  - Every step, with its time and what the bench did or saw.
  - The step's data: latencies, delivery rows, the quote, the arguments of each call.
  - The terminal snapshot at that step. Every dialog the bench answered has a snapshot from before and after the answer.
  - The latency breakdown per event: publish accepted, delivery created, handed to a waiter, confirmed, the code in the pane, `report_seen`, `GOT` visible. The server's stages come from the local core's database.
- **`results.json`** has the same data: each harness's version and model, and per scenario its `grade`, `key`, `why`, `metrics`, `steps` and `notes`.
- **More files in the run directory:** `snapshots/<lane>/NNN-*.ansi` holds the raw (redacted) captures, `logs/<lane>.log` each pane's whole log, and `core/` the core's and the toy app's logs.
- **Exit codes:** 0 when the run finished, 3 when a real config changed, and 1 with `--expect-pass` when a cell didn't pass.

## The toy app (`app/`)

The toy app is an MCP server built on `@wakeshift/sdk`, as an app developer would build one. It has one event, `bench.event`, on the topic `bench/<run>/<lane>`, and three tools: `watch` subscribes, `unwatch` unsubscribes, and `report_seen` is how the agent says which codes it got. Each lane adds it as an MCP server, exactly as Wakeshift's agent picker tells users to (`apps/agentpicker/agents.json`, `src/picker.ts`). So a broken picker command fails the bench.

`WB_APP` runs the same app on each MCP server framework the SDKs support. They all serve one contract (`app/CONTRACT.md`):

- `ts-v2`, the default;
- `ts-v1` and `ts-mastra`;
- `go-sdk` and `go-mcpgo`;
- `py-mcpserver`, `py-fastmcp` and `py-lowlevel`.

Two more runners need no harness (build the bench first: `bench/run --help`, or `npx tsc` in `bench/`):

- `node bench/dist/src/conformance.js [--only go-sdk,…]` checks every variant against the contract on a real core.
- `node bench/dist/src/s10.js` runs a real MCP Events client (Mastra) through subscribe, native delivery and replay.

Both start a core with `go test -run TestSDKHarness ./internal/coretest/` on the development Postgres; `WAKESHIFT_TEST_DATABASE_URL` points them at another.

## MCP Events conformance

`node bench/dist/src/eventsconformance.js [--app ts-v2]` runs the official MCP Events (SEP-3415) server conformance suite, [modelcontextprotocol/conformance#521](https://github.com/modelcontextprotocol/conformance/pull/521), against the toy app on a local core. The suite runs from the commit `mcp-events/conformance.json` pins. Every check is compared with `mcp-events/conformance-expected.yaml`, where each expected failure says why:

- an item of Wakeshift's own spec-compliance plan;
- `bench`, a limit of this setup;
- `suite`, a row the suite can't grade against a server like this one.

The run fails on a failure that isn't listed, and on a listed one that now passes. `--results out.json` keeps every check.

## Adding a driver

Implement `Driver` (`drivers/types.ts`), usually by extending `TmuxDriver` (`drivers/base.ts`), in `drivers/<harness>/index.ts`. Then add it to `drivers/index.ts` and `tiers.yaml`:

- `check()`: is the harness installed and logged in?
- `prepare(lane)`: the temp HOME's config, the borrowed login, and the MCP server added with the harness's own command.
- `start(lane, {resume})` and `stop(lane)`.
- `answer(lane, screen, policy)`: recognizes and answers a dialog at the bottom of the screen.
- `busy`, `waiters`, `installed`, `afterInstall` (the trust step) and `uninstall`.
- `received(lane)` (optional): what the harness put in front of the model, from its own record of the conversation.

The fake driver (`drivers/fake`, with `--target fake`) is the reference for the whole flow.
