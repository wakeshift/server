# Quickstart

Build a crawler whose `start_crawl` tool wakes the conversation that started a crawl when the crawl finishes. Your server needs no sign-in for its users, and on an MCP server you already have it's about twenty lines. For the parts involved and how a wake flows between them, see [How it works](/docs/how-it-works).

**You need:**

- For your server: Node 22.12 or later, Python 3.11 or later, or Go 1.25 or later.
- For step 1's `init`: Node 22 or later, whatever your server's language. Step 1 has a way without Node.
- Claude Code, to watch the wake.

> **Running Wakeshift yourself?** This page uses the hosted service. Follow the [self-hosting guide](https://github.com/wakeshift/server/blob/main/docs/self-hosting.md) to start the server and create your app's key, then set `WAKESHIFT_CORE` to its URL in `.env`. The rest of this page is the same.

> **Using a coding agent?** Paste the prompt below into Claude Code, Codex or another coding agent, in your MCP server's repository. It sets up your app (you sign in once), proposes events from your server's own tools for you to pick, adds them, and checks that an agent really wakes. It follows [AGENTS.md](/AGENTS.md); [llms.txt](/llms.txt) links every docs page as Markdown. The rest of this page is the same thing by hand.

```text Prompt for your coding agent
Add Wakeshift to this MCP server following {{SITE}}/AGENTS.md
```

## 1. Create your app

In your server's directory, run:

```sh
npx -y @wakeshift/cli@0.1.4 init{{CLI_ORIGIN}}
```

It makes your app's key on your computer, writes it to `.env` as `WAKESHIFT_KEY` (and adds `.env` to `.gitignore` if it isn't there), then opens a link to the console. Sign in with Google or GitHub, check that the page shows the same key code as your terminal, and press **Create app**. The app's ID ends in `-dev`: you test with a separate app from the one your users get.

The key is your app's private key. It never leaves your server: the SDK signs each request with it, and Wakeshift only ever has its public half. `init` needs Node 22 or later; without Node, sign in to the [console]({{APP}}/?ref=quickstart), choose **New app**, and put the key it shows once in `.env`:

```sh .env
WAKESHIFT_KEY=wst_…{{CORE_ENV}}
```

## 2. Install the SDK

```sh tab:TypeScript
npm install @wakeshift/sdk @modelcontextprotocol/server @modelcontextprotocol/node zod
npm pkg set type=module
```

```sh tab:Python
pip install 'wakeshift[mcp]' 'uvicorn[standard]'
```

```sh tab:Go
go get github.com/wakeshift/wakeshift-go@latest github.com/modelcontextprotocol/go-sdk@latest
```

On Node 22.12 to 22.17, add `--experimental-strip-types` to step 4's `node` command, which runs the `.ts` files directly. On another MCP framework, check [MCP frameworks](/docs/frameworks).

## 3. Add the code

The event, the tool that subscribes the conversation calling it, and the emit for when the work is done:

```ts tab:TypeScript
import { McpServer } from "@modelcontextprotocol/server"
import { z } from "zod"
import { Wakeshift } from "@wakeshift/sdk"

export const wake = new Wakeshift() // reads WAKESHIFT_KEY; without one, Wakeshift is off and the server runs as before

const crawlFinished = wake.defineEvent("crawl.finished", {
  description: "A crawl finished",
  args: z.object({ job: z.string() }),
  payload: z.object({ status: z.string(), pages: z.number() }),
})

// Your MCP server, made fresh for each request. Serving it is step 4's job, so tests can import this module.
export function makeServer() {
  const server = new McpServer({ name: "crawler", version: "1.0.0" })
  const attached = wake.attach(server)
  attached.registerTool(
    "start_crawl",
    { description: "Crawl a site.", inputSchema: z.object({ url: z.string() }), subscribes: [crawlFinished] },
    async ({ url }, ctx, call) => {
      const job = crypto.randomUUID() // your tool minted the ID, so only this conversation can know it
      await call.subscribe(crawlFinished, { job }, { once: true })
      startCrawl(url, job)
      return `Crawl ${job} started.`
    },
  )
  return server
}

// When the crawl finishes, in your worker or webhook handler:
export async function onCrawlFinished(job: string, pages: number) {
  await crawlFinished.emit({ job }, { status: "completed", pages })
}
```

