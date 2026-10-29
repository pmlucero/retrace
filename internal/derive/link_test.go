package derive

import (
	"context"
	"encoding/json"
	"strconv"
	"testing"
)

const threeToolUses = `{"id":"msg_4","type":"message","role":"assistant","model":"claude-sonnet-5-5","content":[` +
	`{"type":"tool_use","id":"toolu_a","name":"orders__search","input":{"status":"open","customer_id":"c_812"}},` +
	`{"type":"tool_use","id":"toolu_b","name":"orders__cancel","input":{"order":"o_1"}},` +
	`{"type":"tool_use","id":"toolu_c","name":"get_weather","input":{"city":"Paris"}}],` +
	`"stop_reason":"tool_use","usage":{"input_tokens":100,"output_tokens":40}}`

func cancelCall(id int, order string) string {
	return `{"jsonrpc":"2.0","id":` + strconv.Itoa(id) + `,"method":"tools/call","params":{"name":"cancel","arguments":{"order":"` + order + `"}}}`
}

func cancelOK(id int) string {
	return `{"jsonrpc":"2.0","id":` + strconv.Itoa(id) + `,"result":{"content":[]}}`
}

// linkScenarios cover linking tool_call to tool_use (spec §7.4).
var linkScenarios = []scenario{
	{
		// Each tools/call links to the most recent unlinked tool_use with
		// the same canonical arguments whose name ends with the MCP tool
		// name. A tool_use is linked once, and one the agent runs without
		// MCP (get_weather) stays unlinked.
		name: "mcp_tool_linking",
		exchanges: []exchange{
			{request: weatherQuestion, status: 200, response: threeToolUses, durationMillis: 900},
			mcpPost(1000, searchCall).answer(200, searchOK, 50),             // toolu_a: keys and spacing differ
			mcpPost(1100, cancelCall(4, "o_2")).answer(200, cancelOK(4), 5), // other arguments: no link
			mcpPost(1200, cancelCall(5, "o_1")).answer(200, cancelOK(5), 5), // toolu_b
			mcpPost(1300, searchCall).answer(200, searchOK, 50),             // toolu_a is already linked
			{startMillis: 2000, request: weatherQuestion, status: 200, chunks: streamedToolUse(), durationMillis: 1500},
			mcpPost(4000, searchCall).answer(200, searchOK, 50), // toolu_2, from the stream
		},
	},
	{
		// The agent sends the tools/call when it reads message_stop, before
		// the body of the streamed response ends, so its tool_call is
		// derived before the tool_use it answers and stays unlinked
		// (docs/plan/mvp.md, "Where events are derived"). Reindex orders the
		// request before the completion too, so it derives the same.
		name: "mcp_tool_call_before_stream_end",
		exchanges: []exchange{
			{request: weatherQuestion, status: 200, chunks: streamedToolUse(), durationMillis: 1500, completeAfterNext: true},
			mcpPost(1495, searchCall).answer(200, searchOK, 50),
		},
	},
}

