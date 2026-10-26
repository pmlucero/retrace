package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/pmlucero/retrace/internal/cassette"
	"github.com/pmlucero/retrace/internal/derive"
	"github.com/pmlucero/retrace/internal/provider/anthropic"
)

var reindexCommand = command{
	name:    "reindex",
	summary: "rebuild the events of a cassette with this version of retrace",
	run:     runReindex,
}

const reindexUsage = `Usage: retrace reindex <id>

Rebuilds the events of session <id> from its recorded interactions, with
this version's event derivation and price table. Interactions are never
modified.
`

// runReindex implements `retrace reindex` (spec §7.6, §9.8).
func runReindex(ctx context.Context, env *Env, args []string) int {
	fs := flag.NewFlagSet("reindex", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(fs.Output(), reindexUsage) }
	if code, ok := parseCommandFlags(fs, args, env); !ok {
		return code
	}
	if fs.NArg() != 1 {
		return usageError(env.Stderr, "reindex needs exactly one session id")
	}
	id := fs.Arg(0)
	if err := cassette.ValidateSessionID(id); err != nil {
		return usageError(env.Stderr, err.Error())
	}
	log := env.Logger

	path := cassette.Path(env.Globals.Dir, id)
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		log.Error("no such session", "session", id, "dir", env.Globals.Dir)
		return ExitError
	}
	c, err := cassette.Open(ctx, path)
	if err != nil {
		log.Error("cannot open the cassette", "error", err)
		return ExitError
	}
	n, err := derive.Reindex(ctx, c)
	if cerr := c.Close(); err == nil && cerr != nil {
		err = cerr
	}
	if err != nil {
		log.Error("cannot reindex the session", "session", id, "error", err)
		return ExitError
	}
	fmt.Fprintf(env.Stdout, "reindexed %s: %d events (deriver v%d, pricing %s)\n",
		id, n, derive.Version, anthropic.PricingVersion())
	return ExitOK
}
