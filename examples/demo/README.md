# retrace demo

The demo shows retrace working with an agent written in another language, against real MCP servers (AC-23 in the [spec](../../docs/spec/core.md), M7 in the [MVP plan](../../docs/plan/mvp.md)). It is a Python project managed with [uv](https://docs.astral.sh/uv/), separate from the Go module.

So far it contains the two MCP servers. The demo agent and the end-to-end flow (record, exact replay, swap-model replay, diff) come in the next pull requests ([#22](https://github.com/pmlucero/retrace/issues/22)).

## The servers

Both servers are written with the official [`mcp`](https://pypi.org/project/mcp/) Python SDK and speak MCP over Streamable HTTP. They serve a small fictional shop from fixed, in-memory data, so every run gives the same answers and nothing touches the network or the disk.

| Server | Default URL | Tools |
|---|---|---|
| `orders` | `http://127.0.0.1:9001/mcp` | `search_orders(customer_id, status=None)`, `get_order(order_id)` |
| `shipping` | `http://127.0.0.1:9002/mcp` | `track_package(tracking_id)` |

- `search_orders` lists a customer's orders. `status` is optional and is one of `open`, `shipped` or `delivered`. An agent that is asked about an open order may pass `status: "open"`, or fetch every order and filter itself. Which one it does depends on the model, which is why a swap-model replay of the demo is likely to diverge.
- `get_order` returns one order, with its items and, once shipped, its tracking id.
- `track_package` returns a parcel's carrier, status and tracking events.

The data is in [servers/shop.py](servers/shop.py). Customer `c_812`, the one in the spec's and the README's examples, has three orders: `o_1001` (delivered), `o_1002` (shipped, tracking `trk_7342`) and `o_1003` (open).

## Running them

Install uv, then, from this directory:

```sh
uv sync --locked
uv run python -m servers.orders      # orders: http://127.0.0.1:9001/mcp
uv run python -m servers.shipping    # shipping: http://127.0.0.1:9002/mcp
```

Each server listens on `127.0.0.1` only and prints its URL when it is ready. `--port <n>` picks another port, and `--port 0` a free one. Ctrl+C or `SIGTERM` stops it cleanly.

The MCP endpoint is the path `/mcp`. retrace's `--mcp <name>=<url>` takes the full URL, for example:

```sh
retrace record --session checkout \
  --mcp orders=http://127.0.0.1:9001/mcp \
  --mcp shipping=http://127.0.0.1:9002/mcp -- <agent command>
```

## Tests

```sh
uv run pytest
```

The tests start each server on a free port and call its tools through the `mcp` client over Streamable HTTP. CI runs them in the `demo` workflow when this directory changes.

## Dependencies

`uv.lock` pins every package. Releases must be at least 14 days old ([CONTRIBUTING.md](../../CONTRIBUTING.md#dependencies), *Dependencies*): `exclude-newer = "14 days"` in `pyproject.toml` makes `uv lock` ignore newer ones, and Dependabot proposes updates with the same cooldown.
