"""The bench's toy app on FastMCP 4 (the fastmcp package), served over
Streamable HTTP as its docs do (http_app with uvicorn), wrapped with
Wakeshift's Python SDK (wakeshift[fastmcp]; WB_APP=py-fastmcp;
bench/app/CONTRACT.md). Shared parts are in toy.py."""

from typing import Annotated

from fastmcp import Context, FastMCP
from pydantic import Field

from toy import BUILTIN_TOOLS, Toy, identify
from wakeshift import ToolCall

toy = Toy()
t = toy.tools
mcp = FastMCP(toy.server_name, version=toy.contract["server"]["version"])
# Its lanes use only their own tools, so attach adds none of its own.
attached = toy.wake.attach(mcp, identify=identify, builtin_tools=BUILTIN_TOOLS)


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
    toy.serve(mcp.http_app(path="/mcp"))
