package contextview

import (
	"context"
	"encoding/json"
	"testing"

	sdk "github.com/memohai/twilight-ai/sdk"

	agentpkg "github.com/memohai/memoh/internal/agent"
	"github.com/memohai/memoh/internal/contextfrag"
)

func TestApplyProviderContextViewMatchesLegacyAssembly(t *testing.T) {
	t.Parallel()

	cfg := agentpkg.RunConfig{
		System: "system prompt\n\ntool usage guidance",
		Messages: []sdk.Message{
			sdk.UserMessage("history question"),
			sdk.AssistantMessage("history answer"),
			sdk.UserMessage("current question"),
		},
		Query:                    "current question",
		ContextQueryMaterialized: true,
		ContextScope: contextfrag.Scope{
			BotID:     "bot-1",
			SessionID: "session-1",
		},
	}

	legacy := cfg.RefreshContextFrag()
	got := ApplyProviderRunConfig(context.Background(), nil, cfg)

	if got.System != cfg.System {
		t.Fatalf("System = %q, want unchanged %q", got.System, cfg.System)
	}
	if !sdkMessagesJSONEqual(got.Messages, cfg.Messages) {
		t.Fatalf("Messages diverged from legacy input")
	}
	if got.Query != cfg.Query {
		t.Fatalf("Query = %q, want untouched %q", got.Query, cfg.Query)
	}
	if len(got.ContextFrags) == 0 {
		t.Fatal("ContextFrags should be populated by context view")
	}
	if got.ContextManifest.View != contextfrag.ViewRunConfigPreProvider {
		t.Fatalf("Manifest.View = %q, want %q", got.ContextManifest.View, contextfrag.ViewRunConfigPreProvider)
	}
	if len(got.ContextManifest.Items) != len(legacy.ContextManifest.Items) {
		t.Fatalf("manifest items = %d, want legacy %d", len(got.ContextManifest.Items), len(legacy.ContextManifest.Items))
	}
	for i, item := range got.ContextManifest.Items {
		if item.Kind != legacy.ContextManifest.Items[i].Kind || item.Slot != legacy.ContextManifest.Items[i].Slot {
			t.Fatalf("manifest item %d = %s/%s, want legacy %s/%s",
				i, item.Kind, item.Slot,
				legacy.ContextManifest.Items[i].Kind, legacy.ContextManifest.Items[i].Slot)
		}
	}
}

func TestApplyProviderContextViewKeepsUnmaterializedQuery(t *testing.T) {
	t.Parallel()

	cfg := agentpkg.RunConfig{
		System:   "system prompt",
		Messages: []sdk.Message{sdk.UserMessage("history")},
		Query:    "live query",
		ContextScope: contextfrag.Scope{
			BotID:     "bot-1",
			SessionID: "session-1",
		},
	}

	legacy := cfg.RefreshContextFrag()
	got := ApplyProviderRunConfig(context.Background(), nil, cfg)

	if got.Query != "live query" {
		t.Fatalf("Query = %q, want untouched", got.Query)
	}
	if len(got.ContextFrags) != len(legacy.ContextFrags) {
		t.Fatalf("frags = %d, want legacy %d", len(got.ContextFrags), len(legacy.ContextFrags))
	}
	foundCurrentUser := false
	for _, frag := range got.ContextFrags {
		if frag.Kind == contextfrag.KindCurrentUserMessage {
			foundCurrentUser = true
		}
	}
	if !foundCurrentUser {
		t.Fatal("unmaterialized query should produce a current_user fragment")
	}
}

func TestApplyProviderContextViewUsesContextBudget(t *testing.T) {
	t.Parallel()

	cfg := agentpkg.RunConfig{
		Messages: []sdk.Message{
			sdk.UserMessage("old question that should be dropped"),
			sdk.AssistantMessage("old answer that should be dropped"),
			sdk.UserMessage("latest question"),
		},
		ContextHistoryTokenEstimates: []int{100, 100, 100},
		ContextTrimmableMessages:     3,
		ContextQueryMaterialized:     true,
		ContextBudgetMaxTokens:       1,
		ContextScope: contextfrag.Scope{
			BotID:     "bot-1",
			SessionID: "session-1",
		},
	}

	got := ApplyProviderRunConfig(context.Background(), nil, cfg)

	if len(got.Messages) != 2 {
		t.Fatalf("messages = %d, want trim notice plus latest question", len(got.Messages))
	}
	if !sdkMessagesJSONEqual(got.Messages[1], sdk.UserMessage("latest question")) {
		t.Fatalf("kept message = %#v, want latest question", got.Messages[1])
	}
	if len(got.ContextManifest.Items) != 2 ||
		got.ContextManifest.Items[0].ID != "history.trim_notice" ||
		got.ContextManifest.Items[1].ID != "message.002" {
		t.Fatalf("manifest items = %#v, want notice then latest message", got.ContextManifest.Items)
	}
	// The trim notice is always CacheNever and lands first among the
	// non-system items, so StableMessageCount is correctly 0 here: a
	// budget-trimmed prefix is not stable across turns and must not get a
	// cache breakpoint.
	if got.ContextCachePlan.StableMessageCount != 0 {
		t.Fatalf("stable message count = %d, want 0 when a trim notice is present", got.ContextCachePlan.StableMessageCount)
	}
}

