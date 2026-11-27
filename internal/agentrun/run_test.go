package agentrun

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/pmlucero/retrace/internal/cassette"
)

// The agent in these tests is this test binary, run again as TestHelperAgent
// with RETRACE_TEST_AGENT set to what it should do.
const (
	agentModeEnv = "RETRACE_TEST_AGENT"
	agentExitEnv = "RETRACE_TEST_AGENT_EXIT"
	// agentIgnoreTermEnv makes a helper agent ignore SIGTERM, so it runs
	// to its end after a cassette miss (spec §6.2 step 3).
	agentIgnoreTermEnv = "RETRACE_TEST_AGENT_IGNORE_TERM"
	cwdAgentEnv        = "RETRACE_TEST_CWD_AGENT"
)

func agentCommand() []string {
	return []string{os.Args[0], "-test.run=^TestHelperAgent$"}
}

// TestHelperAgent is the fake agent. It makes one call to the proxy, then
// exits with agentExitEnv's code, or in "wait" mode runs until SIGINT. It
// exits without returning so the testing framework prints nothing to stdout.
func TestHelperAgent(t *testing.T) {
	mode := os.Getenv(agentModeEnv)
	if mode == "" {
		t.Skip("subprocess of the session tests")
	}
	if os.Getenv(agentIgnoreTermEnv) != "" {
		signal.Ignore(syscall.SIGTERM)
	}
	if mode == "wait" {
		signal.Notify(make(chan os.Signal, 1), os.Interrupt) // survive until the test is ready
	}
	// The URL is the proxy that retrace injected: calling it is the point.
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, os.Getenv("ANTHROPIC_BASE_URL")+"/v1/messages", strings.NewReader(`{}`)) //nolint:gosec // G704
	resp, err := http.DefaultClient.Do(req)                                                                                                              //nolint:gosec // G704: see above
	if err != nil {
		fmt.Fprintln(os.Stderr, "agent request failed:", err)
		os.Exit(99)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	_ = json.NewEncoder(os.Stdout).Encode(map[string]string{"mode": os.Getenv("RETRACE_MODE")})

	if mode == "wait" {
		sigs := make(chan os.Signal, 1)
		signal.Notify(sigs, os.Interrupt)
		fmt.Println("ready")
		<-sigs
		os.Exit(130)
	}
	code, _ := strconv.Atoi(os.Getenv(agentExitEnv))
	os.Exit(code)
}

// TestHelperCwdAgent prints its working directory. It only runs as a
// subprocess of TestRunRunsTheAgentInCwd.
func TestHelperCwdAgent(t *testing.T) {
	if os.Getenv(cwdAgentEnv) == "" {
		t.Skip("subprocess of the session tests")
	}
	wd, _ := os.Getwd()
	fmt.Print(wd)
	os.Exit(0)
}

// syncBuffer is a bytes.Buffer that the agent's output copier and the test
// can use at the same time.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func openCassette(t *testing.T, path string) *cassette.Cassette {
	t.Helper()
	c, err := cassette.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("opening %s: %v", path, err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// newTestSession returns a session config with a fresh cassette, its writer
// and a loopback listener, and the path of the cassette.
func newTestSession(t *testing.T) (Config, string) {
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
	return Config{Logger: slog.New(slog.DiscardHandler), Stdout: io.Discard, Stderr: io.Discard, Listener: ln, Handler: http.NotFoundHandler(), Cassette: c, Writer: w, ID: "s", Mode: "record", StartMsg: "recording"}, path
}

func TestRunRunsTheAgentInCwd(t *testing.T) {
	t.Setenv(cwdAgentEnv, "1")
	cfg, _ := newTestSession(t)
	cwd := t.TempDir()
	cfg.Command = []string{os.Args[0], "-test.run=^TestHelperCwdAgent$"}
	cfg.Cwd = cwd
	out := &syncBuffer{}
	cfg.Stdout = out
	res, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "completed" || res.Agent.ExitCode != 0 {
		t.Errorf("status %q, agent exit code %d; want completed, 0", res.Status, res.Agent.ExitCode)
	}
	want, _ := filepath.EvalSymlinks(cwd)
	if got, _ := filepath.EvalSymlinks(strings.TrimSpace(out.String())); got != want {
		t.Errorf("agent ran in %q, want %q", got, want)
	}
}

// A replayed session's command comes from the cassette, which is untrusted:
// an empty one fails the session instead of crashing retrace.
func TestRunFailsWithoutACommand(t *testing.T) {
	cfg, path := newTestSession(t)
	res, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "failed" {
		t.Errorf("status %q; want failed", res.Status)
	}
	s, err := openCassette(t, path).Session(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if s.Status != "failed" || s.AgentExitCode != nil {
		t.Errorf("session status %q, agent exit code %v; want failed and none", s.Status, s.AgentExitCode)
	}
}

// divergeOnRequest returns a handler that closes the returned channel at the
// agent's first request, as a cassette miss would, and answers 404.
func divergeOnRequest() (http.Handler, <-chan struct{}) {
	diverged := make(chan struct{})
	var once sync.Once
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(diverged) })
		http.NotFound(w, r)
	}), diverged
}

