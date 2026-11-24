# Python SDK

`wakeshift` for Python 3.11 or later, on the official MCP Python SDK (`mcp` 2.3 or later: `MCPServer` or the low-level `Server`) or [FastMCP](#fastmcp) (4, and 2.12+ or 3.x without native MCP Events). It has the same calls as the [TypeScript](/docs/sdk) and [Go](/docs/go) SDKs, in Python's style: `define_event`, `attach`, `@attached.tool` and `call.subscribe`. [MCP frameworks](/docs/frameworks) lists the Python server frameworks it works with. Every call that reaches Wakeshift is async, on asyncio. Its source is at [github.com/wakeshift/wakeshift-python](https://github.com/wakeshift/wakeshift-python), under the Apache License 2.0.

```sh
pip install 'wakeshift[mcp]'        # the official MCP Python SDK
pip install 'wakeshift[fastmcp]'    # FastMCP 4
```

Or `uv add 'wakeshift[mcp]'`. Each extra pins an MCP SDK the SDK supports. On FastMCP 2.12+ or 3.x, install plain `wakeshift`: `attach` works there, and native MCP Events need FastMCP 4 ([FastMCP 2 and 3](#fastmcp-2-and-3)). Never add `[mcp]` beside FastMCP 2 or 3: they can't share an install.

> **Using a coding agent?** Paste the prompt below, add [AGENTS.md](/AGENTS.md) to your repository, or point your agent at [llms.txt](/llms.txt), which links every docs page as Markdown.

```text Prompt for your coding agent
Add Wakeshift to this MCP server following {{SITE}}/AGENTS.md
```

## The client

```python
from wakeshift import Wakeshift

wake = Wakeshift()  # reads WAKESHIFT_KEY; without one, Wakeshift is off
```

| Argument | What it is |
| --- | --- |
| `key` | Your app's key from the console: `wst_` and 50 characters. Default: `WAKESHIFT_KEY` from the environment. An empty or blank key counts as none, and never reads the environment: `Wakeshift("")` is a disabled client even where `WAKESHIFT_KEY` is set, for tests. |
| `required=True` | Raise when there's no key, instead of running disabled: for a hosted server that must have one. |
| `core=` | The address of the console the key came from. Default: `WAKESHIFT_CORE`, else `https://app.wakeshift.ai`. It must be `https://`, except on `localhost`, `127.0.0.1` and `::1`. |
| `timeout=`, `max_retries=` | Seconds per request (default 20), and how many times a refused or failed connection is retried (default 2). A retried emit never wakes twice. |
| `call_timeouts=` | `CallTimeouts(subscribe=5.0, block=1.0)`: how long, in seconds, a tool call waits on Wakeshift, with no retries. `subscribe` covers `call.subscribe`, `call.unsubscribe` and `call.resolve_session`; `block` covers the addition to the tool's result and the rollback of a failed call. Past it, the request fails with `timeout`. After 5 failures in a row, these requests are skipped for 30 seconds (`unreachable` at once), so tool calls go on without Wakeshift ([inside a tool call](/docs/sdk#inside-a-tool-call)). Emits and watches aren't affected. |
| `http_client=` | An `httpx2.AsyncClient` of your own, for proxies or custom transports. Wakeshift never closes it. |

`Wakeshift()` makes no network request. A malformed key, a core that isn't `https://`, or an unknown value raises `UsageError` with code `config`, and the message never contains any part of the key.

**Without a key, the client is disabled**, and logs that once at debug level (`no_key`), so a server without a key prints nothing it didn't before. Your code needs no guards: the same calls work, and your server runs as it would without Wakeshift.

| On a disabled client | What happens |
| --- | --- |
| `define_event` | Works, and checks its schemas and options |
| `attach`, `@attached.tool` | The tool registers as you wrote it; Wakeshift adds no tools, arguments or MCP Events |
| `call.subscribe` | Returns a `Subscription` with `state="disabled"` |
| `wake.current_call()` | Returns a disabled call, anywhere |
| `event.emit` | Checks the payload, sends nothing, and returns `Emitted(matched=0)` |
| Everything that must return Wakeshift's answer: `create_publish_url`, `status`, `status_of`, `end`, `forget_user`… | Raises `UsageError` with code `disabled` |

`wake.enabled` says whether there's a key.

### The client's lifecycle

```python
from contextlib import asynccontextmanager

from mcp.server.mcpserver import MCPServer


@asynccontextmanager
async def lifespan(server: MCPServer):
    yield
    await wake.aclose()  # optional: closes the HTTP client Wakeshift opened on this event loop
```

- The client opens one HTTP client per running event loop, on first use, and drops it when that loop closes. One `Wakeshift` may be shared across threads and event loops.
- `await wake.aclose()` closes the current loop's HTTP client, and cancels every [`watch`](/docs/events#wait-on-work-you-can-only-poll) and `on_subscribe` task the client started. It's idempotent, and needed only if you use `watch` or `on_subscribe`, so they stop with your server. A watch never keeps the process alive: a stdio server exits when stdin ends, sync checks included; if your process goes on after the server returns, `await wake.aclose()`. `async with Wakeshift() as wake:` does the same at the end of the block. A client you passed as `http_client=` is never closed.
- From sync code, run the call on a loop of its own: `asyncio.run(event.emit(args, payload))` in Celery or Flask, `asgiref.sync.async_to_sync(event.emit)(args, payload)` in Django. Never call `asyncio.run` inside an `async def`: `await` there.
- asyncio only; trio isn't supported.

## Events

Define each event once, at startup, before `attach`. Its arguments and payload are pydantic models (or JSON Schema dicts):

```python
from pydantic import BaseModel

from wakeshift import Subscriber


class Channel(BaseModel):
    channel: str


class Message(BaseModel):
    sender: str
    text: str


class Mention(BaseModel):
    channel: str
    sender: str


class Everyone(BaseModel):
    """No arguments: a personal event reaches the users each emit names."""


def can_read_channel(subscriber: Subscriber, args: Channel) -> bool:
    """Shared channels: only members may watch one (can_read is your own check)."""
    return can_read(subscriber.user, args.channel)


message = wake.define_event(
    "chat.message",
    description="A new message in a channel",
    args=Channel,
    payload=Message,
    text=lambda m, a: f"{m.sender} in #{a.channel}: {m.text}",
    authorize=can_read_channel,
)

mention = wake.define_event(
    "chat.mention",
    description="Someone mentioned you",
    args=Everyone,
    payload=Mention,
    personal=True,
    delete_on_delivery=True,
)
```

| Option | What it is |
| --- | --- |
| name | What clients subscribe to: lowercase words joined by dots, at most 64 characters, not starting with `wakeshift.`. |
| `description=` | What happened, as a plain statement: "A new message in a channel". Agents read it in the description of every tool that subscribes to the event. |
| `args=` | What a subscriber picks. Its required fields route occurrences, and their values are `a-z 0-9 . _ -` ([argument values](/docs/events#argument-values)); an optional field filters on the payload field of the same name. |
| `payload=` | What each occurrence carries. `emit` checks it first. |
| `text=` | The line the agent reads first, `text(payload, args)`. Optional: without it the agent reads the description and the arguments ("A crawl finished (job 3f2a…)"). |
| `personal=True` | Each subscription belongs to its subscriber, and each occurrence goes to the recipients you name ([personal events](/docs/events#personal-events)). |
| `authorize=` | Opens the event to `subscribe_events` and MCP Events clients, gated by `authorize(subscriber, args)`: [below](#sign-in-identify-and-authorize). |
| `public=True` | Anyone may subscribe. Only for streams anyone may watch. Can't be combined with `personal` or `authorize`. |
| `on_subscribe=` | Runs once for each new subscription, however it was made, with a `Subscribed(args, user, via, tool, context)`, in the background: where a [`watch`](/docs/events#wait-on-work-you-can-only-poll) starts. May be async. `context` is what the subscribing call passed as `call.subscribe(..., context=)`, never sent to Wakeshift; for a tool it runs in a copy of the call's `contextvars` ([details](/docs/events#wait-on-work-you-can-only-poll)). A watch already running for the same event and arguments keeps its first subscriber's `context`; a later subscriber's doesn't reach it. |
| `delete_on_delivery=True` | Wakeshift deletes each occurrence's text and payload once it's delivered. |
| `title=`, `topic=`, `filter=` | A short title for clients that show one; your own topic template and payload filter, when the defaults don't fit. |

**Who may subscribe.** An event that declares neither `personal`, `authorize` nor `public` can be subscribed only by your own tools: no `subscribe_events`, no native subscription. That's the safe default for IDs your tool mints. Do you sign people in? No: keep the default. Yes: `personal` for things that belong to one user, `authorize` for shared resources. See [who may subscribe](/docs/who-may-subscribe).

With pydantic models, `text` and `authorize` are type-checked from `args` and `payload`, and `emit` and `subscribe` take a model instance or a dict. `event.authorize` is the function you passed (or `None`), so a test can call it directly.

`event.watch(args, check, every=30, timeout=3600, max_errors=5, is_final=None, id=None, to=None)` calls `check` (sync or async, no arguments) until it returns the payload, then emits once, and returns the `asyncio.Task` running it: [wait on work you can only poll](/docs/events#wait-on-work-you-can-only-poll).

## Attach

```python
from wakeshift import Identity, Request


def identify(r: Request) -> Identity | None:
    """Your own sign-in: user_of is your code, from the token your server's auth verified."""
    return Identity(user=user_of(r.token)) if r.token else None


server = MCPServer("chat", lifespan=lifespan)  # the lifespan above
attached = wake.attach(server, identify=identify, builtin_tools={"wait": True})
```

| Argument | What it is | Default |
| --- | --- | --- |
| `identify=` | Who is behind a request, from your own sign-in: [below](#sign-in-identify-and-authorize). | no user |
| `require_user=True` | Refuse Wakeshift's part of a request with no user, so a `session_key` is never a bearer for its conversation. Set it whenever your server requires sign-in. | anonymous allowed |
| `wrap=` | Tools registered elsewhere that subscribe, by name: `wrap={"start_crawl": [crawl_finished]}`. See [tools registered elsewhere](/docs/low-level#python). | none |
| `builtin_tools=` | Which of Wakeshift's own tools to serve: `subscribe` (`subscribe_events`), `list` (`list_subscriptions`) and `unsubscribe` (`unsubscribe_events`) are on unless `False`; `wait` ([Claude Code's wait tool](/docs/agents#the-wait-tool-for-claude-code)) is off unless `True`. `builtin_tools=False` turns them all off. `BUILTIN_TOOL_NAMES` lists their names. | the first three, where something can subscribe |
| `is_native_client=` | For your own agents that subscribe natively only: see [advanced](/docs/advanced). | the SDK's own checks |
| `push=` | `False` to offer MCP Events clients webhook and poll only, never push (`events/stream`), for a host where something in front of your server forces JSON answers; or `PushOptions` (`max_stream_ms`, `max_heartbeat_ms`, `reauthorize_ms`) ([push options](/docs/mcp-events#push-options)). | push offered where your transport can stream (not `json_response=True`) |

`attach` takes an `MCPServer`, a FastMCP server (4, 3.x or 2.12+) or a low-level `Server`, once per server. It works at the protocol layer, so it wraps none of your callbacks: `tools/list` gets Wakeshift's tools after yours, and every `tools/call` has Wakeshift's arguments taken out before your middleware, validation and code see them. `subscribe_events` is listed only when an event is open to it, and `list_subscriptions` and `unsubscribe_events` only when something on the server can subscribe (a tool declared with `subscribes`, or an open event). Without `identify` those two act on the calling conversation, by its `session_key`. A server that already serves its own MCP Events keeps them, and Wakeshift serves none there (`events_capability_kept`). That's decided when you attach, from the `events/*` handlers and the events extension the server has then: add your own before `attach`. One added after `attach` replaces Wakeshift's for that method, while Wakeshift's capability stays. An unknown option or value raises `UsageError` (`config`). On a missing `mcp` or an unsupported version, `attach` raises `ImportError` naming the extra to install.

## Tools that subscribe

Register a tool that subscribes with `@attached.tool`, as you would with `@server.tool`, and name the events it subscribes to. Add a parameter annotated `ToolCall`; the SDK fills it and hides it from the tool's schema:

```python
from wakeshift import Recipient, ToolCall

members: dict[str, list[Recipient]] = {}  # your app's own membership


@attached.tool(subscribes=[message, mention])
async def join(channel: str, call: ToolCall) -> str:
    """Join a channel, and be woken by its messages and your mentions in it."""
    if not can_read(call.user, channel):  # your own check, before subscribing
        raise ValueError("You can't read that channel.")
    await call.subscribe(message, {"channel": channel}, reason=f"Member of #{channel}")
    sub = await call.subscribe(mention, {})
    if sub.state in ("created", "renewed"):
        members.setdefault(channel, []).append(Recipient(user=sub.user, subscription=sub.id))
    return f"You joined #{channel}."
```

- `subscribes` lists the events the tool subscribes the calling conversation to; `call.subscribe` takes only those, and raises `UsageError` (`undeclared_subscribe`) for any other. `subscribes=[]`, the default, is a tool that subscribes to nothing: it needs no `ToolCall` parameter (one you declare is still filled), never raises, with or without a key, and with a key still takes `session_key`, so `call.unsubscribe` works.
- A tool that subscribes must be `async def`, to `await call.subscribe`. A plain `def` tool works too when it subscribes to nothing (`subscribes=[]`): it runs in a worker thread, as `MCPServer` and FastMCP run one, and gets its `ToolCall` (`call.user`, `wake.current_call()`) there. `@attached.tool(subscribes=[...])` on a plain `def` raises `TypeError`. `@attached.tool` without parentheses works too, and returns your function unchanged. Other options pass through to the server's own registration (on FastMCP, `Tool.from_function`'s).
- `tools/list` gives the tool Wakeshift's arguments (`session_key`, `harness` and `conversation_id`), two sentences after its description saying it's how your app notifies the agent of those events and that the result may ask for a listener (about 300 characters: [the text](/docs/events#subscribe-from-your-own-tools)), and a `wakeshift` field in its output schema. Your function never sees those arguments. The first `tools/list` asks Wakeshift for your app's name once, waiting at most 3 seconds (else the sentence says "this app"), and remembers it; after a failure it asks again a minute later. A one-line docstring that doesn't end its last sentence gets a period before the sentence. Keep the description under 2048 characters with the sentence: Claude Code keeps only that many, and the SDK logs `description_too_long` when it's over.
- The reply gets what the agent needs to be woken, after your result. A call that raises, returns `isError`, returns output its schema refuses, or is canceled ends the subscriptions it made.
- **Your tool decides who may subscribe to what.** `call.subscribe` runs no `authorize` for Claude Code, Codex and the other agents Wakeshift bridges, so when the caller names a resource, check they may see it first. When your tool minted the ID, as in the [quickstart](/docs/quickstart), there's nothing to check.

`ToolCall` has `user` (who `identify` says is calling, or `None`), `tool`, `native` (the client subscribes to your events itself, as ChatGPT does), `native_conversation`, `recognized` (which agent program Wakeshift recognized), `session` (a `SessionKey`, which never prints: the conversation's session once Wakeshift has resolved it, after a `call.subscribe` that reached Wakeshift or `await call.resolve_session()`, and `None` before that; never the raw `session_key` argument, which an agent can forge, so key nothing of your own on that), `wakes` ([whether Wakeshift wakes the conversation](#skip-your-own-waiting-only-when-it-wakes)) and `context` (the tool's MCP context). `call.user` is Wakeshift's view of the caller: your own authorization rests on your own verified credential. `await call.unsubscribe(event, args)` ends the calling conversation's subscription with those arguments.

Tools registered some other way (a low-level `Server`'s handlers, a generated catalog) subscribe through `wrap` and `wake.current_call()`: [tools registered elsewhere](/docs/low-level#python). `wrap` maps tool names to their events, or is a function of a tool's name that returns its events, or `None` for a tool that doesn't subscribe. The function is asked on every `tools/list` and `tools/call`, so it covers tools named at run time: `wake.attach(server, wrap=lambda tool: [build] if tool.startswith("watch_") else None)`. Outside a tool call, `wake.current_call()` raises `UsageError` with code `no_call` (check with `except UsageError as err: if err.code == "no_call": ...`); without a key, it returns a disabled call instead.

## Subscribing

```python
from datetime import timedelta


async def watch_after_paid_work(call: ToolCall, channel: str) -> None:
    # The work is done and paid for; the subscription is a convenience.
    try:
        await call.subscribe(message, {"channel": channel}, replay="1h", ttl=timedelta(days=7), once=True)
    except Exception:
        pass  # the work stands; the agent just won't be woken
```

| Option | What it does |
| --- | --- |
| `once=True` | Ends the subscription after its first delivery: "tell me when this finishes". |
| `replay=` | Also delivers the latest occurrence with these arguments from that far back (over 0, at most 24 hours), for a job that may finish before the subscribe. |
| `ttl=` | Ends it after that long without a delivery or a renewal (default 30 days). |
| `reason=` | Why, shown in the console (at most 200 characters). |
| `label=` | Names a new session, for an app that acts for its agents ([advanced](/docs/advanced)). |

Durations take a `timedelta`, seconds, or a string such as `"30m"`, `"2h"` or `"7d"`. Subscribing again with the same arguments renews the subscription and sets `once` and `ttl` to that call's; `replay` applies only when it's created.

`call.subscribe` returns a `Subscription`, a frozen dataclass whose `state` says what happened:

| `state` | Fields | Meaning |
| --- | --- | --- |
| `"created"`, `"renewed"` | `id`, `user`, `session`, `recognized` | A conversation subscription, new or renewed. Store `id` and `user` to emit to this conversation or end it later. |
| `"native"` | `user`, `recognized` | The client subscribes itself through MCP Events, and the reply tells it what to subscribe to, with a one-time voucher when the event is open only to your tools. `user` is `""` for an anonymous caller. |
| `"unavailable"` | `reason`: `"client_unsupported"`, `"no_user"` or `"refused"` | Nothing happened: the event is open only to your tools, the client subscribes only itself, and Wakeshift couldn't hand it a voucher; or such a client has no signed-in user on a `personal`, `authorize` or `public` event; or the event's `authorize` refused this caller. Logged once per tool and reason. |
| `"disabled"` | | The client is disabled. |

### Skip your own waiting only when it wakes

`call.wakes` is `True` when Wakeshift will wake the conversation for what the call subscribed: a subscribe in this call made or renewed a subscription it hasn't ended again, and a wake is confirmed: the conversation's agent program has a wake path that runs now, with no choice from the user still to come. A client that subscribes itself (`native`: a ChatGPT Work chat on the web) counts as woken once a subscribe comes back `"native"`, since it's woken through MCP Events; one that comes back `"unavailable"` doesn't. It's `False` before any subscribe, after one that raised, once the call has unsubscribed what it subscribed, while the user's choice of how to be notified is pending (Claude Code asks it the first time it watches something; a later call is `True` once their pick runs), for an agent Wakeshift can't wake (ChatGPT's desktop app, ChatGPT through a bridged tool, Codex without its connector, a hooks harness without its hooks, an unrecognized program), and on a disabled client. Skip your own polling or waiting only when it's `True`; otherwise behave as without Wakeshift:

```python
from wakeshift import APIError


async def wait_for_reply(call: ToolCall, channel: str) -> str:
    try:
        await call.subscribe(message, {"channel": channel}, once=True)
    except APIError:
        pass  # go on without wakes
    if call.wakes:
        return f"Waiting for the next message in #{channel}."
    return await next_message(channel)  # as without Wakeshift
```

Don't promise the wake in your own text: Wakeshift's addition tells the agent.

When the agent asks for a wait longer than its program lets one tool call run (Claude Code gives up after 60 seconds), return at once if Wakeshift wakes the conversation: `call.call_limit` is that limit in seconds (60 for Claude Code, 300 for Codex; `None` before a subscribe and when it isn't known), and `call.should_skip_wait(seconds)` is `True` only when `call.wakes` is `True` and the wait reaches it. Don't promise the wake in your own text: Wakeshift's addition tells the agent. Keep the subscription either way ([a wait longer than the program allows](/docs/events#a-wait-longer-than-the-agents-program-allows)):

```python
async def render(prompt: str, wait_seconds: float | None, call: ToolCall) -> str:
    job = await start_render(prompt)
    try:
        await call.subscribe(render_finished, {"job": job}, once=True)
    except APIError:
        pass  # go on without wakes
    if call.should_skip_wait(wait_seconds):
        return f"Started render {job}."
    return await wait_for_render(job, wait_seconds)  # as without Wakeshift
```

In a unit test, `tool_call()` from `wakeshift.testing` gives a call whose `wakes` is `True` after a subscribe (a native one's too); `tool_call(wakes=False)` tests the path without Wakeshift, and `tool_call(call_limit=60)` a program with Claude Code's limit.

## Emit

```python
async def on_message(channel: str, sender: str, text: str) -> None:
    await message.emit(Channel(channel=channel), Message(sender=sender, text=text))


async def on_mention(channel: str, sender: str, mentioned: list[str], message_id: str) -> None:
    out = await mention.emit({}, {"channel": channel, "sender": sender}, to=mentioned, id=message_id)
    if out.unmatched:
        pass  # nobody was subscribed for these users
```

- A shared event goes to every conversation subscribed with those arguments. A personal event needs `to=`: a user, `Recipient(user=, subscription=)`, `Recipient(native=)`, or a list of up to 1,000 of them ([emit to many](/docs/events#emit-to-many)). Take `to` from your own records of who owns what, never from who's subscribed. `to` on a shared event, or none on a personal one, raises `UsageError` (`bad_arguments`).
- `id=` makes retries emit once, for a day: a retry returns the first emit's result with `duplicate=True`. The same `id` on a different emit raises `IdReusedError` and sends nothing.
- `emit` returns `Emitted`: `matched` (the subscriptions that received it, not the agents awake now), `duplicate`, `unmatched` (users and native recipients nothing reached), `ended` (subscription recipients no longer live: forget them) and `text_trimmed` (Wakeshift trimmed a text over 1000 bytes).
- You can emit before any agent has connected, or from a process that never attaches: when Wakeshift doesn't know the event yet, the SDK sends your events once and emits again.

`await event.status(args, to=None)` counts the conversations subscribed with those arguments and how many are connected now; `await event.status_of([Recipient(user=, subscription=)])` reads up to 1,000 subscriptions by ID ([who is subscribed](/docs/events#who-is-subscribed)). `await event.end(user, id)` ends one subscription and tells its conversation; `await wake.forget_user(user)` deletes what Wakeshift keeps for one of your users.

## Publish URLs

```python
async def webhook_url_for(channel: str) -> str | None:
    if not wake.enabled:  # a disabled client can't make one
        return None
    made = await message.create_publish_url({"channel": channel})
    return made.url
```

- `create_publish_url(args, to=None, signing_secret=None)` makes a new URL that emits the event with those arguments each time something posts to it ([publish URLs](/docs/publish-urls)). For a personal event, `to` is the one user it's for.
- `signing_secret=` is the sender's Standard Webhooks secret (`whsec_…`): the URL then accepts only posts signed with it. The result's `signing` has its fingerprint, never the secret.
- The URL is a credential. A `PublishURL` prints without its token; pass `.url` where it's meant to go. `await wake.revoke_publish_url(made.id)` turns it off. [Use them safely](/docs/publish-urls#use-them-safely).

## Sign-in: identify and authorize

`identify` gets a `wakeshift.Request` and returns an `Identity(user=…, client=…)`, or `None` for no user:

- `Request.token` is the access token your server's auth verified, or `None`: an `mcp.server.auth.provider.AccessToken` (`token`, `client_id`, `scopes`, `expires_at`, `subject`, `claims`); on FastMCP, FastMCP's own `AccessToken`, a subclass of it with every claim of the token in `claims`. `Request.headers` are the HTTP request's, as the client sent them, and aren't verified. `Request.client_name` is the MCP client's name; `Request.raw` is the MCP SDK's request context.
- `identify` may be async. It runs only when Wakeshift needs to know (a call of a tool that subscribes, a built-in tool, MCP Events), at most once per request, never for `initialize`, `tools/list` or your other tools.
- Returning `None` means anonymous. If `identify` raises or takes over 10 seconds, Wakeshift's part of the request is refused (`call.subscribe` raises `UsageError` `identify_failed`), and your tool's own code still runs. With `require_user=True`, an anonymous caller is refused the same way, with `user_required`.
- `Identity.client` names the MCP client for a request that doesn't carry its name: a stateless server never sees `initialize`.

`authorize(subscriber, args)` decides every subscription made through `subscribe_events` or MCP Events, on every request, and runs before a tool tells an MCP Events client to subscribe. `subscriber` is a `Subscriber`: `user` (from `identify`), `client`, `native` and `request`. It returns a `bool` and may be async. Raising, or taking over 10 seconds, refuses and logs `authorize_failed` with the error's type. Never decide on `subscriber.client` or `subscriber.native`: the client asserts both. `authorize` runs on every poll, about every 10 seconds per subscriber, so cache your answer in your own layer, keyed by user and arguments.

## Logging and secrets

- The SDK logs through `logging.getLogger("wakeshift")` and adds no handler, so its warnings reach your logs however you configure logging, and Python's last-resort handler prints them when you configure nothing. Each line carries its code in `extra` (`wakeshift_code`, and `wakeshift_event` or `wakeshift_tool` where they apply). The codes are on [errors and log codes](/docs/errors#log-codes).
- Misuse logs once per client; failures that may go on (`identify_error`, `authorize_failed`, `key_refused`, `block_failed`) log at most once every five minutes per code, with a count.
- A Wakeshift failure never changes your tool's result. If Wakeshift's addition to a result can't be had (an error, a timeout, Wakeshift unreachable, a refused key), the agent gets your result exactly as your tool returned it, never an error and never a request to call again; what the call subscribed is ended, and `block_failed` is logged.
- The SDK's own HTTP requests don't log a line each through `httpx2`/`httpcore2`, whatever level you set; your own requests log as before.
- It never logs keys, session keys, your callers' tokens or headers, signing secrets, publish URL tokens, payloads, tool arguments or your own error messages.
- A `SessionKey` (`call.session`, `Subscription.session`; `call.session` is `None` until Wakeshift has resolved the conversation, never the raw `session_key` argument; `await call.resolve_session()` resolves it) prints as `[redacted]` in `repr`, `str`, f-strings and tracebacks, including Sentry's captured locals, and refuses to pickle; `reveal()` returns the key itself, only for sending it where it must go. A `Request` prints only the client's name and whether there's a token, and a `PublishURL` prints without its token.
- For logs the SDK can't reach (your own argument logging, HTTP body logs, Sentry's `before_send`), `redact_secrets(text)` replaces every Wakeshift secret with `[redacted]`.

### Stdio servers

The SDK never writes to stdout, so it's safe in a stdio server, where stdout carries the JSON-RPC. It logs only through `logging`: Python's last-resort handler writes to stderr, and `MCPServer` and FastMCP log to stderr too. If you configure logging yourself, send it to stderr or a file, never stdout: `logging.basicConfig(stream=sys.stderr)` does, and so does a plain `logging.StreamHandler()`.

**What a stdio server can't do.** It runs only while the agent has it open, so a [`watch`](/docs/events#wait-on-work-you-can-only-poll) or any background check of yours ends when the agent closes the server, and a job that finishes hours later may lose its wake: emit long jobs from a webhook or a hosted server. And ChatGPT connects only to remote servers over HTTPS, so it can't reach a stdio server; serve one over HTTP to reach it.

**Where the key lives when your stdio server is a package.** A stdio server runs on each user's machine, so never ship your app key inside the package: anyone could read it and act as your app. Either each person who runs it creates their own app in the console and sets their own `WAKESHIFT_KEY` (without one, the server runs as it would without Wakeshift), or you serve Wakeshift from your hosted HTTP server, where the key stays with you, and the package talks to that.

## Errors

| Class | When |
| --- | --- |
| `WakeshiftError` | Everything the SDK raises. Each has a `code`. |
| `APIError` | Talking to Wakeshift failed: its answer (`status`, `code`, `request_id`), or no answer. |
| `APIConnectionError` (an `APIError`, `status=None`) | No answer: `unreachable`, or `timeout`, whose outcome is unknown, so retry with the same `id`. |
| `IdReusedError` (an `APIError`) | An emit's `id` was used for a different emit. |
| `UsageError` (also a `ValueError`) | A mistake caught before sending; `issues` lists what in the arguments or payload was wrong. |

`except APIError` catches outages too. Each message says what failed, the fix, and a link to its code:

```python
from wakeshift import APIConnectionError, IdReusedError, UsageError


async def emit_message(channel: str, sender: str, text: str, delivery: str) -> int:
    try:
        out = await message.emit({"channel": channel}, {"sender": sender, "text": text}, id=delivery)
    except IdReusedError:
        return 422  # a different emit had this id: don't retry
    except APIConnectionError:
        return 503  # no answer: the sender retries, with the same id
    except UsageError as err:
        if err.code == "bad_arguments":
            return 422
        raise
    return 202 if out.matched or out.duplicate else 200
```

| Code | When |
| --- | --- |
| [`bad_arguments`](/docs/errors#bad_arguments) | Arguments, a payload or recipients that don't fit the event: a personal event without `to`, a shared one with it, a `replay` out of bounds. |
| [`unknown_event`](/docs/errors#unknown_event) | Your app offers no event by that name, or the event is another client's. |
| [`unauthenticated`](/docs/errors#unauthenticated) | Wakeshift refused your key: it was revoked, or it belongs to another console. |
| [`id_reused`](/docs/errors#id_reused) | An emit's `id` was used within the last day for a different emit. |
| [`unreachable`](/docs/errors#unreachable), [`timeout`](/docs/errors#timeout) | No answer. After a timeout the outcome is unknown: retry with the same `id`. |
| [`config`](/docs/errors#config) | A bad option or value at `Wakeshift()`, `define_event` or `attach`. |
| [`disabled`](/docs/errors#disabled) | A call that needs Wakeshift on a disabled client. Check `wake.enabled` first. |
| [`undeclared_subscribe`](/docs/errors#undeclared_subscribe) | A tool subscribed to an event it doesn't list in `subscribes`. |
| [`no_call`](/docs/errors#no_call), [`call_ended`](/docs/errors#call_ended) | `current_call()` outside a tool call, or a call used after its tool returned. |
| [`identify_failed`](/docs/errors#identify_failed) | `identify` failed or timed out. |
| [`user_required`](/docs/errors#user_required) | `require_user` refused an anonymous caller. |

Every code, with its fix, is on [errors](/docs/errors).

### In a webhook receiver

When your own handler receives a webhook and emits, answer the sender so it retries only what can succeed:

| The emit | Answer the sender |
| --- | --- |
| Succeeds, or `duplicate=True` | 2xx |
| `IdReusedError`, or `bad_arguments` or `unknown_event` | 2xx or 422, no retry; log it |
| `APIError` with `rate_limited` (429) | 503 with `Retry-After` |
| `APIConnectionError` (`unreachable`, `timeout`), or a Wakeshift 5xx | 502 or 503, so the sender retries with the same delivery ID as `id` |
| A bad signature, checked before emitting | 401 |

## FastMCP

`attach` takes a [FastMCP](https://gofastmcp.com) 4 server as it takes an `MCPServer`, with the same calls. Install `wakeshift[fastmcp]`:

```python fastmcp_server.py
from fastmcp import FastMCP
from pydantic import BaseModel

from wakeshift import ToolCall, Wakeshift
from wakeshift.fastmcp import identify_by_claim

wake = Wakeshift()


class Repo(BaseModel):
    repo: str


class Build(BaseModel):
    conclusion: str


def can_watch(subscriber, args: Repo) -> bool:
    """Shared repos: only people who can see one may watch it (can_see_repo is your own check)."""
    return can_see_repo(subscriber.user, args.repo)


build = wake.define_event(
    "ci.build",
    description="A CI run on a repo finished",
    args=Repo,
    payload=Build,
    text=lambda b, a: f"CI {b.conclusion} on {a.repo}",
    authorize=can_watch,
)

# verifier is your FastMCP auth (a JWTVerifier, an OAuth provider).
mcp = FastMCP("ci", auth=verifier)
attached = wake.attach(mcp, identify=identify_by_claim("sub"), require_user=True)


@attached.tool(tags={"ci"}, subscribes=[build])
async def watch(repo: str, call: ToolCall) -> str:
    """Wake me when a CI run on a repo finishes."""
    if not can_see_repo(call.user, repo):
        raise ValueError("You can't see that repo.")
    await call.subscribe(build, {"repo": repo}, reason=f"Watching {repo}")
    return f"Watching {repo}."


app = mcp.http_app()
```

- `identify_by_claim(claim, client_claim=None)` names the user by a claim of the request's verified FastMCP access token (`sub` falls back to the token's subject), and the client by another.
- `@attached.tool` takes FastMCP's own tool options (`tags`, `annotations`, `meta`, `output_schema`, `timeout`). A `Context` parameter and FastMCP dependencies are filled as in any FastMCP tool. A tool that subscribes can't be a background task (`task=True`).
- Your FastMCP middleware sees the tool's result before Wakeshift adds to it, and never sees Wakeshift's arguments.
- Attach the server you run. Servers you mount on it keep serving their own tools.
- If your code replaces the MCP SDK's own `tools/call` handler (`server._mcp_server.add_request_handler("tools/call", CallToolRequestParams, handler)` on mcp 2.x), before or after `attach`, Wakeshift's arguments are still taken out before it runs, including when your handler wraps the previous one: Wakeshift runs as the MCP SDK's own middleware (`server._mcp_server.middleware`), not as a handler. Keep that list's entries when you add your own middleware to it.
- Serve it as FastMCP serves any server: `mcp.http_app()`, stateful or with `stateless_http=True`, `mcp.run(transport="http")`, or stdio.

### FastMCP 2 and 3

`attach` works on FastMCP 2.12 and later and on 3.x, which are built on `mcp` 1.x. Install plain `wakeshift` beside them, never `[mcp]` or `[fastmcp]`:

```sh
pip install wakeshift
```

Everything on this page works the same way there: `@attached.tool(subscribes=...)`, `wrap=` with `wake.current_call()`, the built-in tools, `identify` (with FastMCP's verified token in `Request.token`), `require_user`, emit, publish URLs and status. Wakeshift's arguments are taken out of every call before any of your middleware sees it, whether you added it before `attach` or after, and before FastMCP's own logging, its input validation and your tools.

Native MCP Events need FastMCP 4. `mcp` 1.x has no `events/*` methods and no protocol 2026-07-28, so on FastMCP 2 and 3 every client subscribes through Wakeshift's tools and gets the block, including clients that speak MCP Events themselves. `is_native_client` has nothing to choose there. The wait tool needs FastMCP 4 too: `builtin_tools={"wait": True}` raises `UsageError` (`config`). FastMCP before 2.12 raises `ImportError`.

To get native MCP Events, upgrade to FastMCP 4 (`pip install -U "fastmcp>=4.1,<5" "wakeshift[fastmcp]"`) with FastMCP's guides, [from FastMCP 3](https://gofastmcp.com/getting-started/upgrading/from-fastmcp-3) and [from FastMCP 2](https://gofastmcp.com/getting-started/upgrading/from-fastmcp-2). Your Wakeshift code doesn't change.
