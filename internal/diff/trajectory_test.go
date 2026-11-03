package diff

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/pmlucero/retrace/internal/cassette"
)

// ev builds an event of kind at seq. parent is the parent_seq, or 0 for none.
func ev(seq int64, kind string, parent int64) cassette.Event {
	e := cassette.Event{Seq: seq, Kind: kind, Status: "ok"}
	if parent != 0 {
		e.ParentSeq = &parent
	}
	return e
}

// resp builds a model_response at seq with the given result.
func resp(seq int64, result string) cassette.Event {
	e := ev(seq, "model_response", 0)
	if result != "" {
		e.Result = json.RawMessage(result)
	}
	return e
}

// failed builds a model_response at seq whose status is error.
func failed(seq int64, msg *string) cassette.Event {
	e := ev(seq, "model_response", 0)
	e.Status, e.Error = "error", msg
	return e
}

func str(s string) *string { return &s }

// shape is a step reduced to the seqs it holds, so that tests state the
// grouping without repeating whole events.
type shape struct {
	N                                         int
	Response                                  int64 // 0 for no response
	ToolUses, ToolCalls, ToolResults, Diverge []int64
}

func shapes(t Trajectory) []shape {
	seqs := func(es []cassette.Event) []int64 {
		var out []int64
		for _, e := range es {
			out = append(out, e.Seq)
		}
		return out
	}
	var out []shape
	for _, s := range t.Steps {
		sh := shape{N: s.N, ToolUses: seqs(s.ToolUses), ToolCalls: seqs(s.ToolCalls),
			ToolResults: seqs(s.ToolResults), Diverge: seqs(s.Divergences)}
		if s.Response != nil {
			sh.Response = s.Response.Seq
		}
		out = append(out, sh)
	}
	return out
}

