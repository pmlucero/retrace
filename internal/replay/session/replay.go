package session

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"os"
	"time"

	"github.com/pmlucero/retrace/internal/agentrun"
	"github.com/pmlucero/retrace/internal/cassette"
	"github.com/pmlucero/retrace/internal/derive"
	"github.com/pmlucero/retrace/internal/mcp"
	"github.com/pmlucero/retrace/internal/mcpmeta"
	"github.com/pmlucero/retrace/internal/provider/anthropic"
	"github.com/pmlucero/retrace/internal/redact"
	"github.com/pmlucero/retrace/internal/replay"
)

// The replay of one session is split in two so that a caller that replays
// many sessions, such as check (spec §9.9), can resolve what is a usage error
// before it starts the first one: Prepare has no side effects, and Execute is
// the part that creates things.

// PrepareError is a failure of Prepare. A usage error has Usage set and Msg is
// its text. Any other is a runtime error that the caller logs as Msg with
// Attrs.
type PrepareError struct {
	Usage bool
	Msg   string
	Attrs []any // slog attributes of a runtime error
}

func (e *PrepareError) Error() string { return e.Msg }

// UsagePrepareError returns the usage error for err.
func UsagePrepareError(err error) *PrepareError { return &PrepareError{Usage: true, Msg: err.Error()} }

// Params are the resolved inputs of a replay of one source session.
type Params struct {
	Source        string        // the id of the source session
	Mode          cassette.Mode // exact, swap-model or live-tools
	Model         string        // the model of swap-model
	Anthropic     *url.URL
	Command       []string // the agent's command
	Cwd           string   // where the agent runs
	RedactHeaders []string // added to the source's redaction rules
	MCP           []mcpmeta.Server
	Realtime      bool
	Grace         time.Duration

	// ParseURL validates an MCP server URL as the flags do, and Printable
	// makes cassette text safe to print in an error message.
	ParseURL  func(string) (*url.URL, error)
	Printable func(string) string
}

// Plan is everything a replay of one source session needs, resolved.
type Plan struct {
	params   Params
	src      *cassette.Cassette
	srcSess  cassette.Session
	rules    *redact.Rules
	servers  []string     // every MCP server of the source, registered for the agent
	live     []LiveServer // the servers that live-tools forwards to
	replayed []string     // the servers answered from the cassette
	queues   replay.QueueSet
}

// Prepare resolves the inputs of a replay of src, whose session row is sess.
// Its checks run in the order of spec §9.3, which puts the usage errors before
// the replay's cassette exists.
func Prepare(ctx context.Context, src *cassette.Cassette, sess cassette.Session, p Params) (*Plan, *PrepareError) {
	rules, err := redact.RulesFromMetadata(sess.Redaction, p.RedactHeaders...) // spec §4.4
	if err != nil {
		return nil, &PrepareError{Msg: "cannot read the session's redaction rules", Attrs: []any{"session", p.Source, "error", err}}
	}
	servers, err := mcpmeta.Names(sess.MCPServers)
	if err != nil {
		return nil, &PrepareError{Msg: "cannot read the session's MCP servers", Attrs: []any{"session", p.Source, "error", err}}
	}
	// A forwarded channel is not queued: its recorded interactions are not
	// expected to be served (spec §4.4).
	var live []LiveServer
	replayed := servers
	if p.Mode.ForwardsMCP() {
		if live, err = liveMCPServers(sess.MCPServers, p.MCP, p.ParseURL, p.Printable); err != nil {
			return nil, UsagePrepareError(err)
		}
		replayed = nil
	}
	var llm func([]byte) []byte
	if p.Mode.QueuesLLM() {
		llm = anthropic.Canonicalize
	}
	queues, err := replay.BuildQueues(ctx, src, llm, replayed, mcp.Canonicalize)
	if err != nil {
		return nil, &PrepareError{Msg: "cannot read the recorded interactions", Attrs: []any{"session", p.Source, "error", err}}
	}
	return &Plan{
		params: p, src: src, srcSess: sess, rules: rules, servers: servers, live: live, replayed: replayed, queues: queues,
	}, nil
}

// Target says where a replay goes: the directory of its cassette, which need
// not be the source's (check writes to --out), its id and the address of its
// proxy.
type Target struct {
	Dir      string
	ID       string
	Explicit bool          // the user named ID, so it must be new (spec §9.3)
	Listen   string        // port 0 picks a free port
	Timeout  time.Duration // the longest the agent may run; 0 for no limit (check --timeout)
	Insert   InsertFunc    // writes the session row; nil for the default, tests make it fail
}

