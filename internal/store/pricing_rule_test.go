package store_test

import (
	"errors"
	"math"
	"testing"
	"time"

	"lax/internal/store"
)

func int64Ptr(v int64) *int64 { return &v }

func approxEqual(t *testing.T, got *float64, want float64, label string) {
	t.Helper()
	if got == nil {
		t.Fatalf("%s = nil, want %v", label, want)
	}
	if math.Abs(*got-want) > 1e-9 {
		t.Fatalf("%s = %v, want %v", label, *got, want)
	}
}

func TestValidatePricingRule(t *testing.T) {
	validRule := &store.PricingRule{
		Version: 1,
		Tiers: &store.TieredPricing{
			Mode:  store.TierModeProgressive,
			Basis: store.TierBasisTotalInput,
			Brackets: []store.PriceTier{
				{MaxTokens: int64Ptr(32000), Prices: store.PriceSet{In: floatPtr(1.2), Out: floatPtr(6.0)}},
				{MaxTokens: nil, Prices: store.PriceSet{In: floatPtr(2.4), Out: floatPtr(12.0)}},
			},
		},
		Schedule: &store.TimeSchedulePricing{
			Timezone: "Asia/Shanghai",
			Windows: []store.TimeWindow{
				{
					Name:       "offpeak",
					Days:       []int{0, 6},
					Start:      "22:00",
					End:        "06:00",
					Multiplier: floatPtr(0.5),
				},
				{
					Name:     "allday-sun",
					Days:     []int{0},
					Start:    "00:00",
					End:      "24:00",
					Override: &store.PriceSet{In: floatPtr(0.8)},
				},
			},
		},
	}
	if err := store.ValidatePricingRule(validRule); err != nil {
		t.Fatalf("valid rule failed validation: %v", err)
	}

	cases := []struct {
		name    string
		rule    *store.PricingRule
		wantErr error
	}{
		{name: "nil rule", rule: nil},
		{
			name: "bad mode",
			rule: &store.PricingRule{Tiers: &store.TieredPricing{
				Mode:     "bad",
				Brackets: []store.PriceTier{{Prices: store.PriceSet{In: floatPtr(1)}}},
			}},
		},
		{
			name: "bad basis",
			rule: &store.PricingRule{Tiers: &store.TieredPricing{
				Basis:    "bad",
				Brackets: []store.PriceTier{{Prices: store.PriceSet{In: floatPtr(1)}}},
			}},
		},
		{
			name: "empty brackets",
			rule: &store.PricingRule{Tiers: &store.TieredPricing{}},
		},
		{
			name: "nil max_tokens not last",
			rule: &store.PricingRule{Tiers: &store.TieredPricing{
				Brackets: []store.PriceTier{
					{MaxTokens: nil, Prices: store.PriceSet{In: floatPtr(1)}},
					{MaxTokens: int64Ptr(1000), Prices: store.PriceSet{In: floatPtr(2)}},
				},
			}},
		},
		{
			name: "non-increasing max_tokens",
			rule: &store.PricingRule{Tiers: &store.TieredPricing{
				Brackets: []store.PriceTier{
					{MaxTokens: int64Ptr(1000), Prices: store.PriceSet{In: floatPtr(1)}},
					{MaxTokens: int64Ptr(1000), Prices: store.PriceSet{In: floatPtr(2)}},
				},
			}},
		},
		{
			name: "negative tier price",
			rule: &store.PricingRule{Tiers: &store.TieredPricing{
				Brackets: []store.PriceTier{{Prices: store.PriceSet{In: floatPtr(-1)}}},
			}},
			wantErr: store.ErrNegativePrice,
		},
		{
			name: "invalid timezone",
			rule: &store.PricingRule{Schedule: &store.TimeSchedulePricing{
				Timezone: "Mars/Olympus",
				Windows:  []store.TimeWindow{{Name: "w1", Start: "00:00", End: "01:00", Multiplier: floatPtr(0.5)}},
			}},
		},
		{
			name: "duplicate window name",
			rule: &store.PricingRule{Schedule: &store.TimeSchedulePricing{
				Windows: []store.TimeWindow{
					{Name: "w1", Start: "00:00", End: "01:00", Multiplier: floatPtr(0.5)},
					{Name: "w1", Start: "01:00", End: "02:00", Multiplier: floatPtr(0.5)},
				},
			}},
		},
		{
			name: "empty window name",
			rule: &store.PricingRule{Schedule: &store.TimeSchedulePricing{
				Windows: []store.TimeWindow{{Name: "  ", Start: "00:00", End: "01:00", Multiplier: floatPtr(0.5)}},
			}},
		},
		{
			name: "invalid day",
			rule: &store.PricingRule{Schedule: &store.TimeSchedulePricing{
				Windows: []store.TimeWindow{{Name: "w1", Days: []int{7}, Start: "00:00", End: "01:00", Multiplier: floatPtr(0.5)}},
			}},
		},
		{
			name: "start 24:00 invalid",
			rule: &store.PricingRule{Schedule: &store.TimeSchedulePricing{
				Windows: []store.TimeWindow{{Name: "w1", Start: "24:00", End: "01:00", Multiplier: floatPtr(0.5)}},
			}},
		},
		{
			name: "equal start and end",
			rule: &store.PricingRule{Schedule: &store.TimeSchedulePricing{
				Windows: []store.TimeWindow{{Name: "w1", Start: "09:00", End: "09:00", Multiplier: floatPtr(0.5)}},
			}},
		},
		{
			name: "missing multiplier and override",
			rule: &store.PricingRule{Schedule: &store.TimeSchedulePricing{
				Windows: []store.TimeWindow{{Name: "w1", Start: "00:00", End: "01:00"}},
			}},
		},
		{
			name: "negative multiplier",
			rule: &store.PricingRule{Schedule: &store.TimeSchedulePricing{
				Windows: []store.TimeWindow{{Name: "w1", Start: "00:00", End: "01:00", Multiplier: floatPtr(-0.5)}},
			}},
			wantErr: store.ErrNegativePrice,
		},
		{
			name: "NaN price in tier",
			rule: &store.PricingRule{Tiers: &store.TieredPricing{
				Brackets: []store.PriceTier{{Prices: store.PriceSet{In: floatPtr(math.NaN())}}},
			}},
		},
		{
			name: "Inf price in override",
			rule: &store.PricingRule{Schedule: &store.TimeSchedulePricing{
				Windows: []store.TimeWindow{{
					Name: "w1", Start: "00:00", End: "01:00",
					Override: &store.PriceSet{Out: floatPtr(math.Inf(1))},
				}},
			}},
		},
		{
			name: "NaN multiplier",
			rule: &store.PricingRule{Schedule: &store.TimeSchedulePricing{
				Windows: []store.TimeWindow{{Name: "w1", Start: "00:00", End: "01:00", Multiplier: floatPtr(math.NaN())}},
			}},
		},
		{
			name: "Inf multiplier",
			rule: &store.PricingRule{Schedule: &store.TimeSchedulePricing{
				Windows: []store.TimeWindow{{Name: "w1", Start: "00:00", End: "01:00", Multiplier: floatPtr(math.Inf(-1))}},
			}},
		},
		{
			name: "negative tier multiplier",
			rule: &store.PricingRule{Tiers: &store.TieredPricing{
				Brackets: []store.PriceTier{{Multiplier: floatPtr(-2)}},
			}},
			wantErr: store.ErrNegativePrice,
		},
		{
			name: "NaN tier multiplier",
			rule: &store.PricingRule{Tiers: &store.TieredPricing{
				Brackets: []store.PriceTier{{Multiplier: floatPtr(math.NaN())}},
			}},
		},
		{
			name: "Inf tier multiplier",
			rule: &store.PricingRule{Tiers: &store.TieredPricing{
				Brackets: []store.PriceTier{{Multiplier: floatPtr(math.Inf(1))}},
			}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := store.ValidatePricingRule(tc.rule)
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want errors.Is(_, %v)", err, tc.wantErr)
			}
		})
	}
}

