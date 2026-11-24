"""The bench's toy app on the official MCP Python SDK 2.x's MCPServer, served
over Streamable HTTP as its docs do (streamable_http_app with uvicorn),
wrapped with Wakeshift's Python SDK (WB_APP=py-mcpserver;
bench/app/CONTRACT.md). Shared parts are in toy.py."""

from typing import Annotated

from mcp.server.mcpserver import Context, MCPServer
from pydantic import Field

from toy import BUILTIN_TOOLS, Toy, identify
from wakeshift import ToolCall

toy = Toy()
t = toy.tools
server = MCPServer(toy.server_name)
# Its lanes use only their own tools, so attach adds none of its own.
attached = toy.wake.attach(server, identify=identify, builtin_tools=BUILTIN_TOOLS)


@attached.tool(description=t["watch"]["description"], subscribes=[toy.event])
async def watch(call: ToolCall, ctx: Context) -> str:
    return await toy.watch(call, ctx)


@attached.tool(description=t["unwatch"]["description"], subscribes=[])
async def unwatch(call: ToolCall, ctx: Context) -> str:
    return await toy.unwatch(call, ctx)


@attached.tool(description=t["report_seen"]["description"], subscribes=[])
async def report_seen(
    ids: Annotated[list[str], Field(description=t["report_seen"]["idsDescription"])], call: ToolCall, ctx: Context
) -> str:
    return toy.report_seen(ids, ctx)


if __name__ == "__main__":
    toy.serve(server.streamable_http_app())
