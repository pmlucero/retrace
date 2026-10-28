package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"

	"github.com/pmlucero/retrace/internal/cassette"
	"github.com/pmlucero/retrace/internal/mcp"
)

// storeMCPServerInfo records, in the session's mcp_servers, the protocol
// version and server info that each MCP server returned to initialize
// (spec §3.4, §7.2). It reads them from the recorded interactions, so it
// runs once the writer has written everything. A server that was never
// initialized keeps neither. When an agent initializes a server more than
// once, the last answer with a protocol version counts: it is the one the
// session went on with.
func storeMCPServerInfo(ctx context.Context, c *cassette.Cassette) error {
	s, err := c.Session(ctx)
	if err != nil {
		return err
	}
	var servers []mcpServer
	if err := json.Unmarshal(s.MCPServers, &servers); err != nil {
		return fmt.Errorf("reading the session's MCP servers: %w", err)
	}
	if len(servers) == 0 {
		return nil
	}
	all, err := c.Interactions(ctx)
	if err != nil {
		return err
	}
	// Newest first, so the scan stops once every server has its answer.
	done := make([]bool, len(servers))
	resolved := 0
	for _, in := range slices.Backward(all) {
		if resolved == len(servers) {
			break
		}
		if in.Channel != "mcp" || in.HTTPMethod != http.MethodPost || in.Incomplete() {
			continue
		}
		i := slices.IndexFunc(servers, func(sv mcpServer) bool { return "mcp:"+sv.Name == in.Upstream })
		if i < 0 || done[i] {
			continue
		}
		res, ok, err := initializeResult(ctx, c, in)
		if err != nil {
			return err
		}
		if ok {
			servers[i].ProtocolVersion, servers[i].ServerInfo = res.ProtocolVersion, res.ServerInfo
			done[i] = true
			resolved++
		}
	}
	if resolved == 0 {
		return nil
	}
	data, err := json.Marshal(servers)
	if err != nil {
		return fmt.Errorf("encoding the session's MCP servers: %w", err)
	}
	return c.Write(ctx, func(tx *cassette.Tx) error { return tx.SetMCPServers(ctx, data) })
}

// initializeResult returns the result of the last initialize request that
// interaction in carries and that its response answers, JSON or SSE, with a
// protocol version. ok is false when there is none. Bodies that are not
// JSON-RPC are opaque, not errors.
func initializeResult(ctx context.Context, c *cassette.Cassette, in cassette.Interaction) (_ mcp.InitializeResult, ok bool, _ error) {
	if in.RequestBlob == nil {
		return mcp.InitializeResult{}, false, nil
	}
	data, err := c.Blob(ctx, *in.RequestBlob)
	if err != nil {
		return mcp.InitializeResult{}, false, err
	}
	req, err := mcp.ParseBody(data)
	if err != nil || !slices.ContainsFunc(req.Messages, func(m mcp.Message) bool {
		return m.Kind == mcp.Request && m.Method == mcp.MethodInitialize
	}) {
		return mcp.InitializeResult{}, false, nil
	}

	var resps []mcp.Message
	switch {
	case in.Streamed:
		chunks, err := c.Chunks(ctx, in.ID)
		if err != nil {
			return mcp.InitializeResult{}, false, err
		}
		events := make([][]byte, len(chunks))
		for i, ch := range chunks {
			events[i] = ch.Data
		}
		resps = mcp.StreamMessages(events)
	case in.ResponseBlob != nil:
		data, err := c.Blob(ctx, *in.ResponseBlob)
		if err != nil {
			return mcp.InitializeResult{}, false, err
		}
		if body, err := mcp.ParseBody(data); err == nil {
			resps = body.Messages
		}
	}

	var res mcp.InitializeResult
	for _, p := range mcp.PairMessages(req.Messages, resps).Pairs {
		if p.Request.Method != mcp.MethodInitialize || p.Response == nil || p.Response.Result == nil {
			continue
		}
		// A result without a protocol version does not replace a good one.
		if r, err := mcp.ParseInitializeResult(p.Response.Result); err == nil && r.ProtocolVersion != "" {
			res, ok = r, true
		}
	}
	return res, ok, nil
}
