# Vision

> Record, replay and diff AI agent sessions. Swap the model, freeze the tools, see exactly where behavior diverges.

## 1. Problem statement

AI agents are hard to reason about because almost nothing in their execution is reproducible:

- **The model is non-deterministic.** The same prompt can produce a different tool call on the next run, even at temperature 0.
- **The tools are stateful.** MCP servers read live data. A failing session from yesterday cannot be re-run today against the same world.
- **Experiments are expensive.** Each attempt to answer "would a different model have done better here?" means re-running the whole session with real calls, real latency and real cost. Even then the comparison is confounded, because the tools may have returned something different this time.
- **Bugs are anecdotal.** A user reports "the agent did something weird". The only artifact is a trace you can look at but cannot execute.

Observability tools are good at showing what happened. None of them let you run it again while changing exactly one variable.

## 2. Target users and core use cases

**Users:** engineers who build agents on top of LLM APIs and MCP servers, and who need to debug them, evaluate model changes or explain regressions.

| Use case | What retrace provides |
|---|---|
| **Offline debugging** | Re-run a recorded session with zero network calls, zero cost and identical inputs. Attach a debugger to the agent and step through a real failure. |
| **Model migration** | Replay a session against a new model while tools return exactly what they returned originally. Any behavior change comes from the model alone. |
| **Tool regression** | Replay the recorded model decisions against live tools to detect when a tool's output has drifted from what the model originally saw. |
| **Trajectory comparison** | Diff two sessions and get the first step where they diverge: which step, which tool, which arguments. |
| **Shareable bug reports** | A cassette is a single portable file that can be attached to an issue and replayed by someone else. |

## 3. Product principles

1. **Zero-intrusion adoption.** Enabling retrace means changing a base URL. No SDK, no code changes and no wrapper library inside the agent.
2. **Never fabricate silently.** When the replay cannot serve an interaction faithfully, retrace stops or marks the result explicitly. A synthetic response is never indistinguishable from a recorded one.
3. **One variable at a time.** Every replay mode holds everything constant except one dimension (the model or the tools). That is what makes a comparison meaningful.
4. **Cassettes are durable artifacts.** They are portable, self-describing and versioned, and they hold enough data to drive a future visual UI without re-recording.
5. **Do one thing well.** retrace replays and compares. It runs alongside tracing and observability tools and does not try to replace them.

## 4. Non-goals

- **Observability or tracing platform.** No dashboards, no production monitoring, no long-term trace retention. Use Langfuse, LangSmith, Phoenix or OpenTelemetry for that.
- **Evaluation framework.** retrace shows where trajectories diverge, not which one is better. Scoring belongs to eval tools, which can consume retrace's output.
- **Agent framework or orchestrator.** retrace never decides what the agent does. It only controls what the agent sees.
- **Production traffic proxy.** retrace is a development and CI tool. It is not hardened for production traffic.

## 5. Positioning vs. existing tools

| | Tracing / observability (Langfuse, LangSmith, Phoenix) | HTTP record/replay (go-vcr, VCR.py, Polly) | **retrace** |
|---|---|---|---|
| Captures LLM calls | Yes (SDK or OTel instrumentation) | Yes (raw HTTP) | Yes (transparent proxy) |
| Understands MCP tool calls | Partially | No | Yes |
| Deterministic replay | No | Yes, exact only | Yes, exact and partial |
| Swap one variable (model or tools) | No | No | Yes |
| Trajectory diff | Limited (side-by-side views) | No | First-divergence detection |
| Requires code changes in the agent | Yes (SDK or instrumentation) | Yes (test harness) | No (base URL only) |

retrace is meant to be used **with** a tracing tool: trace in production, record what you want to investigate, then replay and diff locally.

## 6. Long-term direction

These are beyond the MVP. They are listed because they constrain today's design, especially the cassette data model.

- **Web UI:** an animated sequence view of a session (user → model → MCP servers), a tool topology graph, and a side-by-side diff between models with the divergence point highlighted.
- **More divergence strategies:** `fallthrough`, `nearest-match` and `llm-stub`. See [ADR 0006](adr/0006-strict-divergence-default.md).
- **More providers:** OpenAI-compatible APIs, Bedrock and Vertex.
- **More MCP transports:** stdio (via a wrapper process) and legacy HTTP+SSE.
- **CI integration:** replay a corpus of cassettes against a candidate model and fail on unexpected divergence.
- **OpenTelemetry export:** emit replayed sessions as traces so they appear in the user's existing observability stack.

## 7. Success criteria for the MVP

The MVP succeeds when a single README GIF shows this flow end to end on a small demo agent that uses two MCP servers:

1. `retrace record` captures a real session, including streaming model output and tool calls to both MCP servers.
2. `retrace run --tools frozen` replays it offline with no network access, and the agent produces the same output.
3. `retrace run --model <other> --tools frozen` replays it against a different model. On a cassette miss it stops at the exact point where the new model asks for something that was never recorded.
4. `retrace diff` prints the first divergence between the two trajectories: the step, the tool and the arguments.

Plus, a reader of the repository should understand the design and its trade-offs from `docs/` alone, without reading the code.
