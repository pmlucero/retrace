package cli

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/pmlucero/retrace/internal/cassette"
	"github.com/pmlucero/retrace/internal/derive"
	"github.com/pmlucero/retrace/internal/provider/anthropic"
)

func runReindexCmd(dir string, args ...string) recordRun {
	r := recordRun{stdout: &syncBuffer{}, stderr: &syncBuffer{}}
	all := append([]string{"--dir", dir, "reindex"}, args...)
	r.code = run(context.Background(), all, nil, r.stdout, r.stderr, fakeEnv(nil), commands)
	return r
}

// Spec §7.6, §9.8: reindex rebuilds the events a recording derived.
func TestReindexRebuildsTheEventsOfARecording(t *testing.T) {
	t.Setenv(agentModeEnv, "call")
	dir := t.TempDir()
	up := fakeAnthropic(t)
	if r := runRecordCmd(t, dir, append([]string{"--session", "s", "--listen", "127.0.0.1:0",
		"--anthropic-upstream", up.URL, "--"}, agentCommand()...)...); r.code != ExitOK {
		t.Fatalf("record exit code %d, stderr:\n%s", r.code, r.stderr)
	}

	// Lose the events, as if the writer had dropped them or an older
	// retrace had recorded the session.
	ctx := context.Background()
	c, err := cassette.Open(ctx, cassette.Path(dir, "s"))
	if err != nil {
		t.Fatal(err)
	}
	recorded, _ := c.Events(ctx)
	err = c.Write(ctx, func(tx *cassette.Tx) error {
		if err := tx.DeleteEvents(ctx); err != nil {
			return err
		}
		return tx.SetDerivation(ctx, 0, "")
	})
	if cerr := c.Close(); err != nil || cerr != nil {
		t.Fatal(err, cerr)
	}

	r := runReindexCmd(dir, "s")
	if r.code != ExitOK {
		t.Fatalf("reindex exit code %d, stderr:\n%s", r.code, r.stderr)
	}
	want := fmt.Sprintf("reindexed s: %d events (deriver v%d, pricing %s)\n", len(recorded), derive.Version, anthropic.PricingVersion())
	if r.stdout.String() != want {
		t.Errorf("stdout = %q, want %q", r.stdout, want)
	}
	c = openCassette(t, cassette.Path(dir, "s"))
	rebuilt, _ := c.Events(ctx)
	if len(rebuilt) != 3 || len(rebuilt) != len(recorded) {
		t.Errorf("rebuilt %d events, the recording had %d", len(rebuilt), len(recorded))
	}
	if s, _ := c.Session(ctx); s.DeriverVersion != derive.Version || s.PricingVersion != anthropic.PricingVersion() {
		t.Errorf("session versions = %d, %q", s.DeriverVersion, s.PricingVersion)
	}
}

// AC-26, spec §7.6: reindex warns on stderr when the session keeps its
// dropped events or has interactions marked incomplete, and still exits 0.
func TestReindexWarnsAboutGaps(t *testing.T) {
	tests := []struct {
		name       string
		status     string
		dropped    int
		err        string // the interaction's error
		wantStderr []string
		wantNot    []string
	}{
		{name: "gaps are recovered", status: "completed", dropped: 2,
			wantNot: []string{"dropped_events", "recording incomplete"}},
		{name: "diverged session keeps its count", status: "diverged", dropped: 2,
			wantStderr: []string{"count of dropped events was kept", "dropped_events=2"},
			wantNot:    []string{"recording incomplete"}},
		{name: "incomplete interaction", status: "interrupted",
			err:        cassette.RecordingIncomplete + ": 1 record(s) not written: queue full",
			wantStderr: []string{"have an incomplete recording", "interactions=1"},
			wantNot:    []string{"dropped_events"}},
		{name: "other interaction errors", status: "completed", err: "upstream connection reset",
			wantNot: []string{"dropped_events", "recording incomplete"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			ctx := context.Background()
			c, err := cassette.Create(ctx, cassette.Path(dir, "s"))
			if err != nil {
				t.Fatal(err)
			}
			start := time.Now()
			err = c.Write(ctx, func(tx *cassette.Tx) error {
				if err := tx.InsertSession(ctx, cassette.Session{ID: "s", Mode: "record", Status: "recording",
					CreatedAt: start, Command: []string{"a"}, OnMiss: "strict"}); err != nil {
					return err
				}
				if err := tx.BeginInteraction(ctx, cassette.Interaction{ID: 1, Channel: "llm", Upstream: "anthropic",
					HTTPMethod: "POST", Path: "/v1/messages", StartedAt: start, Provenance: "recorded"}); err != nil {
					return err
				}
				done := cassette.Completion{DurationMillis: 10}
				if tt.err != "" {
					done.Error = &tt.err
				}
				if err := tx.CompleteInteraction(ctx, 1, done); err != nil {
					return err
				}
				return tx.EndSession(ctx, cassette.SessionEnd{Status: tt.status, EndedAt: start, DroppedEvents: tt.dropped})
			})
			if cerr := c.Close(); err != nil || cerr != nil {
				t.Fatal(err, cerr)
			}

			r := runReindexCmd(dir, "s")
			if r.code != ExitOK {
				t.Fatalf("reindex exit code %d, stderr:\n%s", r.code, r.stderr)
			}
			if !strings.HasPrefix(r.stdout.String(), "reindexed s: ") {
				t.Errorf("stdout = %q", r.stdout)
			}
			for _, s := range tt.wantStderr {
				if !strings.Contains(r.stderr.String(), s) {
					t.Errorf("stderr lacks %q:\n%s", s, r.stderr)
				}
			}
			for _, s := range tt.wantNot {
				if strings.Contains(r.stderr.String(), s) {
					t.Errorf("stderr has %q:\n%s", s, r.stderr)
				}
			}
		})
	}
}

func TestReindexErrors(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	c, err := cassette.Create(ctx, cassette.Path(dir, "live"))
	if err != nil {
		t.Fatal(err)
	}
	err = c.Write(ctx, func(tx *cassette.Tx) error {
		return tx.InsertSession(ctx, cassette.Session{ID: "live", Mode: "record", Status: "recording",
			CreatedAt: time.Now(), Command: []string{"a"}, OnMiss: "strict"})
	})
	if cerr := c.Close(); err != nil || cerr != nil {
		t.Fatal(err, cerr)
	}

	tests := []struct {
		name   string
		args   []string
		code   int
		stderr string
	}{
		{"no id", nil, ExitUsage, "exactly one session id"},
		{"two ids", []string{"a", "b"}, ExitUsage, "exactly one session id"},
		{"invalid id", []string{"../x"}, ExitUsage, "session id"},
		{"unknown flag", []string{"--nope", "a"}, ExitUsage, "flag provided but not defined"},
		{"missing session", []string{"nope"}, ExitError, "no such session"},
		{"still recording", []string{"live"}, ExitError, "still recording"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := runReindexCmd(dir, tt.args...)
			if r.code != tt.code || !strings.Contains(r.stderr.String(), tt.stderr) {
				t.Errorf("exit code %d, stderr %q; want %d and %q", r.code, r.stderr, tt.code, tt.stderr)
			}
		})
	}
}

func TestReindexHelp(t *testing.T) {
	r := runReindexCmd(t.TempDir(), "--help")
	if r.code != ExitOK || !strings.Contains(r.stdout.String(), "Usage: retrace reindex <id>") {
		t.Errorf("exit code %d, stdout %q", r.code, r.stdout)
	}
}
