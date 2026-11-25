# 0016. Redact MCP session ids

- Status: Accepted
- Date: 2026-11-25

## Context

[ADR 0011](0011-secret-redaction-defaults.md) set the header deny-list. Under *Alternatives considered* it rejected a `*-session` pattern because it "would mask `Mcp-Session-Id`, whose value replay needs (spec §5 step 3)". So cassettes store the `Mcp-Session-Id` header of every MCP request and response as it was sent.

The code review of 2026-11-25 (#185) found that replay does not need the value:

- Replay checks only whether a recorded response carried the header (`internal/replay/mcp.go`, `sessionID`), to decide whether the agent gets a session id at all.
- The id the agent receives is always issued fresh by retrace, or echoed from the agent's own request. The recorded value is never served or compared.
- `live-tools` forwards the agent's own header to the real server. It does not read the stored one either.

An MCP session id can work as a bearer credential. A server that does not bind the session to the caller's authentication accepts any request that carries the id while the session is open. A cassette shared with someone else, which ADR 0011 and AC-06 aim to make safe, gives them that id.

Only the reason for leaving the header out was wrong. ADR 0011's decisions stand, and ADRs are immutable once Accepted (ADR 0000). This ADR therefore extends ADR 0011 rather than superseding it, as ADR 0014 does.

## Decision

1. **`mcp-session-id` joins the default deny-list names** of ADR 0011 decision 1. Stored copies of the header, in requests and responses, become `[REDACTED:<sha256-prefix-8>]` in record mode and in every replay mode. Forwarded requests and responses keep the original value.
2. **Replay keeps its behavior.** A masked value is non-empty, so replay still knows that the recorded response carried a session id (spec §5 step 3), and AC-11 is unchanged.
3. **The exact name is added, not a `*-session` pattern.** The pattern would also mask unrelated headers. `Mcp-Session-Id` is the only session header that the transports in scope (ADR 0004) define.
4. **AC-06 covers it:** the scan of the raw cassette bytes includes an MCP `Mcp-Session-Id` header, in a recording and in a replay that forwards requests.

## Consequences

- Sharing a cassette no longer exposes a live MCP session.
- Because one deny-list covers headers and query parameters (ADR 0011), a query parameter named `mcp-session-id` or `mcp_session_id` in a stored URL is masked too. A stored MCP server URL that holds such a marker then needs `--mcp` to be replayed in `live-tools` (spec §4.3), as it would for any other masked parameter. No known server puts the session id in its URL.
- Cassettes recorded before this change keep their raw session ids. `reindex` never changes interactions (spec §7.6), so it cannot fix them. Such cassettes should be re-recorded, or reviewed, before they are shared. Their stored `redaction` metadata lists the older defaults (ADR 0011 decision 6). A replay of one applies the current defaults, so the replay's own cassette is redacted.
- A user who needs the raw value in a cassette for debugging can no longer get it. No retrace feature uses it.

## Alternatives considered

### Keep storing the value and document the risk

This needs no change, but it contradicts the goal of ADR 0011: the common cases should be safe without configuration. Users would learn about the risk only after a cassette had been shared.

### Drop the header from stored copies

Replay needs to know that the header was present, so dropping it would change replay behavior, or need a separate flag in the cassette. Masking keeps presence with no format change.
