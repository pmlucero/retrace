package diff

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/pmlucero/retrace/internal/cassette"
)

// session builds the events of a small session for the comparison tests.
// Seqs are assigned in call order, so interaction ids and start times follow
// them too (see ev).
type session struct {
	events []cassette.Event
	last   int64 // seq of the last model_response
}

func (s *session) next() int64 { return int64(len(s.events)) + 1 }

// respond appends a model_response with stop reason stop and text.
func (s *session) respond(stop, text string) *session {
	out, _ := json.Marshal(map[string]string{"stop_reason": stop, "text": text})
	e := resp(s.next(), string(out))
	s.last = e.Seq
	s.events = append(s.events, e)
	return s
}

// fail appends a model_response whose status is error.
func (s *session) fail(msg string) *session {
	e := failed(s.next(), &msg)
	s.last = e.Seq
	s.events = append(s.events, e)
	return s
}

// use appends a tool_use of the last model_response.
func (s *session) use(tool, args string) *session {
	e := ev(s.next(), "tool_use", s.last)
	e.Source, e.Target, e.Tool, e.Arguments = "model", "agent", &tool, json.RawMessage(args)
	s.events = append(s.events, e)
	return s
}

// call appends a tool_call to server and its tool_result with result.
func (s *session) call(server, tool, args, result string) *session {
	c := ev(s.next(), "tool_call", 0)
	c.Source, c.Target, c.Tool, c.Arguments = "agent", "mcp:"+server, &tool, json.RawMessage(args)
	s.events = append(s.events, c)
	r := ev(s.next(), "tool_result", c.Seq)
	r.Source, r.Target, r.Tool, r.Result = "mcp:"+server, "agent", &tool, json.RawMessage(result)
	s.events = append(s.events, r)
	return s
}

// callNoTool appends a tool_call whose tool is absent, as the deriver gives
// for params that are not an object (spec §7.4), or empty when empty is set.
func (s *session) callNoTool(empty bool) *session {
	c := ev(s.next(), "tool_call", 0)
	c.Source, c.Target = "agent", "mcp:s"
	if empty {
		c.Tool = new(string)
	}
	s.events = append(s.events, c)
	return s
}

// diverge appends a divergence event.
func (s *session) diverge() *session {
	e := ev(s.next(), "divergence", 0)
	e.Source, e.Target, e.Status = "retrace", "agent", "divergence"
	s.events = append(s.events, e)
	return s
}

func (s *session) trajectory() Trajectory { return Build(s.events) }

// checkout is a three-step session: search, then fetch, then answer.
func checkout(status string) *session {
	s := &session{}
	s.respond("tool_use", "Let me search.").use("search_orders", `{"customer_id":"c_812","status":`+status+`}`)
	s.call("shop", "search_orders", `{"customer_id":"c_812","status":`+status+`}`, `{"orders":["o_1"]}`)
	s.respond("tool_use", "Fetching.").use("get_order", `{"id":"o_1"}`)
	s.call("shop", "get_order", `{"id":"o_1"}`, `{"total":12.5}`)
	return s.respond("end_turn", "Your order totals 12.5.")
}

// pointShape is a Point reduced to what the tests state.
type pointShape struct {
	Step         int
	Kind         string
	ASeq, BSeq   int64 // 0 for a nil side
	Field        string
	Paths        []string
	StopA, StopB *Stop
}

func shapeOf(p *Point) *pointShape {
	if p == nil {
		return nil
	}
	ps := &pointShape{Step: p.Step, Kind: p.Kind, Field: p.Field, StopA: p.StopA, StopB: p.StopB}
	if p.A != nil {
		ps.ASeq = p.A.Seq
	}
	if p.B != nil {
		ps.BSeq = p.B.Seq
	}
	for _, e := range p.Diff.Entries {
		ps.Paths = append(ps.Paths, e.Path+": "+string(e.Expected)+" → "+string(e.Received))
	}
	return ps
}

