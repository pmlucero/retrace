# MVP implementation plan

- Status: Approved
- Last updated: 2026-10-23
- Spec: [core.md](../spec/core.md) v0.4 (updated whenever the spec version changes)

This plan orders the implementation of the MVP defined in the [core spec](../spec/core.md). It says *in what order* things are built and *how* progress is tracked. *What* is built is defined by the spec and the [ADRs](../adr/README.md). If they disagree, the spec wins, and this plan is updated.

## Strategy: vertical slice first

Instead of building layer by layer, the plan reaches a working end-to-end slice as early as possible: **record and replay an agent that only talks to the LLM** (M3). The riskiest parts surface early, while they are still cheap to change:

- Forwarding SSE streams without buffering while recording them (AC-03).
- Canonicalization and fingerprint matching (ADR 0007).
- Byte-faithful replay (AC-08, AC-10).

MCP, swap-model, divergence handling and diff are then added on a proven base.

## Milestones

Each milestone is scoped by **spec sections** (what must be implemented) and **acceptance criteria** (what must be tested). Many normative requirements have no AC of their own, such as body redaction, `reindex` or exit code `4`. The spec sections column makes sure they land in a milestone.

| # | Milestone | Scope | Spec sections | Acceptance criteria |
|---|---|---|---|---|
| M0 | Go skeleton and CI | `go.mod`, `cmd/retrace` with `--version`, `internal/` layout, subcommand dispatch, global flags, golangci-lint, Go CI jobs as required checks, Dependabot for `gomod` | §9.1 (flags), §9.7 (codes `1`, `2`) | — |
| M1 | Cassette store | SQLite schema, embedded forward-only migrations, refusal to write newer schemas, content-addressed blobs, append-only writer behind a bounded queue (see *Design points*), reader | §7.1–7.4, §7.6 | Foundation for AC-05, AC-07 |
| M2 | Record: LLM channel | Proxy and router, session id validation, child process with injected environment, Anthropic Messages API (JSON and SSE), header **and body** redaction, signal handling, LLM event derivation, embedded price table, `retrace reindex`, cassette warning in `record --help` | §3.1–3.3, §3.5, §7.4 (LLM), §7.5, §7.6 (`reindex`), §9.2, §9.7 (code `4`), §10 | AC-01, AC-02, AC-03, AC-05 (LLM), AC-06, AC-07 |
| M3 | Exact replay: LLM channel | Canonicalization and fingerprints, per-fingerprint FIFO queues, SSE re-emission, `--realtime`, `retrace run` (exact), unconsumed-interaction warning, `provenance` | §4.1, §4.4, §5 (LLM), §9.3 (exact) | AC-08 (LLM), AC-09 (LLM), AC-10 (LLM), AC-12 |
| | **First usable slice:** record and replay offline an agent that only uses the LLM | | | |
| M4 | MCP: record and replay | Streamable HTTP proxy, JSON and SSE responses, batches, JSON-RPC id and `Mcp-Session-Id` rewriting, MCP event derivation, `tool_call` ↔ `tool_use` linking | §3.4, §5 (MCP), §7.4 (MCP) | AC-04, AC-05 (MCP), AC-08, AC-09 (MCP), AC-10 (MCP), AC-11, AC-13 |
| M5 | Swap-model, strict and live-tools | `model` rewrite, `DivergenceStrategy` interface, `strict` (divergence event, error responses, agent termination), reserved strategies, flag validation, live-tools | §4.2, §4.3, §6, §9.3 (modes), §9.7 (code `3`) | AC-14, AC-15, AC-16, AC-17, AC-18 |
| M6 | Diff and inspection | `retrace ls`, `retrace show`, `retrace diff` (text and JSON), JSON schema for diff output | §8, §9.4–9.6 | AC-19, AC-20, AC-21, AC-22 |
| M7 | Demo and release | Demo agent with two MCP servers, README GIF, GoReleaser, `v0.1.0` | §10 (README) | AC-23 |

Each milestone is tracked as a GitHub issue in the **MVP** milestone, with its spec sections and acceptance criteria as a checklist. A milestone usually takes several small PRs. Each PR references the sections and ACs it covers.

## Design points to settle inside a milestone

