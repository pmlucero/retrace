# 0008. Secret redaction in cassettes

- Status: Proposed
- Date: 2026-10-23

## Context

Cassettes are designed to be shared: attached to issues, committed as fixtures, used in the README demo. Recorded traffic contains credentials (API keys, bearer tokens for MCP servers) and potentially sensitive content (prompts, tool outputs). A leaked key in a public cassette is a realistic failure mode for an open-source project.

## Decision

1. **Credentials are never written to disk.** The proxy forwards auth headers upstream but replaces their values before storing them. The default deny-list (case-insensitive) is `authorization`, `x-api-key`, `proxy-authorization`, `cookie`, `set-cookie`, plus any header matching `*-token`, `*-secret` or `*-key`.
   - The value is replaced with `[REDACTED:<sha256-prefix-8>]`. The hash prefix allows detecting that two sessions used the same key without exposing it.
2. **Replay never needs stored credentials.** In `exact` mode no upstream is contacted. In partial modes the real upstream receives the credentials the agent sends at runtime.
3. **Body redaction is opt-in** with `--redact <jsonpath>` (repeatable), for example to scrub a field from tool results. Redacted body values are replaced before fingerprinting. During replay the same rules are applied to incoming requests before they are fingerprinted, so record and replay stay consistent. The list of active redaction rules is stored in the cassette metadata.
4. The cassette metadata records the retrace version and the redaction rules applied, so a reader knows what was scrubbed.
5. Documentation states clearly that **cassettes contain prompts and tool outputs verbatim** unless body redaction is configured, and must be reviewed before being shared.

## Alternatives considered

### Encrypt cassettes at rest

It protects storage, but defeats sharing and inspection. It also does not solve the main risk, which is accidental publication of a key.

### Redact automatically with secret-scanning heuristics over bodies (regexes for key formats)

It is attractive, but causes both false positives (corrupting replay) and false negatives (false confidence). It may be added later as a `retrace scan` command that warns rather than mutates.

## Consequences

### Positive

- A cassette produced with defaults never contains auth headers.
- Body redaction is explicit and auditable.

### Negative

- Body content is stored verbatim by default. Users are responsible for reviewing it before sharing.

### Risks and mitigations

- *A custom auth header outside the deny-list leaks.* Mitigations: the pattern-based defaults, `--redact-header <name>`, and a test fixture suite with common MCP auth schemes.
