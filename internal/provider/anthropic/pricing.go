package anthropic

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"math"
)

// prices.json is the price table embedded in the binary (spec §7.5). Update
// it from the source it names, and set its version to the date it was checked.
//
//go:embed prices.json
var pricesJSON []byte

type priceTable struct {
	Version string        `json:"version"`
	Source  string        `json:"source"`
	Models  []modelPrices `json:"models"`
}

type modelPrices struct {
	IDs []string `json:"ids"`
	// Tiers are by prompt length, ascending. The last one may be unbounded.
	Tiers []priceTier `json:"tiers"`
	// Fast holds the fast mode rates, if the model has fast mode.
	Fast *rates `json:"fast"`
	// USMultiplier applies to every rate when inference_geo is "us". Zero
	// means the table has no US-only price for the model.
	USMultiplier float64 `json:"us_multiplier"`
}

type priceTier struct {
	// MaxPromptTokens is the longest prompt (input, cache read and cache
	// write tokens) the tier covers. Zero means no limit.
	MaxPromptTokens int64 `json:"max_prompt_tokens"`
	rates
}

// rates are in USD per million tokens.
type rates struct {
	Input        float64 `json:"input"`
	CacheWrite5m float64 `json:"cache_write_5m"`
	CacheWrite1h float64 `json:"cache_write_1h"`
	CacheRead    float64 `json:"cache_read"`
	Output       float64 `json:"output"`
}

var prices = mustLoadPrices(pricesJSON)

func mustLoadPrices(data []byte) priceIndex {
	p, err := loadPrices(data)
	if err != nil {
		panic(err) // a broken embedded table is a build defect; TestEmbeddedPriceTable catches it
	}
	return p
}

type priceIndex struct {
	version string
	byID    map[string]*modelPrices
}

func loadPrices(data []byte) (priceIndex, error) {
	var t priceTable
	if err := json.Unmarshal(data, &t); err != nil {
		return priceIndex{}, fmt.Errorf("parsing price table: %w", err)
	}
	if t.Version == "" {
		return priceIndex{}, fmt.Errorf("price table has no version")
	}
	idx := priceIndex{version: t.Version, byID: map[string]*modelPrices{}}
	for i := range t.Models {
		m := &t.Models[i]
		if len(m.Tiers) == 0 {
			return priceIndex{}, fmt.Errorf("price table: %v has no tiers", m.IDs)
		}
		for _, id := range m.IDs {
			if _, dup := idx.byID[id]; dup {
				return priceIndex{}, fmt.Errorf("price table: model %q listed twice", id)
			}
			idx.byID[id] = m
		}
	}
	return idx, nil
}

// PricingVersion identifies the embedded price table (spec §7.2, §7.5).
func PricingVersion() string { return prices.version }

// Cost estimates the cost in USD of one response by model with usage u (spec
// §7.5). It returns nil when the table cannot price it: an unknown model, no
// input or output count, or a price the table does not have (a service tier
// other than standard, an unknown speed or inference geography, or a prompt
// beyond the model's tiers). The cost covers tokens only.
func Cost(model string, u *Usage) *float64 {
	m := prices.byID[model]
	if m == nil || u == nil || u.InputTokens == nil || u.OutputTokens == nil {
		return nil
	}
	if u.ServiceTier != "" && u.ServiceTier != "standard" {
		return nil
	}
	read := deref(u.CacheReadInputTokens)
	write := deref(u.CacheCreationInputTokens)
	write5m, write1h := write, int64(0)
	if u.CacheCreation5m != nil || u.CacheCreation1h != nil {
		write5m, write1h = deref(u.CacheCreation5m), deref(u.CacheCreation1h)
	}
	prompt := *u.InputTokens + read + write

	var r *rates
	switch u.Speed {
	case "", "standard":
		for i := range m.Tiers {
			if t := &m.Tiers[i]; t.MaxPromptTokens == 0 || prompt <= t.MaxPromptTokens {
				r = &t.rates
				break
			}
		}
	case "fast":
		r = m.Fast
	}
	if r == nil {
		return nil
	}
	mult := 1.0
	switch u.InferenceGeo {
	case "", "global", "not_available":
	case "us":
		if m.USMultiplier == 0 {
			return nil
		}
		mult = m.USMultiplier
	default:
		return nil
	}

	micro := float64(*u.InputTokens)*r.Input + float64(*u.OutputTokens)*r.Output +
		float64(read)*r.CacheRead + float64(write5m)*r.CacheWrite5m + float64(write1h)*r.CacheWrite1h
	// Rounding to 1e-10 USD hides float noise (0.30000000000000004) without
	// losing anything a cost estimate can mean.
	cost := math.Round(micro*mult/1e6*1e10) / 1e10
	return &cost
}

func deref(n *int64) int64 {
	if n == nil {
		return 0
	}
	return *n
}