func TestCalculateCost_AllIn_TotalVsUncachedBasis(t *testing.T) {
	base := store.ModelPrices{In: floatPtr(1.0), Out: floatPtr(2.0)}
	ruleTotal := &store.PricingRule{
		Version: 1,
		Tiers: &store.TieredPricing{
			Mode:  store.TierModeAllIn,
			Basis: store.TierBasisTotalInput,
			Brackets: []store.PriceTier{
				{MaxTokens: int64Ptr(100_000), Prices: store.PriceSet{In: floatPtr(1.0), Out: floatPtr(2.0)}},
				{MaxTokens: nil, Prices: store.PriceSet{In: floatPtr(2.0), Out: floatPtr(4.0)}},
			},
		},
	}

	// 60k uncached + 50k cache_read = 110k total_input -> hits tier 1 (In=2, Out=4, CacheRead falls back to 2)
	in := store.CostInput{
		InTokensUncached: 60_000,
		CacheRead:        50_000,
		OutTokens:        10_000,
	}
	bd, err := store.CalculateCost(base, ruleTotal, in)
	if err != nil {
		t.Fatalf("CalculateCost: %v", err)
	}
	if bd.TierIndex != 1 || bd.BasisTokens != 110_000 {
		t.Fatalf("unexpected tier: index=%d basis=%d", bd.TierIndex, bd.BasisTokens)
	}
	// 60k*2/1e6 + 50k*2/1e6 + 10k*4/1e6 = 0.12 + 0.10 + 0.04 = 0.26
	approxEqual(t, bd.TotalCost, 0.26, "all_in total_input")

	// Same input under uncached_input basis -> 60k <= 100k -> hits tier 0 (In=1, Out=2, CacheRead=1)
	ruleUncached := &store.PricingRule{
		Version: 1,
		Tiers: &store.TieredPricing{
			Mode:     store.TierModeAllIn,
			Basis:    store.TierBasisUncachedInput,
			Brackets: ruleTotal.Tiers.Brackets,
		},
	}
	bd2, err := store.CalculateCost(base, ruleUncached, in)
	if err != nil {
		t.Fatalf("CalculateCost uncached: %v", err)
	}
	if bd2.TierIndex != 0 || bd2.BasisTokens != 60_000 {
		t.Fatalf("unexpected tier: index=%d basis=%d", bd2.TierIndex, bd2.BasisTokens)
	}
	// 60k*1/1e6 + 50k*1/1e6 + 10k*2/1e6 = 0.06 + 0.05 + 0.02 = 0.13
	approxEqual(t, bd2.TotalCost, 0.13, "all_in uncached_input")
}

