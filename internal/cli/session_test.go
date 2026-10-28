package cli

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
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

// newTestSession returns a session config with a fresh cassette, its writer
// and a loopback listener, and the path of the cassette.
func newTestSession(t *testing.T) (sessionConfig, string) {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "s.cassette")
	c, err := cassette.Create(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	err = c.Write(ctx, func(tx *cassette.Tx) error {
		return tx.InsertSession(ctx, cassette.Session{ID: "s", Mode: "record", Status: "recording", OnMiss: "strict"})
	})
	if err != nil {
		t.Fatal(err)
	}
	w, err := cassette.NewWriter(ctx, c, cassette.WriterOptions{Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	return sessionConfig{listener: ln, handler: http.NotFoundHandler(), cassette: c, writer: w, id: "s", mode: "record", startMsg: "recording"}, path
}

func TestRunSessionRunsTheAgentInCwd(t *testing.T) {
	t.Setenv(cwdAgentEnv, "1")
	cfg, _ := newTestSession(t)
	cwd := t.TempDir()
	cfg.command = []string{os.Args[0], "-test.run=^TestHelperCwdAgent$"}
	cfg.cwd = cwd
	out := &syncBuffer{}
	env := &Env{Stdout: out, Stderr: &syncBuffer{}, Logger: slog.New(slog.DiscardHandler)}
	res, err := runSession(context.Background(), env, cfg)
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

// A replayed session's command comes from the cassette, which is untrusted:
// an empty one fails the session instead of crashing retrace.
func TestRunSessionFailsWithoutACommand(t *testing.T) {
	cfg, path := newTestSession(t)
	env := &Env{Stdout: &syncBuffer{}, Stderr: &syncBuffer{}, Logger: slog.New(slog.DiscardHandler)}
	res, err := runSession(context.Background(), env, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if res.status != "failed" || res.exitCode() != ExitError {
		t.Errorf("status %q, exit code %d; want failed, %d", res.status, res.exitCode(), ExitError)
	}
	s, err := openCassette(t, path).Session(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if s.Status != "failed" || s.AgentExitCode != nil {
		t.Errorf("session status %q, agent exit code %v; want failed and none", s.Status, s.AgentExitCode)
	}
}

// A cassette miss ends the session diverged, however the agent then exits
// (spec §6.2, §9.3), but a session whose agent never ran stays failed.
func TestRunSessionEndsDivergedAfterAMiss(t *testing.T) {
	t.Setenv(cwdAgentEnv, "1")
	diverged := make(chan struct{})
	close(diverged)
	for _, tc := range []struct {
		command []string
		want    string
	}{
		{[]string{os.Args[0], "-test.run=^TestHelperCwdAgent$"}, "diverged"},
		{nil, "failed"},
	} {
		cfg, path := newTestSession(t)
		cfg.command, cfg.diverged = tc.command, diverged
		env := &Env{Stdout: &syncBuffer{}, Stderr: &syncBuffer{}, Logger: slog.New(slog.DiscardHandler)}
		res, err := runSession(context.Background(), env, cfg)
		if err != nil {
			t.Fatal(err)
		}
		s, err := openCassette(t, path).Session(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if res.status != tc.want || s.Status != tc.want {
			t.Errorf("command %q: status %q, stored %q; want %q", tc.command, res.status, s.Status, tc.want)
		}
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
		{sessionResult{status: "diverged", agent: agentResult{exitCode: 1}}, ExitDiverged},
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
	if !maps.Equal(got, want) {
		t.Errorf("agentEnv = %v, want %v", got, want)
	}
}
