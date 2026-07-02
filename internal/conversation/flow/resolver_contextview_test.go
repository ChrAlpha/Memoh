package flow

import (
	"context"
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
			TurnID:    "turn-1",
		},
	}

	legacy := cfg.RefreshContextFrag()
	got := applyProviderContextView(context.Background(), nil, cfg)

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
	got := applyProviderContextView(context.Background(), nil, cfg)

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
