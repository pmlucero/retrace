package cli

import (
	"encoding/json"
	"fmt"
	"net/url"
	"slices"

	"github.com/pmlucero/retrace/internal/redact"
)

// liveServer is an MCP server that a live-tools replay forwards to.
type liveServer struct {
	name   string
	target *url.URL
}

// liveMCPServers returns where a live-tools replay forwards each MCP server
// of the source session (spec §4.3): its --mcp URL in overrides, or else the
// url stored in the source's mcp_servers, which must be valid JSON. The
// error, if any, is a usage error (spec §9.3): an override names a server
// the source does not have, or a stored URL that cannot be used has no
// override. A stored URL cannot be used when it holds a redaction marker, so
// it is not the URL that was recorded, or when it is not an http or https
// URL: the cassette is untrusted input.
func liveMCPServers(stored json.RawMessage, overrides []mcpServer) ([]liveServer, error) {
	var recorded []mcpServer
	if len(stored) > 0 {
		if err := json.Unmarshal(stored, &recorded); err != nil {
			return nil, fmt.Errorf("parsing mcp_servers: %w", err)
		}
	}
	for _, o := range overrides {
		if !slices.ContainsFunc(recorded, func(sv mcpServer) bool { return sv.Name == o.Name }) {
			return nil, fmt.Errorf("--mcp %s: the source session has no MCP server named %q", o.Name, o.Name)
		}
	}
	servers := make([]liveServer, 0, len(recorded))
	for _, sv := range recorded {
		if i := slices.IndexFunc(overrides, func(o mcpServer) bool { return o.Name == sv.Name }); i >= 0 {
			u, err := absoluteHTTPURL(overrides[i].URL)
			if err != nil {
				return nil, fmt.Errorf("--mcp %s: %w", sv.Name, err) // validated by the flag; unreachable
			}
			servers = append(servers, liveServer{name: sv.Name, target: u})
			continue
		}
		u, err := absoluteHTTPURL(sv.URL)
		switch {
		case err != nil:
			return nil, fmt.Errorf("MCP server %s: the recorded URL cannot be used (%w); give one with --mcp %s=<url>", sv.Name, err, sv.Name)
		case redact.HasMarker(u):
			return nil, fmt.Errorf("MCP server %s: the recorded URL %s was redacted; give the real one with --mcp %s=<url>", sv.Name, sv.URL, sv.Name)
		}
		servers = append(servers, liveServer{name: sv.Name, target: u})
	}
	return servers, nil
}

// liveMCPServersJSON returns the mcp_servers of a live-tools replay before
// its exchanges complete it (spec §9.3): each server under its name, with the
// URL actually used, redacted (spec §3.5).
func liveMCPServersJSON(servers []liveServer, rules *redact.Rules) json.RawMessage {
	out := make([]mcpServer, len(servers))
	for i, sv := range servers {
		out[i] = mcpServer{Name: sv.name, URL: rules.URL(sv.target)}
	}
	data, err := json.Marshal(out)
	if err != nil {
		return json.RawMessage(`[]`) // strings always marshal; unreachable
	}
	return data
}
