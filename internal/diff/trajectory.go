// Package diff compares the trajectories of two sessions (spec §8).
package diff

import (
	"cmp"
	"encoding/json"
	"slices"
	"time"

	"github.com/pmlucero/retrace/internal/cassette"
)

// Step is one step of a trajectory (spec §8.2): the n-th model_response, in
// the order its LLM request arrived, and the events that belong to it.
type Step struct {
	N           int             // 0 for events before the first model_response
	Response    *cassette.Event // nil for step 0
	ToolUses    []cassette.Event
	ToolCalls   []cassette.Event // in seq order; the comparison treats them as a set (spec §8.3)
	ToolResults []cassette.Event // in seq order; the comparison treats them as a set (spec §8.3)
	Divergences []cassette.Event
}

// Stop is the terminal marker built from the last model_response (spec §8.2).
type Stop struct {
	// StopReason is the response's result.stop_reason. It is empty when
	// Failed is true: the spec takes the error instead of the stop reason.
	StopReason string
	// Error is the response's error when its status is error.
	Error string
	// Failed reports whether the response's status is error. It keeps a
	// failure with no error message apart from a stop marker with no stop
	// reason, which the cassette, as untrusted input, may hold either of.
	Failed bool
}

// Trajectory is a session's steps and its stop marker (spec §8.2).
type Trajectory struct {
	// Steps are in order. Step 0 is present only when it holds an event, so
	// Steps[i].N is i or i+1.
	Steps []Step
	Stop  *Stop // nil when there is no model_response
}

// Build groups events into a trajectory (spec §8.2). The events may come in
// any order, and events is not modified. Kinds other than model_response,
// tool_use, tool_call, tool_result and divergence are ignored.
//
// Steps follow the model_response events by interaction_id, the order their
// requests arrived in, not by seq, which depends on when responses complete.
// A tool_use goes to the step of its model_response, a tool_result to the
// step of its tool_call, a tool_call to the step of the last model_response
// with a lower interaction_id, and a divergence to the step of the last
// model_response that started at or before it.
//
// The cassette is untrusted input, so Build tolerates what a valid one never
// holds. A model_response without an interaction_id comes after those with
// one, in seq order. A tool_use or tool_result whose parent_seq is missing or
// names an event of another kind is placed by its interaction_id: a tool_use
// with the model_response of the same interaction, a tool_result like a
// tool_call. An event that lacks the interaction_id it would be placed by,
// and a tool_use whose interaction has no model_response, are placed by
// their started_at like a divergence.
func Build(events []cassette.Event) Trajectory {
	sorted := slices.Clone(events)
	// Stable, so events sharing a seq, which a valid cassette never has,
	// still give the same trajectory on every run.
	slices.SortStableFunc(sorted, func(a, b cassette.Event) int { return cmp.Compare(a.Seq, b.Seq) })

	var responses []*cassette.Event
	bySeq := map[int64]*cassette.Event{}
	for i := range sorted {
		e := &sorted[i]
		if _, dup := bySeq[e.Seq]; !dup {
			bySeq[e.Seq] = e
		}
		if e.Kind == "model_response" {
			responses = append(responses, e)
		}
	}
	// Stable over seq order, so responses without an interaction_id, or
	// sharing one, keep their seq order.
	slices.SortStableFunc(responses, func(a, b *cassette.Event) int {
		switch {
		case a.InteractionID == nil && b.InteractionID == nil:
			return 0
		case a.InteractionID == nil:
			return 1
		case b.InteractionID == nil:
			return -1
		}
		return cmp.Compare(*a.InteractionID, *b.InteractionID)
	})

	// steps[0] is step 0, and steps[n] holds the n-th response.
	steps := make([]Step, 1, len(responses)+1)
	for _, r := range responses {
		steps = append(steps, Step{N: len(steps), Response: r})
	}
	p := newPlacer(steps[1:])

	for i := range sorted {
		e := &sorted[i]
		var list *[]cassette.Event
		switch e.Kind {
		case "tool_use":
			list = &steps[p.toolUse(e, bySeq)].ToolUses
		case "tool_call":
			list = &steps[p.byInteraction(e)].ToolCalls
		case "tool_result":
			list = &steps[p.toolResult(e, bySeq)].ToolResults
		case "divergence":
			list = &steps[p.byTime(e.StartedAt)].Divergences
		default:
			continue
		}
		*list = append(*list, *e)
	}

	var t Trajectory
	if s := steps[0]; len(s.ToolUses)+len(s.ToolCalls)+len(s.ToolResults)+len(s.Divergences) > 0 {
		t.Steps = steps
	} else if len(steps) > 1 {
		t.Steps = steps[1:]
	}
	if len(responses) > 0 {
		t.Stop = stopOf(*responses[len(responses)-1])
	}
	return t
}