```python tab:Python
from uuid import uuid4

from mcp.server.mcpserver import MCPServer
from pydantic import BaseModel

from wakeshift import ToolCall, Wakeshift

wake = Wakeshift()  # reads WAKESHIFT_KEY; without one, Wakeshift is off


class Job(BaseModel):
    job: str


class Crawl(BaseModel):
    status: str
    pages: int


crawl_finished = wake.define_event("crawl.finished", description="A crawl finished", args=Job, payload=Crawl)

server = MCPServer("crawler")
attached = wake.attach(server)


def start(url: str, job: str) -> None:
    """Starts the crawl (your own code)."""


@attached.tool(subscribes=[crawl_finished])
async def start_crawl(url: str, call: ToolCall) -> str:
    """Crawl a site."""
    job = str(uuid4())  # your tool minted the ID, so only this conversation can know it
    await call.subscribe(crawl_finished, {"job": job}, once=True)
    start(url, job)
    return f"Crawl {job} started."


app = server.streamable_http_app()  # run with uvicorn


# When the crawl finishes, in your worker or webhook handler:
async def on_crawl_finished(job: str, pages: int) -> None:
    await crawl_finished.emit(Job(job=job), Crawl(status="completed", pages=pages))
```

```go tab:Go
wake, err := wakeshift.New() // reads WAKESHIFT_KEY; without one, Wakeshift is off
if err != nil {
	log.Fatal(err) // a malformed key
}

type jobArgs struct {
	Job string `json:"job"`
}
type crawl struct {
	Status string `json:"status"`
	Pages  int    `json:"pages"`
}

crawlFinished, err := wakeshift.DefineEvent(wake, "crawl.finished", wakeshift.EventDef[jobArgs, crawl]{
	Description: "A crawl finished",
})
if err != nil {
	log.Fatal(err)
}

srv := mcp.NewServer(&mcp.Implementation{Name: "crawler", Version: "1.0.0"}, nil)
attached := wake.Attach(srv, wakeshift.AttachOptions{})

type startIn struct {
	URL string `json:"url"`
}
type startOut struct {
	Job string `json:"job"`
}

var startCrawl func(url, job string) // your crawl: step 4 below gives a stand-in

wakeshift.AddTool(attached, &mcp.Tool{Name: "start_crawl", Description: "Crawl a site."},
	func(ctx context.Context, call *wakeshift.ToolCall, in startIn) (*mcp.CallToolResult, startOut, error) {
		job := strings.ToLower(rand.Text()) // your tool minted the ID, so only this conversation can know it
		if _, err := crawlFinished.Subscribe(ctx, call, jobArgs{Job: job}, wakeshift.Once()); err != nil {
			return nil, startOut{}, err
		}
		go startCrawl(in.URL, job)
		return nil, startOut{Job: job}, nil
	}, wakeshift.Subscribes(crawlFinished))

h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil)
http.Handle("/mcp", attached.Handler(h))

// When the crawl finishes, in your worker or webhook handler:
onCrawlFinished := func(ctx context.Context, job string, pages int) error {
	_, err := crawlFinished.Emit(ctx, jobArgs{Job: job}, crawl{Status: "completed", Pages: pages})
	return err
}
```