func TestCalculateCost_Progressive(t *testing.T) {
	base := store.ModelPrices{In: floatPtr(1.0), Out: floatPtr(2.0), CacheRead: floatPtr(0.5)}
	rule := &store.PricingRule{
		Version: 1,
		Tiers: &store.TieredPricing{
			Mode:  store.TierModeProgressive,
			Basis: store.TierBasisTotalInput,
			Brackets: []store.PriceTier{
				{MaxTokens: int64Ptr(100_000), Prices: store.PriceSet{In: floatPtr(1.0), Out: floatPtr(2.0)}},
				{MaxTokens: int64Ptr(200_000), Prices: store.PriceSet{In: floatPtr(2.0), Out: floatPtr(4.0)}},
				{MaxTokens: nil, Prices: store.PriceSet{In: floatPtr(3.0), Out: floatPtr(6.0)}},
			},
		},
	}

	// CacheRead=80k -> offset=80k. Uncached=150k -> [80k, 230k):
	//   [80k, 100k): 20k @ 1.0 = 0.02
	//   [100k, 200k): 100k @ 2.0 = 0.20
	//   [200k, 230k): 30k @ 3.0 = 0.09
	// CacheRead: 80k @ 0.5 = 0.04
	// Out: 10k @ 6.0 (highest hit tier 2) = 0.06
	// ReasoningExtra: 5k @ 6.0 = 0.03
	// Total = 0.44
	bd, err := store.CalculateCost(base, rule, store.CostInput{
		InTokensUncached:     150_000,
		CacheRead:            80_000,
		OutTokens:            10_000,
		ReasoningTokensExtra: 5_000,
	})
	if err != nil {
		t.Fatalf("CalculateCost progressive: %v", err)
	}
	if bd.TierIndex != 2 {
		t.Fatalf("TierIndex = %d, want 2", bd.TierIndex)
	}
	approxEqual(t, bd.TotalCost, 0.44, "progressive total_input")
}

