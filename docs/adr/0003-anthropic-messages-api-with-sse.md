# 0003. Anthropic Messages API with SSE streaming as the first provider

- Status: Accepted
- Date: 2026-10-23

## Context

The MVP needs exactly one LLM provider, done well, rather than several done superficially. Most real agents stream responses, so a record/replay tool that only handles non-streaming calls would not work with them.

## Decision

The first and only provider in the MVP is the **Anthropic Messages API** (`POST /v1/messages`), supporting both:

- **Non-streaming** responses (`application/json`).
- **Streaming** responses (`text/event-stream`), with the full event sequence: `message_start`, `content_block_start`, `content_block_delta`, `content_block_stop`, `message_delta`, `message_stop`, `ping` and `error`.

Recording:

- The proxy tees the upstream response. The client receives every chunk as soon as it arrives, and in parallel each SSE event is stored with its offset in milliseconds from the start of the response.
- The reassembled message (content blocks, `stop_reason`, `usage`) is derived from the stream and used to build semantic events (see [ADR 0009](0009-append-only-event-log.md)).

Replay:

- Recorded SSE events are re-emitted in order. By default they are emitted immediately. `--realtime` honors the recorded offsets.
- Upstream error responses (4xx, 5xx, `overloaded_error`) are recorded and replayed like any other response.

Other endpoints (`/v1/messages/count_tokens`, `/v1/models`) are forwarded and recorded as opaque interactions without semantic parsing.

The provider is behind an internal `Provider` interface (parse request, parse response or stream, extract usage, fingerprint), so adding OpenAI-compatible APIs later does not touch the recorder or the replayer.

## Alternatives considered

### OpenAI Chat Completions first

It has a larger installed base, and many providers are "OpenAI-compatible". However, the semantics of tool calls and streaming deltas differ across those providers, so "compatible" hides real incompatibilities. Anthropic's API has a single well-specified streaming format and first-class tool use, which suits a precise first implementation.

### Non-streaming only for the MVP

It is simpler, but excludes most real agents and would force a later redesign of cassette storage to hold chunks.

### Provider-agnostic from day one (e.g. via a gateway like LiteLLM)

It adds a dependency and a translation layer, and replay fidelity depends on what the gateway preserves.

## Consequences

### Positive

- Byte-faithful replay of real streaming sessions, including timing when needed.
- Usage (input, output and cache tokens) is available per call for cost estimation.

### Negative

- Agents on other providers cannot use the MVP.
- Swap-model is limited to Anthropic models in the MVP.

### Risks and mitigations

- *The API evolves (new block types, beta headers).* Unknown SSE event types and content blocks are stored and replayed verbatim, never dropped. Semantic parsing ignores what it does not understand.
