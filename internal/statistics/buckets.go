package statistics

import (
	"strings"
)

type usageDimensionID uint32

type usageDimensionTable struct {
	IDs        map[string]usageDimensionID
	Values     []string
	References []int64
	Free       []usageDimensionID
}

// usageBucketAggregate 只保存分钟桶未来执行扣减时不可推导的字段。
type usageBucketAggregate struct {
	TotalRequests               int64
	FailureCount                int64
	TotalTokens                 int64
	TotalCost                   float64
	LatencyTotalMs              int64
	FirstByteLatencyTotalMs     int64
	FirstByteLatencySampleCount int64
	TPSOutputTokens             int64
	TPSGenerationDurationMs     int64
	TPSSampleCount              int64
}

type usageBucketDimensionKind uint8

const (
	usageBucketModel usageBucketDimensionKind = iota + 1
	usageBucketCredential
)

type usageBucketDimension struct {
	Metric      usageBucketAggregate
	Primary     usageDimensionID
	Secondary   usageDimensionID
	Kind        usageBucketDimensionKind
	ServiceTier string
}

// usageMinuteBucket 使用扁平维度切片，避免每分钟建立多层 Map 和指针对象。
type usageMinuteBucket struct {
	Dimensions []usageBucketDimension
}

const credentialSourcePrefix = "source:"

type credentialIdentityReference struct {
	ID      usageDimensionID
	Records int64
}

type credentialCategory uint8

const (
	credentialCategoryAll credentialCategory = iota
	credentialCategoryClaudeGPT
	credentialCategoryGemini
)

// credentialWindowMetric 省略可由请求数和失败数推导的成功数。
type credentialWindowMetric struct {
	Requests     int64
	FailureCount int64
	Tokens       int64
	Cost         float64
}

type credentialTimeMetric struct {
	Metric   credentialWindowMetric
	Identity usageDimensionID
	Category credentialCategory
}

// credentialTimeBucket 使用紧凑切片保存同一分钟或小时内的凭证指标。
type credentialTimeBucket struct {
	Metrics []credentialTimeMetric
}

func newUsageDimensionTable() usageDimensionTable {
	return usageDimensionTable{IDs: make(map[string]usageDimensionID)}
}

func (table *usageDimensionTable) intern(value string) (usageDimensionID, string) {
	if id, ok := table.IDs[value]; ok {
		return id, table.value(id)
	}
	value = strings.Clone(value)
	var id usageDimensionID
	if len(table.Free) > 0 {
		last := len(table.Free) - 1
		id = table.Free[last]
		table.Free = table.Free[:last]
		table.Values[int(id)-1] = value
	} else {
		id = usageDimensionID(len(table.Values) + 1)
		table.Values = append(table.Values, value)
		table.References = append(table.References, 0)
	}
	table.IDs[value] = id
	return id, value
}

func (table *usageDimensionTable) retain(id usageDimensionID) {
	if id != 0 {
		table.References[int(id)-1]++
	}
}

func (table *usageDimensionTable) release(value string, records int64) {
	id, ok := table.IDs[value]
	if !ok {
		return
	}
	index := int(id) - 1
	table.References[index] -= records
	if table.References[index] > 0 {
		return
	}
	delete(table.IDs, value)
	table.Values[index] = ""
	table.References[index] = 0
	table.Free = append(table.Free, id)
	if len(table.IDs) == 0 {
		*table = newUsageDimensionTable()
	}
}

func (table *usageDimensionTable) lookup(value string) (usageDimensionID, bool) {
	id, ok := table.IDs[value]
	return id, ok
}

func (table *usageDimensionTable) value(id usageDimensionID) string {
	if id == 0 {
		return ""
	}
	return table.Values[int(id)-1]
}

func newUsageMinuteBucket(contribution *usageRecordContribution) *usageMinuteBucket {
	capacity := 1
	if contribution.CredentialID != 0 {
		capacity++
	}
	bucket := &usageMinuteBucket{
		Dimensions: make([]usageBucketDimension, 0, capacity),
	}
	bucket.add(contribution)
	return bucket
}

