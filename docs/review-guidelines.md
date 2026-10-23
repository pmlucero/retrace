# Review guidelines

How pull requests to retrace are reviewed, by humans and by the automated reviewer (`.github/workflows/claude-review.yml`). The goal is the review an experienced architect would give: it explains *why* something matters, it is grounded in the project's own documents, and it focuses on what will hurt later.

## Principles

1. **Grounded, not generic.** Every finding cites its basis: a spec section, an ADR, a rule in `CLAUDE.md`, a line in the diff, or a concrete failure scenario. If no basis can be stated, the finding is not reported.
2. **Proportional.** Review depth follows the change. A docs-only PR is not reviewed for concurrency. Dimensions that do not apply are marked `N/A` in one line.
3. **Signal over volume.** Important issues come first. Nits are capped (at most 5) and never drown out real problems. Praise is allowed when it is specific.
4. **Actionable.** Each finding says what is wrong, why it matters here, and what to do instead, preferably with a concrete suggestion.
5. **Advisory.** The automated review never blocks a merge. The maintainer is the approver.

## Severity

| Level | Meaning | Examples |
|---|---|---|
| 🔴 **Blocker** | Must be fixed before merging | Contradicts an Accepted ADR; breaks replay determinism; leaks a credential into a cassette; data loss; a race on the proxy hot path |
| 🟠 **Major** | Should be fixed before merging, or tracked explicitly | Missing error handling on an upstream failure; unbounded growth; a test that does not verify what it claims; behavior not in the spec |
| 🟡 **Minor** | Worth fixing, not urgent | An unclear name; a function doing two things; a missing edge-case test |
| ⚪ **Nit** | Optional polish | Wording, small style points that linters do not catch |

## Dimensions

### 1. Architecture and product