func TestApplyProviderContextViewCountsImagesAgainstTokenBudget(t *testing.T) {
	t.Parallel()

	cfg := agentpkg.RunConfig{
		Messages: []sdk.Message{
			sdk.UserMessage("", sdk.ImagePart{Image: "data:image/png;base64,old", MediaType: "image/png"}),
			sdk.UserMessage("", sdk.ImagePart{Image: "data:image/png;base64,latest", MediaType: "image/png"}),
		},
		ContextTrimmableMessages: 2,
		ContextQueryMaterialized: true,
		ContextBudgetMaxTokens:   1,
		ContextScope: contextfrag.Scope{
			BotID:     "bot-1",
			SessionID: "session-1",
		},
	}

	got := ApplyProviderRunConfig(context.Background(), nil, cfg)

	if len(got.Messages) != 2 {
		t.Fatalf("messages = %d, want trim notice plus latest image message", len(got.Messages))
	}
	if !sdkMessagesJSONEqual(got.Messages[1], cfg.Messages[1]) {
		t.Fatalf("kept message = %#v, want latest image message", got.Messages[1])
	}
	if len(got.ContextManifest.Items) != 2 || got.ContextManifest.Items[1].ID != "message.001" {
		t.Fatalf("manifest items = %#v, want notice then latest image message", got.ContextManifest.Items)
	}
}

func TestApplyProviderContextViewPreservesDynamicMutators(t *testing.T) {
	t.Parallel()

	cfg := agentpkg.RunConfig{
		System:   "system prompt",
		Messages: []sdk.Message{sdk.UserMessage("history")},
		ContextDynamicMutators: []contextfrag.DynamicMutator{
			contextfrag.DynamicMutatorPromptCache,
			contextfrag.DynamicMutatorPromptCache,
			contextfrag.DynamicMutatorBeforeModelCallHook,
		},
	}

	got := ApplyProviderRunConfig(context.Background(), nil, cfg)

	want := []contextfrag.DynamicMutator{
		contextfrag.DynamicMutatorPromptCache,
		contextfrag.DynamicMutatorBeforeModelCallHook,
	}
	if !dynamicMutatorsEqual(got.ContextManifest.DynamicMutators, want) {
		t.Fatalf("dynamic mutators = %#v, want %#v", got.ContextManifest.DynamicMutators, want)
	}
}

func dynamicMutatorsEqual(got, want []contextfrag.DynamicMutator) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func sdkMessagesJSONEqual(got, want any) bool {
	gotRaw, gotErr := json.Marshal(got)
	wantRaw, wantErr := json.Marshal(want)
	if gotErr != nil || wantErr != nil {
		return false
	}
	return string(gotRaw) == string(wantRaw)
}

func TestApplyProviderContextViewProducesCachePlan(t *testing.T) {
	t.Parallel()

	cfg := agentpkg.RunConfig{
		System:   "stable system prompt",
		Messages: []sdk.Message{sdk.UserMessage("history")},
		ContextScope: contextfrag.Scope{
			BotID:     "bot-1",
			SessionID: "session-1",
		},
	}

	got := ApplyProviderRunConfig(context.Background(), nil, cfg)

	if got.ContextCachePlan.StablePrefixHash == "" {
		t.Fatal("cache plan should carry the stable prefix hash")
	}
	if got.ContextCachePlan.StableMessageCount != 1 {
		t.Fatalf("history is cache-stable now, stable message count = %d, want 1", got.ContextCachePlan.StableMessageCount)
	}
}

func TestApplyProviderContextViewStableMessageCountExcludesMemoryAndCurrent(t *testing.T) {
	t.Parallel()

	cfg := agentpkg.RunConfig{
		System: "stable system prompt",
		Messages: []sdk.Message{
			sdk.UserMessage("h1"),
			sdk.AssistantMessage("h2"),
			sdk.UserMessage("h3"),
		},
		ContextMemoryText: "remembered fact",
		Query:             "current question",
		ContextScope: contextfrag.Scope{
			BotID:     "bot-1",
			SessionID: "session-1",
		},
	}

	got := ApplyProviderRunConfig(context.Background(), nil, cfg)

	if got.ContextCachePlan.StableMessageCount != 3 {
		t.Fatalf("stable message count = %d, want 3 (memory/current must stay cache-volatile)", got.ContextCachePlan.StableMessageCount)
	}
}
