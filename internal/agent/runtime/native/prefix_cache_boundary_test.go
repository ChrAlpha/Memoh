package native

import (
	"context"
	"testing"
	"time"

	anthropicmessages "github.com/memohai/twilight-ai/provider/anthropic/messages"
	sdk "github.com/memohai/twilight-ai/sdk"

	contextfrag "github.com/memohai/memoh/internal/agent/context/fragment"
	"github.com/memohai/memoh/internal/models"
)

func anthropicPrefixCacheTestModel() *sdk.Model {
	provider := anthropicmessages.New(anthropicmessages.WithAPIKey("test"))
	return provider.ChatModel("claude-test")
}

// newPrefixCacheRunConfig builds a RunConfig wired the way the real pipeline
// wires one: ContextManifest.CachePlan is pre-allocated so
// publishContextCachePlan mutates it in place (RunConfig is copied by value
// across calls, but that CachePlan field is a pointer, so the mutation is
// visible to the caller), and each run gets its own mutation ledger.
func newPrefixCacheRunConfig(identity SessionContext, model *sdk.Model, system string, messages []sdk.Message, stableCount int) RunConfig {
	return RunConfig{
		Model:            model,
		System:           system,
		Messages:         messages,
		PromptCacheTTL:   models.DefaultPromptCacheTTL,
		Identity:         identity,
		ContextCachePlan: contextfrag.CachePlan{StableMessageCount: stableCount},
		ContextManifest:  contextfrag.Manifest{CachePlan: &contextfrag.CachePlan{}},
		ContextMutations: contextfrag.NewMutationLedger(),
	}
}

// TestPrefixCacheGrowthSurvivesAnthropicBreakpointMovement is the P2 RED
// test. Turn 1 caches [system, h1] with the message-level breakpoint on h1
// (the last stable message this turn). Turn 2's history grows to [h1, h2],
// so the breakpoint moves onto h2 and h1 no longer carries cache_control in
// the freshly-decorated payload — but the underlying cached bytes for
// [system, h1] are unchanged.
//
// Before the fix, both the stored hash (contextCachePlanWithComparatorPrefix)
// and the boundary re-hash (recordPrefixCacheBoundary) were computed AFTER
// cache_control decoration, so h1's serialized bytes differ across turns
// purely because the breakpoint moved off it, and the growth-hit branch in
// compareCachePrefix never fires for Anthropic — the only vendor with a
// breakpoint to move. Hashing the raw pre-decoration payload instead makes
// the comparison decoration-agnostic.
func TestPrefixCacheGrowthSurvivesAnthropicBreakpointMovement(t *testing.T) {
	t.Parallel()

	a := &Agent{prefixCache: newPrefixCacheTracker()}
	model := anthropicPrefixCacheTestModel()
	identity := SessionContext{BotID: "bot-1", SessionID: "session-1"}

	turn1 := newPrefixCacheRunConfig(identity, model, "sys", []sdk.Message{sdk.UserMessage("h1")}, 1)
	a.buildGenerateOptions(context.Background(), turn1, nil, nil, nil)
	a.observePrefixCache(turn1)
	if got := turn1.ContextMutations.CacheComparisonValue(); got == nil || got.Outcome != contextfrag.CacheOutcomeFirstObservation {
		t.Fatalf("turn1 outcome = %+v, want first_observation", got)
	}

	turn2 := newPrefixCacheRunConfig(identity, model, "sys", []sdk.Message{sdk.UserMessage("h1"), sdk.AssistantMessage("h2")}, 2)
	a.buildGenerateOptions(context.Background(), turn2, nil, nil, nil)
	// Real cache-read tokens on step 0: per P3 (classifyKnownPrefixOutcome),
	// a hash match alone does not prove a hit, so this test's "hit" claim
	// needs the same reads evidence the equal-prefix branch requires.
	turn2.ContextMutations.RecordCacheUsage(contextfrag.CacheUsageRecord{StepIndex: 0, CacheReadTokens: 100})
	a.observePrefixCache(turn2)

	comparison := turn2.ContextMutations.CacheComparisonValue()
	if comparison == nil {
		t.Fatal("turn2 should have recorded a cache comparison")
	}
	if comparison.Outcome != contextfrag.CacheOutcomeHit {
		t.Fatalf("turn2 outcome = %q, want hit: the breakpoint moving from h1 onto h2 must not change h1's hash contribution", comparison.Outcome)
	}
}

