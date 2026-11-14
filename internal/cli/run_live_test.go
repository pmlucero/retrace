package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/pmlucero/retrace/internal/cassette"
	"github.com/pmlucero/retrace/internal/mcp/mcptest"
)

// liveServers starts the MCP servers of the helper agent again, with lookup
// answering through tool, and returns the --mcp flags that reach them.
func liveServers(t *testing.T, tool mcptest.Tool) []string {
	t.Helper()
	orders := mcptest.New(t, mcptest.Config{Name: "orders", Strict: true, Tools: []mcptest.Tool{tool}})
	weather := mcptest.New(t, mcptest.Config{Name: "weather", Strict: true, Mode: mcptest.SSE, LogEvents: 1,
		Tools: []mcptest.Tool{tool}})
	// orders is registered at a path, and gets exactly that path (spec §3.4, #121).
	t.Cleanup(func() {
		for _, rq := range orders.Received() {
			if rq.Path != "/mcp" {
				t.Errorf("orders received %s %s, want path /mcp", rq.Method, rq.Path)
			}
		}
	})
	return []string{"--mcp", "orders=" + orders.URL + "/mcp", "--mcp", "weather=" + weather.URL}
}

// AC-18: run --tools live serves the LLM from the cassette and forwards MCP
// to the real servers; a changed tool result makes the next LLM request
// miss (spec §4.3).
func TestRunLiveTools(t *testing.T) {
	t.Setenv(mcpFollowUpEnv, "1")
	t.Setenv(agentIgnoreTermEnv, "1") // so it reports every exchange after a miss
	dir := t.TempDir()
	recorded := recordMCPForReplay(t, dir) // its servers are closed now

	t.Run("same results", func(t *testing.T) {
		mcpFlags := liveServers(t, lookupTool())
		r := runRunCmd(t, dir, append([]string{"s", "--recorded-command", "--tools", "live", "--listen", "127.0.0.1:0"}, mcpFlags...)...)
		if r.code != ExitOK {
			t.Fatalf("run: exit %d\n%s", r.code, r.stderr)
		}
		if replayed := agentExchanges(t, r.stdout.String()); len(replayed) != len(recorded) {
			t.Errorf("the agent saw %d MCP exchanges, want %d", len(replayed), len(recorded))
		}
		if strings.Contains(r.stderr.String(), "never replayed") {
			t.Errorf("unexpected unconsumed warning:\n%s", r.stderr)
		}

		s, ins := sessionOf(t, dir, "s.live-tools.1")
		if s.Mode != "live-tools" || s.Status != "completed" || s.ModelOverride != nil {
			t.Errorf("session = %+v, want live-tools, completed and no model_override", s)
		}
		var servers []mcpServer
		if err := json.Unmarshal(s.MCPServers, &servers); err != nil || len(servers) != 2 {
			t.Fatalf("mcp_servers %s: %v", s.MCPServers, err)
		}
		for i, sv := range servers {
			// The URL used, and what the live server answered to initialize.
			if want := strings.TrimPrefix(mcpFlags[2*i+1], sv.Name+"="); sv.URL != want || sv.ProtocolVersion == "" || sv.ServerInfo == nil {
				t.Errorf("mcp_servers[%d] = %+v, want url %s and the live initialize result", i, sv, want)
			}
		}
		var llm, live int
		for _, in := range ins {
			switch {
			case in.Channel == "llm" && in.Provenance == "replayed" && in.SourceInteractionID != nil:
				llm++
			case in.Channel == "mcp" && in.Provenance == "live" && in.SourceInteractionID == nil:
				live++
			default:
				t.Errorf("interaction %d: %s with provenance %s, want replayed LLM and live MCP", in.ID, in.Channel, in.Provenance)
			}
		}
		if llm != 2 || live != len(recorded) {
			t.Errorf("recorded %d replayed LLM and %d live MCP interactions, want 2 and %d", llm, live, len(recorded))
		}
	})

	t.Run("changed result", func(t *testing.T) {
		moved := mcptest.Tool{Name: "lookup", Handler: func(_ context.Context, args json.RawMessage) (string, error) {
			return "moved " + string(args), nil
		}}
		r := runRunCmd(t, dir, append([]string{"s", "--recorded-command", "--tools", "live", "--as", "moved", "--listen", "127.0.0.1:0"},
			liveServers(t, moved)...)...)
		if r.code != ExitDiverged {
			t.Fatalf("run: exit %d, want %d\n%s", r.code, ExitDiverged, r.stderr)
		}
		if !strings.Contains(r.stderr.String(), "first miss: anthropic POST /v1/messages") {
			t.Errorf("stderr lacks the LLM miss in the report:\n%s", r.stderr)
		}
		events, err := openCassette(t, cassette.Path(dir, "moved")).Events(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		var misses int
		for _, e := range events {
			if e.Kind == "divergence" {
				misses++
				if !strings.Contains(string(e.Result), `"channel":"llm"`) || !strings.Contains(string(e.Result), "moved") {
					t.Errorf("divergence %s, want the LLM request with the changed result", e.Result)
				}
			}
		}
		if misses != 1 {
			t.Errorf("got %d divergences, want 1", misses)
		}
	})
}

// spec §4.3, §9.3: the --mcp checks that need the source.
func TestRunLiveToolsUsageErrors(t *testing.T) {
	dir := t.TempDir()
	recordMCPForReplay(t, dir)
	c := openCassette(t, cassette.Path(dir, "s"))
	redacted := json.RawMessage(`[{"name":"orders","url":"https://REDACTED@orders.example.com/mcp"}]`)
	if err := c.Write(context.Background(), func(tx *cassette.Tx) error {
		return tx.SetMCPServers(context.Background(), redacted)
	}); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"unknown server", []string{"--mcp", "billing=http://127.0.0.1:1"}, `no MCP server named "billing"`},
		{"redacted URL", nil, "MCP server orders: the recorded URL https://REDACTED@orders.example.com/mcp was redacted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := runRunCmd(t, dir, append([]string{"s", "--recorded-command", "--tools", "live", "--as", "x", "--listen", "127.0.0.1:0"}, tc.args...)...)
			if r.code != ExitUsage || !strings.Contains(r.stderr.String(), tc.want) {
				t.Errorf("exit %d, stderr %q; want %d and %q", r.code, r.stderr, ExitUsage, tc.want)
			}
			if _, err := os.Stat(cassette.Path(dir, "x")); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("replay cassette: %v, want none created", err)
			}
		})
	}
}
