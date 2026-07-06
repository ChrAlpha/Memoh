package agent

import (
	"testing"
	"time"

	"github.com/memohai/memoh/internal/contextfrag"
)

func TestPrefixCacheTrackerObserveStoresAndReturnsPrevious(t *testing.T) {
	t.Parallel()

	tracker := newPrefixCacheTracker()
	now := time.Unix(1000, 0)

	if _, ok := tracker.observe("bot:session", "hash-1", "model-a", now); ok {
		t.Fatal("first observation must have no previous entry")
	}
	prev, ok := tracker.observe("bot:session", "hash-2", "model-a", now.Add(time.Minute))
	if !ok || prev.hash != "hash-1" || prev.model != "model-a" || !prev.at.Equal(now) {
		t.Fatalf("previous entry = %+v ok=%v, want hash-1/model-a at t0", prev, ok)
	}
	if _, ok := tracker.observe("bot:other", "hash-3", "model-a", now); ok {
		t.Fatal("different session key must not share entries")
	}
}

func TestCompareCachePrefixOutcomes(t *testing.T) {
	t.Parallel()

	now := time.Unix(2000, 0)
	prev := prefixCacheEntry{hash: "same", model: "model-a", at: now.Add(-time.Minute)}
	ttl := 5 * time.Minute

	for _, tc := range []struct {
		name      string
		prev      prefixCacheEntry
		hasPrev   bool
		hash      string
		model     string
		firstRead int
		want      string
	}{
		{name: "first observation", hash: "same", model: "model-a", want: contextfrag.CacheOutcomeFirstObservation},
		{name: "hit", prev: prev, hasPrev: true, hash: "same", model: "model-a", firstRead: 100, want: contextfrag.CacheOutcomeHit},
		{name: "miss same prefix", prev: prev, hasPrev: true, hash: "same", model: "model-a", want: contextfrag.CacheOutcomeMissSamePrefix},
		{name: "prefix changed", prev: prev, hasPrev: true, hash: "different", model: "model-a", want: contextfrag.CacheOutcomePrefixChanged},
		{
			name:    "expired",
			prev:    prefixCacheEntry{hash: "same", model: "model-a", at: now.Add(-10 * time.Minute)},
			hasPrev: true,
			hash:    "same",
			model:   "model-a",
			want:    contextfrag.CacheOutcomeExpired,
		},
		{
			name:    "model changed, hash unchanged",
			prev:    prev,
			hasPrev: true,
			hash:    "same",
			model:   "model-b",
			want:    contextfrag.CacheOutcomeModelChanged,
		},
		{
			name:    "model switched back is still a change relative to immediately-preceding call",
			prev:    prefixCacheEntry{hash: "same", model: "model-b", at: now.Add(-time.Minute)},
			hasPrev: true,
			hash:    "same",
			model:   "model-a",
			want:    contextfrag.CacheOutcomeModelChanged,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := compareCachePrefix(tc.prev, tc.hasPrev, tc.hash, tc.model, tc.firstRead, now, ttl)
			if got.Outcome != tc.want {
				t.Fatalf("outcome = %q, want %q", got.Outcome, tc.want)
			}
			if tc.hasPrev && got.PrevAgeMs <= 0 {
				t.Fatalf("prev age = %d, want positive", got.PrevAgeMs)
			}
		})
	}
}

func TestPrefixCacheTrackerEvictsAtCapacity(t *testing.T) {
	t.Parallel()

	tracker := newPrefixCacheTracker()
	now := time.Unix(3000, 0)
	for i := 0; i < prefixCacheTrackerCap+10; i++ {
		tracker.observe(sessionKeyForTest(i), "hash", "model-a", now.Add(time.Duration(i)*time.Second))
	}
	if size := tracker.size(); size > prefixCacheTrackerCap {
		t.Fatalf("tracker size = %d, want bounded by %d", size, prefixCacheTrackerCap)
	}
}

func sessionKeyForTest(i int) string {
	return "bot:" + string(rune('a'+i%26)) + string(rune('0'+i%10)) + "-" + time.Duration(i).String()
}
