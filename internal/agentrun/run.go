package agentrun

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"github.com/pmlucero/retrace/internal/cassette"
	"github.com/pmlucero/retrace/internal/proxy"
)

// finishTimeout bounds each step of ending a session: waiting for in-flight
// interactions, then writing what is queued (spec §3.1).
const finishTimeout = 10 * time.Second

// Config is what Run needs to run one agent session against a local proxy,
// for recording or for replay.
type Config struct {
	Logger *slog.Logger
	Stdin  io.Reader // the agent's standard input
	Stdout io.Writer // the agent's standard output
	Stderr io.Writer // the agent's standard error
	// NotifySignals subscribes c to the signals that end a session and returns
	// a function that unsubscribes it. Nil means SIGINT and SIGTERM through
	// os/signal. Tests replace it to send signals directly.
	NotifySignals func(c chan<- os.Signal) (stop func())

	Listener net.Listener       // already listening on --listen
	Listen   string             // the --listen value, whose host also names the proxy
	Handler  http.Handler       // serves /llm/... and /mcp/<name>/...
	Cassette *cassette.Cassette // closed by Run
	Writer   *cassette.Writer   // closed by Run with the session's end
	ID       string             // RETRACE_SESSION_ID
	Mode     string             // RETRACE_MODE
	StartMsg string             // logged when the session starts, e.g. "recording"
	MCP      []string           // registered MCP server names
	Command  []string           // the agent command; an empty one fails the session
	Cwd      string             // the agent's working directory; "" inherits retrace's
	Diverged <-chan struct{}    // closed at a cassette miss in a replay; nil when recording
	Timeout  time.Duration      // the longest the agent may run before it is stopped as on a miss; 0 for no limit
	Grace    time.Duration      // how long the agent has to exit after a miss's SIGTERM (spec §6.2)
	// Finish, if set, completes the session's metadata from what was
	// recorded. It runs once the writer has written everything.
	Finish func(ctx context.Context, c *cassette.Cassette) error
}

// Result is how a session ended.
type Result struct {
	Status   string // the session status written to the cassette (spec §3.1)
	Agent    AgentResult
	TimedOut bool // the agent was stopped because Config.Timeout expired
	Stats    cassette.WriterStats
}

// notify subscribes c as cfg.NotifySignals says.
func (cfg Config) notify(c chan<- os.Signal) (stop func()) {
	if cfg.NotifySignals != nil {
		return cfg.NotifySignals(c)
	}
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	return func() { signal.Stop(c) }
}

// guardHosts returns the hosts that name the proxy's address: the host of
// --listen and the host of the listener's address (spec §3.6).
func guardHosts(cfg Config) []string {
	var hosts []string
	for _, addr := range []string{cfg.Listen, cfg.Listener.Addr().String()} {
		if host, _, err := net.SplitHostPort(addr); err == nil {
			hosts = append(hosts, host)
		}
	}
	return hosts
}

// Run serves cfg.Handler on cfg.Listener, runs the agent with the injected
// environment (spec §3.2) until it exits, then finalizes the cassette. It
// returns an error, already logged, only when the cassette may be incomplete.
func Run(ctx context.Context, cfg Config) (Result, error) {
	log := cfg.Logger
	// Rejected requests are answered before any routing, in every mode (spec §3.6).
	srv := &http.Server{Handler: proxy.Guard(cfg.Handler, log, guardHosts(cfg)...), ReadHeaderTimeout: 30 * time.Second}
	go func() {
		if err := srv.Serve(cfg.Listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("proxy stopped", "error", err)
		}
	}()

	base := "http://" + cfg.Listener.Addr().String()

	// Signals are handled until the cassette is closed: one that arrives while
	// the session is being finished must not kill retrace halfway.
	sigs := make(chan os.Signal, 2)
	stopSignals := cfg.notify(sigs)
	defer stopSignals()

	path := cfg.Cassette.Path()
	log.Info(cfg.StartMsg, "session", cfg.ID, "cassette", path, "proxy", base)
	res := Result{Status: "completed"}
	if cmd := startAgent(ctx, cfg, base); cmd == nil {
		res.Status = "failed"
	} else {
		var timedOut <-chan struct{}
		if cfg.Timeout > 0 {
			ch := make(chan struct{})
			t := time.AfterFunc(cfg.Timeout, func() { close(ch) })
			defer t.Stop()
			timedOut = ch
		}
		res.Agent = waitAgent(cmd, sigs, cfg.Diverged, timedOut, cfg.Grace, log)
		res.TimedOut = res.Agent.TimedOut
		// A session stopped for its time is cut short like one that a signal
		// ended, so it has the same status; a miss outweighs both below.
		if res.Agent.Interrupted || res.Agent.TimedOut {
			res.Status = "interrupted"
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
	case <-cfg.Diverged:
		if res.Status != "failed" {
			res.Status = "diverged"
		}
	default:
	}
	end := cassette.SessionEnd{Status: res.Status, EndedAt: time.Now()}
	if res.Status != "failed" {
		end.AgentExitCode = &res.Agent.ExitCode
	}
	wctx, cancelW := context.WithTimeout(context.WithoutCancel(ctx), finishTimeout)
	defer cancelW()
	werr := cfg.Writer.Close(wctx, end)
	if werr == nil && cfg.Finish != nil {
		// A step of its own, with its own deadline. The cassette is valid
		// without it, so the session still succeeds.
		fctx, cancelF := context.WithTimeout(context.WithoutCancel(ctx), finishTimeout)
		defer cancelF()
		if err := cfg.Finish(fctx, cfg.Cassette); err != nil {
			log.Warn("cannot complete the session metadata", "path", path, "error", err)
		}
	}
	if err := errors.Join(werr, cfg.Cassette.Close()); err != nil {
		log.Error("the cassette may be incomplete", "path", path, "error", err)
		return res, err
	}
	res.Stats = cfg.Writer.Stats()
	return res, nil
}

// startAgent starts cfg.Command with the injected environment (spec §3.2), or
// logs why it cannot and returns nil. The command may come from a cassette,
// which is untrusted input, so an empty one is not assumed away.
func startAgent(ctx context.Context, cfg Config, base string) *exec.Cmd {
	if len(cfg.Command) == 0 {
		cfg.Logger.Error("cannot start the agent: no agent command")
		return nil
	}
	cmd := exec.CommandContext(ctx, cfg.Command[0], cfg.Command[1:]...) //nolint:gosec // G204: running the user's agent is the point
	cmd.Dir = cfg.Cwd
	cmd.Env = withEnv(os.Environ(), agentEnv(base, cfg.ID, cfg.Mode, cfg.MCP))
	cmd.Stdin, cmd.Stdout, cmd.Stderr = cfg.Stdin, cfg.Stdout, cfg.Stderr
	if err := cmd.Start(); err != nil {
		cfg.Logger.Error("cannot start the agent", "command", cfg.Command[0], "error", err)
		return nil
	}
	return cmd
}
