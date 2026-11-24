# Wakeshift docs

Agents subscribe to your app's events, your backend emits them and Wakeshift wakes each conversation that subscribed. When a job finishes, a person replies or something changes in your app, the agent that asked wakes up with it, whether it's Claude Code, Codex, ChatGPT's Work chats, Cursor or another agent.

Your events follow MCP Events, the protocol's standard for this: clients that speak it subscribe natively, and Wakeshift wakes the agents that can't subscribe to MCP Events yet, so your code is the same for all of them.

## Start here

**Let your coding agent add it.** Paste this into Claude Code, Codex or Cursor, in your MCP server's repository. It sets up your app (you sign in once), suggests events from your server's own tools, adds the ones you pick and checks that an agent wakes.

```text Prompt for your coding agent
Add Wakeshift to this MCP server following {{SITE}}/AGENTS.md
```

**Add it by hand.** The [quickstart](/docs/quickstart) adds an event to your MCP server and wakes Claude Code with it, in TypeScript, Python or Go: about twenty lines on a server you already have.

**See a wake first.** [Try a wake without an account]({{TRY}}): a webhook URL that wakes your own agent, in two minutes. [Try it](/docs/try) has the steps.

## What you write

You define the events your app offers, with typed arguments and payloads. The tool that starts the work subscribes the conversation calling it, your backend emits when the work is done, and Wakeshift wakes that conversation in whatever program it runs in. [How it works](/docs/how-it-works) shows each step.

```ts tab:Define and attach
const crawlFinished = wake.defineEvent("crawl.finished", {
  description: "A crawl finished",
  args: z.object({ job: z.string() }),
  payload: z.object({ status: z.string(), pages: z.number() }),
})
const attached = wake.attach(server)
```

```ts tab:Subscribe from your tool
attached.registerTool(
  "start_crawl",
  { description: "Crawl a site.", inputSchema: z.object({ url: z.string() }), subscribes: [crawlFinished] },
  async ({ url }, ctx, call) => {
    const job = crypto.randomUUID()
    await call.subscribe(crawlFinished, { job }, { once: true })
    startCrawl(url, job)
    return `Crawl ${job} started.`
  },
)
```

```ts tab:Emit
await crawlFinished.emit({ job: "3f2a" }, { status: "completed", pages: 42 })
```

Wakeshift is open source under Apache-2.0, [the server](https://github.com/wakeshift/server) included. Use the [TypeScript](/docs/sdk) (npm), [Go](/docs/go) or [Python](/docs/python) (PyPI) SDK. Any other language works over the [HTTP API](/docs/other-languages). For the hosted service, [sign up]({{APP}}) with Google or GitHub. To run it yourself, follow the [self-hosting guide](https://github.com/wakeshift/server/blob/main/docs/self-hosting.md).

Weighing it against MCP Events toolkits or an agent's built-in wakes? [How Wakeshift compares](/compare) says where each stops, and the [FAQ](/faq) answers the common questions.
