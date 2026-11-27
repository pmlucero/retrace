package replay

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/pmlucero/retrace/internal/cassette"
)

// benchSource writes a long session of n completed interactions to a
// cassette file in a temp dir: LLM requests and MCP requests of two servers.
func benchSource(b *testing.B, n int) *cassette.Cassette {
	b.Helper()
	ctx := context.Background()
	c, err := cassette.Create(ctx, filepath.Join(b.TempDir(), "long.cassette"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = c.Close() })
	err = c.Write(ctx, func(tx *cassette.Tx) error {
		if err := tx.InsertSession(ctx, cassette.Session{ID: "long", Mode: "record", Status: "completed",
			CreatedAt: time.Now(), Command: []string{"agent"}, OnMiss: "strict"}); err != nil {
			return err
		}
		for i := 1; i <= n; i++ {
			channel, upstream := "llm", "anthropic"
			body := fmt.Sprintf(`{"model":"m","messages":[{"role":"user","content":"turn %d"}]}`, i)
			if i%2 == 0 {
				channel, upstream = "mcp", "mcp:"+[]string{"a", "b"}[i/2%2]
				body = fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":"t","arguments":{"n":%d}}}`, i, i)
			}
			key, err := tx.PutBlob(ctx, []byte(body))
			if err != nil {
				return err
			}
			if err := tx.BeginInteraction(ctx, cassette.Interaction{ID: int64(i), Channel: channel, Upstream: upstream,
				HTTPMethod: http.MethodPost, Path: "/", RequestBlob: &key, StartedAt: time.Now(), Provenance: "recorded"}); err != nil {
				return err
			}
			status := http.StatusOK
			if err := tx.CompleteInteraction(ctx, int64(i), cassette.Completion{StatusCode: &status, DurationMillis: 1}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		b.Fatal(err)
	}
	return c
}

// BenchmarkBuildQueues measures the start of a replay of a long session.
func BenchmarkBuildQueues(b *testing.B) {
	ctx := context.Background()
	c := benchSource(b, 2000)
	canon := func(body []byte) []byte { return body }
	mcpCanon := func(_ string, body []byte) []byte { return body }
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := BuildQueues(ctx, c, canon, []string{"a", "b"}, mcpCanon); err != nil {
			b.Fatal(err)
		}
	}
}
