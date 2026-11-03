package cli

import (
	"strings"
	"testing"

	"github.com/pmlucero/retrace/internal/redact"
)

// AC-18: where a live-tools replay forwards each MCP server (spec §4.3).
func TestLiveMCPServers(t *testing.T) {
	const stored = `[{"name":"orders","url":"http://127.0.0.1:9001/mcp","protocol_version":"2025-06-18"},` +
		`{"name":"crm","url":"https://REDACTED@crm.example.com/mcp"},` +
		`{"name":"search","url":"https://search.example.com/mcp?key=[REDACTED:0a1b2c3d]"},` +
		`{"name":"bad","url":"ftp://files.example.com"}]`
	all := []mcpServer{
		{Name: "crm", URL: "https://u:p@crm.example.com/mcp"}, //nolint:gosec // G101: a fake credential the test looks for
		{Name: "search", URL: "https://search.example.com/mcp?key=sk-1"},
		{Name: "bad", URL: "http://127.0.0.1:9002/mcp"},
	}
	for _, tc := range []struct {
		name      string
		stored    string
		overrides []mcpServer
		want      string // name=url, joined with " "
		wantErr   string
	}{
		{name: "none", stored: ``, want: ""},
		{name: "recorded URL", stored: `[{"name":"orders","url":"http://127.0.0.1:9001/mcp"}]`,
			want: "orders=http://127.0.0.1:9001/mcp"},
		{name: "every override", stored: stored, overrides: all,
			want: "orders=http://127.0.0.1:9001/mcp crm=https://u:p@crm.example.com/mcp " +
				"search=https://search.example.com/mcp?key=sk-1 bad=http://127.0.0.1:9002/mcp"},
		{name: "override replaces a usable URL", stored: `[{"name":"orders","url":"http://127.0.0.1:9001/mcp"}]`,
			overrides: []mcpServer{{Name: "orders", URL: "http://127.0.0.1:9003/mcp"}}, want: "orders=http://127.0.0.1:9003/mcp"},
		{name: "redacted userinfo", stored: stored, overrides: all[1:], wantErr: "MCP server crm: the recorded URL https://REDACTED@crm.example.com/mcp was redacted"},
		{name: "redacted query", stored: stored, overrides: []mcpServer{all[0], all[2]}, wantErr: "MCP server search"},
		{name: "unusable URL", stored: stored, overrides: all[:2], wantErr: "MCP server bad: the recorded URL cannot be used"},
		{name: "unknown name", stored: stored, overrides: []mcpServer{{Name: "billing", URL: "http://127.0.0.1:1"}},
			wantErr: `no MCP server named "billing"`},
		{name: "invalid JSON", stored: `{`, wantErr: "parsing mcp_servers"},
		{name: "invalid name", stored: `[{"name":"../x","url":"http://127.0.0.1:1"}]`, wantErr: "invalid MCP server name"},
		{name: "name twice", stored: `[{"name":"a","url":"http://127.0.0.1:1"},{"name":"a","url":"http://127.0.0.1:2"}]`,
			wantErr: "listed twice"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := liveMCPServers([]byte(tc.stored), tc.overrides)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var parts []string
			for _, sv := range got {
				parts = append(parts, sv.name+"="+sv.target.String())
			}
			if s := strings.Join(parts, " "); s != tc.want {
				t.Errorf("servers %s, want %s", s, tc.want)
			}
		})
	}
}

// spec §9.3: the stored URL is the one used, redacted.
func TestLiveMCPServersJSON(t *testing.T) {
	servers, err := liveMCPServers([]byte(`[{"name":"crm","url":"https://REDACTED@crm.example.com/mcp"}]`),
		[]mcpServer{{Name: "crm", URL: "https://u:p@crm2.example.com/mcp?api_key=sk-1"}}) //nolint:gosec // G101: a fake credential the test looks for
	if err != nil {
		t.Fatal(err)
	}
	got := string(liveMCPServersJSON(servers, redact.NewRules(nil, nil)))
	if !strings.HasPrefix(got, `[{"name":"crm","url":"https://REDACTED@crm2.example.com/mcp?api_key=[REDACTED:`) ||
		strings.Contains(got, "sk-1") || strings.Contains(got, "u:p") {
		t.Errorf("mcp_servers %s, want the URL used, redacted, and nothing else", got)
	}
}
