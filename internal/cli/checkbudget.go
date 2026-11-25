package cli

import (
	"context"
	"fmt"

	"github.com/pmlucero/retrace/internal/cassette"
	"github.com/pmlucero/retrace/internal/provider/anthropic"
)

// ExitBudget is the exit code of a check whose cost budget skipped at least
// one replay (spec §9.9.8).
const ExitBudget = 5

// reasonBudget is the reason of a replay skipped by the budget, as opposed to
// one skipped by an interruption (spec §9.9.5).
const reasonBudget = "budget"

// checkBudget tracks the estimated cost of a swap-model check (spec §9.9.6).
type checkBudget struct {
	max      float64            // --max-cost; 0 when not given
	known    bool               // false when the price of --model is unknown
	perRun   map[string]float64 // estimate of one replay, by source session
	total    float64            // estimate of the whole check
	spent    float64            // cost of the replays so far
	exceeded bool               // a replay would exceed max: the rest are skipped
}

// estimateEvents prices the tokens of a session's model_response events at
// the prices of model. The events carry the four token kinds that the deriver
// priced the recording from (spec §7.5), so the estimate uses the same
// source. Each response is priced on its own, because the price depends on
// the prompt length of one request; the sum equals pricing the summed tokens
// whenever the model has no prompt-length tiers. It reports false when the
// table cannot price a response.
func estimateEvents(model string, events []cassette.Event) (float64, bool) {
	var sum float64
	for _, e := range events {
		if e.Kind != "model_response" || !(billedTokens(e.TokensIn) || billedTokens(e.TokensOut)) {
			continue // not billed by tokens
		}
		// Cache writes are priced at the 5-minute rate: the TTL breakdown
		// is not stored in the events (spec §7.5).
		c := anthropic.Cost(model, &anthropic.Usage{
			InputTokens: orZero(e.TokensIn), OutputTokens: orZero(e.TokensOut),
			CacheReadInputTokens: e.TokensCacheRead, CacheCreationInputTokens: e.TokensCacheWrite,
		})
		if c == nil {
			return 0, false
		}
		sum += *c
	}
	return sum, true
}

func billedTokens(n *int64) bool { return n != nil && *n > 0 }

func orZero(n *int64) *int64 {
	if n == nil {
		return new(int64)
	}
	return n
}

// newCheckBudget estimates the check: the source sessions' tokens at the
// price of model, times repeat (spec §9.9.6).
func newCheckBudget(ctx context.Context, env *Env, corpus []cassette.Session, model string, repeat int, limit float64) (*checkBudget, error) {
	b := &checkBudget{max: limit, known: true, perRun: map[string]float64{}}
	if anthropic.Cost(model, &anthropic.Usage{InputTokens: new(int64), OutputTokens: new(int64)}) == nil {
		b.known = false
		return b, nil
	}
	for _, s := range corpus {
		src, err := cassette.OpenForRead(ctx, cassette.Path(env.Globals.Dir, s.ID))
		if err != nil {
			return nil, fmt.Errorf("opening session %q: %w", s.ID, err)
		}
		events, err := src.Events(ctx)
		_ = src.Close()
		if err != nil {
			return nil, fmt.Errorf("reading session %q: %w", s.ID, err)
		}
		est, ok := estimateEvents(model, events)
		if !ok {
			b.known = false
			return b, nil
		}
		b.perRun[s.ID] = est
		b.total += est * float64(repeat)
	}
	return b, nil
}

// refuses reports whether the estimate of the whole check exceeds the budget.
func (b *checkBudget) refuses() bool { return b.max > 0 && b.total > b.max }

// allows reports whether the replay of session id may start: the cost spent
// plus its estimate must not exceed the budget. Once one does not, no later
// replay starts.
func (b *checkBudget) allows(id string) bool {
	if b.max > 0 && (b.exceeded || b.spent+b.perRun[id] > b.max) {
		b.exceeded = true
		return false
	}
	return true
}

// record adds a finished replay to the cost spent. A replay with a response
// that has tokens but no cost counts with its estimate, so an unknown cost
// never lowers what was spent (spec §9.9.6).
func (b *checkBudget) record(id string, r replayOutcome) {
	spent := r.totals.CostUSD
	if r.totals.Unpriced > 0 {
		spent = max(spent, b.perRun[id])
	}
	b.spent += spent
}

// describe is the line that check prints on stderr before the first replay.
func (b *checkBudget) describe() string {
	if !b.known {
		return "estimated cost: unknown, the model is not in the price table (an estimate, not a cap on billing)"
	}
	s := fmt.Sprintf("estimated cost: $%.4f (an estimate, not a cap on billing)", b.total)
	if b.max > 0 {
		s += fmt.Sprintf(", budget $%.4f", b.max)
	}
	return s
}
