package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
)

const (
	statisticsMinute       = int64(60)
	statisticsHour         = int64(60 * 60)
	statisticsDay          = int64(24 * 60 * 60)
	credentialHistoryRange = int64(31 * 24 * 60 * 60)
	usagePricingRowName    = "current"
)

var statisticsRangeSeconds = map[string]int64{
	"7h":  7 * statisticsHour,
	"24h": 24 * statisticsHour,
	"7d":  7 * statisticsDay,
	"30d": 30 * statisticsDay,
}

type statisticsRangeIndex uint8

const (
	statisticsRange7h statisticsRangeIndex = iota
	statisticsRange24h
	statisticsRange7d
	statisticsRange30d
	statisticsRangeCount
)

var statisticsRanges = [statisticsRangeCount]int64{
	statisticsRange7h:  7 * statisticsHour,
	statisticsRange24h: 24 * statisticsHour,
	statisticsRange7d:  7 * statisticsDay,
	statisticsRange30d: 30 * statisticsDay,
}

type usageRangeState struct {
	Data         usageSummaryData
	CutoffMinute int64
}

type usageStatisticsState struct {
	PricingKey           string
	Pricing              usagePricing
	TierMultipliers      usageTierMultiplierIndex
	All                  usageSummaryData
	Ranges               [statisticsRangeCount]usageRangeState
	Dimensions           usageDimensionTable
	MinuteBuckets        map[int64]*usageMinuteBucket
	CredentialMinutes    map[int64]*credentialTimeBucket
	CredentialHours      map[int64]*credentialTimeBucket
	CredentialIdentities map[string]credentialIdentityReference
	AdvancedMinute       int64
	OldestMinute         int64
	OldestHour           int64
}

type usageStatisticsDelta struct {
	Record usageStatisticsRecord
	Now    int64
}

type usageStatistics struct {
	mu      sync.RWMutex
	state   usageStatisticsState
	pending []usageStatisticsDelta
}

type usageSummaryResponse struct {
	Range        string `json:"range"`
	GeneratedAt  int64  `json:"generated_at"`
	RateRequests int64  `json:"rate_requests"`
	RateTokens   int64  `json:"rate_tokens"`
	RateMinutes  int64  `json:"rate_minutes"`
	*usageSummaryData
}

type credentialWindowQuery struct {
	ID        string `json:"id"`
	AuthIndex string `json:"auth_index"`
	Source    string `json:"source"`
	Category  string `json:"category"`
	Start     int64  `json:"start"`
	End       int64  `json:"end"`
}

type credentialWindowSummary struct {
	Requests     int64   `json:"requests"`
	SuccessCount int64   `json:"success_count"`
	FailureCount int64   `json:"failure_count"`
	Tokens       int64   `json:"tokens"`
	Cost         float64 `json:"cost"`
}

type credentialWindowResponse struct {
	Windows map[string]credentialWindowSummary `json:"windows"`
}

func newUsageStatisticsState(now int64, pricing usagePricing, pricingKey string) usageStatisticsState {
	var ranges [statisticsRangeCount]usageRangeState
	for index, seconds := range statisticsRanges {
		ranges[index] = usageRangeState{
			Data:         newUsageSummaryData(),
			CutoffMinute: floorUnix(now-seconds, statisticsMinute),
		}
	}
	return usageStatisticsState{
		PricingKey:           pricingKey,
		Pricing:              pricing,
		TierMultipliers:      buildTierMultiplierIndex(pricing.TierMultipliers),
		All:                  newUsageSummaryData(),
		Ranges:               ranges,
		Dimensions:           newUsageDimensionTable(),
		MinuteBuckets:        make(map[int64]*usageMinuteBucket),
		CredentialMinutes:    make(map[int64]*credentialTimeBucket),
		CredentialHours:      make(map[int64]*credentialTimeBucket),
		CredentialIdentities: make(map[string]credentialIdentityReference),
		AdvancedMinute:       floorUnix(now, statisticsMinute),
		OldestMinute:         floorUnix(now-credentialHistoryRange, statisticsMinute),
		OldestHour:           floorUnix(now-credentialHistoryRange, statisticsHour),
	}
}

