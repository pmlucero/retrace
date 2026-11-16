package cli

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/pmlucero/retrace/internal/cassette"
)

// replayCommand returns the agent command of a replay (spec §9.3, ADR 0013):
// the command after '--', or with --recorded-command the source's recorded
// command, which then runs in its recorded cwd (recorded is true). The
// recorded command is never run unless asked for, because a cassette from
// someone else is untrusted input. Every error is a usage error.
func replayCommand(opts runOptions, src cassette.Session) (command []string, recorded bool, err error) {
	switch {
	case opts.command != nil:
		return opts.command, false, nil
	case len(src.Command) == 0:
		// --recorded-command would not help, so it is not suggested.
		return nil, false, fmt.Errorf("session %q has no recorded command; give the agent command after '--'", opts.source)
	case opts.recorded:
		if !isDir(src.Cwd) {
			return nil, false, fmt.Errorf("the recorded working directory %s of session %q does not exist "+
				"or is not a directory; give the agent command after '--'", strconv.Quote(src.Cwd), opts.source)
		}
		return src.Command, true, nil
	}
	return nil, false, errors.New(unchosenCommand(opts.source, src))
}

// isDir reports whether path is a directory, following symbolic links. A
// path that cannot be inspected, for lack of permission for example, counts
// as missing (spec §9.3). It can still disappear before the agent starts;
// the start then fails as for any agent that cannot be started (spec §3.1).
func isDir(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}

// unchosenCommand is the usage error of a run with neither '--' nor
// --recorded-command. It prints the recorded argv and cwd as quoted strings,
// so that the user can review them before choosing. strconv.Quote escapes
// every control and bidirectional control character, so the text can
// neither drive the terminal nor hide part of the command. The quoting is
// Go's, not the shell's, and the label says so: pasted after '--', "$(...)"
// would be expanded by the shell, unlike in the recorded argv.
func unchosenCommand(id string, src cassette.Session) string {
	args := make([]string, len(src.Command))
	for i, a := range src.Command {
		args[i] = strconv.Quote(a)
	}
	return fmt.Sprintf("run needs the agent command after '--', or --recorded-command to run the one that "+
		"session %q recorded:\n  argv (Go-quoted, not shell syntax): %s\n  cwd: %s\n"+
		"Review it first: a cassette from someone else can run any program on this machine.",
		id, strings.Join(args, " "), strconv.Quote(src.Cwd))
}
