"""The demo agent: a shop support assistant that answers one customer question.

It talks to Claude with the official anthropic SDK and to the orders and
shipping MCP servers over Streamable HTTP, and prints Claude's final answer.
It is configured only through the environment, so retrace can point it at its
proxy without changing the code (spec §3.2):

- ANTHROPIC_BASE_URL and ANTHROPIC_API_KEY, read by the SDK.
- RETRACE_MCP_ORDERS_URL and RETRACE_MCP_SHIPPING_URL, the MCP endpoints
  (default: the servers' default URLs).
- DEMO_MODEL, the model (default claude-haiku-5-5).

Every request it sends is built only from fixed text, the servers' tool lists
and earlier responses: no timestamps, no random ids. It does not set
temperature: the API version that SDK 2.x sends rejects it. Only the final
answer goes to stdout; errors go to stderr.

Run from examples/demo with: uv run python agent.py
"""

import asyncio
import os
import sys
from collections.abc import Mapping
from contextlib import AsyncExitStack
from typing import Any

import anthropic
from mcp import Client, MCPError

SERVERS = {
    "orders": ("RETRACE_MCP_ORDERS_URL", "http://127.0.0.1:9001/mcp"),
    "shipping": ("RETRACE_MCP_SHIPPING_URL", "http://127.0.0.1:9002/mcp"),
}
DEFAULT_MODEL = "claude-haiku-5-5"
MAX_TOKENS = 1024
MAX_TURNS = 10

# The date is fixed, and inside the shop data's dates, so that answers about
# "when" are the same on every run (#115).
SYSTEM = (
    "You are a customer support assistant for a small online shop that sells "
    "mugs, kettles, teapots and tea. Today's date is 2026-10-22. Use the tools "
    "to look up the customer's orders and parcels, and answer in two or three "
    "sentences."
)

# The status filter of search_orders is optional, so whether the model passes
# status "shipped" here or fetches every order depends on the model.
TASK = "Hi, I'm customer c_812. Where is my order that has shipped, and when will it arrive?"


class AgentError(Exception):
    """An error that ends the run with a one-line message and exit status 1."""


async def connect(stack: AsyncExitStack) -> tuple[dict[str, Client], list[dict[str, Any]]]:
    """Connect to the MCP servers and return the client that owns each tool, and the tools for Claude.

    Servers come in a fixed order, and tools in the order each server lists
    them, so the tools sent to Claude are the same on every run.
    """
    owners: dict[str, Client] = {}
    tools: list[dict[str, Any]] = []
    for server, (env, default) in SERVERS.items():
        url = os.environ.get(env) or default
        try:
            # The legacy mode uses the initialize handshake, which is what
            # retrace records and replays (ADR 0004).
            client = await stack.enter_async_context(Client(url, mode="legacy"))
            listed = (await client.list_tools()).tools
        except Exception as err:
            raise AgentError(f"cannot connect to the {server} MCP server at {url}: {first_error(err)}") from err
        for tool in listed:
            if tool.name in owners:
                raise AgentError(f"tool {tool.name} is offered by two MCP servers")
            owners[tool.name] = client
            spec: dict[str, Any] = {"name": tool.name, "input_schema": tool.input_schema}
            if tool.description:
                spec["description"] = tool.description
            tools.append(spec)
    return owners, tools


async def tool_result(owners: Mapping[str, Client], block: dict[str, Any]) -> dict[str, Any]:
    """Run a tool_use block on the server that owns the tool and return its tool_result.

    A tool that fails, an MCP error answer (such as retrace's answer to a
    request it has no recording of) and an unknown tool all go back to the
    model with is_error, rather than ending the run, so that it can recover.
    """
    client = owners.get(block["name"])
    if client is None:
        text, is_error = f"unknown tool {block['name']}", True
    else:
        try:
            result = await client.call_tool(block["name"], block["input"])
        except MCPError as err:
            text, is_error = f"MCP error: {err}", True
        else:
            text = "".join(item.text for item in result.content if item.type == "text")
            is_error = result.is_error
    answer: dict[str, Any] = {"type": "tool_result", "tool_use_id": block["id"], "content": text}
    if is_error:
        answer["is_error"] = True
    return answer


async def run(model: str) -> str:
    """Run the tool-use loop and return Claude's final answer."""
    async with AsyncExitStack() as stack:
        owners, tools = await connect(stack)

        # No retries: a retried request would be recorded twice.
        claude = anthropic.AsyncAnthropic(max_retries=0)
        messages: list[dict[str, Any]] = [{"role": "user", "content": TASK}]
        for _ in range(MAX_TURNS):
            try:
                response = await claude.messages.create(
                    model=model,
                    max_tokens=MAX_TOKENS,
                    system=SYSTEM,
                    tools=tools,
                    messages=messages,
                )
            except anthropic.APIError as err:
                raise AgentError(f"anthropic API error: {err}") from err

            # Sent back as the API returned it (only the fields it set), which
            # keeps any thinking blocks unchanged, as the API requires.
            content = [block.to_dict() for block in response.content]
            messages.append({"role": "assistant", "content": content})

            if response.stop_reason != "tool_use":
                if response.stop_reason != "end_turn":
                    raise AgentError(f"the model stopped with stop_reason {response.stop_reason}")
                return "".join(block["text"] for block in content if block["type"] == "text")

            results = [await tool_result(owners, block) for block in content if block["type"] == "tool_use"]
            messages.append({"role": "user", "content": results})

        raise AgentError(f"no final answer after {MAX_TURNS} turns")


def first_error(err: BaseException) -> BaseException:
    """Return the first leaf of an exception group, which names the cause."""
    while isinstance(err, BaseExceptionGroup) and err.exceptions:
        err = err.exceptions[0]
    return err


def main() -> None:
    model = os.environ.get("DEMO_MODEL") or DEFAULT_MODEL
    try:
        answer = asyncio.run(run(model))
    # Any failure ends the run with one line on stderr rather than a traceback.
    except Exception as err:
        cause = first_error(err)
        message = str(cause) if isinstance(cause, AgentError) else f"{type(cause).__name__}: {cause}"
        # One line: some causes (httpx, MCP) carry multi-line messages.
        sys.exit("agent: " + " ".join(message.split()))
    print(answer)


if __name__ == "__main__":
    main()
