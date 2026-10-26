package anthropic

import (
	"testing"
)

func TestEmbeddedPriceTable(t *testing.T) {
	p, err := loadPrices(pricesJSON)
	if err != nil {
		t.Fatal(err)
	}
	if p.version != PricingVersion() || p.version == "" {
		t.Errorf("version %q", p.version)
	}
	for id, m := range p.byID {
		for i, tier := range m.Tiers {
			if tier.Input <= 0 || tier.Output <= 0 || tier.CacheRead <= 0 || tier.CacheWrite5m <= 0 || tier.CacheWrite1h <= 0 {
				t.Errorf("%s tier %d has a missing rate: %+v", id, i, tier.rates)
			}
			if last := i == len(m.Tiers)-1; !last && tier.MaxPromptTokens == 0 {
				t.Errorf("%s tier %d is unbounded but not last", id, i)
			}
		}
	}
}

func TestLoadPricesRejectsBrokenTables(t *testing.T) {
	for _, data := range []string{
		`nope`,
		`{"models":[]}`,
		`{"version":"v","models":[{"ids":["a"],"tiers":[]}]}`,
		`{"version":"v","models":[{"ids":["a"],"tiers":[{"input":1}]},{"ids":["a"],"tiers":[{"input":1}]}]}`,
	} {
		if _, err := loadPrices([]byte(data)); err == nil {
			t.Errorf("loadPrices(%s) succeeded", data)
		}
	}
}

func TestCost(t *testing.T) {
	base := func() *Usage { return &Usage{InputTokens: i64(1_000_000), OutputTokens: i64(1_000_000)} }
	with := func(f func(*Usage)) *Usage { u := base(); f(u); return u }

	tests := []struct {
		name  string
		model string
		usage *Usage
		want  *float64 // nil: no estimate
	}{
		{name: "input and output", model: "claude-opus-5-5", usage: base(), want: ptr(24)},
		{name: "cache read and default 5m write", model: "claude-opus-5-5",
			usage: &Usage{InputTokens: i64(0), OutputTokens: i64(0), CacheReadInputTokens: i64(1_000_000), CacheCreationInputTokens: i64(1_000_000)},
			want:  ptr(0.2 + 5)},
		{name: "cache writes by TTL", model: "claude-sonnet-4-6",
			usage: &Usage{InputTokens: i64(0), OutputTokens: i64(0), CacheCreationInputTokens: i64(3_000_000),
				CacheCreation5m: i64(1_000_000), CacheCreation1h: i64(2_000_000)},
			want: ptr(3.75 + 12)},
		{name: "small counts", model: "claude-haiku-4-5-20251001",
			usage: &Usage{InputTokens: i64(1234), OutputTokens: i64(56)}, want: ptr(0.001234 + 0.00028)},
		{name: "haiku 5.5 short prompt", model: "claude-haiku-5-5",
			usage: &Usage{InputTokens: i64(100_000), OutputTokens: i64(0)}, want: ptr(0.01)},
		{name: "haiku 5.5 long prompt counts cache reads", model: "claude-haiku-5-5",
			usage: &Usage{InputTokens: i64(100_000), OutputTokens: i64(0), CacheReadInputTokens: i64(1)},
			want:  ptr(0.05 + 0.00000005)},
		{name: "prompt beyond the table", model: "claude-sonnet-4-5",
			usage: &Usage{InputTokens: i64(200_001), OutputTokens: i64(0)}},
		{name: "fast mode", model: "claude-opus-5-5", usage: with(func(u *Usage) { u.Speed = "fast" }), want: ptr(48)},
		{name: "fast mode unknown for the model", model: "claude-opus-4-7", usage: with(func(u *Usage) { u.Speed = "fast" })},
		{name: "standard speed", model: "claude-opus-4-7", usage: with(func(u *Usage) { u.Speed = "standard" }), want: ptr(30)},
		{name: "unknown speed", model: "claude-opus-4-7", usage: with(func(u *Usage) { u.Speed = "warp" })},
		{name: "us inference", model: "claude-sonnet-5-5", usage: with(func(u *Usage) { u.InferenceGeo = "us" }), want: ptr(13.2)},
		{name: "global inference", model: "claude-sonnet-5-5", usage: with(func(u *Usage) { u.InferenceGeo = "global" }), want: ptr(12)},
		// Within the model's prompt tier, so it is the geography that has no price.
		{name: "us inference not priced for the model", model: "claude-haiku-4-5",
			usage: &Usage{InputTokens: i64(10), OutputTokens: i64(10), InferenceGeo: "us"}},
		{name: "unknown geography", model: "claude-sonnet-5-5", usage: with(func(u *Usage) { u.InferenceGeo = "eu" })},
		{name: "standard tier", model: "claude-sonnet-5-5", usage: with(func(u *Usage) { u.ServiceTier = "standard" }), want: ptr(12)},
		{name: "priority tier", model: "claude-sonnet-5-5", usage: with(func(u *Usage) { u.ServiceTier = "priority" })},
		{name: "unknown model", model: "claude-test", usage: base()},
		{name: "dated id of a model listed by alias only", model: "claude-opus-5-5-20260101", usage: base()},
		{name: "no usage", model: "claude-opus-5-5"},
		{name: "no output count", model: "claude-opus-5-5", usage: &Usage{InputTokens: i64(1)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Cost(tt.model, tt.usage)
			switch {
			case tt.want == nil && got != nil:
				t.Errorf("Cost = %v, want no estimate", *got)
			case tt.want != nil && got == nil:
				t.Errorf("Cost = nil, want %v", *tt.want)
			case tt.want != nil && !near(*got, *tt.want):
				t.Errorf("Cost = %v, want %v", *got, *tt.want)
			}
		})
	}
}

func ptr(f float64) *float64 { return &f }

func near(a, b float64) bool {
	d := a - b
	return d < 1e-9 && d > -1e-9
}
