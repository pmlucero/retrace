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
	"runtime"
	"strings"
	"testing"
	"time"

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

// AC-28: a cassette miss ends the session diverged, however the agent then exits,
// and agent_exit_code keeps its code (spec §6.2, §9.3), but a session whose
// agent never ran stays failed.
func TestRunSessionEndsDivergedAfterAMiss(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SIGINT cannot be sent to a process on Windows")
	}
	diverged := make(chan struct{})
	close(diverged)
	for _, tc := range []struct {
		name      string
		mode      string // the helper agent's mode; "" runs no agent
		exit      string
		interrupt bool
		want      string
		wantCode  *int
		wantExit  int
	}{
		{"agent exits 0", "call", "0", false, "diverged", ptr(0), ExitDiverged},
		{"agent exits non-zero", "call", "3", false, "diverged", ptr(3), ExitDiverged},
		{"interrupted", "wait", "", true, "diverged", ptr(130), ExitDiverged},
		{"agent never ran", "", "", false, "failed", nil, ExitError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(agentModeEnv, tc.mode)
			t.Setenv(agentExitEnv, tc.exit)
			sigs := make(chan chan<- os.Signal, 1)
			saved := notifySignals
			notifySignals = func(c chan<- os.Signal) func() {
				sigs <- c
				return func() {}
			}
			t.Cleanup(func() { notifySignals = saved })

			cfg, path := newTestSession(t)
			cfg.diverged = diverged
			if tc.mode != "" {
				cfg.command = agentCommand()
			}
			out := &syncBuffer{}
			env := &Env{Stdout: out, Stderr: &syncBuffer{}, Logger: slog.New(slog.DiscardHandler)}
			if tc.interrupt {
				go func() {
					c := <-sigs
					// Not waitUntil: t.Fatal must not run outside the test's goroutine.
					deadline := time.Now().Add(30 * time.Second)
					for !strings.Contains(out.String(), "ready") && time.Now().Before(deadline) {
						time.Sleep(10 * time.Millisecond)
					}
					c <- os.Interrupt
				}()
			}
			res, err := runSession(context.Background(), env, cfg)
			if err != nil {
				t.Fatal(err)
			}
			s, err := openCassette(t, path).Session(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if res.status != tc.want || s.Status != tc.want || res.exitCode() != tc.wantExit {
				t.Errorf("status %q, stored %q, exit %d; want %q and %d", res.status, s.Status, res.exitCode(), tc.want, tc.wantExit)
			}
			if (s.AgentExitCode == nil) != (tc.wantCode == nil) || (s.AgentExitCode != nil && *s.AgentExitCode != *tc.wantCode) {
				t.Errorf("agent_exit_code %v, want %v", s.AgentExitCode, tc.wantCode)
			}
		})
	}
}

func ptr(n int) *int { return &n }

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
