# 0013. Explicit agent command for replay

- Status: Accepted
- Date: 2026-11-12

## Context

`retrace run <id>` re-launches the agent. Until spec v0.26, with no `-- <cmd>` it ran the argv and working directory stored in the source session (`command` and `cwd`, spec §7.2). That made the common case short: `retrace run checkout`.

A cassette is meant to be shared: attached to an issue and replayed by someone else ([vision](../vision.md), §2). Its `command` is then code written by someone else, and `run` executed it with the user's privileges without asking (#125, found in the security review #100). Nothing in the cassette format can make that safe: the command is the agent, and the agent is arbitrary code by design.

The `run` command line is part of the CLI contract, so changing it needs an ADR (ADR 0000).

## Decision

1. `run` never runs the recorded command implicitly. It runs the command given after `--`, or the recorded command and cwd only with an explicit `--recorded-command` flag.
2. Without either, `run` fails with a usage error (exit `2`) that prints the stored argv and cwd, escaped, so the user can review them and choose. Giving both is a usage error. These errors come before any cassette is created (spec §9.3).
3. `run` with `-- <cmd>` behaves as before: the agent runs in retrace's working directory.

## Alternatives considered

### Keep the default and document the risk

It keeps `retrace run <id>` short, but the risk falls on exactly the users the sharing use case invites: people who replay a cassette they did not record. A warning in the docs does not stop a command that runs before it is read.

### Ask for confirmation interactively

`run` would print the command and ask "run it? [y/N]". It needs a terminal, so scripts and CI would need a flag to skip the prompt anyway, and that flag would be the explicit opt-in this ADR adopts. A prompt that is answered by habit protects little.

### Trust the recorded command only for cassettes recorded on this machine

It would need a notion of provenance or a signature on cassettes, which is a cassette format change and still fails for a cassette copied between users of one machine. It is far more machinery than the problem needs.

## Consequences

### Positive

- Opening a cassette from someone else never runs its program by accident.
- The user sees the stored command before choosing to run it.

### Negative

- `retrace run <id>` alone no longer works. Existing scripts, the README examples and the demo script must add `--recorded-command` or `-- <cmd>`. There is no release yet, so no published contract breaks.

### Risks and mitigations

- *Users add `--recorded-command` by habit, also for cassettes from others.* The flag name says what it does, and the error that suggests it prints the command first. Beyond that, running someone else's agent is a choice retrace cannot make for the user.
