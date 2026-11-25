package cli

import (
	"cmp"
	"context"
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/pmlucero/retrace/internal/cassette"
	"github.com/pmlucero/retrace/internal/diff"
	"github.com/pmlucero/retrace/internal/summary"
)

// unchosenShown is how many sessions the usage error of a check without an
// agent command lists (spec §9.9.3).
const unchosenShown = 10

var checkCommand = command{
	name:    "check",
	summary: "replay a corpus of sessions against a change and report what diverged",
	run:     runCheck,
}

const checkUsage = `Usage: retrace check [<id>...] [flags] (--recorded-command | -- <agent command...>)

Replays a corpus of recorded sessions, one after another, against one change
and reports which replays kept their source's trajectory and where the
others diverged. Without ids the corpus is every completed recording in the
cassette directory. The replays are written to --out; the cassette directory
is only read. Costs are estimates from retrace's price table.

The change under test is the agent's code (the command that runs), the model
(--model) or today's tools (--tools live). The recorded command never runs
unless --recorded-command is given: a cassette from someone else can run any
program. A corpus is code, so do not check one you do not trust.

Exit codes: 0 every replay identical, 1 an error, 2 a usage error, 3 a
replay diverged.

Flags:
`

// checkOptions are the flags of check. The replay flags are those of run
// (spec §9.9.2).
type checkOptions struct {
	ids    []string
	repeat int
	out    string
	run    runOptions // the replay flags shared by every replay
}

func parseCheckFlags(env *Env, args []string) (checkOptions, int, bool) {
	var opts checkOptions
	ro := &opts.run
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	fs.StringVar(&ro.model, "model", "", "replay with this model, forwarding LLM requests (swap-model)")
	fs.StringVar(&ro.tools, "tools", "frozen", "frozen, or live to forward MCP to the real servers (live-tools)")
	fs.IntVar(&opts.repeat, "repeat", 1, "replays of each session")
	fs.StringVar(&opts.out, "out", "", "directory for the replays (default .retrace/check/<check-id>)")
	fs.StringVar(&ro.cwd, "cwd", "", "run every replay's agent in this `dir`")
	fs.BoolVar(&ro.recorded, "recorded-command", false, "run each session's recorded command")
	fs.StringVar(&ro.onMiss, "on-miss", "strict", "divergence strategy")
	fs.DurationVar(&ro.grace, "grace", 5*time.Second, "how long the agent has to exit after SIGTERM on a cassette miss")
	fs.Func("redact-header", "also redact the request or response header with this literal `name` (repeatable)", redactHeaderFlag(&ro.redactHeader))
	upstream := fs.String("anthropic-upstream", "https://api.anthropic.com", "Anthropic API base URL (swap-model)")
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), checkUsage)
		fs.PrintDefaults()
	}

	rest := args
	if i := indexOf(args, "--"); i >= 0 {
		rest, ro.command = args[:i], args[i+1:]
	}
	positional, code, ok := parseWithPositional(fs, rest, env)
	if !ok {
		return opts, code, false
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	usage := func(msg string) (checkOptions, int, bool) { return opts, usageError(env.Stderr, msg), false }

	opts.ids = positional
	seen := map[string]bool{}
	for _, id := range opts.ids {
		if err := cassette.ValidateSessionID(id); err != nil {
			return usage(err.Error())
		}
		if seen[id] {
			return usage(fmt.Sprintf("session %q is given twice", id))
		}
		seen[id] = true
	}
	switch {
	case ro.command != nil && len(ro.command) == 0:
		return usage("no agent command after '--'")
	case ro.command != nil && ro.recorded:
		return usage("give the agent command after '--' or --recorded-command, not both")
	case ro.tools != "frozen" && ro.tools != "live":
		return usage(fmt.Sprintf("--tools %q: want frozen or live", ro.tools))
	case ro.model != "" && ro.tools == "live":
		return usage("--model with --tools live holds nothing constant; use record instead")
	case set["model"] && ro.model == "":
		return usage("--model: want a model name")
	case set["anthropic-upstream"] && ro.mode() != "swap-model":
		return usage("--anthropic-upstream is only used with --model: the LLM is answered from the cassette")
	case reservedStrategies[ro.onMiss]:
		return usage(fmt.Sprintf("--on-miss %s is not implemented in this version", ro.onMiss))
	case ro.onMiss != "strict":
		return usage(fmt.Sprintf("--on-miss %q: want strict", ro.onMiss))
	case ro.grace < 0:
		return usage(fmt.Sprintf("--grace %s: want a duration of 0 or more", ro.grace))
	case opts.repeat < 1:
		return usage(fmt.Sprintf("--repeat %d: want a positive integer", opts.repeat))
	case set["cwd"] && !isDir(ro.cwd):
		return usage(fmt.Sprintf("--cwd %s is not an existing directory", strconv.Quote(ro.cwd)))
	}
	u, err := absoluteHTTPURL(*upstream)
	if err != nil {
		return usage("--anthropic-upstream: " + err.Error())
	}
	ro.anthropic = u
	ro.listen = "127.0.0.1:0" // every replay has its own proxy on a free port (spec §9.9.4)
	return opts, 0, true
}

