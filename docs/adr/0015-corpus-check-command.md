# 0015. Corpus check command

- Status: Accepted
- Date: 2026-11-25

## Context

`run` replays one session and `diff` compares two. Answering "did my change alter my agent's behavior?" for a pull request means doing that for every recorded session, often several times, and turning the results into one verdict a CI job can act on. The spec listed this as future work ("CI mode", §12). It adds a command, flags and an exit code, so it changes the CLI contract and needs an ADR (ADR 0000).

Testing v0.1.0 against real models showed three constraints:

- A live model can answer the same request differently each time, so one replay per session cannot tell a change from noise.
- The change under test is often the agent's code or prompt, not the model. The replay must run the code of the checkout under test, with the input that each session recorded, which usually lives in the recorded argv.
- A cassette stores the absolute working directory of the machine that recorded it. That directory does not exist on a CI runner, and `run --recorded-command` rejects it.

## Decision

1. A new command, `retrace check`, replays a corpus of sessions and summarizes their outcomes (spec §9.9). By default the corpus is every finished recording in `--dir`.
2. It uses the replay modes and flags of `run`. The mode comes from `--model` and `--tools` as in `run`.
3. The agent command is `-- <cmd>` for every session, or each session's recorded argv with `--recorded-command`. A new `--cwd <dir>` sets the working directory of every agent, so recorded argv runs inside the checkout under test. The explicit opt-in of ADR 0013 applies unchanged.
4. `--repeat <n>` (default `1`) runs `n` replays per session and reports counts per session and per divergence point ("3/5 diverged at `search_orders`").
5. Each replay has one outcome: `identical`, `diverged`, `error` or `skipped`. `diverged` uses `diff`'s definition of the first divergence (§8.3), so `check` and `diff` never disagree.
6. Replays run one at a time, each with its own proxy on a free loopback port. A per-replay `--timeout` (default `10m`) keeps one stuck agent from hanging the job. Replay cassettes go to a fresh `--out` directory, never to `--dir`.
7. `--max-cost` estimates the cost of a `swap-model` check from the corpus's recorded tokens and the embedded price table, refuses to start over budget, and skips the remaining replays once the next one would exceed it.
8. Exit codes: `0` all identical, `3` any divergence, `1` errors or a failed check, `5` budget skipped replays (new), `2` usage error. A divergence takes precedence.
9. Output: a text table, a versioned JSON document (`docs/schema/check.v1.json`) and, optionally, Markdown appended to a file such as `$GITHUB_STEP_SUMMARY`. The output reports facts only.

## Alternatives considered

### Extend `run` to take several ids

It avoids a new command, but `run`'s contract (one source, one `--as`, exit codes tied to one agent) is already dense. Corpus selection, repeats, a budget and aggregated output would change the meaning of most of its flags and exit codes.

### Leave it to a shell loop over `run` and `diff`

It works today, but every user would re-implement corpus selection, repeat counting, aggregation per divergence point, budget checks and a CI summary, and the exit codes of `run` and `diff` do not compose into one verdict.

### A placeholder template for the agent command

`-- python agent.py {args}` would splice the recorded arguments into a new command. It is flexible, but it adds a mini-language to quote and document. Recorded argv plus `--cwd` covers the common case with no new syntax.

### Parallel replays from the start

A `--jobs` flag would make large corpora faster, but it needs isolation between agents, an output order and a shared budget. Sequential replays are enough for a first version, and the free-port proxy keeps `--jobs` possible later.

### Fail on a divergence rate threshold

A threshold such as "fail if more than 20% of replays diverge" fits noisy models, but picking a sensible default needs experience with real corpora. Version 1 fails on any divergence and reports the rates, so a threshold can be added without changing the outcomes.

## Consequences

### Positive

- One command gives a pull request a verdict on the whole corpus, with exit codes a CI job can use directly.
- A corpus recorded on a laptop replays on a CI runner against the checkout's code.
- Repeats and rates make noise from live models visible instead of hiding it in a single run.

### Negative

- Exit code `5` is new and only `check` uses it.
- A check with many sessions and repeats is slow, because replays are sequential.
- The budget limits what starts, not what the API bills, so a check can spend somewhat more than `--max-cost`.

### Risks and mitigations

- *A corpus is code.* `check --recorded-command` runs every cassette's command, so a pull request that edits a cassette chooses what runs in CI. The spec (§10) and the README say that workflows running untrusted pull requests must treat the corpus as part of the untrusted change.
- *Recorded argv with absolute paths fails on another machine.* The documentation advises recording with relative paths or commands found on `PATH`, and the check reports such replays as `error` with the agent's failure.
