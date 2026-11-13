# Security policy

## Supported versions

retrace has not been released yet. Once it is, only the latest minor release will receive security fixes.

## Reporting a vulnerability

**Do not open a public issue for security problems.**

Report vulnerabilities privately through GitHub: go to the [Security tab](https://github.com/pmlucero/retrace/security) and choose **Report a vulnerability**.

Please include:

- The affected version or commit.
- A description of the issue and its impact.
- Steps to reproduce, ideally with a minimal cassette or command. Do not include real API keys or private data.

You can expect an acknowledgement within 7 days. Fixes are coordinated privately and disclosed in a GitHub Security Advisory once a release is available. Reporters are credited unless they prefer otherwise.

## Scope

retrace is a local development tool. These areas are especially relevant:

- **Credential leakage into cassettes.** For example, an auth header or token that bypasses redaction ([ADR 0008](docs/adr/0008-secret-redaction.md)).
- **Network exposure of the proxy.** For example, binding beyond loopback without `--listen-unsafe`, or upstream requests in `exact` mode, which must make no outbound connections.
- **Untrusted cassettes.** For example, a crafted cassette file that causes code execution, path traversal or resource exhaustion when opened with `run`, `show` or `diff`.

Out of scope:

- Sensitive *content* (prompts, tool outputs) stored in cassettes by design. This is documented, and body redaction is opt-in.
- Vulnerabilities in the agent, LLM provider or MCP servers being proxied.

## Accepted risks

The security review before `v0.1.0` ([#100](https://github.com/pmlucero/retrace/issues/100)) fixed what it found or accepted it here, with the reason. An accepted risk is revisited when its reason stops holding.

### Cassettes and the CLI

- **The agent's command line is stored.** A cassette keeps the agent's argv, because a replay may run it again. Do not pass secrets as command-line arguments; use environment variables.
- **Path segments and bodies are stored as they are.** Redaction covers headers, query parameters, URL userinfo and, opt-in, JSON body paths. A credential inside a path segment (`/t/<token>/…`) or in a non-JSON body is not redacted.
- **`live-tools` sends the agent's MCP traffic to the recorded URLs.** With a cassette from someone else, give every server with `--mcp`, or the agent's MCP requests, including any auth header it adds, go to the URLs that cassette names.
- **The proxy does not check `Origin` or `Host`.** It listens on loopback and adds no credentials of its own, so a web page that reaches it can at most add requests to the cassette or cause a divergence.
- **JSON outputs keep control characters.** `show --json` and `diff --format json` escape only what JSON requires. They are for programs, which must escape the values themselves before showing them. The text outputs escape them.
- **Opening an old cassette upgrades it.** Every command that opens a cassette (`ls`, `show`, `diff`, `reindex`, and `run` for its source cassette) applies pending schema migrations to a cassette with an older schema, in place (spec §7.6).
- **SQLite file-format robustness is the driver's.** retrace checks the schema of a cassette it opens (spec §7.6), but a corrupt database file is handled by `modernc.org/sqlite`, a translation of upstream SQLite.

### Dependencies

- **`github.com/remyoudompheng/bigfft` is unmaintained** (last release in 2023). It is a small arithmetic package that `modernc.org/sqlite` pulls in, with no I/O and no parsing of untrusted input.
- **`go.sum` lists modules the binary does not link**, such as the SQLite driver's code generators and test dependencies. They are there to verify the module graph; `go version -m retrace` lists what is linked.
- **The demo's Python dependencies are not part of the product.** `examples/demo/` is not in the Go module or the release archives. Its lock file has hashes, follows the 14-day rule and is audited with `pip-audit` in the review.
- **Some tool versions are pinned by hand.** The versions of golangci-lint, GoReleaser, uv and govulncheck that CI installs are inputs Dependabot does not update; they are bumped by hand ([#141](https://github.com/pmlucero/retrace/issues/141) tracks govulncheck). The docs check installs `markdownlint-cli2` with `npx`, so its own dependencies are not locked. That job has a read-only token and no secrets.

### Repository and pipeline

- **One maintainer approves and merges.** GitHub does not let an author approve their own pull request, so pull requests are merged through the ruleset's administrator bypass, which still requires a pull request. Direct pushes to `main` and `release/*` are blocked, and only an administrator can create, move or delete `v*` tags.
- **The Claude review is advisory.** Its comments are not a security control. It reviews pull requests from this repository only, never runs their code in the job that holds its tokens, and can only comment.
- **The review's coverage job can write to the `main` build cache.** A `@claude review` comment runs the workflow in the context of `main`, and the coverage job runs the pull request's tests there. Tests written to do so could store a poisoned entry in the Go cache that the `go` and `demo` workflows restore on every branch. This needs someone who can push branches to this repository and a maintainer's review comment, and those workflows hold no secrets and a read-only token. Releases are built without a cache. If more people get write access, those two workflows turn the cache off.
- **A release does not re-run CI.** Only an administrator can push a `v*` tag, the release guard requires the tagged commit to be on `main` or `release/*`, and the release process tags a commit whose CI passed.
- **Release builds use the newest patch of the Go release in `go.mod`,** not a fixed patch, so a rebuild picks up standard library security fixes. The release job records the Go version it used.