func TestCalculateCost_ProgressiveCacheFallbackAndFiniteOverflow(t *testing.T) {
	// 1. Progressive tier overrides In (with CacheRead/CacheWrite nil -> fallback to tier's In)
	//    and explicitly overrides CacheWrite.
	base := store.ModelPrices{In: floatPtr(1.0), Out: floatPtr(2.0)}
	rule := &store.PricingRule{
		Version: 1,
		Tiers: &store.TieredPricing{
			Mode:  store.TierModeProgressive,
			Basis: store.TierBasisTotalInput,
			Brackets: []store.PriceTier{
				{MaxTokens: int64Ptr(100_000), Prices: store.PriceSet{In: floatPtr(1.0), Out: floatPtr(2.0)}},
				{MaxTokens: nil, Prices: store.PriceSet{In: floatPtr(3.0), Out: floatPtr(6.0), CacheWrite: floatPtr(4.0)}},
			},
		},
	}
	// CacheRead=60k, CacheWrite=60k -> offset=120k (hits tier 1).
	// Uncached=50k -> [120k, 170k) all in tier 1 @ 3.0 = 0.15
	// CacheRead=60k @ tier 1 In fallback 3.0 = 0.18
	// CacheWrite=60k @ tier 1 CacheWrite 4.0 = 0.24
	// Out=10k @ tier 1 Out 6.0 = 0.06
	// Total = 0.63
	bd, err := store.CalculateCost(base, rule, store.CostInput{
		InTokensUncached: 50_000,
		CacheRead:        60_000,
		CacheWrite:       60_000,
		OutTokens:        10_000,
	})
	if err != nil {
		t.Fatalf("CalculateCost progressive cache: %v", err)
	}
	if bd.TierIndex != 1 {
		t.Fatalf("TierIndex = %d, want 1", bd.TierIndex)
	}
	approxEqual(t, bd.EffectivePrices.In, 3.0, "eff.In")
	approxEqual(t, bd.EffectivePrices.CacheRead, 3.0, "eff.CacheRead fallback to tier In")
	approxEqual(t, bd.EffectivePrices.CacheWrite, 4.0, "eff.CacheWrite")
	approxEqual(t, bd.TotalCost, 0.63, "progressive cache total")

	// 2. Finite MaxTokens only: exceeding all brackets aligns with all_in (TierIndex = -1, TierLabel = "", Out/Cache fall back to base).
	finiteRule := &store.PricingRule{
		Version: 1,
		Tiers: &store.TieredPricing{
			Mode:  store.TierModeProgressive,
			Basis: store.TierBasisUncachedInput,
			Brackets: []store.PriceTier{
				{MaxTokens: int64Ptr(100_000), Prices: store.PriceSet{In: floatPtr(5.0), Out: floatPtr(10.0)}},
			},
		},
	}
	// Uncached=150k > 100k:
	//   [0, 100k): 100k @ 5.0 = 0.50
	//   [100k, 150k): 50k @ base.In (1.0) = 0.05
	//   Out=100k @ base.Out (2.0) = 0.20
	//   Total = 0.75, TierIndex = -1, TierLabel = ""
	bdOver, err := store.CalculateCost(base, finiteRule, store.CostInput{
		InTokensUncached: 150_000,
		OutTokens:        100_000,
	})
	if err != nil {
		t.Fatalf("CalculateCost finite overflow: %v", err)
	}
	if bdOver.TierIndex != -1 || bdOver.TierLabel != "" {
		t.Fatalf("TierIndex=%d TierLabel=%q, want -1 and empty", bdOver.TierIndex, bdOver.TierLabel)
	}
	approxEqual(t, bdOver.TotalCost, 0.75, "progressive finite overflow")
}

