package main

import (
	"encoding/json"
	"sort"
	"strings"
)

type usageContextTierPrice struct {
	Threshold   float64 `json:"threshold"`
	Input       float64 `json:"input"`
	Output      float64 `json:"output"`
	CacheCreate float64 `json:"cacheCreate"`
	CacheRead   float64 `json:"cacheRead"`
}

type usageModelPrice struct {
	Input        float64                 `json:"input"`
	Output       float64                 `json:"output"`
	CacheCreate  float64                 `json:"cacheCreate"`
	CacheRead    float64                 `json:"cacheRead"`
	ContextTiers []usageContextTierPrice `json:"contextTiers,omitempty"`
}

type usageTierMultiplier struct {
	Model      string  `json:"model"`
	Tier       string  `json:"tier"`
	Multiplier float64 `json:"multiplier"`
}

type usagePricing struct {
	Prices          map[string]usageModelPrice `json:"prices"`
	TierMultipliers []usageTierMultiplier      `json:"tier_multipliers"`
}

type usageTierMultiplierIndex map[string]float64

func normalizeUsagePricing(pricing usagePricing) (usagePricing, string) {
	normalized := usagePricing{
		Prices:          make(map[string]usageModelPrice, len(pricing.Prices)),
		TierMultipliers: make([]usageTierMultiplier, 0, len(pricing.TierMultipliers)),
	}
	for model, price := range pricing.Prices {
		model = strings.TrimSpace(model)
		if model == "" {
			continue
		}
		price.Input = nonNegativeFloat(price.Input)
		price.Output = nonNegativeFloat(price.Output)
		price.CacheCreate = nonNegativeFloat(price.CacheCreate)
		price.CacheRead = nonNegativeFloat(price.CacheRead)
		tiers := make([]usageContextTierPrice, 0, len(price.ContextTiers))
		for _, tier := range price.ContextTiers {
			if tier.Threshold <= 0 {
				continue
			}
			tier.Input = nonNegativeFloat(tier.Input)
			tier.Output = nonNegativeFloat(tier.Output)
			tier.CacheCreate = nonNegativeFloat(tier.CacheCreate)
			tier.CacheRead = nonNegativeFloat(tier.CacheRead)
			tiers = append(tiers, tier)
		}
		sort.SliceStable(tiers, func(i, j int) bool { return tiers[i].Threshold < tiers[j].Threshold })
		price.ContextTiers = tiers
		normalized.Prices[model] = price
	}
	multipliers := make(map[string]usageTierMultiplier, len(pricing.TierMultipliers))
	for _, multiplier := range pricing.TierMultipliers {
		multiplier.Model = strings.TrimSpace(multiplier.Model)
		multiplier.Tier = strings.TrimSpace(multiplier.Tier)
		if multiplier.Model == "" || multiplier.Tier == "" || multiplier.Multiplier <= 0 {
			continue
		}
		key := strings.ToLower(multiplier.Model + "\x00" + multiplier.Tier)
		multipliers[key] = multiplier
	}
	for _, multiplier := range multipliers {
		normalized.TierMultipliers = append(normalized.TierMultipliers, multiplier)
	}
	sort.Slice(normalized.TierMultipliers, func(i, j int) bool {
		left := strings.ToLower(normalized.TierMultipliers[i].Model + "\x00" + normalized.TierMultipliers[i].Tier)
		right := strings.ToLower(normalized.TierMultipliers[j].Model + "\x00" + normalized.TierMultipliers[j].Tier)
		return left < right
	})
	encoded, _ := json.Marshal(normalized)
	return normalized, string(encoded)
}

func nonNegativeFloat(value float64) float64 {
	if value < 0 {
		return 0
	}
	return value
}

func buildTierMultiplierIndex(values []usageTierMultiplier) usageTierMultiplierIndex {
	result := make(usageTierMultiplierIndex, len(values))
	for _, value := range values {
		result[strings.ToLower(value.Model)+"\x00"+strings.ToLower(value.Tier)] = value.Multiplier
	}
	return result
}

func calculateStoredRecordCost(
	record Record,
	prices map[string]usageModelPrice,
	multipliers usageTierMultiplierIndex,
) float64 {
	return calculateStatisticsRecordCost(newUsageStatisticsRecord(record), prices, multipliers)
}

func calculateStatisticsRecordCost(
	record usageStatisticsRecord,
	prices map[string]usageModelPrice,
	multipliers usageTierMultiplierIndex,
) float64 {
	price, exists := prices[record.Model]
	if !exists {
		return 0
	}
	pureInputTokens := record.InputTokens
	provider := strings.ToLower(record.Provider)
	if !strings.Contains(provider, "claude") && !strings.Contains(provider, "anthropic") {
		pureInputTokens = max(record.InputTokens-record.CachedTokens-record.CacheCreationTokens, 0)
	}
	contextTokens := float64(pureInputTokens + record.CachedTokens + record.CacheCreationTokens)
	selected := usageContextTierPrice{
		Input:       price.Input,
		Output:      price.Output,
		CacheCreate: price.CacheCreate,
		CacheRead:   price.CacheRead,
	}
	selectedThreshold := float64(-1)
	for _, tier := range price.ContextTiers {
		if contextTokens > tier.Threshold && tier.Threshold > selectedThreshold {
			selected = tier
			selectedThreshold = tier.Threshold
		}
	}
	const priceUnit = 1_000_000
	cost := (float64(pureInputTokens)*selected.Input +
		float64(record.OutputTokens)*selected.Output +
		float64(record.CachedTokens)*selected.CacheRead +
		float64(record.CacheCreationTokens)*selected.CacheCreate) / priceUnit
	if len(multipliers) != 0 {
		multiplier := multipliers[strings.ToLower(record.Model)+"\x00"+strings.ToLower(record.ServiceTier)]
		if multiplier > 0 {
			cost *= multiplier
		}
	}
	if cost < 0 {
		return 0
	}
	return cost
}