// runCheck implements `retrace check` (spec §9.9).
func runCheck(ctx context.Context, env *Env, args []string) int {
	opts, code, ok := parseCheckFlags(env, args)
	if !ok {
		return code
	}
	log := env.Logger
	mode := opts.run.mode()

	// The checks that need the corpus come before --out exists (spec §9.9.8).
	corpus, err := loadCorpus(ctx, env, opts.ids)
	if err != nil {
		log.Error("cannot read the corpus", "dir", env.Globals.Dir, "error", err)
		return ExitError
	}
	if len(corpus) == 0 {
		log.Error("the corpus is empty: no completed recording to check", "dir", env.Globals.Dir)
		return ExitError
	}
	if opts.run.command == nil && !opts.run.recorded {
		return usageError(env.Stderr, unchosenCorpusCommand(corpus))
	}
	for _, s := range corpus {
		ro := opts.run
		ro.source = s.ID
		src, err := cassette.OpenForRead(ctx, cassette.Path(env.Globals.Dir, s.ID))
		if err != nil {
			log.Error("cannot open the cassette", "session", s.ID, "error", err)
			return ExitError
		}
		_, perr := prepareReplay(ctx, src, s, ro)
		_ = src.Close()
		if perr != nil {
			return perr.report(env)
		}
		if err := cassette.ValidateSessionID(replayID(s.ID, mode, opts.repeat)); err != nil {
			return usageError(env.Stderr, fmt.Sprintf("session %q: %v; its replay ids <id>.<mode>.<k> must fit the id format", s.ID, err))
		}
	}
	out := opts.out
	if out == "" {
		id, err := cassette.NewSessionID("", time.Now(), rand.Reader)
		if err != nil {
			log.Error("cannot choose the check's id", "error", err)
			return ExitError
		}
		out = filepath.Join(".retrace", "check", id)
	}
	// Glob fails only on a bad pattern, which globEscape rules out.
	if held, _ := filepath.Glob(filepath.Join(globEscape(out), "*.cassette")); len(held) > 0 {
		return usageError(env.Stderr, fmt.Sprintf("--out %s already holds cassettes; give a new directory", strconv.Quote(out)))
	}
	if err := os.MkdirAll(out, 0o700); err != nil {
		log.Error("cannot create --out", "dir", out, "error", err)
		return ExitError
	}

	report := checkReport{mode: mode, model: opts.run.model, repeat: opts.repeat, out: out}
	for _, s := range corpus {
		sr := checkSession{id: s.ID}
		for k := 1; k <= opts.repeat; k++ {
			r := checkOne(ctx, env, opts.run, s, replayID(s.ID, mode, k), out)
			fmt.Fprintf(env.Stderr, "%s  %s\n", printable(r.id), r.outcome)
			sr.replays = append(sr.replays, r)
		}
		report.sessions = append(report.sessions, sr)
	}
	if err := report.writeText(env.Stdout); err != nil {
		log.Error("cannot write the report", "error", err)
		return ExitError
	}
	return report.exitCode()
}

func replayID(source, mode string, k int) string { return fmt.Sprintf("%s.%s.%d", source, mode, k) }

// globEscape escapes the pattern characters of a directory name.
func globEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, "*", `\*`, "?", `\?`, "[", `\[`).Replace(s)
}

