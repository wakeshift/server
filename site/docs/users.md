# Your users

Your users don't sign up for Wakeshift. You have the Wakeshift account; they add your app to their agent and use it. This is how Stripe and Pusher work too. Your app identifies its own users, with its own sign-in, and Wakeshift never signs them in for you.

## Adding your app is the consent

Wakeshift keeps a workspace for each of your users, owned by your app and used only by it. Adding your app to an agent is the user's consent. There's no Wakeshift screen to click through.

In that workspace, Wakeshift handles delivery and acknowledgments, pacing, marking outside messages, stamping senders, and an audit log.

## Your terms and privacy policy

Your app publishes its own terms and privacy policy, and the step where people add your app links them. Some agents also need Wakeshift's connector, a one-time install on the user's computer. Wakeshift's [Connector Terms](/connector-terms) and [Privacy Policy](/privacy) apply to it. Neither the agent nor the installer shows them: your terms carry them to your users. In your terms or privacy policy:

- refer to Wakeshift's Connector Terms and Privacy Policy, with links to both;
- incorporate them, or have your users agree to them, as they apply to the connector and to Wakeshift's processing;
- name Clawvisor Inc. as a third-party beneficiary of the Connector Terms, able to enforce them; and
- give your users the notices, and get the consents, that the law requires for what Wakeshift processes on your behalf.

[Section 6 of the Terms of Service](/terms#your-users) sets out the obligation in full (6.6).

## Who a subscriber is

Without sign-in, each conversation stands alone: Wakeshift identifies it from the tool call, and `list_subscriptions` and `unsubscribe_events` act on the calling conversation's own subscriptions, found by the `session_key` the agent passes. If you know who the person is, give `attach` an `identify` that returns your own ID for them as `user`. Their subscriptions then belong to them, personal events can reach them, and `list_subscriptions` shows them theirs:

```ts
const attached = wake.attach(server, { identify: (ctx) => ({ user: ctx.http?.authInfo?.extra?.userId as string | undefined }) })
```

Personal events need a user, and so does a native subscription (ChatGPT) a client makes on its own; one your tool hands it a voucher for doesn't, if the client keeps a session with your server (ChatGPT doesn't). In your own tools, `call.user` is who `identify` said.

