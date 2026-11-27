package replay

import (
	"context"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/pmlucero/retrace/internal/cassette"
)

// queuesSource writes the interactions of TestBuildQueues: 1 and 2 are LLM
// requests, 3 and 5 are MCP requests of server a, 4 is an MCP notification of
// a, which replay answers without matching, and 6 is an MCP request of a
// server that is not replayed.
func queuesSource(t *testing.T) *cassette.Cassette {
	t.Helper()
	ctx := context.Background()
	c := newCassette(t, "record")
	type rec struct {
		channel, upstream, method, body string
	}
	recs := []rec{
		{"llm", "anthropic", http.MethodPost, `{"p":1}`},
		{"llm", "anthropic", http.MethodPost, `{"p":2}`},
		{"mcp", "mcp:a", http.MethodPost, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`},
		{"mcp", "mcp:a", http.MethodPost, `{"jsonrpc":"2.0","method":"notifications/initialized"}`},
		{"mcp", "mcp:a", http.MethodPost, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`},
		{"mcp", "mcp:b", http.MethodPost, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`},
	}
	err := c.Write(ctx, func(tx *cassette.Tx) error {
		for i, r := range recs {
			key, err := tx.PutBlob(ctx, []byte(r.body))
			if err != nil {
				return err
			}
			if err := tx.BeginInteraction(ctx, cassette.Interaction{ID: int64(i + 1), Channel: r.channel, Upstream: r.upstream,
				HTTPMethod: r.method, Path: "/", RequestBlob: &key, StartedAt: time.Now(), Provenance: "recorded"}); err != nil {
				return err
			}
			status := http.StatusOK
			if err := tx.CompleteInteraction(ctx, int64(i+1), cassette.Completion{StatusCode: &status}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func ids(ins []cassette.Interaction) []int64 {
	out := make([]int64, len(ins))
	for i, in := range ins {
		out[i] = in.ID
	}
	return out
}

// spec §4.4, §5 step 3, AC-32.
func TestBuildQueues(t *testing.T) {
	ctx := context.Background()
	src := queuesSource(t)
	identity := func(b []byte) []byte { return b }
	mcpCanon := func(_ string, b []byte) []byte { return b }

	t.Run("llm and one server", func(t *testing.T) {
		qs, err := BuildQueues(ctx, src, identity, []string{"a"}, mcpCanon)
		if err != nil {
			t.Fatal(err)
		}
		if qs.LLM == nil || qs.MCP["a"] == nil || len(qs.MCP) != 1 {
			t.Fatalf("queues: llm %v, mcp %v; want the LLM and server a", qs.LLM, qs.MCP)
		}
		// 4 is answered without matching and 6 belongs to a server that is not
		// replayed, so neither is queued; the rest is sorted by id.
		if got, want := ids(qs.Unconsumed()), []int64{1, 2, 3, 5}; !slices.Equal(got, want) {
			t.Errorf("unconsumed %v, want %v", got, want)
		}
		next, ok := qs.LLM.Next(testFingerprint([]byte(`{"p":2}`)))
		if !ok || next.ID != 2 {
			t.Fatalf("LLM request 2 served %v, %v; want interaction 2", next.ID, ok)
		}
		if got, want := ids(qs.Unconsumed()), []int64{1, 3, 5}; !slices.Equal(got, want) {
			t.Errorf("unconsumed after serving 2: %v, want %v", got, want)
		}
	})

	t.Run("forwarded llm", func(t *testing.T) {
		qs, err := BuildQueues(ctx, src, nil, []string{"a", "b"}, mcpCanon)
		if err != nil {
			t.Fatal(err)
		}
		if qs.LLM != nil {
			t.Error("a forwarded LLM has queues")
		}
		if got, want := ids(qs.Unconsumed()), []int64{3, 5, 6}; !slices.Equal(got, want) {
			t.Errorf("unconsumed %v, want %v", got, want)
		}
	})

	t.Run("no servers", func(t *testing.T) {
		qs, err := BuildQueues(ctx, src, identity, nil, mcpCanon)
		if err != nil {
			t.Fatal(err)
		}
		if got, want := ids(qs.Unconsumed()), []int64{1, 2}; !slices.Equal(got, want) {
			t.Errorf("unconsumed %v, want %v", got, want)
		}
	})
}
