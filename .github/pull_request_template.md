## Summary

<!-- What does this PR change, and why? Link the issue if there is one: "Closes #123". -->

## Type

<!-- Keep the ones that apply. -->

- Spec change (`docs/spec/`)
- ADR (`docs/adr/`)
- Implementation
- Tests only
- Docs / chore

## Traceability

<!-- Spec-Driven Development: implementation follows an approved spec. -->

- Spec sections: <!-- e.g. core.md §5 -->
- Acceptance criteria: <!-- e.g. AC-12, AC-13 -->
- ADRs: <!-- e.g. ADR 0007 -->

## Checklist

- [ ] Behavior is described in the spec, or this PR updates the spec in a separate commit
- [ ] Significant decisions (cassette format, CLI contract, protocols, dependencies) have an ADR
- [ ] Tests reference the acceptance criteria they verify (`// AC-xx`)
- [ ] `go test -race ./...`, `go vet ./...` and `golangci-lint run` pass
- [ ] Cassette schema changes include a migration and a `PRAGMA user_version` bump
- [ ] No real cassettes, `.retrace/` directories or secrets are committed
- [ ] Commits are small and follow Conventional Commits

## Notes for the reviewer

<!-- Trade-offs, open questions, what to look at first. -->