func (bucket *usageMinuteBucket) add(contribution *usageRecordContribution) {
	modelIndex := -1
	credentialIndex := -1
	for index := range bucket.Dimensions {
		dimension := &bucket.Dimensions[index]
		switch dimension.Kind {
		case usageBucketModel:
			if dimension.Primary == contribution.APIID && dimension.Secondary == contribution.ModelID && dimension.ServiceTier == contribution.ServiceTier {
				modelIndex = index
			}
		case usageBucketCredential:
			if dimension.Primary == contribution.CredentialID {
				credentialIndex = index
			}
		}
	}

	if modelIndex >= 0 {
		bucket.Dimensions[modelIndex].Metric.add(&contribution.UsageAggregate)
	} else {
		bucket.Dimensions = append(bucket.Dimensions, usageBucketDimension{
			Metric:      newUsageBucketAggregate(&contribution.UsageAggregate),
			Primary:     contribution.APIID,
			Secondary:   contribution.ModelID,
			Kind:        usageBucketModel,
			ServiceTier: strings.Clone(contribution.ServiceTier),
		})
	}
	if contribution.CredentialID == 0 {
		return
	}
	if credentialIndex >= 0 {
		bucket.Dimensions[credentialIndex].Metric.add(&contribution.UsageAggregate)
		return
	}
	bucket.Dimensions = append(bucket.Dimensions, usageBucketDimension{
		Metric:  newUsageBucketAggregate(&contribution.UsageAggregate),
		Primary: contribution.CredentialID,
		Kind:    usageBucketCredential,
	})
}

func (state *Accumulator) addCredentialWindowContribution(contribution *usageRecordContribution, now int64) {
	if contribution.CredentialID == 0 || contribution.CompletedAt < now-credentialHistoryRange || contribution.CompletedAt > now {
		return
	}
	minuteBucket := state.CredentialMinutes[contribution.CompletedMinute]
	if minuteBucket == nil {
		minuteBucket = newCredentialTimeBucket(contribution)
		state.CredentialMinutes[contribution.CompletedMinute] = minuteBucket
	} else {
		minuteBucket.addContribution(contribution)
	}
	hourBucket := state.CredentialHours[contribution.CompletedHour]
	if hourBucket == nil {
		hourBucket = newCredentialTimeBucket(contribution)
		state.CredentialHours[contribution.CompletedHour] = hourBucket
	} else {
		hourBucket.addContribution(contribution)
	}
}

func newCredentialTimeBucket(contribution *usageRecordContribution) *credentialTimeBucket {
	capacity := 1
	if contribution.IsClaudeGPT {
		capacity++
	}
	if contribution.IsGemini {
		capacity++
	}
	bucket := &credentialTimeBucket{
		Metrics: make([]credentialTimeMetric, 0, capacity),
	}
	bucket.addContribution(contribution)
	return bucket
}

func (bucket *credentialTimeBucket) addContribution(contribution *usageRecordContribution) {
	bucket.add(contribution.CredentialID, credentialCategoryAll, &contribution.UsageAggregate)
	if contribution.IsClaudeGPT {
		bucket.add(contribution.CredentialID, credentialCategoryClaudeGPT, &contribution.UsageAggregate)
	}
	if contribution.IsGemini {
		bucket.add(contribution.CredentialID, credentialCategoryGemini, &contribution.UsageAggregate)
	}
}

func (bucket *credentialTimeBucket) add(
	identity usageDimensionID,
	category credentialCategory,
	contribution *UsageAggregate,
) {
	for index := range bucket.Metrics {
		metric := &bucket.Metrics[index]
		if metric.Identity == identity && metric.Category == category {
			metric.Metric.add(contribution)
			return
		}
	}
	metric := credentialTimeMetric{Identity: identity, Category: category}
	metric.Metric.add(contribution)
	bucket.Metrics = append(bucket.Metrics, metric)
}