func (s *usageStatistics) pricingKey() string {
	s.mu.RLock()
	key := s.state.PricingKey
	s.mu.RUnlock()
	return key
}

func (s *usageStatistics) pricing() usagePricing {
	s.mu.RLock()
	pricing := s.state.Pricing
	s.mu.RUnlock()
	return pricing
}

func (s *usageStatistics) add(record Record, now int64) {
	statisticsRecord := newUsageStatisticsRecord(record)
	s.mu.Lock()
	s.state.advance(now)
	s.state.addRecord(statisticsRecord, now)
	if s.pending != nil {
		s.pending = append(s.pending, usageStatisticsDelta{Record: statisticsRecord, Now: now})
	}
	s.mu.Unlock()
}

func (s *usageStatistics) remove(removed *usageStatisticsState, now int64) {
	s.mu.Lock()
	if s.state.AdvancedMinute > floorUnix(now, statisticsMinute) {
		now = s.state.AdvancedMinute
	}
	s.state.advance(now)
	removed.advance(now)
	s.state.subtract(removed)
	s.mu.Unlock()
}

func (s *usageStatistics) beginPricingRebuild() {
	s.mu.Lock()
	s.pending = make([]usageStatisticsDelta, 0)
	s.mu.Unlock()
}

func (s *usageStatistics) cancelPricingRebuild() {
	s.mu.Lock()
	s.pending = nil
	s.mu.Unlock()
}

func (s *usageStatistics) finishPricingRebuild(next usageStatisticsState) {
	s.mu.Lock()
	for _, delta := range s.pending {
		next.advance(delta.Now)
		next.addRecord(delta.Record, delta.Now)
	}
	s.state = next
	s.pending = nil
	s.mu.Unlock()
}

// lockCurrentForRead 只在跨分钟时清理过期桶，返回时保持读锁。
func (s *usageStatistics) lockCurrentForRead(now int64) {
	minute := floorUnix(now, statisticsMinute)
	for {
		s.mu.RLock()
		if s.state.AdvancedMinute >= minute {
			return
		}
		s.mu.RUnlock()

		s.mu.Lock()
		s.state.advance(now)
		s.mu.Unlock()
	}
}

func (s *usageStatistics) summaryJSON(rangeName string, now int64) ([]byte, error) {
	s.lockCurrentForRead(now)
	defer s.mu.RUnlock()

	response := usageSummaryResponse{
		Range:            rangeName,
		GeneratedAt:      now,
		usageSummaryData: &s.state.All,
	}
	if rangeName != "all" {
		index, ok := statisticsRangeByName(rangeName)
		if !ok {
			return nil, fmt.Errorf("invalid usage range")
		}
		state := &s.state.Ranges[index]
		response.RateRequests = state.Data.TotalRequests
		response.RateTokens = state.Data.TotalTokens
		response.RateMinutes = statisticsRanges[index] / statisticsMinute
		response.usageSummaryData = &state.Data
	}
	return json.Marshal(response)
}

func statisticsRangeByName(name string) (statisticsRangeIndex, bool) {
	switch name {
	case "7h":
		return statisticsRange7h, true
	case "24h":
		return statisticsRange24h, true
	case "7d":
		return statisticsRange7d, true
	case "30d":
		return statisticsRange30d, true
	default:
		return 0, false
	}
}

func (s *usageStatistics) credentialWindowsJSON(queries []credentialWindowQuery, now int64) ([]byte, error) {
	s.lockCurrentForRead(now)
	defer s.mu.RUnlock()

	result := credentialWindowResponse{Windows: make(map[string]credentialWindowSummary, len(queries))}
	for _, query := range queries {
		id := strings.TrimSpace(query.ID)
		if id != "" {
			result.Windows[id] = s.state.credentialWindow(query)
		}
	}
	return json.Marshal(result)
}

func (s *SQLiteStore) usageSummaryJSON(rangeName string, now int64) ([]byte, error) {
	return s.statistics.summaryJSON(rangeName, now)
}

func (s *SQLiteStore) credentialWindowsJSON(queries []credentialWindowQuery, now int64) ([]byte, error) {
	return s.statistics.credentialWindowsJSON(queries, now)
}