func TestCalculateCost_TierMultiplier_AllInAndProgressive(t *testing.T) {
	base := store.ModelPrices{In: floatPtr(2.0), Out: floatPtr(4.0)}

	// all_in: <=200k no multiplier (2/4), >200k multiplier=2 (4/8)
	allInRule := &store.PricingRule{
		Version: 1,
		Tiers: &store.TieredPricing{
			Mode:  store.TierModeAllIn,
			Basis: store.TierBasisTotalInput,
			Brackets: []store.PriceTier{
				{MaxTokens: int64Ptr(200_000)},
				{MaxTokens: nil, Multiplier: floatPtr(2.0)},
			},
		},
	}

	// <= 200k -> 2/4
	bdLow, err := store.CalculateCost(base, allInRule, store.CostInput{
		InTokensUncached: 100_000,
		CacheRead:        50_000,
		OutTokens:        50_000,
	})
	if err != nil {
		t.Fatalf("allIn low: %v", err)
	}
	approxEqual(t, bdLow.EffectivePrices.In, 2.0, "allIn low eff.In")
	approxEqual(t, bdLow.EffectivePrices.CacheRead, 2.0, "allIn low eff.CacheRead")
	approxEqual(t, bdLow.EffectivePrices.Out, 4.0, "allIn low eff.Out")
	// 100k*2 + 50k*2 + 50k*4 = 0.2 + 0.1 + 0.2 = 0.5
	approxEqual(t, bdLow.TotalCost, 0.5, "allIn low total")

	// > 200k -> 4/8
	bdHigh, err := store.CalculateCost(base, allInRule, store.CostInput{
		InTokensUncached: 200_000,
		CacheRead:        50_000,
		OutTokens:        50_000,
	})
	if err != nil {
		t.Fatalf("allIn high: %v", err)
	}
	approxEqual(t, bdHigh.EffectivePrices.In, 4.0, "allIn high eff.In")
	approxEqual(t, bdHigh.EffectivePrices.CacheRead, 4.0, "allIn high eff.CacheRead")
	approxEqual(t, bdHigh.EffectivePrices.Out, 8.0, "allIn high eff.Out")
	// 200k*4 + 50k*4 + 50k*8 = 0.8 + 0.2 + 0.4 = 1.4
	approxEqual(t, bdHigh.TotalCost, 1.4, "allIn high total")

	// Combined with 0.5x schedule -> stacks back to 2/4
	combinedRule := &store.PricingRule{
		Version: 1,
		Tiers:   allInRule.Tiers,
		Schedule: &store.TimeSchedulePricing{
			Timezone: "UTC",
			Windows: []store.TimeWindow{
				{Name: "half", Start: "00:00", End: "24:00", Multiplier: floatPtr(0.5)},
			},
		},
	}
	bdStack, err := store.CalculateCost(base, combinedRule, store.CostInput{
		TS:               1795186800,
		InTokensUncached: 200_000,
		CacheRead:        50_000,
		OutTokens:        50_000,
	})
	if err != nil {
		t.Fatalf("allIn + schedule: %v", err)
	}
	approxEqual(t, bdStack.EffectivePrices.In, 2.0, "stacked eff.In")
	approxEqual(t, bdStack.EffectivePrices.Out, 4.0, "stacked eff.Out")
	approxEqual(t, bdStack.TotalCost, 0.7, "stacked total")

	// progressive: <=200k @ 2.0, >200k @ 2.0*2 = 4.0; Out @ 4.0*2 = 8.0; then 0.5x schedule -> 1.0 / 2.0 / 4.0
	progRule := &store.PricingRule{
		Version: 1,
		Tiers: &store.TieredPricing{
			Mode:  store.TierModeProgressive,
			Basis: store.TierBasisUncachedInput,
			Brackets: []store.PriceTier{
				{MaxTokens: int64Ptr(200_000)},
				{MaxTokens: nil, Multiplier: floatPtr(2.0)},
			},
		},
		Schedule: combinedRule.Schedule,
	}
	// Uncached=300k:
	//   [0, 200k): 200k @ 2.0 * 0.5 = 1.0 -> 0.20
	//   [200k, 300k): 100k @ 2.0 * 2.0 * 0.5 = 2.0 -> 0.20
	//   Out=100k @ 4.0 * 2.0 * 0.5 = 4.0 -> 0.40
	//   Total = 0.80
	bdProg, err := store.CalculateCost(base, progRule, store.CostInput{
		TS:               1795186800,
		InTokensUncached: 300_000,
		OutTokens:        100_000,
	})
	if err != nil {
		t.Fatalf("progressive + tier multiplier + schedule: %v", err)
	}
	approxEqual(t, bdProg.EffectivePrices.Out, 4.0, "prog stacked eff.Out")
	approxEqual(t, bdProg.TotalCost, 0.80, "prog stacked total")
}

