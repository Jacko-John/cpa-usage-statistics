package main

import (
	"fmt"
	"strings"
	"time"
)

type usageAggregate struct {
	TotalRequests               int64   `json:"total_requests"`
	SuccessCount                int64   `json:"success_count"`
	FailureCount                int64   `json:"failure_count"`
	TotalTokens                 int64   `json:"total_tokens"`
	TotalCost                   float64 `json:"total_cost"`
	LatencyTotalMs              int64   `json:"latency_total_ms"`
	LatencySampleCount          int64   `json:"latency_sample_count"`
	FirstByteLatencyTotalMs     int64   `json:"first_byte_latency_total_ms"`
	FirstByteLatencySampleCount int64   `json:"first_byte_latency_sample_count"`
	TPSOutputTokens             int64   `json:"tps_output_tokens"`
	TPSGenerationDurationMs     int64   `json:"tps_generation_duration_ms"`
	TPSSampleCount              int64   `json:"tps_sample_count"`
}

type usageAPIAggregate struct {
	usageAggregate
	Models map[string]*usageAggregate `json:"models"`
}

type usageCredentialAggregate struct {
	usageAggregate
	Source   string `json:"source,omitempty"`
	AuthType string `json:"auth_type,omitempty"`
}

// usageDay 使用服务器本地自然日聚合，JSON 键直接输出日期，避免浏览器再次换算时区。
type usageDay int32

func serverUsageDay(timestamp int64) usageDay {
	local := time.Unix(timestamp, 0).In(time.Local)
	return usageDay(local.Year()*10000 + int(local.Month())*100 + local.Day())
}

func (day usageDay) String() string {
	value := int(day)
	return fmt.Sprintf("%04d-%02d-%02d", value/10000, value/100%100, value%100)
}

func (day usageDay) MarshalText() ([]byte, error) {
	return []byte(day.String()), nil
}

type usageSummaryData struct {
	usageAggregate
	APIs        map[string]*usageAPIAggregate        `json:"apis"`
	Credentials map[string]*usageCredentialAggregate `json:"credentials"`
	Hours       map[int64]*usageAggregate            `json:"hours"`
	Days        map[usageDay]*usageAggregate         `json:"days"`
}

// usageStatisticsRecord 只保留预计算统计实际读取的持久化字段。
type usageStatisticsRecord struct {
	Timestamp           int64
	APIKey              string
	Provider            string
	Model               string
	Source              string
	AuthType            string
	ServiceTier         string
	LatencyMs           int64
	TTFTMs              int64
	InputTokens         int64
	OutputTokens        int64
	CachedTokens        int64
	CacheCreationTokens int64
	TotalTokens         int64
	Failed              bool
}

type usageRecordContribution struct {
	usageAggregate
	APIKey          string
	Model           string
	CredentialKey   string
	APIID           usageDimensionID
	ModelID         usageDimensionID
	CredentialID    usageDimensionID
	Source          string
	AuthType        string
	Timestamp       int64
	Minute          int64
	Hour            int64
	Day             usageDay
	CompletedAt     int64
	CompletedMinute int64
	CompletedHour   int64
	IsClaudeGPT     bool
	IsGemini        bool
}

func newUsageSummaryData() usageSummaryData {
	return usageSummaryData{
		APIs:        make(map[string]*usageAPIAggregate),
		Credentials: make(map[string]*usageCredentialAggregate),
		Hours:       make(map[int64]*usageAggregate),
		Days:        make(map[usageDay]*usageAggregate),
	}
}

