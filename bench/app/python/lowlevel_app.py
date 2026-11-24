"""The bench's toy app on the official MCP Python SDK 2.x's low-level Server
(mcp.server.lowlevel), with on_list_tools and on_call_tool handlers as its
docs write them, served over Streamable HTTP with uvicorn, and Wakeshift
attached (WB_APP=py-lowlevel; bench/app/CONTRACT.md). Shared parts are in
toy.py.

A low-level Server has no tool registry, so attach's wrap names the three
tools (tools/list gets Wakeshift's arguments, each reply its block) and the
call handler finds the call with wake.current_call()."""

from typing import Any

import mcp_types as types
from mcp.server.lowlevel import Server

from toy import BUILTIN_TOOLS, Toy, identify

toy = Toy()
t = toy.tools
NO_INPUT: dict[str, Any] = {"type": "object", "properties": {}}

TOOLS = [
    types.Tool(name="watch", description=t["watch"]["description"], input_schema=NO_INPUT),
    types.Tool(name="unwatch", description=t["unwatch"]["description"], input_schema=NO_INPUT),
    types.Tool(
        name="report_seen",
        description=t["report_seen"]["description"],
        input_schema={
            "type": "object",
            "properties": {
                "ids": {"type": "array", "items": {"type": "string"}, "description": t["report_seen"]["idsDescription"]}
            },
            "required": ["ids"],
        },
    ),
]


def _text(s: str) -> types.CallToolResult:
    return types.CallToolResult(content=[types.TextContent(type="text", text=s)])


async def list_tools(ctx: Any, params: Any) -> types.ListToolsResult:
    return types.ListToolsResult(tools=TOOLS)


async def call_tool(ctx: Any, params: types.CallToolRequestParams) -> types.CallToolResult:
    args = params.arguments or {}
    if params.name == "watch":
        return _text(await toy.watch(toy.wake.current_call(), ctx))
    if params.name == "unwatch":
        return _text(await toy.unwatch(toy.wake.current_call(), ctx))
    if params.name == "report_seen":  # named in wrap, so Wakeshift's block goes on this reply too
        return _text(toy.report_seen([str(x) for x in args.get("ids") or []], ctx))
    raise ValueError(f"unknown tool {params.name}")


server = Server(toy.server_name, version=toy.contract["server"]["version"], on_list_tools=list_tools, on_call_tool=call_tool)
# Its lanes use only their own tools, so attach adds none of its own.
toy.wake.attach(
    server,
    identify=identify,
    builtin_tools=BUILTIN_TOOLS,
    wrap={"watch": [toy.event], "unwatch": [], "report_seen": []},
)

if __name__ == "__main__":
    toy.serve(server.streamable_http_app())