// placer finds the step of an event (spec §8.2). Each method follows at most
// one parent_seq, so a cycle in untrusted parent_seq values cannot loop.
type placer struct {
	stepOf map[*cassette.Event]int // model_response to its step
	// ids are the interaction ids of the responses that have one, ascending.
	// Those responses come first in step order, so ids[i] is that of step i+1.
	ids []int64
	// starts are the responses' start times, ascending, and maxN[i] is the
	// highest step among those that started at or before starts[i].
	starts []time.Time
	maxN   []int
}

func newPlacer(steps []Step) placer {
	p := placer{stepOf: make(map[*cassette.Event]int, len(steps))}
	for _, s := range steps {
		p.stepOf[s.Response] = s.N
		if s.Response.InteractionID != nil {
			p.ids = append(p.ids, *s.Response.InteractionID)
		}
	}
	// The order of steps is not the order of start times when the cassette
	// is inconsistent, so the time index keeps the highest step per prefix.
	byStart := slices.Clone(steps)
	slices.SortStableFunc(byStart, func(a, b Step) int { return a.Response.StartedAt.Compare(b.Response.StartedAt) })
	for _, s := range byStart {
		n := s.N
		if len(p.maxN) > 0 {
			n = max(n, p.maxN[len(p.maxN)-1])
		}
		p.starts = append(p.starts, s.Response.StartedAt)
		p.maxN = append(p.maxN, n)
	}
	return p
}

// parent returns the event that e's parent_seq names when it has kind, or nil.
func parent(e *cassette.Event, kind string, bySeq map[int64]*cassette.Event) *cassette.Event {
	if e.ParentSeq == nil {
		return nil
	}
	if pe := bySeq[*e.ParentSeq]; pe != nil && pe.Kind == kind {
		return pe
	}
	return nil
}

// toolUse places a tool_use with its model_response.
func (p placer) toolUse(e *cassette.Event, bySeq map[int64]*cassette.Event) int {
	if r := parent(e, "model_response", bySeq); r != nil {
		return p.stepOf[r]
	}
	if e.InteractionID != nil {
		// A tool_use comes from the same interaction as its model_response.
		if i, found := slices.BinarySearch(p.ids, *e.InteractionID); found {
			return i + 1
		}
	}
	return p.byTime(e.StartedAt)
}

// toolResult places a tool_result with its tool_call.
func (p placer) toolResult(e *cassette.Event, bySeq map[int64]*cassette.Event) int {
	if c := parent(e, "tool_call", bySeq); c != nil {
		return p.byInteraction(c)
	}
	return p.byInteraction(e)
}

// byInteraction returns the step of the last model_response whose
// interaction_id is lower than e's, or 0 when there is none.
func (p placer) byInteraction(e *cassette.Event) int {
	if e.InteractionID == nil {
		return p.byTime(e.StartedAt)
	}
	i, _ := slices.BinarySearch(p.ids, *e.InteractionID)
	return i // the number of lower ids, which is the step of the last of them
}

// byTime returns the step of the last model_response that started at or
// before t, the later step on a tie, or 0 when there is none.
func (p placer) byTime(t time.Time) int {
	// i is the number of responses that started at or before t.
	i, _ := slices.BinarySearchFunc(p.starts, t, func(s, t time.Time) int {
		if s.After(t) {
			return 1
		}
		return -1
	})
	if i == 0 {
		return 0
	}
	return p.maxN[i-1]
}

// stopOf builds the stop marker from the last model_response.
func stopOf(e cassette.Event) *Stop {
	if e.Status == "error" {
		s := &Stop{Failed: true}
		if e.Error != nil {
			s.Error = *e.Error
		}
		return s
	}
	return &Stop{StopReason: parseOutput(e.Result).StopReason}
}

// Text returns the result.text of a model_response event, or "" when it has
// none, for --text (spec §8.3 step 2).
func Text(e cassette.Event) string {
	return parseOutput(e.Result).Text
}

// modelOutput is the result of a model_response (spec §7.4).
type modelOutput struct {
	StopReason string
	Text       string
}

// parseOutput decodes a model_response result. The cassette is untrusted
// input, so a result that is absent, not an object, or has fields of the
// wrong type gives the zero value for what cannot be read, not an error: a
// diff still has to report the rest of the session.
func parseOutput(raw json.RawMessage) modelOutput {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return modelOutput{}
	}
	var out modelOutput
	// Each field is decoded on its own, so a bad one does not hide the other.
	if json.Unmarshal(fields["stop_reason"], &out.StopReason) != nil {
		out.StopReason = ""
	}
	if json.Unmarshal(fields["text"], &out.Text) != nil {
		out.Text = ""
	}
	return out
}
