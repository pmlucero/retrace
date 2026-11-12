# 0011. Secret redaction defaults

- Status: Accepted
- Date: 2026-11-12

## Context

[ADR 0008](0008-secret-redaction.md) decided that credentials never reach a cassette: header values on a deny-list are replaced before anything is written, body redaction is opt-in, and the rules are stored in the cassette. The principle holds. The security review before `v0.1.0` (#100) found gaps in how it is applied:

- **The header deny-list is too narrow** (#127). It misses headers that real services use for credentials: `apikey` (Kong, Supabase), `token`, `x-auth`, `*-auth`, and signature headers such as `x-amz-signature`. The query parameter rule of spec §3.5 already redacts `apikey`, `token`, `auth` and `signature` in URLs, so the same credential was redacted in a query and stored in plain text in a header.
- **Signed URLs leak through their query** (#127). `X-Amz-Signature` and `X-Amz-Credential` (S3 presigned URLs), and the `X-Goog-*` equivalents, match neither the query names nor the suffixes.
- **URLs in header values are not redacted** (#128). Redaction works on header names. A `Location` header that carries an OAuth redirect with a `code` or a token in its query, or a signed URL, reaches disk as it is.
- **A replay cannot add a header** (#127). `record` has `--redact-header`, `run` does not. A replay in `swap-model` or `live-tools` forwards requests and records them, so it can meet a credential header the recording never saw.
- **AC-06 tests too little.** Its file scan covers the LLM `x-api-key` and the MCP URL, not MCP `Authorization`, a `*-token` header, a query token, `Set-Cookie`, or the cassette of a replay.

ADR 0008 is Accepted and ADRs are immutable (ADR 0000), so this ADR restates its decision with the changes and supersedes it.

## Decision

1. **Credentials are never written to disk.** The proxy forwards credentials upstream unchanged and replaces them in what it stores. One deny-list, compared case-insensitively, applies to header names and to query parameter names (with `_` read as `-`, spec §3.5):
   - Names: `authorization`, `proxy-authorization`, `x-api-key`, `cookie`, `set-cookie`, `apikey`, `auth`, `key`, `password`, `secret`, `sig`, `signature`, `token`.
   - Patterns: `*-token`, `*-secret`, `*-key`, `*-auth`, `*-password`, `*-signature`, `*-credential`, and `*apikey` (with or without a dash before it, e.g. `x-apikey`).
   - Names added with `--redact-header` join the list, for headers and query parameters alike.
   - A redacted value becomes `[REDACTED:<sha256-prefix-8>]`. The hash prefix lets a reader see that two sessions used the same key without exposing it.
   - The query names of spec §3.5 (`key`, `token`, `secret`, `password`, `apikey`, `auth`, `sig`, `signature`) are now on the shared list, so headers and URLs follow one rule. `X-Amz-Signature`, `X-Amz-Credential`, `X-Goog-Signature` and `X-Goog-Credential` are covered by the patterns, and `X-Amz-Security-Token` already was.
2. **URLs in header values are redacted like stored URLs.** The values of `Location`, `Content-Location`, `Referer` and `Link` (each URI reference inside `<…>`) get the URL rule of spec §3.5: userinfo becomes `REDACTED`, and query parameters on the deny-list are masked. This applies to request and response headers, in record and in every replay mode. A header that is itself on the deny-list is masked whole.
3. **`run` takes `--redact-header`**, repeatable, as `record` does. The names are added to the source session's rules, and the new cassette stores the combined rules. Headers are not part of a fingerprint (ADR 0007), so this does not change matching. `run` still takes no `--redact`: body rules change fingerprints, so a replay must use the recording's.
4. **Replay never needs stored credentials.** In `exact` mode no upstream is contacted. In `swap-model` and `live-tools`, the real upstream receives the credentials the agent sends at runtime.
5. **Body redaction is opt-in** with `--redact <jsonpath>` (repeatable). It is applied before fingerprinting, during record and replay alike, as specified in spec §3.5 and §5.
6. **The cassette says what was scrubbed.** Its metadata records the retrace version and the redaction rules applied. A cassette written by an older binary lists the older defaults. A replay of it applies the current defaults plus its stored names, and stores those.
7. **Documentation states that cassettes contain prompts and tool outputs verbatim** unless body redaction is configured, and that they must be reviewed before being shared.
8. **AC-06 covers every path to disk.** Its scan of the raw cassette bytes includes the LLM `x-api-key`, an MCP `Authorization` header, a `*-token` header, a token in a query string, a credential in a `Location` URL and a `Set-Cookie` response header, both in a recorded cassette and in the cassette of a replay that forwards requests.

## Alternatives considered

### Keep ADR 0008's list and rely on `--redact-header`

It needs no change, but a user learns that a header leaked only after the cassette was shared. The defaults exist so that the common cases are safe without configuration.

### Redact every header except an allow-list

It is the safest option for credentials, but stored headers are part of what a cassette is for: `show --raw` displays them, and a replay serves recorded response headers back to the agent (spec §4.4). Masking `content-type`, `anthropic-version` or rate-limit headers would make cassettes less useful and replays less faithful. A deny-list with broad patterns covers the realistic cases.

### Add `*-credentials` and `*-session` patterns

`*-credentials` would also mask `Access-Control-Allow-Credentials`, a CORS response header whose value is `true`. `*-session` would mask `Mcp-Session-Id`, whose value replay needs (spec §5 step 3). Both are left out. The singular `*-credential` covers signed URLs.

### Scan header values for anything that looks like a URL

It would catch URLs in custom headers, but parsing every value as a URL is a heuristic with false positives, the same objection ADR 0008 makes to secret-scanning bodies. The listed headers are the standard ones whose values are URLs.

## Consequences

### Positive

- Common credential headers and signed URLs are redacted with no configuration.
- Headers and query parameters follow one rule, so a credential is redacted wherever it travels.
- A replay can add a header to redact, and its cassette is covered by AC-06.

### Negative

- The broader patterns can mask harmless values, for example a `*-signature` header that only carries a content checksum. The value is lost from the cassette, but only from what is stored: the agent and the upstream receive it unchanged, and headers do not affect matching.
- A credential in a custom header that matches no pattern still needs `--redact-header`.
- A replay serves recorded response headers back to the agent (spec §4.4), so it now serves a `Location`, `Content-Location` or `Link` URL with redaction markers where the agent saw the real URL while recording. An agent that follows such a URL in replay sends a request that differs from the recorded one, which is a cassette miss, so the difference shows up as a divergence rather than silently. This is the same trade-off ADR 0008 made for `Set-Cookie`: a credential is never served from a cassette.

### Risks and mitigations

- *A future header carries credentials under a new name.* Mitigations: the patterns, `--redact-header`, the AC-06 scan, and SECURITY.md, which lists redaction bypasses as in scope.
- *A replay of an older cassette stores more redacted values than its source.* That is the safe direction: a replay never stores a value its source redacted.
