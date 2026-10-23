-- Schema v1: spec §7 (cassette data model).
-- Timestamps are RFC 3339 UTC strings with microsecond precision; durations are
-- integer milliseconds; JSON columns hold UTF-8 JSON text.

-- §7.2: exactly one row per cassette.
CREATE TABLE session (
    id                TEXT    PRIMARY KEY,
    mode              TEXT    NOT NULL CHECK (mode IN ('record', 'exact', 'swap-model', 'live-tools')),
    parent_session_id TEXT,
    status            TEXT    NOT NULL CHECK (status IN ('recording', 'completed', 'diverged', 'failed', 'interrupted')),
    created_at        TEXT    NOT NULL,
    ended_at          TEXT,
    command           TEXT    NOT NULL,
    cwd               TEXT    NOT NULL,
    agent_exit_code   INTEGER,
    model_override    TEXT,
    on_miss           TEXT    NOT NULL,
    mcp_servers       TEXT    NOT NULL,
    redaction         TEXT    NOT NULL,
    retrace_version   TEXT    NOT NULL,
    deriver_version   INTEGER NOT NULL,
    pricing_version   TEXT    NOT NULL,
    labels            TEXT    NOT NULL
);

CREATE TRIGGER session_single_row BEFORE INSERT ON session
WHEN (SELECT COUNT(*) FROM session) > 0
BEGIN
    SELECT RAISE(ABORT, 'cassette already has a session');
END;

-- §7.3: content-addressed payloads, deduplicated by SHA-256 (ADR 0005).
CREATE TABLE blobs (
    sha256 TEXT    PRIMARY KEY,
    size   INTEGER NOT NULL,
    data   BLOB    NOT NULL
);

-- §7.3: one row per HTTP exchange through the proxy (raw layer, ADR 0009).
CREATE TABLE interactions (
    id                    INTEGER PRIMARY KEY,
    channel               TEXT    NOT NULL CHECK (channel IN ('llm', 'mcp')),
    upstream              TEXT    NOT NULL,
    http_method           TEXT    NOT NULL,
    path                  TEXT    NOT NULL,
    status_code           INTEGER,
    request_headers       TEXT    NOT NULL,
    response_headers      TEXT,
    request_blob          TEXT    REFERENCES blobs (sha256),
    response_blob         TEXT    REFERENCES blobs (sha256),
    streamed              INTEGER NOT NULL DEFAULT 0 CHECK (streamed IN (0, 1)),
    fingerprint           TEXT,
    canonical_blob        TEXT    REFERENCES blobs (sha256),
    started_at            TEXT    NOT NULL,
    ttfb_ms               INTEGER,
    duration_ms           INTEGER,
    provenance            TEXT    NOT NULL CHECK (provenance IN ('recorded', 'replayed', 'live')),
    source_interaction_id INTEGER,
    error                 TEXT
    -- An interaction is in flight while duration_ms IS NULL. Completing it sets
    -- duration_ms, and that happens exactly once (ADR 0009).
);

-- Replay builds per-fingerprint FIFO queues (spec §5).
CREATE INDEX interactions_fingerprint ON interactions (fingerprint, id);

-- §7.3: raw SSE events, in order, with their offset from the start of the response.
CREATE TABLE chunks (
    interaction_id INTEGER NOT NULL REFERENCES interactions (id),
    idx            INTEGER NOT NULL,
    offset_ms      INTEGER NOT NULL,
    data           BLOB    NOT NULL,
    PRIMARY KEY (interaction_id, idx)
) WITHOUT ROWID;

-- §7.4: semantic layer, append-only, ordered by seq.
CREATE TABLE events (
    seq                INTEGER PRIMARY KEY,
    parent_seq         INTEGER REFERENCES events (seq),
    interaction_id     INTEGER REFERENCES interactions (id),
    kind               TEXT    NOT NULL CHECK (kind IN ('user_message', 'model_response', 'tool_use', 'tool_call', 'tool_result', 'mcp_message', 'divergence')),
    source             TEXT    NOT NULL,
    target             TEXT    NOT NULL,
    started_at         TEXT    NOT NULL,
    duration_ms        INTEGER,
    model              TEXT,
    requested_model    TEXT,
    tool               TEXT,
    arguments          TEXT,
    result             TEXT,
    tokens_in          INTEGER,
    tokens_out         INTEGER,
    tokens_cache_read  INTEGER,
    tokens_cache_write INTEGER,
    cost_usd           REAL,
    status             TEXT    NOT NULL CHECK (status IN ('ok', 'error', 'divergence')),
    error              TEXT,
    provenance         TEXT    NOT NULL CHECK (provenance IN ('recorded', 'replayed', 'live', 'fallthrough', 'nearest', 'synthetic')),
    deriver_version    INTEGER NOT NULL
);

CREATE INDEX events_interaction ON events (interaction_id);
