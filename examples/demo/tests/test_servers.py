"""Runs each demo server as a process and calls its tools over Streamable HTTP."""

import signal
import subprocess
import sys
from collections.abc import Iterator
from pathlib import Path

import pytest
from mcp import Client

DEMO_DIR = Path(__file__).resolve().parent.parent


@pytest.fixture
def anyio_backend() -> str:
    return "asyncio"


def start(module: str) -> tuple[subprocess.Popen[str], str]:
    """Start a server on a free port and return the process and its MCP URL."""
    proc = subprocess.Popen(
        [sys.executable, "-m", module, "--port", "0"],
        cwd=DEMO_DIR,
        stdout=subprocess.PIPE,
        stderr=subprocess.DEVNULL,
        text=True,
    )
    line = proc.stdout.readline()  # blocks until the port is bound, or EOF
    if not line:
        proc.kill()
        pytest.fail(f"{module} exited before printing its URL (status {proc.wait()})")
    return proc, line.split(": ", 1)[1].strip()


def stop(proc: subprocess.Popen[str]) -> int:
    proc.send_signal(signal.SIGTERM)
    try:
        return proc.wait(timeout=10)
    except subprocess.TimeoutExpired:
        proc.kill()
        raise


@pytest.fixture(scope="module")
def orders_url() -> Iterator[str]:
    proc, url = start("servers.orders")
    yield url
    assert stop(proc) == 0


@pytest.fixture(scope="module")
def shipping_url() -> Iterator[str]:
    proc, url = start("servers.shipping")
    yield url
    assert stop(proc) == 0


# The legacy mode uses the initialize handshake, which is what retrace records
# and replays (ADR 0004).
async def call(url: str, tool: str, arguments: dict) -> tuple[bool, dict | None, str]:
    async with Client(url, mode="legacy") as client:
        result = await client.call_tool(tool, arguments)
    text = "".join(block.text for block in result.content if block.type == "text")
    return result.is_error, result.structured_content, text


def order_ids(structured: dict | None) -> list[str]:
    assert structured is not None
    return [order["order_id"] for order in structured["orders"]]


@pytest.mark.anyio
@pytest.mark.parametrize(
    ("arguments", "want"),
    [
        ({"customer_id": "c_812"}, ["o_1001", "o_1002", "o_1003"]),
        ({"customer_id": "c_812", "status": "open"}, ["o_1003"]),
        ({"customer_id": "c_812", "status": "shipped"}, ["o_1002"]),
        ({"customer_id": "c_404"}, []),
    ],
)
async def test_search_orders(orders_url: str, arguments: dict, want: list[str]) -> None:
    is_error, structured, _ = await call(orders_url, "search_orders", arguments)
    assert not is_error
    assert order_ids(structured) == want


@pytest.mark.anyio
async def test_search_orders_rejects_unknown_status(orders_url: str) -> None:
    is_error, _, _ = await call(orders_url, "search_orders", {"customer_id": "c_812", "status": "lost"})
    assert is_error


@pytest.mark.anyio
async def test_tools_are_listed(orders_url: str, shipping_url: str) -> None:
    async with Client(orders_url, mode="legacy") as client:
        tools = {tool.name: tool for tool in (await client.list_tools()).tools}
    assert sorted(tools) == ["get_order", "search_orders"]
    schema = tools["search_orders"].input_schema
    assert schema["required"] == ["customer_id"]
    assert "status" in schema["properties"]

    async with Client(shipping_url, mode="legacy") as client:
        assert [tool.name for tool in (await client.list_tools()).tools] == ["track_package"]


@pytest.mark.anyio
async def test_get_order(orders_url: str) -> None:
    is_error, structured, _ = await call(orders_url, "get_order", {"order_id": "o_1002"})
    assert not is_error
    assert structured is not None
    assert structured["status"] == "shipped"
    assert structured["tracking_id"] == "trk_7342"

    is_error, _, text = await call(orders_url, "get_order", {"order_id": "o_9999"})
    assert is_error
    assert "no order with id o_9999" in text


@pytest.mark.anyio
async def test_track_package(shipping_url: str) -> None:
    is_error, structured, _ = await call(shipping_url, "track_package", {"tracking_id": "trk_7342"})
    assert not is_error
    assert structured is not None
    assert structured["status"] == "in_transit"
    assert [event["description"] for event in structured["events"]] == ["Picked up", "In transit"]

    is_error, _, text = await call(shipping_url, "track_package", {"tracking_id": "trk_0000"})
    assert is_error
    assert "no package with tracking id trk_0000" in text
