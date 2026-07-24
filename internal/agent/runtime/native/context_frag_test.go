package native

import (
	"testing"

	sdk "github.com/memohai/twilight-ai/sdk"

	"github.com/memohai/memoh/internal/agent/background"
	contextfrag "github.com/memohai/memoh/internal/agent/context/fragment"
	tools "github.com/memohai/memoh/internal/agent/tool"
)

func TestRefreshContextFragOmitsMaterializedQuery(t *testing.T) {
	t.Parallel()

	cfg := RunConfig{
		System:                   "base system",
		Query:                    "current user",
		Messages:                 []sdk.Message{sdk.UserMessage("current user")},
		ContextQueryMaterialized: true,
	}

	cfg = cfg.RefreshContextFrag()

	if manifestHasAgentKind(cfg.ContextManifest, contextfrag.KindCurrentUserMessage) {
		t.Fatalf("manifest should not include pending current user query after it was materialized: %#v", cfg.ContextManifest.Items)
	}
	if cfg.ContextManifest.Counts.Messages != 1 {
		t.Fatalf("manifest message count = %d, want 1", cfg.ContextManifest.Counts.Messages)
	}
}

func TestRefreshContextFragOmitsMaterializedInlineImages(t *testing.T) {
	t.Parallel()

	image := sdk.ImagePart{Image: "data:image/png;base64,abc", MediaType: "image/png"}
	cfg := RunConfig{
		System:                   "base system",
		Query:                    "current user",
		Messages:                 []sdk.Message{sdk.UserMessage("current user", image)},
		InlineImages:             []sdk.ImagePart{image},
		ContextQueryMaterialized: true,
	}

	cfg = cfg.RefreshContextFrag()

	if cfg.ContextManifest.Counts.Images != 1 {
		t.Fatalf("manifest image count = %d, want only materialized message image: %#v", cfg.ContextManifest.Counts.Images, cfg.ContextManifest.Items)
	}
	rendered := contextfrag.Render(cfg.ContextFrags)
	if len(rendered.InlineImages) != 0 {
		t.Fatalf("rendered inline images = %#v, want images only inside materialized message", rendered.InlineImages)
	}
}

func TestRefreshContextFragMarksToolUsageBeforeWorkspaceInstructions(t *testing.T) {
	t.Parallel()

	cfg := RunConfig{
		System: appendToolUsageToSystem(
			"base system\n\n## Workspace instruction files\n\nworkspace text",
			"## Tool usage\n\nusage text",
		),
		ContextToolUsage: "## Tool usage\n\nusage text",
		Messages:         []sdk.Message{sdk.UserMessage("hi")},
	}

	cfg = cfg.RefreshContextFrag()

	toolIndex := manifestAgentKindIndex(cfg.ContextManifest, contextfrag.KindToolUsage)
	workspaceIndex := manifestAgentKindIndex(cfg.ContextManifest, contextfrag.KindWorkspaceInstruction)
	if toolIndex < 0 {
		t.Fatalf("manifest missing tool usage item: %#v", cfg.ContextManifest.Items)
	}
	if workspaceIndex < 0 {
		t.Fatalf("manifest missing workspace instruction item: %#v", cfg.ContextManifest.Items)
	}
	if toolIndex > workspaceIndex {
		t.Fatalf("tool usage manifest index = %d, workspace index = %d; want tool usage before workspace", toolIndex, workspaceIndex)
	}
	rendered := contextfrag.Render(cfg.ContextFrags)
	if rendered.System != cfg.System {
		t.Fatalf("rendered system = %q, want %q", rendered.System, cfg.System)
	}
}

func TestAppendToolUsageToSystemUsesSharedWorkspaceAnchor(t *testing.T) {
	t.Parallel()

	system := "base system" + contextfrag.WorkspaceInstructionAnchor + "\n\nworkspace text"
	got := appendToolUsageToSystem(system, "## Tool usage\n\nusage text")

	want := "base system\n\n## Tool usage\n\nusage text\n\n## Workspace instruction files\n\nworkspace text"
	if got != want {
		t.Fatalf("appendToolUsageToSystem() = %q, want %q", got, want)
	}
}

func TestSpawnRunConfigCarriesContextScopeAndMaterializedQuery(t *testing.T) {
	t.Parallel()

	rc := runConfigFromSpawnRunConfig(tools.SpawnRunConfig{
		Query:    "do the task",
		Messages: []sdk.Message{sdk.UserMessage("history")},
		Identity: tools.SpawnIdentity{
			BotID:             "bot-1",
			ChatID:            "chat-1",
			SessionID:         "child-1",
			ChannelIdentityID: "identity-1",
			CurrentPlatform:   "telegram",
			IsSubagent:        true,
		},
	})

	if !rc.ContextQueryMaterialized {
		t.Fatal("spawn query should be marked materialized because it is appended to Messages")
	}
	rc = rc.RefreshContextFrag()
	if manifestHasAgentKind(rc.ContextManifest, contextfrag.KindCurrentUserMessage) {
		t.Fatalf("manifest should not include duplicate pending current user query: %#v", rc.ContextManifest.Items)
	}
	if rc.ContextManifest.Counts.Messages != 2 {
		t.Fatalf("manifest message count = %d, want history + materialized query", rc.ContextManifest.Counts.Messages)
	}
	for _, item := range rc.ContextManifest.Items {
		if item.Scope.BotID != "bot-1" || item.Scope.SessionID != "child-1" || item.Scope.ChannelIdentityID != "identity-1" {
			t.Fatalf("manifest item lost subagent scope: %#v", item.Scope)
		}
	}
}

