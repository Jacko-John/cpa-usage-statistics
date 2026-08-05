package main

import (
	"slices"
	"testing"
	"time"
)

func TestCacheInsertsOutOfOrderWithoutTemporaryRebuild(t *testing.T) {
	base := time.Date(2026, 5, 2, 10, 0, 0, 0, time.UTC)
	record := func(id string, offset time.Duration) cachedUsageRecord {
		return cachedUsageRecord{detail: RequestDetail{ID: id, Timestamp: base.Add(offset)}}
	}
	ids := func(cache *usageRecordCache) []string {
		result := make([]string, cache.count)
		for i := range cache.count {
			result[i] = cache.record(i).detail.ID
		}
		return result
	}

	t.Run("available capacity", func(t *testing.T) {
		cache := newUsageRecordCache(4)
		cache.add(record("one", time.Minute))
		cache.add(record("three", 3*time.Minute))
		cache.add(record("two", 2*time.Minute))
		if want := []string{"one", "two", "three"}; !slices.Equal(ids(cache), want) {
			t.Fatalf("ids = %v, want %v", ids(cache), want)
		}
	})

	t.Run("full capacity", func(t *testing.T) {
		cache := newUsageRecordCache(3)
		cache.add(record("one", time.Minute))
		cache.add(record("three", 3*time.Minute))
		cache.add(record("four", 4*time.Minute))
		cache.add(record("two", 2*time.Minute))
		if want := []string{"two", "three", "four"}; !slices.Equal(ids(cache), want) {
			t.Fatalf("ids = %v, want %v", ids(cache), want)
		}
	})

	t.Run("older than full cache", func(t *testing.T) {
		cache := newUsageRecordCache(3)
		cache.add(record("one", time.Minute))
		cache.add(record("two", 2*time.Minute))
		cache.add(record("three", 3*time.Minute))
		cache.add(record("old", 0))
		if want := []string{"one", "two", "three"}; !slices.Equal(ids(cache), want) {
			t.Fatalf("ids = %v, want %v", ids(cache), want)
		}
		if cache.boundary == nil || !cache.boundary.Equal(base) {
			t.Fatalf("boundary = %v, want %v", cache.boundary, base)
		}
	})

	t.Run("wrapped shift", func(t *testing.T) {
		cache := newUsageRecordCache(4)
		cache.add(record("one", time.Minute))
		cache.add(record("two", 2*time.Minute))
		cache.add(record("three", 3*time.Minute))
		cache.add(record("four", 4*time.Minute))
		cache.add(record("five", 5*time.Minute))
		cache.add(record("three-half", 3*time.Minute+30*time.Second))
		if want := []string{"three", "three-half", "four", "five"}; !slices.Equal(ids(cache), want) {
			t.Fatalf("ids = %v, want %v", ids(cache), want)
		}
	})
}

func TestCacheSegmentsPreserveWrappedOrder(t *testing.T) {
	base := time.Date(2026, 5, 2, 10, 0, 0, 0, time.UTC)
	cache := newUsageRecordCache(3)
	for i, id := range []string{"one", "two", "three", "four"} {
		cache.add(cachedUsageRecord{detail: RequestDetail{
			ID:        id,
			Timestamp: base.Add(time.Duration(i) * time.Minute),
		}})
	}

	segments := cache.matching(QueryRange{})
	got := make([]string, 0, cache.count)
	for _, records := range [][]cachedUsageRecord{segments.first, segments.second} {
		for i := range records {
			got = append(got, records[i].detail.ID)
		}
	}
	if want := []string{"two", "three", "four"}; !slices.Equal(got, want) {
		t.Fatalf("ids = %v, want %v", got, want)
	}
}

func TestCacheRemoveCompactsWrappedBuffer(t *testing.T) {
	base := time.Date(2026, 5, 2, 10, 0, 0, 0, time.UTC)
	cache := newUsageRecordCache(4)
	for i, id := range []string{"one", "two", "three", "four", "five"} {
		cache.add(cachedUsageRecord{detail: RequestDetail{
			ID:        id,
			Timestamp: base.Add(time.Duration(i) * time.Minute),
		}})
	}

	cache.remove(map[string]struct{}{"three": {}, "five": {}})
	segments := cache.segments(0, cache.count)
	got := make([]string, 0, cache.count)
	for _, records := range [2][]cachedUsageRecord{segments.first, segments.second} {
		for i := range records {
			got = append(got, records[i].detail.ID)
		}
	}
	if want := []string{"two", "four"}; !slices.Equal(got, want) {
		t.Fatalf("ids = %v, want %v", got, want)
	}

	cache.add(cachedUsageRecord{detail: RequestDetail{ID: "six", Timestamp: base.Add(5 * time.Minute)}})
	if cache.count != 3 || cache.record(2).detail.ID != "six" {
		t.Fatalf("cache after append: count=%d last=%q", cache.count, cache.record(cache.count-1).detail.ID)
	}
}

func TestCacheDeleteBeforeMovesWrappedStart(t *testing.T) {
	base := time.Date(2026, 5, 2, 10, 0, 0, 0, time.UTC)
	cache := newUsageRecordCache(4)
	for i, id := range []string{"one", "two", "three", "four", "five"} {
		cache.add(cachedUsageRecord{detail: RequestDetail{
			ID:        id,
			Timestamp: base.Add(time.Duration(i) * time.Minute),
		}})
	}

	cache.deleteBefore(base.Add(3 * time.Minute))
	if cache.count != 2 {
		t.Fatalf("count = %d, want 2", cache.count)
	}
	if cache.record(0).detail.ID != "four" || cache.record(1).detail.ID != "five" {
		t.Fatalf("remaining ids = %q, %q", cache.record(0).detail.ID, cache.record(1).detail.ID)
	}
	if cache.boundary != nil {
		t.Fatalf("boundary = %v, want nil", cache.boundary)
	}

	cache.deleteBefore(base.Add(5 * time.Minute))
	if cache.count != 0 || cache.start != 0 {
		t.Fatalf("empty cache state: count=%d start=%d", cache.count, cache.start)
	}
}
