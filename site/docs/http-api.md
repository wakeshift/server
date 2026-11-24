# HTTP API

The SDKs for [TypeScript](/docs/sdk), [Go](/docs/go) and [Python](/docs/python) wrap this API. Use it directly from any other language: [other languages](/docs/other-languages) says what your server does. Every path below is under `{{APP}}/api/apps/v1`, takes and returns JSON, and answers errors as `{ "error": { "code", "message" } }`. A request body field this page doesn't list is a `400`.

## Authentication

Your key is an Ed25519 private key. Don't send it. Instead, send a fresh signed assertion with each request:

```http
POST /api/apps/v1/events/emit
Authorization: Bearer <assertion>
Content-Type: application/json
```

The assertion is a compact JWS:

- **Header:** `{"alg": "EdDSA", "typ": "app+jwt", "kid": "<kid>"}`. The `kid` is the first 16 bytes of the SHA-256 of your public key (the 32 raw bytes), base64url: 22 characters. It names your key, and through it your app.
- **Claims:** `iss` and `sub` are your app's ID (or both the `kid`); `aud` is Wakeshift's URL without a trailing slash, plus `/api/apps` (`{{APP}}/api/apps`); `iat` is now, in whole seconds (an integer); `exp` is `iat + 120`; `jti` is 16 random bytes, base64url.
- **Signature:** Ed25519 over `base64url(header) + "." + base64url(claims)`. All base64url is without padding.

Wakeshift refuses an assertion that has expired, lives longer than 5 minutes, is issued more than a minute in the future, has no `jti`, or reuses one. An assertion without a `kid` still works, with `iss` and `sub` your app's ID.

`GET /app` says which app and key signed the assertion: `{"id", "name", "key": {"key_id", "name"}}`. Call it once if your code needs the app's ID and your key doesn't carry it.

To read a key, trim it and strip `wst_`. What's left is one of two formats:

- **50 characters, no `_`:** the current format. Decode them as one base58 number (the alphabet `123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz`), refuse one of 2^288 or more, and write it as 36 big-endian bytes. The first 32 are the Ed25519 seed; the last 4 must be the CRC32 (IEEE, big-endian) of the seed, or the key is mistyped. The key doesn't name your app: sign with `iss` and `sub` the `kid`, and call `GET /app` if you need the ID.
- **`test_<app>_<seed>` or `live_<app>_<seed>`:** an older key. Split on the first `_` (the mode) and the next `_` (the app ID). The rest is the 32-byte seed, base64url without padding. App IDs never contain `_`; seeds can.

A current key defaults to `{{APP}}`. A key from another console is refused here as unknown: send it to that console's address.

## The caller

Calls made for a conversation (subscribing it, unsubscribing it, the addition to its tool results) carry a `call` object, built from the tool call:

```json
{ "user": "u_123", "client": "Cursor", "meta": { }, "user_agent": "Cursor/1.0.0", "session": "<session key>", "harness": "grok-bot", "conversation": "bc-…", "tool": "join" }
```

