package cli

import (
	"encoding/json"
	"io"

	"github.com/pmlucero/retrace/internal/summary"
)

// checkFormatVersion is the format_version of `check --format json`. A change
// that is not backward compatible increments it and adds a schema file next
// to docs/schema/check.v1.json (spec §9.9.7, §8.4).
const checkFormatVersion = 1

// checkDocument is the document of `check --format json`. Its schema is
// docs/schema/check.v1.json, so a field added here goes there too: the
// schema is open for consumers, but TestCheckJSON rejects undeclared members.
type checkDocument struct {
	FormatVersion int     `json:"format_version"`
	Mode          string  `json:"mode"`
	Model         *string `json:"model"`
	Repeat        int     `json:"repeat"`
	CorpusSize    int     `json:"corpus_size"`
	Interrupted   bool    `json:"interrupted"`
	// Out is null when nothing was written: the budget refused the check.
	Out *string `json:"out"`
	// Budget is null unless --max-cost was given.
	Budget     *checkBudgetJSON   `json:"budget"`
	Cost       checkCostJSON      `json:"cost"`
	Totals     checkTotalsJSON    `json:"totals"`
	DivergedAt []checkCorpusJSON  `json:"diverged_at"`
	Sessions   []checkSessionJSON `json:"sessions"`
}

// checkBudgetJSON is the budget of spec §9.9.6.
type checkBudgetJSON struct {
	Max      float64 `json:"max_cost_usd"`
	Estimate float64 `json:"estimate_usd"`
	Refused  bool    `json:"refused"`
	Skipped  int     `json:"skipped_replays"`
}

// checkCostJSON is a cost as spec §8.2 gives it: the sum of the known costs
// and the number of responses with no known cost.
type checkCostJSON struct {
	CostUSD           float64 `json:"cost_usd"`
	UnpricedResponses int     `json:"unpriced_responses"`
}

func costJSON(t summary.Totals) checkCostJSON {
	return checkCostJSON{CostUSD: t.CostUSD, UnpricedResponses: t.Unpriced}
}

// checkCountsJSON counts something by outcome. Every outcome has a member,
// zeros included.
type checkCountsJSON struct {
	Diverged  int `json:"diverged"`
	Error     int `json:"error"`
	Skipped   int `json:"skipped"`
	Identical int `json:"identical"`
}

func countsJSON(n func(string) int) checkCountsJSON {
	return checkCountsJSON{Diverged: n(outcomeDiverged), Error: n(outcomeError), Skipped: n(outcomeSkipped), Identical: n(outcomeIdentical)}
}

type checkTotalsJSON struct {
	Sessions checkCountsJSON `json:"sessions"`
	Replays  checkCountsJSON `json:"replays"`
}

// checkCorpusJSON is the sessions that have a divergence point of one event
// kind and tool. Tool is null for an event without one.
type checkCorpusJSON struct {
	Kind     string  `json:"kind"`
	Tool     *string `json:"tool"`
	Sessions int     `json:"sessions"`
}

// checkPointJSON is the replays of one session that diverged at one step.
type checkPointJSON struct {
	Step    int     `json:"step"`
	Kind    string  `json:"kind"`
	Tool    *string `json:"tool"`
	Replays int     `json:"replays"`
}

type checkSessionJSON struct {
	ID               string           `json:"id"`
	Outcome          string           `json:"outcome"`
	Counts           checkCountsJSON  `json:"counts"`
	DivergencePoints []checkPointJSON `json:"divergence_points"`
	// TextDifferences are the steps whose texts differ and were not compared.
	TextDifferences []int             `json:"text_differences"`
	Replays         []checkReplayJSON `json:"replays"`
}

type checkReplayJSON struct {
	ID         string         `json:"id"`
	Outcome    string         `json:"outcome"`
	Reason     *string        `json:"reason"`
	Cost       checkCostJSON  `json:"cost"`
	Divergence *diffPointJSON `json:"divergence"`
}

func toolPtr(tool string) *string {
	if tool == "" {
		return nil
	}
	return &tool
}

// writeJSON writes the JSON output (spec §9.9.7): the same information as
// writeText, with every count, and the values of the cassettes unescaped.
func (r checkReport) writeJSON(w io.Writer) error {
	sessions, replays, cost := r.tally()
	doc := checkDocument{
		FormatVersion: checkFormatVersion,
		Mode:          r.mode,
		Repeat:        r.repeat,
		CorpusSize:    len(r.sessions),
		Interrupted:   r.interrupted,
		Cost:          costJSON(cost),
		Totals: checkTotalsJSON{
			Sessions: countsJSON(func(o string) int { return sessions[o] }),
			Replays:  countsJSON(func(o string) int { return replays[o] }),
		},
		DivergedAt: []checkCorpusJSON{},
		Sessions:   []checkSessionJSON{},
	}
	if r.model != "" {
		doc.Model = &r.model
	}
	if r.out != "" {
		doc.Out = &r.out
	}
	if r.budgetMax > 0 {
		doc.Budget = &checkBudgetJSON{Max: r.budgetMax, Estimate: r.budgetEstimate, Refused: r.budgetRefused, Skipped: r.budgetSkipped()}
	}
	for _, g := range r.divergedGroups() {
		doc.DivergedAt = append(doc.DivergedAt, checkCorpusJSON{Kind: g.kind, Tool: toolPtr(g.tool), Sessions: g.sessions})
	}
	for _, s := range r.sessions {
		sj := checkSessionJSON{
			ID: s.id, Outcome: s.outcome(), Counts: countsJSON(s.count),
			DivergencePoints: []checkPointJSON{}, TextDifferences: s.textSteps(), Replays: []checkReplayJSON{},
		}
		if sj.TextDifferences == nil {
			sj.TextDifferences = []int{}
		}
		for _, g := range s.pointGroups() {
			sj.DivergencePoints = append(sj.DivergencePoints, checkPointJSON{Step: g.step, Kind: g.kind, Tool: toolPtr(g.tool), Replays: g.replays})
		}
		for _, rep := range s.replays {
			rj := checkReplayJSON{ID: rep.id, Outcome: rep.outcome, Cost: costJSON(rep.totals), Divergence: pointJSON(rep.point)}
			if rep.reason != "" {
				rj.Reason = &rep.reason
			}
			sj.Replays = append(sj.Replays, rj)
		}
		doc.Sessions = append(doc.Sessions, sj)
	}
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	return enc.Encode(doc)
}
