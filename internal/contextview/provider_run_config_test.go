package contextview

import (
	"context"
	"reflect"
	"testing"

	sdk "github.com/memohai/twilight-ai/sdk"

	contextfrag "github.com/memohai/memoh/internal/agent/context/fragment"
	agentpkg "github.com/memohai/memoh/internal/agent/runtime/native"
)

func TestApplyProviderRunConfigProducesManifestLedgerAndCachePlan(t *testing.T) {
	t.Parallel()
	ledger := contextfrag.NewMutationLedger()
	cfg := agentpkg.RunConfig{
		System: "system", Messages: []sdk.Message{sdk.UserMessage("history")}, Query: "current",
		ContextMutations:       ledger,
		ContextToolDefs:        []contextfrag.ToolDefAccounting{{Provider: "native", Name: "read", TokenEstimate: 7}},
		ContextDynamicMutators: []contextfrag.DynamicMutator{contextfrag.DynamicMutatorPromptCache},
	}
	got := ApplyProviderRunConfig(context.Background(), nil, cfg)
	if got.ContextMutations != ledger || got.ContextManifest.Mutations != ledger {
		t.Fatal("mutation ledger pointer was not preserved")
	}
	if got.ContextManifest.CachePlan == nil || *got.ContextManifest.CachePlan != got.ContextCachePlan {
		t.Fatalf("cache plan = %#v, field = %#v", got.ContextManifest.CachePlan, got.ContextCachePlan)
	}
	if len(got.ContextManifest.ToolDefs) != 1 || got.ContextManifest.ToolDefs[0].Name != "read" {
		t.Fatalf("tool definitions = %#v", got.ContextManifest.ToolDefs)
	}
	if !reflect.DeepEqual(got.ContextManifest.DynamicMutators, cfg.ContextDynamicMutators) {
		t.Fatalf("dynamic mutators = %#v", got.ContextManifest.DynamicMutators)
	}
	if got.ContextCachePlan.StableMessageCount != 1 {
		t.Fatalf("stable messages = %d, want history only", got.ContextCachePlan.StableMessageCount)
	}
	if got.ContextCachePlan.StablePrefixTokenEstimate <= 7 {
		t.Fatalf("stable prefix estimate = %d, want tool defs plus fragments", got.ContextCachePlan.StablePrefixTokenEstimate)
	}
}

func TestProviderRunConfigApplierUsesInjectedLoggerShape(t *testing.T) {
	t.Parallel()
	applier := ProviderRunConfigApplier(nil)
	got := applier(context.Background(), agentpkg.RunConfig{System: "system", Query: "query"})
	if got.System != "system" || len(got.Messages) != 1 {
		t.Fatalf("got = %#v", got)
	}
}