// loadCorpus returns the session rows to check, in id order, comparing ids
// byte by byte (spec §9.9.1). Given ids must exist. Without ids the corpus is
// every completed recording: replays have a parent and are left out.
func loadCorpus(ctx context.Context, env *Env, ids []string) ([]cassette.Session, error) {
	var corpus []cassette.Session
	if len(ids) == 0 {
		all, err := listSessions(ctx, env)
		if err != nil {
			return nil, err
		}
		for _, l := range all {
			if l.session.ParentSessionID == nil && l.session.Status == "completed" {
				corpus = append(corpus, l.session)
			}
		}
	}
	for _, id := range ids {
		path := cassette.Path(env.Globals.Dir, id)
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("no such session %q", id)
		}
		s, _, err := readSession(ctx, path, env, (*cassette.Cassette).EventHeads)
		if err != nil {
			return nil, fmt.Errorf("reading session %q: %w", id, err)
		}
		corpus = append(corpus, s)
	}
	slices.SortFunc(corpus, func(a, b cassette.Session) int { return cmp.Compare(a.ID, b.ID) })
	return corpus, nil
}

// unchosenCorpusCommand is the usage error of a check with neither '--' nor
// --recorded-command: the argv and cwd that each session recorded, quoted as
// in run (spec §9.9.3, §9.3).
func unchosenCorpusCommand(corpus []cassette.Session) string {
	var b strings.Builder
	b.WriteString("check needs the agent command after '--', or --recorded-command to run the ones that the sessions recorded.\n")
	for _, s := range corpus[:min(len(corpus), unchosenShown)] {
		args := make([]string, len(s.Command))
		for i, a := range s.Command {
			args[i] = strconv.Quote(a)
		}
		fmt.Fprintf(&b, "session %s:\n  argv (Go-quoted, not shell syntax): %s\n  cwd: %s\n",
			strconv.Quote(s.ID), strings.Join(args, " "), strconv.Quote(s.Cwd))
	}
	if n := len(corpus) - unchosenShown; n > 0 {
		fmt.Fprintf(&b, "and %d more\n", n)
	}
	b.WriteString("Review them first: a cassette from someone else can run any program on this machine.")
	return b.String()
}

// checkOne runs one replay of session s and classifies it (spec §9.9.5). A
// replay that cannot run has the outcome error and never stops the check.
func checkOne(ctx context.Context, env *Env, ro runOptions, s cassette.Session, id, out string) replayOutcome {
	fail := func(why string, err error) replayOutcome {
		env.Logger.Warn("replay failed", "replay", id, "reason", why, "error", err)
		return replayOutcome{id: id, outcome: outcomeError}
	}
	ro.source = s.ID
	srcPath := cassette.Path(env.Globals.Dir, s.ID)
	src, err := cassette.OpenForRead(ctx, srcPath)
	if err != nil {
		return fail("opening the source", err)
	}
	defer src.Close()
	plan, perr := prepareReplay(ctx, src, s, ro)
	if perr != nil {
		return fail(perr.msg, perr)
	}
	// The agents' output goes to stderr, so that stdout holds only the report.
	agentEnv := *env
	agentEnv.Stdout = env.Stderr
	res, err := executeReplay(ctx, &agentEnv, plan, replayTarget{dir: out, id: id, explicit: true, listen: ro.listen})
	if err != nil {
		return replayOutcome{id: id, outcome: outcomeError} // executeReplay logged it
	}
	srcEvents, err := src.Events(ctx)
	if err != nil {
		return fail("reading the source", err)
	}
	rs, events, err := readSession(ctx, res.path, env, (*cassette.Cassette).Events)
	if err != nil {
		return fail("reading the replay", err)
	}
	cmpRes := diff.Compare(diff.Build(srcEvents), diff.Build(events), diff.Options{})
	r := replayOutcome{id: id, point: cmpRes.Point, totals: summary.Of(rs, events)}
	wantExit := 0
	if s.AgentExitCode != nil {
		wantExit = *s.AgentExitCode
	}
	switch {
	case res.status == "diverged" || cmpRes.Point != nil:
		r.outcome = outcomeDiverged
	case res.status != "completed" || res.agentExit != wantExit:
		r.outcome = outcomeError
	default:
		r.outcome = outcomeIdentical
	}
	return r
}
