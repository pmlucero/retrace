"""Runs the demo agent end to end against the fake Anthropic API and the MCP servers."""

import subprocess
import sys
from pathlib import Path
from typing import Any

import pytest
from mcp import Client, MCPError

import agent
from fake_anthropic import FINAL_HAIKU, FINAL_SONNET

DEMO_DIR = Path(__file__).resolve().parent.parent


def run_agent(env: dict[str, str]) -> subprocess.CompletedProcess[str]:
    # A minimal environment, so that nothing from the caller's (a real API
    # key, a proxy) reaches the agent.
    return subprocess.run(
        [sys.executable, "agent.py"],
        cwd=DEMO_DIR,
        env={"PATH": "/usr/bin:/bin", **env},
        capture_output=True,
        text=True,
        check=False,
        timeout=120,
    )


@pytest.fixture
def demo_env(anthropic_url: str, orders_url: str, shipping_url: str) -> dict[str, str]:
    return {
        "ANTHROPIC_BASE_URL": anthropic_url,
        "ANTHROPIC_API_KEY": "test-key",
        "RETRACE_MCP_ORDERS_URL": orders_url,
        "RETRACE_MCP_SHIPPING_URL": shipping_url,
    }


# Each run takes a few seconds, mostly importing the SDK, so the cases are few.
@pytest.mark.parametrize(
    ("model", "want"),
    [
        ("claude-haiku-5-5", FINAL_HAIKU),
        ("claude-sonnet-5-5", FINAL_SONNET),
    ],
)
def test_agent_answers(demo_env: dict[str, str], model: str, want: str) -> None:
    result = run_agent({**demo_env, "DEMO_MODEL": model})
    assert result.returncode == 0, result.stderr
    assert result.stdout == want + "\n"


# AC-08 compares the agent's stdout across runs, so it must not change. This
# also checks that DEMO_MODEL defaults to claude-haiku-5-5.
def test_agent_stdout_is_stable(demo_env: dict[str, str]) -> None:
    first = run_agent(demo_env)
    second = run_agent(demo_env)
    assert first.returncode == second.returncode == 0, first.stderr + second.stderr
    assert first.stdout == second.stdout == FINAL_HAIKU + "\n"


@pytest.mark.parametrize(
    ("override", "want"),
    [
        ({"DEMO_MODEL": "claude-unknown"}, "agent: anthropic API error"),
        ({"RETRACE_MCP_SHIPPING_URL": "http://127.0.0.1:1/mcp"}, "agent: cannot connect to the shipping MCP server"),
    ],
)
def test_agent_fails_cleanly(demo_env: dict[str, str], override: dict[str, str], want: str) -> None:
    result = run_agent({**demo_env, **override})
    assert result.returncode == 1
    assert result.stdout == ""
    assert result.stderr.startswith(want)
    assert result.stderr.count("\n") == 1


def tool_use(name: str, arguments: dict[str, Any]) -> dict[str, Any]:
    return {"type": "tool_use", "id": "toolu_test", "name": name, "input": arguments}


@pytest.mark.anyio
async def test_tool_result(orders_url: str) -> None:
    async with Client(orders_url, mode="legacy") as client:
        owners = {"get_order": client}

        result = await agent.tool_result(owners, tool_use("get_order", {"order_id": "o_1002"}))
        assert result.keys() == {"type", "tool_use_id", "content"}
        assert result["type"] == "tool_result"
        assert result["tool_use_id"] == "toolu_test"
        assert "trk_7342" in result["content"]

        result = await agent.tool_result(owners, tool_use("get_order", {"order_id": "o_9999"}))
        assert result["is_error"] is True
        assert "no order with id o_9999" in result["content"]

        result = await agent.tool_result(owners, tool_use("cancel_order", {"order_id": "o_1002"}))
        assert result == {
            "type": "tool_result",
            "tool_use_id": "toolu_test",
            "content": "unknown tool cancel_order",
            "is_error": True,
        }


class MissingRecording:
    """A client whose server answers every call with a JSON-RPC error, as
    retrace does for a request it has no recording of (spec §5, §6.2)."""

    async def call_tool(self, name: str, arguments: dict[str, Any]) -> None:
        raise MCPError(-32099, "retrace: no recorded interaction matches this request")


@pytest.mark.anyio
async def test_tool_result_for_an_mcp_error() -> None:
    owners: dict[str, Any] = {"search_orders": MissingRecording()}
    result = await agent.tool_result(owners, tool_use("search_orders", {"customer_id": "c_812"}))
    assert result["is_error"] is True
    assert "no recorded interaction matches this request" in result["content"]
