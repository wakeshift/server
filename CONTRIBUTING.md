# Contributing

We read every issue. Report bugs, ask questions and suggest changes in this repository's issues. If an agent didn't wake, say which agent and version, which SDK and version, and what the agent's tool result said.

Pull requests are welcome; we don't guarantee we'll accept every one.

## How a pull request lands

For now, this repository is published from Wakeshift's main repository. An accepted pull request is applied there, keeping you as the commit's author, and comes back here with the next publish, usually within a day. We close the pull request here with a link to the commit. Each publish says which commit it came from (`.source-commit`).

## Before you open one

- Run `make lint` and `make test` (Postgres in Docker: `make db`; [AGENTS.md](AGENTS.md) has the details). Add tests for what you change; changes to tenant isolation, consent, tokens, beacons or stream links need them.
- Keep the change focused, and say what it fixes or adds, and why. For a client that doesn't behave as the spec says, link what shows it: its source, a captured request, its docs.
- Don't include keys, tokens or other secrets, including in tests.
- Change only the code: files the publish generates (`go.mod`, `go.sum`, `LICENSE`, `.source-commit`) are updated when it's published.

## Sign your commits off

Every commit must be signed off under the [Developer Certificate of Origin](https://developercertificate.org): by adding the line below, you certify that you wrote the change or otherwise have the right to submit it under this repository's license.

```text
Signed-off-by: Your Name <you@example.com>
```

`git commit -s` adds it, using your `user.name` and `user.email`. Use your real name. By contributing, you agree your contribution is licensed under the Apache License 2.0.

## Which repository a change belongs in

| Change | Repository |
| --- | --- |
| The server, the console, MCP Events delivery, install offers, migrations | Here |
| The connector (`wakeshift`), its hooks for each agent, the OpenClaw plugin, the npm packages for opencode, pi and Cursor, `wakeshift-cli` | [wakeshift/connectors](https://github.com/wakeshift/connectors). The same packages are here too, since the server builds the connector; a fix sent to either repository is applied the same way |
| The plugins for Claude Code, Codex, Copilot CLI and Cursor | [wakeshift/connectors](https://github.com/wakeshift/connectors) (`plugins/agent-plugins`); [wakeshift/agent-plugins](https://github.com/wakeshift/agent-plugins) is generated from it |
| The Antigravity CLI plugin | [wakeshift/antigravity-plugin](https://github.com/wakeshift/antigravity-plugin) |
| The Hermes Agent plugin | [wakeshift/hermes-plugin](https://github.com/wakeshift/hermes-plugin) |
| The SDKs | [wakeshift-node](https://github.com/wakeshift/wakeshift-node), [wakeshift-python](https://github.com/wakeshift/wakeshift-python), [wakeshift-go](https://github.com/wakeshift/wakeshift-go) |
| Agent Chat, Webhooks, the games, Slackline | [wakeshift/apps](https://github.com/wakeshift/apps) |

Not sure? Open it here, and we'll move it.

## Security

Report a security problem privately, not in an issue or pull request: see [SECURITY](SECURITY.md).
