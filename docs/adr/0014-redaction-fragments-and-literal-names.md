# 0014. Redaction of URL fragments and literal `--redact-header` names

- Status: Accepted
- Date: 2026-11-16

## Context

[ADR 0011](0011-secret-redaction-defaults.md) decided where credentials are redacted. Its decision 2 applies the URL rule of spec §3.5 to stored URLs and to URLs in header values: userinfo becomes `REDACTED`, and query parameters on the deny-list are masked. Its decision 1 lets `--redact-header` add names to the deny-list.

Spec v0.27 (#152), from the security review before `v0.1.0` (#100), went further in two places:

- **Fragments carry tokens.** The OAuth implicit flow returns its token in the fragment of a redirect (`Location: https://app.example/cb#access_token=…&state=…`), and a single-page app can put a query-like route there (`#/cb?access_token=…`). The query rule does not see a fragment, so such a token reached disk.
- **A `*` name on the command line was ambiguous.** The stored redaction rules mark patterns with a leading `*`, so a literal name `*-x` added with `--redact-header` could not be told apart from a pattern, and a user who wrote `*-x` expecting a pattern silently got none.

Neither change contradicts ADR 0011, but a reader of ADR 0011 alone would miss both. ADRs are immutable once Accepted (ADR 0000), so this ADR records them. It extends ADR 0011 and does not supersede it.

## Decision

1. **The fragment of a URL gets the query rule, element by element.** Wherever ADR 0011 redacts a URL, the fragment is split on `&` and on `?`, and in each `name=value` element whose name is on the deny-list the value is masked. The rest of the fragment is kept as it was. The exact rule, including its edge cases, is in spec §3.5, and AC-06 covers it.
2. **`--redact-header` takes literal names only.** A name that starts with `*` is a usage error (exit `2`) that says patterns are not accepted on the command line (spec §9.2). The default patterns of ADR 0011 are unchanged.

## Alternatives considered

### Redact the whole fragment

It is simpler and catches any token, but a fragment often holds routing state that a reader of `show --raw` needs, and the query rule already defines what a credential name is. Masking only deny-listed elements keeps the two rules the same.

### Accept `*` patterns from the command line

It would let users add patterns, but it needs a way to tell a stored literal from a stored pattern, which is a cassette format change. No user has asked for it. The default patterns cover the common cases, and a literal name covers a specific header.

### Leave it to the spec

The spec already states both rules, so this ADR adds no behavior. But the ADRs are where a reader looks for why redaction works as it does, and ADR 0011 alone would give an incomplete answer.

## Consequences

### Positive

- OAuth implicit-flow tokens and similar fragment credentials are redacted with no configuration.
- A `--redact-header` name means exactly what it says.

### Negative

- Redaction is now described in two ADRs. A reader needs both, and this one points back to ADR 0011.

### Risks and mitigations

- *A fragment element without `=`, or a token after a raw `?` inside a value, is not masked.* Spec §3.5 documents both. A user can add the name with `--redact-header`, which also applies to query and fragment parameters.
