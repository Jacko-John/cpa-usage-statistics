package statistics

import (
	"slices"
	"strings"
)

type APIWindowDetail struct {
	Model       string  `json:"model"`
	ServiceTier string  `json:"service_tier"`
	Requests    int64   `json:"requests"`
	Tokens      int64   `json:"tokens"`
	Cost        float64 `json:"cost"`
	Unpriced    int64   `json:"unpriced_requests"`
}

type apiWindowDetailKey struct {
	model, tier string
}

type apiWindowAccumulator struct {
	details map[apiWindowDetailKey]*APIWindowDetail
}

func (window *apiWindowAccumulator) add(model, tier string, requests, tokens int64, cost float64, unpriced int64) {
	if window.details == nil {
		window.details = make(map[apiWindowDetailKey]*APIWindowDetail)
	}
	key := apiWindowDetailKey{model, tier}
	detail := window.details[key]
	if detail == nil {
		detail = &APIWindowDetail{Model: model, ServiceTier: tier}
		window.details[key] = detail
	}
	detail.Requests += requests
	detail.Tokens += tokens
	detail.Cost += cost
	detail.Unpriced += unpriced
}

func (window *apiWindowAccumulator) summary() APIWindowSummary {
	result := APIWindowSummary{Details: make([]APIWindowDetail, 0, len(window.details))}
	for _, detail := range window.details {
		result.Details = append(result.Details, *detail)
	}
	slices.SortFunc(result.Details, func(a, b APIWindowDetail) int {
		if order := strings.Compare(a.Model, b.Model); order != 0 {
			return order
		}
		return strings.Compare(a.ServiceTier, b.ServiceTier)
	})
	for _, detail := range result.Details {
		result.Requests += detail.Requests
		result.Tokens += detail.Tokens
		result.Cost += detail.Cost
		result.Unpriced += detail.Unpriced
	}
	return result
}

func normalizeServiceTier(tier string) string {
	return strings.ToLower(strings.TrimSpace(tier))
}
