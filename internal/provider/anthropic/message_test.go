package anthropic

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func i64(n int64) *int64 { return &n }

func TestAPIError(t *testing.T) {
	tests := map[string]string{
		`{"type":"rate_limit_error","message":"slow down"}`: "rate_limit_error: slow down",
		`{"type":"rate_limit_error"}`:                       "rate_limit_error",
		`{"message":"slow down"}`:                           "slow down",
		`{}`:                                                "unknown error",
		`"[REDACTED:00aa11bb]"`:                             "unknown error",
	}
	for raw, want := range tests {
		if got := apiError(json.RawMessage(raw)); got != want {
			t.Errorf("apiError(%s) = %q, want %q", raw, got, want)
		}
	}
}

func TestParseRequest(t *testing.T) {
	tests := []struct {
		name      string
		body      string
		model     string
		hasText   bool
		text      string
		wantErr   bool
		sameKeyAs string // another body whose trailing message must have the same key
	}{
		{name: "string content", body: `{"model":"m","messages":[{"role":"user","content":"hello"}]}`,
			model: "m", hasText: true, text: "hello"},
		{name: "text blocks joined", body: `{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"a"},{"type":"image"},{"type":"text","text":"b"}]}]}`,
			model: "m", hasText: true, text: "a\nb"},
		{name: "tool results only", body: `{"model":"m","messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":"x"}]}]}`,
			model: "m"},
		{name: "tool result and text", body: `{"model":"m","messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t"},{"type":"text","text":"go on"}]}]}`,
			model: "m", hasText: true, text: "go on"},
		{name: "trailing assistant", body: `{"model":"m","messages":[{"role":"user","content":"q"},{"role":"assistant","content":"prefill"}]}`,
			model: "m"},
		{name: "no messages", body: `{"model":"m","messages":[]}`, model: "m"},
		{name: "redacted messages", body: `{"model":"m","messages":"[REDACTED:0a1b2c3d]"}`, model: "m"},
		{name: "redacted model", body: `{"model":{"x":1},"messages":[{"role":"user","content":"hi"}]}`, hasText: true, text: "hi"},
		{name: "formatting does not change the key", body: `{"model":"m","messages":[{"content":"hi","role":"user"}]}`,
			model: "m", hasText: true, text: "hi", sameKeyAs: `{"model":"m", "messages":[ {"role":"user", "content":"hi"} ]}`},
		{name: "not JSON", body: `nope`, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseRequest([]byte(tt.body))
			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v, want error %v", err, tt.wantErr)
			}
			if got.Model != tt.model || got.HasUserText != tt.hasText || got.UserText != tt.text {
				t.Errorf("got %+v, want model %q, text %v %q", got, tt.model, tt.hasText, tt.text)
			}
			if got.HasUserText && got.UserKey == "" {
				t.Error("user text without a key")
			}
			if tt.sameKeyAs != "" {
				other, _ := ParseRequest([]byte(tt.sameKeyAs))
				if other.UserKey != got.UserKey {
					t.Errorf("keys differ: %s and %s", got.UserKey, other.UserKey)
				}
			}
		})
	}
}

// A message is identified by the conversation up to it (spec §7.4): the same
// text later in the conversation is a different message.
func TestParseRequestKeyDependsOnConversation(t *testing.T) {
	first, _ := ParseRequest([]byte(`{"messages":[{"role":"user","content":"continue"}]}`))
	later, _ := ParseRequest([]byte(`{"messages":[{"role":"user","content":"continue"},{"role":"assistant","content":"ok"},{"role":"user","content":"continue"}]}`))
	if first.UserKey == later.UserKey {
		t.Error("the same text at a later turn got the same key")
	}
}

func TestParseResponse(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		want    Message
		wantErr error
	}{
		{
			name: "text and tool use",
			body: `{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-5-5",
				"content":[{"type":"thinking","thinking":""},{"type":"text","text":"Let me look."},
				{"type":"tool_use","id":"tu_1","name":"search","input":{ "q": "x",  "n": 1.50 }}],
				"stop_reason":"tool_use","usage":{"input_tokens":10,"output_tokens":5,
				"cache_read_input_tokens":3,"cache_creation_input_tokens":2,
				"cache_creation":{"ephemeral_5m_input_tokens":2,"ephemeral_1h_input_tokens":0},
				"service_tier":"standard","inference_geo":"global"}}`,
			want: Message{Model: "claude-opus-5-5", StopReason: "tool_use", Text: "Let me look.", Complete: true,
				ToolUses: []ToolUse{{ID: "tu_1", Name: "search", Input: json.RawMessage(`{"q":"x","n":1.50}`)}},
				Usage: &Usage{InputTokens: i64(10), OutputTokens: i64(5), CacheReadInputTokens: i64(3),
					CacheCreationInputTokens: i64(2), CacheCreation5m: i64(2), CacheCreation1h: i64(0),
					ServiceTier: "standard", InferenceGeo: "global"}},
		},
		{
			name: "api error",
			body: `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`,
			want: Message{Error: "overloaded_error: Overloaded", Complete: true},
		},
		{
			name: "redacted usage counts are absent",
			body: `{"type":"message","model":"m","content":[],"usage":{"input_tokens":"[REDACTED:abcd1234]","output_tokens":7}}`,
			want: Message{Model: "m", Complete: true, Usage: &Usage{OutputTokens: i64(7)}},
		},
		{
			// --redact '$..content' replaces the whole array: the rest still counts.
			name: "redacted content",
			body: `{"type":"message","model":"m","content":"[REDACTED:0a1b2c3d]","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":2}}`,
			want: Message{Model: "m", StopReason: "end_turn", Complete: true, Usage: &Usage{InputTokens: i64(1), OutputTokens: i64(2)}},
		},
		{name: "other JSON", body: `{"type":"list"}`, wantErr: ErrNotMessage},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseResponse([]byte(tt.body))
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("error = %v, want %v", err, tt.wantErr)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got  %+v\nwant %+v", got, tt.want)
			}
		})
	}
}

