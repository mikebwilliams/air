package main

import "testing"

func TestBuiltinModelStandardPricing(t *testing.T) {
	tests := []struct {
		name      string
		shortRate TokenPrices
		longRate  TokenPrices
	}{
		{"gpt-5.6-sol", TokenPrices{5000, 500, 6250, 30000}, TokenPrices{10000, 1000, 12500, 45000}},
		{"gpt-5.6", TokenPrices{5000, 500, 6250, 30000}, TokenPrices{10000, 1000, 12500, 45000}},
		{"gpt-5.6-terra", TokenPrices{2000, 200, 2500, 12000}, TokenPrices{4000, 400, 5000, 18000}},
		{"gpt-5.6-luna", TokenPrices{200, 20, 250, 1200}, TokenPrices{400, 40, 500, 1800}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			pricing := modelByName(test.name).Pricing
			if pricing == nil {
				t.Fatal("pricing is unknown")
			}
			if pricing.ServiceTier != "standard" || pricing.LongContextInputTokens != 272_000 ||
				pricing.ShortContext != test.shortRate || pricing.LongContext != test.longRate {
				t.Fatalf("pricing = %+v", pricing)
			}
		})
	}
}

func TestKnownModelEstimatesAllBillingCategories(t *testing.T) {
	cacheWrites := int64(10)
	estimate, err := modelByName("gpt-5.6-luna").EstimateCost(TokenUsage{
		InputTokens:           100,
		CachedInputTokens:     25,
		CacheWriteTokens:      &cacheWrites,
		OutputTokens:          20,
		ReasoningOutputTokens: 5,
	})
	if err != nil {
		t.Fatalf("EstimateCost: %v", err)
	}
	if estimate == nil || estimate.MinimumMicrousd != 40 || estimate.MaximumMicrousd != 40 ||
		estimate.Context != "short" || !estimate.Complete {
		t.Fatalf("estimate = %+v", estimate)
	}
}

func TestKnownModelBoundsUnreportedCacheWrites(t *testing.T) {
	estimate, err := modelByName("gpt-5.6-luna").EstimateCost(TokenUsage{
		InputTokens:       100,
		CachedInputTokens: 25,
		OutputTokens:      20,
	})
	if err != nil {
		t.Fatalf("EstimateCost: %v", err)
	}
	if estimate == nil || estimate.MinimumMicrousd != 40 || estimate.MaximumMicrousd != 43 || estimate.Complete {
		t.Fatalf("estimate = %+v", estimate)
	}
}

func TestKnownModelUsesLongContextPricesAboveThreshold(t *testing.T) {
	cacheWrites := int64(0)
	estimate, err := modelByName("gpt-5.6-terra").EstimateCost(TokenUsage{
		InputTokens:      longContextInputTokens + 1,
		CacheWriteTokens: &cacheWrites,
		OutputTokens:     1_000,
	})
	if err != nil {
		t.Fatalf("EstimateCost: %v", err)
	}
	if estimate == nil || estimate.Context != "long" || !estimate.Complete {
		t.Fatalf("estimate = %+v", estimate)
	}
}

func TestUnknownModelHasUnknownPricing(t *testing.T) {
	model := modelByName("private-model")
	if model.Pricing != nil {
		t.Fatalf("pricing = %+v", model.Pricing)
	}
	estimate, err := model.EstimateCost(TokenUsage{InputTokens: 10, OutputTokens: 2})
	if err != nil || estimate != nil {
		t.Fatalf("estimate = %+v, %v", estimate, err)
	}
}
