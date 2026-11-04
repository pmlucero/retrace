"""The demo shipping MCP server: tracks the parcels of shipped orders.

Run from examples/demo with: uv run python -m servers.shipping [--port 9002]
"""

from typing import Any

from mcp.server.mcpserver import MCPServer
from mcp.server.mcpserver.exceptions import ToolError

from servers.shop import PACKAGES, serve

DEFAULT_PORT = 9002

mcp = MCPServer("shipping", version="0.1.0")


@mcp.tool()
def track_package(tracking_id: str) -> dict[str, Any]:
    """Get a parcel's carrier, status, estimated delivery date and tracking events."""
    package = PACKAGES.get(tracking_id)
    if package is None:
        raise ToolError(f"no package with tracking id {tracking_id}")
    return package


if __name__ == "__main__":
    serve(mcp, DEFAULT_PORT)
