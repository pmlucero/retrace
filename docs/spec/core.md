# retrace core specification

- Status: Approved
- Version: 0.26
- Last updated: 2026-11-12

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
- Any other path under `/llm/anthropic/` is forwarded and recorded as an opaque interaction, unless it is rejected (§3.6).

### 3.4 MCP channel

- All methods on `/mcp/<name>` (`POST`, `GET`, `DELETE`) are forwarded and recorded ([ADR 0004](../adr/0004-mcp-streamable-http-first.md)).
- A request for `/mcp/<name>` is forwarded to exactly the URL registered for `<name>`, and a request for `/mcp/<name>/<rest>` to that URL's path followed by `/<rest>`. retrace MUST NOT add a trailing slash, because many servers treat `/mcp` and `/mcp/` as different endpoints. For example, with `--mcp orders=http://127.0.0.1:9001/mcp`, `/mcp/orders` is forwarded to `http://127.0.0.1:9001/mcp` and `/mcp/orders/x` to `http://127.0.0.1:9001/mcp/x`. A registered URL with no path gets `/`.
  - The registered URL's path and `/<rest>` are joined with a single `/`, so `http://127.0.0.1:9001/mcp/` and `/mcp/orders/x` give `/mcp/x`. The request's query string is forwarded, after any query in the registered URL.
  - The forwarding rule does not change the stored `path` (§7.2), which is `/` for a request for `/mcp/<name>` or `/mcp/<name>/`.
  - A `<rest>` with a dot segment is rejected (§3.6), so a request cannot leave the registered path.
- A `POST` body MAY be a single JSON-RPC message or a batch. Each JSON-RPC request in it is matched to its response, whether the response comes as JSON or inside an SSE stream.
- The protocol version and server info from `initialize` are stored in the session metadata (§7.2).
  - They are read from the recorded exchanges when the session ends. A cassette left by a retrace that was killed has the servers without them. `reindex` never changes `mcp_servers`.
  - A server's values come from its exchange with the highest interaction `id` among those whose recording is complete (§7.3) and whose `initialize` result has a non-empty string `protocolVersion`. Other results are ignored.

### 3.5 Redaction

Header and body redaction are applied before anything is written to disk, as specified in [ADR 0011](../adr/0011-secret-redaction-defaults.md).

- A header is redacted when its name, compared case-insensitively, is on the deny-list: `authorization`, `proxy-authorization`, `x-api-key`, `cookie`, `set-cookie`, `apikey`, `auth`, `key`, `password`, `secret`, `sig`, `signature`, `token`, any name matching `*-token`, `*-secret`, `*-key`, `*-auth`, `*-password`, `*-signature`, `*-credential` or `*apikey`, and any name given with `--redact-header`. Its value becomes `[REDACTED:<sha256-prefix-8>]`.

Credentials can also travel in URLs, so the same rule covers every URL retrace stores: the registered MCP server URLs, each interaction's `path`, and the values of the URL-carrying headers `Location`, `Content-Location`, `Referer` and `Link` (each URI reference inside `<…>`), in request and response headers, in record and in every replay mode. Only what is stored is redacted: a forwarded request or response keeps its original URLs.

- Userinfo (`https://user:pass@host`) is replaced with `REDACTED`.
- A query parameter is redacted when its name, lower-cased and with `_` read as `-`, is on the header deny-list. Its value becomes `[REDACTED:<sha256-prefix-8>]`, and the rest of the URL is kept as it was. So `X-Amz-Signature`, `X-Amz-Credential` and `access_token` are redacted.
- A URL-carrying header that is itself on the deny-list is masked whole, as any other redacted header.

**Body redaction** (`--redact <jsonpath>`, repeatable) is opt-in.

- Each rule is a JSONPath in this subset: it starts with `$`, followed by any of `.name`, `['name']`, `[n]` (array index), `.*` or `[*]` (every member or element), and `..name` (`name` at any depth). A dotted name has letters, digits, `_` and `-`; any other name is written `['name']`. Any other syntax is a usage error (exit `2`).
- Rules apply to every JSON body retrace stores, on both channels: request bodies, non-streamed response bodies, and the `data` of each SSE event. A body that is not JSON is stored as it is.
- Each matched value, whatever its type, is replaced by the string `[REDACTED:<sha256-prefix-8>]`. A string is hashed as it is, so it gets the same marker as the same value in a header. Any other value is hashed over its JSON text. A value selected by several rules, or nested inside another selected value, is masked once, over its original content.
- A body with no match is stored byte for byte. A body with a match is re-encoded compactly, keeping member order and number formats. In an SSE event, a `data` field split over several lines is written back as one `data:` line when something in it is masked. The other lines and their endings are kept.
- When body rules are set, a body nested more than 1000 levels deep cannot be redacted safely, so it is not stored, and the interaction's `error` says why. The same applies to an SSE event that cannot be redacted (the rest of the stream is not stored either), and to a body or SSE event larger than the proxy's in-memory limit for one body or event (64 MiB). In each case the interaction's recording is incomplete (§7.3), so replaying it is a cassette miss (§5).
- Redaction runs on the forwarding path. It parses each stored JSON body once, which takes milliseconds for a megabyte, against model latencies of hundreds of milliseconds.
- The agent and the upstream always receive the original bodies. Only what is written to the cassette is redacted.
- The rules are stored in the session's `redaction` metadata (§7.2). M3 applies them to incoming requests before fingerprinting them (§5).

### 3.6 Rejected requests

Some requests are answered by retrace itself, before they are forwarded or matched. These rules apply in record and in every replay mode, on both routes.

