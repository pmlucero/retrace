package cassette

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Tx appends to a cassette inside one transaction. Get one from Cassette.Write.
type Tx struct {
	tx *sql.Tx
}

// InsertSession writes the session row. A cassette has exactly one.
func (t *Tx) InsertSession(ctx context.Context, s Session) error {
	command, err := json.Marshal(s.Command)
	if err != nil {
		return fmt.Errorf("encoding session command: %w", err)
	}
	labels, err := json.Marshal(nonNilMap(s.Labels))
	if err != nil {
		return fmt.Errorf("encoding session labels: %w", err)
	}
	_, err = t.tx.ExecContext(ctx, `
		INSERT INTO session (id, mode, parent_session_id, status, created_at, ended_at, command, cwd,
			agent_exit_code, model_override, on_miss, mcp_servers, redaction, retrace_version,
			deriver_version, pricing_version, labels)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		s.ID, s.Mode, s.ParentSessionID, s.Status, FormatTime(s.CreatedAt), nullTime(s.EndedAt),
		string(command), s.Cwd, s.AgentExitCode, s.ModelOverride, s.OnMiss,
		jsonOr(s.MCPServers, "[]"), jsonOr(s.Redaction, "{}"), s.RetraceVersion,
		s.DeriverVersion, s.PricingVersion, string(labels))
	if err != nil {
		return fmt.Errorf("inserting session %s: %w", s.ID, err)
	}
	return nil
}

// EndSession records how the session ended (spec §3.1). The session row only
// changes here and in SetMCPServers.
func (t *Tx) EndSession(ctx context.Context, end SessionEnd) error {
	res, err := t.tx.ExecContext(ctx,
		`UPDATE session SET status = ?, ended_at = ?, agent_exit_code = ?`,
		end.Status, FormatTime(end.EndedAt), end.AgentExitCode)
	if err != nil {
		return fmt.Errorf("ending session: %w", err)
	}
	return expectOneRow(res, "session")
}

// SetMCPServers replaces the session's MCP server metadata. It is called when
// an MCP server's initialize response provides its protocol version and server
// info (spec §3.4).
func (t *Tx) SetMCPServers(ctx context.Context, servers json.RawMessage) error {
	res, err := t.tx.ExecContext(ctx, `UPDATE session SET mcp_servers = ?`, jsonOr(servers, "[]"))
	if err != nil {
		return fmt.Errorf("updating session MCP servers: %w", err)
	}
	return expectOneRow(res, "session")
}

// SessionEnd holds the fields set when a session ends.
type SessionEnd struct {
	Status        string
	EndedAt       time.Time
	AgentExitCode *int
}

// PutBlob stores data once, keyed by its SHA-256, and returns the key. Storing
// the same content again is a no-op (ADR 0005).
func (t *Tx) PutBlob(ctx context.Context, data []byte) (string, error) {
	key := BlobKey(data)
	_, err := t.tx.ExecContext(ctx,
		`INSERT INTO blobs (sha256, size, data) VALUES (?, ?, ?) ON CONFLICT (sha256) DO NOTHING`,
		key, len(data), data)
	if err != nil {
		return "", fmt.Errorf("storing blob %s: %w", key, err)
	}
	return key, nil
}

// BlobKey returns the key that PutBlob stores data under: its SHA-256, in hex.
func BlobKey(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// BeginInteraction inserts an in-flight interaction when its request arrives.
// The caller assigns the ID, so the proxy never waits for the database
// (ADR 0009). DurationMillis must be nil: it is set by CompleteInteraction.
func (t *Tx) BeginInteraction(ctx context.Context, in Interaction) error {
	if in.DurationMillis != nil {
		return fmt.Errorf("beginning interaction %d: duration is set by CompleteInteraction", in.ID)
	}
	_, err := t.tx.ExecContext(ctx, `
		INSERT INTO interactions (id, channel, upstream, http_method, path, request_headers,
			request_blob, fingerprint, canonical_blob, started_at, provenance, source_interaction_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		in.ID, in.Channel, in.Upstream, in.HTTPMethod, in.Path, jsonOr(in.RequestHeaders, "{}"),
		in.RequestBlob, in.Fingerprint, in.CanonicalBlob, FormatTime(in.StartedAt), in.Provenance,
		in.SourceInteractionID)
	if err != nil {
		return fmt.Errorf("beginning interaction %d: %w", in.ID, err)
	}
	return nil
}

// CompleteInteraction records the response of an in-flight interaction. It
// succeeds once per interaction. Repeating it never changes the stored row
// (idempotent, ADR 0009) and returns ErrAlreadyCompleted, so a caller retrying
// after an ambiguous commit failure can treat that error as success.
func (t *Tx) CompleteInteraction(ctx context.Context, id int64, done Completion) error {
	res, err := t.tx.ExecContext(ctx, `
		UPDATE interactions
		SET status_code = ?, response_headers = ?, response_blob = ?, streamed = ?,
			ttfb_ms = ?, duration_ms = ?, error = ?
		WHERE id = ? AND duration_ms IS NULL`,
		done.StatusCode, jsonOr(done.ResponseHeaders, "{}"), done.ResponseBlob, done.Streamed,
		done.TTFBMillis, done.DurationMillis, done.Error, id)
	if err != nil {
		return fmt.Errorf("completing interaction %d: %w", id, err)
	}
	if n, err := res.RowsAffected(); err != nil || n == 1 {
		return err
	}
	var exists bool
	if err := t.tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM interactions WHERE id = ?)`, id).Scan(&exists); err != nil {
		return fmt.Errorf("completing interaction %d: %w", id, err)
	}
	if exists {
		return fmt.Errorf("completing interaction %d: %w", id, ErrAlreadyCompleted)
	}
	return fmt.Errorf("completing interaction %d: %w", id, ErrNotFound)
}

// interactionState returns when interaction id started and whether it is
// complete, or ErrNotFound.
func (t *Tx) interactionState(ctx context.Context, id int64) (startedAt time.Time, complete bool, err error) {
	var (
		started  string
		duration sql.NullInt64
	)
	err = t.tx.QueryRowContext(ctx, `SELECT started_at, duration_ms FROM interactions WHERE id = ?`, id).
		Scan(&started, &duration)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, false, fmt.Errorf("reading interaction %d: %w", id, ErrNotFound)
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("reading interaction %d: %w", id, err)
	}
	if startedAt, err = parseTime(started); err != nil {
		return time.Time{}, false, fmt.Errorf("reading interaction %d: started_at: %w", id, err)
	}
	return startedAt, duration.Valid, nil
}

// AppendChunk stores one raw SSE event of a streamed response.
func (t *Tx) AppendChunk(ctx context.Context, ch Chunk) error {
	_, err := t.tx.ExecContext(ctx,
		`INSERT INTO chunks (interaction_id, idx, offset_ms, data) VALUES (?, ?, ?, ?)`,
		ch.InteractionID, ch.Index, ch.OffsetMillis, ch.Data)
	if err != nil {
		return fmt.Errorf("appending chunk %d of interaction %d: %w", ch.Index, ch.InteractionID, err)
	}
	return nil
}

// AppendEvent appends to the semantic log. The caller assigns Seq, strictly
// increasing (spec §7.4).
func (t *Tx) AppendEvent(ctx context.Context, e Event) error {
	_, err := t.tx.ExecContext(ctx, `
		INSERT INTO events (seq, parent_seq, interaction_id, kind, source, target, started_at,
			duration_ms, model, requested_model, tool, arguments, result, tokens_in, tokens_out,
			tokens_cache_read, tokens_cache_write, cost_usd, status, error, provenance, deriver_version)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.Seq, e.ParentSeq, e.InteractionID, e.Kind, e.Source, e.Target, FormatTime(e.StartedAt),
		e.DurationMillis, e.Model, e.RequestedModel, e.Tool, jsonOrNull(e.Arguments), jsonOrNull(e.Result),
		e.TokensIn, e.TokensOut, e.TokensCacheRead, e.TokensCacheWrite, e.CostUSD, e.Status, e.Error,
		e.Provenance, e.DeriverVersion)
	if err != nil {
		return fmt.Errorf("appending event %d: %w", e.Seq, err)
	}
	return nil
}

func expectOneRow(res sql.Result, what string) error {
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("updating %s: %w", what, err)
	}
	if n != 1 {
		return fmt.Errorf("updating %s: %w", what, ErrNotFound)
	}
	return nil
}

// jsonOr returns raw as text, or fallback when raw is empty (for NOT NULL columns).
func jsonOr(raw json.RawMessage, fallback string) string {
	if len(raw) == 0 {
		return fallback
	}
	return string(raw)
}

// jsonOrNull returns raw as text, or SQL NULL when raw is empty.
func jsonOrNull(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	return string(raw)
}

func nonNilMap(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

// nullTime formats t, or returns SQL NULL when t is nil.
func nullTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return FormatTime(*t)
}