// TestPrefixCacheObserveUsesOwnPeekedSnapshotUnderConcurrency is the P4 RED
// test. Two runs of the same session (A and B) both peek the tracker before
// either observes, simulating concurrent requests: both see the same seed
// entry. B's observe() runs first and overwrites the tracker with its own
// entry. A's observe() must still classify against the snapshot A itself
// peeked at build time — not against whatever the tracker holds by the time
// A gets around to observing, which by then is B's entry.
func TestPrefixCacheObserveUsesOwnPeekedSnapshotUnderConcurrency(t *testing.T) {
	t.Parallel()

	a := &Agent{prefixCache: newPrefixCacheTracker()}
	model := anthropicPrefixCacheTestModel()
	identity := SessionContext{BotID: "bot-1", SessionID: "session-1"}

	seed := newPrefixCacheRunConfig(identity, model, "sys", []sdk.Message{sdk.UserMessage("seed")}, 1)
	a.buildGenerateOptions(context.Background(), seed, nil, nil, nil)
	a.observePrefixCache(seed)

	// Both A and B peek before either observes.
	runA := newPrefixCacheRunConfig(identity, model, "sys", []sdk.Message{sdk.UserMessage("seed")}, 1)
	a.buildGenerateOptions(context.Background(), runA, nil, nil, nil)

	runB := newPrefixCacheRunConfig(identity, model, "sys", []sdk.Message{sdk.UserMessage("totally different content")}, 1)
	a.buildGenerateOptions(context.Background(), runB, nil, nil, nil)

	// B finishes first, overwriting the tracker with its own entry.
	a.observePrefixCache(runB)
	if got := runB.ContextMutations.CacheComparisonValue(); got == nil || got.Outcome != contextfrag.CacheOutcomePrefixChanged {
		t.Fatalf("runB outcome = %+v, want prefix_changed", got)
	}

	// A finishes second. Its comparison must be based on what A itself
	// peeked (the seed entry, same content as A), not on B's now-stored entry.
	a.observePrefixCache(runA)
	comparison := runA.ContextMutations.CacheComparisonValue()
	if comparison == nil {
		t.Fatal("runA should have recorded a cache comparison")
	}
	if comparison.Outcome != contextfrag.CacheOutcomeMissSamePrefix {
		t.Fatalf("runA outcome = %q, want miss_same_prefix based on its own peeked snapshot (same content as seed), not a misclassification from racing against runB's later write", comparison.Outcome)
	}
}

// TestRecordPrefixCacheBoundarySkipsHashWhenCountUnchanged is the P6 RED
// test. compareCachePrefix's equal-prefix branch (prev.stableCount ==
// nowCount) never reads prevBoundaryHash — only the growth branch
// (prev.stableCount < nowCount) does. So when this turn's stable count
// matches the previous turn's exactly, recordPrefixCacheBoundary hashing a
// boundary slice is wasted work computed on every turn for nothing.
func TestRecordPrefixCacheBoundarySkipsHashWhenCountUnchanged(t *testing.T) {
	t.Parallel()

	a := &Agent{prefixCache: newPrefixCacheTracker()}
	identity := SessionContext{BotID: "bot-1", SessionID: "session-1"}
	key := prefixCacheSessionKey(identity)
	a.prefixCache.observe(key, 2, "prior-hash", "model-x", time.Now())

	cfg := RunConfig{
		Identity:         identity,
		ContextMutations: contextfrag.NewMutationLedger(),
	}
	messages := []sdk.Message{sdk.UserMessage("a"), sdk.UserMessage("b")}
	a.recordPrefixCacheBoundary(cfg, "sys", messages, nil, 2) // prefixCount == prev.stableCount(2)

	if got := cfg.ContextMutations.PrevBoundaryHash(); got != "" {
		t.Fatalf("boundary hash = %q, want empty: the equal-prefix branch never reads it, so it must not be computed", got)
	}
	if got := cfg.ContextMutations.ComparatorPrefixMessageCount(); got != 2 {
		t.Fatalf("comparator prefix message count = %d, want 2 (still recorded regardless of the boundary-hash skip)", got)
	}
	peeked := cfg.ContextMutations.PeekedPrevCacheEntry()
	if !peeked.Found || peeked.StableCount != 2 {
		t.Fatalf("peeked prev entry = %+v, want Found=true StableCount=2 (P4's snapshot capture stays unconditional)", peeked)
	}
}
