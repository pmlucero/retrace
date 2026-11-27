package cli

import (
	"context"
	"os"

	"github.com/pmlucero/retrace/internal/cassette"
	"github.com/pmlucero/retrace/internal/replay/session"
)

// prepareReplay resolves the inputs of a replay of src, whose session row is
// sess, and hands them to session.Prepare. The command and the working
// directory come from the flags, so they are checked first, as spec §9.3
// orders: usage errors come before the replay's cassette exists. With
// opts.cwd set the agent runs there, and the recorded working directory is
// not checked (spec §9.9.3).
func prepareReplay(ctx context.Context, src *cassette.Cassette, sess cassette.Session, opts runOptions) (*session.Plan, *session.PrepareError) {
	command, recorded, err := replayCommand(opts, sess)
	if err != nil {
		return nil, session.UsagePrepareError(err)
	}
	cwd := sess.Cwd
	switch {
	case opts.cwd != "":
		cwd = opts.cwd
	case !recorded:
		if cwd, err = os.Getwd(); err != nil {
			return nil, &session.PrepareError{Msg: "reading the working directory", Attrs: []any{"error", err}}
		}
	}
	return session.Prepare(ctx, src, sess, session.Params{
		Source: opts.source, Mode: opts.mode(), Model: opts.model, Anthropic: opts.anthropic,
		Command: command, Cwd: cwd, RedactHeaders: opts.redactHeader, MCP: opts.mcp,
		Realtime: opts.realtime, Grace: opts.grace,
		ParseURL: absoluteHTTPURL, Printable: printable,
	})
}

// reportPrepare prints a failure of prepareReplay the way run always has and
// returns its exit code.
func reportPrepare(env *Env, e *session.PrepareError) int {
	if e.Usage {
		return usageError(env.Stderr, e.Msg)
	}
	env.Logger.Error(e.Msg, e.Attrs...)
	return ExitError
}

// sessionEnv returns what session.Execute needs from env.
func sessionEnv(env *Env) session.Env {
	return session.Env{
		Logger: env.Logger, Stdin: env.Stdin, Stdout: env.Stdout, Stderr: env.Stderr,
		NotifySignals: env.notifySignals, Version: Version(),
	}
}
