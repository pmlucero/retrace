package diff

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/pmlucero/retrace/internal/cassette"
)

var t0 = time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)

// ev builds an event of kind at seq. parent is the parent_seq, or 0 for none.
// Its interaction_id is seq and it starts seq seconds after t0, so that both
// follow seq unless a test says otherwise with iid, noIID or at. A divergence
// has no interaction, as in a real cassette.
func ev(seq int64, kind string, parent int64) cassette.Event {
	e := cassette.Event{Seq: seq, Kind: kind, Status: "ok", StartedAt: t0.Add(time.Duration(seq) * time.Second)}
	if kind != "divergence" {
		e.InteractionID = &seq
	}
	if parent != 0 {
		e.ParentSeq = &parent
	}
	return e
}

// at returns e starting secs seconds after t0.
func at(e cassette.Event, secs int) cassette.Event {
	e.StartedAt = t0.Add(time.Duration(secs) * time.Second)
	return e
}

// iid returns e with interaction_id id.
func iid(e cassette.Event, id int64) cassette.Event {
	e.InteractionID = &id
	return e
}

// noIID returns e without an interaction_id.
func noIID(e cassette.Event) cassette.Event {
	e.InteractionID = nil
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
			name: "a recorded session",
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
			// AC-19. The race of §8.2 and §7.4 (Order): the agent acts on the
			// streamed response of request 2 before its model_response is
			// appended, so the tool_call (request 3) has the lower seq. An
			// identical replay may append them the other way round; by
			// interaction_id both land in step 2.
			name: "tool_call appended before its model_response",
			events: []cassette.Event{
				iid(resp(1, ""), 1), iid(ev(2, "tool_call", 0), 3), iid(resp(3, ""), 2),
				iid(ev(4, "tool_use", 3), 2), iid(ev(5, "tool_result", 2), 3),
			},
			want: []shape{
				{N: 1, Response: 1},
				{N: 2, Response: 3, ToolUses: []int64{4}, ToolCalls: []int64{2}, ToolResults: []int64{5}},
			},
		},
		{
			// AC-19. The same session as above, with the tool_call appended
			// after the model_response: the same steps.
			name: "tool_call appended after its model_response",
			events: []cassette.Event{
				iid(resp(1, ""), 1), iid(resp(2, ""), 2), iid(ev(3, "tool_use", 2), 2),
				iid(ev(4, "tool_call", 0), 3), iid(ev(5, "tool_result", 4), 3),
			},
			want: []shape{
				{N: 1, Response: 1},
				{N: 2, Response: 2, ToolUses: []int64{3}, ToolCalls: []int64{4}, ToolResults: []int64{5}},
			},
		},
		{
			// AC-19. Requests 1 and 2 run concurrently and complete in reverse
			// order, so their model_response seqs are swapped. Steps follow
			// the requests, not the completions.
			name: "concurrent requests that complete in reverse order",
			events: []cassette.Event{
				iid(resp(1, ""), 2), iid(ev(2, "tool_use", 1), 2),
				iid(resp(3, ""), 1), iid(ev(4, "tool_use", 3), 1),
				iid(ev(5, "tool_call", 0), 3),
			},
			want: []shape{
				{N: 1, Response: 3, ToolUses: []int64{4}},
				{N: 2, Response: 1, ToolUses: []int64{2}, ToolCalls: []int64{5}},
			},
		},
		{
			// §7.4: linking is best-effort. Without parent_seq, a tool_call
			// and a tool_result are placed by interaction_id, and a tool_use
			// goes with the model_response of its interaction.
			name: "placement without parent_seq",
			events: []cassette.Event{
				resp(1, ""), iid(ev(2, "tool_use", 0), 1), iid(ev(3, "tool_use", 0), 1),
				ev(4, "tool_call", 0), iid(ev(5, "tool_result", 0), 4),
				resp(6, ""), ev(7, "tool_call", 0),
			},
			want: []shape{
				{N: 1, Response: 1, ToolUses: []int64{2, 3}, ToolCalls: []int64{4}, ToolResults: []int64{5}},
				{N: 2, Response: 6, ToolCalls: []int64{7}},
			},
		},
		{
			// No clock is involved for a tool_call: a started_at before the
			// response's does not move it.
			name: "tool_call placed by interaction_id, not time",
			events: []cassette.Event{
				at(resp(1, ""), 5), at(ev(2, "tool_call", 0), 1),
			},
			want: []shape{{N: 1, Response: 1, ToolCalls: []int64{2}}},
		},
		{
			// A tool_use follows its parent, whatever its interaction_id.
			name: "tool_use goes to the step of its model_response",
			events: []cassette.Event{
				resp(1, ""), resp(2, ""), iid(ev(3, "tool_use", 1), 2),
			},
			want: []shape{{N: 1, Response: 1, ToolUses: []int64{3}}, {N: 2, Response: 2}},
		},
		{
			// The tool_result follows its tool_call, also when its own
			// interaction_id would place it in a later step.
			name: "tool_result goes to the step of its tool_call",
			events: []cassette.Event{
				resp(1, ""), ev(2, "tool_call", 0), resp(3, ""), iid(ev(4, "tool_result", 2), 9),
			},
			want: []shape{
				{N: 1, Response: 1, ToolCalls: []int64{2}, ToolResults: []int64{4}},
				{N: 2, Response: 3},
			},
		},
		{
			// The cassette is untrusted: a parent_seq that names no event,
			// or an event of another kind, falls back to interaction_id.
			name: "dangling or mistyped parent_seq",
			events: []cassette.Event{
				resp(1, ""), resp(2, ""),
				iid(ev(3, "tool_use", 99), 1), iid(ev(4, "tool_use", 3), 2),
				iid(ev(5, "tool_result", 99), 2), iid(ev(6, "tool_result", 1), 3),
			},
			want: []shape{
				{N: 1, Response: 1, ToolUses: []int64{3}, ToolResults: []int64{5}},
				{N: 2, Response: 2, ToolUses: []int64{4}, ToolResults: []int64{6}},
			},
		},
		{
			// Without the interaction_id it would be placed by, an event is
			// placed by its started_at, and a model_response without one
			// comes after those with one.
			name: "missing interaction_id",
			events: []cassette.Event{
				noIID(at(resp(1, ""), 1)), at(resp(2, ""), 2),
				noIID(at(ev(3, "tool_call", 0), 3)), noIID(at(ev(4, "tool_result", 0), 0)),
				noIID(at(ev(5, "tool_use", 0), 3)), iid(at(ev(6, "tool_use", 0), 0), 42),
			},
			want: []shape{
				{N: 0, ToolResults: []int64{4}, ToolUses: []int64{6}},
				{N: 1, Response: 2},
				{N: 2, Response: 1, ToolUses: []int64{5}, ToolCalls: []int64{3}},
			},
		},
		{
			// A cycle of parent_seq values must not loop.
			name: "parent_seq cycle",
			events: []cassette.Event{
				resp(1, ""), ev(2, "tool_call", 3), ev(3, "tool_result", 2), ev(4, "tool_use", 4),
			},
			want: []shape{{N: 1, Response: 1, ToolUses: []int64{4}, ToolCalls: []int64{2}, ToolResults: []int64{3}}},
		},
		{
			name: "divergence placed by time",
			events: []cassette.Event{
				at(resp(1, ""), 1), at(ev(2, "divergence", 0), 3), at(resp(3, ""), 2),
			},
			want: []shape{{N: 1, Response: 1}, {N: 2, Response: 3, Diverge: []int64{2}}},
		},
		{
			// §8.2: a tie gives the later step, both with the step's own
			// response and between two responses that started together.
			name: "divergence tie gives the later step",
			events: []cassette.Event{
				at(resp(1, ""), 1), at(resp(2, ""), 5), at(resp(3, ""), 5), at(ev(4, "divergence", 0), 5),
			},
			want: []shape{{N: 1, Response: 1}, {N: 2, Response: 2}, {N: 3, Response: 3, Diverge: []int64{4}}},
		},
		{
			// An inconsistent cassette: step 2 started before step 1. A
			// divergence goes to the highest step among the responses that
			// started at or before it.
			name: "response start times that disagree with the step order",
			events: []cassette.Event{
				at(resp(1, ""), 5), at(resp(2, ""), 3),
				at(ev(3, "divergence", 0), 2), at(ev(4, "divergence", 0), 4), at(ev(5, "divergence", 0), 6),
			},
			want: []shape{
				{N: 0, Diverge: []int64{3}},
				{N: 1, Response: 1},
				{N: 2, Response: 2, Diverge: []int64{4, 5}},
			},
		},
		{
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
			name: "duplicate seq",
			events: []cassette.Event{
				resp(1, ""), ev(1, "tool_use", 1), ev(2, "tool_call", 0),
			},
			want: []shape{{N: 1, Response: 1, ToolUses: []int64{1}, ToolCalls: []int64{2}}},
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
			// Concurrent requests that complete in reverse order: the last
			// response is that of the last request, not the last appended.
			name:   "last by interaction_id, not by seq",
			events: []cassette.Event{iid(resp(1, `{"stop_reason":"end_turn"}`), 2), iid(resp(3, `{"stop_reason":"tool_use"}`), 1)},
			want:   &Stop{StopReason: "end_turn"},
		},
		{
			// The cassette is untrusted: responses without an
			// interaction_id come after those with one, by seq.
			name: "responses without interaction_id come last",
			events: []cassette.Event{
				noIID(resp(1, `{"stop_reason":"a"}`)), noIID(resp(2, `{"stop_reason":"b"}`)),
				resp(3, `{"stop_reason":"c"}`), noIID(resp(4, `{"stop_reason":"d"}`)), resp(5, `{"stop_reason":"e"}`),
			},
			want: &Stop{StopReason: "d"},
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
