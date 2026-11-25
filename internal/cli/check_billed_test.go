package cli

import (
	"testing"

	"github.com/pmlucero/retrace/internal/cassette"
)

// AC-39, AC-43: a replay's billed cost counts only its live responses, so
// responses answered from the cassette cost nothing and never reach the budget.
func TestBilledCountsOnlyLiveResponses(t *testing.T) {
	in, out := int64(10), int64(5)
	c1, c2 := 0.25, 0.5
	resp := func(prov string, cost *float64) cassette.Event {
		return cassette.Event{Kind: "model_response", Provenance: prov, TokensIn: &in, TokensOut: &out, CostUSD: cost}
	}
	events := []cassette.Event{
		resp("replayed", &c1),
		resp("live", &c2),
		resp("live", nil),
		{Kind: "tool_call", Provenance: "replayed"},
	}
	got := billed(cassette.Session{}, events)
	if got.CostUSD != 0.5 || got.Priced != 1 || got.Unpriced != 1 || got.ToolCalls != 1 {
		t.Errorf("billed = %+v, want cost 0.5, 1 priced, 1 unpriced", got)
	}
	all := billed(cassette.Session{}, events[:1])
	if all.CostUSD != 0 || all.Priced != 0 || all.Unpriced != 0 {
		t.Errorf("a replayed response was billed: %+v", all)
	}
	b := &checkBudget{max: 1, perRun: map[string]float64{"a": 0.5}}
	b.record("a", replayOutcome{totals: all})
	if b.spent != 0 {
		t.Errorf("spent %v after an all-replayed replay, want 0", b.spent)
	}
}
