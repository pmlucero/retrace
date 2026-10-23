# MVP implementation plan

- Status: Approved
- Last updated: 2026-10-23
- Spec: [core.md](../spec/core.md) v0.1

This plan orders the implementation of the MVP defined in the [core spec](../spec/core.md). It says *in what order* things are built and *how* progress is tracked. *What* is built is defined by the spec and the [ADRs](../adr/README.md). If they disagree, the spec wins, and this plan is updated.

## Strategy: vertical slice first

Instead of building layer by layer, the plan reaches a working end-to-end slice as early as possible: **record and replay an agent that only talks to the LLM** (M3). The riskiest parts surface early, while they are still cheap to change:

- Forwarding SSE streams without buffering while recording them (AC-03).
- Canonicalization and fingerprint matching (ADR 0007).
- Byte-faithful replay (AC-08, AC-10).

MCP, swap-model, divergence handling and diff are then added on a proven base.

## Milestones

| # | Milestone | Scope | Acceptance criteria |
|---|---|---|---|
| M0 | Go skeleton and CI | `go.mod`, `cmd/retrace` with `--version`, `internal/` layout, subcommand dispatch, golangci-lint, Go CI jobs as required checks, Dependabot for `gomod` | — |
| M1 | Cassette store | SQLite schema (spec §7), embedded forward-only migrations, content-addressed blobs, append-only writer, reader | Foundation for AC-05, AC-07 |
| M2 | Record: LLM channel | Proxy and router, child process with injected environment, Anthropic Messages API (JSON and SSE), header redaction, signal handling, LLM event derivation with tokens and estimated cost | AC-01, AC-02, AC-03, AC-05 (LLM), AC-06, AC-07 |
| M3 | Exact replay: LLM channel | Canonicalization and fingerprints, per-fingerprint FIFO queues, SSE re-emission, `--realtime`, `retrace run`, no-network guarantee | AC-08 (LLM), AC-09, AC-10, AC-12 |
| | **First usable slice:** record and replay offline an agent that only uses the LLM | | |
| M4 | MCP: record and replay | Streamable HTTP proxy, JSON and SSE responses, batches, JSON-RPC id and `Mcp-Session-Id` rewriting, MCP event derivation, `tool_call` ↔ `tool_use` linking | AC-04, AC-05 (MCP), AC-08, AC-11, AC-13 |
| M5 | Swap-model, strict and live-tools | `model` rewrite, `DivergenceStrategy` interface, `strict` (divergence event, error responses, agent termination), reserved strategies, flag validation, live-tools | AC-14, AC-15, AC-16, AC-17, AC-18 |
| M6 | Diff and inspection | `retrace ls`, `retrace show`, `retrace diff` (text and JSON), JSON schema for diff output | AC-19, AC-20, AC-21, AC-22 |
| M7 | Demo and release | Demo agent with two MCP servers, README GIF, GoReleaser, `v0.1.0` | AC-23 |

Each milestone is tracked as a GitHub issue in the **MVP** milestone, with its acceptance criteria as a checklist. A milestone usually takes several small PRs. Each PR references the ACs it covers.

## Decisions taken for the plan

These do not need an ADR: no new dependency, no contract change. They are recorded here so they are not re-discussed.

- **CLI:** standard library only (`flag` plus a small subcommand dispatcher). The command set is small, and `--` passthrough for the agent command is straightforward. A CLI framework such as `cobra` would be a new dependency and would need an ADR.
- **Go version:** `go.mod` targets the oldest supported Go release (1.26 at the time of writing). CI tests against both supported releases (1.26 and 1.27).
- **Demo agent (M7):** Python, using the official Anthropic SDK, with two MCP servers over Streamable HTTP, also in Python. This shows that retrace works with agents in other languages, and it matches how most agents are written. The demo lives in `examples/demo/` and is not part of the Go module.
- **Testing against real APIs:** never in CI. Tests use in-process fake upstreams (CLAUDE.md, *Testing*). Before `v0.1.0`, a manual smoke test runs against the real Anthropic API.

## Working rules

- **Spec first.** If implementation reveals behavior that the spec does not cover, or gets wrong, a spec PR comes first (version 0.1 → 0.2, see CONTRIBUTING.md).
- **New dependencies** beyond those named in the ADRs (`modernc.org/sqlite`) need an ADR.
- **Definition of done for a milestone:**
  - All of its ACs are covered by tests that reference them.
  - CI is green, and the Claude review findings are resolved.
  - The milestone issue checklist is complete.
  - README and docs reflect the new state.

## Risks

| Risk | Mitigation |
|---|---|
| SSE tee adds latency or buffering | AC-03 test asserts the first chunk reaches the client before the upstream finishes. Implemented in M2, before anything builds on it. |
| Canonicalization edge cases cause false misses | Golden tests per provider and transport. The canonical form is stored with each interaction for inspection (ADR 0007). |
| Pure-Go SQLite is too slow on the hot path | Writes are asynchronous to forwarding. Benchmark in M1 with a synthetic long session. |
| MCP Streamable HTTP details (batches, SSE responses, sessions) differ across servers | Fake MCP server in tests, plus the two demo servers in M7 as a real-world check. |
| Scope creep from spec §12 items | CLAUDE.md scope gate. Anything outside the MVP becomes an issue without a milestone. |
