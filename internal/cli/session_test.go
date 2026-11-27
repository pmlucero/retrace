package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"testing"

	"github.com/pmlucero/retrace/internal/agentrun"
	"github.com/pmlucero/retrace/internal/cassette"
)

const cwdAgentEnv = "RETRACE_TEST_CWD_AGENT"

// TestHelperCwdAgent prints its working directory. It only runs as a
// subprocess of TestRunSessionRunsTheAgentInCwd and of the run tests that
// check whether, and where, the agent ran.
func TestHelperCwdAgent(t *testing.T) {
	if os.Getenv(cwdAgentEnv) == "" {
		t.Skip("subprocess of the session tests")
	}
	wd, _ := os.Getwd()
	fmt.Print(wd)
	os.Exit(0)
}

// errInsertFailed is what failSessionInsert makes the session insert return.
var errInsertFailed = errors.New("disk full (injected)")

// failSessionInsert makes every session insert fail until the test ends, as a
// full disk or a locked database would.
func failSessionInsert(t *testing.T) {
	t.Helper()
	orig := insertSession
	insertSession = func(*cassette.Tx, context.Context, cassette.Session) error { return errInsertFailed }
	t.Cleanup(func() { insertSession = orig })
}

// assertDirHolds fails the test unless dir holds exactly the named files: no
// other cassette and no -wal or -shm file.
func assertDirHolds(t *testing.T, dir string, want ...string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range entries {
		got = append(got, e.Name())
	}
	if !slices.Equal(got, want) {
		t.Errorf("%s holds %v, want %v", dir, got, want)
	}
}

// #56: a cassette whose session row cannot be written is removed.
func TestCreateSessionCassetteRemovesTheFileWhenTheInsertFails(t *testing.T) {
	failSessionInsert(t)
	dir := t.TempDir()
	path := cassette.Path(dir, "s")
	_, err := createSessionCassette(context.Background(), path, cassette.Session{ID: "s", Mode: "record", Status: "recording", OnMiss: "strict"})
	if !errors.Is(err, errInsertFailed) {
		t.Fatalf("err = %v, want the insert's error", err)
	}
	assertDirHolds(t, dir)
}

// The cassette of another session at the same path is never removed.
func TestCreateSessionCassetteKeepsAnExistingFile(t *testing.T) {
	failSessionInsert(t)
	dir := t.TempDir()
	path := cassette.Path(dir, "s")
	if err := os.WriteFile(path, []byte("another session"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := createSessionCassette(context.Background(), path, cassette.Session{ID: "s"}); !errors.Is(err, cassette.ErrExists) {
		t.Fatalf("err = %v, want ErrExists", err)
	}
	data, _ := os.ReadFile(path) //nolint:gosec // G304: the file this test created
	if string(data) != "another session" {
		t.Errorf("existing file now holds %q", data)
	}
}

func TestSessionExitCode(t *testing.T) {
	for _, tc := range []struct {
		res  agentrun.Result
		want int
	}{
		{agentrun.Result{Status: "completed"}, ExitOK},
		{agentrun.Result{Status: "completed", Agent: agentrun.AgentResult{ExitCode: 2}}, ExitAgentFailed},
		{agentrun.Result{Status: "interrupted", Agent: agentrun.AgentResult{ExitCode: 130, Interrupted: true}}, ExitAgentFailed},
		{agentrun.Result{Status: "interrupted", Agent: agentrun.AgentResult{Interrupted: true}}, ExitOK},
		{agentrun.Result{Status: "failed"}, ExitError},
		{agentrun.Result{Status: "diverged", Agent: agentrun.AgentResult{ExitCode: 1}}, ExitDiverged},
	} {
		if got := sessionExitCode(tc.res); got != tc.want {
			t.Errorf("%+v: exit code %d, want %d", tc.res, got, tc.want)
		}
	}
}