func TestSpawnRunConfigCarriesContextBudgetAndToolExchangePolicy(t *testing.T) {
	t.Parallel()

	policy := &contextfrag.ToolExchangePolicy{MinMessages: 10}
	rc := runConfigFromSpawnRunConfig(tools.SpawnRunConfig{
		ContextBudgetMaxTokens:    128000,
		ContextToolExchangePolicy: policy,
	})

	if rc.ContextBudgetMaxTokens != 128000 {
		t.Fatalf("ContextBudgetMaxTokens = %d, want 128000", rc.ContextBudgetMaxTokens)
	}
	if rc.ContextToolExchangePolicy != policy {
		t.Fatalf("ContextToolExchangePolicy = %p, want same pointer %p", rc.ContextToolExchangePolicy, policy)
	}
}

func TestRefreshContextFragWithMutatorsMarksPreProviderBoundary(t *testing.T) {
	t.Parallel()

	injectCh := make(chan InjectMessage)
	cfg := RunConfig{
		System:            "base system",
		Messages:          []sdk.Message{sdk.UserMessage("hi")},
		InjectCh:          injectCh,
		BackgroundManager: background.New(nil),
	}
	cfg.ContextDynamicMutators = cfg.contextDynamicMutators(true, true, true)
	cfg = cfg.RefreshContextFrag()

	if cfg.ContextManifest.View != contextfrag.ViewRunConfigPreProvider {
		t.Fatalf("manifest view = %q, want %q", cfg.ContextManifest.View, contextfrag.ViewRunConfigPreProvider)
	}
	for _, want := range []contextfrag.DynamicMutator{
		contextfrag.DynamicMutatorInjectCh,
		contextfrag.DynamicMutatorReadMedia,
		contextfrag.DynamicMutatorBeforeModelCallHook,
		contextfrag.DynamicMutatorBackgroundSummary,
		contextfrag.DynamicMutatorMidTaskPrune,
	} {
		if !manifestHasMutator(cfg.ContextManifest, want) {
			t.Fatalf("manifest dynamic mutators = %#v, want %q", cfg.ContextManifest.DynamicMutators, want)
		}
	}
}

func TestRefreshContextFragPreservesLifecycleAccounting(t *testing.T) {
	t.Parallel()

	holder := contextfrag.NewLifecycleHolder()
	ledger := contextfrag.NewMutationLedger()
	ledger.Record(contextfrag.MutationBeforeModelCallHook, "system_bytes=12")
	ledger.RecordCacheUsage(contextfrag.CacheUsageRecord{StepIndex: 0, CacheReadTokens: 7, CacheWriteTokens: 3})
	ledger.SetFinalInputHash("final-hash")
	plan := contextfrag.CachePlan{
		StablePrefixHash:           "stable-hash",
		StableMessageCount:         2,
		CacheComparatorPrefixHash:  "rendered-hash",
		CacheComparatorPrefixBytes: 128,
	}
	cfg := RunConfig{
		System:           "base system",
		Messages:         []sdk.Message{sdk.UserMessage("hi")},
		ContextLifecycle: holder,
		ContextManifest: contextfrag.Manifest{
			CachePlan: &plan,
			Mutations: ledger,
			Selection: &contextfrag.SelectionTrace{
				Selected: 2,
				Dropped:  1,
				DropReasons: map[string]int{
					"can_drop": 1,
				},
			},
		},
	}

	cfg = cfg.RefreshContextFrag()

	if cfg.ContextManifest.Mutations != ledger {
		t.Fatal("RefreshContextFrag dropped the existing mutation ledger")
	}
	if cfg.ContextManifest.CachePlan == nil || cfg.ContextManifest.CachePlan.CacheComparatorPrefixHash != "rendered-hash" {
		t.Fatalf("RefreshContextFrag cache plan = %#v, want previous cache plan", cfg.ContextManifest.CachePlan)
	}
	snapshot, ok := holder.Snapshot()
	if !ok {
		t.Fatal("lifecycle holder has no snapshot")
	}
	if snapshot.FinalInputHash != "final-hash" {
		t.Fatalf("snapshot final hash = %q, want final-hash", snapshot.FinalInputHash)
	}
	if snapshot.CacheComparatorPrefixHash != "rendered-hash" {
		t.Fatalf("snapshot cache comparator prefix hash = %q, want rendered-hash", snapshot.CacheComparatorPrefixHash)
	}
	if snapshot.CacheReadTokens != 7 || snapshot.CacheWriteTokens != 3 {
		t.Fatalf("snapshot cache read/write = %d/%d, want 7/3", snapshot.CacheReadTokens, snapshot.CacheWriteTokens)
	}
	if snapshot.Selection.DropReasons["can_drop"] != 1 {
		t.Fatalf("snapshot selection = %#v, want preserved drop reason", snapshot.Selection)
	}
}

func manifestHasAgentKind(manifest contextfrag.Manifest, kind contextfrag.Kind) bool {
	return manifestAgentKindIndex(manifest, kind) >= 0
}

func manifestHasMutator(manifest contextfrag.Manifest, want contextfrag.DynamicMutator) bool {
	for _, got := range manifest.DynamicMutators {
		if got == want {
			return true
		}
	}
	return false
}

func manifestAgentKindIndex(manifest contextfrag.Manifest, kind contextfrag.Kind) int {
	for i, item := range manifest.Items {
		if item.Kind == kind {
			return i
		}
	}
	return -1
}