func newUsageStatisticsRecord(record Record) usageStatisticsRecord {
	return usageStatisticsRecord{
		Timestamp:           record.Timestamp.Unix(),
		APIKey:              record.APIKey,
		Provider:            record.Provider,
		Model:               record.Model,
		Source:              record.Source,
		AuthType:            record.AuthType,
		ServiceTier:         record.ServiceTier,
		LatencyMs:           record.LatencyMs,
		TTFTMs:              record.TTFTMs,
		InputTokens:         record.Tokens.InputTokens,
		OutputTokens:        record.Tokens.OutputTokens,
		CachedTokens:        record.Tokens.CachedTokens,
		CacheCreationTokens: record.Tokens.CacheCreationTokens,
		TotalTokens:         record.Tokens.TotalTokens,
		Failed:              record.Failed,
	}
}

func (state *usageStatisticsState) addRecord(record usageStatisticsRecord, now int64) {
	cost := calculateStatisticsRecordCost(record, state.Pricing.Prices, state.TierMultipliers)
	contribution := state.newRecordContribution(record, cost)
	state.All.add(&contribution)

	if contribution.Timestamp <= now {
		for index := range state.Ranges {
			rng := &state.Ranges[index]
			if contribution.Timestamp >= rng.CutoffMinute {
				rng.Data.add(&contribution)
			}
		}
	}

	if contribution.Minute >= state.OldestMinute && contribution.Minute <= now {
		bucket := state.MinuteBuckets[contribution.Minute]
		if bucket == nil {
			bucket = newUsageMinuteBucket(&contribution)
			state.MinuteBuckets[contribution.Minute] = bucket
		} else {
			bucket.add(&contribution)
		}
	}
	state.addCredentialWindowContribution(&contribution, now)
}

func (state *usageStatisticsState) newRecordContribution(record usageStatisticsRecord, cost float64) usageRecordContribution {
	apiID, apiKey := state.Dimensions.intern(groupingKey(record.APIKey, record.Provider))
	modelID, model := state.Dimensions.intern(normalizeModel(record.Model))
	credentialID, credentialKey, credentialSource := state.internCredentialSource(record.Source)
	state.Dimensions.retain(apiID)
	state.Dimensions.retain(modelID)
	state.Dimensions.retain(credentialID)
	var aggregate usageAggregate
	aggregate.addRecord(record, cost)
	contribution := usageRecordContribution{
		usageAggregate: aggregate,
		APIKey:         apiKey,
		Model:          model,
		CredentialKey:  credentialKey,
		APIID:          apiID,
		ModelID:        modelID,
		CredentialID:   credentialID,
		Source:         credentialSource,
		AuthType:       strings.TrimSpace(record.AuthType),
		Timestamp:      record.Timestamp,
		Minute:         floorUnix(record.Timestamp, statisticsMinute),
		Hour:           floorUnix(record.Timestamp, statisticsHour),
		Day:            serverUsageDay(record.Timestamp),
	}
	if credentialID != 0 {
		contribution.CompletedAt = record.Timestamp + record.LatencyMs/1000
		contribution.CompletedMinute = floorUnix(contribution.CompletedAt, statisticsMinute)
		contribution.CompletedHour = floorUnix(contribution.CompletedAt, statisticsHour)
		normalizedModel := strings.ToLower(model)
		contribution.IsClaudeGPT = strings.Contains(normalizedModel, "claude") ||
			strings.Contains(normalizedModel, "gpt")
		contribution.IsGemini = strings.Contains(normalizedModel, "gemini")
	}
	return contribution
}

func (data *usageSummaryData) add(contribution *usageRecordContribution) {
	data.usageAggregate.add(&contribution.usageAggregate)

	api := data.APIs[contribution.APIKey]
	if api == nil {
		api = &usageAPIAggregate{Models: make(map[string]*usageAggregate)}
		data.APIs[contribution.APIKey] = api
	}
	api.usageAggregate.add(&contribution.usageAggregate)
	addAggregateMetric(api.Models, contribution.Model, &contribution.usageAggregate)

	if contribution.CredentialID != 0 {
		credential := data.Credentials[contribution.CredentialKey]
		if credential == nil {
			credential = &usageCredentialAggregate{
				Source:   contribution.Source,
				AuthType: contribution.AuthType,
			}
			data.Credentials[contribution.CredentialKey] = credential
		}
		credential.usageAggregate.add(&contribution.usageAggregate)
	}

	addAggregateMetric(data.Hours, contribution.Hour, &contribution.usageAggregate)
	addAggregateMetric(data.Days, contribution.Day, &contribution.usageAggregate)
}

