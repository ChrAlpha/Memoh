package agent

import (
	"context"
	"testing"

	sdk "github.com/memohai/twilight-ai/sdk"
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
