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

	if _, ok := tracker.observe("bot:session", 2, "hash-1", "model-a", now); ok {
		t.Fatal("first observation must have no previous entry")
	}
	prev, ok := tracker.observe("bot:session", 4, "hash-2", "model-a", now.Add(time.Minute))
	if !ok || prev.hash != "hash-1" || prev.model != "model-a" || prev.stableCount != 2 || !prev.at.Equal(now) {
		t.Fatalf("previous entry = %+v ok=%v, want hash-1/model-a/stableCount=2 at t0", prev, ok)
	}
	if _, ok := tracker.observe("bot:other", 1, "hash-3", "model-a", now); ok {
		t.Fatal("different session key must not share entries")
	}
}

func TestPrefixCacheTrackerPeek(t *testing.T) {
	t.Parallel()

	var nilTracker *prefixCacheTracker
	if entry, ok := nilTracker.peek("bot:session"); ok || entry != (prefixCacheEntry{}) {
		t.Fatalf("nil tracker peek = %+v ok=%v, want zero value / false", entry, ok)
	}

	tracker := newPrefixCacheTracker()
	if entry, ok := tracker.peek("bot:session"); ok || entry != (prefixCacheEntry{}) {
		t.Fatalf("peek on empty tracker = %+v ok=%v, want zero value / false", entry, ok)
	}

	now := time.Unix(1000, 0)
	tracker.observe("bot:session", 3, "hash-1", "model-a", now)

	peeked, ok := tracker.peek("bot:session")
	if !ok || peeked.hash != "hash-1" || peeked.model != "model-a" || peeked.stableCount != 3 || !peeked.at.Equal(now) {
		t.Fatalf("peeked entry = %+v ok=%v, want hash-1/model-a/stableCount=3 at t0", peeked, ok)
	}

	// peek must not mutate state: a subsequent peek returns the same entry
	// and observe still reports it as the previous entry.
	if again, ok := tracker.peek("bot:session"); !ok || again != peeked {
		t.Fatalf("second peek = %+v ok=%v, want unchanged %+v", again, ok, peeked)
	}
	prev, hasPrev := tracker.observe("bot:session", 5, "hash-2", "model-a", now.Add(time.Minute))
	if !hasPrev || prev != peeked {
		t.Fatalf("observe after peek saw prev = %+v hasPrev=%v, want unchanged %+v", prev, hasPrev, peeked)
	}
}

func TestCompareCachePrefixOutcomes(t *testing.T) {
	t.Parallel()

	now := time.Unix(2000, 0)
	prev := prefixCacheEntry{hash: "same", model: "model-a", at: now.Add(-time.Minute)}
	ttl := 5 * time.Minute

	for _, tc := range []struct {
		name             string
		prev             prefixCacheEntry
		hasPrev          bool
		nowCount         int
		hash             string
		model            string
		prevBoundaryHash string
		firstRead        int
		want             string
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
		{
			name:             "prefix-preserving growth with real cache reads is a hit",
			prev:             prefixCacheEntry{hash: "prefix-a", model: "model-a", stableCount: 2, at: now.Add(-time.Minute)},
			hasPrev:          true,
			nowCount:         4,
			hash:             "prefix-a-plus-more",
			model:            "model-a",
			prevBoundaryHash: "prefix-a",
			firstRead:        100,
			want:             contextfrag.CacheOutcomeHit,
		},
		{
			// P3: the growth branch must apply the same reads-informed
			// classification as the equal-prefix branch. A hash match alone
			// (byte-identical bytes were requested) does not prove Anthropic
			// actually served them from cache; zero measured cache-read
			// tokens means no real hit happened yet.
			name:             "prefix-preserving growth with no cache reads is miss_same_prefix",
			prev:             prefixCacheEntry{hash: "prefix-a", model: "model-a", stableCount: 2, at: now.Add(-time.Minute)},
			hasPrev:          true,
			nowCount:         4,
			hash:             "prefix-a-plus-more",
			model:            "model-a",
			prevBoundaryHash: "prefix-a",
			firstRead:        0,
			want:             contextfrag.CacheOutcomeMissSamePrefix,
		},
		{
			// P3: real cache reads dominate TTL expiry, matching the equal-
			// prefix branch's precedence (reads>0 wins even past the TTL
			// window — the provider evidently still had it cached).
			name:             "prefix-preserving growth past TTL with real cache reads is still a hit",
			prev:             prefixCacheEntry{hash: "prefix-a", model: "model-a", stableCount: 2, at: now.Add(-10 * time.Minute)},
			hasPrev:          true,
			nowCount:         4,
			hash:             "prefix-a-plus-more",
			model:            "model-a",
			prevBoundaryHash: "prefix-a",
			firstRead:        100,
			want:             contextfrag.CacheOutcomeHit,
		},
		{
			name:             "same count but edited hash is prefix changed",
			prev:             prefixCacheEntry{hash: "prefix-a", model: "model-a", stableCount: 2, at: now.Add(-time.Minute)},
			hasPrev:          true,
			nowCount:         2,
			hash:             "prefix-a-edited",
			model:            "model-a",
			prevBoundaryHash: "prefix-a-edited",
			want:             contextfrag.CacheOutcomePrefixChanged,
		},
		{
			name:             "shrink is prefix changed regardless of hash",
			prev:             prefixCacheEntry{hash: "prefix-a", model: "model-a", stableCount: 4, at: now.Add(-time.Minute)},
			hasPrev:          true,
			nowCount:         2,
			hash:             "prefix-a",
			model:            "model-a",
			prevBoundaryHash: "prefix-a",
			want:             contextfrag.CacheOutcomePrefixChanged,
		},
		{
			name:             "model switch wins over growth-hit condition",
			prev:             prefixCacheEntry{hash: "prefix-a", model: "model-a", stableCount: 2, at: now.Add(-time.Minute)},
			hasPrev:          true,
			nowCount:         4,
			hash:             "prefix-a-plus-more",
			model:            "model-b",
			prevBoundaryHash: "prefix-a",
			want:             contextfrag.CacheOutcomeModelChanged,
		},
		{
			name:             "growth satisfied but TTL expired",
			prev:             prefixCacheEntry{hash: "prefix-a", model: "model-a", stableCount: 2, at: now.Add(-10 * time.Minute)},
			hasPrev:          true,
			nowCount:         4,
			hash:             "prefix-a-plus-more",
			model:            "model-a",
			prevBoundaryHash: "prefix-a",
			want:             contextfrag.CacheOutcomeExpired,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := compareCachePrefix(tc.prev, tc.hasPrev, tc.nowCount, tc.hash, tc.model, tc.prevBoundaryHash, tc.firstRead, now, ttl)
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
		tracker.observe(sessionKeyForTest(i), 0, "hash", "model-a", now.Add(time.Duration(i)*time.Second))
	}
	if size := tracker.size(); size > prefixCacheTrackerCap {
		t.Fatalf("tracker size = %d, want bounded by %d", size, prefixCacheTrackerCap)
	}
}

func sessionKeyForTest(i int) string {
	return "bot:" + string(rune('a'+i%26)) + string(rune('0'+i%10)) + "-" + time.Duration(i).String()
}
