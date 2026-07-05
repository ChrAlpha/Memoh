package contextview

import (
	"context"
	"strings"
	"testing"

	sdk "github.com/memohai/twilight-ai/sdk"

	agentpkg "github.com/memohai/memoh/internal/agent"
	"github.com/memohai/memoh/internal/contextfrag"
)

func providerRunConfigFixture() agentpkg.RunConfig {
	return agentpkg.RunConfig{
		System:   "base system",
		Messages: []sdk.Message{sdk.UserMessage("hi")},
		ContextScope: contextfrag.Scope{
			BotID:     "bot-1",
			SessionID: "session-1",
		},
	}
}

func TestApplyProviderRunConfigCachePlanCoversToolUsage(t *testing.T) {
	t.Parallel()

	base := ApplyProviderRunConfig(context.Background(), nil, providerRunConfigFixture())

	withUsage := providerRunConfigFixture()
	withUsage.System = "base system\n\n## Tool usage\n\nUSE_TOOLS_WISELY"
	withUsage.ContextToolUsage = "USE_TOOLS_WISELY"
	got := ApplyProviderRunConfig(context.Background(), nil, withUsage)

	if base.ContextCachePlan.StablePrefixHash == "" || got.ContextCachePlan.StablePrefixHash == "" {
		t.Fatal("both runs must produce a stable prefix hash")
	}
	if base.ContextCachePlan.StablePrefixHash == got.ContextCachePlan.StablePrefixHash {
		t.Fatal("tool usage must be part of the stable prefix hash")
	}

	var toolUsageFrag bool
	for _, frag := range got.ContextFrags {
		if frag.Kind == contextfrag.KindToolUsage {
			toolUsageFrag = true
		}
	}
	if !toolUsageFrag {
		t.Fatal("tool usage must be selected as its own fragment")
	}
}

func TestApplyProviderRunConfigProducesManifestAndLedger(t *testing.T) {
	t.Parallel()

	got := ApplyProviderRunConfig(context.Background(), nil, providerRunConfigFixture())
	if len(got.ContextManifest.Items) == 0 {
		t.Fatal("provider view must produce a manifest")
	}
	if got.ContextMutations == nil {
		t.Fatal("provider view must install a mutation ledger")
	}
	if got.System == "" || len(got.Messages) == 0 {
		t.Fatal("rendered payload must populate system and messages")
	}
}

func TestApplyProviderRunConfigManifestCarriesLifecycle(t *testing.T) {
	t.Parallel()

	got := ApplyProviderRunConfig(context.Background(), nil, providerRunConfigFixture())

	if got.ContextManifest.CachePlan == nil || *got.ContextManifest.CachePlan != got.ContextCachePlan {
		t.Fatalf("manifest cache plan = %v, want the run cache plan %v", got.ContextManifest.CachePlan, got.ContextCachePlan)
	}
	if got.ContextManifest.Mutations != got.ContextMutations {
		t.Fatal("manifest must reference the same mutation ledger as the run config")
	}

	got.ContextMutations.Record(contextfrag.MutationBackgroundSummary, "test")
	got.ContextMutations.SetFinalInputHash("final-hash")
	if got.ContextManifest.Mutations.FinalInputHash() != "final-hash" {
		t.Fatal("mutations recorded after the view must be visible through the manifest")
	}
	if records := got.ContextManifest.Mutations.Records(); len(records) != 1 {
		t.Fatalf("manifest mutation records = %d, want 1", len(records))
	}
}

func TestApplyProviderRunConfigPublishesLifecycleToHolder(t *testing.T) {
	t.Parallel()

	holder := contextfrag.NewLifecycleHolder()
	cfg := providerRunConfigFixture()
	cfg.ContextLifecycle = holder

	got := ApplyProviderRunConfig(context.Background(), nil, cfg)
	got.ContextMutations.SetFinalInputHash("final-hash")

	snapshot, ok := holder.Snapshot()
	if !ok {
		t.Fatal("lifecycle holder did not receive provider manifest")
	}
	if snapshot.View != contextfrag.ViewRunConfigPreProvider {
		t.Fatalf("snapshot view = %q", snapshot.View)
	}
	if snapshot.StablePrefixHash != got.ContextCachePlan.StablePrefixHash {
		t.Fatalf("snapshot stable prefix hash = %q, want %q", snapshot.StablePrefixHash, got.ContextCachePlan.StablePrefixHash)
	}
	if snapshot.FinalInputHash != "final-hash" {
		t.Fatalf("snapshot final hash = %q", snapshot.FinalInputHash)
	}
}

func TestProviderStepReselectorPreservesPrefixAndDropsLoopSpan(t *testing.T) {
	t.Parallel()

	prefix := []sdk.Message{
		sdk.UserMessage("initial request"),
		sdk.AssistantMessage("initial answer"),
	}
	messages := append(append([]sdk.Message(nil), prefix...),
		sdk.Message{Role: sdk.MessageRoleAssistant, Content: []sdk.MessagePart{sdk.ToolCallPart{
			ToolCallID: "old-call",
			ToolName:   "search",
			Input:      map[string]any{"q": "old"},
		}}},
		sdk.ToolMessage(sdk.ToolResultPart{
			ToolCallID: "old-call",
			ToolName:   "search",
			Result:     strings.Repeat("old ", 2048),
		}),
		sdk.Message{Role: sdk.MessageRoleAssistant, Content: []sdk.MessagePart{sdk.ToolCallPart{
			ToolCallID: "new-call",
			ToolName:   "search",
			Input:      map[string]any{"q": "new"},
		}}},
		sdk.ToolMessage(sdk.ToolResultPart{
			ToolCallID: "new-call",
			ToolName:   "search",
			Result:     "new",
		}),
	)

	result := SelectProviderStepMessages(context.Background(), agentpkg.ContextStepSelectionInput{
		Scope:               contextfrag.Scope{BotID: "bot-1", SessionID: "session-1"},
		InitialMessageCount: len(prefix),
		Messages:            messages,
		BudgetMaxTokens:     1,
	})

	if result.Dropped != 2 {
		t.Fatalf("Dropped = %d, want 2", result.Dropped)
	}
	if got := len(result.Messages); got != 5 {
		t.Fatalf("Messages len = %d, want 5", got)
	}
	for i := range prefix {
		if result.Messages[i].Role != prefix[i].Role {
			t.Fatalf("prefix role %d = %q, want %q", i, result.Messages[i].Role, prefix[i].Role)
		}
	}
	notice, ok := result.Messages[2].Content[0].(sdk.TextPart)
	if !ok || notice.Text != HistoryTrimNotice {
		t.Fatalf("trim notice = %#v, want history trim notice", result.Messages[2].Content[0])
	}
	call, ok := result.Messages[3].Content[0].(sdk.ToolCallPart)
	if !ok || call.ToolCallID != "new-call" {
		t.Fatalf("first loop message after trim notice = %#v, want new tool call", result.Messages[3].Content[0])
	}
	if result.DropReasons[string(TagCanDrop)] != 2 {
		t.Fatalf("DropReasons = %#v, want the droppable cause can_drop:2", result.DropReasons)
	}
}