// Spec §8.2: grouping events into steps.
func TestBuildSteps(t *testing.T) {
	tests := []struct {
		name   string
		events []cassette.Event
		want   []shape
	}{
		{
			name: "steps by position",
			events: []cassette.Event{
				ev(1, "user_message", 0), resp(2, `{"stop_reason":"tool_use"}`),
				ev(3, "tool_use", 2), ev(4, "tool_call", 3), ev(5, "tool_result", 4),
				resp(6, `{"stop_reason":"end_turn"}`),
			},
			want: []shape{
				{N: 1, Response: 2, ToolUses: []int64{3}, ToolCalls: []int64{4}, ToolResults: []int64{5}},
				{N: 2, Response: 6},
			},
		},
		{
			// §7.4: linking is best-effort, so an unlinked tool_call still
			// belongs to the step it falls in.
			name: "positional assignment without parent_seq",
			events: []cassette.Event{
				resp(1, ""), ev(2, "tool_use", 1), ev(3, "tool_use", 1),
				ev(4, "tool_call", 0), ev(5, "tool_result", 0), ev(6, "tool_call", 0), ev(7, "tool_result", 0),
				resp(8, ""), ev(9, "tool_call", 0),
			},
			want: []shape{
				{N: 1, Response: 1, ToolUses: []int64{2, 3}, ToolCalls: []int64{4, 6}, ToolResults: []int64{5, 7}},
				{N: 2, Response: 8, ToolCalls: []int64{9}},
			},
		},
		{
			// parent_seq pointing at an earlier step does not move the event.
			name: "parent_seq to another step is not followed",
			events: []cassette.Event{
				resp(1, ""), ev(2, "tool_use", 1), resp(3, ""), ev(4, "tool_call", 2),
			},
			want: []shape{
				{N: 1, Response: 1, ToolUses: []int64{2}},
				{N: 2, Response: 3, ToolCalls: []int64{4}},
			},
		},
		{
			name:   "step 0 holds events before the first response",
			events: []cassette.Event{ev(1, "tool_call", 0), ev(2, "tool_result", 1), resp(3, "")},
			want: []shape{
				{N: 0, ToolCalls: []int64{1}, ToolResults: []int64{2}},
				{N: 1, Response: 3},
			},
		},
		{
			name: "no step 0 when only ignored kinds precede the first response",
			events: []cassette.Event{
				ev(1, "mcp_message", 0), ev(2, "user_message", 0), resp(3, ""),
			},
			want: []shape{{N: 1, Response: 3}},
		},
		{
			name: "events out of seq order",
			events: []cassette.Event{
				ev(5, "tool_result", 4), resp(1, ""), ev(4, "tool_call", 3), resp(6, ""), ev(3, "tool_use", 1),
				ev(2, "tool_use", 1),
			},
			want: []shape{
				{N: 1, Response: 1, ToolUses: []int64{2, 3}, ToolCalls: []int64{4}, ToolResults: []int64{5}},
				{N: 2, Response: 6},
			},
		},
		{
			name: "ignored kinds",
			events: []cassette.Event{
				resp(1, ""), ev(2, "user_message", 0), ev(3, "mcp_message", 0), ev(4, "unknown", 0), ev(5, "", 0),
			},
			want: []shape{{N: 1, Response: 1}},
		},
		{
			// A strict stop: the divergence ends the session after the
			// last response.
			name: "divergence in the last step",
			events: []cassette.Event{
				resp(1, ""), ev(2, "tool_use", 1), ev(3, "divergence", 0),
			},
			want: []shape{{N: 1, Response: 1, ToolUses: []int64{2}, Diverge: []int64{3}}},
		},
		{
			name:   "divergence before any response",
			events: []cassette.Event{ev(1, "user_message", 0), ev(2, "divergence", 0)},
			want:   []shape{{N: 0, Diverge: []int64{2}}},
		},
		{
			name:   "no model_response",
			events: []cassette.Event{ev(1, "tool_call", 0), ev(2, "tool_result", 1), ev(3, "mcp_message", 0)},
			want:   []shape{{N: 0, ToolCalls: []int64{1}, ToolResults: []int64{2}}},
		},
		{name: "no events"},
		{name: "only ignored kinds", events: []cassette.Event{ev(1, "user_message", 0)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shapes(Build(tt.events)); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("steps = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// Spec §8.2: the stop marker comes from the last model_response.
func TestBuildStop(t *testing.T) {
	tests := []struct {
		name   string
		events []cassette.Event
		want   *Stop
	}{
		{
			name:   "stop_reason of the last response",
			events: []cassette.Event{resp(1, `{"stop_reason":"tool_use"}`), resp(3, `{"stop_reason":"end_turn","text":"done"}`)},
			want:   &Stop{StopReason: "end_turn"},
		},
		{
			name:   "last by seq, not by position in the input",
			events: []cassette.Event{resp(3, `{"stop_reason":"end_turn"}`), resp(1, `{"stop_reason":"tool_use"}`)},
			want:   &Stop{StopReason: "end_turn"},
		},
		{
			name:   "error of a failed last response",
			events: []cassette.Event{resp(1, `{"stop_reason":"tool_use"}`), failed(2, str("overloaded_error: Overloaded"))},
			want:   &Stop{Error: "overloaded_error: Overloaded", Failed: true},
		},
		{
			// The spec takes the error, not the stop reason, of a failed response.
			name: "failed response with a stop reason",
			events: func() []cassette.Event {
				e := failed(1, str("the stream ended before message_stop"))
				e.Result = json.RawMessage(`{"stop_reason":"end_turn"}`)
				return []cassette.Event{e}
			}(),
			want: &Stop{Error: "the stream ended before message_stop", Failed: true},
		},
		{
			name:   "failed response without an error message",
			events: []cassette.Event{failed(1, nil)},
			want:   &Stop{Failed: true},
		},
		{
			name:   "response without a result",
			events: []cassette.Event{resp(1, "")},
			want:   &Stop{},
		},
		{
			name:   "a later divergence does not change the stop",
			events: []cassette.Event{resp(1, `{"stop_reason":"tool_use"}`), ev(2, "divergence", 0)},
			want:   &Stop{StopReason: "tool_use"},
		},
		{
			name:   "no model_response",
			events: []cassette.Event{ev(1, "tool_call", 0), ev(2, "divergence", 0)},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Build(tt.events).Stop; !reflect.DeepEqual(got, tt.want) {
				t.Errorf("stop = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// Build must not reorder the caller's slice while it sorts.
func TestBuildKeepsInput(t *testing.T) {
	events := []cassette.Event{ev(3, "tool_use", 1), resp(1, "")}
	Build(events)
	if events[0].Seq != 3 || events[1].Seq != 1 {
		t.Errorf("input reordered: seqs %d, %d", events[0].Seq, events[1].Seq)
	}
}

// Spec §8.3 step 2, and §7.4 for the shape of the result. The cassette is
// untrusted input, so malformed results give "" instead of failing.
func TestText(t *testing.T) {
	tests := []struct {
		name, result, wantText, wantStop string
	}{
		{name: "text and stop reason", result: `{"stop_reason":"end_turn","text":"Hello"}`, wantText: "Hello", wantStop: "end_turn"},
		{name: "absent result"},
		{name: "null result", result: `null`},
		{name: "not an object", result: `["text"]`},
		{name: "string result", result: `"Hello"`},
		{name: "invalid JSON", result: `{"text":`},
		{name: "text of the wrong type keeps the stop reason", result: `{"stop_reason":"end_turn","text":42}`, wantStop: "end_turn"},
		{name: "stop reason of the wrong type keeps the text", result: `{"stop_reason":{},"text":"Hi"}`, wantText: "Hi"},
		{name: "null fields", result: `{"stop_reason":null,"text":null}`},
		{name: "unknown fields are ignored", result: `{"text":"Hi","extra":[1]}`, wantText: "Hi"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := resp(1, tt.result)
			if got := Text(e); got != tt.wantText {
				t.Errorf("Text = %q, want %q", got, tt.wantText)
			}
			if got := Build([]cassette.Event{e}).Stop.StopReason; got != tt.wantStop {
				t.Errorf("StopReason = %q, want %q", got, tt.wantStop)
			}
		})
	}
}
