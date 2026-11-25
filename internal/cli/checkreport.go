package cli

import (
	"bytes"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/pmlucero/retrace/internal/cassette"
	"github.com/pmlucero/retrace/internal/diff"
	"github.com/pmlucero/retrace/internal/summary"
)

// The outcomes of a replay (spec §9.9.5). skipped comes with the budget.
const (
	outcomeDiverged  = "diverged"
	outcomeError     = "error"
	outcomeIdentical = "identical"
)

// outcomes in the order the summary lists them.
var outcomeOrder = []string{outcomeIdentical, outcomeDiverged, outcomeError}

// replayOutcome is one replay of a check.
type replayOutcome struct {
	id      string
	outcome string
	point   *diff.Point // where it diverged from its source, when it did
	totals  summary.Totals
}

type checkSession struct {
	id      string
	replays []replayOutcome
}

// count returns how many replays of the session have outcome o.
func (s checkSession) count(o string) int {
	n := 0
	for _, r := range s.replays {
		if r.outcome == o {
			n++
		}
	}
	return n
}

// outcome is the first of diverged, error and identical that any replay has
// (spec §9.9.5).
func (s checkSession) outcome() string {
	for _, o := range []string{outcomeDiverged, outcomeError} {
		if s.count(o) > 0 {
			return o
		}
	}
	return outcomeIdentical
}

// firstPoint is the divergence point of the first replay that has one.
func (s checkSession) firstPoint() *diff.Point {
	for _, r := range s.replays {
		if r.point != nil {
			return r.point
		}
	}
	return nil
}

// checkReport is what `retrace check` reports.
type checkReport struct {
	mode, model string
	repeat      int
	out         string
	sessions    []checkSession
}

// exitCode follows spec §9.9.8: a divergence takes precedence over an error.
func (r checkReport) exitCode() int {
	code := ExitOK
	for _, s := range r.sessions {
		switch s.outcome() {
		case outcomeDiverged:
			return ExitDiverged
		case outcomeError:
			code = ExitError
		}
	}
	return code
}

// writeText writes the text output (spec §9.9.7). Every value read from a
// cassette goes through printable.
func (r checkReport) writeText(w io.Writer) error {
	var buf bytes.Buffer
	mode := r.mode
	if r.model != "" {
		mode += " (" + printable(r.model) + ")"
	}
	fmt.Fprintf(&buf, "retrace check  %s  repeat %d  %s\n\n", mode, r.repeat, plural(int64(len(r.sessions)), "session"))

	tw := tabwriter.NewWriter(&buf, 0, 0, 3, ' ', 0)
	sessions, replays := map[string]int{}, map[string]int{}
	var cost summary.Totals
	for _, s := range r.sessions {
		// A session that is not identical lists its diverged and error
		// replays: the rest of its replays were identical.
		line := fmt.Sprintf("%d/%d identical", s.count(outcomeIdentical), len(s.replays))
		if s.outcome() != outcomeIdentical {
			var parts []string
			for _, o := range []string{outcomeDiverged, outcomeError} {
				if n := s.count(o); n > 0 {
					parts = append(parts, fmt.Sprintf("%d/%d %s", n, len(s.replays), o))
				}
			}
			line = strings.Join(parts, ", ")
		}
		if p := s.firstPoint(); p != nil && s.outcome() == outcomeDiverged {
			line += "  " + pointText(*p)
		}
		fmt.Fprintf(tw, "  %s\t%s\n", printable(s.id), line)
		sessions[s.outcome()]++
		for _, rep := range s.replays {
			replays[rep.outcome]++
			cost.CostUSD += rep.totals.CostUSD
			cost.Priced += rep.totals.Priced
			cost.Unpriced += rep.totals.Unpriced
		}
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	counts := func(n map[string]int) string {
		var parts []string
		for _, o := range outcomeOrder {
			if n[o] > 0 {
				parts = append(parts, fmt.Sprintf("%d %s", n[o], o))
			}
		}
		return strings.Join(parts, ", ")
	}
	fmt.Fprintf(&buf, "\nsessions     %s\nreplays      %s\ncost (est.)  %s\nreplays in   %s\n",
		counts(sessions), counts(replays), summary.FormatCost(cost), printable(r.out))
	_, err := w.Write(buf.Bytes())
	return err
}

// pointText names a divergence point by its step, event kind and tool.
func pointText(p diff.Point) string {
	if p.Kind == diff.KindStop {
		return fmt.Sprintf("step %d stop", p.Step)
	}
	text := fmt.Sprintf("step %d %s", p.Step, p.Kind)
	for _, e := range []*cassette.Event{p.A, p.B} {
		if e != nil && e.Tool != nil {
			return text + " " + printable(*e.Tool)
		}
	}
	return text
}
