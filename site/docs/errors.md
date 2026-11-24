# Errors and log codes

Every error the SDKs throw has a code, and the codes are the same in TypeScript, Python, Go and the [HTTP API](/docs/http-api#errors). Each message says what failed and the fix in one clause, and ends with a link to the code's entry on this page:

```text
wakeshift: start_crawl subscribed to crawl.finished, which isn't in its subscribes. Add the event to the tool's subscribes. https://wakeshift.ai/docs/errors#undeclared_subscribe
```

Messages never contain a key, a token, a payload or your own code's error messages.

## Classes

| Class | TypeScript | Python | Go | When |
| --- | --- | --- | --- | --- |
| Base | `WakeshiftError` | `WakeshiftError` | (none) | Everything the SDK throws |
| Wakeshift answered with an error, or didn't answer | `APIError` | `APIError` | `*wakeshift.APIError` | `status`, `code`, the message, and the request ID when the answer has one |
| No answer | `APIConnectionError`, an `APIError` with `status: null` | `APIConnectionError`, an `APIError` with `status=None` | `*APIError` with `Status` 0 | [`unreachable`](#unreachable) or [`timeout`](#timeout); its cause is the transport's error |
| An emit ID reused | `IdReusedError`, an `APIError` | `IdReusedError`, an `APIError` | `*APIError`; `wakeshift.IsCode(err, wakeshift.CodeIDReused)` | [`id_reused`](#id_reused) |
| Caught before sending | `UsageError` | `UsageError`, also a `ValueError` | `*wakeshift.UsageError` | Bad arguments or payload (with `issues`), configuration, an undeclared subscribe, a disabled client, no call |

Catching `APIError` catches outages too. Check one code with `isWakeshiftError(err, "timeout")` in TypeScript (it narrows to the class for that code, and a misspelled code doesn't compile), `isinstance` and `err.code` in Python, and `wakeshift.IsCode(err, wakeshift.CodeTimeout)` in Go.

```ts
try {
  await notification.emit({}, payload, { to: members, id: messageId })
} catch (err) {
  if (isWakeshiftError(err, "rate_limited")) return retryLater(messageId)
  throw err
}
```

Canceling a request is never a Wakeshift error: TypeScript rethrows your `AbortSignal`'s reason unchanged, Python raises `CancelledError`, and Go returns an error that `errors.Is(err, context.Canceled)` (or `context.DeadlineExceeded`) matches.

## Caught before sending

The SDK checks these itself and throws a `UsageError` without asking Wakeshift.

#### `config` {#config}

A bad option or value when you create the client, define an event or attach: an option the SDK doesn't know (`builtinTools: { subcribe: false }`), a malformed key, a core that isn't `https://` (except `localhost`, `127.0.0.1` and `::1`), `public` with `personal` or `authorize`, a second `attach` on the same server, or a tool registered with a built-in tool's name or a schema that names Wakeshift's arguments. **Fix:** the option or value the message names.

#### `disabled` {#disabled}

A call that needs Wakeshift's answer (`createPublishUrl`, `status`, `statusOf`, `end`, `forgetUser`) on a client without a key. Emits and subscribes on a disabled client do nothing instead of throwing. **Fix:** set `WAKESHIFT_KEY`, or check `wake.enabled` (Go `wake.Enabled()`) before the call.

#### `undeclared_subscribe` {#undeclared_subscribe}

A tool subscribed to an event it doesn't name in `subscribes`. **Fix:** add the event to the tool's `subscribes` (Go `wakeshift.Subscribes(…)`).

#### `no_call` {#no_call}

`wake.currentCall()` outside a tool call that subscribes, or Go's `Subscribe` with a nil call. Never on a disabled client. **Fix:** call it inside the handler of a tool you registered through the attachment or named in `wrap`, or pass the request's context: `wake.currentCall(ctx)`.

#### `call_ended` {#call_ended}

A `call.subscribe`, `call.unsubscribe` or `call.resolveSession` after the tool's handler returned. **Fix:** await each one before the handler returns.

#### `identify_failed` {#identify_failed}

Your `identify` threw, failed or took over 10 seconds (logged as `identify_error`). `call.subscribe` throws it (Python raises it, Go returns it); the built-in tools answer `isError` and native requests are refused instead, with a neutral line for the agent (the details go to your logs, never to the agent). Your tool's own code still runs. **Fix:** make `identify` return within 10 seconds, return no user for a credential it can't verify, and throw only when the upstream can't answer. When only your upstream API can verify the credential, see [identify through your upstream API](/docs/users#when-only-your-upstream-api-can-verify-a-credential).

#### `user_required` {#user_required}

`requireUser` (Python `require_user`, Go `RequireUser`) is on and `identify` returned no user, so Wakeshift refused its part of the request; the log line has the same code. `call.subscribe` throws it (Python raises it, Go returns it); the built-in tools answer `isError` and native requests are refused instead, with a line for the agent asking it to sign in. Your tool's own code still runs. **Fix:** nothing, if anonymous callers are meant to be refused (catch it where your tool also serves them); otherwise check that the caller signed in and that `identify` finds their user.

## From Wakeshift

Wakeshift answered with an error: an `APIError`. A few are also caught before sending, with the same code, so the code doesn't depend on which side caught it.

#### `bad_request` {#bad_request}

Something in the request is invalid; the message says what. Also, before sending, more than 1,000 subscriptions in one `statusOf`, or one without its user. **Fix:** check the call against the reference.

#### `bad_arguments` {#bad_arguments}

An event's arguments or payload don't match its schemas (a `UsageError` with `issues` before sending, an `APIError` from Wakeshift); a routing value that isn't `a-z 0-9 . _ -`; recipients that don't fit the event: a personal event's emit, publish URL or status without `to`, or a shared event's with one; or a `replay` that isn't over 0 and at most 24 hours. **Fix:** pass arguments and a payload that match the event's schemas. To route on a case-sensitive ID, [encode it](/docs/events#argument-values).

#### `bad_payload` {#bad_payload}

A post to a publish URL whose body isn't a JSON object that matches the event's payload. **Fix:** send a payload that matches the payload schema.

#### `unknown_event` {#unknown_event}

Your app offers no event by that name, or `call.subscribe` was given another client's event. An emit gets it only after the SDK has sent your events and tried again. **Fix:** define the event in this app with `defineEvent`; the SDK sends your events itself.

#### `unknown_session` {#unknown_session}

The `session_key` isn't a session Wakeshift knows. **Fix:** use the `session_key` from this conversation's own results.

#### `unknown_conversation` {#unknown_conversation}

The conversation the call names isn't one Wakeshift knows. **Fix:** use the `session_key` from this conversation's own results.

#### `other_client` {#other_client}

The `session_key` is a conversation in another MCP client of the same user. **Fix:** only the client that made a subscription can change it; call again without the key.

#### `private_topic` {#private_topic}

A subscription to a topic this app and caller may not read. **Fix:** subscribe only to topics this app and caller may read.

#### `subscription_limit` {#subscription_limit}

A user's 21st live webhook subscription (`events/subscribe`) to your app at once (a `once` subscription stops counting when it delivers), a user's 21st push subscription (`events/stream`), or a push stream past the 2,000 your app can have open at once (`limit: "streams"`). Renewing a webhook subscription is never refused; the client gets [ResourceExhausted](#what-mcp-events-clients-get), InvalidParams for ChatGPT. A signed-in user's voucher subscriptions count toward their own 20. Voucher subscriptions without a user (a push stream bound to a session, say) count toward no user's limit; instead, they count, webhook and push together, toward at most 100 for one event and arguments and 2,000 for your app, and past those a voucher is refused as Forbidden, like a spent one. **Fix:** end subscriptions that are no longer needed.

#### `subscription_stopped` {#subscription_stopped}

The person stopped this subscription. **Fix:** subscribe again only if they ask.

#### `id_reused` {#id_reused}

An emit's `id` was used within the last day for a different emit (another event, arguments, recipients or payload). Nothing was sent. It's an `IdReusedError`. **Fix:** use a new `id` for a different emit; reuse an `id` only to retry the same emit.

#### `rate_limited` {#rate_limited}

Your emits to lists named over 10,000 recipients in a minute. **Fix:** slow down, and retry after the `Retry-After` interval.

#### `too_large` {#too_large}

A request over Wakeshift's limits: an emit's text over 1000 bytes is trimmed, not refused, but a payload over 64 KB, or a request body over 512 KB, is refused. **Fix:** keep text under 1000 bytes and the payload under 64 KB.

#### `unauthenticated` {#unauthenticated}

Wakeshift refused your app's key: it was revoked, or it belongs to another console. Every call fails this way until it's fixed, and the SDK logs [`key_refused`](#log-codes). **Fix:** check `WAKESHIFT_KEY`, and `WAKESHIFT_CORE` if your key came from another console.

#### `waitlisted` {#waitlisted}

The key isn't an app's yet: the person who opened the link from `npx @wakeshift/cli init` signed in while Wakeshift's sign-ups were at their cap, so they're on the waitlist and the app wasn't created. `init` exits 3 with it. **Fix:** wait for the email saying their account is ready, then run `init` again: it keeps the key in `.env` and prints the link again.

#### `no_grant` {#no_grant}

An [agent your app acts for](/docs/advanced) has no grant, or the user revoked it. **Fix:** ask the user to grant your app access first.

#### `unknown_workspace` {#unknown_workspace}

The workspace isn't one of your app's, or it was forgotten. **Fix:** use a workspace ID from `wake.workspace`.

#### `unknown_publish_url` {#unknown_publish_url}

The publish URL ID isn't one of your app's. **Fix:** create a new publish URL: this one was revoked or never existed.

#### `bad_signing_secret` {#bad_signing_secret}

A publish URL's signing secret isn't `whsec_` and the base64 of 24 to 64 bytes. **Fix:** pass the sender's Standard Webhooks secret.

#### `unsupported` {#unsupported}

This Wakeshift doesn't support an option the call used. **Fix:** upgrade the SDK, or leave the option out.

#### `internal` {#internal}

Wakeshift failed. **Fix:** retry later; if it persists, contact Wakeshift with the request ID.

#### `bad_response` {#bad_response}

The answer wasn't Wakeshift's. **Fix:** check that `WAKESHIFT_CORE`, or the `core` option, is the address of the console your key came from.

#### `callback_failed` {#callback_failed}

A webhook URL a native client gave `events/subscribe` didn't pass Wakeshift's verification, which runs before anything is delivered there. Wakeshift POSTs `{"type": "verification", "challenge": "<random>"}`, signed like every delivery ([Standard Webhooks](https://www.standardwebhooks.com/) headers `webhook-id`, `webhook-timestamp` and `webhook-signature`, with the subscription's secret), and the URL must answer with a 2xx whose JSON body echoes the challenge: `{"challenge": "<the same value>"}`. A 2xx without the echo fails (`challenge_failed`), as do a redirect (never followed), any other status, no answer within 10 seconds, and an address that isn't public: a private, LAN or loopback address, or a URL that isn't `https`; the refusal's message then says the client needs a public HTTPS callback and, for most clients, how to be woken without one ([no inbound HTTPS](/docs/mcp-events#no-inbound-https)). An agent that can't take inbound HTTPS uses poll, push or Wakeshift's install instead ([no inbound HTTPS](/docs/mcp-events#no-inbound-https)). The secret is the one `events/subscribe` sent as `delivery.secret`: `whsec_` and the base64 of 24 to 64 bytes. **Fix:** answer the verification POST with the challenge it carries.

## No answer

An `APIConnectionError` (Go: an `*APIError` with `Status` 0). The SDK retries a refused or failed connection itself before throwing, except inside a tool call, where it never retries ([inside a tool call](/docs/sdk#inside-a-tool-call)).

#### `unreachable` {#unreachable}

No answer at all: a wrong core address or a network problem. The message names the core it tried. Inside a tool call, it's also what `call.subscribe` throws, without sending anything, for 30 seconds after 5 of the tool calls' requests in a row failed; the message then says `skipped`, and the SDK logs `core_unavailable`. **Fix:** check that `WAKESHIFT_CORE`, or the `core` option, is the address of the console your key came from.

#### `timeout` {#timeout}

The SDK's own timeout passed (default 20 seconds; inside a tool call, 5 seconds for `call.subscribe` and 1 second for the addition to the result, set with `callTimeouts`). **The outcome is unknown:** the emit may have gone. **Fix:** retry with the same `id`, which emits once.

## What MCP Events clients get

When your server refuses a client that subscribes natively (`events/list`, `events/subscribe`, `events/poll`, `events/stream`), the SDK answers with an MCP Events error. ChatGPT (client name or `User-Agent` starting `openai-mcp`) gets its own codes; every other client gets the standard's. Tests that assert a code should match the client they play.

| Error | Every other client | ChatGPT | When |
| --- | --- | --- | --- |
| NotFound | `-32023` | `-32011` | No such event on your server ([`unknown_event`](#unknown_event)). |
| Forbidden | `-32024` | `-32012` | A tools-only event without a voucher, or `events/poll` of one; a spent, expired or mismatched voucher, or one from a caller who isn't signed in, past the 100 such voucher subscriptions to one event and arguments or the 2,000 to your app ([`subscription_limit`](#subscription_limit)); `authorize` said no; an open event without a signed-in user; a subscription the person stopped ([`subscription_stopped`](#subscription_stopped)). |
| ResourceExhausted | `-32025` | `-32602` (InvalidParams: ChatGPT has no ResourceExhausted) | Past the 20 live webhook subscriptions or 20 push subscriptions a user can have to your app, or the 2,000 push streams your app can have open at once (then `data.limit` is `"streams"`) ([`subscription_limit`](#subscription_limit)). |
| Unsupported | `-32026` | `-32602` (InvalidParams) | A delivery mode Wakeshift doesn't offer for that request: `data` is `{feature: "deliveryMode", value}`, with `value: "push"` for `events/stream` where your server doesn't offer push ([push on your host](/docs/mcp-events#push-on-your-host)). |
| CallbackEndpoint | `-32027` | `-32015` | The webhook failed Wakeshift's verification ([`callback_failed`](#callback_failed)). |
| InvalidParams | `-32602` | `-32602` | Arguments the event refuses ([`bad_arguments`](#bad_arguments)), or a malformed request. |
| HeaderMismatch | `-32020` | `-32020` | A 2026-07-28 request whose `Mcp-Method` or `Mcp-Name` header doesn't match its body, answered with HTTP 400. A request without `Mcp-Name` is accepted ([where Wakeshift differs](/docs/mcp-events#the-standard-and-where-wakeshift-differs)). |
| InternalError | `-32603` | `-32603` | Wakeshift couldn't be reached, or failed. For `events/stream`, also when your `identify` or `authorize` threw, failed or took over 10 seconds as the stream opened: the client retries, and the stream opens once they answer ([`push_open_check_failed`](#log-codes)). |

## In a webhook receiver

When your own handler receives a webhook and emits, answer the sender by what the emit did:

| The emit | Answer the sender |
| --- | --- |
| Succeeded, or `duplicate` | 2xx |
| [`id_reused`](#id_reused), [`bad_arguments`](#bad_arguments), [`unknown_event`](#unknown_event) | 2xx or 422, and log it: a retry won't help |
| [`rate_limited`](#rate_limited) | 503 with a `Retry-After` |
| [`unreachable`](#unreachable), [`timeout`](#timeout), or a 5xx from Wakeshift | 502 or 503, so the sender retries; emit with the sender's delivery ID as `id` so the retry emits once |
| A bad signature, checked before emitting | 401 |

## Log codes

Problems that don't fail a call are logged, never thrown. A Wakeshift failure never changes your tool's result: when Wakeshift's addition to a result can't be had, the agent gets the result exactly as your tool gave it, never an error and never a request to call again, and the SDK logs `block_failed`. Every line carries one of these codes, and never a key, a session key, a token, a header, a payload, tool arguments or your own error messages. Misuse logs once per client; failures that can go on (a refused key, a broken `identify`) log at most once per five minutes per code, with a count. Where each SDK logs, and how to silence it, is on its page under "Logging and secrets".

| Code | Level | When | Fix |
| --- | --- | --- | --- |
| `no_key` | debug | No key, so the client is disabled. Debug, so a server without a key, stdio included, behaves exactly as before | Set `WAKESHIFT_KEY` to turn Wakeshift on |
| `no_identify` | debug | `attach` without `identify`, the default: a `session_key` is then a bearer for its conversation | Add `identify` to tie subscriptions to signed-in users, and `requireUser` to refuse anonymous callers |
| `no_events` | warn | `attach` with no events defined | Define events before `attach` |
| `text_trimmed` | warn | Wakeshift trimmed an emit's text over 1000 bytes; the emit went | Keep text to a short headline and put the details in the payload |
| `rollback_failed` | warn | Ending a failed tool call's subscriptions failed | Nothing to change; Wakeshift retries the cleanup |
| `authorize_failed` | error | `authorize` threw, failed or took over 10 seconds; the subscription was refused | Fix `authorize` |
| `identify_error` | error | `identify` threw, failed or took over 10 seconds; Wakeshift's part of the request was refused ([`identify_failed`](#identify_failed)). The line names the error's type | Make `identify` return within 10 seconds: return no user for a credential it can't verify, and throw only when the upstream can't answer |
| `user_required` | info | `requireUser` is on and `identify` returned no user, so Wakeshift's part of the request was refused ([`user_required`](#user_required)); `tool` names the tool called | Nothing, if anonymous callers are meant to be refused; otherwise sign the caller in, or turn `requireUser` off |
| `native_unavailable` | info | `call.subscribe` returned `unavailable` (`client_unsupported`, `no_user` or `refused`) | For `no_user`, sign callers in; for `refused`, check the event's `authorize`; `client_unsupported` means Wakeshift couldn't hand the client a voucher for a tools-only event ([who may subscribe](/docs/who-may-subscribe#chatgpt-and-other-clients-that-subscribe-natively)) |
| `event_closed` | warn | Wakeshift made an event tools-only on upgrade; the line says how many subscriptions ended | Add `authorize`, `personal` or `public: true` to reopen it |
| `wrap_unknown_tool` | warn | A name in `wrap` the server never lists | Check the tool's name in `wrap` |
| `wrap_arg_conflict` | error | A tool in `wrap` whose schema already names `session_key`, `harness`, `conversation_id` or `wakeshift`; it's left unwrapped | Rename that field |
| `wrap_bad_event` | error | `wrap`'s function gave a tool something other than a list of this client's events; the tool is served unwrapped | Return only events defined on this client, never null, from the wrap function |
| `description_too_long` | warn | A tool that subscribes has a description over 2048 characters once Wakeshift's sentence is added: Claude Code cuts off the rest | Shorten the tool's description |
| `builtin_name_conflict` | error | A tool registered elsewhere has a built-in tool's name; Wakeshift's tool wins | Rename your tool, or turn the built-in off |
| `events_capability_kept` | info | Your server serves its own MCP Events, so Wakeshift serves none there; Wakeshift's events reach MCP Events clients only through your tools | Nothing, if you meant to serve your own |
| `on_subscribe_failed` | error | An event's `onSubscribe` (Python `on_subscribe`, Go `OnSubscribe`) threw, rejected or returned an error; the subscription stands | Fix the event's `onSubscribe` |
| `watch_failed` | warn | A [`watch`](/docs/events#wait-on-work-you-can-only-poll) stopped without emitting: `reason` is `errors` (too many errors in a row) or `emit` (the emit failed and retrying wouldn't help) | Check the watch's check function and the emit |
| `watch_stopped` | info | A `watch` stopped at once because its check's error was marked final (`isFinal`, Python `is_final`, Go `wakeshift.FinalIf`), such as "no such job" | Nothing to change: the watch stopped as asked |
| `watch_timeout` | info | A `watch` reached its timeout before its check returned a payload; once per event | Raise the watch's timeout if the work can take longer |
| `handler_missing` | warn | Go: tool calls arrive over HTTP without `attached.Handler` | Wrap your HTTP handler with `attached.Handler` |
| `key_refused` | error | Wakeshift refused your app's key; each call also fails [`unauthenticated`](#unauthenticated) | Check `WAKESHIFT_KEY` |
| `block_failed` | warn | Wakeshift's addition to a result of a tool that subscribes failed (an error, a timeout, Wakeshift unreachable, a refused key). The result went out as your tool gave it, and what the call subscribed was ended | Nothing to change |
| `core_unavailable` | warn | 5 requests in a row made inside tool calls failed (no answer, a timeout or a 5xx), so for 30 seconds tool calls skip Wakeshift: `call.subscribe` throws [`unreachable`](#unreachable) at once and results go out unchanged. Then one call goes through to check. Emits and watches aren't affected | Nothing to change; tool calls go on without Wakeshift until it answers again |
| `subscribe_in_flight` | warn | A tool returned while one of its subscribes was still running; the SDK waited for it | Await `subscribe`, `unsubscribe` and `resolveSession` before the tool returns |
| `mcp_patch_unavailable` | warn | TypeScript: an `@modelcontextprotocol/sdk` 1.x older than 1.23 | Upgrade `@modelcontextprotocol/sdk` to 1.23 or later |
| `monitor_line_long` | warn | An emit's text (the event's `text`, else its description and arguments) is over 200 bytes. Claude Code's Monitor shows a 480-byte line, about 210 of which are Wakeshift's own fields, so the payload had no room and the agent sees a cut preview. Once per event; a text over 1000 bytes logs `text_trimmed` instead | Keep the text under 200 bytes and put the details in the payload |
| `config` | error | A configuration mistake thrown while attaching or registering a tool ([`config`](#config)), logged too, since some frameworks turn the throw into a bare HTTP 500 | Fix what the message names |
| `push_recheck_failed` | warn | An open push stream asked `identify` and the event's `authorize` again, and either threw, failed or took over 10 seconds; the stream goes on. `event` names the event | Check `identify` and `authorize`: while they fail, an open stream goes on until its last successful check is 15 minutes old (three checks, if your interval is longer) |
| `push_recheck_stale` | warn | An open push stream went 15 minutes (three checks, if your interval is longer) without a successful check, so it ended and its client reconnects with its cursor | Check `identify` and `authorize`: the client retries while they fail |
| `push_open_check_failed` | warn | `identify` or `authorize` threw, failed or took over 10 seconds as a push stream opened (a reconnect, say): the client got a retryable error ([InternalError](#what-mcp-events-clients-get)), not Forbidden | Check `identify` and `authorize`: the client retries, and its stream opens once they answer |