- A request whose path has a `.` or `..` segment, anywhere in it, or an invalid percent-encoding (such as `%zz`) is answered with HTTP `400`. The check reads the whole request path as the agent sent it, before routing and before any path cleaning or redirect by the HTTP router, so the answer is `400` in every mode, never a redirect. The path is percent-decoded once and then split on `/` and `\`, so `%2e%2e`, `..%2f` and `..\` count too. The route is a boundary: without this check, `/mcp/orders/../admin` would reach a path on the MCP server's host outside the registered URL.
  - A request line with an invalid percent-encoding can be refused by the HTTP server with `400` before retrace's handler sees it. The body and logging rules below then do not apply, and the request is still neither forwarded nor recorded.
- A request whose body is larger than 64 MiB (2^26 bytes) is answered with HTTP `413`. A `Content-Length` above the limit is rejected before the body is read, and a body without one is read only up to the limit, so a local client cannot make retrace hold an unbounded body in memory. 64 MiB is the proxy's limit for one stored response body or SSE event (§3.5), and is well above what the Anthropic Messages API accepts in one request. It is not configurable in the MVP.
- The response body says why the request was rejected, starting with `retrace:`. On the LLM route it has the Anthropic API's error shape, `{"type":"error","error":{"type":...,"message":...}}`, with type `invalid_request_error` for `400` and `request_too_large` for `413`, so SDK clients show a readable error. On the MCP route and outside both routes it is plain text. The route of a rejected request, for its body and its warning, is the prefix of the raw path as the agent sent it, so `/mcp/../llm/anthropic/v1/messages` is on the MCP route.
- A rejected request is not forwarded and not recorded. retrace logs a warning with the route, if any, and the reason, but not the request's path or query, which may carry a credential that is never redacted because it is not recorded. The agent gets the same answer in every mode, so the recording and its replays stay consistent without storing it. It is not a cassette miss.

## 4. Replay modes

`retrace run <id>` re-launches the agent (the command given after `--`, or the recorded command and working directory with `--recorded-command`, §9.3) against a proxy that serves the channels as follows:

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
  - The rewrite applies to `POST` requests to `/v1/messages` and `/v1/messages/count_tokens`, the endpoints whose body names the model. It sets the top-level `model` member of the request body, and adds it when it is missing. The other members keep their bytes, as body redaction does (§3.5). A body that is not a JSON object is forwarded unchanged, so the API answers it with its own error. Requests to other paths are forwarded unchanged.
  - The upstream is `--anthropic-upstream` (§9.3), with the same default as in `record`.
  - Forwarding follows record mode (§3.3): streaming is not buffered (AC-03), and the interaction is recorded with `provenance = live`.
- MCP requests are answered from the cassette via matching. A miss triggers the divergence strategy (§6).
- The resulting cassette records both the original model requested by the agent and the model actually used (§7.2, §7.3).
  - The new cassette stores each LLM request as the agent sent it, with its original `model`. The rewrite applies only to what is forwarded. So the `model_response` event's `requested_model` comes from the agent's request, and `model` comes from the response (§7.4).
  - The session's `model_override` holds the value of `--model`.

### 4.3 `live-tools`

- LLM requests are answered from the cassette via matching.
- MCP requests are forwarded to the real servers: the recorded URLs, unless overridden with `--mcp`.
  - Each server is reached at its `--mcp` URL when one is given, and otherwise at the `url` stored in the source's `mcp_servers` (§7.2).
  - A stored URL that holds a redaction marker (§3.5) is not the URL that was recorded, so that server needs `--mcp`. Without it, `run` fails with a usage error (exit `2`) that names the server. A marker is userinfo equal to `REDACTED`, or a query value of the form `[REDACTED:<8 hex digits>]`. A real URL that happens to have that form also needs `--mcp`.
  - Every MCP request is forwarded as in record mode (§3.4), including `GET` streams, `DELETE` and messages that hold only notifications or responses. §5 step 3 does not apply: request ids and `Mcp-Session-Id` headers pass through unchanged, and the interactions are recorded with `provenance = live`.
- If a live tool returns a different result, the next LLM request's `messages` differ and its fingerprint misses. That is a divergence, handled by the same strategy (§6). No separate tool-result comparison is needed.

### 4.4 Common replay behavior

- A replay produces a new session with `parent_session_id` set to the source session and `mode` set accordingly.
- Requests are checked as in record mode before they are matched or forwarded (§3.6).
- Interactions answered from the cassette are stored in the new cassette with `provenance = replayed` and a reference to the source interaction. MCP requests answered without matching (§5 step 3) are also `replayed`, with no source interaction. Forwarded ones get `provenance = live`.
- The new session uses the source session's redaction rules (`redaction`, §7.2): its body rules and header names, plus the current default deny-list (§3.5) and any `--redact-header` names (§9.3). It stores those combined rules, so a replay of a cassette written with older defaults redacts at least as much as its source. They apply to everything the replay stores, forwarded interactions included, as in record mode (§3.5).
- At the end of a replay, recorded interactions that were never consumed are reported as a warning (count and first few), not as an error. Only channels answered from the cassette count: the LLM in `exact` and `live-tools`, and MCP in `exact` and `swap-model`. MCP interactions that replay answers without matching (§5 step 3) are never reported as unconsumed.
- A response answered from the cassette, in any mode and on either channel, carries the recorded status and headers, except hop-by-hop headers (RFC 9110 §7.6.1, plus any listed in `Connection`) and `Content-Length`. A non-streamed body gets the `Content-Length` of the body as served, which may differ from the original because of redaction and, for MCP, the id rewrite of §5 step 3. An interaction recorded with a transport error has neither status nor headers, and is served as in §5 step 2.

## 5. Request matching

Matching follows [ADR 0007](../adr/0007-request-matching-fingerprints.md). In summary:

1. Incoming requests on a matched channel are canonicalized and fingerprinted.
   - **LLM**: the body without `metadata`.
   - **MCP**: `server + method + params` without JSON-RPC `id` and without `params._meta`. A batch is fingerprinted as one unit: the server name and the list of its messages, each canonicalized the same way, in their order. A batch of one message and the bare message get different fingerprints, because their responses have different shapes. A batch the agent composes differently from the recording (for example, a notification sent in it alongside a request) is therefore a miss. This refines point 1 of ADR 0007, which predates batches.
     - Each message in the canonical form carries its kind (request, notification, response or invalid), so a notification never matches a request with the same method and params.
     - `_meta` is removed only from `params` itself. A `_meta` nested deeper (e.g. in `params.arguments`) or inside a client response's `result` is kept.
     - A response from the client contributes its `result` or `error`, without its id. A batch element that is not a JSON-RPC message contributes the whole element.
     - A body that is valid JSON but is neither a JSON-RPC message nor a non-empty batch is canonicalized as a whole, together with the server name.

   Body redaction rules (§3.5) are applied first. A body that is not valid JSON, or that is nested more than 1000 levels deep, is fingerprinted over its raw bytes (for MCP, together with the server name). This refines point 1 of ADR 0007. A request whose body cannot be redacted (§3.5) cannot be matched: it is a cassette miss, and its divergence event has a null `received_fingerprint`, so no hash of unredacted data reaches the cassette.
2. Each fingerprint has a FIFO queue of recorded interactions. A request consumes the head of its queue, and an empty queue is a cassette miss.
   On either channel, an interaction recorded with a transport error (no status, §7.3) whose recording is complete is served as the answer the agent got while recording: HTTP `502`, with the headers the proxy sends for a transport error and a body that starts with `retrace: upstream request failed:` and includes the interaction's `error`. Only the prefix is fixed, because the stored `error` may be reworded, e.g. when the agent had already closed the connection. This reproduces what the agent saw, so it is not a fabricated response. The new interaction is stored with `provenance = replayed`, no status and the source interaction's `error`, so it derives the same events as the source (§7.4).
   When an interaction's recording is incomplete (§7.3), replay never fabricates its response silently ([ADR 0010](../adr/0010-record-event-log-gaps.md)). Such an interaction keeps its place in its queue but cannot be served: a request that reaches it is a cassette miss, and it stays at the head, so the divergence shows up at the step where it happened. It is not reported as unconsumed (§4.4). This refines point 2 of ADR 0007, which predates incomplete recordings.
3. Replayed MCP responses get their JSON-RPC `id` rewritten to the incoming request's id. In a batch, or in an SSE response, each response gets the id of the request it answers, and the rest of each message is kept as recorded.
   - A replayed `initialize` response that carried an `Mcp-Session-Id` gets a fresh one, one per replayed `initialize`. One recorded without it gets none, so the agent never sees a session the recorded server did not open. This refines ADR 0004, which assumes every recorded server opens a session. Every other replayed response that carried an `Mcp-Session-Id` echoes the incoming request's `Mcp-Session-Id` when retrace issued it, and drops the header otherwise. The id therefore follows the agent's session, whichever recorded interaction serves the request. Incoming `Mcp-Session-Id` headers are not checked, so requests are still served after a replayed `DELETE`.
   - Some requests are answered without matching, and consume no recorded interaction: a `POST` that holds only notifications or responses from the client gets `202`, `GET` gets `405` with `Allow: POST, DELETE`, and `DELETE` gets `200`. They are stored in the new cassette with `provenance = replayed` and no source interaction, so the replay's log shows what the agent sent. Server-initiated messages recorded on a `GET` stream are therefore not replayed ([ADR 0004](../adr/0004-mcp-streamable-http-first.md)): an agent that reacts to them can behave differently in replay without a divergence event.
   - A miss on the MCP channel is answered with HTTP `200`, `application/json` and a JSON-RPC error `-32099` with message `retrace: cassette miss` (§6.2 step 2) and a `data` string with the details, which says so when the matching recording is incomplete (step 4). It has the incoming request's id (`null` when it cannot be read, as JSON-RPC 2.0 §5 says), or one error per request in a batch.
4. On a miss, the replayer computes a structural diff against the next unconsumed recorded interaction on the same channel and server. The diff is a list of JSON paths with expected and received values, and it is attached to the divergence event.
   - The next unconsumed interaction is the one with the lowest `id` among the source interactions on that channel and server that are queued by fingerprint (step 2) and that no request has consumed yet. MCP interactions answered without matching (step 3) are never candidates. An interaction whose recording is incomplete can be one, because its request is stored. When there is no candidate, the diff is empty.
   - The diff compares the canonical forms (step 1) of the recorded request and the received one. Both are taken after redaction, so a redacted value never reaches the diff. Objects are compared member by member, and arrays element by element, by index. An entry is emitted at the deepest path where the two sides differ: a different scalar, a different type, or a member or element present on only one side.
   - Paths use the JSONPath subset of §3.5 (`$.messages[3].content`, `$['a name']`). For the LLM, they are relative to the canonical body. For MCP, they leave out the server name, which both sides share: they are relative to the canonical message (its kind, and its method and params or its result), to the array of canonical messages of a batch, or to the canonical body of a JSON body that is neither (step 1). So a changed tool argument is at `$.params.arguments.<name>`, a different kind at `$.kind`, and a batch received where a single message was recorded at `$`.
   - `expected` and `received` hold the JSON values at that path. A side where the path does not exist omits its member. A value whose compact JSON is longer than 1 KiB is replaced by the string `[retrace: <n> bytes elided]`, and the event's result sets `diff_truncated`.
   - A canonical form that is not JSON (a body fingerprinted over its raw bytes) is compared as a whole: one entry at `$`, with each side as a JSON string, invalid UTF-8 replaced by U+FFFD. No entry is emitted when the two are equal.
   - The diff is ordered as the walk meets each path (in an object, the recorded side's members in their order, then the members only the received side has, in theirs; in an array, by index), and holds at most 50 entries. When more differ, or a value is elided, the divergence event's result sets `diff_truncated` to `true`. It is absent otherwise.
   - A request that cannot be redacted (step 1) gets an empty diff, so no unredacted value reaches the cassette.

   When the request reaches an interaction whose recording is incomplete, no diff is computed. The divergence event sets `incomplete_interaction` to that interaction's id, and the error the agent receives (its message for the LLM, its `data` for MCP, step 3) says the matching recording is incomplete, so the user knows to record the session again rather than debug the agent.

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

1. Append a `divergence` event (§7.4) with the miss details.
2. Answer the agent: a JSON-RPC error `-32099 "retrace: cassette miss"` for MCP, or HTTP `422` with `{"type":"error","error":{"type":"retrace_cassette_miss","message":...}}` for the LLM API.
3. Send `SIGTERM` to the agent, then `SIGKILL` after `--grace` (default `5s`).
4. Finalize the cassette with status `diverged`. Print the divergence report (§6.4) and exit `3`.

Steps 1 and 2 apply to every miss. Step 3 starts at the first miss and happens once:

- Requests that arrive during the grace period are still answered: from the cassette when they match, and with steps 1 and 2 when they miss.
- An agent that exits within the grace period is not killed.
- With `--grace 0`, the agent is killed at once. On Windows, which has no `SIGTERM`, the agent is also killed at once.
- As in §3.1, only the agent process is signalled. Processes it started itself may outlive it.
- A signal that retrace receives during the grace period is handled as in §3.1. The session still ends `diverged` and retrace exits `3` (§9.3).

### 6.3 Reserved strategies

`fallthrough`, `nearest-match` and `llm-stub` are accepted by the flag parser and rejected with `not implemented in this version` (exit `2`). Any response they produce in the future MUST set `provenance` to `fallthrough`, `nearest` or `synthetic` respectively.

### 6.4 Divergence report

When a replay ends with status `diverged`, retrace prints a short report on stderr, after the cassette is finalized. It describes the first miss the replayer saw, from the same data as that miss's `divergence` event, so it shows no unredacted value. It does not read the events back, so a `divergence` event that the cassette writer dropped (§7.7) does not change it. When the session has `dropped_events > 0`, the report also says that the event log is incomplete. The full comparison of the two sessions is `retrace diff` (§8).

```text
retrace: replay 20261023-checkout.swap-model.1 diverged from 20261023-checkout
  first miss: mcp:orders tools/call search_orders
    $.params.arguments.status: "open" → (missing)
  misses: 2
  compare: retrace diff 20261023-checkout 20261023-checkout.swap-model.1
