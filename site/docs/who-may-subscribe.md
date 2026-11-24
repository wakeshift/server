# Who may subscribe

Every event answers one question: who may subscribe to it? By default, only your own tools can. An event opens to other paths only when it says so, with `personal`, `authorize` or `public`, and Wakeshift enforces that on its side too.

## One question: do you sign people in?

- **No.** Keep the default. Your tools subscribe the conversation calling them, to IDs your tool minted or checked (jobs, crawls, runs), and nobody else can subscribe at all.
- **Yes.** Use `personal` for things that belong to one user (their turn, their messages), and `authorize` for shared resources the caller must be allowed to see (a repo, a project).
- **`public`** is for the rare stream anyone may watch.

Either way, step 0: if you have an `identify`, it reads a credential your server verified, never an unverified header.

| The event declares | A tool that subscribes | `subscribe_events` | Native subscriptions (`events/subscribe`, `events/poll`, `events/stream`) | Use it for |
| --- | --- | --- | --- | --- |
| nothing (the default) | ✓ | refused, and not listed | only with a voucher your tool handed out ([below](#chatgpt-and-other-clients-that-subscribe-natively)); refused otherwise | IDs your tool mints or checks: jobs, crawls, runs |
| `personal: true` | ✓ | ✓ (the caller's own) | ✓ (the caller's own; needs a user) | Things that belong to one user |
| `authorize` | ✓ | ✓ if `authorize` allows | ✓ if `authorize` allows, on every request | Shared resources the caller must be allowed to see |
| `public: true` | ✓ | ✓ | ✓ (needs a user) | Truly public streams |

`public` can't be combined with `personal` or `authorize`: the SDK refuses it when you define the event.

## Shared resources: `authorize`

```ts tab:TypeScript
const ciFinished = wake.defineEvent("ci.finished", {
  description: "CI finished on a GitHub repo",
  args: z.object({ owner: z.string(), repo: z.string() }),
  payload: z.object({ branch: z.string(), conclusion: z.string() }),
  authorize: (subscriber, { owner, repo }) => canSeeRepo(subscriber.authInfo, `${owner}/${repo}`),
})
```

```python tab:Python
ci_finished = wake.define_event(
    "ci.finished",
    description="CI finished on a GitHub repo",
    args=Repo,
    payload=Run,
    authorize=lambda subscriber, a: can_see_repo(subscriber.user, f"{a.owner}/{a.repo}"),
)
```

```go tab:Go
wake, err := wakeshift.New()
if err != nil {
	log.Fatal(err)
}

type repoArgs struct {
	Owner string `json:"owner"`
	Repo  string `json:"repo"`
}
type ciRun struct {
	Branch     string `json:"branch"`
	Conclusion string `json:"conclusion"`
}

ciFinished, err := wakeshift.DefineEvent(wake, "ci.finished", wakeshift.EventDef[repoArgs, ciRun]{
	Description: "CI finished on a GitHub repo",
	Authorize: func(ctx context.Context, s wakeshift.Subscriber, a repoArgs) (bool, error) {
		return canSeeRepo(s.User, a.Owner+"/"+a.Repo), nil
	},
})
if err != nil {
	log.Fatal(err)
}
_ = ciFinished
```

- `authorize` decides every subscription made through `subscribe_events`, and every native `events/subscribe`, `events/poll` and `events/stream` when it opens (and every 5 minutes while a push stream is open; a refusal ends the stream), about every ten seconds for each native subscriber. Cache allows in your own layer, keyed by user and arguments, if the check is expensive.
- For a client that subscribes natively, such as ChatGPT, the SDK also asks `authorize` before your tool tells it what to subscribe to. A refusal makes `call.subscribe` return `state: "unavailable"` with `reason: "refused"`, and the client is told nothing.
- It gets the subscriber: `user` (from your `identify`), the caller's verified token (TypeScript `authInfo`; Python and Go `request`), and `client` and `native`. **Never decide on `client` or `native`**: the client asserts both.
- A throw, an error, or a run over 10 seconds refuses the subscription, and the SDK logs `authorize_failed` with the error's type.
- Your own tools don't consult `authorize`: a tool that subscribes to a resource the caller names checks the caller can see it, in its own code.

### Resources a team shares

When a resource belongs to a team (an organization's project, a shared workspace, a channel), put the team or the resource in the event's arguments and check membership in `authorize`, from your own records and the caller's verified sign-in: `authorize: async (subscriber, { team }) => !!subscriber.user && (await isMember(subscriber.user, team))`, where `isMember` is your own code. Every member who subscribes with those arguments gets each occurrence you emit with them, and nobody else can subscribe. Keep such an event shared, not `personal`: a personal event goes only to the users each emit names, so it suits what belongs to one person. Cache membership in your own layer if the check is slow, since native clients' polls ask `authorize` about every ten seconds. When someone leaves the team, end their subscriptions yourself ([when someone loses access](/docs/users#when-someone-loses-access)): a subscription made through `subscribe_events` isn't checked again.

## Things that belong to one user: `personal`

A personal event's subscriptions each belong to the user who made them, and an emit goes only to the users you name. Nothing in the arguments names the subscriber, so nobody can subscribe to someone else's.

```ts
const yourTurn = wake.defineEvent("game.turn", {
  description: "It's your move at a table",
  args: z.object({ table: z.string() }),
  payload: z.object({ toCall: z.number() }),
  personal: true,
})

await yourTurn.emit({ table: "juniper" }, { toCall: 2 }, { to: "player-abc123" })
```

**`to` comes from your own ownership record**: your seating table, your membership list, who owns the order. Never take it from the subscriber list, and never from anything the caller sent. A personal event needs a signed-in user to subscribe, so pass `identify` to `attach`, and `requireUser: true` whenever your server requires sign-in. More on [personal events](/docs/events#personal-events) and [emitting to many](/docs/events#emit-to-many).

## Don't close native subscriptions with `authorize`

An `authorize` that always returns `false`, written to keep clients from subscribing natively, does what the default already does, and reads as if the event were open. Remove it and leave the event at the default.

## ChatGPT and other clients that subscribe natively

ChatGPT is woken only through MCP Events: it subscribes itself and receives each occurrence as a webhook. Claude Code, Codex and other agents are woken through your tools.

| The event | ChatGPT and other MCP Events clients | Claude Code, Codex and other agents |
| --- | --- | --- |
| The default | Served through a subscribe voucher: when your tool subscribes it, its result hands the client a one-time voucher for exactly that event and those arguments, which it uses to subscribe within 4 hours. With sign-in, only the same user can use it; without, only from the session it keeps with your server (`Mcp-Session-Id`), and a client that keeps none, such as ChatGPT, gets no voucher, since anyone who read it could use it. `call.subscribe` returns `state: "native"` | Through your tools |
| `personal` | Served, to a signed-in user | Your tools and `subscribe_events` |
| `authorize` | Served if `authorize` allows, asked before your tool's reply and on every request | Your tools and `subscribe_events` |
| `public` | Served, to a signed-in user | Your tools and `subscribe_events` |
| Your server serves its own MCP Events | Gets your server's own events, not Wakeshift's | Unchanged |

So ChatGPT is served whatever you choose, on a server it can reach: through your tools' vouchers by default, or directly through `personal`, `authorize` or `public`. ChatGPT connects only to a remote server over HTTP, never to a stdio one, and the Python SDK serves MCP Events only on mcp 2.x hosts (MCPServer, FastMCP 4, the low-level `Server`), not on FastMCP 2 or 3. ChatGPT reads your events list when it connects, so a user connected before an event became available sees it after **Refresh tools** in ChatGPT's plugin settings. The SDK logs `native_unavailable` once per tool and reason, so you see any client your events don't reach.

How a voucher works:

- **It's in your tool's result**, in the text Wakeshift adds (and in `structuredContent.wakeshift` when your tool returns structured content): `wsv_` and 32 characters. You write nothing for it.
- **The client sends it back** as the event argument `wakeshift_voucher` in `events/subscribe`, with a webhook, or in `events/stream` (push). Push takes a voucher when the caller is signed in, or keeps a session with your server (`Mcp-Session-Id`); the subscription stays bound to that user or session. `events/list` shows your tools-only events to MCP Events clients with that argument required. Their `delivery` is `webhook`, and `push` where your server offers it: `events/poll` of a tools-only event is refused, voucher or not. The SDK takes the voucher out of the arguments before `authorize`, `onSubscribe` or your logs see them, so `wakeshift_voucher` is a reserved argument name.
- **It creates one subscription**, to exactly that event and those arguments, and lets the client renew and end it. The subscription keeps what your tool's `call.subscribe` asked for: `once` ends it at its first delivery, and `ttl` bounds it. It starts with the latest occurrence since your tool handed out the voucher (or within `replay`, if that reaches further back), so an emit between your tool's reply and the client's subscribe isn't lost.
- **Using it again refreshes that subscription, never makes a second.** While the subscription is live, `events/subscribe` with the same voucher and the same webhook URL renews it: for an anonymous caller only with the same secret, and for a signed-in user only as that user, who may send a new secret to replace the old one. Once the subscription has ended (after a `once` delivery, say), the voucher is spent.
- **Anything else is refused** with MCP Events' forbidden error: a spent or expired voucher, one for other arguments or another user, another webhook URL, and a tools-only event without a voucher. The error is `-32012` for ChatGPT (client name or User-Agent `openai-mcp`) and `-32024` for every other client; the message says the voucher "was used, it expired, or it's for other arguments".
- **A signed-in user has at most 20 live webhook subscriptions to your app** (`events/subscribe`), vouchers' and `personal`, `authorize` or `public` ones together, and 20 push subscriptions (`events/stream`), counted apart. Past that, a new one is refused with MCP Events' `ResourceExhausted` error (`InvalidParams`, `-32602`, for ChatGPT) until one ends or lapses. A `once` subscription stops counting when it delivers, and renewing one is never refused.
- **`onSubscribe` runs when the client subscribes** with the voucher, with `via: "tool"` and your tool's name, as for any subscription your tool made, and with the `context` your tool passed to `call.subscribe` when this process handed out the voucher (a redemption on another instance gets none).

## Upgrading from an earlier SDK

This rule arrives with TypeScript SDK 0.11, Python SDK 0.10 and Go SDK v0.10. **Before you upgrade, nothing changes**: Wakeshift keeps treating your events as open to every path, exactly as today.

When a new SDK first publishes your app's events, each event that declares neither `personal`, `authorize` nor `public` becomes tools-only, and Wakeshift ends the subscriptions that were made to it through `subscribe_events` or natively. Subscriptions your tools made keep working. The SDK logs `event_closed` at warn level for each such event, with how many subscriptions ended and how to reopen it.

- **If only your tools ever subscribed** (the common case), do nothing: the log says 0 subscriptions ended.
- **To keep other paths open**, add `authorize` (gated) or `public: true` (open to anyone) before you deploy.
- **If ChatGPT subscribes to those events on its own**, without your tool handing it a voucher, decide before you deploy: add `authorize` or `personal`, or those subscriptions end. Subscriptions your tools hand ChatGPT vouchers for keep working on the default.

Reopening an event later takes a new SDK publishing `authorize`, `personal` or `public: true` for it, and Wakeshift records the change.

## A worked example

A shopping server offers `product.price.changed` and `product.availability.changed`, keyed by a watch ID its own `watch_product` tool mints. It has no sign-in, and agents subscribe only through `watch_product`.

- **On the new SDK**, both events are tools-only, which is what it meant. Nothing that matters ends: no agent subscribed any other way. Its first deploy logs `event_closed` for both events with 0 subscriptions ended.
- **ChatGPT** gets no voucher without sign-in: it keeps no session with the server, so a voucher would work for anyone who read it. A guessed watch ID without a voucher is refused.
- **Once it adds sign-in**, `watch_product`'s result hands ChatGPT a voucher for that watch ID, bound to the signed-in user, and ChatGPT subscribes with it. To let ChatGPT subscribe without calling `watch_product` first, it adds `authorize`, checking that the watch ID belongs to the caller. `public: true` would also work, but opens the events to anyone who learns a watch ID.
