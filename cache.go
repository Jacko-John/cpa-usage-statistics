package main

import (
	"sort"
	"time"
)

// cachedUsageRecord contains the fields needed to build the grouped API result.
type cachedUsageRecord struct {
	groupKey string
	modelKey string
	detail   RequestDetail
}

// cachedUsageSegments represents one logical cache range as up to two physical
// slices because a ring-buffer range can wrap around the end of entries.
type cachedUsageSegments struct {
	first  []cachedUsageRecord
	second []cachedUsageRecord
}

// usageRecordCache is a fixed-size chronological ring buffer. A nil boundary
// means the cache contains every database record. A non-nil boundary gives
// SQLite records at or before that time and the cache records strictly after it.
type usageRecordCache struct {
	entries  []cachedUsageRecord
	start    int
	count    int
	capacity int
	boundary *time.Time
}

func newUsageRecordCache(capacity int) *usageRecordCache {
	if capacity < 0 {
		capacity = 0
	}
	return &usageRecordCache{
		entries:  make([]cachedUsageRecord, capacity),
		capacity: capacity,
	}
}

func (c *usageRecordCache) reset(records []cachedUsageRecord, boundary *time.Time) {
	c.start = 0
	c.count = min(len(records), c.capacity)
	c.boundary = boundary
	copy(c.entries[0:c.count], records[len(records)-c.count:])
}

func (c *usageRecordCache) physicalIndex(index int) int {
	position := c.start + index
	if position >= c.capacity {
		position -= c.capacity
	}
	return position
}

func (c *usageRecordCache) record(index int) *cachedUsageRecord {
	return &c.entries[c.physicalIndex(index)]
}

func (c *usageRecordCache) add(record cachedUsageRecord) {
	if c == nil || c.capacity == 0 {
		return
	}
	// 缓存只负责 timestamp > boundary 部分，尽管缓存里面可能存在 timestamp == boundary
	if c.boundary != nil && !record.detail.Timestamp.After(*c.boundary) {
		return
	}
	if c.count == 0 {
		c.entries[0] = record
		c.start = 0
		c.count = 1
		return
	}

	last := c.record(c.count - 1)
	if !cachedRecordLess(&record, last) {
		c.appendNewest(record)
		return
	}
	c.insertOutOfOrder(record)
}

// insertOutOfOrder inserts directly into the ring buffer without allocating a
// temporary copy. The common chronological append path remains in add.
func (c *usageRecordCache) insertOutOfOrder(record cachedUsageRecord) {
	index := sort.Search(c.count, func(i int) bool {
		return !cachedRecordLess(c.record(i), &record)
	})

	if c.count == c.capacity {
		// A record older than the whole cache belongs to SQLite and becomes the
		// new database/cache boundary.
		if index == 0 {
			c.markIncomplete(record.detail.Timestamp)
			return
		}

		// Evict the oldest cached record before opening the insertion position.
		evictedTimestamp := c.record(0).detail.Timestamp
		c.start = c.physicalIndex(1)
		c.count--
		index--
		c.markIncomplete(evictedTimestamp)
	}

	c.shiftRight(index)
	c.setRecord(index, record)
	c.count++
}

// shiftRight opens one logical position at index using at most two overlapping
// slice copies, including ranges that wrap around the physical buffer boundary.
func (c *usageRecordCache) shiftRight(index int) {
	length := c.count - index
	if length <= 0 {
		return
	}

	start := c.physicalIndex(index)
	end := start + length
	switch {
	case end < c.capacity:
		copy(c.entries[start+1:end+1], c.entries[start:end])
	case end == c.capacity:
		c.entries[0] = c.entries[c.capacity-1]
		copy(c.entries[start+1:c.capacity], c.entries[start:c.capacity-1])
	default:
		headLength := end - c.capacity
		copy(c.entries[1:headLength+1], c.entries[:headLength])
		c.entries[0] = c.entries[c.capacity-1]
		copy(c.entries[start+1:c.capacity], c.entries[start:c.capacity-1])
	}
}

