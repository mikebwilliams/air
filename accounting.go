package main

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Pricing contains estimated USD prices per million tokens. Reasoning tokens
// are already included in output tokens and are not charged a second time.
type Pricing struct {
	InputUSDPerMillion       float64
	CachedInputUSDPerMillion float64
	OutputUSDPerMillion      float64
}

func parsePricing(input, cachedInput, output string) (*Pricing, error) {
	values := []string{
		strings.TrimSpace(input),
		strings.TrimSpace(cachedInput),
		strings.TrimSpace(output),
	}
	provided := 0
	for _, value := range values {
		if value != "" {
			provided++
		}
	}
	if provided == 0 {
		return nil, nil
	}
	if provided != len(values) {
		return nil, errors.New("input, cached-input, and output token prices must all be configured")
	}

	parsed := make([]float64, len(values))
	for index, value := range values {
		price, err := strconv.ParseFloat(value, 64)
		if err != nil || math.IsNaN(price) || math.IsInf(price, 0) || price < 0 {
			return nil, fmt.Errorf("token prices must be non-negative decimal numbers")
		}
		parsed[index] = price
	}
	return &Pricing{
		InputUSDPerMillion:       parsed[0],
		CachedInputUSDPerMillion: parsed[1],
		OutputUSDPerMillion:      parsed[2],
	}, nil
}

func validateTokenUsage(usage TokenUsage) error {
	if usage.InputTokens < 0 || usage.CachedInputTokens < 0 ||
		usage.OutputTokens < 0 || usage.ReasoningOutputTokens < 0 {
		return errors.New("token counts must not be negative")
	}
	if usage.CachedInputTokens > usage.InputTokens {
		return errors.New("cached input tokens exceed input tokens")
	}
	if usage.ReasoningOutputTokens > usage.OutputTokens {
		return errors.New("reasoning output tokens exceed output tokens")
	}
	return nil
}

// EstimateMicrousd returns an estimate rounded to the nearest millionth of a
// US dollar. Prices are supplied per million tokens, so token*price is already
// denominated in microdollars.
func (pricing Pricing) EstimateMicrousd(usage TokenUsage) (int64, error) {
	if err := validateTokenUsage(usage); err != nil {
		return 0, err
	}
	rates := []float64{
		pricing.InputUSDPerMillion,
		pricing.CachedInputUSDPerMillion,
		pricing.OutputUSDPerMillion,
	}
	for _, rate := range rates {
		if math.IsNaN(rate) || math.IsInf(rate, 0) || rate < 0 {
			return 0, errors.New("token prices must be non-negative finite numbers")
		}
	}
	uncachedInput := usage.InputTokens - usage.CachedInputTokens
	estimate := float64(uncachedInput)*pricing.InputUSDPerMillion +
		float64(usage.CachedInputTokens)*pricing.CachedInputUSDPerMillion +
		float64(usage.OutputTokens)*pricing.OutputUSDPerMillion
	if estimate > math.MaxInt64 {
		return 0, errors.New("estimated review cost exceeds storage range")
	}
	return int64(math.Round(estimate)), nil
}

func totalTokens(usage TokenUsage) int64 {
	return usage.InputTokens + usage.OutputTokens
}

func addTokenUsage(total, additional TokenUsage) (TokenUsage, error) {
	if err := validateTokenUsage(additional); err != nil {
		return TokenUsage{}, err
	}
	add := func(left, right int64) (int64, error) {
		if right > math.MaxInt64-left {
			return 0, errors.New("token count exceeds storage range")
		}
		return left + right, nil
	}
	var result TokenUsage
	var err error
	if result.InputTokens, err = add(total.InputTokens, additional.InputTokens); err != nil {
		return TokenUsage{}, err
	}
	if result.CachedInputTokens, err = add(total.CachedInputTokens, additional.CachedInputTokens); err != nil {
		return TokenUsage{}, err
	}
	if result.OutputTokens, err = add(total.OutputTokens, additional.OutputTokens); err != nil {
		return TokenUsage{}, err
	}
	if result.ReasoningOutputTokens, err = add(total.ReasoningOutputTokens, additional.ReasoningOutputTokens); err != nil {
		return TokenUsage{}, err
	}
	return result, nil
}
