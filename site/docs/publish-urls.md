# Publish URLs

A publish URL emits one of your [events](/docs/events), with the arguments you made it for, each time something POSTs to it. Point GitHub, Stripe, a CI system or any other webhook sender at it, and the agents subscribed to that event with those arguments wake up. You write no handler.

## Make one

In the console, open your app's **Publish URLs**, pick the event, and fill in its arguments (and the user, for a personal event). The URL is shown once.

From your code:

```ts
const { id, url } = await ciFinished.createPublishUrl({ owner: "acme", repo: "api" })
```

Give `url` to the service that sends the webhooks. Keep `id` so you can turn the URL off later. Each call makes a new URL. For a personal event, pass the one user it's for: `turn.createPublishUrl({ table: "juniper" }, { to: "player-abc123" })`. In Python it's `await event.create_publish_url(args)`, in Go `event.CreatePublishURL(ctx, args)`.

On a client without a key, making a URL throws `disabled`, so a tool that offers notifications guards it with `if (wake.enabled)` (Python `wake.enabled`, Go `wake.Enabled()`).

> **Anyone with the URL can emit the event with those arguments.** Treat it like a password: don't put it in a public repo, a page or a log. The SDKs never print it: a publish URL's JSON and printed forms leave out its token, so pass `.url` (Go `.URL`) explicitly where it's meant to go.

## What a POST becomes

Each POST emits one occurrence:

- The body is the occurrence's payload: a JSON object that matches the event's `payload` schema. Anything else gets `400 bad_payload`, and nothing is emitted. Bodies can be up to 64 KB.
- Your `text` function doesn't run, since your code isn't involved. The agent reads the event's default text, its description with the URL's arguments (`A CI run on a repo finished (repo api)`), and the payload whole, as the event's data. A Claude Code conversation's Monitor line is at most 480 bytes, so a payload up to about 200 bytes arrives whole; a longer one arrives as the start of it ([what the line holds](/docs/testing#claude-code)).
- Optional (filter) arguments match against the body's top-level fields, as they do for any emit.

Posts with fields your payload schema doesn't model are accepted: every SDK describes your payload without refusing unknown fields, whether it's a zod object, a pydantic model or a Go struct. Only a schema you make strict (zod's `z.strictObject`) refuses them, and that turns every post into a `400` the day the sender adds a field.

The body is used as the sender sends it. Most services send their own shape: GitHub's `workflow_run` event has the branch at `workflow_run.head_branch`, not at the top. When the sender's body doesn't match your payload, or you want a better line for the agent, point the service at your own handler and call `emit` from there.

If the sender retries, send the same `Idempotency-Key` header (GitHub's `X-GitHub-Delivery` and Standard Webhooks' `webhook-id` or `svix-id` work too) and Wakeshift emits once, for a day. A key over 150 characters is ignored, so each post with it emits.

## Signed webhooks

A publish URL can also check who sent each post. Give it the sender's signing secret, and it accepts only posts signed with that secret, the [Standard Webhooks](https://www.standardwebhooks.com) way. Svix, and every service that sends its webhooks through Svix, signs like this, as do OpenAI, Clerk and others.

To wake agents from a Svix sender:

1. Make a publish URL for the event, as above.
2. In the sender's webhook portal (Svix's App Portal, or your own Svix application), add an endpoint and paste the publish URL. Filter it to the event types you want.
3. Copy the endpoint's signing secret. It starts with `whsec_`.
4. In the console, open **Publish URLs** and choose **Set signing secret** on the URL, then paste the secret. You can also paste it when you make the URL.

From then on, each event the sender delivers wakes the agents subscribed to it, and anything else is refused. Svix's retries emit once: the signed `webhook-id` (Svix's `svix-id`) is the idempotency key, and other idempotency headers are ignored.

A signed post needs:

- `webhook-id`, `webhook-timestamp` and `webhook-signature` headers (or Svix's `svix-id`, `svix-timestamp` and `svix-signature`).
- A `v1` signature: HMAC-SHA256, under the secret, of the ID, the timestamp and the body joined by dots. The header can list several signatures separated by spaces; one match is enough.
- A timestamp, in Unix seconds, within 5 minutes of Wakeshift's clock.

Anything else gets `401 invalid_signature`, and nothing is emitted.

Wakeshift stores the secret encrypted and never shows it again: the console shows only its fingerprint, the first 12 hex digits of its SHA-256 (`printf %s "$SECRET" | shasum -a 256` shows the same).

**Rotating.** Rotate the secret in the sender first, then choose **Rotate secret** and paste the new one. The old secret keeps working for 24 hours, so posts the sender signed before the switch still get through. **Remove secret** turns the check off at once. Revoking the URL deletes its secret.

From your server, pass the secret when you make the URL: `createPublishUrl(args, { signingSecret })` in TypeScript, `create_publish_url(args, signing_secret=…)` in Python, `wakeshift.SigningSecret(secret)` in Go. The result's `signing` has the fingerprint, never the secret. The [HTTP API](/docs/http-api#publish-urls) also sets, rotates or removes one later.

## Use them safely

- **Prefer a signed URL** whenever the sender signs its webhooks: then only the sender can emit.
- **Otherwise, one URL per job,** made when the job starts and revoked at its last event, so a leaked URL is good for one job at most.
- **Treat what an unsigned URL delivers as outside data.** Anyone with the URL can post, so don't build an event's `text` from free text in an unsigned post. Agents read occurrences as information from your app, never as instructions, but a forged one can still mislead.

## Responses

| Status | Meaning |
| --- | --- |
| `202` | Emitted: `{ "matched": n, "duplicate": false }`. A retry with the same key gets `"duplicate": true`, and emits nothing. |
| `400` | `bad_payload`: the body isn't a JSON object that matches the event's payload. |
| `401` | `invalid_signature`: the URL has a signing secret, and the post isn't signed with it, or its timestamp is more than 5 minutes off. See [signed webhooks](#signed-webhooks). |
| `404` | No such URL. |
| `410` | The URL was revoked, or your app no longer offers the event. |
| `413` | The body is over 64 KB. |
| `429` | Over 60 posts a minute. `Retry-After` says when to try again. (The [try app](/docs/try)'s webhooks have lower limits, per hour.) |

## Turn one off

Choose **Revoke** in the console, or:

```ts
await wake.revokePublishUrl(id)
```

From then on the URL answers `410`. Make a new one if you need it again.
