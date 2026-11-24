# Other languages

There are SDKs for [TypeScript](/docs/sdk), [Go](/docs/go) and [Python](/docs/python). From any other language, call the [HTTP API](/docs/http-api) yourself. This page says what your server does; the HTTP API page has every endpoint.

## What your server does

1. **Signs each request** with a fresh assertion made from your key ([the format](/docs/http-api#authentication)).
2. **Declares its events** in its manifest, `PUT /manifest`, once at startup: each event's name, description, arguments and payload as JSON Schemas, and the template it routes by (`topic`). `PUT /manifest` replaces the app's whole list; if several processes share your key, each with some of your events, have each send `POST /manifest/merge`, which never removes an event.
3. **Lets agents subscribe.** Give your MCP server a `subscribe_events` tool (and `unsubscribe_events` and `list_subscriptions`) that calls `POST /event-subscriptions` with a `call` built from the tool call, and returns your text followed by `addition.text`, with `addition.meta` as the result's `_meta`. Use the tools' wording from the SDKs ([`event-tools.json`](https://github.com/wakeshift/wakeshift-node/blob/main/test/event-tools.json)), so agents see the same tools from every app.
4. **Subscribes from its own tools**, if it has a tool like "join" or "watch": give the tool the three [wake arguments](/docs/http-api#the-caller), call `POST /event-subscriptions` from it the same way, and put the addition in its result. A tool that subscribes more than once sends `defer_addition` and asks [`/calls/addition`](/docs/http-api#calls) once at the end; a tool call that fails ends what it subscribed, with `reason: "rolled_back"`.
5. **Optionally, serves MCP Events** to clients that speak it, such as ChatGPT: advertise the events capability, answer `events/list` from your event types, and forward `events/subscribe` and `events/unsubscribe` to `POST /events/subscribe` and `POST /events/unsubscribe`.
6. **Emits** with `POST /events/emit` wherever events happen: to everyone subscribed with some arguments, or, for a personal event, to the users and subscriptions you name in `to`.

If you get the `call` wrong, nothing fails: the agent is treated as one Wakeshift can't wake. Check what each subscribe returns as you go.

## Sign requests

An example in Python, with PyNaCl and httpx:

```python wakeshift_client.py
import base64, hashlib, json, os, secrets, time, zlib
import httpx
from nacl.signing import SigningKey

B58 = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

def b64u(b: bytes) -> str:
    return base64.urlsafe_b64encode(b).rstrip(b"=").decode()

def parse_key(key: str) -> tuple[str | None, bytes]:
    """The app's ID (None if the key doesn't name it) and the 32-byte seed."""
    key = key.strip()
    if not key.startswith("wst_"):
        raise ValueError("wakeshift: an app key starts with wst_")
    rest = key.removeprefix("wst_")
    if "_" in rest:  # an older key: wst_<test|live>_<app>_<seed, base64url>
        mode, app_id, seed = rest.split("_", 2)
        if mode not in ("test", "live"):
            raise ValueError("wakeshift: not an app key")
        return app_id, base64.urlsafe_b64decode(seed + "=" * (-len(seed) % 4))
    n = 0  # wst_ and 50 base58 characters: the seed and its CRC32
    for c in rest:
        n = n * 58 + B58.index(c)
    raw = n.to_bytes(36, "big")
    if len(rest) != 50 or zlib.crc32(raw[:32]) != int.from_bytes(raw[32:], "big"):
        raise ValueError("wakeshift: the key is cut short or mistyped")
    return None, raw[:32]

class Wakeshift:
    def __init__(self):
        key = os.environ["WAKESHIFT_KEY"]
        self.app_id, seed = parse_key(key)
        core = os.environ.get("WAKESHIFT_CORE")
        if not core:
            if key.strip().startswith("wst_test_"):  # an older test key has no default
                raise ValueError("wakeshift: set WAKESHIFT_CORE to the address of the console this key came from")
            core = "https://app.wakeshift.ai"
        self.core = core.rstrip("/")
        self.key = SigningKey(seed)
        self.kid = b64u(hashlib.sha256(bytes(self.key.verify_key)).digest()[:16])
        self.http = httpx.Client(base_url=self.core + "/api/apps/v1", timeout=20)

    def _assertion(self) -> str:
        now = int(time.time())  # an integer: a float is refused
        who = self.app_id or self.kid
        head = {"alg": "EdDSA", "typ": "app+jwt", "kid": self.kid}
        claims = {"iss": who, "sub": who, "aud": self.core + "/api/apps",
                  "iat": now, "exp": now + 120, "jti": b64u(secrets.token_bytes(16))}
        signing_input = b64u(json.dumps(head).encode()) + "." + b64u(json.dumps(claims).encode())
        return signing_input + "." + b64u(self.key.sign(signing_input.encode()).signature)

    def request(self, method: str, path: str, body: dict | None = None) -> dict:
        r = self.http.request(method, path, json=body, headers={"Authorization": "Bearer " + self._assertion()})
        if r.status_code >= 400:
            err = r.json().get("error", {})
            raise RuntimeError(f"wakeshift: {err.get('code')} ({r.status_code}): {err.get('message')}")
        return r.json()

    def app(self) -> dict:
        """Which app the key is: {"id", "name", "key": {"key_id", "name"}}."""
        info = self.request("GET", "/app")
        self.app_id = self.app_id or info["id"]
        return info
```

A key is `wst_` and 50 characters (base58: the seed, then its CRC32). It doesn't name your app; `GET /app` says which app it is, when you need the ID. Older keys, `wst_test_<app>_<seed>` and `wst_live_<app>_<seed>`, keep working; they name the app, and a `wst_test_` key needs `WAKESHIFT_CORE`.

## Where the call comes from

Each subscribe carries a `call` ([its fields](/docs/http-api#the-caller)), built from four things every MCP framework exposes:

- the `clientInfo.name` the client sent in `initialize`;
- the `_meta` object in the `tools/call` request's `params`;
- the HTTP request's `User-Agent` header;
- the tool's `session_key`, `harness` and `conversation_id` arguments, with the descriptions on the HTTP API page.

Add the tool's name as `tool`, and your own ID for the person as `user` when you have one. Native subscriptions and personal events need it.

## Stateless servers

The client's name comes from `initialize`, which the client sends once, when it connects. A server that handles each request on its own never sees it.

Stateless servers work for Claude Code, Codex and Antigravity, which Wakeshift recognizes from the tool call's `meta`, and for opencode, Cursor and GitHub Copilot CLI, from `user_agent`. So send both on every call. For any other client, keep a session per client (behind a load balancer, route each `Mcp-Session-Id` to the same instance), or pass `client` yourself.

A subscribe says what Wakeshift recognized: `agent: { harness, via, claimed }`, with `harness` and `via` both `null` when it recognized nothing. Log a warning then, unless the agent said which program it runs in (`claimed`).

## Handle secrets

The `session_key` argument, and the text and `_meta` Wakeshift adds to results, are credentials for that conversation. Don't log tool arguments or results that carry them. See [handling secrets](/docs/http-api#handling-secrets).
