package replay

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
)

// fakeStrategy resolves every miss with res and err, and keeps the misses it
// was asked about.
type fakeStrategy struct {
	res Resolution
	err error

	mu     sync.Mutex
	misses []Miss
}

func (*fakeStrategy) Name() string { return "fake" }

func (s *fakeStrategy) OnMiss(_ context.Context, m Miss) (Resolution, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.misses = append(s.misses, m)
	return s.res, s.err
}

func TestStrict(t *testing.T) {
	var s DivergenceStrategy = Strict{}
	if s.Name() != "strict" {
		t.Errorf("Name() = %q, want strict", s.Name())
	}
	res, err := s.OnMiss(context.Background(), Miss{Channel: "llm", Upstream: "anthropic", Reason: "r"})
	if err != nil || res.Respond != nil {
		t.Errorf("OnMiss = %+v, %v; want an abort", res, err)
	}
}

// Spec §6.1: the handlers ask their strategy about each miss, with the
// channel, upstream and reason of its divergence event, and the event is
// recorded.
func TestHandlersAskTheStrategy(t *testing.T) {
	llm := newReplayHarness(t, sourceCassette(t, recorded{request: `{"q":"a"}`, body: `{}`}), nil, Options{})
	llmStrategy := &fakeStrategy{}
	llm.h.rc.strategy = llmStrategy
	if status, _ := llm.post(`{"q":"b"}`); status != http.StatusUnprocessableEntity {
		t.Fatalf("LLM status %d, want 422", status)
	}
	_, events := llm.stop()
	divergenceResult(t, events)

	m := newMCPHarness(t, mcpSourceCassette(t), nil)
	mcpStrategy := &fakeStrategy{}
	m.h.rc.strategy = mcpStrategy
	if resp, _ := m.post("", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`); resp.status != http.StatusOK {
		t.Fatalf("MCP status %d, want 200 with an error", resp.status)
	}
	_, events = m.stop()
	divergenceOn(t, events, "mcp", "mcp:"+testServer)

	for _, tc := range []struct {
		name     string
		strategy *fakeStrategy
		want     Miss
	}{
		{"llm", llmStrategy, Miss{Channel: "llm", Upstream: "anthropic", Reason: "no recorded request matches this one"}},
		{"mcp", mcpStrategy, Miss{Channel: "mcp", Upstream: "mcp:" + testServer, Reason: "no recorded request matches this one"}},
	} {
		if len(tc.strategy.misses) != 1 || tc.strategy.misses[0] != tc.want {
			t.Errorf("%s: strategy asked about %+v, want [%+v]", tc.name, tc.strategy.misses, tc.want)
		}
	}
}

// No strategy of this version responds, so a strategy that fails or
// responds has its miss aborted, as strict does (spec §6.2).
func TestUnsupportedResolutionsAbort(t *testing.T) {
	for _, tc := range []struct {
		name     string
		strategy *fakeStrategy
	}{
		{"error", &fakeStrategy{err: errors.New("boom")}},
		{"respond", &fakeStrategy{res: Resolution{Respond: &Response{Provenance: "synthetic"}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newReplayHarness(t, sourceCassette(t, recorded{request: `{"q":"a"}`, body: `{}`}), nil, Options{})
			r.h.rc.strategy = tc.strategy
			status, body := r.post(`{"q":"b"}`)
			checkMissResponse(t, status, body, "no recorded request matches")
			if !r.diverged() {
				t.Error("Diverged not closed after an aborted miss")
			}
			ins, events := r.stop()
			divergenceResult(t, events)
			if len(ins) != 0 {
				t.Errorf("recorded %d interactions, want none for an aborted miss", len(ins))
			}
		})
	}
}