// AC-05: the parent_seq of a tool_call is its tool_use (spec §7.4).
func TestToolLinks(t *testing.T) {
	type use struct{ name, input string }
	tests := []struct {
		name      string
		uses      []use // seqs 1, 2, ...
		tool      string
		arguments string // "" for absent
		want      int64  // 0 for no link
	}{
		{"same name", []use{{"search", `{"q":"a"}`}}, "search", `{"q":"a"}`, 1},
		{"prefixed name", []use{{"mcp__orders__search", `{"q":"a"}`}}, "search", `{"q":"a"}`, 1},
		{"plain suffix", []use{{"research", `{"q":"a"}`}}, "search", `{"q":"a"}`, 1},
		{"name is longer", []use{{"search", `{"q":"a"}`}}, "orders__search", `{"q":"a"}`, 0},
		{"name is a prefix", []use{{"search_all", `{"q":"a"}`}}, "search", `{"q":"a"}`, 0},
		{"case differs", []use{{"Search", `{"q":"a"}`}}, "search", `{"q":"a"}`, 0},
		{"no MCP tool name", []use{{"search", `{"q":"a"}`}}, "", `{"q":"a"}`, 0},
		{"keys, spacing and escapes", []use{{"search", `{"a":1,"q":"é"}`}}, "search", " {\"q\" : \"é\", \"a\": 1}", 1},
		{"number text differs", []use{{"search", `{"n":1}`}}, "search", `{"n":1.0}`, 0},
		{"arguments differ", []use{{"search", `{"q":"a"}`}}, "search", `{"q":"b"}`, 0},
		{"absent arguments are {}", []use{{"list", `{}`}}, "list", "", 1},
		{"null arguments are not {}", []use{{"list", `{}`}}, "list", `null`, 0},
		{"input kept as a string", []use{{"search", `"{\"q\":"`}}, "search", `{"q":`, 0},
		{"most recent", []use{{"search", `{"q":"a"}`}, {"search", `{"q":"a"}`}, {"other", `{"q":"a"}`}}, "search", `{"q":"a"}`, 2},
	}
	for _, tt := range tests {
		var l toolLinks
		for i, u := range tt.uses {
			l.add(int64(i+1), u.name, json.RawMessage(u.input))
		}
		var args json.RawMessage
		if tt.arguments != "" {
			args = json.RawMessage(tt.arguments)
		}
		got := l.link(tt.tool, args)
		if (got == nil) != (tt.want == 0) || (got != nil && *got != tt.want) {
			t.Errorf("%s: link = %v, want %d", tt.name, deref(got), tt.want)
		}
	}
}

// AC-05: a tool_use is linked at most once, and only the most recent
// maxUnlinked tool_use events can be linked.
func TestToolLinksOnceAndBounded(t *testing.T) {
	var l toolLinks
	l.add(1, "search", json.RawMessage(`{"q":"a"}`))
	l.add(2, "search", json.RawMessage(`{"q":"a"}`))
	for _, want := range []int64{2, 1, 0} {
		if got := l.link("search", json.RawMessage(`{"q":"a"}`)); deref(got) != want {
			t.Errorf("link = %d, want %d", deref(got), want)
		}
	}

	l = toolLinks{}
	for i := range maxUnlinked + 1 {
		l.add(int64(i+1), "t", json.RawMessage(`{"i":`+strconv.Itoa(i)+`}`))
	}
	if len(l.unlinked) != maxUnlinked {
		t.Errorf("kept %d tool_use events, want %d", len(l.unlinked), maxUnlinked)
	}
	if got := l.link("t", json.RawMessage(`{"i":0}`)); got != nil {
		t.Errorf("the oldest tool_use was linked (seq %d) after it was pruned", *got)
	}
	if got := l.link("t", json.RawMessage(`{"i":1}`)); deref(got) != 2 {
		t.Errorf("link = %d, want 2", deref(got))
	}
}

// AC-05: the race of a tool_call queued before the end of the streamed
// response it answers leaves it unlinked, at record time and in reindex.
func TestToolCallBeforeStreamEndIsUnlinked(t *testing.T) {
	ctx := context.Background()
	c := record(t, scenarioExchanges(t, "mcp_tool_call_before_stream_end"))
	for _, when := range []string{"recorded", "reindexed"} {
		if when == "reindexed" {
			if _, err := Reindex(ctx, c); err != nil {
				t.Fatal(err)
			}
		}
		events, err := c.Events(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var kinds []string
		for _, e := range events {
			kinds = append(kinds, e.Kind)
			if e.Kind == "tool_call" && e.ParentSeq != nil {
				t.Errorf("%s: tool_call linked to seq %d", when, *e.ParentSeq)
			}
		}
		if got, want := len(kinds), 5; got != want || kinds[1] != "tool_call" {
			t.Errorf("%s: kinds = %v, want the tool_call second of 5 events", when, kinds)
		}
	}
}

func deref(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}
