// Package summary computes a session's totals (spec §8.2) and formats them for
// the text outputs of diff, ls and show, so the three commands agree.
package summary

import (
	"fmt"
	"math"
	"strconv"
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
	// null cost is never guessed (spec §7.5), so it adds nothing here. It is
	// always a finite number (spec §8.2).
	CostUSD float64
	// Unpriced counts the model_response events that have tokens but a null
	// cost_usd. A response without tokens, such as an API error or a cassette
	// miss, was not billed by tokens, so it is not unpriced either.
	Unpriced int
	// Priced counts the model_response events whose cost_usd is known. It
	// tells a known sum of zero from no known sum at all.
	Priced int
	Models []string // distinct model values, in order of first appearance
	// Duration is ended_at - created_at, and nil while the session has not
	// ended.
	Duration      *time.Duration
	DroppedEvents int // from the session row (spec §7.7)
}

// Of computes the totals of session s from its events, which must be in seq
// order, as the cassette reads them: a cost that would make the sum not
// finite counts as null, so the order decides which one does (spec §8.2).
// Events of other sessions must not be passed: Of does not check either.
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
			// Only input and output tokens decide whether a response was
			// billed: any billed response has both, and cache tokens only
			// come with them, so checking the cache fields too would change
			// nothing.
			switch cost, ok := knownCost(e.CostUSD, t.CostUSD); {
			case ok:
				t.CostUSD = cost
				t.Priced++
			case positive(e.TokensIn) || positive(e.TokensOut):
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

// knownCost returns sum plus cost, and false when cost counts as null: when it
// is null, less than zero or not finite, which a valid cassette never holds,
// or when it would make the sum not finite (spec §8.2). The sum then stays a
// number that every output, JSON included, can show.
func knownCost(cost *float64, sum float64) (float64, bool) {
	if cost == nil || *cost < 0 || math.IsInf(*cost, 0) || math.IsNaN(*cost) {
		return sum, false
	}
	if next := sum + *cost; !math.IsInf(next, 0) {
		return next, true
	}
	return sum, false
}

func positive(n *int64) bool {
	return n != nil && *n > 0
}

// FormatCost renders the cost of t without a label: callers put "estimated"
// in the column header (spec §7.5). It shows the known sum with two decimals,
// "<$0.01" for a sum that would round to zero but is not, and the number of
// unpriced responses after it: "$0.18", "$0.18 + 2 unpriced". When no response
// is priced there is no known sum to show, so it is only "2 unpriced". A
// session with nothing priced or unpriced costs "$0.00".
func FormatCost(t Totals) string {
	if t.Unpriced > 0 && t.Priced == 0 {
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

// FormatTokens abbreviates a token count for the narrow columns of ls
// (spec §9.5). Outputs with room for the exact count, such as diff (spec
// §8.4), use FormatCount. It shows the count as is below
// 1,000, else rounded to one decimal with a k or M suffix ("950", "41.2k",
// "1.2M"). A value that rounds up to the next unit uses it, so 999,960 is
// "1.0M", not "1000.0k".
func FormatTokens(n int64) string {
	if n > -1000 && n < 1000 {
		return strconv.FormatInt(n, 10)
	}
	// Working on the magnitude as a float avoids negating math.MinInt64.
	sign, f := "", float64(n)
	if f < 0 {
		sign, f = "-", -f
	}
	if k := math.Round(f/100) / 10; k < 1000 {
		return fmt.Sprintf("%s%.1fk", sign, k)
	}
	return fmt.Sprintf("%s%.1fM", sign, math.Round(f/100_000)/10)
}

// FormatDuration renders d compactly: seconds with one decimal below a minute
// ("38.2s"), then whole seconds with zero-padded fields ("1m02s", "1h02m03s").
// The padding keeps the values of a column comparable at a glance. d is
// rounded first, so 59.96s is "1m00s", not "60.0s".
func FormatDuration(d time.Duration) string {
	if d < 0 {
		// Abs saturates instead of overflowing on the minimum duration.
		return "-" + FormatDuration(d.Abs())
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

// FormatCount renders n in full with comma thousands separators ("41,230"),
// for outputs that show exact token counts, such as diff (spec §8.4).
func FormatCount(n int64) string {
	digits := strconv.FormatInt(n, 10)
	var b strings.Builder
	if n < 0 {
		// Formatting before taking the sign off avoids negating math.MinInt64.
		b.WriteByte('-')
		digits = digits[1:]
	}
	for i, d := range digits {
		if i > 0 && (len(digits)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(d)
	}
	return b.String()
}
