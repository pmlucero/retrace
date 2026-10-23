# 0005. One SQLite file per cassette

- Status: Proposed
- Date: 2026-10-23

## Context

A cassette must:

- Store an ordered stream of events with rich metadata, plus raw request and response payloads and SSE chunks with timing.
- Be queryable, for `ls`, `show` and `diff` now and a web UI later.
- Be **portable**: one artifact that can be attached to a bug report or committed as a test fixture.
- Be written incrementally while the agent runs, so that a crash does not lose the session.

## Decision

Each cassette is **one SQLite database file**, stored by default at `.retrace/cassettes/<session-id>.cassette` (the content is SQLite, and the extension makes intent clear).

- Driver: `modernc.org/sqlite` (pure Go, compatible with `CGO_ENABLED=0`, see [ADR 0001](0001-go-single-binary.md)).
- Journal mode is WAL while recording. On session close retrace checkpoints and switches back to `DELETE` mode, so the cassette is a single self-contained file again.
- The schema version is stored in `PRAGMA user_version`. Migrations are forward-only and embedded in the binary.
- Large payloads are stored once in a content-addressed `blobs` table (SHA-256), which deduplicates the repeated system prompts and tool definitions that are sent on every LLM call.
- The schema itself is defined in the [core spec §7](../spec/core.md#7-cassette-data-model).

The cassette directory is configurable with `--dir` and `RETRACE_DIR`.

## Alternatives considered

### JSON or YAML files (one per cassette, like VCR.py)
They are human-readable and diffable in git. However, they cannot be written incrementally without rewriting the file, they are poor at holding thousands of SSE chunks, they cannot be queried, and they grow large with repeated prompts.

### JSONL append-only log
It is easy to append to and to stream. Queries for the UI and for diff would need a full scan and an in-memory index every time, and there is no deduplication. It remains a good **export** format (see Consequences).

### One shared SQLite database for all sessions
It makes cross-session queries easy, but loses portability: sharing one session means extracting it.

### Embedded KV store (bbolt, Badger)
It is not queryable with standard tools, and it is less familiar to contributors and users.

## Consequences

### Positive
- Cassettes are single files that can be opened with any SQLite tool for ad-hoc inspection.
- Incremental, crash-safe writes.
- The future web UI can query cassettes directly.

### Negative
- Cassettes are binary and do not produce meaningful git diffs. Mitigation: `retrace show <id> --json` (and later `retrace export`) produce a stable JSONL representation.
- Cross-session queries (`ls`) must open each file. This is acceptable at the expected scale of tens to hundreds of cassettes.

### Risks and mitigations
- *Cassettes contain sensitive data (prompts and tool outputs).* See [ADR 0008](0008-secret-redaction.md). Documentation must warn before cassettes are shared.
