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