| Field | What it is |
| --- | --- |
| `client` | The MCP client's name, `clientInfo.name` from `initialize`. It tells Wakeshift which agent this is: `claude-code`, `codex-mcp-client`, `Cursor`, `copilot-cli`, `opencode`, `antigravity-client`, and others. Send it whenever you have it: it wins over `meta` and `user_agent`. A stateless server doesn't have it: see [stateless servers](/docs/other-languages#stateless-servers). |
| `meta` | The tool call's `_meta`, as the client sent it. Codex sends its thread ID there. Without `client`, it tells Wakeshift Claude Code, Codex and Antigravity apart. |
| `user_agent` | The HTTP request's `User-Agent` header. Without `client`, it tells Wakeshift opencode, Cursor and GitHub Copilot CLI apart. |
| `session` | The `session_key` argument the agent passed back, if any. It's an opaque key: pass it on unchanged. |
| `harness` | The `harness` argument: the program the agent says it runs in. A hint, not a credential: it only refines what `client`, `meta` and `user_agent` say (a `Cursor` call may be Grok Bot's or a Cursor cloud agent's), or names the agent when they say nothing. One that contradicts them is ignored. Pass it on unchanged. |
| `conversation` | The `conversation_id` argument, if any. Cursor tells MCP servers nothing about the conversation, so its agents (and Grok Bot) pass their `CURSOR_CONVERSATION_ID`. Pass it on unchanged. |
| `tool` | The name of the tool being called, as your server lists it. What Wakeshift adds for the agent's wake-ups works only in that tool's result, so send it. |
| `user` | Optional. Your own ID for the person ([what it can contain](/docs/users#user-ids)). |

`client` is at most 200 characters, `session` at most 100 and `user` at most 200. Longer is a `400`.

Give a tool of yours that subscribes the caller three optional string arguments, described this way, and leave them out of `required`. The `subscribe_events` tool has them too:

```json
"session_key": { "type": "string", "description": "This conversation's session_key, from an earlier result (a secret token, or a handle such as claude-code#2). Keep it to this conversation; don't share or log it." },
"harness": { "type": "string", "description": "The program you're running in: codex, claude-code, cursor (local CLI or IDE), cursor-cloud (Cursor cloud agent), grok-bot, copilot-cli, opencode, pi, antigravity, openclaw (even when it runs your turns in Codex), hermes, chatgpt, or your program's name if it isn't listed" },
"conversation_id": { "type": "string", "description": "Your own ID for this conversation, if your program gives you one. Cursor and Grok Bot: the output of `printenv CURSOR_CONVERSATION_ID`. Codex and Antigravity send it automatically; others leave it empty" }
```

## Handling secrets

Some of what passes through your server is a credential for one conversation. Anyone who has it can act as that conversation: read its events, subscribe it to more, or end its subscriptions.

- the `session_key` argument and the `session` a subscribe returns;
- `addition.text` and `addition.meta`, and the `text`, `trailer` and `meta` of `/calls/addition`, which can hold what lets that conversation receive its events.

Don't log tool arguments or tool results that carry them, don't put them in error reports, and don't show them on a page. To name a conversation where people can see it, use the subscription's `id`, or an HMAC of the session key under a secret of your own. Your key never leaves your server; [publish URLs](/docs/publish-urls) are credentials too.

## Events

[Events](/docs/events) are what agents subscribe to. Declare them in your manifest, let agents subscribe, and emit occurrences.

| Endpoint | Body | Returns |
| --- | --- | --- |
| `PUT /manifest` | `{ description?, …, events: [{ name, description, title?, arguments, payload, topic, filter?, personal?, delete_on_delivery?, subscribe? }] }` | The manifest as Wakeshift keeps it, and `changed: [{ event, from, to, ended }]`. `arguments` and `payload` are JSON Schemas (2020-12). |
| `POST /manifest/merge` | The same fields, each optional | The manifest as Wakeshift keeps it, with these merged in, and `changed` |
| `POST /events/emit` | `{ name, arguments, payload, text, id?, to? }` | `{ matched, duplicate, unmatched, ended }` |
| `POST /events/status` | `{ name, arguments, to? }`, or `{ name, subscriptions: [{ user, id }] }` (at most 1,000) | `{ subscriptions, connected, via }`: the conversations subscribed to the event with those arguments (a personal event's: `to`'s); or per subscription `{ id, user, live, connected, via }` |
| `POST /event-subscriptions` | `{ call, name, arguments, ttl?, reason?, label?, once?, replay?, defer_addition?, pending? }` | `{ event_subscription: { id, user, event, arguments, created, expires_at }, session, addition: { text?, meta? }, agent, pending?, wakes, call_limit? }` |
| `POST /event-subscriptions/unsubscribe` | `{ call, name, arguments, reason? }` | `{ ended }` |
| `POST /event-subscriptions/list` | `{ user }` | `{ subscriptions: [{ id, event, arguments, destination, webhook_host?, created_at }] }` |
| `POST /event-subscriptions/end` | `{ user, id }` | `{ ended }` |
| `POST /events/subscribe` | `{ call, name, arguments, delivery, dialect, ttl_ms?, cursor?, max_age_ms? }` | `{ id, refresh_before, cursor, truncated, delivery_status? }` |
| `POST /events/unsubscribe` | `{ call, name, arguments, delivery, dialect }` | `{ ended }` |

- **Manifest.** `PUT /manifest` replaces the whole manifest: an event it leaves out is removed, and a setting it leaves out is cleared. `POST /manifest/merge` changes only what it sends: each event it lists is added, or replaces the event of that name, and each setting it sends replaces that setting (`agent` as a whole). Events and settings it leaves out, or sends as `null`, stay as they are. Use merge when several processes share your key, each with some of your events; use `PUT` to remove an event or the agent scope.
- **Event types.** `topic` is the template occurrences route by: `ev/<name>/{arg}…`, one segment per required string, integer or boolean argument. `filter` maps a payload field to the optional argument it must equal. With `personal: true`, each subscription belongs to its subscriber and `emit` needs `to`. With `delete_on_delivery: true`, each occurrence's text and payload are deleted once delivered. The SDKs build all of this from the event's definition.
- **Who may subscribe.** `subscribe: "tools"` lets only your own tools subscribe to the event: `/event-subscriptions` from `subscribe_events`, or with no `call.tool`, and every native subscribe and poll are refused with `403 tools_only`. `subscribe: "any"` also lets agents subscribe through `subscribe_events` and natively. Unsubscribing and listing always work. An event that has never had the setting is `"any"`. A manifest that leaves it out keeps the setting Wakeshift has, even with `PUT`, so an older deployment can't reopen an event. Moving an event from `"any"` to `"tools"` ends its subscriptions made through `subscribe_events` or natively, at once. `changed` lists each event whose setting the manifest changed, with `ended`, the number of subscriptions that ended. Send `call.tool`, the name of the tool being called, with every `/event-subscriptions` request.
- **Emit.** `payload` must match the event's schema; Wakeshift trims `text` over 1000 bytes to 1000, cut between characters and ending with "…", rather than refusing it, so there's no need to trim it yourself; the response then has `"text_trimmed": true` (it's left out otherwise; [why the cap](/docs/events#emit)); `id` makes retries safe: the same emit (event, arguments, recipients, payload and text, the text as you sent it, before any trim) with the same `id` within a day emits once, and the retry returns the first's result with `duplicate: true`; the same `id` on a different emit is refused with `409 id_reused`, and nothing is sent. `to` names a personal event's recipients. One user (`"u1"`) reaches every subscription they have to the event with these arguments. To reach many at once, `to` takes a list (at most 1,000) of users, subscriptions (`{"user": "u1", "subscription": "<id>"}`: exactly that one, the `id` a subscribe returned for that user) and native subscribers (`{"native": "u1"}`: only their own MCP Events subscriptions). No conversation hears one emit twice. The response's `matched` counts conversations reached, `unmatched` lists the users and native subscribers nothing reached (one user included), and `ended` the subscriptions that are no longer live, so you can forget them; a retry with the same `id` returns the same lists. An app's emits with a list may name 10,000 recipients a minute in all (`429 rate_limited` past it; a single user isn't counted). The payload is at most 64 KB; the whole body of an emit, or of a status call, may be 512 KB, room for 1,000 recipients with long IDs.
- **Status** only reads. A subscription that isn't that user's, your app's or the event's, or has ended, reports `live: false`.
- **Bridged subscriptions** (`/event-subscriptions`) subscribe the calling conversation, as `subscribe_events` does, or renew its subscription with those arguments (`created: false`). Store `event_subscription.id` and `user` to address that conversation in `to`, or end it with `/event-subscriptions/end`. `reason` (at most 200 characters) is shown in the console. `label` names a new session for an agent your app acts for. `ttl` ends the subscription after that long without a delivery or a fresh subscribe: seconds, or a string like `"90s"`, `"5m"`, `"2h"` or `"30d"`, from 1 minute to 365 days, default 30 days. `once: true` ends it after its first delivery; `replay` (at most 24 hours) also delivers the latest occurrence emitted that long before subscribing. `list` and `end` act on one of your users' subscriptions, whatever their destination. `wakes` says whether Wakeshift wakes this conversation as it stands: its agent's program has a way to wake it while idle that runs now, with nothing left to install, no further call to your tools and no choice from the user still pending (a foreground wait doesn't count, nor does Claude Code before the user picks how to be notified). When it's `false`, keep any polling or waiting your tool does without Wakeshift. `call_limit`, when the agent's program is known, is how many seconds it lets one tool call take (Claude Code 60, Codex 300): when `wakes` is `true` and the agent asked for a wait at least that long, return at once rather than wait.
- **The addition.** Without `defer_addition`, put `addition.text` after your own text in the tool result, and `addition.meta` in the result's `_meta`. For a tool call that subscribes more than once, see [calls](#calls).
- **Unsubscribe** is the calling conversation's own request: no notice, and occurrences still waiting for it are dropped. `end` tells the conversation, after what was emitted before. Either way, a conversation's last subscription ending stops what wakes it ([when a subscription ends](/docs/events#when-a-subscription-ends)). Unsubscribe takes `reason: "rolled_back"` for the subscription of a tool call that failed: the conversation never learned of it.
- **Native subscriptions** (`/events/subscribe`) are what your server forwards when a client sends MCP Events' `events/subscribe`: pass the client's `delivery` object as it sent it, and `dialect`, `"chatgpt"` for ChatGPT and `"draft"` for every other client. Wakeshift checks the client's callback, then delivers occurrences to it, signed and in order.

`agent` is what Wakeshift recognized the caller as: `harness` (`claude-code`, `codex`, `cursor`, `cursor-cloud`, `grok-bot`, `copilot`, `opencode`, `antigravity`, …) and `via` (`"client"`, `"meta"`, `"user_agent"`, or `"claim"` when only `call.harness` named it), both `null` when it recognized nothing; and `claimed`, what `call.harness` said, normalized (`"Grok Bot"` is `grok-bot`), or `null`. A `claimed` that differs from `harness` was ignored. When `harness` and `claimed` are both `null`, the agent can't be woken: log a warning.

## Calls

A tool result carries one addition, however many subscriptions the tool call made. For a tool that subscribes more than once, or that should carry Wakeshift's addition without subscribing:

| Endpoint | Body | Returns |
| --- | --- | --- |
| `POST /calls/session` | `{ call }` | `{ session, pending, agent }` |
| `POST /calls/addition` | `{ call, pending?, native? }` | `{ text, trailer?, meta?, agent }` |

1. Send each `/event-subscriptions` in the call with `defer_addition: true`. The first returns `pending`; pass it as `pending` on the call's later subscribes. Their `addition` is empty.
2. When your tool's result is ready, call `/calls/addition` once, with the same `call` and the `pending`. Put `text` after your own text, `trailer` (when there is one) as the last line of the result's text, after everything else, and `meta` in the result's `_meta`.
3. If the tool call fails instead, end each subscription it made (`created: true`) with `/event-subscriptions/unsubscribe` and `reason: "rolled_back"`, and skip `/calls/addition`.

- `pending` works once, for the conversation it was made for, for 10 minutes. One that was used, expired, or names another conversation is a `400`: start the call's requests again without it.
- `/calls/session` finds or starts the calling conversation's session without subscribing it, and returns the call's `pending`.
- Without `pending`, `/calls/addition` returns what any other tool result of yours carries for the conversation: at most a beacon its hooks redeem and a one-time note that it's connected. Usually it's empty. No result carries held occurrences.
- `native` lists, for a client that subscribes itself (MCP Events), the events to tell it to subscribe to: up to 20 `{ name, arguments }`. `text` then says which, or that it already has.

## Publish URLs

| Endpoint | Body | Returns |
| --- | --- | --- |
| `POST /events/publish-urls` | `{ name, arguments, to?, signing_secret? }` | `{ id, event, url, signing? }` |
| `DELETE /events/publish-urls/{id}` | None | `{ revoked: true }` |
| `PUT /events/publish-urls/{id}/signing-secret` | `{ secret }` | `{ signing }` |
| `DELETE /events/publish-urls/{id}/signing-secret` | None | `{ removed }`: `false` if it had none. |

The URL itself takes no authentication unless it has a signing secret. It takes the event's payload, a JSON object that matches its schema, and answers `400 bad_payload` for anything else, or `410 unknown_event` once your app no longer offers the event; `to` is required for a personal event, and names one user. See [publish URLs](/docs/publish-urls#what-a-post-becomes).

`signing_secret` and `secret` are a Standard Webhooks secret (`whsec_` and the base64 of 24 to 64 bytes), such as a Svix endpoint's. With one set, the URL accepts only posts signed with it, and answers `401 invalid_signature` to the rest; see [signed webhooks](/docs/publish-urls#signed-webhooks). Setting a different secret rotates it: the previous one is still accepted for 24 hours. Setting the same one again changes nothing. `signing` is `{ fingerprint, set_at, previous_valid_until? }`: the first 12 hex digits of the secret's SHA-256, never the secret, which no response returns.

## Users

| Endpoint | Body | Returns |
| --- | --- | --- |
| `POST /users/forget` | `{ user }` | `{ forgotten }`: `true` if there was something to delete. See [forgetting a user](/docs/users#forgetting-a-user). |

## Errors

| Status | Code | When |
| --- | --- | --- |
| `400` | `bad_request` | Something in the request is invalid, including a body field this page doesn't list. The message says what. |
| `401` | `unauthenticated` | The assertion was refused: expired, reused, signed with another key, or for another `aud` (a key for another Wakeshift). The message says which. |
| `400` | `bad_arguments` | An event's arguments or an emit's payload don't match the event's schema, or a routing value isn't a lowercase ID. The message says which. |
| `400` | `bad_manifest` | A manifest (`PUT /manifest` or `POST /manifest/merge`) that isn't valid. The message says what. |
| `400` | `ambiguous_topic` | A manifest with a topic that could reach the wrong subscribers: two placeholders in one segment (`jobs/{org}-{job}`), or two events whose topics can be the same. The message names the topics. See [events](/docs/events#define-an-event). |
| `400` | `bad_payload` | A POST to a publish URL whose body isn't a JSON object that matches the event's payload. |
| `400` | `bad_signing_secret` | A publish URL's signing secret isn't `whsec_` and the base64 of 24 to 64 bytes. |
| `401` | `invalid_signature` | A POST to a publish URL with a signing secret that isn't signed with it: missing headers, no matching signature, or a timestamp more than 5 minutes off. The message says which. |
| `403` | `subscription_limit` | A native subscription (`/events/subscribe`) past the 20 a user can have to your app at once. Renewing one is never refused; your SDK answers the client with MCP Events' `ResourceExhausted` (`InvalidParams` for ChatGPT). |
| `403` | `tools_only` | A subscription to an event with `subscribe: "tools"` that isn't from one of your tools: `subscribe_events`, no `call.tool`, or a native subscribe or poll. Your SDK reports the subscription as unavailable to that client. |
| `403` | `waitlisted` | The key isn't an app's: the person who opened the link from `npx @wakeshift/cli init` for it is on Wakeshift's waitlist. Once their account is ready, they confirm the link again and the key works. |
| `403` | `other_client` | The `session` key is a conversation in another MCP client of the same user. Call again without it. |
| `404` | `unknown_publish_url` | The publish URL ID isn't one of your app's. |
| `404` | `unknown_event` | Your app has no event by that name. Wakeshift learns your events from your manifest: send it (`PUT /manifest`, or `POST /manifest/merge`) before emitting. |
| `409` | `id_reused` | An emit's `id` was used within the last day for a different emit (another event, arguments, recipients, payload or text). Nothing was sent. Give each occurrence its own `id`. |
| `410` | `revoked` | A POST to a revoked publish URL, or a change to its signing secret. |
| `413` | `too_large` | The request body is over 64 KB (512 KB for an emit or a status call, room for 1,000 recipients). The limit is on the whole body, so the payload must leave room for the rest of the request. |
| `429` | `rate_limited` | Your emits with a list named over 10,000 recipients in a minute. |
