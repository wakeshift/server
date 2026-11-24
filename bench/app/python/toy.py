"""What the bench's Python toy apps share (bench/app/CONTRACT.md): the
contract's words, the event, the bench's side API and the plumbing around
the MCP endpoint. Each port only builds its MCP server on its framework,
with the three tools wrapped by Wakeshift's Python SDK, the way an app on
that framework does.

A port runs behind the bench's front (bench/app/front.ts), which records
every call from the HTTP exchange. It reads, from the environment:

    PORT                where to listen (127.0.0.1)
    BENCH_KEY_FILE      the app key's file (mode 0600; never printed)
    WAKESHIFT_CORE      the core
    BENCH_CONTROL_FILE  the bench's control token's file (mode 0600)
    BENCH_CONTRACT      contract.json
    BENCH_MCP_NAME      the MCP server's name (default the contract's)
    BENCH_WAIT          "1": serve the SDK's wait tool (attach's builtin_tools wait)

It exits when its standard input closes (the front is gone).
"""

from __future__ import annotations

import hashlib
import hmac
import json
import os
import re
import stat
import sys
import threading
from collections.abc import Mapping
from typing import Any

from pydantic import BaseModel
from starlette.requests import Request
from starlette.responses import JSONResponse
from starlette.routing import Route

from wakeshift import BuiltinTools, Identity, Recipient, ToolCall, Wakeshift
from wakeshift import Request as WakeRequest

CALL_HEADER = "x-wakebench-call"
#: attach's own tools: none (the lanes use only their tools), and the wait
#: tool only with BENCH_WAIT=1 (off, the SDK's default, otherwise).
BUILTIN_TOOLS: BuiltinTools = {
    "subscribe": False,
    "list": False,
    "unsubscribe": False,
    "wait": os.environ.get("BENCH_WAIT") == "1",
}
_LANE = re.compile(r"^[a-z0-9-]{1,40}$")


def _secret_file(name: str) -> str:
    path = os.environ.get(name)
    if not path:
        raise SystemExit(f"set {name}")
    if os.stat(path).st_mode & (stat.S_IRWXG | stat.S_IRWXO):
        raise SystemExit(f"{name} must be readable only by you (chmod 600)")
    with open(path, encoding="utf-8") as f:
        return f.read().strip()


def _exit_with_parent() -> None:
    def watch() -> None:
        sys.stdin.read()
        os._exit(0)

    threading.Thread(target=watch, daemon=True).start()


class Args(BaseModel):
    run: str
    lane: str


class Payload(BaseModel):
    code: str
    seq: int
    sent_at: str
    tag: str | None = None


def short(s: str) -> str:
    """A session's short hash, as the TypeScript app records it."""
    return hashlib.sha256(s.encode()).hexdigest()[:12] if s else ""


def headers_of(ctx: Any) -> Mapping[str, str]:
    """The HTTP headers of the request a handler or identify is serving: a
    wakeshift.Request's, or a handler's context's."""
    if isinstance(ctx, WakeRequest):
        return ctx.headers
    rc = getattr(ctx, "request_context", ctx)
    req = getattr(rc, "request", None)
    h = getattr(req, "headers", None) if req is not None else None
    return h if h is not None else {}


def lane_of(ctx: Any) -> str:
    """The lane: the bearer token the front puts on every request."""
    auth = headers_of(ctx).get("authorization", "")
    tok = auth[len("Bearer ") :] if auth.startswith("Bearer ") else ""
    return tok if _LANE.match(tok) else ""


def identify(r: WakeRequest) -> Identity | None:
    """Every caller of a lane's server is that lane's user."""
    lane = lane_of(r)
    return Identity(user=lane) if lane else None


