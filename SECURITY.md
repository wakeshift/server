# Security

Report a vulnerability privately, not in a public issue or pull request:

- with GitHub's private vulnerability reporting: this repository's **Security** tab, **Report a vulnerability**; or
- by email to support@wakeshift.ai, with "Security" in the subject (the contact in Wakeshift's [security.txt](https://wakeshift.ai/.well-known/security.txt)).

Say what's affected (the commit or release, and whether it's a self-hosted install or the hosted service), how to reproduce it, step by step, and what an attacker could do with it.

## What to expect

We respond on a best-effort basis. We aim to confirm we got your report within a few business days, tell you what we found, and keep you told while we fix it. We credit you when the fix ships unless you'd rather we didn't. Please give us a reasonable time to fix the issue before you disclose it.

## Scope

- **This server's code**, including self-hosted installs: tenant isolation, consent, sign-in, tokens and keys, beacons and stream links, MCP Events delivery, and anything that lets one agent wake or act as another.
- **The hosted service** at `wakeshift.ai` and its subdomains: the policy in [Wakeshift's security policy](https://wakeshift.ai/.well-known/security.txt) applies, and only test against your own accounts and workspaces.
- **The connector, plugins and SDKs in this repository** (`cmd/wakeshift-connector`, `plugins/`, `sdk/`): report them here, the same way. They're also published in their own repositories ([CONTRIBUTING](CONTRIBUTING.md) lists them), which take reports the same way; one report, to either, is enough.
- **The container image** (`ghcr.io/wakeshift/server`) and this repository's build and CI.

Not in scope: denial of service, volumetric or automated scanning, social engineering, physical attacks, reports from automated tools without a demonstrated impact, and problems that need a self-hosted install to be misconfigured against [docs/self-hosting.md](docs/self-hosting.md) (development sign-in on a public host, a superuser database role, credential-bearing URLs in a proxy's logs).

## Self-hosted installs

Fixes land on `main` and in the next release. We fix the latest code; we don't backport to older commits. If you run your own server, watch this repository's releases and security advisories, and update promptly.

We won't take legal action against research done in good faith under this policy: test only against your own installs, accounts and workspaces, don't read, change or keep other people's data (stop and tell us if you reach any), and don't degrade anyone's service.
