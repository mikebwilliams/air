package main

import (
	"errors"
	"math"
	"strings"
)

const (
	standardServiceTier      = "standard"
	longContextInputTokens   = 272_000
	openAIPricingSource      = "https://developers.openai.com/api/docs/pricing"
	openAIPricingSnapshotDay = "2026-08-15"
)

// Model is the complete AIR configuration for a model identifier. A nil
// Pricing value explicitly means that AIR does not know how to price it.
type Model struct {
	Name    string        `json:"name"`
	Pricing *ModelPricing `json:"pricing,omitempty"`
}

// ModelPricing stores an immutable pricing snapshot. Rates use nanodollars per
// token so all published per-million-token decimal prices remain exact.
type ModelPricing struct {
	ServiceTier            string      `json:"service_tier"`
	Source                 string      `json:"source"`
	AsOf                   string      `json:"as_of"`
	LongContextInputTokens int64       `json:"long_context_input_tokens"`
	ShortContext           TokenPrices `json:"short_context"`
	LongContext            TokenPrices `json:"long_context"`
}

type TokenPrices struct {
	InputNanousdPerToken       int64 `json:"input_nanousd_per_token"`
	CachedInputNanousdPerToken int64 `json:"cached_input_nanousd_per_token"`
	CacheWriteNanousdPerToken  int64 `json:"cache_write_nanousd_per_token"`
	OutputNanousdPerToken      int64 `json:"output_nanousd_per_token"`
}

type CostEstimate struct {
	MinimumMicrousd int64
	MaximumMicrousd int64
	Context         string
	Complete        bool
}

var builtinModels = map[string]Model{
	"gpt-5.6-sol":   pricedModel("gpt-5.6-sol", tokenPrices(5, 0.5, 6.25, 30), tokenPrices(10, 1, 12.5, 45)),
	"gpt-5.6":       pricedModel("gpt-5.6", tokenPrices(5, 0.5, 6.25, 30), tokenPrices(10, 1, 12.5, 45)),
	"gpt-5.6-terra": pricedModel("gpt-5.6-terra", tokenPrices(2, 0.2, 2.5, 12), tokenPrices(4, 0.4, 5, 18)),
	"gpt-5.6-luna":  pricedModel("gpt-5.6-luna", tokenPrices(0.2, 0.02, 0.25, 1.2), tokenPrices(0.4, 0.04, 0.5, 1.8)),
}

func pricedModel(name string, shortContext, longContext TokenPrices) Model {
	return Model{
		Name: name,
		Pricing: &ModelPricing{
			ServiceTier:            standardServiceTier,
			Source:                 openAIPricingSource,
			AsOf:                   openAIPricingSnapshotDay,
			LongContextInputTokens: longContextInputTokens,
			ShortContext:           shortContext,
			LongContext:            longContext,
		},
	}
}

func tokenPrices(input, cachedInput, cacheWrite, output float64) TokenPrices {
	return TokenPrices{
		InputNanousdPerToken:       int64(math.Round(input * 1_000)),
		CachedInputNanousdPerToken: int64(math.Round(cachedInput * 1_000)),
		CacheWriteNanousdPerToken:  int64(math.Round(cacheWrite * 1_000)),
		OutputNanousdPerToken:      int64(math.Round(output * 1_000)),
	}
}

func modelByName(name string) Model {
	name = strings.TrimSpace(name)
	if model, ok := builtinModels[name]; ok {
		pricing := *model.Pricing
		model.Pricing = &pricing
		return model
	}
	return Model{Name: name}
}

