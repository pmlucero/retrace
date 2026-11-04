"""The demo orders MCP server: finds a customer's orders.

Run from examples/demo with: uv run python -m servers.orders [--port 9001]
"""

from typing import Any, Literal

from mcp.server.mcpserver import MCPServer
from mcp.server.mcpserver.exceptions import ToolError

from servers.shop import ORDERS, serve

DEFAULT_PORT = 9001

mcp = MCPServer("orders", version="0.1.0")


# status is optional on purpose: whether a model passes it is a choice that can
# change when the model changes, which is what a swap-model replay detects.
@mcp.tool()
def search_orders(
    customer_id: str,
    status: Literal["open", "shipped", "delivered"] | None = None,
) -> dict[str, Any]:
    """List a customer's orders, oldest first, optionally only those with one status."""
    orders = [
        order
        for order in ORDERS.values()
        if order["customer_id"] == customer_id and (status is None or order["status"] == status)
    ]
    return {"orders": orders}


@mcp.tool()
def get_order(order_id: str) -> dict[str, Any]:
    """Get one order by its id, including its items and its tracking id once shipped."""
    order = ORDERS.get(order_id)
    if order is None:
        raise ToolError(f"no order with id {order_id}")
    return order


if __name__ == "__main__":
    serve(mcp, DEFAULT_PORT)
