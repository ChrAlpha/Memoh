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
	hash        string
	model       string
	stableCount int
	at          time.Time
}

func newPrefixCacheTracker() *prefixCacheTracker {
	return &prefixCacheTracker{entries: make(map[string]prefixCacheEntry)}
}

// observe stores the current hash, model and stable-prefix message count for
// the session key and returns the previous entry, if any.
func (t *prefixCacheTracker) observe(key string, stableCount int, hash, model string, now time.Time) (prefixCacheEntry, bool) {
	if t == nil {
		return prefixCacheEntry{}, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	prev, ok := t.entries[key]
	if !ok && len(t.entries) >= prefixCacheTrackerCap {
		t.evictOldestLocked()
	}
	t.entries[key] = prefixCacheEntry{hash: hash, model: model, stableCount: stableCount, at: now}
	return prev, ok
}

// peek returns the current entry for the session key without mutating any
// state (no timestamp refresh, no eviction, no store).
func (t *prefixCacheTracker) peek(key string) (prefixCacheEntry, bool) {
	if t == nil {
		return prefixCacheEntry{}, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	entry, ok := t.entries[key]
	return entry, ok
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
//
// Because history frags are cache-stable, the stable-prefix message count
// legitimately grows turn over turn as history accumulates, so a flat hash
// comparison over the whole (longer) prefix would never match the previous
// turn's (shorter) hash even when the model's cached bytes are unchanged.
// The growth branch below recognizes that case via prevBoundaryHash: the
// previous turn's rendered prefix re-hashed against this turn's messages. If
// that still equals the previous turn's stored hash, the previously-cached
// bytes are byte-identical, so the same reads-informed classification used
// for an unchanged prefix applies here too (see classifyKnownPrefixOutcome):
// a hash match only proves identical bytes were requested, not that the
// provider actually served them from cache.
func compareCachePrefix(prev prefixCacheEntry, hasPrev bool, nowCount int, hash, model, prevBoundaryHash string, firstStepCacheRead int, now time.Time, ttlWindow time.Duration) contextfrag.CacheComparison {
	comparison := contextfrag.CacheComparison{
		FirstStepCacheReadTokens: firstStepCacheRead,
	}
	if !hasPrev {
		comparison.Outcome = contextfrag.CacheOutcomeFirstObservation
		return comparison
	}
	comparison.PrevAgeMs = now.Sub(prev.at).Milliseconds()
	expired := ttlWindow > 0 && now.Sub(prev.at) > ttlWindow
	switch {
	case prev.model != model:
		comparison.Outcome = contextfrag.CacheOutcomeModelChanged
	case prev.stableCount == nowCount && prev.hash == hash:
		comparison.Outcome = classifyKnownPrefixOutcome(expired, firstStepCacheRead)
	case prev.stableCount < nowCount && prevBoundaryHash != "" && prevBoundaryHash == prev.hash:
		comparison.Outcome = classifyKnownPrefixOutcome(expired, firstStepCacheRead)
	default:
		comparison.Outcome = contextfrag.CacheOutcomePrefixChanged
	}
	return comparison
}

// classifyKnownPrefixOutcome is the reads-informed classification shared by
// both branches that established the rendered prefix matches the previous
// turn's cached bytes (byte-for-byte equal, or a prefix-preserving growth of
// them): a hash match alone only proves the same bytes were requested, not
// that the provider actually served them from cache, so measured cache-read
// tokens decide hit vs miss, and only take the TTL window into account when
// there is no such evidence either way.
func classifyKnownPrefixOutcome(expired bool, firstStepCacheRead int) string {
	switch {
	case firstStepCacheRead > 0:
		return contextfrag.CacheOutcomeHit
	case expired:
		return contextfrag.CacheOutcomeExpired
	default:
		return contextfrag.CacheOutcomeMissSamePrefix
	}
}
