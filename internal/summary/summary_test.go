package summary

import (
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
			want:    Totals{Steps: 1, TokensIn: 10, TokensOut: 2, CostUSD: 0.01, Models: []string{"m"}},
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
				Steps: 2, ToolCalls: 2, TokensIn: 3000, TokensOut: 150, CostUSD: 0.75,
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
			want: Totals{Steps: 4, TokensIn: 4, TokensOut: 4, CostUSD: 0.1875, Unpriced: 2, Models: []string{"a", "b"}},
		},
		{
			name:    "models in order of first appearance",
			session: cassette.Session{CreatedAt: created},
			events: []cassette.Event{
				response("z", 0, 0, nil),
				response("a", 0, 0, nil),
				response("z", 0, 0, nil),
				{Kind: "model_response"}, // error before a model was known
				response("m", 0, 0, nil),
			},
			want: Totals{Steps: 5, Unpriced: 5, Models: []string{"z", "a", "m"}},
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
		{"all priced", Totals{Steps: 3, CostUSD: 0.1849}, "$0.18"},
		{"some unpriced", Totals{Steps: 5, CostUSD: 0.18, Unpriced: 2}, "$0.18 + 2 unpriced"},
		{"none priced", Totals{Steps: 2, Unpriced: 2}, "2 unpriced"},
		{"priced at zero", Totals{Steps: 1}, "$0.00"},
		{"below a cent", Totals{Steps: 1, CostUSD: 0.0031}, "<$0.01"},
		{"below a cent, some unpriced", Totals{Steps: 2, CostUSD: 0.0031, Unpriced: 1}, "<$0.01 + 1 unpriced"},
		{"rounds up to a cent", Totals{Steps: 1, CostUSD: 0.006}, "$0.01"},
		{"large", Totals{Steps: 1, CostUSD: 1234.5}, "$1234.50"},
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
		{-41230, "-41.2k"},
	}
	for _, tt := range tests {
		if got := FormatTokens(tt.n); got != tt.want {
			t.Errorf("FormatTokens(%d) = %q, want %q", tt.n, got, tt.want)
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
	}
	for _, tt := range tests {
		if got := FormatDuration(tt.d); got != tt.want {
			t.Errorf("FormatDuration(%v) = %q, want %q", tt.d, got, tt.want)
		}
	}
}