```

- The first-miss line names the upstream and the request: for MCP, the JSON-RPC method, plus the tool name for `tools/call` (`batch` for a batch); for the LLM, the HTTP method and path.
- Each diff entry (§5 step 4) gives its path and `expected → received`. Values are compact JSON. A missing side is `(missing)`, and a value longer than 80 characters is cut and ends with `…`. A truncated diff ends with a line that says more paths differ.
- Text from the cassette is escaped as in §10.
- When there is no diff, the report says why: the matching recording is incomplete (record the session again), the request could not be redacted, no recorded request was left to compare against, or the two canonical forms are equal.
- `misses` counts the misses of the session.
- The layout above is illustrative. Its content is normative.

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
| `mcp_servers` | JSON | `[{name, url, protocol_version, server_info}]`. `protocol_version` and `server_info` are absent, not `null`, when no `initialize` exchange qualifies (§3.4). `server_info` is the `serverInfo` object of the recorded result (after redaction, §3.5), and is also absent when the chosen result has no `serverInfo` object |
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
| `source_interaction_id` | INTEGER NULL | For `replayed`: the interaction id in the parent cassette. Null for MCP requests answered without matching (§5 step 3) |
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
| `divergence` | `retrace → agent` | Cassette miss | `result` = `{channel, upstream, received_fingerprint, diff: [{path, expected?, received?}], diff_truncated?, incomplete_interaction?}` (§5 step 4) |

**Deriving LLM events.** LLM events are derived from `POST /v1/messages` interactions on the LLM channel, whatever their query string. Other LLM paths produce no events.

- Events are derived from what the cassette stores, after redaction (§3.5), so that `reindex` (§7.6) derives the same events and a redacted value never reaches an event. Each event has the `provenance` of its interaction. A `divergence` event, which has no interaction, has `provenance = replayed`.
- `user_message`: its text is the message's string content, or its `text` blocks joined with `\n`. A message is identified by the request's `messages` up to and including it, so a repeated request emits no second `user_message`, while the same text sent again later in the conversation does. It starts at the interaction's `started_at` and has no duration.
- `model_response`: there is one for every interaction, also when the response is an error or never arrived. Then `status` is `error`, and `error` holds the API error (`<type>: <message>`, from the body or from an SSE `error` event) or the interaction's `error`. An interaction whose recording is incomplete (§7.3) also gives `error`. It starts at the interaction's `started_at` and lasts its `duration_ms`. `requested_model` comes from the request, and `model` and tokens from the response: `input_tokens`, `output_tokens`, `cache_read_input_tokens` and `cache_creation_input_tokens`. For SSE, usage fields in `message_delta` replace those of `message_start`. `parent_seq` is the `user_message` of the request's trailing user message, if there is one.
- `tool_use`: `arguments` is the block's `input`. A streamed input that is not valid JSON is kept as a JSON string. It happens when the response ends, and has no duration.
- Order: the events of a request are appended when the request is recorded, and those of a response when it is complete, so `seq` follows causality: an agent cannot send a request based on a response before that response's events exist. Derivation never delays forwarding.

**Deriving MCP events.** MCP events are derived from `POST` interactions on the MCP channel whose body is JSON-RPC. `GET`, `DELETE`, bodies that are not JSON-RPC and invalid batch elements produce no events. A batch gives one event per message.

- `tool_call` and `mcp_message` are request events: they start at the interaction's `started_at` and have no duration. `mcp_message` has `status = ok` and only `arguments.method`. Client notifications also give an `mcp_message`. Client responses give no event, and neither do responses to methods other than `tools/call`, so a failing `initialize` shows only in the raw layer.
- `tool_result` also sets `tool`. It starts at the interaction's `started_at` and lasts its `duration_ms`, also inside an SSE batch. Its `status` is `error`, and `error` is set, in this order of precedence:
  - a JSON-RPC error gives `<code>: <message>`;
  - an incomplete recording (§7.3) gives the interaction's `error`, keeping a stored `result`;
  - `isError: true` gives the `text` blocks of its content joined with `\n`, or `the tool reported an error` when it has none;
  - a missing response gives the interaction's `error`, or a fixed message saying why there is none (no response, an HTTP error status, a body that is not JSON-RPC, a stream that ended first, a body that was not recorded, or no message with the request's id).
- A transport error recorded after the response arrived does not fail the `tool_result`: MCP clients often close an SSE stream as soon as they have their answer.
- Messages that the server sends inside a response (progress, logging, server requests), and responses that answer no request, produce no events in the MVP.
- Redacted values are kept as stored. `params` that are not an object give a `tool_call` with null `tool` and `arguments`, and a `result` that is not an object is `ok` with that value.

**Linking `tool_call` to `tool_use`.** Agents often rename MCP tools when exposing them to the model (e.g. `weather__get_forecast`), so linking is best-effort. A `tool_call` is linked to the most recent unlinked `tool_use` whose canonical arguments are equal and whose tool name equals or ends with the MCP tool name. If none qualifies, `parent_seq` stays null.

- "Ends with" is a plain, case-sensitive suffix. An empty MCP tool name links to nothing.
- Arguments are compared in their canonical form (ADR 0007): keys sorted, number text kept. Absent MCP `arguments` compare as `{}`. A `tool_use` input kept as a JSON string (§7.4) is compared as that string. Arguments on either side that cannot be decoded (nested more than 1000 levels deep) are never linked.
- Each `tool_use` is linked at most once. Only the 128 most recent unlinked `tool_use` events are considered, so derivation keeps bounded state.
- A `tool_call` appended before the `tool_use` it answers (when the agent acts on a streamed response before its end is recorded, see Order) stays unlinked, because events are append-only.

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
- A cassette is untrusted input, and SQL stored in its schema runs when the file is read or written: a recursive view named `events`, for example, would hang `ls`. So before reading or migrating a cassette, retrace checks its schema objects (`sqlite_schema`):
  - `session`, `interactions`, `events`, `blobs` and `chunks` MUST be ordinary tables, not views or virtual tables.
  - None of their columns may be generated (`PRAGMA table_xinfo` reports them), because reading a generated column evaluates its expression, and `trusted_schema(0)` does not stop innocuous functions such as `printf`.
  - The file MUST NOT hold any view, any virtual table, or any trigger other than those retrace's migrations create, compared by name and SQL text.
  - Other tables and indexes are allowed. Reading never evaluates them: an index runs SQL only when its table is written, and `trusted_schema(0)` limits the functions it can call to those SQLite marks innocuous. A crafted index can at worst slow down a write to the cassette, such as `reindex`, or make it fail (a `UNIQUE` index), which leaves the cassette as it was because the write is one transaction.
  - The check applies to a cassette with a newer schema version too, against the objects of retrace's own schema. When such a cassette fails it, the error says that its schema is newer than this retrace, since a later migration may have added the object.
  - A file that fails it is a cassette that cannot be read: `ls` skips it with a warning (§9.5), and the other commands fail as they do for a corrupt cassette.
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

`retrace diff <a> <b>` compares two sessions. The typical pair is a recording and one of its replays, but any two sessions can be compared. A session that is still `recording`, or never ended, is compared as it is, and the output says that it never ended.

### 8.2 Trajectory

A session's trajectory is its events with `kind ∈ {tool_use, tool_call, tool_result, divergence}` plus a terminal **stop** marker built from the last `model_response` (`stop_reason`). These are grouped into **steps**: step *n* is the *n*-th `model_response` together with its `tool_use` events and the resulting `tool_call` and `tool_result` events.

- One order defines the steps: the order in which their LLM requests arrived, which is the order of the `model_response` events by `interaction_id` (§7.3: assigned when the request arrives). `seq` is not used for it, because it is not deterministic here. A `model_response` is appended when its response completes, so concurrent LLM requests can complete in a different order in a replay. An agent that acts on a streamed response as soon as it arrives can also have its `tool_call` appended before that response's `model_response` (§7.4, Order). Linking a `tool_call` to its `tool_use` is best-effort too (§7.4). Step *n* is the *n*-th `model_response` in this order, and the last one in this order gives the stop marker.
- Each event is assigned to a step by causality:
  - A `tool_use` belongs to the step of its `model_response` (its `parent_seq`).
  - A `tool_call` belongs to the step of the last `model_response` whose `interaction_id` is lower than its own: a request cannot act on a response whose request had not arrived yet. Interaction ids are unique, so there is no tie, and no clock is involved.
  - A `divergence` has no interaction. It belongs to the step of the last `model_response` whose `started_at` is at or before its own, and a tie gives the later step.
  - A `tool_result` belongs to the step of its `tool_call` (its `parent_seq`), or, without one, is placed by its `interaction_id` like a `tool_call`.
  - Events before the first `model_response` form step `0`, which exists only when it holds an event.
- The stop marker holds the `stop_reason` of the last `model_response`, or its `error` when its `status` is `error`. A session with no `model_response` has no stop marker.
- The `model_response` events themselves are not compared, except for their text under `--text` (§8.3). They define the steps and the totals.

**Totals.** `diff` (§8.4), `ls` (§9.5) and `show` (§9.6) summarize a session from its events and its session row:

- steps: the number of `model_response` events. Step `0`, which has no `model_response`, is not counted. Tool calls: the number of `tool_call` events.
- tokens: the sums of `tokens_in` and `tokens_out` over `model_response` events.
- cost: the sum of the known `cost_usd`, with the number of `model_response` events that have tokens but a null cost. A null cost is never guessed (§7.5): the output shows the known sum and that some responses are unpriced, e.g. `$0.18 + 2 unpriced`. A response with no tokens, such as an API error or a cassette miss, was not billed by tokens and is not counted as unpriced. A stored `cost_usd` that is less than zero or not a finite number, which a valid cassette never holds, counts as null. So does a cost that would make the sum not finite, adding in `seq` order, so that the known sum is always a finite number.
- models: the distinct `model` values of `model_response` events, in order of first appearance.
- duration: `ended_at − created_at`, absent while the session has not ended.

### 8.3 First divergence

1. Walk both trajectories step by step and event by event, comparing on **action identity** = `(kind, source, target, tool, canonical arguments)`. For `tool_result`, the canonical `result` and the `status` are also compared. Stop markers are equal when their `stop_reason`, or their `error`, is.
   - Within a step, `tool_use` events are compared in `seq` order, which is the order the model emitted them in. `tool_call` events are compared as a set, and so are `tool_result` events: the agent may run tool calls concurrently, so their order depends on which finishes first, and an exact replay answers at once. Each set is ordered by its full identity, `(target or source, tool, canonical arguments or result, status)`, before it is compared, so that no tie depends on `seq`.
   - The order of comparison within a step is: `tool_use`, then `tool_call`, then `tool_result`, then `divergence`.
2. Model text is **not** part of action identity by default, because different models almost always phrase things differently. `--text` includes text in the comparison: the `result.text` of each step's `model_response`, compared before its other events. Without the flag, text differences are reported as informational: the steps whose text differs are listed, and the exit code does not change.
3. The first unequal pair (or the point where one trajectory ends early) is the **divergence point**. It is reported as step number, event kind, tool, and an argument-level diff (JSON paths with values on each side).
4. If either session contains a `divergence` event (strict stop), it is reported, and the divergence point is at or before it.

### 8.4 Output

Text output (default) has a summary of the two sessions with their totals (§8.2), the first divergence, if any, and the number of identical steps before it:

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

- Each argument-level diff entry gives its path and `expected → received`, formatted as in the divergence report (§6.4): compact JSON values, `(missing)` for a side without the path, and values longer than 80 characters cut with `…`.
- When text differs in steps that are not compared, a line says so, e.g. `text differs in steps 2, 5 (not compared; use --text)`.

`--format json` emits a stable JSON document with the same information, for CI and for the future UI. It has a top-level `format_version` (`1`), and its JSON Schema is committed in the repository as `docs/schema/diff.v1.json`. A change that is not backward compatible increments `format_version` and adds a new schema file. Adding a member to an object is backward compatible and keeps the version, so consumers MUST ignore members they do not know. A compatible addition updates `docs/schema/diff.v1.json` in place, and the added member is not `required`, so documents from earlier releases with the same `format_version` stay valid. Removing or renaming a member, changing its type or its meaning, and adding a value to an enumerated member (such as an event kind) are not backward compatible. Text differences are listed in `text_differences`, as step numbers. It is always present, and empty under `--text`, where text is part of the comparison.

When either session has `dropped_events > 0` (§7.7), the text output starts with a warning that names the session and the count, and says that the divergence point may be caused by missing events. The JSON document carries `dropped_events` for each session. The exit code is unchanged.

Exit codes: `0` identical trajectories, `1` divergence found, `2` error. These follow `diff(1)`. Because `1` means a divergence, every error is `2`: a usage error, a session that does not exist, and a cassette that cannot be read.

## 9. CLI

### 9.1 Global flags

| Flag | Env | Default | Description |
|---|---|---|---|
| `--dir` | `RETRACE_DIR` | `.retrace/cassettes` | Cassette directory |
| `--log-level` | `RETRACE_LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error` (logs go to stderr) |
| `--no-color` | `NO_COLOR` | — | Disable colored output |

Global flags MUST precede the command: `retrace [global flags] <command> [command flags] [arguments]`. Flags after the command name belong to that command, so a global flag placed there is a usage error (exit `2`). That error SHOULD say that global flags go before the command.

`--dir` need not exist.

- The commands that create a cassette (`record` and `run`) create a missing `--dir`, with any missing parents, with mode `0700`, when they create the cassette. An existing directory is used as it is.
- The other commands never create it. For them a missing `--dir` holds no sessions: `ls` lists none, and a command that names a session reports that it does not exist.

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
- `--force`: replaces the cassette of an existing session. The old cassette stays unchanged until the new one holds its session row (§7.2), and the new file then takes its place with an atomic rename. If `record` fails before that, for example because the session row cannot be written, the old session is left as it was. If a step right after the rename fails (removing the old cassette's SQLite log files, or reopening the new cassette), the new cassette is removed as well, and the id is left free.
- `--mcp`: registers an MCP upstream. Repeatable.
- `--listen`: default `127.0.0.1:4790`. Port `0` picks a free port. Non-loopback addresses require `--listen-unsafe`.
- `--anthropic-upstream`: the Anthropic API base URL. Default `https://api.anthropic.com`. A value that is not an absolute `http` or `https` URL is a usage error.

