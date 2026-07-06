package agent

import (
	"context"
	"testing"

	anthropicmessages "github.com/memohai/twilight-ai/provider/anthropic/messages"
	sdk "github.com/memohai/twilight-ai/sdk"

	"github.com/memohai/memoh/internal/contextfrag"
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
// Before the fix, both the stored hash (contextCachePlanWithRenderedPrefix)
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
	a.observePrefixCache(turn2)

	comparison := turn2.ContextMutations.CacheComparisonValue()
	if comparison == nil {
		t.Fatal("turn2 should have recorded a cache comparison")
	}
	if comparison.Outcome != contextfrag.CacheOutcomeHit {
		t.Fatalf("turn2 outcome = %q, want hit: the breakpoint moving from h1 onto h2 must not change h1's hash contribution", comparison.Outcome)
	}
}
