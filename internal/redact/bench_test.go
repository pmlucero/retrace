package redact

import (
	"fmt"
	"strings"
	"testing"
)

// BenchmarkRulesBody measures the cost --redact adds on the forwarding path
// (spec §3.5): about 2.5 ms for a 1 MB request and 2 µs per SSE event.
func BenchmarkRulesBody(b *testing.B) {
	r := mustRules(&testing.T{}, "$..email")
	var msgs []string
	for i := range 400 {
		msgs = append(msgs, fmt.Sprintf(`{"role":"user","content":[{"type":"text","text":%q}]}`, strings.Repeat("lorem ipsum ", 200)+fmt.Sprint(i)))
	}
	request := []byte(`{"model":"m","max_tokens":1024,"messages":[` + strings.Join(msgs, ",") + `]}`)
	event := []byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"a few tokens\"}}\n\n")

	b.Run(fmt.Sprintf("request-%dKB", len(request)>>10), func(b *testing.B) {
		b.SetBytes(int64(len(request)))
		for b.Loop() {
			_, _ = r.Body(request)
		}
	})
	b.Run("sse-event", func(b *testing.B) {
		for b.Loop() {
			_, _ = r.SSEEvent(event)
		}
	})
}
