# 0006. `strict` as the default divergence strategy

- Status: Proposed
- Date: 2026-10-23

## Context

During a partial replay the agent eventually sends a request that the cassette cannot answer. This is a **cassette miss**:

- In `swap-model`, the new model asks for a tool call (or arguments) that was never recorded.
- In `live-tools`, a live tool returns something different. The agent's next LLM request then no longer matches any recorded one.
- In `exact`, the agent itself behaves differently (code change, non-determinism in the agent).

Several responses are possible, and they trade fidelity against how far the replay can get:

| Strategy | Behavior on miss | Fidelity |
|---|---|---|
| `strict` | Stop and report the divergence point | Full: nothing invented |
| `fallthrough` | Forward to the real upstream (real tool or model) | Real, but the comparison is no longer controlled |
| `nearest-match` | Return the most similar recorded response | Approximate |
| `llm-stub` | Ask an LLM to synthesize a plausible response, marked synthetic | Invented |

## Decision

1. The **default and only MVP strategy is `strict`**. On a cassette miss retrace:
   1. Records a `divergence` event with the expected vs. received request (see [ADR 0007](0007-request-matching-fingerprints.md)).
   2. Answers the agent with an explicit error: a JSON-RPC error (code `-32099`, message `retrace: cassette miss`) for MCP, and HTTP `422` with an Anthropic-shaped error body (`{"type":"error","error":{"type":"retrace_cassette_miss",...}}`) for the LLM API. A 4xx is used on purpose: SDKs retry 5xx responses automatically, which would only repeat the miss.
   3. Terminates the agent process: `SIGTERM`, then `SIGKILL` after a grace period (default 5 s). This stops it from continuing on top of an error it might misinterpret.
   4. Exits with code `3` and prints the divergence point.
2. Divergence handling is behind an internal `DivergenceStrategy` interface, selected with `--on-miss <strategy>`. The MVP accepts only `strict`. The other values are reserved and rejected with a clear "not implemented" message.
3. Any future strategy that produces a response not taken verbatim from the cassette must mark the resulting event with `provenance = fallthrough | nearest | synthetic`. That marking is visible in `show`, `diff` and the UI. It is a hard invariant, from the vision's "never fabricate silently" principle.

## Alternatives considered

### `fallthrough` as default
It is more convenient, because replays "just work". However, it silently turns a controlled experiment into an uncontrolled one, which defeats the purpose of swap-model.

### Returning an error to the agent without terminating it
It is less disruptive, but agents often retry or improvise after a tool error, so the resulting trajectory is an artifact of the replay, not of the model. It may become an option (`--on-miss strict --keep-running`) later.

## Consequences

### Positive
- Every replay result is trustworthy: if it completed, nothing was invented.
- The divergence point is exactly the most interesting fact in a model comparison, and `strict` surfaces it directly.

### Negative
- swap-model replays will often stop early when the new model explores differently. That is expected and documented as a finding, not a failure of the tool.

### Risks and mitigations
- *Users perceive early stops as bugs.* The CLI output explains the divergence and suggests the reserved strategies on the roadmap.