- Does the change serve the [vision](vision.md), and stay out of the [non-goals](vision.md#4-non-goals)?
- Is the behavior described in the [spec](spec/core.md)? Behavior that is not in the spec is a finding (CLAUDE.md, *Workflow*).
- Is it consistent with the Accepted [ADRs](adr/README.md)? If it contradicts one, is there a superseding ADR?
- Is the *why* of the change clear from the PR, the commits and the code?
- Is it globally consistent: same concepts, same names and same patterns as the rest of the codebase?

### 2. Design: coupling and dependencies

- Do package dependencies point in the right direction (e.g. `cassette` must not import `proxy`)? Are there cycles, or a "god package" forming?
- Are interfaces defined at the consumer, kept small, and justified (CLAUDE.md, *Go conventions*)?
- Is every new module dependency justified? Does it need an ADR (ADR 0000)? Is it compatible with `CGO_ENABLED=0` (ADR 0001)?
- **Overengineering:** abstractions without a second implementation, configuration nobody asked for, generic frameworks for a single use. Simpler is preferred unless the spec requires otherwise.

### 3. Scalability, performance and time bombs

- **Unbounded growth:** in-memory buffers, maps, slices or goroutines that grow with session length or payload size. Large cassettes and long SSE streams are expected.
- **Time bombs:** code that works today and fails later. Examples: hard-coded limits, dates or model ids; assumptions about payload size or ordering; schema choices that block the future UI (ADR 0009); missing migrations (spec §7.6).
- **Data:** SQLite access patterns (N+1 queries, missing indexes, long write transactions on the hot path), content-addressed blobs used correctly (ADR 0005).
- **Hot path:** the proxy must not add buffering to forwarded streams (AC-03), and must not block forwarding on cassette writes.
- **Future technical debt:** what this change makes harder later. Is the debt deliberate and documented?

### 4. Resilience and error handling

- **Upstream failures:** connection refused, timeouts, 4xx/5xx, `overloaded_error`, streams cut mid-way, malformed SSE or JSON-RPC.
- **Edge cases:** empty bodies, batches, duplicate requests, concurrent identical requests, unknown event or content-block types (they must be preserved, ADR 0003), very large payloads.
- **Partial states:** what remains on disk if retrace or the agent dies mid-session? Is the cassette still valid (AC-07)? Are interactions completed exactly once (ADR 0009)?
- **System and user-facing states:** are the exit codes correct (spec §9.7)? Are error messages actionable? Is the session `status` accurate?
- **Errors are wrapped with context, never swallowed.** No panics outside `main` or impossible states.

### 5. Concurrency

- Every goroutine has a clear owner and termination path. No leaks when the client disconnects or the context is cancelled.
- Shared state is protected. Code must be race-free under `go test -race`.
- `context.Context` is propagated and honored on all I/O. Response bodies and streams are always closed.
- Child process handling (signals, grace period, zombie processes) is correct (spec §3.1, §6.2).

### 6. Determinism and replay fidelity

This is the core promise of retrace. Look for:

- Map iteration order, `time.Now()`, randomness or goroutine scheduling leaking into fingerprints, cassette ordering or replay output.
- Canonicalization changes that alter fingerprints of existing cassettes (ADR 0007).
- Any path where a response that is not from the cassette could reach the agent without a `provenance` mark (ADR 0006).
- Network access in `exact` mode (AC-09).

### 7. Contracts and compatibility

- **CLI:** flags, output formats (`--json`) and exit codes are a public contract (spec §9). Breaking changes need a spec change.
- **Cassette schema:** any change requires a migration and a `PRAGMA user_version` bump (spec §7.6). Old cassettes must remain readable.
- **Protocols:** behavior towards the Anthropic API and MCP must match ADRs 0003 and 0004.

### 8. Security

- No credential reaches disk: header redaction (ADR 0008), logs above `debug`, error messages.
- The proxy binds to loopback by default. There is no unexpected network exposure.
- Cassettes are untrusted input when read: check paths, sizes and parsing robustness (SECURITY.md, *Scope*).
- No shell or command injection, including in CI workflows.

### 9. Readability and maintainability

- **Names explain themselves.** A reader should not need the implementation to understand what a function, type or variable is.
- **Single responsibility.** Functions and types do one thing. Long functions with several levels of nesting are a finding.
- **Junior-friendly.** Could a junior developer understand, modify and debug this code? If clever code is needed, its *why* is commented.
- Comments explain *why*, not *what*. Exported identifiers are documented.
- The code follows the surrounding code's idioms and the Go conventions in CLAUDE.md.

### 10. Tests

- **Quality over coverage.** Tests assert behavior, not implementation details. A test that cannot fail is a finding.
- Acceptance criteria are traced: tests reference the `AC-xx` they verify, and claimed ACs are actually covered.
- Edge and failure cases from dimension 4 are tested, not just the happy path.
- Golden files are used for canonical outputs and regenerated with `-update`, not by hand.
- No real external calls. Fake upstreams are used, and tests are deterministic (no sleeps used for synchronization, no order dependence).

### 11. Git and PR hygiene

- Commits are small, atomic and in Conventional Commits format. The body explains *why*. Spec, ADR and implementation are in separate commits.
- The PR description fills in the traceability section (spec sections, AC-xx, ADRs) truthfully.
- The PR is focused. Unrelated changes are a finding.
- Nothing is committed that should not be: real cassettes, `.retrace/`, secrets, editor files, build artifacts.

### 12. Documentation

- If behavior, the CLI or the cassette format changed, the spec, README and help texts are updated in the same PR.
- ADR statuses and the ADR index are consistent (also enforced by CI).

## Output format (automated reviewer)

The automated reviewer posts **one summary comment** and **inline comments** for line-specific findings.

The summary comment uses this structure:

```markdown
## 🔍 Review: <one-line verdict>

**Verdict:** ✅ Ready to merge | 💬 Merge after addressing majors | ⛔ Blockers found
**Scope:** <PR type> · <files changed> · <dimensions that apply>

### Summary
<2–4 sentences: what the PR does, whether it achieves it, the most important finding>

### Findings
| # | Severity | Dimension | Location | Finding |
|---|---|---|---|---|
| 1 | 🔴 | Determinism | `internal/match/canon.go:42` | … |

### By dimension
<One short paragraph per applicable dimension. Use one line "N/A: <reason>" for the rest.>

### Questions for the author
<Optional: things that need context, not necessarily problems>
```

Inline comments start with the severity and the dimension, e.g. `🟠 Resilience: ...`, and include the fix when possible.
