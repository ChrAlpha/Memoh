package agent

import (
	"context"
	"testing"

	sdk "github.com/memohai/twilight-ai/sdk"

	"github.com/memohai/memoh/internal/contextfrag"
	"github.com/memohai/memoh/internal/models"
)

func TestBuildGenerateOptionsAnthropicDecoratedHashDiffersFromComparator(t *testing.T) {
	t.Parallel()

	a := &Agent{prefixCache: newPrefixCacheTracker()}
	model := anthropicPrefixCacheTestModel()
	identity := SessionContext{BotID: "bot-1", SessionID: "session-1"}
	cfg := newPrefixCacheRunConfig(identity, model, "sys", []sdk.Message{sdk.UserMessage("h1")}, 1)

	a.buildGenerateOptions(context.Background(), cfg, nil, nil, nil)

	plan := cfg.ContextManifest.CachePlan
	if plan.CacheComparatorPrefixHash == "" || plan.DecoratedProviderPrefixHash == "" {
		t.Fatalf("expected both hashes populated, got %#v", plan)
	}
	if plan.DecoratedProviderPrefixHash == plan.CacheComparatorPrefixHash {
		t.Fatal("Anthropic decoration promotes the system prompt and adds cache_control, so the decorated hash must differ from the pre-decoration comparator hash")
	}
}

func TestBuildGenerateOptionsUndecoratedVendorHashesMatch(t *testing.T) {
	t.Parallel()

	a := &Agent{prefixCache: newPrefixCacheTracker()}
	model := &sdk.Model{ID: "openai-model", Provider: &usageRecordingProvider{}, Type: sdk.ModelTypeChat}
	identity := SessionContext{BotID: "bot-1", SessionID: "session-1"}
	cfg := newPrefixCacheRunConfig(identity, model, "sys", []sdk.Message{sdk.UserMessage("h1")}, 1)

	a.buildGenerateOptions(context.Background(), cfg, nil, nil, nil)

	plan := cfg.ContextManifest.CachePlan
	if plan.CacheComparatorPrefixHash == "" {
		t.Fatal("comparator hash should be populated even for a vendor without cache decoration")
	}
	if plan.DecoratedProviderPrefixHash != plan.CacheComparatorPrefixHash {
		t.Fatalf("undecorated vendor: decorated hash %q must equal comparator hash %q (ApplyPromptCacheWithPlan is a no-op)", plan.DecoratedProviderPrefixHash, plan.CacheComparatorPrefixHash)
	}
}

// TestBuildGenerateOptionsUndecoratedVendorZeroStableCountHashesMatch is the
// RED test for the finding: on a no-op decoration path (non-Anthropic
// vendor) with StableMessageCount=0 on a fresh session, the decorated and
// comparator hashes must still agree even though the comparator hashes a
// nil message span and decoratedProviderPrefixHash historically sliced a
// non-nil empty span, which serializes differently.
func TestBuildGenerateOptionsUndecoratedVendorZeroStableCountHashesMatch(t *testing.T) {
	t.Parallel()

	a := &Agent{prefixCache: newPrefixCacheTracker()}
	model := &sdk.Model{ID: "openai-model", Provider: &usageRecordingProvider{}, Type: sdk.ModelTypeChat}
	identity := SessionContext{BotID: "bot-1", SessionID: "session-1"}
	cfg := newPrefixCacheRunConfig(identity, model, "sys", []sdk.Message{sdk.UserMessage("h1")}, 0)

	a.buildGenerateOptions(context.Background(), cfg, nil, nil, nil)

	plan := cfg.ContextManifest.CachePlan
	if plan.CacheComparatorPrefixHash == "" {
		t.Fatal("comparator hash should be populated even for a vendor without cache decoration")
	}
	if plan.DecoratedProviderPrefixHash != plan.CacheComparatorPrefixHash {
		t.Fatalf("undecorated vendor, zero stable count: decorated hash %q must equal comparator hash %q (ApplyPromptCacheWithPlan is a no-op)", plan.DecoratedProviderPrefixHash, plan.CacheComparatorPrefixHash)
	}
}

