package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"time"

	"github.com/pmlucero/retrace/internal/cassette"
	"github.com/pmlucero/retrace/internal/mcpmeta"
	"github.com/pmlucero/retrace/internal/replay/session"
)

// ExitDiverged is returned when a replay stopped on a cassette miss
// (spec §6.2, §9.7).
const ExitDiverged = 3

// unconsumedShown is how many unconsumed interaction ids the end-of-replay
// warning lists (spec §4.4).
const unconsumedShown = 5

var runCommand = command{
	name:    "run",
	summary: "replay a recorded session against its cassette",
	run:     runRun,
}

const runUsage = `Usage: retrace run <id> [flags] (--recorded-command | -- <agent command...>)

Runs an agent against the cassette of session <id>, and answers its requests
from the cassette. The replay is recorded as a new session. The agent is the
command after '--', run in the current directory, or with --recorded-command
the command that the session recorded, run in its recorded directory. The
recorded command never runs unless asked for: a cassette from someone else
can run any program. Without either, run prints it so that you can review it.

Without --model or --tools live it is an exact replay. With --model it is a
swap-model replay: LLM requests are forwarded to --anthropic-upstream with
their model set to --model, and MCP is answered from the cassette. With
--tools live it is a live-tools replay: MCP requests are forwarded to the
recorded servers, or to the --mcp URLs, and the LLM is answered from the
cassette.

Flags:
`

type runOptions struct {
	source       string
	as           string
	model        string
	tools        string
	onMiss       string
	grace        time.Duration
	realtime     bool
	listen       string
	listenUnsafe bool
	redactHeader []string         // added to the source's redaction rules
	command      []string         // the command after "--", nil without one
	recorded     bool             // --recorded-command: run the source's recorded command
	anthropic    *url.URL         // the LLM upstream of swap-model
	mcp          []mcpmeta.Server // the --mcp overrides of live-tools
	cwd          string           // runs the agent here and skips the recorded-cwd checks (check --cwd); run never sets it
}

// mode returns the replay mode the flags select (spec §9.3).
func (o runOptions) mode() cassette.Mode {
	switch {
	case o.model != "":
		return cassette.ModeSwapModel
	case o.tools == "live":
		return cassette.ModeLiveTools
	}
	return cassette.ModeExact
}

// reservedStrategies are accepted by the parser and rejected (spec §6.3).
var reservedStrategies = map[string]bool{"fallthrough": true, "nearest-match": true, "llm-stub": true}

func parseRunFlags(env *Env, args []string) (runOptions, int, bool) {
	var opts runOptions
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.StringVar(&opts.as, "as", "", "id of the new session (default <id>.<mode>.<n>)")
	fs.StringVar(&opts.model, "model", "", "replay with this model, forwarding LLM requests (swap-model)")
	fs.StringVar(&opts.tools, "tools", "frozen", "frozen, or live to forward MCP to the real servers (live-tools)")
	fs.Func("mcp", "in live-tools, reach MCP server `name=url` at this URL (repeatable)", mcpFlag(&opts.mcp))
	fs.StringVar(&opts.onMiss, "on-miss", "strict", "divergence strategy")
	fs.DurationVar(&opts.grace, "grace", 5*time.Second, "how long the agent has to exit after SIGTERM on a cassette miss")
	fs.Func("redact-header", "also redact the request or response header with this literal `name` (repeatable)", redactHeaderFlag(&opts.redactHeader))
	fs.BoolVar(&opts.realtime, "realtime", false, "replay SSE chunks with their recorded timing")
	fs.StringVar(&opts.listen, "listen", "127.0.0.1:4790", "proxy address; port 0 picks a free port")
	fs.BoolVar(&opts.listenUnsafe, "listen-unsafe", false, "allow a non-loopback --listen address")
	fs.BoolVar(&opts.recorded, "recorded-command", false, "run the session's recorded command in its recorded directory")
	upstream := fs.String("anthropic-upstream", "https://api.anthropic.com", "Anthropic API base URL (swap-model; plain http only for loopback hosts)")
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), runUsage)
		fs.PrintDefaults()
	}

	// The id comes before the flags, which flag.Parse would stop at, so
	// parsing resumes after each positional argument until "--".
	rest := args
	if i := indexOf(args, "--"); i >= 0 {
		rest, opts.command = args[:i], args[i+1:]
	}
	var positional []string
	for {
		if code, ok := parseCommandFlags(fs, rest, env); !ok {
			return opts, code, false
		}
		if fs.NArg() == 0 {
			break
		}
		positional = append(positional, fs.Arg(0))
		rest = fs.Args()[1:]
	}

	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })

	if len(positional) != 1 {
		return opts, usageError(env.Stderr, "run needs exactly one session id"), false
	}
	opts.source = positional[0]
	for _, id := range []string{opts.source, opts.as} {
		if id == "" {
			continue
		}
		if err := cassette.ValidateSessionID(id); err != nil {
			return opts, usageError(env.Stderr, err.Error()), false
		}
	}
	if opts.command != nil && len(opts.command) == 0 {
		return opts, usageError(env.Stderr, "no agent command after '--'"), false
	}
	if opts.command != nil && opts.recorded {
		return opts, usageError(env.Stderr, "give the agent command after '--' or --recorded-command, not both"), false
	}
	switch {
	case opts.tools != "frozen" && opts.tools != "live":
		return opts, usageError(env.Stderr, fmt.Sprintf("--tools %q: want frozen or live", opts.tools)), false
	case opts.model != "" && opts.tools == "live":
		return opts, usageError(env.Stderr, "--model with --tools live holds nothing constant; use record instead"), false
	case set["model"] && opts.model == "":
		return opts, usageError(env.Stderr, "--model: want a model name"), false
	case set["anthropic-upstream"] && !opts.mode().ForwardsLLM():
		return opts, usageError(env.Stderr, "--anthropic-upstream is only used with --model: the LLM is answered from the cassette"), false
	case len(opts.mcp) > 0 && !opts.mode().ForwardsMCP():
		return opts, usageError(env.Stderr, "--mcp is only used with --tools live: MCP is answered from the cassette"), false
	case reservedStrategies[opts.onMiss]:
		return opts, usageError(env.Stderr, fmt.Sprintf("--on-miss %s is not implemented in this version", opts.onMiss)), false
	case opts.onMiss != "strict":
		return opts, usageError(env.Stderr, fmt.Sprintf("--on-miss %q: want strict", opts.onMiss)), false
	case opts.grace < 0:
		return opts, usageError(env.Stderr, fmt.Sprintf("--grace %s: want a duration of 0 or more", opts.grace)), false
	}
	u, err := upstreamURL(*upstream)
	if err != nil {
		return opts, usageError(env.Stderr, "--anthropic-upstream: "+err.Error()), false
	}
	opts.anthropic = u
	if err := checkListen(opts.listen, opts.listenUnsafe); err != nil {
		return opts, usageError(env.Stderr, err.Error()), false
	}
	return opts, 0, true
}

