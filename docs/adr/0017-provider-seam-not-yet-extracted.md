# 0017. The Provider seam is planned, not yet extracted

- Status: Accepted
- Date: 2026-11-27

## Context

[ADR 0003](0003-anthropic-messages-api-with-sse.md) says the provider is "behind an internal `Provider` interface (parse request, parse response or stream, extract usage, fingerprint)". The code has no such type. The Anthropic provider is called directly from several packages.

CLAUDE.md says not to create an interface until there are two implementations or a test needs a seam. With one provider, that rule applies. The docs implied an interface that does not exist (#203).

Only the Provider paragraph of ADR 0003 is wrong. Its other decisions stand. ADRs are immutable once Accepted (ADR 0000). This ADR therefore replaces that paragraph only and leaves ADR 0003 unchanged, as ADR 0014 and ADR 0016 do for their parents. The ADR status values have no partial form, so ADR 0003 stays `Accepted`.

## Decision

1. **`Provider` is a planned seam, not yet extracted.** The Anthropic provider (`internal/provider/anthropic`) is wired directly, and no interface exists.
2. **Extract it when a second provider arrives.** That needs an approved spec change first (spec §12). Then define a small struct or interface at the consumer, as CLAUDE.md requires.
3. **These places name the Anthropic provider (as of 2026-11-27)** and are the ones to change at that point:
   - `internal/cli` (`record.go`, `reindex.go`, `checkbudget.go`): the `/llm/anthropic` route with upstream `anthropic`, `anthropic.PricingVersion` and `anthropic.Cost`.
   - `internal/replay/session` (`replay.go`, `handler.go`): `anthropic.Canonicalize`, `anthropic.ModelRewriter`, `anthropic.PricingVersion` and the `/llm/anthropic/` mux route.
   - `internal/replay` (`llm.go`): the `llmUpstream = "anthropic"` constant.
   - `internal/derive` (`derive.go`, `reindex.go`): `isMessages`, which checks `POST /v1/messages`, plus `anthropic.ParseRequest`, `anthropic.ParseResponse`, `anthropic.Stream`, `anthropic.Message`, `anthropic.Cost` and `anthropic.PricingVersion`.
   - `internal/proxy` (`reject.go`): the `/llm/anthropic` prefix in the path check.
   - `internal/agentrun` (`agent.go`): `ANTHROPIC_BASE_URL` set to `<base>/llm/anthropic`.

## Consequences

- The docs now match the code. Nobody looks for an interface that is not there.
- Adding a second provider means touching the places above. The list is the starting point, not a promise of its size.
- Extracting the seam later is a refactor of known size, not a surprise. Doing it early would guess the shape from one implementation.

## Alternatives considered

### Extract the interface now

It would match ADR 0003 as written. But with one implementation the interface would only be a guess, and it breaks the CLAUDE.md rule on interfaces.

### Edit ADR 0003 in place

ADRs are immutable once Accepted (ADR 0000). A superseding ADR keeps the history.
