// Command retrace records, replays and diffs AI agent sessions.
//
// All logic lives in internal/cli; main only wires the process to it.
package main

import (
	"context"
	"os"

	"github.com/pmlucero/retrace/internal/cli"
)

func main() {
	os.Exit(cli.Run(context.Background(), os.Args[1:], os.Stdout, os.Stderr, os.Getenv))
}
