package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math"
	"net/url"
	"os"
	"strconv"
	"strings"
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
func (o runOptions) mode() string {
	switch {
	case o.model != "":
		return "swap-model"
	case o.tools == "live":
		return "live-tools"
	}
	return "exact"
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
	case set["anthropic-upstream"] && opts.mode() != "swap-model":
		return opts, usageError(env.Stderr, "--anthropic-upstream is only used with --model: the LLM is answered from the cassette"), false
	case len(opts.mcp) > 0 && opts.mode() != "live-tools":
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
		return perr.report(env) // usage errors come before any cassette is created (spec §9.3)
	}
	res, err := executeReplay(ctx, env, plan, replayTarget{
		dir: env.Globals.Dir, id: id, explicit: opts.as != "", listen: opts.listen,
	})
	if err != nil {
		return ExitError
	}
	if res.status == "diverged" && res.hasMiss {
		divergenceReport{source: opts.source, replay: res.id, first: res.first, misses: res.misses,
			droppedEvents: res.droppedEvents}.write(env.Stderr)
	}
	warnUnconsumed(env, res.unconsumed)
	log.Info("session replayed", "session", res.id, "source", opts.source, "status", res.status, "cassette", res.path)
	return res.exitCode
}

// mcpServersCopy returns the mcp_servers of an exact replay: a copy of the
// source's, since every MCP answer comes from its cassette (spec §9.3).
func mcpServersCopy(stored json.RawMessage) json.RawMessage {
	if len(stored) == 0 {
		return json.RawMessage(`[]`)
	}
	return stored
}

// createReplayCassette creates the cassette of a replay with session row s.
// An explicit id must be new (spec §9.3); a default one moves on to the next
// n if another replay took it first, unless that n would not fit the id
// format or would reach math.MaxInt64. It returns the id it used.
func createReplayCassette(ctx context.Context, dir, id string, explicit bool, s cassette.Session) (*cassette.Cassette, string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, "", fmt.Errorf("creating %s: %w", dir, err)
	}
	for range 100 {
		s.ID = id
		c, err := createSessionCassette(ctx, cassette.Path(dir, id), s)
		if errors.Is(err, cassette.ErrExists) && !explicit {
			prefix := strings.TrimRightFunc(id, func(r rune) bool { return r >= '0' && r <= '9' })
			n, _ := strconv.Atoi(strings.TrimPrefix(id, prefix))
			if n >= math.MaxInt64-1 {
				break
			}
			next := prefix + strconv.Itoa(n+1)
			if cassette.ValidateSessionID(next) != nil {
				break
			}
			id = next
			continue
		}
		if errors.Is(err, cassette.ErrExists) {
			return nil, "", fmt.Errorf("session %q already exists in %s", id, dir)
		}
		if err != nil {
			return nil, "", err
		}
		return c, id, nil
	}
	return nil, "", fmt.Errorf("no free default id after %s; name the replay with --as", id)
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
