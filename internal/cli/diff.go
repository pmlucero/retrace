package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/pmlucero/retrace/internal/cassette"
	"github.com/pmlucero/retrace/internal/diff"
	"github.com/pmlucero/retrace/internal/summary"
)

// ExitDiffFound is the exit code of `retrace diff` when the trajectories
// differ. diff follows diff(1), so every error is ExitUsage (spec §8.4).
const ExitDiffFound = 1

var diffCommand = command{
	name:    "diff",
	summary: "compare the trajectories of two sessions",
	run:     runDiff,
}

const diffUsage = `Usage: retrace diff <id-a> <id-b> [--format text|json] [--text]

Compares the trajectories of sessions <id-a> and <id-b>: their tool calls,
tool results and stops, step by step. It prints both sessions' totals, the
first divergence, if any, and how many steps were identical before it.
Model text is not compared unless --text is given. Costs are estimates from
retrace's price table.

--format json prints the same information as one JSON document, whose
schema is docs/schema/diff.v1.json in the retrace repository.

Exit codes: 0 identical, 1 the trajectories differ, 2 an error.

Flags:
  --format text|json   the output format (default text)
  --text               also compare the text of each model response
`

// runDiff implements `retrace diff` (spec §8, §9.4).
func runDiff(ctx context.Context, env *Env, args []string) int {
	fs := flag.NewFlagSet("diff", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(fs.Output(), diffUsage) }
	format := fs.String("format", "text", "")
	text := fs.Bool("text", false, "")
	positional, code, ok := parseWithPositional(fs, args, env)
	if !ok {
		return code
	}
	write := diffReport.writeText
	switch *format {
	case "text":
	case "json":
		write = diffReport.writeJSON
	default:
		return usageError(env.Stderr, fmt.Sprintf("--format %q: want text or json", *format))
	}
	if len(positional) != 2 {
		return usageError(env.Stderr, "diff needs exactly two session ids")
	}
	var sides [2]diffSide
	for i, id := range positional {
		if err := cassette.ValidateSessionID(id); err != nil {
			return usageError(env.Stderr, err.Error())
		}
		path := cassette.Path(env.Globals.Dir, id)
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			env.Logger.Error("no such session", "session", id, "dir", env.Globals.Dir)
			return ExitUsage
		}
		s, events, err := readSession(ctx, path, env)
		if err != nil {
			env.Logger.Error("cannot read the session", "session", id, "error", err)
			return ExitUsage
		}
		sides[i] = diffSide{id: id, session: s, totals: summary.Of(s, events), trajectory: diff.Build(events)}
	}

	r := newDiffReport(sides[0], sides[1], diff.Options{Text: *text})
	if err := write(r, env.Stdout); err != nil {
		env.Logger.Error("cannot write the diff", "error", err)
		return ExitUsage
	}
	if r.result.Point != nil {
		return ExitDiffFound
	}
	return ExitOK
}

// diffSide is one of the two sessions of a diff.
type diffSide struct {
	id         string // as given on the command line, so already validated
	session    cassette.Session
	totals     summary.Totals
	trajectory diff.Trajectory
	// diverged is the first divergence event, or nil, and divergedStep its
	// step (spec §8.3 step 4).
	diverged     *cassette.Event
	divergedStep int
}

// stopped reports whether the side ended on a strict stop: it has a
// divergence event, or its status says so even though the event was not
// written (spec §7.7).
func (s diffSide) stopped() bool {
	return s.diverged != nil || s.session.Status == "diverged"
}

// diffReport is what `retrace diff` reports. It is built once, so that each
// output format only writes it.
type diffReport struct {
	a, b         diffSide
	result       diff.Result
	textCompared bool // --text
}

func newDiffReport(a, b diffSide, opts diff.Options) diffReport {
	res := diff.Compare(a.trajectory, b.trajectory, opts)
	a.diverged, a.divergedStep = res.DivergedA, stepOf(a.trajectory, res.DivergedA)
	b.diverged, b.divergedStep = res.DivergedB, stepOf(b.trajectory, res.DivergedB)
	return diffReport{a: a, b: b, result: res, textCompared: opts.Text}
}

