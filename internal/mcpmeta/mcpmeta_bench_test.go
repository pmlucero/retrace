package mcpmeta

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/pmlucero/retrace/internal/cassette"
)

// BenchmarkStoreServerInfo measures StoreServerInfo on a long session of 2000
// interactions: server a initialized at the start, server b never, so the
// scan goes through every interaction.
func BenchmarkStoreServerInfo(b *testing.B) {
	ctx := context.Background()
	c, err := cassette.Create(ctx, filepath.Join(b.TempDir(), "long.cassette"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = c.Close() })
	servers := json.RawMessage(`[{"name":"a","url":"http://a"},{"name":"b","url":"http://b"}]`)
	err = c.Write(ctx, func(tx *cassette.Tx) error {
		if err := tx.InsertSession(ctx, cassette.Session{ID: "long", Mode: "record", Status: "completed",
			CreatedAt: time.Now(), OnMiss: "strict", MCPServers: servers}); err != nil {
			return err
		}
		for i := 1; i <= 2000; i++ {
			channel, upstream := "llm", "anthropic"
			body := fmt.Sprintf(`{"model":"m","messages":[{"role":"user","content":"turn %d"}]}`, i)
			resp := `{"type":"message"}`
			if i == 1 {
				channel, upstream = "mcp", "mcp:a"
				body = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`
				resp = `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-06-18","serverInfo":{"name":"a"}}}`
			} else if i%2 == 0 {
				channel, upstream = "mcp", "mcp:b"
				body = fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":"t"}}`, i)
				resp = fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":{}}`, i)
			}
			req, err := tx.PutBlob(ctx, []byte(body))
			if err != nil {
				return err
			}
			res, err := tx.PutBlob(ctx, []byte(resp))
			if err != nil {
				return err
			}
			if err := tx.BeginInteraction(ctx, cassette.Interaction{ID: int64(i), Channel: channel, Upstream: upstream,
				HTTPMethod: "POST", Path: "/", RequestBlob: &req, StartedAt: time.Now(), Provenance: "recorded"}); err != nil {
				return err
			}
			status := 200
			if err := tx.CompleteInteraction(ctx, int64(i), cassette.Completion{StatusCode: &status, ResponseBlob: &res, DurationMillis: 1}); err != nil {
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
		if err := StoreServerInfo(ctx, c); err != nil {
			b.Fatal(err)
		}
	}
}
