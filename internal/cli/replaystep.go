package cli

import (
	"context"
	"fmt"
	"net"
	"os"
	"time"

	"github.com/pmlucero/retrace/internal/cassette"
	"github.com/pmlucero/retrace/internal/derive"
	"github.com/pmlucero/retrace/internal/provider/anthropic"
	"github.com/pmlucero/retrace/internal/redact"
	"github.com/pmlucero/retrace/internal/replay"
)

// The replay of one session is split in two so that a caller that replays
// many sessions, such as check (spec §9.9), can resolve what is a usage error
// before it starts the first one: prepareReplay has no side effects, and
// executeReplay is the part that creates things.

// prepareError is a failure of prepareReplay. A usage error exits with
// ExitUsage and msg is its text. Any other is a runtime error that the caller
// logs as msg with attrs and exits with ExitError.
type prepareError struct {
	usage bool
	msg   string
	attrs []any // slog attributes of a runtime error
}

func (e *prepareError) Error() string { return e.msg }

// report prints the error the way run always has and returns its exit code.
func (e *prepareError) report(env *Env) int {
	if e.usage {
		return usageError(env.Stderr, e.msg)
	}
	env.Logger.Error(e.msg, e.attrs...)
	return ExitError
}

func usagePrepareError(err error) *prepareError { return &prepareError{usage: true, msg: err.Error()} }

// replayPlan is everything a replay of one source session needs, resolved.
type replayPlan struct {
	opts     runOptions
	src      *cassette.Cassette
	srcID    string
	srcSess  cassette.Session
	mode     string
	command  []string
	cwd      string
	rules    *redact.Rules
	servers  []string     // every MCP server of the source, registered for the agent
	live     []liveServer // the servers that live-tools forwards to
	replayed []string     // the servers answered from the cassette
	queues   queueSet
}

// prepareReplay resolves the inputs of a replay of src, whose session row is
// sess. Its checks run in the order of spec §9.3, which puts the usage errors
// before the replay's cassette exists. With opts.cwd set the agent runs
// there, and the recorded working directory is not checked (spec §9.9.3).
func prepareReplay(ctx context.Context, src *cassette.Cassette, sess cassette.Session, opts runOptions) (*replayPlan, *prepareError) {
	command, recorded, err := replayCommand(opts, sess)
	if err != nil {
		return nil, usagePrepareError(err)
	}
	cwd := sess.Cwd
	switch {
	case opts.cwd != "":
		cwd = opts.cwd
	case !recorded:
		if cwd, err = os.Getwd(); err != nil {
			return nil, &prepareError{msg: "reading the working directory", attrs: []any{"error", err}}
		}
	}

	rules, err := redact.RulesFromMetadata(sess.Redaction, opts.redactHeader...) // spec §4.4
	if err != nil {
		return nil, &prepareError{msg: "cannot read the session's redaction rules", attrs: []any{"session", opts.source, "error", err}}
	}
	servers, err := mcpServerNames(sess.MCPServers)
	if err != nil {
		return nil, &prepareError{msg: "cannot read the session's MCP servers", attrs: []any{"session", opts.source, "error", err}}
	}
	// A forwarded channel is not queued: its recorded interactions are not
	// expected to be served (spec §4.4).
	mode := opts.mode()
	var live []liveServer
	replayed := servers
	if mode == "live-tools" {
		if live, err = liveMCPServers(sess.MCPServers, opts.mcp); err != nil {
			return nil, usagePrepareError(err)
		}
		replayed = nil
	}
	queues, err := replayQueues(ctx, src, mode != "swap-model", replayed)
	if err != nil {
		return nil, &prepareError{msg: "cannot read the recorded interactions", attrs: []any{"session", opts.source, "error", err}}
	}
	return &replayPlan{
		opts: opts, src: src, srcID: opts.source, srcSess: sess, mode: mode, command: command, cwd: cwd,
		rules: rules, servers: servers, live: live, replayed: replayed, queues: queues,
	}, nil
}

// replayTarget says where a replay goes: the directory of its cassette, which
// need not be the source's (check writes to --out), its id and the address of
// its proxy.
type replayTarget struct {
	dir      string
	id       string
	explicit bool   // the user named id, so it must be new (spec §9.3)
	listen   string // port 0 picks a free port
}

