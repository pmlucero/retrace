// Package mcpmeta holds the session's MCP server metadata (spec §7.2): the
// persisted mcp_servers entries, the validation of their names and the
// protocol version and server info recorded from initialize (spec §3.4).
package mcpmeta

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"slices"

	"github.com/pmlucero/retrace/internal/cassette"
	"github.com/pmlucero/retrace/internal/derive"
)

// Server is an entry of the session's mcp_servers (spec §7.2).
type Server struct {
	Name string `json:"name"`
	URL  string `json:"url"`
	// From the server's initialize result (spec §3.4); absent until then.
	ProtocolVersion string          `json:"protocol_version,omitempty"`
	ServerInfo      json.RawMessage `json:"server_info,omitempty"`
}

var namePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

// ValidName reports whether name is one record accepts for an MCP server:
// it becomes a route and an environment variable.
func ValidName(name string) bool { return namePattern.MatchString(name) }

// Names returns the names of the source session's MCP servers (spec §7.2).
// The cassette is untrusted input, so each name must be one record accepts.
func Names(stored json.RawMessage) ([]string, error) {
	if len(stored) == 0 {
		return nil, nil
	}
	var servers []Server
	if err := json.Unmarshal(stored, &servers); err != nil {
		return nil, fmt.Errorf("parsing mcp_servers: %w", err)
	}
	names := make([]string, 0, len(servers))
	for _, sv := range servers {
		if !ValidName(sv.Name) {
			return nil, fmt.Errorf("invalid MCP server name %q", sv.Name)
		}
		if slices.Contains(names, sv.Name) {
			return nil, fmt.Errorf("MCP server %q is listed twice", sv.Name)
		}
		names = append(names, sv.Name)
	}
	return names, nil
}

// StoreServerInfo records, in the session's mcp_servers, the protocol
// version and server info that each MCP server returned to initialize
// (spec §3.4, §7.2). It reads them from the recorded interactions, so it
// runs once the writer has written everything. A server that was never
// initialized keeps neither. When an agent initializes a server more than
// once, the last answer with a protocol version counts: it is the one the
// session went on with.
func StoreServerInfo(ctx context.Context, c *cassette.Cassette) error {
	s, err := c.Session(ctx)
	if err != nil {
		return err
	}
	var servers []Server
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
		i := slices.IndexFunc(servers, func(sv Server) bool { return "mcp:"+sv.Name == in.Upstream })
		if i < 0 || done[i] {
			continue
		}
		res, ok, err := derive.InitializeResult(ctx, c, in)
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
