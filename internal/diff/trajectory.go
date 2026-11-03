// Package diff compares the trajectories of two sessions (spec §8).
package diff

import (
	"cmp"
	"encoding/json"
	"slices"
	"time"

	"github.com/pmlucero/retrace/internal/cassette"
)

// Step is one step of a trajectory (spec §8.2): the n-th model_response and
// the events that belong to it. Events are placed by causality and time, not
// by seq alone, because an agent that acts on a streamed response at once can
// have its tool_call appended before the response's model_response.
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
// A tool_use goes to the step of its model_response, a tool_result to the
// step of its tool_call, and a tool_call or divergence to the step of the
// last model_response that started at or before it. The cassette is
// untrusted input, so a tool_use or tool_result whose parent_seq is missing
// or does not name an event of the right kind is placed by its started_at.
func Build(events []cassette.Event) Trajectory {
	sorted := slices.Clone(events)
	// Stable, so events sharing a seq, which a valid cassette never has,
	// still give the same trajectory on every run.
	slices.SortStableFunc(sorted, func(a, b cassette.Event) int { return cmp.Compare(a.Seq, b.Seq) })

	// steps[0] is step 0, and steps[n] holds the n-th model_response by seq.
	steps := []Step{{N: 0}}
	bySeq := map[int64]*cassette.Event{}
	for i := range sorted {
		e := &sorted[i]
		if _, dup := bySeq[e.Seq]; !dup {
			bySeq[e.Seq] = e
		}
		if e.Kind == "model_response" {
			steps = append(steps, Step{N: len(steps), Response: e})
		}
	}
	stepOf := make(map[*cassette.Event]int, len(steps)-1)
	for _, s := range steps[1:] {
		stepOf[s.Response] = s.N
	}
	byTime := newTimeIndex(steps[1:])

	var last *cassette.Event
	for i := range sorted {
		e := &sorted[i]
		if e.Kind == "model_response" {
			last = e
			continue
		}
		if !stepKinds[e.Kind] {
			continue
		}
		cur := &steps[place(e, bySeq, stepOf, byTime)]
		switch e.Kind {
		case "tool_use":
			cur.ToolUses = append(cur.ToolUses, *e)
		case "tool_call":
			cur.ToolCalls = append(cur.ToolCalls, *e)
		case "tool_result":
			cur.ToolResults = append(cur.ToolResults, *e)
		case "divergence":
			cur.Divergences = append(cur.Divergences, *e)
		}
	}

	var t Trajectory
	if s := steps[0]; len(s.ToolUses)+len(s.ToolCalls)+len(s.ToolResults)+len(s.Divergences) > 0 {
		t.Steps = steps
	} else if len(steps) > 1 {
		t.Steps = steps[1:]
	}
	if last != nil {
		t.Stop = stopOf(*last)
	}
	return t
}

// stepKinds are the kinds a step holds besides its model_response.
var stepKinds = map[string]bool{"tool_use": true, "tool_call": true, "tool_result": true, "divergence": true}

// place returns the step of e (spec §8.2). It follows at most one
// parent_seq, so a cycle in untrusted parent_seq values cannot loop.
func place(e *cassette.Event, bySeq map[int64]*cassette.Event, stepOf map[*cassette.Event]int, byTime timeIndex) int {
	var parent *cassette.Event
	if e.ParentSeq != nil {
		parent = bySeq[*e.ParentSeq]
	}
	switch {
	case e.Kind == "tool_use" && parent != nil && parent.Kind == "model_response":
		return stepOf[parent]
	case e.Kind == "tool_result" && parent != nil && parent.Kind == "tool_call":
		// The tool_call is itself placed by its started_at.
		return byTime.step(parent.StartedAt)
	}
	return byTime.step(e.StartedAt)
}

// timeIndex finds the step of the last model_response that started at or
// before a given time. "Last" is by seq, which numbers the steps: concurrent
// requests can make started_at and seq disagree, so the step is the highest
// one among the responses that started in time, not the one that started
// latest.
type timeIndex struct {
	starts []time.Time // response start times, ascending
	maxN   []int       // maxN[i] is the highest step among starts[:i+1]
}

func newTimeIndex(responses []Step) timeIndex {
	rs := slices.Clone(responses)
	slices.SortStableFunc(rs, func(a, b Step) int { return a.Response.StartedAt.Compare(b.Response.StartedAt) })
	var x timeIndex
	for _, s := range rs {
		n := s.N
		if len(x.maxN) > 0 {
			n = max(n, x.maxN[len(x.maxN)-1])
		}
		x.starts = append(x.starts, s.Response.StartedAt)
		x.maxN = append(x.maxN, n)
	}
	return x
}

// step returns the step of an event that started at t, or 0 when no
// model_response started at or before it.
func (x timeIndex) step(t time.Time) int {
	// i is the number of responses that started at or before t.
	i, _ := slices.BinarySearchFunc(x.starts, t, func(s, t time.Time) int {
		if s.After(t) {
			return 1
		}
		return -1
	})
	if i == 0 {
		return 0
	}
	return x.maxN[i-1]
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
