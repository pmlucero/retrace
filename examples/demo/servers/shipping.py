"""The demo shipping MCP server: tracks the parcels of shipped orders.

Run from examples/demo with: uv run python -m servers.shipping [--port 9002] [--delayed]
"""

import sys
from typing import Any

from mcp.server.mcpserver import MCPServer
from mcp.server.mcpserver.exceptions import ToolError

from servers.shop import PACKAGES, serve

DEFAULT_PORT = 9002

# --delayed makes trk_7342 come back late. The demo's live-tools step points a
# replay at a second server started with it, to show a changed tool result.
# The default data, which the recorded sessions and the fake rely on, is unchanged.
DELAYED = "--delayed" in sys.argv
if DELAYED:
    # serve() parses the command line and does not know this flag.
    sys.argv.remove("--delayed")

DELAYED_UPDATE = {
    "status": "delayed",
    "estimated_delivery": "2026-10-29",
    "events": [
        *PACKAGES["trk_7342"]["events"],
        {"date": "2026-10-22", "location": "Central hub", "description": "Delayed: carrier backlog"},
    ],
}

mcp = MCPServer("shipping", version="0.1.0")


@mcp.tool()
def track_package(tracking_id: str) -> dict[str, Any]:
    """Get a parcel's carrier, status, estimated delivery date and tracking events."""
    package = PACKAGES.get(tracking_id)
    if package is None:
        raise ToolError(f"no package with tracking id {tracking_id}")
    if DELAYED and tracking_id == "trk_7342":
        return {**package, **DELAYED_UPDATE}
    return package


if __name__ == "__main__":
    serve(mcp, DEFAULT_PORT)
