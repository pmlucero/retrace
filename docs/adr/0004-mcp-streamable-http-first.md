# 0004. MCP over Streamable HTTP as the first transport

- Status: Accepted
- Date: 2026-10-23

## Context

MCP defines two current transports: **stdio** (the client spawns the server as a subprocess) and **Streamable HTTP** (a single HTTP endpoint that accepts JSON-RPC over `POST`, and can answer with JSON or an SSE stream). A proxy based on base URLs ([ADR 0002](0002-transparent-proxy-base-url.md)) applies naturally to HTTP. stdio needs a different interception mechanism.

## Decision

The MVP supports **MCP over Streamable HTTP** only.

Recording:

- `POST` requests carry JSON-RPC requests, notifications or responses. Both response forms are recorded: `application/json` and `text/event-stream`, each SSE event with its offset.
- Headers relevant to the protocol are recorded: `Mcp-Session-Id` and `MCP-Protocol-Version`.
- `GET` (a server-initiated SSE stream) is proxied and its messages are recorded, but replaying it is out of the MVP (see below).
- `DELETE` (session termination) is proxied and recorded.

Replay (with frozen tools):

- retrace answers as the server. On `initialize` it returns the recorded result and issues **its own** `Mcp-Session-Id`, since the recorded session id is meaningless now.
- JSON-RPC `id`s in replayed responses are **rewritten** to match the ids of the incoming requests, because the client generates them and they may differ between runs.
- Notifications from the client (e.g. `notifications/initialized`) get `202 Accepted`.
- `GET` returns `405 Method Not Allowed`. The specification allows servers not to offer this stream.
- `tools/list`, `resources/list`, `prompts/list` and other non-tool methods are frozen in the same way as `tools/call`.

## Alternatives considered

### stdio first

Most local MCP servers today use stdio. Intercepting it requires a wrapper binary (`retrace mcp-wrap -- <server cmd>`) that the agent config launches instead of the real server, so it is a different mechanism from the proxy. It is planned as the next transport, after the MVP.

### Legacy HTTP+SSE transport (pre-2025)

It is deprecated in the MCP specification and is not worth supporting first.

## Consequences

### Positive

- MCP and the LLM API share one interception mechanism and one proxy.
- The demo can use two small HTTP MCP servers that are easy to run in CI.

### Negative

- Agents that only use stdio MCP servers cannot use the MVP, unless the servers are exposed through an stdio-to-HTTP bridge.
- Server-initiated messages (`GET` stream, and server-to-client requests such as sampling or elicitation) are recorded but not replayed in the MVP.

### Risks and mitigations

- *Protocol version drift.* The negotiated `protocolVersion` is stored per cassette, and messages are stored verbatim. Semantic parsing only depends on `tools/call` and `initialize`.
