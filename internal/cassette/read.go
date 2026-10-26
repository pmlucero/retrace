package cassette

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Session returns the session row.
func (c *Cassette) Session(ctx context.Context) (Session, error) {
	var (
		s                              Session
		parent, endedAt, modelOverride sql.NullString
		createdAt, command, labels     string
		mcpServers, redaction          string
		agentExitCode                  sql.NullInt64
	)
	err := c.db.QueryRowContext(ctx, `
		SELECT id, mode, parent_session_id, status, created_at, ended_at, command, cwd,
			agent_exit_code, model_override, on_miss, mcp_servers, redaction, retrace_version,
			deriver_version, pricing_version, labels, dropped_events
		FROM session`).Scan(
		&s.ID, &s.Mode, &parent, &s.Status, &createdAt, &endedAt, &command, &s.Cwd,
		&agentExitCode, &modelOverride, &s.OnMiss, &mcpServers, &redaction, &s.RetraceVersion,
		&s.DeriverVersion, &s.PricingVersion, &labels, &s.DroppedEvents)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, fmt.Errorf("reading session of %s: %w", c.path, ErrNotFound)
	}
	if err != nil {
		return Session{}, fmt.Errorf("reading session of %s: %w", c.path, err)
	}

	if s.CreatedAt, err = parseTime(createdAt); err != nil {
		return Session{}, fmt.Errorf("reading session of %s: created_at: %w", c.path, err)
	}
	if s.EndedAt, err = parseNullTime(endedAt); err != nil {
		return Session{}, fmt.Errorf("reading session of %s: ended_at: %w", c.path, err)
	}
	if err := json.Unmarshal([]byte(command), &s.Command); err != nil {
		return Session{}, fmt.Errorf("reading session of %s: command: %w", c.path, err)
	}
	if err := json.Unmarshal([]byte(labels), &s.Labels); err != nil {
		return Session{}, fmt.Errorf("reading session of %s: labels: %w", c.path, err)
	}
	s.ParentSessionID = stringPtr(parent)
	s.ModelOverride = stringPtr(modelOverride)
	s.AgentExitCode = intPtr(agentExitCode)
	s.MCPServers = json.RawMessage(mcpServers)
	s.Redaction = json.RawMessage(redaction)
	return s, nil
}

