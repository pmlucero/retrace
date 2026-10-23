// Package cli implements the retrace command-line interface (spec §9):
// global flags, subcommand dispatch and exit codes.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"strings"
)

// Exit codes shared by all commands (spec §9.7). Command-specific codes
// (3: divergence, 4: agent failure) are defined with the commands that use them.
const (
	ExitOK    = 0
	ExitError = 1
	ExitUsage = 2
)

// Defaults for global flags, used when neither the flag nor its environment variable is set.
const (
	defaultDir      = ".retrace/cassettes"
	defaultLogLevel = "info"
)

// Globals holds the global flags (spec §9.1), resolved with precedence flag > environment > default.
type Globals struct {
	Dir      string
	LogLevel slog.Level
	NoColor  bool
}

// Env is what a command receives: resolved globals, output streams and a logger.
type Env struct {
	Globals Globals
	Stdout  io.Writer
	Stderr  io.Writer
	Logger  *slog.Logger
	Getenv  func(string) string
}

// command is one retrace subcommand. run returns the process exit code.
type command struct {
	name    string
	summary string
	run     func(ctx context.Context, env *Env, args []string) int
}

// commands are added by the milestones that implement them (docs/plan/mvp.md).
var commands []command

// Run parses global flags, dispatches to a subcommand and returns the exit code.
// getenv is injected so tests can control environment variables.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer, getenv func(string) string) int {
	return run(ctx, args, stdout, stderr, getenv, commands)
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer, getenv func(string) string, cmds []command) int {
	fs := flag.NewFlagSet("retrace", flag.ContinueOnError)
	fs.SetOutput(io.Discard) // errors and help are printed by us, to the right stream
	dir := fs.String("dir", envOr(getenv, "RETRACE_DIR", defaultDir), "cassette directory (env RETRACE_DIR)")
	logLevel := fs.String("log-level", envOr(getenv, "RETRACE_LOG_LEVEL", defaultLogLevel), "debug, info, warn or error (env RETRACE_LOG_LEVEL)")
	noColor := fs.Bool("no-color", false, "disable colored output (env NO_COLOR)")
	showVersion := fs.Bool("version", false, "print the version and exit")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printUsage(stdout, fs, cmds)
			return ExitOK
		}
		return usageError(stderr, err.Error())
	}
	if *showVersion {
		fmt.Fprintf(stdout, "retrace %s\n", Version())
		return ExitOK
	}

	level, err := parseLogLevel(*logLevel)
	if err != nil {
		if !flagWasSet(fs, "log-level") {
			// The value came from the environment: say so, or the error looks inexplicable (spec §9.1).
			err = fmt.Errorf("%w (from RETRACE_LOG_LEVEL)", err)
		}
		return usageError(stderr, err.Error())
	}

	// Global flags precede the command (spec §9.1): parsing stops at the command
	// name, and everything after it belongs to the command.
	if fs.NArg() == 0 {
		printUsage(stderr, fs, cmds)
		return ExitUsage
	}
	name, cmdArgs := fs.Arg(0), fs.Args()[1:]
	cmd, ok := findCommand(cmds, name)
	if !ok {
		return usageError(stderr, fmt.Sprintf("unknown command %q", name))
	}

	env := &Env{
		// NO_COLOR (any non-empty value) and --no-color can each only disable color (spec §9.1).
		Globals: Globals{Dir: *dir, LogLevel: level, NoColor: *noColor || getenv("NO_COLOR") != ""},
		Stdout:  stdout,
		Stderr:  stderr,
		Logger:  slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: level})),
		Getenv:  getenv,
	}
	return cmd.run(ctx, env, cmdArgs)
}

func envOr(getenv func(string) string, key, fallback string) string {
	if v := getenv(key); v != "" {
		return v
	}
	return fallback
}

func flagWasSet(fs *flag.FlagSet, name string) bool {
	set := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			set = true
		}
	})
	return set
}

func parseLogLevel(s string) (slog.Level, error) {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("invalid log level %q: want debug, info, warn or error", s)
	}
}

func findCommand(cmds []command, name string) (command, bool) {
	for _, c := range cmds {
		if c.name == name {
			return c, true
		}
	}
	return command{}, false
}

func usageError(stderr io.Writer, msg string) int {
	fmt.Fprintf(stderr, "retrace: %s\nRun 'retrace --help' for usage.\n", msg)
	return ExitUsage
}

func printUsage(w io.Writer, fs *flag.FlagSet, cmds []command) {
	fmt.Fprintln(w, "Usage: retrace [global flags] <command> [arguments]")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Commands:")
	if len(cmds) == 0 {
		fmt.Fprintln(w, "  (none yet: commands are added as the MVP milestones land)")
	}
	for _, c := range cmds {
		fmt.Fprintf(w, "  %-10s %s\n", c.name, c.summary)
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Global flags:")
	fs.SetOutput(w)
	fs.PrintDefaults()
	fs.SetOutput(io.Discard)
}
