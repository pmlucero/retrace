package derive

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/pmlucero/retrace/internal/cassette"
)

// BenchmarkReindex measures reindexing a long session of 2000 completed LLM
// interactions, each with a request blob.
func BenchmarkReindex(b *testing.B) {
	ctx := context.Background()
	c, err := cassette.Create(ctx, filepath.Join(b.TempDir(), "long.cassette"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = c.Close() })
	err = c.Write(ctx, func(tx *cassette.Tx) error {
		if err := tx.InsertSession(ctx, cassette.Session{ID: "long", Mode: "record", Status: "completed",
			CreatedAt: time.Now(), Command: []string{"agent"}, OnMiss: "strict", DeriverVersion: Version}); err != nil {
			return err
		}
		for i := 1; i <= 2000; i++ {
			body := fmt.Sprintf(`{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"turn %d"}]}`, i)
			key, err := tx.PutBlob(ctx, []byte(body))
			if err != nil {
				return err
			}
			resp, err := tx.PutBlob(ctx, []byte(`{"type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
			if err != nil {
				return err
			}
			start := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC).Add(time.Duration(i) * time.Second)
			if err := tx.BeginInteraction(ctx, cassette.Interaction{ID: int64(i), Channel: "llm", Upstream: "anthropic",
				HTTPMethod: http.MethodPost, Path: "/v1/messages", RequestBlob: &key, StartedAt: start, Provenance: "recorded"}); err != nil {
				return err
			}
			status := http.StatusOK
			if err := tx.CompleteInteraction(ctx, int64(i), cassette.Completion{StatusCode: &status, ResponseBlob: &resp, DurationMillis: 1}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := Reindex(ctx, c); err != nil {
			b.Fatal(err)
		}
	}
}
