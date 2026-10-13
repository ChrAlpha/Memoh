package contextview

import (
	"context"
	"fmt"
	"strings"
	"testing"

	sdk "github.com/felinics/twilight/sdk"

	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	agentpkg "github.com/felinics/memoh/internal/agent/runtime/native"
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

// History over the window goes to the provider as it is (the provider names
// the overflow): nothing is trimmed and no local budget failure is recorded.
func TestApplyProviderRunConfigSendsHistoryOverTheWindowAsItIs(t *testing.T) {
	t.Parallel()

	frags := append(
		[]contextfrag.ContextFrag{systemTextFrag("system", strings.Repeat("s", 400), contextfrag.KindSystemPrompt, 100)},
		floorPricedHistory(60, 1_000)...,
	)
	frags = append(frags, currentMessageFrag("current", "latest question"))

	out, err := ProviderRunConfigApplier(nil)(context.Background(), agentpkg.RunConfig{
		ContextSourceFrags:     frags,
		ContextBudgetMaxTokens: 16_000,
	})
	if err != nil {
		t.Fatalf("ApplyProviderRunConfig() error = %v, want the history sent as it is", err)
	}
	plan := out.ContextManifest.BudgetPlan
	if plan == nil {
		t.Fatal("budget plan missing from manifest")
	}
	if rendered := contextfrag.ProviderEnvelopeTokens(out.System, out.Messages, nil); rendered+plan.OutputReserve <= plan.Window {
		t.Fatalf("rendered envelope = %d + reserve %d fits window %d; the fixture must overflow it", rendered, plan.OutputReserve, plan.Window)
	}
	if len(out.Messages) != 61 {
		t.Fatalf("messages = %d, want all 60 history rows and the current message", len(out.Messages))
	}
	for _, record := range out.ContextMutations.Records() {
		if record.Kind == contextfrag.MutationContextBudgetFailure || record.Kind == contextfrag.MutationContextViewFallback {
			t.Fatalf("history over the window recorded %+v", record)
		}
	}
}
