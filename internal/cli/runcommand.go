package cli

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/pmlucero/retrace/internal/cassette"
)

// replayCommand returns the agent command of a replay and its working
// directory (spec §9.3, ADR 0013): the command after '--', run in retrace's
// working directory, or with --recorded-command the source's recorded
// command in its recorded cwd. The recorded command is never run unless
// asked for, because a cassette from someone else is untrusted input. A
// non-empty usage message reports a usage error.
func replayCommand(opts runOptions, src cassette.Session) (command []string, cwd, usage string, err error) {
	switch {
	case opts.command != nil:
		cwd, err := os.Getwd()
		if err != nil {
			return nil, "", "", fmt.Errorf("reading the working directory: %w", err)
		}
		return opts.command, cwd, "", nil
	case len(src.Command) == 0:
		// --recorded-command would not help, so it is not suggested.
		return nil, "", fmt.Sprintf("session %q has no recorded command; give the agent command after '--'", opts.source), nil
	case opts.recorded:
		return src.Command, src.Cwd, "", nil
	}
	return nil, "", unchosenCommand(opts.source, src), nil
}

// unchosenCommand is the usage error of a run with neither '--' nor
// --recorded-command. It prints the recorded command and cwd as quoted
// strings, so that the user can review them before choosing. strconv.Quote
// escapes every control and bidirectional control character, so the text
// can neither drive the terminal nor hide part of the command.
func unchosenCommand(id string, src cassette.Session) string {
	args := make([]string, len(src.Command))
	for i, a := range src.Command {
		args[i] = strconv.Quote(a)
	}
	return fmt.Sprintf("run needs the agent command after '--', or --recorded-command to run the one that "+
		"session %q recorded:\n  command: %s\n  cwd:     %s\n"+
		"Review it first: a cassette from someone else can run any program on this machine.",
		id, strings.Join(args, " "), strconv.Quote(src.Cwd))
}
