package cli

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/pmlucero/retrace/internal/cassette"
	"github.com/pmlucero/retrace/internal/derive"
	"github.com/pmlucero/retrace/internal/match"
	"github.com/pmlucero/retrace/internal/mcp"
	"github.com/pmlucero/retrace/internal/provider/anthropic"
	"github.com/pmlucero/retrace/internal/proxy"
	"github.com/pmlucero/retrace/internal/redact"
	"github.com/pmlucero/retrace/internal/replay"
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

const runUsage = `Usage: retrace run <id> [flags] [-- <agent command...>]

Runs the agent of session <id> again, by default with its recorded command
and working directory, and answers its requests from the cassette. The
replay is recorded as a new session.

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
	command      []string    // nil: the recorded one
	anthropic    *url.URL    // the LLM upstream of swap-model
	mcp          []mcpServer // the --mcp overrides of live-tools
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
	fs.BoolVar(&opts.realtime, "realtime", false, "replay SSE chunks with their recorded timing")
	fs.StringVar(&opts.listen, "listen", "127.0.0.1:4790", "proxy address; port 0 picks a free port")
	fs.BoolVar(&opts.listenUnsafe, "listen-unsafe", false, "allow a non-loopback --listen address")
	upstream := fs.String("anthropic-upstream", "https://api.anthropic.com", "Anthropic API base URL (swap-model)")
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
	u, err := absoluteHTTPURL(*upstream)
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
		id, err = nextReplayID(env.Globals.Dir, opts.source, mode)
		switch {
		case errors.Is(err, errNoDefaultID):
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

	command, cwd := srcSession.Command, srcSession.Cwd
	if opts.command != nil {
		if cwd, err = os.Getwd(); err != nil {
			log.Error("reading the working directory", "error", err)
			return ExitError
		}
		command = opts.command
	}
	if len(command) == 0 {
		// Before any cassette is created (spec §9.3).
		return usageError(env.Stderr, fmt.Sprintf("session %q has no recorded command; give one after '--'", opts.source))
	}

	rules, err := redact.RulesFromMetadata(srcSession.Redaction)
	if err != nil {
		log.Error("cannot read the session's redaction rules", "session", opts.source, "error", err)
		return ExitError
	}
	servers, err := mcpServerNames(srcSession.MCPServers)
	if err != nil {
		log.Error("cannot read the session's MCP servers", "session", opts.source, "error", err)
		return ExitError
	}
	// A forwarded channel is not queued: its recorded interactions are not
	// expected to be served (spec §4.4).
	var live []liveServer
	replayed := servers
	if mode == "live-tools" {
		if live, err = liveMCPServers(srcSession.MCPServers, opts.mcp); err != nil {
			return usageError(env.Stderr, err.Error()) // before the new session exists (spec §9.3)
		}
		replayed = nil
	}
	queues, err := replayQueues(ctx, src, mode != "swap-model", replayed)
	if err != nil {
		log.Error("cannot read the recorded interactions", "session", opts.source, "error", err)
		return ExitError
	}

	// Listen before creating the cassette, so a busy port leaves nothing behind.
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", opts.listen)
	if err != nil {
		log.Error("cannot start the proxy", "listen", opts.listen, "error", err)
		return ExitError
	}
	defer ln.Close()

	strategy := replay.Strict{} // the only one parseRunFlags accepts (spec §6.3)
	var modelOverride *string
	if mode == "swap-model" {
		modelOverride = &opts.model
	}
	mcpServers := mcpServersCopy(srcSession.MCPServers)
	var finish func(context.Context, *cassette.Cassette) error
	if mode == "live-tools" {
		// Completed from the replay's own exchanges, as in record (spec §9.3).
		mcpServers, finish = liveMCPServersJSON(live, rules), storeMCPServerInfo
	}
	c, id, err := createReplayCassette(ctx, env.Globals.Dir, id, opts.as != "", cassette.Session{
		Mode: mode, ModelOverride: modelOverride, ParentSessionID: &opts.source, Status: "recording", CreatedAt: time.Now(),
		Command: command, Cwd: cwd, OnMiss: strategy.Name(),
		MCPServers: mcpServers, Redaction: srcSession.Redaction, RetraceVersion: Version(),
		DeriverVersion: derive.Version, PricingVersion: anthropic.PricingVersion(),
	})
	if err != nil {
		log.Error("cannot create the cassette", "error", err)
		return ExitError
	}
	w, err := cassette.NewWriter(ctx, c, cassette.WriterOptions{Logger: log, Deriver: derive.New()})
	if err != nil {
		log.Error("cannot record into the cassette", "error", err)
		if err := c.Close(); err != nil {
			log.Error("closing the cassette", "path", c.Path(), "error", err)
		}
		return ExitError
	}

	divergence := replay.NewDivergence()
	mux := replayHandler(replayRoutes{
		mode: mode, src: src, queues: queues, rules: rules, writer: w, divergence: divergence,
		strategy: strategy, log: log, opts: replay.Options{Realtime: opts.realtime},
		anthropic: opts.anthropic, model: opts.model, live: live, replayed: replayed,
	})
	path := c.Path()
	res, err := runSession(ctx, env, sessionConfig{
		listener: ln,
		handler:  mux,
		cassette: c,
		writer:   w,
		id:       id,
		mode:     mode,
		startMsg: "replaying " + opts.source,
		mcp:      servers,
		command:  command,
		cwd:      cwd,
		diverged: divergence.Done(),
		grace:    opts.grace,
		finish:   finish,
	})
	if err != nil {
		return ExitError
	}
	if res.status == "diverged" {
		if first, misses, ok := divergence.Report(); ok {
			divergenceReport{source: opts.source, replay: id, first: first, misses: misses,
				droppedEvents: res.stats.DroppedEvents}.write(env.Stderr)
		}
	}
	warnUnconsumed(env, queues.unconsumed())
	log.Info("session replayed", "session", id, "source", opts.source, "status", res.status, "cassette", path)
	return res.exitCode()
}

// replayRoutes is what replayHandler routes a replay's requests to.
type replayRoutes struct {
	mode       string
	src        *cassette.Cassette
	queues     queueSet
	rules      *redact.Rules
	writer     *cassette.Writer
	divergence *replay.Divergence
	strategy   replay.DivergenceStrategy
	log        *slog.Logger
	opts       replay.Options
	anthropic  *url.URL     // the LLM upstream of swap-model
	model      string       // the model of swap-model
	live       []liveServer // the MCP servers that live-tools forwards to
	replayed   []string     // the MCP servers answered from the cassette
}

// replayHandler returns the handler of a replay's proxy (spec §4). In exact
// mode every channel answers from the cassette and nothing is forwarded
// upstream (spec §4.1). swap-model forwards the LLM with the model rewritten
// (spec §4.2), and live-tools forwards MCP unchanged (spec §4.3). A miss on
// a channel served from the cassette ends the replay (spec §6.2).
func replayHandler(rt replayRoutes) http.Handler {
	mux := http.NewServeMux()
	if rt.mode == "swap-model" {
		mux.Handle("/llm/anthropic/", proxy.New(proxy.Config{Routes: []proxy.Route{{
			Prefix: "/llm/anthropic", Channel: "llm", Upstream: "anthropic", Target: rt.anthropic,
			Provenance: "live", Rewrite: anthropic.ModelRewriter(rt.model),
		}}, Writer: rt.writer, Redact: rt.rules, Logger: rt.log}))
	} else {
		mux.Handle("/llm/anthropic/", http.StripPrefix("/llm/anthropic", replay.NewLLMHandler(replay.LLMConfig{
			Source: rt.src, Queues: rt.queues.llm, Redact: rt.rules, Canonicalize: anthropic.Canonicalize, Writer: rt.writer,
			Divergence: rt.divergence, Strategy: rt.strategy, Logger: rt.log, Options: rt.opts,
		})))
	}
	if len(rt.live) > 0 {
		routes := make([]proxy.Route, len(rt.live))
		for i, sv := range rt.live {
			routes[i] = proxy.Route{Prefix: "/mcp/" + sv.name, Channel: "mcp", Upstream: "mcp:" + sv.name,
				Target: sv.target, Provenance: "live"}
		}
		forward := proxy.New(proxy.Config{Routes: routes, Writer: rt.writer, Redact: rt.rules, Logger: rt.log})
		for _, r := range routes {
			mux.Handle(r.Prefix, forward) // the URL the agent got (spec §3.2)
			mux.Handle(r.Prefix+"/", forward)
		}
	}
	for _, name := range rt.replayed {
		prefix := "/mcp/" + name
		h := http.StripPrefix(prefix, replay.NewMCPHandler(replay.MCPConfig{
			Server: name, Source: rt.src, Queues: rt.queues.mcp[name], Redact: rt.rules,
			Canonicalize: mcpCanonicalize(name), Writer: rt.writer, Divergence: rt.divergence, Strategy: rt.strategy,
			Logger: rt.log, Options: rt.opts,
		}))
		mux.Handle(prefix, h) // the URL the agent got (spec §3.2)
		mux.Handle(prefix+"/", h)
	}
	return mux
}

// mcpCanonicalize returns the canonical form of a request body sent to MCP
// server name, after redaction (spec §5 step 1).
func mcpCanonicalize(name string) func(body []byte) []byte {
	return func(body []byte) []byte { return mcp.Canonicalize(name, body) }
}

// mcpServerNames returns the names of the source session's MCP servers
// (spec §7.2). The cassette is untrusted input, so each name must be one
// record accepts: it becomes a route and an environment variable.
func mcpServerNames(stored json.RawMessage) ([]string, error) {
	if len(stored) == 0 {
		return nil, nil
	}
	var servers []mcpServer
	if err := json.Unmarshal(stored, &servers); err != nil {
		return nil, fmt.Errorf("parsing mcp_servers: %w", err)
	}
	names := make([]string, 0, len(servers))
	for _, sv := range servers {
		if !mcpNamePattern.MatchString(sv.Name) {
			return nil, fmt.Errorf("invalid MCP server name %q", sv.Name)
		}
		if slices.Contains(names, sv.Name) {
			return nil, fmt.Errorf("MCP server %q is listed twice", sv.Name)
		}
		names = append(names, sv.Name)
	}
	return names, nil
}

// mcpServersCopy returns the mcp_servers of an exact replay: a copy of the
// source's, since every MCP answer comes from its cassette (spec §9.3).
func mcpServersCopy(stored json.RawMessage) json.RawMessage {
	if len(stored) == 0 {
		return json.RawMessage(`[]`)
	}
	return stored
}

// queueSet holds the queues of each route replayed from the cassette.
type queueSet struct {
	llm *match.Queues            // nil when the LLM is forwarded
	mcp map[string]*match.Queues // by server name
}

// unconsumed returns the recorded interactions of every route that the
// replay never served, in recorded order (spec §4.4).
func (q queueSet) unconsumed() []cassette.Interaction {
	var all []cassette.Interaction
	if q.llm != nil {
		all = q.llm.Unconsumed()
	}
	for _, mq := range q.mcp {
		all = append(all, mq.Unconsumed()...)
	}
	slices.SortFunc(all, func(a, b cassette.Interaction) int { return cmp.Compare(a.ID, b.ID) })
	return all
}

// replayQueues queues the recorded interactions of src by the fingerprint of
// their stored request bodies, which are already redacted, so they match
// incoming requests redacted with the same rules: one set of queues for the
// LLM route, when llm is set, and one per MCP server in servers. MCP requests that replay
// answers without matching are left out, so they are never reported as
// unconsumed (spec §5 step 3, AC-32).
func replayQueues(ctx context.Context, src *cassette.Cassette, llm bool, servers []string) (queueSet, error) {
	ins, err := src.Interactions(ctx)
	if err != nil {
		return queueSet{}, fmt.Errorf("reading interactions: %w", err)
	}
	type key struct {
		route string // "" for the LLM, or the MCP server name
		fp    string
	}
	keys := make(map[int64]key, len(ins))
	for _, in := range ins {
		var route string
		switch name, isMCP := strings.CutPrefix(in.Upstream, "mcp:"); {
		case in.Channel == "llm" && llm:
			route = ""
		case in.Channel == "mcp" && isMCP && slices.Contains(servers, name):
			route = name
		default:
			continue
		}
		var body []byte
		if in.RequestBlob != nil {
			if body, err = src.Blob(ctx, *in.RequestBlob); err != nil {
				return queueSet{}, fmt.Errorf("interaction %d: %w", in.ID, err)
			}
		}
		switch {
		case in.Channel == "llm":
			keys[in.ID] = key{route, match.Fingerprint(anthropic.Canonicalize(body))}
		case !replay.AnsweredWithoutMatching(in.HTTPMethod, body):
			keys[in.ID] = key{route, match.Fingerprint(mcp.Canonicalize(route, body))}
		}
	}
	queuesOf := func(route string) *match.Queues {
		return match.NewQueues(ins, func(in cassette.Interaction) (string, bool) {
			k, ok := keys[in.ID]
			return k.fp, ok && k.route == route
		})
	}
	qs := queueSet{mcp: make(map[string]*match.Queues, len(servers))}
	if llm {
		qs.llm = queuesOf("")
	}
	for _, name := range servers {
		qs.mcp[name] = queuesOf(name)
	}
	return qs, nil
}

// errNoDefaultID reports that the default replay id cannot be used, which is
// a usage error (spec §9.3). Other nextReplayID errors are runtime errors.
var errNoDefaultID = errors.New("no default replay id")

// nextReplayID returns the default id of a replay of source: source.mode.n,
// with n one more than the highest in use, or 1 (spec §9.3). Only an n in
// decimal digits, without leading zeros and below math.MaxInt64, counts: a
// crafted file name must not make n+1 overflow.
func nextReplayID(dir, source, mode string) (string, error) {
	prefix := source + "." + mode + "."
	// Not filepath.Glob: --dir may hold pattern characters such as '['.
	entries, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("listing replays of %s: %w", source, err)
	}
	n := 0
	for _, e := range entries {
		s, hasPrefix := strings.CutPrefix(e.Name(), prefix)
		s, hasSuffix := strings.CutSuffix(s, ".cassette")
		if !hasPrefix || !hasSuffix {
			continue
		}
		if s == "" || s[0] == '0' || strings.Trim(s, "0123456789") != "" {
			continue // not "07", "+7" or "x"
		}
		if k, err := strconv.Atoi(s); err == nil && k > n && k < math.MaxInt64 {
			n = k
		}
	}
	if n >= math.MaxInt64-1 {
		return "", fmt.Errorf("%w: the default replay number of %s would reach 2^63 - 1; name the replay with --as", errNoDefaultID, source)
	}
	id := fmt.Sprintf("%s%d", prefix, n+1)
	if err := cassette.ValidateSessionID(id); err != nil {
		return "", fmt.Errorf("%w: %w; name the replay with --as", errNoDefaultID, err)
	}
	return id, nil
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
			return nil, "", fmt.Errorf("creating session %s: %w", id, err)
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
