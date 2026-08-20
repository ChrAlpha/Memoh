package contextview

import (
	"context"
	"fmt"
	"strings"
	"testing"

	sdk "github.com/memohai/twilight-ai/sdk"

	contextfrag "github.com/memohai/memoh/internal/agent/context/fragment"
	agentpkg "github.com/memohai/memoh/internal/agent/runtime/native"
)

// floorPricedHistory mirrors the application's history estimates, which are
// the legacy bytes/4 floor rather than the provider envelope estimate.
func floorPricedHistory(count, bytesEach int) []contextfrag.ContextFrag {
	frags := make([]contextfrag.ContextFrag, 0, count)
	for i := range count {
		text := strings.Repeat(string(rune('a'+i%26)), bytesEach)
		msg := sdk.UserMessage(text)
		if i%2 == 1 {
			msg = sdk.AssistantMessage(text)
		}
		frag := historyMessageFrag(fmt.Sprintf("h%02d", i), msg)
		frag.TokenEstimate = contextfrag.TokensFromBytes(bytesEach)
		frags = append(frags, frag)
	}
	return frags
}

func TestApplyProviderRunConfigTrimsFloorPricedHistoryToTheRenderedEnvelope(t *testing.T) {
	t.Parallel()

	frags := append(
		[]contextfrag.ContextFrag{systemTextFrag("system", strings.Repeat("s", 400), contextfrag.KindSystemPrompt, 100)},
		floorPricedHistory(60, 1_000)...,
	)
	frags = append(frags, currentMessageFrag("current", "latest question"))
	zero := 0
	cfg := agentpkg.RunConfig{
		ContextSourceFrags:         frags,
		ContextBudgetMaxTokens:     16_000,
		ContextRecentProtectTokens: &zero,
	}

	out, err := ProviderRunConfigApplier(nil)(context.Background(), cfg)
	if err != nil {
		t.Fatalf("ApplyProviderRunConfig() error = %v, want history trimmed until the rendered envelope fits", err)
	}
	plan := out.ContextManifest.BudgetPlan
	if plan == nil {
		t.Fatal("budget plan missing from manifest")
	}
	rendered := contextfrag.ProviderEnvelopeTokens(out.System, out.Messages, nil)
	if rendered+plan.OutputReserve > plan.Window {
		t.Fatalf("rendered envelope = %d + reserve %d exceeds window %d", rendered, plan.OutputReserve, plan.Window)
	}
	if len(out.Messages) < 2 || len(out.Messages) >= 61 {
		t.Fatalf("messages = %d, want a trimmed history that still keeps recent turns", len(out.Messages))
	}
	for _, record := range out.ContextMutations.Records() {
		if record.Kind == contextfrag.MutationContextBudgetFailure || record.Kind == contextfrag.MutationContextViewFallback {
			t.Fatalf("trimmable history recorded %+v", record)
		}
	}
}