func (c *usageRecordCache) setRecord(index int, record cachedUsageRecord) {
	c.entries[c.physicalIndex(index)] = record
}

func (c *usageRecordCache) appendNewest(record cachedUsageRecord) {
	if c.count < c.capacity {
		index := c.physicalIndex(c.count)
		c.entries[index] = record
		c.count++
		return
	}

	evictedTimestamp := c.entries[c.start].detail.Timestamp
	c.entries[c.start] = record
	c.start = c.physicalIndex(1)
	c.markIncomplete(evictedTimestamp)
}

func (c *usageRecordCache) markIncomplete(boundary time.Time) {
	if c.boundary == nil || boundary.After(*c.boundary) {
		c.boundary = &boundary
	}
}

func (c *usageRecordCache) remove(ids map[string]struct{}) {
	if c == nil || c.count == 0 || len(ids) == 0 {
		return
	}

	oldCount := c.count
	kept := 0
	writeCursor := c.start
	segments := c.segments(0, oldCount)
	recordSegments := [2][]cachedUsageRecord{segments.first, segments.second}
	for segmentIndex := range recordSegments {
		records := recordSegments[segmentIndex]
		for i := range records {
			record := &records[i]
			if _, remove := ids[record.detail.ID]; remove {
				continue
			}
			target := &c.entries[writeCursor]
			if target != record {
				*target = *record
			}
			writeCursor++
			if writeCursor == c.capacity {
				writeCursor = 0
			}
			kept++
		}
	}
	c.count = kept
}

func (c *usageRecordCache) deleteBefore(cutoff time.Time) {
	if c == nil {
		return
	}
	if c.boundary != nil && cutoff.After(*c.boundary) {
		c.boundary = nil
	}
	if c.count == 0 {
		return
	}

	removed := sort.Search(c.count, func(i int) bool {
		return !c.record(i).detail.Timestamp.Before(cutoff)
	})
	if removed == 0 {
		return
	}
	c.start = c.physicalIndex(removed)
	c.count -= removed
	if c.count == 0 {
		c.start = 0
	}
}

// matching: rng -> [start, end)
func (c *usageRecordCache) matching(rng QueryRange) cachedUsageSegments {
	if c == nil || c.count == 0 {
		return cachedUsageSegments{}
	}
	first := 0
	if c.boundary != nil {
		first = sort.Search(c.count, func(i int) bool {
			return c.record(i).detail.Timestamp.After(*c.boundary)
		})
	}
	if rng.Start != nil && !rng.Start.IsZero() {
		start := sort.Search(c.count, func(i int) bool {
			return !c.record(i).detail.Timestamp.Before(*rng.Start)
		})
		first = max(first, start)
	}
	last := c.count
	if rng.End != nil && !rng.End.IsZero() {
		last = sort.Search(c.count, func(i int) bool {
			return !c.record(i).detail.Timestamp.Before(*rng.End)
		})
	}
	if first >= last {
		return cachedUsageSegments{}
	}
	return c.segments(first, last)
}

// segments maps a logical half-open range to at most two direct slices of the
// ring buffer, preserving chronological order without allocation or copying.
func (c *usageRecordCache) segments(first, last int) cachedUsageSegments {
	length := last - first
	physicalStart := c.physicalIndex(first)
	firstLength := min(length, c.capacity-physicalStart)
	segments := cachedUsageSegments{
		first: c.entries[physicalStart : physicalStart+firstLength],
	}
	if firstLength < length {
		segments.second = c.entries[:length-firstLength]
	}
	return segments
}

func cachedRecordLess(left, right *cachedUsageRecord) bool {
	if left.detail.Timestamp.Equal(right.detail.Timestamp) {
		return left.detail.ID < right.detail.ID
	}
	return left.detail.Timestamp.Before(right.detail.Timestamp)
}