// TestBuildGenerateOptionsAnthropicDecoratedHashMatchesIndependentRecompute
// is a wiring tripwire: it recomputes the expected decorated hash from a
// standalone call to models.ApplyPromptCacheWithPlan rather than from
// decoratedProviderPrefixHash itself, so a call-site mutation at agent.go's
// buildGenerateOptions (e.g. passing rawPrefixCount instead of
// actualStableCount, or a hardcoded systemPrepended) changes the hash
// buildGenerateOptions produces without changing this test's expectation.
// The message list forces the Anthropic breakpoint to fall back to an
// earlier message (ToolResultPart carries no cache_control), so
// actualStableCount(1) diverges from the raw claimed stable count(2) and the
// two would disagree if the wrong count were wired through.
func TestBuildGenerateOptionsAnthropicDecoratedHashMatchesIndependentRecompute(t *testing.T) {
	t.Parallel()

	a := &Agent{prefixCache: newPrefixCacheTracker()}
	model := anthropicPrefixCacheTestModel()
	identity := SessionContext{BotID: "bot-1", SessionID: "session-1"}
	system := "sys"
	messages := []sdk.Message{
		sdk.UserMessage("stable text message"),
		sdk.ToolMessage(sdk.ToolResultPart{ToolCallID: "call-1", ToolName: "search", Result: "ok"}),
		sdk.UserMessage("volatile question"),
	}

	expectedSystem, expectedMessages, expectedTools, systemPrepended, actualStableCount := models.ApplyPromptCacheWithPlan(
		model, models.DefaultPromptCacheTTL, contextfrag.CachePlan{StableMessageCount: 2}, system, messages, nil,
	)
	count := actualStableCount
	if systemPrepended {
		count++
	}
	if count < 0 {
		count = 0
	}
	if count > len(expectedMessages) {
		count = len(expectedMessages)
	}
	prefixMessages := append([]sdk.Message(nil), expectedMessages[:count]...)
	wantHash, _ := contextfrag.ProviderPayloadHashAndBytes(expectedSystem, prefixMessages, expectedTools)

	cfg := newPrefixCacheRunConfig(identity, model, system, messages, 2)
	a.buildGenerateOptions(context.Background(), cfg, nil, nil, nil)

	plan := cfg.ContextManifest.CachePlan
	if plan.DecoratedProviderPrefixHash != wantHash {
		t.Fatalf("decorated hash = %q, want %q (independently recomputed from ApplyPromptCacheWithPlan outputs)", plan.DecoratedProviderPrefixHash, wantHash)
	}
}

func TestBuildGenerateOptionsToolSchemaChangeShiftsBothHashes(t *testing.T) {
	t.Parallel()

	a := &Agent{prefixCache: newPrefixCacheTracker()}
	model := anthropicPrefixCacheTestModel()
	identity := SessionContext{BotID: "bot-1", SessionID: "session-1"}

	toolsA := []sdk.Tool{{Name: "alpha"}}
	toolsB := []sdk.Tool{{Name: "alpha"}, {Name: "beta"}}

	cfgA := newPrefixCacheRunConfig(identity, model, "sys", []sdk.Message{sdk.UserMessage("h1")}, 1)
	a.buildGenerateOptions(context.Background(), cfgA, toolsA, nil, nil)
	planA := cfgA.ContextManifest.CachePlan

	cfgB := newPrefixCacheRunConfig(identity, model, "sys", []sdk.Message{sdk.UserMessage("h1")}, 1)
	a.buildGenerateOptions(context.Background(), cfgB, toolsB, nil, nil)
	planB := cfgB.ContextManifest.CachePlan

	if planA.CacheComparatorPrefixHash == planB.CacheComparatorPrefixHash {
		t.Fatal("comparator hash must change when tool schema changes")
	}
	if planA.DecoratedProviderPrefixHash == planB.DecoratedProviderPrefixHash {
		t.Fatal("decorated hash must change when tool schema changes")
	}
}
