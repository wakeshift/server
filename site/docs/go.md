# Go SDK

`github.com/wakeshift/wakeshift-go`, for Go 1.25 or later, on the official MCP Go SDK (`github.com/modelcontextprotocol/go-sdk`, v1.8 or later) or on [mcp-go](#mcp-go) (`github.com/mark3labs/mcp-go`, v1.0 or later). It has the same calls as the [TypeScript](/docs/sdk) and [Python](/docs/python) SDKs, in Go's style: `ctx` first, functional options for calls, option structs for what you declare once, and `error` returns. [MCP frameworks](/docs/frameworks) lists the Go server frameworks it works with. Its source is at [github.com/wakeshift/wakeshift-go](https://github.com/wakeshift/wakeshift-go), under the Apache License 2.0.

```sh
go get github.com/wakeshift/wakeshift-go@latest
```

```go
import "github.com/wakeshift/wakeshift-go"
```

> **Using a coding agent?** Paste the prompt below, add [AGENTS.md](/AGENTS.md) to your repository, or point your agent at [llms.txt](/llms.txt), which links every docs page as Markdown.

```text Prompt for your coding agent
Add Wakeshift to this MCP server following {{SITE}}/AGENTS.md
```

The [quickstart](/docs/quickstart) has a whole server in each language. This page goes through each call.

## The client

```go
wake, err := wakeshift.New() // reads WAKESHIFT_KEY; without one, Wakeshift is off
if err != nil {
	log.Fatal(err) // a malformed key
}
```

`New` reads your app key (`wst_…`) from `WAKESHIFT_KEY` and Wakeshift's address from `WAKESHIFT_CORE` (default `https://app.wakeshift.ai`). It makes no network request: your app's ID is asked of Wakeshift the first time a call needs it.

| Option | What it does |
| --- | --- |
| `wakeshift.WithKey(k)` | The key, instead of `WAKESHIFT_KEY`. `WithKey("")` means no key; it never reads the environment. |
| `wakeshift.Required()` | A missing key is an error, instead of a disabled client: for a hosted server that must have one. |
| `wakeshift.WithCore(url)` | Wakeshift's address, instead of `WAKESHIFT_CORE`. `https://` only, except `localhost`, `127.0.0.1` and `::1`. |
| `wakeshift.WithHTTPClient(c)` | The `*http.Client` requests go through. |
| `wakeshift.WithLogger(l)` | Where the SDK logs: a `*slog.Logger`. Default: `slog.Default()`, read when each line is logged. |
| `wakeshift.WithMaxRetries(n)` | How many times a request the load balancer refused, or that couldn't connect, is sent again. Default 2. |
| `wakeshift.WithCallTimeouts(wakeshift.CallTimeouts{Subscribe: d, Block: d})` | How long a tool call waits on Wakeshift, with no retries: `Subscribe` for `Subscribe`, `Unsubscribe` and `ResolveSession` (default 5 seconds), `Block` for the addition to the tool's result (default 1 second). Past it, the request fails with `timeout`. After 5 failures in a row, these requests are skipped for 30 seconds (`unreachable` at once), so tool calls go on without Wakeshift. Emits and watches aren't affected. |

A malformed key, a core that isn't `https://`, or an unknown value is an error from `New`, whose message never contains any part of the key.

**Without a key, the client is disabled**, and logs that once at debug (`no_key`), so a server without a key prints nothing it didn't before. In tests, `wakeshift.New(wakeshift.WithKey(""))` is a disabled client even where `WAKESHIFT_KEY` is set, and `wakeshifttest.NewClient()` an enabled one that sends nothing ([test your integration](/docs/testing#unit-tests)). Nothing changes shape, so no call site needs a guard:

| On a disabled client | What happens |
| --- | --- |
| `DefineEvent` | Works, and checks the definition. |
| `wake.Attach`, `attached.Handler` | Add nothing: no built-in tools, no Wakeshift arguments, no MCP Events. `Handler(next)` returns `next`. `mcpgo.Attach` adds one tool middleware that passes every call straight through (a tool named in `Wrap` gets a disabled call), and nothing else. |
| `wakeshift.AddTool` | Registers your tool as you wrote it. `call.User` is `""`. |
| `event.Subscribe` | Returns a `Subscription` with state `SubscriptionDisabled`. |
| `event.Emit` | Checks the arguments and payload, sends nothing, and returns `Matched: 0`. |
| `CreatePublishURL`, `Status`, `StatusOf`, `RevokePublishURL`, `EventSubscriptions`, `ForgetUser`, `PublishManifest`, `AppID`, `Info` | Return a `*UsageError` with `CodeDisabled`. |

`wake.Enabled()` reports whether Wakeshift is on. `*Client`, `*Event` and `*Attachment` are safe for concurrent use. Create one `Client` per process.

## Events

Define each event once, at startup, before `Attach`. Its arguments and payload are Go types; their JSON Schemas come from the types (field names from `json` tags, descriptions from `jsonschema` tags).

```go
type repoArgs struct {
	Owner string `json:"owner" jsonschema:"The repo's owner: acme"`
	Repo  string `json:"repo" jsonschema:"The repo's name: api"`
}
type ciRun struct {
	Branch     string `json:"branch"`
	Conclusion string `json:"conclusion"`
	Name       string `json:"name"`
	URL        string `json:"url"`
}

ciFinished, err := wakeshift.DefineEvent(wake, "ci.finished", wakeshift.EventDef[repoArgs, ciRun]{
	Description: "CI finished on a GitHub repo",
	Text: func(r ciRun, a repoArgs) string {
		return "CI " + r.Conclusion + " on " + a.Owner + "/" + a.Repo + " " + r.Branch + ": " + r.Name
	},
})
```

| `EventDef` field | What it is |
| --- | --- |
| `Description` | Required. What happened, as a plain statement: "CI finished on a GitHub repo". Agents read it in the description of every tool that subscribes to the event. |
| `Text(payload, args)` | The line the agent reads first. Optional: without it, the agent reads the description and the arguments ("CI finished on a GitHub repo (owner acme, repo api)"). |
| `Personal` | Each subscription belongs to its subscriber, and each emit goes to the users you name ([personal events](/docs/events#personal-events)). |
| `Authorize(ctx, subscriber, args) (bool, error)` | Opens the event to `subscribe_events` and MCP Events clients, gated by your check ([below](#sign-in-identify-and-authorize)). |
| `Public` | Opens the event to anyone. Can't be combined with `Personal` or `Authorize`. |
| `OnSubscribe(ctx, s) error` | Runs once for each new subscription, however it was made, in its own goroutine, with a `ctx` that outlives the request and a `wakeshift.Subscribed[A]{Args, User, Via, Tool, Context}`: where a [`Watch`](/docs/events#wait-on-work-you-can-only-poll) starts (call `Watch` directly there: it's already in its own goroutine, and `wake.Close()` waits for it). For a tool, `ctx` carries the values of the `ctx` passed to `Subscribe`, without its cancellation, and `Context` is the value given as `wakeshift.Context(v)`, never sent to Wakeshift ([details](/docs/events#wait-on-work-you-can-only-poll)). A `Watch` already running for the same event and arguments keeps its first subscriber's `Context`; a later subscriber's doesn't reach it. An error logs `on_subscribe_failed`. |
| `DeleteOnDelivery` | Wakeshift deletes each occurrence's text and payload once it's delivered. |
| `Title` | A short title, for clients that show one. |
| `Topic`, `Filter` | Your own routing, when the default doesn't fit ([events](/docs/events#define-an-event)). |

The required fields of the arguments route each occurrence: it reaches the subscribers who picked the same values. Values are `a-z 0-9 . _ -`, at most 100 characters; encode a case-sensitive ID with `wakeshift.EncodeArgValue(id)`, and never lowercase it ([argument values](/docs/events#argument-values)).

**Who may subscribe.** An event that declares neither `Personal`, `Authorize` nor `Public` can be subscribed only by your own tools: no `subscribe_events`, no MCP Events subscription. Keep that default for IDs your tool mints or checks. Do you sign people in? Yes: `Personal` for things that belong to one user, `Authorize` for shared resources the caller must be allowed to see. `Public` is for the rare stream anyone may watch. Wakeshift enforces the same rule. See [who may subscribe](/docs/who-may-subscribe).

## Attach

Attach Wakeshift to your MCP server, and serve the server's HTTP handler through the attachment's `Handler`, after your endpoint's authentication:

```go
srv := mcp.NewServer(&mcp.Implementation{Name: "ci-bot", Version: "1.0.0"}, nil)
attached := wake.Attach(srv, wakeshift.AttachOptions{
	// Once per HTTP request, with that request's own ctx and verified token.
	Identify: func(ctx context.Context, r wakeshift.Request) (wakeshift.Identity, error) {
		if r.Token == nil {
			return wakeshift.Identity{}, nil
		}
		return wakeshift.Identity{User: r.Token.UserID}, nil
	},
	RequireUser: true, // refuse anonymous callers
})
mux.Handle("/mcp", wakeshift.RequireBearer(verify, metadataURL)(attached.Handler(mcpHandler)))
```

| `AttachOptions` field | What it is | Default |
| --- | --- | --- |
| `Identify(ctx, r) (Identity, error)` | Who is behind a request, from your own sign-in ([below](#sign-in-identify-and-authorize)). | No user |
| `RequireUser` | Refuse Wakeshift's part of a request with no user, so a `session_key` is never a bearer for its conversation. Set it whenever your server requires sign-in. | Anonymous allowed |
| `Wrap` | Tools registered elsewhere that subscribe, by name: `map[string][]wakeshift.ToolOption{"start_crawl": {wakeshift.Subscribes(crawlFinished)}}` ([tools registered elsewhere](/docs/low-level#go)). | None |
| `WrapFunc(tool) []ToolOption` | `Wrap` as a function, for tools you know only by name at run time. It's asked on each tools/list and tools/call about a tool that `AddTool` and `Wrap` don't declare, never a built-in, and its answer isn't kept. Return the tool's options, or nil for a tool that doesn't subscribe. A nil event or another client's logs `wrap_bad_event`, and the tool is served unwrapped. | None |
| `BuiltinTools` | Which built-in tools: `wakeshift.BuiltinTools{Wait: true, DisableSubscribe: true}`, or `wakeshift.BuiltinTools{DisableAll: true}` for none. `wakeshift.BuiltinToolNames()` lists their names. | `subscribe_events`, `list_subscriptions`, `unsubscribe_events` on, where something can subscribe; `wakeshift_wait` off |
| `IsNativeClient(ctx, r) bool` | For your own agents the SDK can't recognize as MCP Events clients ([advanced](/docs/advanced)). | The SDK's own checks |
| `Push` | `wakeshift.PushOff` to offer MCP Events clients webhook and poll only, never push (`events/stream`), for a host where something in front of your server forces JSON answers; or `wakeshift.PushOptions{MaxStream, MaxHeartbeat, Reauthorize}`, where zero keeps the default ([push options](/docs/mcp-events#push-options)). Over stdio, Go serves no MCP Events. | Push offered over HTTP |

- **`Handler` is required on HTTP.** It takes Wakeshift's arguments out of every tool call before go-sdk parses the request, so your middleware, validation and tools never see them, makes each HTTP request the unit `Identify` runs for, and serves MCP Events to clients that speak it. Tool calls that arrive over HTTP without it log `handler_missing`.
- **Built-in tools.** `subscribe_events` is listed only when an event is open to it, and `list_subscriptions` and `unsubscribe_events` only when something on the server can subscribe (a tool declared with `Subscribes`, or an open event). Without `Identify` those two act on the calling conversation, by its `session_key`; with a user, on that user's subscriptions. `DisableList` and `DisableUnsubscribe` turn them off; turn them off only if your own tools let a person see and stop their subscriptions. `Wait: true` adds Claude Code's [wait tool](/docs/agents#the-wait-tool-for-claude-code): give the endpoint a write timeout of 30 seconds or more.
- **Where the options live.** `attached.Handler` serves with the attachment's own `AttachOptions`, so a server you attach once sets them in one place. A server built per request has no one attachment to take them from: attach each server as you build it, and put one `wake.Handler(handler, opts)` around the HTTP handler that builds them, passing the same `AttachOptions` value to both (keep it in one variable). `Identify`, `RequireUser`, `BuiltinTools` and `IsNativeClient` act through the handler; `Wrap` and `WrapFunc` take effect through each server's `Attach`, so `wake.Handler` doesn't read them.
- **One attach per server.** A second `Attach` on the same server panics.
- **Your app's name.** The first tools/list asks Wakeshift for your app's name once, waiting at most 3 seconds (else the sentence says "this app"), and remembers it. After a failure it asks again a minute later, not on every list. `New` itself makes no request.
- **A server with its own MCP Events** keeps them: Wakeshift serves none there, and logs `events_capability_kept`. The order of `Attach` doesn't matter in Go: `Handler` decides from your server's first `initialize` or `server/discover` answer, and once that declares MCP Events, `events/*` requests go to your server (before that first answer, `Handler` answers them itself).

### Stdio servers

A stdio server attaches the same way. Serve stdin through `attached.Stdin`, which takes Wakeshift's arguments out of every tool call before go-sdk reads it, as `Handler` does on HTTP, so no middleware of yours sees them:

```go file
// main.go: a stdio server.
package main

import (
	"context"
	"log"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/wakeshift/wakeshift-go"
)

func main() {
	wake, err := wakeshift.New() // logs through slog.Default(), which writes to stderr
	if err != nil {
		log.Fatal(err)
	}
	srv := mcp.NewServer(&mcp.Implementation{Name: "ci-bot", Version: "1.0.0"}, nil)
	attached := wake.Attach(srv, wakeshift.AttachOptions{})
	// Define events and add tools as on HTTP, then serve stdin through the attachment.
	if err := srv.Run(context.Background(), attached.StdioTransport()); err != nil {
		log.Fatal(err)
	}
}
```

`attached.StdioTransport()` is go-sdk's stdio transport with `attached.Stdin` in front, and like `mcp.StdioTransport` it never closes your process's stdout when the session ends (an `mcp.IOTransport` given `os.Stdout` itself would). On a disabled client it's a plain stdio transport. With `&mcp.StdioTransport{}` instead, receiving middleware you add after `Attach` sees `session_key` ([the middleware order](/docs/low-level#go)). Stdout is the MCP connection, so the SDK never writes to it: it logs through `slog`, which writes to stderr by default. If you pass `wakeshift.WithLogger(l)`, point `l` at stderr too, for example `slog.New(slog.NewTextHandler(os.Stderr, nil))`.

**What a stdio server can't do.** It runs only while the agent has it open, so a [`watch`](/docs/events#wait-on-work-you-can-only-poll) or any background check of yours ends when the agent closes the server, and a job that finishes hours later may lose its wake: emit long jobs from a webhook or a hosted server. And ChatGPT connects only to remote servers over HTTPS, so it can't reach a stdio server; serve one over HTTP to reach it.

**Where the key lives when your stdio server is a package.** A stdio server runs on each user's machine, so never ship your app key inside the binary or package: anyone could read it and act as your app. Either each person who runs it creates their own app in the console and sets their own `WAKESHIFT_KEY` (without one, the server runs as it would without Wakeshift), or you serve Wakeshift from your hosted HTTP server, where the key stays with you, and the package talks to that.

## Tools that subscribe

A tool of yours subscribes the conversation calling it, so one call does what your app needs and sets up the wake-ups. Register it with `wakeshift.AddTool`, which is `mcp.AddTool` with the call after `ctx`, and name the events it subscribes to:

```go
type watchIn struct {
	Owner string `json:"owner" jsonschema:"The repo's owner: acme"`
	Repo  string `json:"repo" jsonschema:"The repo's name: api"`
}
type watchOut struct {
	Watching string `json:"watching"`
}

wakeshift.AddTool(attached, &mcp.Tool{Name: "watch_ci", Description: "Wake me when CI finishes on a repo."},
	func(ctx context.Context, call *wakeshift.ToolCall, in watchIn) (*mcp.CallToolResult, watchOut, error) {
		if !canSeeRepo(call.User, in.Owner+"/"+in.Repo) { // your own check, with your own sign-in
			return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "You can't see that repo."}}}, watchOut{}, nil
		}
		sub, err := ciFinished.Subscribe(ctx, call, repoArgs{Owner: in.Owner, Repo: in.Repo}, wakeshift.Reason("Watching CI on "+in.Repo))
		if err != nil {
			return nil, watchOut{}, err
		}
		_ = sub // sub.ID and sub.User address this conversation later
		return nil, watchOut{Watching: in.Owner + "/" + in.Repo}, nil
	}, wakeshift.Subscribes(ciFinished))
```

- **Your input type stays your own.** `tools/list` adds Wakeshift's arguments to the tool's schema, two sentences to its description saying it's how your app notifies the agent of those events and that the result may ask for a listener (about 300 characters: [the text](/docs/events#subscribe-from-your-own-tools)), and a `wakeshift` field to its output schema. `wakeshift.Subscribes()` with no events is a tool that subscribes to nothing. A one-line description that doesn't end its last sentence gets a period before the sentence.
- **Ownership is your tool's job.** `Subscribe` runs no `Authorize` for Claude Code, Codex and other bridged agents. When the caller names a resource, check they can see it first, as above. When your tool minted the ID itself, there's nothing to check.
- **`call`** is the `*wakeshift.ToolCall`: `User` (from `Identify`), `Tool`, `Native`, `NativeConversation`, `Recognized`, `Session()` (a `SessionKey`, which prints as `[redacted]`: the conversation's session once Wakeshift has resolved it, after a `Subscribe` that reached Wakeshift or `call.ResolveSession(ctx)`, and empty before that; never the raw `session_key` argument, which an agent can forge, so key nothing of your own on that), `Wakes()` ([below](#skip-your-own-waiting-only-when-wakeshift-wakes)) and `Request` (go-sdk's request; nil under mcp-go). `call.User` is Wakeshift's view of the caller: your own authorization rests on your own verified credential.
- **A call that fails** (an error, an `IsError` result, output its schema refuses, a panic, a canceled request) ends the subscriptions it made, and its reply gets nothing added.
- **Registration mistakes panic**, as `mcp.AddTool`'s do: nil arguments, a built-in tool's name, an input schema that names `session_key`, `harness` or `conversation_id`, an output schema that names `wakeshift`, an `In` or `Out` that can't be made a schema, or another client's event in `Subscribes`. Everything at request time returns an error or logs.

A tool registered some other way (`srv.AddTool`, a generated catalog) is named in `AttachOptions.Wrap` (or `WrapFunc`) and finds its call with `wakeshift.ToolCallFrom(ctx)`: see [tools registered elsewhere](/docs/low-level#go). Outside a tool call that subscribes, `ToolCallFrom` returns nil, and `Subscribe` with it returns `CodeNoCall` (on a disabled client, a `SubscriptionDisabled` subscription and no error).

### Subscribing

`event.Subscribe(ctx, call, args, opts…)` subscribes the calling conversation, or renews its subscription with those arguments. It takes only the events the tool names in `Subscribes` (otherwise `CodeUndeclaredSubscribe`).

| Option | What it does |
| --- | --- |
| `wakeshift.Once()` | Ends the subscription after its first delivery: "tell me when this finishes". |
| `wakeshift.Replay(d)` | Also delivers the latest occurrence with these arguments from that far back (over 0, at most 24 hours): for a job whose ID exists only after it started ([subscribe before you start the work](/docs/low-level#subscribe-before-you-start-the-work)). Applies only when the subscription is created. |
| `wakeshift.TTL(d)` | Ends it after that long without a delivery or a renewal (default 30 days). |
| `wakeshift.Reason(s)` | Why your app subscribed the conversation, shown in the console (at most 200 characters). |
| `wakeshift.Label(s)` | Names a new session, for an app that acts for its users' agents ([advanced](/docs/advanced)). |
| `wakeshift.Context(v)` | A value of yours for the event's `OnSubscribe` (`Subscribed.Context`), such as the caller's credential; never sent to Wakeshift or logged. |

A renewal sets `Once` and `TTL` to that call's. It returns a `wakeshift.Subscription` whose `State` says what happened:

| `State` | Fields | Meaning |
| --- | --- | --- |
| `SubscriptionCreated`, `SubscriptionRenewed` | `ID`, `User`, `Session`, `Recognized` | A conversation subscription, new or renewed. Store `ID` and `User` to [emit to this conversation](/docs/events#emit-to-many) later. |
| `SubscriptionNative` | `User`, `Recognized` | An MCP Events client subscribes itself; the reply tells it what to subscribe to, with a one-time voucher when only your tools subscribe to the event. `User` is `""` for an anonymous caller. |
| `SubscriptionUnavailable` | `Reason`: `client_unsupported`, `no_user` or `refused` | Nothing happened: an MCP Events client and an event only your tools subscribe to that Wakeshift couldn't hand it a voucher for; such a client without a signed-in user on a `Personal`, `Authorize` or `Public` event; or the event's `Authorize` refused. Logged once per tool and reason. |
| `SubscriptionDisabled` | | The client is disabled. |

`event.Unsubscribe(ctx, call, args)` ends the conversation's subscription with those arguments. Work the user paid for comes first: do it, then subscribe, and don't fail the call if the subscribe fails.

#### Skip your own waiting only when Wakeshift wakes

`call.Wakes()` says whether Wakeshift wakes this conversation for what the call subscribed. It's true only after a `Subscribe` in this call made or renewed a subscription that's still live, and a wake is confirmed: Wakeshift can wake the conversation's agent now, with no choice from the user still to come. A ChatGPT Work chat on the web counts as woken once a `Subscribe` returns `SubscriptionNative` (it subscribes itself, through MCP Events); ChatGPT's desktop app and ChatGPT through a bridged tool don't. It's false before any `Subscribe`, after one that returned an error (a timeout, Wakeshift unreachable, a refusal) or `SubscriptionUnavailable`, once the call has unsubscribed it again, while the user's choice of how to be notified is pending (Claude Code asks it the first time it watches something; a later call is true once their pick runs), on a disabled client, and for an agent Wakeshift can't wake. Skip your own polling or waiting only when it's true; otherwise behave as without Wakeshift:

```go
type runOut struct {
	Run    string `json:"run"`
	Status string `json:"status"`
}

var ci interface { // your own CI
	Start(ctx context.Context, repo repoArgs) (run string, err error)
	Wait(ctx context.Context, run string) (status string, err error)
}

wakeshift.AddTool(attached, &mcp.Tool{Name: "run_ci", Description: "Run CI on a repo."},
	func(ctx context.Context, call *wakeshift.ToolCall, in watchIn) (*mcp.CallToolResult, runOut, error) {
		repo := repoArgs{Owner: in.Owner, Repo: in.Repo}
		run, err := ci.Start(ctx, repo)
		if err != nil {
			return nil, runOut{}, err
		}
		if _, err := ciFinished.Subscribe(ctx, call, repo, wakeshift.Once()); err != nil {
			// go on without wakes
		}
		if call.Wakes() {
			return nil, runOut{Run: run, Status: "started"}, nil
		}
		status, err := ci.Wait(ctx, run) // as without Wakeshift
		return nil, runOut{Run: run, Status: status}, err
	}, wakeshift.Subscribes(ciFinished))
```

Don't promise the wake in your own text: Wakeshift's addition tells the agent.

## Emit

When CI finishes, emit it:

```go
_, err = ciFinished.Emit(ctx, repoArgs{Owner: "acme", Repo: "api"},
	ciRun{Branch: "main", Conclusion: "failure", Name: "tests", URL: runURL},
	wakeshift.WithID(deliveryID))
```

- **`wakeshift.WithID(id)`** makes retries emit once, for a day: a retry returns the first emit's result with `Duplicate: true`, and the same ID on a different emit is refused (`CodeIDReused`). A retry the SDK makes itself never wakes twice.
- **`Emitted.Matched`** counts the subscriptions that received the occurrence, not agents awake now. `Emitted.TextTrimmed` says Wakeshift trimmed a text over 1000 bytes. A payload is at most 64 KB.
- **Before any agent connects.** You can emit at startup, or from a webhook worker: if Wakeshift doesn't know the event yet, the SDK sends your manifest once and emits again.

A personal event's occurrence goes to its recipients, mixed: `wakeshift.To(users...)` reaches every subscription each user has with those arguments, `wakeshift.ToSubscription(user, id)` exactly the conversation a `Subscribe` returned, and `wakeshift.ToNative(users...)` only the users' own MCP Events subscriptions. Take the users from your own record of who owns what, never from who's subscribed.

```go
type tableArgs struct {
	Table string `json:"table"`
}
type turnMsg struct {
	ToCall int `json:"to_call"`
}

turn, err := wakeshift.DefineEvent(wake, "cards.turn", wakeshift.EventDef[tableArgs, turnMsg]{
	Description: "It's your move at a table",
	Personal:    true,
	Text:        func(m turnMsg, a tableArgs) string { return "Your move at " + a.Table },
})
emitted, err := turn.Emit(ctx, tableArgs{Table: "oak"}, turnMsg{ToCall: 20},
	wakeshift.ToSubscription("ada", "es_7c1…"), wakeshift.ToSubscription("bo", "es_91d…"), wakeshift.ToNative("cy"),
	wakeshift.WithID("oak:hand-12"))
_ = emitted.Ended // subscriptions that ended: forget them
```

At most 1,000 recipients per emit. `Emitted.Unmatched` lists users and native recipients nobody was subscribed for, and `Emitted.Ended` the subscriptions that have ended. A personal event emitted without a recipient, or a shared one with one, is refused before sending (`CodeBadArguments`).

## Publish URLs

A publish URL emits the event, with the arguments you made it for, each time a webhook sender posts to it ([publish URLs](/docs/publish-urls)):

```go
if wake.Enabled() {
	u, err := ciFinished.CreatePublishURL(ctx, repoArgs{Owner: "acme", Repo: "api"},
		wakeshift.SigningSecret(os.Getenv("SENDER_SIGNING_SECRET")))
	if err != nil {
		log.Print(err)
	} else {
		log.Printf("made publish URL %s", u.ID) // give u.URL to the sender; keep u.ID to revoke it
		_ = wake.RevokePublishURL(ctx, u.ID)
	}
}
```

- Each call mints a new URL. `wakeshift.To(user)` makes one for a personal event's one user. `wakeshift.SigningSecret(s)` takes the sender's Standard Webhooks secret (`whsec_…`): the URL then accepts only posts signed with it, and `u.Signing` has its fingerprint, never the secret.
- **The URL is a credential.** A `PublishURL` prints, logs and marshals without its token; pass `u.URL` explicitly where it's meant to go. Use them signed when the sender signs, otherwise one per job, revoked at the job's last event ([use them safely](/docs/publish-urls#use-them-safely)).
- On a disabled client `CreatePublishURL` returns `CodeDisabled`, hence the `wake.Enabled()` guard where a tool offers one.

## Sign-in: Identify and Authorize

Your app signs its users in. `Identify` tells Wakeshift who is behind a request; `Authorize` decides who may subscribe to an event through `subscribe_events` or MCP Events.

- **`Identify(ctx, r)`** gets each HTTP request's own `ctx` (with your middleware's values) and a `wakeshift.Request`: `Token` (the `*auth.TokenInfo` go-sdk's auth middleware verified, nil without one), `Header` (not verified; nil on stdio, on go-sdk and mcp-go alike, and `r.Header.Get` is safe on a nil header), `ClientName` and `Meta`. It runs only when Wakeshift needs to know (a call of a tool that subscribes, a built-in tool, MCP Events), at most once per request. Return `wakeshift.Identity{User: …}` with your ID for the person, from a verified credential; `Client` names the MCP client for a request that doesn't carry its name.
- **If `Identify` fails**, panics or takes over 10 seconds, Wakeshift's part of the request is refused (`CodeIdentifyFailed` from `Subscribe`) and your tool's own code still runs. `RequireUser` refuses an anonymous caller the same way, with `CodeUserRequired`.
- **`wakeshift.RequireBearer(verify, metadataURL)`** checks the tokens your sign-in issues on your MCP endpoint, so `r.Token` is set. Header-based identity needs your own verifier.

```go
deployFinished, err := wakeshift.DefineEvent(wake, "deploy.finished", wakeshift.EventDef[repoArgs, ciRun]{
	Description: "A deploy finished on a repo",
	Authorize: func(ctx context.Context, s wakeshift.Subscriber, a repoArgs) (bool, error) {
		return canSeeRepo(s.User, a.Owner+"/"+a.Repo), nil
	},
})
_ = deployFinished
```

- **`Authorize`** decides every subscription made through `subscribe_events`, MCP Events subscribe and every poll, and runs before a tool tells an MCP Events client to subscribe. `s.Request` is the request behind it. An error, a panic or a run over 10 seconds refuses, and logs `authorize_failed` with the error's type.
- **Never decide on `Subscriber.Client` or `Native`:** the client asserts both. Never write an `Authorize` that refuses everyone to close MCP Events: the default already does that.
- It runs on every poll (about every 10 seconds per subscriber), so cache allows in your own layer, keyed by user and arguments.

Apps that act for their users' agents use `Workspace`, `AddAgent`, `ActAs` and `CheckAgent`: see [advanced](/docs/advanced).

## mcp-go

If your server is built with [mcp-go](https://github.com/mark3labs/mcp-go), attach Wakeshift with `wakeshift/mcpgo`. Defining events, subscribing and emitting are the same `wakeshift` calls. An app on the official SDK never builds mcp-go.

```go mcp-go
srv := server.NewMCPServer("ci", "1.0.0")
attached := mcpgo.Attach(wake, srv, wakeshift.AttachOptions{})
mcpgo.AddTool(attached,
	mcp.NewTool("watch_ci", mcp.WithDescription("Wake me when CI finishes on a repo"),
		mcp.WithString("owner", mcp.Required()), mcp.WithString("repo", mcp.Required())),
	func(ctx context.Context, call *wakeshift.ToolCall, _ mcp.CallToolRequest, in watchIn) (*mcp.CallToolResult, error) {
		if _, err := ciFinished.Subscribe(ctx, call, repoArgs{Owner: in.Owner, Repo: in.Repo}); err != nil {
			return nil, err
		}
		return mcp.NewToolResultText("Watching " + in.Owner + "/" + in.Repo + "."), nil
	}, wakeshift.Subscribes(ciFinished))
mux.Handle("/mcp", wakeshift.RequireBearer(verify, metadataURL)(attached.Handler(server.NewStreamableHTTPServer(srv))))
```

- `mcpgo.Attach` takes the same `wakeshift.AttachOptions`, `Wrap` and `WrapFunc` included: a tool you register with `srv.AddTool` and name there finds its call with `wakeshift.ToolCallFrom(ctx)`, and its result gets Wakeshift's block. Put `attached.Handler` in front of `server.NewStreamableHTTPServer(srv)`, after your authentication.
- `mcpgo.AddTool`'s handler gets the call, mcp-go's request, and the call's arguments bound to your input type. A handler that returns `nil, nil` replies with Wakeshift's block alone.
- On stdio, serve with `mcpgo.ServeStdio(attached)` instead of `server.ServeStdio(srv)`, or pass `attached.Stdin(os.Stdin)` to your `StdioServer`'s `Listen`. Wakeshift's arguments then leave every tool call before mcp-go reads it, so no hook or middleware of yours sees them. Without it, they're taken out in a `BeforeCallTool` hook that `Attach` adds: tool middleware never sees them, but hooks that run before it do (`BeforeAny`, `OnRequestInitialization`, and `BeforeCallTool` hooks added before `Attach`).

## Calls

| Call | What it does |
| --- | --- |
| `wakeshift.New(opts…)` | The client, from `WAKESHIFT_KEY`. No I/O; no key gives a disabled client. |
| `wake.Enabled()`, `wake.KeyID()`, `wake.AppID(ctx)`, `wake.Info(ctx)` | Whether Wakeshift is on; the key's ID; your app's ID (asked of Wakeshift on first need); which app and key Wakeshift says this is. |
| `wakeshift.DefineEvent(wake, name, wakeshift.EventDef[A, P]{…})` | Defines an event. Define every event before `Attach`. |
| `wake.Attach(srv, wakeshift.AttachOptions{…})` | Attaches Wakeshift to a go-sdk server. Its `Handler(next)` serves the endpoint. `wake.Handler(next, opts)` is for servers built per request. |
| `wakeshift.AddTool(attached, tool, handler, wakeshift.Subscribes(events…))` | Registers a tool that subscribes. `wakeshift.ToolCallFrom(ctx)` is the call for a tool named in `Wrap`. |
| `event.Subscribe(ctx, call, args, opts…)`, `event.Unsubscribe(ctx, call, args)` | Subscribe the conversation calling a tool, or end its subscription with those arguments. |
| `call.Wakes()` | Whether Wakeshift wakes this conversation for what the call subscribed: skip your own polling or waiting only when it's true ([skip your own waiting](#skip-your-own-waiting-only-when-wakeshift-wakes)). |
| `call.CallLimit()`, `call.ShouldSkipWait(d)` | How long the agent's program lets one tool call take (Claude Code: 60 seconds, Codex: 300; 0 when not known), and whether to skip a wait of `d` the agent asked for: true only when `Wakes()` is true and `d` reaches the limit ([a wait longer than the program allows](/docs/events#a-wait-longer-than-the-agents-program-allows)). |
| `call.ResolveSession(ctx)` | Finds or starts the calling conversation's session without subscribing it ([advanced](/docs/advanced)); empty, with no request to Wakeshift, when nothing names the conversation. |
| `event.Emit(ctx, args, payload, opts…)` | Sends an occurrence to everyone subscribed with those arguments, or to a personal event's recipients (`To`, `ToSubscription`, `ToNative`). |
| `event.Watch(ctx, args, check, opts…)` | Calls `check` until it says the work is done, then emits once; blocks, so run it in a goroutine. Options `Every`, `Timeout`, `MaxErrors`, `FinalIf`, `WithID`, `To`: [wait on work you can only poll](/docs/events#wait-on-work-you-can-only-poll). |
| `event.Status(ctx, args, opts…)`, `event.StatusOf(ctx, subs…)` | How many conversations are subscribed with those arguments (`wakeshift.To(user)` for a personal event) and how many are connected now; or, by `Recipient{User, Subscription}`, whether each subscription is live and connected ([who is subscribed](/docs/events#who-is-subscribed)). |
| `event.CreatePublishURL(ctx, args, opts…)`, `wake.RevokePublishURL(ctx, id)` | Make a publish URL, or turn one off. |
| `wake.EventSubscriptions(ctx, user)` | A user's live subscriptions to your events. |
| `wake.ForgetUser(ctx, user)` | Ends the user's subscriptions and deletes what Wakeshift keeps for them ([forgetting a user](/docs/users#forgetting-a-user)). |
| `wake.PublishManifest(ctx, wakeshift.Manifest{…})` | Tells Wakeshift what your app may do, replacing what it said before. Optional: the SDK sends the manifest itself, adding its events and never removing one. Call it to remove an event. |
| `wakeshift.EncodeArgValue(id)`, `wakeshift.DecodeArgValue(v)` | Encode a case-sensitive ID as an argument value, and decode it. |
| `wakeshift.RedactSecrets(text)` | Replaces every Wakeshift secret in text with `[redacted]`. |

## Logging and secrets

The SDK logs through `slog.Default()`, or `wakeshift.WithLogger(l)`; `slog.New(slog.DiscardHandler)` silences it. Every line has a `code` ([log codes](/docs/errors#log-codes)), and `event` or `tool` where it's about one. Misuse logs once per client; failures that can go on (`identify_error`, `authorize_failed`, `key_refused`, `block_failed`) log at most once per five minutes, with a count.

A Wakeshift failure never changes your tool's result. If Wakeshift's addition to a result can't be had (an error, a timeout, Wakeshift unreachable, a refused key), the agent gets your result exactly as your handler returned it, never an error and never a request to call again; what the call subscribed is ended, and `block_failed` is logged.

It never logs keys, session keys, your callers' tokens or headers, signing secrets, publish URL tokens, payloads, tool arguments or your own error messages: only codes, names and error types. A `*Client`, a `SessionKey` (`call.Session()`), a `Request`, a `Subscriber`, a `*ToolCall`, a `PublishURL` and an `Agent` print, log and marshal without their secrets; `string(call.Session())` is the key, where you mean to use it; it's empty until Wakeshift has resolved the conversation, never the raw `session_key` argument an agent passed. Keep `session_key` out of your own structs and logs. For logs the SDK can't reach (HTTP body logging, a proxy, an error reporter), `wakeshift.RedactSecrets(text)` replaces every Wakeshift secret with `[redacted]`.

## Errors

An `*wakeshift.APIError` is Wakeshift's answer, with `Status`, `Code`, `Message` and `RequestID`, or no answer at all: `Status` 0, with `CodeUnreachable` or `CodeTimeout`, unwrapping to the transport's error. A `*wakeshift.UsageError` is a mistake caught before sending, with `Issues` for bad arguments. Every message says what failed, the fix, and a link to the code. `wakeshift.IsCode(err, code)` checks either type:

```go
if _, err := ciFinished.Emit(ctx, repoArgs{Owner: "acme", Repo: "api"}, ciRun{Conclusion: "success"}, wakeshift.WithID(deliveryID)); err != nil {
	switch {
	case wakeshift.IsCode(err, wakeshift.CodeIDReused):
		log.Print("that delivery ID was used for a different emit")
	case wakeshift.IsCode(err, wakeshift.CodeTimeout):
		log.Print("the outcome is unknown: retry with the same ID")
	}
}
```

`wakeshift.CodeOf(err)` returns the error's `Code` ("" for an error that isn't the SDK's), for logging or a `switch`.

`wake.Close()` stops every `Watch` and `OnSubscribe` the client started and waits for them; nothing the SDK starts outlives it. Call `defer wake.Close()` where the process goes on after serving, such as in tests.

A canceled or expired `ctx` returns an error that `errors.Is(err, context.Canceled)` (or `context.DeadlineExceeded`) matches, never an `APIError`.

| Code | When |
| --- | --- |
| [`CodeBadArguments`](/docs/errors#bad_arguments) | Arguments, a payload or recipients that don't fit the event: a personal event without `To`, a shared one with it, a `Replay` out of bounds. |
| [`CodeUnknownEvent`](/docs/errors#unknown_event) | Your app offers no event by that name, or the event is another client's. |
| [`CodeUnauthenticated`](/docs/errors#unauthenticated) | Wakeshift refused your key: it was revoked, or it belongs to another console. |
| [`CodeIDReused`](/docs/errors#id_reused) | An emit's ID was used within the last day for a different emit. Nothing was sent. |
| [`CodeRateLimited`](/docs/errors#rate_limited) | Too many emits to lists of recipients: retry after a while. |
| [`CodeUnreachable`](/docs/errors#unreachable), [`CodeTimeout`](/docs/errors#timeout) | No answer. After a timeout the outcome is unknown: retry with the same ID. |
| [`CodeConfig`](/docs/errors#config) | A bad option or value at `New`, `DefineEvent` or `Attach`. |
| [`CodeDisabled`](/docs/errors#disabled) | A call that needs Wakeshift on a disabled client. Check `wake.Enabled()` first. |
| [`CodeUndeclaredSubscribe`](/docs/errors#undeclared_subscribe) | A tool subscribed to an event it doesn't name in `Subscribes`. |
| [`CodeNoCall`](/docs/errors#no_call), [`CodeCallEnded`](/docs/errors#call_ended) | `Subscribe` outside a tool call, or after its handler returned. |
| [`CodeIdentifyFailed`](/docs/errors#identify_failed) | `Identify` failed or timed out. |
| [`CodeUserRequired`](/docs/errors#user_required) | `RequireUser` refused an anonymous caller. |

Every code, with its fix, is on [errors](/docs/errors).

### In a webhook receiver

When your own handler receives a webhook and emits, answer the sender by what `Emit` returned, so it retries only what can succeed:

| `Emit` returned | Answer the sender |
| --- | --- |
| No error, or `Duplicate: true` | `2xx` |
| `CodeIDReused`, `CodeBadArguments`, `CodeUnknownEvent` | `2xx` or `422`, and log it: a retry won't help |
| `CodeRateLimited` | `503` with `Retry-After` |
| `CodeUnreachable`, `CodeTimeout`, or a Wakeshift `5xx` | `502` or `503`, so the sender retries with the same delivery ID |
| A bad signature, checked before emitting | `401` |

## Examples

Agent Chat, Secret Agent, Nightfall and Slackline are built on this SDK; their source is in [wakeshift/apps](https://github.com/wakeshift/apps).
