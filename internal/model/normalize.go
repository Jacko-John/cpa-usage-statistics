package model

import (
	"strings"
	"time"
)

func NormalizeRecord(record Record) Record {
	if record.Timestamp.IsZero() {
		record.Timestamp = time.Now()
	}
	record.Timestamp = record.Timestamp.UTC()
	record.Model = NormalizeModel(record.Model)
	record.LatencyMs = max(record.LatencyMs, 0)
	record.TTFTMs = max(record.TTFTMs, 0)
	record.Tokens = NonNegativeTokenStats(record.Tokens)
	record.Tokens.TotalTokens = NormalizeTotalTokens(record.Tokens)
	record.FailureStatusCode = max(record.FailureStatusCode, 0)
	return record
}

func GroupingKey(apiKey, provider string) string {
	if trimmed := strings.TrimSpace(apiKey); trimmed != "" {
		return trimmed
	}
	if trimmed := strings.TrimSpace(provider); trimmed != "" {
		return trimmed
	}
	return "unknown"
}

func NormalizeModel(model string) string {
	if trimmed := strings.TrimSpace(model); trimmed != "" {
		return trimmed
	}
	return "unknown"
}

func NormalizeTotalTokens(tokens TokenStats) int64 {
	if tokens.TotalTokens != 0 {
		return tokens.TotalTokens
	}
	total := tokens.InputTokens + tokens.OutputTokens + tokens.ReasoningTokens
	if total != 0 {
		return total
	}
	return tokens.CachedTokens
}

func NonNegativeTokenStats(tokens TokenStats) TokenStats {
	tokens.InputTokens = max(tokens.InputTokens, 0)
	tokens.OutputTokens = max(tokens.OutputTokens, 0)
	tokens.ReasoningTokens = max(tokens.ReasoningTokens, 0)
	tokens.CachedTokens = max(tokens.CachedTokens, 0)
	tokens.CacheReadTokens = max(tokens.CacheReadTokens, 0)
	tokens.CacheCreationTokens = max(tokens.CacheCreationTokens, 0)
	tokens.TotalTokens = max(tokens.TotalTokens, 0)
	return tokens
}
