"""The fictional shop behind the demo MCP servers, and the code that runs them.

The data is fixed and in memory, so every run of the demo answers the same
questions with the same results, and nothing touches the network or the disk.
Customers, orders and parcels are invented.
"""

import argparse
import signal
import socket
import sys

import uvicorn
from mcp.server.mcpserver import MCPServer

HOST = "127.0.0.1"

# The path where the SDK serves Streamable HTTP. retrace's --mcp <name>=<url>
# takes the full URL, e.g. http://127.0.0.1:9001/mcp.
MCP_PATH = "/mcp"

ORDERS = {
    "o_1001": {
        "order_id": "o_1001",
        "customer_id": "c_812",
        "status": "delivered",
        "placed_at": "2026-09-14",
        "items": [{"sku": "mug-blue", "name": "Blue mug", "quantity": 2, "unit_price": 12.5}],
        "total": 25.0,
        "tracking_id": "trk_7301",
    },
    "o_1002": {
        "order_id": "o_1002",
        "customer_id": "c_812",
        "status": "shipped",
        "placed_at": "2026-10-19",
        "items": [{"sku": "kettle-steel", "name": "Steel kettle", "quantity": 1, "unit_price": 39.0}],
        "total": 39.0,
        "tracking_id": "trk_7342",
    },
    "o_1003": {
        "order_id": "o_1003",
        "customer_id": "c_812",
        "status": "open",
        "placed_at": "2026-10-22",
        "items": [
            {"sku": "tea-green", "name": "Green tea, 100 g", "quantity": 3, "unit_price": 6.0},
            {"sku": "mug-blue", "name": "Blue mug", "quantity": 1, "unit_price": 12.5},
        ],
        "total": 30.5,
        "tracking_id": None,
    },
    "o_1004": {
        "order_id": "o_1004",
        "customer_id": "c_455",
        "status": "open",
        "placed_at": "2026-10-21",
        "items": [{"sku": "teapot-clay", "name": "Clay teapot", "quantity": 1, "unit_price": 45.0}],
        "total": 45.0,
        "tracking_id": None,
    },
}

PACKAGES = {
    "trk_7301": {
        "tracking_id": "trk_7301",
        "carrier": "Parcelo",
        "status": "delivered",
        "estimated_delivery": "2026-09-18",
        "events": [
            {"date": "2026-09-15", "location": "Shop warehouse", "description": "Picked up"},
            {"date": "2026-09-16", "location": "Central hub", "description": "In transit"},
            {"date": "2026-09-18", "location": "Customer address", "description": "Delivered"},
        ],
    },
    "trk_7342": {
        "tracking_id": "trk_7342",
        "carrier": "Parcelo",
        "status": "in_transit",
        "estimated_delivery": "2026-10-24",
        "events": [
            {"date": "2026-10-20", "location": "Shop warehouse", "description": "Picked up"},
            {"date": "2026-10-21", "location": "Central hub", "description": "In transit"},
        ],
    },
}


def serve(server: MCPServer, default_port: int) -> None:
    """Serve server over Streamable HTTP on loopback until SIGINT or SIGTERM.

    Prints the MCP endpoint URL once the port is bound, so that a script (or a
    test, with --port 0) can read the actual port from the first line of stdout.
    """
    parser = argparse.ArgumentParser(description=f"Run the demo {server.name} MCP server.")
    parser.add_argument(
        "--port",
        type=int,
        default=default_port,
        help=f"TCP port on {HOST} (default {default_port}; 0 picks a free port)",
    )
    args = parser.parse_args()

    # Binding here, rather than in uvicorn, gives the real port for --port 0
    # and a one-line error for a port in use. Connections that arrive before
    # uvicorn starts accepting wait in the listen backlog.
    sock = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    sock.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    try:
        sock.bind((HOST, args.port))
    except OSError as err:
        sys.exit(f"{server.name}: cannot listen on {HOST}:{args.port}: {err.strerror}")
    sock.listen()
    port = sock.getsockname()[1]

    # uvicorn shuts down gracefully on SIGINT/SIGTERM and then raises the signal
    # again, which would end the process with a traceback or exit status 143.
    # Exiting with 0 here makes a stop by signal a clean stop.
    for sig in (signal.SIGINT, signal.SIGTERM):
        signal.signal(sig, lambda *_: sys.exit(0))

    app = server.streamable_http_app(streamable_http_path=MCP_PATH, host=HOST)
    config = uvicorn.Config(
        app,
        log_level="warning",
        # An open SSE stream must not keep the server from stopping.
        timeout_graceful_shutdown=2,
    )
    print(f"{server.name}: http://{HOST}:{port}{MCP_PATH}", flush=True)
    uvicorn.Server(config).run(sockets=[sock])