func (state *Accumulator) advance(now int64) {
	minuteNow := FloorUnix(now, StatisticsMinute)
	if minuteNow <= state.AdvancedMinute {
		return
	}

	for index, seconds := range statisticsRanges {
		rng := &state.Ranges[index]
		cutoff := FloorUnix(now-seconds, StatisticsMinute)
		for minute := rng.CutoffMinute; minute < cutoff; minute += StatisticsMinute {
			if bucket := state.MinuteBuckets[minute]; bucket != nil {
				rng.Data.subtractBucket(minute, bucket, &state.Dimensions)
			}
		}
		rng.CutoffMinute = cutoff
	}

	oldestMinute := FloorUnix(now-credentialHistoryRange, StatisticsMinute)
	for minute := state.OldestMinute; minute < oldestMinute; minute += StatisticsMinute {
		delete(state.MinuteBuckets, minute)
		delete(state.CredentialMinutes, minute)
	}
	state.OldestMinute = max(state.OldestMinute, oldestMinute)

	oldestHour := FloorUnix(now-credentialHistoryRange, StatisticsHour)
	for hour := state.OldestHour; hour < oldestHour; hour += StatisticsHour {
		delete(state.CredentialHours, hour)
	}
	state.OldestHour = max(state.OldestHour, oldestHour)
	state.AdvancedMinute = minuteNow
}

func (state *Accumulator) subtract(removed *Accumulator) {
	state.All.subtract(&removed.All)
	for index := range state.Ranges {
		state.Ranges[index].Data.subtract(&removed.Ranges[index].Data)
	}
	for minute, removedBucket := range removed.MinuteBuckets {
		bucket := state.MinuteBuckets[minute]
		if bucket == nil {
			continue
		}
		bucket.subtract(removedBucket, &state.Dimensions, &removed.Dimensions)
		if bucket.empty() {
			delete(state.MinuteBuckets, minute)
		}
	}
	for minute, removedBucket := range removed.CredentialMinutes {
		bucket := state.CredentialMinutes[minute]
		if bucket == nil {
			continue
		}
		bucket.subtract(removedBucket, &state.Dimensions, &removed.Dimensions)
		if bucket.empty() {
			delete(state.CredentialMinutes, minute)
		}
	}
	for hour, removedBucket := range removed.CredentialHours {
		bucket := state.CredentialHours[hour]
		if bucket == nil {
			continue
		}
		bucket.subtract(removedBucket, &state.Dimensions, &removed.Dimensions)
		if bucket.empty() {
			delete(state.CredentialHours, hour)
		}
	}
	if len(removed.MinuteBuckets) > 0 && len(state.MinuteBuckets) == 0 {
		state.MinuteBuckets = make(map[int64]*usageMinuteBucket)
	}
	if len(removed.CredentialMinutes) > 0 && len(state.CredentialMinutes) == 0 {
		state.CredentialMinutes = make(map[int64]*credentialTimeBucket)
	}
	if len(removed.CredentialHours) > 0 && len(state.CredentialHours) == 0 {
		state.CredentialHours = make(map[int64]*credentialTimeBucket)
	}
	state.releaseDimensions(&removed.All)
}

func (state *Accumulator) releaseDimensions(removed *UsageSummaryData) {
	for apiKey, api := range removed.APIs {
		state.Dimensions.release(apiKey, api.TotalRequests)
		for model, metric := range api.Models {
			state.Dimensions.release(model, metric.TotalRequests)
		}
	}
	for identity, credential := range removed.Credentials {
		state.Dimensions.release(identity, credential.TotalRequests)
		source := normalizeCredentialSource(credential.Source)
		if source == "" {
			continue
		}
		reference, exists := state.CredentialIdentities[source]
		if !exists {
			continue
		}
		reference.Records -= credential.TotalRequests
		if reference.Records <= 0 {
			delete(state.CredentialIdentities, source)
		} else {
			state.CredentialIdentities[source] = reference
		}
	}
	if len(removed.Credentials) > 0 && len(state.CredentialIdentities) == 0 {
		state.CredentialIdentities = make(map[string]credentialIdentityReference)
	}
}