### 9.3 `retrace run`

```text
retrace run <id> [--model <model>] [--tools frozen|live] [--as <new-id>]
                 [--on-miss strict] [--realtime] [--grace <duration>]
                 [--anthropic-upstream <url>] [--mcp <name>=<url>]...
                 [--redact-header <name>]... [--listen <addr>]
                 (--recorded-command | -- <agent command...>)
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
  - As in `record` (§9.2), existence is decided by the cassette file. If another session takes the default id before the cassette is created, the next `n` is tried. If that `n` does not fit the id format or reaches 2^63 − 1, `run` fails with exit `1` and asks for `--as`.
  - If the default does not fit the id format (§9.2), or its `n` reaches 2^63 − 1, it is a usage error that asks for `--as`.
  - An `--as` id that already exists fails with exit `1`, as in `record` without `--force`. `run` has no `--force`.
- `-- <cmd>`: the agent command. The agent runs in retrace's working directory.
- `--recorded-command`: runs the source session's recorded command (`command`, §7.2) in its recorded working directory (`cwd`). A source with no recorded command (an empty `command`) is a usage error (exit `2`).
- `run` MUST NOT run the recorded command unless `--recorded-command` is given ([ADR 0013](../adr/0013-explicit-replay-command.md)). A cassette may come from someone else, so it is untrusted input, and its `command` is code that would run with the user's privileges (the shareable bug reports of the [vision](../vision.md), §2).
  - Without `-- <cmd>` or `--recorded-command`, `run` fails with a usage error (exit `2`) that prints the stored argv and cwd, so the user can review them before choosing. Each argv element and the cwd are printed as quoted strings, with control and bidirectional control characters escaped, so the text cannot drive the terminal or hide part of the command. When the source has no recorded command, the error says so and asks for `-- <cmd>`, since `--recorded-command` would not help.
  - Giving both `-- <cmd>` and `--recorded-command` is a usage error (exit `2`).
- `--model`: the model for `swap-model` (§4.2). An empty value is a usage error.
- `--anthropic-upstream`: the Anthropic API base URL in `swap-model`, with the same default and validation as in `record` (§9.2). In other modes the LLM channel is not forwarded, so the flag is a usage error.
- `--mcp`: in `live-tools`, overrides recorded MCP URLs (§4.3), with the same syntax as in `record` (§9.2).
  - Its name MUST be one of the source's `mcp_servers` (§7.2). Another name is a usage error, because the agent would have no `RETRACE_MCP_<NAME>_URL` for it.
  - In other modes MCP is served from the cassette, so the flag is a usage error.
- `--redact-header`: adds a header name to the source session's redaction rules (§3.5), as in `record`. Repeatable. The new session stores the combined rules. Headers are not fingerprinted (§5), so matching does not change. `run` has no `--redact`: body rules change fingerprints, so a replay uses the source's.
- `--grace`: how long the agent has to exit after `SIGTERM` on a divergence (§6.2), as a Go duration (`5s`, `500ms`). Default `5s`. A negative value is a usage error.
- MCP servers: a replay serves the MCP servers of the source session (`mcp_servers`, §7.2) under the same names, and gives the agent the same `RETRACE_MCP_<NAME>_URL` variables (§3.2), pointing at retrace. In `exact` and `swap-model`, where every MCP answer comes from the cassette, its `mcp_servers` is a copy of the source's. In `live-tools`, each server keeps its name, its `url` is the one actually used (after `--mcp`, redacted as in §3.5), and `protocol_version` and `server_info` are read from the replay's own exchanges as in §3.4.
- Errors in the flags and arguments (exit `2`), including both `-- <cmd>` and `--recorded-command`, and a default id that does not fit or whose `n` reaches 2^63 − 1, are reported before the source session is opened. The checks that need the source come after, and before the check that an `--as` id already exists (exit `1`): neither `-- <cmd>` nor `--recorded-command` (the error prints the stored argv and cwd), `--recorded-command` with an empty command, an `--mcp` name that is not in `mcp_servers`, and, in `live-tools`, a stored URL with a redaction marker and no `--mcp` (§4.3).
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

- The values are the totals of §8.2. `TOKENS` is `in/out`, abbreviated (e.g. `41.2k/3.9k`). `DURATION` is empty while a session has not ended.
- A file in `--dir` that ends in `.cassette` but cannot be opened or read (e.g. a newer schema, a corrupt database, a missing session row, or a schema that fails the checks of §7.6) is skipped with a warning on stderr, and `ls` still exits `0`.
- `--json` emits one JSON array with an object per session, newest first, with the same information unabbreviated.

When a session has `dropped_events > 0` (§7.7), its `STATUS` is followed by `+gaps` (e.g. `completed+gaps`), because its steps and totals come from an incomplete log. `--json` carries `dropped_events` for each session.

### 9.6 `retrace show`

```text
retrace show <id> [--json] [--raw <interaction-id>]
```

- The default output is the session summary (metadata and totals, §8.2) followed by a timeline of events: `seq`, relative time, `source → target`, kind, tool, arguments (truncated), status. The relative time counts from the session's `created_at`. Arguments are compact JSON, cut after 60 characters with `…`. Text from the cassette, here and in `--raw`, is escaped as in §10.
- When `dropped_events > 0` (§7.7), the summary warns that the event log is incomplete and gives the count, and the timeline shows a marker line where `seq` values are missing, with how many events are missing there. `--json` emits only the events that exist; consumers find gaps from `seq`.
- `--json`: emits events as JSONL, one event per line, as a stable format. Each line is an object with one member per column of the `events` table (§7.4), named as the column, with `null` for a null value. `arguments` and `result` are the stored JSON values. A stored value that is not valid JSON, which a valid cassette never holds, is emitted as a JSON string, with invalid UTF-8 replaced by U+FFFD. A stored number that is not finite, such as a `cost_usd` of `+Inf`, is emitted as `null`, so that one bad value does not stop the stream either. `started_at` is RFC 3339, in UTC for a valid cassette (§7.1), with the trailing zeros of the fraction left out (`2026-10-23T09:30:02Z`, `2026-10-23T09:30:02.5Z`), unlike the stored form of §7.1. Consumers parse it as RFC 3339 and do not assume six fractional digits.
- `--raw`: dumps one interaction (headers, bodies, SSE chunks with offsets), as stored, after redaction. A body or chunk that is not valid UTF-8 is shown as `<n bytes, not UTF-8>`. `--raw` with `--json` is a usage error.

### 9.7 Exit codes

| Code | Meaning |
|---|---|
| `0` | Success (for `diff`: trajectories identical) |
| `1` | Runtime error (for `diff`: divergence found) |
| `2` | Usage error, or unsupported option (reserved strategies, `--model` with `--tools live`). For `diff`: any error (§8.4) |
| `3` | `run`: replay stopped on a divergence (`strict`) |
| `4` | `record` / `run`: the agent exited with a non-zero code. Its code is stored in `agent_exit_code` |

### 9.8 `retrace reindex`

```text
retrace reindex <id>
```

Rebuilds the events of session `<id>` (§7.6) and prints how many there are, with the deriver and pricing versions used. It exits `0` on success, `2` for an invalid id, and `1` when the session does not exist or cannot be reindexed.

## 10. Security and privacy

- Credentials in headers and URLs are redacted before storage, and body redaction is opt-in (§3.5, [ADR 0011](../adr/0011-secret-redaction-defaults.md)).
- The proxy binds to loopback by default.
- In `exact` mode no outbound connection is made.
- Cassettes contain prompts and tool outputs verbatim. The README and `record --help` MUST say so.
- A cassette from someone else is untrusted input. `run` never runs its recorded command implicitly (§9.3), and text that retrace prints from a cassette is escaped so that it cannot drive the terminal.
- **Escaped text output.** Every text output escapes each control character (C0 and C1) and each bidirectional control character that comes from a cassette. That covers `ls`, `show`, `show --raw`, `diff` in text format and the divergence report of §6.4, and also the warnings, errors and log messages of any command that include a value read from a cassette, such as a stored MCP URL (§4.3) or command (§9.3). The one exception is the multi-line text of `show --raw` (bodies and SSE chunks), which keeps line feeds and tabs so it keeps its layout. They cannot drive the terminal.
  - The length limits of §6.4 (80 characters) and §9.6 (60 characters) count characters before escaping. An escaped value can therefore print wider than the limit, but a cut never splits an escape.
  - Text output is for people. The JSON outputs (`show --json`, `diff --format json`) are the faithful form of the cassette's values.

## 11. Acceptance criteria

Each criterion is verified by an automated test unless marked *(manual)*. "Fake upstream" means an in-process test server that emulates the Anthropic API or an MCP server.

### Recording

- **AC-01** `record` launches the agent with `ANTHROPIC_BASE_URL` and `RETRACE_MCP_<NAME>_URL` set, and the session ends when the agent exits.
- **AC-02** A non-streaming `/v1/messages` exchange is recorded with request, response, status, headers (redacted) and timing.
- **AC-03** A streaming `/v1/messages` exchange is recorded with every SSE event and its offset. The agent receives the first chunk before the upstream finishes (no buffering).
- **AC-04** MCP `initialize`, `tools/list` and `tools/call` exchanges over Streamable HTTP are recorded, for both JSON and SSE responses, against two distinct MCP servers in the same session.
- **AC-05** Events are derived with all §7.4 fields populated where applicable, including tokens and estimated cost for each `model_response`, and correct `source`/`target`.
- **AC-06** Credentials never appear in the cassette file, verified by scanning the raw file bytes: the LLM `x-api-key`, an MCP `Authorization` header, a `*-token` header, a token in a query string, a credential in the query or userinfo of a `Location` URL, and a `Set-Cookie` response header. The scan covers the cassette of a recording and the cassette of a replay that forwards requests (`swap-model` and `live-tools`).
- **AC-07** Killing retrace with `SIGINT` mid-session leaves a readable cassette with status `interrupted`.
- **AC-34** In record mode and in exact mode, a request for `/mcp/<name>/../x`, for `/mcp/../llm/anthropic/v1/messages`, or with `%2e%2e` in place of `..`, gets HTTP `400` (never a redirect), and a request whose body is larger than 64 MiB gets HTTP `413`, with or without a `Content-Length`. On the LLM route the body has the Anthropic error shape with type `invalid_request_error` or `request_too_large`. None of them reaches the upstream or is stored in the cassette, and the logged warning does not contain the request's path or query (checked with a token in the query).
- **AC-24** `record --session` with an invalid id (for example `../x`, `.hidden`, `a/b`, `NUL` or 129 characters) exits `2` and creates no file.
- **AC-25** `record --redact '$..email'` stores no `email` value from request bodies, response bodies or SSE events (verified by scanning the raw file bytes), while the agent and the upstream receive the original bodies.
- **AC-26** When the cassette writer drops events during `record` (forced with failing event writes or a stalled writer), the ended session has `dropped_events` equal to the number of dropped events, including events dropped at the end of the log, the missing `seq` values are exactly those of the other dropped events, and `reindex` restores the events of complete interactions and sets `dropped_events` to `0`.

### Exact replay

- **AC-08** `run <id>` (exact) replays a recorded session with all upstreams unreachable. The agent's stdout matches the original run's stdout.
- **AC-09** In exact mode the proxy opens zero outbound connections (enforced by a dialer that fails the test if called).
- **AC-10** Streaming responses are replayed byte-identically, except for the JSON-RPC ids that MCP replay rewrites (§5 step 3). With `--realtime`, chunk offsets are within ±20 ms of the recorded ones.
- **AC-11** JSON-RPC ids in replayed MCP responses match the incoming request ids. A replayed `initialize` response carries a freshly issued `Mcp-Session-Id` when the recorded one carried it, and none otherwise.
- **AC-12** Identical requests repeated N times are served the N recorded responses in order (FIFO per fingerprint).
- **AC-13** Requests arriving in a different order than recorded (parallel tool calls) replay without a divergence.
- **AC-31** In exact mode, an MCP request that cannot be served from its queue gets a JSON-RPC error `-32099` with the request's id (`null` when it cannot be read), or one error per request for a batch, appends a `divergence` event with `channel = mcp` and the server's `upstream` (with `incomplete_interaction` set, and the error's `data` saying so, when the queue head is an incomplete recording), and the replay ends with status `diverged` and exit `3`.
- **AC-32** In exact mode, a `POST` with only client notifications or responses gets `202`, `GET` gets `405` and `DELETE` gets `200`. None of them consumes a recorded interaction, each is stored with `provenance = replayed` and no source interaction, and recorded ones of these kinds are not reported as unconsumed.
- **AC-28** In exact mode, an LLM request that cannot be served from its queue (the queue is empty, or its head is an interaction whose recording is incomplete) gets HTTP `422` with error type `retrace_cassette_miss`, appends a `divergence` event (with `incomplete_interaction` set in the second case), and the replay ends with status `diverged` and exit `3`. This holds even when the agent then exits with a non-zero code or retrace receives a signal, and in both cases `agent_exit_code` keeps the agent's code. The incomplete interaction is not reported as unconsumed.
- **AC-29** In exact mode, an LLM or MCP request whose queue head is an interaction recorded with a transport error and a complete recording gets HTTP `502` with a body starting `retrace: upstream request failed:`. The interaction is consumed, no `divergence` event is appended, the replay does not end `diverged` on its account, and the new interaction is stored with `provenance = replayed`, no status and the source's `error`, deriving the same events as the source (for an LLM request, the same `model_response`).
- **AC-30** `run` with a source session that does not exist, or with an `--as` id that already exists, exits `1`; `run --recorded-command` of a source with no recorded command exits `2`. None of them creates a new cassette.
- **AC-33** `run <id>` with neither `-- <cmd>` nor `--recorded-command` exits `2`, starts no process, creates no cassette, and prints the recorded argv and cwd with control characters escaped. For a source with no recorded command, the error says so and asks for `-- <cmd>` without suggesting `--recorded-command`. `run` with both exits `2`. `run <id> --recorded-command` runs the recorded command in the recorded working directory.

### Swap-model and strict

- **AC-14** `run <id> --model M` forwards LLM requests with `model` rewritten to `M` and serves MCP from the cassette. The new cassette records `requested_model` and `model`.
- **AC-15** When the new model calls a tool with arguments not in the cassette, retrace records a `divergence` event with a JSON-path diff, answers with JSON-RPC error `-32099`, terminates the agent (`SIGKILL` after `--grace` if it ignores `SIGTERM`), prints the divergence report and exits `3`.
- **AC-16** `--on-miss nearest-match` (and the other reserved values) exits `2` with a "not implemented" message.
- **AC-17** `--model` combined with `--tools live` exits `2`.

### Live-tools

- **AC-18** `run <id> --tools live` serves LLM responses from the cassette and forwards MCP to the real servers. A changed tool result produces a divergence on the next LLM request.

### Diff and inspection

- **AC-19** `diff` of a session with its exact replay reports identical trajectories and exits `0`.
- **AC-20** `diff` of a session with a diverging swap-model replay reports the correct step, event, tool and argument diff, and exits `1`.
- **AC-21** `diff --format json` output validates against a JSON schema committed in the repo.
- **AC-22** `ls` and `show` (text and `--json`) display the fields listed in §9.5 and §9.6.
- **AC-35** A cassette whose `events` is a view, that holds a virtual table or a trigger retrace's migrations do not create, or whose `events` table has a generated column, is rejected before it is read: `ls` skips it with a warning and exits `0`, `show` and `run` exit `1`, and the file is not modified.
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
