package main

import (
	"strings"
	"testing"
)

func TestPricingEstimateSeparatesCachedInput(t *testing.T) {
	pricing := Pricing{
		InputUSDPerMillion:       10,
		CachedInputUSDPerMillion: 1,
		OutputUSDPerMillion:      20,
	}
	cost, err := pricing.EstimateMicrousd(TokenUsage{
		InputTokens:           100,
		CachedInputTokens:     25,
		OutputTokens:          20,
		ReasoningOutputTokens: 5,
	})
	if err != nil {
		t.Fatalf("EstimateMicrousd: %v", err)
	}
	if cost != 1175 {
		t.Fatalf("cost = %d microUSD, want 1175", cost)
	}
}

func TestParsePricingRequiresAllRates(t *testing.T) {
	pricing, err := parsePricing("", "", "")
	if err != nil || pricing != nil {
		t.Fatalf("empty pricing = %+v, %v", pricing, err)
	}
	_, err = parsePricing("1", "", "2")
	if err == nil || !strings.Contains(err.Error(), "must all be configured") {
		t.Fatalf("partial pricing error = %v", err)
	}
	_, err = parsePricing("1", "-1", "2")
	if err == nil || !strings.Contains(err.Error(), "non-negative") {
		t.Fatalf("negative pricing error = %v", err)
	}
}
