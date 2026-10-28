package cli

import (
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"strings"
	"syscall"
	"time"
)

// ExitAgentFailed is returned when the agent exits with a non-zero code
// (spec §9.7).
const ExitAgentFailed = 4

// interruptGrace is how long retrace waits after SIGINT before forwarding it:
// a terminal's Ctrl+C has already reached the agent (spec §3.1).
const interruptGrace = time.Second

// notifySignals subscribes c to the signals that end a session and returns a
// function that unsubscribes it. Tests replace it to send signals directly.
var notifySignals = func(c chan<- os.Signal) (stop func()) {
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	return func() { signal.Stop(c) }
}

// agentResult is how the agent process ended.
type agentResult struct {
	exitCode    int
	interrupted bool // retrace received a signal while the agent ran
}

// waitAgent waits for cmd, which has started, while handling the signals that
// arrive on sigs: the first is forwarded to the agent (SIGINT after
// interruptGrace), a second one kills it.
func waitAgent(cmd *exec.Cmd, sigs <-chan os.Signal, log *slog.Logger) agentResult {
	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait() // the exit status is read from cmd.ProcessState
		close(exited)
	}()

	var (
		res     agentResult
		forward <-chan time.Time
		pending os.Signal
	)
	for {
		select {
		case <-exited:
			// A terminal's Ctrl+C reaches both processes at once, and the agent
			// may exit before select sees the signal (spec §3.1).
			select {
			case <-sigs:
				res.interrupted = true
			default:
			}
			res.exitCode = exitCodeOf(cmd.ProcessState)
			return res
		case sig := <-sigs:
			if res.interrupted {
				_ = cmd.Process.Kill()
				continue
			}
			res.interrupted = true
			log.Info("stopping: waiting for the agent to exit; send the signal again to kill it", "signal", sig)
			if sig == os.Interrupt {
				pending, forward = sig, time.After(interruptGrace)
				continue
			}
			signalAgent(cmd, sig)
		case <-forward:
			signalAgent(cmd, pending)
			forward = nil
		}
	}
}

// signalAgent delivers sig, or kills the agent where sig cannot be sent
// (os.Interrupt on Windows).
func signalAgent(cmd *exec.Cmd, sig os.Signal) {
	if err := cmd.Process.Signal(sig); err != nil && !errors.Is(err, os.ErrProcessDone) {
		_ = cmd.Process.Kill()
	}
}

// exitCodeOf returns the agent's exit code, or 128 + the signal number when a
// signal terminated it (spec §3.1).
func exitCodeOf(state *os.ProcessState) int {
	if ws, ok := state.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return state.ExitCode()
}

var nonAlnum = regexp.MustCompile(`[^A-Z0-9]`)

// mcpEnvName returns the variable that carries an MCP server's URL:
// RETRACE_MCP_<NAME>_URL, with NAME upper-cased and every other character
// replaced by '_' (spec §3.2).
func mcpEnvName(name string) string {
	return "RETRACE_MCP_" + nonAlnum.ReplaceAllString(strings.ToUpper(name), "_") + "_URL"
}

// agentEnv returns the variables injected into the agent (spec §3.2), for a
// proxy listening at base.
func agentEnv(base, id, mode string, mcp []string) map[string]string {
	vars := map[string]string{
		"ANTHROPIC_BASE_URL": base + "/llm/anthropic",
		"RETRACE_SESSION_ID": id,
		"RETRACE_MODE":       mode,
	}
	for _, name := range mcp {
		vars[mcpEnvName(name)] = base + "/mcp/" + name
	}
	return vars
}

// withEnv returns environ with each of set replacing any earlier value.
func withEnv(environ []string, set map[string]string) []string {
	out := make([]string, 0, len(environ)+len(set))
	for _, kv := range environ {
		key, _, _ := strings.Cut(kv, "=")
		if _, replaced := set[key]; !replaced {
			out = append(out, kv)
		}
	}
	for key, value := range set {
		out = append(out, key+"="+value)
	}
	return out
}