func addAggregateMetric[K comparable](metrics map[K]*usageAggregate, key K, contribution *usageAggregate) {
	metric := metrics[key]
	if metric == nil {
		metric = &usageAggregate{}
		metrics[key] = metric
	}
	metric.add(contribution)
}

func (data *usageSummaryData) subtractBucket(
	minute int64,
	bucket *usageMinuteBucket,
	dimensions *usageDimensionTable,
) {
	hour := floorUnix(minute, statisticsHour)
	day := serverUsageDay(minute)
	for index := range bucket.Dimensions {
		dimension := &bucket.Dimensions[index]
		switch dimension.Kind {
		case usageBucketModel:
			data.usageAggregate.subtractBucket(&dimension.Metric)
			subtractBucketMetric(data.Hours, hour, &dimension.Metric)
			subtractBucketMetric(data.Days, day, &dimension.Metric)

			api := data.APIs[dimensions.value(dimension.Primary)]
			if api == nil {
				continue
			}
			api.usageAggregate.subtractBucket(&dimension.Metric)
			if model := api.Models[dimensions.value(dimension.Secondary)]; model != nil {
				model.subtractBucket(&dimension.Metric)
				if model.empty() {
					delete(api.Models, dimensions.value(dimension.Secondary))
				}
			}
			if api.usageAggregate.empty() {
				delete(data.APIs, dimensions.value(dimension.Primary))
			}
		case usageBucketCredential:
			key := dimensions.value(dimension.Primary)
			if credential := data.Credentials[key]; credential != nil {
				credential.usageAggregate.subtractBucket(&dimension.Metric)
				if credential.usageAggregate.empty() {
					delete(data.Credentials, key)
				}
			}
		}
	}
	if data.usageAggregate.empty() {
		*data = newUsageSummaryData()
	}
}

func subtractBucketMetric[K comparable](metrics map[K]*usageAggregate, key K, removed *usageBucketAggregate) {
	if metric := metrics[key]; metric != nil {
		metric.subtractBucket(removed)
		if metric.empty() {
			delete(metrics, key)
		}
	}
}

func (data *usageSummaryData) subtract(other *usageSummaryData) {
	data.usageAggregate.subtract(&other.usageAggregate)
	for apiKey, otherAPI := range other.APIs {
		api := data.APIs[apiKey]
		if api == nil {
			continue
		}
		api.usageAggregate.subtract(&otherAPI.usageAggregate)
		for modelName, otherModel := range otherAPI.Models {
			if model := api.Models[modelName]; model != nil {
				model.subtract(otherModel)
				if model.empty() {
					delete(api.Models, modelName)
				}
			}
		}
		if api.usageAggregate.empty() {
			delete(data.APIs, apiKey)
		}
	}
	for key, otherCredential := range other.Credentials {
		if credential := data.Credentials[key]; credential != nil {
			credential.usageAggregate.subtract(&otherCredential.usageAggregate)
			if credential.usageAggregate.empty() {
				delete(data.Credentials, key)
			}
		}
	}
	for hour, otherHour := range other.Hours {
		subtractAggregateMetric(data.Hours, hour, otherHour)
	}
	for day, otherDay := range other.Days {
		subtractAggregateMetric(data.Days, day, otherDay)
	}
	if data.usageAggregate.empty() {
		*data = newUsageSummaryData()
	}
}

func subtractAggregateMetric[K comparable](metrics map[K]*usageAggregate, key K, removed *usageAggregate) {
	if metric := metrics[key]; metric != nil {
		metric.subtract(removed)
		if metric.empty() {
			delete(metrics, key)
		}
	}
}

