package cli

import (
	"context"
	"errors"

	"github.com/pmlucero/retrace/internal/agentrun"
	"github.com/pmlucero/retrace/internal/cassette"
)

// insertSession writes a new cassette's session row. It is a variable so that
// tests can make the insert fail. Tests that replace it must not run in
// parallel with tests that create sessions.
var insertSession = (*cassette.Tx).InsertSession

// createSessionCassette creates the cassette at path with session row s. If
// the row cannot be written, it closes and removes the file, so a failed start
// leaves no cassette without a session and the id stays free (#56). Errors
// from cassette.Create are wrapped, so callers can test for
// cassette.ErrExists. Errors name the path or the session already.
func createSessionCassette(ctx context.Context, path string, s cassette.Session) (*cassette.Cassette, error) {
	c, err := cassette.Create(ctx, path)
	if err != nil {
		return nil, err
	}
	if err := c.Write(ctx, func(tx *cassette.Tx) error { return insertSession(tx, ctx, s) }); err != nil {
		// Create made this file (it refuses an existing one), so removing it
		// cannot touch another session's cassette.
		return nil, errors.Join(err, c.Close(), cassette.Remove(path))
	}
	return c, nil
}

// replaceSessionCassette creates the cassette at path with session row s,
// replacing the cassette there, if any (record --force, spec §9.2). The old
// cassette is kept unless the new one gets its session row (#111).
func replaceSessionCassette(ctx context.Context, path string, s cassette.Session) (*cassette.Cassette, error) {
	return cassette.Replace(ctx, path, func(tx *cassette.Tx) error { return insertSession(tx, ctx, s) })
}

// ExitAgentFailed is returned when the agent exits with a non-zero code
// (spec §9.7).
const ExitAgentFailed = 4

// sessionExitCode maps how the session ended to retrace's exit code (spec §9.7).
func sessionExitCode(r agentrun.Result) int {
	switch {
	case r.Status == "failed":
		return ExitError
	case r.Status == "diverged":
		return ExitDiverged
	case r.Agent.ExitCode != 0:
		return ExitAgentFailed
	}
	return ExitOK
}