These are not open product questions: the spec and ADRs constrain them. They are implementation choices that are easy to get wrong, so each one is named here and must be decided explicitly in the PR that introduces it.

- **Writer queue (M1, used from M2).** Recording must never block forwarding (ADR 0009, AC-03), and memory must stay bounded on long SSE streams. Use a bounded queue with batched transactions. When the queue is full, retrace marks the affected interaction as `error` (and logs it) instead of stalling the stream. On `SIGINT`/`SIGTERM` the queue is drained before the cassette is finalized (AC-07). Tests: kill retrace with writes still queued, and the cassette is valid and `interrupted`. The M1 benchmark measures queue depth under a long synthetic SSE stream, not only throughput. Decided in M1 (#27): the queue is bounded by records and bytes; once a record of an interaction is dropped, its later records are dropped too and the interaction is completed with an `error` on close; a dropped event is not marked on its interaction (which may already be complete), and events whose parent or interaction row is missing are dropped so foreign keys hold.
- **Body redaction before fingerprints (M2 → M3).** Body redaction rules are applied before canonicalization (spec §5). They land in M2, so fingerprints never change for cassettes that were already recorded with redaction rules.
- **Injectable clock (M3).** `--realtime` replay (AC-10) takes its clock and sleeper as dependencies. Tests assert the scheduled offsets deterministically, with at most one loose wall-clock sanity check.

## Decisions taken for the plan

These do not need an ADR: no new dependency for the retrace binary, no contract change. They are recorded here so they are not re-discussed.

- **CLI:** standard library only (`flag` plus a small subcommand dispatcher). The command set is small, and `--` passthrough for the agent command is straightforward. A CLI framework such as `cobra` would be a new dependency and would need an ADR.
- **Go version:** `go.mod` targets the oldest supported Go release (1.26 at the time of writing). CI tests against both supported releases (1.26 and 1.27).
- **Demo agent (M7):** Python, using the official Anthropic SDK, with two MCP servers over Streamable HTTP, also in Python. This shows that retrace works with agents in other languages, and it matches how most agents are written.
  - The demo lives in `examples/demo/` and is not part of the Go module.
  - Its dependencies are pinned in a lock file, follow the 14-day age rule (CONTRIBUTING.md, *Dependencies*), and get a Python Dependabot entry with the same `cooldown`.
- **Testing against real APIs:** never in CI. Tests use in-process fake upstreams (CLAUDE.md, *Testing*). Before `v0.1.0`, a manual smoke test runs against the real Anthropic API.

## Working rules

- **Spec first.** If implementation reveals behavior that the spec does not cover, or gets wrong, a spec PR comes first (version 0.1 → 0.2, see CONTRIBUTING.md). The same PR updates the spec version in this plan's header.
- **New dependencies** for the retrace binary, beyond those named in the ADRs (`modernc.org/sqlite`), need an ADR.
- **Definition of done for a milestone:**
  - Its spec sections are implemented.
  - Its ACs are covered by tests that reference them.
  - Its design points are decided and tested.
  - CI is green, and the Claude review findings are resolved.
  - The milestone issue checklist is complete.
  - README and docs reflect the new state.

## Risks

| Risk | Mitigation |
|---|---|
| SSE tee adds latency or buffering | AC-03 test asserts the first chunk reaches the client before the upstream finishes. Implemented in M2, before anything builds on it. |
| Writer queue grows without bound or loses data on shutdown | Bounded queue, explicit full-queue behavior, drain on signals (see *Design points*). Implemented in M1. |
| Canonicalization edge cases cause false misses | Golden tests per provider and transport. The canonical form is stored with each interaction for inspection (ADR 0007). |
| Timing assertions are flaky on CI runners under `-race` | Injectable clock for `--realtime` (see *Design points*). |
| Pure-Go SQLite is too slow on the hot path | Writes are asynchronous to forwarding. Benchmark in M1 with a synthetic long session. |
| MCP Streamable HTTP details (batches, SSE responses, sessions) differ across servers | Fake MCP server in tests, plus the two demo servers in M7 as a real-world check. |
| Scope creep from spec §12 items | CLAUDE.md scope gate. Anything outside the MVP becomes an issue without a milestone. |