func (bucket *usageMinuteBucket) subtract(
	removed *usageMinuteBucket,
	dimensions, removedDimensions *usageDimensionTable,
) {
	for removedIndex := range removed.Dimensions {
		removedDimension := &removed.Dimensions[removedIndex]
		primary, ok := dimensions.lookup(removedDimensions.value(removedDimension.Primary))
		if !ok {
			continue
		}
		secondary := usageDimensionID(0)
		if removedDimension.Secondary != 0 {
			secondary, ok = dimensions.lookup(removedDimensions.value(removedDimension.Secondary))
			if !ok {
				continue
			}
		}
		for index := range bucket.Dimensions {
			dimension := &bucket.Dimensions[index]
			if dimension.Kind != removedDimension.Kind ||
				dimension.Primary != primary ||
				dimension.Secondary != secondary ||
				dimension.ServiceTier != removedDimension.ServiceTier {
				continue
			}
			dimension.Metric.subtract(&removedDimension.Metric)
			if dimension.Metric.empty() {
				bucket.Dimensions = removeUsageBucketDimension(bucket.Dimensions, index)
			}
			break
		}
	}
}

func removeUsageBucketDimension(
	dimensions []usageBucketDimension,
	index int,
) []usageBucketDimension {
	copy(dimensions[index:], dimensions[index+1:])
	last := len(dimensions) - 1
	dimensions[last] = usageBucketDimension{}
	return dimensions[:last]
}

func (bucket *usageMinuteBucket) empty() bool {
	return len(bucket.Dimensions) == 0
}

func (bucket *credentialTimeBucket) subtract(
	removed *credentialTimeBucket,
	dimensions, removedDimensions *usageDimensionTable,
) {
	for removedIndex := range removed.Metrics {
		removedMetric := &removed.Metrics[removedIndex]
		identity, ok := dimensions.lookup(removedDimensions.value(removedMetric.Identity))
		if !ok {
			continue
		}
		for index := range bucket.Metrics {
			metric := &bucket.Metrics[index]
			if metric.Identity != identity || metric.Category != removedMetric.Category {
				continue
			}
			metric.Metric.subtract(&removedMetric.Metric)
			if metric.Metric.empty() {
				bucket.Metrics = removeCredentialTimeMetric(bucket.Metrics, index)
			}
			break
		}
	}
}

func removeCredentialTimeMetric(
	metrics []credentialTimeMetric,
	index int,
) []credentialTimeMetric {
	copy(metrics[index:], metrics[index+1:])
	last := len(metrics) - 1
	metrics[last] = credentialTimeMetric{}
	return metrics[:last]
}

func (bucket *credentialTimeBucket) empty() bool {
	return len(bucket.Metrics) == 0
}

func newUsageBucketAggregate(contribution *UsageAggregate) usageBucketAggregate {
	return usageBucketAggregate{
		TotalRequests:               contribution.TotalRequests,
		FailureCount:                contribution.FailureCount,
		TotalTokens:                 contribution.TotalTokens,
		TotalCost:                   contribution.TotalCost,
		LatencyTotalMs:              contribution.LatencyTotalMs,
		FirstByteLatencyTotalMs:     contribution.FirstByteLatencyTotalMs,
		FirstByteLatencySampleCount: contribution.FirstByteLatencySampleCount,
		TPSOutputTokens:             contribution.TPSOutputTokens,
		TPSGenerationDurationMs:     contribution.TPSGenerationDurationMs,
		TPSSampleCount:              contribution.TPSSampleCount,
	}
}

