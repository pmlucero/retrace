# Real-API smoke test and README GIF

A manual checklist for running the demo against the real Anthropic API once, and for recording the README GIF (AC-23, M7 in the [MVP plan](../../docs/plan/mvp.md)). CI never does this: it has no API key, and [e2e.sh](e2e.sh) covers the same flow against the fake.

Cost: a few cents per run. The session is short (one question, three tool calls) and uses Haiku for the recording and Sonnet for the swap-model replay. Check your usage in the Anthropic console.

## Prerequisites

- Go, to build retrace, and [uv](https://docs.astral.sh/uv/).
- [VHS](https://github.com/charmbracelet/vhs), only for the GIF: `brew install vhs`. It is a manual tool, not a CI or Go module dependency.
- An API key in your shell: `export ANTHROPIC_API_KEY=sk-ant-...`. Never put it in a file in the repo. Exact and live-tools replays make no API call, but the agent's Anthropic SDK refuses to start without the variable set (any value), so keep it set for every step.

From the repository root, build retrace and start the servers in two terminals:

```sh
CGO_ENABLED=0 go build -o /tmp/retrace-smoke/retrace ./cmd/retrace
export PATH=/tmp/retrace-smoke:$PATH RETRACE_DIR=/tmp/retrace-smoke/cassettes
cd examples/demo && uv sync --locked
uv run python -m servers.orders --port 9001      # terminal 1
uv run python -m servers.shipping --port 9002    # terminal 2
```

Then, in a third terminal in `examples/demo` with the same `PATH`, `RETRACE_DIR` and key, follow the steps. Exit codes are in [spec §9.7](../../docs/spec/core.md).

## Steps

1. **Record with Haiku.** Without `--anthropic-upstream`, retrace uses `https://api.anthropic.com`.

   ```sh
   retrace record --session checkout \
     --mcp orders=http://127.0.0.1:9001/mcp \
     --mcp shipping=http://127.0.0.1:9002/mcp -- uv run --locked python agent.py
   ```

   Expect exit `0`. The agent prints an answer about order `o_1002` (shipped, tracking `trk_7342`), and retrace logs `session recorded ... status=completed`. If it exits `4`, the agent failed: read its stderr (a bad key shows as an API error).

2. **Inspect it.** `retrace ls` lists `checkout` with `completed` status. `retrace show checkout` shows an `llm` request, MCP `tools/list`, and tool calls `search_orders`, `get_order` and `track_package`, with token counts and an estimated cost..

3. **Exact replay.** Exit `0`. It makes no network call, so you can disconnect to be sure.

   ```sh
   retrace run checkout --as checkout.exact --recorded-command
   retrace diff checkout checkout.exact     # exit 0: "trajectories identical"
   ```

4. **Live-tools replay: a changed shipping API.** This is the divergence the GIF shows, and it makes no API call: the LLM is served from the cassette and MCP goes to a live server. The second shipping server (`--delayed`, port 9003, started by `demo_up`) returns another `track_package` result for `trk_7342`. If you run the steps by hand, start it with `uv run python -m servers.shipping --port 9003 --delayed`.

   ```sh
   retrace run checkout --as checkout.live --tools live --mcp shipping=http://127.0.0.1:9003/mcp --recorded-command
   retrace diff checkout checkout.live
   ```

   Expect exit `3` (the next LLM request holds the new tool result, which the cassette does not) and `diff` exit `1`, with the first divergence at step 3, a `track_package` `tool_result` whose `status` changed from `in_transit` to `delayed`. It is deterministic, so it needs no retakes.

5. **Swap-model replay with Sonnet (optional, not in the GIF).** This calls the real API again.

   ```sh
   retrace run checkout --as checkout.swap --model claude-sonnet-5-5 --recorded-command
   retrace diff checkout checkout.swap
   ```

   Against the real API this usually gives exit `0` and "trajectories identical": Sonnet makes the same tool calls as Haiku. That is a valid outcome, which is why the GIF does not rely on it. Exit `3` and `diff` exit `1` (a tool call the cassette does not hold) is also valid.

6. **No key in the cassette.** The cassette must hold no credentials. The `grep -l` must print nothing and the count must be `0`:

   ```sh
   : "${ANTHROPIC_API_KEY:?}"
   grep -l -a -F "${ANTHROPIC_API_KEY:0:12}" "$RETRACE_DIR"/*.cassette
   retrace show checkout --json | grep -c -F "${ANTHROPIC_API_KEY:0:12}"    # prints 0
   ```

   Also pick an `interaction_id` from `retrace show checkout --json` and check that `retrace show checkout --raw <id>` shows `x-api-key` as redacted. If the key appears anywhere, stop and report it as a security bug (see [SECURITY.md](../../SECURITY.md)). Rotate that key.

## Record the GIF

The tape drives the same commands, with the setup hidden. From the repository root:

```sh
DEMO_FAKE=1 vhs examples/demo/demo.tape    # free rehearsal against the fake
vhs examples/demo/demo.tape                # real API, needs ANTHROPIC_API_KEY
```

It writes `examples/demo/demo.gif`, which is git-ignored: a take is not committed. In rehearsal, the hidden setup also starts the fake on port 9000 and wraps `retrace` so that `record` and any `run` that calls an LLM go to it. The commands shown are the same in both modes. The tape stops the servers when it ends. The ports 9000 to 9003 and 4790 must be free.

The live-tools divergence is deterministic, in rehearsal and with the real API, so one take is enough. The final GIF is placed in the README in a later step.
