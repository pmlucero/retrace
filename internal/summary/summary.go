// Package summary computes a session's totals (spec §8.2) and formats them for
// the text outputs of diff, ls and show, so the three commands agree.
package summary

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/pmlucero/retrace/internal/cassette"
)

// Totals summarizes a session (spec §8.2).
type Totals struct {
	Steps     int   // model_response events
	ToolCalls int   // tool_call events
	TokensIn  int64 // sum of tokens_in over model_response events
	TokensOut int64 // sum of tokens_out over model_response events
	// CostUSD is the sum of the known cost_usd of model_response events. A
	// null cost is never guessed (spec §7.5), so it adds nothing here and is
	// counted in Unpriced instead.
	CostUSD  float64
	Unpriced int      // model_response events whose cost_usd is null
	Models   []string // distinct model values, in order of first appearance
	// Duration is ended_at - created_at, and nil while the session has not
	// ended.
	Duration      *time.Duration
	DroppedEvents int // from the session row (spec §7.7)
}

// Priced is the number of model_response events whose cost is known.
func (t Totals) Priced() int {
	return t.Steps - t.Unpriced
}

// Of computes the totals of session s from its events. Events of other
// sessions must not be passed: Of does not check.
func Of(s cassette.Session, events []cassette.Event) Totals {
	t := Totals{DroppedEvents: s.DroppedEvents}
	seen := map[string]bool{}
	for _, e := range events {
		switch e.Kind {
		case "tool_call":
			t.ToolCalls++
		case "model_response":
			t.Steps++
			if e.TokensIn != nil {
				t.TokensIn += *e.TokensIn
			}
			if e.TokensOut != nil {
				t.TokensOut += *e.TokensOut
			}
			if e.CostUSD != nil {
				t.CostUSD += *e.CostUSD
			} else {
				t.Unpriced++
			}
			if e.Model != nil && !seen[*e.Model] {
				seen[*e.Model] = true
				t.Models = append(t.Models, *e.Model)
			}
		}
	}
	if s.EndedAt != nil {
		// Both times come from the wall clock, which can step back between
		// them. A negative duration would only confuse the reader.
		d := max(s.EndedAt.Sub(s.CreatedAt), 0)
		t.Duration = &d
	}
	return t
}

// FormatCost renders the cost of t without a label: callers put "estimated"
// in the column header (spec §7.5). It shows the known sum with two decimals,
// "<$0.01" for a sum that would round to zero but is not, and the number of
// unpriced responses after it: "$0.18", "$0.18 + 2 unpriced". When no response
// is priced there is no known sum to show, so it is only "2 unpriced". A
// session with no model_response costs "$0.00".
func FormatCost(t Totals) string {
	if t.Unpriced > 0 && t.Priced() == 0 {
		return fmt.Sprintf("%d unpriced", t.Unpriced)
	}
	sum := fmt.Sprintf("$%.2f", t.CostUSD)
	if t.CostUSD > 0 && t.CostUSD < 0.005 {
		sum = "<$0.01"
	}
	if t.Unpriced > 0 {
		return fmt.Sprintf("%s + %d unpriced", sum, t.Unpriced)
	}
	return sum
}

// FormatTokens abbreviates a token count for narrow columns: as is below
// 1,000, else rounded to one decimal with a k or M suffix ("950", "41.2k",
// "1.2M"). A value that rounds up to the next unit uses it, so 999,960 is
// "1.0M", not "1000.0k".
func FormatTokens(n int64) string {
	if n < 0 {
		return "-" + FormatTokens(-n)
	}
	if n < 1000 {
		return fmt.Sprintf("%d", n)
	}
	if k := math.Round(float64(n)/100) / 10; k < 1000 {
		return fmt.Sprintf("%.1fk", k)
	}
	return fmt.Sprintf("%.1fM", math.Round(float64(n)/100_000)/10)
}

// FormatDuration renders d compactly: seconds with one decimal below a minute
// ("38.2s"), then whole seconds with zero-padded fields ("1m02s", "1h02m03s").
// The padding keeps the values of a column comparable at a glance. d is
// rounded first, so 59.96s is "1m00s", not "60.0s".
func FormatDuration(d time.Duration) string {
	if d < 0 {
		return "-" + FormatDuration(-d)
	}
	if r := d.Round(100 * time.Millisecond); r < time.Minute {
		return fmt.Sprintf("%.1fs", r.Seconds())
	}
	d = d.Round(time.Second)
	h, m, s := int64(d/time.Hour), int64(d%time.Hour/time.Minute), int64(d%time.Minute/time.Second)
	var b strings.Builder
	if h > 0 {
		fmt.Fprintf(&b, "%dh%02dm", h, m)
	} else {
		fmt.Fprintf(&b, "%dm", m)
	}
	fmt.Fprintf(&b, "%02ds", s)
	return b.String()
}
