package agent

import (
	"sync"
	"time"

	"github.com/memohai/memoh/internal/contextfrag"
)

const prefixCacheTrackerCap = 4096

// prefixCacheTracker remembers the last rendered stable prefix hash per
// session so consecutive turns can attribute prompt cache hits and misses
// in-process instead of offline.
//
// This state is in-memory and per-process: under a horizontally-scaled
// deployment where replicas share a session, each replica only observes its
// own slice of turns, so this is a single-instance observability aid, not a
// substitute for offline/cross-instance analysis.
type prefixCacheTracker struct {
	mu      sync.Mutex
	entries map[string]prefixCacheEntry
}

type prefixCacheEntry struct {
	hash  string
	model string
	at    time.Time
}

func newPrefixCacheTracker() *prefixCacheTracker {
	return &prefixCacheTracker{entries: make(map[string]prefixCacheEntry)}
}

// observe stores the current hash and model for the session key and returns
// the previous entry, if any.
func (t *prefixCacheTracker) observe(key, hash, model string, now time.Time) (prefixCacheEntry, bool) {
	if t == nil {
		return prefixCacheEntry{}, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	prev, ok := t.entries[key]
	if !ok && len(t.entries) >= prefixCacheTrackerCap {
		t.evictOldestLocked()
	}
	t.entries[key] = prefixCacheEntry{hash: hash, model: model, at: now}
	return prev, ok
}

func (t *prefixCacheTracker) evictOldestLocked() {
	var oldestKey string
	var oldestAt time.Time
	first := true
	for key, entry := range t.entries {
		if first || entry.at.Before(oldestAt) {
			oldestKey = key
			oldestAt = entry.at
			first = false
		}
	}
	if !first {
		delete(t.entries, oldestKey)
	}
}

func (t *prefixCacheTracker) size() int {
	if t == nil {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.entries)
}

// compareCachePrefix classifies this run's rendered prefix against the
// previous run of the same session. ttlWindow <= 0 disables the expired
// classification (cache disabled or unknown vendor TTL).
func compareCachePrefix(prev prefixCacheEntry, hasPrev bool, hash, model string, firstStepCacheRead int, now time.Time, ttlWindow time.Duration) contextfrag.CacheComparison {
	comparison := contextfrag.CacheComparison{
		FirstStepCacheReadTokens: firstStepCacheRead,
	}
	if !hasPrev {
		comparison.Outcome = contextfrag.CacheOutcomeFirstObservation
		return comparison
	}
	comparison.PrevAgeMs = now.Sub(prev.at).Milliseconds()
	switch {
	case prev.model != model:
		comparison.Outcome = contextfrag.CacheOutcomeModelChanged
	case prev.hash != hash:
		comparison.Outcome = contextfrag.CacheOutcomePrefixChanged
	case firstStepCacheRead > 0:
		comparison.Outcome = contextfrag.CacheOutcomeHit
	case ttlWindow > 0 && now.Sub(prev.at) > ttlWindow:
		comparison.Outcome = contextfrag.CacheOutcomeExpired
	default:
		comparison.Outcome = contextfrag.CacheOutcomeMissSamePrefix
	}
	return comparison
}