// Env is what Execute needs from its caller: the streams of the agent, the
// logger and how to subscribe to the signals that end a session.
type Env struct {
	Logger        *slog.Logger
	Stdin         io.Reader
	Stdout        io.Writer
	Stderr        io.Writer
	NotifySignals func(c chan<- os.Signal) (stop func()) // nil for SIGINT and SIGTERM
	Version       string                                 // the retrace version recorded in the session
}

// Result is how an executed replay ended. The caller decides what to print.
type Result struct {
	ID   string
	Path string
	Run  agentrun.Result
	// First and Misses describe the replay's divergence when HasMiss is set.
	First      replay.MissReport
	Misses     int
	HasMiss    bool
	Unconsumed []cassette.Interaction
}

// Execute runs a prepared replay: it listens, creates the replay's cassette
// and runs the agent against it. It logs its own failures, which are runtime
// errors, and returns them. It stops when ctx is done.
func Execute(ctx context.Context, env Env, plan *Plan, target Target) (Result, error) {
	log := env.Logger
	p := plan.params

	// Listen before creating the cassette, so a busy port leaves nothing behind.
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", target.Listen)
	if err != nil {
		log.Error("cannot start the proxy", "listen", target.Listen, "error", err)
		return Result{}, fmt.Errorf("listening on %s: %w", target.Listen, err)
	}
	defer ln.Close()

	strategy := replay.Strict{} // the only one the flags accept (spec §6.3)
	var modelOverride *string
	if p.Mode.ForwardsLLM() {
		modelOverride = &p.Model
	}
	mcpServers := mcpServersCopy(plan.srcSess.MCPServers)
	var finish func(context.Context, *cassette.Cassette) error
	if p.Mode.ForwardsMCP() {
		// Completed from the replay's own exchanges, as in record (spec §9.3).
		mcpServers, finish = liveMCPServersJSON(plan.live, plan.rules), mcpmeta.StoreServerInfo
	}
	source := p.Source
	c, id, err := createReplayCassette(ctx, target.Dir, target.ID, target.Explicit, cassette.Session{
		Mode: p.Mode, ModelOverride: modelOverride, ParentSessionID: &source, Status: "recording", CreatedAt: time.Now(),
		Command: p.Command, Cwd: p.Cwd, OnMiss: strategy.Name(),
		MCPServers: mcpServers, Redaction: plan.rules.Metadata(), RetraceVersion: env.Version,
		DeriverVersion: derive.Version, PricingVersion: anthropic.PricingVersion(),
	}, target.Insert)
	if err != nil {
		log.Error("cannot create the cassette", "error", err)
		return Result{}, fmt.Errorf("creating the cassette: %w", err)
	}
	w, err := cassette.NewWriter(ctx, c, cassette.WriterOptions{Logger: log, Deriver: derive.New()})
	if err != nil {
		log.Error("cannot record into the cassette", "error", err)
		if err := c.Close(); err != nil {
			log.Error("closing the cassette", "path", c.Path(), "error", err)
		}
		return Result{}, fmt.Errorf("recording into the cassette: %w", err)
	}

	divergence := replay.NewDivergence()
	mux := Handler(Routes{
		Mode: p.Mode, Src: plan.src, Queues: plan.queues, Rules: plan.rules, Writer: w, Divergence: divergence,
		Strategy: strategy, Log: log, Opts: replay.Options{Realtime: p.Realtime},
		Anthropic: p.Anthropic, Model: p.Model, Live: plan.live, Replayed: plan.replayed,
		LLMCanonicalize: anthropic.Canonicalize, MCPCanonicalize: mcp.Canonicalize,
	})
	path := c.Path()
	res, err := agentrun.Run(ctx, agentrun.Config{
		Logger: log, Stdin: env.Stdin, Stdout: env.Stdout, Stderr: env.Stderr, NotifySignals: env.NotifySignals,
		Listener: ln,
		Listen:   target.Listen,
		Handler:  mux,
		Cassette: c,
		Writer:   w,
		ID:       id,
		Mode:     string(p.Mode),
		StartMsg: "replaying " + p.Source,
		MCP:      plan.servers,
		Command:  p.Command,
		Cwd:      p.Cwd,
		Diverged: divergence.Done(),
		Grace:    p.Grace,
		Timeout:  target.Timeout,
		Finish:   finish,
	})
	if err != nil {
		return Result{}, fmt.Errorf("running the session: %w", err)
	}
	out := Result{ID: id, Path: path, Run: res, Unconsumed: plan.queues.Unconsumed()}
	out.First, out.Misses, out.HasMiss = divergence.Report()
	return out, nil
}