// Groundwork for AC-19 and AC-20: the CLI tests that claim them build on
// these results.
func TestCompare(t *testing.T) {
	tests := []struct {
		name      string
		a, b      *session
		opts      Options
		want      *pointShape
		identical Span
		textDiffs []int
	}{
		{
			name:      "identical sessions",
			a:         checkout(`"open"`),
			b:         checkout(`"open"`),
			identical: Span{First: 1, Last: 3, ToolCalls: 2},
			textDiffs: []int{},
		},
		{
			name:      "member order and whitespace do not matter",
			a:         (&session{}).respond("tool_use", "").use("t", `{"a":1,"b":2}`),
			b:         (&session{}).respond("tool_use", "").use("t", `{ "b": 2, "a": 1 }`),
			identical: Span{First: 1, Last: 1},
			textDiffs: []int{},
		},
		{
			name:      "an argument differs at step 1",
			a:         checkout(`"open"`),
			b:         checkout(`"closed"`),
			want:      &pointShape{Step: 1, Kind: "tool_use", ASeq: 2, BSeq: 2, Field: "arguments", Paths: []string{`$.status: "open" → "closed"`}},
			identical: Span{First: 1, Last: 0},
			textDiffs: []int{},
		},
		{
			name:      "a missing argument shows as missing",
			a:         (&session{}).respond("tool_use", "").use("search_orders", `{"customer_id":"c_812","status":"open"}`),
			b:         (&session{}).respond("tool_use", "").use("search_orders", `{"customer_id":"c_812"}`),
			want:      &pointShape{Step: 1, Kind: "tool_use", ASeq: 2, BSeq: 2, Field: "arguments", Paths: []string{`$.status: "open" → `}},
			identical: Span{First: 1, Last: 0},
			textDiffs: []int{},
		},
		{
			name:      "another tool, same arguments",
			a:         (&session{}).respond("tool_use", "").use("a", `{}`),
			b:         (&session{}).respond("tool_use", "").use("b", `{}`),
			want:      &pointShape{Step: 1, Kind: "tool_use", ASeq: 2, BSeq: 2},
			identical: Span{First: 1, Last: 0},
			textDiffs: []int{},
		},
		{
			name:      "text differs without --text: listed, not a divergence",
			a:         (&session{}).respond("end_turn", "Hello."),
			b:         (&session{}).respond("end_turn", "Hi."),
			identical: Span{First: 1, Last: 1},
			textDiffs: []int{1},
		},
		{
			name:      "text differs under --text",
			a:         (&session{}).respond("end_turn", "Hello."),
			b:         (&session{}).respond("end_turn", "Hi."),
			opts:      Options{Text: true},
			want:      &pointShape{Step: 1, Kind: "model_response", ASeq: 1, BSeq: 1, Field: "text", Paths: []string{`$: "Hello." → "Hi."`}},
			identical: Span{First: 1, Last: 0},
			textDiffs: []int{},
		},
		{
			name:      "text differences past the divergence point are listed",
			a:         (&session{}).respond("tool_use", "x").use("a", `{}`).respond("end_turn", "y"),
			b:         (&session{}).respond("tool_use", "x").use("b", `{}`).respond("end_turn", "z"),
			want:      &pointShape{Step: 1, Kind: "tool_use", ASeq: 2, BSeq: 2},
			identical: Span{First: 1, Last: 0},
			textDiffs: []int{2},
		},
		{
			name:      "B ends early, with fewer steps",
			a:         checkout(`"open"`),
			b:         (&session{}).respond("tool_use", "Let me search.").use("search_orders", `{"customer_id":"c_812","status":"open"}`).call("shop", "search_orders", `{"customer_id":"c_812","status":"open"}`, `{"orders":["o_1"]}`),
			want:      &pointShape{Step: 2, Kind: "model_response", ASeq: 5},
			identical: Span{First: 1, Last: 1, ToolCalls: 1},
			textDiffs: []int{},
		},
		{
			name:      "A has a tool_use that B lacks",
			a:         (&session{}).respond("tool_use", "").use("a", `{}`).use("b", `{}`),
			b:         (&session{}).respond("tool_use", "").use("a", `{}`),
			want:      &pointShape{Step: 1, Kind: "tool_use", ASeq: 3},
			identical: Span{First: 1, Last: 0},
			textDiffs: []int{},
		},
		{
			name:      "tool_use order matters",
			a:         (&session{}).respond("tool_use", "").use("a", `{}`).use("b", `{}`),
			b:         (&session{}).respond("tool_use", "").use("b", `{}`).use("a", `{}`),
			want:      &pointShape{Step: 1, Kind: "tool_use", ASeq: 2, BSeq: 2},
			identical: Span{First: 1, Last: 0},
			textDiffs: []int{},
		},
		{
			name: "tool calls and results that complete in another order are equal",
			a: (&session{}).respond("tool_use", "").
				call("s", "a", `{}`, `{"r":1}`).call("s", "b", `{}`, `{"r":2}`),
			b: (&session{}).respond("tool_use", "").
				call("s", "b", `{}`, `{"r":2}`).call("s", "a", `{}`, `{"r":1}`),
			identical: Span{First: 1, Last: 1, ToolCalls: 2},
			textDiffs: []int{},
		},
		{
			name:      "A calls a tool that B does not: the shared call is set aside",
			a:         (&session{}).respond("tool_use", "").call("s", "a", `{}`, `{}`).call("s", "b", `{"x":1}`, `{}`),
			b:         (&session{}).respond("tool_use", "").call("s", "b", `{"x":1}`, `{}`),
			want:      &pointShape{Step: 1, Kind: "tool_call", ASeq: 2},
			identical: Span{First: 1, Last: 0},
			textDiffs: []int{},
		},
		{
			name:      "unmatched calls of the same tool are paired with an argument diff",
			a:         (&session{}).respond("tool_use", "").call("s", "a", `{}`, `{}`).call("s", "b", `{"x":1}`, `{}`),
			b:         (&session{}).respond("tool_use", "").call("s", "b", `{"x":2}`, `{}`).call("s", "a", `{}`, `{}`),
			want:      &pointShape{Step: 1, Kind: "tool_call", ASeq: 4, BSeq: 2, Field: "arguments", Paths: []string{`$.x: 1 → 2`}},
			identical: Span{First: 1, Last: 0},
			textDiffs: []int{},
		},
		{
			name:      "different tools get no argument diff",
			a:         (&session{}).respond("tool_use", "").use("a", `{"x":1}`),
			b:         (&session{}).respond("tool_use", "").use("b", `{"x":2}`),
			want:      &pointShape{Step: 1, Kind: "tool_use", ASeq: 2, BSeq: 2},
			identical: Span{First: 1, Last: 0},
			textDiffs: []int{},
		},
		{
			name:      "two absent tools are equal",
			a:         (&session{}).respond("tool_use", "").callNoTool(false),
			b:         (&session{}).respond("tool_use", "").callNoTool(false),
			identical: Span{First: 1, Last: 1, ToolCalls: 1},
			textDiffs: []int{},
		},
		{
			name:      "an absent tool and an empty one differ",
			a:         (&session{}).respond("tool_use", "").callNoTool(false),
			b:         (&session{}).respond("tool_use", "").callNoTool(true),
			want:      &pointShape{Step: 1, Kind: "tool_call", ASeq: 2, BSeq: 2},
			identical: Span{First: 1, Last: 0},
			textDiffs: []int{},
		},
		{
			name:      "an empty tool and an absent one differ",
			a:         (&session{}).respond("tool_use", "").callNoTool(true),
			b:         (&session{}).respond("tool_use", "").callNoTool(false),
			want:      &pointShape{Step: 1, Kind: "tool_call", ASeq: 2, BSeq: 2},
			identical: Span{First: 1, Last: 0},
			textDiffs: []int{},
		},
		{
			name:      "a tool_result differs in its result",
			a:         (&session{}).respond("tool_use", "").call("s", "a", `{}`, `{"r":1}`),
			b:         (&session{}).respond("tool_use", "").call("s", "a", `{}`, `{"r":2}`),
			want:      &pointShape{Step: 1, Kind: "tool_result", ASeq: 3, BSeq: 3, Field: "result", Paths: []string{`$.r: 1 → 2`}},
			identical: Span{First: 1, Last: 0},
			textDiffs: []int{},
		},
		{
			name:      "a tool_call to another server",
			a:         (&session{}).respond("tool_use", "").call("s1", "a", `{}`, `{}`),
			b:         (&session{}).respond("tool_use", "").call("s2", "a", `{}`, `{}`),
			want:      &pointShape{Step: 1, Kind: "tool_call", ASeq: 2, BSeq: 2},
			identical: Span{First: 1, Last: 0},
			textDiffs: []int{},
		},
		{
			name:      "a divergence in B",
			a:         (&session{}).respond("tool_use", "").call("s", "a", `{}`, `{}`),
			b:         (&session{}).respond("tool_use", "").call("s", "a", `{}`, `{}`).diverge(),
			want:      &pointShape{Step: 1, Kind: "divergence", BSeq: 4},
			identical: Span{First: 1, Last: 0},
			textDiffs: []int{},
		},
		{
			name:      "stop markers differ",
			a:         (&session{}).respond("end_turn", ""),
			b:         (&session{}).respond("max_tokens", ""),
			want:      &pointShape{Step: 1, Kind: KindStop, StopA: &Stop{StopReason: "end_turn"}, StopB: &Stop{StopReason: "max_tokens"}},
			identical: Span{First: 1, Last: 1},
			textDiffs: []int{},
		},
		{
			name:      "failed responses with the same error are equal",
			a:         (&session{}).fail("overloaded_error: busy"),
			b:         (&session{}).fail("overloaded_error: busy"),
			identical: Span{First: 1, Last: 1},
			textDiffs: []int{},
		},
		{
			name: "failed responses with another error",
			a:    (&session{}).fail("overloaded_error: busy"),
			b:    (&session{}).fail("cassette miss"),
			want: &pointShape{Step: 1, Kind: KindStop,
				StopA: &Stop{Failed: true, Error: "overloaded_error: busy"}, StopB: &Stop{Failed: true, Error: "cassette miss"}},
			identical: Span{First: 1, Last: 1},
			textDiffs: []int{},
		},
		{
			name:      "step 0 on one side only",
			a:         (&session{}).diverge(),
			b:         &session{},
			want:      &pointShape{Step: 0, Kind: "divergence", ASeq: 1},
			identical: Span{First: 0, Last: -1},
			textDiffs: []int{},
		},
		{
			name:      "two empty sessions",
			a:         &session{},
			b:         &session{},
			identical: Span{First: 1, Last: 0},
			textDiffs: []int{},
		},
		{
			name:      "one session without responses",
			a:         (&session{}).respond("end_turn", ""),
			b:         &session{},
			want:      &pointShape{Step: 1, Kind: "model_response", ASeq: 1},
			identical: Span{First: 1, Last: 0},
			textDiffs: []int{},
		},
		{
			name:      "arguments that are not JSON are compared as bytes",
			a:         (&session{}).respond("tool_use", "").use("t", `"{\"a\":"`),
			b:         (&session{}).respond("tool_use", "").use("t", `"{\"a\":1}"`),
			want:      &pointShape{Step: 1, Kind: "tool_use", ASeq: 2, BSeq: 2, Field: "arguments", Paths: []string{`$: "{\"a\":" → "{\"a\":1}"`}},
			identical: Span{First: 1, Last: 0},
			textDiffs: []int{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Compare(tt.a.trajectory(), tt.b.trajectory(), tt.opts)
			if g := shapeOf(got.Point); !reflect.DeepEqual(g, tt.want) {
				t.Errorf("point = %+v, want %+v", g, tt.want)
			}
			if got.Identical != tt.identical {
				t.Errorf("identical = %+v, want %+v", got.Identical, tt.identical)
			}
			if !reflect.DeepEqual(got.TextDifferences, tt.textDiffs) {
				t.Errorf("text differences = %v, want %v", got.TextDifferences, tt.textDiffs)
			}
		})
	}
}