// Interactions returns all interactions in arrival order.
func (c *Cassette) Interactions(ctx context.Context) ([]Interaction, error) {
	rows, err := c.db.QueryContext(ctx, `
		SELECT id, channel, upstream, http_method, path, status_code, request_headers, response_headers,
			request_blob, response_blob, streamed, fingerprint, canonical_blob, started_at, ttfb_ms,
			duration_ms, provenance, source_interaction_id, error
		FROM interactions ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("reading interactions of %s: %w", c.path, err)
	}
	defer rows.Close()

	var out []Interaction
	for rows.Next() {
		var (
			in                                                  Interaction
			statusCode, ttfb, duration, sourceID                sql.NullInt64
			requestHeaders, startedAt                           string
			responseHeaders, reqBlob, respBlob, fp, canon, ierr sql.NullString
		)
		if err := rows.Scan(&in.ID, &in.Channel, &in.Upstream, &in.HTTPMethod, &in.Path, &statusCode,
			&requestHeaders, &responseHeaders, &reqBlob, &respBlob, &in.Streamed, &fp, &canon, &startedAt,
			&ttfb, &duration, &in.Provenance, &sourceID, &ierr); err != nil {
			return nil, fmt.Errorf("reading interactions of %s: %w", c.path, err)
		}
		if in.StartedAt, err = parseTime(startedAt); err != nil {
			return nil, fmt.Errorf("reading interaction %d of %s: started_at: %w", in.ID, c.path, err)
		}
		in.StatusCode = intPtr(statusCode)
		in.RequestHeaders = json.RawMessage(requestHeaders)
		if responseHeaders.Valid {
			in.ResponseHeaders = json.RawMessage(responseHeaders.String)
		}
		in.RequestBlob, in.ResponseBlob = stringPtr(reqBlob), stringPtr(respBlob)
		in.Fingerprint, in.CanonicalBlob, in.Error = stringPtr(fp), stringPtr(canon), stringPtr(ierr)
		in.TTFBMillis, in.DurationMillis = int64Ptr(ttfb), int64Ptr(duration)
		in.SourceInteractionID = int64Ptr(sourceID)
		out = append(out, in)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading interactions of %s: %w", c.path, err)
	}
	return out, nil
}

// Chunks returns the SSE chunks of one interaction, in order.
func (c *Cassette) Chunks(ctx context.Context, interactionID int64) ([]Chunk, error) {
	rows, err := c.db.QueryContext(ctx,
		`SELECT idx, offset_ms, data FROM chunks WHERE interaction_id = ? ORDER BY idx`, interactionID)
	if err != nil {
		return nil, fmt.Errorf("reading chunks of interaction %d: %w", interactionID, err)
	}
	defer rows.Close()

	var out []Chunk
	for rows.Next() {
		ch := Chunk{InteractionID: interactionID}
		if err := rows.Scan(&ch.Index, &ch.OffsetMillis, &ch.Data); err != nil {
			return nil, fmt.Errorf("reading chunks of interaction %d: %w", interactionID, err)
		}
		out = append(out, ch)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading chunks of interaction %d: %w", interactionID, err)
	}
	return out, nil
}

// Blob returns the content stored under a SHA-256 key.
func (c *Cassette) Blob(ctx context.Context, key string) ([]byte, error) {
	var data []byte
	err := c.db.QueryRowContext(ctx, `SELECT data FROM blobs WHERE sha256 = ?`, key).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("reading blob %s: %w", key, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("reading blob %s: %w", key, err)
	}
	return data, nil
}

// Events returns the semantic log in seq order.
func (c *Cassette) Events(ctx context.Context) ([]Event, error) {
	rows, err := c.db.QueryContext(ctx, `
		SELECT seq, parent_seq, interaction_id, kind, source, target, started_at, duration_ms, model,
			requested_model, tool, arguments, result, tokens_in, tokens_out, tokens_cache_read,
			tokens_cache_write, cost_usd, status, error, provenance, deriver_version
		FROM events ORDER BY seq`)
	if err != nil {
		return nil, fmt.Errorf("reading events of %s: %w", c.path, err)
	}
	defer rows.Close()

	var out []Event
	for rows.Next() {
		var (
			e                                                  Event
			startedAt                                          string
			parent, interaction, duration, tin, tout, tcr, tcw sql.NullInt64
			model, reqModel, tool, args, result, eerr          sql.NullString
			cost                                               sql.NullFloat64
		)
		if err := rows.Scan(&e.Seq, &parent, &interaction, &e.Kind, &e.Source, &e.Target, &startedAt,
			&duration, &model, &reqModel, &tool, &args, &result, &tin, &tout, &tcr, &tcw, &cost,
			&e.Status, &eerr, &e.Provenance, &e.DeriverVersion); err != nil {
			return nil, fmt.Errorf("reading events of %s: %w", c.path, err)
		}
		if e.StartedAt, err = parseTime(startedAt); err != nil {
			return nil, fmt.Errorf("reading event %d of %s: started_at: %w", e.Seq, c.path, err)
		}
		e.ParentSeq, e.InteractionID, e.DurationMillis = int64Ptr(parent), int64Ptr(interaction), int64Ptr(duration)
		e.Model, e.RequestedModel, e.Tool, e.Error = stringPtr(model), stringPtr(reqModel), stringPtr(tool), stringPtr(eerr)
		e.TokensIn, e.TokensOut = int64Ptr(tin), int64Ptr(tout)
		e.TokensCacheRead, e.TokensCacheWrite = int64Ptr(tcr), int64Ptr(tcw)
		if args.Valid {
			e.Arguments = json.RawMessage(args.String)
		}
		if result.Valid {
			e.Result = json.RawMessage(result.String)
		}
		if cost.Valid {
			e.CostUSD = &cost.Float64
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading events of %s: %w", c.path, err)
	}
	return out, nil
}

func parseTime(s string) (time.Time, error) {
	return time.Parse(time.RFC3339Nano, s)
}

func parseNullTime(s sql.NullString) (*time.Time, error) {
	if !s.Valid {
		return nil, nil
	}
	t, err := parseTime(s.String)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func stringPtr(s sql.NullString) *string {
	if !s.Valid {
		return nil
	}
	return &s.String
}

func int64Ptr(n sql.NullInt64) *int64 {
	if !n.Valid {
		return nil
	}
	return &n.Int64
}

func intPtr(n sql.NullInt64) *int {
	if !n.Valid {
		return nil
	}
	v := int(n.Int64)
	return &v
}
