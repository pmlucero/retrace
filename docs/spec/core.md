# retrace core specification

- Status: Approved
- Version: 0.15
- Last updated: 2026-10-28

This document specifies the behavior of retrace's MVP: recording, replay modes, the cassette data model, diff and the CLI. Rationale for the main choices lives in the [ADRs](../adr/README.md). This spec references them instead of repeating them.

The key words MUST, MUST NOT, SHOULD and MAY are used as in RFC 2119.

## 1. Scope and terminology

**In scope (MVP):** recording and replaying sessions of an agent that talks to the Anthropic Messages API and to MCP servers over Streamable HTTP. It covers the `exact`, `swap-model` and `live-tools` modes, the `strict` divergence strategy, and a terminal diff.

**Out of scope (MVP):** web UI, other LLM providers, MCP stdio transport, and the `fallthrough`, `nearest-match` and `llm-stub` strategies. See §12.

| Term | Definition |
|---|---|
| **Agent** | The user's program under test. retrace launches it as a child process. |
| **Upstream** | A real endpoint behind the proxy: the Anthropic API or an MCP server. |
| **Channel** | A class of traffic: `llm` or `mcp`. |
| **Interaction** | One HTTP request/response exchange through the proxy, stored byte-faithfully. |
| **Event** | A semantic, timestamped fact derived from interactions (e.g. "model requested tool `search` with args X"). |
| **Session** | One execution of the agent under retrace, either a recording or a replay. |
| **Cassette** | The file that stores one session (interactions plus events). |
| **Trajectory** | The ordered sequence of a session's action events, used by diff. |
| **Fingerprint** | Hash of a canonicalized request, used for matching ([ADR 0007](../adr/0007-request-matching-fingerprints.md)). |
| **Cassette miss** | During replay, a request on a matched channel has no unconsumed recorded counterpart. |
| **Divergence** | The first point where two trajectories differ, or a cassette miss during replay. |

## 2. System overview

```mermaid
flowchart LR
    agent["Agent (child process)"]
    subgraph retrace
        router["Router"]
        fwd["Forwarder + recorder"]
        rep["Replayer (matching)"]
        cas[("Cassette (SQLite)")]
    end
    llm["Anthropic API"]
    mcp["MCP servers"]

    agent -- "/llm/anthropic" --> router
    agent -- "/mcp/&lt;name&gt;" --> router
    router -- "channel live" --> fwd
    router -- "channel from cassette" --> rep
    fwd --> llm
    fwd --> mcp
    fwd -- "append" --> cas
    rep -- "read source cassette" --> cas
```

- One local listener with path-based routing ([ADR 0002](../adr/0002-transparent-proxy-base-url.md)).
- For each channel, the active mode decides whether a request is **forwarded** to the upstream (and recorded) or **answered from the cassette** (and logged as replayed).
- Every session, recording or replay, produces its own cassette. Replays never modify the cassette they read.

## 3. Recording

### 3.1 Lifecycle

1. `retrace record` opens a new cassette, starts the proxy, and launches the agent command with the injected environment (§3.2).
2. Every request through the proxy is forwarded upstream. Request and response are recorded as an interaction, and events are derived (§7.3).
3. The session ends when the agent process exits. retrace waits for in-flight interactions to complete (default timeout 10 s), finalizes the cassette and exits (§9.7).
4. On `SIGINT` or `SIGTERM`, retrace forwards the signal to the agent, waits for it to exit, and finalizes the cassette with status `interrupted`. A partial cassette MUST remain readable and valid.
   - A terminal's Ctrl+C already delivers `SIGINT` to the agent, which shares retrace's process group. So retrace forwards `SIGINT` only if the agent has not exited 1 s later, and avoids interrupting it twice. `SIGTERM` is forwarded at once.
   - A second signal kills the agent. Only the agent process is killed: processes it started itself (for example under `sh -c`) may outlive it.
5. The session status is `completed` when the agent exits on its own, whatever its exit code, `interrupted` after a signal, and `failed` when retrace cannot run the session, for example when the agent command cannot be started.
6. retrace exits with `4` when the agent's exit code is non-zero, including after a signal, and with `0` when it is zero (§9.7). An agent terminated by a signal has exit code `128 + signal number`.

### 3.2 Environment injected into the agent

| Variable | Value |
|---|---|
| `ANTHROPIC_BASE_URL` | `http://<listen>/llm/anthropic` |
| `RETRACE_MCP_<NAME>_URL` | `http://<listen>/mcp/<name>` for each registered MCP server (`<NAME>` upper-cased, non-alphanumerics replaced with `_`) |
| `RETRACE_SESSION_ID` | the session id |
| `RETRACE_MODE` | `record`, `exact`, `swap-model` or `live-tools` |

The agent's MCP client configuration MUST use the `RETRACE_MCP_*_URL` values. That is the only change required in the agent.

### 3.3 LLM channel

- `POST /llm/anthropic/v1/messages` is recorded with full semantics, both streaming and non-streaming ([ADR 0003](../adr/0003-anthropic-messages-api-with-sse.md)).
- For streaming responses, each SSE event is forwarded to the agent **as soon as it is received** and stored as a chunk with its offset from the start of the response, that is, from when the response headers arrive. Recording MUST NOT add buffering to the forwarded stream.
- Responses reach the agent without `Content-Encoding`: retrace accepts a compressed response from the upstream and decodes it. What is stored is exactly what the agent received.
- Any other path under `/llm/anthropic/` is forwarded and recorded as an opaque interaction.

