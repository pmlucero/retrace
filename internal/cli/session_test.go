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

// errInsertFailed is what failingInsert returns.
var errInsertFailed = errors.New("disk full (injected)")

// failingInsert makes the session insert fail, as a full disk or a locked
// database would. It is the insert of runs through recordRun.insert.
func failingInsert(*cassette.Tx, context.Context, cassette.Session) error { return errInsertFailed }

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
