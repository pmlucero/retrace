package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// AC-48: a replay whose agent outlives --timeout is stopped and is an error,
// and the next replay starts.
func TestCheckTimeoutIsAnErrorAndTheNextReplayRuns(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no SIGTERM")
	}
	dir, out := t.TempDir(), filepath.Join(t.TempDir(), "out")
	recordForReplay(t, dir, "s", 1)
	t.Setenv(replayPromptEnv, "Weather in Paris?")
	t.Setenv(replayStallEnv, "s.exact.1") // only the first replay hangs

	start := time.Now()
	r := runCheckCmd(t, dir, afterDash([]string{"--out", out, "--repeat", "2", "--timeout", "500ms", "--grace", "5s"}, replayAgentCommand())...)
	if d := time.Since(start); d > 30*time.Second {
		t.Errorf("check took %s, want the stalled agent stopped at the timeout", d)
	}
	if r.code != ExitError {
		t.Fatalf("exit %d, want %d\n%s", r.code, ExitError, r.stderr)
	}
	if want := "s.exact.1  error (timed out after 500ms)"; !strings.Contains(r.stderr.String(), want) {
		t.Errorf("stderr lacks %q:\n%s", want, r.stderr)
	}
	// The second replay ran: its outcome depends on how fast its agent starts.
	if !strings.Contains(r.stderr.String(), "s.exact.2  ") {
		t.Errorf("the next replay did not run:\n%s", r.stderr)
	}
	// The stopped replay's cassette is finalized with an existing status.
	s, _ := sessionOf(t, out, "s.exact.1")
	if s.Status != "interrupted" {
		t.Errorf("timed out replay has status %q, want interrupted", s.Status)
	}
}

func TestCheckRejectsANegativeTimeout(t *testing.T) {
	r := runCheckCmd(t, t.TempDir(), "--timeout", "-1s", "--", "true")
	if r.code != ExitUsage || !strings.Contains(r.stderr.String(), "--timeout") {
		t.Errorf("exit %d, stderr %q; want a usage error naming --timeout", r.code, r.stderr)
	}
}

// signalFanout is the Env.notifySignals of a test, which delivers a signal to
// every subscriber, as the kernel does.
type signalFanout struct {
	mu   sync.Mutex
	subs []chan<- os.Signal
}

func (f *signalFanout) notify(c chan<- os.Signal) func() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.subs = append(f.subs, c)
	return func() {}
}

func (f *signalFanout) send(sig os.Signal) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.subs {
		select {
		case c <- sig:
		default:
		}
	}
}

// AC-48: a signal during a check stops the running replay, skips the rest,
// prints the report and exits 1, or 3 when a replay already diverged.
func TestCheckInterrupted(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SIGTERM cannot be sent to a process on Windows")
	}
	for _, tc := range []struct {
		name     string
		prompt   string // what the agents ask: another one than recorded diverges
		wantCode int
		want     []string
	}{
		{"identical so far", "Weather in Paris?", ExitError, []string{"1 error, 1 skipped, 1 identical"}},
		{"after a divergence", "Weather in Rome?", ExitDiverged, []string{"1 diverged"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, out := t.TempDir(), filepath.Join(t.TempDir(), "out")
			for _, id := range []string{"a", "b", "c"} {
				recordForReplay(t, dir, id, 1)
			}
			t.Setenv(replayPromptEnv, tc.prompt)
			t.Setenv(replayStallEnv, "b.exact.1")
			marker := filepath.Join(t.TempDir(), "stalled")
			t.Setenv(replayStallMarkerEnv, marker)
			fan := &signalFanout{}

			done := make(chan recordRun, 1)
			go func() {
				done <- runCheckInto(recordRun{stdout: &syncBuffer{}, stderr: &syncBuffer{}, notify: fan.notify}, dir, afterDash([]string{"--out", out, "--grace", "5s"}, replayAgentCommand())...)
			}()
			// b's agent runs, and so has its replay's subscription, once it
			// has written the marker.
			waitUntil(t, func() bool { _, err := os.Stat(marker); return err == nil })
			fan.send(syscall.SIGTERM)

			var r recordRun
			select {
			case r = <-done:
			case <-time.After(30 * time.Second):
				t.Fatal("check did not end after the signal")
			}
			if r.code != tc.wantCode {
				t.Errorf("exit %d, want %d\n%s", r.code, tc.wantCode, r.stderr)
			}
			if !strings.Contains(r.stdout.String(), "sessions") {
				t.Errorf("stdout has no summary:\n%s", r.stdout)
			}
			if !strings.Contains(r.stdout.String(), "skipped") {
				t.Errorf("stdout reports no skipped replay:\n%s", r.stdout)
			}
			for _, want := range tc.want {
				if !strings.Contains(r.stdout.String(), want) {
					t.Errorf("stdout lacks %q:\n%s", want, r.stdout)
				}
			}
			if _, err := os.Stat(filepath.Join(out, "c.exact.1.cassette")); err == nil {
				t.Error("c was replayed after the signal")
			}
		})
	}
}