func (metric *usageBucketAggregate) add(contribution *UsageAggregate) {
	metric.TotalRequests += contribution.TotalRequests
	metric.FailureCount += contribution.FailureCount
	metric.TotalTokens += contribution.TotalTokens
	metric.TotalCost += contribution.TotalCost
	metric.LatencyTotalMs += contribution.LatencyTotalMs
	metric.FirstByteLatencyTotalMs += contribution.FirstByteLatencyTotalMs
	metric.FirstByteLatencySampleCount += contribution.FirstByteLatencySampleCount
	metric.TPSOutputTokens += contribution.TPSOutputTokens
	metric.TPSGenerationDurationMs += contribution.TPSGenerationDurationMs
	metric.TPSSampleCount += contribution.TPSSampleCount
}

func (metric *usageBucketAggregate) subtract(other *usageBucketAggregate) {
	metric.TotalRequests -= other.TotalRequests
	metric.FailureCount -= other.FailureCount
	metric.TotalTokens -= other.TotalTokens
	metric.TotalCost -= other.TotalCost
	metric.LatencyTotalMs -= other.LatencyTotalMs
	metric.FirstByteLatencyTotalMs -= other.FirstByteLatencyTotalMs
	metric.FirstByteLatencySampleCount -= other.FirstByteLatencySampleCount
	metric.TPSOutputTokens -= other.TPSOutputTokens
	metric.TPSGenerationDurationMs -= other.TPSGenerationDurationMs
	metric.TPSSampleCount -= other.TPSSampleCount
	if metric.TotalCost < 0 && metric.TotalCost > -1e-9 {
		metric.TotalCost = 0
	}
}

func (metric *usageBucketAggregate) empty() bool {
	return metric.TotalRequests <= 0
}

func (metric *credentialWindowMetric) add(contribution *UsageAggregate) {
	metric.Requests += contribution.TotalRequests
	metric.FailureCount += contribution.FailureCount
	metric.Tokens += contribution.TotalTokens
	metric.Cost += contribution.TotalCost
}

func (metric *credentialWindowMetric) addMetric(other *credentialWindowMetric) {
	metric.Requests += other.Requests
	metric.FailureCount += other.FailureCount
	metric.Tokens += other.Tokens
	metric.Cost += other.Cost
}

func (metric *credentialWindowMetric) subtract(other *credentialWindowMetric) {
	metric.Requests -= other.Requests
	metric.FailureCount -= other.FailureCount
	metric.Tokens -= other.Tokens
	metric.Cost -= other.Cost
	if metric.Cost < 0 && metric.Cost > -1e-9 {
		metric.Cost = 0
	}
}

func (metric credentialWindowMetric) summary() CredentialWindowSummary {
	return CredentialWindowSummary{
		Requests:     metric.Requests,
		SuccessCount: metric.Requests - metric.FailureCount,
		FailureCount: metric.FailureCount,
		Tokens:       metric.Tokens,
		Cost:         metric.Cost,
	}
}

func (metric credentialWindowMetric) empty() bool {
	return metric.Requests <= 0
}

// apiWindows 一次遍历相关分钟桶；超出缓存的窗口整段交给持久化查询。
func (state *Accumulator) apiWindows(queries []APIWindowQuery, now int64) ([]APIWindowSummary, []int) {
	windows := make([]apiWindowAccumulator, len(queries))
	var missing []int
	byAPI := make(map[usageDimensionID][]int)
	limit := FloorUnix(now, StatisticsMinute) + StatisticsMinute
	start, end := limit, int64(0)
	for i, query := range queries {
		until := min(query.End, limit)
		if query.Start >= until {
			continue
		}
		if query.Start < state.OldestMinute {
			missing = append(missing, i)
			continue
		}
		id, ok := state.Dimensions.lookup(strings.TrimSpace(query.APIKey))
		if !ok {
			continue
		}
		byAPI[id] = append(byAPI[id], i)
		start = min(start, query.Start)
		end = max(end, until)
	}
	for minute := start; minute < end; minute += StatisticsMinute {
		bucket := state.MinuteBuckets[minute]
		if bucket == nil {
			continue
		}
		for _, dimension := range bucket.Dimensions {
			if dimension.Kind != usageBucketModel {
				continue
			}
			for _, i := range byAPI[dimension.Primary] {
				query := queries[i]
				if minute < query.Start || minute >= query.End {
					continue
				}
				model := state.Dimensions.value(dimension.Secondary)
				var unpriced int64
				if _, priced := state.Pricing.Prices[model]; !priced {
					unpriced = dimension.Metric.TotalRequests
				}
				windows[i].add(model, dimension.ServiceTier, dimension.Metric.TotalRequests, dimension.Metric.TotalTokens, dimension.Metric.TotalCost, unpriced)
			}
		}
	}
	results := make([]APIWindowSummary, len(windows))
	for i := range windows {
		results[i] = windows[i].summary()
	}
	return results, missing
}

