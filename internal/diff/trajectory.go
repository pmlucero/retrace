// Package diff compares the trajectories of two sessions (spec §8).
package diff

import (
	"cmp"
	"encoding/json"
	"slices"

	"github.com/pmlucero/retrace/internal/cassette"
)

// Step is one step of a trajectory (spec §8.2): the n-th model_response and
// the events up to the next one, assigned by position (seq), not parent_seq.
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

// stepKinds are the kinds a step holds besides its model_response.
var stepKinds = map[string]bool{"tool_use": true, "tool_call": true, "tool_result": true, "divergence": true}

// Build groups events into a trajectory (spec §8.2). The events may come in
// any order: they are sorted by seq, and events is not modified. Kinds other
// than model_response, tool_use, tool_call, tool_result and divergence are
// ignored.
func Build(events []cassette.Event) Trajectory {
	sorted := slices.Clone(events)
	// Stable, so events sharing a seq, which a valid cassette never has,
	// still give the same trajectory on every run.
	slices.SortStableFunc(sorted, func(a, b cassette.Event) int { return cmp.Compare(a.Seq, b.Seq) })

	var t Trajectory
	var last *cassette.Event
	for i := range sorted {
		e := &sorted[i]
		if e.Kind == "model_response" {
			n := 1
			if len(t.Steps) > 0 {
				n = t.Steps[len(t.Steps)-1].N + 1
			}
			t.Steps = append(t.Steps, Step{N: n, Response: e})
			last = e
			continue
		}
		if !stepKinds[e.Kind] {
			continue
		}
		if len(t.Steps) == 0 {
			// Step 0 exists only from its first event on.
			t.Steps = append(t.Steps, Step{N: 0})
		}
		cur := &t.Steps[len(t.Steps)-1]
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
	if last != nil {
		t.Stop = stopOf(*last)
	}
	return t
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
