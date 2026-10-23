# 0001. Go as the implementation language, shipped as a single binary

- Status: Accepted
- Date: 2026-10-23

## Context

retrace is a local proxy and CLI that developers install next to agents written in any language: Python, TypeScript, Go and others. It has to:

- Handle many concurrent HTTP connections, including long-lived SSE streams, with low and predictable overhead.
- Install trivially: `brew install`, `go install`, or downloading one file. It should not depend on a language runtime that may conflict with the agent's.
- Be easy to read, since it is a public portfolio project.

## Decision

Implement retrace in **Go** and distribute it as a **single statically linked binary** with no runtime dependencies.

- Build with `CGO_ENABLED=0` so cross-compilation is trivial. This implies a pure-Go SQLite driver (see [ADR 0005](0005-sqlite-cassette-storage.md)).
- Keep the dependency set small. The standard library covers HTTP, reverse proxying, JSON and structured logging (`log/slog`).
- Release artifacts are built with GoReleaser for macOS, Linux and Windows on amd64 and arm64.

## Alternatives considered

### Python

The agent ecosystem lives there, and integrating with Python agents would be natural. However, installing it would conflict with agents' virtualenvs, packaging a self-contained binary is awkward, and the concurrency model for a streaming proxy is weaker.

### TypeScript / Node

It has a good HTTP and SSE story and the official MCP SDK. It requires a Node runtime, and a single-binary distribution (`bun build --compile`, `pkg`) is possible but heavier and less mature.

### Rust

It has excellent performance and also produces single binaries. Iteration is slower and the code is harder to read for the intended audience. The performance gain is irrelevant for a local developer tool whose latency is dominated by the LLM.

## Consequences

### Positive

- Trivial installation, and it works with agents in any language.
- `net/http`, `httputil.ReverseProxy` and goroutines map naturally onto a streaming proxy.
- Fast compile and test cycle.

### Negative

- There is no official Anthropic SDK reuse inside retrace. It does not need one, because it handles raw HTTP, but request and response types for the Messages API and MCP must be modeled locally.
- Requiring `CGO_ENABLED=0` rules out the reference C SQLite driver (`mattn/go-sqlite3`).

### Risks and mitigations

- *Pure-Go SQLite is slower than the C version.* Cassettes are small (from hundreds to a few thousand events), so this is acceptable. It will be revisited if benchmarks show otherwise.