func indexOf(args []string, s string) int {
	for i, a := range args {
		if a == s {
			return i
		}
	}
	return -1
}

// runRun implements `retrace run` (spec §4, §9.3).
func runRun(ctx context.Context, env *Env, args []string) int {
	opts, code, ok := parseRunFlags(env, args)
	if !ok {
		return code
	}
	log := env.Logger
	mode := opts.mode()

	// Usage errors come before the source is opened (spec §9.3).
	id := opts.as
	if id == "" {
		var err error
		id, err = session.NextID(env.Globals.Dir, opts.source, mode)
		switch {
		case errors.Is(err, session.ErrNoDefaultID):
			return usageError(env.Stderr, err.Error())
		case err != nil:
			log.Error("cannot choose the replay's id", "error", err)
			return ExitError
		}
	}

	srcPath := cassette.Path(env.Globals.Dir, opts.source)
	if _, err := os.Stat(srcPath); errors.Is(err, os.ErrNotExist) {
		log.Error("no such session", "session", opts.source, "dir", env.Globals.Dir)
		return ExitError
	}
	src, err := cassette.Open(ctx, srcPath)
	if err != nil {
		log.Error("cannot open the cassette", "error", err)
		return ExitError
	}
	defer src.Close()
	srcSession, err := src.Session(ctx)
	if err != nil {
		log.Error("cannot read the session", "session", opts.source, "error", err)
		return ExitError
	}

	plan, perr := prepareReplay(ctx, src, srcSession, opts)
	if perr != nil {
		return reportPrepare(env, perr) // usage errors come before any cassette is created (spec §9.3)
	}
	res, err := session.Execute(ctx, sessionEnv(env), plan, session.Target{
		Dir: env.Globals.Dir, ID: id, Explicit: opts.as != "", Listen: opts.listen, Insert: env.insertSession,
	})
	if err != nil {
		return ExitError
	}
	if res.Run.Status == "diverged" && res.HasMiss {
		divergenceReport{source: opts.source, replay: res.ID, first: res.First, misses: res.Misses,
			droppedEvents: res.Run.Stats.DroppedEvents}.write(env.Stderr)
	}
	warnUnconsumed(env, res.Unconsumed)
	log.Info("session replayed", "session", res.ID, "source", opts.source, "status", res.Run.Status, "cassette", res.Path)
	return sessionExitCode(res.Run)
}

// warnUnconsumed reports recorded interactions that the replay never served
// (spec §4.4). It is a warning, not an error.
func warnUnconsumed(env *Env, unconsumed []cassette.Interaction) {
	if len(unconsumed) == 0 {
		return
	}
	ids := make([]int64, 0, unconsumedShown)
	for _, in := range unconsumed[:min(len(unconsumed), unconsumedShown)] {
		ids = append(ids, in.ID)
	}
	env.Logger.Warn("some recorded interactions were never replayed", "count", len(unconsumed), "first", ids)
}
