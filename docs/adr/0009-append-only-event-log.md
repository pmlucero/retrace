# 0009. Cassette as an append-only event log

- Status: Proposed
- Date: 2026-10-23

## Context

The cassette serves two consumers with different needs:

- **The replayer** needs raw, byte-faithful HTTP interactions: request, response and SSE chunks.
- **The CLI (`show`, `diff`) and the future web UI** (sequence view with animated replay, tool topology, model diff) need a **semantic** view: who talked to whom, in what order, when, for how long, with which tool and arguments, how many tokens and at what cost.

Deriving the semantic view only at read time would couple every consumer to protocol parsing. Storing only the semantic view would lose replay fidelity. The UI also needs data that is only observable at record time, such as wall-clock start times and durations.

## Decision

The cassette has **two layers**, both append-only:

1. **Interactions** (raw layer): one row per HTTP exchange through the proxy, with references to request, response and chunk blobs. This is the source of truth for replay.
2. **Events** (semantic layer): an ordered, append-only log derived from interactions **at record time**. Each event has every field required by the UI from day one: `seq`, `started_at`, `duration_ms`, `source`, `target`, `kind`, `tool`, `arguments`, `result`, `tokens_in`, `tokens_out`, `cost_usd`, `model`, `status`, `error`, plus `interaction_id` and `parent_seq` for causality.

Rules:

- Rows are never updated after they are complete, and never deleted. An in-flight interaction is inserted when the request arrives and completed once when the response ends. This is the only allowed update, and it is idempotent.
- `seq` is a strictly increasing integer per session, assigned when the request arrives. It defines the canonical order even under concurrency.
- Times: `started_at` is wall-clock UTC with microsecond precision, and `duration_ms` is measured with the monotonic clock.
- Events carry a `deriver_version`. If derivation logic improves, `retrace reindex` can rebuild events from interactions. Interactions are never rebuilt.
- Replays are new sessions with their own log, linked to the source session with `parent_session_id` and `mode`. A replay never mutates the cassette it replays.

## Alternatives considered

### Raw interactions only, semantic view computed on read

It is simpler to write, but every reader would re-implement protocol parsing, and timing information that only exists at record time would be lost.

### Semantic events only

It is compact, but loses byte fidelity (SSE framing, unknown fields), which breaks `exact` replay.

### Mutable session document (update in place as the session progresses)

It is harder to make crash-safe, and loses the ordering guarantees the sequence view relies on.

## Consequences

### Positive

- The web UI can be built later on top of existing cassettes without re-recording.
- `diff` operates on a stable, protocol-agnostic event stream.
- Crash safety: a partial session is a valid prefix of a log.

### Negative

- Some data is duplicated between layers. This is accepted for clarity, and blobs are deduplicated anyway.
- The derivation code runs on the hot path while recording. It must be cheap and must never block forwarding.

### Risks and mitigations

- *Derivation bugs corrupt the semantic layer.* `retrace reindex` plus `deriver_version` make it recoverable, because raw interactions are always intact.
