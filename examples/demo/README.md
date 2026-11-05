# retrace demo

The demo shows retrace working with an agent written in another language, against real MCP servers (AC-23 in the [spec](../../docs/spec/core.md), M7 in the [MVP plan](../../docs/plan/mvp.md)). It is a Python project managed with [uv](https://docs.astral.sh/uv/), separate from the Go module.

It contains a support agent, the two MCP servers it uses, and a scripted fake of the Anthropic API that lets the demo run without an API key. The end-to-end flow in CI (record, exact replay, swap-model replay, diff) comes in a later pull request ([#22](https://github.com/pmlucero/retrace/issues/22)).

## The servers

Both servers are written with the official [`mcp`](https://pypi.org/project/mcp/) Python SDK and speak MCP over Streamable HTTP. They serve a small fictional shop from fixed, in-memory data, so every run gives the same answers and nothing touches the network or the disk.

| Server | Default URL | Tools |
|---|---|---|
| `orders` | `http://127.0.0.1:9001/mcp` | `search_orders(customer_id, status=None)`, `get_order(order_id)` |
| `shipping` | `http://127.0.0.1:9002/mcp` | `track_package(tracking_id)` |

- `search_orders` lists a customer's orders. `status` is optional and is one of `open`, `shipped` or `delivered`. An agent that is asked about a shipped order may pass `status: "shipped"`, or fetch every order and filter itself. Which one it does depends on the model, which is why a swap-model replay of the demo is likely to diverge.
- `get_order` returns one order, with its items and, once shipped, its tracking id.
- `track_package` returns a parcel's carrier, status and tracking events.

The data is in [servers/shop.py](servers/shop.py). Customer `c_812`, the one in the spec's and the README's examples, has three orders: `o_1001` (delivered), `o_1002` (shipped, tracking `trk_7342`) and `o_1003` (open).

## The agent

[agent.py](agent.py) is a support assistant for the shop. It uses the official [`anthropic`](https://pypi.org/project/anthropic/) Python SDK and the `mcp` client. It asks Claude one fixed question, from customer `c_812`: where is my order that has shipped, and when will it arrive? It gives Claude the servers' tools under their own names, runs each `tool_use` on the server that owns the tool, and prints Claude's final answer on stdout. Its system prompt says today is 2026-10-22, a date inside the shop's data, so the answer does not depend on when the demo runs.

It is configured only through the environment, which is what `retrace record` and `retrace run` set (spec §3.2):

| Variable | Default | Meaning |
|---|---|---|
| `ANTHROPIC_BASE_URL` | `https://api.anthropic.com` | The Anthropic API, read by the SDK |
| `ANTHROPIC_API_KEY` | none | The API key, read by the SDK |
| `RETRACE_MCP_ORDERS_URL` | `http://127.0.0.1:9001/mcp` | The `orders` server |
| `RETRACE_MCP_SHIPPING_URL` | `http://127.0.0.1:9002/mcp` | The `shipping` server |
| `DEMO_MODEL` | `claude-haiku-5-5` | The model |

Every request it sends is the same on every run. A tool call that fails goes back to Claude as a `tool_result` with `is_error: true`. Errors it cannot recover from (a server it cannot reach, an API error, no answer after 10 turns) print one line to stderr and exit with status 1.

## The fake Anthropic API

[fake_anthropic.py](fake_anthropic.py) is a test double for CI, not an emulation of the real API. It serves `POST /v1/messages`, non-streaming only, with canned responses. It picks a script by the request's `model`, and the step by how many assistant messages the request already holds, so the same request always gets the same response:

| Model | Tool calls, in order |
|---|---|
| `claude-haiku-5-5` | `search_orders {"customer_id": "c_812", "status": "shipped"}`, `get_order {"order_id": "o_1002"}`, `track_package {"tracking_id": "trk_7342"}` |
| `claude-sonnet-5-5` | the same, but `search_orders {"customer_id": "c_812"}`, without `status` |

Then each script ends with a final answer. Because the first call differs, a session recorded with `claude-haiku-5-5` and replayed with `--model claude-sonnet-5-5` diverges at step 1. Another model gets a `404 not_found_error`, a request with `"stream": true` or past the end of a script a `400 invalid_request_error`. It uses only the standard library.

## Running it

Install uv, then, from this directory, start the servers and the fake, each in its own terminal:

```sh
uv sync --locked
uv run python -m servers.orders      # orders: http://127.0.0.1:9001/mcp
uv run python -m servers.shipping    # shipping: http://127.0.0.1:9002/mcp
uv run python fake_anthropic.py      # fake-anthropic: http://127.0.0.1:9000
```

Each one listens on `127.0.0.1` only and prints `<name>: <url>` as its first line when it is ready. `--port <n>` picks another port, and `--port 0` a free one. Ctrl+C or `SIGTERM` stops it cleanly.

Then run the agent against them:

```sh
ANTHROPIC_BASE_URL=http://127.0.0.1:9000 ANTHROPIC_API_KEY=test \
  uv run python agent.py
DEMO_MODEL=claude-sonnet-5-5 ANTHROPIC_BASE_URL=http://127.0.0.1:9000 ANTHROPIC_API_KEY=test \
  uv run python agent.py
```

Through retrace, the proxy sets `ANTHROPIC_BASE_URL` and the `RETRACE_MCP_*_URL` variables for the agent, and `--anthropic-upstream` points it at the fake. The MCP endpoint is the path `/mcp`, and `--mcp <name>=<url>` takes the full URL:

```sh
ANTHROPIC_API_KEY=test retrace record --session checkout \
  --anthropic-upstream http://127.0.0.1:9000 \
  --mcp orders=http://127.0.0.1:9001/mcp \
  --mcp shipping=http://127.0.0.1:9002/mcp -- uv run python agent.py
```

Without `--anthropic-upstream` and with a real `ANTHROPIC_API_KEY`, the same command records a session with the real API.

## Tests

```sh
uv run pytest
```

The tests start the servers and the fake on free ports. They call the servers' tools through the `mcp` client over Streamable HTTP, check the fake's scripts and errors over HTTP, and run the agent against them for both models. CI runs them in the `demo` workflow when this directory changes.

## Dependencies

`uv.lock` pins every package. Releases must be at least 14 days old ([CONTRIBUTING.md](../../CONTRIBUTING.md#dependencies), *Dependencies*): `exclude-newer = "14 days"` in `pyproject.toml` makes `uv lock` ignore newer ones, and Dependabot proposes updates with the same cooldown.
