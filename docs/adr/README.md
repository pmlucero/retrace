# Architecture Decision Records

This directory records the significant architectural decisions of retrace, following [ADR 0000](0000-record-architecture-decisions.md).

## Index

| # | Title | Status |
|---|---|---|
| [0000](0000-record-architecture-decisions.md) | Record architecture decisions | Accepted |
| [0001](0001-go-single-binary.md) | Go as the implementation language, shipped as a single binary | Accepted |
| [0002](0002-transparent-proxy-base-url.md) | Transparent proxy enabled by changing the base URL | Accepted |
| [0003](0003-anthropic-messages-api-with-sse.md) | Anthropic Messages API with SSE streaming as the first provider | Accepted |
| [0004](0004-mcp-streamable-http-first.md) | MCP over Streamable HTTP as the first transport | Accepted |
| [0005](0005-sqlite-cassette-storage.md) | One SQLite file per cassette | Accepted |
| [0006](0006-strict-divergence-default.md) | `strict` as the default divergence strategy | Accepted |
| [0007](0007-request-matching-fingerprints.md) | Request matching by canonical fingerprint | Accepted |
| [0008](0008-secret-redaction.md) | Secret redaction in cassettes | Accepted |
| [0009](0009-append-only-event-log.md) | Cassette as an append-only event log | Accepted |
| [0010](0010-record-event-log-gaps.md) | Record gaps in the semantic event log | Proposed |

## Statuses

- **Proposed**: under discussion. The decision is not yet binding.
- **Accepted**: binding. Code must follow it.
- **Superseded by NNNN**: replaced by a later ADR. Kept for history and never deleted.
- **Rejected**: considered and discarded. Kept so the discussion is not repeated.

## Template

```markdown
# NNNN. Title in imperative or noun form

- Status: Proposed
- Date: YYYY-MM-DD

## Context

What forces are at play: requirements, constraints, unknowns.

## Decision

What we will do, stated plainly.

## Alternatives considered

### Alternative A
Pros / cons, and why it was not chosen.

## Consequences

### Positive
### Negative
### Risks and mitigations
```
