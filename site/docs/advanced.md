# Advanced

Calls most apps never need: acting for your users' agents, reaching one conversation by its subscription, and the finer options of `call.subscribe`. Everything here works the same in the three SDKs, in each language's style.

## Apps that act for their users' agents

Some apps run agents of their own for their users: a game's house players, a team bot. Wakeshift keeps a workspace for each of your users, and your app can add its agents there, each with a grant, then make calls as one of them.

```ts
const workspace = await wake.workspace({ user: "u_7f3a", name: "Ada's games" })
await wake.addAgent({ workspace, agent: "dealer", displayName: "The dealer" })

// A request from that agent: your auth middleware verified its token and set extra.agent.
const dealer = await wake.actAs({ workspace, agent: "dealer" })
const attached = wake.attach(server, { identify: (ctx) => (ctx.http?.authInfo?.extra?.agent === "dealer" ? { agent: dealer } : undefined) })
```

| | TypeScript | Python | Go |
| --- | --- | --- | --- |
| The workspace for one of your users, made on first use | `wake.workspace({ user, name })` | `await wake.workspace(user, name)` | `wake.Workspace(ctx, user, name)` |
| Add an agent there, with a grant for your app; returns the grant's ID | `wake.addAgent({ workspace, agent, displayName })` | `await wake.add_agent(workspace, agent, display_name)` | `wake.AddAgent(ctx, workspace, agent, displayName)` |
| Act as the agent: an `Agent` to return from `identify` | `wake.actAs({ workspace, agent })` | (not in the Python SDK) | `wake.ActAs(ctx, workspace, agent)` |
| Check its grant is still live: throws `no_grant` once revoked, `unknown_workspace` once forgotten | `wake.checkAgent(workspace, agent)` | `await wake.check_agent(workspace, agent)` | `wake.CheckAgent(ctx, workspace, agent)` |

- `identify` returns `{ agent }` only from a credential your server verified, never from a header the caller sets: a call made as an agent is that agent's, in its user's workspace.
- `actAs` caches the agent until shortly before its token expires. The `Agent` never prints its token.
- What your app may do as its agents is its manifest's scope: `wake.publishManifest({ agent: { … } })` ([the manifest](#the-manifest)).

### Your own agents that subscribe natively

The SDK tells which callers subscribe to your events themselves (MCP Events) from what their requests carry. Your own agents may not carry it. `isNativeClient` (Python `is_native_client`, Go `IsNativeClient`) says so for them:

```ts
const houseAttached = wake.attach(server, { identify: (ctx) => ({ user: userOf(ctx.http?.authInfo) }), isNativeClient: (ctx) => ctx.http?.req?.headers.get("x-house-agent") === "1" })
```

Use it for your own agents only. It decides how a call is answered, never who may subscribe: [who may subscribe](/docs/who-may-subscribe) is the event's.

## Reaching one conversation

A personal event's emit reaches every subscription a user has with those arguments. To reach exactly one conversation, store the `id` and `user` that `call.subscribe` returned, and emit to that subscription:

| Recipient | TypeScript | Python | Go | Reaches |
| --- | --- | --- | --- | --- |
| A user | `"u1"` | `"u1"` | `wakeshift.To("u1")` | Every subscription the user has to the event with these arguments |
| One subscription | `{ user, subscription }` | `Recipient(user=, subscription=)` | `wakeshift.ToSubscription(user, id)` | Exactly that conversation, if it's that user's |
| A user's native subscriptions | `{ native: user }` | `Recipient(native=)` | `wakeshift.ToNative(user)` | Only the user's own MCP Events subscriptions |

One emit takes up to 1,000 recipients, mixed, and makes one delivery to each conversation it reaches, however many times `to` names it (delivery is at least once: a redelivery is marked `"redelivered": true`). `ended` in the result lists the subscriptions that are no longer live: forget them. [Emit to many](/docs/events#emit-to-many) shows the whole pattern.

## More of `call.subscribe`

| Option | What it does |
| --- | --- |
| `reason` | Why your app subscribed the conversation, shown to the person in the console. At most 200 characters. |
| `label` | Names a new session for an agent your app acts for (`agent@label`), so one agent can hold several. |
| `ttl` | Ends the subscription after that long without a delivery or a renewal. Default 30 days. |
| `replay` | Also delivers the latest occurrence with these arguments from that far back, at most 24 hours: for a job whose ID exists only once it's started. |
| `once` | Ends the subscription after its first delivery. |

Subscribing again with the same arguments renews the subscription: `once` and `ttl` become that call's, and `replay` applies only when the subscription is created.

**`replay` with `once`.** `replay` delivers the latest earlier occurrence with the same arguments, and with `once` that delivery ends the subscription. So when argument values repeat (a branch name, a reused job ID), an old occurrence can arrive at once and end a `once` subscription before the one you're waiting for. So the rule is: subscribe before you start the work, without `replay`, whenever your tool has the ID first; use `replay` with `once` only for an ID that exists once the work has started, which is unique to that job; and leave `replay` off on arguments that repeat.

`call.resolveSession()` (Python `resolve_session`, Go `call.ResolveSession(ctx)`) finds or starts the calling conversation's session without subscribing it, for a tool that needs to know which conversation is calling. It doesn't make the conversation wakeable. When nothing names the conversation (no `session_key`, no `conversation_id` and no conversation in the call's `_meta`, such as a Codex thread), it returns empty (`null`, Python `None`, Go `""`) without a request to Wakeshift, so a tool can call it on every call to find its own record of the conversation. Without a user (no `identify` user), only the `session_key` names the conversation: a conversation ID from the harness (a Codex thread) is sent to every MCP server the user has, so it never finds a session by itself, and `resolveSession` returns empty for it too. If your tool needs to know which conversation is calling then, ask the agent for its `session_key`. Until a subscribe or `resolveSession` has reached Wakeshift, `call.session` is empty (`null`, Python `None`, Go an empty `SessionKey`): it's never the raw `session_key` argument, which an agent can forge, so key nothing of your own on that.

## Finding the call without the async context

`wake.currentCall()` finds the call from anywhere in a tool handler's async call chain. Code that lost that chain (a callback queue, a thread pool, a framework that runs handlers elsewhere) passes the request context its handler was given:

| | TypeScript | Python | Go |
| --- | --- | --- | --- |
| In the call chain | `wake.currentCall()` | `wake.current_call()` | `wakeshift.ToolCallFrom(ctx)` |
| With the request context | `wake.currentCall(ctx)` (1.x: `extra`) | `wake.current_call(ctx)` | (the `ctx` always carries it) |

Outside a tool call it throws `no_call`; on a client without a key it returns a call whose `subscribe` does nothing.

## The manifest

The SDK sends your app's manifest itself: your events, added to the ones Wakeshift has or replacing those of the same name. It never removes an event or clears a setting, so processes that share your key keep each other's events. `wake.publishManifest(manifest)` (Python `publish_manifest`, Go `PublishManifest`) replaces the whole manifest instead: `description`, `agent` (what your app may do as its agents), `othersCanWake`, `preapproveTools`, and events defined elsewhere. It's optional, and the way to remove an event: stop defining it in every process, then publish the manifest.
