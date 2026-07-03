package contextview

import (
	"context"
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