That's the whole integration. `startCrawl` (Python: `start`) is your own code. The Go code runs in your `main`, importing `context`, `crypto/rand`, `log`, `net/http`, `strings`, `time`, `github.com/modelcontextprotocol/go-sdk/mcp` and `github.com/wakeshift/wakeshift-go`. Argument values are lowercase `a-z 0-9 . _ -`, so it lowercases `rand.Text()`'s base32 ID; for an ID your own systems mint, use `wakeshift.EncodeArgValue` ([Argument values](/docs/events#argument-values)).

## 4. Run it

For a first try, swap your crawl for one that finishes ten seconds after it starts, then serve the server on port 3000:

```ts tab:TypeScript
// In server.ts, a stand-in crawl: it finishes ten seconds after it starts.
function startCrawl(url: string, job: string) {
  setTimeout(() => void onCrawlFinished(job, 42), 10_000)
}

// main.ts serves it: the only file that listens.
import { createServer } from "node:http"
import { createMcpHandler } from "@modelcontextprotocol/server"
import { toNodeHandler } from "@modelcontextprotocol/node"
import { makeServer } from "./server.ts"

createServer(toNodeHandler(createMcpHandler(makeServer))).listen(3000)
```

```python tab:Python
import asyncio


def start(url: str, job: str) -> None:
    """A stand-in crawl: it finishes ten seconds after it starts."""
    asyncio.get_running_loop().call_later(10, lambda: asyncio.ensure_future(on_crawl_finished(job, 42)))
```

```go tab:Go
// A stand-in crawl: it finishes ten seconds after it starts.
startCrawl = func(url, job string) {
	time.Sleep(10 * time.Second)
	if err := onCrawlFinished(context.Background(), job, 42); err != nil {
		log.Print(err)
	}
}
log.Fatal(http.ListenAndServe(":3000", nil))
```

Start it: `node --env-file=.env main.ts`, `uvicorn server:app --port 3000 --env-file .env`, or `go run .` with the key exported.

## 5. Ask Claude Code for a crawl

Claude Code needs no install to try this.

```sh
claude mcp add --transport http crawler http://localhost:3000/mcp
```

```text Prompt
Crawl https://example.com with start_crawl, and tell me when it's done.
```

Claude Code calls `start_crawl`, then asks whether to install Wakeshift's plugin for Claude Code, showing the exact command. Say yes, type `/reload-plugins` and send any message: from then on it wakes within seconds of each occurrence, even while it's idle, with nothing more to approve. If you say no, it starts a background monitor for this session instead (in manual permission mode, Claude Code asks you to approve it).

## 6. Watch it wake

Ten seconds after the crawl starts, `onCrawlFinished` emits `crawl.finished`, and Claude Code wakes with `A crawl finished (job …)` and the payload, without being asked.

## 7. Check it as your coding agent would

With the server still running, `verify` plays an agent: it calls `start_crawl`, checks that the conversation subscribed, emits a test `crawl.finished`, waits for the wake and cleans up. Each check it fails says what to fix.

```sh
npx -y @wakeshift/cli@0.1.4 verify --url http://localhost:3000/mcp --tool start_crawl --args '{"url":"https://example.com"}'
```

It runs against your `-dev` app, and emits only when nobody else is subscribed with the same arguments, so it never wakes your users.

## What just happened

- **The key comes from `WAKESHIFT_KEY`.** Without one, Wakeshift is off: every call still works, nothing is sent, and your server runs as it would without Wakeshift. Creating the client makes no network request.
- **One call registers the tool and names the events it subscribes to.** It's your MCP SDK's own registration (`registerTool`, `@tool`, `AddTool`) with `subscribes` added and `call` passed to your handler. Your input schema stays yours: Wakeshift adds its arguments in `tools/list` and takes them out before your code runs, and adds what the agent needs to be woken to your tool's reply.
- **Only your tools can subscribe to `crawl.finished`**, because it declares neither `personal`, `authorize` nor `public`. Your tool minted the job ID, so there's nothing to check. A tool that subscribes to something the caller names, such as a repo, checks the caller can see it first.
- **`once` ends the subscription after one delivery**, and a call that fails ends what it subscribed.
- **`emit` wakes every conversation subscribed with those arguments.** The event has no `text`, so the agent reads its description and arguments.

## On your own server

Three questions map the crawl onto your server:

1. **What would an agent otherwise wait for?** Long work finishing, a change someone else makes, something arriving. That's your event: name it for what happened (`build.finished`), with the arguments a subscriber picks (a job ID, a repo) and the outcome in its payload (`status`: passed, failed or canceled) rather than a second event for failures.
2. **Which tool starts it?** Register that tool through the attachment, with the event in `subscribes`, and subscribe before starting the work. If no tool starts it, add a `watch_…` tool that subscribes.
3. **Where does it finish?** Emit there, once the change is saved, with your own ID for it as the emit's `id`, so a retry emits once.

Then run `verify` on that tool. When the caller picks the arguments (a repo, a project), put `wsverify-` and a random suffix in one of them, and when your server signs people in, pass a test user's token with `--header-env` ([the CLI](/docs/sdk#the-cli)).

## Serving ChatGPT

ChatGPT subscribes to `crawl.finished` itself, with the voucher `start_crawl`'s result hands it ([how](/docs/who-may-subscribe#chatgpt-and-other-clients-that-subscribe-natively)). It connects to a remote server over HTTPS, never to a local or stdio one, and sends each request on MCP protocol 2026-07-28, without a session:

- **TypeScript:** `createMcpHandler` serves it as written.
- **Python:** `server.streamable_http_app()` serves it as written, and so does `streamable_http_app(stateless_http=True)`, for a server behind a load balancer ([Python](/docs/python)).
- **Go:** go-sdk's handler keeps sessions by default and refuses 2026-07-28 requests, so serve without them: `mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, &mcp.StreamableHTTPOptions{Stateless: true})`, through `attached.Handler` as before. A server you build for each request takes one `wake.Handler(h, opts)` around the handler instead ([Attach](/docs/go#attach)).

## Next steps

- **Sign people in, and use `personal`.** Tie subscriptions to your own users with `identify`, and send each user their own occurrences: [Events](/docs/events#personal-events) and [Your users](/docs/users#using-your-own-sign-in).
- **Wake from a webhook.** Point GitHub, Stripe or any other sender at a URL that emits your event: [Publish URLs](/docs/publish-urls).
- **Decide who may subscribe.** Open an event to agents beyond your own tools, safely: [Who may subscribe](/docs/who-may-subscribe).
