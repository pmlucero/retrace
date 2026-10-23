# retrace

[![docs](https://github.com/pmlucero/retrace/actions/workflows/docs.yml/badge.svg)](https://github.com/pmlucero/retrace/actions/workflows/docs.yml)
[![License: Apache 2.0](https://img.shields.io/github/license/pmlucero/retrace)](LICENSE)
[![Status: MVP in progress](https://img.shields.io/badge/status-MVP%20in%20progress-yellow)](#project-status)

**Record, replay and diff AI agent sessions. Swap the model, freeze the tools, see exactly where behavior diverges.**

> **Status: MVP in progress.** The design (phase 0) is approved, and implementation has started. There is no usable release yet. See [Project status](#project-status).

## What is retrace

retrace is a VCR for AI agents. It is a transparent proxy, shipped as a single Go binary, that sits between your agent and:

- the **LLM API** (Anthropic Messages API, including streaming), and
- your **MCP servers** (Streamable HTTP).

It records every session into a **cassette**. You can then replay that session deterministically while changing exactly one variable:

- Keep the tools frozen and **swap the model** to see what a different model would have done with the exact same tool results.
- Keep the model's decisions and run the **live tools** to detect when a tool's behavior has drifted.
- **Diff** two sessions to find the first step where they diverge: the step, the tool and the arguments.

Enabling it requires no SDK and no code changes. You point the agent's base URLs at retrace.

## What retrace is not

retrace is **not** an observability or tracing tool. Langfuse, LangSmith, Phoenix and OpenTelemetry already do that well. retrace is about **replay and experimentation**, and it is designed to be used alongside your tracing stack, not instead of it. See [docs/vision.md](docs/vision.md#5-positioning-vs-existing-tools).

## Replay modes

| Mode | Model | Tools | Use it to… |
|---|---|---|---|
| `exact` | recorded | recorded | Debug a session offline, for free, with no network. |
| `swap-model` | **real, different model** | recorded | Compare models under identical tool results. |
| `live-tools` | recorded | **real** | Detect tool drift against recorded model decisions. |
| `diff` | — | — | Find the first divergence between two trajectories. |

When a replay hits something that was never recorded, retrace stops by default (`strict`) and reports the exact divergence point. It never invents a response silently.

## CLI at a glance

> Tentative. The authoritative definition is in [the spec](docs/spec/core.md#9-cli).

```sh
# Record a session (the agent reads ANTHROPIC_BASE_URL and RETRACE_MCP_*_URL)
retrace record --session checkout --mcp orders=http://localhost:9001/mcp \
               --mcp email=http://localhost:9002/mcp -- python agent.py

# Replay it offline, exactly
retrace run checkout

# Same tool results, different model
retrace run checkout --model <other-model> --tools frozen

# Where did the trajectories diverge?
retrace diff checkout checkout.swap-model.1

retrace ls
retrace show checkout
```

> ⚠️ Cassettes store prompts and tool outputs verbatim. Auth headers are always redacted, but review a cassette before sharing it.

## Project status

retrace follows **Spec-Driven Development**: vision, spec and ADRs first, code after approval.

| Phase | Scope | State |
|---|---|---|
| 0 | Vision, core spec, ADRs, repo conventions | Done |
| MVP | Record (LLM + MCP), exact replay, swap-model with `strict`, terminal diff, demo agent with two MCP servers | **In progress** |
| Later | Web UI (sequence view, tool topology, model diff), more divergence strategies, more providers and transports | Planned |

## Documentation

- [Vision](docs/vision.md): the problem, principles, non-goals and positioning.
- [Core spec](docs/spec/core.md): modes, matching, the cassette data model, diff, CLI and acceptance criteria.
- [Architecture decisions](docs/adr/README.md).
- [MVP plan](docs/plan/mvp.md): milestones and the order of implementation.

## License

[Apache 2.0](LICENSE)