// stepOf returns the number of the step that holds divergence event e, or 0.
func stepOf(t diff.Trajectory, e *cassette.Event) int {
	if e == nil {
		return 0
	}
	for _, s := range t.Steps {
		for i := range s.Divergences {
			// Compare returns a pointer into the trajectory, so identity
			// finds the event even if an untrusted cassette repeats a seq.
			if &s.Divergences[i] == e {
				return s.N
			}
		}
	}
	return 0
}

// writeText writes the text output (spec §8.4). Every value read from a
// cassette goes through printable or diffValue: cassettes are untrusted
// input (SECURITY.md).
func (r diffReport) writeText(w io.Writer) error {
	var buf bytes.Buffer
	for _, s := range []diffSide{r.a, r.b} {
		if n := s.session.DroppedEvents; n > 0 {
			fmt.Fprintf(&buf, "warning: session %s is missing %s from its event log; the divergence point may be caused by missing events\n",
				s.id, plural(int64(n), "event"))
		}
	}
	if r.a.session.DroppedEvents > 0 || r.b.session.DroppedEvents > 0 {
		buf.WriteString("\n")
	}
	fmt.Fprintf(&buf, "retrace diff %s  %s\n\n", r.a.id, r.b.id)
	if err := r.writeTotals(&buf); err != nil {
		return err
	}
	buf.WriteString("\n")

	res := r.result
	var notes []string
	if res.Point == nil {
		fmt.Fprintf(&buf, "✓ trajectories identical (%s, %s)\n",
			plural(int64(r.a.totals.Steps), "step"), plural(int64(res.Identical.ToolCalls), "tool call"))
	} else {
		writePoint(&buf, *res.Point)
		if span := res.Identical; span.Last >= span.First {
			notes = append(notes, fmt.Sprintf("%s identical (%s)", stepSpan(span), plural(int64(span.ToolCalls), "tool call")))
		}
	}
	// Spec §8.3 step 4: a strict stop is reported even when both sides
	// stopped at the same place, which leaves no divergence point.
	for _, s := range []struct {
		name string
		side diffSide
	}{{"A", r.a}, {"B", r.b}} {
		if s.side.diverged != nil {
			notes = append(notes, fmt.Sprintf("%s stopped at step %d: %s", s.name, s.side.divergedStep, missReason(*s.side.diverged)))
		}
	}
	if len(notes) > 0 {
		buf.WriteString("\n")
	}
	for _, n := range notes {
		fmt.Fprintf(&buf, "  %s\n", n)
	}
	if steps := res.TextDifferences; len(steps) > 0 {
		names := make([]string, len(steps))
		for i, n := range steps {
			names[i] = strconv.Itoa(n)
		}
		noun := "steps"
		if len(steps) == 1 {
			noun = "step"
		}
		fmt.Fprintf(&buf, "\ntext differs in %s %s (not compared; use --text)\n", noun, strings.Join(names, ", "))
	}
	_, err := w.Write(buf.Bytes())
	return err
}

// writeTotals writes the two-column summary of the sessions (spec §8.2).
func (r diffReport) writeTotals(w io.Writer) error {
	var buf bytes.Buffer
	tw := tabwriter.NewWriter(&buf, 0, 0, 4, ' ', 0)
	row := func(name, a, b string) { fmt.Fprintf(tw, "%s\t%s\t%s\n", name, a, b) }
	a, b := r.a.totals, r.b.totals
	row("", "A: "+models(a), "B: "+models(b))
	row("steps", stepsCell(r.a), stepsCell(r.b))
	row("tool calls", strconv.Itoa(a.ToolCalls), strconv.Itoa(b.ToolCalls))
	row("tokens", tokens(a), tokens(b))
	row("cost (est.)", summary.FormatCost(a), summary.FormatCost(b))
	row("duration", duration(a), duration(b))
	if err := tw.Flush(); err != nil {
		return err
	}
	return writeTrimmed(w, buf.String())
}

func models(t summary.Totals) string {
	if len(t.Models) == 0 {
		return "(no model)"
	}
	return printable(strings.Join(t.Models, ", "))
}

func stepsCell(s diffSide) string {
	n := strconv.Itoa(s.totals.Steps)
	if s.stopped() {
		// The strict stop of spec §6.2: the replay ended on a cassette miss.
		n += "  (stopped: cassette miss)"
	}
	return n
}

func tokens(t summary.Totals) string {
	return summary.FormatCount(t.TokensIn) + " in / " + summary.FormatCount(t.TokensOut) + " out"
}