func validateTokenUsage(usage TokenUsage) error {
	if usage.InputTokens < 0 || usage.CachedInputTokens < 0 ||
		usage.OutputTokens < 0 || usage.ReasoningOutputTokens < 0 {
		return errors.New("token counts must not be negative")
	}
	if usage.CacheWriteTokens != nil && *usage.CacheWriteTokens < 0 {
		return errors.New("cache-write token count must not be negative")
	}
	if usage.CachedInputTokens > usage.InputTokens {
		return errors.New("cached input tokens exceed input tokens")
	}
	if usage.CacheWriteTokens != nil &&
		*usage.CacheWriteTokens > usage.InputTokens-usage.CachedInputTokens {
		return errors.New("cached input and cache-write tokens exceed input tokens")
	}
	if usage.ReasoningOutputTokens > usage.OutputTokens {
		return errors.New("reasoning output tokens exceed output tokens")
	}
	return nil
}

func (model Model) EstimateCost(usage TokenUsage) (*CostEstimate, error) {
	if err := validateTokenUsage(usage); err != nil {
		return nil, err
	}
	if model.Pricing == nil {
		return nil, nil
	}
	pricing := model.Pricing
	prices := pricing.ShortContext
	contextClass := "short"
	if usage.InputTokens > pricing.LongContextInputTokens {
		prices = pricing.LongContext
		contextClass = "long"
	}

	fixed, err := costNanousd(usage.CachedInputTokens, prices.CachedInputNanousdPerToken)
	if err != nil {
		return nil, err
	}
	outputCost, err := costNanousd(usage.OutputTokens, prices.OutputNanousdPerToken)
	if err != nil {
		return nil, err
	}
	fixed, err = addNanousd(fixed, outputCost)
	if err != nil {
		return nil, err
	}

	if usage.CacheWriteTokens != nil {
		ordinaryInput := usage.InputTokens - usage.CachedInputTokens - *usage.CacheWriteTokens
		ordinaryCost, err := costNanousd(ordinaryInput, prices.InputNanousdPerToken)
		if err != nil {
			return nil, err
		}
		writeCost, err := costNanousd(*usage.CacheWriteTokens, prices.CacheWriteNanousdPerToken)
		if err != nil {
			return nil, err
		}
		total, err := addNanousd(fixed, ordinaryCost, writeCost)
		if err != nil {
			return nil, err
		}
		cost := roundNanousdToMicrousd(total)
		return &CostEstimate{
			MinimumMicrousd: cost,
			MaximumMicrousd: cost,
			Context:         contextClass,
			Complete:        true,
		}, nil
	}

	uncategorizedInput := usage.InputTokens - usage.CachedInputTokens
	minimumRate := min(prices.InputNanousdPerToken, prices.CacheWriteNanousdPerToken)
	maximumRate := max(prices.InputNanousdPerToken, prices.CacheWriteNanousdPerToken)
	minimumInput, err := costNanousd(uncategorizedInput, minimumRate)
	if err != nil {
		return nil, err
	}
	maximumInput, err := costNanousd(uncategorizedInput, maximumRate)
	if err != nil {
		return nil, err
	}
	minimumTotal, err := addNanousd(fixed, minimumInput)
	if err != nil {
		return nil, err
	}
	maximumTotal, err := addNanousd(fixed, maximumInput)
	if err != nil {
		return nil, err
	}
	return &CostEstimate{
		MinimumMicrousd: roundNanousdToMicrousd(minimumTotal),
		MaximumMicrousd: roundNanousdToMicrousd(maximumTotal),
		Context:         contextClass,
		Complete:        false,
	}, nil
}

func costNanousd(tokens, rate int64) (int64, error) {
	if tokens < 0 || rate < 0 {
		return 0, errors.New("tokens and prices must not be negative")
	}
	if rate != 0 && tokens > math.MaxInt64/rate {
		return 0, errors.New("estimated review cost exceeds storage range")
	}
	return tokens * rate, nil
}

func addNanousd(values ...int64) (int64, error) {
	var total int64
	for _, value := range values {
		if value > math.MaxInt64-total {
			return 0, errors.New("estimated review cost exceeds storage range")
		}
		total += value
	}
	return total, nil
}

func roundNanousdToMicrousd(value int64) int64 {
	return value/1_000 + (value%1_000)/500
}

func totalTokens(usage TokenUsage) int64 {
	return usage.InputTokens + usage.OutputTokens
}
