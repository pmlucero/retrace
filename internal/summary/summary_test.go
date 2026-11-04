package summary

import (
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/pmlucero/retrace/internal/cassette"
)

func ptr[T any](v T) *T { return &v }

func response(model string, in, out int64, cost *float64) cassette.Event {
	return cassette.Event{Kind: "model_response", Model: ptr(model), TokensIn: ptr(in), TokensOut: ptr(out), CostUSD: cost}
}

// AC-22: these are the totals that ls and show display (spec §8.2).
func TestOf(t *testing.T) {
	created := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	ended := created.Add(38200 * time.Millisecond)
	dur := 38200 * time.Millisecond
	zero := time.Duration(0)

	tests := []struct {
		name    string
		session cassette.Session
		events  []cassette.Event
		want    Totals
	}{
		{
			name:    "empty events",
			session: cassette.Session{CreatedAt: created, EndedAt: &ended},
			want:    Totals{Duration: &dur},
		},
		{
			name:    "no duration while recording",
			session: cassette.Session{Status: "recording", CreatedAt: created},
			events:  []cassette.Event{response("m", 10, 2, ptr(0.01))},
			want:    Totals{Steps: 1, TokensIn: 10, TokensOut: 2, CostUSD: 0.01, Priced: 1, Models: []string{"m"}},
		},
		{
			name:    "counts only model responses and tool calls",
			session: cassette.Session{CreatedAt: created, EndedAt: &ended, DroppedEvents: 3},
			events: []cassette.Event{
				{Kind: "user_message"},
				response("a", 1000, 100, ptr(0.25)),
				{Kind: "tool_use", Tool: ptr("search")},
				{Kind: "tool_call", Tool: ptr("search"), TokensIn: ptr(int64(999))},
				{Kind: "tool_result", Tool: ptr("search")},
				{Kind: "tool_call", Tool: ptr("cancel")},
				{Kind: "mcp_message"},
				{Kind: "divergence"},
				response("a", 2000, 50, ptr(0.5)),
			},
			want: Totals{
				Steps: 2, ToolCalls: 2, TokensIn: 3000, TokensOut: 150, CostUSD: 0.75, Priced: 2,
				Models: []string{"a"}, Duration: &dur, DroppedEvents: 3,
			},
		},
		{
			name:    "null costs counted as unpriced, never added",
			session: cassette.Session{CreatedAt: created},
			events: []cassette.Event{
				response("a", 1, 1, ptr(0.125)),
				response("b", 1, 1, nil),
				response("a", 1, 1, ptr(0.0625)),
				response("b", 1, 1, nil),
			},
			want: Totals{Steps: 4, TokensIn: 4, TokensOut: 4, CostUSD: 0.1875, Unpriced: 2, Priced: 2, Models: []string{"a", "b"}},
		},
		{
			name:    "responses without tokens are not unpriced",
			session: cassette.Session{CreatedAt: created},
			events: []cassette.Event{
				{Kind: "model_response", Status: "error"},                                                    // API error: no usage
				{Kind: "model_response", Status: "divergence", TokensIn: ptr(int64(0))},                      // cassette miss
				{Kind: "model_response", Model: ptr("a"), TokensIn: ptr(int64(0)), TokensOut: ptr(int64(0))}, // zero usage
				{Kind: "model_response", Model: ptr("a"), TokensIn: ptr(int64(5))},                           // input only
				{Kind: "model_response", Model: ptr("a"), TokensOut: ptr(int64(7))},
				{Kind: "model_response", Model: ptr("a"), CostUSD: ptr(0.5)}, // a known cost counts even without tokens                          // output only
			},
			want: Totals{Steps: 6, TokensIn: 5, TokensOut: 7, CostUSD: 0.5, Unpriced: 2, Priced: 1, Models: []string{"a"}},
		},
		{
			name:    "models in order of first appearance",
			session: cassette.Session{CreatedAt: created},
			events: []cassette.Event{
				response("z", 1, 1, nil),
				response("a", 1, 1, nil),
				response("z", 1, 1, nil),
				{Kind: "model_response"}, // error before a model was known
				response("m", 1, 1, nil),
			},
			want: Totals{Steps: 5, TokensIn: 4, TokensOut: 4, Unpriced: 4, Models: []string{"z", "a", "m"}},
		},
		{
			// AC-22: spec §8.2 counts an invalid cost as null, so the known
			// sum stays a finite number.
			name:    "invalid costs count as null",
			session: cassette.Session{CreatedAt: created},
			events: []cassette.Event{
				response("a", 1, 1, ptr(-0.5)),
				response("a", 1, 1, ptr(math.Inf(1))),
				response("a", 1, 1, ptr(math.Inf(-1))),
				response("a", 1, 1, ptr(math.NaN())),
				response("a", 1, 1, ptr(0.25)),                     // valid after invalid ones
				{Kind: "model_response", CostUSD: ptr(math.NaN())}, // no tokens: neither priced nor unpriced
			},
			want: Totals{Steps: 6, TokensIn: 5, TokensOut: 5, CostUSD: 0.25, Unpriced: 4, Priced: 1, Models: []string{"a"}},
		},
		{
			name:    "negative zero is a known cost",
			session: cassette.Session{CreatedAt: created},
			events:  []cassette.Event{response("a", 1, 1, ptr(math.Copysign(0, -1)))},
			want:    Totals{Steps: 1, TokensIn: 1, TokensOut: 1, Priced: 1, Models: []string{"a"}},
		},
		{
			// AC-22: adding in seq order, the cost that would overflow the
			// sum counts as null, and later costs still add (1 is lost in the
			// rounding of so large a sum).
			name:    "cost that overflows the sum counts as null",
			session: cassette.Session{CreatedAt: created},
			events: []cassette.Event{
				response("a", 1, 1, ptr(math.MaxFloat64)),
				response("a", 1, 1, ptr(math.MaxFloat64)),
				response("a", 1, 1, ptr(1.0)),
			},
			want: Totals{Steps: 3, TokensIn: 3, TokensOut: 3, CostUSD: math.MaxFloat64, Unpriced: 1, Priced: 2, Models: []string{"a"}},
		},
		{
			name:    "clock stepped back",
			session: cassette.Session{CreatedAt: ended, EndedAt: &created},
			want:    Totals{Duration: &zero},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Of(tt.session, tt.events); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Of() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// AC-22
func TestFormatCost(t *testing.T) {
	tests := []struct {
		name string
		t    Totals
		want string
	}{
		{"no responses", Totals{}, "$0.00"},
		{"all priced", Totals{Steps: 3, Priced: 3, CostUSD: 0.1849}, "$0.18"},
		{"some unpriced", Totals{Steps: 5, Priced: 3, CostUSD: 0.18, Unpriced: 2}, "$0.18 + 2 unpriced"},
		{"none priced", Totals{Steps: 2, Unpriced: 2}, "2 unpriced"},
		{"only responses without tokens", Totals{Steps: 2}, "$0.00"},
		{"priced at zero", Totals{Steps: 1, Priced: 1}, "$0.00"},
		{"priced at zero, some unpriced", Totals{Steps: 2, Priced: 1, Unpriced: 1}, "$0.00 + 1 unpriced"},
		{"below a cent", Totals{Steps: 1, Priced: 1, CostUSD: 0.0031}, "<$0.01"},
		{"below a cent, some unpriced", Totals{Steps: 2, Priced: 1, CostUSD: 0.0031, Unpriced: 1}, "<$0.01 + 1 unpriced"},
		{"rounds up to a cent", Totals{Steps: 1, Priced: 1, CostUSD: 0.006}, "$0.01"},
		{"large", Totals{Steps: 1, Priced: 1, CostUSD: 1234.5}, "$1234.50"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := FormatCost(tt.t); got != tt.want {
				t.Errorf("FormatCost(%+v) = %q, want %q", tt.t, got, tt.want)
			}
		})
	}
}

// AC-22
func TestFormatTokens(t *testing.T) {
	tests := []struct {
		n    int64
		want string
	}{
		{0, "0"},
		{950, "950"},
		{999, "999"},
		{1000, "1.0k"},
		{3902, "3.9k"},
		{41230, "41.2k"},
		{999_949, "999.9k"},
		{999_950, "1.0M"},
		{1_234_567, "1.2M"},
		{25_000_000_000, "25000.0M"},
		{-950, "-950"},
		{-41230, "-41.2k"},
		{math.MinInt64, "-9223372036854.8M"},
	}
	for _, tt := range tests {
		if got := FormatTokens(tt.n); got != tt.want {
			t.Errorf("FormatTokens(%d) = %q, want %q", tt.n, got, tt.want)
		}
	}
}

func TestFormatCount(t *testing.T) {
	tests := []struct {
		n    int64
		want string
	}{
		{0, "0"},
		{999, "999"},
		{1000, "1,000"},
		{3902, "3,902"},
		{41230, "41,230"},
		{1_234_567, "1,234,567"},
		{-41230, "-41,230"},
		{-999, "-999"},
		{math.MinInt64, "-9,223,372,036,854,775,808"},
	}
	for _, tt := range tests {
		if got := FormatCount(tt.n); got != tt.want {
			t.Errorf("FormatCount(%d) = %q, want %q", tt.n, got, tt.want)
		}
	}
}

// AC-22
func TestFormatDuration(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{0, "0.0s"},
		{40 * time.Millisecond, "0.0s"},
		{50 * time.Millisecond, "0.1s"},
		{38200 * time.Millisecond, "38.2s"},
		{59940 * time.Millisecond, "59.9s"},
		{59960 * time.Millisecond, "1m00s"},
		{62 * time.Second, "1m02s"},
		{62600 * time.Millisecond, "1m03s"},
		{59*time.Minute + 59*time.Second + 600*time.Millisecond, "1h00m00s"},
		{time.Hour + 2*time.Minute + 3*time.Second, "1h02m03s"},
		{-1500 * time.Millisecond, "-1.5s"},
		{math.MinInt64, "-2562047h47m16s"}, // Round saturates at the maximum duration
	}
	for _, tt := range tests {
		if got := FormatDuration(tt.d); got != tt.want {
			t.Errorf("FormatDuration(%v) = %q, want %q", tt.d, got, tt.want)
		}
	}
}