### 3.4 MCP channel

- All methods on `/mcp/<name>` (`POST`, `GET`, `DELETE`) are forwarded and recorded ([ADR 0004](../adr/0004-mcp-streamable-http-first.md)).
- A `POST` body MAY be a single JSON-RPC message or a batch. Each JSON-RPC request in it is matched to its response, whether the response comes as JSON or inside an SSE stream.
- The protocol version and server info from `initialize` are stored in the session metadata (§7.2).

### 3.5 Redaction

Header and body redaction are applied before anything is written to disk, as specified in [ADR 0008](../adr/0008-secret-redaction.md).

Credentials can also travel in URLs, so the same rule covers every URL retrace stores (the registered MCP server URLs and each interaction's `path`). The upstream always receives the original URL.

- Userinfo (`https://user:pass@host`) is replaced with `REDACTED`.
- A query parameter is redacted when its name, lower-cased and with `_` read as `-`, is a redacted header name, ends in one of the header suffixes (`-token`, `-secret`, `-key`), or is one of `key`, `token`, `secret`, `password`, `apikey`, `auth`, `sig` or `signature`. Its value becomes `[REDACTED:<sha256-prefix-8>]`, and the rest of the query is kept as it was.

**Body redaction** (`--redact <jsonpath>`, repeatable) is opt-in.

- Each rule is a JSONPath in this subset: it starts with `$`, followed by any of `.name`, `['name']`, `[n]` (array index), `.*` or `[*]` (every member or element), and `..name` (`name` at any depth). A dotted name has letters, digits, `_` and `-`; any other name is written `['name']`. Any other syntax is a usage error (exit `2`).
- Rules apply to every JSON body retrace stores, on both channels: request bodies, non-streamed response bodies, and the `data` of each SSE event. A body that is not JSON is stored as it is.
- Each matched value, whatever its type, is replaced by the string `[REDACTED:<sha256-prefix-8>]`. A string is hashed as it is, so it gets the same marker as the same value in a header. Any other value is hashed over its JSON text. A value selected by several rules, or nested inside another selected value, is masked once, over its original content.
- A body with no match is stored byte for byte. A body with a match is re-encoded compactly, keeping member order and number formats. In an SSE event, a `data` field split over several lines is written back as one `data:` line when something in it is masked. The other lines and their endings are kept.
- When body rules are set, a body nested more than 1000 levels deep cannot be redacted safely, so it is not stored, and the interaction's `error` says why. The same applies to an SSE event that cannot be redacted (the rest of the stream is not stored either), and to a body or SSE event larger than the proxy's in-memory limit for one body or event. In each case the interaction's recording is incomplete (§7.3), so replaying it is a cassette miss (§5).
- Redaction runs on the forwarding path. It parses each stored JSON body once, which takes milliseconds for a megabyte, against model latencies of hundreds of milliseconds.
- The agent and the upstream always receive the original bodies. Only what is written to the cassette is redacted.
- The rules are stored in the session's `redaction` metadata (§7.2). M3 applies them to incoming requests before fingerprinting them (§5).

## 4. Replay modes

`retrace run <id>` re-launches the agent (by default with the recorded command and working directory) against a proxy that serves the channels as follows:

| Mode | LLM channel | MCP channel | Network needed | Typical question |
|---|---|---|---|---|
| `exact` | from cassette | from cassette | none | "Let me debug this exact session offline." |
| `swap-model` | **real**, model overridden | from cassette | LLM only | "What would model X have done, given identical tool results?" |
| `live-tools` | from cassette | **real** | MCP only | "Do today's tools still return what the model saw?" |

### 4.1 `exact`

- Every request on both channels MUST be answered from the cassette via matching (§5). The proxy MUST NOT open any upstream connection, and a test MUST enforce this.
- Streaming responses are replayed chunk by chunk, immediately by default or with recorded timing under `--realtime`.

### 4.2 `swap-model`

- LLM requests are forwarded to the real API **after rewriting the `model` field** to the value of `--model`. The agent's code is unchanged. All other request fields are preserved.
- MCP requests are answered from the cassette via matching. A miss triggers the divergence strategy (§6).
- The resulting cassette records both the original model requested by the agent and the model actually used (§7.2, §7.3).

### 4.3 `live-tools`

- LLM requests are answered from the cassette via matching.
- MCP requests are forwarded to the real servers: the recorded URLs, unless overridden with `--mcp`.
- If a live tool returns a different result, the next LLM request's `messages` differ and its fingerprint misses. That is a divergence, handled by the same strategy (§6). No separate tool-result comparison is needed.

### 4.4 Common replay behavior

- A replay produces a new session with `parent_session_id` set to the source session and `mode` set accordingly.
- Interactions answered from the cassette are stored in the new cassette with `provenance = replayed` and a reference to the source interaction. Forwarded ones get `provenance = live`.
- At the end of a replay, recorded interactions that were never consumed are reported as a warning (count and first few), not as an error.
- A response answered from the cassette, in any mode and on either channel, carries the recorded status and headers, except hop-by-hop headers (RFC 9110 §7.6.1, plus any listed in `Connection`) and `Content-Length`. A non-streamed body gets the `Content-Length` of the stored body, which may differ from the original because of redaction. An interaction recorded with a transport error has neither status nor headers, and is served as in §5 step 2.

## 5. Request matching

Matching follows [ADR 0007](../adr/0007-request-matching-fingerprints.md). In summary:

1. Incoming requests on a matched channel are canonicalized and fingerprinted.
   - **LLM**: the body without `metadata`.
   - **MCP**: `server + method + params` without JSON-RPC `id` and without `params._meta`.

   Body redaction rules (§3.5) are applied first. A body that is not valid JSON, or that is nested more than 1000 levels deep, is fingerprinted over its raw bytes (for MCP, together with the server name). This refines point 1 of ADR 0007. A request whose body cannot be redacted (§3.5) cannot be matched: it is a cassette miss, and its divergence event has a null `received_fingerprint`, so no hash of unredacted data reaches the cassette.
2. Each fingerprint has a FIFO queue of recorded interactions. A request consumes the head of its queue, and an empty queue is a cassette miss.
   On the LLM channel, an interaction recorded with a transport error (no status, §7.3) whose recording is complete is served as the answer the agent got while recording: HTTP `502`, with the headers the proxy sends for a transport error and a body that starts with `retrace: upstream request failed:` and includes the interaction's `error`. Only the prefix is fixed, because the stored `error` may be reworded, e.g. when the agent had already closed the connection. This reproduces what the agent saw, so it is not a fabricated response. The new interaction is stored with `provenance = replayed`, no status and the source interaction's `error`, so it derives the same events as the source (§7.4).
   When an interaction's recording is incomplete (§7.3), replay never fabricates its response silently ([ADR 0010](../adr/0010-record-event-log-gaps.md)). Such an interaction keeps its place in its queue but cannot be served: a request that reaches it is a cassette miss, and it stays at the head, so the divergence shows up at the step where it happened. It is not reported as unconsumed (§4.4). This refines point 2 of ADR 0007, which predates incomplete recordings.
3. Replayed MCP responses get their JSON-RPC `id` rewritten to the incoming request's id. `initialize` responses get a fresh `Mcp-Session-Id`. Client notifications get `202`, and `GET` gets `405`.
4. On a miss, the replayer computes a structural diff against the next unconsumed recorded interaction on the same channel and server. The diff is a list of JSON paths with expected and received values, and it is attached to the divergence event.
   When the request reaches an interaction whose recording is incomplete, no diff is computed. The divergence event sets `incomplete_interaction` to that interaction's id, and the error message the agent receives says the matching recording is incomplete, so the user knows to record the session again rather than debug the agent.

## 6. Divergence handling

### 6.1 Strategy interface

```go
// DivergenceStrategy decides how to answer a request that missed the cassette.
type DivergenceStrategy interface {
    Name() string
    OnMiss(ctx context.Context, miss Miss) (Resolution, error)
}
```

`Resolution` is one of: **abort** (with an error response for the agent), or **respond** (with a response and a mandatory `provenance`). This is illustrative. The exact types are defined during implementation, but the two resolution kinds and the mandatory provenance are normative.

### 6.2 `strict` (MVP, default)

As specified in [ADR 0006](../adr/0006-strict-divergence-default.md):

1. Append a `divergence` event (§7.3) with the miss details.
2. Answer the agent: a JSON-RPC error `-32099 "retrace: cassette miss"` for MCP, or HTTP `422` with `{"type":"error","error":{"type":"retrace_cassette_miss","message":...}}` for the LLM API.
3. Send `SIGTERM` to the agent, then `SIGKILL` after `--grace` (default `5s`).
4. Finalize the cassette with status `diverged`. Print the divergence report (§8.4) and exit `3`.

### 6.3 Reserved strategies

`fallthrough`, `nearest-match` and `llm-stub` are accepted by the flag parser and rejected with `not implemented in this version` (exit `2`). Any response they produce in the future MUST set `provenance` to `fallthrough`, `nearest` or `synthetic` respectively.

## 7. Cassette data model

Storage is one SQLite file per session ([ADR 0005](../adr/0005-sqlite-cassette-storage.md)), structured as an append-only log with a raw layer and a semantic layer ([ADR 0009](../adr/0009-append-only-event-log.md)).

### 7.1 Overview

```text
session (1 row) ──┬── interactions ──┬── chunks
                  │                  └── blobs (request / response / canonical bodies)
                  └── events ──────────── (interaction_id → interactions)
```

All timestamps are RFC 3339 UTC strings with microsecond precision. Durations are integer milliseconds measured with a monotonic clock. JSON columns hold UTF-8 JSON text.

### 7.2 `session`

| Column | Type | Description |
|---|---|---|
| `id` | TEXT PK | Session id (user-given or generated `YYYYMMDD-HHMMSS-xxxx`) |
| `mode` | TEXT | `record`, `exact`, `swap-model` or `live-tools` |
| `parent_session_id` | TEXT NULL | Source session for replays |
| `status` | TEXT | `recording`, `completed`, `diverged`, `failed` or `interrupted` |
| `created_at` / `ended_at` | TEXT | Session bounds |
| `command` | JSON | Agent argv, e.g. `["python","agent.py"]` |
| `cwd` | TEXT | Working directory of the agent |
| `agent_exit_code` | INTEGER NULL | Exit code of the agent process |
| `model_override` | TEXT NULL | `--model` value in `swap-model` |
| `on_miss` | TEXT | Divergence strategy name |
| `mcp_servers` | JSON | `[{name, url, protocol_version, server_info}]` |
| `redaction` | JSON | Active header and body redaction rules |
| `retrace_version` | TEXT | Version of the binary that wrote the cassette |
| `deriver_version` | INTEGER | Version of the event derivation logic |
| `pricing_version` | TEXT | Version of the embedded price table (§7.5) |
| `labels` | JSON | Free-form key/value pairs (`--label k=v`) |
| `dropped_events` | INTEGER | Events missing from the semantic log because the cassette writer could not write them (§7.7). Written when the session ends |

The schema version is held in `PRAGMA user_version`.

### 7.3 `interactions` (raw layer)

| Column | Type | Description |
|---|---|---|
| `id` | INTEGER PK | Monotonic, assigned when the request arrives |
| `channel` | TEXT | `llm` or `mcp` |
| `upstream` | TEXT | `anthropic` or `mcp:<name>` |
| `http_method`, `path` | TEXT | Request line, path relative to the route |
| `status_code` | INTEGER NULL | HTTP status of the response |
| `request_headers`, `response_headers` | JSON | After redaction |
| `request_blob`, `response_blob` | TEXT NULL | SHA-256 references into `blobs`. `response_blob` is null when streamed |
| `streamed` | INTEGER | 1 if the response was SSE |
| `fingerprint` | TEXT NULL | Matching fingerprint (§5) |
| `canonical_blob` | TEXT NULL | Canonical form used for the fingerprint, kept for inspection |
| `started_at` | TEXT | Request arrival |
| `ttfb_ms`, `duration_ms` | INTEGER NULL | Time to first byte, and total time |
| `provenance` | TEXT | `recorded` (record mode), `replayed` or `live` |
| `source_interaction_id` | INTEGER NULL | For `replayed`: the interaction id in the parent cassette |
| `error` | TEXT NULL | Transport-level error (connection reset, timeout), or why the recording of the interaction is incomplete (e.g. records dropped because the cassette writer could not keep up). The message then contains `recording incomplete`. Recording never delays the proxy to avoid this |

**Incomplete recordings.** An interaction's recording is incomplete when its `error` contains `recording incomplete`, or when it has no `duration_ms` because recording stopped before it completed (for example, retrace was killed). The cassette then does not hold the response the agent received. Replay (§5), event derivation (§7.4) and `reindex` (§7.6) use this definition.

**`chunks`**: `(interaction_id, idx, offset_ms, data BLOB)`. Each row holds one raw SSE event as received, including its framing.

**`blobs`**: `(sha256 TEXT PK, size INTEGER, data BLOB)`. Content-addressed and deduplicated.

### 7.4 `events` (semantic layer)

Each event carries every field the future UI needs ([ADR 0009](../adr/0009-append-only-event-log.md)):

| Column | Type | Description |
|---|---|---|
| `seq` | INTEGER PK | Strictly increasing order within the session. Assigned without gaps; a missing `seq` marks a dropped event (§7.7) |
| `parent_seq` | INTEGER NULL | Causal parent (see below) |
| `interaction_id` | INTEGER NULL | Raw interaction this event was derived from |
| `kind` | TEXT | See the kinds table |
| `source`, `target` | TEXT | Participants: `user`, `agent`, `model`, `mcp:<name>` or `retrace` |
| `started_at` | TEXT | Start time |
| `duration_ms` | INTEGER NULL | Duration (null for instantaneous events) |
| `model` | TEXT NULL | Model actually used (for `model_response`) |
| `requested_model` | TEXT NULL | Model in the agent's request (differs in `swap-model`) |
| `tool` | TEXT NULL | Tool name |
| `arguments` | JSON NULL | Tool arguments, or request summary |
| `result` | JSON NULL | Tool result, model output summary, or divergence details |
| `tokens_in`, `tokens_out` | INTEGER NULL | Input and output tokens |
| `tokens_cache_read`, `tokens_cache_write` | INTEGER NULL | Prompt-cache tokens |
| `cost_usd` | REAL NULL | Estimated cost (§7.5) |
| `status` | TEXT | `ok`, `error` or `divergence` |
| `error` | TEXT NULL | Error message (upstream error, `isError` tool result, miss) |
| `provenance` | TEXT | Same values as interactions, plus reserved `fallthrough`, `nearest` and `synthetic` |
| `deriver_version` | INTEGER | Derivation logic version |

**Event kinds (MVP):**

| `kind` | source → target | Derived from | Key fields |
|---|---|---|---|
| `user_message` | `user → model` | Trailing `user` message of an LLM request that contains text blocks, emitted once per distinct message | `result.text` |
| `model_response` | `model → agent` | LLM response (one per interaction) | `model`, tokens, cost, `result.stop_reason`, `result.text` |
| `tool_use` | `model → agent` | Each `tool_use` block in a model response | `tool`, `arguments`; `parent_seq` = its `model_response` |
| `tool_call` | `agent → mcp:<name>` | MCP `tools/call` request | `tool`, `arguments`; `parent_seq` = matching `tool_use` (see below) |
| `tool_result` | `mcp:<name> → agent` | MCP `tools/call` response | `result`, `status`, `error`; `parent_seq` = its `tool_call` |
| `mcp_message` | `agent → mcp:<name>` | Other MCP methods (`initialize`, `tools/list`, …) | `arguments.method` |
| `divergence` | `retrace → agent` | Cassette miss | `result` = `{channel, upstream, received_fingerprint, diff: [{path, expected, received}], incomplete_interaction?}` (§5 step 4) |

**Deriving LLM events.** LLM events are derived from `POST /v1/messages` interactions on the LLM channel, whatever their query string. Other LLM paths produce no events.

- Events are derived from what the cassette stores, after redaction (§3.5), so that `reindex` (§7.6) derives the same events and a redacted value never reaches an event. Each event has the `provenance` of its interaction. A `divergence` event, which has no interaction, has `provenance = replayed`.
- `user_message`: its text is the message's string content, or its `text` blocks joined with `\n`. A message is identified by the request's `messages` up to and including it, so a repeated request emits no second `user_message`, while the same text sent again later in the conversation does. It starts at the interaction's `started_at` and has no duration.
- `model_response`: there is one for every interaction, also when the response is an error or never arrived. Then `status` is `error`, and `error` holds the API error (`<type>: <message>`, from the body or from an SSE `error` event) or the interaction's `error`. An interaction whose recording is incomplete (§7.3) also gives `error`. It starts at the interaction's `started_at` and lasts its `duration_ms`. `requested_model` comes from the request, and `model` and tokens from the response: `input_tokens`, `output_tokens`, `cache_read_input_tokens` and `cache_creation_input_tokens`. For SSE, usage fields in `message_delta` replace those of `message_start`. `parent_seq` is the `user_message` of the request's trailing user message, if there is one.
- `tool_use`: `arguments` is the block's `input`. A streamed input that is not valid JSON is kept as a JSON string. It happens when the response ends, and has no duration.
- Order: the events of a request are appended when the request is recorded, and those of a response when it is complete, so `seq` follows causality: an agent cannot send a request based on a response before that response's events exist. Derivation never delays forwarding.

**Linking `tool_call` to `tool_use`.** Agents often rename MCP tools when exposing them to the model (e.g. `weather__get_forecast`), so linking is best-effort. A `tool_call` is linked to the most recent unlinked `tool_use` whose canonical arguments are equal and whose tool name equals or ends with the MCP tool name. If none qualifies, `parent_seq` stays null.

### 7.5 Cost estimation

- `cost_usd` is computed from usage tokens (input, output, cache read and cache write) and a **price table embedded in the binary**, identified by `pricing_version`.
- The table holds prices per million tokens, by model id, from Anthropic's published pricing. `pricing_version` is the date the table was last checked, e.g. `2026-10-26`.
- Cache writes are priced by TTL when `usage.cache_creation` breaks them down, and at the 5-minute rate otherwise, which is the API default.
- Prices that depend on the request are applied when `usage` says so and the table has them: prompt-length tiers, fast mode (`usage.speed`) and US-only inference (`usage.inference_geo`).
- If the model is not in the table, `cost_usd` is null. It MUST NOT be guessed. The same holds when `usage` shows a price the table does not have, such as a service tier other than `standard` or a prompt beyond the table's tiers.
- The cost covers tokens only. Charges per server tool use, such as web searches, are not included.
- Cost is always labeled "estimated" in the CLI output.

### 7.6 Schema versioning

- `PRAGMA user_version` holds the schema version. retrace refuses to write to a cassette with a newer schema version, and migrates older ones forward when opening them for read.
- `retrace reindex <id>` rebuilds `events` from `interactions` with the current deriver (§9.8). Interactions are never modified.
  - It replaces the events in one transaction, so a failure leaves them as they were. It is the only operation that removes events, as [ADR 0009](../adr/0009-append-only-event-log.md) foresees.
  - Events that are not derived from interactions (`divergence`) are kept, and placed among the rebuilt events by `started_at`.
  - Interactions are taken in the order the recording handled them: requests by `id`, responses by when they completed (`started_at` plus `duration_ms`), so the rebuilt `seq` order matches the original one up to the millisecond.
  - It updates the session's `deriver_version` and `pricing_version`.
  - It refuses a session whose status is `recording`, because it may still be in progress, and a cassette whose `deriver_version` is newer than its own.
  - The rebuilt log holds every event the deriver can derive from the stored interactions, so `reindex` sets `dropped_events` to `0` (§7.7). The exception is a session with status `diverged`: its `divergence` event comes from a cassette miss, not from an interaction, so a dropped one cannot be rebuilt. There `reindex` keeps the count and warns that it may include events it has now recovered.
  - If any interaction's recording is incomplete (§7.3), `reindex` warns with their count: events derived from partial data may still be missing. This includes interactions left without a `duration_ms`, for example the request in flight when an `interrupted` session ended.

### 7.7 Gaps in the event log

The cassette writer never delays forwarding ([ADR 0009](../adr/0009-append-only-event-log.md)). When it cannot keep up, or a write fails, it drops records instead of waiting. A dropped interaction record is marked on its interaction (§7.3). A dropped event cannot be: its interaction may already be complete. So the session records the gap itself ([ADR 0010](../adr/0010-record-event-log-gaps.md)):

- `session.dropped_events` counts the events that were assigned a `seq` but not written, for any reason, before the session ended. It is written once, when the session ends, together with `status` and `ended_at`. Only `reindex` changes it afterwards (§7.6). While the status is `recording` it is `0` and says nothing: the session never ended, so its completeness is unknown.
- `seq` values are assigned without gaps, and the seq of a dropped event is never reused. Every missing `seq` below the highest one written is a dropped event, so readers can tell where each gap is. Events dropped at the end of the log leave no missing `seq`; the counter still includes them. A session's log is written by one writer, from its start to its end: a cassette is never reopened to record more, so no later writer can hand out the seq of a dropped event again.
- A session with `dropped_events > 0` has an incomplete trajectory. Readers MUST NOT present it as complete: `ls` (§9.5), `show` (§9.6) and `diff` (§8.4) mark or warn, and `reindex` (§7.6) rebuilds the missing events from the interactions.
- Only the count of missing events is stored. Writer statistics such as queue depth stay in the logs, because monitoring retrace itself is not a goal ([vision](../vision.md), §4).

## 8. Diff

### 8.1 Inputs

`retrace diff <a> <b>` compares two sessions. The typical pair is a recording and one of its replays, but any two sessions can be compared.

### 8.2 Trajectory

A session's trajectory is its events with `kind ∈ {tool_use, tool_call, tool_result, divergence}` plus a terminal **stop** marker built from the last `model_response` (`stop_reason`), ordered by `seq`. These are grouped into **steps**: step *n* is the *n*-th `model_response` together with its `tool_use` events and the resulting `tool_call` and `tool_result` events.

### 8.3 First divergence

1. Walk both trajectories step by step and event by event, comparing on **action identity** = `(kind, target, tool, canonical arguments)`. For `tool_result`, the canonical `result` is also compared.
2. Model text is **not** part of action identity by default, because different models almost always phrase things differently. `--text` includes text in the comparison. Without the flag, text differences are reported as informational.
3. The first unequal pair (or the point where one trajectory ends early) is the **divergence point**. It is reported as step number, event kind, tool, and an argument-level diff (JSON paths with values on each side).
4. If either session contains a `divergence` event (strict stop), it is reported, and the divergence point is at or before it.

### 8.4 Output

Text output (default):

```text
retrace diff 20261023-checkout  20261023-checkout.swap-model.1

             A: claude-sonnet-4-5          B: claude-haiku-4-5
steps        7                             4  (stopped: cassette miss)
tool calls   9                             5
tokens       41,230 in / 3,902 out         28,114 in / 2,207 out
cost (est.)  $0.18                         $0.04
duration     38.2s                         11.9s

✗ first divergence at step 3, event tool_use
    A  search_orders   {"customer_id": "c_812", "status": "open"}
    B  search_orders   {"customer_id": "c_812"}
       - $.status: "open" → (missing)

  steps 1–2 identical (4 tool calls)
```

The model names and values above are illustrative.

`--format json` emits a stable JSON document with the same information, for CI and for the future UI.

When either session has `dropped_events > 0` (§7.7), the text output starts with a warning that names the session and the count, and says that the divergence point may be caused by missing events. The JSON document carries `dropped_events` for each session. The exit code is unchanged.

Exit codes: `0` identical trajectories, `1` divergence found, `2` error. These follow `diff(1)`.

## 9. CLI

### 9.1 Global flags

| Flag | Env | Default | Description |
|---|---|---|---|
| `--dir` | `RETRACE_DIR` | `.retrace/cassettes` | Cassette directory |
| `--log-level` | `RETRACE_LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error` (logs go to stderr) |
| `--no-color` | `NO_COLOR` | — | Disable colored output |

Global flags MUST precede the command: `retrace [global flags] <command> [command flags] [arguments]`. Flags after the command name belong to that command, so a global flag placed there is a usage error (exit `2`). That error SHOULD say that global flags go before the command.

Each global flag's value is resolved with the precedence flag > environment variable > default. When an invalid value comes from an environment variable, the error message MUST name the variable. `NO_COLOR` follows the [no-color.org](https://no-color.org) convention: any non-empty value disables color, and no value is invalid. `--no-color` can only disable color. It never re-enables color that `NO_COLOR` disabled.

### 9.2 `retrace record`

```text
retrace record [--session <id>] [--mcp <name>=<url>]... [--listen <addr>]
               [--anthropic-upstream <url>] [--redact <jsonpath>]... [--redact-header <name>]...
               [--label <k>=<v>]... [--force] -- <agent command...>
```

- `--session`: the session id. Generated if omitted. An existing id fails unless `--force` is given.
  - A session id MUST match `^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`, because it names the cassette file (`<dir>/<id>.cassette`). Any other value is a usage error (exit `2`). Every command that takes a session id (`run`, `run --as`, `diff`, `show`, `reindex`) validates it the same way.
  - An id whose part before the first `.` is a Windows reserved device name (`CON`, `PRN`, `AUX`, `NUL`, `COM1`–`COM9`, `LPT1`–`LPT9`, in any case) is also a usage error, on every platform, so cassettes stay portable.
  - Whether an id exists is decided by the cassette file, not by comparing ids, so on a case-insensitive file system `Foo` and `foo` are the same session.
  - A generated id has the form `YYYYMMDD-HHMMSS-xxxx`: local time, then four random lowercase hexadecimal digits. If it already exists, a new random suffix is drawn.
- `--mcp`: registers an MCP upstream. Repeatable.
- `--listen`: default `127.0.0.1:4790`. Port `0` picks a free port. Non-loopback addresses require `--listen-unsafe`.

### 9.3 `retrace run`

```text
retrace run <id> [--model <model>] [--tools frozen|live] [--as <new-id>]
                 [--on-miss strict] [--realtime] [--grace <duration>]
                 [--mcp <name>=<url>]... [--listen <addr>] [-- <agent command...>]
```

The mode is derived from the flags:

| `--model` | `--tools` | Mode |
|---|---|---|
| absent | `frozen` (default) | `exact` |
| present | `frozen` | `swap-model` |
| absent | `live` | `live-tools` |
| present | `live` | **error** (exit `2`): nothing would be held constant; use `record` instead |

- `<id>`: the source session. If it does not exist, `run` exits `1`, as `reindex` does (§9.8), and creates no cassette.
- `--as`: id of the new session.
  - The default is `<id>.<mode>.<n>`, with `n` incrementing: `1` for the first replay of `<id>` in that mode, then one more than the highest `n` among the cassette files in `--dir` named `<id>.<mode>.<n>`. Only an `n` written in decimal digits, without leading zeros and below 2^63 − 1, counts. Other names are ignored.
  - As in `record` (§9.2), existence is decided by the cassette file. If another session takes the default id before the cassette is created, the next `n` is tried.
  - If the default does not fit the id format (§9.2), it is a usage error that asks for `--as`.
  - An `--as` id that already exists fails with exit `1`, as in `record` without `--force`. `run` has no `--force`.
- `-- <cmd>`: overrides the recorded command, and the agent runs in retrace's working directory. Without it, the recorded command and cwd are used. A source session with no recorded command (an empty `command`) and no `-- <cmd>` is a usage error (exit `2`), reported before the new session's cassette is created.
- `--mcp`: in `live-tools`, overrides recorded MCP URLs.
- Errors in the flags and arguments (exit `2`), including a default id that does not fit, are reported before the source session is opened. The empty-command check needs the source, so it comes after.
- A replay that hit a cassette miss under `strict` ends with status `diverged` and exits `3` (§6.2), even when the agent then exits with a non-zero code or retrace is interrupted. `agent_exit_code` still stores the agent's code (§3.1), including `128 + signal number` when retrace's own signal ended it.

### 9.4 `retrace diff`

```text
retrace diff <id-a> <id-b> [--format text|json] [--text]
```

See §8.

### 9.5 `retrace ls`

```text
retrace ls [--json]
```

Lists the cassettes in `--dir`, newest first, with these columns: `ID`, `CREATED`, `MODE`, `PARENT`, `STATUS`, `STEPS`, `TOKENS`, `COST (est.)`, `DURATION`.

When a session has `dropped_events > 0` (§7.7), its `STATUS` is followed by `+gaps` (e.g. `completed+gaps`), because its steps and totals come from an incomplete log. `--json` carries `dropped_events` for each session.

### 9.6 `retrace show`

```text
retrace show <id> [--json] [--raw <interaction-id>]
```

- The default output is the session summary (metadata and totals) followed by a timeline of events: `seq`, relative time, `source → target`, kind, tool, arguments (truncated), status.
- When `dropped_events > 0` (§7.7), the summary warns that the event log is incomplete and gives the count, and the timeline shows a marker line where `seq` values are missing, with how many events are missing there. `--json` emits only the events that exist; consumers find gaps from `seq`.
- `--json`: emits events as JSONL, one event per line, as a stable format.
- `--raw`: dumps one interaction (headers, bodies, SSE chunks with offsets).

### 9.7 Exit codes

| Code | Meaning |
|---|---|
| `0` | Success (for `diff`: trajectories identical) |
| `1` | Runtime error (for `diff`: divergence found) |
| `2` | Usage error, or unsupported option (reserved strategies, `--model` with `--tools live`) |
| `3` | `run`: replay stopped on a divergence (`strict`) |
| `4` | `record` / `run`: the agent exited with a non-zero code. Its code is stored in `agent_exit_code` |

### 9.8 `retrace reindex`

```text
retrace reindex <id>
```

Rebuilds the events of session `<id>` (§7.6) and prints how many there are, with the deriver and pricing versions used. It exits `0` on success, `2` for an invalid id, and `1` when the session does not exist or cannot be reindexed.

## 10. Security and privacy

- Auth headers are redacted before storage, and body redaction is opt-in ([ADR 0008](../adr/0008-secret-redaction.md)).
- The proxy binds to loopback by default.
- In `exact` mode no outbound connection is made.
- Cassettes contain prompts and tool outputs verbatim. The README and `record --help` MUST say so.

## 11. Acceptance criteria

Each criterion is verified by an automated test unless marked *(manual)*. "Fake upstream" means an in-process test server that emulates the Anthropic API or an MCP server.

### Recording

- **AC-01** `record` launches the agent with `ANTHROPIC_BASE_URL` and `RETRACE_MCP_<NAME>_URL` set, and the session ends when the agent exits.
- **AC-02** A non-streaming `/v1/messages` exchange is recorded with request, response, status, headers (redacted) and timing.
- **AC-03** A streaming `/v1/messages` exchange is recorded with every SSE event and its offset. The agent receives the first chunk before the upstream finishes (no buffering).
- **AC-04** MCP `initialize`, `tools/list` and `tools/call` exchanges over Streamable HTTP are recorded, for both JSON and SSE responses, against two distinct MCP servers in the same session.
- **AC-05** Events are derived with all §7.4 fields populated where applicable, including tokens and estimated cost for each `model_response`, and correct `source`/`target`.
- **AC-06** `authorization`, `x-api-key` and `*-token` header values never appear in the cassette file (verified by scanning the raw file bytes).
- **AC-07** Killing retrace with `SIGINT` mid-session leaves a readable cassette with status `interrupted`.
- **AC-24** `record --session` with an invalid id (for example `../x`, `.hidden`, `a/b`, `NUL` or 129 characters) exits `2` and creates no file.
- **AC-25** `record --redact '$..email'` stores no `email` value from request bodies, response bodies or SSE events (verified by scanning the raw file bytes), while the agent and the upstream receive the original bodies.
- **AC-26** When the cassette writer drops events during `record` (forced with failing event writes or a stalled writer), the ended session has `dropped_events` equal to the number of dropped events, including events dropped at the end of the log, the missing `seq` values are exactly those of the other dropped events, and `reindex` restores the events of complete interactions and sets `dropped_events` to `0`.

### Exact replay

- **AC-08** `run <id>` (exact) replays a recorded session with all upstreams unreachable. The agent's stdout matches the original run's stdout.
- **AC-09** In exact mode the proxy opens zero outbound connections (enforced by a dialer that fails the test if called).
- **AC-10** Streaming responses are replayed byte-identically. With `--realtime`, chunk offsets are within ±20 ms of the recorded ones.
- **AC-11** JSON-RPC ids in replayed MCP responses match the incoming request ids, and `Mcp-Session-Id` is freshly issued.
- **AC-12** Identical requests repeated N times are served the N recorded responses in order (FIFO per fingerprint).
- **AC-13** Requests arriving in a different order than recorded (parallel tool calls) replay without a divergence.
- **AC-28** In exact mode, an LLM request that cannot be served from its queue (the queue is empty, or its head is an interaction whose recording is incomplete) gets HTTP `422` with error type `retrace_cassette_miss`, appends a `divergence` event (with `incomplete_interaction` set in the second case), and the replay ends with status `diverged` and exit `3`, also when the agent then exits with a non-zero code or retrace receives a signal, and `agent_exit_code` keeps the agent's code. The incomplete interaction is not reported as unconsumed.
- **AC-29** In exact mode, an LLM request whose queue head is an interaction recorded with a transport error and a complete recording gets HTTP `502` with a body starting `retrace: upstream request failed:`. The interaction is consumed, no `divergence` event is appended, the replay does not end `diverged` on its account, and the new interaction is stored with `provenance = replayed`, no status and the source's `error`, deriving the same `model_response` as the source.
- **AC-30** `run` with a source session that does not exist, or with an `--as` id that already exists, exits `1`; `run` of a source with no recorded command and no `-- <cmd>` exits `2`. None of them creates a new cassette.

### Swap-model and strict

- **AC-14** `run <id> --model M` forwards LLM requests with `model` rewritten to `M` and serves MCP from the cassette. The new cassette records `requested_model` and `model`.
- **AC-15** When the new model calls a tool with arguments not in the cassette, retrace records a `divergence` event with a JSON-path diff, answers with JSON-RPC error `-32099`, terminates the agent and exits `3`.
- **AC-16** `--on-miss nearest-match` (and the other reserved values) exits `2` with a "not implemented" message.
- **AC-17** `--model` combined with `--tools live` exits `2`.

### Live-tools

- **AC-18** `run <id> --tools live` serves LLM responses from the cassette and forwards MCP to the real servers. A changed tool result produces a divergence on the next LLM request.

### Diff and inspection

- **AC-19** `diff` of a session with its exact replay reports identical trajectories and exits `0`.
- **AC-20** `diff` of a session with a diverging swap-model replay reports the correct step, event, tool and argument diff, and exits `1`.
- **AC-21** `diff --format json` output validates against a JSON schema committed in the repo.
- **AC-22** `ls` and `show` (text and `--json`) display the fields listed in §9.5 and §9.6.
- **AC-27** For a session with `dropped_events > 0`, `ls` shows `+gaps` after its status and `dropped_events` in `--json`, `show` warns with the count and marks each gap in its timeline, and `diff` warns in text output and reports `dropped_events` in `--format json`.

### Demo (manual)

- **AC-23** The repo contains a demo agent that uses two MCP servers. The README GIF shows record → exact replay → swap-model with divergence → diff.

## 12. Out of scope / future work

- Web UI: sequence view with animated replay, tool topology, model diff. The cassette model (§7) is designed for it.
- Divergence strategies `fallthrough`, `nearest-match` and `llm-stub` (§6.3).
- Providers: OpenAI-compatible APIs, Bedrock, Vertex.
- MCP stdio transport (wrapper process), replay of server-initiated messages (`GET` stream, sampling, elicitation).
- User-defined matching ignore rules (`--ignore <jsonpath>`).
- `retrace proxy` (proxy without a child process), `retrace export` (JSONL), `retrace scan` (secret detection), OpenTelemetry export.
- CI mode: replay a corpus of cassettes against a candidate model and summarize divergences.
- A `retrace.yaml` configuration file for MCP servers, the listen address, redaction rules and the Anthropic upstream. Reading YAML needs a new dependency and an ADR. In the MVP, configuration is given with flags.
