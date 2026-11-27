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

// BenchmarkBodyCases measures Body and SSEEvent on a 1 MB JSON body and on
// one SSE event: no rules, a rule whose key is absent, and a rule that
// matches.
func BenchmarkBodyCases(b *testing.B) {
	var msgs []string
	for i := range 4000 {
		msgs = append(msgs, fmt.Sprintf(`{"role":"user","content":[{"type":"text","text":%q}]}`, strings.Repeat("lorem ipsum ", 16)+fmt.Sprint(i)))
	}
	plain := []byte(`{"model":"m","max_tokens":1024,"messages":[` + strings.Join(msgs, ",") + `]}`)
	withKey := []byte(`{"email":"a@b.c",` + string(plain[1:]))
	plainEvent := []byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"a few tokens\"}}\n\n")
	keyEvent := []byte("event: x\ndata: {\"email\":\"a@b.c\",\"index\":0}\n\n")

	none := NewRules(nil, nil)
	rules := mustRules(&testing.T{}, "$..email")
	indexRules := mustRules(&testing.T{}, "$.items[0].email")
	indexBody := []byte(`{"items":[{"email":"a@b.c"}],` + string(plain[1:]))
	for _, tt := range []struct {
		name  string
		r     *Rules
		body  []byte
		event []byte
	}{
		{"no-rules", none, plain, plainEvent},
		{"rules-no-key", rules, plain, plainEvent},
		{"rules-key-present", rules, withKey, keyEvent},
		{"index-rules-no-key", indexRules, plain, plainEvent},
		{"index-rules-key-present", indexRules, indexBody, keyEvent},
	} {
		b.Run("body-1MB/"+tt.name, func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(tt.body)))
			for b.Loop() {
				_, _ = tt.r.Body(tt.body)
			}
		})
		b.Run("sse/"+tt.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_, _ = tt.r.SSEEvent(tt.event)
			}
		})
	}
}
