package agentrun

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"slices"
	"testing"
)

func TestMCPEnvName(t *testing.T) {
	for name, want := range map[string]string{
		"orders":    "RETRACE_MCP_ORDERS_URL",
		"orders-db": "RETRACE_MCP_ORDERS_DB_URL",
		"a_b9":      "RETRACE_MCP_A_B9_URL",
	} {
		if got := mcpEnvName(name); got != want {
			t.Errorf("mcpEnvName(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestWithEnvReplacesInheritedValues(t *testing.T) {
	got := withEnv([]string{"PATH=/bin", "ANTHROPIC_BASE_URL=https://api.anthropic.com"},
		map[string]string{"ANTHROPIC_BASE_URL": "http://127.0.0.1:1/llm/anthropic"})
	want := []string{"PATH=/bin", "ANTHROPIC_BASE_URL=http://127.0.0.1:1/llm/anthropic"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("withEnv = %v, want %v", got, want)
	}
}

// A terminal's Ctrl+C reaches the agent and retrace together. Even when the
// agent's exit is seen first, the session counts as interrupted (spec §3.1).
func TestWaitAgentCountsASignalThatRacesTheExit(t *testing.T) {
	cmd := exec.CommandContext(context.Background(), os.Args[0], "-test.run=^$") //nolint:gosec // G204: this test binary, exiting at once
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	sigs := make(chan os.Signal, 1)
	sigs <- os.Interrupt
	res := waitAgent(cmd, sigs, nil, nil, 0, slog.New(slog.DiscardHandler))
	if !res.Interrupted {
		t.Error("the session was not marked as interrupted")
	}
}

// AC-37: guardHosts names the proxy by the --listen host and by the
// listener's address.
func TestGuardHosts(t *testing.T) {
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	got := guardHosts(Config{Listener: ln, Listen: "myhost.internal:4790"})
	if want := []string{"myhost.internal", "127.0.0.1"}; !slices.Equal(got, want) {
		t.Errorf("guardHosts = %q, want %q", got, want)
	}
	if got := guardHosts(Config{Listener: ln}); !slices.Equal(got, []string{"127.0.0.1"}) {
		t.Errorf("guardHosts without --listen = %q, want only the listener's host", got)
	}
}
