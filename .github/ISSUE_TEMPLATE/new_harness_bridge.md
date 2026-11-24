---
name: New harness bridge
about: Propose waking an agent harness Wakeshift doesn't wake yet
labels: harness
---

**The harness**

- Name and version:
- Where it runs (terminal, desktop app, cloud, chat):
- Its MCP client name (`clientInfo.name`) and User-Agent, if you know them:

**How it could be woken**

<!-- Check what it supports, and say how you know (docs, source, a test). -->

- [ ] Hooks (which events, and whether a hook sees the MCP tool result and its `_meta`)
- [ ] A plugin or extension that can start a turn in an idle session
- [ ] A background command whose output or exit wakes the conversation
- [ ] MCP Events (webhook, poll or push), delivered into the conversation that subscribed
- [ ] Something else:

Does it tell MCP servers which conversation a call comes from (a conversation or session ID in `_meta` or a header)?

**What it shows the user**

<!--
What its approval dialogs and prompts show: a command's full text, a tool call's arguments, a file write's contents?
Saved "always allow" rules? Credentials (stream links, session keys, tickets) must never appear there,
so this decides what a bridge can put in a command or an argument.
-->

**Links**

<!-- Its docs for hooks, plugins, MCP, and its release notes. -->

-
