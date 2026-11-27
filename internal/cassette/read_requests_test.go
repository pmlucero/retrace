package cassette

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"testing"
)

// requestsCassette holds, in id order: 1 an LLM request, 2 an MCP request of
// x, 3 an MCP GET of y with no request blob, 4 an LLM request with an empty
// body, 5 an MCP request of x whose blob row was deleted afterwards.
func requestsCassette(t *testing.T) *Cassette {
	t.Helper()
	ctx := context.Background()
	c, _ := newCassette(t)
	type rec struct {
		channel, upstream, method string
		body                      *string
	}
	recs := []rec{
		{"llm", "anthropic", "POST", ptr("a")},
		{"mcp", "mcp:x", "POST", ptr("bb")},
		{"mcp", "mcp:y", "GET", nil},
		{"llm", "anthropic", "POST", ptr("")},
		{"mcp", "mcp:x", "POST", ptr("gone")},
	}
	var gone string
	write(t, c, func(tx *Tx) error {
		if err := tx.InsertSession(ctx, testSession()); err != nil {
			return err
		}
		for i, r := range recs {
			in := Interaction{ID: int64(i + 1), Channel: r.channel, Upstream: r.upstream, HTTPMethod: r.method,
				Path: "/", StartedAt: at(i), Provenance: "recorded"}
			if r.body != nil {
				key, err := tx.PutBlob(ctx, []byte(*r.body))
				if err != nil {
					return err
				}
				in.RequestBlob = &key
				gone = key
			}
			if err := tx.BeginInteraction(ctx, in); err != nil {
				return err
			}
		}
		return nil
	})
	// A cassette is untrusted input: its foreign keys may not hold.
	conn, err := c.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, `DELETE FROM blobs WHERE sha256 = ?`, gone); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys = ON`); err != nil {
		t.Fatal(err)
	}
	return c
}

func idsOf(ins []Interaction) []int64 {
	out := make([]int64, len(ins))
	for i, in := range ins {
		out[i] = in.ID
	}
	return out
}

// The batched read returns what Interactions plus one Blob per interaction
// return: same interactions in id order, same bodies, and the same error for
// a blob row that is missing.
func TestInteractionsWithRequestsMatchesBlob(t *testing.T) {
	ctx := context.Background()
	c := requestsCassette(t)
	all, err := c.Interactions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.InteractionsWithRequests(ctx, InteractionFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(all) {
		t.Fatalf("got %d interactions, want %d", len(got), len(all))
	}
	for i, in := range all {
		if got[i].ID != in.ID || got[i].Upstream != in.Upstream {
			t.Fatalf("interaction %d = %+v, want %+v", i, got[i].Interaction, in)
		}
		if in.RequestBlob == nil {
			if got[i].Request != nil || got[i].RequestErr != nil {
				t.Errorf("interaction %d without a blob has request %q, err %v", in.ID, got[i].Request, got[i].RequestErr)
			}
			continue
		}
		want, wantErr := c.Blob(ctx, *in.RequestBlob)
		if (wantErr == nil) != (got[i].RequestErr == nil) || (wantErr != nil && wantErr.Error() != got[i].RequestErr.Error()) {
			t.Errorf("interaction %d: err = %v, want %v", in.ID, got[i].RequestErr, wantErr)
		}
		if !bytes.Equal(got[i].Request, want) {
			t.Errorf("interaction %d: request = %q, want %q", in.ID, got[i].Request, want)
		}
	}
	if !errors.Is(got[4].RequestErr, ErrNotFound) {
		t.Errorf("missing blob: err = %v, want ErrNotFound", got[4].RequestErr)
	}
	if got[2].Request != nil {
		t.Errorf("no blob: request = %q, want nil", got[2].Request)
	}
	if got[3].RequestErr != nil || len(got[3].Request) != 0 {
		t.Errorf("empty blob: request = %q, err = %v", got[3].Request, got[3].RequestErr)
	}
}

func TestInteractionFilter(t *testing.T) {
	ctx := context.Background()
	c := requestsCassette(t)
	for _, tc := range []struct {
		name string
		f    InteractionFilter
		want []int64
	}{
		{"none", InteractionFilter{}, []int64{1, 2, 3, 4, 5}},
		{"channel", InteractionFilter{Channel: "mcp"}, []int64{2, 3, 5}},
		{"method", InteractionFilter{Method: "GET"}, []int64{3}},
		{"upstream", InteractionFilter{Upstreams: []string{"mcp:x"}}, []int64{2, 5}},
		{"upstreams", InteractionFilter{Upstreams: []string{"mcp:y", "mcp:x"}}, []int64{2, 3, 5}},
		{"all", InteractionFilter{Channel: "mcp", Method: "POST", Upstreams: []string{"mcp:x", "mcp:y"}}, []int64{2, 5}},
		{"no match", InteractionFilter{Channel: "llm", Upstreams: []string{"mcp:x"}}, []int64{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ins, err := c.InteractionsFiltered(ctx, tc.f)
			if err != nil {
				t.Fatal(err)
			}
			if got := idsOf(ins); !slices.Equal(got, tc.want) {
				t.Errorf("InteractionsFiltered ids = %v, want %v", got, tc.want)
			}
			reqs, err := c.InteractionsWithRequests(ctx, tc.f)
			if err != nil {
				t.Fatal(err)
			}
			got := make([]int64, len(reqs))
			for i, r := range reqs {
				got[i] = r.ID
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("InteractionsWithRequests ids = %v, want %v", got, tc.want)
			}
		})
	}
}
