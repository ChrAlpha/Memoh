package contextview

import (
	"context"
	"strings"
	"testing"

	sdk "github.com/memohai/twilight-ai/sdk"

	contextfrag "github.com/memohai/memoh/internal/agent/context/fragment"
	agentpkg "github.com/memohai/memoh/internal/agent/runtime/native"
)

func TestProviderViewFallbackKeepsLegacyBytesAndAudit(t *testing.T) {
	t.Parallel()
	duplicate := systemTextFrag("duplicate", "source ignored", contextfrag.KindSystemPrompt, 20)
	image := sdk.ImagePart{Image: "data:image/png;base64,abc", MediaType: "image/png"}
	cfg := agentpkg.RunConfig{
		System: "  legacy system \n", Messages: []sdk.Message{sdk.AssistantMessage("history")},
		Query: "  current \n", InlineImages: []sdk.ImagePart{image},
		ContextSourceFrags: []contextfrag.ContextFrag{duplicate, duplicate},
	}
	got := ApplyProviderRunConfig(context.Background(), nil, cfg)
	if got.System != cfg.System {
		t.Fatalf("system = %q, want %q", got.System, cfg.System)
	}
	assertMessagesEqual(t, got.Messages, []sdk.Message{sdk.AssistantMessage("history"), sdk.UserMessage(cfg.Query, image)})
	if got.ContextManifest.CachePlan == nil || got.ContextManifest.Mutations == nil || got.ContextMutations == nil {
		t.Fatalf("manifest = %#v", got.ContextManifest)
	}
	records := got.ContextMutations.Records()
	if len(records) != 1 || records[0].Kind != contextfrag.MutationContextViewFallback {
		t.Fatalf("records = %#v", records)
	}
}

func TestLegacyMaterializeQuerySplicesToolUsageBeforeWorkspace(t *testing.T) {
	t.Parallel()
	cfg := agentpkg.RunConfig{
		System:           "base\n\n## Workspace instruction files\n\nworkspace",
		ContextToolUsage: "## Tool usage\n\nUSE_TOOLS",
	}
	got := legacyMaterializeQuery(cfg)
	if !strings.Contains(got.System, "## Tool usage") {
		t.Fatalf("system = %q, want tool usage", got.System)
	}
	if strings.Index(got.System, "## Tool usage") > strings.Index(got.System, "## Workspace instruction files") {
		t.Fatalf("system = %q", got.System)
	}
}

func TestLegacyMaterializeQueryPreservesRawMemoryBeforeCurrent(t *testing.T) {
	t.Parallel()
	memory := sdk.UserMessage("<memory>raw & unescaped</memory>")
	cfg := agentpkg.RunConfig{Messages: []sdk.Message{memory}, Query: "current"}
	got := legacyMaterializeQuery(cfg)
	assertMessagesEqual(t, got.Messages, []sdk.Message{memory, sdk.UserMessage("current")})
}
