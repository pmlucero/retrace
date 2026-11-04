package cli

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"os/exec"
	"time"

	"github.com/pmlucero/retrace/internal/cassette"
)

// finishTimeout bounds each step of ending a session: waiting for in-flight
// interactions, then writing what is queued (spec §3.1).
const finishTimeout = 10 * time.Second

// insertSession writes a new cassette's session row. It is a variable so that
// tests can make the insert fail. Tests that replace it must not run in
// parallel with tests that create sessions.
var insertSession = (*cassette.Tx).InsertSession

// createSessionCassette creates the cassette at path with session row s. If
// the row cannot be written, it closes and removes the file, so a failed start
// leaves no cassette without a session and the id stays free (#56). Errors
// from cassette.Create are wrapped, so callers can test for
// cassette.ErrExists. Errors name the path or the session already.
func createSessionCassette(ctx context.Context, path string, s cassette.Session) (*cassette.Cassette, error) {
	c, err := cassette.Create(ctx, path)
	if err != nil {
		return nil, err
	}
	if err := c.Write(ctx, func(tx *cassette.Tx) error { return insertSession(tx, ctx, s) }); err != nil {
		// Create made this file (it refuses an existing one), so removing it
		// cannot touch another session's cassette.
		return nil, errors.Join(err, c.Close(), cassette.Remove(path))
	}
	return c, nil
}

// replaceSessionCassette creates the cassette at path with session row s,
// replacing the cassette there, if any (record --force, spec §9.2). The old
// cassette is kept unless the new one gets its session row (#111).
func replaceSessionCassette(ctx context.Context, path string, s cassette.Session) (*cassette.Cassette, error) {
	return cassette.Replace(ctx, path, func(tx *cassette.Tx) error { return insertSession(tx, ctx, s) })
}

// sessionConfig is what runSession needs to run one agent session against a
// local proxy, for recording or for replay.
type sessionConfig struct {
	listener net.Listener       // already listening on --listen
	handler  http.Handler       // serves /llm/... and /mcp/<name>/...
	cassette *cassette.Cassette // closed by runSession
	writer   *cassette.Writer   // closed by runSession with the session's end
	id       string             // RETRACE_SESSION_ID
	mode     string             // RETRACE_MODE
	startMsg string             // logged when the session starts, e.g. "recording"
	mcp      []string           // registered MCP server names
	command  []string           // the agent command; an empty one fails the session
	cwd      string             // the agent's working directory; "" inherits retrace's
	diverged <-chan struct{}    // closed at a cassette miss in a replay; nil when recording
	grace    time.Duration      // how long the agent has to exit after a miss's SIGTERM (spec §6.2)
	// finish, if set, completes the session's metadata from what was
	// recorded. It runs once the writer has written everything.
	finish func(ctx context.Context, c *cassette.Cassette) error
}

// sessionResult is how a session ended.
type sessionResult struct {
	status string // the session status written to the cassette (spec §3.1)
	agent  agentResult
	stats  cassette.WriterStats
}

// runSession serves cfg.handler on cfg.listener, runs the agent with the
// injected environment (spec §3.2) until it exits, then finalizes the
// cassette. It returns an error, already logged, only when the cassette may be
// incomplete.
func runSession(ctx context.Context, env *Env, cfg sessionConfig) (sessionResult, error) {
	log := env.Logger
	srv := &http.Server{Handler: cfg.handler, ReadHeaderTimeout: 30 * time.Second}
	go func() {
		if err := srv.Serve(cfg.listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("proxy stopped", "error", err)
		}
	}()

	base := "http://" + cfg.listener.Addr().String()

	// Signals are handled until the cassette is closed: one that arrives while
	// the session is being finished must not kill retrace halfway.
	sigs := make(chan os.Signal, 2)
	stopSignals := notifySignals(sigs)
	defer stopSignals()

	path := cfg.cassette.Path()
	log.Info(cfg.startMsg, "session", cfg.id, "cassette", path, "proxy", base)
	res := sessionResult{status: "completed"}
	if cmd := startAgent(ctx, env, cfg, base); cmd == nil {
		res.status = "failed"
	} else {
		res.agent = waitAgent(cmd, sigs, cfg.diverged, cfg.grace, log)
		if res.agent.interrupted {
			res.status = "interrupted"
		}
	}

	// The agent is gone; let in-flight exchanges finish, then write what is queued.
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), finishTimeout)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		log.Warn("in-flight requests did not finish in time; they are recorded as incomplete", "error", err)
		_ = srv.Close()
	}
	// Checked after the shutdown, so a miss among the last requests counts. A
	// miss outweighs how the agent then ended (spec §6.2, §9.3).
	select {
	case <-cfg.diverged:
		if res.status != "failed" {
			res.status = "diverged"
		}
	default:
	}
	end := cassette.SessionEnd{Status: res.status, EndedAt: time.Now()}
	if res.status != "failed" {
		end.AgentExitCode = &res.agent.exitCode
	}
	wctx, cancelW := context.WithTimeout(context.WithoutCancel(ctx), finishTimeout)
	defer cancelW()
	werr := cfg.writer.Close(wctx, end)
	if werr == nil && cfg.finish != nil {
		// A step of its own, with its own deadline. The cassette is valid
		// without it, so the session still succeeds.
		fctx, cancelF := context.WithTimeout(context.WithoutCancel(ctx), finishTimeout)
		defer cancelF()
		if err := cfg.finish(fctx, cfg.cassette); err != nil {
			log.Warn("cannot complete the session metadata", "path", path, "error", err)
		}
	}
	if err := errors.Join(werr, cfg.cassette.Close()); err != nil {
		log.Error("the cassette may be incomplete", "path", path, "error", err)
		return res, err
	}
	res.stats = cfg.writer.Stats()
	return res, nil
}

// startAgent starts cfg.command with the injected environment (spec §3.2), or
// logs why it cannot and returns nil. The command may come from a cassette,
// which is untrusted input, so an empty one is not assumed away.
func startAgent(ctx context.Context, env *Env, cfg sessionConfig, base string) *exec.Cmd {
	if len(cfg.command) == 0 {
		env.Logger.Error("cannot start the agent: no agent command")
		return nil
	}
	cmd := exec.CommandContext(ctx, cfg.command[0], cfg.command[1:]...) //nolint:gosec // G204: running the user's agent is the point
	cmd.Dir = cfg.cwd
	cmd.Env = withEnv(os.Environ(), agentEnv(base, cfg.id, cfg.mode, cfg.mcp))
	cmd.Stdin, cmd.Stdout, cmd.Stderr = env.Stdin, env.Stdout, env.Stderr
	if err := cmd.Start(); err != nil {
		env.Logger.Error("cannot start the agent", "command", cfg.command[0], "error", err)
		return nil
	}
	return cmd
}

// exitCode maps how the session ended to retrace's exit code (spec §9.7).
func (r sessionResult) exitCode() int {
	switch {
	case r.status == "failed":
		return ExitError
	case r.status == "diverged":
		return ExitDiverged
	case r.agent.exitCode != 0:
		return ExitAgentFailed
	}
	return ExitOK
}
