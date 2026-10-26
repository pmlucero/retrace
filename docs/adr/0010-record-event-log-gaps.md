# 0010. Record gaps in the semantic event log

- Status: Proposed
- Date: 2026-10-26

## Context

The cassette writer sits behind a bounded queue, so that recording never blocks forwarding ([ADR 0009](0009-append-only-event-log.md), AC-03). When the queue is full, or a write fails, it drops records instead of waiting (docs/plan/mvp.md, *Writer queue*).

A dropped record of an interaction is visible in the cassette: the interaction is completed with an `error` that says its recording is incomplete. A dropped **event** leaves no trace in the cassette. Only the logs and `Writer.Stats` know about it. An event is not marked on its interaction, because that interaction may already be complete, and complete rows are never updated (ADR 0009).

The semantic log is what `show` and `diff` read. A session with a silently missing `tool_use` or `tool_call` has a shorter trajectory. `diff` would then report a divergence the agent never made, or hide one it did make, and nothing would tell the user why. That contradicts two product principles ([vision](../vision.md), §3):

- **Never fabricate silently.** An incomplete trajectory presented as complete is a fabrication by omission.
- **Cassettes are durable artifacts.** A cassette is attached to issues and read by someone else, later, without the recording's logs. Whether it is complete must be stated in the file itself.

Gaps are expected to be rare. The queue holds 4096 records and 64 MiB, and they only happen when the disk cannot keep up with the proxy. The mechanism must therefore cost nothing on the hot path, and add as little schema as possible.

## Decision

1. The `session` row gets a new column, `dropped_events INTEGER NOT NULL DEFAULT 0`: the number of events that the recorder assigned a `seq` to but did not write, for any reason (queue full, write failure, or a dropped parent), before the session ended. Events that reach the writer after that cannot be counted, because the session row is already final.
2. It is written once, when the session ends, in the same update that sets `status` and `ended_at`. The writer already counts drops in memory, so the hot path does not change. The session row is already updated when the session ends, so recording adds no new kind of update to ADR 0009.
3. `seq` values are assigned contiguously, and the seq of a dropped event is never reused. A missing `seq` therefore marks where a gap is, with no extra rows. Drops at the end of the log leave no missing `seq`, and the counter still covers them. This relies on one writer per session: a cassette is never reopened to record more, so no later writer, seeded from `MAX(seq)`, can hand out a dropped seq again.
4. Readers use it. `ls` marks the session's status, because its steps and totals come from the same log. `show` warns in the session summary and marks each gap in its timeline. `diff` warns, in text and in JSON, that a divergence point may be caused by missing events. `reindex` rebuilds the events from the stored interactions, which recovers the dropped ones, and resets the counter to `0`. It warns when interactions are marked incomplete, because events derived from partial data may still be missing. A `divergence` event comes from a cassette miss, not from an interaction, so it cannot be rebuilt: for a session with status `diverged`, `reindex` keeps the count and says it may be too high.
5. `reindex` is the only operation that changes the counter after the session has ended. This is an explicit exception to ADR 0009's rule that complete rows are never updated, and it has the same footing as `reindex` itself: ADR 0009 already lets it rebuild the event layer, and the counter describes that layer.
6. The change ships as schema migration `0002`, which raises `PRAGMA user_version` to `2` (spec §7.6). Cassettes of schema version 1 migrate with `dropped_events = 0`. They were all written before any release, so this assumption costs nothing in practice.

## Alternatives considered

### Document that gaps are not recorded

This needs no schema change. Readers could still infer some gaps from interactions marked incomplete. But an event dropped on a complete interaction would stay invisible, and `diff` would silently present a truncated trajectory as real behavior. It was rejected because it breaks "never fabricate silently".

### A `gap` event kind in the event log

The writer would append a marker event at the seq of each dropped event (or each run of them) when it closes. This records the position as well as the count. But it puts a non-semantic row into the semantic layer, which every reader of `events` (`diff`, `show --json`, the future UI) must then filter out. It needs a change to the `kind` constraint, which also means a migration. `reindex` would have to decide whether to keep or rebuild markers that are not derived from interactions. And writing one row per gap at close time does not scale as well as one counter. Contiguous seqs already give the position for free.

### A JSON metadata field (for example `recording: {dropped_events, dropped_records, incomplete_interactions}`)

It is more extensible, but no other field is needed today. Incomplete interactions are already marked on their own rows, and record counts beyond events say nothing a reader can act on. It would also be a migration, so it has no advantage over an integer column. Storing it in `labels` was rejected, because labels belong to the user.

### Writer statistics in the cassette (queue depth, high-water marks, drop counts per kind)

That is observability of retrace itself, which is a non-goal ([vision](../vision.md), §4). The cassette records only what a reader needs in order to trust the trajectory: whether events are missing, and where.

## Consequences

### Positive

- A cassette says by itself whether its trajectory is complete, without the logs of the run that produced it.
- `show`, `diff` and `reindex` can warn the user instead of presenting a truncated trajectory as real.
- No cost on the hot path: the writer already counts drops in memory.

### Negative

- It is a schema change: migration `0002` and `user_version` `2`. Older binaries refuse to write the new cassettes (spec §7.6).
- The count is only known once the session ends. A session left with status `recording` (retrace was killed, or the writer got stuck) has `dropped_events = 0`, which says nothing. Its status already tells readers that the cassette is a prefix of the log of unknown completeness.

### Risks and mitigations

- *After `reindex`, a `diverged` session may over-report its gaps.* Its count is kept because a dropped `divergence` event cannot be rebuilt. Over-reporting is the safe side: a user is told to distrust a trajectory that may be complete, never the reverse.

- *Contiguous seqs become a contract.* Any future writer that assigns seqs with gaps of its own would create false gap markers. The spec states the rule, and a test asserts that missing seqs are exactly the dropped ones.
- *`reindex` resets the counter, but events derived from incomplete interactions may still be missing.* `reindex` warns when it finds interactions marked incomplete. Those interactions keep their `error`, so the information is not lost.
