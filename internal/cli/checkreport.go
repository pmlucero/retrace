package cli

import (
	"bytes"
	"cmp"
	"fmt"
	"io"
	"slices"
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
	outcomeSkipped   = "skipped"
)

// outcomeOrder is the order in which every count of outcomes lists them
// (spec §9.9.7).
var outcomeOrder = []string{outcomeDiverged, outcomeError, outcomeSkipped, outcomeIdentical}

// outcomesText lists the outcomes that have a count, in the order of
// outcomeOrder: "3 diverged, 2 identical".
func outcomesText(n func(string) int) string {
	var parts []string
	for _, o := range outcomeOrder {
		if c := n(o); c > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", c, o))
		}
	}
	return strings.Join(parts, ", ")
}

// replayOutcome is one replay of a check.
type replayOutcome struct {
	id      string
	outcome string
	point   *diff.Point // where it diverged from its source, when it did
	// textSteps are the steps whose texts differ and were not compared, which
	// is only possible without --text (spec §8.3).
	textSteps []int
	totals    summary.Totals
	reason    string // why it is an error, when retrace knows: a timeout
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

// outcome is the first of diverged, error, skipped and identical that any replay has
// (spec §9.9.5).
func (s checkSession) outcome() string {
	for _, o := range []string{outcomeDiverged, outcomeError, outcomeSkipped} {
		if s.count(o) > 0 {
			return o
		}
	}
	return outcomeIdentical
}

// pointKey is what the corpus summary groups divergence points by: the event
// kind and the tool, which is empty for an event without one (a stop marker,
// a cassette miss).
type pointKey struct{ kind, tool string }

func keyOf(p diff.Point) pointKey {
	k := pointKey{kind: p.Kind}
	if p.Kind == diff.KindStop {
		return k
	}
	for _, e := range []*cassette.Event{p.A, p.B} {
		if e != nil && e.Tool != nil {
			k.tool = *e.Tool
			break
		}
	}
	return k
}

// text is the key as the corpus summary shows it, tool first.
func (k pointKey) text() string {
	if k.tool == "" {
		return k.kind
	}
	return printable(k.tool) + " " + k.kind
}

// sessionPoint is a divergence point of one session: a step and a key.
type sessionPoint struct {
	step int
	pointKey
}

// pointGroup is the replays of one session that diverged at one step, with
// one event kind and tool.
type pointGroup struct {
	sessionPoint
	replays int
}

// pointGroups groups the divergence points of the session's replays by step,
// event kind and tool. The groups are in descending order of count, then by
// step, kind and tool, so the output is deterministic.
func (s checkSession) pointGroups() []pointGroup {
	counts := map[sessionPoint]int{}
	for _, r := range s.replays {
		if r.point != nil {
			counts[sessionPoint{r.point.Step, keyOf(*r.point)}]++
		}
	}
	groups := make([]pointGroup, 0, len(counts))
	for k, n := range counts {
		groups = append(groups, pointGroup{k, n})
	}
	slices.SortFunc(groups, func(a, b pointGroup) int {
		return cmp.Or(cmp.Compare(b.replays, a.replays), cmp.Compare(a.step, b.step),
			cmp.Compare(a.kind, b.kind), cmp.Compare(a.tool, b.tool))
	})
	return groups
}

// pointsText lists the groups of pointGroups with the number of replays at
// each: "step 1 tool_use get_invoice (4), step 2 tool_call lookup (1)".
func (s checkSession) pointsText() string {
	groups := s.pointGroups()
	parts := make([]string, len(groups))
	for i, g := range groups {
		text := fmt.Sprintf("step %d %s", g.step, g.kind)
		if g.tool != "" {
			text += " " + printable(g.tool)
		}
		parts[i] = fmt.Sprintf("%s (%d)", text, g.replays)
	}
	return strings.Join(parts, ", ")
}

// textSteps is the union of the steps whose texts differ in any replay.
func (s checkSession) textSteps() []int {
	var steps []int
	for _, r := range s.replays {
		steps = append(steps, r.textSteps...)
	}
	slices.Sort(steps)
	return slices.Compact(steps)
}

// checkReport is what `retrace check` reports.
type checkReport struct {
	mode, model string
	repeat      int
	out         string
	sessions    []checkSession
	interrupted bool // a signal stopped the check (spec §9.9.4)

	budgetMax      float64 // --max-cost; 0 when not given
	budgetEstimate float64 // the estimate of the whole check, in swap-model
	budgetRefused  bool    // the estimate exceeded the budget: nothing ran
}

// budgetSkipped counts the replays that the budget skipped.
func (r checkReport) budgetSkipped() int {
	n := 0
	for _, s := range r.sessions {
		for _, rep := range s.replays {
			if rep.outcome == outcomeSkipped && rep.reason == reasonBudget {
				n++
			}
		}
	}
	return n
}

// exitCode follows spec §9.9.8: a divergence takes precedence over an error,
// and an interruption is an error. A budget skip, exit 5, comes below both.
func (r checkReport) exitCode() int {
	code := ExitOK
	if r.budgetSkipped() > 0 {
		code = ExitBudget
	}
	if r.interrupted {
		code = ExitError
	}
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

// corpusGroup is the sessions of the corpus that have a divergence point of
// one event kind and tool.
type corpusGroup struct {
	pointKey
	sessions int
}

// divergedGroups groups the divergence points of the whole corpus by event
// kind and tool, since steps differ between sessions, and counts the sessions
// that have each. The groups are in descending order of sessions, then by
// kind and tool.
func (r checkReport) divergedGroups() []corpusGroup {
	sessions := map[pointKey]int{}
	for _, s := range r.sessions {
		seen := map[pointKey]bool{}
		for _, rep := range s.replays {
			if rep.point == nil {
				continue
			}
			if k := keyOf(*rep.point); !seen[k] {
				seen[k] = true
				sessions[k]++
			}
		}
	}
	groups := make([]corpusGroup, 0, len(sessions))
	for k, n := range sessions {
		groups = append(groups, corpusGroup{k, n})
	}
	slices.SortFunc(groups, func(a, b corpusGroup) int {
		return cmp.Or(cmp.Compare(b.sessions, a.sessions), cmp.Compare(a.kind, b.kind), cmp.Compare(a.tool, b.tool))
	})
	return groups
}

// divergedAt lists the groups of divergedGroups: "search_orders tool_use (5
// sessions), get_invoice tool_use (1 session)".
func (r checkReport) divergedAt() string {
	groups := r.divergedGroups()
	parts := make([]string, len(groups))
	for i, g := range groups {
		parts[i] = fmt.Sprintf("%s (%s)", g.text(), plural(int64(g.sessions), "session"))
	}
	return strings.Join(parts, ", ")
}

// tally counts the sessions and the replays by outcome and adds up the cost
// of the replays.
func (r checkReport) tally() (sessions, replays map[string]int, cost summary.Totals) {
	sessions, replays = map[string]int{}, map[string]int{}
	for _, s := range r.sessions {
		sessions[s.outcome()]++
		for _, rep := range s.replays {
			replays[rep.outcome]++
			cost.CostUSD += rep.totals.CostUSD
			cost.Priced += rep.totals.Priced
			cost.Unpriced += rep.totals.Unpriced
		}
	}
	return sessions, replays, cost
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
	sessions, replays, cost := r.tally()
	for _, s := range r.sessions {
		line := outcomesText(s.count)
		if p := s.pointsText(); p != "" {
			line += "  " + p
		}
		// Without --text, a step whose texts differ is not a divergence; say
		// so as diff does, once per session, for the union of its replays.
		if note := textDiffersNote(s.textSteps()); note != "" {
			line += "  " + note
		}
		fmt.Fprintf(tw, "  %s\t%s\n", printable(s.id), line)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	counts := func(n map[string]int) string { return outcomesText(func(o string) int { return n[o] }) }
	fmt.Fprintf(&buf, "\nsessions     %s\nreplays      %s\n", counts(sessions), counts(replays))
	if at := r.divergedAt(); at != "" {
		fmt.Fprintf(&buf, "diverged at  %s\n", at)
	}
	fmt.Fprintf(&buf, "cost (est.)  %s\n", summary.FormatCost(cost))
	if r.budgetRefused {
		fmt.Fprintf(&buf, "budget       refused: the estimate $%.4f exceeds --max-cost $%.4f; no replay started\nreplays in   none (nothing was written)\n",
			r.budgetEstimate, r.budgetMax)
		_, err := w.Write(buf.Bytes())
		return err
	}
	if n := r.budgetSkipped(); n > 0 {
		fmt.Fprintf(&buf, "budget       reached: %s skipped (--max-cost $%.4f; costs are estimates)\n", plural(int64(n), "replay"), r.budgetMax)
	}
	fmt.Fprintf(&buf, "replays in   %s\n", printable(r.out))
	_, err := w.Write(buf.Bytes())
	return err
}