func sseEvents(events ...string) [][]byte {
	out := make([][]byte, len(events))
	for i, e := range events {
		out[i] = []byte(e)
	}
	return out
}

func TestStream(t *testing.T) {
	full := sseEvents(
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"claude-sonnet-5-5\",\"usage\":{\"input_tokens\":20,\"output_tokens\":1,\"cache_read_input_tokens\":0}}}\n\n",
		"event: content_block_start\r\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\r\n\r\n",
		"event: ping\ndata: {\"type\":\"ping\"}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Hel\"}}\n\n",
		"event: content_block_delta\rdata: {\"type\":\"content_block_delta\",\"index\":0,\r\rdata: \"delta\":{\"type\":\"text_delta\",\"text\":\"lo\"}}\r\r",
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"tool_use\",\"id\":\"tu_9\",\"name\":\"get_weather\",\"input\":{}}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"city\\\": \"}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"\\\"Paris\\\"}\"}}\n\n",
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":1}\n\n",
		"event: some_future_event\ndata: {\"type\":\"some_future_event\"}\n\n",
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":42,\"cache_read_input_tokens\":4}}\n\n",
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
	)
	want := Message{
		Model: "claude-sonnet-5-5", StopReason: "tool_use", Text: "Hello", Complete: true,
		ToolUses: []ToolUse{{ID: "tu_9", Name: "get_weather", Input: json.RawMessage(`{"city":"Paris"}`)}},
		Usage:    &Usage{InputTokens: i64(20), OutputTokens: i64(42), CacheReadInputTokens: i64(4)},
	}

	tests := []struct {
		name   string
		events [][]byte
		want   Message
	}{
		{name: "complete", events: full, want: want},
		{
			// Cut in the middle of the tool use block: it is left out.
			name: "cut", events: full[:8],
			want: Message{Model: "claude-sonnet-5-5", Text: "Hello",
				Usage: &Usage{InputTokens: i64(20), OutputTokens: i64(1), CacheReadInputTokens: i64(0)}},
		},
		{
			name:   "error event",
			events: sseEvents(string(full[0]), "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"Overloaded\"}}\n\n"),
			want: Message{Model: "claude-sonnet-5-5", Error: "overloaded_error: Overloaded",
				Usage: &Usage{InputTokens: i64(20), OutputTokens: i64(1), CacheReadInputTokens: i64(0)}},
		},
		{
			name: "invalid streamed input is kept as a string",
			events: sseEvents(
				"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"t\",\"name\":\"n\",\"input\":{}}}\n\n",
				"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"[REDACTED:0011aabb]\"}}\n\n",
				"data: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
				"data: {\"type\":\"message_stop\"}\n\n",
				"data: [DONE]\n\n",
				": comment only\n\n",
			),
			want: Message{Complete: true, ToolUses: []ToolUse{{ID: "t", Name: "n", Input: json.RawMessage(`"[REDACTED:0011aabb]"`)}}},
		},
		{
			name: "tool use with no deltas keeps its start input",
			events: sseEvents(
				"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"t\",\"name\":\"n\",\"input\":{}}}\n\n",
				"data: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
			),
			want: Message{ToolUses: []ToolUse{{ID: "t", Name: "n", Input: json.RawMessage(`{}`)}}},
		},
		{
			name: "redacted block and index leave the rest",
			events: sseEvents(
				string(full[0]),
				"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":\"[REDACTED:0a1b2c3d]\"}\n\n",
				"data: {\"type\":\"content_block_delta\",\"index\":\"[REDACTED:0a1b2c3d]\",\"delta\":{\"type\":\"text_delta\",\"text\":\"x\"}}\n\n",
				"data: {\"type\":\"message_stop\"}\n\n",
			),
			want: Message{Model: "claude-sonnet-5-5", Complete: true,
				Usage: &Usage{InputTokens: i64(20), OutputTokens: i64(1), CacheReadInputTokens: i64(0)}},
		},
		{
			name:   "out of range index is ignored",
			events: sseEvents("data: {\"type\":\"content_block_start\",\"index\":99999999,\"content_block\":{\"type\":\"text\",\"text\":\"x\"}}\n\n"),
			want:   Message{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var s Stream
			for _, e := range tt.events {
				s.Feed(e)
			}
			if got := s.Message(); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got  %+v\nwant %+v", got, tt.want)
			}
		})
	}
}

func TestSSEData(t *testing.T) {
	tests := []struct {
		event string
		data  string
		ok    bool
	}{
		{"event: x\ndata: {}\n\n", "{}", true},
		{"data:{}\r\n\r\n", "{}", true},
		{"data: a\ndata: b\n\n", "a\nb", true},
		{"data\n\n", "", true},
		{": comment\n\n", "", false},
		{"data: unterminated", "unterminated", true},
	}
	for _, tt := range tests {
		data, ok := sseData([]byte(tt.event))
		if string(data) != tt.data || ok != tt.ok {
			t.Errorf("sseData(%q) = %q, %v; want %q, %v", tt.event, data, ok, tt.data, tt.ok)
		}
	}
	if _, ok := sseData([]byte(strings.Repeat("x", 10))); ok {
		t.Error("a line with no field name counted as data")
	}
}
