package cli

import "github.com/pmlucero/retrace/internal/agentrun"

// ExitAgentFailed is returned when the agent exits with a non-zero code
// (spec §9.7).
const ExitAgentFailed = 4

// sessionExitCode maps how the session ended to retrace's exit code (spec §9.7).
func sessionExitCode(r agentrun.Result) int {
	switch {
	case r.Status == "failed":
		return ExitError
	case r.Status == "diverged":
		return ExitDiverged
	case r.Agent.ExitCode != 0:
		return ExitAgentFailed
	}
	return ExitOK
}