func (metric *usageAggregate) addRecord(record usageStatisticsRecord, cost float64) {
	metric.TotalRequests++
	if record.Failed {
		metric.FailureCount++
	} else {
		metric.SuccessCount++
	}
	metric.TotalTokens += record.TotalTokens
	metric.TotalCost += cost
	metric.LatencyTotalMs += record.LatencyMs
	metric.LatencySampleCount++
	if record.TTFTMs > 0 {
		metric.FirstByteLatencyTotalMs += record.TTFTMs
		metric.FirstByteLatencySampleCount++
	}
	generationMs := record.LatencyMs - record.TTFTMs
	if !record.Failed && record.OutputTokens > 0 && generationMs > 0 {
		metric.TPSOutputTokens += record.OutputTokens
		metric.TPSGenerationDurationMs += generationMs
		metric.TPSSampleCount++
	}
}

func (metric *usageAggregate) subtract(other *usageAggregate) {
	metric.TotalRequests -= other.TotalRequests
	metric.SuccessCount -= other.SuccessCount
	metric.FailureCount -= other.FailureCount
	metric.TotalTokens -= other.TotalTokens
	metric.TotalCost -= other.TotalCost
	metric.LatencyTotalMs -= other.LatencyTotalMs
	metric.LatencySampleCount -= other.LatencySampleCount
	metric.FirstByteLatencyTotalMs -= other.FirstByteLatencyTotalMs
	metric.FirstByteLatencySampleCount -= other.FirstByteLatencySampleCount
	metric.TPSOutputTokens -= other.TPSOutputTokens
	metric.TPSGenerationDurationMs -= other.TPSGenerationDurationMs
	metric.TPSSampleCount -= other.TPSSampleCount
	metric.normalizeCostResidual()
}

func (metric *usageAggregate) subtractBucket(other *usageBucketAggregate) {
	metric.TotalRequests -= other.TotalRequests
	metric.SuccessCount -= other.TotalRequests - other.FailureCount
	metric.FailureCount -= other.FailureCount
	metric.TotalTokens -= other.TotalTokens
	metric.TotalCost -= other.TotalCost
	metric.LatencyTotalMs -= other.LatencyTotalMs
	metric.LatencySampleCount -= other.TotalRequests
	metric.FirstByteLatencyTotalMs -= other.FirstByteLatencyTotalMs
	metric.FirstByteLatencySampleCount -= other.FirstByteLatencySampleCount
	metric.TPSOutputTokens -= other.TPSOutputTokens
	metric.TPSGenerationDurationMs -= other.TPSGenerationDurationMs
	metric.TPSSampleCount -= other.TPSSampleCount
	metric.normalizeCostResidual()
}

func (metric *usageAggregate) normalizeCostResidual() {
	if metric.TotalCost < 0 && metric.TotalCost > -1e-9 {
		metric.TotalCost = 0
	}
}

func (metric *usageAggregate) add(other *usageAggregate) {
	metric.TotalRequests += other.TotalRequests
	metric.SuccessCount += other.SuccessCount
	metric.FailureCount += other.FailureCount
	metric.TotalTokens += other.TotalTokens
	metric.TotalCost += other.TotalCost
	metric.LatencyTotalMs += other.LatencyTotalMs
	metric.LatencySampleCount += other.LatencySampleCount
	metric.FirstByteLatencyTotalMs += other.FirstByteLatencyTotalMs
	metric.FirstByteLatencySampleCount += other.FirstByteLatencySampleCount
	metric.TPSOutputTokens += other.TPSOutputTokens
	metric.TPSGenerationDurationMs += other.TPSGenerationDurationMs
	metric.TPSSampleCount += other.TPSSampleCount
}

func (metric *usageAggregate) empty() bool {
	return metric.TotalRequests <= 0
}