// duration is the session's duration, or says that it never ended, which a
// session still recording, or one whose recorder died, has not (spec §8.1).
func duration(t summary.Totals) string {
	if t.Duration == nil {
		return "never ended"
	}
	return summary.FormatDuration(*t.Duration)
}

// writePoint writes the divergence point: what each side has there, and the
// diff of what differs (spec §8.3 step 3).
func writePoint(w io.Writer, p diff.Point) {
	if p.Kind == diff.KindStop {
		// Every step was equal, so the stop marker is after the last one,
		// not in it: the steps line that follows counts that step too.
		fmt.Fprintf(w, "✗ first divergence after step %d, at the stop marker\n", p.Step)
		fmt.Fprintf(w, "    A  %s\n    B  %s\n", stopText(p.StopA), stopText(p.StopB))
		return
	}
	kind := p.Kind
	if p.Field == diff.FieldText {
		kind += " (text)"
	}
	fmt.Fprintf(w, "✗ first divergence at step %d, event %s\n", p.Step, kind)
	fmt.Fprintf(w, "    A  %s\n    B  %s\n", pointEvent(p, p.A, "B"), pointEvent(p, p.B, "A"))
	for _, e := range p.Diff.Entries {
		fmt.Fprintf(w, "       - %s: %s → %s\n", printable(e.Path), diffValue(e.Expected), diffValue(e.Received))
	}
	if p.Diff.MorePaths {
		fmt.Fprintf(w, "       … more paths differ\n")
	}
}

// pointEvent describes one side's event at the divergence point, or why the
// side has none: other is the name of the side that has one.
func pointEvent(p diff.Point, e *cassette.Event, other string) string {
	if e == nil {
		if p.Kind == "model_response" {
			return fmt.Sprintf("(none: only %s has a step %d)", other, p.Step)
		}
		return fmt.Sprintf("(none: no matching %s in step %d)", p.Kind, p.Step)
	}
	switch e.Kind {
	case "model_response":
		if e.Model == nil {
			return "model (unknown)"
		}
		return "model " + printable(*e.Model)
	case "divergence":
		return missReason(*e)
	case "tool_result":
		return eventTool(*e, e.Source) + "  " + diffValue(compact(e.Result)) + "  " + printable(e.Status)
	case "tool_call":
		return eventTool(*e, e.Target) + "  " + diffValue(compact(e.Arguments))
	}
	return eventTool(*e, "") + "  " + diffValue(compact(e.Arguments))
}

// eventTool names an event's tool, after the MCP server it ran on, if any:
// the same tool on two servers is a divergence too.
func eventTool(e cassette.Event, server string) string {
	tool := "(no tool)"
	if e.Tool != nil {
		tool = *e.Tool
	}
	if server != "" {
		tool = server + " " + tool
	}
	return printable(tool)
}

// missReason describes a divergence event from its result (spec §7.4), e.g.
// "cassette miss on mcp:orders". The cassette is untrusted, so a result that
// cannot be read only loses the upstream.
func missReason(e cassette.Event) string {
	var res struct{ Upstream string }
	if json.Unmarshal(e.Result, &res) != nil || res.Upstream == "" {
		return "cassette miss"
	}
	return "cassette miss on " + printable(res.Upstream)
}

func stopText(s *diff.Stop) string {
	switch {
	case s == nil:
		return "(none: no model response)"
	case s.Failed && s.Error != "":
		return "stop: error: " + printable(s.Error)
	case s.Failed:
		return "stop: error"
	case s.StopReason == "":
		return "stop: (no stop_reason)"
	}
	return "stop: " + printable(s.StopReason)
}

// diffValue formats a value as the divergence report does (spec §6.4,
// §8.4), made safe to print. It escapes after the cut, so the cut never
// splits an escape; the limit counts the characters before escaping.
func diffValue(v []byte) string {
	return printable(reportValue(v))
}

// compact returns stored JSON in compact form, or as stored when it is not
// JSON, which a valid cassette never holds. Absent stays nil.
func compact(raw json.RawMessage) []byte {
	if len(raw) == 0 {
		return nil
	}
	var b bytes.Buffer
	if json.Compact(&b, raw) != nil {
		return raw
	}
	return b.Bytes()
}

func stepSpan(s diff.Span) string {
	if s.First == s.Last {
		return fmt.Sprintf("step %d", s.First)
	}
	return fmt.Sprintf("steps %d–%d", s.First, s.Last)
}
