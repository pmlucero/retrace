package agentrun

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/pmlucero/retrace/internal/cassette"
)

// The session still succeeds when Finish fails: the cassette is valid without
// the metadata, so Run only logs a warning (spec §3.1).
func TestRunSucceedsWhenFinishFails(t *testing.T) {
	for _, tc := range []struct {
		name      string
		finishErr error
		wantWarn  bool
	}{
		{"finish fails", errors.New("disk full"), true},
		{"finish succeeds", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(agentModeEnv, "call")
			t.Setenv(agentExitEnv, "0")
			cfg, path := newTestSession(t)
			logs := &syncBuffer{}
			cfg.Logger = slog.New(slog.NewTextHandler(logs, nil))
			cfg.Command = agentCommand()
			calls := 0
			cfg.Finish = func(context.Context, *cassette.Cassette) error {
				calls++
				return tc.finishErr
			}
			res, err := Run(context.Background(), cfg)
			if err != nil {
				t.Fatalf("Run returned %v; a Finish error must not fail the session", err)
			}
			if calls != 1 {
				t.Errorf("Finish called %d times, want 1", calls)
			}
			if res.Status != "completed" || res.Agent.ExitCode != 0 {
				t.Errorf("status %q, agent exit code %d; want completed, 0", res.Status, res.Agent.ExitCode)
			}
			s, err := openCassette(t, path).Session(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if s.Status != "completed" {
				t.Errorf("stored status %q, want completed", s.Status)
			}
			warned := strings.Contains(logs.String(), "cannot complete the session metadata")
			if warned != tc.wantWarn {
				t.Errorf("warning logged = %v, want %v; logs:\n%s", warned, tc.wantWarn, logs.String())
			}
			if tc.wantWarn && !strings.Contains(logs.String(), "disk full") {
				t.Errorf("warning does not carry the error; logs:\n%s", logs.String())
			}
		})
	}
}

// Config.Timeout stops the agent as a cassette miss does (SIGTERM, spec §6.2
// step 3), reports TimedOut and ends the session interrupted (spec §9.9.4).
func TestRunStopsTheAgentWhenTheTimeoutExpires(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no SIGTERM: the agent is killed at once")
	}
	t.Setenv(agentModeEnv, "wait") // runs until a signal ends it
	cfg, path := newTestSession(t)
	cfg.Command = agentCommand()
	cfg.Timeout = 200 * time.Millisecond
	cfg.Grace = time.Minute
	res, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !res.TimedOut || !res.Agent.TimedOut {
		t.Errorf("TimedOut = %v, Agent.TimedOut = %v; want both true", res.TimedOut, res.Agent.TimedOut)
	}
	if res.Status != "interrupted" {
		t.Errorf("status %q, want interrupted", res.Status)
	}
	s, err := openCassette(t, path).Session(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if s.Status != "interrupted" {
		t.Errorf("stored status %q, want interrupted", s.Status)
	}
	if s.AgentExitCode == nil || *s.AgentExitCode != 128+int(syscall.SIGTERM) {
		t.Errorf("agent_exit_code %v, want %d (SIGTERM)", s.AgentExitCode, 128+int(syscall.SIGTERM))
	}
}

// AC-07, AC-28: an agent that exits after retrace received a signal ends the
// session interrupted, unless a cassette miss makes it diverged (spec §3.1,
// §6.2, §9.3).
func TestRunStatusAfterASignal(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("signals cannot be sent to a process on Windows")
	}
	for _, tc := range []struct {
		name     string
		miss     bool
		sig      os.Signal
		want     string
		wantCode int
	}{
		{"signal only", false, syscall.SIGTERM, "interrupted", 128 + int(syscall.SIGTERM)},
		// The agent ignores the miss's SIGTERM, so it is still running when
		// the SIGINT arrives during the grace period (forwarded after 1s).
		{"signal and a miss", true, os.Interrupt, "diverged", 130},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(agentModeEnv, "wait")
			if tc.miss {
				t.Setenv(agentIgnoreTermEnv, "1")
			}
			sigs := make(chan chan<- os.Signal, 1)
			cfg, path := newTestSession(t)
			cfg.NotifySignals = func(c chan<- os.Signal) func() {
				sigs <- c
				return func() {}
			}
			cfg.Command = agentCommand()
			cfg.Grace = time.Minute
			if tc.miss {
				cfg.Handler, cfg.Diverged = divergeOnRequest()
			}
			out := &syncBuffer{}
			cfg.Stdout = out
			go func() {
				c := <-sigs
				// Not waitUntil: t.Fatal must not run outside the test's goroutine.
				deadline := time.Now().Add(30 * time.Second)
				for !strings.Contains(out.String(), "ready") && time.Now().Before(deadline) {
					time.Sleep(10 * time.Millisecond)
				}
				c <- tc.sig
			}()
			res, err := Run(context.Background(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			s, err := openCassette(t, path).Session(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if res.Status != tc.want || s.Status != tc.want {
				t.Errorf("status %q, stored %q; want %q", res.Status, s.Status, tc.want)
			}
			if !res.Agent.Interrupted {
				t.Error("Agent.Interrupted = false, want true")
			}
			if s.AgentExitCode == nil || *s.AgentExitCode != tc.wantCode {
				t.Errorf("agent_exit_code %v, want %d", s.AgentExitCode, tc.wantCode)
			}
		})
	}
}