A stateless MCP server never sees `initialize`, so it doesn't know the client's name, which tells Wakeshift which program the agent runs in. If your sign-in knows it (an OAuth registration's `client_name`, say), return it from `identify` as `client`:

```ts
const attached = wake.attach(server, { identify: (ctx) => ({ user: userOf(ctx.http?.authInfo), client: clientNameOf(ctx.http?.authInfo) }) })
```

A name the request carries wins. Python's `Identity` and Go's `wakeshift.Identity` take the same `client`.

A conversation without a user that your app hasn't acted for in over 30 days, and that no live subscription still delivers to, is forgotten for you. Your users aren't: you forget them with `forgetUser` (below).

## Using your own sign-in

If your MCP server already signs people in, with OAuth or API tokens, there's nothing Wakeshift-specific to wire up: no token exchange and no callback. Authenticate as you do now, and pass your ID for the person as `user`.

`ctx.http?.authInfo` above is whatever your auth code put on the request. `toNodeHandler` from `@modelcontextprotocol/node` passes `req.auth` on as each request's `ctx.http?.authInfo` (on `@modelcontextprotocol/sdk` 1.x, the Streamable HTTP transport copies it into `extra.authInfo` when your code calls its `handleRequest`; a framework that runs the transport itself may keep the token in its own request context instead, as [mcp-use](/docs/frameworks#mcp-use) does), and `userId` is a field you choose to set:

```ts
// Your auth middleware, before the MCP transport handles the request:
app.use("/mcp", async (req, res, next) => {
  const token = (req.headers.authorization ?? "").replace(/^Bearer /, "")
  const person = await lookUpToken(token) // your own code
  if (!person) return res.status(401).end()
  req.auth = { token, clientId: person.clientId, scopes: [], extra: { userId: person.wakeshiftId } }
  next()
})
```

## When only your upstream API can verify a credential

Many MCP servers pass the caller's API key or token straight through to their own API, and never check it themselves. Then `identify` can't know who the caller is from the request alone. Choose one of these:

- **No `identify`.** Each conversation stands alone, and the `session_key` the agent passes is what ties its calls together. Your tools still subscribe, and Claude Code, Codex and the other agents are woken. MCP Events clients that keep a session with your server still subscribe with the vouchers your tools hand them; ChatGPT, which keeps none, gets no voucher without sign-in. What you give up: personal events, native subscriptions a client makes on its own, and listing one person's subscriptions across conversations.
- **`identify` asks your API who the credential belongs to**, with a "who am I" call, and returns your ID for that account. Then cache the answer, keyed by a hash of the credential (never the credential itself) for a minute or so, since `identify` runs on each request that needs it.

If you call your API from `identify`:

- **Return no user** when the API says the credential is invalid (a 401 or 403): the caller is anonymous, and `requireUser` refuses them if you set it.
- **Throw** only when you can't tell (a 5xx, a timeout, no answer): Wakeshift's part of that request is refused (`identify_failed`; the SDK logs `identify_error`), and your tool still runs. Returning no user is logged only as `user_required`, at info, and only under `requireUser`, where `call.subscribe` throws `user_required` too.
- **Finish within 10 seconds.** Past that, `identify` counts as failed. An HTTP client that retries with backoff (most API clients do, on a 5xx) can spend longer than that on one call, so give `identify` its own client with a short timeout (a few seconds) and at most one retry.
- Don't pass the raw credential to Wakeshift, in `user` or anywhere else: return your own opaque ID for the account.

## User IDs

`user` is your ID for the person. Wakeshift trims spaces at either end, then keeps it as is. The rules:

- At most 200 characters, and it can't start with `~`.
- Use an ID that stays the same and isn't personal information. Not an email address: map emails, mixed-case IDs and IDs like `auth0|abc123` to an opaque one, such as a hash of your internal ID:

```ts
import { createHash } from "node:crypto"
const wakeshiftId = (internalId: string) => createHash("sha256").update("wakeshift:" + internalId).digest("hex").slice(0, 32)
```

## Signing in MCP clients

MCP clients like Claude Code, Codex and ChatGPT sign in with OAuth: they read your server's protected-resource metadata, register as a client, and get a token with an authorization code and PKCE. If your app has accounts, put your OAuth server, or your identity provider's, in front of your MCP endpoint, and pass your ID for the person as `user`. If it has none, run a small OAuth server of your own. Our games do: clicking **Authorize** on a game's page starts a session with a random ID, which is the player's `user`, and the game forgets the session after 31 days unused. Their code is in [wakeshift/apps](https://github.com/wakeshift/apps) (`gamekit/oauth.go`, and Agent Hold'em's `src/oauth.ts`).

Whichever you run:

- Accept only redirect URIs a person's own agent can have: loopback (`http://localhost`, `127.0.0.1` or `[::1]`, any port), an app's own scheme (`cursor://…`), and the callbacks of the web agents you support, exactly: Claude's `https://claude.ai/api/mcp/auth_callback` and `https://claude.com/api/mcp/auth_callback`, and ChatGPT's `https://chatgpt.com/connector_platform_oauth_redirect`. Any other URL can send a person's code to someone else's site.
- Require PKCE (S256), and limit registration and sign-in per address.
- When you delete a user or a session, call `forgetUser` for it.

Wakeshift caps how many users an app can have: 10,000, conversations without a user included. Past the cap, new ones are refused (`workspace_limit`); write to support@wakeshift.ai to raise it. Wakeshift also limits how many new users and conversations an app takes in an hour: 2,000. Past that, new ones get `workspace_rate_limited`; try again later.

## Keys

You make keys in the console (your app, then **Keys**). A key starts with `wst_`.

- A key is your app's private key. It never goes over the wire: the SDK signs a short-lived assertion with it for each request. Wakeshift stores only the public key.
- An app can have up to 10 named keys at once, say one for production and one for CI. Each works on its own. The console shows a key once, when it's made.
- **Rename** changes a key's name; the key itself doesn't change. **Revoke** stops one key at once and leaves the others working. **Rotate** replaces a key with a new one of the same name; the old one stops at once. For a change with no gap, make a new key, deploy it, then revoke the old one.
- **Delete** (in Settings) revokes every key and ends everything your app subscribed. Within 30 days, Wakeshift deletes everything it keeps for your app's users: their workspaces, agents, conversations, subscriptions, events and deliveries, and the grants your app made there. You can read your app's log for 30 days after deleting it; then it's deleted too.

For separate environments, register separate apps (`my-app-dev` and `my-app`) in the same organization.

## When someone loses access

Wakeshift checks access only when a conversation subscribes: your `authorize`, or your own tool, decides then, and not again. When someone leaves an org or loses access to a repo, end their subscriptions yourself:

```ts
// One subscription: the user and id call.subscribe returned (or list_subscriptions shows).
await ciFinished.end(member.user, member.subscription)
```

Each conversation is told its subscription ended. `forgetUser` ends all of a user's subscriptions at once.

## What Wakeshift stores about your users

In the workspace it keeps for each user:

- the `user` ID you pass, exactly as you send it;
- their conversations' sessions: the agent's client name, and the conversation's ID where the agent sends one (a Codex thread ID, for example);
- their subscriptions (with the `reason` your tool gave), and what each agent needs in order to be woken;
- the occurrences you emitted to them, with their text and payload (until delivery, for an event with `deleteOnDelivery`), and the record of each delivery;
- an audit log of what happened in the workspace.

It doesn't get your users' names, emails or tokens unless you put them in a user ID, or in an event's arguments, payload or text. Don't.

## Forgetting a user

When a person deletes their account, call `forgetUser`:

```ts
await wake.forgetUser(user.id)
```

It ends their subscriptions, closes their agents' streams, and deletes the workspace Wakeshift keeps for them, with everything in it listed above. It returns `true` if there was something to delete; calling it again returns `false`. If the same ID comes back later, it starts with an empty workspace.

Forgetting a user takes a key that an owner or admin of the app's organization made, as forgetting one in the console does. A key a developer made gets `403 forbidden`.
