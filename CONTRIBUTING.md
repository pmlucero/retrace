# Contributing to retrace

Thanks for your interest. retrace is in **phase 0 (design)**: the vision, spec and ADRs exist, and implementation has not started. Design feedback is the most valuable contribution right now.

By participating you agree to the [Code of Conduct](CODE_OF_CONDUCT.md).

## How the project works: Spec-Driven Development

Behavior is specified before it is implemented:

1. **Vision** ([docs/vision.md](docs/vision.md)): why retrace exists, its principles and its non-goals.
2. **Spec** ([docs/spec/core.md](docs/spec/core.md)): what retrace does, including numbered acceptance criteria (`AC-xx`).
3. **ADRs** ([docs/adr/](docs/adr/README.md)): why the significant decisions were made.
4. **Code**: implements the spec and is verified by tests that reference the acceptance criteria.

Changes flow in that order. A pull request that adds behavior not described in the spec will be asked to update the spec first.

## Ways to contribute

- **Report a bug or propose a feature** using the [issue forms](https://github.com/pmlucero/retrace/issues/new/choose).
- **Challenge a design decision.** Open an issue that references the ADR or spec section. ADRs in `Proposed` status are explicitly open for discussion.
- **Propose a spec change.** Open an issue first for anything non-trivial. Then send a PR that changes only `docs/spec/`, and explain the motivation and how acceptance criteria are affected.
- **Propose an ADR.** Copy the template in [docs/adr/README.md](docs/adr/README.md), use the next free number, set the status to `Proposed`, and add it to the index. An ADR is needed for changes to the cassette format, the CLI contract, supported protocols, or new dependencies.
- **Implement** (once phase 0 is approved): pick an issue, comment that you are working on it, and reference the acceptance criteria you cover.

Please read the [non-goals](docs/vision.md#4-non-goals) before proposing features. In particular, observability and tracing features are out of scope.

## Pull requests

- Changes to `docs/spec/`, `docs/adr/` and `.github/CODEOWNERS` require review from the maintainer (see [CODEOWNERS](.github/CODEOWNERS)).
- All PRs to `main` need an approving review. Fill in the PR template: the traceability section (spec sections, AC-xx, ADRs) is what reviewers look at first.
- CI must pass. It checks Markdown style (`markdownlint-cli2`, configured in `.markdownlint-cli2.jsonc`), internal links and ADR consistency (`.github/scripts/check_docs.py`), and the PR title format. Run them locally with:

  ```sh
  npx markdownlint-cli2 "**/*.md"
  python3 .github/scripts/check_docs.py
  ```

- Every PR from a branch of this repository gets an advisory review from Claude, following [docs/review-guidelines.md](docs/review-guidelines.md). It does not block merging. Maintainers can request another pass by commenting `@claude review`. PRs from forks are reviewed by the maintainer.
- Keep PRs focused. A spec change, an ADR and an implementation belong in separate commits, and preferably in separate PRs.

### Branches and releases

retrace uses **trunk-based development**. `main` is the only long-lived branch, and it is always releasable.

- Work happens on short-lived branches cut from `main` and merged back through a PR. Rebase or merge `main` often, and keep branches to days, not weeks.
- Branch names are `<type>/<kebab-case-description>`. The types are the same as for commits: `feat/`, `fix/`, `docs/`, `test/`, `refactor/`, `chore/`. Example: `feat/sse-replay-timing`. CI enforces this (`branch-name` check).
- **Urgent fixes** are ordinary `fix/` branches from `main`, prioritized for review. There is no separate hotfix flow.
- **Releases** are tags `vX.Y.Z` (Semantic Versioning) on `main`. Binaries are built from the tag (ADR 0001).
- **Maintenance branches** (`release/<major>.<minor>`) are created from a tag only if an older release needs a patch while `main` has moved on. Fixes land on `main` first and are cherry-picked.

### Commits

- One logical change per commit.
- [Conventional Commits](https://www.conventionalcommits.org/): `feat:`, `fix:`, `docs:`, `test:`, `refactor:`, `chore:`, with an optional scope, e.g. `feat(replay): serve SSE chunks with recorded offsets`.
- The subject is imperative and at most 72 characters. The body explains *why* and references `AC-xx` or `ADR NNNN` when relevant.

### Code

Go conventions, the package layout and testing rules are in [CLAUDE.md](CLAUDE.md). That file is written for coding agents, and the same rules apply to humans. In short:

- `CGO_ENABLED=0` must build, and the standard library is preferred.
- `go test -race ./...`, `go vet ./...` and `golangci-lint run` must pass.
- Tests never call real external APIs. They use in-process fake upstreams.
- Tests that verify an acceptance criterion reference it (`// AC-12`).

## Cassettes and secrets

Never commit cassettes from real sessions, `.retrace/` directories, API keys or tokens. Cassettes store prompts and tool outputs verbatim. Test cassettes under `testdata/` must be generated by retrace from fake upstreams.

## Security

Report vulnerabilities privately. See [SECURITY.md](SECURITY.md).

## License

By contributing, you agree that your contributions are licensed under the [Apache License 2.0](LICENSE).