// Spec §8.3 step 4: a divergence event is reported even when both sides
// stopped at the same step, where the two compare equal.
func TestCompareReportsDivergences(t *testing.T) {
	a := (&session{}).respond("tool_use", "").call("s", "a", `{}`, `{}`).diverge()
	b := (&session{}).respond("tool_use", "").call("s", "a", `{}`, `{}`).diverge()
	got := Compare(a.trajectory(), b.trajectory(), Options{})
	if got.Point != nil {
		t.Errorf("point = %+v, want none", got.Point)
	}
	if got.DivergedA == nil || got.DivergedA.Seq != 4 || got.DivergedB == nil || got.DivergedB.Seq != 4 {
		t.Errorf("diverged = %+v, %+v; want seq 4 on both sides", got.DivergedA, got.DivergedB)
	}

	got = Compare(a.trajectory(), checkout(`"open"`).trajectory(), Options{})
	if got.DivergedA == nil || got.DivergedB != nil {
		t.Errorf("diverged = %+v, %+v; want only A", got.DivergedA, got.DivergedB)
	}
}

// The comparison is symmetric in where it stops: swapping the sides swaps A
// and B in the point and nothing else.
func TestCompareSwapped(t *testing.T) {
	a, b := checkout(`"open"`).trajectory(), checkout(`"closed"`).trajectory()
	ab, ba := Compare(a, b, Options{}), Compare(b, a, Options{})
	if ab.Point.Step != ba.Point.Step || ab.Point.Kind != ba.Point.Kind {
		t.Fatalf("points differ: %+v vs %+v", ab.Point, ba.Point)
	}
	if ab.Point.A.Seq != ba.Point.B.Seq || ab.Point.B.Seq != ba.Point.A.Seq {
		t.Errorf("sides not swapped: %+v vs %+v", ab.Point, ba.Point)
	}
}

// Malformed arguments in an untrusted cassette compare without panicking.
func TestCompareInvalidJSON(t *testing.T) {
	a := (&session{}).respond("tool_use", "").use("t", `{`)
	b := (&session{}).respond("tool_use", "").use("t", `[`)
	got := Compare(a.trajectory(), b.trajectory(), Options{})
	if got.Point == nil || got.Point.Field != FieldArguments || len(got.Point.Diff.Entries) != 1 {
		t.Fatalf("point = %+v, want one argument entry", got.Point)
	}
}