// replayResult is how an executed replay ended. The caller decides what to
// print.
type replayResult struct {
	id        string
	path      string
	status    string
	agentExit int
	exitCode  int // what run exits with
	// first and misses describe the replay's divergence when hasMiss is set.
	first         replay.MissReport
	misses        int
	hasMiss       bool
	droppedEvents int
	unconsumed    []cassette.Interaction
}

// executeReplay runs a prepared replay: it listens, creates the replay's
// cassette and runs the agent against it. It logs its own failures, which
// are runtime errors, and returns them. It stops when ctx is done.
func executeReplay(ctx context.Context, env *Env, plan *replayPlan, target replayTarget) (replayResult, error) {
	log := env.Logger
	opts := plan.opts

	// Listen before creating the cassette, so a busy port leaves nothing behind.
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", target.listen)
	if err != nil {
		log.Error("cannot start the proxy", "listen", target.listen, "error", err)
		return replayResult{}, fmt.Errorf("listening on %s: %w", target.listen, err)
	}
	defer ln.Close()

	strategy := replay.Strict{} // the only one parseRunFlags accepts (spec §6.3)
	var modelOverride *string
	if plan.mode == "swap-model" {
		modelOverride = &opts.model
	}
	mcpServers := mcpServersCopy(plan.srcSess.MCPServers)
	var finish func(context.Context, *cassette.Cassette) error
	if plan.mode == "live-tools" {
		// Completed from the replay's own exchanges, as in record (spec §9.3).
		mcpServers, finish = liveMCPServersJSON(plan.live, plan.rules), storeMCPServerInfo
	}
	source := plan.srcID
	c, id, err := createReplayCassette(ctx, target.dir, target.id, target.explicit, cassette.Session{
		Mode: plan.mode, ModelOverride: modelOverride, ParentSessionID: &source, Status: "recording", CreatedAt: time.Now(),
		Command: plan.command, Cwd: plan.cwd, OnMiss: strategy.Name(),
		MCPServers: mcpServers, Redaction: plan.rules.Metadata(), RetraceVersion: Version(),
		DeriverVersion: derive.Version, PricingVersion: anthropic.PricingVersion(),
	})
	if err != nil {
		log.Error("cannot create the cassette", "error", err)
		return replayResult{}, fmt.Errorf("creating the cassette: %w", err)
	}
	w, err := cassette.NewWriter(ctx, c, cassette.WriterOptions{Logger: log, Deriver: derive.New()})
	if err != nil {
		log.Error("cannot record into the cassette", "error", err)
		if err := c.Close(); err != nil {
			log.Error("closing the cassette", "path", c.Path(), "error", err)
		}
		return replayResult{}, fmt.Errorf("recording into the cassette: %w", err)
	}

	divergence := replay.NewDivergence()
	mux := replayHandler(replayRoutes{
		mode: plan.mode, src: plan.src, queues: plan.queues, rules: plan.rules, writer: w, divergence: divergence,
		strategy: strategy, log: log, opts: replay.Options{Realtime: opts.realtime},
		anthropic: opts.anthropic, model: opts.model, live: plan.live, replayed: plan.replayed,
	})
	path := c.Path()
	res, err := runSession(ctx, env, sessionConfig{
		listener: ln,
		listen:   target.listen,
		handler:  mux,
		cassette: c,
		writer:   w,
		id:       id,
		mode:     plan.mode,
		startMsg: "replaying " + plan.srcID,
		mcp:      plan.servers,
		command:  plan.command,
		cwd:      plan.cwd,
		diverged: divergence.Done(),
		grace:    opts.grace,
		finish:   finish,
	})
	if err != nil {
		return replayResult{}, fmt.Errorf("running the session: %w", err)
	}
	out := replayResult{
		id: id, path: path, status: res.status, agentExit: res.agent.exitCode, exitCode: res.exitCode(),
		droppedEvents: res.stats.DroppedEvents, unconsumed: plan.queues.unconsumed(),
	}
	out.first, out.misses, out.hasMiss = divergence.Report()
	return out, nil
}