// AC-28: a cassette miss ends the session diverged, however the agent then exits,
// and agent_exit_code keeps its code (spec §6.2, §9.3), but a session whose
// agent never ran stays failed.
func TestRunEndsDivergedAfterAMiss(t *testing.T) {
	for _, tc := range []struct {
		name      string
		mode      string // the helper agent's mode; "" runs no agent
		exit      string
		interrupt bool
		want      string
		wantCode  *int
	}{
		// The agent ignores the miss's SIGTERM and exits within the grace
		// period, so it is not killed.
		{"agent exits 0", "call", "0", false, "diverged", ptr(0)},
		{"agent exits non-zero", "call", "3", false, "diverged", ptr(3)},
		// A signal during the grace period is handled as in spec §3.1.
		{"interrupted", "wait", "", true, "diverged", ptr(130)},
		{"agent never ran", "", "", false, "failed", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.interrupt && runtime.GOOS == "windows" {
				t.Skip("SIGINT cannot be sent to a process on Windows")
			}
			t.Setenv(agentModeEnv, tc.mode)
			t.Setenv(agentExitEnv, tc.exit)
			t.Setenv(agentIgnoreTermEnv, "1")
			sigs := make(chan chan<- os.Signal, 1)

			cfg, path := newTestSession(t)
			cfg.NotifySignals = func(c chan<- os.Signal) func() {
				sigs <- c
				return func() {}
			}
			cfg.Grace = time.Minute
			if tc.mode != "" {
				cfg.Command = agentCommand()
				cfg.Handler, cfg.Diverged = divergeOnRequest()
			} else {
				diverged := make(chan struct{})
				close(diverged)
				cfg.Diverged = diverged
			}
			out := &syncBuffer{}
			cfg.Stdout = out
			cfg.Stdout = out
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
			start := time.Now()
			res, err := Run(context.Background(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			if d := time.Since(start); d > 30*time.Second {
				t.Errorf("session took %s; the agent was not let exit on its own", d)
			}
			s, err := openCassette(t, path).Session(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if res.Status != tc.want || s.Status != tc.want {
				t.Errorf("status %q, stored %q; want %q", res.Status, s.Status, tc.want)
			}
			if (s.AgentExitCode == nil) != (tc.wantCode == nil) || (s.AgentExitCode != nil && *s.AgentExitCode != *tc.wantCode) {
				t.Errorf("agent_exit_code %v, want %v", s.AgentExitCode, tc.wantCode)
			}
		})
	}
}

// AC-15 (termination only; the divergence report has its own test): at a
// cassette miss the agent gets SIGTERM, then SIGKILL if it has not exited
// after --grace, or at once with --grace 0 (spec §6.2 step 3).
func TestRunTerminatesTheAgentOnAMiss(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no SIGTERM: the agent is killed at once")
	}
	const (
		sigterm = 128 + 15
		sigkill = 128 + 9
	)
	for _, tc := range []struct {
		name       string
		ignoreTerm bool
		grace      time.Duration
		wantCode   int
	}{
		{"exits on SIGTERM", false, time.Minute, sigterm},
		{"killed after the grace period", true, 200 * time.Millisecond, sigkill},
		{"grace 0 kills at once", false, 0, sigkill},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(agentModeEnv, "wait") // runs until a signal ends it
			if tc.ignoreTerm {
				t.Setenv(agentIgnoreTermEnv, "1")
			}
			cfg, path := newTestSession(t)
			cfg.Command = agentCommand()
			cfg.Handler, cfg.Diverged = divergeOnRequest()
			cfg.Grace = tc.grace
			start := time.Now()
			res, err := Run(context.Background(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			d := time.Since(start)
			if d > 30*time.Second {
				t.Errorf("session took %s, want the agent ended at the miss", d)
			}
			if tc.ignoreTerm && d < tc.grace {
				t.Errorf("agent killed after %s, want no sooner than --grace %s", d, tc.grace)
			}
			s, err := openCassette(t, path).Session(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if res.Status != "diverged" ||
				s.AgentExitCode == nil || *s.AgentExitCode != tc.wantCode {
				t.Errorf("status %q, agent_exit_code %v; want diverged and %d",
					res.Status, s.AgentExitCode, tc.wantCode)
			}
		})
	}
}

func ptr(n int) *int { return &n }

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
