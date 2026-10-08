package statistics

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	usagemodel "github.com/Jacko-John/cpa-usage-statistics/internal/model"
)

const (
	StatisticsMinute       = int64(60)
	StatisticsHour         = int64(60 * 60)
	statisticsDay          = int64(24 * 60 * 60)
	credentialHistoryRange = int64(31 * 24 * 60 * 60)
)

var statisticsRangeSeconds = map[string]int64{
	"7h":  7 * StatisticsHour,
	"24h": 24 * StatisticsHour,
	"7d":  7 * statisticsDay,
	"30d": 30 * statisticsDay,
}

func RangeSeconds(name string) (int64, bool) {
	seconds, exists := statisticsRangeSeconds[name]
	return seconds, exists
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
	statisticsRange7h:  7 * StatisticsHour,
	statisticsRange24h: 24 * StatisticsHour,
	statisticsRange7d:  7 * statisticsDay,
	statisticsRange30d: 30 * statisticsDay,
}

type usageRangeState struct {
	Data         UsageSummaryData
	CutoffMinute int64
}

type Accumulator struct {
	PricingKey           string
	Pricing              Pricing
	TierMultipliers      TierMultiplierIndex
	All                  UsageSummaryData
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
	Record Entry
	Now    int64
}

type Engine struct {
	mu      sync.RWMutex
	state   Accumulator
	pending []usageStatisticsDelta
}

type UsageSummaryResponse struct {
	Range        string `json:"range"`
	GeneratedAt  int64  `json:"generated_at"`
	RateRequests int64  `json:"rate_requests"`
	RateTokens   int64  `json:"rate_tokens"`
	RateMinutes  int64  `json:"rate_minutes"`
	*UsageSummaryData
}

type CredentialWindowQuery struct {
	ID        string `json:"id"`
	AuthIndex string `json:"auth_index"`
	Source    string `json:"source"`
	Category  string `json:"category"`
	Start     int64  `json:"start"`
	End       int64  `json:"end"`
}

type CredentialWindowSummary struct {
	Requests     int64   `json:"requests"`
	SuccessCount int64   `json:"success_count"`
	FailureCount int64   `json:"failure_count"`
	Tokens       int64   `json:"tokens"`
	Cost         float64 `json:"cost"`
}

type CredentialWindowResponse struct {
	Windows map[string]CredentialWindowSummary `json:"windows"`
}

// usageAPIWindowQuery 使用分钟对齐的请求时间区间 [Start, End)。
type APIWindowQuery struct {
	APIKey string
	Start  int64
	End    int64
}

type APIWindowSummary struct {
	Requests int64
	Tokens   int64
	Cost     float64
	Unpriced int64
	Details  []APIWindowDetail
}

func (s *Engine) apiWindows(queries []APIWindowQuery, now int64) ([]APIWindowSummary, []int, Pricing) {
	s.lockCurrentForRead(now)
	defer s.mu.RUnlock()
	results, missing := s.state.apiWindows(queries, now)
	return results, missing, s.state.Pricing
}

func NewAccumulator(now int64, pricing Pricing, pricingKey string) Accumulator {
	var ranges [statisticsRangeCount]usageRangeState
	for index, seconds := range statisticsRanges {
		ranges[index] = usageRangeState{
			Data:         NewUsageSummaryData(),
			CutoffMinute: FloorUnix(now-seconds, StatisticsMinute),
		}
	}
	return Accumulator{
		PricingKey:           pricingKey,
		Pricing:              pricing,
		TierMultipliers:      BuildTierMultiplierIndex(pricing.TierMultipliers),
		All:                  NewUsageSummaryData(),
		Ranges:               ranges,
		Dimensions:           newUsageDimensionTable(),
		MinuteBuckets:        make(map[int64]*usageMinuteBucket),
		CredentialMinutes:    make(map[int64]*credentialTimeBucket),
		CredentialHours:      make(map[int64]*credentialTimeBucket),
		CredentialIdentities: make(map[string]credentialIdentityReference),
		AdvancedMinute:       FloorUnix(now, StatisticsMinute),
		OldestMinute:         FloorUnix(now-credentialHistoryRange, StatisticsMinute),
		OldestHour:           FloorUnix(now-credentialHistoryRange, StatisticsHour),
	}
}

func (s *Engine) PricingKey() string {
	s.mu.RLock()
	key := s.state.PricingKey
	s.mu.RUnlock()
	return key
}

func (s *Engine) Pricing() Pricing {
	s.mu.RLock()
	pricing := s.state.Pricing
	s.mu.RUnlock()
	return pricing
}

func (s *Engine) Add(record usagemodel.Record, now int64) {
	statisticsRecord := newUsageStatisticsRecord(record)
	s.mu.Lock()
	s.state.advance(now)
	s.state.AddRecord(statisticsRecord, now)
	if s.pending != nil {
		s.pending = append(s.pending, usageStatisticsDelta{Record: statisticsRecord, Now: now})
	}
	s.mu.Unlock()
}

func (s *Engine) Remove(removed *Accumulator, now int64) {
	s.mu.Lock()
	if s.state.AdvancedMinute > FloorUnix(now, StatisticsMinute) {
		now = s.state.AdvancedMinute
	}
	s.state.advance(now)
	removed.advance(now)
	s.state.subtract(removed)
	s.mu.Unlock()
}

func (s *Engine) BeginPricingRebuild() {
	s.mu.Lock()
	s.pending = make([]usageStatisticsDelta, 0)
	s.mu.Unlock()
}

func (s *Engine) CancelPricingRebuild() {
	s.mu.Lock()
	s.pending = nil
	s.mu.Unlock()
}

func (s *Engine) FinishPricingRebuild(next Accumulator) {
	s.mu.Lock()
	for _, delta := range s.pending {
		next.advance(delta.Now)
		next.AddRecord(delta.Record, delta.Now)
	}
	s.state = next
	s.pending = nil
	s.mu.Unlock()
}

// lockCurrentForRead 只在跨分钟时清理过期桶，返回时保持读锁。
func (s *Engine) lockCurrentForRead(now int64) {
	minute := FloorUnix(now, StatisticsMinute)
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

func (s *Engine) SummaryJSON(rangeName string, now int64) ([]byte, error) {
	s.lockCurrentForRead(now)
	defer s.mu.RUnlock()

	response := UsageSummaryResponse{
		Range:            rangeName,
		GeneratedAt:      now,
		UsageSummaryData: &s.state.All,
	}
	if rangeName != "all" {
		index, ok := statisticsRangeByName(rangeName)
		if !ok {
			return nil, fmt.Errorf("invalid usage range")
		}
		state := &s.state.Ranges[index]
		response.RateRequests = state.Data.TotalRequests
		response.RateTokens = state.Data.TotalTokens
		response.RateMinutes = statisticsRanges[index] / StatisticsMinute
		response.UsageSummaryData = &state.Data
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

func (s *Engine) CredentialWindowsJSON(queries []CredentialWindowQuery, now int64) ([]byte, error) {
	s.lockCurrentForRead(now)
	defer s.mu.RUnlock()

	result := CredentialWindowResponse{Windows: make(map[string]CredentialWindowSummary, len(queries))}
	for _, query := range queries {
		id := strings.TrimSpace(query.ID)
		if id != "" {
			result.Windows[id] = s.state.credentialWindow(query)
		}
	}
	return json.Marshal(result)
}