func TestCalculateCost_ScheduleCrossMidnightAndCombined(t *testing.T) {
	shanghai, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}

	base := store.ModelPrices{In: floatPtr(1.0), Out: floatPtr(2.0)}
	rule := &store.PricingRule{
		Version: 1,
		Tiers: &store.TieredPricing{
			Mode:  store.TierModeProgressive,
			Basis: store.TierBasisUncachedInput,
			Brackets: []store.PriceTier{
				{MaxTokens: int64Ptr(100_000), Prices: store.PriceSet{In: floatPtr(1.0), Out: floatPtr(2.0)}},
				{MaxTokens: nil, Prices: store.PriceSet{In: floatPtr(2.0), Out: floatPtr(4.0)}},
			},
		},
		Schedule: &store.TimeSchedulePricing{
			Timezone: "Asia/Shanghai",
			Windows: []store.TimeWindow{
				{
					// Friday (5) 22:00 through Saturday 06:00
					Name:       "fri-night",
					Days:       []int{5},
					Start:      "22:00",
					End:        "06:00",
					Multiplier: floatPtr(0.5),
				},
				{
					Name:     "sun-override",
					Days:     []int{0},
					Start:    "00:00",
					End:      "24:00",
					Override: &store.PriceSet{Out: floatPtr(10.0)},
					Multiplier: floatPtr(0.5),
				},
			},
		},
	}

	// 2026-11-20 is Friday.
	friNight := time.Date(2026, 11, 20, 23, 30, 0, 0, shanghai).Unix()
	satEarly := time.Date(2026, 11, 21, 2, 30, 0, 0, shanghai).Unix()
	satDay := time.Date(2026, 11, 21, 14, 0, 0, 0, shanghai).Unix()
	sunNoon := time.Date(2026, 11, 22, 12, 0, 0, 0, shanghai).Unix()

	in := store.CostInput{
		InTokensUncached: 150_000, // progressive: 100k*1 + 50k*2 = 0.20 before multiplier
		OutTokens:        100_000, // tier 1 out=4 -> 0.40 before multiplier
	}

	// Friday 23:30 -> matches fri-night (0.5x): (0.20 + 0.40) * 0.5 = 0.30
	in.TS = friNight
	bdFri, err := store.CalculateCost(base, rule, in)
	if err != nil {
		t.Fatalf("friNight: %v", err)
	}
	if bdFri.WindowName != "fri-night" {
		t.Fatalf("WindowName = %q, want fri-night", bdFri.WindowName)
	}
	approxEqual(t, bdFri.TotalCost, 0.30, "fri-night")

	// Saturday 02:30 -> still inside Friday's cross-midnight window!
	in.TS = satEarly
	bdSatEarly, err := store.CalculateCost(base, rule, in)
	if err != nil {
		t.Fatalf("satEarly: %v", err)
	}
	if bdSatEarly.WindowName != "fri-night" {
		t.Fatalf("WindowName = %q, want fri-night", bdSatEarly.WindowName)
	}
	approxEqual(t, bdSatEarly.TotalCost, 0.30, "sat-early")

	// Saturday 14:00 -> no window matched -> 0.60
	in.TS = satDay
	bdSatDay, err := store.CalculateCost(base, rule, in)
	if err != nil {
		t.Fatalf("satDay: %v", err)
	}
	if bdSatDay.WindowName != "" {
		t.Fatalf("WindowName = %q, want empty", bdSatDay.WindowName)
	}
	approxEqual(t, bdSatDay.TotalCost, 0.60, "sat-day")

	// Sunday 12:00 -> Override Out=10.0, then Multiplier 0.5 -> In: 0.20*0.5=0.10, Out: 100k*(10*0.5)/1e6=0.50 -> 0.60
	in.TS = sunNoon
	bdSun, err := store.CalculateCost(base, rule, in)
	if err != nil {
		t.Fatalf("sunNoon: %v", err)
	}
	if bdSun.WindowName != "sun-override" {
		t.Fatalf("WindowName = %q, want sun-override", bdSun.WindowName)
	}
	approxEqual(t, bdSun.TotalCost, 0.60, "sun-override")
}

