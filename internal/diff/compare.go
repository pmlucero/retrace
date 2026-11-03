package diff

import (
	"bytes"
	"cmp"
	"encoding/json"
	"slices"

	"github.com/pmlucero/retrace/internal/cassette"
	"github.com/pmlucero/retrace/internal/divergence"
	"github.com/pmlucero/retrace/internal/jsonvalue"
)

// Options selects what Compare includes in the comparison (spec §8.3).
type Options struct {
	// Text compares the result.text of each step's model_response, before
	// the step's other events (--text).
	Text bool
}

// Kinds of a Point besides the event kinds of a trajectory.
const (
	// KindStop is a Point at the stop markers, after every step was equal.
	KindStop = "stop"
)

// Fields that a Point's Diff can be about.
const (
	FieldArguments = "arguments"
	FieldResult    = "result"
	FieldText      = "text"
)

// Point is the divergence point: the first pair of events that differ, or
// the place where one trajectory ends early (spec §8.3 step 3).
type Point struct {
	Step int // the step number, Step.N
	// Kind is the event kind compared: tool_use, tool_call, tool_result or
	// divergence. It is model_response when the steps' texts differ under
	// --text, or when only one side has the step at all, and KindStop when
	// only the stop markers differ.
	Kind string
	// A and B are the events compared. One of them is nil where that side
	// has no event left in the step. Both are nil for KindStop.
	A, B *cassette.Event
	// StopA and StopB are the stop markers, set only for KindStop. Either
	// may be nil: a session without a model_response has none.
	StopA, StopB *Stop
	// Field names what Diff is about (FieldArguments, FieldResult or
	// FieldText). It is empty, and Diff has no entries, when the events
	// differ elsewhere (kind, source, target, tool or status), when a side
	// is missing, and for KindStop.
	Field string
	// Diff is the structural diff of Field, with A as expected and B as
	// received, formatted like the divergence report (spec §6.4).
	Diff divergence.Diff
}

// Span is a run of consecutive steps, First to Last. It is empty when Last
// is lower than First.
type Span struct {
	First, Last int
	ToolCalls   int // tool_call events in the span, counted on side A
}

// Result is the outcome of Compare.
type Result struct {
	Point *Point // nil when the trajectories are identical
	// Identical is the run of steps before the divergence point, which are
	// equal on both sides, or every step when there is no divergence point.
	Identical Span
	// TextDifferences lists, in order, the steps whose texts differ when
	// text is not compared (spec §8.3 step 2). It is never nil, and empty
	// under Options.Text.
	TextDifferences []int
}

// Compare finds the first divergence between trajectories a and b (spec
// §8.3). Steps are compared by number. Within a step the order is: text
// (only under Options.Text), tool_use events in seq order, tool_call events
// and then tool_result events as sets, and divergence events in seq order.
// The stop markers are compared last. Events are equal on their action
// identity: kind, source, target, tool and canonical arguments, plus the
// canonical result and the status for a tool_result.
func Compare(a, b Trajectory, opts Options) Result {
	res := Result{TextDifferences: []int{}}
	last := max(lastN(a), lastN(b))
	first := 1
	if hasStep0(a) || hasStep0(b) {
		first = 0
	}
	res.Identical = Span{First: first, Last: last}

	for n := first; n <= last; n++ {
		sa, sb := stepAt(a, n), stepAt(b, n)
		if !opts.Text && sa.Response != nil && sb.Response != nil && Text(*sa.Response) != Text(*sb.Response) {
			// Listed past the divergence point too: steps are compared by
			// number, so the list stays informative there.
			res.TextDifferences = append(res.TextDifferences, n)
		}
		if res.Point != nil {
			continue
		}
		if p := compareSteps(n, sa, sb, opts); p != nil {
			res.Point = p
			res.Identical.Last = n - 1
			continue
		}
		res.Identical.ToolCalls += len(sa.ToolCalls)
	}
	if res.Point == nil && !equalStops(a.Stop, b.Stop) {
		res.Point = &Point{Step: last, Kind: KindStop, StopA: a.Stop, StopB: b.Stop}
	}
	return res
}

// lastN is the number of the last step of t, or 0 when it has none.
func lastN(t Trajectory) int {
	if len(t.Steps) == 0 {
		return 0
	}
	return t.Steps[len(t.Steps)-1].N
}

func hasStep0(t Trajectory) bool {
	return len(t.Steps) > 0 && t.Steps[0].N == 0
}

// stepAt returns step n of t, or an empty step when t has none. Steps[i].N
// is i or i+1 (see Trajectory), so no search is needed.
func stepAt(t Trajectory, n int) Step {
	for _, i := range []int{n, n - 1} {
		if i >= 0 && i < len(t.Steps) && t.Steps[i].N == n {
			return t.Steps[i]
		}
	}
	return Step{N: n}
}

