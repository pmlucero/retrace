# 0000. Record architecture decisions

- Status: Accepted
- Date: 2026-10-23

## Context

retrace is developed with Spec-Driven Development: vision, spec and decisions come before code. It is also a public portfolio project, so readers need to understand why the design is the way it is, not just what it is. Decisions taken in chats or in commit messages get lost.

## Decision

Every significant architectural decision is recorded as an Architecture Decision Record (ADR) in `docs/adr/`, using the format described by Michael Nygard and the template in [`README.md`](README.md):

- One decision per file, named `NNNN-kebab-case-title.md`, numbered sequentially.
- Sections: Context, Decision, Alternatives considered, Consequences.
- ADRs are immutable once Accepted. To change a decision, write a new ADR that supersedes the old one and update the old one's status line only.
- A decision is "significant" if it affects the cassette format, the CLI contract, the external protocols supported, or adds a runtime dependency.

## Alternatives considered

### Design notes in a single document

Simpler, but a single document loses the history of why a decision changed, and it grows into something nobody re-reads.

### Decisions in issues or PR descriptions

They are discoverable only by people who already know where to look, and they are tied to a hosting platform.

## Consequences

### Positive

- The rationale is versioned with the code and reviewable in PRs.
- New contributors, and coding agents working on the repo, can check the ADRs before proposing a change that was already discussed.

### Negative

- Writing ADRs adds a small overhead to each significant change.

### Risks and mitigations

- *ADRs drift from reality.* Mitigation: `CLAUDE.md` requires that code contradicting an Accepted ADR comes with a superseding ADR in the same PR.
