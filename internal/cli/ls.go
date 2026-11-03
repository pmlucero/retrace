package cli

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/pmlucero/retrace/internal/cassette"
	"github.com/pmlucero/retrace/internal/summary"
)

var lsCommand = command{
	name:    "ls",
	summary: "list the recorded and replayed sessions",
	run:     runLs,
}

const lsUsage = `Usage: retrace ls [--json]

Lists the sessions in the cassette directory, newest first, with their
totals. Costs are estimates from retrace's price table. A file that cannot
be read is skipped with a warning.

Flags:
  --json   emit one JSON array with an object per session, unabbreviated
`

// createdLayout is how ls shows created_at, in displayLocation.
const createdLayout = "2006-01-02 15:04:05"

// displayLocation is the time zone of the times shown as text: local time,
// which tests replace to get the same output on every machine.
var displayLocation = time.Local

// listed is one session as ls shows it.
type listed struct {
	session cassette.Session
	totals  summary.Totals
}

// runLs implements `retrace ls` (spec §9.5).
func runLs(ctx context.Context, env *Env, args []string) int {
	fs := flag.NewFlagSet("ls", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(fs.Output(), lsUsage) }
	asJSON := fs.Bool("json", false, "")
	if code, ok := parseCommandFlags(fs, args, env); !ok {
		return code
	}
	if fs.NArg() != 0 {
		return usageError(env.Stderr, "ls takes no arguments")
	}

	sessions, err := listSessions(ctx, env)
	if err != nil {
		env.Logger.Error("cannot list the sessions", "dir", env.Globals.Dir, "error", err)
		return ExitError
	}
	if *asJSON {
		err = writeLsJSON(env.Stdout, sessions)
	} else {
		err = writeLsTable(env.Stdout, sessions)
	}
	if err != nil {
		env.Logger.Error("cannot write the list", "error", err)
		return ExitError
	}
	return ExitOK
}

// listSessions reads every cassette in the directory, newest first. A
// directory that does not exist holds no sessions yet. A cassette that
// cannot be read is skipped with a warning (spec §9.5).
func listSessions(ctx context.Context, env *Env) ([]listed, error) {
	entries, err := os.ReadDir(env.Globals.Dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", env.Globals.Dir, err)
	}
	var out []listed
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".cassette") {
			continue
		}
		path := filepath.Join(env.Globals.Dir, entry.Name())
		s, events, err := readSession(ctx, path, env)
		if err != nil {
			env.Logger.Warn("skipping a cassette that cannot be read", "file", path, "error", err)
			continue
		}
		out = append(out, listed{session: s, totals: summary.Of(s, events)})
	}
	slices.SortStableFunc(out, func(a, b listed) int {
		return cmp.Or(b.session.CreatedAt.Compare(a.session.CreatedAt), cmp.Compare(a.session.ID, b.session.ID))
	})
	return out, nil
}

// readSession reads the session row and the events of the cassette at path.
func readSession(ctx context.Context, path string, env *Env) (cassette.Session, []cassette.Event, error) {
	c, err := cassette.OpenForRead(ctx, path)
	if err != nil {
		return cassette.Session{}, nil, err
	}
	defer func() {
		if err := c.Close(); err != nil {
			env.Logger.Debug("closing a cassette after reading it", "file", path, "error", err)
		}
	}()
	s, err := c.Session(ctx)
	if err != nil {
		return cassette.Session{}, nil, err
	}
	events, err := c.Events(ctx)
	if err != nil {
		return cassette.Session{}, nil, err
	}
	return s, events, nil
}

func writeLsTable(w io.Writer, sessions []listed) error {
	var buf bytes.Buffer
	tw := tabwriter.NewWriter(&buf, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tCREATED\tMODE\tPARENT\tSTATUS\tSTEPS\tTOKENS\tCOST (est.)\tDURATION")
	for _, l := range sessions {
		s, t := l.session, l.totals
		status := s.Status
		if s.DroppedEvents > 0 {
			// The steps and totals come from an incomplete log (spec §7.7).
			status += "+gaps"
		}
		var parent, duration string
		if s.ParentSessionID != nil {
			parent = *s.ParentSessionID
		}
		if t.Duration != nil {
			duration = summary.FormatDuration(*t.Duration)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%d\t%s/%s\t%s\t%s\n",
			printable(s.ID), s.CreatedAt.In(displayLocation).Format(createdLayout), printable(s.Mode),
			printable(parent), printable(status), t.Steps,
			summary.FormatTokens(t.TokensIn), summary.FormatTokens(t.TokensOut), summary.FormatCost(t), duration)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	return writeTrimmed(w, buf.String())
}

// writeTrimmed writes the lines of a table built with tabwriter without
// their trailing spaces: tabwriter pads every cell but the last, so a row
// whose last cells are empty, such as one without a duration, would end in
// spaces.
func writeTrimmed(w io.Writer, table string) error {
	for line := range strings.Lines(table) {
		if _, err := io.WriteString(w, strings.TrimRight(line, " \n")+"\n"); err != nil {
			return err
		}
	}
	return nil
}

// lsEntry is one session in `ls --json`. Its fields are unabbreviated, and
// cost_usd is the known sum: unpriced_responses says how many responses it
// leaves out (spec §7.5).
type lsEntry struct {
	ID                string     `json:"id"`
	CreatedAt         time.Time  `json:"created_at"`
	EndedAt           *time.Time `json:"ended_at"`
	Mode              string     `json:"mode"`
	ParentSessionID   *string    `json:"parent_session_id"`
	Status            string     `json:"status"`
	DroppedEvents     int        `json:"dropped_events"`
	Steps             int        `json:"steps"`
	ToolCalls         int        `json:"tool_calls"`
	TokensIn          int64      `json:"tokens_in"`
	TokensOut         int64      `json:"tokens_out"`
	CostUSD           float64    `json:"cost_usd"`
	UnpricedResponses int        `json:"unpriced_responses"`
	Models            []string   `json:"models"`
	DurationMillis    *int64     `json:"duration_ms"`
}

// newLsEntry is session s with its totals t. diff --format json describes
// each session with it too, so both documents name the totals alike.
func newLsEntry(s cassette.Session, t summary.Totals) lsEntry {
	e := lsEntry{
		ID: s.ID, CreatedAt: s.CreatedAt, EndedAt: s.EndedAt, Mode: s.Mode,
		ParentSessionID: s.ParentSessionID, Status: s.Status, DroppedEvents: s.DroppedEvents,
		Steps: t.Steps, ToolCalls: t.ToolCalls, TokensIn: t.TokensIn, TokensOut: t.TokensOut,
		CostUSD: t.CostUSD, UnpricedResponses: t.Unpriced, Models: t.Models,
	}
	if e.Models == nil {
		e.Models = []string{}
	}
	if t.Duration != nil {
		ms := t.Duration.Milliseconds()
		e.DurationMillis = &ms
	}
	return e
}

func writeLsJSON(w io.Writer, sessions []listed) error {
	entries := make([]lsEntry, 0, len(sessions))
	for _, l := range sessions {
		entries = append(entries, newLsEntry(l.session, l.totals))
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(entries)
}
