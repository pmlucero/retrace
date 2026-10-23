# 0007. Request matching by canonical fingerprint

- Status: Proposed
- Date: 2026-10-23

## Context

Replay depends on one question: **is this incoming request "the same" as a recorded one?** The answer defines what a cassette miss is ([ADR 0006](0006-strict-divergence-default.md)), and it must behave sensibly when:

- The agent sends requests in a slightly different order (parallel tool calls, concurrent subagents).
- The same request legitimately happens more than once (e.g. calling `get_time` twice).
- Some fields vary between runs without semantic meaning: JSON-RPC ids, key order, `_meta.progressToken`, auth headers.

## Decision

Matching is **fingerprint-based with per-fingerprint FIFO queues**, not position-based.

1. **Fingerprint** = SHA-256 of a canonical form of the request:
   - JSON canonicalized: keys sorted, insignificant whitespace removed, numbers in their original textual form.
   - Headers are never part of the fingerprint.
   - **LLM (Anthropic)**: the request body without `stream` and `metadata`. All other fields are included (`model`, `system`, `messages`, `tools`, `tool_choice`, sampling parameters). Because `messages` contains the whole conversation so far, the fingerprint implicitly encodes the trajectory up to that point.
   - **MCP**: `server name + method + params`, excluding JSON-RPC `id` and `params._meta`.
2. **Queues**: during replay every recorded interaction is placed in a queue keyed by its fingerprint, in recorded order. An incoming request with fingerprint F consumes the head of queue F. If queue F is empty, that is a **cassette miss**.
3. **Which channels are matched depends on the mode**:

   | Mode | LLM requests | MCP requests |
   |---|---|---|
   | `exact` | matched | matched |
   | `swap-model` | real (not matched) | matched |
   | `live-tools` | matched | real (not matched) |

   In `live-tools` a changed tool result changes the next LLM request's `messages`, so the fingerprint misses. Tool drift is therefore detected as a divergence with no extra mechanism.
4. **Miss diagnostics**: on a miss retrace finds the closest recorded request on the same channel (the next unconsumed interaction in recorded order) and stores a structural diff (JSON paths that differ) in the `divergence` event. This is for reporting only and never used to answer.
5. **Unconsumed interactions** at the end of a replay are reported as a warning ("the agent finished before using N recorded interactions"), not as a failure.
6. Normalization is defined in code in one place per provider or transport, with golden tests. User-configurable ignore rules (e.g. a timestamp inside the system prompt) are a future extension (`--ignore <jsonpath>`).

## Alternatives considered

### Strict sequential matching (next recorded interaction must match)
It is simpler and detects any reordering. However, it produces false divergences under concurrency (parallel tool calls, subagents), which are common in real agents.

### Matching on method plus URL only (typical HTTP VCR)
It is too loose for LLM calls, because every request is `POST /v1/messages`, so it would replay wrong answers silently.

### Semantic or embedding similarity
It is non-deterministic and opaque. It belongs to the future `nearest-match` strategy, not to the core.

## Consequences

### Positive
- Robust to reordering and concurrency, and exact about content.
- `live-tools` divergence detection falls out of the design.
- Misses carry a precise "expected vs. received" diff, which is the input for `retrace diff` and the UI.

### Negative
- Any byte-level change in the agent's prompts, such as a timestamp in the system prompt, causes misses in `exact` mode until ignore rules exist.

### Risks and mitigations
- *Canonicalization bugs cause false misses.* Mitigation: canonicalization is pure, heavily unit-tested, and the canonical form is stored with each interaction, so a mismatch can be inspected.