func (state *Accumulator) credentialWindow(query CredentialWindowQuery) CredentialWindowSummary {
	start := query.Start - StatisticsMinute
	end := query.End + StatisticsMinute
	if start > end {
		return CredentialWindowSummary{}
	}
	source := normalizeCredentialSource(query.Source)
	if source == "" {
		return CredentialWindowSummary{}
	}
	reference := state.CredentialIdentities[source]
	if reference.ID == 0 {
		return CredentialWindowSummary{}
	}
	identity := reference.ID
	category := credentialCategoryForQuery(query.Category)
	var result credentialWindowMetric

	firstFullHour := FloorUnix(start, StatisticsHour)
	if firstFullHour < start {
		firstFullHour += StatisticsHour
	}
	lastFullHour := FloorUnix(end+1, StatisticsHour)

	for minute := FloorUnix(start, StatisticsMinute); minute < firstFullHour && minute <= end; minute += StatisticsMinute {
		if bucket := state.CredentialMinutes[minute]; bucket != nil {
			if metric := bucket.metric(identity, category); metric != nil {
				result.addMetric(metric)
			}
		}
	}
	for hour := firstFullHour; hour < lastFullHour; hour += StatisticsHour {
		if bucket := state.CredentialHours[hour]; bucket != nil {
			if metric := bucket.metric(identity, category); metric != nil {
				result.addMetric(metric)
			}
		}
	}
	for minute := max(firstFullHour, lastFullHour); minute <= end; minute += StatisticsMinute {
		if bucket := state.CredentialMinutes[minute]; bucket != nil {
			if metric := bucket.metric(identity, category); metric != nil {
				result.addMetric(metric)
			}
		}
	}
	return result.summary()
}

func (bucket *credentialTimeBucket) metric(
	identity usageDimensionID,
	category credentialCategory,
) *credentialWindowMetric {
	for index := range bucket.Metrics {
		metric := &bucket.Metrics[index]
		if metric.Identity == identity && metric.Category == category {
			return &metric.Metric
		}
	}
	return nil
}

func credentialCategoryForQuery(category string) credentialCategory {
	switch strings.ToLower(strings.TrimSpace(category)) {
	case "claude_gpt":
		return credentialCategoryClaudeGPT
	case "gemini":
		return credentialCategoryGemini
	default:
		return credentialCategoryAll
	}
}

func (state *Accumulator) internCredentialSource(
	source string,
) (usageDimensionID, string, string) {
	source = normalizeCredentialSource(source)
	if source == "" {
		return 0, "", ""
	}
	if reference, exists := state.CredentialIdentities[source]; exists {
		identity := state.Dimensions.value(reference.ID)
		reference.Records++
		state.CredentialIdentities[source] = reference
		return reference.ID, identity, identity[len(credentialSourcePrefix):]
	}
	id, identity := state.Dimensions.intern(credentialSourcePrefix + source)
	source = identity[len(credentialSourcePrefix):]
	state.CredentialIdentities[source] = credentialIdentityReference{ID: id, Records: 1}
	return id, identity, source
}

func normalizeCredentialSource(source string) string {
	source = strings.TrimSpace(source)
	if after, ok := strings.CutPrefix(source, "t:"); ok {
		source = strings.TrimSpace(after)
	}
	return source
}

func FloorUnix(value, size int64) int64 {
	if value >= 0 {
		return value / size * size
	}
	return (value - size + 1) / size * size
}
