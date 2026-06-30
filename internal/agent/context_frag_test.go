package agent

import (
	"testing"

	sdk "github.com/memohai/twilight-ai/sdk"

	"github.com/memohai/memoh/internal/contextfrag"
)

func TestRefreshContextFragBuildsTypedViewWithoutChangingLegacyFields(t *testing.T) {
	t.Parallel()

	cfg := RunConfig{
		System:       "system prompt",
		Messages:     []sdk.Message{sdk.UserMessage("history")},
		Query:        "current query",
		InlineImages: []sdk.ImagePart{{Image: "data:image/png;base64,abc", MediaType: "image/png"}},
		ContextScope: contextfrag.Scope{
			BotID:          "bot-1",
			SessionID:      "session-1",
			TurnID:         "turn-1",
			ViewHeadTurnID: "head-1",
			TurnMessageSeq: 3,
			SessionMode:    "chat",
			RuntimeType:    "model",
		},
		ContextDynamicMutators: []contextfrag.DynamicMutator{
			contextfrag.DynamicMutatorReadMedia,
			contextfrag.DynamicMutatorReadMedia,
		},
	}

	got := cfg.RefreshContextFrag()

	if got.System != cfg.System || got.Query != cfg.Query {
		t.Fatalf("legacy text fields changed: got system=%q query=%q", got.System, got.Query)
	}
	if len(got.Messages) != 1 || len(got.InlineImages) != 1 {
		t.Fatalf("legacy message/image fields changed: messages=%d images=%d", len(got.Messages), len(got.InlineImages))
	}
	if len(got.ContextFrags) == 0 {
		t.Fatal("RefreshContextFrag should populate typed fragments")
	}
	if got.ContextManifest.View != contextfrag.ViewRunConfigPreProvider {
		t.Fatalf("manifest view = %q", got.ContextManifest.View)
	}
	if len(got.ContextManifest.DynamicMutators) != 1 || got.ContextManifest.DynamicMutators[0] != contextfrag.DynamicMutatorReadMedia {
		t.Fatalf("dynamic mutators = %#v", got.ContextManifest.DynamicMutators)
	}
	for _, item := range got.ContextManifest.Items {
		if item.Scope.TurnID != "turn-1" || item.Scope.ViewHeadTurnID != "head-1" {
			t.Fatalf("manifest item lost Turn DAG scope: %#v", item.Scope)
		}
	}
}
