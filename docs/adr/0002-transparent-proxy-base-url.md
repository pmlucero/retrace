# 0002. Transparent proxy enabled by changing the base URL

- Status: Proposed
- Date: 2026-10-23

## Context

To record and replay, retrace must sit between the agent and both the LLM API and the MCP servers. The vision's first principle is zero-intrusion adoption: no SDK and no code changes in the agent.

Most clients already support redirecting traffic:

- The official Anthropic SDKs read the `ANTHROPIC_BASE_URL` environment variable.
- MCP clients over Streamable HTTP connect to a URL taken from configuration.

## Decision

retrace runs as a **local HTTP reverse proxy**. The agent is pointed at it by changing base URLs only.

- One listener (default `127.0.0.1:4790`) with path-based routing:
  - `/llm/anthropic/...` → the Anthropic API (default upstream `https://api.anthropic.com`).
  - `/mcp/<name>/...` → the MCP server registered under `<name>`.
- `retrace record -- <cmd>` and `retrace run -- <cmd>` start the proxy, launch the agent as a child process, and inject `ANTHROPIC_BASE_URL=http://127.0.0.1:4790/llm/anthropic` into its environment. For MCP servers, retrace exports `RETRACE_MCP_<NAME>_URL` per registered server. The agent's MCP configuration must use those URLs, which is the only mandatory configuration change.
- MCP upstreams are declared with `--mcp <name>=<url>` or in a `retrace.yaml` file.
- A `retrace proxy` sub-mode, which runs the proxy without launching a child, is reserved for agents that cannot be launched as a child. It is not part of the MVP.

The proxy is transparent: it forwards requests and responses byte-for-byte, except for the rewrites required by replay (see [ADR 0004](0004-mcp-streamable-http-first.md) and [ADR 0007](0007-request-matching-fingerprints.md)).

## Alternatives considered

### SDK wrapper or monkey-patching
It gives richer context, such as the agent's internal state, but it requires one implementation per language and SDK, and it violates zero-intrusion.

### HTTPS MITM proxy (`HTTPS_PROXY` plus a local CA)
It would capture traffic without changing URLs. However, it requires installing a trusted CA, and many clients pin or ignore proxy settings. It is invasive and fragile, and a poor first experience for an open-source tool.

### OpenTelemetry ingestion
It only observes and cannot control responses, so it cannot replay.

## Consequences

### Positive
- Works with any agent whose LLM and MCP endpoints are configurable, in any language.
- Launching the agent as a child gives retrace a clear session lifecycle: the session ends when the process exits. It also allows terminating the agent on a strict divergence.

### Negative
- Agents with hard-coded endpoints are not supported.
- The agent's MCP configuration must reference retrace URLs, so it is not purely an environment-variable change for MCP.
- The proxy speaks plain HTTP on localhost. TLS is only used upstream.

### Risks and mitigations
- *Port conflicts.* `--listen` overrides the address, and port `0` picks a free port, which is propagated through the injected environment variables.
