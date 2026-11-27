package anthropic

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// historyBody builds a Messages API request of about 1 MB: 50 messages
// alternating user and assistant, with tool results, HTML characters,
// non-ASCII text and a number with an exponent.
func historyBody() []byte {
	var sb strings.Builder
	sb.WriteString(`{"model":"claude-sonnet-4-5","max_tokens":1024,"stream":true,"messages":[`)
	pad := strings.Repeat("Lorem <ipsum> & dolor sit amet, ñandú é \"quoted\" 12345. ", 330)
	for i := 0; i < 50; i++ {
		if i > 0 {
			sb.WriteByte(',')
		}
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		fmt.Fprintf(&sb, `{"role":%q,"content":[{"type":"text","text":%q},{"type":"tool_result","tool_use_id":"toolu_%d","content":"ok","is_error":false,"n":1.50e3}]}`, role, pad, i)
	}
	sb.WriteString(`,{"role":"user","content":"last question"}]}`)
	return []byte(sb.String())
}

func BenchmarkParseRequest(b *testing.B) {
	body := historyBody()
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	for b.Loop() {
		if _, err := ParseRequest(body); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkConversationKey measures the canonicalization of a whole history,
// without the prefix cache that makes ParseRequest skip it on a repeated body.
func BenchmarkConversationKey(b *testing.B) {
	var req struct {
		Messages []json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(historyBody(), &req); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, ok := conversationKeyCached(nil, req.Messages); !ok {
			b.Fatal("not valid JSON")
		}
	}
}

func BenchmarkStreamFeed(b *testing.B) {
	events := [][]byte{
		[]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-sonnet-4-5\",\"content\":[],\"usage\":{\"input_tokens\":25,\"output_tokens\":1}}}\n\n"),
		[]byte("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n"),
	}
	for i := 0; i < 200; i++ {
		events = append(events, fmt.Appendf(nil, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"word%d \"}}\n\n", i))
	}
	events = append(events,
		[]byte("event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n"),
		[]byte("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_1\",\"name\":\"get\",\"input\":{}}}\n\n"),
		[]byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"a\\\":1}\"}}\n\n"),
		[]byte("event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":1}\n\n"),
		[]byte("event: ping\ndata: {\"type\":\"ping\"}\n\n"),
		[]byte("event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":99}}\n\n"),
		[]byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"),
	)
	b.ReportAllocs()
	for b.Loop() {
		var s Stream
		for _, e := range events {
			s.Feed(e)
		}
		_ = s.Message()
	}
}
