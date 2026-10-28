package cli

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pmlucero/retrace/internal/cassette"
)

const cwdAgentEnv = "RETRACE_TEST_CWD_AGENT"

// TestHelperCwdAgent prints its working directory. It only runs as a
// subprocess of TestRunSessionRunsTheAgentInCwd.
func TestHelperCwdAgent(t *testing.T) {
	if os.Getenv(cwdAgentEnv) == "" {
		t.Skip("subprocess of the session tests")
	}
	wd, _ := os.Getwd()
	fmt.Print(wd)
	os.Exit(0)
}

func TestRunSessionRunsTheAgentInCwd(t *testing.T) {
	t.Setenv(cwdAgentEnv, "1")
	ctx := context.Background()
	dir := t.TempDir()
	c, err := cassette.Create(ctx, filepath.Join(dir, "s.cassette"))
	if err != nil {
		t.Fatal(err)
	}
	err = c.Write(ctx, func(tx *cassette.Tx) error {
		return tx.InsertSession(ctx, cassette.Session{ID: "s", Mode: "record", Status: "recording", OnMiss: "strict"})
	})
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.DiscardHandler)
	w, err := cassette.NewWriter(ctx, c, cassette.WriterOptions{Logger: log})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	cwd := t.TempDir()
	out := &syncBuffer{}
	env := &Env{Stdout: out, Stderr: &syncBuffer{}, Logger: log}
	res, err := runSession(ctx, env, sessionConfig{
		listener: ln, handler: http.NotFoundHandler(), cassette: c, writer: w,
		id: "s", mode: "record", command: []string{os.Args[0], "-test.run=^TestHelperCwdAgent$"}, cwd: cwd,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.status != "completed" || res.exitCode() != ExitOK {
		t.Errorf("status %q, exit code %d; want completed, %d", res.status, res.exitCode(), ExitOK)
	}
	want, _ := filepath.EvalSymlinks(cwd)
	if got, _ := filepath.EvalSymlinks(strings.TrimSpace(out.String())); got != want {
		t.Errorf("agent ran in %q, want %q", got, want)
	}
}

func TestSessionResultExitCode(t *testing.T) {
	for _, tc := range []struct {
		res  sessionResult
		want int
	}{
		{sessionResult{status: "completed"}, ExitOK},
		{sessionResult{status: "completed", agent: agentResult{exitCode: 2}}, ExitAgentFailed},
		{sessionResult{status: "interrupted", agent: agentResult{exitCode: 130, interrupted: true}}, ExitAgentFailed},
		{sessionResult{status: "interrupted", agent: agentResult{interrupted: true}}, ExitOK},
		{sessionResult{status: "failed"}, ExitError},
	} {
		if got := tc.res.exitCode(); got != tc.want {
			t.Errorf("%+v: exit code %d, want %d", tc.res, got, tc.want)
		}
	}
}

func TestAgentEnv(t *testing.T) {
	got := agentEnv("http://127.0.0.1:1", "s1", "exact", []string{"orders-db"})
	want := map[string]string{
		"ANTHROPIC_BASE_URL":        "http://127.0.0.1:1/llm/anthropic",
		"RETRACE_SESSION_ID":        "s1",
		"RETRACE_MODE":              "exact",
		"RETRACE_MCP_ORDERS_DB_URL": "http://127.0.0.1:1/mcp/orders-db",
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("agentEnv = %v, want %v", got, want)
	}
}