// compareSteps returns the first difference between two steps with the same
// number, or nil.
func compareSteps(n int, a, b Step, opts Options) *Point {
	if (a.Response == nil) != (b.Response == nil) {
		// One trajectory has fewer model_response events: it ends here.
		return &Point{Step: n, Kind: "model_response", A: a.Response, B: b.Response}
	}
	if opts.Text && a.Response != nil {
		ta, tb := Text(*a.Response), Text(*b.Response)
		if ta != tb {
			return &Point{
				Step: n, Kind: "model_response", A: a.Response, B: b.Response,
				Field: FieldText, Diff: divergence.Compare(jsonvalue.Encode(ta), jsonvalue.Encode(tb)),
			}
		}
	}
	lists := []struct {
		kind   string
		a, b   []cassette.Event
		sorted bool
	}{
		{"tool_use", a.ToolUses, b.ToolUses, false},
		{"tool_call", a.ToolCalls, b.ToolCalls, true},
		{"tool_result", a.ToolResults, b.ToolResults, true},
		{"divergence", a.Divergences, b.Divergences, false},
	}
	for _, l := range lists {
		if p := compareLists(n, l.kind, l.a, l.b, l.sorted); p != nil {
			return p
		}
	}
	return nil
}

// compareLists compares two lists of events of one kind pairwise. A set
// (sorted) is first ordered by its full identity, so that the order the
// events were appended in does not matter (spec §8.3 step 1).
func compareLists(n int, kind string, as, bs []cassette.Event, sorted bool) *Point {
	ia, ib := identities(as), identities(bs)
	if sorted {
		slices.SortStableFunc(ia, compareIdentities)
		slices.SortStableFunc(ib, compareIdentities)
	}
	for i := range max(len(ia), len(ib)) {
		switch {
		case i >= len(ib):
			return &Point{Step: n, Kind: kind, A: ia[i].event}
		case i >= len(ia):
			return &Point{Step: n, Kind: kind, B: ib[i].event}
		}
		if p := compareEvents(n, kind, ia[i], ib[i]); p != nil {
			return p
		}
	}
	return nil
}

// identity is the action identity of an event (spec §8.3 step 1).
type identity struct {
	event          *cassette.Event
	source, target string
	tool           *string
	args, result   []byte // canonical; nil when absent
}

func identities(es []cassette.Event) []identity {
	out := make([]identity, len(es))
	for i := range es {
		e := &es[i]
		out[i] = identity{
			event: e, source: e.Source, target: e.Target, tool: e.Tool,
			args: canonical(e.Arguments), result: canonical(e.Result),
		}
	}
	return out
}

// compareIdentities orders a set by (target or source, tool, canonical
// arguments or result, status) (spec §8.3 step 1). Both are compared, since
// a tool_call carries no result and a tool_result no arguments, and the
// remaining fields break the ties left, so that no tie depends on seq.
func compareIdentities(x, y identity) int {
	return cmp.Or(
		cmp.Compare(x.target, y.target),
		cmp.Compare(x.source, y.source),
		compareTools(x.tool, y.tool),
		bytes.Compare(x.args, y.args),
		bytes.Compare(x.result, y.result),
		cmp.Compare(x.event.Status, y.event.Status),
	)
}

// compareTools orders a missing tool before any name, so that an absent
// tool and an empty one stay apart.
func compareTools(x, y *string) int {
	switch {
	case x == nil && y == nil:
		return 0
	case x == nil:
		return -1
	case y == nil:
		return 1
	}
	return cmp.Compare(*x, *y)
}

// compareEvents returns a Point when x and y, both of kind, differ on their
// action identity, or nil.
func compareEvents(n int, kind string, x, y identity) *Point {
	same := x.source == y.source && x.target == y.target &&
		compareTools(x.tool, y.tool) == 0 && bytes.Equal(x.args, y.args)
	if kind == "tool_result" {
		same = same && bytes.Equal(x.result, y.result) && x.event.Status == y.event.Status
	}
	if same {
		return nil
	}
	p := &Point{Step: n, Kind: kind, A: x.event, B: y.event}
	switch {
	case !bytes.Equal(x.args, y.args):
		p.Field, p.Diff = FieldArguments, divergence.Compare(x.args, y.args)
	case kind == "tool_result" && !bytes.Equal(x.result, y.result):
		p.Field, p.Diff = FieldResult, divergence.Compare(x.result, y.result)
	}
	return p
}

// canonical returns the canonical form of a stored JSON value (ADR 0007):
// keys sorted, number text kept. The cassette is untrusted input, so a value
// that cannot be decoded is compared as its bytes.
func canonical(raw json.RawMessage) []byte {
	if len(raw) == 0 {
		return nil
	}
	v, err := jsonvalue.Decode(raw)
	if err != nil {
		return slices.Clone(raw)
	}
	return jsonvalue.EncodeCanonical(v)
}

// equalStops reports whether two stop markers are equal: both absent, or
// both failed with the same error, or both with the same stop_reason.
func equalStops(x, y *Stop) bool {
	if x == nil || y == nil {
		return x == y
	}
	return *x == *y
}