class Toy:
    """A running toy app's shared state."""

    def __init__(self) -> None:
        _exit_with_parent()
        with open(os.environ["BENCH_CONTRACT"], encoding="utf-8") as f:
            self.contract: dict[str, Any] = json.load(f)
        c = self.contract
        self.wake = Wakeshift(_secret_file("BENCH_KEY_FILE"))  # its warnings reach stderr (logging's last resort)
        self._control = _secret_file("BENCH_CONTROL_FILE")
        self.run = os.environ.get("BENCH_RUN") or "manual"
        self.notes: dict[str, dict[str, Any]] = {}
        ev = c["event"]

        def text(p: Payload, _a: Args) -> str:
            if p.tag:
                return ev["textTagged"].replace("{code}", p.code).replace("{tag}", p.tag)
            return ev["text"].replace("{code}", p.code)

        # public: the bench's lanes subscribe through every path (design §3).
        self.event = self.wake.define_event(
            ev["name"], description=ev["description"], args=Args, payload=Payload, text=text, topic=ev["topic"], public=True
        )

    @property
    def server_name(self) -> str:
        return os.environ.get("BENCH_MCP_NAME") or self.contract["server"]["name"]

    @property
    def tools(self) -> dict[str, Any]:
        return self.contract["tools"]

    def args_of(self, lane: str) -> dict[str, str]:
        return {"run": self.run, "lane": lane}

    def noted(self, ctx: Any, note: dict[str, Any]) -> None:
        call = headers_of(ctx).get(CALL_HEADER, "")
        if call:
            self.notes[call] = note

    # The three tools' bodies, whatever the framework.

    async def watch(self, call: ToolCall, ctx: Any) -> str:
        lane = lane_of(ctx)
        sub = await call.subscribe(self.event, self.args_of(lane), ttl=self.contract["subscribeTtl"])
        r = sub.recognized
        agent = {"harness": r.harness, "via": r.via, "claimed": r.claimed} if r else {"harness": None, "via": None, "claimed": None}
        session = short(sub.session.reveal()) if sub.session else ""
        self.noted(ctx, {"tool": "watch", "lane": lane, "subscription": sub.id, "session": session, "agent": agent})
        return self.tools["watch"]["text"]

    async def unwatch(self, call: ToolCall, ctx: Any) -> str:
        lane = lane_of(ctx)
        ended = await call.unsubscribe(self.event, self.args_of(lane))
        self.noted(ctx, {"tool": "unwatch", "lane": lane})
        return self.tools["unwatch"]["ended" if ended else "notWatching"]

    def report_seen(self, ids: list[str], ctx: Any) -> str:
        self.noted(ctx, {"tool": "report_seen", "lane": lane_of(ctx)})
        return self.tools["report_seen"]["text"].replace("{n}", str(len(ids)))

    # The bench's side API (bearer: the control token).

    def _authorized(self, request: Request) -> bool:
        return hmac.compare_digest(request.headers.get("authorization", ""), f"Bearer {self._control}")

    async def _emit(self, request: Request) -> JSONResponse:
        if not self._authorized(request):
            return JSONResponse({"error": {"code": "unauthenticated", "message": "control token"}}, 401)
        b = await request.json()
        lane = b.get("lane", "")
        if not _LANE.match(lane):
            return JSONResponse({"error": {"code": "bad_request", "message": "lane"}}, 400)
        p = {k: v for k, v in (b.get("payload") or {}).items() if v is not None}
        out = await self.event.emit(self.args_of(lane), p, id=b.get("id"))
        return JSONResponse({"matched": out.matched})

    async def _status(self, request: Request) -> JSONResponse:
        if not self._authorized(request):
            return JSONResponse({"error": {"code": "unauthenticated", "message": "control token"}}, 401)
        b = await request.json()
        lane = b.get("lane", "")
        if not _LANE.match(lane):
            return JSONResponse({"error": {"code": "bad_request", "message": "lane"}}, 400)
        st = await self.event.status(self.args_of(lane))
        ids = [str(x) for x in b.get("subscriptions") or []]
        each: list[dict[str, Any]] = []
        if ids:
            try:
                got = await self.event.status_of([Recipient(user=lane, subscription=i) for i in ids])
                each = [{"id": s.id, "user": s.user, "live": s.live, "connected": s.connected, "via": s.via} for s in got]
            except Exception as err:  # noqa: BLE001 - reported to the bench
                each = [{"id": i, "error": str(err)} for i in ids]
        return JSONResponse({"event": {"subscriptions": st.subscriptions, "connected": st.connected, "via": st.via}, "subscriptions": each})

    async def _note(self, request: Request) -> JSONResponse:
        if not self._authorized(request):
            return JSONResponse({"error": {"code": "unauthenticated", "message": "control token"}}, 401)
        return JSONResponse({"note": self.notes.pop(request.query_params.get("call", ""), None)})

    async def _health(self, _request: Request) -> JSONResponse:
        return JSONResponse({"ok": True})

    def routes(self) -> list[Route]:
        return [
            Route("/health", self._health, methods=["GET"]),
            Route("/bench/emit", self._emit, methods=["POST"]),
            Route("/bench/status", self._status, methods=["POST"]),
            Route("/bench/note", self._note, methods=["GET"]),
        ]

    def serve(self, app: Any) -> None:
        """Serves the framework's ASGI app (MCP at /mcp) with the side API added."""
        import uvicorn

        app.router.routes.extend(self.routes())
        uvicorn.run(app, host="127.0.0.1", port=int(os.environ["PORT"]), log_level="warning")