func TestCalculateCost_UnpricedNonZeroReturnsNil(t *testing.T) {
	base := store.ModelPrices{In: floatPtr(1.0)} // Out is nil
	bd, err := store.CalculateCost(base, nil, store.CostInput{
		InTokensUncached: 0,
		OutTokens:        500,
	})
	if err != nil {
		t.Fatalf("CalculateCost: %v", err)
	}
	if bd.TotalCost != nil {
		t.Fatalf("TotalCost = %v, want nil when all non-zero usage dimensions have no price", *bd.TotalCost)
	}
}

func TestPricingRuleStoreSnapshotAndUsageFallback(t *testing.T) {
	st := openTemp(t)
	acc := catalogAccountForPrice(t, st, "a1")

	if err := st.SetProviderModelPrices("acme", "m1", store.ModelPrices{
		In: floatPtr(1.0), Out: floatPtr(2.0),
	}); err != nil {
		t.Fatalf("SetProviderModelPrices: %v", err)
	}

	providerRule := &store.PricingRule{
		Version: 1,
		Tiers: &store.TieredPricing{
			Mode:  store.TierModeAllIn,
			Basis: store.TierBasisTotalInput,
			Brackets: []store.PriceTier{
				{MaxTokens: int64Ptr(100_000), Prices: store.PriceSet{In: floatPtr(1.0), Out: floatPtr(2.0)}},
				{MaxTokens: nil, Prices: store.PriceSet{In: floatPtr(5.0), Out: floatPtr(10.0)}},
			},
		},
	}
	if err := st.SetProviderPricingRule("acme", "m1", providerRule); err != nil {
		t.Fatalf("SetProviderPricingRule: %v", err)
	}

	// Verify EffectivePricing & EffectiveModels read provider rule.
	_, gotRule, baseSrc, ruleSrc, resolvedProv, err := st.EffectivePricing("a1", "", "m1")
	if err != nil {
		t.Fatalf("EffectivePricing: %v", err)
	}
	if gotRule == nil || baseSrc != "provider" || ruleSrc != "provider" || resolvedProv != "acme" {
		t.Fatalf("EffectivePricing mismatch: rule=%v baseSrc=%q ruleSrc=%q prov=%q", gotRule, baseSrc, ruleSrc, resolvedProv)
	}

	// Account override rule takes precedence over provider rule.
	accountRule := &store.PricingRule{
		Version: 1,
		Tiers: &store.TieredPricing{
			Mode:  store.TierModeAllIn,
			Basis: store.TierBasisTotalInput,
			Brackets: []store.PriceTier{
				{MaxTokens: nil, Prices: store.PriceSet{In: floatPtr(3.0), Out: floatPtr(6.0)}},
			},
		},
	}
	if err := st.SetAccountPricingRule("a1", "m1", accountRule); err != nil {
		t.Fatalf("SetAccountPricingRule: %v", err)
	}

	infos, err := st.EffectiveModels(acc.ID)
	if err != nil {
		t.Fatalf("EffectiveModels: %v", err)
	}
	if len(infos) != 1 || infos[0].PricingRule == nil || *infos[0].PricingRule.Tiers.Brackets[0].Prices.In != 3.0 {
		t.Fatalf("EffectiveModels did not prefer account pricing rule: %+v", infos)
	}

	// SimulateModelCost
	sim, _, rSrc, _, err := st.SimulateModelCost("a1", "", "m1", store.CostInput{
		InTokensUncached: 1_000_000,
		OutTokens:        1_000_000,
	})
	if err != nil || rSrc != "account" {
		t.Fatalf("SimulateModelCost: err=%v rSrc=%q", err, rSrc)
	}
	approxEqual(t, sim.TotalCost, 9.0, "simulate account rule")

	// InsertRequest snapshots Cost = 9.0 (1M*3 + 1M*6)
	now := time.Now().Unix()
	rec := store.RequestRecord{
		TS: now, Inbound: "anthropic", Outbound: "anthropic",
		ModelRequested: "m1", ModelUsed: "m1", AccountID: acc.ID,
		HTTPStatus: 200, OK: true,
		InTokens: 1_000_000, InTokensUncached: 1_000_000, OutTokens: 1_000_000,
	}
	if err := st.InsertRequest(&rec); err != nil {
		t.Fatalf("InsertRequest: %v", err)
	}
	approxEqual(t, rec.Cost, 9.0, "snapshotted request cost")

	// Clear account rule and change provider prices -> snapshotted request must still be 9.0 in Usage!
	if err := st.ClearAccountPricingRule("a1", "m1"); err != nil {
		t.Fatalf("ClearAccountPricingRule: %v", err)
	}
	if err := st.ClearProviderPricingRule("acme", "m1"); err != nil {
		t.Fatalf("ClearProviderPricingRule: %v", err)
	}
	if err := st.SetProviderModelPrices("acme", "m1", store.ModelPrices{
		In: floatPtr(10.0), Out: floatPtr(20.0),
	}); err != nil {
		t.Fatalf("SetProviderModelPrices update: %v", err)
	}

	rows, err := st.Usage("model", now-60, 0)
	if err != nil || len(rows) != 1 {
		t.Fatalf("Usage after price change: err=%v rows=%+v", err, rows)
	}
	approxEqual(t, rows[0].Cost, 9.0, "usage preserves snapshot after price change")

	// Insert a legacy row with cost = NULL directly via SQL -> Usage falls back to dynamic SQL (10 + 20 = 30), total = 39.
	if _, err := st.DB().Exec(
		`INSERT INTO requests (ts, inbound, outbound, model_requested, model_used, account_id, http_status, ok, in_tokens, in_tokens_uncached, out_tokens, cost)
		 VALUES (?, 'anthropic', 'anthropic', 'm1', 'm1', ?, 200, 1, 1000000, 1000000, 1000000, NULL)`,
		now, acc.ID,
	); err != nil {
		t.Fatalf("insert legacy row: %v", err)
	}
	rows2, err := st.Usage("model", now-60, 0)
	if err != nil || len(rows2) != 1 {
		t.Fatalf("Usage with legacy row: err=%v rows=%+v", err, rows2)
	}
	approxEqual(t, rows2[0].Cost, 39.0, "usage combines snapshot (9) + legacy dynamic fallback (30)")
}
